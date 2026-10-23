package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// Controlled replay and backfill (ZS-SVC-T-001 §10.3). A replay is a governed re-run of held exchanges and nothing
// else:
//
//	Scope        explicit - the held rows of one batch by outcome or row number, explicit exchange ids, or a bounded time
//	             window of one connection and operation. "Replay everything" does not exist (NP-39).
//	Versions     the contract and mapping the data ARRIVED under, recorded on the job. A replay never silently uses
//	             today's mapping; a migration replay that does is named as such and always approved (NP-41).
//	Reason       one of the documented reasons plus words, recorded.
//	Approval     a replay that is large, regulated, a migration or a backfill waits for someone other than its requester
//	             and says so with XIC-015 (NP-40). A material replay needs a dry run first.
//	Dry run      projects what would be accepted, quarantined, duplicated and changed, writing nothing.
//	Idempotency  an exchange already accepted, handed off or final is never touched: the domain's own idempotency and
//	             compensation rules own anything beyond that (NP-43); a body that is no longer retained is not
//	             reconstructed (NP-42).
//	Evidence     the job, its initiator and approver, its versions, its per-exchange differences and its counts.

// ReplayRequest asks for a replay or a dry run.
type ReplayRequest struct {
	Scope             ReplayScope `json:"scope"`
	ReasonCode        string      `json:"reason_code"`
	Reason            string      `json:"reason"`
	DryRun            bool        `json:"dry_run"`
	DryRunOf          string      `json:"dry_run_of,omitempty"`
	UseCurrentMapping bool        `json:"use_current_mapping"`
}

// RequestReplay validates the scope, pins the versions, assesses the risk and either completes a dry run, queues the
// replay for approval, or approves it for the worker.
func (s *Service) RequestReplay(ctx context.Context, a Actor, in ReplayRequest) (*ReplayJob, error) {
	cfg := s.cfg()
	if !replayReasons[in.ReasonCode] || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 255 {
		return nil, fmt.Errorf("%w: reason_code (%s) and a reason of at most 255 characters are required", ErrBadRequest, strings.Join(reasonList(), ", "))
	}
	tenant := tenantOf(ctx)
	job := &ReplayJob{ReplayID: uuid.NewString(), TenantID: tenant, ReasonCode: in.ReasonCode, Reason: in.Reason, DryRun: in.DryRun, DryRunOf: in.DryRunOf,
		UseCurrentMapping: in.UseCurrentMapping, RequestedBy: a.PrincipalID, CorrelationID: a.CorrelationID, Version: 1, CreatedAt: s.now(), UpdatedAt: s.now()}
	ids, err := s.resolveScope(ctx, job, in.Scope)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: nothing in the scope can be replayed", ErrReplayScope)
	}
	if len(ids) > cfg.ReplayMaxItems {
		return nil, fmt.Errorf("%w: the scope covers more than %d exchanges", ErrReplayScope, cfg.ReplayMaxItems)
	}
	job.SelectedItems = len(ids)
	sc, _ := json.Marshal(in.Scope)
	job.Scope = sc

	// the versions the replay runs under
	conn, v, op, err := s.pinFor(ctx, job, ids[0])
	if err != nil {
		return nil, err
	}
	job.ConnectionID, job.OperationID = conn.ConnectionID, op.ID
	job.ContractVersionID = v.ContractVersionID
	if mv := v.MappingFor(op.RequestSchema); mv != nil {
		job.MappingVersionID = mv.MappingVersionID
	}

	// risk
	var reasons []string
	if len(ids) >= cfg.ReplayApprovalRows {
		reasons = append(reasons, "VOLUME")
	}
	if in.UseCurrentMapping {
		reasons = append(reasons, "CURRENT_MAPPING")
	}
	if in.ReasonCode == ReasonMigrationValidation || in.ReasonCode == ReasonApprovedBackfill {
		reasons = append(reasons, "REASON_"+in.ReasonCode)
	}
	for _, c := range op.DataClasses {
		for _, r := range cfg.RegulatedClasses {
			if strings.EqualFold(c, r) {
				reasons = append(reasons, "REGULATED_DATA")
			}
		}
	}
	job.Risk, job.RiskReasons = RiskLow, reasons
	if len(reasons) > 0 {
		job.Risk = RiskHigh
	}
	if in.UseCurrentMapping && job.ScopeKind == ScopeExchanges {
		return nil, fmt.Errorf("%w: a migration replay covers one connection (a batch or a window), not loose exchange ids", ErrReplayScope)
	}

	items := make([]ReplayItem, 0, len(ids))
	for _, id := range ids {
		items = append(items, ReplayItem{ReplayID: job.ReplayID, TenantID: tenant, ExchangeID: id})
	}

	if in.DryRun {
		counts, err := s.project(ctx, job, items)
		if err != nil {
			return nil, err
		}
		job.State = ReplayDryRunComplete
		job.Projected = mustJSON(counts)
		t := s.now()
		job.StartedAt, job.FinishedAt = &t, &t
		if err := s.Repo.InsertReplay(ctx, job, items, s.replayEvent("integration.replay.requested", job, a.PrincipalID)); err != nil {
			return nil, err
		}
		return job, nil
	}

	// a material replay is preceded by a dry run of the same scope
	if job.Risk == RiskHigh {
		if err := s.requireDryRun(ctx, job, in); err != nil {
			return nil, err
		}
		job.State = ReplayPendingApproval
	} else {
		job.State = ReplayApproved
		t := s.now()
		job.DecidedAt = &t
		job.DecisionNote = "within the approval threshold"
	}
	if err := s.Repo.InsertReplay(ctx, job, items, s.replayEvent("integration.replay.requested", job, a.PrincipalID)); err != nil {
		return nil, err
	}
	s.observe("replay_requested", job.Risk)
	return job, nil
}

