package ar03

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// CodeApprovalInvalidated: an approval was requested for content that is no longer current.
const CodeApprovalInvalidated = "ORDER_APPROVAL_INVALIDATED"

func errSoD(msg string) *kit.Problem { return kit.Forbidden(kit.CodeSegregationOfDuties, msg) }

func errApprovalInvalidated(msg string) *kit.Problem {
	return kit.Conflict(CodeApprovalInvalidated, msg)
}

func reasonRequired() *kit.Problem {
	return kit.Err(http.StatusBadRequest, kit.CodeReasonRequired, "reason_code is required")
}

func evidenceRequired() *kit.Problem {
	return kit.Err(http.StatusBadRequest, kit.CodeEvidenceRequired, "evidence_id is required")
}

// cmdBase is the envelope shared by every command on an existing order. legal_entity_id is
// optional: the entity is derived from the stored order and a body entity that disagrees is
// refused (spec invariant 16).
type cmdBase struct {
	OrderID         string `json:"order_id"`
	LegalEntityID   string `json:"legal_entity_id,omitempty"`
	ExpectedVersion *int   `json:"expected_version"`
	ReasonCode      string `json:"reason_code,omitempty"`
	EvidenceID      string `json:"evidence_id,omitempty"`
}

// prepare identifies the caller, loads the order to learn its entity, then runs Guard
// (tenant+entity reconciliation before authz) for the action.
func (m *Module) prepare(w http.ResponseWriter, r *http.Request, b *cmdBase, action string) (kit.Caller, *Header, bool) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return kit.Caller{}, nil, false
	}
	if err := mustUUID("order_id", b.OrderID); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if b.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return kit.Caller{}, nil, false
	}
	var h *Header
	if err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		h, err = getHeader(r.Context(), tx, c.TenantID, b.OrderID, false)
		return err
	}); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if b.LegalEntityID != "" && b.LegalEntityID != h.LegalEntityID {
		m.fail(w, kit.Forbidden(kit.CodeTenantScope, "legal_entity_id does not match the order's legal entity"))
		return kit.Caller{}, nil, false
	}
	c, ok = m.deps.Guard(w, r, h.LegalEntityID, action)
	if !ok {
		return kit.Caller{}, nil, false
	}
	return c, h, true
}

func evPayload(c kit.Caller, h *Header, extra map[string]any) map[string]any {
	p := map[string]any{"order_id": h.OrderID, "order_number": h.OrderNumber, "legal_entity_id": h.LegalEntityID,
		"customer_party_id": h.CustomerPartyID, "currency": h.Currency, "state": h.State, "version": h.Version,
		"version_no": h.CurrentVersionNo, "net_total": h.NetTotal.String(), "hold_active": h.HoldActive, "correlation_id": c.CorrelationID}
	if h.SourceQuoteRef != "" {
		p["source_quote_ref"] = h.SourceQuoteRef
	}
	for k, x := range extra {
		p[k] = x
	}
	return p
}

// stepSpec describes one state-changing command on an existing order.
type stepSpec struct {
	action  string
	command string
	// tr is the lifecycle move (empty = none: the guard alone decides legality).
	tr Transition
	// guard is extra legality beyond the state machine (e.g. a hold overlay).
	guard func(h *Header) error
	// pre runs outside the transaction with the current version loaded: profile, authority,
	// credit and segregation checks live here.
	pre func(ctx context.Context, c kit.Caller, h *Header, v *Version) error
	// apply mutates nh (a copy of the locked header, already moved by tr) and returns the evidence.
	apply func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, lines []Line, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error)
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
func (m *Module) failStep(w http.ResponseWriter, ctx context.Context, c kit.Caller, orderID string, err error) {
	var d *denial
	if errors.As(err, &d) {
		m.recordDenial(ctx, c, orderID, d)
	}
	m.fail(w, err)
}

