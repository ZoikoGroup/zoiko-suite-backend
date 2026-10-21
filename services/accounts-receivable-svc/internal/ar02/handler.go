package ar02

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
)

// cmdBase is the envelope shared by every command on an existing quote.
// legal_entity_id is optional: the entity is derived from the stored quote and a
// body entity that disagrees is refused (spec invariant 16).
type cmdBase struct {
	QuoteID         string `json:"quote_id"`
	LegalEntityID   string `json:"legal_entity_id,omitempty"`
	ExpectedVersion *int   `json:"expected_version"`
	ReasonCode      string `json:"reason_code,omitempty"`
	EvidenceID      string `json:"evidence_id,omitempty"`
}

// prepare identifies the caller, loads the quote to learn its entity, then runs
// Guard (tenant+entity reconciliation before authz) for the action.
func (m *Module) prepare(w http.ResponseWriter, r *http.Request, b *cmdBase, action string) (kit.Caller, *Header, bool) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return kit.Caller{}, nil, false
	}
	if err := mustUUID("quote_id", b.QuoteID); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if b.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return kit.Caller{}, nil, false
	}
	var h *Header
	if err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		h, err = getHeader(r.Context(), tx, c.TenantID, b.QuoteID, false)
		return err
	}); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if b.LegalEntityID != "" && b.LegalEntityID != h.LegalEntityID {
		m.fail(w, kit.Forbidden(kit.CodeTenantScope, "legal_entity_id does not match the quote's legal entity"))
		return kit.Caller{}, nil, false
	}
	c, ok = m.deps.Guard(w, r, h.LegalEntityID, action)
	if !ok {
		return kit.Caller{}, nil, false
	}
	return c, h, true
}

// checkAction is Authorize without writing the response (used inside step hooks).
func (m *Module) checkAction(ctx context.Context, c kit.Caller, action string) error {
	err := m.deps.Authz.CheckAllowed(ctx, c.PrincipalID, c.LegalEntityID, action)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrAuthorizationDenied):
		return kit.Forbidden(kit.CodeForbidden, "")
	default:
		m.deps.Log.Error("authorization check failed - failing closed", zap.Error(err))
		return kit.Unavailable("authorization service unavailable")
	}
}

func evPayload(c kit.Caller, h *Header, v *Version, extra map[string]any) map[string]any {
	p := map[string]any{"quote_id": h.QuoteID, "quote_number": h.QuoteNumber, "legal_entity_id": h.LegalEntityID,
		"customer_party_id": h.CustomerPartyID, "currency": h.Currency, "state": h.State, "version": h.Version,
		"version_no": h.CurrentVersionNo, "correlation_id": c.CorrelationID}
	if v != nil {
		p["content_hash"], p["net_total"], p["subtotal"] = v.ContentHash, v.NetTotal.String(), v.Subtotal.String()
		p["discount_percent"], p["validity_date"] = v.DiscountPercent.String(), v.ValidityDate
	}
	for k, x := range extra {
		p[k] = x
	}
	return p
}

// stepSpec describes one state-changing command on an existing quote.
type stepSpec struct {
	action string
	tr     Transition
	// already: a quote already in this state answers with its current linkage and
	// changes nothing (idempotent conversion).
	already State
	// pre runs outside the transaction with the current version loaded; it is where
	// profile, authority, expiry and external estimate checks live.
	pre func(ctx context.Context, c kit.Caller, h *Header, v *Version) error
	// apply mutates nh (a copy of the locked header) and returns the evidence.
	apply func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error)
}

func (m *Module) runStep(w http.ResponseWriter, r *http.Request, req *cmdBase, sp stepSpec) {
	c, h, ok := m.prepare(w, r, req, sp.action)
	if !ok {
		return
	}
	ctx := r.Context()
	if !(sp.already != "" && h.State == sp.already) {
		if h.Version != *req.ExpectedVersion {
			m.fail(w, kit.ErrStaleVersion)
			return
		}
		if _, err := Next(h.State, sp.tr); err != nil {
			m.fail(w, err)
			return
		}
		if sp.pre != nil {
			var v *Version
			if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) {
				v, err = getVersion(ctx, tx, c.TenantID, h.QuoteID, h.CurrentVersionNo)
				return err
			}); err != nil {
				m.fail(w, err)
				return
			}
			if err := sp.pre(ctx, c, h, v); err != nil {
				m.fail(w, err)
				return
			}
		}
	}
	now := m.today()
	var out *Quote
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, c.TenantID, req.QuoteID, true)
		if err != nil {
			return err
		}
		if sp.already != "" && cur.State == sp.already {
			out, err = loadQuote(ctx, tx, c.TenantID, cur, 0)
			return err
		}
		if cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		next, err := Next(cur.State, sp.tr)
		if err != nil {
			return err
		}
		v, err := getVersion(ctx, tx, c.TenantID, cur.QuoteID, cur.CurrentVersionNo)
		if err != nil {
			return err
		}
		nh := *cur
		nh.State = next
		hs, events, err := sp.apply(ctx, tx, c, cur, v, &nh, now)
		if err != nil {
			return err
		}
		hs.At = now
		if hs.ContentHash == "" {
			hs.ContentHash = v.ContentHash
		}
		saved, err := commit(ctx, tx, c, cur, &nh, hs, events)
		if err != nil {
			return err
		}
		out, err = loadQuote(ctx, tx, c.TenantID, saved, 0)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// ── CreateQuote ──────────────────────────────────────────────────────────────

