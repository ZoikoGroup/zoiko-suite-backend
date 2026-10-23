package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// Connections is what rollout needs of the connection layer.
type Connections interface {
	Connection(ctx context.Context, id string) (*xicconn.Connection, error)
	CertificationStatusOf(ctx context.Context, c *xicconn.Connection) (*xicconn.CertStatus, error)
}

// Config holds the policy knobs, each with a safe default (§19.1 OD-11, OD-13).
type Config struct {
	RequestTTL       time.Duration // how long a change request stays open
	MaxPilotDailyCap int           // the largest daily budget a pilot may be given
	DefaultSoakHours int
	DefaultSuccess   int
	DefaultFailPct   int
	UnknownSLA       time.Duration // an UNKNOWN attempt older than this blocks a step forward and rolls a pilot back (OD-13)
	MinPilotSample   int           // outcomes a pilot needs before its failure rate can roll it back
	ParityMinSamples int
	ParityMaxAge     time.Duration // a cutover request needs a passed parity run this recent
	ShadowMinHours   int
	RetireAfterHours int
	MaxPerCall       int // registrations in one call
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{RequestTTL: 72 * time.Hour, MaxPilotDailyCap: 100000, DefaultSoakHours: 24, DefaultSuccess: 20, DefaultFailPct: 5,
		UnknownSLA: 24 * time.Hour, MinPilotSample: 10, ParityMinSamples: 20, ParityMaxAge: 24 * time.Hour, ShadowMinHours: 24,
		RetireAfterHours: 72, MaxPerCall: 500}
}

// Service runs the staged rollout and the legacy migration.
type Service struct {
	Repo  Repo
	Conns Connections
	Cfg   Config
	Clock func() time.Time
	// Observe receives a short reason for every outcome, for metrics. It must not block.
	Observe func(kind, reason string)
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) cfg() Config {
	if s.Cfg.RequestTTL == 0 {
		return DefaultConfig()
	}
	return s.Cfg
}

func (s *Service) observe(kind, reason string) {
	if s.Observe != nil {
		s.Observe(kind, reason)
	}
}

func bad(path, rule string) error { return fmt.Errorf("%w: %s (%s)", ErrBadRequest, path, rule) }

// NotReadyError carries the readiness report with the refusal so the caller sees what blocks the step.
type NotReadyError struct{ Report Readiness }

func (e *NotReadyError) Error() string {
	var failed []string
	for _, c := range e.Report.Checks {
		if !c.Pass {
			failed = append(failed, c.Name)
		}
	}
	return ErrNotReady.Error() + ": " + strings.Join(failed, ", ")
}
func (e *NotReadyError) Unwrap() error { return ErrNotReady }

func evt(a Actor, typ, aggType, id string, data map[string]any) OutEvent {
	return OutEvent{Type: typ, AggregateType: aggType, AggregateID: id, ActorID: a.PrincipalID, CorrelationID: a.CorrelationID, Data: data}
}

