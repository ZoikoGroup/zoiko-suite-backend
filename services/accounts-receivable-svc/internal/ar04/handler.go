package ar04

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
)

func errSoD(msg string) *kit.Problem { return kit.Forbidden(kit.CodeSegregationOfDuties, msg) }

func errApprovalInvalidated(msg string) *kit.Problem {
	return kit.Conflict(kit.CodeInvoiceApprovalInvalidated, msg)
}

func reasonRequired() *kit.Problem {
	return kit.Err(http.StatusBadRequest, kit.CodeReasonRequired, "reason_code is required")
}

func evidenceRequired() *kit.Problem {
	return kit.Err(http.StatusBadRequest, kit.CodeEvidenceRequired, "evidence_id is required")
}

// cmdBase is the envelope shared by every command on an existing invoice. legal_entity_id is
// optional: the entity is derived from the stored invoice and a body entity that disagrees is
// refused (spec invariant 16).
type cmdBase struct {
	InvoiceID       string `json:"invoice_id"`
	LegalEntityID   string `json:"legal_entity_id,omitempty"`
	ExpectedVersion *int   `json:"expected_version"`
	ReasonCode      string `json:"reason_code,omitempty"`
	EvidenceID      string `json:"evidence_id,omitempty"`
}

// prepare identifies the caller, loads the invoice to learn its entity, then runs Guard (tenant +
// entity reconciliation before authz) for the action.
func (m *Module) prepare(w http.ResponseWriter, r *http.Request, b *cmdBase, action string, versionRequired bool) (kit.Caller, *Header, bool) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return kit.Caller{}, nil, false
	}
	if err := mustUUID("invoice_id", b.InvoiceID); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if versionRequired && b.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return kit.Caller{}, nil, false
	}
	var h *Header
	if err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		h, err = getHeader(r.Context(), tx, c.TenantID, b.InvoiceID, false)
		return err
	}); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if b.LegalEntityID != "" && b.LegalEntityID != h.LegalEntityID {
		m.fail(w, kit.Forbidden(kit.CodeTenantScope, "legal_entity_id does not match the invoice's legal entity"))
		return kit.Caller{}, nil, false
	}
	c, ok = m.deps.Guard(w, r, h.LegalEntityID, action)
	if !ok {
		return kit.Caller{}, nil, false
	}
	return c, h, true
}

func evPayload(c kit.Caller, h *Header, extra map[string]any) map[string]any {
	p := map[string]any{"invoice_id": h.InvoiceID, "legal_entity_id": h.LegalEntityID, "customer_party_id": h.CustomerPartyID, "currency": h.Currency,
		"lifecycle_state": h.State, "version": h.Version, "net_total": h.NetTotal.String(), "tax_total": h.TaxTotal.String(), "gross_total": h.GrossTotal.String(),
		"content_hash": h.ContentHash, "correlation_id": c.CorrelationID}
	if h.InvoiceNumber != "" {
		p["invoice_number"] = h.InvoiceNumber
	}
	for k, x := range extra {
		p[k] = x
	}
	return p
}

// stepSpec describes one state-changing command on an existing invoice.
type stepSpec struct {
	action  string
	command string
	// tr is the lifecycle move (empty = none: the guard alone decides legality).
	tr Transition
	// post marks a command on an ISSUED invoice: it never rewrites content, its apply writes only the
	// one dimension it owns and runStep then records history, audit and events.
	post bool
	// versionOptional lets a system caller report an orthogonal-dimension fact without a version.
	versionOptional bool
	// guard is extra legality beyond the state machine.
	guard func(h *Header) error
	// pre runs outside the transaction with the current invoice loaded: profile, tax, authority and
	// segregation checks live here.
	pre func(ctx context.Context, c kit.Caller, inv *Invoice) error
	// apply mutates nh (a copy of the locked header, already moved by tr) and returns the evidence.
	apply func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, inv *Invoice, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error)
}

func (sp stepSpec) legal(h *Header) (State, error) {
	next := h.State
	if sp.tr != "" {
		n, err := Next(h.State, sp.tr)
		if err != nil {
			return "", err
		}
		next = n
	}
	if sp.guard != nil {
		if err := sp.guard(h); err != nil {
			return "", err
		}
	}
	return next, nil
}

// failStep answers an error; a denial first leaves its audit evidence.
func (m *Module) failStep(w http.ResponseWriter, ctx context.Context, c kit.Caller, invoiceID string, err error) {
	var d *denial
	if errors.As(err, &d) {
		m.recordDenial(ctx, c, invoiceID, d)
	}
	m.fail(w, err)
}