func (m *Module) runStep(w http.ResponseWriter, r *http.Request, req *cmdBase, sp stepSpec) {
	c, h, ok := m.prepare(w, r, req, sp.action)
	if !ok {
		return
	}
	ctx := r.Context()
	if h.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	if _, err := sp.legal(h); err != nil {
		m.fail(w, err)
		return
	}
	if sp.pre != nil {
		var v *Version
		if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) {
			v, err = getVersion(ctx, tx, c.TenantID, h.OrderID, h.CurrentVersionNo)
			return err
		}); err != nil {
			m.fail(w, err)
			return
		}
		if err := sp.pre(ctx, c, h, v); err != nil {
			m.failStep(w, ctx, c, h.OrderID, err)
			return
		}
	}
	now := m.today()
	var out *Order
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, c.TenantID, req.OrderID, true)
		if err != nil {
			return err
		}
		if cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		next, err := sp.legal(cur)
		if err != nil {
			return err
		}
		v, err := getVersion(ctx, tx, c.TenantID, cur.OrderID, cur.CurrentVersionNo)
		if err != nil {
			return err
		}
		lines, err := getLines(ctx, tx, c.TenantID, cur.OrderID, false)
		if err != nil {
			return err
		}
		nh := *cur
		nh.State = next
		hs, events, err := sp.apply(ctx, tx, c, cur, v, lines, &nh, now)
		if err != nil {
			return err
		}
		hs.At = now
		if hs.ContentHash == "" {
			hs.ContentHash = v.Hash
		}
		saved, err := commit(ctx, tx, c, cur, &nh, hs, events)
		if err != nil {
			return err
		}
		out, err = loadOrder(ctx, tx, c.TenantID, saved, 0)
		return err
	})
	if err != nil {
		m.failStep(w, ctx, c, req.OrderID, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// ── CreateSalesOrder ─────────────────────────────────────────────────────────

type createReq struct {
	LegalEntityID     string      `json:"legal_entity_id"`
	CustomerPartyID   string      `json:"customer_party_id"`
	Currency          string      `json:"currency"`
	CustomerPORef     string      `json:"customer_po_ref"`
	Terms             string      `json:"terms"`
	PriceSourceRef    string      `json:"price_source_ref"`
	CatalogVersionRef string      `json:"catalog_version_ref"`
	DiscountPercent   kit.Amount  `json:"discount_percent"`
	Lines             []LineInput `json:"lines"`
	ReasonCode        string      `json:"reason_code"`
	EvidenceID        string      `json:"evidence_id"`
}

type createInput struct {
	Content
	Totals
	CustomerPartyID string
	SourceQuoteRef  string
	SourceRef       string
	Command         string
	Reason          string
	Evidence        string
}

// createOrderTx inserts a DRAFT order, its first version and lines, history, audit and the
// SalesOrderCreated event in the caller's transaction.
func createOrderTx(ctx context.Context, tx pgx.Tx, c kit.Caller, in createInput, now time.Time) (*Header, error) {
	num, err := nextNumber(ctx, tx, c.TenantID, c.LegalEntityID)
	if err != nil {
		return nil, err
	}
	hash := ContentHash(in.Content)
	h := &Header{OrderID: uuid.NewString(), LegalEntityID: c.LegalEntityID, OrderNumber: num, CustomerPartyID: in.CustomerPartyID,
		Currency: in.Currency, State: StateDraft, CustomerPORef: in.CustomerPORef, SourceQuoteRef: in.SourceQuoteRef, SourceRef: in.SourceRef,
		NetTotal: in.NetTotal, DiscountPercent: in.DiscountPercent, CurrentVersionNo: 1, Version: 1, CreatedBy: c.PrincipalID}
	if err := insertHeader(ctx, tx, c, h, now); err != nil {
		return nil, err
	}
	if err := insertVersion(ctx, tx, c, h, 1, "CREATE", in.Reason, in.Evidence, in.Content, in.Totals, hash, now); err != nil {
		return nil, err
	}
	if err := insertLines(ctx, tx, c, h.OrderID, in.Lines); err != nil {
		return nil, err
	}
	newVals := map[string]any{"state": StateDraft, "currency": in.Currency, "net_total": in.NetTotal.String(), "lines": len(in.Lines),
		"discount_percent": in.DiscountPercent.String(), "price_source_ref": in.PriceSourceRef, "customer_po_ref": in.CustomerPORef}
	if in.SourceQuoteRef != "" {
		newVals["source_quote_ref"] = in.SourceQuoteRef
	}
	if err := commitCreate(ctx, tx, c, h, hist{At: now, Command: in.Command, Reason: in.Reason, Evidence: in.Evidence, ContentHash: hash, New: newVals},
		func(h *Header) []kit.Event {
			return []kit.Event{{AggregateType: AggregateType, AggregateID: h.OrderID, EventType: EventSalesOrderCreated, Payload: evPayload(c, h, nil)}}
		}); err != nil {
		return nil, err
	}
	return getHeader(ctx, tx, c.TenantID, h.OrderID, false)
}

func (m *Module) createSalesOrder(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if err := mustUUID("customer_party_id", req.CustomerPartyID); err != nil {
		m.fail(w, err)
		return
	}
	lines, err := BuildLines(req.Currency, req.Lines)
	if err != nil {
		m.fail(w, err)
		return
	}
	totals, err := ApplyDiscountPercent(req.Currency, lines, req.DiscountPercent)
	if err != nil {
		m.fail(w, err)
		return
	}
	content := Content{Currency: req.Currency, CustomerPORef: req.CustomerPORef, Terms: req.Terms, PriceSourceRef: req.PriceSourceRef,
		CatalogVersionRef: req.CatalogVersionRef, DiscountPercent: req.DiscountPercent, Lines: lines}
	if err := content.Validate(); err != nil {
		m.fail(w, err)
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionOrderManage)
	if !ok {
		return
	}
	ctx := r.Context()
	// A credit hold does not block drafting; an inactive profile does.
	if _, err := m.profile(ctx, c.TenantID, c.LegalEntityID, req.CustomerPartyID); err != nil {
		m.fail(w, err)
		return
	}
	now := m.today()
	var out *Order
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		h, err := createOrderTx(ctx, tx, c, createInput{Content: content, Totals: totals, CustomerPartyID: req.CustomerPartyID,
			Command: "createSalesOrder", Reason: req.ReasonCode, Evidence: req.EvidenceID}, now)
		if err != nil {
			return err
		}
		out, err = loadOrder(ctx, tx, c.TenantID, h, 0)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, out)
}

// ── AmendSalesOrder ──────────────────────────────────────────────────────────

type amendReq struct {
	cmdBase
	CustomerPORef     *string      `json:"customer_po_ref,omitempty"`
	Terms             *string      `json:"terms,omitempty"`
	PriceSourceRef    *string      `json:"price_source_ref,omitempty"`
	CatalogVersionRef *string      `json:"catalog_version_ref,omitempty"`
	DiscountPercent   *kit.Amount  `json:"discount_percent,omitempty"`
	Lines             *[]LineInput `json:"lines,omitempty"`
}

func (a amendReq) hasFields() bool {
	return a.CustomerPORef != nil || a.Terms != nil || a.PriceSourceRef != nil || a.CatalogVersionRef != nil || a.DiscountPercent != nil || a.Lines != nil
}

func contentLinesToInputs(ls []ContentLine) []LineInput {
	out := make([]LineInput, len(ls))
	for i, l := range ls {
		n := l.LineNo
		out[i] = LineInput{LineNo: &n, Description: l.Description, ProductRef: l.ProductRef, Quantity: l.Quantity, UnitPrice: l.UnitPrice}
	}
	return out
}

// contentDiff records only what changed, for history/audit prior and new values.
func contentDiff(old, nw Content) (prior, next map[string]any) {
	prior, next = map[string]any{}, map[string]any{}
	add := func(k string, a, b any, changed bool) {
		if changed {
			prior[k], next[k] = a, b
		}
	}
	add("customer_po_ref", old.CustomerPORef, nw.CustomerPORef, old.CustomerPORef != nw.CustomerPORef)
	add("terms", old.Terms, nw.Terms, old.Terms != nw.Terms)
	add("price_source_ref", old.PriceSourceRef, nw.PriceSourceRef, old.PriceSourceRef != nw.PriceSourceRef)
	add("catalog_version_ref", old.CatalogVersionRef, nw.CatalogVersionRef, old.CatalogVersionRef != nw.CatalogVersionRef)
	add("discount_percent", old.DiscountPercent.String(), nw.DiscountPercent.String(), old.DiscountPercent != nw.DiscountPercent)
	linesChanged := len(old.Lines) != len(nw.Lines)
	for i := 0; !linesChanged && i < len(old.Lines); i++ {
		linesChanged = old.Lines[i] != nw.Lines[i]
	}
	add("lines", old.Lines, nw.Lines, linesChanged)
	return prior, next
}

// OpenMilestoneHold is what recorded-but-unbilled milestones already claim on a line.
type OpenMilestoneHold struct{ Qty, Amount kit.Amount }

// CheckLiveAmendment validates a post-confirmation amendment: same line numbers, no line
// increases in quantity or net, nothing below what is already invoiced (DOCUMENT_IMMUTABLE)
// and nothing below what recorded milestones still claim (ORDER_AMENDMENT_NOT_ALLOWED).
func CheckLiveAmendment(cur []Line, open map[int]OpenMilestoneHold, next []ContentLine) error {
	if len(cur) != len(next) {
		return kit.Conflict(CodeAmendmentNotAllowed, "a confirmed order keeps its lines: lines cannot be added or removed after confirmation")
	}
	byNo := map[int]Line{}
	for _, l := range cur {
		byNo[l.LineNo] = l
	}
	for _, n := range next {
		old, ok := byNo[n.LineNo]
		if !ok {
			return kit.Conflict(CodeAmendmentNotAllowed, "line "+strconv.Itoa(n.LineNo)+" does not exist on the order")
		}
		if n.Quantity < old.InvoicedQty || n.LineNet < old.InvoicedAmount {
			return kit.Conflict(kit.CodeImmutable, "line "+strconv.Itoa(n.LineNo)+" is billed: it cannot be reduced below the invoiced quantity or amount")
		}
		if n.Quantity > old.Quantity || n.LineNet > old.LineNet {
			return kit.Conflict(CodeAmendmentNotAllowed, "line "+strconv.Itoa(n.LineNo)+": a confirmed order can only be reduced; raise a new order for an increase")
		}
		if h := open[n.LineNo]; n.Quantity < old.InvoicedQty.Add(h.Qty) || n.LineNet < old.InvoicedAmount.Add(h.Amount) {
			return kit.Conflict(CodeAmendmentNotAllowed, "line "+strconv.Itoa(n.LineNo)+" cannot go below what recorded billable milestones claim")
		}
	}
	return nil
}

func (m *Module) amendSalesOrder(w http.ResponseWriter, r *http.Request) {
	var req amendReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if !req.hasFields() {
		m.fail(w, kit.Invalid("nothing to amend: supply at least one content field"))
		return
	}
	c, h, ok := m.prepare(w, r, &req.cmdBase, ActionOrderManage)
	if !ok {
		return
	}
	ctx := r.Context()
	if h.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	switch {
	case h.State.CanAmendDraft():
		if _, err := m.profile(ctx, c.TenantID, h.LegalEntityID, h.CustomerPartyID); err != nil {
			m.fail(w, err)
			return
		}
	case h.State.CanAmendLive():
		// A governed amendment of a confirmed order always carries its justification.
		if strings.TrimSpace(req.ReasonCode) == "" {
			m.fail(w, reasonRequired())
			return
		}
		if strings.TrimSpace(req.EvidenceID) == "" {
			m.fail(w, evidenceRequired())
			return
		}
	default:
		m.fail(w, kit.Conflict(kit.CodeInvalidTransition, "an order in state "+string(h.State)+" cannot be amended"))
		return
	}
	now := m.today()
	var out *Order
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, c.TenantID, req.OrderID, true)
		if err != nil {
			return err
		}
		if cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		draft, live := cur.State.CanAmendDraft(), cur.State.CanAmendLive()
		if !draft && !live {
			return kit.Conflict(kit.CodeInvalidTransition, "an order in state "+string(cur.State)+" cannot be amended")
		}
		old, err := getVersion(ctx, tx, c.TenantID, cur.OrderID, cur.CurrentVersionNo)
		if err != nil {
			return err
		}
		curLines, err := getLines(ctx, tx, c.TenantID, cur.OrderID, true)
		if err != nil {
			return err
		}
		next := old.Content
		if req.CustomerPORef != nil {
			next.CustomerPORef = *req.CustomerPORef
		}
		if req.Terms != nil {
			next.Terms = *req.Terms
		}
		if req.PriceSourceRef != nil {
			next.PriceSourceRef = *req.PriceSourceRef
		}
		if req.CatalogVersionRef != nil {
			next.CatalogVersionRef = *req.CatalogVersionRef
		}
		if req.DiscountPercent != nil {
			next.DiscountPercent = *req.DiscountPercent
		}
		inputs := contentLinesToInputs(old.Lines)
		if req.Lines != nil {
			inputs = *req.Lines
		}
		if next.Lines, err = BuildLines(next.Currency, inputs); err != nil {
			return err
		}
		if live {
			// Lines are matched by number: every input names an existing line, each once.
			seen := map[int]bool{}
			for i, in := range inputs {
				if in.LineNo == nil || seen[*in.LineNo] {
					return kit.Invalid("a confirmed order is amended line by line: every line needs its own line_no")
				}
				seen[*in.LineNo] = true
				next.Lines[i].LineNo = *in.LineNo
			}
			sortByLineNo(next.Lines)
		}
		totals, err := ApplyDiscountPercent(next.Currency, next.Lines, next.DiscountPercent)
		if err != nil {
			return err
		}
		if err := next.Validate(); err != nil {
			return err
		}
		hash := ContentHash(next)
		if hash == old.Hash {
			return kit.Invalid("the amendment changes nothing")
		}
		if live {
			open := map[int]OpenMilestoneHold{}
			ms, err := getMilestones(ctx, tx, c.TenantID, cur.OrderID)
			if err != nil {
				return err
			}
			for _, x := range ms {
				if x.State == MsRecorded {
					o := open[x.LineNo]
					open[x.LineNo] = OpenMilestoneHold{Qty: o.Qty.Add(x.ScheduledQty), Amount: o.Amount.Add(x.ScheduledAmount)}
				}
			}
			if err := CheckLiveAmendment(curLines, open, next.Lines); err != nil {
				return err
			}
		}
		vno := cur.CurrentVersionNo + 1
		if err := insertVersion(ctx, tx, c, cur, vno, "AMEND", req.ReasonCode, req.EvidenceID, next, totals, hash, now); err != nil {
			return err
		}
		nh := *cur
		nh.CurrentVersionNo, nh.NetTotal, nh.CustomerPORef, nh.DiscountPercent = vno, totals.NetTotal, next.CustomerPORef, next.DiscountPercent
		prior, nw := contentDiff(old.Content, next)
		prior["state"], nw["state"] = cur.State, cur.State
		prior["version_no"], nw["version_no"] = old.VersionNo, vno
		prior["net_total"], nw["net_total"] = old.NetTotal.String(), totals.NetTotal.String()
		if live {
			if err := updateLines(ctx, tx, c, cur.OrderID, next.Lines); err != nil {
				return err
			}
		} else {
			if err := replaceLines(ctx, tx, c, cur.OrderID, next.Lines); err != nil {
				return err
			}
			// Any content change voids the prior approval: it was bound to the old content hash.
			nh.State = StateDraft
			nw["state"] = StateDraft
			if cur.ApprovedHash != "" || cur.CreditExceptionID != "" {
				nw["approval_invalidated"], prior["approved_content_hash"] = true, cur.ApprovedHash
				if cur.CreditExceptionID != "" {
					prior["credit_exception_id"] = cur.CreditExceptionID
				}
			}
			nh.SubmittedBy, nh.ApprovedVersionNo, nh.ApprovedHash, nh.ApprovedBy, nh.ApprovedAt, nh.CreditExceptionID = "", 0, "", "", nil, ""
		}
		saved, err := commit(ctx, tx, c, cur, &nh, hist{At: now, Command: "amendSalesOrder", Reason: req.ReasonCode, Evidence: req.EvidenceID,
			ContentHash: hash, Prior: prior, New: nw}, nil)
		if err != nil {
			return err
		}
		out, err = loadOrder(ctx, tx, c.TenantID, saved, 0)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func sortByLineNo(ls []ContentLine) {
	for i := 1; i < len(ls); i++ {
		for j := i; j > 0 && ls[j].LineNo < ls[j-1].LineNo; j-- {
			ls[j], ls[j-1] = ls[j-1], ls[j]
		}
	}
}

// ── SubmitOrder ──────────────────────────────────────────────────────────────

func requirePriceSource(v *Version) error {
	if strings.TrimSpace(v.PriceSourceRef) == "" {
		return kit.Unprocessable(kit.CodeValidationFailed, "price_source_ref is required before an order can be approved: pricing is never invented")
	}
	return nil
}

func (m *Module) submitOrder(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	m.runStep(w, r, &req, stepSpec{action: ActionOrderManage, command: "submitOrder", tr: TrSubmit,
		pre: func(ctx context.Context, c kit.Caller, h *Header, v *Version) error {
			if err := requirePriceSource(v); err != nil {
				return err
			}
			_, err := m.profile(ctx, c.TenantID, h.LegalEntityID, h.CustomerPartyID)
			return err
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, lines []Line, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if err := requirePriceSource(v); err != nil {
				return hist{}, nil, err
			}
			nh.SubmittedBy = c.PrincipalID
			return hist{Command: "submitOrder", Reason: req.ReasonCode, Evidence: req.EvidenceID,
				Prior: map[string]any{"state": cur.State}, New: map[string]any{"state": nh.State, "submitted_by": c.PrincipalID}}, nil, nil
		}})
}

// ── ApproveOrder ─────────────────────────────────────────────────────────────

type creditExceptionReq struct {
	ReasonCode string `json:"reason_code"`
	EvidenceID string `json:"evidence_id"`
}

type approveReq struct {
	cmdBase
	VersionNo       *int                `json:"version_no"`
	ContentHash     string              `json:"content_hash,omitempty"`
	CreditException *creditExceptionReq `json:"credit_exception,omitempty"`
}

// validateCreditException checks a credit exception being recorded by the caller: reason and
// evidence are mandatory, the caller must hold the credit-exception authority and must be a
// different principal from the order's creator and submitter.
func (m *Module) validateCreditException(ctx context.Context, c kit.Caller, h *Header, e *creditExceptionReq, command string) error {
	if strings.TrimSpace(e.ReasonCode) == "" {
		return reasonRequired()
	}
	if strings.TrimSpace(e.EvidenceID) == "" {
		return evidenceRequired()
	}
	if len(e.ReasonCode) > 128 || len(e.EvidenceID) > 255 {
		return kit.Invalid("credit exception reason_code (128) and evidence_id (255) are too long")
	}
	if err := m.checkAction(ctx, c, ActionCreditExceptionApprove); err != nil {
		return err
	}
	if c.PrincipalID == h.CreatedBy || c.PrincipalID == h.SubmittedBy {
		return deny(errSoD("a credit exception must be approved by a principal who neither created nor submitted the order"), command,
			map[string]any{"net_total": h.NetTotal.String(), "version_no": h.CurrentVersionNo})
	}
	return nil
}

func (m *Module) approveOrder(w http.ResponseWriter, r *http.Request) {
	var req approveReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if req.VersionNo == nil {
		m.fail(w, kit.Invalid("version_no is required: an approval is bound to one version"))
		return
	}
	var sod bool
	var credit CreditResult
	m.runStep(w, r, &req.cmdBase, stepSpec{action: ActionOrderApprove, command: "approveOrder", tr: TrApprove,
		pre: func(ctx context.Context, c kit.Caller, h *Header, v *Version) error {
			if *req.VersionNo != h.CurrentVersionNo {
				return errApprovalInvalidated("approval was requested for version " + strconv.Itoa(*req.VersionNo) + " but the current version is " +
					strconv.Itoa(h.CurrentVersionNo) + ": an approval cannot be reused after the order changed")
			}
			if req.ContentHash != "" && req.ContentHash != v.Hash {
				return errApprovalInvalidated("content_hash does not match the current version content")
			}
			if err := requirePriceSource(v); err != nil {
				return err
			}
			view, err := m.profile(ctx, c.TenantID, h.LegalEntityID, h.CustomerPartyID)
			if err != nil {
				return err
			}
			sod = SoDRequired(h.NetTotal, h.Currency, view, req.CreditException != nil)
			if req.CreditException != nil {
				if err := m.validateCreditException(ctx, c, h, req.CreditException, "approveOrder"); err != nil {
					return err
				}
				if credit, err = m.creditCheck(ctx, h, view); err != nil {
					return err
				}
			}
			if sod && (c.PrincipalID == h.CreatedBy || c.PrincipalID == h.SubmittedBy) {
				return deny(errSoD("an order above the commercial or credit threshold must be approved by a principal who neither created nor submitted it"),
					"approveOrder", map[string]any{"net_total": h.NetTotal.String(), "version_no": h.CurrentVersionNo, "content_hash": v.Hash})
			}
			return nil
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, lines []Line, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if *req.VersionNo != cur.CurrentVersionNo {
				return hist{}, nil, errApprovalInvalidated("the order changed while the approval was in flight")
			}
			if err := insertApproval(ctx, tx, c, cur, v, sod, now); err != nil {
				return hist{}, nil, err
			}
			t := now
			nh.ApprovedVersionNo, nh.ApprovedHash, nh.ApprovedBy, nh.ApprovedAt = v.VersionNo, v.Hash, c.PrincipalID, &t
			newVals := map[string]any{"state": nh.State, "approved_version_no": v.VersionNo, "approved_content_hash": v.Hash,
				"net_total": v.NetTotal.String(), "sod_required": sod}
			if req.CreditException != nil {
				id, err := insertCreditException(ctx, tx, c, cur, v, CreditExceptionInput{ReasonCode: req.CreditException.ReasonCode,
					EvidenceID: req.CreditException.EvidenceID, Check: credit}, now)
				if err != nil {
					return hist{}, nil, err
				}
				nh.CreditExceptionID = id
				newVals["credit_exception_id"], newVals["credit_exception_reason"] = id, req.CreditException.ReasonCode
				newVals["credit_exception_evidence"], newVals["credit_check"] = req.CreditException.EvidenceID, credit
			}
			return hist{Command: "approveOrder", Reason: req.ReasonCode, Evidence: req.EvidenceID, ApproverID: c.PrincipalID,
				Prior: map[string]any{"state": cur.State}, New: newVals}, nil, nil
		}})
}

// ── ConfirmOrder ─────────────────────────────────────────────────────────────

type confirmReq struct {
	cmdBase
	CreditException *creditExceptionReq `json:"credit_exception,omitempty"`
}

func approvalIntact(h *Header, v *Version) error {
	if h.ApprovedHash == "" || h.ApprovedHash != v.Hash || h.ApprovedVersionNo == 0 {
		return errApprovalInvalidated("the current order content is not the content that was approved: re-approval is required")
	}
	return nil
}

func (m *Module) confirmOrder(w http.ResponseWriter, r *http.Request) {
	var req confirmReq
	if !kit.Decode(w, r, &req) {
		return
	}
	var credit CreditResult
	m.runStep(w, r, &req.cmdBase, stepSpec{action: ActionOrderConfirm, command: "confirmOrder", tr: TrConfirm,
		pre: func(ctx context.Context, c kit.Caller, h *Header, v *Version) error {
			if err := approvalIntact(h, v); err != nil {
				return err
			}
			if h.HoldActive {
				if h.HoldKind == HoldCredit {
					return deny(kit.Unprocessable(kit.CodeCreditHoldActive, "the order is on credit hold: release the hold before confirming"),
						"confirmOrder", map[string]any{"hold_kind": h.HoldKind})
				}
				return kit.Conflict(kit.CodeInvalidTransition, "the order is on hold: release the hold before confirming")
			}
			view, err := m.profile(ctx, c.TenantID, h.LegalEntityID, h.CustomerPartyID)
			if err != nil {
				return err
			}
			if credit, err = m.creditCheck(ctx, h, view); err != nil {
				return err
			}
			if req.CreditException != nil {
				if err := m.validateCreditException(ctx, c, h, req.CreditException, "confirmOrder"); err != nil {
					return err
				}
			}
			if credit.Blocked() && h.CreditExceptionID == "" && req.CreditException == nil {
				return deny(kit.Unprocessable(kit.CodeCreditHoldActive, credit.Detail(h.Currency)), "confirmOrder",
					map[string]any{"credit_check": credit, "net_total": h.NetTotal.String()})
			}
			return nil
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, lines []Line, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if err := approvalIntact(cur, v); err != nil {
				return hist{}, nil, err
			}
			if cur.HoldActive {
				return hist{}, nil, kit.Conflict(kit.CodeInvalidTransition, "the order is on hold: release the hold before confirming")
			}
			newVals := map[string]any{"state": nh.State, "credit_check": credit}
			if req.CreditException != nil {
				id, err := insertCreditException(ctx, tx, c, cur, v, CreditExceptionInput{ReasonCode: req.CreditException.ReasonCode,
					EvidenceID: req.CreditException.EvidenceID, Check: credit}, now)
				if err != nil {
					return hist{}, nil, err
				}
				nh.CreditExceptionID = id
				newVals["credit_exception_reason"], newVals["credit_exception_evidence"] = req.CreditException.ReasonCode, req.CreditException.EvidenceID
			}
			if nh.CreditExceptionID != "" {
				newVals["credit_exception_id"] = nh.CreditExceptionID
			}
			t := now
			nh.ConfirmedBy, nh.ConfirmedAt = c.PrincipalID, &t
			newVals["confirmed_by"] = c.PrincipalID
			return hist{Command: "confirmOrder", Reason: req.ReasonCode, Evidence: req.EvidenceID,
					Prior: map[string]any{"state": cur.State}, New: newVals},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.OrderID, EventType: EventSalesOrderConfirmed,
						Payload: evPayload(c, h, map[string]any{"confirmed_by": c.PrincipalID, "credit_check": credit, "credit_exception_id": nh.CreditExceptionID})}}
				}, nil
		}})
}

