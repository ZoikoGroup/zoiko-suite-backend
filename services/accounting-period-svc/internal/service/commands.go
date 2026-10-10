package service

import (
	"context"
	"time"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/events"
)

// ReopenRequest is the scope and time bound of an AuthorizeReopen.
type ReopenRequest struct {
	BookScope   string
	ModuleScope string
	ExpiresAt   *time.Time
}

// CommandInput is RequestSoftClose / SetHardClosed / AuthorizeReopen / ReclosePeriod.
type CommandInput struct {
	Meta
	PeriodID           string
	Command            domain.Command
	ExpectedVersion    int64
	Acc14WorkflowRef   string
	ControlSnapshotRef string
	// Reopen is required for AUTHORIZE_REOPEN and ignored otherwise.
	Reopen *ReopenRequest
}

func eventTypeFor(c domain.Command) string {
	switch c {
	case domain.CmdSoftClose:
		return events.EventPeriodSoftClosed
	case domain.CmdHardClose:
		return events.EventPeriodHardClosed
	case domain.CmdAuthorizeReopen:
		return events.EventPeriodReopened
	}
	return events.EventPeriodReclosed
}

// Command applies one named state command to a period.
//
// Order of checks, each with its own typed error:
//
//	context (CONTEXT_INVALID) -> existence (NOT_FOUND) ->
//	ACC-14 refs present (CONTEXT_INVALID, control event) ->
//	reopen scope/window (CONTEXT_INVALID) -> expected_version (VERSION_CONFLICT) ->
//	lifecycle (INVALID_TRANSITION) -> ACC-14 verification (SOURCE_UNVERIFIED /
//	DEPENDENCY_UNAVAILABLE, control event) -> [write tx, re-checked under row lock]
//	segregation of duties (SOD_DENIED, control event).
//
// Provenance is verified BEFORE the write transaction (no network call while a
// row lock is held) and the version/lifecycle checks are repeated inside it.
// A refusal because of missing/failed provenance or SoD records a
// PeriodCommandRejected control event in its own transaction (the command's
// transaction rolls back, and the refusal must still leave evidence).
func (s *Service) Command(ctx context.Context, in CommandInput) (*domain.Period, bool, error) {
	if err := in.Meta.check(); err != nil {
		return nil, false, err
	}
	if !isStateCommand(in.Command) {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "unknown command %q", in.Command)
	}
	if in.PeriodID == "" {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "period id is required")
	}
	if in.ExpectedVersion < 1 {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "expected_version is required and must be >= 1")
	}
	op := string(in.Command) + ":" + in.PeriodID

	// Phase 1: replay, or load the period.
	var replay, cur *domain.Period
	err := s.store.InTx(ctx, in.TenantID, func(tx Tx) error {
		rec, err := tx.GetIdempotency(ctx, in.IdempotencyKey)
		if err != nil {
			return err
		}
		if replay, err = replayOrNil[domain.Period](rec, in.Meta, op); err != nil || replay != nil {
			return err
		}
		cur, err = tx.GetPeriod(ctx, in.PeriodID, false)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if replay != nil {
		return replay, true, nil
	}
	if cur == nil {
		return nil, false, domain.Errf(domain.CodeNotFound, "accounting period %s not found", in.PeriodID)
	}

	// Phase 2: checks that need no row lock.
	if in.Acc14WorkflowRef == "" || in.ControlSnapshotRef == "" {
		err := domain.Errf(domain.CodeContextInvalid,
			"%s requires ACC-14 evidence: acc14_workflow_ref and control_snapshot_ref are both required", in.Command)
		s.recordRejection(ctx, in, cur, err)
		return nil, false, err
	}
	var window *domain.ReopenWindow
	if in.Command == domain.CmdAuthorizeReopen {
		w, err := s.validateReopen(in, cur)
		if err != nil {
			return nil, false, err
		}
		window = w
	}
	if cur.Version != in.ExpectedVersion {
		return nil, false, domain.Errf(domain.CodeVersionConflict, "expected_version %d does not match current version %d", in.ExpectedVersion, cur.Version)
	}
	if _, err := domain.Next(in.Command, cur.State); err != nil {
		return nil, false, err
	}
	if err := s.provenance.Verify(ctx, ProvenanceRequest{
		TenantID: in.TenantID, LegalEntityID: cur.LegalEntityID, PeriodKey: cur.PeriodKey,
		Command: in.Command, Acc14WorkflowRef: in.Acc14WorkflowRef, ControlSnapshotRef: in.ControlSnapshotRef,
	}); err != nil {
		if de, ok := domain.AsError(err); ok {
			s.recordRejection(ctx, in, cur, de)
			return nil, false, de
		}
		de := domain.Errf(domain.CodeDependencyUnavailable, "ACC-14 provenance could not be verified; failing closed")
		s.recordRejection(ctx, in, cur, de)
		return nil, false, de
	}

	// Phase 3: the write.
	var res *domain.Period
	var replayed bool
	err = s.store.InTx(ctx, in.TenantID, func(tx Tx) error {
		out, rep, err := idempotent(ctx, tx, in.Meta, op, func() (*domain.Period, error) {
			return s.doCommand(ctx, tx, in, window)
		})
		if err != nil {
			return err
		}
		res, replayed = out, rep
		return nil
	})
	if err != nil {
		if de, ok := domain.AsError(err); ok && de.Code == domain.CodeSoDDenied {
			s.recordRejection(ctx, in, cur, de)
		}
		return nil, false, err
	}
	return res, replayed, nil
}

func isStateCommand(c domain.Command) bool {
	for _, v := range domain.StateCommands {
		if v == c {
			return true
		}
	}
	return false
}

// validateReopen checks the "time-bound / scoped" rule: expires_at is required,
// in the future, and no further than the configured maximum window; the scope
// may only narrow the period's own scope.
func (s *Service) validateReopen(in CommandInput, p *domain.Period) (*domain.ReopenWindow, error) {
	if in.Reopen == nil || in.Reopen.ExpiresAt == nil {
		return nil, domain.Errf(domain.CodeContextInvalid, "authorize-reopen requires reopen_scope and expires_at (a reopen is time-bound and scoped)")
	}
	now := s.now()
	exp := in.Reopen.ExpiresAt.UTC()
	if !exp.After(now) {
		return nil, domain.Errf(domain.CodeContextInvalid, "expires_at must be in the future")
	}
	if exp.After(now.Add(s.maxReopen)) {
		return nil, domain.Errf(domain.CodeContextInvalid, "expires_at is beyond the maximum reopen window of %s", s.maxReopen)
	}
	book, module := in.Reopen.BookScope, in.Reopen.ModuleScope
	if p.BookScope != "" && book != p.BookScope {
		return nil, domain.Errf(domain.CodeContextInvalid, "reopen book_scope must equal the period's book_scope %q", p.BookScope)
	}
	if p.ModuleScope != "" && module != p.ModuleScope {
		return nil, domain.Errf(domain.CodeContextInvalid, "reopen module_scope must equal the period's module_scope %q", p.ModuleScope)
	}
	return &domain.ReopenWindow{BookScope: book, ModuleScope: module, ExpiresAt: exp}, nil
}

func (s *Service) doCommand(ctx context.Context, tx Tx, in CommandInput, window *domain.ReopenWindow) (*domain.Period, error) {
	p, err := tx.GetPeriod(ctx, in.PeriodID, true)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, domain.Errf(domain.CodeNotFound, "accounting period %s not found", in.PeriodID)
	}
	if p.Version != in.ExpectedVersion {
		return nil, domain.Errf(domain.CodeVersionConflict, "expected_version %d does not match current version %d", in.ExpectedVersion, p.Version)
	}
	to, err := domain.Next(in.Command, p.State)
	if err != nil {
		return nil, err
	}
	hist, err := tx.ListHistory(ctx, p.PeriodID)
	if err != nil {
		return nil, err
	}
	fp := domain.DecisionFingerprint(p.PeriodID, in.Command, p.State, in.Acc14WorkflowRef, in.ControlSnapshotRef, window)
	for _, h := range hist {
		if h.DecisionFingerprint == fp {
			return nil, domain.Errf(domain.CodeContextInvalid, "this ACC-14 decision was already applied to the period")
		}
	}
	// SoD: hard close / reopen need an actor other than the soft-close requester.
	if domain.NeedsSoD(in.Command) {
		for i := len(hist) - 1; i >= 0; i-- {
			if hist[i].Command == domain.CmdSoftClose {
				if hist[i].RequestedBy == in.Actor {
					return nil, domain.Errf(domain.CodeSoDDenied,
						"%s must be requested by someone other than the actor who requested the soft close", in.Command)
				}
				break
			}
		}
	}

	now := s.now()
	prevState, prevVersion := p.State, p.Version
	p.State = to
	p.Version++
	p.UpdatedAt = now
	switch in.Command {
	case domain.CmdAuthorizeReopen:
		w := *window
		p.Reopen = &w
	case domain.CmdReclose:
		p.Reopen = nil // the window is closed; it stays in the history row that opened it
	}
	if err := tx.UpdatePeriodState(ctx, p, prevVersion); err != nil {
		if err == domain.ErrVersionConflictStore {
			return nil, domain.Errf(domain.CodeVersionConflict, "period changed concurrently; re-read and retry")
		}
		return nil, err
	}
	if err := tx.InsertHistory(ctx, &domain.HistoryEntry{
		HistoryID: s.newID(), TenantID: in.TenantID, PeriodID: p.PeriodID, FromState: prevState, ToState: to,
		Command: in.Command, Acc14WorkflowRef: in.Acc14WorkflowRef, ControlSnapshotRef: in.ControlSnapshotRef,
		RequestedBy: in.Actor, Reason: in.Reason, RecordedAt: now, ExpectedVersion: in.ExpectedVersion,
		ResultingVersion: p.Version, DecisionFingerprint: fp, Reopen: window, CorrelationID: in.CorrelationID,
	}); err != nil {
		return nil, err
	}
	data := periodEventData(p)
	data["command"] = string(in.Command)
	data["from_state"] = string(prevState)
	data["to_state"] = string(to)
	data["control_snapshot_ref"] = in.ControlSnapshotRef
	data["reason"] = in.Reason
	if window != nil {
		data["reopen_book_scope"] = window.BookScope
		data["reopen_module_scope"] = window.ModuleScope
		data["reopen_expires_at"] = window.ExpiresAt
	}
	if err := s.enqueue(ctx, tx, events.Event{
		Type: eventTypeFor(in.Command), TenantID: in.TenantID, Scope: events.ScopeTenant,
		ObjectType: events.ObjectTypeAccountingPeriod, ObjectID: p.PeriodID, ObjectVersion: p.Version,
		EffectiveAt: now, RecordedAt: now, Actor: in.Actor, CorrelationID: in.CorrelationID, CausationID: in.CausationID,
		Acc14WorkflowRef: in.Acc14WorkflowRef, Data: data,
	}); err != nil {
		return nil, err
	}
	return p, nil
}

