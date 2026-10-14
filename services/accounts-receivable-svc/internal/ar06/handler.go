package ar06

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/ar04"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
)

func reasonRequired() *kit.Problem {
	return kit.Err(http.StatusBadRequest, kit.CodeReasonRequired, "reason_code is required")
}

func evidenceRequired() *kit.Problem {
	return kit.Err(http.StatusBadRequest, kit.CodeEvidenceRequired, fmt.Sprintf("evidence_id is required for a credit note above %s", EvidenceRequiredThreshold))
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// cmdBase is the envelope shared by every command on an existing correction. legal_entity_id is optional: the entity is derived
// from the stored correction and a body entity that disagrees is refused (spec invariant 16).
type cmdBase struct {
	CorrectionID    string `json:"correction_id"`
	LegalEntityID   string `json:"legal_entity_id,omitempty"`
	ExpectedVersion *int   `json:"expected_version"`
	ReasonCode      string `json:"reason_code,omitempty"`
	EvidenceID      string `json:"evidence_id,omitempty"`
}

// prepare identifies the caller, loads the correction to learn its entity, then runs Guard (tenant + entity reconciliation
// before authz) for the action.
func (m *Module) prepare(w http.ResponseWriter, r *http.Request, b *cmdBase, action string, versionRequired bool) (kit.Caller, *Header, bool) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return kit.Caller{}, nil, false
	}
	if err := mustUUID("correction_id", b.CorrectionID); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if versionRequired && b.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return kit.Caller{}, nil, false
	}
	var h *Header
	if err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		h, err = getHeader(r.Context(), tx, c.TenantID, b.CorrectionID, false)
		return err
	}); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if b.LegalEntityID != "" && b.LegalEntityID != h.LegalEntityID {
		m.fail(w, kit.Forbidden(kit.CodeTenantScope, "legal_entity_id does not match the correction's legal entity"))
		return kit.Caller{}, nil, false
	}
	c, ok = m.deps.Guard(w, r, h.LegalEntityID, action)
	if !ok {
		return kit.Caller{}, nil, false
	}
	return c, h, true
}

func evPayload(c kit.Caller, h *Header, extra map[string]any) map[string]any {
	p := map[string]any{"correction_id": h.CorrectionID, "kind": h.Kind, "source_invoice_id": h.SourceInvoiceID, "legal_entity_id": h.LegalEntityID,
		"customer_party_id": h.CustomerPartyID, "currency": h.Currency, "lifecycle_state": h.State, "version": h.Version, "net_total": h.NetTotal.String(),
		"tax_total": h.TaxTotal.String(), "gross_total": h.GrossTotal.String(), "content_hash": h.ContentHash, "correlation_id": c.CorrelationID}
	if h.DocumentNumber != "" {
		p["document_number"] = h.DocumentNumber
	}
	for k, x := range extra {
		p[k] = x
	}
	return p
}

// stepSpec describes one pre-issue state-changing command on an existing correction.
type stepSpec struct {
	action  string
	command string
	tr      Transition
	// pre runs outside the transaction with the current correction loaded: source, tax and segregation checks live here.
	pre func(ctx context.Context, c kit.Caller, cor *Correction) error
	// apply mutates nh (a copy of the locked header, already moved by tr) and returns the evidence.
	apply func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, cor *Correction, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error)
}