// ── PlaceOrderHold / ReleaseOrderHold ────────────────────────────────────────

type holdReq struct {
	cmdBase
	HoldKind string `json:"hold_kind"`
}

func (m *Module) placeOrderHold(w http.ResponseWriter, r *http.Request) {
	var req holdReq
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
	if !holdKinds[req.HoldKind] {
		m.fail(w, kit.Invalid("hold_kind must be CREDIT, COMMERCIAL, FULFILLMENT or OTHER"))
		return
	}
	m.runStep(w, r, &req.cmdBase, stepSpec{action: ActionOrderHold, command: "placeOrderHold", tr: TrPlaceHold,
		guard: func(h *Header) error {
			if h.HoldActive {
				return kit.Conflict(kit.CodeInvalidTransition, "the order is already on hold")
			}
			return nil
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, lines []Line, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			t := now
			nh.HoldActive, nh.HoldKind, nh.HoldReason, nh.HoldEvidenceID, nh.HeldBy, nh.HeldAt = true, req.HoldKind, req.ReasonCode, req.EvidenceID, c.PrincipalID, &t
			return hist{Command: "placeOrderHold", Reason: req.ReasonCode, Evidence: req.EvidenceID,
					Prior: map[string]any{"hold_active": false}, New: map[string]any{"hold_active": true, "hold_kind": req.HoldKind, "state": nh.State}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.OrderID, EventType: EventSalesOrderHeld,
						Payload: evPayload(c, h, map[string]any{"hold_kind": req.HoldKind, "reason_code": req.ReasonCode, "evidence_id": req.EvidenceID})}}
				}, nil
		}})
}

