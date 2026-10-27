package rollout

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

var (
	nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,78}[A-Za-z0-9]$`)
	refRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@#+=-]{0,159}$`)
	hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// LegacyInput describes an ad-hoc connector found in a domain service.
type LegacyInput struct {
	Name          string `json:"name"`
	OwningService string `json:"owning_service"`
	Direction     string `json:"direction"`
	Description   string `json:"description,omitempty"`
}

// RegisterLegacy adds an ad-hoc connector to the migration inventory in DISCOVERED.
func (s *Service) RegisterLegacy(ctx context.Context, a Actor, in LegacyInput) (*Legacy, error) {
	in.Name, in.OwningService = strings.TrimSpace(in.Name), strings.TrimSpace(in.OwningService)
	in.Direction = strings.ToUpper(strings.TrimSpace(in.Direction))
	if !nameRE.MatchString(in.Name) {
		return nil, bad("name", "3-80 letters, digits, dot, dash or underscore")
	}
	if in.OwningService == "" || len(in.OwningService) > 80 {
		return nil, bad("owning_service", "required, at most 80")
	}
	if in.Direction != DirInbound && in.Direction != DirOutbound && in.Direction != DirBatch {
		return nil, bad("direction", "INBOUND, OUTBOUND or BATCH")
	}
	now := s.now()
	l := &Legacy{LegacyID: uuid.NewString(), TenantID: tenantOf(ctx), Name: in.Name, OwningService: in.OwningService, Direction: in.Direction,
		Description: truncate(in.Description, 1000), State: LegacyDiscovered, StateSince: now, CreatedBy: a.PrincipalID, Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.Repo.InsertLegacy(ctx, l, event(KindLegacy, l.LegacyID, "DISCOVERED", a, now, map[string]any{"owning_service": l.OwningService, "direction": l.Direction}),
		[]OutEvent{evt(a, "integration.legacy.discovered", "legacy-connector", l.LegacyID,
			map[string]any{"tenant_id": l.TenantID, "owning_service": l.OwningService, "direction": l.Direction})}); err != nil {
		return nil, err
	}
	s.observe("legacy", "discovered")
	return l, nil
}

// MapLegacy points a legacy connector at the registry connection that will replace it.
func (s *Service) MapLegacy(ctx context.Context, a Actor, id, connectionID string, version int64) (*Legacy, error) {
	conn, err := s.Conns.Connection(ctx, connectionID)
	if err != nil || conn.TenantID != tenantOf(ctx) {
		return nil, ErrNotFound // another tenant's connection is indistinguishable from a missing one
	}
	if conn.Status == xicconn.StatusRevoked || conn.Status == xicconn.StatusDraft {
		return nil, bad("connection_id", "the connection is "+strings.ToLower(conn.Status))
	}
	now := s.now()
	return s.moveLegacy(ctx, id, version, func(tx *LegacyTx) error {
		if tx.L.State != LegacyDiscovered && tx.L.State != LegacyMapped {
			return ErrState
		}
		from := tx.L.State
		tx.L.State, tx.L.StateSince, tx.L.ConnectionID = LegacyMapped, now, conn.ConnectionID
		tx.Event = append(tx.Event, event(KindLegacy, tx.L.LegacyID, "MAPPED", a, now, map[string]any{"from": from, "connection_id": conn.ConnectionID}))
		tx.Out = append(tx.Out, evt(a, "integration.legacy.mapped", "legacy-connector", tx.L.LegacyID,
			map[string]any{"tenant_id": tx.L.TenantID, "connection_id": conn.ConnectionID, "connector_id": conn.ConnectorID}))
		return nil
	})
}

func (s *Service) moveLegacy(ctx context.Context, id string, version int64, fn func(tx *LegacyTx) error) (*Legacy, error) {
	l, err := s.Repo.MutateLegacy(ctx, id, func(tx *LegacyTx) error {
		if version > 0 && tx.L.Version != version {
			return ErrVersionConflict
		}
		return fn(tx)
	})
	if err == nil {
		s.observe("legacy", "moved")
	}
	return l, err
}

// StartShadow begins shadow running: the owning service registers its attempts while the platform carries the same
// exchanges, and parity is measured. The connection must be live.
func (s *Service) StartShadow(ctx context.Context, a Actor, id string, version int64) (*Legacy, error) {
	l, err := s.Repo.GetLegacy(ctx, id)
	if err != nil {
		return nil, err
	}
	if l.State != LegacyMapped {
		return nil, ErrState
	}
	conn, err := s.Conns.Connection(ctx, l.ConnectionID)
	if err != nil {
		return nil, ErrNotFound
	}
	if !conn.Live() {
		return nil, bad("connection_id", "the mapped connection is "+strings.ToLower(conn.Status))
	}
	now := s.now()
	return s.moveLegacy(ctx, id, version, func(tx *LegacyTx) error {
		if tx.L.State != LegacyMapped {
			return ErrState
		}
		tx.L.State, tx.L.StateSince = LegacyShadow, now
		tx.Event = append(tx.Event, event(KindLegacy, tx.L.LegacyID, "SHADOW_STARTED", a, now, nil))
		tx.Out = append(tx.Out, evt(a, "integration.legacy.shadow-started", "legacy-connector", tx.L.LegacyID,
			map[string]any{"tenant_id": tx.L.TenantID, "connection_id": tx.L.ConnectionID}))
		return nil
	})
}

// StopShadow returns a shadowing connector to MAPPED. It is the safe direction and needs no approval.
func (s *Service) StopShadow(ctx context.Context, a Actor, id string, version int64) (*Legacy, error) {
	now := s.now()
	return s.moveLegacy(ctx, id, version, func(tx *LegacyTx) error {
		if tx.L.State != LegacyShadow {
			return ErrState
		}
		tx.L.State, tx.L.StateSince = LegacyMapped, now
		tx.Event = append(tx.Event, event(KindLegacy, tx.L.LegacyID, "SHADOW_STOPPED", a, now, nil))
		tx.Out = append(tx.Out, evt(a, "integration.legacy.shadow-stopped", "legacy-connector", tx.L.LegacyID, map[string]any{"tenant_id": tx.L.TenantID}))
		return nil
	})
}

// RevertCutover gives the connector back to the legacy path (CUTOVER -> SHADOW). Safe direction: no approval.
func (s *Service) RevertCutover(ctx context.Context, a Actor, id, reason string, version int64) (*Legacy, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, bad("reason", "required")
	}
	now := s.now()
	return s.moveLegacy(ctx, id, version, func(tx *LegacyTx) error {
		if tx.L.State != LegacyCutover {
			return ErrState
		}
		tx.L.State, tx.L.StateSince = LegacyShadow, now
		tx.Event = append(tx.Event, event(KindLegacy, tx.L.LegacyID, "CUTOVER_REVERTED", a, now, map[string]any{"reason": truncate(reason, 200)}))
		tx.Out = append(tx.Out, evt(a, "integration.legacy.cutover-reverted", "legacy-connector", tx.L.LegacyID,
			map[string]any{"tenant_id": tx.L.TenantID, "connection_id": tx.L.ConnectionID}))
		return nil
	})
}

