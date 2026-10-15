package ar09

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// liveView is the AR-07 truth the case reads at decision time. Nothing here is persisted as a balance.
type liveView struct {
	views []kit.OpenItemView
	aging *Aging
}

func (m *Module) live(ctx context.Context, tx pgx.Tx, cs *Case, now time.Time) (*liveView, error) {
	ids, err := caseItemIDs(ctx, tx, cs.TenantID, cs.CaseID)
	if err != nil {
		return nil, err
	}
	views := make([]kit.OpenItemView, 0, len(ids))
	for _, id := range ids {
		v, err := m.recv.GetByID(ctx, cs.TenantID, id)
		if err != nil {
			return nil, err
		}
		views = append(views, *v)
	}
	return &liveView{views: views, aging: BuildAging(views, cs.Currency, now)}, nil
}

// blockReason is the hard suppression set: an active dispute / hold, an inactive customer profile or a case hold. It is
// evaluated INSIDE the command transaction, after the case row is locked, on live AR-07 / AR-01 answers (#27).
func (m *Module) blockReason(ctx context.Context, tx pgx.Tx, cs *Case, lv *liveView, now time.Time) (string, error) {
	if cs.State == StateHeld {
		return SuppCaseHold, nil
	}
	if h, err := activeHold(ctx, tx, cs.TenantID, cs.CaseID); err != nil {
		return "", err
	} else if h != nil {
		return SuppCaseHold, nil
	}
	if _, err := m.profiles.ActiveAt(ctx, cs.TenantID, cs.LegalEntityID, cs.CustomerPartyID, now); err != nil {
		var p *kit.Problem
		if errors.As(err, &p) {
			if p.Code == kit.CodeCustomerProfileInactive {
				return SuppProfileInactive, nil
			}
			return "", err
		}
		return "", kit.Unavailable("customer profile unavailable")
	}
	for _, v := range lv.views {
		if v.Residual.IsPositive() && (v.Disputed || v.CollectionHold) {
			return SuppItemDisputeOrHold, nil
		}
	}
	blocked, err := m.recv.CollectionBlocked(ctx, cs.TenantID, cs.LegalEntityID, cs.CustomerPartyID)
	if err != nil {
		return "", err
	}
	if blocked {
		return SuppCollectionBlocked, nil
	}
	return "", nil
}

// contactReason is the contact-policy set: promise-to-pay grace, blackout and rate limiting (all per-policy data).
func (m *Module) contactReason(ctx context.Context, tx pgx.Tx, cs *Case, pol *Policy, now time.Time) (string, error) {
	p, err := latestPromise(ctx, tx, cs.TenantID, cs.CaseID)
	if err != nil {
		return "", err
	}
	if p != nil {
		d, _ := time.Parse("2006-01-02", p.PromisedDate)
		if !startOfDayUTC(now).After(d.AddDate(0, 0, pol.PromiseGraceDays)) {
			return SuppPromiseGrace, nil
		}
	}
	if r := pol.BlackoutReason(now); r != "" {
		return r, nil
	}
	if pol.MaxNoticesPerWindow > 0 {
		n, err := attemptsInWindow(ctx, tx, cs.TenantID, cs.LegalEntityID, cs.CustomerPartyID, now.AddDate(0, 0, -pol.WindowDays))
		if err != nil {
			return "", err
		}
		if n >= pol.MaxNoticesPerWindow {
			return SuppRateLimited, nil
		}
	}
	return "", nil
}

func (m *Module) suppression(ctx context.Context, tx pgx.Tx, cs *Case, pol *Policy, lv *liveView, now time.Time) (string, error) {
	if r, err := m.blockReason(ctx, tx, cs, lv, now); r != "" || err != nil {
		return r, err
	}
	return m.contactReason(ctx, tx, cs, pol, now)
}

// recordSuppression writes the evidence of a refused contact (once per stage and reason until something else happens).
func (m *Module) recordSuppression(ctx context.Context, tx pgx.Tx, c kit.Caller, cs *Case, st *Stage, reason string, automated bool, now time.Time) error {
	last, err := lastSuppression(ctx, tx, cs.TenantID, cs.CaseID, st.StageNo)
	if err != nil {
		return err
	}
	if last == reason {
		return nil
	}
	if err := insertAction(ctx, tx, c, cs, &Action{StageNo: st.StageNo, Channel: st.Channel, TemplateKey: st.TemplateKey, DeliveryResult: ResultSuppressed,
		SuppressionReason: reason, ActorID: c.PrincipalID, Automated: automated}, "", now); err != nil {
		return err
	}
	return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateCase, AggregateID: cs.CaseID, Command: "DunningSuppressed", ReasonCode: reason,
		Detail: map[string]any{"stage_no": st.StageNo, "automated": automated}})
}

