package inbound

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/eventing/egress"
)

// Handoff is the durable delivery of canonical payloads to their owning domains. It runs apart from the request
// that accepted the delivery, so the sender's answer is transport acceptance only (202) and never implies a
// business effect. A domain that times out after possibly accepting is resolved by resending with the SAME
// idempotency key (the exchange id); the domain's own idempotency, not a second command, settles it (NP-14).
//
// Every attempt re-checks the destination against egress policy (DNS, address ranges, region) before any request
// is built, and the HTTP client dials only addresses that pass the same policy, so a destination that was fine at
// approval and is not now is not called.

// HandoffDeps are the collaborators of the handoff worker.
type HandoffDeps struct {
	Egress         egress.Policy
	AllowedRegions []string
	HTTP           *http.Client
	// Check overrides the destination check. It exists for tests that must call a loopback server; production leaves
	// it nil and every destination goes through Egress.
	Check     func(ctx context.Context, url string) (string, error)
	Principal string
}

const maxDomainResponse = 64 << 10

func (s *Service) client() *http.Client {
	if s.Handoff.HTTP != nil {
		return s.Handoff.HTTP
	}
	return s.Handoff.Egress.NewHTTPClient(15 * time.Second)
}

// Backoff is the delay before the nth handoff attempt after a failure.
func (s *Service) Backoff(attempt int) time.Duration {
	cfg := s.cfg()
	d := cfg.HandoffBackoffBase
	for i := 1; i < attempt && d < cfg.HandoffBackoffMax; i++ {
		d *= 2
	}
	if d > cfg.HandoffBackoffMax {
		d = cfg.HandoffBackoffMax
	}
	return d
}

var errNotDue = errors.New("not due")

// DriveDue delivers every exchange the store says is due. It returns how many it worked on; one exchange failing
// does not stop the others.
func (s *Service) DriveDue(ctx context.Context, limit int) (int, error) {
	due, err := s.Repo.Due(ctx, s.now(), limit)
	if err != nil {
		return 0, err
	}
	n := 0
	var first error
	for _, d := range due {
		did, err := s.Drive(ctx, d)
		if did {
			n++
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return n, first
}

// RunHandoff drives due exchanges every interval and purges expired bodies, until ctx ends.
func (s *Service) RunHandoff(ctx context.Context, interval time.Duration, batch int, report func(n int, err error)) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.DriveDue(ctx, batch)
			if _, perr := s.Repo.PurgeExpired(ctx, s.now(), batch); perr != nil && err == nil {
				err = perr
			}
			if report != nil {
				report(n, err)
			}
		}
	}
}

func transientCode(status int) string { return fmt.Sprintf("HTTP_%d", status) }

// Drive makes one delivery attempt for one exchange.
func (s *Service) Drive(ctx context.Context, ref DueRef) (bool, error) {
	tctx := middleware.WithTenant(ctx, ref.TenantID)
	now := s.now()
	cfg := s.cfg()

	// claim: lease the exchange so concurrent workers leave it alone, and count the attempt
	var claimed *Exchange
	_, err := s.Repo.Mutate(tctx, ref.ExchangeID, func(tx *Tx) error {
		if tx.E.State != StateValidated || tx.E.NextAttemptAt == nil || tx.E.NextAttemptAt.After(now) {
			return errNotDue
		}
		lease := now.Add(cfg.HandoffLease)
		tx.E.NextAttemptAt = &lease
		tx.E.HandoffAttempts++
		cpy := *tx.E
		claimed = &cpy
		tx.NewEvent = &Event{Kind: "HANDOFF_ATTEMPT", From: StateValidated, To: StateValidated, Actor: "system:xic-inbound-gateway", At: now,
			Detail: mustJSON(map[string]any{"attempt": tx.E.HandoffAttempts})}
		return nil
	})
	if errors.Is(err, errNotDue) || errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	target, terr := s.Repo.ActiveTarget(tctx, claimed.CanonicalContract)
	if terr != nil || target == nil {
		return true, s.recordUnknown(tctx, claimed, "NO_TARGET", 0, now)
	}
	if !containsFold(s.Handoff.AllowedRegions, target.Region) {
		_ = s.Repo.RecordSignal(tctx, claimed.TenantID, OutEvent{Type: "integration.security.handoff-region-blocked", AggregateType: "inbound-exchange", AggregateID: claimed.ExchangeID,
			ActorID: "system:xic-inbound-gateway", Data: map[string]any{"tenant_id": claimed.TenantID, "exchange_id": claimed.ExchangeID, "reason_code": "XIC-016"}})
		return true, s.recordUnknown(tctx, claimed, "REGION_BLOCKED", 0, now)
	}
	full := strings.TrimRight(target.BaseURL, "/") + target.Path
	if _, err := s.checkDest(ctx, full); err != nil {
		_ = s.Repo.RecordSignal(tctx, claimed.TenantID, OutEvent{Type: "integration.security.handoff-egress-blocked", AggregateType: "inbound-exchange", AggregateID: claimed.ExchangeID,
			ActorID: "system:xic-inbound-gateway", Data: map[string]any{"tenant_id": claimed.TenantID, "exchange_id": claimed.ExchangeID, "reason_code": "XIC-009"}})
		return true, s.recordUnknown(tctx, claimed, "EGRESS_BLOCKED", 0, now)
	}

	status, body, sendErr := s.send(ctx, claimed, target, full)
	switch {
	case sendErr != nil:
		return true, s.recordUnknown(tctx, claimed, "NETWORK", 0, now)
	case status >= 200 && status < 300:
		return true, s.recordAccepted(tctx, claimed, target, status, body, now)
	case status == 400 || status == 404 || status == 409 || status == 412 || status == 422:
		return true, s.recordDomainRejected(tctx, claimed, target, status, now)
	default:
		// 401/403 (our identity is not accepted), 429, 5xx, 3xx: the outcome is not a business answer. Resend with the
		// same key later; never treat it as a rejection and never as success.
		return true, s.recordUnknown(tctx, claimed, transientCode(status), status, now)
	}
}

