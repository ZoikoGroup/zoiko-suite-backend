package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"zoiko.io/fiscal-calendar-svc/internal/domain"
	"zoiko.io/fiscal-calendar-svc/internal/events"
)

// Command names used for idempotency scoping and metrics.
const (
	CommandCreate        = "create-calendar"
	CommandProposeChange = "propose-change"
	CommandApprove       = "approve"
	CommandActivate      = "activate"
	CommandCreatePlan    = "create-transition-plan"
	CommandApprovePlan   = "approve-plan"
	CommandRejectPlan    = "reject-plan"
)

var (
	codeRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	scopeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
)

// CalendarWithVersion is the result of CreateFiscalCalendar.
type CalendarWithVersion struct {
	Calendar *domain.FiscalCalendar        `json:"calendar"`
	Version  *domain.FiscalCalendarVersion `json:"version"`
}

// VersionSpec is the protected definition carried by a new version.
type VersionSpec struct {
	Pattern       json.RawMessage
	StartMonth    int
	StartDay      int
	EffectiveFrom domain.Date
	EffectiveTo   *domain.Date
}

// validate checks a version definition and returns the canonical pattern. It
// also computes the fiscal year of effective_from, so a definition that cannot
// produce periods is refused at proposal time rather than at activation.
func (sp VersionSpec) validate() (*domain.Pattern, error) {
	p, err := domain.ParsePattern(sp.Pattern)
	if err != nil {
		return nil, err
	}
	if err := domain.ValidateStart(sp.StartMonth, sp.StartDay); err != nil {
		return nil, err
	}
	if sp.EffectiveFrom.IsZero() {
		return nil, domain.Errf(domain.CodeContextInvalid, "effective_from is required")
	}
	if sp.EffectiveTo != nil && !sp.EffectiveTo.After(sp.EffectiveFrom) {
		return nil, domain.Errf(domain.CodeContextInvalid, "effective_to must be after effective_from")
	}
	probe := &domain.FiscalCalendarVersion{Pattern: *p, FiscalYearStartMonth: sp.StartMonth, FiscalYearStartDay: sp.StartDay}
	if _, err := domain.PreviewPeriods(probe, sp.EffectiveFrom.Year()); err != nil {
		return nil, err
	}
	return p, nil
}

// ── CreateFiscalCalendar ─────────────────────────────────────────────────────

// CreateCalendarInput is CreateFiscalCalendar: the calendar header plus its
// first DRAFT version. Meta.LegalEntityID is the entity the calendar belongs to.
type CreateCalendarInput struct {
	Meta
	Code  string
	Scope string
	VersionSpec
}

// CreateFiscalCalendar creates a calendar and its first DRAFT version.
func (s *Service) CreateFiscalCalendar(ctx context.Context, in CreateCalendarInput) (*CalendarWithVersion, bool, error) {
	if err := in.Meta.check(); err != nil {
		return nil, false, err
	}
	if !codeRe.MatchString(in.Code) {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "code must be 1-64 characters of letters, digits, '.', '_' or '-'")
	}
	if !scopeRe.MatchString(in.Scope) {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "scope must be 1-64 characters of letters, digits, '.', '_', ':' or '-'")
	}
	pattern, err := in.VersionSpec.validate()
	if err != nil {
		return nil, false, err
	}
	return run(ctx, s, in.Meta, CommandCreate, func(tx Tx) (*CalendarWithVersion, error) {
		existing, err := tx.FindCalendarByCode(ctx, in.LegalEntityID, in.Code)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return nil, domain.Errf(domain.CodeDuplicateCandidate, "calendar %q already exists for this legal entity (calendar_id %s)", in.Code, existing.CalendarID)
		}
		now := s.now()
		cal := &domain.FiscalCalendar{
			CalendarID: s.newID(), TenantID: in.TenantID, LegalEntityID: in.LegalEntityID, Code: in.Code, Scope: in.Scope,
			Status: domain.CalendarDraft, Version: 1, CreatedBy: in.Actor, CreatedAt: now, RecordedAt: now,
		}
		if err := tx.InsertCalendar(ctx, cal); err != nil {
			return nil, err
		}
		v := newVersion(s.newID(), cal, 1, *pattern, in.VersionSpec, in.Meta, now)
		if err := tx.InsertVersion(ctx, v); err != nil {
			return nil, err
		}
		if err := tx.InsertStatusHistory(ctx, &domain.StatusHistoryEntry{
			HistoryID: s.newID(), VersionID: v.VersionID, ToStatus: domain.VersionDraft, Actor: in.Actor, Reason: in.Reason,
			RecordedAt: now, ResultingVersion: v.Version, CorrelationID: in.CorrelationID,
		}); err != nil {
			return nil, err
		}
		if err := s.enqueue(ctx, tx, events.Event{
			Type: events.EventFiscalCalendarCreated, TenantID: in.TenantID, Scope: events.ScopeTenant,
			ObjectType: events.ObjectTypeFiscalCalendar, ObjectID: cal.CalendarID, ObjectVersion: cal.Version,
			EffectiveAt: midnightUTC(v.EffectiveFrom), RecordedAt: now, Actor: in.Actor, CorrelationID: in.CorrelationID, CausationID: in.CausationID,
			Data: map[string]any{
				"calendar_id": cal.CalendarID, "legal_entity_id": cal.LegalEntityID, "code": cal.Code, "calendar_scope": cal.Scope,
				"version_id": v.VersionID, "version_no": v.VersionNo, "reason": in.Reason,
			},
		}); err != nil {
			return nil, err
		}
		return &CalendarWithVersion{Calendar: cal, Version: v}, nil
	})
}

