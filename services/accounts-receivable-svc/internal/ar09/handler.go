package ar09

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// record writes the history row and the audit record of a case transition (same tx as the state change).
func (m *Module) record(ctx *cmdCtx, event, from, reason, evidence string, detail map[string]any) error {
	if err := addHistory(ctx.r.Context(), ctx.tx, ctx.c, ctx.cs, event, from, ctx.cs.State, reason, detail); err != nil {
		return err
	}
	return kit.RecordAudit(ctx.r.Context(), ctx.tx, ctx.c, kit.Audit{AggregateType: AggregateCase, AggregateID: ctx.cs.CaseID, Command: event,
		ReasonCode: reason, EvidenceID: evidence, Before: map[string]any{"state": from},
		After: map[string]any{"state": ctx.cs.State, "version": ctx.cs.Version, "dunning_stage": ctx.cs.DunningStage}, Detail: detail})
}

type cmdCtx struct {
	r  *http.Request
	tx pgx.Tx
	c  kit.Caller
	cs *Case
}

type caseReq struct {
	LegalEntityID   string `json:"legal_entity_id"`
	CaseID          string `json:"case_id"`
	ExpectedVersion *int   `json:"expected_version"`
}

// withCase is the shared command shape: ONE transaction, case locked, legal entity and expected_version enforced.
// fn may return a non-empty suppression reason with a nil error: the evidence it wrote is committed and the request is then
// refused with COLLECTION_HOLD_ACTIVE.
func (m *Module) withCase(w http.ResponseWriter, r *http.Request, c kit.Caller, req caseReq, fn func(*cmdCtx) (suppressedReason string, out any, err error)) {
	if _, err := uuid.Parse(req.CaseID); err != nil {
		m.fail(w, kit.Invalid("case_id is not a valid UUID"))
		return
	}
	var out any
	var reason string
	err := m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		cs, err := lockForCommand(r.Context(), tx, c, req.CaseID, req.ExpectedVersion)
		if err != nil {
			return err
		}
		reason, out, err = fn(&cmdCtx{r: r, tx: tx, c: c, cs: cs})
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	if reason != "" {
		m.fail(w, suppressed(reason))
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// ---- OpenCollectionCase ----

type openReq struct {
	LegalEntityID   string   `json:"legal_entity_id"`
	CustomerPartyID string   `json:"customer_party_id"`
	Currency        string   `json:"currency"`
	OpenItemIDs     []string `json:"open_item_ids"`
	OwnerPrincipal  string   `json:"owner_principal"`
}

func (m *Module) openCollectionCase(w http.ResponseWriter, r *http.Request) {
	var req openReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionCollectionsManage)
	if !ok {
		return
	}
	ctx := r.Context()
	if _, err := uuid.Parse(req.CustomerPartyID); err != nil {
		m.fail(w, kit.Invalid("customer_party_id is not a valid UUID"))
		return
	}
	if len(req.Currency) != 3 || req.Currency != strings.ToUpper(req.Currency) {
		m.fail(w, kit.Invalid("currency must be a 3-letter ISO code"))
		return
	}
	now := m.deps.Now()
	prof, err := m.profiles.ActiveAt(ctx, c.TenantID, c.LegalEntityID, req.CustomerPartyID, now)
	if err != nil {
		m.fail(w, err)
		return
	}
	if strings.TrimSpace(prof.DunningProfile) == "" {
		m.fail(w, kit.Unprocessable(kit.CodeValidationFailed, "the customer's financial profile names no dunning profile"))
		return
	}
	all, err := m.recv.ListOpenForCustomer(ctx, c.TenantID, c.LegalEntityID, req.CustomerPartyID, req.Currency)
	if err != nil {
		m.fail(w, err)
		return
	}
	selected, err := selectOverdue(all, req.OpenItemIDs, now)
	if err != nil {
		m.fail(w, err)
		return
	}
	owner := strings.TrimSpace(req.OwnerPrincipal)
	if owner == "" {
		owner = c.PrincipalID
	}
	var cs *Case
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		pol, err := policyAt(ctx, tx, c.TenantID, c.LegalEntityID, prof.DunningProfile, now)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(selected))
		for _, v := range selected {
			ids = append(ids, v.OpenItemID)
		}
		cs = &Case{CaseID: uuid.NewString(), TenantID: c.TenantID, LegalEntityID: c.LegalEntityID, CustomerPartyID: req.CustomerPartyID,
			Currency: req.Currency, State: StateEligible, OwnerPrincipal: owner, DunningProfile: prof.DunningProfile, PolicyID: pol.PolicyID,
			AgingSnapshot: BuildAging(selected, req.Currency, now), Version: 1, CreatedBy: c.PrincipalID, CreatedAt: now, UpdatedAt: now}
		if err := insertCase(ctx, tx, cs, ids); err != nil {
			if kit.IsUniqueViolation(err, "uq_ar09_one_live_case") {
				return kit.Conflict(kit.CodeInvalidTransition, "a live collection case already exists for this customer and currency")
			}
			return err
		}
		cc := &cmdCtx{r: r, tx: tx, c: c, cs: cs}
		if err := m.record(cc, "CASE_OPENED", "", "", "", map[string]any{"policy_version": pol.Version, "item_count": len(ids)}); err != nil {
			return err
		}
		return kit.Emit(ctx, tx, c, kit.Event{AggregateType: AggregateCase, AggregateID: cs.CaseID, EventType: EvCaseOpened, Payload: map[string]any{
			"case_id": cs.CaseID, "customer_party_id": cs.CustomerPartyID, "currency": cs.Currency, "dunning_profile": cs.DunningProfile,
			"policy_id": pol.PolicyID, "policy_version": pol.Version, "open_item_ids": ids, "version": cs.Version}})
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, cs)
}