func (m *Module) runStep(w http.ResponseWriter, r *http.Request, req *cmdBase, sp stepSpec) {
	c, h, ok := m.prepare(w, r, req, sp.action, !sp.versionOptional)
	if !ok {
		return
	}
	ctx := r.Context()
	if req.ExpectedVersion != nil && h.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	if _, err := sp.legal(h); err != nil {
		m.fail(w, err)
		return
	}
	if sp.pre != nil {
		var inv *Invoice
		if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) {
			inv, err = loadInvoice(ctx, tx, c.TenantID, h)
			return err
		}); err != nil {
			m.fail(w, err)
			return
		}
		if err := sp.pre(ctx, c, inv); err != nil {
			m.failStep(w, ctx, c, h.InvoiceID, err)
			return
		}
	}
	now := m.today()
	var out *Invoice
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, c.TenantID, req.InvoiceID, true)
		if err != nil {
			return err
		}
		if req.ExpectedVersion != nil && cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		next, err := sp.legal(cur)
		if err != nil {
			return err
		}
		inv, err := loadInvoice(ctx, tx, c.TenantID, cur)
		if err != nil {
			return err
		}
		nh := *cur
		nh.State = next
		hs, events, err := sp.apply(ctx, tx, c, cur, inv, &nh, now)
		if err != nil {
			return err
		}
		hs.At = now
		if hs.Command == "" {
			hs.Command = sp.command
		}
		var saved *Header
		if sp.post {
			if saved, err = getHeader(ctx, tx, c.TenantID, req.InvoiceID, false); err != nil {
				return err
			}
			if hs.ContentHash == "" {
				hs.ContentHash = saved.IssuedContentHash
			}
			if err := record(ctx, tx, c, saved, hs, events); err != nil {
				return err
			}
		} else {
			if hs.ContentHash == "" {
				hs.ContentHash = nh.ContentHash
			}
			if saved, err = commit(ctx, tx, c, cur, &nh, hs, events); err != nil {
				return err
			}
		}
		out, err = loadInvoice(ctx, tx, c.TenantID, saved)
		return err
	})
	if err != nil {
		m.failStep(w, ctx, c, req.InvoiceID, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// ── order basis ──────────────────────────────────────────────────────────────

// checkBasis validates the optional AR-03 order / milestone basis: the order must be billable, belong
// to the invoice's customer and currency and, once the net is known (submit), the net must equal
// the milestone's eligible net exactly.
func (m *Module) checkBasis(ctx context.Context, tenantID string, ct Content, customerID, currency string, net *kit.Amount) error {
	if ct.OrderID == "" {
		return nil
	}
	if m.orders == nil {
		return kit.Unavailable("sales order service not wired: an order-based invoice cannot be validated")
	}
	b, err := m.orders.InvoiceEligibility(ctx, tenantID, ct.OrderID, ct.MilestoneID)
	if err != nil {
		var p *kit.Problem
		if errors.As(err, &p) {
			return err
		}
		m.deps.Log.Error("ar04 order eligibility unavailable - failing closed")
		return kit.Unavailable("sales order service unavailable")
	}
	switch {
	case b == nil:
		return kit.Unprocessable(kit.CodeOrderNotBillable, "the order basis returned no billable position")
	case b.CustomerParty != customerID:
		return kit.Unprocessable(kit.CodeOrderNotBillable, "the order belongs to another customer")
	case b.Currency != currency:
		return kit.Unprocessable(kit.CodeOrderNotBillable, "the order is in another currency")
	case net != nil && *net != b.EligibleNet:
		return kit.Unprocessable(kit.CodeOrderNotBillable, "the invoice net "+net.String()+" is not the milestone's eligible net "+b.EligibleNet.String())
	}
	return nil
}

// ── CreateInvoiceDraft ───────────────────────────────────────────────────────

type createReq struct {
	LegalEntityID   string      `json:"legal_entity_id"`
	CustomerPartyID string      `json:"customer_party_id"`
	Currency        string      `json:"currency"`
	SupplyDate      string      `json:"supply_date"`
	DueDate         string      `json:"due_date,omitempty"`
	CustomerRef     string      `json:"customer_reference,omitempty"`
	Notes           string      `json:"notes,omitempty"`
	OrderID         string      `json:"order_id,omitempty"`
	MilestoneID     string      `json:"milestone_id,omitempty"`
	TaxContext      TaxContext  `json:"tax_context"`
	Lines           []LineInput `json:"lines"`
	ReasonCode      string      `json:"reason_code,omitempty"`
	EvidenceID      string      `json:"evidence_id,omitempty"`
}

func (m *Module) createInvoiceDraft(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if err := mustUUID("customer_party_id", req.CustomerPartyID); err != nil {
		m.fail(w, err)
		return
	}
	if !currencyRe.MatchString(req.Currency) {
		m.fail(w, kit.Invalid("currency must be a 3-letter upper-case ISO 4217 code"))
		return
	}
	content := Content{SupplyDate: req.SupplyDate, RequestedDueDate: req.DueDate, CustomerRef: req.CustomerRef, Notes: req.Notes, OrderID: req.OrderID,
		MilestoneID: req.MilestoneID, Tax: req.TaxContext}
	if err := content.Validate(); err != nil {
		m.fail(w, err)
		return
	}
	lines, err := BuildLines(req.Currency, req.Lines)
	if err != nil {
		m.fail(w, err)
		return
	}
	totals, err := TotalsOf(lines, nil, req.Currency)
	if err != nil {
		m.fail(w, err)
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionInvoiceManage)
	if !ok {
		return
	}
	ctx := r.Context()
	// A draft needs an ACTIVE customer profile (no stale fallback); credit holds are an order-level concern.
	if _, err := m.profile(ctx, c.TenantID, c.LegalEntityID, req.CustomerPartyID, m.today()); err != nil {
		m.fail(w, err)
		return
	}
	if err := m.checkBasis(ctx, c.TenantID, content, req.CustomerPartyID, req.Currency, nil); err != nil {
		m.fail(w, err)
		return
	}
	now := m.today()
	h := &Header{InvoiceID: uuid.NewString(), TenantID: c.TenantID, LegalEntityID: c.LegalEntityID, CustomerPartyID: req.CustomerPartyID, Currency: req.Currency,
		State: StateDraft, Dimensions: InitialDimensions(), AccountingState: "NOT_QUEUED", Content: content, NetTotal: totals.Net, TaxTotal: 0, GrossTotal: totals.Gross,
		CreatedBy: c.PrincipalID, Version: 1}
	h.ContentHash = ContentHash(h, lines, nil)
	var out *Invoice
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		if err := insertHeader(ctx, tx, c, h, now); err != nil {
			return err
		}
		if err := insertLines(ctx, tx, c, h.InvoiceID, lines); err != nil {
			return err
		}
		saved, err := getHeader(ctx, tx, c.TenantID, h.InvoiceID, false)
		if err != nil {
			return err
		}
		newVals := map[string]any{"lifecycle_state": StateDraft, "currency": req.Currency, "net_total": totals.Net.String(), "lines": len(lines),
			"supply_date": content.SupplyDate, "customer_party_id": req.CustomerPartyID}
		if content.OrderID != "" {
			newVals["order_id"], newVals["milestone_id"] = content.OrderID, content.MilestoneID
		}
		if err := record(ctx, tx, c, saved, hist{At: now, Command: "createInvoiceDraft", Reason: req.ReasonCode, Evidence: req.EvidenceID, ContentHash: h.ContentHash, New: newVals},
			func(o *Header) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: o.InvoiceID, EventType: EventInvoiceDraftCreated, Payload: evPayload(c, o, nil)}}
			}); err != nil {
			return err
		}
		out, err = loadInvoice(ctx, tx, c.TenantID, saved)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, out)
}

