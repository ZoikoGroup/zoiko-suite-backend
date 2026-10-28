package recon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// Connections is what reconciliation needs of the connection layer.
type Connections interface {
	Connection(ctx context.Context, id string) (*xicconn.Connection, error)
}

// Connectors resolves a connector's family, which picks the policy. *registry.MemRepo and the PG registry repo satisfy it.
type Connectors interface {
	GetConnector(ctx context.Context, id string) (*registry.Connector, error)
}

// LiveSource lists live connections across tenants for the scheduler.
type LiveSource interface {
	LiveRefs(ctx context.Context, limit int) ([]xicconn.DueRef, error)
}

// Config holds the service's limits.
type Config struct {
	MaxPeriod      time.Duration // the widest period one run covers
	Lease          time.Duration // a run being evaluated is left alone this long
	ScheduleEvery  time.Duration // how often the scheduler looks for connections that are due
	MaxExternalRef int
}

// DefaultConfig returns safe defaults.
func DefaultConfig() Config {
	return Config{MaxPeriod: 31 * 24 * time.Hour, Lease: 5 * time.Minute, ScheduleEvery: time.Minute, MaxExternalRef: 10000}
}

// Service is the reconciliation engine.
type Service struct {
	Repo       Repo
	Conns      Connections
	Connectors Connectors
	Live       LiveSource
	Policies   map[string]Policy
	Cfg        Config
	Clock      func() time.Time
	// Observe receives a short reason for every outcome, for metrics. It must not block.
	Observe func(kind, reason string)

	schedMu   sync.Mutex
	lastSched time.Time
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) cfg() Config {
	if s.Cfg.MaxPeriod == 0 {
		return DefaultConfig()
	}
	return s.Cfg
}

func (s *Service) observe(kind, reason string) {
	if s.Observe != nil {
		s.Observe(kind, reason)
	}
}

var (
	currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)
	amountRE   = regexp.MustCompile(`^-?[0-9]{1,18}(\.[0-9]{1,6})?$`)
	countNames = map[string]bool{"received": true, "accepted": true, "rejected": true, "quarantined": true, "duplicate": true,
		"submitted": true, "settled": true, "failed": true, "unknown": true, "pending": true, "cancelled": true}
)

// RunRequest asks for a reconciliation run.
type RunRequest struct {
	ConnectionID string    `json:"connection_id"`
	PeriodFrom   time.Time `json:"period_from"`
	PeriodTo     time.Time `json:"period_to"`
	Basis        []string  `json:"basis"`
	External     *External `json:"external,omitempty"`
}

func sha(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Service) familyOf(ctx context.Context, connectorID string) string {
	if s.Connectors != nil {
		if c, err := s.Connectors.GetConnector(ctx, connectorID); err == nil && c.Family != "" {
			return c.Family
		}
	}
	return "API"
}

