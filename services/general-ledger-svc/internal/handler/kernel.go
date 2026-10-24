package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/kernel"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// Authorization actions of the accounting kernel API. Posting-rule activation is its own action so build, test,
// approval and activation can be held by different people (standard §30).
const (
	actionBookManage      = "GL_BOOK_MANAGE"
	actionBookRead        = "GL_BOOK_READ"
	actionEventRegister   = "GL_EVENT_REGISTER"
	actionEventRead       = "GL_EVENT_READ"
	actionRuleManage      = "GL_RULE_MANAGE"
	actionRuleApprove     = "GL_RULE_APPROVE"
	actionRuleActivate    = "GL_RULE_ACTIVATE"
	actionRuleRead        = "GL_RULE_READ"
	actionPostingEvaluate = "GL_POSTING_EVALUATE"
	actionDimensionManage = "GL_DIMENSION_MANAGE"
	actionDimensionRead   = "GL_DIMENSION_READ"
)

// KernelHandler serves the Global Accounting Kernel: books, accounting events, posting rule sets, evaluation and
// dimensions. Evaluation is a dry run: nothing here writes a journal.
type KernelHandler struct {
	svc   *kernel.Service
	repo  kernel.Repo
	ref   kernel.RefData
	units kernel.CurrencyUnits
	fx    kernel.FXSource
	authz AuthZClient
	gate  kernel.PeriodGate
	log   *zap.Logger
}

// NewKernel builds the handler. fx may be nil, in which case a foreign-currency evaluation must carry its fx_basis.
func NewKernel(svc *kernel.Service, repo kernel.Repo, ref kernel.RefData, units kernel.CurrencyUnits, fx kernel.FXSource, az AuthZClient, log *zap.Logger) *KernelHandler {
	return &KernelHandler{svc: svc, repo: repo, ref: ref, units: units, fx: fx, authz: az, log: log}
}

// WithPeriodGate wires the server-side period gate that posting needs. Without one the posting command refuses (fails closed).
func (h *KernelHandler) WithPeriodGate(g kernel.PeriodGate) *KernelHandler {
	h.gate = g
	return h
}

// RegisterKernelRoutes mounts the kernel routes.
func RegisterKernelRoutes(r chi.Router, h *KernelHandler) {
	r.Route("/v1/accounting-books", func(r chi.Router) {
		r.Post("/", h.CreateBook)
		r.Get("/", h.ListBooks)
		r.Get("/{id}", h.GetBook)
		r.Post("/{id}/activate", h.bookStatus(kernel.BookActive))
		r.Post("/{id}/suspend", h.bookStatus(kernel.BookSuspended))
		r.Post("/{id}/retire", h.bookStatus(kernel.BookRetired))
	})
	r.Route("/v1/accounting-events", func(r chi.Router) {
		r.Post("/", h.RegisterEvent)
		r.Get("/", h.ListEvents)
		r.Get("/{id}", h.GetEvent)
	})
	r.Route("/v1/posting-rule-sets", func(r chi.Router) {
		r.Post("/", h.CreateRuleSet)
		r.Get("/", h.ListRuleSets)
		r.Get("/{id}", h.GetRuleSet)
		r.Get("/{id}/events", h.RuleSetEvents)
		r.Post("/{id}/test", h.TestRuleSet)
		r.Post("/{id}/approve", h.ApproveRuleSet)
		r.Post("/{id}/activate", h.ActivateRuleSet)
		r.Post("/{id}/retire", h.RetireRuleSet)
		r.Post("/{id}/return-to-draft", h.ReturnToDraft)
	})
	r.Post("/v1/posting-evaluations", h.Evaluate)
	r.Post("/v1/accounting-postings", h.PostEvent)
	r.Get("/v1/posting-traces", h.GetPostingTrace)
	r.Route("/v1/reconciliation-runs", func(r chi.Router) {
		r.Post("/", h.RunReconciliation)
		r.Get("/", h.ListReconciliationRuns)
		r.Get("/{id}", h.GetReconciliationRun)
		r.Post("/{id}/assign", h.AssignReconciliationRun)
		r.Post("/{id}/accept", h.AcceptReconciliationRun)
		r.Post("/{id}/resolve", h.ResolveReconciliationRun)
		r.Post("/{id}/verify", h.VerifyReconciliationRun)
	})
	r.Get("/v1/reconciliation-status", h.GetReconciliationStatus)
	r.Post("/v1/reconciliation-tolerances", h.CreateReconciliationTolerance)
	r.Get("/v1/reconciliation-tolerances", h.ListReconciliationTolerances)
	r.Get("/v1/counterparty-balances", h.GetCounterpartyBalances)
	r.Get("/v1/posting-rule-packs", h.ListRulePacks)
	r.Post("/v1/posting-rule-packs/{name}/instantiate", h.InstantiateRulePack)
	r.Get("/v1/account-applicability/{account_code}", h.GetAccountScope)
	r.Put("/v1/account-applicability/{account_code}", h.SetAccountScope)
	r.Route("/v1/dimensions", func(r chi.Router) {
		r.Post("/", h.CreateDimension)
		r.Get("/", h.ListDimensions)
		r.Post("/{code}/values", h.CreateDimensionValue)
	})
	r.Get("/v1/dimension-values", h.ListDimensionValues)
	r.Post("/v1/dimension-values/{id}/retire", h.RetireDimensionValue)
}