// ── AmendInvoiceDraft ────────────────────────────────────────────────────────

// amendReq carries NO tax field: a misspelled or attempted tax_amount is an unknown field and is
// refused (invariant 3: the invoice editor cannot supply or override tax).
type amendReq struct {
	cmdBase
	SupplyDate  *string      `json:"supply_date,omitempty"`
	DueDate     *string      `json:"due_date,omitempty"`
	CustomerRef *string      `json:"customer_reference,omitempty"`
	Notes       *string      `json:"notes,omitempty"`
	OrderID     *string      `json:"order_id,omitempty"`
	MilestoneID *string      `json:"milestone_id,omitempty"`
	TaxContext  *TaxContext  `json:"tax_context,omitempty"`
	Lines       *[]LineInput `json:"lines,omitempty"`
}

func (a amendReq) hasFields() bool {
	return a.SupplyDate != nil || a.DueDate != nil || a.CustomerRef != nil || a.Notes != nil || a.OrderID != nil || a.MilestoneID != nil || a.TaxContext != nil || a.Lines != nil
}

func (a amendReq) applyTo(c Content) Content {
	if a.SupplyDate != nil {
		c.SupplyDate = *a.SupplyDate
	}
	if a.DueDate != nil {
		c.RequestedDueDate = *a.DueDate
	}
	if a.CustomerRef != nil {
		c.CustomerRef = *a.CustomerRef
	}
	if a.Notes != nil {
		c.Notes = *a.Notes
	}
	if a.OrderID != nil {
		c.OrderID = *a.OrderID
	}
	if a.MilestoneID != nil {
		c.MilestoneID = *a.MilestoneID
	}
	if a.TaxContext != nil {
		c.Tax = *a.TaxContext
	}
	return c
}

func linesToInputs(ls []Line) []LineInput {
	out := make([]LineInput, len(ls))
	for i, l := range ls {
		out[i] = LineInput{Description: l.Description, ProductRef: l.ProductRef, Quantity: l.Quantity, UnitPrice: l.UnitPrice, TaxCategory: l.TaxCategory,
			ProductClassification: l.ProductClassification, SupplyKind: l.SupplyKind}
	}
	return out
}

// draftHash is the content hash of the draft part only (no tax result).
func draftHash(h *Header, lines []Line) string {
	ll := make([]Line, len(lines))
	copy(ll, lines)
	for i := range ll {
		ll[i].Tax = 0
	}
	return ContentHash(h, ll, nil)
}

// resetToDraft clears the tax result, the line taxes, the approval and the submission, and recomputes
// the draft totals and hash into nh. Used by an amendment and by an approval invalidation.
func resetToDraft(ctx context.Context, tx pgx.Tx, c kit.Caller, inv *Invoice, nh *Header, lines []Line) error {
	for i := range lines {
		lines[i].Tax = 0
	}
	if err := replaceLines(ctx, tx, c, inv.InvoiceID, lines); err != nil {
		return err
	}
	if err := replaceTax(ctx, tx, c, inv.InvoiceID, nil); err != nil {
		return err
	}
	var net kit.Amount
	for _, l := range lines {
		net = net.Add(l.Net)
	}
	nh.NetTotal, nh.TaxTotal, nh.GrossTotal = net, 0, net
	nh.ApprovedHash, nh.ApprovedBy, nh.ApprovedAt, nh.SubmittedBy, nh.SubmittedAt = "", "", nil, "", nil
	nh.ContentHash = ContentHash(nh, lines, nil)
	return nil
}

func (m *Module) amendInvoiceDraft(w http.ResponseWriter, r *http.Request) {
	var req amendReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if !req.hasFields() {
		m.fail(w, kit.Invalid("nothing to amend: supply at least one content field"))
		return
	}
	m.runStep(w, r, &req.cmdBase, stepSpec{action: ActionInvoiceManage, command: "amendInvoiceDraft", tr: TrAmend,
		pre: func(ctx context.Context, c kit.Caller, inv *Invoice) error {
			next := req.applyTo(inv.Content)
			if err := next.Validate(); err != nil {
				return err
			}
			if _, err := m.profile(ctx, c.TenantID, inv.LegalEntityID, inv.CustomerPartyID, m.today()); err != nil {
				return err
			}
			return m.checkBasis(ctx, c.TenantID, next, inv.CustomerPartyID, inv.Currency, nil)
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, inv *Invoice, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			next := req.applyTo(inv.Content)
			if err := next.Validate(); err != nil {
				return hist{}, nil, err
			}
			inputs := linesToInputs(inv.Lines)
			if req.Lines != nil {
				inputs = *req.Lines
			}
			lines, err := BuildLines(cur.Currency, inputs)
			if err != nil {
				return hist{}, nil, err
			}
			hdr := *cur
			hdr.Content = next
			if draftHash(&hdr, lines) == draftHash(cur, inv.Lines) {
				return hist{}, nil, kit.Invalid("the amendment changes nothing")
			}
			nh.Content = next
			if err := resetToDraft(ctx, tx, c, inv, nh, lines); err != nil {
				return hist{}, nil, err
			}
			prior := map[string]any{"lifecycle_state": cur.State, "net_total": cur.NetTotal.String(), "content_hash": cur.ContentHash}
			nw := map[string]any{"lifecycle_state": StateDraft, "net_total": nh.NetTotal.String(), "content_hash": nh.ContentHash, "lines": len(lines)}
			if cur.ApprovedHash != "" {
				nw["approval_invalidated"], prior["approved_content_hash"] = true, cur.ApprovedHash
			}
			if len(inv.TaxGroups) > 0 {
				nw["tax_result_discarded"] = true
			}
			return hist{Reason: req.ReasonCode, Evidence: req.EvidenceID, Prior: prior, New: nw}, nil, nil
		}})
}