func (s *Service) send(ctx context.Context, e *Exchange, t *Target, full string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full, bytes.NewReader(e.Canonical))
	if err != nil {
		return 0, nil, err
	}
	principal := dflt(s.Handoff.Principal, "system:xic-inbound-gateway")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", e.TenantID)
	req.Header.Set("X-Principal-Id", principal)
	req.Header.Set("X-Request-Id", e.ExchangeID)
	req.Header.Set("X-Correlation-Id", dflt(e.CorrelationID, e.ExchangeID))
	req.Header.Set("X-Source-Channel", "integration")
	req.Header.Set("X-Source-System", e.ConnectorID)
	req.Header.Set("X-Purpose-Context", "inbound-exchange")
	req.Header.Set("X-Timezone", "UTC")
	req.Header.Set("X-Xic-Exchange-Id", e.ExchangeID)
	req.Header.Set("X-Xic-Source-Ref-Hash", e.SourceRefHash)
	// the idempotency key is stable for the exchange, across every retry: a resend after an ambiguous answer asks
	// the domain what it already did instead of asking it to do it again
	req.Header.Set("Idempotency-Key", e.ExchangeID)
	if t.LegalEntityPointer != "" {
		if le := pointerString(parseJSON(e.Canonical), t.LegalEntityPointer); le != "" {
			req.Header.Set("X-Legal-Entity-Id", le)
		}
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxDomainResponse))
	return resp.StatusCode, b, nil
}

func domainRef(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	for _, k := range []string{"id", "reference", "ref", "object_id"} {
		if v, ok := m[k].(string); ok && v != "" && len(v) <= 128 {
			return v
		}
	}
	return ""
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func (s *Service) recordAccepted(ctx context.Context, claimed *Exchange, t *Target, status int, body []byte, now time.Time) error {
	_, err := s.Repo.Mutate(ctx, claimed.ExchangeID, func(tx *Tx) error {
		if tx.E.State != StateValidated {
			return errNotDue // released, dismissed or already finished by another path
		}
		tx.E.State, tx.E.DomainOutcome, tx.E.TargetID = StateHandedOff, DomainAccepted, t.TargetID
		tx.E.DomainStatus, tx.E.DomainRef, tx.E.LastErrorCode, tx.E.NextAttemptAt = status, domainRef(body), "", nil
		tx.NewEvent = &Event{Kind: "HANDED_OFF", From: StateValidated, To: StateHandedOff, Actor: "system:xic-inbound-gateway", At: now,
			Detail: mustJSON(map[string]any{"domain_status": status, "attempt": tx.E.HandoffAttempts})}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.inbound.handed-off", AggregateType: "inbound-exchange", AggregateID: tx.E.ExchangeID, ActorID: "system:xic-inbound-gateway",
			CorrelationID: tx.E.CorrelationID, Data: map[string]any{"exchange_id": tx.E.ExchangeID, "connection_id": tx.E.ConnectionID, "tenant_id": tx.E.TenantID,
				"canonical_contract": tx.E.CanonicalContract, "domain_status": status}})
		s.observe("handed_off", "accepted")
		return nil
	})
	if errors.Is(err, errNotDue) {
		return nil
	}
	return err
}