// selectOverdue picks the case's items: the named ones (each must exist, carry a residual and be overdue) or, by default,
// every overdue item with a residual.
func selectOverdue(all []kit.OpenItemView, named []string, now time.Time) ([]kit.OpenItemView, error) {
	overdue := func(v kit.OpenItemView) bool { return v.Residual.IsPositive() && DaysPastDue(v.DueDate, now) > 0 }
	var out []kit.OpenItemView
	if len(named) == 0 {
		for _, v := range all {
			if overdue(v) {
				out = append(out, v)
			}
		}
	} else {
		byID := map[string]kit.OpenItemView{}
		for _, v := range all {
			byID[v.OpenItemID] = v
		}
		seen := map[string]bool{}
		for _, id := range named {
			v, ok := byID[id]
			if !ok || seen[id] {
				return nil, kit.Unprocessable(kit.CodeValidationFailed, "open item "+id+" is not an open receivable of this customer and currency")
			}
			seen[id] = true
			if !overdue(v) {
				return nil, kit.Unprocessable(kit.CodeValidationFailed, "open item "+id+" is not overdue")
			}
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, kit.Unprocessable(kit.CodeValidationFailed, "AR-07 shows no overdue receivable for this customer and currency")
	}
	return out, nil
}

// ---- AdvanceDunningStage / SendDunningNotice ----

func (m *Module) advanceDunningStage(w http.ResponseWriter, r *http.Request) {
	var req caseReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionDunningSend)
	if !ok {
		return
	}
	m.withCase(w, r, c, req, func(cc *cmdCtx) (string, any, error) {
		now := m.deps.Now()
		pol, err := policyByID(r.Context(), cc.tx, c.TenantID, cc.cs.PolicyID)
		if err != nil {
			return "", nil, err
		}
		lv, err := m.live(r.Context(), cc.tx, cc.cs, now)
		if err != nil {
			return "", nil, err
		}
		res, err := m.doAdvance(r.Context(), cc.tx, c, cc.cs, pol, lv, now, false)
		return res.Suppressed, cc.cs, err
	})
}

type sendReq struct {
	caseReq
	StageNo int `json:"stage_no"`
}

func (m *Module) sendDunningNotice(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionDunningSend)
	if !ok {
		return
	}
	m.withCase(w, r, c, req.caseReq, func(cc *cmdCtx) (string, any, error) {
		if req.StageNo != 0 && req.StageNo != cc.cs.DunningStage {
			return "", nil, kit.Conflict(kit.CodeInvalidTransition, "stage_no is not the case's current dunning stage")
		}
		now := m.deps.Now()
		pol, err := policyByID(r.Context(), cc.tx, c.TenantID, cc.cs.PolicyID)
		if err != nil {
			return "", nil, err
		}
		lv, err := m.live(r.Context(), cc.tx, cc.cs, now)
		if err != nil {
			return "", nil, err
		}
		res, err := m.doSend(r.Context(), cc.tx, c, cc.cs, pol, lv, now, false)
		return res.Suppressed, map[string]any{"case": cc.cs, "action": res.Action}, err
	})
}