func (s *Service) validate(req RunRequest, now time.Time) (basis []string, err error) {
	cfg := s.cfg()
	if req.PeriodFrom.IsZero() || req.PeriodTo.IsZero() || !req.PeriodFrom.Before(req.PeriodTo) {
		return nil, fmt.Errorf("%w: a period has a start before its end", ErrBadRequest)
	}
	if req.PeriodTo.After(now.Add(time.Minute)) {
		return nil, fmt.Errorf("%w: a period cannot end in the future", ErrBadRequest)
	}
	if req.PeriodTo.Sub(req.PeriodFrom) > cfg.MaxPeriod {
		return nil, fmt.Errorf("%w: a run covers at most %s", ErrBadRequest, cfg.MaxPeriod)
	}
	seen := map[string]bool{}
	for _, b := range req.Basis {
		b = strings.ToUpper(strings.TrimSpace(b))
		if !contains(Bases, b) {
			return nil, fmt.Errorf("%w: unknown basis %q", ErrBadRequest, b)
		}
		if !seen[b] {
			seen[b] = true
			basis = append(basis, b)
		}
	}
	if len(basis) == 0 {
		return nil, fmt.Errorf("%w: name what to reconcile (basis)", ErrBadRequest)
	}
	sort.Strings(basis)
	ext := req.External
	need := func(b string, ok bool) error {
		if seen[b] && !ok {
			return fmt.Errorf("%w (%s)", ErrNotIndependent, b)
		}
		return nil
	}
	if ext != nil && !contains(ExternalSources, ext.Source) {
		return nil, fmt.Errorf("%w: the source of a declaration is one of %s", ErrNotIndependent, strings.Join(ExternalSources, ", "))
	}
	for _, c := range []struct {
		b  string
		ok bool
	}{{BasisCount, ext != nil && len(ext.Counts) > 0}, {BasisReference, ext != nil && len(ext.References) > 0},
		{BasisAmount, ext != nil && ext.TotalAmount != ""}, {BasisTime, ext != nil && ext.Cutoff != nil}} {
		if err := need(c.b, c.ok); err != nil {
			return nil, err
		}
	}
	if ext != nil {
		for n, v := range ext.Counts {
			if !countNames[n] || v < 0 {
				return nil, fmt.Errorf("%w: a declared count is one of the known names and not negative", ErrBadRequest)
			}
		}
		if len(ext.References) > cfg.MaxExternalRef {
			return nil, fmt.Errorf("%w: at most %d references", ErrBadRequest, cfg.MaxExternalRef)
		}
		for _, r := range ext.References {
			if r == "" || len(r) > 128 {
				return nil, fmt.Errorf("%w: a reference is 1 to 128 characters", ErrBadRequest)
			}
		}
		if ext.TotalAmount != "" && (!amountRE.MatchString(ext.TotalAmount) || !currencyRE.MatchString(ext.Currency)) {
			return nil, fmt.Errorf("%w: a total is a decimal with a three-letter currency", ErrBadRequest)
		}
		if ext.Cutoff != nil && (ext.Cutoff.Before(req.PeriodFrom) || ext.Cutoff.After(req.PeriodTo)) {
			return nil, fmt.Errorf("%w: the cutoff lies inside the period", ErrBadRequest)
		}
	}
	return basis, nil
}

// RequestRun records a reconciliation run. It returns the run and whether it already existed: the same request is the
// same run, so a retry never starts a second one.
func (s *Service) RequestRun(ctx context.Context, a Actor, req RunRequest) (*Run, bool, error) {
	tenant := middleware.GetTenantID(ctx)
	now := s.now()
	basis, err := s.validate(req, now)
	if err != nil {
		return nil, false, err
	}
	if a.PrincipalID == "" {
		return nil, false, fmt.Errorf("%w: who is asking", ErrBadRequest)
	}
	conn, err := s.Conns.Connection(ctx, req.ConnectionID)
	if err != nil || conn.TenantID != tenant {
		if err == nil { // NP-52: another tenant's object in a reconcile request is refused and raised as a boundary signal
			_ = s.Repo.RecordSignal(withTenant(ctx, tenant), tenant, OutEvent{Type: "integration.security.tenant-scope-mismatch", AggregateType: "connection",
				AggregateID: req.ConnectionID, ActorID: a.PrincipalID, Data: map[string]any{"tenant_id": tenant, "connection_id": req.ConnectionID, "reason_code": "XIC-008", "surface": "reconciliation"}})
			s.observe("run_refused", "TENANT_SCOPE")
		}
		return nil, false, ErrNotFound
	}
	family := s.familyOf(ctx, conn.ConnectorID)
	r := &Run{RunID: uuid.NewString(), TenantID: tenant, ConnectionID: conn.ConnectionID, ConnectorID: conn.ConnectorID, Family: family, Origin: OriginRequested,
		PeriodFrom: req.PeriodFrom.UTC(), PeriodTo: req.PeriodTo.UTC(), Basis: basis, State: RunOpen, RequestedBy: a.PrincipalID, CorrelationID: a.CorrelationID,
		Version: 1, CreatedAt: now, UpdatedAt: now, NextEvaluateAt: &now, AwaitingDomain: contains(basis, BasisAmount)}
	extJSON := ""
	if req.External != nil {
		e := *req.External
		sort.Strings(e.References)
		raw := mustJSON(e)
		r.External, r.ExternalSource, r.ExternalSuppliedBy = raw, e.Source, a.PrincipalID
		extJSON = string(raw)
	}
	r.DedupeKey = sha("run", tenant, conn.ConnectionID, r.PeriodFrom.Format(time.RFC3339), r.PeriodTo.Format(time.RFC3339), strings.Join(basis, ","), extJSON)
	ev := Event{Kind: "REQUESTED", Actor: a.PrincipalID, At: now, Detail: mustJSON(map[string]any{"basis": basis, "period_from": r.PeriodFrom, "period_to": r.PeriodTo, "external_source": r.ExternalSource})}
	held, existing, err := s.Repo.InsertRun(withTenant(ctx, tenant), r, ev, []OutEvent{s.runEvent("integration.reconciliation.requested", r, a.PrincipalID, nil)})
	if err != nil {
		return nil, false, err
	}
	if existing {
		return held, true, nil
	}
	s.observe("run_requested", r.Origin)
	return r, false, nil
}