// RegisterExchanges is how the owning service puts its attempts into the generic exchange history while the
// connector migrates. A registration is a reference, an outcome, a time and a hash of the content: never the
// content. It is append-only; the same registration again is a no-op, and the same reference with a different content
// hash is refused and changes nothing. Only the migrating states accept registrations: after cutover the legacy path
// is not supposed to send.
func (s *Service) RegisterExchanges(ctx context.Context, a Actor, id string, in []LegacyExchange) (stored, duplicates int, err error) {
	l, err := s.Repo.GetLegacy(ctx, id)
	if err != nil {
		return 0, 0, err
	}
	if l.State != LegacyMapped && l.State != LegacyShadow {
		return 0, 0, ErrState
	}
	if len(in) == 0 || len(in) > s.cfg().MaxPerCall {
		return 0, 0, bad("exchanges", fmt.Sprintf("between 1 and %d per call", s.cfg().MaxPerCall))
	}
	now := s.now()
	clean := make([]LegacyExchange, 0, len(in))
	for i, e := range in {
		at := fmt.Sprintf("exchanges[%d].", i)
		e.Outcome = strings.ToUpper(strings.TrimSpace(e.Outcome))
		e.PayloadHash = strings.ToLower(strings.TrimSpace(e.PayloadHash))
		if !refRE.MatchString(e.ExternalRef) {
			return 0, 0, bad(at+"external_ref", "1-160 characters of letters, digits and ._:/@#+=-")
		}
		if len(e.DomainRequestRef) > 128 || len(e.Operation) > 80 {
			return 0, 0, bad(at+"domain_request_ref", "too long")
		}
		switch e.Outcome {
		case OutPending, OutSettled, OutFailed, OutUnknown:
		default:
			return 0, 0, bad(at+"outcome", "PENDING, SETTLED, FAILED or UNKNOWN")
		}
		if !hashRE.MatchString(e.PayloadHash) {
			return 0, 0, bad(at+"payload_hash", "64 hex characters (sha-256 of the content)")
		}
		if e.OccurredAt.IsZero() || e.OccurredAt.After(now.Add(5*time.Minute)) || e.OccurredAt.Before(now.Add(-400*24*time.Hour)) {
			return 0, 0, bad(at+"occurred_at", "within the last 400 days and not in the future")
		}
		e.OccurredAt = e.OccurredAt.UTC()
		clean = append(clean, e)
	}
	stored, duplicates, err = s.Repo.RegisterExchanges(ctx, id, clean, a.PrincipalID, now)
	if err == nil {
		s.observe("legacy", "registered")
	}
	return stored, duplicates, err
}