// ---- RecordPromiseToPay ----

type promiseReq struct {
	caseReq
	PromisedDate   string     `json:"promised_date"`
	PromisedAmount kit.Amount `json:"promised_amount"`
	ReasonCode     string     `json:"reason_code"`
	EvidenceID     string     `json:"evidence_id"`
}

func (m *Module) recordPromiseToPay(w http.ResponseWriter, r *http.Request) {
	var req promiseReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionCollectionsManage)
	if !ok {
		return
	}
	date, err := parseDate("promised_date", req.PromisedDate)
	if err != nil {
		m.fail(w, err)
		return
	}
	if !req.PromisedAmount.IsPositive() {
		m.fail(w, kit.Invalid("promised_amount must be positive"))
		return
	}
	m.withCase(w, r, c, req.caseReq, func(cc *cmdCtx) (string, any, error) {
		ctx, cs, now := r.Context(), cc.cs, m.deps.Now()
		if cs.State != StateEligible && cs.State != StateActive && cs.State != StatePromiseToPay {
			return "", nil, invalidTransition(cs.State, StatePromiseToPay)
		}
		if date.Before(startOfDayUTC(now)) {
			return "", nil, kit.Unprocessable(kit.CodeValidationFailed, "promised_date cannot be in the past")
		}
		if !req.PromisedAmount.FitsCurrency(cs.Currency) {
			return "", nil, kit.Invalid("promised_amount has more decimals than the case currency allows")
		}
		lv, err := m.live(ctx, cc.tx, cs, now)
		if err != nil {
			return "", nil, err
		}
		if req.PromisedAmount > lv.aging.Total {
			return "", nil, kit.Unprocessable(kit.CodeValidationFailed, "promised_amount exceeds what AR-07 currently shows outstanding")
		}
		prev, err := latestPromise(ctx, cc.tx, c.TenantID, cs.CaseID)
		if err != nil {
			return "", nil, err
		}
		p := &Promise{PromiseID: uuid.NewString(), CaseID: cs.CaseID, VersionNo: 1, PromisedDate: req.PromisedDate, PromisedAmount: req.PromisedAmount, Currency: cs.Currency,
			ReasonCode: strings.TrimSpace(req.ReasonCode), RecordedBy: c.PrincipalID, RecordedAt: now}
		if prev != nil {
			if strings.TrimSpace(req.ReasonCode) == "" {
				return "", nil, kit.Err(http.StatusBadRequest, kit.CodeReasonRequired, "reason_code is required to change a promise (a new version is recorded, the old one is kept)")
			}
			if prev.PromisedDate == req.PromisedDate && prev.PromisedAmount == req.PromisedAmount {
				return "", nil, kit.Conflict(kit.CodeInvalidTransition, "the promise is unchanged")
			}
			p.VersionNo, p.Supersedes = prev.VersionNo+1, prev.PromiseID
		}
		if _, err := cc.tx.Exec(ctx, `INSERT INTO ar09_promises (promise_id, tenant_id, legal_entity_id, case_id, version_no, promised_date, promised_amount, currency,
			supersedes, reason_code, recorded_by, recorded_at) VALUES ($1,$2,$3,$4,$5,$6::date,$7,$8,$9,NULLIF($10,''),$11,$12)`,
			p.PromiseID, c.TenantID, cs.LegalEntityID, cs.CaseID, p.VersionNo, p.PromisedDate, p.PromisedAmount, p.Currency, nullUUID(p.Supersedes), p.ReasonCode,
			p.RecordedBy, now); err != nil {
			if kit.IsUniqueViolation(err, "") {
				return "", nil, kit.ErrStaleVersion
			}
			return "", nil, err
		}
		from := cs.State
		cs.State = StatePromiseToPay
		if err := saveCase(ctx, cc.tx, cs, now); err != nil {
			return "", nil, err
		}
		if err := m.record(cc, "PROMISE_RECORDED", from, p.ReasonCode, req.EvidenceID, map[string]any{"promise_id": p.PromiseID, "version_no": p.VersionNo,
			"promised_date": p.PromisedDate, "promised_amount": p.PromisedAmount.String(), "supersedes": p.Supersedes}); err != nil {
			return "", nil, err
		}
		err = kit.Emit(ctx, cc.tx, c, kit.Event{AggregateType: AggregateCase, AggregateID: cs.CaseID, EventType: EvPromiseRecorded, Payload: map[string]any{
			"case_id": cs.CaseID, "promise_id": p.PromiseID, "version_no": p.VersionNo, "promised_date": p.PromisedDate,
			"promised_amount": p.PromisedAmount.String(), "currency": p.Currency, "supersedes": p.Supersedes, "version": cs.Version}})
		return "", map[string]any{"case": cs, "promise": p}, err
	})
}