func (m *Module) runStep(w http.ResponseWriter, r *http.Request, req *cmdBase, sp stepSpec) {
	c, h, ok := m.prepare(w, r, req, sp.action, true)
	if !ok {
		return
	}
	ctx := r.Context()
	if h.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	if _, err := Next(h.State, sp.tr); err != nil {
		m.fail(w, err)
		return
	}
	if sp.pre != nil {
		var cor *Correction
		if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) {
			cor, err = loadCorrection(ctx, tx, c.TenantID, h)
			return err
		}); err != nil {
			m.fail(w, err)
			return
		}
		if err := sp.pre(ctx, c, cor); err != nil {
			m.failStep(w, ctx, c, h.CorrectionID, err)
			return
		}
	}
	now := m.today()
	var out *Correction
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, c.TenantID, req.CorrectionID, true)
		if err != nil {
			return err
		}
		if cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		next, err := Next(cur.State, sp.tr)
		if err != nil {
			return err
		}
		cor, err := loadCorrection(ctx, tx, c.TenantID, cur)
		if err != nil {
			return err
		}
		nh := *cur
		nh.State = next
		hs, events, err := sp.apply(ctx, tx, c, cur, cor, &nh, now)
		if err != nil {
			return err
		}
		hs.At = now
		if hs.Command == "" {
			hs.Command = sp.command
		}
		if hs.ContentHash == "" {
			hs.ContentHash = nh.ContentHash
		}
		saved, err := commit(ctx, tx, c, cur, &nh, hs, events)
		if err != nil {
			return err
		}
		out, err = loadCorrection(ctx, tx, c.TenantID, saved)
		return err
	})
	if err != nil {
		m.failStep(w, ctx, c, req.CorrectionID, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// ── CreateCorrectionDraft ────────────────────────────────────────────────────

type createReq struct {
	LegalEntityID   string `json:"legal_entity_id"`
	Kind            Kind   `json:"kind"`
	SourceInvoiceID string `json:"source_invoice_id"`
	CustomerPartyID string `json:"customer_party_id,omitempty"`
	Currency        string `json:"currency,omitempty"`
	Content
	Lines []LineInput `json:"lines"`
}

func netOf(lines []Line) kit.Amount {
	var n kit.Amount
	for _, l := range lines {
		n = n.Add(l.Net)
	}
	return n
}

func (m *Module) createCorrectionDraft(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionCorrectionManage)
	if !ok {
		return
	}
	ctx := r.Context()
	if !req.Kind.valid() {
		m.fail(w, kit.Invalid("kind must be CREDIT_NOTE or DEBIT_NOTE"))
		return
	}
	if err := req.Content.Validate(); err != nil {
		m.fail(w, err)
		return
	}
	// The source must be an ISSUED invoice of this tenant, entity and customer (spec section 18 #18).
	src, err := m.source(ctx, c.TenantID, req.SourceInvoiceID)
	if err != nil {
		m.fail(w, err)
		return
	}
	switch {
	case src.LegalEntityID != req.LegalEntityID:
		m.fail(w, kit.Err(http.StatusUnprocessableEntity, CodeCorrectionSourceInvalid, "the source invoice belongs to another legal entity"))
		return
	case req.CustomerPartyID != "" && req.CustomerPartyID != src.CustomerPartyID:
		m.fail(w, kit.Err(http.StatusUnprocessableEntity, CodeCorrectionSourceInvalid, "the source invoice belongs to another customer"))
		return
	case req.Currency != "" && req.Currency != src.Currency:
		m.fail(w, kit.Err(http.StatusUnprocessableEntity, CodeCorrectionSourceInvalid, "the correction currency differs from the source invoice currency"))
		return
	}
	lines, err := BuildLines(req.Kind, src.Currency, req.Lines, src.Lines)
	if err != nil {
		m.fail(w, err)
		return
	}
	net := netOf(lines)
	if !net.IsPositive() || !net.FitsCurrency(src.Currency) {
		m.fail(w, kit.Invalid("the correction net must be greater than zero and fit the currency"))
		return
	}
	if EvidenceNeeded(req.Kind, net) && strings.TrimSpace(req.EvidenceID) == "" {
		m.fail(w, evidenceRequired())
		return
	}
	now := m.today()
	h := &Header{CorrectionID: uuid.NewString(), TenantID: c.TenantID, LegalEntityID: src.LegalEntityID, CustomerPartyID: src.CustomerPartyID, Kind: req.Kind,
		SourceInvoiceID: src.InvoiceID, SourceInvoiceNumber: src.InvoiceNumber, Currency: src.Currency, State: StateDraft, Content: req.Content,
		NetTotal: net, GrossTotal: net, CreatedBy: c.PrincipalID}
	h.ContentHash = ContentHash(h, lines, nil)
	var out *Correction
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		if req.Kind == KindCredit {
			if err := lockSource(ctx, tx, c.TenantID, src.InvoiceID); err != nil {
				return err
			}
			cn, cg, err := creditedTotals(ctx, tx, c.TenantID, src.InvoiceID, "")
			if err != nil {
				return err
			}
			if err := EligibilityOf(src, cn, cg).Check(net, 0, false); err != nil {
				return err
			}
		}
		if err := insertHeader(ctx, tx, c, h, now); err != nil {
			return err
		}
		if err := insertLines(ctx, tx, c, h.CorrectionID, lines); err != nil {
			return err
		}
		saved, err := getHeader(ctx, tx, c.TenantID, h.CorrectionID, false)
		if err != nil {
			return err
		}
		if err := record(ctx, tx, c, saved, hist{At: now, Command: "createCorrectionDraft", Reason: req.ReasonCode, Evidence: req.EvidenceID, ContentHash: h.ContentHash,
			New: map[string]any{"lifecycle_state": StateDraft, "kind": req.Kind, "source_invoice_id": src.InvoiceID, "source_invoice_number": src.InvoiceNumber,
				"net_total": net.String(), "reason_code": req.ReasonCode}},
			func(o *Header) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: o.CorrectionID, EventType: EventDraftCreated, Payload: evPayload(c, o, nil)}}
			}); err != nil {
			return err
		}
		out, err = loadCorrection(ctx, tx, c.TenantID, saved)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, out)
}

// ── tax ──────────────────────────────────────────────────────────────────────

// determineTax obtains one tax determination per tax group of the correction's lines from tax-determination-svc. AR never
// computes tax: anything the TAX domain cannot answer leaves the correction a draft.
func (m *Module) determineTax(ctx context.Context, c kit.Caller, cor *Correction) ([]ar04.TaxGroup, error) {
	if m.tax == nil {
		return nil, kit.Unavailable("tax determination service not wired: the correction cannot be submitted")
	}
	specs := BuildGroups(cor.Lines)
	out := make([]ar04.TaxGroup, 0, len(specs))
	now := m.today()
	for _, g := range specs {
		d, err := m.tax.Determine(ctx, c.SvcCaller(), taxdet.Request{
			TransactionID: fmt.Sprintf("ar06:%s:v%d:g%d", cor.CorrectionID, cor.Version, g.GroupNo), SourceModule: "AR", LegalEntityID: cor.LegalEntityID,
			JurisdictionID: cor.Content.Tax.JurisdictionID, SupplyJurisdictionID: cor.Content.Tax.SupplyJurisdictionID, TaxCategory: g.TaxCategory,
			GrossAmount: g.Net, Currency: cor.Currency, EffectiveFrom: cor.Content.SupplyDate, SupplyDate: cor.Content.SupplyDate,
			SellerPartyID: cor.LegalEntityID, BuyerPartyID: cor.CustomerPartyID, ProductClassification: g.ProductClassification, SupplyKind: g.SupplyKind,
			SupplyType: cor.Content.Tax.SupplyType, BuyerTaxRegistrationID: cor.Content.Tax.BuyerTaxRegistrationID, EvaluatedBy: c.PrincipalID})
		switch {
		case errors.Is(err, taxdet.ErrRejected):
			return nil, kit.Unprocessable(kit.CodeTaxDeterminationMissing, "tax determination could not be produced: "+clip(err.Error(), 400))
		case err != nil:
			return nil, kit.Unavailable("tax determination service unavailable: the correction cannot be submitted")
		}
		tg, err := ar04.GroupFromDetermination(d, g, cor.LegalEntityID, cor.Currency, c.PrincipalID, now)
		if err != nil {
			return nil, err
		}
		out = append(out, tg)
	}
	return out, nil
}

