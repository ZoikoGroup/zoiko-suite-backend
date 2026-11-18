package handler

// REF-10 Payment Terms (ZS-SVC-I-001 s4.20).
//
//	POST /v1/admin/payment-terms                                      register a term
//	POST /v1/admin/payment-terms/{code}/versions                      draft the next version
//	POST /v1/admin/payment-terms/{code}/versions/{n}/approve          ApprovePaymentTerm
//	POST /v1/admin/payment-terms/{code}/versions/{n}/supersede        SupersedePaymentTerm
//	POST /v1/admin/payment-terms/{code}/versions/{n}/retire           RetirePaymentTerm
//	GET  /v1/payment-terms/{code}[/versions[/{n}]]                    GetPaymentTerm
//	GET  /v1/payment-terms/{code}/due-date?invoice_date=              CalculateDueDate
//	GET  /v1/payment-terms/{code}/explain-due-date?invoice_date=      ExplainDueDate
//
// A due date is a deterministic function of the term version in force on the
// invoice date and the calendar version that governs the date it falls on, and
// the answer names both: "Due-date calculation returns due date plus term
// version, calendar version, basis date and adjustment convention." A version
// that is later superseded, or a calendar amended later, never changes a due
// date asked as it was known then, and either version can be pinned to
// reproduce an answer exactly.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// paymentTermStore is the registry's payment-term capability, optional like the
// calendar one; the routes answer 503 without it.
type paymentTermStore interface {
	CreatePaymentTerm(ctx context.Context, code, name string, jurisdictionID *string, actor string) (*domain.PaymentTerm, bool, error)
	GetPaymentTerm(ctx context.Context, code string) (*domain.PaymentTerm, error)
	GetPaymentTermVersion(ctx context.Context, code string, version int) (*domain.PaymentTermVersion, error)
	ListPaymentTermVersions(ctx context.Context, code string) ([]*domain.PaymentTermVersion, error)
	CreatePaymentTermVersion(ctx context.Context, code string, in domain.PaymentTermVersionInput, actor string) (*domain.PaymentTermVersion, error)
	ApprovePaymentTermVersion(ctx context.Context, code string, version int, actor string) (*domain.PaymentTermVersion, bool, error)
	SupersedePaymentTermVersion(ctx context.Context, code string, version, supersedes int, actor string) (*domain.PaymentTermVersion, *domain.PaymentTermVersion, bool, error)
	RetirePaymentTermVersion(ctx context.Context, code string, version int, reason string, endsOn *string, actor string) (*domain.PaymentTermVersion, bool, error)
	ResolvePaymentTermVersion(ctx context.Context, code, on string, knownAt *time.Time, pin *int) (*domain.PaymentTermVersion, bool, error)
	ResolveCalendarVersion(ctx context.Context, code, on string, knownAt *time.Time, pin *int) (*store.CalendarVersionRecord, bool, error)
}

func (h *Handler) paymentTerms(w http.ResponseWriter, r *http.Request) (paymentTermStore, bool) {
	s, ok := h.registry.(paymentTermStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "the registry does not support payment terms")
	}
	return s, ok
}

func registerPaymentTermRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/payment-terms/{term_code}", h.GetPaymentTerm)
	r.Get("/v1/payment-terms/{term_code}/versions", h.ListPaymentTermVersions)
	r.Get("/v1/payment-terms/{term_code}/versions/{version}", h.GetPaymentTermVersion)
	r.Get("/v1/payment-terms/{term_code}/due-date", h.CalculateDueDate)
	r.Get("/v1/payment-terms/{term_code}/explain-due-date", h.ExplainDueDate)

	r.Post("/v1/admin/payment-terms", h.CreatePaymentTerm)
	r.Post("/v1/admin/payment-terms/{term_code}/versions", h.CreatePaymentTermVersion)
	r.Post("/v1/admin/payment-terms/{term_code}/versions/{version}/approve", h.ApprovePaymentTerm)
	r.Post("/v1/admin/payment-terms/{term_code}/versions/{version}/supersede", h.SupersedePaymentTerm)
	r.Post("/v1/admin/payment-terms/{term_code}/versions/{version}/retire", h.RetirePaymentTerm)
}