func newVersion(id string, cal *domain.FiscalCalendar, no int, p domain.Pattern, sp VersionSpec, m Meta, now time.Time) *domain.FiscalCalendarVersion {
	return &domain.FiscalCalendarVersion{
		VersionID: id, CalendarID: cal.CalendarID, TenantID: cal.TenantID, LegalEntityID: cal.LegalEntityID, Scope: cal.Scope,
		VersionNo: no, Pattern: p, FiscalYearStartMonth: sp.StartMonth, FiscalYearStartDay: sp.StartDay,
		EffectiveFrom: sp.EffectiveFrom, EffectiveTo: sp.EffectiveTo, Status: domain.VersionDraft,
		ProposedBy: m.Actor, ProposalReason: m.Reason, RecordedAt: now, Version: 1,
	}
}

// ── ProposeCalendarChange ────────────────────────────────────────────────────

// ProposeChangeInput is ProposeCalendarChange: a new DRAFT version of an
// existing calendar. ExpectedVersion is the calendar's current version.
type ProposeChangeInput struct {
	Meta
	CalendarID      string
	ExpectedVersion int64
	VersionSpec
}

// ProposeCalendarChange appends a new DRAFT version. It never edits an existing
// version: a change to an approved definition is always a new version.
func (s *Service) ProposeCalendarChange(ctx context.Context, in ProposeChangeInput) (*domain.FiscalCalendarVersion, bool, error) {
	if err := in.Meta.check(); err != nil {
		return nil, false, err
	}
	if in.CalendarID == "" || in.ExpectedVersion < 1 {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "calendar id and expected_version (>= 1) are required")
	}
	pattern, err := in.VersionSpec.validate()
	if err != nil {
		return nil, false, err
	}
	return run(ctx, s, in.Meta, CommandProposeChange+":"+in.CalendarID, func(tx Tx) (*domain.FiscalCalendarVersion, error) {
		cal, err := tx.GetCalendarByID(ctx, in.CalendarID, true)
		if err != nil {
			return nil, err
		}
		if cal == nil {
			return nil, domain.Errf(domain.CodeNotFound, "fiscal calendar %s not found", in.CalendarID)
		}
		if err := requireEntity(in.Meta, cal.LegalEntityID); err != nil {
			return nil, err
		}
		if cal.Version != in.ExpectedVersion {
			return nil, domain.Errf(domain.CodeVersionConflict, "expected_version %d does not match current calendar version %d", in.ExpectedVersion, cal.Version)
		}
		vs, err := tx.ListVersions(ctx, cal.CalendarID)
		if err != nil {
			return nil, err
		}
		next := 1
		for _, e := range vs {
			if e.VersionNo >= next {
				next = e.VersionNo + 1
			}
		}
		now := s.now()
		v := newVersion(s.newID(), cal, next, *pattern, in.VersionSpec, in.Meta, now)
		if err := tx.InsertVersion(ctx, v); err != nil {
			return nil, err
		}
		prev := cal.Version
		cal.Version++
		cal.RecordedAt = now
		if err := tx.UpdateCalendar(ctx, cal, prev); err != nil {
			if err == domain.ErrVersionConflictStore {
				return nil, domain.Errf(domain.CodeVersionConflict, "calendar changed concurrently; re-read and retry")
			}
			return nil, err
		}
		if err := tx.InsertStatusHistory(ctx, &domain.StatusHistoryEntry{
			HistoryID: s.newID(), VersionID: v.VersionID, ToStatus: domain.VersionDraft, Actor: in.Actor, Reason: in.Reason,
			RecordedAt: now, ResultingVersion: v.Version, CorrelationID: in.CorrelationID,
		}); err != nil {
			return nil, err
		}
		if err := s.enqueue(ctx, tx, events.Event{
			Type: events.EventFiscalCalendarChangeProposed, TenantID: in.TenantID, Scope: events.ScopeTenant,
			ObjectType: events.ObjectTypeFiscalCalendarVersion, ObjectID: v.VersionID, ObjectVersion: v.Version,
			EffectiveAt: midnightUTC(v.EffectiveFrom), RecordedAt: now, Actor: in.Actor, CorrelationID: in.CorrelationID, CausationID: in.CausationID,
			Data: map[string]any{
				"calendar_id": cal.CalendarID, "legal_entity_id": cal.LegalEntityID, "version_no": v.VersionNo, "reason": in.Reason,
			},
		}); err != nil {
			return nil, err
		}
		return v, nil
	})
}