type createReq struct {
	LegalEntityID      string      `json:"legal_entity_id"`
	CustomerPartyID    string      `json:"customer_party_id"`
	Currency           string      `json:"currency"`
	ValidityDate       string      `json:"validity_date"`
	Terms              string      `json:"terms"`
	Assumptions        string      `json:"assumptions"`
	DiscountPercent    kit.Amount  `json:"discount_percent"`
	PriceSourceRef     string      `json:"price_source_ref"`
	CatalogVersionRef  string      `json:"catalog_version_ref"`
	RequestTaxEstimate bool        `json:"request_tax_estimate"`
	TaxParams          *TaxParams  `json:"tax_params"`
	Lines              []LineInput `json:"lines"`
	ReasonCode         string      `json:"reason_code"`
	EvidenceID         string      `json:"evidence_id"`
}

func (m *Module) checkValidityNotPast(validity string) error {
	if validity < day(m.today()) {
		return kit.Invalid("validity_date must not be before today")
	}
	return nil
}

func (m *Module) createQuote(w http.ResponseWriter, r *http.Request) {
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
	content := Content{Currency: req.Currency, ValidityDate: req.ValidityDate, Terms: req.Terms, Assumptions: req.Assumptions,
		DiscountPercent: req.DiscountPercent, PriceSourceRef: req.PriceSourceRef, CatalogVersionRef: req.CatalogVersionRef,
		RequestTaxEstimate: req.RequestTaxEstimate, TaxParams: req.TaxParams, Lines: lines}
	if !content.RequestTaxEstimate {
		content.TaxParams = nil
	}
	if err := content.Validate(); err != nil {
		m.fail(w, err)
		return
	}
	if err := m.checkValidityNotPast(content.ValidityDate); err != nil {
		m.fail(w, err)
		return
	}
	totals, err := ComputeTotals(content.Currency, content.Lines, content.DiscountPercent)
	if err != nil {
		m.fail(w, err)
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionQuoteManage)
	if !ok {
		return
	}
	ctx := r.Context()
	// A credit hold does not block drafting; an inactive profile does.
	if _, err := m.profile(ctx, c.TenantID, &Header{LegalEntityID: c.LegalEntityID, CustomerPartyID: req.CustomerPartyID}); err != nil {
		m.fail(w, err)
		return
	}
	now := m.today()
	hash := ContentHash(content)
	var out *Quote
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		num, err := nextNumber(ctx, tx, c.TenantID, c.LegalEntityID)
		if err != nil {
			return err
		}
		h := &Header{QuoteID: uuid.NewString(), LegalEntityID: c.LegalEntityID, QuoteNumber: num, CustomerPartyID: req.CustomerPartyID,
			Currency: content.Currency, State: StateDraft, CurrentVersionNo: 1, Version: 1, CreatedBy: c.PrincipalID}
		if err := insertHeader(ctx, tx, c, h, now); err != nil {
			return err
		}
		if err := insertVersion(ctx, tx, c, h.QuoteID, 1, "CREATE", false, content, totals, hash, now); err != nil {
			return err
		}
		v := &Version{VersionNo: 1, ContentHash: hash, Content: content, Totals: totals}
		newVals := map[string]any{"state": StateDraft, "currency": content.Currency, "validity_date": content.ValidityDate,
			"net_total": totals.NetTotal.String(), "discount_percent": content.DiscountPercent.String(), "lines": len(content.Lines),
			"price_source_ref": content.PriceSourceRef}
		if err := commitCreate(ctx, tx, c, h, hist{At: now, Command: "createQuote", Reason: req.ReasonCode, Evidence: req.EvidenceID,
			ContentHash: hash, New: newVals}, func(h *Header) []kit.Event {
			return []kit.Event{{AggregateType: AggregateType, AggregateID: h.QuoteID, EventType: EventQuoteCreated, Payload: evPayload(c, h, v, nil)}}
		}); err != nil {
			return err
		}
		saved, err := getHeader(ctx, tx, c.TenantID, h.QuoteID, false)
		if err != nil {
			return err
		}
		out, err = loadQuote(ctx, tx, c.TenantID, saved, 0)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, out)
}