func (h *Handler) writePaymentTermError(w http.ResponseWriter, r *http.Request, err error, corr string) {
	var invalid *store.PaymentTermInvalid
	switch {
	case errors.As(err, &invalid):
		writeProblemWith(w, r, http.StatusBadRequest, "invalid_payment_term", err.Error(), map[string]any{"problems": invalid.Problems})
	case errors.Is(err, domain.ErrInvalidPaymentTerm):
		writeError(w, r, http.StatusBadRequest, "invalid_payment_term", err.Error())
	case errors.Is(err, domain.ErrPaymentTermNotFound):
		writeError(w, r, http.StatusNotFound, "payment_term_not_found", err.Error())
	case errors.Is(err, domain.ErrPaymentTermVersionNotFound):
		writeError(w, r, http.StatusNotFound, "payment_term_version_not_found", err.Error())
	case errors.Is(err, domain.ErrNoPaymentTermVersion):
		writeError(w, r, http.StatusNotFound, "no_payment_term_version", err.Error())
	case errors.Is(err, domain.ErrPaymentTermNotDraft):
		writeError(w, r, http.StatusConflict, "payment_term_not_draft", err.Error())
	case errors.Is(err, domain.ErrPaymentTermNotActive):
		writeError(w, r, http.StatusConflict, "payment_term_not_active", err.Error())
	case errors.Is(err, domain.ErrPaymentTermOverlap):
		writeError(w, r, http.StatusConflict, "supersession_required", err.Error())
	case errors.Is(err, domain.ErrInvalidSupersession):
		writeError(w, r, http.StatusConflict, "invalid_supersession", err.Error())
	case errors.Is(err, domain.ErrCalendarNotReady):
		writeError(w, r, http.StatusConflict, "calendar_not_ready", err.Error())
	case errors.Is(err, domain.ErrBasisDateMissing):
		writeError(w, r, http.StatusBadRequest, "basis_date_required", err.Error())
	default:
		h.writeCalendarError(w, r, err, corr)
	}
}

// ── terms and versions ──────────────────────────────────────────────────────

// CreatePaymentTermRequest is the body of POST /v1/admin/payment-terms.
type CreatePaymentTermRequest struct {
	TermCode       string  `json:"term_code"`
	TermName       string  `json:"term_name"`
	JurisdictionID *string `json:"jurisdiction_id"`
}

// CreatePaymentTerm handles POST /v1/admin/payment-terms: 201 registered, 200 an
// identical term already exists, 409 the code names a different term.
func (h *Handler) CreatePaymentTerm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.paymentTerms(w, r)
	if !ok {
		return
	}
	actor, ok := h.admin(w, r, "payment_term", "create")
	if !ok {
		return
	}
	var req CreatePaymentTermRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !domain.ValidTermCode(req.TermCode) {
		writeError(w, r, http.StatusBadRequest, "invalid_term_code", "term_code must be upper-case letters, digits and underscores, starting with a letter (2-32 characters), such as NET30")
		return
	}
	if req.JurisdictionID != nil && *req.JurisdictionID == "" {
		req.JurisdictionID = nil
	}
	t, created, err := s.CreatePaymentTerm(r.Context(), req.TermCode, req.TermName, req.JurisdictionID, actor)
	if err != nil {
		h.writePaymentTermError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, t)
}