func (m *Module) releaseOrderHold(w http.ResponseWriter, r *http.Request) {
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
	m.runStep(w, r, &req, stepSpec{action: ActionOrderHold, command: "releaseOrderHold",
		guard: func(h *Header) error {
			if !h.HoldActive {
				return kit.Conflict(kit.CodeInvalidTransition, "the order is not on hold")
			}
			return nil
		},
		pre: func(ctx context.Context, c kit.Caller, h *Header, v *Version) error {
			if h.HoldKind != HoldCredit {
				return nil
			}
			// A credit hold needs independent release authority: a different principal who holds it.
			if err := m.checkAction(ctx, c, ActionCreditExceptionApprove); err != nil {
				return err
			}
			if c.PrincipalID == h.HeldBy {
				return deny(errSoD("a credit hold must be released by a different principal from the one who placed it"), "releaseOrderHold",
					map[string]any{"hold_kind": h.HoldKind, "held_by": h.HeldBy})
			}
			return nil
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, lines []Line, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if cur.HoldKind == HoldCredit && c.PrincipalID == cur.HeldBy {
				return hist{}, nil, deny(errSoD("a credit hold must be released by a different principal from the one who placed it"), "releaseOrderHold",
					map[string]any{"hold_kind": cur.HoldKind, "held_by": cur.HeldBy})
			}
			prevKind := cur.HoldKind
			nh.HoldActive, nh.HoldKind, nh.HoldReason, nh.HoldEvidenceID, nh.HeldBy, nh.HeldAt = false, "", "", "", "", nil
			return hist{Command: "releaseOrderHold", Reason: req.ReasonCode, Evidence: req.EvidenceID, ApproverID: c.PrincipalID,
					Prior: map[string]any{"hold_active": true, "hold_kind": prevKind, "held_by": cur.HeldBy}, New: map[string]any{"hold_active": false, "state": nh.State}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.OrderID, EventType: EventSalesOrderReleased,
						Payload: evPayload(c, h, map[string]any{"hold_kind": prevKind, "released_by": c.PrincipalID, "reason_code": req.ReasonCode, "evidence_id": req.EvidenceID})}}
				}, nil
		}})
}