// verifyTax re-verifies every approved determination with tax-determination-svc: it must still exist, be usable, belong to the
// same entity and currency, and carry the same tax amount and provenance. Missing => TAX_DETERMINATION_MISSING; a changed
// result => invalidation; unreachable => 503 (fail closed).
func (m *Module) verifyTax(ctx context.Context, c kit.Caller, cor *Correction) error {
	if len(cor.TaxGroups) == 0 {
		return kit.Unprocessable(kit.CodeTaxDeterminationMissing, "the correction has no tax determination: it cannot be issued")
	}
	for _, g := range cor.TaxGroups {
		d, err := m.tax.Get(ctx, c.SvcCaller(), g.DeterminationID)
		switch {
		case errors.Is(err, taxdet.ErrNotFound):
			return kit.Unprocessable(kit.CodeTaxDeterminationMissing, "tax determination "+g.DeterminationID+" no longer exists: the correction cannot be issued")
		case err != nil:
			return kit.Unavailable("tax determination service unavailable: the approved tax result cannot be re-verified")
		case d == nil:
			return kit.Unprocessable(kit.CodeTaxDeterminationMissing, "tax determination "+g.DeterminationID+" returned no result")
		}
		var why string
		switch {
		case !d.Usable():
			why = "determination is no longer usable (status " + d.Status + ")"
		case d.LegalEntityID != "" && d.LegalEntityID != cor.LegalEntityID:
			why = "determination now belongs to another legal entity"
		case d.Currency != "" && d.Currency != cor.Currency:
			why = "determination is now in another currency"
		case d.Tax != g.Tax:
			why = "tax amount changed from " + g.Tax.String() + " to " + d.Tax.String()
		case d.Provenance() != g.Provenance:
			why = "rule-pack provenance changed from " + g.Provenance + " to " + d.Provenance()
		}
		if why != "" {
			return &invalidation{Reason: "TAX_RESULT_CHANGED_AFTER_APPROVAL", Detail: map[string]any{"determination_id": g.DeterminationID, "group_no": g.GroupNo,
				"change": why, "approved_tax": g.Tax.String(), "approved_provenance": g.Provenance}}
		}
	}
	return nil
}

// ── SubmitCorrectionForApproval ──────────────────────────────────────────────

func (m *Module) submitCorrectionForApproval(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	var groups []ar04.TaxGroup
	var src *kit.IssuedInvoice
	m.runStep(w, r, &req, stepSpec{action: ActionCorrectionManage, command: "submitCorrectionForApproval", tr: TrSubmit,
		pre: func(ctx context.Context, c kit.Caller, cor *Correction) error {
			if err := ValidateForTax(cor.Content, cor.Lines); err != nil {
				return err
			}
			if !netOf(cor.Lines).IsPositive() {
				return kit.Invalid("the correction net must be greater than zero")
			}
			var err error
			if src, err = m.source(ctx, c.TenantID, cor.SourceInvoiceID); err != nil {
				return err
			}
			groups, err = m.determineTax(ctx, c, cor)
			return err
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, cor *Correction, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if len(groups) == 0 {
				return hist{}, nil, kit.Unprocessable(kit.CodeTaxDeterminationMissing, "no tax determination was obtained for the correction")
			}
			lines, err := ApplyTax(cor.Lines, groups, cur.Currency)
			if err != nil {
				return hist{}, nil, err
			}
			totals, err := TotalsOf(lines, groups, cur.Currency)
			if err != nil {
				return hist{}, nil, err
			}
			if cur.Kind == KindCredit {
				if err := lockSource(ctx, tx, c.TenantID, cur.SourceInvoiceID); err != nil {
					return hist{}, nil, err
				}
				cn, cg, err := creditedTotals(ctx, tx, c.TenantID, cur.SourceInvoiceID, cur.CorrectionID)
				if err != nil {
					return hist{}, nil, err
				}
				if err := EligibilityOf(src, cn, cg).Check(totals.Net, totals.Gross, true); err != nil {
					return hist{}, nil, err
				}
			}
			if EvidenceNeeded(cur.Kind, totals.Gross) && strings.TrimSpace(cur.Content.EvidenceID) == "" {
				return hist{}, nil, evidenceRequired()
			}
			if err := replaceLines(ctx, tx, c, cur.CorrectionID, lines); err != nil {
				return hist{}, nil, err
			}
			if err := replaceTax(ctx, tx, c, cur.CorrectionID, groups); err != nil {
				return hist{}, nil, err
			}
			t := now
			nh.NetTotal, nh.TaxTotal, nh.GrossTotal = totals.Net, totals.Tax, totals.Gross
			nh.SubmittedBy, nh.SubmittedAt = c.PrincipalID, &t
			nh.ContentHash = ContentHash(nh, lines, groups)
			ids := make([]string, len(groups))
			for i, g := range groups {
				ids[i] = g.DeterminationID
			}
			return hist{Reason: req.ReasonCode, Evidence: req.EvidenceID,
					Prior: map[string]any{"lifecycle_state": cur.State},
					New: map[string]any{"lifecycle_state": nh.State, "submitted_by": c.PrincipalID, "net_total": totals.Net.String(), "tax_total": totals.Tax.String(),
						"gross_total": totals.Gross.String(), "tax_determination_ids": ids}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.CorrectionID, EventType: EventSubmitted, Payload: evPayload(c, h, map[string]any{"tax_determination_ids": ids})}}
				}, nil
		}})
}