// ── ApproveCalendarTransition (version) / ActivateCalendarVersion ────────────

// VersionCommandInput targets one version with its current version number.
type VersionCommandInput struct {
	Meta
	VersionID       string
	ExpectedVersion int64
}

func (in VersionCommandInput) check() error {
	if err := in.Meta.check(); err != nil {
		return err
	}
	if in.VersionID == "" || in.ExpectedVersion < 1 {
		return domain.Errf(domain.CodeContextInvalid, "version id and expected_version (>= 1) are required")
	}
	return nil
}

// loadVersion applies the shared check order: existence (NOT_FOUND) ->
// entity context -> expected_version (VERSION_CONFLICT).
func loadVersion(ctx context.Context, tx Tx, in VersionCommandInput) (*domain.FiscalCalendarVersion, error) {
	v, err := tx.GetVersionByID(ctx, in.VersionID, true)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, domain.Errf(domain.CodeNotFound, "fiscal calendar version %s not found", in.VersionID)
	}
	if err := requireEntity(in.Meta, v.LegalEntityID); err != nil {
		return nil, err
	}
	if v.Version != in.ExpectedVersion {
		return nil, domain.Errf(domain.CodeVersionConflict, "expected_version %d does not match current version %d", in.ExpectedVersion, v.Version)
	}
	return v, nil
}

// ApproveVersion moves a DRAFT version to APPROVED. The proposer may not
// approve their own proposal (SOD_DENIED). From APPROVED on, the version's
// pattern and effective dates are immutable (domain rule and database trigger).
func (s *Service) ApproveVersion(ctx context.Context, in VersionCommandInput) (*domain.FiscalCalendarVersion, bool, error) {
	if err := in.check(); err != nil {
		return nil, false, err
	}
	return run(ctx, s, in.Meta, CommandApprove+":"+in.VersionID, func(tx Tx) (*domain.FiscalCalendarVersion, error) {
		v, err := loadVersion(ctx, tx, in)
		if err != nil {
			return nil, err
		}
		if err := domain.ValidateTransition(v.Status, domain.VersionApproved); err != nil {
			return nil, err
		}
		if v.ProposedBy == in.Actor {
			return nil, domain.Errf(domain.CodeSoDDenied, "the proposer of a calendar version may not approve it; a different actor must")
		}
		now := s.now()
		prev := v.Version
		v.Status = domain.VersionApproved
		v.ApprovedBy, v.ApprovalReason, v.ApprovedAt = in.Actor, in.Reason, &now
		v.Version++
		v.RecordedAt = now
		if err := s.saveVersion(ctx, tx, v, prev); err != nil {
			return nil, err
		}
		if err := tx.InsertStatusHistory(ctx, &domain.StatusHistoryEntry{
			HistoryID: s.newID(), VersionID: v.VersionID, FromStatus: domain.VersionDraft, ToStatus: domain.VersionApproved,
			Actor: in.Actor, Reason: in.Reason, RecordedAt: now, ResultingVersion: v.Version, CorrelationID: in.CorrelationID,
		}); err != nil {
			return nil, err
		}
		return v, nil
	})
}