// ── AmendQuote ───────────────────────────────────────────────────────────────

type amendReq struct {
	cmdBase
	Currency           *string      `json:"currency,omitempty"`
	ValidityDate       *string      `json:"validity_date,omitempty"`
	Terms              *string      `json:"terms,omitempty"`
	Assumptions        *string      `json:"assumptions,omitempty"`
	DiscountPercent    *kit.Amount  `json:"discount_percent,omitempty"`
	PriceSourceRef     *string      `json:"price_source_ref,omitempty"`
	CatalogVersionRef  *string      `json:"catalog_version_ref,omitempty"`
	RequestTaxEstimate *bool        `json:"request_tax_estimate,omitempty"`
	TaxParams          *TaxParams   `json:"tax_params,omitempty"`
	Lines              *[]LineInput `json:"lines,omitempty"`
}

func (a amendReq) hasFields() bool {
	return a.Currency != nil || a.ValidityDate != nil || a.Terms != nil || a.Assumptions != nil || a.DiscountPercent != nil ||
		a.PriceSourceRef != nil || a.CatalogVersionRef != nil || a.RequestTaxEstimate != nil || a.TaxParams != nil || a.Lines != nil
}

func linesToInputs(ls []Line) []LineInput {
	out := make([]LineInput, len(ls))
	for i, l := range ls {
		out[i] = LineInput{Description: l.Description, ProductRef: l.ProductRef, Quantity: l.Quantity, UnitPrice: l.UnitPrice}
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
	add("currency", old.Currency, nw.Currency, old.Currency != nw.Currency)
	add("validity_date", old.ValidityDate, nw.ValidityDate, old.ValidityDate != nw.ValidityDate)
	add("terms", old.Terms, nw.Terms, old.Terms != nw.Terms)
	add("assumptions", old.Assumptions, nw.Assumptions, old.Assumptions != nw.Assumptions)
	add("discount_percent", old.DiscountPercent.String(), nw.DiscountPercent.String(), old.DiscountPercent != nw.DiscountPercent)
	add("price_source_ref", old.PriceSourceRef, nw.PriceSourceRef, old.PriceSourceRef != nw.PriceSourceRef)
	add("catalog_version_ref", old.CatalogVersionRef, nw.CatalogVersionRef, old.CatalogVersionRef != nw.CatalogVersionRef)
	add("request_tax_estimate", old.RequestTaxEstimate, nw.RequestTaxEstimate, old.RequestTaxEstimate != nw.RequestTaxEstimate)
	add("tax_params", old.TaxParams, nw.TaxParams, !taxParamsEqual(old.TaxParams, nw.TaxParams))
	if hashOf(Content{Lines: old.Lines, Currency: old.Currency}, false) != hashOf(Content{Lines: nw.Lines, Currency: old.Currency}, false) ||
		old.Currency != nw.Currency {
		prior["lines"], next["lines"] = old.Lines, nw.Lines
	}
	return prior, next
}

func (m *Module) amendQuote(w http.ResponseWriter, r *http.Request) {
	var req amendReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if !req.hasFields() {
		m.fail(w, kit.Invalid("nothing to amend: supply at least one content field"))
		return
	}
	c, h, ok := m.prepare(w, r, &req.cmdBase, ActionQuoteManage)
	if !ok {
		return
	}
	ctx := r.Context()
	if h.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	if !h.State.CanAmend() {
		m.fail(w, kit.Conflict(kit.CodeInvalidTransition, "a quote in state "+string(h.State)+" cannot be amended; raise a new quote"))
		return
	}
	if _, err := m.profile(ctx, c.TenantID, h); err != nil {
		m.fail(w, err)
		return
	}
	now := m.today()
	var out *Quote
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getHeader(ctx, tx, c.TenantID, req.QuoteID, true)
		if err != nil {
			return err
		}
		if cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		if !cur.State.CanAmend() {
			return kit.Conflict(kit.CodeInvalidTransition, "a quote in state "+string(cur.State)+" cannot be amended; raise a new quote")
		}
		old, err := getVersion(ctx, tx, c.TenantID, cur.QuoteID, cur.CurrentVersionNo)
		if err != nil {
			return err
		}
		next := old.Content
		if req.Currency != nil {
			next.Currency = *req.Currency
		}
		if req.ValidityDate != nil {
			next.ValidityDate = *req.ValidityDate
		}
		if req.Terms != nil {
			next.Terms = *req.Terms
		}
		if req.Assumptions != nil {
			next.Assumptions = *req.Assumptions
		}
		if req.DiscountPercent != nil {
			next.DiscountPercent = *req.DiscountPercent
		}
		if req.PriceSourceRef != nil {
			next.PriceSourceRef = *req.PriceSourceRef
		}
		if req.CatalogVersionRef != nil {
			next.CatalogVersionRef = *req.CatalogVersionRef
		}
		if req.RequestTaxEstimate != nil {
			next.RequestTaxEstimate = *req.RequestTaxEstimate
		}
		if req.TaxParams != nil {
			next.TaxParams = req.TaxParams
		}
		if !next.RequestTaxEstimate {
			next.TaxParams = nil
		}
		inputs := linesToInputs(old.Lines)
		if req.Lines != nil {
			inputs = *req.Lines
		}
		if next.Lines, err = BuildLines(next.Currency, inputs); err != nil {
			return err
		}
		if err := next.Validate(); err != nil {
			return err
		}
		if SameContent(old.Content, next) {
			return kit.Invalid("the amendment changes nothing")
		}
		if next.ValidityDate < day(now) {
			if next.ValidityDate != old.ValidityDate {
				return kit.Invalid("validity_date must not be before today")
			}
			return errExpired(next.ValidityDate)
		}
		totals, err := ComputeTotals(next.Currency, next.Lines, next.DiscountPercent)
		if err != nil {
			return err
		}
		material := Material(old.Content, next)
		hash := ContentHash(next)
		vno := cur.CurrentVersionNo + 1
		if err := insertVersion(ctx, tx, c, cur.QuoteID, vno, "AMEND", material, next, totals, hash, now); err != nil {
			return err
		}
		nh := *cur
		nh.CurrentVersionNo, nh.Currency = vno, next.Currency
		nh.State = AfterAmend(cur.State, material)
		carried := false
		switch {
		case material:
			// The prior approval can never be reused: it was bound to the old content hash.
			nh.ApprovedVersionNo, nh.ApprovedContentHash, nh.ApprovedBy, nh.ApprovedAt = 0, "", "", nil
			nh.SubmittedBy, nh.SentAt, nh.SentVersionNo = "", nil, 0
		case cur.ApprovedContentHash != "":
			nh.ApprovedVersionNo, nh.ApprovedContentHash = vno, hash
			carried = true
		}
		if !material && old.TaxEstimate != nil {
			// Same price base: the estimate that backed the approval still describes this version.
			est := *old.TaxEstimate
			if err := insertTaxEstimate(ctx, tx, c, cur, vno, &est); err != nil {
				return err
			}
		}
		prior, nw := contentDiff(old.Content, next)
		prior["state"], nw["state"] = cur.State, nh.State
		nw["version_no"], nw["material_change"], nw["net_total"] = vno, material, totals.NetTotal.String()
		prior["version_no"], prior["net_total"] = old.VersionNo, old.NetTotal.String()
		if material && cur.ApprovedContentHash != "" {
			nw["approval_invalidated"], prior["approved_content_hash"] = true, cur.ApprovedContentHash
		}
		if carried {
			nw["approval_carried_forward"] = true
		}
		saved, err := commit(ctx, tx, c, cur, &nh, hist{At: now, Command: "amendQuote", Reason: req.ReasonCode, Evidence: req.EvidenceID,
			ContentHash: hash, Prior: prior, New: nw}, nil)
		if err != nil {
			return err
		}
		out, err = loadQuote(ctx, tx, c.TenantID, saved, 0)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// ── SubmitQuoteForApproval ───────────────────────────────────────────────────

func (m *Module) preProfile(ctx context.Context, c kit.Caller, h *Header, v *Version) error {
	_, err := m.profile(ctx, c.TenantID, h)
	return err
}

func (m *Module) requireLive(v *Version) error {
	if Expired(v.ValidityDate, m.today()) {
		return errExpired(v.ValidityDate)
	}
	return nil
}

func requirePriceSource(v *Version) error {
	if strings.TrimSpace(v.PriceSourceRef) == "" {
		return kit.Unprocessable(kit.CodeValidationFailed, "price_source_ref is required before a quote can be approved: pricing is never invented")
	}
	return nil
}

func (m *Module) submitQuoteForApproval(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	m.runStep(w, r, &req, stepSpec{action: ActionQuoteManage, tr: TrSubmit,
		pre: func(ctx context.Context, c kit.Caller, h *Header, v *Version) error {
			for _, err := range []error{m.requireLive(v), requirePriceSource(v)} {
				if err != nil {
					return err
				}
			}
			return m.preProfile(ctx, c, h, v)
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if err := m.requireLive(v); err != nil {
				return hist{}, nil, err
			}
			if err := requirePriceSource(v); err != nil {
				return hist{}, nil, err
			}
			nh.SubmittedBy = c.PrincipalID
			return hist{Command: "submitQuoteForApproval", Reason: req.ReasonCode, Evidence: req.EvidenceID,
				Prior: map[string]any{"state": cur.State}, New: map[string]any{"state": nh.State, "submitted_by": c.PrincipalID}}, nil, nil
		}})
}

// ── ApproveQuote ─────────────────────────────────────────────────────────────

type approveReq struct {
	cmdBase
	VersionNo   *int   `json:"version_no"`
	ContentHash string `json:"content_hash,omitempty"`
}

func (m *Module) estimateTax(ctx context.Context, c kit.Caller, h *Header, v *Version) (*TaxEstimate, error) {
	if m.tax == nil {
		return nil, kit.Unavailable("tax determination service not wired: the requested tax estimate is unavailable")
	}
	p := v.TaxParams
	now := m.today()
	d, err := m.tax.Determine(ctx, c.SvcCaller(), taxdet.Request{
		TransactionID: "quote:" + h.QuoteID + ":v" + itoa(v.VersionNo), SourceModule: "AR", LegalEntityID: h.LegalEntityID,
		JurisdictionID: p.JurisdictionID, SupplyJurisdictionID: p.SupplyJurisdictionID, TaxCategory: p.TaxCategory,
		GrossAmount: v.NetTotal, Currency: v.Currency, EffectiveFrom: day(now), SupplyDate: day(now),
		SellerPartyID: h.LegalEntityID, BuyerPartyID: h.CustomerPartyID, ProductClassification: p.ProductClassification,
		SupplyKind: p.SupplyKind, SupplyType: p.SupplyType, BuyerTaxRegistrationID: p.BuyerTaxRegistrationID, EvaluatedBy: c.PrincipalID})
	switch {
	case errors.Is(err, taxdet.ErrRejected):
		return nil, kit.Unprocessable(kit.CodeTaxDeterminationMissing, "tax determination could not produce the requested estimate")
	case err != nil:
		return nil, kit.Unavailable("tax determination service unavailable: the requested tax estimate cannot be obtained")
	case d == nil || !d.Usable():
		return nil, kit.Unprocessable(kit.CodeTaxDeterminationMissing, "tax determination returned no usable result")
	}
	return &TaxEstimate{DeterminationID: d.DeterminationID, Provenance: d.Provenance(), Status: d.Status, TaxableAmount: d.TaxableAmount,
		TaxRate: d.TaxRatePercentage, TaxAmount: d.Tax, Currency: d.Currency, JurisdictionID: d.JurisdictionID, IsEstimate: true,
		RecordedBy: c.PrincipalID, RecordedAt: now}, nil
}

func (m *Module) approveQuote(w http.ResponseWriter, r *http.Request) {
	var req approveReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if req.VersionNo == nil {
		m.fail(w, kit.Invalid("version_no is required: an approval is bound to one version"))
		return
	}
	var estimate *TaxEstimate
	var needsAuthority bool
	var preparedBy string
	m.runStep(w, r, &req.cmdBase, stepSpec{action: ActionQuoteApprove, tr: TrApprove,
		pre: func(ctx context.Context, c kit.Caller, h *Header, v *Version) error {
			if *req.VersionNo != h.CurrentVersionNo {
				return errApprovalInvalidated("approval was requested for version " + itoa(*req.VersionNo) + " but the current version is " +
					itoa(h.CurrentVersionNo) + ": an approval cannot be reused after the quote changed")
			}
			if req.ContentHash != "" && req.ContentHash != v.ContentHash {
				return errApprovalInvalidated("content_hash does not match the current version content")
			}
			needsAuthority = NeedsDiscountAuthority(v.DiscountPercent)
			if needsAuthority {
				if err := m.checkAction(ctx, c, ActionQuoteDiscountApprove); err != nil {
					return err
				}
				if c.PrincipalID == v.CreatedBy || c.PrincipalID == h.CreatedBy || c.PrincipalID == h.SubmittedBy {
					m.recordSoDDenial(ctx, c, h, v)
					return errSoD
				}
			}
			preparedBy = v.CreatedBy
			for _, err := range []error{m.requireLive(v), requirePriceSource(v)} {
				if err != nil {
					return err
				}
			}
			if _, err := m.profile(ctx, c.TenantID, h); err != nil {
				return err
			}
			if v.RequestTaxEstimate && v.TaxEstimate == nil {
				var err error
				if estimate, err = m.estimateTax(ctx, c, h, v); err != nil {
					return err
				}
			}
			return nil
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if *req.VersionNo != cur.CurrentVersionNo {
				return hist{}, nil, errApprovalInvalidated("the quote changed while the approval was in flight")
			}
			if err := m.requireLive(v); err != nil {
				return hist{}, nil, err
			}
			if estimate != nil {
				if err := insertTaxEstimate(ctx, tx, c, cur, v.VersionNo, estimate); err != nil {
					return hist{}, nil, err
				}
				v.TaxEstimate = estimate
			}
			if v.RequestTaxEstimate && v.TaxEstimate == nil {
				return hist{}, nil, kit.Unavailable("the requested tax estimate is not available")
			}
			if err := insertApproval(ctx, tx, c, cur, v, preparedBy, needsAuthority, now); err != nil {
				return hist{}, nil, err
			}
			t := now
			nh.ApprovedVersionNo, nh.ApprovedContentHash, nh.ApprovedBy, nh.ApprovedAt = v.VersionNo, v.ContentHash, c.PrincipalID, &t
			newVals := map[string]any{"state": nh.State, "approved_version_no": v.VersionNo, "approved_content_hash": v.ContentHash,
				"discount_percent": v.DiscountPercent.String(), "discount_authority_required": needsAuthority}
			if v.TaxEstimate != nil {
				newVals["tax_determination_id"], newVals["tax_provenance"] = v.TaxEstimate.DeterminationID, v.TaxEstimate.Provenance
			}
			return hist{Command: "approveQuote", Reason: req.ReasonCode, Evidence: req.EvidenceID, ApproverID: c.PrincipalID,
					Prior: map[string]any{"state": cur.State}, New: newVals},
				func(h *Header) []kit.Event {
					extra := map[string]any{"approved_by": c.PrincipalID, "prepared_by": preparedBy, "discount_authority_required": needsAuthority}
					if v.TaxEstimate != nil {
						extra["tax_determination_id"] = v.TaxEstimate.DeterminationID
					}
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.QuoteID, EventType: EventQuoteApproved, Payload: evPayload(c, h, v, extra)}}
				}, nil
		}})
}