func (s *Service) runEvent(typ string, r *Run, actor string, extra map[string]any) OutEvent {
	d := map[string]any{"run_id": r.RunID, "connection_id": r.ConnectionID, "connector_id": r.ConnectorID, "state": r.State, "origin": r.Origin,
		"basis": r.Basis, "period_from": r.PeriodFrom, "period_to": r.PeriodTo}
	for k, v := range extra {
		d[k] = v
	}
	return OutEvent{Type: typ, AggregateType: "reconciliation-run", AggregateID: r.RunID, ActorID: actor, CorrelationID: r.CorrelationID, Data: d}
}

func (s *Service) varianceEvent(typ string, v *Variance, actor string, extra map[string]any) OutEvent {
	var refs map[string]any
	_ = json.Unmarshal(v.Refs, &refs)
	d := map[string]any{"run_id": v.RunID, "variance_id": v.VarianceID, "connection_id": v.ConnectionID, "variance_type": v.Type, "severity": v.Severity,
		"state": v.State, "repeat_count": v.RepeatCount, "refs": refs}
	for k, x := range extra {
		d[k] = x
	}
	return OutEvent{Type: typ, AggregateType: "reconciliation-variance", AggregateID: v.VarianceID, ActorID: actor, Data: d}
}

// SupplyDomainTotals records the owning domain's side of an amount comparison. It is the domain service's step: the
// person who supplied the provider's declaration cannot supply the domain's (I-25), and the totals are supplied once.
func (s *Service) SupplyDomainTotals(ctx context.Context, a Actor, runID string, t DomainTotals) (*Run, error) {
	if !amountRE.MatchString(t.TotalAmount) || !currencyRE.MatchString(t.Currency) {
		return nil, fmt.Errorf("%w: a total is a decimal with a three-letter currency", ErrBadRequest)
	}
	if a.PrincipalID == "" {
		return nil, fmt.Errorf("%w: who is supplying", ErrBadRequest)
	}
	return s.Repo.MutateRun(ctx, runID, func(tx *RunTx) error {
		r := tx.Run
		switch {
		case r.State != RunOpen || !r.AwaitingDomain:
			return fmt.Errorf("%w: this run is not waiting for the domain's totals", ErrState)
		case a.PrincipalID == r.ExternalSuppliedBy || a.PrincipalID == r.RequestedBy:
			return ErrSelfAction
		}
		now := s.now()
		r.DomainTotals, r.DomainTotalsBy, r.AwaitingDomain, r.NextEvaluateAt = mustJSON(t), a.PrincipalID, false, &now
		tx.Event = append(tx.Event, Event{Kind: "DOMAIN_TOTALS_SUPPLIED", Actor: a.PrincipalID, At: now, Detail: mustJSON(map[string]any{"currency": t.Currency, "reference": t.Reference})})
		return nil
	})
}