func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- PlaceCollectionHold / ResumeCollections ----

type holdReq struct {
	caseReq
	ReasonCode string `json:"reason_code"`
	EvidenceID string `json:"evidence_id"`
}

func (m *Module) placeCollectionHold(w http.ResponseWriter, r *http.Request) {
	var req holdReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionHoldManage)
	if !ok || !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	m.withCase(w, r, c, req.caseReq, func(cc *cmdCtx) (string, any, error) {
		ctx, cs, now := r.Context(), cc.cs, m.deps.Now()
		if !CanTransition(cs.State, StateHeld) {
			return "", nil, invalidTransition(cs.State, StateHeld)
		}
		h := Hold{HoldID: uuid.NewString(), CaseID: cs.CaseID, ReasonCode: req.ReasonCode, EvidenceID: req.EvidenceID, PlacedBy: c.PrincipalID, PlacedAt: now}
		if _, err := cc.tx.Exec(ctx, `INSERT INTO ar09_holds (hold_id, tenant_id, legal_entity_id, case_id, reason_code, evidence_id, placed_by, placed_at)
			VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8)`, h.HoldID, c.TenantID, cs.LegalEntityID, cs.CaseID, h.ReasonCode, h.EvidenceID, h.PlacedBy, now); err != nil {
			if kit.IsUniqueViolation(err, "uq_ar09_one_active_hold") {
				return "", nil, kit.Conflict(kit.CodeInvalidTransition, "the case already has an active hold")
			}
			return "", nil, err
		}
		from := cs.State
		cs.StateBefore, cs.State = from, StateHeld
		if err := saveCase(ctx, cc.tx, cs, now); err != nil {
			return "", nil, err
		}
		if err := m.record(cc, "HOLD_PLACED", from, req.ReasonCode, req.EvidenceID, map[string]any{"hold_id": h.HoldID}); err != nil {
			return "", nil, err
		}
		err := kit.Emit(ctx, cc.tx, c, kit.Event{AggregateType: AggregateCase, AggregateID: cs.CaseID, EventType: EvHoldPlaced, Payload: map[string]any{
			"case_id": cs.CaseID, "hold_id": h.HoldID, "reason_code": req.ReasonCode, "version": cs.Version}})
		return "", map[string]any{"case": cs, "hold": h}, err
	})
}

func (m *Module) resumeCollections(w http.ResponseWriter, r *http.Request) {
	var req holdReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionHoldManage)
	if !ok || !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	m.withCase(w, r, c, req.caseReq, func(cc *cmdCtx) (string, any, error) {
		ctx, cs, now := r.Context(), cc.cs, m.deps.Now()
		if cs.State != StateHeld {
			return "", nil, invalidTransition(cs.State, "resumed collections")
		}
		h, err := activeHold(ctx, cc.tx, c.TenantID, cs.CaseID)
		if err != nil {
			return "", nil, err
		}
		if h == nil {
			return "", nil, kit.Conflict(kit.CodeInvalidTransition, "the case has no active hold")
		}
		if h.PlacedBy == c.PrincipalID {
			return "", nil, kit.Forbidden(kit.CodeSegregationOfDuties, "the principal who placed a collection hold cannot release it")
		}
		if _, err := cc.tx.Exec(ctx, `UPDATE ar09_holds SET released_by = $3, released_at = $4, release_reason = $5 WHERE tenant_id = $1 AND hold_id = $2`,
			c.TenantID, h.HoldID, c.PrincipalID, now, req.ReasonCode); err != nil {
			return "", nil, err
		}
		from := cs.State
		cs.State, cs.StateBefore = cs.StateBefore, ""
		if cs.State == "" || cs.State == StateHeld {
			cs.State = StateEligible
			if cs.DunningStage > 0 {
				cs.State = StateActive
			}
		}
		if err := saveCase(ctx, cc.tx, cs, now); err != nil {
			return "", nil, err
		}
		return "", cs, m.record(cc, "COLLECTIONS_RESUMED", from, req.ReasonCode, req.EvidenceID, map[string]any{"hold_id": h.HoldID})
	})
}