// recordSoDDenial keeps evidence of the refused self-approval (the command wrote
// nothing else, so the audit is its own transaction).
func (m *Module) recordSoDDenial(ctx context.Context, c kit.Caller, h *Header, v *Version) {
	_ = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: h.QuoteID, Command: "approveQuote.denied",
			Detail: map[string]any{"denial": kit.CodeSegregationOfDuties, "version_no": v.VersionNo, "discount_percent": v.DiscountPercent.String(),
				"content_hash": v.ContentHash}})
	})
}

// ── SendQuote ────────────────────────────────────────────────────────────────

type sendReq struct {
	cmdBase
	SendChannel string `json:"send_channel,omitempty"`
	Recipient   string `json:"recipient,omitempty"`
}

// approvalIntact refuses to act on a version whose content is not the exact
// content that was approved (INVOICE_APPROVAL_INVALIDATED analogue for quotes).
func approvalIntact(h *Header, v *Version) error {
	if h.ApprovedContentHash == "" || h.ApprovedContentHash != v.ContentHash || h.ApprovedVersionNo == 0 {
		return errApprovalInvalidated("the current quote content is not the content that was approved: re-approval is required")
	}
	return nil
}

func (m *Module) sendQuote(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if !kit.Decode(w, r, &req) {
		return
	}
	if req.SendChannel != "" && !codeRe.MatchString(req.SendChannel) {
		m.fail(w, kit.Invalid("send_channel must be 1-32 characters of A-Z, 0-9 or '_'"))
		return
	}
	if len(req.Recipient) > 255 {
		m.fail(w, kit.Invalid("recipient is limited to 255 characters"))
		return
	}
	check := func(h *Header, v *Version) error {
		if err := approvalIntact(h, v); err != nil {
			return err
		}
		if err := m.requireLive(v); err != nil {
			return err
		}
		if v.RequestTaxEstimate && v.TaxEstimate == nil {
			return kit.Unprocessable(kit.CodeTaxDeterminationMissing, "the requested tax estimate is missing for this version")
		}
		return nil
	}
	m.runStep(w, r, &req.cmdBase, stepSpec{action: ActionQuoteManage, tr: TrSend,
		pre: func(ctx context.Context, c kit.Caller, h *Header, v *Version) error {
			if err := check(h, v); err != nil {
				return err
			}
			return m.preProfile(ctx, c, h, v)
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if err := check(cur, v); err != nil {
				return hist{}, nil, err
			}
			t := now
			nh.SentAt, nh.SentVersionNo = &t, v.VersionNo
			return hist{Command: "sendQuote", Reason: req.ReasonCode, Evidence: req.EvidenceID,
					Prior: map[string]any{"state": cur.State}, New: map[string]any{"state": nh.State, "sent_version_no": v.VersionNo,
						"send_channel": req.SendChannel, "recipient": req.Recipient}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.QuoteID, EventType: EventQuoteSent,
						Payload: evPayload(c, h, v, map[string]any{"send_channel": req.SendChannel, "recipient": req.Recipient, "evidence_id": req.EvidenceID})}}
				}, nil
		}})
}

