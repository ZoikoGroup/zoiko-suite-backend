package ar10

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

type writeOffBase struct {
	LegalEntityID   string `json:"legal_entity_id,omitempty"`
	WriteOffID      string `json:"write_off_id"`
	ExpectedVersion *int   `json:"expected_version"`
}

func writeOffPayload(c kit.Caller, w *WriteOff, extra map[string]any) map[string]any {
	p := map[string]any{"write_off_id": w.WriteOffID, "open_item_id": w.OpenItemID, "legal_entity_id": w.LegalEntityID, "customer_party_id": w.CustomerPartyID,
		"amount": w.Amount.String(), "currency": w.Currency, "reason_category": w.ReasonCategory, "tax_treatment": w.TaxTreatment,
		"tax_relief_amount": w.TaxReliefAmount.String(), "state": w.State, "version": w.Version, "correlation_id": c.CorrelationID}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func emitWriteOff(ctx context.Context, tx pgx.Tx, c kit.Caller, w *WriteOff, event string, extra map[string]any) error {
	return kit.Emit(ctx, tx, c, kit.Event{AggregateType: AggregateWriteOff, AggregateID: w.WriteOffID, EventType: event, Payload: writeOffPayload(c, w, extra)})
}

// prepareWriteOff loads the write-off, refuses a body entity that disagrees with the stored one and guards action ("" = tenant +
// entity only) against the STORED entity.
func (m *Module) prepareWriteOff(w http.ResponseWriter, r *http.Request, b writeOffBase, action string) (kit.Caller, *WriteOff, bool) {
	c0, ok := m.deps.Identify(w, r)
	if !ok {
		return kit.Caller{}, nil, false
	}
	if err := mustUUID("write_off_id", b.WriteOffID); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	var wo *WriteOff
	if err := m.read(r.Context(), c0.TenantID, func(tx pgx.Tx) (e error) {
		wo, e = getWriteOff(r.Context(), tx, c0.TenantID, b.WriteOffID, false)
		return
	}); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if b.LegalEntityID != "" && b.LegalEntityID != wo.LegalEntityID {
		m.fail(w, errScope("the write-off belongs to another legal entity"))
		return kit.Caller{}, nil, false
	}
	c, ok := m.deps.Guard(w, r, wo.LegalEntityID, action)
	return c, wo, ok
}

func (m *Module) lockWriteOff(ctx context.Context, c kit.Caller, id string, fn func(tx pgx.Tx, cur *WriteOff) error) (*WriteOff, error) {
	var out *WriteOff
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getWriteOff(ctx, tx, c.TenantID, id, true)
		if err != nil {
			return err
		}
		if err := fn(tx, cur); err != nil {
			return err
		}
		out, err = getWriteOff(ctx, tx, c.TenantID, id, false)
		return err
	})
	return out, err
}

func setWriteOff(ctx context.Context, tx pgx.Tx, tenantID, id, set string, args ...any) error {
	a := append([]any{tenantID, id}, args...)
	_, err := tx.Exec(ctx, `UPDATE ar10_writeoffs SET `+set+`, version = version + 1, updated_at = now() WHERE tenant_id = $1 AND writeoff_id = $2`, a...)
	return err
}

func validTax(treatment string, relief, amount kit.Amount, currency string) error {
	switch treatment {
	case TaxNoneRequired:
		if !relief.IsZero() {
			return kit.Invalid("tax_relief_amount must be zero when no tax treatment is required")
		}
	case TaxReliefClaimed:
		if !relief.IsPositive() || relief > amount || !relief.FitsCurrency(currency) {
			return kit.Invalid("tax_relief_amount must be positive, not exceed the write-off and fit the currency when relief is claimed")
		}
	case TaxReviewNeeded:
		if !relief.IsZero() {
			return kit.Invalid("tax_relief_amount must be zero until the tax review decides")
		}
	default:
		return kit.Invalid("tax_treatment must be NONE_REQUIRED, RELIEF_CLAIMED or REVIEW_REQUIRED")
	}
	return nil
}

// ── CreateWriteOffRequest ────────────────────────────────────────────────────