func (s *Service) saveVersion(ctx context.Context, tx Tx, v *domain.FiscalCalendarVersion, expected int64) error {
	if err := tx.UpdateVersion(ctx, v, expected); err != nil {
		if err == domain.ErrVersionConflictStore {
			return domain.Errf(domain.CodeVersionConflict, "version changed concurrently; re-read and retry")
		}
		return err
	}
	return nil
}

// ActivateVersion moves an APPROVED version to ACTIVE and supersedes the
// calendar's open-ended ACTIVE predecessor (ending it where this version
// begins). Guards, in order: lifecycle, effective-interval overlap (one
// in-force version per entity/scope/date), and, for any version that is not the
// first of its calendar, the history check against REF-05 (fail closed).
func (s *Service) ActivateVersion(ctx context.Context, in VersionCommandInput) (*domain.FiscalCalendarVersion, bool, error) {
	if err := in.check(); err != nil {
		return nil, false, err
	}
	return run(ctx, s, in.Meta, CommandActivate+":"+in.VersionID, func(tx Tx) (*domain.FiscalCalendarVersion, error) {
		return s.doActivate(ctx, tx, in)
	})
}

func (s *Service) doActivate(ctx context.Context, tx Tx, in VersionCommandInput) (*domain.FiscalCalendarVersion, error) {
	v, err := loadVersion(ctx, tx, in)
	if err != nil {
		return nil, err
	}
	if err := domain.ValidateTransition(v.Status, domain.VersionActive); err != nil {
		return nil, err
	}
	// Serialise activations of one (entity, scope): the overlap check below
	// reads rows another activation could be writing.
	if err := tx.LockKey(ctx, "scope:"+in.TenantID+":"+v.LegalEntityID+":"+v.Scope); err != nil {
		return nil, err
	}
	cal, err := tx.GetCalendarByID(ctx, v.CalendarID, true)
	if err != nil {
		return nil, err
	}
	if cal == nil {
		return nil, fmt.Errorf("calendar %s of version %s is missing", v.CalendarID, v.VersionID)
	}
	inForce, err := tx.ListInForceVersions(ctx, v.LegalEntityID, v.Scope)
	if err != nil {
		return nil, err
	}

	// The calendar's open-ended ACTIVE predecessor is ended where v begins.
	var closable *domain.FiscalCalendarVersion
	for i := range inForce {
		o := &inForce[i]
		if o.CalendarID == v.CalendarID && o.Status == domain.VersionActive && o.EffectiveTo == nil && o.EffectiveFrom.Before(v.EffectiveFrom) {
			closable = o
			break
		}
	}
	for i := range inForce {
		o := &inForce[i]
		if closable != nil && o.VersionID == closable.VersionID {
			continue
		}
		if o.Interval().Overlaps(v.Interval()) {
			return nil, domain.Errf(domain.CodeInvalidTransition,
				"effective interval of version %s overlaps %s version %s (calendar %s, effective %s onward); only one calendar version may be in force for an entity and scope on any date",
				v.VersionID, o.Status, o.VersionID, o.CalendarID, o.EffectiveFrom)
		}
	}

	var planID string
	if v.VersionNo > 1 {
		planID, err = s.requirePlanIfHistoryTouched(ctx, tx, in.Meta, v, closable)
		if err != nil {
			return nil, err
		}
	}

	now := s.now()
	if closable != nil {
		prevC := closable.Version
		to := v.EffectiveFrom
		closable.EffectiveTo = &to
		closable.Status = domain.VersionSuperseded
		closable.SupersededByVersion = v.VersionID
		closable.Version++
		closable.RecordedAt = now
		if err := s.saveVersion(ctx, tx, closable, prevC); err != nil {
			return nil, err
		}
		if err := tx.InsertStatusHistory(ctx, &domain.StatusHistoryEntry{
			HistoryID: s.newID(), VersionID: closable.VersionID, FromStatus: domain.VersionActive, ToStatus: domain.VersionSuperseded,
			Actor: in.Actor, Reason: in.Reason, RecordedAt: now, ResultingVersion: closable.Version, CorrelationID: in.CorrelationID,
		}); err != nil {
			return nil, err
		}
		if err := s.enqueue(ctx, tx, events.Event{
			Type: events.EventFiscalCalendarSuperseded, TenantID: in.TenantID, Scope: events.ScopeTenant,
			ObjectType: events.ObjectTypeFiscalCalendarVersion, ObjectID: closable.VersionID, ObjectVersion: closable.Version,
			EffectiveAt: midnightUTC(v.EffectiveFrom), RecordedAt: now, Actor: in.Actor, CorrelationID: in.CorrelationID, CausationID: in.CausationID,
			Data: map[string]any{
				"calendar_id": closable.CalendarID, "legal_entity_id": closable.LegalEntityID, "version_no": closable.VersionNo,
				"superseded_by_version_id": v.VersionID, "effective_to": v.EffectiveFrom, "reason": in.Reason,
			},
		}); err != nil {
			return nil, err
		}
	}

	prev := v.Version
	v.Status = domain.VersionActive
	v.ActivatedBy, v.ActivatedAt = in.Actor, &now
	v.Version++
	v.RecordedAt = now
	if err := s.saveVersion(ctx, tx, v, prev); err != nil {
		return nil, err
	}
	if err := tx.InsertStatusHistory(ctx, &domain.StatusHistoryEntry{
		HistoryID: s.newID(), VersionID: v.VersionID, FromStatus: domain.VersionApproved, ToStatus: domain.VersionActive,
		Actor: in.Actor, Reason: in.Reason, RecordedAt: now, ResultingVersion: v.Version, CorrelationID: in.CorrelationID,
	}); err != nil {
		return nil, err
	}
	prevCal := cal.Version
	cal.Status = domain.CalendarActive
	cal.Version++
	cal.RecordedAt = now
	if err := tx.UpdateCalendar(ctx, cal, prevCal); err != nil {
		if err == domain.ErrVersionConflictStore {
			return nil, domain.Errf(domain.CodeVersionConflict, "calendar changed concurrently; re-read and retry")
		}
		return nil, err
	}
	data := map[string]any{
		"calendar_id": v.CalendarID, "legal_entity_id": v.LegalEntityID, "calendar_scope": v.Scope, "version_no": v.VersionNo,
		"effective_from": v.EffectiveFrom, "effective_to": v.EffectiveTo, "reason": in.Reason,
	}
	if closable != nil {
		data["previous_version_id"] = closable.VersionID
	}
	if planID != "" {
		data["transition_plan_id"] = planID
	}
	if err := s.enqueue(ctx, tx, events.Event{
		Type: events.EventFiscalCalendarVersionActivated, TenantID: in.TenantID, Scope: events.ScopeTenant,
		ObjectType: events.ObjectTypeFiscalCalendarVersion, ObjectID: v.VersionID, ObjectVersion: v.Version,
		EffectiveAt: midnightUTC(v.EffectiveFrom), RecordedAt: now, Actor: in.Actor, CorrelationID: in.CorrelationID, CausationID: in.CausationID,
		Data: data,
	}); err != nil {
		return nil, err
	}
	return v, nil
}