func (h *KernelHandler) guard(w http.ResponseWriter, r *http.Request, action string) (tenant string, a kernel.Actor, ok bool) {
	principal := r.Header.Get("X-Principal-Id")
	if principal == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", domain.ErrIdentityMissing.Error())
		return
	}
	tenant = svcmiddleware.TenantFromContext(r.Context())
	if tenant == "" {
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", domain.ErrTenantScopeMissing.Error())
		return
	}
	entity := r.Header.Get("X-Legal-Entity-Id")
	if entity == "" {
		entity = tenant
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, entity, action); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "authorization_denied", "")
		} else {
			h.log.Error("authorization check failed — failing closed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "authorization_service_unavailable", "")
		}
		return
	}
	return tenant, kernel.Actor{PrincipalID: principal, CorrelationID: r.Header.Get("X-Correlation-ID")}, true
}

type kernelProblem struct {
	Error  string         `json:"error"`
	Family string         `json:"family,omitempty"`
	Code   string         `json:"code,omitempty"`
	Detail string         `json:"detail,omitempty"`
	Facts  map[string]any `json:"facts,omitempty"`
}

var familyStatus = map[string]int{
	kernel.FamAuth: http.StatusForbidden, kernel.FamPeriod: http.StatusConflict, kernel.FamDup: http.StatusConflict, kernel.FamConc: http.StatusConflict,
	kernel.FamExt: http.StatusServiceUnavailable,
}

func (h *KernelHandler) fail(w http.ResponseWriter, err error) {
	if e, ok := kernel.AsException(err); ok {
		st := http.StatusUnprocessableEntity
		if s, found := familyStatus[e.Family]; found {
			st = s
		}
		if st == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "5")
		}
		writeJSON(w, st, kernelProblem{Error: strings.ToLower(e.Family) + "_" + strings.ToLower(e.Code), Family: e.Family, Code: e.Code, Detail: e.Detail, Facts: e.Facts})
		return
	}
	switch {
	case errors.Is(err, kernel.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "")
	case errors.Is(err, kernel.ErrSelfApproval):
		writeError(w, http.StatusForbidden, "separation_of_duties", err.Error())
	case errors.Is(err, kernel.ErrNotTested):
		writeError(w, http.StatusConflict, "not_tested", err.Error())
	case errors.Is(err, kernel.ErrOverlap):
		writeError(w, http.StatusConflict, "rule_overlap", err.Error())
	case errors.Is(err, kernel.ErrVersion):
		writeError(w, http.StatusConflict, "version_conflict", err.Error())
	case errors.Is(err, kernel.ErrExists):
		writeError(w, http.StatusConflict, "already_exists", err.Error())
	case errors.Is(err, kernel.ErrState), errors.Is(err, kernel.ErrIdempotency):
		writeError(w, http.StatusConflict, "state_conflict", err.Error())
	default:
		h.log.Error("kernel request failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

func queryLimit(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return n
}

// ── books ──────────────────────────────────────────────────────────────────────

func (h *KernelHandler) CreateBook(w http.ResponseWriter, r *http.Request) {
	var b kernel.Book
	if !decodeJSON(w, r, &b) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionBookManage)
	if !ok {
		return
	}
	got, err := h.svc.CreateBook(r.Context(), tenant, a, b)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, got)
}