// ── SubmitInvoiceForApproval ─────────────────────────────────────────────────

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// determineTax obtains one tax determination per tax group from tax-determination-svc. AR never
// computes tax: anything the TAX domain cannot answer leaves the invoice a draft.
func (m *Module) determineTax(ctx context.Context, c kit.Caller, inv *Invoice) ([]TaxGroup, error) {
	if m.tax == nil {
		return nil, kit.Unavailable("tax determination service not wired: the invoice cannot be submitted")
	}
	specs := BuildGroups(inv.Lines)
	out := make([]TaxGroup, 0, len(specs))
	now := m.today()
	for _, g := range specs {
		d, err := m.tax.Determine(ctx, c.SvcCaller(), taxdet.Request{
			TransactionID: fmt.Sprintf("ar04:%s:v%d:g%d", inv.InvoiceID, inv.Version, g.GroupNo), SourceModule: "AR", LegalEntityID: inv.LegalEntityID,
			JurisdictionID: inv.Content.Tax.JurisdictionID, SupplyJurisdictionID: inv.Content.Tax.SupplyJurisdictionID, TaxCategory: g.TaxCategory,
			GrossAmount: g.Net, Currency: inv.Currency, EffectiveFrom: inv.Content.SupplyDate, SupplyDate: inv.Content.SupplyDate,
			SellerPartyID: inv.LegalEntityID, BuyerPartyID: inv.CustomerPartyID, ProductClassification: g.ProductClassification, SupplyKind: g.SupplyKind,
			SupplyType: inv.Content.Tax.SupplyType, BuyerTaxRegistrationID: inv.Content.Tax.BuyerTaxRegistrationID, EvaluatedBy: c.PrincipalID})
		switch {
		case errors.Is(err, taxdet.ErrRejected):
			return nil, kit.Unprocessable(kit.CodeTaxDeterminationMissing, "tax determination could not be produced: "+clip(err.Error(), 400))
		case err != nil:
			return nil, kit.Unavailable("tax determination service unavailable: the invoice cannot be submitted")
		}
		tg, err := GroupFromDetermination(d, g, inv.LegalEntityID, inv.Currency, c.PrincipalID, now)
		if err != nil {
			return nil, err
		}
		out = append(out, tg)
	}
	return out, nil
}

func (m *Module) submitInvoiceForApproval(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	var groups []TaxGroup
	m.runStep(w, r, &req, stepSpec{action: ActionInvoiceManage, command: "submitInvoiceForApproval", tr: TrSubmit,
		pre: func(ctx context.Context, c kit.Caller, inv *Invoice) error {
			if err := ValidateForTax(inv.Content, inv.Lines); err != nil {
				return err
			}
			if _, err := m.profile(ctx, c.TenantID, inv.LegalEntityID, inv.CustomerPartyID, m.today()); err != nil {
				return err
			}
			var net kit.Amount
			for _, l := range inv.Lines {
				net = net.Add(l.Net)
			}
			if !net.IsPositive() {
				return kit.Invalid("the invoice net must be greater than zero")
			}
			if err := m.checkBasis(ctx, c.TenantID, inv.Content, inv.CustomerPartyID, inv.Currency, &net); err != nil {
				return err
			}
			g, err := m.determineTax(ctx, c, inv)
			groups = g
			return err
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, inv *Invoice, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if len(groups) == 0 {
				return hist{}, nil, kit.Unprocessable(kit.CodeTaxDeterminationMissing, "no tax determination was obtained for the invoice")
			}
			lines := make([]Line, len(inv.Lines))
			copy(lines, inv.Lines)
			if err := ApplyTax(lines, groups, cur.Currency); err != nil {
				return hist{}, nil, err
			}
			totals, err := TotalsOf(lines, groups, cur.Currency)
			if err != nil {
				return hist{}, nil, err
			}
			if !totals.Net.IsPositive() {
				return hist{}, nil, kit.Invalid("the invoice net must be greater than zero")
			}
			if err := replaceLines(ctx, tx, c, cur.InvoiceID, lines); err != nil {
				return hist{}, nil, err
			}
			if err := replaceTax(ctx, tx, c, cur.InvoiceID, groups); err != nil {
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
					"gross_total": totals.Gross.String(), "tax_determination_ids": ids}}, nil, nil
		}})
}

// ── ApproveInvoice ───────────────────────────────────────────────────────────

type approveReq struct {
	cmdBase
	ContentHash string `json:"content_hash"`
}

// integrity proves the stored rows still hash to the recorded content hash.
func integrity(inv *Invoice) error {
	if ContentHash(&inv.Header, inv.Lines, inv.TaxGroups) != inv.ContentHash {
		return errApprovalInvalidated("the stored invoice no longer matches its recorded content hash: re-approval is required")
	}
	return nil
}

func (m *Module) approveInvoice(w http.ResponseWriter, r *http.Request) {
	var req approveReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ContentHash) == "" {
		m.fail(w, kit.Invalid("content_hash is required: an approval is bound to the content the approver saw"))
		return
	}
	m.runStep(w, r, &req.cmdBase, stepSpec{action: ActionInvoiceApprove, command: "approveInvoice", tr: TrApprove,
		pre: func(ctx context.Context, c kit.Caller, inv *Invoice) error {
			if req.ContentHash != inv.ContentHash {
				return errApprovalInvalidated("content_hash does not match the current invoice content: an approval cannot be reused after the invoice changed")
			}
			if err := integrity(inv); err != nil {
				return err
			}
			if len(inv.TaxGroups) == 0 {
				return kit.Unprocessable(kit.CodeTaxDeterminationMissing, "the invoice has no tax determination to approve")
			}
			if c.PrincipalID == inv.CreatedBy || c.PrincipalID == inv.SubmittedBy {
				return deny(errSoD("an invoice must be approved by a principal who neither created nor submitted it"), "approveInvoice",
					map[string]any{"gross_total": inv.GrossTotal.String(), "content_hash": inv.ContentHash})
			}
			return nil
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, inv *Invoice, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if req.ContentHash != cur.ContentHash {
				return hist{}, nil, errApprovalInvalidated("the invoice changed while the approval was in flight")
			}
			if err := integrity(inv); err != nil {
				return hist{}, nil, err
			}
			if err := insertApproval(ctx, tx, c, cur, now); err != nil {
				return hist{}, nil, err
			}
			t := now
			nh.ApprovedHash, nh.ApprovedBy, nh.ApprovedAt = cur.ContentHash, c.PrincipalID, &t
			return hist{Reason: req.ReasonCode, Evidence: req.EvidenceID, ApproverID: c.PrincipalID,
					Prior: map[string]any{"lifecycle_state": cur.State},
					New:   map[string]any{"lifecycle_state": nh.State, "approved_content_hash": cur.ContentHash, "gross_total": cur.GrossTotal.String()}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.InvoiceID, EventType: EventInvoiceApproved,
						Payload: evPayload(c, h, map[string]any{"approved_by": c.PrincipalID, "approved_content_hash": cur.ContentHash})}}
				}, nil
		}})
}