// requirePlanIfHistoryTouched enforces "a change that overlaps posted/closed
// history requires a transition plan; never rewrite historical periods".
//
// This service does not know what has been posted (that is REF-05). It asks
// REF-05; if it cannot get an answer it FAILS CLOSED. A change touches history
// when REF-05 reports posted or closed periods and the new version takes effect
// on or before the end of the latest of them. In that case an APPROVED plan
// that declares the posted-period impact, and that originates from the version
// being superseded, must exist. Returns the plan id used, if any.
func (s *Service) requirePlanIfHistoryTouched(ctx context.Context, tx Tx, m Meta, v, closable *domain.FiscalCalendarVersion) (string, error) {
	if s.history == nil {
		return "", domain.Errf(domain.CodeDependencyUnavailable,
			"period history (accounting-period-svc) is not configured; cannot verify that this calendar change leaves posted/closed periods untouched; failing closed")
	}
	h, err := s.history.CalendarUsage(ctx, m.TenantID, v.LegalEntityID, v.CalendarID)
	if err != nil || h == nil {
		return "", domain.Errf(domain.CodeDependencyUnavailable,
			"period history (accounting-period-svc) unavailable; cannot verify that this calendar change leaves posted/closed periods untouched; failing closed")
	}
	touches := h.HasPostedOrClosedPeriods && (h.LatestPeriodEnd == nil || !v.EffectiveFrom.After(*h.LatestPeriodEnd))
	if !touches {
		return "", nil
	}
	plans, err := tx.ListPlansForVersion(ctx, v.VersionID)
	if err != nil {
		return "", err
	}
	for _, p := range plans {
		if p.Status != domain.PlanApproved || !p.AffectsPostedPeriods {
			continue
		}
		if closable != nil && p.FromVersionID != closable.VersionID {
			continue
		}
		return p.PlanID, nil
	}
	latest := "an unknown date"
	if h.LatestPeriodEnd != nil {
		latest = h.LatestPeriodEnd.String()
	}
	return "", domain.Errf(domain.CodeTransitionPlanRequired,
		"version %s takes effect %s, on or before %s, the end of the latest posted/closed period; an APPROVED transition plan declaring posted-period impact is required (historical periods are never rewritten)",
		v.VersionID, v.EffectiveFrom, latest)
}