// recordDomainRejected is the domain's own "no": business rejection, recorded apart from the transport acceptance
// that preceded it (NP-13). The owning domain or its workflow decides the retry or case path.
func (s *Service) recordDomainRejected(ctx context.Context, claimed *Exchange, t *Target, status int, now time.Time) error {
	_, err := s.Repo.Mutate(ctx, claimed.ExchangeID, func(tx *Tx) error {
		if tx.E.State != StateValidated {
			return errNotDue
		}
		tx.E.State, tx.E.DomainOutcome, tx.E.TargetID = StateQuarantined, DomainRejected, t.TargetID
		tx.E.QuarantineClass, tx.E.ReasonCode, tx.E.DomainStatus, tx.E.NextAttemptAt = ClassDomainPrecondition, fmt.Sprintf("DOMAIN_REJECTED_%d", status), status, nil
		tx.NewEvent = &Event{Kind: "DOMAIN_REJECTED", From: StateValidated, To: StateQuarantined, Actor: "system:xic-inbound-gateway", At: now,
			Detail: mustJSON(map[string]any{"domain_status": status, "attempt": tx.E.HandoffAttempts})}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.inbound.quarantined", AggregateType: "inbound-exchange", AggregateID: tx.E.ExchangeID, ActorID: "system:xic-inbound-gateway",
			CorrelationID: tx.E.CorrelationID, Data: map[string]any{"exchange_id": tx.E.ExchangeID, "connection_id": tx.E.ConnectionID, "tenant_id": tx.E.TenantID,
				"reason_code": tx.E.ReasonCode, "schema_version": tx.E.SourceSchema, "remediation_class": ClassDomainPrecondition}})
		s.observe("quarantined", tx.E.ReasonCode)
		return nil
	})
	if errors.Is(err, errNotDue) {
		return nil
	}
	return err
}

// recordUnknown records an attempt with no business answer. The exchange stays VALIDATED with domain outcome
// UNKNOWN and is retried with the same idempotency key on a backoff; after the configured number of attempts the
// stall is escalated once. It is never marked failed to clear a queue.
func (s *Service) recordUnknown(ctx context.Context, claimed *Exchange, code string, status int, now time.Time) error {
	cfg := s.cfg()
	_, err := s.Repo.Mutate(ctx, claimed.ExchangeID, func(tx *Tx) error {
		if tx.E.State != StateValidated {
			return errNotDue
		}
		next := now.Add(s.Backoff(tx.E.HandoffAttempts))
		tx.E.DomainOutcome, tx.E.LastErrorCode, tx.E.DomainStatus, tx.E.NextAttemptAt = DomainUnknown, code, status, &next
		tx.NewEvent = &Event{Kind: "HANDOFF_UNKNOWN", From: StateValidated, To: StateValidated, Actor: "system:xic-inbound-gateway", At: now,
			Detail: mustJSON(map[string]any{"code": code, "attempt": tx.E.HandoffAttempts, "next_attempt_at": next.Format(time.RFC3339)})}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.inbound.handoff-unknown", AggregateType: "inbound-exchange", AggregateID: tx.E.ExchangeID, ActorID: "system:xic-inbound-gateway",
			CorrelationID: tx.E.CorrelationID, Data: map[string]any{"exchange_id": tx.E.ExchangeID, "tenant_id": tx.E.TenantID, "reason_code": code, "attempt": tx.E.HandoffAttempts}})
		if tx.E.HandoffAttempts == cfg.HandoffMaxAttempts {
			tx.Out = append(tx.Out, OutEvent{Type: "integration.inbound.handoff-stalled", AggregateType: "inbound-exchange", AggregateID: tx.E.ExchangeID, ActorID: "system:xic-inbound-gateway",
				CorrelationID: tx.E.CorrelationID, Data: map[string]any{"exchange_id": tx.E.ExchangeID, "tenant_id": tx.E.TenantID, "reason_code": code, "attempt": tx.E.HandoffAttempts}})
		}
		s.observe("handoff_unknown", code)
		return nil
	})
	if errors.Is(err, errNotDue) {
		return nil
	}
	return err
}

// ── handoff targets (platform data, maker-checker) ───────────────────────────

// CreateTargetInput registers where a canonical contract's payloads are delivered.
type CreateTargetInput struct {
	CanonicalContract  string `json:"canonical_contract"`
	BaseURL            string `json:"base_url"`
	Path               string `json:"path"`
	Region             string `json:"region"`
	LegalEntityPointer string `json:"legal_entity_pointer,omitempty"`
}