// ── CancelUnissuedInvoice ────────────────────────────────────────────────────

func (m *Module) cancelUnissuedInvoice(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	m.runStep(w, r, &req, stepSpec{action: ActionInvoiceManage, command: "cancelUnissuedInvoice", tr: TrCancel,
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, inv *Invoice, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			t := now
			nh.CancelReason, nh.CancelledBy, nh.CancelledAt = req.ReasonCode, c.PrincipalID, &t
			return hist{Reason: req.ReasonCode, Evidence: req.EvidenceID,
					Prior: map[string]any{"lifecycle_state": cur.State},
					New:   map[string]any{"lifecycle_state": nh.State, "cancelled_by": c.PrincipalID}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.InvoiceID, EventType: EventInvoiceCancelled,
						Payload: evPayload(c, h, map[string]any{"reason_code": req.ReasonCode, "prior_state": cur.State})}}
				}, nil
		}})
}

// ── IssueInvoice ─────────────────────────────────────────────────────────────

// invalidation is returned out of the issue transaction when the approved basis no longer holds
// (tax result changed, stored content no longer matches its approval): the transaction rolls back
// and the invoice is then returned to DRAFT in its own transaction.
type invalidation struct {
	Reason string
	Detail map[string]any
}

func (i *invalidation) Error() string { return "approval invalidated: " + i.Reason }

// verifyTax re-verifies every approved determination with tax-determination-svc: it must still exist,
// be usable, belong to the same entity and currency, and carry the same tax amount and provenance.
// Missing => TAX_DETERMINATION_MISSING; a changed result => invalidation; unreachable => 503.
func (m *Module) verifyTax(ctx context.Context, c kit.Caller, inv *Invoice) error {
	if len(inv.TaxGroups) == 0 {
		return kit.Unprocessable(kit.CodeTaxDeterminationMissing, "the invoice has no tax determination: it cannot be issued")
	}
	for _, g := range inv.TaxGroups {
		d, err := m.tax.Get(ctx, c.SvcCaller(), g.DeterminationID)
		switch {
		case errors.Is(err, taxdet.ErrNotFound):
			return kit.Unprocessable(kit.CodeTaxDeterminationMissing, "tax determination "+g.DeterminationID+" no longer exists: the invoice cannot be issued")
		case err != nil:
			return kit.Unavailable("tax determination service unavailable: the approved tax result cannot be re-verified")
		case d == nil:
			return kit.Unprocessable(kit.CodeTaxDeterminationMissing, "tax determination "+g.DeterminationID+" returned no result")
		}
		var why string
		switch {
		case !d.Usable():
			why = "determination is no longer usable (status " + d.Status + ")"
		case d.LegalEntityID != "" && d.LegalEntityID != inv.LegalEntityID:
			why = "determination now belongs to another legal entity"
		case d.Currency != "" && d.Currency != inv.Currency:
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

// invalidateApproval returns an approved invoice to DRAFT (approval and tax result discarded) after
// the approved basis was found stale. It runs in its own transaction because the issue transaction
// rolled back; it is a no-op when the invoice has already moved on.
func (m *Module) invalidateApproval(ctx context.Context, c kit.Caller, invoiceID string, expected int, inv *invalidation) {
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, c.TenantID, invoiceID, true)
		if err != nil {
			return err
		}
		if cur.Version != expected || cur.State != StateApproved {
			return nil
		}
		full, err := loadInvoice(ctx, tx, c.TenantID, cur)
		if err != nil {
			return err
		}
		nh := *cur
		nh.State, err = Next(cur.State, TrInvalidate)
		if err != nil {
			return err
		}
		lines := make([]Line, len(full.Lines))
		copy(lines, full.Lines)
		if err := resetToDraft(ctx, tx, c, full, &nh, lines); err != nil {
			return err
		}
		now := m.today()
		newVals := map[string]any{"lifecycle_state": StateDraft, "invalidation": inv.Reason}
		for k, v := range inv.Detail {
			newVals[k] = v
		}
		_, err = commit(ctx, tx, c, cur, &nh, hist{At: now, Command: "invalidateApproval", Reason: inv.Reason, ContentHash: nh.ContentHash,
			Prior: map[string]any{"lifecycle_state": cur.State, "approved_content_hash": cur.ApprovedHash}, New: newVals},
			func(h *Header) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: h.InvoiceID, EventType: EventInvoiceApprovalInvalidated,
					Payload: evPayload(c, h, map[string]any{"reason": inv.Reason, "detail": inv.Detail})}}
			})
		return err
	})
	if err != nil {
		m.deps.Log.Error("ar04 could not return an invalidated approval to draft")
	}
}

