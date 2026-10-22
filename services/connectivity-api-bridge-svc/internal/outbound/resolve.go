package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/adapter/ext"
)

// ResolveInput asks for an UNKNOWN or PENDING attempt to be resolved. Method defaults to STATUS_QUERY: the engine
// asks the provider, through the contract's registered status operation, what happened to the ORIGINAL attempt. A
// MANUAL resolution records an operator's finding from reconciliation evidence and is made by someone other than
// whoever reserved the attempt.
type ResolveInput struct {
	Method      string `json:"method,omitempty"`
	Outcome     string `json:"outcome,omitempty"`      // MANUAL only: SETTLED or FAILED
	EvidenceRef string `json:"evidence_ref,omitempty"` // MANUAL only: the reconciliation or provider evidence
	Reason      string `json:"reason,omitempty"`       // MANUAL only
}

// statusVocabulary is how a provider's status text is read. Anything outside it leaves the attempt as it was: the
// engine never guesses an outcome from words it does not know.
var (
	settledWords = map[string]bool{"SETTLED": true, "COMPLETED": true, "SUCCEEDED": true, "SUCCESS": true, "PROCESSED": true, "PAID": true}
	failedWords  = map[string]bool{"FAILED": true, "REJECTED": true, "DECLINED": true, "CANCELLED": true, "CANCELED": true, "EXPIRED": true, "ERROR": true}
	pendingWords = map[string]bool{"PENDING": true, "PROCESSING": true, "IN_PROGRESS": true, "ACCEPTED": true, "SUBMITTED": true, "QUEUED": true}
)

// readStatus interprets a status-operation answer.
func readStatus(body []byte) (state, text, ref string) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return "", "", ""
	}
	t, _ := m["status"].(string)
	t = strings.ToUpper(strings.TrimSpace(t))
	ref = providerRef(body)
	switch {
	case settledWords[t]:
		return StateSettled, t, ref
	case failedWords[t]:
		return StateFailed, t, ref
	case pendingWords[t]:
		return StatePending, t, ref
	}
	return "", t, ref
}