func targetFingerprint(in CreateTargetInput) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{in.CanonicalContract, in.BaseURL, in.Path, in.Region, in.LegalEntityPointer}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (s *Service) validateTarget(ctx context.Context, in *CreateTargetInput) error {
	var is []string
	if strings.TrimSpace(in.CanonicalContract) == "" || len(in.CanonicalContract) > 128 {
		is = append(is, "canonical_contract")
	}
	u, err := url.Parse(in.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		is = append(is, "base_url")
	}
	if !strings.HasPrefix(in.Path, "/") || strings.ContainsAny(in.Path, "?#@ ") || strings.Contains(in.Path, "..") || strings.Contains(in.Path, "//") {
		is = append(is, "path")
	}
	in.Region = strings.ToLower(strings.TrimSpace(in.Region))
	if !containsFold(s.Handoff.AllowedRegions, in.Region) {
		is = append(is, "region")
	}
	if in.LegalEntityPointer != "" && !strings.HasPrefix(in.LegalEntityPointer, "/") {
		is = append(is, "legal_entity_pointer")
	}
	if len(is) > 0 {
		return fmt.Errorf("%w: %s", ErrBadRequest, strings.Join(is, ", "))
	}
	return nil
}

// CreateTarget registers a DRAFT target. Platform scope only.
func (s *Service) CreateTarget(ctx context.Context, tenant string, a Actor, in CreateTargetInput) (*Target, error) {
	if tenant != platformScope {
		return nil, ErrNotPlatform
	}
	if err := s.validateTarget(ctx, &in); err != nil {
		return nil, err
	}
	now := s.now()
	t := &Target{TargetID: newID(), CanonicalContract: in.CanonicalContract, BaseURL: strings.TrimRight(in.BaseURL, "/"), Path: in.Path, Region: in.Region,
		LegalEntityPointer: in.LegalEntityPointer, Status: TargetDraft, Fingerprint: targetFingerprint(in), CreatedBy: a.PrincipalID, Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.Repo.CreateTarget(ctx, t, OutEvent{Type: "integration.handoff-target.created", AggregateType: "handoff-target", AggregateID: t.TargetID, ActorID: a.PrincipalID,
		CorrelationID: a.CorrelationID, Data: map[string]any{"target_id": t.TargetID, "canonical_contract": t.CanonicalContract, "region": t.Region}}); err != nil {
		return nil, err
	}
	return t, nil
}

// ApproveTarget activates a target. Someone other than its author approves it, and its destination must pass egress
// policy now (and is checked again on every delivery).
func (s *Service) ApproveTarget(ctx context.Context, tenant string, a Actor, id string, expected int64) (*Target, error) {
	if tenant != platformScope {
		return nil, ErrNotPlatform
	}
	cur, err := s.Repo.GetTarget(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.checkDest(ctx, cur.BaseURL+cur.Path); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEgress, err)
	}
	return s.Repo.MutateTarget(ctx, id, func(t *Target) (*OutEvent, error) {
		if t.Version != expected {
			return nil, ErrVersionConflict
		}
		if t.Status != TargetDraft && t.Status != TargetSuspended {
			return nil, ErrTransition
		}
		if t.Status == TargetDraft && a.PrincipalID == t.CreatedBy {
			return nil, ErrSelfAction
		}
		now := s.now()
		t.Status, t.ApprovedBy, t.ApprovedAt = TargetActive, a.PrincipalID, &now
		return &OutEvent{Type: "integration.handoff-target.activated", AggregateType: "handoff-target", AggregateID: t.TargetID, ActorID: a.PrincipalID,
			CorrelationID: a.CorrelationID, Data: map[string]any{"target_id": t.TargetID, "canonical_contract": t.CanonicalContract}}, nil
	})
}

// SetTargetStatus suspends or retires a target; deliveries for its contract wait (stay VALIDATED) until a target is
// active again.
func (s *Service) SetTargetStatus(ctx context.Context, tenant string, a Actor, id string, expected int64, status string) (*Target, error) {
	if tenant != platformScope {
		return nil, ErrNotPlatform
	}
	if status != TargetSuspended && status != TargetRetired {
		return nil, ErrBadRequest
	}
	return s.Repo.MutateTarget(ctx, id, func(t *Target) (*OutEvent, error) {
		if t.Version != expected {
			return nil, ErrVersionConflict
		}
		t.Status = status
		return &OutEvent{Type: "integration.handoff-target." + strings.ToLower(status), AggregateType: "handoff-target", AggregateID: t.TargetID, ActorID: a.PrincipalID,
			CorrelationID: a.CorrelationID, Data: map[string]any{"target_id": t.TargetID, "canonical_contract": t.CanonicalContract}}, nil
	})
}

const platformScope = "00000000-0000-0000-0000-00000000f001"

func (s *Service) checkDest(ctx context.Context, u string) (string, error) {
	if s.Handoff.Check != nil {
		return s.Handoff.Check(ctx, u)
	}
	return s.Handoff.Egress.CheckDestination(ctx, u)
}