func contactable(state string) bool {
	switch state {
	case StateEligible, StateActive, StatePromiseToPay, StateHeld, StateDisputed:
		return true
	}
	return false
}

// stepResult reports what an advance / send attempt did.
type stepResult struct {
	Done       bool    // the stage advanced / the notice was recorded as an attempt
	Suppressed string  // non-empty: refused for this reason (evidence recorded, nothing else changed)
	Action     *Action // the notice action when one was recorded
}

func errNoStageDue() error {
	return kit.Conflict(kit.CodeInvalidTransition, "no further dunning stage is due for this case")
}

// doAdvance moves the case to the next due stage unless contact is suppressed. cs must be locked by tx.
func (m *Module) doAdvance(ctx context.Context, tx pgx.Tx, c kit.Caller, cs *Case, pol *Policy, lv *liveView, now time.Time, automated bool) (stepResult, error) {
	if !contactable(cs.State) {
		return stepResult{}, invalidTransition(cs.State, StateActive)
	}
	st := pol.nextDueStage(cs.DunningStage, MaxDaysPastDue(lv.aging))
	if st == nil {
		return stepResult{}, errNoStageDue()
	}
	reason, err := m.suppression(ctx, tx, cs, pol, lv, now)
	if err != nil {
		return stepResult{}, err
	}
	if reason != "" {
		return stepResult{Suppressed: reason}, m.recordSuppression(ctx, tx, c, cs, st, reason, automated, now)
	}
	from := cs.State
	before := cs.DunningStage
	cs.DunningStage, cs.State = st.StageNo, StateActive
	if err := saveCase(ctx, tx, cs, now); err != nil {
		return stepResult{}, err
	}
	if err := addHistory(ctx, tx, c, cs, "STAGE_ADVANCED", from, cs.State, "", map[string]any{"from_stage": before, "to_stage": st.StageNo,
		"days_past_due": MaxDaysPastDue(lv.aging), "automated": automated}); err != nil {
		return stepResult{}, err
	}
	err = kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateCase, AggregateID: cs.CaseID, Command: "AdvanceDunningStage",
		Before: map[string]any{"state": from, "dunning_stage": before}, After: map[string]any{"state": cs.State, "dunning_stage": cs.DunningStage, "version": cs.Version}})
	return stepResult{Done: true}, err
}