func (m *Module) issueInvoice(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	c, h, ok := m.prepare(w, r, &req, ActionInvoiceIssue, true)
	if !ok {
		return
	}
	ctx := r.Context()
	// An invoice that is already issued is answered before the version is compared: a second, differently
	// keyed issue of the same invoice is INVOICE_ALREADY_ISSUED, never a second number.
	switch {
	case h.State.IsIssued():
		m.fail(w, kit.Conflict(kit.CodeInvoiceAlreadyIssued, "the invoice is already issued as "+h.InvoiceNumber+": it is never issued twice"))
		return
	case h.State == StateCancelled:
		m.fail(w, kit.Conflict(kit.CodeInvalidTransition, "a cancelled invoice cannot be issued"))
		return
	case h.Version != *req.ExpectedVersion:
		m.fail(w, kit.ErrStaleVersion)
		return
	case h.State != StateApproved:
		m.fail(w, kit.Conflict(kit.CodeInvalidTransition, "ISSUE is not allowed from state "+string(h.State)+": the invoice must be approved"))
		return
	}
	// Every prerequisite that is not wired fails closed BEFORE any state change, number or posting.
	switch {
	case m.openItems == nil:
		m.fail(w, kit.Unavailable("AR-07 receivables not wired: an issued invoice without a receivable is a control defect, so nothing is issued"))
		return
	case m.tax == nil:
		m.fail(w, kit.Unavailable("tax determination service not wired: the approved tax result cannot be re-verified"))
		return
	case m.periods == nil:
		m.fail(w, kit.Unavailable("accounting period service not wired: the issue date's period cannot be verified"))
		return
	}
	// Maker-checker: the approver is never the creator or submitter (enforced at approval); above the
	// materiality threshold the issuer must also be a different principal from the approver.
	if IsMaterial(h.GrossTotal) && c.PrincipalID == h.ApprovedBy {
		m.failStep(w, ctx, c, h.InvoiceID, deny(errSoD("a material invoice must be issued by a principal other than its approver"), "issueInvoice",
			map[string]any{"gross_total": h.GrossTotal.String(), "materiality_threshold": MaterialInvoiceThreshold.String(), "approved_by": h.ApprovedBy}))
		return
	}
	now := m.today()
	issueDate := day(now)
	view, err := m.profile(ctx, c.TenantID, h.LegalEntityID, h.CustomerPartyID, now)
	if err != nil {
		m.fail(w, err)
		return
	}
	dueDate := day(now.AddDate(0, 0, view.PaymentTermsDays))
	if rd := h.Content.RequestedDueDate; rd != "" {
		if rd < issueDate {
			m.fail(w, kit.Unprocessable(kit.CodeValidationFailed, "the requested due date "+rd+" is before the issue date "+issueDate))
			return
		}
		dueDate = rd
	}
	einvRequired := false
	if m.einvoice != nil {
		einvRequired, err = m.einvoice.Required(ctx, c.TenantID, h.LegalEntityID, h.Content.Tax.JurisdictionID, kit.DocInvoice)
		if err != nil {
			m.deps.Log.Error("ar04 e-invoice requirement unavailable - failing closed")
			m.fail(w, kit.Unavailable("e-invoice requirement could not be determined"))
			return
		}
	}

	var out *Invoice
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, c.TenantID, req.InvoiceID, true)
		if err != nil {
			return err
		}
		if cur.State.IsIssued() {
			return kit.Conflict(kit.CodeInvoiceAlreadyIssued, "the invoice is already issued as "+cur.InvoiceNumber+": it is never issued twice")
		}
		if cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		if _, err := Next(cur.State, TrIssue); err != nil {
			return err
		}
		inv, err := loadInvoice(ctx, tx, c.TenantID, cur)
		if err != nil {
			return err
		}
		// The approval is bound to the content hash: the stored rows must still hash to it.
		if cur.ApprovedHash == "" || cur.ApprovedHash != cur.ContentHash || ContentHash(&inv.Header, inv.Lines, inv.TaxGroups) != cur.ApprovedHash {
			return &invalidation{Reason: "APPROVED_CONTENT_NO_LONGER_MATCHES", Detail: map[string]any{"approved_content_hash": cur.ApprovedHash, "content_hash": cur.ContentHash}}
		}
		cc := c
		cc.LegalEntityID = cur.LegalEntityID
		if err := m.verifyTax(ctx, cc, inv); err != nil {
			return err
		}
		periodKey, err := m.periods.PostingPeriod(ctx, cc, cur.LegalEntityID, now)
		if err != nil {
			return err
		}
		prof, err := numberingProfile(ctx, tx, c.TenantID, cur.LegalEntityID)
		if err != nil {
			return err
		}
		seq, err := nextSequence(ctx, tx, c.TenantID, cur.LegalEntityID, prof.Series)
		if err != nil {
			return err
		}
		number := prof.Format(seq)
		canonical, hash, err := BuildIssued(IssueInput{H: &inv.Header, Lines: inv.Lines, Groups: inv.TaxGroups, Number: number, Series: prof.Series, Sequence: seq,
			IssueDate: issueDate, DueDate: dueDate, TermsCode: view.PaymentTermsCode, TermsDays: view.PaymentTermsDays, IssuedBy: c.PrincipalID, IssuedAt: now,
			EInvoiceRequired: einvRequired})
		if err != nil {
			return err
		}
		issueT, _ := parseDate(issueDate)
		dueT, _ := parseDate(dueDate)
		ref, err := m.openItems.CreateFromDocument(ctx, tx, cc, kit.OpenItemFromDocument{DocumentKind: kit.DocInvoice, SourceDocumentID: cur.InvoiceID,
			DocumentNumber: number, CustomerPartyID: cur.CustomerPartyID, Currency: cur.Currency, OriginalAmount: cur.GrossTotal, IssueDate: issueT, DueDate: dueT})
		if err != nil {
			var p *kit.Problem
			if errors.As(err, &p) {
				return err
			}
			m.deps.Log.Error("ar04 receivable creation failed - the invoice is not issued")
			return kit.Unavailable("AR-07 could not create the receivable: the invoice is not issued")
		}
		if strings.TrimSpace(ref.OpenItemID) == "" {
			return kit.Unavailable("AR-07 returned no open item: the invoice is not issued")
		}
		saved, err := writeIssued(ctx, tx, cc, cur, issueWrite{Number: number, Series: prof.Series, Sequence: seq, IssueDate: issueDate, DueDate: dueDate,
			TermsCode: view.PaymentTermsCode, TermsDays: view.PaymentTermsDays, IssuedBy: c.PrincipalID, IssuedAt: now, Canonical: canonical, Hash: hash,
			OpenItemID: ref.OpenItemID, EInvoiceRequired: einvRequired})
		if err != nil {
			return err
		}
		if cur.Content.OrderID != "" {
			if m.orders == nil {
				return kit.Unavailable("sales order service not wired: the order cannot be marked invoiced")
			}
			if err := m.orders.RecordInvoiced(ctx, tx, cc, cur.Content.OrderID, cur.Content.MilestoneID, cur.InvoiceID, cur.NetTotal); err != nil {
				var p *kit.Problem
				if errors.As(err, &p) {
					return err
				}
				return kit.Unavailable("sales order service could not record the invoicing")
			}
		}
		// Invariant 14: the accounting consequence is a durable event on the ACC-04 queue, in this transaction.
		lines := []kit.PostingLine{{MappingKey: MappingReceivable, Debit: cur.GrossTotal, Description: "Customer invoice " + number}}
		lines = append(lines, kit.PostingLine{MappingKey: MappingRevenue, Credit: cur.NetTotal, Description: "Customer invoice " + number})
		if cur.TaxTotal.IsPositive() {
			lines = append(lines, kit.PostingLine{MappingKey: MappingTaxOutput, Credit: cur.TaxTotal, Description: "Output tax " + number})
		}
		if err := kit.QueuePosting(ctx, tx, cc, kit.SourceInvoice, cur.InvoiceID, kit.PostingEvent{FiscalPeriod: periodKey, Description: "Customer invoice " + number,
			SourceEventID: cur.InvoiceID, TransactionCurrency: cur.Currency, DocumentDate: issueDate, PostingDate: issueDate, Lines: lines}); err != nil {
			return err
		}
		ids := make([]string, len(inv.TaxGroups))
		for i, g := range inv.TaxGroups {
			ids[i] = g.DeterminationID
		}
		hs := hist{At: now, Command: "issueInvoice", Reason: req.ReasonCode, Evidence: req.EvidenceID, ApproverID: cur.ApprovedBy, ContentHash: hash,
			Prior: map[string]any{"lifecycle_state": cur.State},
			New: map[string]any{"lifecycle_state": StateIssued, "invoice_number": number, "numbering_series": prof.Series, "sequence_no": seq, "issue_date": issueDate,
				"due_date": dueDate, "issued_content_hash": hash, "open_item_id": ref.OpenItemID, "tax_determination_ids": ids, "fiscal_period": periodKey,
				"einvoice_required": einvRequired}}
		if err := record(ctx, tx, cc, saved, hs, func(o *Header) []kit.Event {
			return []kit.Event{{AggregateType: AggregateType, AggregateID: o.InvoiceID, EventType: EventCustomerInvoiceIssued,
				Payload: evPayload(cc, o, map[string]any{"issue_date": issueDate, "due_date": dueDate, "issued_content_hash": hash, "open_item_id": ref.OpenItemID,
					"tax_determination_ids": ids, "order_id": cur.Content.OrderID, "milestone_id": cur.Content.MilestoneID, "einvoice_required": einvRequired,
					"approved_by": cur.ApprovedBy})}}
		}); err != nil {
			return err
		}
		out, err = loadInvoice(ctx, tx, c.TenantID, saved)
		return err
	})
	if err != nil {
		var inval *invalidation
		if errors.As(err, &inval) {
			m.invalidateApproval(ctx, c, req.InvoiceID, *req.ExpectedVersion, inval)
			m.fail(w, errApprovalInvalidated("the approved basis no longer holds ("+inval.Reason+"): the invoice was returned to draft and must be re-submitted and re-approved"))
			return
		}
		m.failStep(w, ctx, c, req.InvoiceID, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// ── RequestLegalCancellation / DecideLegalCancellation ───────────────────────

// The request NEVER edits or voids the invoice: the jurisdiction semantics of cancelling an issued
// invoice is a correction document (AR-06). It records a request with reason and evidence on the
// attention dimension; a different principal decides it.

func (m *Module) requestLegalCancellation(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	if strings.TrimSpace(req.EvidenceID) == "" {
		m.fail(w, evidenceRequired())
		return
	}
	m.runStep(w, r, &req, stepSpec{action: ActionInvoiceCancelRequest, command: "requestLegalCancellation", post: true,
		guard: func(h *Header) error {
			switch {
			case h.State != StateIssued:
				return kit.Conflict(kit.CodeInvalidTransition, "a legal cancellation can be requested only for an issued invoice (state "+string(h.State)+")")
			case h.Attention == AttentionCancelReq || h.Attention == AttentionCancelOK:
				return kit.Conflict(kit.CodeInvalidTransition, "a cancellation request is already open for this invoice")
			}
			return nil
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, inv *Invoice, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			id := uuid.NewString()
			if _, err := tx.Exec(ctx, `INSERT INTO ar04_cancellation_requests (request_id, tenant_id, legal_entity_id, invoice_id, status, reason_code, evidence_id, requested_by, requested_at)
				VALUES ($1,$2,$3,$4,'REQUESTED',$5,$6,$7,$8)`, id, c.TenantID, cur.LegalEntityID, cur.InvoiceID, req.ReasonCode, req.EvidenceID, c.PrincipalID, now.UTC()); err != nil {
				if kit.IsUniqueViolation(err, "") {
					return hist{}, nil, kit.Conflict(kit.CodeInvalidTransition, "a cancellation request is already open for this invoice")
				}
				return hist{}, nil, kit.MapPgErr(err)
			}
			if _, err := writeAttention(ctx, tx, c.TenantID, cur, AttentionCancelReq, now); err != nil {
				return hist{}, nil, err
			}
			return hist{Reason: req.ReasonCode, Evidence: req.EvidenceID,
					Prior: map[string]any{"attention_state": cur.Attention},
					New:   map[string]any{"attention_state": AttentionCancelReq, "request_id": id, "lifecycle_state": cur.State}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.InvoiceID, EventType: EventInvoiceCancellationRequest,
						Payload: evPayload(c, h, map[string]any{"request_id": id, "requested_by": c.PrincipalID, "reason_code": req.ReasonCode, "evidence_id": req.EvidenceID})}}
				}, nil
		}})
}