// ── RecordAcceptance ─────────────────────────────────────────────────────────

var acceptanceChannels = map[string]bool{"PORTAL": true, "EMAIL": true, "SIGNED_DOCUMENT": true, "PURCHASE_ORDER": true, "EDI": true}

type acceptReq struct {
	cmdBase
	VersionNo         *int   `json:"version_no"`
	AcceptedBy        string `json:"accepted_by"`
	AcceptanceChannel string `json:"acceptance_channel"`
	AcceptedAt        string `json:"accepted_at,omitempty"`
}

func (m *Module) recordAcceptance(w http.ResponseWriter, r *http.Request) {
	var req acceptReq
	if !kit.Decode(w, r, &req) {
		return
	}
	// An internal user cannot fabricate an acceptance: evidence is mandatory.
	if strings.TrimSpace(req.EvidenceID) == "" {
		kit.WriteError(w, http.StatusBadRequest, kit.CodeEvidenceRequired, "evidence_id is required to record a customer acceptance")
		return
	}
	switch {
	case len(req.EvidenceID) > 255:
		m.fail(w, kit.Invalid("evidence_id is limited to 255 characters"))
		return
	case req.VersionNo == nil:
		m.fail(w, kit.Invalid("version_no is required: acceptance is of one version"))
		return
	case strings.TrimSpace(req.AcceptedBy) == "" || len(req.AcceptedBy) > 255:
		m.fail(w, kit.Invalid("accepted_by (the customer signatory) is required"))
		return
	case !acceptanceChannels[req.AcceptanceChannel]:
		m.fail(w, kit.Invalid("acceptance_channel must be PORTAL, EMAIL, SIGNED_DOCUMENT, PURCHASE_ORDER or EDI"))
		return
	}
	acceptedAt := m.today()
	if req.AcceptedAt != "" {
		t, err := time.Parse(time.RFC3339, req.AcceptedAt)
		if err != nil {
			m.fail(w, kit.Invalid("accepted_at must be RFC3339"))
			return
		}
		if t.After(m.today()) {
			m.fail(w, kit.Invalid("accepted_at must not be in the future"))
			return
		}
		acceptedAt = t.UTC()
	}
	m.runStep(w, r, &req.cmdBase, stepSpec{action: ActionQuoteManage, tr: TrAccept,
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if err := approvalIntact(cur, v); err != nil {
				return hist{}, nil, err
			}
			accepted, err := getVersion(ctx, tx, c.TenantID, cur.QuoteID, *req.VersionNo)
			if err != nil {
				return hist{}, nil, err
			}
			if accepted.ContentHash != cur.ApprovedContentHash {
				return hist{}, nil, kit.Conflict(CodeVersionSuperseded, "version "+itoa(*req.VersionNo)+" is not the version that was approved and sent")
			}
			if day(acceptedAt) > v.ValidityDate {
				return hist{}, nil, errExpired(v.ValidityDate)
			}
			if cur.SentAt != nil && day(acceptedAt) < day(*cur.SentAt) {
				return hist{}, nil, kit.Invalid("accepted_at precedes the date the quote was sent")
			}
			t := acceptedAt
			nh.AcceptedAt, nh.AcceptedBy, nh.AcceptanceChannel, nh.AcceptanceEvidenceID, nh.AcceptedVersionNo =
				&t, strings.TrimSpace(req.AcceptedBy), req.AcceptanceChannel, req.EvidenceID, accepted.VersionNo
			extra := map[string]any{"accepted_by": nh.AcceptedBy, "acceptance_channel": req.AcceptanceChannel, "evidence_id": req.EvidenceID,
				"accepted_at": acceptedAt.Format(time.RFC3339), "accepted_version_no": accepted.VersionNo, "recorded_by": c.PrincipalID}
			return hist{Command: "recordAcceptance", Reason: req.ReasonCode, Evidence: req.EvidenceID,
					Prior: map[string]any{"state": cur.State}, New: map[string]any{"state": nh.State, "accepted_by": nh.AcceptedBy,
						"acceptance_channel": req.AcceptanceChannel, "accepted_version_no": accepted.VersionNo}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.QuoteID, EventType: EventQuoteAccepted, Payload: evPayload(c, h, v, extra)}}
				}, nil
		}})
}