func reasonList() []string {
	return []string{ReasonIncidentRepair, ReasonSourceCorrection, ReasonProviderRedelivery, ReasonMigrationValidation, ReasonApprovedBackfill}
}

func tenantOf(ctx context.Context) string {
	return middlewareTenant(ctx)
}

// resolveScope turns an explicit scope into the exchanges it covers. A scope with no selector is refused.
func (s *Service) resolveScope(ctx context.Context, job *ReplayJob, sc ReplayScope) ([]string, error) {
	cfg := s.cfg()
	tenant := job.TenantID
	switch {
	case sc.BatchID != "":
		job.ScopeKind, job.BatchID = ScopeBatch, sc.BatchID
		if len(sc.RowStates) == 0 && len(sc.RowNumbers) == 0 {
			return nil, fmt.Errorf("%w: name the row outcomes or row numbers to replay", ErrReplayScope)
		}
		for _, st := range sc.RowStates {
			if st != RowQuarantined && st != RowDomainRejected {
				return nil, fmt.Errorf("%w: only held rows (%s, %s) can be replayed", ErrReplayScope, RowQuarantined, RowDomainRejected)
			}
		}
		b, err := s.Repo.Get(ctx, sc.BatchID)
		if err != nil {
			return nil, err
		}
		var ids []string
		after := 0
		for {
			rows, err := s.Repo.Rows(ctx, b.BatchID, RowFilter{States: sc.RowStates, RowNos: sc.RowNumbers, After: after, Limit: 1000})
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				if r.ExchangeID != "" {
					ids = append(ids, r.ExchangeID)
				}
				after = r.RowNo
			}
			if len(rows) < 1000 || len(ids) > cfg.ReplayMaxItems {
				break
			}
		}
		return ids, nil
	case len(sc.ExchangeIDs) > 0:
		job.ScopeKind = ScopeExchanges
		if len(sc.ExchangeIDs) > cfg.ReplayMaxItems {
			return nil, fmt.Errorf("%w: more than %d exchange ids", ErrReplayScope, cfg.ReplayMaxItems)
		}
		sums, err := s.In.Repo.Summaries(ctx, sc.ExchangeIDs)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependency, err)
		}
		seen := map[string]bool{}
		var ids []string
		for _, id := range sc.ExchangeIDs {
			if _, ok := sums[id]; !ok {
				// another tenant's object, or none at all: the request is refused whole, never partly
				return nil, ErrNotFound
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		return ids, nil
	case sc.ConnectionID != "" || sc.OperationID != "" || sc.From != nil || sc.To != nil:
		job.ScopeKind = ScopeWindow
		if sc.ConnectionID == "" || sc.OperationID == "" || sc.From == nil || sc.To == nil || !sc.From.Before(*sc.To) {
			return nil, fmt.Errorf("%w: a window names a connection, an operation and a time range", ErrReplayScope)
		}
		if sc.To.Sub(*sc.From) > cfg.ReplayWindowMax {
			return nil, fmt.Errorf("%w: a window covers at most %s", ErrReplayScope, cfg.ReplayWindowMax)
		}
		conn, err := s.Conns.Connection(ctx, sc.ConnectionID)
		if err != nil || conn.TenantID != tenant {
			return nil, ErrNotFound
		}
		return s.In.Repo.Held(ctx, sc.ConnectionID, sc.OperationID, *sc.From, *sc.To, cfg.ReplayMaxItems+1)
	}
	return nil, ErrReplayScope
}