// recordRejection writes the PeriodCommandRejected security/control event in
// its OWN transaction (the command's transaction rolled back). It is best
// effort by necessity: if it fails the caller still gets the original refusal,
// and the failure is reported through the audit-failure hook.
func (s *Service) recordRejection(ctx context.Context, in CommandInput, p *domain.Period, cause *domain.Error) {
	now := s.now()
	err := s.store.InTx(ctx, in.TenantID, func(tx Tx) error {
		return s.enqueue(ctx, tx, events.Event{
			Type: events.EventPeriodCommandRejected, TenantID: in.TenantID, Scope: events.ScopeTenant,
			ObjectType: events.ObjectTypeAccountingPeriod, ObjectID: p.PeriodID, ObjectVersion: p.Version,
			EffectiveAt: now, RecordedAt: now, Actor: in.Actor, CorrelationID: in.CorrelationID, CausationID: in.CausationID,
			Acc14WorkflowRef: in.Acc14WorkflowRef,
			Data: map[string]any{
				"legal_entity_id": p.LegalEntityID, "period_key": p.PeriodKey, "state": string(p.State),
				"command": string(in.Command), "rejection_code": string(cause.Code), "rejection_message": cause.Message,
				"control_snapshot_ref": in.ControlSnapshotRef, "reason": in.Reason,
			},
		})
	})
	if err != nil {
		s.auditFailed(err)
	}
}