// ── ApproveCorrection ────────────────────────────────────────────────────────

type approveReq struct {
	cmdBase
	ContentHash string `json:"content_hash"`
}

// integrity proves the stored rows still hash to the recorded content hash.
func integrity(cor *Correction) error {
	if ContentHash(&cor.Header, cor.Lines, cor.TaxGroups) != cor.ContentHash {
		return errApprovalInvalidated("the stored correction no longer matches its recorded content hash: re-approval is required")
	}
	return nil
}

func (m *Module) approveCorrection(w http.ResponseWriter, r *http.Request) {
	var req approveReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ContentHash) == "" {
		m.fail(w, kit.Invalid("content_hash is required: an approval is bound to the content the approver saw"))
		return
	}
	var src *kit.IssuedInvoice
	m.runStep(w, r, &req.cmdBase, stepSpec{action: ActionCorrectionApprove, command: "approveCorrection", tr: TrApprove,
		pre: func(ctx context.Context, c kit.Caller, cor *Correction) error {
			if req.ContentHash != cor.ContentHash {
				return errApprovalInvalidated("content_hash does not match the current correction content: an approval cannot be reused after the correction changed")
			}
			if err := integrity(cor); err != nil {
				return err
			}
			if len(cor.TaxGroups) == 0 {
				return kit.Unprocessable(kit.CodeTaxDeterminationMissing, "the correction has no tax determination to approve")
			}
			if c.PrincipalID == cor.CreatedBy || c.PrincipalID == cor.SubmittedBy {
				return deny(errSoD("a correction must be approved by a principal who neither created nor submitted it"), "approveCorrection",
					map[string]any{"gross_total": cor.GrossTotal.String(), "content_hash": cor.ContentHash, "material": IsMaterial(cor.GrossTotal)})
			}
			var err error
			src, err = m.source(ctx, c.TenantID, cor.SourceInvoiceID)
			return err
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, cor *Correction, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if req.ContentHash != cur.ContentHash {
				return hist{}, nil, errApprovalInvalidated("the correction changed while the approval was in flight")
			}
			if err := integrity(cor); err != nil {
				return hist{}, nil, err
			}
			if cur.Kind == KindCredit {
				if err := lockSource(ctx, tx, c.TenantID, cur.SourceInvoiceID); err != nil {
					return hist{}, nil, err
				}
				cn, cg, err := creditedTotals(ctx, tx, c.TenantID, cur.SourceInvoiceID, cur.CorrectionID)
				if err != nil {
					return hist{}, nil, err
				}
				if err := EligibilityOf(src, cn, cg).Check(cur.NetTotal, cur.GrossTotal, true); err != nil {
					return hist{}, nil, err
				}
			}
			t := now
			nh.ApprovedHash, nh.ApprovedBy, nh.ApprovedAt = cur.ContentHash, c.PrincipalID, &t
			return hist{Reason: req.ReasonCode, Evidence: req.EvidenceID, ApproverID: c.PrincipalID,
					Prior: map[string]any{"lifecycle_state": cur.State},
					New:   map[string]any{"lifecycle_state": nh.State, "approved_content_hash": cur.ContentHash, "gross_total": cur.GrossTotal.String()}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.CorrectionID, EventType: EventApproved,
						Payload: evPayload(c, h, map[string]any{"approved_by": c.PrincipalID, "approved_content_hash": cur.ContentHash})}}
				}, nil
		}})
}

// ── CancelUnissuedCorrection ─────────────────────────────────────────────────

func (m *Module) cancelUnissuedCorrection(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	m.runStep(w, r, &req, stepSpec{action: ActionCorrectionManage, command: "cancelUnissuedCorrection", tr: TrCancel,
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, cor *Correction, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			t := now
			nh.CancelReason, nh.CancelledBy, nh.CancelledAt = req.ReasonCode, c.PrincipalID, &t
			return hist{Reason: req.ReasonCode, Evidence: req.EvidenceID,
					Prior: map[string]any{"lifecycle_state": cur.State},
					New:   map[string]any{"lifecycle_state": nh.State, "cancelled_by": c.PrincipalID}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.CorrectionID, EventType: EventCancelled,
						Payload: evPayload(c, h, map[string]any{"reason_code": req.ReasonCode, "prior_state": cur.State})}}
				}, nil
		}})
}

// ── issue ────────────────────────────────────────────────────────────────────