// ---- EscalateCollectionCase ----

type escalateReq struct {
	caseReq
	ReasonCode     string `json:"reason_code"`
	ReferralReason string `json:"referral_reason"`
	EvidenceID     string `json:"evidence_id"`
}

// escalateCollectionCase records that the case is referred. It grants NO legal-case authority and takes no financial action:
// the referral reason is recorded for the governed legal workflow to pick up (spec SoD: legal escalation does not grant
// legal-case authority).
func (m *Module) escalateCollectionCase(w http.ResponseWriter, r *http.Request) {
	var req escalateReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionCollectionsEscalate)
	if !ok || !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	if strings.TrimSpace(req.ReferralReason) == "" || len(req.ReferralReason) > 500 {
		m.fail(w, kit.Invalid("referral_reason is required (max 500 characters)"))
		return
	}
	m.withCase(w, r, c, req.caseReq, func(cc *cmdCtx) (string, any, error) {
		ctx, cs, now := r.Context(), cc.cs, m.deps.Now()
		if !CanTransition(cs.State, StateEscalated) {
			return "", nil, invalidTransition(cs.State, StateEscalated)
		}
		if cs.DunningStage < 1 {
			return "", nil, kit.Conflict(kit.CodeInvalidTransition, "a case is escalated only after dunning has started")
		}
		pol, err := policyByID(ctx, cc.tx, c.TenantID, cs.PolicyID)
		if err != nil {
			return "", nil, err
		}
		lv, err := m.live(ctx, cc.tx, cs, now)
		if err != nil {
			return "", nil, err
		}
		if reason, err := m.blockReason(ctx, cc.tx, cs, lv, now); err != nil || reason != "" {
			return reason, nil, err
		}
		if st := pol.stage(cs.DunningStage); st != nil && st.EscalationThreshold.IsPositive() && lv.aging.Total < st.EscalationThreshold {
			return "", nil, kit.Unprocessable(kit.CodeValidationFailed, "the outstanding amount AR-07 shows is below the escalation threshold of the current stage")
		}
		from, t := cs.State, now
		cs.State, cs.ReferralReason, cs.EscalatedBy, cs.EscalatedAt = StateEscalated, req.ReferralReason, c.PrincipalID, &t
		if err := saveCase(ctx, cc.tx, cs, now); err != nil {
			return "", nil, err
		}
		if err := m.record(cc, "ESCALATED", from, req.ReasonCode, req.EvidenceID, map[string]any{"referral_reason": req.ReferralReason, "aging": lv.aging}); err != nil {
			return "", nil, err
		}
		err = kit.Emit(ctx, cc.tx, c, kit.Event{AggregateType: AggregateCase, AggregateID: cs.CaseID, EventType: EvEscalated, Payload: map[string]any{
			"case_id": cs.CaseID, "customer_party_id": cs.CustomerPartyID, "referral_reason": req.ReferralReason, "reason_code": req.ReasonCode, "version": cs.Version}})
		return "", cs, err
	})
}

// ---- CloseCollectionCase ----

type closeReq struct {
	caseReq
	Outcome    string `json:"outcome"` // RESOLVED | ABANDONED
	ReasonCode string `json:"reason_code"`
	Reason     string `json:"reason"`
	EvidenceID string `json:"evidence_id"`
}