// ── CancelOrder / CloseOrder ─────────────────────────────────────────────────

func clearHold(nh *Header) {
	nh.HoldActive, nh.HoldKind, nh.HoldReason, nh.HoldEvidenceID, nh.HeldBy, nh.HeldAt = false, "", "", "", "", nil
}

func (m *Module) cancelOrder(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	m.runStep(w, r, &req, stepSpec{action: ActionOrderCancel, command: "cancelOrder", tr: TrCancel,
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, lines []Line, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			voided, err := voidOpenMilestones(ctx, tx, c.TenantID, cur.OrderID, "ORDER_CANCELLED", now)
			if err != nil {
				return hist{}, nil, err
			}
			t := now
			nh.CancelReason, nh.CancelledBy, nh.CancelledAt = req.ReasonCode, c.PrincipalID, &t
			clearHold(nh)
			sum := Summarize(lines)
			return hist{Command: "cancelOrder", Reason: req.ReasonCode, Evidence: req.EvidenceID,
					Prior: map[string]any{"state": cur.State, "hold_active": cur.HoldActive},
					New:   map[string]any{"state": nh.State, "voided_milestones": voided, "invoiced_net": sum.InvoicedNet.String(), "uninvoiced_net": sum.RemainingNet.String()}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.OrderID, EventType: EventSalesOrderCancelled,
						Payload: evPayload(c, h, map[string]any{"reason_code": req.ReasonCode, "evidence_id": req.EvidenceID, "prior_state": cur.State,
							"invoiced_net": sum.InvoicedNet.String(), "cancelled_net": sum.RemainingNet.String(), "voided_milestones": voided})}}
				}, nil
		}})
}