func (h *KernelHandler) ListBooks(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionBookRead)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := h.repo.ListBooks(r.Context(), tenant, kernel.BookFilter{LegalEntityID: q.Get("legal_entity_id"), Status: strings.ToUpper(q.Get("status")), Limit: queryLimit(r)})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"books": list, "total": len(list)})
}

func (h *KernelHandler) GetBook(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionBookRead)
	if !ok {
		return
	}
	b, err := h.repo.GetBook(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

type versionBody struct {
	ExpectedVersion int64  `json:"expected_version"`
	BookID          string `json:"book_id"`
	Reason          string `json:"reason"`
}

func (h *KernelHandler) bookStatus(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body versionBody
		if !decodeJSON(w, r, &body) {
			return
		}
		tenant, a, ok := h.guard(w, r, actionBookManage)
		if !ok {
			return
		}
		b, err := h.svc.SetBookStatus(r.Context(), tenant, a, chi.URLParam(r, "id"), to, body.ExpectedVersion)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, b)
	}
}

// ── events ─────────────────────────────────────────────────────────────────────

// RegisterEvent is RegisterAccountingEvent: 201 for a new event, 200 with the original for a replay of the same event.
func (h *KernelHandler) RegisterEvent(w http.ResponseWriter, r *http.Request) {
	var e kernel.AccountingEvent
	if !decodeJSON(w, r, &e) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionEventRegister)
	if !ok {
		return
	}
	// the tenant and the registering principal are resolved here, never taken from the body
	if e.IdempotencyKey == "" {
		e.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	if e.CorrelationID == "" {
		e.CorrelationID = a.CorrelationID
	}
	got, existing, err := h.svc.RegisterEvent(r.Context(), tenant, a, e)
	if err != nil {
		h.fail(w, err)
		return
	}
	if existing {
		w.Header().Set("X-Replayed", "true")
		writeJSON(w, http.StatusOK, got)
		return
	}
	writeJSON(w, http.StatusCreated, got)
}

func (h *KernelHandler) GetEvent(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionEventRead)
	if !ok {
		return
	}
	e, err := h.repo.GetEvent(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (h *KernelHandler) ListEvents(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionEventRead)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := h.repo.ListEvents(r.Context(), tenant, kernel.EventFilter{LegalEntityID: q.Get("legal_entity_id"), EventType: q.Get("event_type"), SourceObject: q.Get("source_object_id"), Limit: queryLimit(r)})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": list, "total": len(list)})
}

// ── rule sets ──────────────────────────────────────────────────────────────────

func (h *KernelHandler) CreateRuleSet(w http.ResponseWriter, r *http.Request) {
	var rs kernel.RuleSet
	if !decodeJSON(w, r, &rs) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionRuleManage)
	if !ok {
		return
	}
	got, err := h.svc.CreateRuleSet(r.Context(), tenant, a, rs)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, got)
}

func (h *KernelHandler) ListRuleSets(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionRuleRead)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := h.repo.ListRuleSets(r.Context(), tenant, kernel.RuleFilter{EventType: q.Get("event_type"), Status: strings.ToUpper(q.Get("status")), Name: q.Get("name"), Limit: queryLimit(r)})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"posting_rule_sets": list, "total": len(list)})
}