// ParityInput is the window of a parity run.
type ParityInput struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// RunParity compares what the owning service registered in a window with what the platform holds for the mapped
// connection. It reads both sides and writes only its own record. A run passes when it compared enough exchanges
// and nothing is missing from the platform or differs in outcome; exchanges only the platform has are reported,
// not failed (the platform may already carry more than the legacy path did).
func (s *Service) RunParity(ctx context.Context, a Actor, id string, in ParityInput) (*ParityRun, error) {
	l, err := s.Repo.GetLegacy(ctx, id)
	if err != nil {
		return nil, err
	}
	if l.State != LegacyShadow {
		return nil, ErrState
	}
	if in.From.IsZero() || in.To.IsZero() || !in.From.Before(in.To) || in.To.Sub(in.From) > 31*24*time.Hour {
		return nil, bad("from", "a window of at most 31 days, from before to")
	}
	if in.To.After(s.now().Add(time.Minute)) {
		return nil, bad("to", "not in the future")
	}
	const limit = 5000
	reg, err := s.Repo.LatestExchanges(ctx, id, in.From, in.To, limit+1)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	if len(reg) > limit {
		return nil, bad("from", "the window holds more than 5000 registered exchanges: use a shorter one")
	}
	refKey := func(e LegacyExchange) string {
		if l.Direction == DirOutbound && e.DomainRequestRef != "" {
			return e.DomainRequestRef
		}
		return e.ExternalRef
	}
	p := &ParityRun{ParityID: uuid.NewString(), TenantID: l.TenantID, LegacyID: l.LegacyID, WindowFrom: in.From.UTC(), WindowTo: in.To.UTC(), RunBy: a.PrincipalID, CreatedAt: s.now()}
	type sample struct {
		Ref    string `json:"ref"`
		Kind   string `json:"kind"`
		Legacy string `json:"legacy,omitempty"`
		XIC    string `json:"xic,omitempty"`
	}
	samples := []sample{}
	note := func(sm sample) {
		if len(samples) < 20 {
			samples = append(samples, sm)
		}
	}
	p.Compared = len(reg)
	refs := make([]string, 0, len(reg))
	for _, e := range reg {
		refs = append(refs, refKey(e))
	}
	switch l.Direction {
	case DirInbound:
		n, err := s.Repo.InboundCount(ctx, l.ConnectionID, in.From, in.To)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependency, err)
		}
		p.Matched = min(len(reg), n)
		p.MissingInXIC = max(0, len(reg)-n)
		p.XICOnly = max(0, n-len(reg))
		if p.MissingInXIC > 0 {
			note(sample{Ref: "count", Kind: "MISSING_IN_XIC", Legacy: fmt.Sprint(len(reg)), XIC: fmt.Sprint(n)})
		}
	default:
		have, err := s.Repo.Counterparts(ctx, l.Direction, l.ConnectionID, refs)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependency, err)
		}
		for _, e := range reg {
			st, ok := have[refKey(e)]
			switch {
			case !ok:
				p.MissingInXIC++
				note(sample{Ref: refKey(e), Kind: "MISSING_IN_XIC", Legacy: e.Outcome})
			case l.Direction == DirOutbound && !sameOutcome(e.Outcome, st):
				p.OutcomeDiffers++
				note(sample{Ref: refKey(e), Kind: "OUTCOME_DIFFERS", Legacy: e.Outcome, XIC: st})
			default:
				p.Matched++
			}
		}
		p.XICOnly, err = s.Repo.XICOnly(ctx, l.Direction, l.ConnectionID, in.From, in.To, refs)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependency, err)
		}
	}
	p.Samples, _ = json.Marshal(samples)
	p.Passed = p.Compared >= s.cfg().ParityMinSamples && p.MissingInXIC == 0 && p.OutcomeDiffers == 0
	ev := event(KindLegacy, l.LegacyID, "PARITY_RUN", a, p.CreatedAt, map[string]any{"parity_id": p.ParityID, "compared": p.Compared, "passed": p.Passed})
	if err := s.Repo.InsertParity(ctx, p, ev, []OutEvent{evt(a, "integration.legacy.parity-run", "legacy-connector", l.LegacyID,
		map[string]any{"tenant_id": l.TenantID, "connection_id": l.ConnectionID, "parity_id": p.ParityID, "compared": p.Compared, "matched": p.Matched,
			"missing_in_xic": p.MissingInXIC, "outcome_differs": p.OutcomeDiffers, "passed": p.Passed})}); err != nil {
		return nil, err
	}
	s.observe("legacy", "parity_run")
	return p, nil
}