// Evaluate compares one run. It claims the run with a lease, reads the platform's records, and stores the outcome and
// every variance in one transaction. It writes nothing to any exchange, attempt, receipt or batch.
func (s *Service) Evaluate(ctx context.Context, tenant, runID string) (*Run, error) {
	ctx = withTenant(ctx, tenant)
	now := s.now()
	var claimed *Run
	if _, err := s.Repo.MutateRun(ctx, runID, func(tx *RunTx) error {
		r := tx.Run
		if r.State != RunOpen || r.AwaitingDomain || (r.LeaseUntil != nil && r.LeaseUntil.After(now)) {
			return nil
		}
		l := now.Add(s.cfg().Lease)
		r.LeaseUntil = &l
		c := *r
		claimed = &c
		return nil
	}); err != nil {
		return nil, err
	}
	if claimed == nil {
		return s.Repo.GetRun(ctx, runID)
	}
	var ext *External
	var dom *DomainTotals
	if len(claimed.External) > 0 {
		ext = &External{}
		_ = json.Unmarshal(claimed.External, ext)
	}
	if len(claimed.DomainTotals) > 0 {
		dom = &DomainTotals{}
		_ = json.Unmarshal(claimed.DomainTotals, dom)
	}
	pol := s.PolicyFor(claimed.Family)
	res, err := compare(ctx, s.Repo, pol, claimed, ext, dom, now)
	if err != nil {
		s.release(ctx, runID)
		s.observe("run_failed", "COMPARE")
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	vs := make([]Variance, 0, len(res.Findings))
	high, escalate := 0, false
	for _, f := range res.Findings {
		v := Variance{VarianceID: uuid.NewString(), TenantID: tenant, RunID: runID, LastRunID: runID, ConnectionID: claimed.ConnectionID, Type: f.Type, Severity: f.Severity,
			State: VarOpen, Fingerprint: f.fingerprint(claimed.ConnectionID), Refs: mustJSON(f.Refs), RepeatCount: 1, Version: 1, CreatedAt: now, UpdatedAt: now}
		if f.Severity == SevHigh {
			high++
			escalate = true
			v.State, v.EscalatedAt = VarEscalated, &now
		}
		vs = append(vs, v)
	}
	out, err := s.Repo.MutateRun(ctx, runID, func(tx *RunTx) error {
		r := tx.Run
		if r.State != RunOpen {
			return nil
		}
		r.EvaluatedAt, r.NextEvaluateAt, r.LeaseUntil = &now, nil, nil
		r.Result = mustJSON(res.Result)
		r.VarianceCount, r.OpenVariances, r.HighVariances = len(vs), len(vs), high
		switch {
		case len(vs) == 0:
			r.State = RunMatched
		case escalate:
			r.State = RunEscalated
		default:
			r.State = RunVariance
		}
		tx.Variances = vs
		tx.VarianceEvent = func(v Variance, repeat bool) OutEvent {
			return s.varianceEvent("integration.reconciliation.variance", &v, "system:xic-reconciler", map[string]any{"repeated": repeat})
		}
		tx.Event = append(tx.Event, Event{Kind: "EVALUATED", Actor: "system:xic-reconciler", At: now, Detail: mustJSON(map[string]any{"state": r.State, "findings": len(vs), "high": high})})
		tx.Out = append(tx.Out, s.runEvent("integration.reconciliation.completed", r, "system:xic-reconciler", map[string]any{"variances": len(vs), "high": high}))
		if escalate {
			tx.Out = append(tx.Out, s.runEvent("integration.reconciliation.escalated", r, "system:xic-reconciler", map[string]any{"high": high}))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.observe("run_evaluated", out.State)
	return out, nil
}

func (s *Service) release(ctx context.Context, runID string) {
	_, _ = s.Repo.MutateRun(ctx, runID, func(tx *RunTx) error {
		if tx.Run.State == RunOpen {
			tx.Run.LeaseUntil = nil
			n := s.now().Add(30 * time.Second)
			tx.Run.NextEvaluateAt = &n
		}
		return nil
	})
}

// DriveDue evaluates the runs that are ready and returns how many it looked at.
func (s *Service) DriveDue(ctx context.Context, limit int) (int, error) {
	due, err := s.Repo.DueRuns(ctx, s.now(), limit)
	if err != nil {
		return 0, err
	}
	var first error
	for _, d := range due {
		if _, err := s.Evaluate(ctx, d.TenantID, d.ID); err != nil && first == nil {
			first = err
		}
	}
	return len(due), first
}

// Schedule starts the periodic runs every live connection is due for (OD-12). A scheduled run needs no declaration
// from anyone: it reconciles what the provider has already declared through callbacks and manifests against the
// platform's own state, so the comparison is still independent.
func (s *Service) Schedule(ctx context.Context) (int, error) {
	if s.Live == nil {
		return 0, nil
	}
	refs, err := s.Live.LiveRefs(ctx, 5000)
	if err != nil {
		return 0, err
	}
	now := s.now()
	created := 0
	for _, ref := range refs {
		tctx := withTenant(ctx, ref.TenantID)
		conn, err := s.Conns.Connection(ctx, ref.ConnectionID)
		if err != nil || conn.TenantID != ref.TenantID {
			continue
		}
		family := s.familyOf(tctx, conn.ConnectorID)
		pol := s.PolicyFor(family)
		freq := time.Duration(pol.FrequencyHours) * time.Hour
		if freq <= 0 {
			continue
		}
		end := now.Add(-pol.Grace).Truncate(time.Minute)
		last, err := s.Repo.LastScheduledEnd(tctx, ref.TenantID, ref.ConnectionID)
		if err != nil {
			continue
		}
		start := last
		if start.IsZero() {
			start = end.Add(-freq)
		}
		if end.Sub(start) < freq {
			continue
		}
		if end.Sub(start) > s.cfg().MaxPeriod {
			start = end.Add(-s.cfg().MaxPeriod)
		}
		r := &Run{RunID: uuid.NewString(), TenantID: ref.TenantID, ConnectionID: conn.ConnectionID, ConnectorID: conn.ConnectorID, Family: family, Origin: OriginScheduled,
			PeriodFrom: start, PeriodTo: end, Basis: []string{BasisEvidence, BasisState}, State: RunOpen, RequestedBy: "system:xic-reconciler", Version: 1, CreatedAt: now, UpdatedAt: now, NextEvaluateAt: &now}
		r.DedupeKey = sha("scheduled", ref.TenantID, conn.ConnectionID, start.Format(time.RFC3339), end.Format(time.RFC3339))
		_, existing, err := s.Repo.InsertRun(tctx, r, Event{Kind: "REQUESTED", Actor: r.RequestedBy, At: now, Detail: mustJSON(map[string]any{"origin": OriginScheduled})},
			[]OutEvent{s.runEvent("integration.reconciliation.requested", r, r.RequestedBy, nil)})
		if err == nil && !existing {
			created++
			s.observe("run_requested", OriginScheduled)
		}
	}
	return created, nil
}

// RunWorker evaluates due runs and starts scheduled ones until ctx ends.
func (s *Service) RunWorker(ctx context.Context, interval time.Duration, batch int, report func(n int, err error)) {
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
			var err error
			s.schedMu.Lock()
			due := s.now().Sub(s.lastSched) >= s.cfg().ScheduleEvery
			if due {
				s.lastSched = s.now()
			}
			s.schedMu.Unlock()
			if due {
				_, err = s.Schedule(ctx)
			}
			n, derr := s.DriveDue(ctx, batch)
			if err == nil {
				err = derr
			}
			if report != nil {
				report(n, err)
			}
		}
	}
}

// ── variance commands ──────────────────────────────────────────────────────

func (s *Service) mutateVariance(ctx context.Context, id string, expected int64, fn func(tx *VarTx) error) (*Variance, error) {
	return s.Repo.MutateVariance(ctx, id, func(tx *VarTx) error {
		tx.RunClosed = s.VarianceClosedRun
		if tx.V.Final() {
			return fmt.Errorf("%w: the variance is %s", ErrState, strings.ToLower(tx.V.State))
		}
		if expected > 0 && tx.V.Version != expected {
			return ErrVersionConflict
		}
		return fn(tx)
	})
}

// LinkCase attaches the case or exception that is handling a variance (I-26: a variance has a case path).
func (s *Service) LinkCase(ctx context.Context, a Actor, id, caseRef string, expected int64) (*Variance, error) {
	caseRef = strings.TrimSpace(caseRef)
	if caseRef == "" || len(caseRef) > 255 {
		return nil, fmt.Errorf("%w: a case reference is 1 to 255 characters", ErrBadRequest)
	}
	return s.mutateVariance(ctx, id, expected, func(tx *VarTx) error {
		tx.V.CaseRef = caseRef
		tx.Event = append(tx.Event, Event{Kind: "CASE_LINKED", VarianceID: id, Actor: a.PrincipalID, At: s.now(), Detail: mustJSON(map[string]any{"case_ref": caseRef})})
		return nil
	})
}

// Resolve settles a variance by naming how it was put right at its source. It never edits the exchange history.
func (s *Service) Resolve(ctx context.Context, a Actor, id, resolution, note string, expected int64) (*Variance, error) {
	resolution = strings.ToUpper(strings.TrimSpace(resolution))
	if !contains(Resolutions, resolution) {
		return nil, fmt.Errorf("%w: a resolution is one of %s", ErrBadRequest, strings.Join(Resolutions, ", "))
	}
	if len(strings.TrimSpace(note)) < 3 || len(note) > 1000 {
		return nil, fmt.Errorf("%w: say what was done (3 to 1000 characters)", ErrBadRequest)
	}
	return s.mutateVariance(ctx, id, expected, func(tx *VarTx) error {
		now := s.now()
		tx.V.State, tx.V.Resolution, tx.V.ResolutionNote, tx.V.ResolvedBy, tx.V.ResolvedAt = VarResolved, resolution, note, a.PrincipalID, &now
		tx.Event = append(tx.Event, Event{Kind: "RESOLVED", VarianceID: id, Actor: a.PrincipalID, At: now, Detail: mustJSON(map[string]any{"resolution": resolution})})
		tx.Out = append(tx.Out, s.varianceEvent("integration.reconciliation.variance-resolved", tx.V, a.PrincipalID, map[string]any{"resolution": resolution}))
		return nil
	})
}

// Accept records a decision to live with a difference. It needs a case and a reason; a HIGH variance cannot be accepted
// by the person who requested the run or supplied the provider's declaration.
func (s *Service) Accept(ctx context.Context, a Actor, id, caseRef, note string, expected int64) (*Variance, error) {
	if len(strings.TrimSpace(note)) < 3 || len(note) > 1000 {
		return nil, fmt.Errorf("%w: say why the difference is accepted (3 to 1000 characters)", ErrBadRequest)
	}
	return s.mutateVariance(ctx, id, expected, func(tx *VarTx) error {
		ref := strings.TrimSpace(caseRef)
		if ref == "" {
			ref = tx.V.CaseRef
		}
		if ref == "" || len(ref) > 255 {
			return fmt.Errorf("%w: accepting a difference needs the case that decided it", ErrBadRequest)
		}
		if tx.V.Severity == SevHigh && tx.Run != nil && (a.PrincipalID == tx.Run.RequestedBy || a.PrincipalID == tx.Run.ExternalSuppliedBy) {
			return ErrSelfAction
		}
		now := s.now()
		tx.V.CaseRef, tx.V.State, tx.V.Resolution, tx.V.ResolutionNote, tx.V.ResolvedBy, tx.V.ResolvedAt = ref, VarAccepted, "ACCEPTED_DIFFERENCE", note, a.PrincipalID, &now
		tx.Event = append(tx.Event, Event{Kind: "ACCEPTED", VarianceID: id, Actor: a.PrincipalID, At: now, Detail: mustJSON(map[string]any{"case_ref": ref})})
		tx.Out = append(tx.Out, s.varianceEvent("integration.reconciliation.variance-resolved", tx.V, a.PrincipalID, map[string]any{"resolution": "ACCEPTED_DIFFERENCE"}))
		return nil
	})
}

// CloseRun closes a run whose variances are all settled (a matched run closes as it is).
func (s *Service) CloseRun(ctx context.Context, a Actor, id, note string) (*Run, error) {
	if len(note) > 1000 {
		return nil, fmt.Errorf("%w: the note is at most 1000 characters", ErrBadRequest)
	}
	return s.Repo.MutateRun(ctx, id, func(tx *RunTx) error {
		r := tx.Run
		if r.State == RunOpen || r.State == RunClosed {
			return fmt.Errorf("%w: the run is %s", ErrState, strings.ToLower(r.State))
		}
		if r.OpenVariances > 0 {
			return fmt.Errorf("%w: the run still has %d open variances", ErrState, r.OpenVariances)
		}
		r.State, r.ClosedBy, r.CloseNote = RunClosed, a.PrincipalID, note
		tx.Event = append(tx.Event, Event{Kind: "CLOSED", Actor: a.PrincipalID, At: s.now()})
		tx.Out = append(tx.Out, s.runEvent("integration.reconciliation.closed", r, a.PrincipalID, nil))
		return nil
	})
}

// VarianceClosedRun builds the event for a run that closed because its last variance was settled.
func (s *Service) VarianceClosedRun(r *Run) OutEvent {
	return s.runEvent("integration.reconciliation.closed", r, r.ClosedBy, map[string]any{"auto": true})
}