// doSend delivers the current stage's notice unless contact is suppressed or the stage was already sent. A delivery
// failure is recorded (DELIVERY_FAILED) and leaves every balance untouched.
func (m *Module) doSend(ctx context.Context, tx pgx.Tx, c kit.Caller, cs *Case, pol *Policy, lv *liveView, now time.Time, automated bool) (stepResult, error) {
	if !contactable(cs.State) {
		return stepResult{}, invalidTransition(cs.State, "a dunning notice")
	}
	if cs.DunningStage < 1 {
		return stepResult{}, kit.Conflict(kit.CodeInvalidTransition, "no dunning stage has been reached: advance the stage first")
	}
	st := pol.stage(cs.DunningStage)
	if st == nil {
		return stepResult{}, kit.Unprocessable(kit.CodeValidationFailed, "the pinned policy has no such stage")
	}
	if sent, err := stageSent(ctx, tx, cs.TenantID, cs.CaseID, st.StageNo); err != nil {
		return stepResult{}, err
	} else if sent {
		return stepResult{}, kit.Conflict(kit.CodeInvalidTransition, fmt.Sprintf("the stage %d notice was already sent", st.StageNo))
	}
	if MaxDaysPastDue(lv.aging) <= 0 || !lv.aging.Total.IsPositive() {
		return stepResult{}, kit.Conflict(kit.CodeInvalidTransition, "AR-07 shows no overdue residual for this case")
	}
	reason, err := m.suppression(ctx, tx, cs, pol, lv, now)
	if err != nil {
		return stepResult{}, err
	}
	if reason != "" {
		return stepResult{Suppressed: reason}, m.recordSuppression(ctx, tx, c, cs, st, reason, automated, now)
	}
	body, hash := RenderNotice(cs, st, lv.aging)
	rcpt, derr := m.deliverer.Deliver(ctx, Notice{TenantID: cs.TenantID, LegalEntityID: cs.LegalEntityID, CaseID: cs.CaseID, CustomerPartyID: cs.CustomerPartyID,
		StageNo: st.StageNo, Channel: st.Channel, TemplateKey: st.TemplateKey, Body: body, ArtifactHash: hash,
		IdempotencyKey: fmt.Sprintf("%s:%d", cs.CaseID, st.StageNo)})
	a := &Action{StageNo: st.StageNo, Channel: st.Channel, TemplateKey: st.TemplateKey, NoticeArtifactHash: hash, AgingSnapshot: lv.aging, ActorID: c.PrincipalID, Automated: automated}
	switch {
	case derr != nil:
		a.DeliveryResult = ResultFailed
		a.FailureDetail = truncate(derr.Error(), 500)
	case rcpt.Delivered:
		a.DeliveryResult, a.ProviderRef = ResultDelivered, rcpt.ProviderRef
	default:
		a.DeliveryResult, a.ProviderRef = ResultSent, rcpt.ProviderRef
	}
	if err := insertAction(ctx, tx, c, cs, a, body, now); err != nil {
		return stepResult{}, err
	}
	from := cs.State
	if cs.State == StatePromiseToPay || cs.State == StateDisputed {
		cs.State = StateActive // the grace / dispute that held the state is over (checked above)
	}
	if err := saveCase(ctx, tx, cs, now); err != nil {
		return stepResult{}, err
	}
	event := "NOTICE_SENT"
	if a.DeliveryResult == ResultFailed {
		event = "NOTICE_DELIVERY_FAILED"
	}
	if err := addHistory(ctx, tx, c, cs, event, from, cs.State, "", map[string]any{"stage_no": st.StageNo, "delivery_result": a.DeliveryResult,
		"notice_artifact_hash": hash, "automated": automated}); err != nil {
		return stepResult{}, err
	}
	if err := kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateCase, AggregateID: cs.CaseID, Command: "SendDunningNotice",
		After:  map[string]any{"stage_no": st.StageNo, "delivery_result": a.DeliveryResult, "notice_artifact_hash": hash, "version": cs.Version},
		Detail: map[string]any{"automated": automated, "failure": a.FailureDetail}}); err != nil {
		return stepResult{}, err
	}
	if a.DeliveryResult != ResultFailed {
		if err := kit.Emit(ctx, tx, c, kit.Event{AggregateType: AggregateCase, AggregateID: cs.CaseID, EventType: EvNoticeSent, Payload: map[string]any{
			"case_id": cs.CaseID, "customer_party_id": cs.CustomerPartyID, "stage_no": st.StageNo, "channel": st.Channel,
			"delivery_result": a.DeliveryResult, "notice_artifact_hash": hash, "version": cs.Version}}); err != nil {
			return stepResult{}, err
		}
	}
	return stepResult{Done: true, Action: a}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// resolveIfSettled moves the case to RESOLVED when AR-07 shows every item settled, written off or credited (read live).
// The case never marks anything paid: it only observes.
func (m *Module) resolveIfSettled(ctx context.Context, tx pgx.Tx, c kit.Caller, cs *Case, lv *liveView, now time.Time, event string) (bool, error) {
	if !ItemsSettled(lv.views) {
		return false, nil
	}
	items := make([]map[string]any, 0, len(lv.views))
	for _, v := range lv.views {
		items = append(items, map[string]any{"open_item_id": v.OpenItemID, "item_state": v.State, "residual": v.Residual.String()})
	}
	from := cs.State
	if !CanTransition(from, StateResolved) {
		return false, invalidTransition(from, StateResolved)
	}
	t := now
	cs.State, cs.Resolution, cs.ResolvedAt = StateResolved, map[string]any{"source": "AR-07 live read", "as_of": now.Format(time.RFC3339), "items": items}, &t
	if err := saveCase(ctx, tx, cs, now); err != nil {
		return false, err
	}
	if err := addHistory(ctx, tx, c, cs, event, from, StateResolved, "", map[string]any{"items": items}); err != nil {
		return false, err
	}
	if err := kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateCase, AggregateID: cs.CaseID, Command: event,
		Before: map[string]any{"state": from}, After: map[string]any{"state": StateResolved, "version": cs.Version}, Detail: map[string]any{"items": items}}); err != nil {
		return false, err
	}
	err := kit.Emit(ctx, tx, c, kit.Event{AggregateType: AggregateCase, AggregateID: cs.CaseID, EventType: EvCaseResolved, Payload: map[string]any{
		"case_id": cs.CaseID, "customer_party_id": cs.CustomerPartyID, "version": cs.Version, "items": items}})
	return true, err
}