func event(kind, id, evKind string, a Actor, at time.Time, detail map[string]any) Event {
	var d json.RawMessage
	if detail != nil {
		d, _ = json.Marshal(detail)
	}
	return Event{SubjectKind: kind, SubjectID: id, Kind: evKind, Detail: d, Actor: a.PrincipalID, At: at}
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// ── staged rollout ─────────────────────────────────────────────────────────────

// Create plans the rollout of a production connection. The connection must be a live, production connection of
// this tenant; the pilot terms are bounded here and fixed once the pilot starts.
func (s *Service) Create(ctx context.Context, a Actor, connectionID string, t Terms) (*Rollout, error) {
	conn, err := s.Conns.Connection(ctx, connectionID)
	if err != nil {
		return nil, ErrNotFound
	}
	if conn.TenantID != tenantOf(ctx) {
		return nil, ErrNotFound
	}
	if conn.Environment != xicconn.EnvProduction {
		return nil, bad("connection_id", "only a production connection is rolled out in stages")
	}
	if conn.Status == xicconn.StatusRevoked || conn.Status == xicconn.StatusDraft {
		return nil, bad("connection_id", "the connection is "+strings.ToLower(conn.Status))
	}
	c := s.cfg()
	if t.PilotDailyCap == 0 {
		return nil, bad("pilot_daily_cap", "required")
	}
	if t.PilotDailyCap < 1 || t.PilotDailyCap > c.MaxPilotDailyCap {
		return nil, bad("pilot_daily_cap", fmt.Sprintf("between 1 and %d", c.MaxPilotDailyCap))
	}
	if t.MinSoakHours < 0 || t.MinSoakHours > 24*90 {
		return nil, bad("min_soak_hours", "between 0 and 2160")
	}
	if t.MinSoakHours == 0 {
		t.MinSoakHours = c.DefaultSoakHours
	}
	if t.MinSuccess < 0 {
		return nil, bad("min_success", "not negative")
	}
	if t.MinSuccess == 0 {
		t.MinSuccess = c.DefaultSuccess
	}
	if t.MaxFailurePct < 0 || t.MaxFailurePct > 100 {
		return nil, bad("max_failure_pct", "between 0 and 100")
	}
	if t.MaxFailurePct == 0 {
		t.MaxFailurePct = c.DefaultFailPct
	}
	ops := dedupe(t.PilotOperations)
	if len(ops) > 50 {
		return nil, bad("pilot_operations", "at most 50")
	}
	now := s.now()
	r := &Rollout{RolloutID: uuid.NewString(), TenantID: conn.TenantID, ConnectionID: conn.ConnectionID, ConnectorID: conn.ConnectorID,
		Stage: StagePlanned, PilotOps: ops, PilotDailyCap: t.PilotDailyCap, MinSoakHours: t.MinSoakHours, MinSuccess: t.MinSuccess,
		MaxFailurePct: t.MaxFailurePct, StageSince: now, CreatedBy: a.PrincipalID, Version: 1, CreatedAt: now, UpdatedAt: now}
	ev := event(KindRollout, r.RolloutID, "PLANNED", a, now, map[string]any{"pilot_daily_cap": r.PilotDailyCap, "pilot_operations": ops})
	err = s.Repo.InsertRollout(ctx, r, ev, []OutEvent{evt(a, "integration.rollout.planned", "rollout", r.RolloutID,
		map[string]any{"tenant_id": r.TenantID, "connection_id": r.ConnectionID, "connector_id": r.ConnectorID, "stage": StagePlanned})})
	if err != nil {
		return nil, err
	}
	s.observe("rollout", "planned")
	return r, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func nextStage(stage string) string {
	switch stage {
	case StagePlanned:
		return StagePilot
	case StagePilot:
		return StageGeneral
	}
	return ""
}

// Readiness computes the evidence for the next step of a rollout: the connection is live and certified, nothing
// serious is open, and for the step to GENERAL the pilot soaked long enough with enough clean exchanges.
func (s *Service) Readiness(ctx context.Context, r *Rollout) (*Readiness, error) {
	to := nextStage(r.Stage)
	if to == "" {
		return nil, ErrState
	}
	conn, err := s.Conns.Connection(ctx, r.ConnectionID)
	if err != nil {
		return nil, ErrNotFound
	}
	now := s.now()
	var checks []Check
	add := func(name string, pass bool, detail string) {
		checks = append(checks, Check{Name: name, Pass: pass, Detail: detail})
	}
	add("connection_active", conn.Status == xicconn.StatusActive, "the connection is "+strings.ToLower(conn.Status))
	cs, err := s.Conns.CertificationStatusOf(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	add("certification_valid", cs.State == xicconn.CertValid, "certification is "+strings.ToLower(cs.State))
	high, err := s.Repo.OpenHighVariances(ctx, r.ConnectionID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	add("no_open_high_variance", high == 0, fmt.Sprintf("%d open HIGH variances", high))
	over, err := s.Repo.OverdueUnknown(ctx, r.ConnectionID, now.Add(-s.cfg().UnknownSLA))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	add("no_overdue_unknown", over == 0, fmt.Sprintf("%d UNKNOWN attempts past the escalation limit", over))
	if to == StageGeneral {
		soak := now.Sub(r.StageSince)
		add("pilot_soaked", soak >= time.Duration(r.MinSoakHours)*time.Hour, fmt.Sprintf("%dh of %dh", int(soak.Hours()), r.MinSoakHours))
		o, err := s.Repo.Outcomes(ctx, r.ConnectionID, r.StageSince, now.Add(time.Second))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependency, err)
		}
		add("pilot_volume", o.Success >= r.MinSuccess, fmt.Sprintf("%d of %d successful exchanges", o.Success, r.MinSuccess))
		total := o.Success + o.Failure + o.Unknown
		pct := 0
		if total > 0 {
			pct = (o.Failure + o.Unknown) * 100 / total
		}
		add("pilot_failure_rate", pct <= r.MaxFailurePct, fmt.Sprintf("%d%% failed or unknown, limit %d%%", pct, r.MaxFailurePct))
	}
	rep := &Readiness{Ready: true, Checks: checks}
	for _, c := range checks {
		if !c.Pass {
			rep.Ready = false
		}
	}
	return rep, nil
}

// RequestAdvance asks for the next stage. The attestations are what a person vouches for outside the platform's
// reach (§18.2): all four to start the pilot, no open P0/P1 defects to go general.
func (s *Service) RequestAdvance(ctx context.Context, a Actor, rolloutID string, attestations []string, reason string) (*Request, error) {
	r, err := s.Repo.GetRollout(ctx, rolloutID)
	if err != nil {
		return nil, err
	}
	to := nextStage(r.Stage)
	if to == "" {
		return nil, ErrState
	}
	att := dedupe(attestations)
	for _, x := range att {
		if !contains(Attestations, x) {
			return nil, bad("attestations", x+" is not an attestation")
		}
	}
	need := []string{AttNoP0P1}
	if to == StagePilot {
		need = Attestations
	}
	for _, n := range need {
		if !contains(att, n) {
			return nil, bad("attestations", n+" is required for "+to)
		}
	}
	rep, err := s.Readiness(ctx, r)
	if err != nil {
		return nil, err
	}
	if !rep.Ready {
		s.observe("rollout", "not_ready")
		return nil, &NotReadyError{Report: *rep}
	}
	now := s.now()
	rb, _ := json.Marshal(rep)
	q := &Request{RequestID: uuid.NewString(), TenantID: r.TenantID, SubjectKind: KindRollout, SubjectID: r.RolloutID, FromState: r.Stage, ToState: to,
		Status: ReqPending, Attestations: att, Readiness: rb, Reason: truncate(reason, 1000), RequestedBy: a.PrincipalID,
		ExpiresAt: now.Add(s.cfg().RequestTTL), CreatedAt: now, Version: 1}
	err = s.Repo.InsertRequest(ctx, q, event(KindRollout, r.RolloutID, "ADVANCE_REQUESTED", a, now, map[string]any{"to": to, "request_id": q.RequestID, "attestations": att}),
		[]OutEvent{evt(a, "integration.rollout.advance-requested", "rollout", r.RolloutID,
			map[string]any{"tenant_id": r.TenantID, "connection_id": r.ConnectionID, "from": r.Stage, "to": to, "request_id": q.RequestID})})
	if err != nil {
		return nil, err
	}
	s.observe("rollout", "advance_requested")
	return q, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Decide approves or rejects a change request. The decider is not the requester. An approval is idempotent and
// crash-safe: it records the approval first and then applies it, and a repeat of the approval applies what a
// crash left unapplied. Readiness is recomputed at the decision, so an approval never rests on stale evidence.
func (s *Service) Decide(ctx context.Context, a Actor, requestID string, approve bool, note string) (*Request, error) {
	q, err := s.Repo.GetRequest(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if a.PrincipalID == q.RequestedBy {
		return nil, ErrSelfAction
	}
	now := s.now()
	switch q.Status {
	case ReqUsed:
		return q, nil
	case ReqApproved:
		if !approve {
			return nil, ErrState
		}
		// a crash between approval and application: finish it
		return s.apply(ctx, a, q)
	case ReqPending:
	default:
		return nil, ErrState
	}
	if !q.ExpiresAt.After(now) {
		_, _ = s.Repo.MutateRequest(ctx, q.RequestID, func(tx *RequestTx) error {
			if tx.Q.Status == ReqPending {
				tx.Q.Status = ReqExpired
			}
			return nil
		})
		return nil, ErrState
	}
	if !approve {
		q, err = s.Repo.MutateRequest(ctx, q.RequestID, func(tx *RequestTx) error {
			if tx.Q.Status != ReqPending {
				return ErrState
			}
			tx.Q.Status, tx.Q.DecidedBy, tx.Q.DecisionNote, tx.Q.DecidedAt = ReqRejected, a.PrincipalID, truncate(note, 1000), &now
			tx.Event = append(tx.Event, event(q.SubjectKind, q.SubjectID, "REQUEST_REJECTED", a, now, map[string]any{"request_id": q.RequestID, "to": q.ToState}))
			tx.Out = append(tx.Out, evt(a, "integration.rollout.request-rejected", strings.ToLower(q.SubjectKind), q.SubjectID,
				map[string]any{"tenant_id": q.TenantID, "request_id": q.RequestID, "to": q.ToState}))
			return nil
		})
		if err == nil {
			s.observe("rollout", "request_rejected")
		}
		return q, err
	}
	// the evidence at the decision
	if err := s.recheck(ctx, q); err != nil {
		return nil, err
	}
	q, err = s.Repo.MutateRequest(ctx, q.RequestID, func(tx *RequestTx) error {
		if tx.Q.Status != ReqPending {
			return ErrState
		}
		tx.Q.Status, tx.Q.DecidedBy, tx.Q.DecisionNote, tx.Q.DecidedAt = ReqApproved, a.PrincipalID, truncate(note, 1000), &now
		tx.Event = append(tx.Event, event(q.SubjectKind, q.SubjectID, "REQUEST_APPROVED", a, now, map[string]any{"request_id": q.RequestID, "to": q.ToState}))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.apply(ctx, a, q)
}

// recheck recomputes the evidence of the request's subject.
func (s *Service) recheck(ctx context.Context, q *Request) error {
	switch q.SubjectKind {
	case KindRollout:
		r, err := s.Repo.GetRollout(ctx, q.SubjectID)
		if err != nil {
			return err
		}
		if r.Stage != q.FromState {
			return ErrState
		}
		rep, err := s.Readiness(ctx, r)
		if err != nil {
			return err
		}
		if !rep.Ready {
			return &NotReadyError{Report: *rep}
		}
	case KindLegacy:
		l, err := s.Repo.GetLegacy(ctx, q.SubjectID)
		if err != nil {
			return err
		}
		if l.State != q.FromState {
			return ErrState
		}
		rep, err := s.legacyReadiness(ctx, l, q.ToState)
		if err != nil {
			return err
		}
		if !rep.Ready {
			return &NotReadyError{Report: *rep}
		}
	}
	return nil
}

func (s *Service) apply(ctx context.Context, a Actor, q *Request) (*Request, error) {
	now := s.now()
	switch q.SubjectKind {
	case KindRollout:
		_, err := s.Repo.MutateRollout(ctx, q.SubjectID, func(tx *RolloutTx) error {
			if tx.R.Stage != q.FromState {
				return ErrState
			}
			from := tx.R.Stage
			tx.R.Stage, tx.R.StageSince, tx.R.AdvanceRequest = q.ToState, now, q.RequestID
			tx.Use = q.RequestID
			tx.Event = append(tx.Event, event(KindRollout, tx.R.RolloutID, "ADVANCED", a, now, map[string]any{"from": from, "to": q.ToState, "request_id": q.RequestID}))
			tx.Out = append(tx.Out, evt(a, "integration.rollout.advanced", "rollout", tx.R.RolloutID,
				map[string]any{"tenant_id": tx.R.TenantID, "connection_id": tx.R.ConnectionID, "from": from, "to": q.ToState, "request_id": q.RequestID}))
			return nil
		})
		if err != nil {
			return nil, err
		}
		s.observe("rollout", "advanced")
	case KindLegacy:
		_, err := s.Repo.MutateLegacy(ctx, q.SubjectID, func(tx *LegacyTx) error {
			if tx.L.State != q.FromState {
				return ErrState
			}
			from := tx.L.State
			tx.L.State, tx.L.StateSince, tx.L.AdvanceRequest = q.ToState, now, q.RequestID
			tx.Use = q.RequestID
			typ := "integration.legacy.cutover"
			if q.ToState == LegacyRetired {
				typ = "integration.legacy.retired"
			}
			tx.Event = append(tx.Event, event(KindLegacy, tx.L.LegacyID, q.ToState, a, now, map[string]any{"from": from, "request_id": q.RequestID}))
			tx.Out = append(tx.Out, evt(a, typ, "legacy-connector", tx.L.LegacyID,
				map[string]any{"tenant_id": tx.L.TenantID, "connection_id": tx.L.ConnectionID, "from": from, "to": q.ToState, "request_id": q.RequestID}))
			return nil
		})
		if err != nil {
			return nil, err
		}
		s.observe("legacy", strings.ToLower(q.ToState))
	}
	return s.Repo.GetRequest(ctx, q.RequestID)
}

// Rollback stops the rollout at once. It is the safe direction, so no second person is needed; it needs a reason.
// A rolled-back connection carries nothing until a new rollout is planned and passes its steps again.
func (s *Service) Rollback(ctx context.Context, a Actor, rolloutID, reason string, expectedVersion int64) (*Rollout, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, bad("reason", "required")
	}
	now := s.now()
	r, err := s.Repo.MutateRollout(ctx, rolloutID, func(tx *RolloutTx) error {
		if expectedVersion > 0 && tx.R.Version != expectedVersion {
			return ErrVersionConflict
		}
		if tx.R.Stage == StageRolledBack {
			return ErrState
		}
		from := tx.R.Stage
		tx.R.Stage, tx.R.StageSince, tx.R.RollbackReason, tx.R.RolledBackBy = StageRolledBack, now, truncate(reason, 1000), a.PrincipalID
		tx.Event = append(tx.Event, event(KindRollout, tx.R.RolloutID, "ROLLED_BACK", a, now, map[string]any{"from": from, "reason": truncate(reason, 200)}))
		tx.Out = append(tx.Out, evt(a, "integration.rollout.rolled-back", "rollout", tx.R.RolloutID,
			map[string]any{"tenant_id": tx.R.TenantID, "connection_id": tx.R.ConnectionID, "from": from, "automatic": strings.HasPrefix(a.PrincipalID, "system:")}))
		return nil
	})
	if err == nil {
		s.observe("rollout", "rolled_back")
	}
	return r, err
}

// EvaluatePilots looks at every pilot across tenants and rolls back the ones that misbehave: a failure rate over the
// pilot's limit once it has enough outcomes, an open HIGH variance, or an UNKNOWN attempt past its escalation limit.
// It returns how many it looked at; one pilot that fails to evaluate does not stop the others.
func (s *Service) EvaluatePilots(ctx context.Context, batch int) (int, error) {
	due, err := s.Repo.PilotRollouts(ctx, batch)
	if err != nil {
		return 0, err
	}
	var first error
	for _, d := range due {
		if err := s.evaluate(withTenant(ctx, d.TenantID), d.ID); err != nil && first == nil {
			first = err
		}
	}
	return len(due), first
}

func (s *Service) evaluate(ctx context.Context, id string) error {
	r, err := s.Repo.GetRollout(ctx, id)
	if err != nil {
		return err
	}
	if r.Stage != StagePilot {
		return nil
	}
	now := s.now()
	reason := ""
	high, err := s.Repo.OpenHighVariances(ctx, r.ConnectionID)
	if err != nil {
		return err
	}
	over, err := s.Repo.OverdueUnknown(ctx, r.ConnectionID, now.Add(-s.cfg().UnknownSLA))
	if err != nil {
		return err
	}
	o, err := s.Repo.Outcomes(ctx, r.ConnectionID, r.StageSince, now.Add(time.Second))
	if err != nil {
		return err
	}
	total := o.Success + o.Failure + o.Unknown
	switch {
	case high > 0:
		reason = "AUTO: an open HIGH reconciliation variance"
	case over > 0:
		reason = "AUTO: an UNKNOWN attempt is past its escalation limit"
	case total >= s.cfg().MinPilotSample && (o.Failure+o.Unknown)*100/total > r.MaxFailurePct:
		reason = fmt.Sprintf("AUTO: %d%% of %d pilot exchanges failed or are unknown, limit %d%%", (o.Failure+o.Unknown)*100/total, total, r.MaxFailurePct)
	}
	if reason == "" {
		return nil
	}
	_, err = s.Rollback(ctx, Actor{PrincipalID: "system:xic-rollout"}, id, reason, 0)
	if errors.Is(err, ErrState) {
		return nil
	}
	if err == nil {
		s.observe("rollout", "auto_rolled_back")
	}
	return err
}

// RunWorker evaluates pilots every interval until ctx ends.
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
			n, err := s.EvaluatePilots(ctx, batch)
			if report != nil {
				report(n, err)
			}
		}
	}
}

// ── the gate ───────────────────────────────────────────────────────────────────

// Gate is the xicconn.StageGate of a deployment: it applies the stage of a production connection's rollout to every
// operation that starts. With Required set a production connection with no rollout carries nothing; without it a
// connection that was never put through a rollout keeps working (the stages govern the ones that have one).
type Gate struct {
	Repo     Repo
	Required bool
	Observe  func(kind, reason string)
}

var _ xicconn.StageGate = (*Gate)(nil)

func (g *Gate) refuse(c *xicconn.Connection, code xicconn.RefusalCode, detail, reason string) error {
	if g.Observe != nil {
		g.Observe("gate", reason)
	}
	return &xicconn.Refusal{Code: code, Detail: detail, ConnectionID: c.ConnectionID}
}

// Admit implements xicconn.StageGate.
func (g *Gate) Admit(ctx context.Context, c *xicconn.Connection, operationID string, now time.Time) error {
	r, err := g.Repo.LatestRolloutFor(ctx, c.ConnectionID)
	if errors.Is(err, ErrNotFound) {
		if g.Required {
			return g.refuse(c, xicconn.CodeNotActive, "the production connection has no staged rollout", "no_rollout")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: rollout stage: %v", xicconn.ErrDependency, err)
	}
	switch r.Stage {
	case StageGeneral:
		return nil
	case StagePlanned:
		return g.refuse(c, xicconn.CodeNotActive, "the rollout is planned and has not started", "planned")
	case StageRolledBack:
		return g.refuse(c, xicconn.CodeNotActive, "the rollout was rolled back", "rolled_back")
	case StagePilot:
		if len(r.PilotOps) > 0 && !contains(r.PilotOps, operationID) {
			return g.refuse(c, xicconn.CodeNotActive, "the operation is not part of the pilot", "not_in_pilot")
		}
		if xicconn.BudgetFree(ctx) {
			return nil
		}
		ok, err := g.Repo.Admit(ctx, r, now)
		if err != nil {
			return fmt.Errorf("%w: pilot budget: %v", xicconn.ErrDependency, err)
		}
		if !ok {
			return g.refuse(c, xicconn.CodeRateLimited, "the pilot's daily budget is spent", "budget_spent")
		}
		return nil
	}
	return g.refuse(c, xicconn.CodeNotActive, "the rollout is in an unknown stage", "unknown_stage")
}

func tenantOf(ctx context.Context) string { return middleware.GetTenantID(ctx) }