// invalidateApproval returns an approved correction to DRAFT (approval and tax result discarded) after the approved basis was
// found stale. It runs in its own transaction because the issue transaction rolled back; it is a no-op when the correction has
// already moved on. A draft is re-submitted (tax re-determined) and re-approved.
func (m *Module) invalidateApproval(ctx context.Context, c kit.Caller, correctionID string, expected int, inv *invalidation) {
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, c.TenantID, correctionID, true)
		if err != nil {
			return err
		}
		if cur.Version != expected || cur.State != StateApproved {
			return nil
		}
		cor, err := loadCorrection(ctx, tx, c.TenantID, cur)
		if err != nil {
			return err
		}
		nh := *cur
		if nh.State, err = Next(cur.State, TrInvalidate); err != nil {
			return err
		}
		lines := make([]Line, len(cor.Lines))
		copy(lines, cor.Lines)
		for i := range lines {
			lines[i].Tax = 0
		}
		if err := replaceTax(ctx, tx, c, cur.CorrectionID, nil); err != nil {
			return err
		}
		if err := replaceLines(ctx, tx, c, cur.CorrectionID, lines); err != nil {
			return err
		}
		nh.NetTotal, nh.TaxTotal, nh.GrossTotal = netOf(lines), 0, netOf(lines)
		nh.ApprovedHash, nh.ApprovedBy, nh.ApprovedAt, nh.SubmittedBy, nh.SubmittedAt = "", "", nil, "", nil
		nh.ContentHash = ContentHash(&nh, lines, nil)
		now := m.today()
		newVals := map[string]any{"lifecycle_state": StateDraft, "invalidation": inv.Reason}
		for k, v := range inv.Detail {
			newVals[k] = v
		}
		_, err = commit(ctx, tx, c, cur, &nh, hist{At: now, Command: "invalidateApproval", Reason: inv.Reason, ContentHash: nh.ContentHash,
			Prior: map[string]any{"lifecycle_state": cur.State, "approved_content_hash": cur.ApprovedHash}, New: newVals},
			func(h *Header) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: h.CorrectionID, EventType: EventApprovalInvalidated,
					Payload: evPayload(c, h, map[string]any{"reason": inv.Reason, "detail": inv.Detail})}}
			})
		return err
	})
	if err != nil {
		m.deps.Log.Error("ar06 could not return an invalidated approval to draft")
	}
}

type issueReq struct {
	cmdBase
}