// ── CalendarTransitionPlan ───────────────────────────────────────────────────

// CreatePlanInput creates a CalendarTransitionPlan from a version that has been
// in force to the version that will replace it. ExpectedVersion is the
// to-version's current version (it is checked, not bumped).
type CreatePlanInput struct {
	Meta
	ToVersionID          string
	FromVersionID        string
	ExpectedVersion      int64
	ImpactAssessment     json.RawMessage
	Mapping              json.RawMessage
	AffectsPostedPeriods bool
}

// CreateTransitionPlan records the transition plan, status PROPOSED.
func (s *Service) CreateTransitionPlan(ctx context.Context, in CreatePlanInput) (*domain.CalendarTransitionPlan, bool, error) {
	if err := in.Meta.check(); err != nil {
		return nil, false, err
	}
	if in.ToVersionID == "" || in.FromVersionID == "" || in.ExpectedVersion < 1 {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "version id, from_version_id and expected_version (>= 1) are required")
	}
	impact, mapping, err := domain.ValidatePlanContent(in.ImpactAssessment, in.Mapping, in.AffectsPostedPeriods)
	if err != nil {
		return nil, false, err
	}
	return run(ctx, s, in.Meta, CommandCreatePlan+":"+in.ToVersionID, func(tx Tx) (*domain.CalendarTransitionPlan, error) {
		to, err := tx.GetVersionByID(ctx, in.ToVersionID, true)
		if err != nil {
			return nil, err
		}
		if to == nil {
			return nil, domain.Errf(domain.CodeNotFound, "fiscal calendar version %s not found", in.ToVersionID)
		}
		if err := requireEntity(in.Meta, to.LegalEntityID); err != nil {
			return nil, err
		}
		if to.Version != in.ExpectedVersion {
			return nil, domain.Errf(domain.CodeVersionConflict, "expected_version %d does not match current version %d", in.ExpectedVersion, to.Version)
		}
		if to.Status != domain.VersionDraft && to.Status != domain.VersionApproved {
			return nil, domain.Errf(domain.CodeInvalidTransition, "a transition plan can only be created for a DRAFT or APPROVED version, not %s", to.Status)
		}
		from, err := tx.GetVersionByID(ctx, in.FromVersionID, false)
		if err != nil {
			return nil, err
		}
		if from == nil {
			return nil, domain.Errf(domain.CodeNotFound, "fiscal calendar version %s not found", in.FromVersionID)
		}
		if from.CalendarID != to.CalendarID || from.VersionID == to.VersionID || from.VersionNo >= to.VersionNo {
			return nil, domain.Errf(domain.CodeContextInvalid, "from_version_id must be an earlier version of the same calendar")
		}
		if !from.Status.InForce() {
			return nil, domain.Errf(domain.CodeInvalidTransition, "from version is %s; a transition leaves a version that has been in force (ACTIVE or SUPERSEDED)", from.Status)
		}
		existing, err := tx.ListPlansForVersion(ctx, to.VersionID)
		if err != nil {
			return nil, err
		}
		for _, p := range existing {
			if p.Status != domain.PlanRejected {
				return nil, domain.Errf(domain.CodeDuplicateCandidate, "version %s already has a %s transition plan (%s)", to.VersionID, p.Status, p.PlanID)
			}
		}
		now := s.now()
		p := &domain.CalendarTransitionPlan{
			PlanID: s.newID(), TenantID: in.TenantID, LegalEntityID: to.LegalEntityID, CalendarID: to.CalendarID,
			FromVersionID: from.VersionID, ToVersionID: to.VersionID, ImpactAssessment: impact, AffectsPostedPeriods: in.AffectsPostedPeriods,
			Mapping: mapping, Status: domain.PlanProposed, ProposedBy: in.Actor, Reason: in.Reason, Version: 1, CreatedAt: now,
		}
		if err := tx.InsertPlan(ctx, p); err != nil {
			return nil, err
		}
		return p, nil
	})
}