// Resolve resolves an UNKNOWN or PENDING attempt (§9.1). It never submits again.
func (s *Service) Resolve(ctx context.Context, actor Actor, id string, in ResolveInput) (*Attempt, error) {
	a, err := s.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !a.Open() {
		return a, nil
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	switch method {
	case "", MethodStatusQuery:
		return s.statusQuery(ctx, actor, a)
	case MethodManual:
		return s.resolveManual(ctx, actor, a, in)
	}
	return nil, fmt.Errorf("%w: method", ErrBadRequest)
}

func (s *Service) resolveManual(ctx context.Context, actor Actor, a *Attempt, in ResolveInput) (*Attempt, error) {
	outcome := strings.ToUpper(strings.TrimSpace(in.Outcome))
	if (outcome != StateSettled && outcome != StateFailed) || strings.TrimSpace(in.EvidenceRef) == "" || len(in.EvidenceRef) > 255 ||
		strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 255 {
		return nil, fmt.Errorf("%w: outcome, evidence_ref and reason are required", ErrBadRequest)
	}
	if actor.PrincipalID == a.RequestedBy {
		return nil, ErrSelfAction
	}
	if a.State != StateUnknown && a.State != StatePending {
		return nil, ErrTransition
	}
	return s.toFinal(ctx, a, outcome, MethodManual, "MANUAL_"+outcome, 0, "", actor.PrincipalID, in.EvidenceRef, s.now())
}

// statusQuery asks the provider about the original attempt. The query is a read through the contract's registered
// status operation, on the same governed endpoint and credential rules as a submission.
func (s *Service) statusQuery(ctx context.Context, actor Actor, a *Attempt) (*Attempt, error) {
	now := s.now()
	if a.State == StateSubmitting {
		if a.NextResolutionAt != nil && a.NextResolutionAt.After(now) {
			return nil, ErrTransition // still inside its lease: the call may be in flight
		}
		recovered, err := s.toUnknown(ctx, a, ReasonCrashRecovery, 0, "", true, now)
		if err != nil {
			return nil, err
		}
		a = recovered
	}
	if a.State == StateReserved {
		return nil, ErrTransition
	}
	conn, err := s.Conns.Connection(ctx, a.ConnectionID)
	if err != nil {
		return nil, err
	}
	v, err := s.Conns.PinnedContract(ctx, conn)
	if err != nil {
		return nil, err
	}
	orig := v.Operation(a.OperationID)
	if orig == nil || orig.UnknownResolution.StatusQueryOperation == "" {
		return nil, ErrNotResolvable
	}
	statusOp := orig.UnknownResolution.StatusQueryOperation
	q := url.Values{}
	if a.ProviderReference != "" {
		q.Set("reference", a.ProviderReference)
	}
	q.Set("client_ref", a.DomainRequestRef)
	p, err := s.prepare(ctx, a, statusOp, q)
	if err != nil {
		s.recordBlocked(ctx, a, err)
		return nil, err
	}
	defer p.wipe()

	r := s.call(ctx, p, a, dflt(p.op.Method, http.MethodGet), nil)
	rctx := context.WithoutCancel(ctx)
	return s.applyStatus(rctx, a, p, r, now)
}

func (s *Service) applyStatus(ctx context.Context, a *Attempt, p *prepared, r response, now time.Time) (*Attempt, error) {
	if r.err != nil || r.status < 200 || r.status >= 300 {
		reason := ReasonUnclassified
		switch {
		case r.redirectBlocked:
			reason = ReasonRedirectRefused
		case r.err != nil:
			reason = ReasonConnectionLost
		case r.status == http.StatusNotFound:
			reason = ReasonStatusNotFound // the provider not knowing it is not proof that it did not act
		case r.status >= 500:
			reason = ReasonProviderUnavail
		case r.status == http.StatusTooManyRequests:
			reason = ReasonRateLimited
		}
		return s.queryFailed(ctx, a, reason, r.status, now)
	}
	if p.op.ResponseSchema != "" {
		if sch := p.v.SchemaByRef(p.op.ResponseSchema); sch != nil {
			if compiled, err := adapter.CompileSchema(sch.ValidationProfile); err != nil || compiled.ValidateDocument(r.body) != nil {
				return s.queryFailed(ctx, a, ReasonStatusUnrecognised, r.status, now)
			}
		}
	}
	state, text, ref := readStatus(r.body)
	switch state {
	case StateSettled, StateFailed:
		reason := ""
		if state == StateFailed {
			reason = ReasonProviderRejected
		}
		return s.toFinal(ctx, a, state, MethodStatusQuery, reason, r.status, ref, "", "", now)
	case StatePending:
		return s.queryPending(ctx, a, r.status, ref, now)
	}
	_ = text
	return s.queryFailed(ctx, a, ReasonStatusUnrecognised, r.status, now)
}

// queryFailed records a status query that did not settle the question. The attempt keeps its state and is queried
// again on a longer backoff; it is never marked failed to clear a queue.
func (s *Service) queryFailed(ctx context.Context, a *Attempt, reason string, status int, now time.Time) (*Attempt, error) {
	return s.mutate(ctx, a.AttemptID, func(tx *Tx) error {
		if tx.A.State != StateUnknown && tx.A.State != StatePending {
			return errNoChange
		}
		tx.A.ResolveCount++
		tx.A.NextResolutionAt = s.nextResolution(tx.A, true, now)
		if tx.A.State == StateUnknown {
			tx.A.AmbiguityReason = reason
		}
		tx.Event = &Event{Kind: "RESOLUTION_ATTEMPT", From: tx.A.State, To: tx.A.State, Actor: "system:xic-outbound", At: now,
			Detail: mustJSON(map[string]any{"result": reason, "provider_status": status, "resolve_count": tx.A.ResolveCount})}
		s.observe("resolution_inconclusive", reason)
		return nil
	})
}

func (s *Service) queryPending(ctx context.Context, a *Attempt, status int, ref string, now time.Time) (*Attempt, error) {
	return s.mutate(ctx, a.AttemptID, func(tx *Tx) error {
		if tx.A.State != StateUnknown && tx.A.State != StatePending {
			return errNoChange
		}
		from := tx.A.State
		tx.A.ResolveCount++
		tx.A.State, tx.A.AmbiguityReason, tx.A.ProviderStatus = StatePending, "", status
		if ref != "" && tx.A.ProviderReference == "" {
			tx.A.ProviderReference = ref
		}
		tx.A.NextResolutionAt = s.nextResolution(tx.A, true, now)
		tx.Event = &Event{Kind: "RESOLUTION_ATTEMPT", From: from, To: StatePending, Actor: "system:xic-outbound", At: now,
			Detail: mustJSON(map[string]any{"result": "PENDING", "provider_status": status, "resolve_count": tx.A.ResolveCount})}
		if from == StateUnknown {
			tx.Out = append(tx.Out, OutEvent{Type: "integration.outbound.pending", AggregateType: "outbound-attempt", AggregateID: tx.A.AttemptID, ActorID: "system:xic-outbound",
				CorrelationID: tx.A.CorrelationID, Data: map[string]any{"attempt_id": tx.A.AttemptID, "tenant_id": tx.A.TenantID, "connection_id": tx.A.ConnectionID, "provider_reference": tx.A.ProviderReference}})
		}
		return nil
	})
}

// ── the resolver worker ──────────────────────────────────────────────────────

// DriveDue resolves every attempt the store says is due and escalates those open too long. It never submits. It
// returns how many it worked on; one attempt failing does not stop the others.
func (s *Service) DriveDue(ctx context.Context, limit int) (int, error) {
	now := s.now()
	due, err := s.Repo.DueForResolution(ctx, now, now.Add(-s.cfg().MaxUnknownAge), limit)
	if err != nil {
		return 0, err
	}
	var first error
	for _, d := range due {
		tctx := withTenant(ctx, d.TenantID)
		a, err := s.Repo.Get(tctx, d.AttemptID)
		if err != nil {
			if first == nil && !errors.Is(err, ErrNotFound) {
				first = err
			}
			continue
		}
		s.escalate(tctx, a, now)
		if _, err := s.Resolve(tctx, Actor{PrincipalID: "system:xic-outbound-resolver"}, d.AttemptID, ResolveInput{}); err != nil && first == nil && !expected(err) {
			first = err
		}
	}
	return len(due), first
}

// expected errors are the normal reasons a query cannot run right now; they are recorded as evidence, not alarms.
func expected(err error) bool {
	return errors.Is(err, ErrTransition) || errors.Is(err, ErrNotResolvable) || errors.Is(err, ErrBlocked) || errors.Is(err, ErrAuthUnsupported)
}

// escalate raises, once, an attempt that has stayed open longer than the maximum UNKNOWN age (OD-13): it needs a
// case or an incident, not another retry.
func (s *Service) escalate(ctx context.Context, a *Attempt, now time.Time) {
	if a.EscalatedAt != nil || (a.State != StateUnknown && a.State != StatePending) || a.LastSubmitAt == nil ||
		a.LastSubmitAt.After(now.Add(-s.cfg().MaxUnknownAge)) {
		return
	}
	_, _ = s.mutate(ctx, a.AttemptID, func(tx *Tx) error {
		if tx.A.EscalatedAt != nil || (tx.A.State != StateUnknown && tx.A.State != StatePending) {
			return errNoChange
		}
		tx.A.EscalatedAt = &now
		tx.Event = &Event{Kind: "ESCALATED", From: tx.A.State, To: tx.A.State, Actor: "system:xic-outbound-resolver", At: now,
			Detail: mustJSON(map[string]any{"state": tx.A.State, "ambiguity_reason": tx.A.AmbiguityReason})}
		tx.Out = append(tx.Out, OutEvent{Type: "integration.outbound.unknown-escalated", AggregateType: "outbound-attempt", AggregateID: tx.A.AttemptID, ActorID: "system:xic-outbound-resolver",
			CorrelationID: tx.A.CorrelationID, Data: map[string]any{"attempt_id": tx.A.AttemptID, "tenant_id": tx.A.TenantID, "connection_id": tx.A.ConnectionID,
				"state": tx.A.State, "ambiguity_reason": tx.A.AmbiguityReason}})
		return nil
	})
	s.observe("escalated", a.State)
}

// RunResolver drives due attempts every interval and purges bodies that are no longer needed, until ctx ends.
func (s *Service) RunResolver(ctx context.Context, interval time.Duration, batch int, report func(n int, err error)) {
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
			if _, perr := s.Repo.PurgeBodies(ctx, s.now(), batch); perr != nil && err == nil {
				err = perr
			}
			if report != nil {
				report(n, err)
			}
		}
	}
}

// VaultSecrets releases credential material through the vault's narrow "use" operation: the secret is held for the
// one call and zeroed by the caller. A refused or unknown reference is a refusal; an outage is retryable.
type VaultSecrets struct {
	Vault     *ext.VaultHTTP
	Principal string
}

// NewVaultSecrets builds the adapter for the vault at base, acting as principal.
func NewVaultSecrets(base, principal string) *VaultSecrets {
	return &VaultSecrets{Vault: ext.NewVaultHTTP(base, principal), Principal: principal}
}

// Secret implements Secrets.
func (v *VaultSecrets) Secret(ctx context.Context, tenantID, credentialRef string) ([]byte, error) {
	cred, err := v.Vault.Use(ctx, credentialRef, ext.Caller{TenantID: tenantID, PrincipalID: v.Principal})
	switch {
	case err == nil:
		return cred.Secret, nil
	case errors.Is(err, ext.ErrCredentialDown):
		return nil, ErrSecretUnavailable
	default:
		return nil, ErrBlocked
	}
}

var (
	_ Secrets = (*VaultSecrets)(nil)
)