// issueNote is IssueCreditNote / IssueDebitNote. Everything that reads another module or service (source invoice, source open
// item, tax re-verification, period gate, e-invoice requirement) happens BEFORE the transaction; the transaction then does only
// this module's writes plus the tx-taking ports (AR-07 receivable, AR-04 link, ACC-04 queue), all atomically. A failure of any
// of them leaves no number, no state change, no adjustment, no posting and no outbox row.
func (m *Module) issueNote(w http.ResponseWriter, r *http.Request, kind Kind) {
	var req issueReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, h, ok := m.prepare(w, r, &req.cmdBase, ActionCorrectionIssue, true)
	if !ok {
		return
	}
	ctx := r.Context()
	if h.Kind != kind {
		m.fail(w, kit.Unprocessable(kit.CodeValidationFailed, fmt.Sprintf("this correction is a %s: use the matching issue command", h.Kind)))
		return
	}
	// An issued note is answered before the version is compared: a second, differently keyed issue is CORRECTION_ALREADY_ISSUED,
	// never a second number, adjustment or posting.
	switch {
	case h.State.IsIssued():
		m.fail(w, kit.Conflict(CodeCorrectionAlreadyIssued, "the note is already issued as "+h.DocumentNumber+": it is never issued twice"))
		return
	case h.State == StateCancelled:
		m.fail(w, kit.Conflict(kit.CodeInvalidTransition, "a cancelled correction cannot be issued"))
		return
	case h.Version != *req.ExpectedVersion:
		m.fail(w, kit.ErrStaleVersion)
		return
	case h.State != StateApproved:
		m.fail(w, kit.Conflict(kit.CodeInvalidTransition, "ISSUE is not allowed from state "+string(h.State)+": the correction must be approved"))
		return
	}
	// Every prerequisite that is not wired fails closed BEFORE any state change, number or posting.
	switch {
	case m.openItems == nil:
		m.fail(w, kit.Unavailable("AR-07 receivables not wired: an issued note without its receivable consequence is a control defect, so nothing is issued"))
		return
	case m.tax == nil:
		m.fail(w, kit.Unavailable("tax determination service not wired: the approved tax result cannot be re-verified"))
		return
	case m.periods == nil:
		m.fail(w, kit.Unavailable("accounting period service not wired: the issue date's period cannot be verified"))
		return
	case m.invoices == nil:
		m.fail(w, kit.Unavailable("AR-04 invoices not wired: the source invoice cannot be verified"))
		return
	}
	if IsMaterial(h.GrossTotal) && c.PrincipalID == h.ApprovedBy {
		m.failStep(w, ctx, c, h.CorrectionID, deny(errSoD("a material correction must be issued by a principal other than its approver"), "issue"+string(kindCmd(kind)),
			map[string]any{"gross_total": h.GrossTotal.String(), "materiality_threshold": MaterialCorrectionThreshold.String(), "approved_by": h.ApprovedBy}))
		return
	}
	cc := c
	cc.LegalEntityID = h.LegalEntityID
	var cor *Correction
	if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) {
		cor, err = loadCorrection(ctx, tx, c.TenantID, h)
		return err
	}); err != nil {
		m.fail(w, err)
		return
	}
	invalidate := func(inv *invalidation) {
		m.invalidateApproval(ctx, cc, h.CorrectionID, *req.ExpectedVersion, inv)
		m.fail(w, errApprovalInvalidated("the approved basis no longer holds ("+inv.Reason+"): the correction was returned to draft and must be re-submitted and re-approved"))
	}
	if cor.ApprovedHash == "" || cor.ApprovedHash != cor.ContentHash || ContentHash(&cor.Header, cor.Lines, cor.TaxGroups) != cor.ApprovedHash {
		invalidate(&invalidation{Reason: "APPROVED_CONTENT_NO_LONGER_MATCHES", Detail: map[string]any{"approved_content_hash": cor.ApprovedHash, "content_hash": cor.ContentHash}})
		return
	}
	src, err := m.source(ctx, c.TenantID, h.SourceInvoiceID)
	if err != nil {
		m.fail(w, err)
		return
	}
	if err := m.verifyTax(ctx, cc, cor); err != nil {
		var inv *invalidation
		if errors.As(err, &inv) {
			invalidate(inv)
			return
		}
		m.fail(w, err)
		return
	}
	now := m.today()
	issueDate := day(now)
	periodKey, err := m.periods.PostingPeriod(ctx, cc, h.LegalEntityID, now)
	if err != nil {
		m.fail(w, err)
		return
	}
	einvRequired := false
	if m.einvoice != nil {
		einvRequired, err = m.einvoice.Required(ctx, c.TenantID, h.LegalEntityID, h.Content.Tax.JurisdictionID, string(kind))
		if err != nil {
			m.deps.Log.Error("ar06 e-invoice requirement unavailable - failing closed")
			m.fail(w, kit.Unavailable("e-invoice requirement could not be determined"))
			return
		}
	}
	dueDate := ""
	var sourceItem *kit.OpenItemView
	if kind == KindCredit {
		sourceItem, err = m.openItems.GetBySourceDocument(ctx, c.TenantID, h.SourceInvoiceID)
		switch {
		case err != nil:
			var p *kit.Problem
			if errors.As(err, &p) && p.Status == http.StatusNotFound {
				m.fail(w, kit.Unprocessable(kit.CodeReceivableResidualInvalid, "the source invoice has no open item to apply the credit to"))
				return
			}
			if errors.As(err, &p) {
				m.fail(w, err)
				return
			}
			m.fail(w, kit.Unavailable("AR-07 could not read the source receivable: the note is not issued"))
			return
		case sourceItem == nil || strings.TrimSpace(sourceItem.OpenItemID) == "":
			m.fail(w, kit.Unprocessable(kit.CodeReceivableResidualInvalid, "the source invoice has no open item to apply the credit to"))
			return
		case sourceItem.Currency != h.Currency || sourceItem.LegalEntityID != "" && sourceItem.LegalEntityID != h.LegalEntityID:
			m.fail(w, kit.Err(http.StatusUnprocessableEntity, CodeCorrectionSourceInvalid, "the source open item does not match the correction's entity and currency"))
			return
		}
	} else {
		dueDate = day(src.DueDate)
		if src.DueDate.IsZero() || dueDate < issueDate {
			dueDate = issueDate
		}
	}

	var out *Correction
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, c.TenantID, req.CorrectionID, true)
		if err != nil {
			return err
		}
		if cur.State.IsIssued() {
			return kit.Conflict(CodeCorrectionAlreadyIssued, "the note is already issued as "+cur.DocumentNumber+": it is never issued twice")
		}
		if cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		if _, err := Next(cur.State, TrIssue); err != nil {
			return err
		}
		full, err := loadCorrection(ctx, tx, c.TenantID, cur)
		if err != nil {
			return err
		}
		// The approval is bound to the content hash: the stored rows must still hash to it.
		if cur.ApprovedHash == "" || cur.ApprovedHash != cur.ContentHash || ContentHash(&full.Header, full.Lines, full.TaxGroups) != cur.ApprovedHash ||
			cur.ContentHash != cor.ContentHash {
			return &invalidation{Reason: "APPROVED_CONTENT_NO_LONGER_MATCHES", Detail: map[string]any{"approved_content_hash": cur.ApprovedHash, "content_hash": cur.ContentHash}}
		}
		if kind == KindCredit {
			// Lock the source invoice, then derive the credited total exactly: concurrent credits cannot jointly exceed the invoice.
			if err := lockSource(ctx, tx, c.TenantID, cur.SourceInvoiceID); err != nil {
				return err
			}
			cn, cg, err := creditedTotals(ctx, tx, c.TenantID, cur.SourceInvoiceID, cur.CorrectionID)
			if err != nil {
				return err
			}
			if err := EligibilityOf(src, cn, cg).Check(cur.NetTotal, cur.GrossTotal, true); err != nil {
				return err
			}
		}
		prof, err := numberingProfile(ctx, tx, c.TenantID, cur.LegalEntityID, kind)
		if err != nil {
			return err
		}
		seq, err := nextSequence(ctx, tx, c.TenantID, cur.LegalEntityID, prof.Series)
		if err != nil {
			return err
		}
		number := prof.Format(seq)
		canonical, hash, err := BuildIssued(IssueInput{H: &full.Header, Lines: full.Lines, Groups: full.TaxGroups, Number: number, Series: prof.Series, Sequence: seq,
			IssueDate: issueDate, DueDate: dueDate, IssuedBy: c.PrincipalID, IssuedAt: now, EInvoiceRequired: einvRequired})
		if err != nil {
			return err
		}
		// Receivable consequence (AR-07), in this transaction. Credit note: adjustment of the SOURCE invoice's open item; debit note: its own open item.
		var openItemID string
		wrap := func(err error, what string) error {
			var p *kit.Problem
			if errors.As(err, &p) {
				return err
			}
			m.deps.Log.Error("ar06 receivable consequence failed - the note is not issued")
			return kit.Unavailable("AR-07 could not " + what + ": the note is not issued")
		}
		if kind == KindCredit {
			view, err := m.openItems.ApplyAdjustment(ctx, tx, cc, kit.OpenItemAdjustment{OpenItemID: sourceItem.OpenItemID, Kind: kit.AdjCreditNote, Amount: cur.GrossTotal,
				SourceRefType: "CREDIT_NOTE", SourceRefID: cur.CorrectionID, EventKey: cur.CorrectionID + ":issue", Reason: cur.Content.ReasonCode})
			if err != nil {
				return wrap(err, "apply the credit to the source receivable")
			}
			openItemID = sourceItem.OpenItemID
			if view != nil && view.OpenItemID != "" {
				openItemID = view.OpenItemID
			}
		} else {
			issueT, _ := time.Parse("2006-01-02", issueDate)
			dueT, _ := time.Parse("2006-01-02", dueDate)
			ref, err := m.openItems.CreateFromDocument(ctx, tx, cc, kit.OpenItemFromDocument{DocumentKind: kit.DocDebitNote, SourceDocumentID: cur.CorrectionID,
				DocumentNumber: number, CustomerPartyID: cur.CustomerPartyID, Currency: cur.Currency, OriginalAmount: cur.GrossTotal, IssueDate: issueT, DueDate: dueT,
				SourceInvoiceID: cur.SourceInvoiceID})
			if err != nil {
				return wrap(err, "create the receivable")
			}
			if strings.TrimSpace(ref.OpenItemID) == "" {
				return kit.Unavailable("AR-07 returned no open item: the note is not issued")
			}
			openItemID = ref.OpenItemID
		}
		saved, err := writeIssued(ctx, tx, cc, cur, issueWrite{Number: number, Series: prof.Series, Sequence: seq, IssueDate: issueDate, DueDate: dueDate,
			IssuedBy: c.PrincipalID, IssuedAt: now, Canonical: canonical, Hash: hash, OpenItemID: openItemID, EInvoiceRequired: einvRequired})
		if err != nil {
			return err
		}
		// The source invoice is only LINKED (dimensions/links via the AR-04 port); its issued content is never touched.
		if err := m.invoices.LinkCorrection(ctx, tx, cc, cur.SourceInvoiceID, cur.CorrectionID, string(kind)); err != nil {
			var p *kit.Problem
			if errors.As(err, &p) {
				return err
			}
			return kit.Unavailable("AR-04 could not link the correction to the source invoice: the note is not issued")
		}
		// Invariant 14: the accounting consequence is a durable event on the ACC-04 queue, in this transaction.
		if err := kit.QueuePosting(ctx, tx, cc, postingSource(kind), cur.CorrectionID, kit.PostingEvent{FiscalPeriod: periodKey,
			Description: kindLabel(kind) + " " + number, SourceEventID: cur.CorrectionID, TransactionCurrency: cur.Currency, DocumentDate: issueDate, PostingDate: issueDate,
			Lines: postingLines(kind, number, cur.NetTotal, cur.TaxTotal, cur.GrossTotal)}); err != nil {
			return err
		}
		ids := make([]string, len(full.TaxGroups))
		for i, g := range full.TaxGroups {
			ids[i] = g.DeterminationID
		}
		issuedEvent := EventCreditNoteIssued
		if kind == KindDebit {
			issuedEvent = EventDebitNoteIssued
		}
		hs := hist{At: now, Command: "issue" + string(kindCmd(kind)), Reason: req.ReasonCode, Evidence: req.EvidenceID, ApproverID: cur.ApprovedBy, ContentHash: hash,
			Prior: map[string]any{"lifecycle_state": cur.State},
			New: map[string]any{"lifecycle_state": StateIssued, "document_number": number, "numbering_series": prof.Series, "sequence_no": seq, "issue_date": issueDate,
				"issued_content_hash": hash, "open_item_id": openItemID, "source_invoice_id": cur.SourceInvoiceID, "tax_determination_ids": ids, "fiscal_period": periodKey,
				"application_state": AppApplied, "einvoice_required": einvRequired}}
		if err := record(ctx, tx, cc, saved, hs, func(o *Header) []kit.Event {
			base := map[string]any{"issue_date": issueDate, "issued_content_hash": hash, "open_item_id": openItemID, "tax_determination_ids": ids,
				"einvoice_required": einvRequired, "approved_by": cur.ApprovedBy}
			return []kit.Event{
				{AggregateType: AggregateType, AggregateID: o.CorrectionID, EventType: issuedEvent, Payload: evPayload(cc, o, base)},
				{AggregateType: AggregateType, AggregateID: o.CorrectionID, EventType: EventInvoiceCorrected, Payload: evPayload(cc, o, map[string]any{"source_invoice_number": o.SourceInvoiceNumber})},
				{AggregateType: AggregateType, AggregateID: o.CorrectionID, EventType: EventApplicationRequest, Payload: evPayload(cc, o, map[string]any{"open_item_id": openItemID, "application_kind": string(kind)})},
			}
		}); err != nil {
			return err
		}
		out, err = loadCorrection(ctx, tx, c.TenantID, saved)
		return err
	})
	if err != nil {
		var inval *invalidation
		if errors.As(err, &inval) {
			invalidate(inval)
			return
		}
		m.failStep(w, ctx, c, req.CorrectionID, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func kindCmd(k Kind) string {
	if k == KindDebit {
		return "DebitNote"
	}
	return "CreditNote"
}

func kindLabel(k Kind) string {
	if k == KindDebit {
		return "Debit note"
	}
	return "Credit note"
}

func postingSource(k Kind) string {
	if k == KindDebit {
		return kit.SourceDebitNote
	}
	return kit.SourceCreditNote
}

// postingLines is the accounting event of a note. Credit note: Dr credit-note revenue (net) + Dr output tax, Cr receivable (gross).
// Debit note: Dr receivable (gross), Cr revenue (net) + Cr output tax.
func postingLines(k Kind, number string, net, tax, gross kit.Amount) []kit.PostingLine {
	if k == KindDebit {
		l := []kit.PostingLine{{MappingKey: MappingReceivable, Debit: gross, Description: "Debit note " + number},
			{MappingKey: MappingRevenue, Credit: net, Description: "Debit note " + number}}
		if tax.IsPositive() {
			l = append(l, kit.PostingLine{MappingKey: MappingTaxOutput, Credit: tax, Description: "Output tax " + number})
		}
		return l
	}
	l := []kit.PostingLine{{MappingKey: MappingCreditNoteRevenue, Debit: net, Description: "Credit note " + number}}
	if tax.IsPositive() {
		l = append(l, kit.PostingLine{MappingKey: MappingTaxOutput, Debit: tax, Description: "Output tax reversal " + number})
	}
	return append(l, kit.PostingLine{MappingKey: MappingReceivable, Credit: gross, Description: "Credit note " + number})
}

// ── LinkReplacementInvoice ───────────────────────────────────────────────────

type linkReq struct {
	cmdBase
	ReplacementInvoiceID string `json:"replacement_invoice_id"`
}

// linkReplacementInvoice records that an ISSUED credit note is followed by a replacement invoice (the correction chain, append-only).
// The issued note is never edited (only its version moves). When the credit note(s) cover the source invoice's whole gross, the
// source is also linked as FULL_CORRECTION through the AR-04 port, which moves it Issued -> Corrected.
func (m *Module) linkReplacementInvoice(w http.ResponseWriter, r *http.Request) {
	var req linkReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	if err := mustUUID("replacement_invoice_id", req.ReplacementInvoiceID); err != nil {
		m.fail(w, err)
		return
	}
	c, h, ok := m.prepare(w, r, &req.cmdBase, ActionCorrectionLink, true)
	if !ok {
		return
	}
	ctx := r.Context()
	switch {
	case h.Version != *req.ExpectedVersion:
		m.fail(w, kit.ErrStaleVersion)
		return
	case h.Kind != KindCredit || !h.State.IsIssued():
		m.fail(w, kit.Conflict(kit.CodeInvalidTransition, "a replacement invoice can be linked only to an issued credit note"))
		return
	}
	cc := c
	cc.LegalEntityID = h.LegalEntityID
	src, err := m.source(ctx, c.TenantID, h.SourceInvoiceID)
	if err != nil {
		m.fail(w, err)
		return
	}
	rep, err := m.source(ctx, c.TenantID, req.ReplacementInvoiceID)
	if err != nil {
		m.fail(w, err)
		return
	}
	if rep.InvoiceID == h.SourceInvoiceID || rep.LegalEntityID != h.LegalEntityID || rep.CustomerPartyID != h.CustomerPartyID || rep.Currency != h.Currency {
		m.fail(w, kit.Err(http.StatusUnprocessableEntity, CodeCorrectionSourceInvalid, "the replacement must be a different issued invoice of the same entity, customer and currency"))
		return
	}
	now := m.today()
	var out *Correction
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, c.TenantID, req.CorrectionID, true)
		if err != nil {
			return err
		}
		if cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		var exists int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ar06_replacement_links WHERE tenant_id = $1 AND correction_id = $2 AND replacement_invoice_id = $3`,
			c.TenantID, cur.CorrectionID, req.ReplacementInvoiceID).Scan(&exists); err != nil {
			return kit.MapPgErr(err)
		}
		if exists > 0 {
			out, err = loadCorrection(ctx, tx, c.TenantID, cur) // idempotent: the link was already recorded
			return err
		}
		if err := lockSource(ctx, tx, c.TenantID, cur.SourceInvoiceID); err != nil {
			return err
		}
		_, cg, err := creditedTotals(ctx, tx, c.TenantID, cur.SourceInvoiceID, "")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ar06_replacement_links (link_id, tenant_id, legal_entity_id, correction_id, replacement_invoice_id, replacement_number, reason_code, linked_by, linked_at)
			VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9)`, uuid.NewString(), c.TenantID, cur.LegalEntityID, cur.CorrectionID, req.ReplacementInvoiceID, rep.InvoiceNumber,
			req.ReasonCode, c.PrincipalID, now.UTC()); err != nil {
			return kit.MapPgErr(err)
		}
		full := cg == src.Gross
		if full {
			if err := m.invoices.LinkCorrection(ctx, tx, cc, cur.SourceInvoiceID, cur.CorrectionID+":replacement", ar04.KindFullCorrection); err != nil {
				var p *kit.Problem
				if errors.As(err, &p) {
					return err
				}
				return kit.Unavailable("AR-04 could not link the full correction to the source invoice")
			}
		}
		v, err := bump(ctx, tx, `UPDATE ar06_corrections SET version = version + 1, updated_at = $3 WHERE tenant_id = $1 AND correction_id = $2 AND version = $4 RETURNING version`,
			c.TenantID, cur.CorrectionID, now.UTC(), cur.Version)
		if err != nil {
			return err
		}
		nh := *cur
		nh.Version = v
		if err := record(ctx, tx, cc, &nh, hist{At: now, Command: "linkReplacementInvoice", Reason: req.ReasonCode, Evidence: req.EvidenceID, ContentHash: cur.IssuedContentHash,
			New: map[string]any{"replacement_invoice_id": req.ReplacementInvoiceID, "replacement_number": rep.InvoiceNumber, "full_correction": full}},
			func(o *Header) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: o.CorrectionID, EventType: EventReplacementLinked,
					Payload: evPayload(cc, o, map[string]any{"replacement_invoice_id": req.ReplacementInvoiceID, "full_correction": full})}}
			}); err != nil {
			return err
		}
		saved, err := getHeader(ctx, tx, c.TenantID, cur.CorrectionID, false)
		if err != nil {
			return err
		}
		out, err = loadCorrection(ctx, tx, c.TenantID, saved)
		return err
	})
	if err != nil {
		m.failStep(w, ctx, c, req.CorrectionID, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}