// PlanDecisionInput approves or rejects a plan.
type PlanDecisionInput struct {
	Meta
	PlanID          string
	ExpectedVersion int64
	Approve         bool
}

// DecideTransitionPlan is ApproveCalendarTransition (and its reject twin). The
// plan's proposer, and the proposer of the version it leads to, may not approve
// it (SOD_DENIED).
func (s *Service) DecideTransitionPlan(ctx context.Context, in PlanDecisionInput) (*domain.CalendarTransitionPlan, bool, error) {
	if err := in.Meta.check(); err != nil {
		return nil, false, err
	}
	if in.PlanID == "" || in.ExpectedVersion < 1 {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "plan id and expected_version (>= 1) are required")
	}
	op := CommandRejectPlan
	target := domain.PlanRejected
	if in.Approve {
		op, target = CommandApprovePlan, domain.PlanApproved
	}
	return run(ctx, s, in.Meta, op+":"+in.PlanID, func(tx Tx) (*domain.CalendarTransitionPlan, error) {
		p, err := tx.GetPlanByID(ctx, in.PlanID, true)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, domain.Errf(domain.CodeNotFound, "calendar transition plan %s not found", in.PlanID)
		}
		if err := requireEntity(in.Meta, p.LegalEntityID); err != nil {
			return nil, err
		}
		if p.Version != in.ExpectedVersion {
			return nil, domain.Errf(domain.CodeVersionConflict, "expected_version %d does not match current plan version %d", in.ExpectedVersion, p.Version)
		}
		if err := domain.ValidatePlanDecision(p.Status, target); err != nil {
			return nil, err
		}
		to, err := tx.GetVersionByID(ctx, p.ToVersionID, false)
		if err != nil {
			return nil, err
		}
		if to == nil {
			return nil, fmt.Errorf("to-version %s of plan %s is missing", p.ToVersionID, p.PlanID)
		}
		if in.Approve {
			if to.Status != domain.VersionDraft && to.Status != domain.VersionApproved {
				return nil, domain.Errf(domain.CodeInvalidTransition, "the plan's target version is already %s; a plan cannot be approved after the fact", to.Status)
			}
			if p.ProposedBy == in.Actor || to.ProposedBy == in.Actor {
				return nil, domain.Errf(domain.CodeSoDDenied, "the proposer of a transition plan, or of the version it leads to, may not approve it; a different actor must")
			}
		}
		now := s.now()
		prev := p.Version
		p.Status = target
		p.DecidedBy, p.DecisionReason, p.DecidedAt = in.Actor, in.Reason, &now
		p.Version++
		if err := tx.UpdatePlan(ctx, p, prev); err != nil {
			if err == domain.ErrVersionConflictStore {
				return nil, domain.Errf(domain.CodeVersionConflict, "plan changed concurrently; re-read and retry")
			}
			return nil, err
		}
		return p, nil
	})
}