func (m *Module) closeOrder(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	m.runStep(w, r, &req, stepSpec{action: ActionOrderManage, command: "closeOrder", tr: TrClose,
		pre: func(ctx context.Context, c kit.Caller, h *Header, v *Version) error {
			// Closing before everything is billed is a short-close: it forfeits the unbilled remainder.
			if h.State != StateFullyBilled && strings.TrimSpace(req.ReasonCode) == "" {
				return reasonRequired()
			}
			return nil
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, lines []Line, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if cur.State != StateFullyBilled && strings.TrimSpace(req.ReasonCode) == "" {
				return hist{}, nil, reasonRequired()
			}
			voided, err := voidOpenMilestones(ctx, tx, c.TenantID, cur.OrderID, "ORDER_CLOSED", now)
			if err != nil {
				return hist{}, nil, err
			}
			t := now
			nh.CloseReason, nh.ClosedBy, nh.ClosedAt = req.ReasonCode, c.PrincipalID, &t
			clearHold(nh)
			sum := Summarize(lines)
			return hist{Command: "closeOrder", Reason: req.ReasonCode, Evidence: req.EvidenceID,
				Prior: map[string]any{"state": cur.State},
				New:   map[string]any{"state": nh.State, "short_close": cur.State != StateFullyBilled, "voided_milestones": voided, "forfeited_net": sum.RemainingNet.String()}}, nil, nil
		}})
}