// sameOutcome compares a legacy outcome with an attempt state of the platform.
func sameOutcome(legacy, attempt string) bool {
	switch legacy {
	case OutSettled:
		return attempt == "SETTLED"
	case OutFailed:
		return attempt == "FAILED"
	case OutUnknown:
		return attempt == "UNKNOWN"
	case OutPending:
		return attempt == "PENDING" || attempt == "SUBMITTING" || attempt == "RESERVED"
	}
	return false
}

// legacyReadiness is the evidence for CUTOVER or RETIRED.
func (s *Service) legacyReadiness(ctx context.Context, l *Legacy, to string) (*Readiness, error) {
	conn, err := s.Conns.Connection(ctx, l.ConnectionID)
	if err != nil {
		return nil, ErrNotFound
	}
	now := s.now()
	var checks []Check
	add := func(name string, pass bool, detail string) {
		checks = append(checks, Check{Name: name, Pass: pass, Detail: detail})
	}
	add("connection_active", conn.Status == xicconn.StatusActive, "the connection is "+strings.ToLower(conn.Status))
	if conn.Environment == xicconn.EnvProduction {
		cs, err := s.Conns.CertificationStatusOf(ctx, conn)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependency, err)
		}
		add("certification_valid", cs.State == xicconn.CertValid, "certification is "+strings.ToLower(cs.State))
		if ro, err := s.Repo.LatestRolloutFor(ctx, conn.ConnectionID); err == nil {
			add("rollout_general", ro.Stage == StageGeneral, "the rollout is "+strings.ToLower(ro.Stage))
		}
	}
	high, err := s.Repo.OpenHighVariances(ctx, conn.ConnectionID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	add("no_open_high_variance", high == 0, fmt.Sprintf("%d open HIGH variances", high))
	over, err := s.Repo.OverdueUnknown(ctx, conn.ConnectionID, now.Add(-s.cfg().UnknownSLA))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	add("no_overdue_unknown", over == 0, fmt.Sprintf("%d UNKNOWN attempts past the escalation limit", over))
	switch to {
	case LegacyCutover:
		soak := now.Sub(l.StateSince)
		add("shadow_soaked", soak >= time.Duration(s.cfg().ShadowMinHours)*time.Hour, fmt.Sprintf("%dh of %dh", int(soak.Hours()), s.cfg().ShadowMinHours))
		runs, err := s.Repo.ListParity(ctx, l.LegacyID, 1)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependency, err)
		}
		switch {
		case len(runs) == 0:
			add("parity_passed", false, "no parity run")
		case !runs[0].CreatedAt.After(l.StateSince):
			add("parity_passed", false, "the latest parity run is from before shadowing began")
		case now.Sub(runs[0].CreatedAt) > s.cfg().ParityMaxAge:
			add("parity_passed", false, "the latest parity run is too old")
		default:
			add("parity_passed", runs[0].Passed, fmt.Sprintf("%d compared, %d missing, %d differ", runs[0].Compared, runs[0].MissingInXIC, runs[0].OutcomeDiffers))
		}
	case LegacyRetired:
		soak := now.Sub(l.StateSince)
		add("cutover_soaked", soak >= time.Duration(s.cfg().RetireAfterHours)*time.Hour, fmt.Sprintf("%dh of %dh", int(soak.Hours()), s.cfg().RetireAfterHours))
	}
	rep := &Readiness{Ready: true, Checks: checks}
	for _, c := range checks {
		if !c.Pass {
			rep.Ready = false
		}
	}
	return rep, nil
}