func (h *KernelHandler) GetRuleSet(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionRuleRead)
	if !ok {
		return
	}
	rs, err := h.repo.GetRuleSet(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (h *KernelHandler) RuleSetEvents(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionRuleRead)
	if !ok {
		return
	}
	list, err := h.repo.RuleSetEvents(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": list, "total": len(list)})
}

// TestRuleSet runs the rule set's regression pack through the engine. 200 when it passed, 422 with the failed cases
// when it did not (the rule set stays a draft).
func (h *KernelHandler) TestRuleSet(w http.ResponseWriter, r *http.Request) {
	var body versionBody
	if !decodeJSON(w, r, &body) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionRuleManage)
	if !ok {
		return
	}
	rs, failed, err := h.svc.TestRuleSet(r.Context(), tenant, a, chi.URLParam(r, "id"), body.BookID, body.ExpectedVersion)
	if err != nil {
		h.fail(w, err)
		return
	}
	if len(failed) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "test_pack_failed", "failed_cases": failed, "posting_rule_set": rs})
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (h *KernelHandler) ApproveRuleSet(w http.ResponseWriter, r *http.Request) {
	var body versionBody
	if !decodeJSON(w, r, &body) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionRuleApprove)
	if !ok {
		return
	}
	rs, err := h.svc.ApproveRuleSet(r.Context(), tenant, a, chi.URLParam(r, "id"), body.ExpectedVersion)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (h *KernelHandler) ActivateRuleSet(w http.ResponseWriter, r *http.Request) {
	var body versionBody
	if !decodeJSON(w, r, &body) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionRuleActivate)
	if !ok {
		return
	}
	rs, superseded, err := h.svc.ActivateRuleSet(r.Context(), tenant, a, chi.URLParam(r, "id"), body.ExpectedVersion)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"posting_rule_set": rs, "superseded": superseded})
}

func (h *KernelHandler) RetireRuleSet(w http.ResponseWriter, r *http.Request) {
	var body versionBody
	if !decodeJSON(w, r, &body) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionRuleActivate)
	if !ok {
		return
	}
	rs, err := h.svc.RetireRuleSet(r.Context(), tenant, a, chi.URLParam(r, "id"), body.Reason, body.ExpectedVersion)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (h *KernelHandler) ReturnToDraft(w http.ResponseWriter, r *http.Request) {
	var body versionBody
	if !decodeJSON(w, r, &body) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionRuleManage)
	if !ok {
		return
	}
	rs, err := h.svc.ReturnToDraft(r.Context(), tenant, a, chi.URLParam(r, "id"), body.ExpectedVersion)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

// ── evaluation ─────────────────────────────────────────────────────────────────

// Evaluate is EvaluatePosting: a dry-run journal proposal with its rule provenance. Nothing is written.
func (h *KernelHandler) Evaluate(w http.ResponseWriter, r *http.Request) {
	var req kernel.EvaluateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant, _, ok := h.guard(w, r, actionPostingEvaluate)
	if !ok {
		return
	}
	dir := kernel.TenantDirectory{Tenant: tenant, Ref: h.ref, Units: h.units, FX: h.fx}
	p, err := h.svc.Evaluate(r.Context(), tenant, req, dir)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── dimensions ─────────────────────────────────────────────────────────────────

type dimensionBody struct {
	DimensionCode string `json:"dimension_code"`
	Name          string `json:"name"`
}

func (h *KernelHandler) CreateDimension(w http.ResponseWriter, r *http.Request) {
	var b dimensionBody
	if !decodeJSON(w, r, &b) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionDimensionManage)
	if !ok {
		return
	}
	d, err := h.svc.CreateDimension(r.Context(), tenant, a, b.DimensionCode, b.Name)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (h *KernelHandler) ListDimensions(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionDimensionRead)
	if !ok {
		return
	}
	list, err := h.repo.ListDimensions(r.Context(), tenant)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dimensions": list, "total": len(list)})
}

func (h *KernelHandler) CreateDimensionValue(w http.ResponseWriter, r *http.Request) {
	var v kernel.DimensionValue
	if !decodeJSON(w, r, &v) {
		return
	}
	tenant, a, ok := h.guard(w, r, actionDimensionManage)
	if !ok {
		return
	}
	v.DimensionCode = chi.URLParam(r, "code")
	got, err := h.svc.CreateDimensionValue(r.Context(), tenant, a, v)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, got)
}

func (h *KernelHandler) ListDimensionValues(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.guard(w, r, actionDimensionRead)
	if !ok {
		return
	}
	list, err := h.repo.ListDimensionValues(r.Context(), tenant, r.URL.Query().Get("dimension"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"values": list, "total": len(list)})
}

func (h *KernelHandler) RetireDimensionValue(w http.ResponseWriter, r *http.Request) {
	tenant, a, ok := h.guard(w, r, actionDimensionManage)
	if !ok {
		return
	}
	v, err := h.repo.RetireDimensionValue(r.Context(), tenant, chi.URLParam(r, "id"), a.PrincipalID, time.Now().UTC())
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