// ── RecordBillableMilestone ──────────────────────────────────────────────────

type milestoneReq struct {
	cmdBase
	ExternalRef            string     `json:"external_ref"`
	LineNo                 int        `json:"line_no"`
	Description            string     `json:"description,omitempty"`
	ScheduledQty           kit.Amount `json:"scheduled_qty"`
	ScheduledAmount        kit.Amount `json:"scheduled_amount"`
	FulfillmentSource      string     `json:"fulfillment_source"`
	FulfillmentEvidenceRef string     `json:"fulfillment_evidence_ref"`
}

func (q milestoneReq) validate() error {
	switch {
	case !refRe.MatchString(q.ExternalRef):
		return kit.Invalid("external_ref is required (1-255 characters of letters, digits and _ . : / # -)")
	case q.LineNo < 1:
		return kit.Invalid("line_no is required")
	case utf8Len(q.Description) > 500:
		return kit.Invalid("description is limited to 500 characters")
	case q.ScheduledQty.IsNegative() || q.ScheduledQty > kit.MustAmount("1000000000"):
		return kit.Invalid("scheduled_qty must be between 0 and 1000000000")
	case !q.ScheduledAmount.IsPositive() || q.ScheduledAmount > maxLineNet:
		return kit.Invalid("scheduled_amount must be greater than zero")
	case !codeRe.MatchString(q.FulfillmentSource):
		return kit.Invalid("fulfillment_source (the owning operational domain) is required: 1-64 characters of A-Z, 0-9 or '_'")
	case !refRe.MatchString(q.FulfillmentEvidenceRef):
		return kit.Invalid("fulfillment_evidence_ref is required: the owning domain's evidence reference is recorded, never invented")
	}
	return nil
}