// CreatePaymentTermVersion handles POST /v1/admin/payment-terms/{term_code}/versions:
// drafts the next version. Every problem with the rule is reported at once.
func (h *Handler) CreatePaymentTermVersion(w http.ResponseWriter, r *http.Request) {
	s, ok := h.paymentTerms(w, r)
	if !ok {
		return
	}
	actor, ok := h.admin(w, r, "payment_term", "create")
	if !ok {
		return
	}
	var in domain.PaymentTermVersionInput
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := s.CreatePaymentTermVersion(r.Context(), chi.URLParam(r, "term_code"), in, actor)
	if err != nil {
		h.writePaymentTermError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (h *Handler) GetPaymentTerm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.paymentTerms(w, r)
	if !ok {
		return
	}
	t, err := s.GetPaymentTerm(r.Context(), chi.URLParam(r, "term_code"))
	if err != nil {
		h.writePaymentTermError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *Handler) ListPaymentTermVersions(w http.ResponseWriter, r *http.Request) {
	s, ok := h.paymentTerms(w, r)
	if !ok {
		return
	}
	p, ok := h.beginList(w, r, nil, nil)
	if !ok {
		return
	}
	vs, err := s.ListPaymentTermVersions(r.Context(), chi.URLParam(r, "term_code"))
	if err != nil {
		h.writePaymentTermError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	finishListAs(h, w, r, p, "versions", pageOf(p, vs))
}

func (h *Handler) GetPaymentTermVersion(w http.ResponseWriter, r *http.Request) {
	s, ok := h.paymentTerms(w, r)
	if !ok {
		return
	}
	n, ok := parseVersionParam(w, r)
	if !ok {
		return
	}
	v, err := s.GetPaymentTermVersion(r.Context(), chi.URLParam(r, "term_code"), n)
	if err != nil {
		h.writePaymentTermError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ── lifecycle ───────────────────────────────────────────────────────────────

func termVersionPayload(v *domain.PaymentTermVersion) map[string]any {
	return map[string]any{
		"term_code": v.TermCode, "term_version": v.Version, "term_version_id": v.TermVersionID, "content_digest": v.ContentDigest,
		"formula_text": v.FormulaText, "effective_from": v.EffectiveFrom, "ends_on": strPtrValue(v.EndsOn), "status": v.Status,
		"basis_date_rule": v.BasisDateRule, "due_method": v.DueMethod, "business_day_convention": v.BusinessDayConvention,
		"calendar_code": nilIfEmptyStr(v.CalendarCode), "controlled_credit": v.ControlledCredit,
	}
}

// strPtrValue is the string a pointer points to, or nil.
func strPtrValue(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func nilIfEmptyStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ApprovePaymentTerm handles POST .../versions/{version}/approve.
//
//	200 → approved: in force from its effective_from (or already was)
//	403 → the approver is the author · 409 → not a draft, or an approved version
//	already governs part of its period (use supersede), or its calendar has no published version
//
// The author cannot approve their own version. That is stricter than the spec's
// rule for an exceptional term above the credit threshold, and applies to every term.
func (h *Handler) ApprovePaymentTerm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.paymentTerms(w, r)
	if !ok {
		return
	}
	actor, ok := h.admin(w, r, "payment_term", "approve")
	if !ok {
		return
	}
	n, ok := parseVersionParam(w, r)
	if !ok {
		return
	}
	v, changed, err := s.ApprovePaymentTermVersion(r.Context(), chi.URLParam(r, "term_code"), n, actor)
	if err != nil {
		h.writePaymentTermError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, events.EventPaymentTermActivated, v.TermVersionID, actor, termVersionPayload(v))
	}
	writeJSON(w, http.StatusOK, v)
}

// SupersedeTermRequest is the body of the supersede command.
type SupersedeTermRequest struct {
	SupersedesVersion int `json:"supersedes_version"`
}

// SupersedePaymentTerm handles POST .../versions/{version}/supersede.
//
// It approves the draft over the active version named in supersedes_version, which
// stops governing on the new version's effective_from: every date before it keeps
// the replaced version's rule. 409 invalid_supersession when the version named is
// not the active one this version can take over from.
func (h *Handler) SupersedePaymentTerm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.paymentTerms(w, r)
	if !ok {
		return
	}
	actor, ok := h.admin(w, r, "payment_term", "supersede")
	if !ok {
		return
	}
	n, ok := parseVersionParam(w, r)
	if !ok {
		return
	}
	var req SupersedeTermRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SupersedesVersion < 1 {
		writeMissingField(w, r, "supersedes_version")
		return
	}
	next, old, changed, err := s.SupersedePaymentTermVersion(r.Context(), chi.URLParam(r, "term_code"), n, req.SupersedesVersion, actor)
	if err != nil {
		h.writePaymentTermError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, events.EventPaymentTermSuperseded, old.TermVersionID, actor, func() map[string]any {
			p := termVersionPayload(old)
			p["superseded_by_version"], p["superseded_by_version_id"] = next.Version, next.TermVersionID
			return p
		}())
		h.emitRegistry(r, events.EventPaymentTermActivated, next.TermVersionID, actor, func() map[string]any {
			p := termVersionPayload(next)
			p["supersedes_version"], p["supersedes_version_id"] = old.Version, old.TermVersionID
			return p
		}())
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": next, "superseded": old})
}

// RetireTermRequest is the body of the retire command.
type RetireTermRequest struct {
	Reason string  `json:"reason"`
	EndsOn *string `json:"ends_on"`
}

// RetirePaymentTerm handles POST .../versions/{version}/retire. A draft is
// withdrawn; an active version stops governing on ends_on (default today), so the
// dates before it keep the rule. A reason is required.
func (h *Handler) RetirePaymentTerm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.paymentTerms(w, r)
	if !ok {
		return
	}
	actor, ok := h.admin(w, r, "payment_term", "retire")
	if !ok {
		return
	}
	n, ok := parseVersionParam(w, r)
	if !ok {
		return
	}
	var req RetireTermRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	v, changed, err := s.RetirePaymentTermVersion(r.Context(), chi.URLParam(r, "term_code"), n, req.Reason, req.EndsOn, actor)
	if err != nil {
		h.writePaymentTermError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		p := termVersionPayload(v)
		p["retire_reason"] = v.RetireReason
		h.emitRegistry(r, events.EventPaymentTermRetired, v.TermVersionID, actor, p)
	}
	writeJSON(w, http.StatusOK, v)
}

// ── CalculateDueDate and ExplainDueDate ─────────────────────────────────────

// CalculateDueDate handles GET /v1/payment-terms/{term_code}/due-date.
//
//	invoice_date=YYYY-MM-DD    required: also the date the term version in force is chosen by
//	receipt_date, delivery_date  needed only by a term that counts from them (400 basis_date_required)
//	known_at=RFC3339           answer as the term and calendar were known then (default now)
//	term_version=N             pin that term version exactly
//	calendar_version=N         pin that calendar version exactly
//
//	200 → the due date, with the term version, the calendar version, the basis date
//	      and the adjustment convention that gave it
//	404 → no such term, no approved version in force, or no calendar version governs
//
// "Historical due dates are never silently recalculated after calendar/term
// update": ask as it was known then, or pin the versions the first answer named.
func (h *Handler) CalculateDueDate(w http.ResponseWriter, r *http.Request) {
	h.dueDate(w, r, false)
}

// ExplainDueDate handles GET /v1/payment-terms/{term_code}/explain-due-date: the
// same answer with the steps that produced it, in order. The explanation is the
// executed calculation, never written separately.
func (h *Handler) ExplainDueDate(w http.ResponseWriter, r *http.Request) {
	h.dueDate(w, r, true)
}

func parseOptionalDate(w http.ResponseWriter, r *http.Request, name string) (*time.Time, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, true
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_"+name, name+" must be YYYY-MM-DD")
		return nil, false
	}
	return &t, true
}

func parseOptionalPin(w http.ResponseWriter, r *http.Request, name string) (*int, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		writeError(w, r, http.StatusBadRequest, "invalid_version", name+" must be a positive integer")
		return nil, false
	}
	return &n, true
}