type createWriteOffReq struct {
	LegalEntityID   string     `json:"legal_entity_id"`
	OpenItemID      string     `json:"open_item_id"`
	Amount          kit.Amount `json:"amount"`
	ReasonCategory  string     `json:"reason_category"`
	ReasonCode      string     `json:"reason_code"`
	EvidenceID      string     `json:"evidence_id"`
	TaxTreatment    string     `json:"tax_treatment,omitempty"` // default REVIEW_REQUIRED; any other decision needs the tax-review authority and evidence
	TaxReliefAmount kit.Amount `json:"tax_relief_amount,omitempty"`
	TaxEvidenceID   string     `json:"tax_evidence_id,omitempty"`
}

// createWriteOffRequest records a write-off of (part of) an open item's residual in PENDING_APPROVAL. A controlled reason category,
// a reason code and evidence are mandatory. The tax treatment starts as REVIEW_REQUIRED (committing is blocked until a tax reviewer
// decides) unless the requester holds the tax-review authority and supplies evidence for an explicit decision.
func (m *Module) createWriteOffRequest(w http.ResponseWriter, r *http.Request) {
	var req createWriteOffReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionWriteOffCreate)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := mustUUID("open_item_id", req.OpenItemID); err != nil {
		m.fail(w, err)
		return
	}
	switch {
	case !req.Amount.IsPositive():
		m.fail(w, kit.Invalid("amount must be positive"))
		return
	case !ReasonCategories[req.ReasonCategory]:
		m.fail(w, kit.Invalid("reason_category must be one of BAD_DEBT, INSOLVENCY, UNCOLLECTIBLE_SMALL_BALANCE, STATUTE_BARRED, SETTLEMENT_DISCOUNT"))
		return
	case strings.TrimSpace(req.ReasonCode) == "":
		m.fail(w, reasonRequired())
		return
	case strings.TrimSpace(req.EvidenceID) == "":
		m.fail(w, evidenceRequired("for a write-off"))
		return
	}
	tax := req.TaxTreatment
	if tax == "" {
		tax = TaxReviewNeeded
	}
	if err := validTax(tax, req.TaxReliefAmount, req.Amount, ""); err != nil && tax != TaxReliefClaimed {
		m.fail(w, err)
		return
	}
	var reviewer, taxEvidence string
	if tax != TaxReviewNeeded {
		if err := m.checkAction(ctx, c, ActionWriteOffTaxReview); err != nil {
			m.fail(w, err)
			return
		}
		if strings.TrimSpace(req.TaxEvidenceID) == "" {
			m.fail(w, evidenceRequired("(tax_evidence_id) for an explicit tax treatment"))
			return
		}
		reviewer, taxEvidence = c.PrincipalID, req.TaxEvidenceID
	}
	if m.items == nil || m.authority == nil {
		m.fail(w, kit.Unavailable("AR-07 receivables / authority policy not wired: no write-off was recorded"))
		return
	}
	it, err := m.items.GetByID(ctx, c.TenantID, req.OpenItemID)
	if err != nil {
		m.fail(w, err)
		return
	}
	switch {
	case it.LegalEntityID != c.LegalEntityID:
		m.fail(w, errScope("the open item belongs to another legal entity"))
		return
	case !req.Amount.FitsCurrency(it.Currency):
		m.fail(w, kit.Invalid("amount has more decimals than the currency allows"))
		return
	case it.Disputed:
		m.fail(w, errTransition("the open item is disputed: resolve the dispute before writing it off"))
		return
	case !it.Residual.IsPositive() || req.Amount > it.Residual:
		m.fail(w, kit.Unprocessable(kit.CodeReceivableResidualInvalid, "the write-off amount "+req.Amount.String()+" exceeds the open item's residual "+it.Residual.String()))
		return
	}
	if tax == TaxReliefClaimed {
		if err := validTax(tax, req.TaxReliefAmount, req.Amount, it.Currency); err != nil {
			m.fail(w, err)
			return
		}
	}
	held, err := m.authority.Held(ctx, c)
	if err != nil {
		m.fail(w, err)
		return
	}
	now := m.deps.Now()
	wo := &WriteOff{WriteOffID: newID(), LegalEntityID: c.LegalEntityID, OpenItemID: it.OpenItemID, CustomerPartyID: it.CustomerPartyID, Currency: it.Currency, Amount: req.Amount,
		ResidualAtRequest: it.Residual, ReasonCategory: req.ReasonCategory, ReasonCode: req.ReasonCode, EvidenceID: req.EvidenceID, TaxTreatment: tax, TaxReliefAmount: req.TaxReliefAmount,
		RequiredTier: m.authority.Required(req.Amount, it.Currency).Level, RequesterTier: held, State: WoPendingApproval, RequestedBy: c.PrincipalID, Version: 1}
	wo.BindingHash = wo.Binding()
	var out *WriteOff
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO ar10_writeoffs (writeoff_id, tenant_id, legal_entity_id, open_item_id, customer_party_id, currency, amount, residual_at_request, reason_category,
				reason_code, evidence_id, tax_treatment, tax_relief_amount, tax_reviewer, tax_evidence_id, tax_reviewed_at, required_tier, requester_tier, state, binding_hash, requested_by, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,''),NULLIF($15,''),CASE WHEN $14 = '' THEN NULL ELSE $16::timestamptz END,$17,$18,'PENDING_APPROVAL',$19,$20,$16,$16)`,
			wo.WriteOffID, c.TenantID, wo.LegalEntityID, wo.OpenItemID, wo.CustomerPartyID, wo.Currency, wo.Amount, wo.ResidualAtRequest, wo.ReasonCategory, wo.ReasonCode, wo.EvidenceID,
			wo.TaxTreatment, wo.TaxReliefAmount, reviewer, taxEvidence, now, wo.RequiredTier, wo.RequesterTier, wo.BindingHash, wo.RequestedBy); err != nil {
			if kit.IsUniqueViolation(err, "uq_ar10_writeoff_inflight") {
				return kit.Conflict(kit.CodeInvalidTransition, "a write-off is already in flight for this open item")
			}
			return err
		}
		if err := addHistory(ctx, tx, c, "WRITEOFF", wo.WriteOffID, "createWriteOffRequest", WoDraft, WoPendingApproval,
			map[string]any{"amount": wo.Amount.String(), "reason_category": wo.ReasonCategory, "required_tier": wo.RequiredTier}); err != nil {
			return err
		}
		var e error
		if out, e = getWriteOff(ctx, tx, c.TenantID, wo.WriteOffID, false); e != nil {
			return e
		}
		if err := audit(ctx, tx, c, AggregateWriteOff, wo.WriteOffID, "createWriteOffRequest", wo.ReasonCode, wo.EvidenceID, nil, out,
			map[string]any{"required_tier": wo.RequiredTier, "requester_tier": wo.RequesterTier, "tax_treatment": wo.TaxTreatment, "tax_evidence_id": taxEvidence}); err != nil {
			return err
		}
		return emitWriteOff(ctx, tx, c, out, EvWriteOffRequested, map[string]any{"reason_code": wo.ReasonCode, "evidence_id": wo.EvidenceID, "required_tier": wo.RequiredTier})
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, out)
}

// ── ResolveWriteOffTaxTreatment ──────────────────────────────────────────────

type taxDecisionReq struct {
	writeOffBase
	TaxTreatment    string     `json:"tax_treatment"`
	TaxReliefAmount kit.Amount `json:"tax_relief_amount,omitempty"`
	EvidenceID      string     `json:"evidence_id"`
	ReasonCode      string     `json:"reason_code,omitempty"`
}

// resolveWriteOffTaxTreatment records the tax decision (NONE_REQUIRED / RELIEF_CLAIMED) with reviewer and evidence. The reviewer is
// never the requester. Changing the decision of an APPROVED write-off invalidates the approval (it is bound to the tax treatment).
func (m *Module) resolveWriteOffTaxTreatment(w http.ResponseWriter, r *http.Request) {
	var req taxDecisionReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, cur, ok := m.prepareWriteOff(w, r, req.writeOffBase, ActionWriteOffTaxReview)
	if !ok {
		return
	}
	ctx := r.Context()
	switch {
	case req.ExpectedVersion == nil:
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	case req.TaxTreatment != TaxNoneRequired && req.TaxTreatment != TaxReliefClaimed:
		m.fail(w, kit.Invalid("tax_treatment must be NONE_REQUIRED or RELIEF_CLAIMED"))
		return
	case strings.TrimSpace(req.EvidenceID) == "":
		m.fail(w, evidenceRequired("for a tax treatment decision"))
		return
	case cur.State != WoPendingApproval && cur.State != WoApproved:
		m.fail(w, errTransition("the tax treatment of a "+cur.State+" write-off cannot change"))
		return
	}
	if err := validTax(req.TaxTreatment, req.TaxReliefAmount, cur.Amount, cur.Currency); err != nil {
		m.fail(w, err)
		return
	}
	if c.PrincipalID == cur.RequestedBy {
		m.fail(w, m.denied(ctx, c, AggregateWriteOff, cur.WriteOffID, "resolveWriteOffTaxTreatment", errSoD("the tax treatment must be decided by a principal other than the requester"), nil))
		return
	}
	now := m.deps.Now()
	out, err := m.lockWriteOff(ctx, c, cur.WriteOffID, func(tx pgx.Tx, p *WriteOff) error {
		if p.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		if p.State != WoPendingApproval && p.State != WoApproved {
			return errTransition("the tax treatment of a " + p.State + " write-off cannot change")
		}
		np := *p
		np.TaxTreatment, np.TaxReliefAmount = req.TaxTreatment, req.TaxReliefAmount
		if err := setWriteOff(ctx, tx, c.TenantID, p.WriteOffID, `tax_treatment = $3, tax_relief_amount = $4, tax_reviewer = $5, tax_evidence_id = $6, tax_reviewed_at = $7,
			binding_hash = $8, approved_hash = NULL, approved_by = NULL, approved_tier = NULL, approved_at = NULL, state = 'PENDING_APPROVAL'`,
			np.TaxTreatment, np.TaxReliefAmount, c.PrincipalID, req.EvidenceID, now, np.Binding()); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "WRITEOFF", p.WriteOffID, "resolveWriteOffTaxTreatment", p.State, WoPendingApproval,
			map[string]any{"tax_treatment": np.TaxTreatment, "tax_relief_amount": np.TaxReliefAmount.String(), "approval_invalidated": p.State == WoApproved}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateWriteOff, p.WriteOffID, "resolveWriteOffTaxTreatment", req.ReasonCode, req.EvidenceID,
			map[string]any{"state": p.State, "tax_treatment": p.TaxTreatment, "tax_relief_amount": p.TaxReliefAmount.String()},
			map[string]any{"state": WoPendingApproval, "tax_treatment": np.TaxTreatment, "tax_relief_amount": np.TaxReliefAmount.String(), "tax_reviewer": c.PrincipalID}, nil); err != nil {
			return err
		}
		np.State, np.Version = WoPendingApproval, p.Version+1
		evs := []kit.Event{{AggregateType: AggregateWriteOff, AggregateID: p.WriteOffID, EventType: EvWriteOffTaxDecided,
			Payload: writeOffPayload(c, &np, map[string]any{"tax_reviewer": c.PrincipalID, "tax_evidence_id": req.EvidenceID})}}
		if p.State == WoApproved {
			evs = append(evs, kit.Event{AggregateType: AggregateWriteOff, AggregateID: p.WriteOffID, EventType: EvWriteOffInvalid,
				Payload: writeOffPayload(c, &np, map[string]any{"reason": "tax treatment changed after approval"})})
		}
		return kit.Emit(ctx, tx, c, evs...)
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// ── ApproveWriteOff ──────────────────────────────────────────────────────────

type approveWriteOffReq struct {
	writeOffBase
	ReasonCode string `json:"reason_code,omitempty"`
}

// approveWriteOff approves the write-off as currently bound. Authority is by tier (AuthorityPolicy): the approver must hold a tier
// at or above the one the amount needs (403 WRITEOFF_APPROVAL_REQUIRED otherwise), is never the requester, and for a material write-off
// is not a principal who handled the item (dispute opener / resolver, collection hold owner). Every refusal is audited as ".denied".
func (m *Module) approveWriteOff(w http.ResponseWriter, r *http.Request) {
	var req approveWriteOffReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, cur, ok := m.prepareWriteOff(w, r, req.writeOffBase, "")
	if !ok {
		return
	}
	ctx := r.Context()
	if req.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	}
	if cur.State != WoPendingApproval {
		m.fail(w, errTransition("only a PENDING_APPROVAL write-off can be approved (it is "+cur.State+")"))
		return
	}
	if m.authority == nil || m.parties == nil {
		m.fail(w, kit.Unavailable("authority policy / AR-07 not wired: the write-off was not approved"))
		return
	}
	deny := func(err error, extra map[string]any) {
		if extra == nil {
			extra = map[string]any{}
		}
		extra["required_tier"], extra["amount"] = cur.RequiredTier, cur.Amount.String()
		m.fail(w, m.denied(ctx, c, AggregateWriteOff, cur.WriteOffID, "approveWriteOff", err, extra))
	}
	if c.PrincipalID == cur.RequestedBy {
		deny(errSoD("a write-off cannot be approved by the principal who requested it"), nil)
		return
	}
	held, err := m.authority.Held(ctx, c)
	if err != nil {
		m.fail(w, err)
		return
	}
	req0 := m.authority.Required(cur.Amount, cur.Currency)
	switch {
	case held == 0:
		deny(kit.Forbidden(kit.CodeForbidden, "the principal holds no write-off approval authority"), map[string]any{"held_tier": held})
		return
	case held < req0.Level:
		deny(errApprovalRequired("this write-off needs approval authority tier "+itoa(req0.Level)+" (limit of the approver's tier is lower than the amount)"), map[string]any{"held_tier": held})
		return
	}
	if cur.Amount > MaterialWriteOff {
		opener, resolver, err := m.parties.DisputeParticipants(ctx, c.TenantID, cur.OpenItemID)
		if err != nil {
			m.fail(w, err)
			return
		}
		var collector string
		if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (e error) {
			collector, e = collectionHoldOwner(ctx, tx, c.TenantID, cur.OpenItemID)
			return
		}); err != nil {
			m.fail(w, err)
			return
		}
		for role, who := range map[string]string{"dispute opener": opener, "dispute resolver": resolver, "collection hold owner": collector} {
			if who != "" && who == c.PrincipalID {
				deny(errSoD("a material write-off cannot be approved by its "+role), map[string]any{"role": role, "held_tier": held})
				return
			}
		}
	}
	now := m.deps.Now()
	out, err := m.lockWriteOff(ctx, c, cur.WriteOffID, func(tx pgx.Tx, p *WriteOff) error {
		if p.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		if p.State != WoPendingApproval {
			return errTransition("only a PENDING_APPROVAL write-off can be approved (it is " + p.State + ")")
		}
		if c.PrincipalID == p.RequestedBy {
			return errSoD("a write-off cannot be approved by the principal who requested it")
		}
		if p.Binding() != p.BindingHash {
			return errApprovalRequired("the write-off content no longer matches its binding hash")
		}
		if err := setWriteOff(ctx, tx, c.TenantID, p.WriteOffID, `state = 'APPROVED', approved_hash = binding_hash, approved_by = $3, approved_tier = $4, approved_at = $5`,
			c.PrincipalID, held, now); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "WRITEOFF", p.WriteOffID, "approveWriteOff", p.State, WoApproved, map[string]any{"approved_tier": held, "required_tier": p.RequiredTier}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateWriteOff, p.WriteOffID, "approveWriteOff", req.ReasonCode, "", map[string]any{"state": p.State},
			map[string]any{"state": WoApproved, "approved_by": c.PrincipalID, "approved_tier": held}, map[string]any{"required_tier": p.RequiredTier, "approved_hash": p.BindingHash}); err != nil {
			return err
		}
		na := *p
		na.State, na.Version = WoApproved, p.Version+1
		return emitWriteOff(ctx, tx, c, &na, EvWriteOffApproved, map[string]any{"approved_by": c.PrincipalID, "approved_tier": held, "approved_hash": p.BindingHash})
	})
	if err != nil {
		var p *kit.Problem
		if errors.As(err, &p) && p.Code == kit.CodeSegregationOfDuties {
			err = m.denied(ctx, c, AggregateWriteOff, cur.WriteOffID, "approveWriteOff", err, nil)
		}
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func itoa(n int) string { return strconv.Itoa(n) }

// ── CommitWriteOff ───────────────────────────────────────────────────────────

// commitWriteOff derecognises the receivable. In ONE transaction: the approval is re-checked against the current content, the AR-07
// residual is adjusted (AdjWriteOff), the ACC-04 posting is queued (Dr bad debt [+ Dr output tax relief] / Cr receivable) and
// the events and audit are written. Blocked while the tax treatment is unresolved (#34), while the period is closed, or when AR-07 /
// the period gate are not wired (503, no state change). A repeat commit is a no-op.
func (m *Module) commitWriteOff(w http.ResponseWriter, r *http.Request) {
	var req writeOffBase
	if !kit.Decode(w, r, &req) {
		return
	}
	c, cur, ok := m.prepareWriteOff(w, r, req, ActionWriteOffCommit)
	if !ok {
		return
	}
	ctx := r.Context()
	if req.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	}
	switch cur.State {
	case WoCommitted:
		kit.WriteJSON(w, http.StatusOK, cur) // idempotent
		return
	case WoApproved:
	default:
		m.fail(w, errTransition("only an APPROVED write-off can be committed (it is "+cur.State+")"))
		return
	}
	if m.items == nil {
		m.fail(w, kit.Unavailable("AR-07 receivables not wired: nothing was committed"))
		return
	}
	if cur.TaxTreatment == TaxReviewNeeded {
		m.fail(w, m.denied(ctx, c, AggregateWriteOff, cur.WriteOffID, "commitWriteOff",
			kit.Unprocessable(kit.CodeTaxDeterminationMissing, "the tax treatment of this write-off is unresolved: a tax review must decide it before commit"), nil))
		return
	}
	now := m.deps.Now()
	pk, err := m.period(ctx, c, now)
	if err != nil {
		m.fail(w, err)
		return
	}
	if it, err := m.items.GetByID(ctx, c.TenantID, cur.OpenItemID); err != nil {
		m.fail(w, err)
		return
	} else if it.Disputed {
		m.fail(w, errTransition("the open item is disputed: resolve the dispute before committing the write-off"))
		return
	}
	out, err := m.lockWriteOff(ctx, c, cur.WriteOffID, func(tx pgx.Tx, p *WriteOff) error {
		if p.State == WoCommitted {
			return nil
		}
		if p.State != WoApproved {
			return errTransition("only an APPROVED write-off can be committed (it is " + p.State + ")")
		}
		if p.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		if p.ApprovedHash == "" || p.Binding() != p.ApprovedHash {
			return errApprovalRequired("the write-off no longer matches the content that was approved")
		}
		if p.TaxTreatment == TaxReviewNeeded {
			return kit.Unprocessable(kit.CodeTaxDeterminationMissing, "the tax treatment of this write-off is unresolved")
		}
		if _, err := m.items.ApplyAdjustment(ctx, tx, c, kit.OpenItemAdjustment{OpenItemID: p.OpenItemID, Kind: kit.AdjWriteOff, Amount: p.Amount,
			SourceRefType: "WRITE_OFF", SourceRefID: p.WriteOffID, EventKey: p.WriteOffID + ":write_off", Reason: "write-off " + p.ReasonCategory + ": " + p.ReasonCode}); err != nil {
			return err
		}
		if err := setWriteOff(ctx, tx, c.TenantID, p.WriteOffID, `state = 'COMMITTED', committed_by = $3, committed_at = $4, fiscal_period = $5, accounting_state = 'PENDING'`,
			c.PrincipalID, now, pk); err != nil {
			return err
		}
		if err := kit.QueuePosting(ctx, tx, c, kit.SourceWriteOff, p.WriteOffID, kit.PostingEvent{FiscalPeriod: pk, Description: "Write-off " + p.WriteOffID,
			SourceEventID: p.WriteOffID, TransactionCurrency: p.Currency, DocumentDate: day(now), PostingDate: day(now), Lines: BuildWriteOffPosting(p)}); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "WRITEOFF", p.WriteOffID, "commitWriteOff", p.State, WoCommitted, map[string]any{"fiscal_period": pk}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateWriteOff, p.WriteOffID, "commitWriteOff", p.ReasonCode, p.EvidenceID, map[string]any{"state": p.State},
			map[string]any{"state": WoCommitted, "committed_by": c.PrincipalID}, map[string]any{"fiscal_period": pk, "tax_treatment": p.TaxTreatment, "approved_by": p.ApprovedBy}); err != nil {
			return err
		}
		nc := *p
		nc.State, nc.Version = WoCommitted, p.Version+1
		return emitWriteOff(ctx, tx, c, &nc, EvReceivableWritten, map[string]any{"committed_by": c.PrincipalID, "fiscal_period": pk, "approved_by": p.ApprovedBy})
	})
	if err != nil {
		var p *kit.Problem
		if errors.As(err, &p) && (p.Code == kit.CodeSegregationOfDuties || p.Code == kit.CodeWriteOffApprovalRequired) {
			err = m.denied(ctx, c, AggregateWriteOff, cur.WriteOffID, "commitWriteOff", err, nil)
		}
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}