// pinFor returns the connection, contract version and operation a replay runs under: the version the data arrived
// under, or - for a governed migration replay - the connection's current one.
func (s *Service) pinFor(ctx context.Context, job *ReplayJob, firstExchange string) (*xicconn.Connection, *registry.ContractVersion, *registry.Operation, error) {
	e, err := s.In.Repo.Get(ctx, firstExchange)
	if err != nil {
		return nil, nil, nil, err
	}
	conn, err := s.Conns.Connection(ctx, e.ConnectionID)
	if err != nil || conn.TenantID != job.TenantID {
		return nil, nil, nil, ErrNotFound
	}
	var v *registry.ContractVersion
	if job.UseCurrentMapping {
		v, err = s.Conns.PinnedContract(ctx, conn)
	} else if s.In.Versions != nil {
		v, err = s.In.Versions.GetVersion(ctx, e.ContractVersionID)
	} else {
		err = errors.New("contract versions are not available")
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	op := v.Operation(e.OperationID)
	if op == nil {
		return nil, nil, nil, fmt.Errorf("%w: the pinned contract version has no such operation", ErrReplayScope)
	}
	return conn, v, op, nil
}

func (s *Service) requireDryRun(ctx context.Context, job *ReplayJob, in ReplayRequest) error {
	if in.DryRunOf == "" {
		return fmt.Errorf("%w: a replay that needs approval is preceded by a dry run of the same scope (dry_run_of)", ErrBadRequest)
	}
	d, err := s.Repo.GetReplay(ctx, in.DryRunOf)
	if err != nil {
		return fmt.Errorf("%w: dry_run_of names no dry run of yours", ErrBadRequest)
	}
	sameScope := d.ScopeKind == job.ScopeKind && sameJSON(d.Scope, job.Scope)
	if !d.DryRun || d.State != ReplayDryRunComplete || !sameScope || s.now().Sub(d.FinishedAtOrCreated()) > 24*time.Hour {
		return fmt.Errorf("%w: dry_run_of must be a completed dry run of exactly this scope from the last 24 hours", ErrBadRequest)
	}
	return nil
}

// FinishedAtOrCreated is when a job last finished, or was created.
func (j *ReplayJob) FinishedAtOrCreated() time.Time {
	if j.FinishedAt != nil {
		return *j.FinishedAt
	}
	return j.CreatedAt
}

// project runs a replay without writing: what would be accepted, held again, a duplicate, or changed.
func (s *Service) project(ctx context.Context, job *ReplayJob, items []ReplayItem) (ReplayCounts, error) {
	var pin *registry.ContractVersion
	if job.UseCurrentMapping {
		conn, err := s.Conns.Connection(ctx, job.ConnectionID)
		if err != nil {
			return ReplayCounts{}, fmt.Errorf("%w: %v", ErrDependency, err)
		}
		if pin, err = s.Conns.PinnedContract(ctx, conn); err != nil {
			return ReplayCounts{}, fmt.Errorf("%w: %v", ErrDependency, err)
		}
	}
	counts := ReplayCounts{Selected: len(items)}
	for i := range items {
		r, err := s.In.Replay(ctx, inbound.ReplayInput{ExchangeID: items[i].ExchangeID, Pin: pin, JobID: job.ReplayID, DryRun: true})
		if err != nil {
			if errors.Is(err, inbound.ErrDependency) {
				return ReplayCounts{}, fmt.Errorf("%w: %v", ErrDependency, err)
			}
			items[i].Skipped, items[i].Done = "ERROR", true
			counts.Failed++
			continue
		}
		fillItem(&items[i], r)
		items[i].Done = true
		tally(&counts, r, false)
	}
	return counts, nil
}

func fillItem(it *ReplayItem, r *inbound.ReplayResult) {
	it.BeforeState, it.BeforeClass, it.BeforeReason = r.Before.State, r.Before.Class, r.Before.Reason
	it.AfterState, it.AfterClass, it.AfterReason = r.After.State, r.After.Class, r.After.Reason
	it.Changed, it.Applied, it.Skipped = r.Changed, r.Applied, r.Skipped
}

func tally(c *ReplayCounts, r *inbound.ReplayResult, applied bool) {
	if r.Skipped != "" {
		c.Skipped++
		return
	}
	c.Replayable++
	switch r.After.State {
	case inbound.StateValidated:
		c.WouldAccept++
	case inbound.StateQuarantined:
		c.Quarantined++
	case inbound.StateDuplicate:
		c.Duplicate++
	}
	if r.Changed {
		c.Changed++
	}
	if applied && r.Applied {
		c.Applied++
	}
}

func (s *Service) replayEvent(typ string, j *ReplayJob, actor string) OutEvent {
	return OutEvent{Type: typ, AggregateType: "replay-job", AggregateID: j.ReplayID, ActorID: actor, CorrelationID: j.CorrelationID,
		Data: map[string]any{"replay_job_id": j.ReplayID, "tenant_id": j.TenantID, "connection_id": j.ConnectionID, "source_scope": j.ScopeKind, "contract_version_id": j.ContractVersionID,
			"mapping_version_id": j.MappingVersionID, "reason": j.ReasonCode, "risk": j.Risk, "state": j.State, "items": j.SelectedItems, "dry_run": j.DryRun}}
}

// DecideReplay approves or rejects a replay that is waiting for approval. The approver is never the requester.
func (s *Service) DecideReplay(ctx context.Context, a Actor, id string, approve bool, note string) (*ReplayJob, error) {
	if len(note) > 255 {
		return nil, fmt.Errorf("%w: note", ErrBadRequest)
	}
	now := s.now()
	return s.Repo.MutateReplay(ctx, id, func(tx *ReplayTx) error {
		if tx.J.State != ReplayPendingApproval {
			return ErrReplayState
		}
		if a.PrincipalID == tx.J.RequestedBy {
			return ErrSelfAction
		}
		tx.J.DecidedBy, tx.J.DecidedAt, tx.J.DecisionNote = a.PrincipalID, &now, note
		typ := "integration.replay.approved"
		tx.J.State = ReplayApproved
		if !approve {
			tx.J.State, typ = ReplayRejected, "integration.replay.rejected"
			t := now
			tx.J.FinishedAt = &t
		}
		tx.Out = append(tx.Out, s.replayEvent(typ, tx.J, a.PrincipalID))
		return nil
	})
}

// CancelReplay withdraws a replay that has not started.
func (s *Service) CancelReplay(ctx context.Context, a Actor, id, reason string) (*ReplayJob, error) {
	if strings.TrimSpace(reason) == "" || len(reason) > 255 {
		return nil, fmt.Errorf("%w: a reason is required", ErrBadRequest)
	}
	now := s.now()
	return s.Repo.MutateReplay(ctx, id, func(tx *ReplayTx) error {
		if tx.J.State == ReplayCancelled {
			return nil
		}
		if tx.J.State != ReplayPendingApproval && tx.J.State != ReplayApproved {
			return ErrReplayState
		}
		tx.J.State, tx.J.DecisionNote = ReplayCancelled, reason
		t := now
		tx.J.FinishedAt = &t
		tx.Out = append(tx.Out, s.replayEvent("integration.replay.cancelled", tx.J, a.PrincipalID))
		return nil
	})
}

// DriveReplays runs every approved replay the store says is due.
func (s *Service) DriveReplays(ctx context.Context, limit int) (int, error) {
	due, err := s.Repo.DueReplays(ctx, limit)
	if err != nil {
		return 0, err
	}
	var first error
	for _, d := range due {
		if _, err := s.RunReplay(ctx, d.TenantID, d.ID); err != nil && first == nil && !errors.Is(err, ErrDependency) {
			first = err
		}
	}
	return len(due), first
}

// RunReplay executes an approved replay. It is idempotent and resumable: an exchange already replayed is not
// replayed again, and an interrupted run continues where it stopped.
func (s *Service) RunReplay(ctx context.Context, tenant, id string) (*ReplayJob, error) {
	ictx := withTenant(ctx, tenant)
	now := s.now()
	lease := now.Add(s.cfg().ReplayLease)
	job, err := s.Repo.MutateReplay(ictx, id, func(tx *ReplayTx) error {
		switch {
		case tx.J.State == ReplayApproved:
			tx.J.State = ReplayRunning
			t := now
			tx.J.StartedAt = &t
			tx.Out = append(tx.Out, s.replayEvent("integration.replay.started", tx.J, "system:xic-replay"))
		case tx.J.State == ReplayRunning && (tx.J.LeaseUntil == nil || !tx.J.LeaseUntil.After(now)):
		default:
			return errNotDue
		}
		tx.J.LeaseUntil = &lease
		return nil
	})
	if errors.Is(err, errNotDue) {
		return s.Repo.GetReplay(ictx, id)
	}
	if err != nil {
		return nil, err
	}
	items, err := s.Repo.ReplayItems(ictx, id, 0)
	if err != nil {
		return nil, err
	}
	var pin *registry.ContractVersion
	if job.UseCurrentMapping {
		conn, cerr := s.Conns.Connection(ictx, job.ConnectionID)
		if cerr == nil {
			pin, cerr = s.Conns.PinnedContract(ictx, conn)
		}
		if cerr != nil {
			s.releaseReplay(ictx, id)
			return job, fmt.Errorf("%w: %v", ErrDependency, cerr)
		}
	}
	counts := ReplayCounts{Selected: len(items)}
	var batch []ReplayItem
	for i := range items {
		it := &items[i]
		if it.Done {
			tallyItem(&counts, it)
			continue
		}
		r, rerr := s.In.Replay(ictx, inbound.ReplayInput{ExchangeID: it.ExchangeID, Pin: pin, Actor: inbound.Actor{PrincipalID: "system:xic-replay", CorrelationID: job.CorrelationID}, JobID: job.ReplayID})
		switch {
		case rerr == nil:
			fillItem(it, r)
			tally(&counts, r, true)
		case errors.Is(rerr, inbound.ErrDependency):
			_ = s.Repo.UpdateReplayItems(ictx, batch)
			s.releaseReplay(ictx, id)
			return job, fmt.Errorf("%w: %v", ErrDependency, rerr)
		default:
			it.Skipped = "ERROR"
			counts.Failed++
		}
		it.Done = true
		batch = append(batch, *it)
		if len(batch) >= 100 {
			if err := s.Repo.UpdateReplayItems(ictx, batch); err != nil {
				return job, err
			}
			batch = batch[:0]
		}
	}
	if err := s.Repo.UpdateReplayItems(ictx, batch); err != nil {
		return job, err
	}
	fin := s.now()
	job, err = s.Repo.MutateReplay(ictx, id, func(tx *ReplayTx) error {
		if tx.J.State != ReplayRunning {
			return errNotDue
		}
		tx.J.State, tx.J.LeaseUntil, tx.J.FinishedAt = ReplayCompleted, nil, &fin
		tx.J.Result = mustJSON(counts)
		tx.Out = append(tx.Out, s.replayEvent("integration.replay.completed", tx.J, "system:xic-replay"))
		return nil
	})
	if err != nil && !errors.Is(err, errNotDue) {
		return nil, err
	}
	if job != nil && job.BatchID != "" {
		_, _ = s.Refresh(ictx, tenant, job.BatchID) // the batch reports on what the replay changed
	}
	s.observe("replay_completed", job.Risk)
	return job, nil
}

func tallyItem(c *ReplayCounts, it *ReplayItem) {
	if it.Skipped != "" {
		if it.Skipped == "ERROR" {
			c.Failed++
		} else {
			c.Skipped++
		}
		return
	}
	c.Replayable++
	switch it.AfterState {
	case inbound.StateValidated:
		c.WouldAccept++
	case inbound.StateQuarantined:
		c.Quarantined++
	}
	if it.Changed {
		c.Changed++
	}
	if it.Applied {
		c.Applied++
	}
}

func (s *Service) releaseReplay(ctx context.Context, id string) {
	_, _ = s.Repo.MutateReplay(ctx, id, func(tx *ReplayTx) error {
		tx.J.LeaseUntil = nil
		return nil
	})
}

// sameJSON compares two JSON documents by value, so a store that normalises its JSON (key order, spacing) does not
// make the same scope look different.
func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