// ── RejectQuote ──────────────────────────────────────────────────────────────

func (m *Module) rejectQuote(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	if !kit.RequireReason(w, req.ReasonCode) {
		return
	}
	m.runStep(w, r, &req, stepSpec{action: ActionQuoteManage, tr: TrReject,
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			t := now
			nh.RejectedAt, nh.RejectionReason = &t, req.ReasonCode
			return hist{Command: "rejectQuote", Reason: req.ReasonCode, Evidence: req.EvidenceID,
					Prior: map[string]any{"state": cur.State}, New: map[string]any{"state": nh.State}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.QuoteID, EventType: EventQuoteRejected,
						Payload: evPayload(c, h, v, map[string]any{"reason_code": req.ReasonCode, "prior_state": cur.State, "evidence_id": req.EvidenceID})}}
				}, nil
		}})
}

// ── ExpireQuote ──────────────────────────────────────────────────────────────

func (m *Module) expireQuote(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	reason := req.ReasonCode
	if reason == "" {
		reason = DefaultExpireReason
	}
	notYet := func(v *Version) error {
		if !Expired(v.ValidityDate, m.today()) {
			return kit.Unprocessable(kit.CodeValidationFailed, "the quote validity has not elapsed (valid through "+v.ValidityDate+")")
		}
		return nil
	}
	m.runStep(w, r, &req, stepSpec{action: ActionQuoteManage, tr: TrExpire,
		pre: func(ctx context.Context, c kit.Caller, h *Header, v *Version) error { return notYet(v) },
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if err := notYet(v); err != nil {
				return hist{}, nil, err
			}
			t := now
			nh.ExpiredAt = &t
			return hist{Command: "expireQuote", Reason: reason, Evidence: req.EvidenceID,
					Prior: map[string]any{"state": cur.State}, New: map[string]any{"state": nh.State, "validity_date": v.ValidityDate}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.QuoteID, EventType: EventQuoteExpired,
						Payload: evPayload(c, h, v, map[string]any{"reason_code": reason, "prior_state": cur.State})}}
				}, nil
		}})
}