func (h *Handler) dueDate(w http.ResponseWriter, r *http.Request, explain bool) {
	s, ok := h.paymentTerms(w, r)
	if !ok {
		return
	}
	code := chi.URLParam(r, "term_code")
	corr := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()
	if q.Get("invoice_date") == "" {
		writeMissingField(w, r, "invoice_date")
		return
	}
	invoice, ok := parseOptionalDate(w, r, "invoice_date")
	if !ok {
		return
	}
	receipt, ok := parseOptionalDate(w, r, "receipt_date")
	if !ok {
		return
	}
	delivery, ok := parseOptionalDate(w, r, "delivery_date")
	if !ok {
		return
	}
	var knownAt *time.Time
	if v := q.Get("known_at"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_known_at", "known_at must be an RFC3339 timestamp")
			return
		}
		knownAt = &t
	}
	termPin, ok := parseOptionalPin(w, r, "term_version")
	if !ok {
		return
	}
	calPin, ok := parseOptionalPin(w, r, "calendar_version")
	if !ok {
		return
	}

	v, pinned, err := s.ResolvePaymentTermVersion(r.Context(), code, q.Get("invoice_date"), knownAt, termPin)
	if err != nil {
		h.writePaymentTermError(w, r, err, corr)
		return
	}
	lookup := func(on time.Time) (*domain.CalendarVersion, *domain.CalendarProvenance, error) {
		rec, calPinned, err := s.ResolveCalendarVersion(r.Context(), v.CalendarCode, on.Format("2006-01-02"), knownAt, calPin)
		if err != nil {
			return nil, nil, err
		}
		cal, prov := rec.Domain(), rec.Provenance(calPinned)
		return &cal, &prov, nil
	}
	res, err := domain.CalculatePaymentDueDate(*v, domain.DueDateFacts{InvoiceDate: *invoice, ReceiptDate: receipt, DeliveryDate: delivery}, lookup)
	if err != nil {
		h.writePaymentTermError(w, r, err, corr)
		return
	}
	res.Term = domain.TermProvenance{
		TermCode: v.TermCode, TermVersion: v.Version, TermVersionID: v.TermVersionID, ContentDigest: v.ContentDigest, FormulaText: v.FormulaText,
		EffectiveFrom: v.EffectiveFrom, EndsOn: v.EndsOn, Pinned: pinned,
	}
	if !explain {
		res.Steps = nil
	}
	writeJSON(w, http.StatusOK, res)
}