type decideReq struct {
	cmdBase
	Decision string `json:"decision"` // APPROVE | REJECT
}

func (m *Module) decideLegalCancellation(w http.ResponseWriter, r *http.Request) {
	var req decideReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if req.Decision != "APPROVE" && req.Decision != "REJECT" {
		m.fail(w, kit.Invalid("decision must be APPROVE or REJECT"))
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	m.runStep(w, r, &req.cmdBase, stepSpec{action: ActionInvoiceCancelApprove, command: "decideLegalCancellation", post: true,
		guard: func(h *Header) error {
			if h.Attention != AttentionCancelReq {
				return kit.Conflict(kit.CodeInvalidTransition, "there is no pending cancellation request for this invoice")
			}
			return nil
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, inv *Invoice, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			open, err := openCancellation(ctx, tx, c.TenantID, cur.InvoiceID, true)
			if err != nil {
				return hist{}, nil, err
			}
			if open == nil || open.Status != "REQUESTED" {
				return hist{}, nil, kit.Conflict(kit.CodeInvalidTransition, "there is no pending cancellation request for this invoice")
			}
			if open.RequestedBy == c.PrincipalID {
				return hist{}, nil, deny(errSoD("a cancellation request must be decided by a principal other than the one who requested it"), "decideLegalCancellation",
					map[string]any{"request_id": open.RequestID, "requested_by": open.RequestedBy})
			}
			status, attention := "APPROVED", AttentionCancelOK
			if req.Decision == "REJECT" {
				status, attention = "REJECTED", AttentionNone
			}
			if _, err := tx.Exec(ctx, `UPDATE ar04_cancellation_requests SET status = $3, decided_by = $4, decided_at = $5, decision_reason = $6
				WHERE tenant_id = $1 AND request_id = $2`, c.TenantID, open.RequestID, status, c.PrincipalID, now.UTC(), req.ReasonCode); err != nil {
				return hist{}, nil, kit.MapPgErr(err)
			}
			if _, err := writeAttention(ctx, tx, c.TenantID, cur, attention, now); err != nil {
				return hist{}, nil, err
			}
			return hist{Reason: req.ReasonCode, Evidence: req.EvidenceID, ApproverID: c.PrincipalID,
					Prior: map[string]any{"attention_state": cur.Attention},
					New:   map[string]any{"attention_state": attention, "request_id": open.RequestID, "decision": status, "lifecycle_state": cur.State}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.InvoiceID, EventType: EventInvoiceCancellationDecided,
						Payload: evPayload(c, h, map[string]any{"request_id": open.RequestID, "decision": status, "decided_by": c.PrincipalID, "reason_code": req.ReasonCode})}}
				}, nil
		}})
}