// LegacyReadiness reports the evidence for the next approval step of a legacy connector (CUTOVER from SHADOW,
// RETIRED from CUTOVER).
func (s *Service) LegacyReadiness(ctx context.Context, id string) (*Readiness, string, error) {
	l, err := s.Repo.GetLegacy(ctx, id)
	if err != nil {
		return nil, "", err
	}
	to := ""
	switch l.State {
	case LegacyShadow:
		to = LegacyCutover
	case LegacyCutover:
		to = LegacyRetired
	default:
		return nil, "", ErrState
	}
	rep, err := s.legacyReadiness(ctx, l, to)
	return rep, to, err
}

// RequestLegacyAdvance asks for CUTOVER (from SHADOW) or RETIRED (from CUTOVER).
func (s *Service) RequestLegacyAdvance(ctx context.Context, a Actor, id, reason string) (*Request, error) {
	l, err := s.Repo.GetLegacy(ctx, id)
	if err != nil {
		return nil, err
	}
	rep, to, err := s.LegacyReadiness(ctx, id)
	if err != nil {
		return nil, err
	}
	if !rep.Ready {
		s.observe("legacy", "not_ready")
		return nil, &NotReadyError{Report: *rep}
	}
	now := s.now()
	rb, _ := json.Marshal(rep)
	q := &Request{RequestID: uuid.NewString(), TenantID: l.TenantID, SubjectKind: KindLegacy, SubjectID: l.LegacyID, FromState: l.State, ToState: to,
		Status: ReqPending, Attestations: []string{}, Readiness: rb, Reason: truncate(reason, 1000), RequestedBy: a.PrincipalID,
		ExpiresAt: now.Add(s.cfg().RequestTTL), CreatedAt: now, Version: 1}
	if err := s.Repo.InsertRequest(ctx, q, event(KindLegacy, l.LegacyID, "ADVANCE_REQUESTED", a, now, map[string]any{"to": to, "request_id": q.RequestID}),
		[]OutEvent{evt(a, "integration.legacy.cutover-requested", "legacy-connector", l.LegacyID,
			map[string]any{"tenant_id": l.TenantID, "connection_id": l.ConnectionID, "to": to, "request_id": q.RequestID})}); err != nil {
		return nil, err
	}
	s.observe("legacy", "advance_requested")
	return q, nil
}