// ── ConvertQuoteToOrder ──────────────────────────────────────────────────────

func (m *Module) convertQuoteToOrder(w http.ResponseWriter, r *http.Request) {
	var req cmdBase
	if !kit.Decode(w, r, &req) {
		return
	}
	// AR-03 is the owner of orders: with no order service wired nothing can be
	// committed, and the quote must stay ACCEPTED.
	if m.orders == nil {
		m.fail(w, kit.Unavailable("AR-03 sales order service not wired"))
		return
	}
	check := func(h *Header, v *Version) error {
		if err := approvalIntact(h, v); err != nil {
			return err
		}
		return m.requireLive(v)
	}
	m.runStep(w, r, &req, stepSpec{action: ActionQuoteConvert, tr: TrConvert, already: StateConverted,
		pre: func(ctx context.Context, c kit.Caller, h *Header, v *Version) error {
			if err := check(h, v); err != nil {
				return err
			}
			view, err := m.profile(ctx, c.TenantID, h)
			if err != nil {
				return err
			}
			if view.CreditHold {
				return kit.Unprocessable(kit.CodeCreditHoldActive, "the customer is on credit hold: a quote cannot be converted to an order")
			}
			return nil
		},
		apply: func(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, v *Version, nh *Header, now time.Time) (hist, func(*Header) []kit.Event, error) {
			if err := check(cur, v); err != nil {
				return hist{}, nil, err
			}
			in := OrderFromQuote{QuoteID: cur.QuoteID, QuoteNumber: cur.QuoteNumber, VersionNo: v.VersionNo, ContentHash: v.ContentHash,
				LegalEntityID: cur.LegalEntityID, CustomerPartyID: cur.CustomerPartyID, Currency: v.Currency, Subtotal: v.Subtotal,
				DiscountPercent: v.DiscountPercent, DiscountAmount: v.DiscountAmount, NetTotal: v.NetTotal, PriceSourceRef: v.PriceSourceRef,
				Terms: v.Terms, Ref: "quote:" + cur.QuoteID + ":v" + itoa(v.VersionNo)}
			for _, l := range v.Lines {
				in.Lines = append(in.Lines, OrderLine{LineNo: l.LineNo, Description: l.Description, ProductRef: l.ProductRef,
					Quantity: l.Quantity, UnitPrice: l.UnitPrice, LineNet: l.LineNet})
			}
			orderID, err := m.orders.CreateFromQuote(ctx, tx, c, in)
			if err != nil {
				return hist{}, nil, err
			}
			if strings.TrimSpace(orderID) == "" {
				return hist{}, nil, errors.New("order service returned no order id")
			}
			t := now
			nh.ConvertedOrderID, nh.ConvertedAt, nh.ConvertedVersionNo = orderID, &t, v.VersionNo
			return hist{Command: "convertQuoteToOrder", Reason: req.ReasonCode, Evidence: req.EvidenceID,
					Prior: map[string]any{"state": cur.State}, New: map[string]any{"state": nh.State, "order_id": orderID, "converted_version_no": v.VersionNo}},
				func(h *Header) []kit.Event {
					return []kit.Event{{AggregateType: AggregateType, AggregateID: h.QuoteID, EventType: EventQuoteConverted,
						Payload: evPayload(c, h, v, map[string]any{"order_id": orderID, "source_ref": in.Ref})}}
				}, nil
		}})
}

func itoa(n int) string { return strconv.Itoa(n) }