// ── MarkDeliveryResult ───────────────────────────────────────────────────────

type deliveryReq struct {
	cmdBase
	Result        string `json:"result"` // SENT | DELIVERY_FAILED | DELIVERED
	Channel       string `json:"channel,omitempty"`
	Reference     string `json:"reference,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
}

// markDeliveryResult updates ONLY the delivery dimension of an issued invoice. A delivery failure never
// voids, edits or rolls back the invoice (spec failure semantics; anti-pattern "treat timeout as
// failure"). expected_version is optional here: it is a system report on an orthogonal dimension;
// when supplied it is enforced.
func (m *Module) markDeliveryResult(w http.ResponseWriter, r *http.Request) {
	var req deliveryReq
	if !kit.Decode(w, r, &req) {
		return
	}
	switch {
	case req.Result != DeliverySent && req.Result != DeliveryFailed && req.Result != DeliveryDelivered:
		m.fail(w, kit.Invalid("result must be SENT, DELIVERY_FAILED or DELIVERED"))
		return
	case len(req.Channel) > 64 || len(req.Reference) > 255 || len(req.FailureReason) > maxText:
		m.fail(w, kit.Invalid("channel (64), reference (255) or failure_reason are too long"))
		return
	case req.Result == DeliveryFailed && strings.TrimSpace(req.FailureReason) == "":
		m.fail(w, kit.Invalid("failure_reason is required when the result is DELIVERY_FAILED"))
		return
	}
	m.runStep(w, r, &req.cmdBase, stepSpec{action: ActionInvoiceDeliveryRecord, command: "markDeliveryResult", post: true, versionOptional: true,
		guard: func(h *Header) error {
			if !h.State.IsIssued() {
				return kit.Conflict(kit.CodeInvalidTransition, "a delivery result can be recorded only for an issued invoice (state "+string(h.State)+")")
			}
			return NextDelivery(h.Delivery, req.Result)
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, inv *Invoice, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			detail := req.FailureReason
			if req.Result != DeliveryFailed {
				detail = ""
			}
			if _, err := writeDelivery(ctx, tx, c.TenantID, cur, req.Result, req.Reference, detail, now); err != nil {
				return hist{}, nil, err
			}
			hs := hist{Reason: req.ReasonCode, Evidence: req.EvidenceID,
				Prior: map[string]any{"delivery_state": cur.Delivery},
				New: map[string]any{"delivery_state": req.Result, "channel": req.Channel, "reference": req.Reference, "failure_reason": detail,
					"lifecycle_state": cur.State}}
			if req.Result != DeliveryFailed {
				return hs, nil, nil
			}
			return hs, func(h *Header) []kit.Event {
				return []kit.Event{{AggregateType: AggregateType, AggregateID: h.InvoiceID, EventType: EventInvoiceDeliveryFailed,
					Payload: evPayload(c, h, map[string]any{"channel": req.Channel, "reference": req.Reference, "failure_reason": detail})}}
			}, nil
		}})
}

// ── LinkCorrectionDocument ───────────────────────────────────────────────────

type linkReq struct {
	cmdBase
	CorrectionID string `json:"correction_id"`
	Kind         string `json:"kind"` // CREDIT_NOTE | DEBIT_NOTE | FULL_CORRECTION
}

// linkCorrectionDocument is the operator route of the kit.Invoices.LinkCorrection port (AR-06 calls the
// port inside its own transaction). Reason and evidence are mandatory.
func (m *Module) linkCorrectionDocument(w http.ResponseWriter, r *http.Request) {
	var req linkReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	if strings.TrimSpace(req.EvidenceID) == "" {
		m.fail(w, evidenceRequired())
		return
	}
	c, h, ok := m.prepare(w, r, &req.cmdBase, ActionInvoiceCorrectionLink, true)
	if !ok {
		return
	}
	ctx := r.Context()
	if h.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	var out *Invoice
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		saved, err := m.linkCorrection(ctx, tx, c, req.InvoiceID, req.CorrectionID, req.Kind, req.ReasonCode, req.EvidenceID, req.ExpectedVersion)
		if err != nil {
			return err
		}
		out, err = loadInvoice(ctx, tx, c.TenantID, saved)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}