func utf8Len(s string) int { return len([]rune(s)) }

func (m *Module) recordBillableMilestone(w http.ResponseWriter, r *http.Request) {
	var req milestoneReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		m.fail(w, err)
		return
	}
	dup := func(existing string) error {
		return deny(kit.Conflict(CodeDuplicateMilestone, "a billable milestone with this external_ref is already recorded for the order"),
			"recordBillableMilestone", map[string]any{"external_ref": req.ExternalRef, "existing_milestone_id": existing})
	}
	m.runStep(w, r, &req.cmdBase, stepSpec{action: ActionOrderManage, command: "recordBillableMilestone",
		guard: func(h *Header) error {
			switch {
			case h.State == StateCancelled:
				return kit.Conflict(kit.CodeInvalidTransition, "the order is cancelled: no milestone can be recorded")
			case !h.State.Billable():
				return kit.Conflict(kit.CodeInvalidTransition, "a billable milestone is recorded only on a confirmed order (state "+string(h.State)+")")
			}
			return nil
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, lines []Line, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if !req.ScheduledAmount.FitsCurrency(cur.Currency) {
				return hist{}, nil, kit.Invalid("scheduled_amount has more precision than the currency allows")
			}
			var line *Line
			for i := range lines {
				if lines[i].LineNo == req.LineNo {
					line = &lines[i]
				}
			}
			if line == nil {
				return hist{}, nil, kit.Invalid("line_no does not exist on this order")
			}
			ms, err := getMilestones(ctx, tx, c.TenantID, cur.OrderID)
			if err != nil {
				return hist{}, nil, err
			}
			var openQty, openAmt kit.Amount
			for _, x := range ms {
				if x.ExternalRef == req.ExternalRef {
					return hist{}, nil, dup(x.MilestoneID)
				}
				if x.State == MsRecorded && x.LineNo == req.LineNo {
					openQty, openAmt = openQty.Add(x.ScheduledQty), openAmt.Add(x.ScheduledAmount)
				}
			}
			if req.ScheduledAmount > line.RemainingAmount.Sub(openAmt) || req.ScheduledQty > line.RemainingQty.Sub(openQty) {
				return hist{}, nil, errNotBillable("the milestone exceeds the remaining eligible quantity or amount of the order line")
			}
			id := uuid.NewString()
			if _, err := tx.Exec(ctx, `INSERT INTO ar03_billing_milestones (milestone_id, tenant_id, legal_entity_id, order_id, external_ref, line_no, description,
					scheduled_qty, scheduled_amount, state, fulfillment_source, fulfillment_evidence_ref, recorded_by, recorded_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9::numeric,'RECORDED',$10,$11,$12,$13)`,
				id, c.TenantID, cur.LegalEntityID, cur.OrderID, req.ExternalRef, req.LineNo, req.Description, req.ScheduledQty.String(),
				req.ScheduledAmount.String(), req.FulfillmentSource, req.FulfillmentEvidenceRef, c.PrincipalID, now.UTC()); err != nil {
				if kit.IsUniqueViolation(err, "uq_ar03_milestone_ref") {
					return hist{}, nil, dup("")
				}
				return hist{}, nil, kit.MapPgErr(err)
			}
			if cur.State == StateConfirmed {
				nh.State, _ = Next(cur.State, TrFulfil)
			}
			return hist{Command: "recordBillableMilestone", Reason: req.ReasonCode, Evidence: req.EvidenceID,
					Prior: map[string]any{"state": cur.State},
					New: map[string]any{"state": nh.State, "milestone_id": id, "external_ref": req.ExternalRef, "line_no": req.LineNo,
						"scheduled_qty": req.ScheduledQty.String(), "scheduled_amount": req.ScheduledAmount.String(),
						"fulfillment_source": req.FulfillmentSource, "fulfillment_evidence_ref": req.FulfillmentEvidenceRef}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.OrderID, EventType: EventBillableMilestoneRecorded,
						Payload: evPayload(c, h, map[string]any{"milestone_id": id, "external_ref": req.ExternalRef, "line_no": req.LineNo,
							"scheduled_qty": req.ScheduledQty.String(), "scheduled_amount": req.ScheduledAmount.String(),
							"fulfillment_source": req.FulfillmentSource, "fulfillment_evidence_ref": req.FulfillmentEvidenceRef})}}
				}, nil
		}})
}