// closeCollectionCase ends a case. RESOLVED is accepted only when AR-07 (read live now) shows every item settled, written off
// or credited - the case never marks anything paid. ABANDONED is an administrative close with a reason, and the case owner
// cannot abandon their own case (4-eyes: an owner must not be able to walk away from collectable debt alone).
func (m *Module) closeCollectionCase(w http.ResponseWriter, r *http.Request) {
	var req closeReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionCollectionsManage)
	if !ok {
		return
	}
	if req.Outcome != OutcomeResolved && req.Outcome != OutcomeAbandoned {
		m.fail(w, kit.Invalid("outcome must be RESOLVED or ABANDONED"))
		return
	}
	if req.Outcome == OutcomeAbandoned && !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	m.withCase(w, r, c, req.caseReq, func(cc *cmdCtx) (string, any, error) {
		ctx, cs, now := r.Context(), cc.cs, m.deps.Now()
		if IsTerminal(cs.State) {
			return "", nil, invalidTransition(cs.State, StateClosed)
		}
		lv, err := m.live(ctx, cc.tx, cs, now)
		if err != nil {
			return "", nil, err
		}
		if req.Outcome == OutcomeResolved {
			done, err := m.resolveIfSettled(ctx, cc.tx, c, cs, lv, now, "RESOLVED")
			if err != nil {
				return "", nil, err
			}
			if !done {
				return "", nil, kit.Conflict(kit.CodeInvalidTransition, "AR-07 still shows an outstanding residual: a case resolves only when its items are settled, written off or credited")
			}
			return "", cs, nil
		}
		if cs.OwnerPrincipal == c.PrincipalID {
			return "", nil, kit.Forbidden(kit.CodeSegregationOfDuties, "the case owner cannot abandon their own collection case")
		}
		if h, err := activeHold(ctx, cc.tx, c.TenantID, cs.CaseID); err != nil {
			return "", nil, err
		} else if h != nil {
			if _, err := cc.tx.Exec(ctx, `UPDATE ar09_holds SET released_by = $3, released_at = $4, release_reason = 'CASE_CLOSED' WHERE tenant_id = $1 AND hold_id = $2`,
				c.TenantID, h.HoldID, c.PrincipalID, now); err != nil {
				return "", nil, err
			}
		}
		from := cs.State
		cs.State, cs.ClosedBy, cs.CloseReason = StateClosed, c.PrincipalID, truncate(req.Reason, 500)
		if err := saveCase(ctx, cc.tx, cs, now); err != nil {
			return "", nil, err
		}
		return "", cs, m.record(cc, "CASE_CLOSED", from, req.ReasonCode, req.EvidenceID, map[string]any{"outcome": req.Outcome, "reason": cs.CloseReason})
	})
}

// ---- PublishDunningPolicy (policy data; versions are immutable) ----

type policyReq struct {
	LegalEntityID       string   `json:"legal_entity_id"`
	DunningProfile      string   `json:"dunning_profile"`
	EffectiveFrom       string   `json:"effective_from"`
	Stages              []Stage  `json:"stages"`
	BlackoutWeekdays    []int    `json:"blackout_weekdays"`
	BlackoutDates       []string `json:"blackout_dates"`
	MaxNoticesPerWindow int      `json:"max_notices_per_window"`
	WindowDays          int      `json:"window_days"`
	PromiseGraceDays    int      `json:"promise_grace_days"`
}

func (m *Module) publishDunningPolicy(w http.ResponseWriter, r *http.Request) {
	var req policyReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionPolicyManage)
	if !ok {
		return
	}
	p := &Policy{LegalEntityID: c.LegalEntityID, DunningProfile: strings.TrimSpace(req.DunningProfile), EffectiveFrom: req.EffectiveFrom, Stages: req.Stages,
		BlackoutWeekdays: req.BlackoutWeekdays, BlackoutDates: req.BlackoutDates, MaxNoticesPerWindow: req.MaxNoticesPerWindow, WindowDays: req.WindowDays,
		PromiseGraceDays: req.PromiseGraceDays}
	if p.BlackoutWeekdays == nil {
		p.BlackoutWeekdays = []int{}
	}
	if p.BlackoutDates == nil {
		p.BlackoutDates = []string{}
	}
	if err := ValidatePolicy(p); err != nil {
		m.fail(w, err)
		return
	}
	err := m.deps.DB.WithTx(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		if err := insertPolicy(r.Context(), tx, c, p); err != nil {
			return err
		}
		return kit.RecordAudit(r.Context(), tx, c, kit.Audit{AggregateType: "AR_DUNNING_POLICY", AggregateID: p.PolicyID, Command: "PublishDunningPolicy",
			After: map[string]any{"dunning_profile": p.DunningProfile, "version": p.Version, "effective_from": p.EffectiveFrom}})
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, p)
}
