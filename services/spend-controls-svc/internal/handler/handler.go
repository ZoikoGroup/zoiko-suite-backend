package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/spend-controls-svc/internal/domain"
	svcmiddleware "zoiko.io/spend-controls-svc/internal/middleware"
)

type Store interface {
	// CreatePolicy end-dates any limit it replaces and reports how many, so the
	// caller can say what happened rather than infer it from a separate read.
	CreatePolicy(ctx context.Context, p *domain.SpendPolicy) (superseded int, err error)
	DeactivatePolicy(ctx context.Context, spendPolicyID string) error
	ListPolicies(ctx context.Context, legalEntityID, category string, activeOnly bool) ([]domain.SpendPolicy, error)
	// PolicyUsageTotals aggregates in the database, over each policy's own
	// enforcement window, so a budget meter and a spend check agree.
	PolicyUsageTotals(ctx context.Context, legalEntityID, category string) ([]domain.PolicyUsageTotal, error)
	// EvaluateSpend decides and records in one transaction with the policy row
	// locked. It replaces a Find/Sum/Record sequence that spanned three separate
	// transactions, which let concurrent checks each see the same prior total and
	// each be recorded — overspending the very threshold being enforced.
	EvaluateSpend(ctx context.Context, in domain.SpendEvaluation) (*domain.SpendDecision, error)
	ListConsumptions(ctx context.Context, legalEntityID, spendPolicyID string) ([]domain.SpendConsumption, error)
}

type Publisher interface {
	PublishThresholdBreached(ctx context.Context, correlationID string, check domain.SpendCheckRequest, policy domain.SpendPolicy, projected float64)
	PublishBlockApplied(ctx context.Context, correlationID string, check domain.SpendCheckRequest, policy domain.SpendPolicy)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

const (
	actionPolicyManage = "SPEND_POLICY_MANAGE"
	actionPolicyView   = "SPEND_POLICY_VIEW"
	actionCheckSubmit  = "SPEND_CHECK_SUBMIT"
)

var validPeriods = map[string]bool{"PER_TRANSACTION": true, "MONTHLY": true, "ANNUAL": true}

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/spend-policies", func(r chi.Router) {
		r.Post("/", h.CreatePolicy)
		r.Get("/", h.ListPolicies)
		// Withdrawing a limit was previously impossible: active_flag could only
		// ever be TRUE, so a category once governed stayed governed forever.
		r.Post("/{spend_policy_id}/deactivate", h.DeactivatePolicy)
		// Aggregated per-policy totals. Without this a caller wanting budget
		// meters had to fetch every consumption row and sum them itself.
		r.Get("/usage", h.PolicyUsage)
	})
	r.Route("/v1/spend-checks", func(r chi.Router) {
		r.Post("/", h.SubmitCheck)
	})
	r.Route("/v1/spend-consumptions", func(r chi.Router) {
		r.Get("/", h.ListConsumptions)
	})
}

// ── POST /v1/spend-policies ───────────────────────────────────────────────────

func (h *Handler) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	var req domain.CreatePolicyRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.LegalEntityID == "" || req.Category == "" || req.CurrencyCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, category, currency_code are required")
		return
	}
	if req.ThresholdAmount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_threshold", "threshold_amount must be > 0")
		return
	}
	if !validPeriods[req.Period] {
		writeError(w, http.StatusBadRequest, "invalid_period", "period must be PER_TRANSACTION, MONTHLY, or ANNUAL")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionPolicyManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	now := time.Now().UTC()
	policy := &domain.SpendPolicy{
		SpendPolicyID:        uuid.NewString(),
		TenantID:             svcmiddleware.TenantFromContext(r.Context()),
		LegalEntityID:        req.LegalEntityID,
		Category:             req.Category,
		Period:               req.Period,
		ThresholdAmount:      req.ThresholdAmount,
		CurrencyCode:         req.CurrencyCode,
		ActiveFlag:           true,
		CreatedByPrincipalID: principalID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	superseded, err := h.store.CreatePolicy(r.Context(), policy)
	if err != nil {
		h.writeStoreErr(w, "failed to create spend policy", err)
		return
	}

	writeJSON(w, http.StatusCreated, domain.CreatePolicyResponse{
		SpendPolicy: policy,
		Superseded:  superseded,
	})
}

// ── GET /v1/spend-policies ────────────────────────────────────────────────────

func (h *Handler) ListPolicies(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	category := r.URL.Query().Get("category")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// Checked unconditionally. This used to run only when legal_entity_id was
	// supplied, so omitting a single optional query parameter skipped
	// authorization altogether and returned every policy in the tenant to a
	// principal holding no view grant. Tenant isolation still held, so nothing
	// leaked across tenants — but a permission that a caller can opt out of by
	// leaving a filter blank is not a permission.
	if !h.authorizeScope(w, r, principalID, legalEntityID, actionPolicyView) {
		return
	}

	// Defaults to in-force only. `?active=false` widens it to the full history,
	// including superseded and withdrawn limits — useful for answering "what was
	// the limit in March", which is precisely what keeping the rows is for.
	activeOnly := r.URL.Query().Get("active") != "false"

	list, err := h.store.ListPolicies(r.Context(), legalEntityID, category, activeOnly)
	if err != nil {
		h.writeStoreErr(w, "failed to list spend policies", err)
		return
	}
	if list == nil {
		list = []domain.SpendPolicy{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/spend-policies/usage ──────────────────────────────────────────────

// PolicyUsage returns committed spend and refusal counts per active policy.
//
// Aggregated in the database over each policy's own enforcement window, so the
// figure a budget meter shows is the same one a spend check is judged against.
// Computing it client-side, as the console did, meant fetching the tenant's entire
// consumption history on every render AND applying no window at all — a MONTHLY
// limit's meter reported lifetime spend.
func (h *Handler) PolicyUsage(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	category := r.URL.Query().Get("category")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorizeScope(w, r, principalID, legalEntityID, actionPolicyView) {
		return
	}

	totals, err := h.store.PolicyUsageTotals(r.Context(), legalEntityID, category)
	if err != nil {
		h.writeStoreErr(w, "failed to aggregate spend usage", err)
		return
	}
	if totals == nil {
		totals = []domain.PolicyUsageTotal{}
	}
	writeJSON(w, http.StatusOK, totals)
}

// ── POST /v1/spend-policies/{spend_policy_id}/deactivate ──────────────────────

// DeactivatePolicy withdraws a limit so the category stops being governed.
//
// Gated on SPEND_POLICY_MANAGE, the same grant as setting one: removing a control
// is at least as consequential as adding it.
//
// The row is kept, not deleted. Every consumption recorded against this policy
// keeps its foreign key, and the history of what was once enforced stays readable
// through `?active=false`.
func (h *Handler) DeactivatePolicy(w http.ResponseWriter, r *http.Request) {
	spendPolicyID := chi.URLParam(r, "spend_policy_id")
	if spendPolicyID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "spend_policy_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// Resolved before the write so authorization is checked against the entity the
	// policy actually belongs to, not one the caller names.
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrTenantMissing))
		return
	}
	existing, err := h.store.ListPolicies(r.Context(), "", "", false)
	if err != nil {
		h.writeStoreErr(w, "failed to resolve spend policy", err)
		return
	}
	var target *domain.SpendPolicy
	for i := range existing {
		if existing[i].SpendPolicyID == spendPolicyID {
			target = &existing[i]
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "policy_not_found", "")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, target.LegalEntityID, actionPolicyManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.DeactivatePolicy(r.Context(), spendPolicyID); err != nil {
		if errors.Is(err, domain.ErrPolicyNotFound) {
			// Already withdrawn, or superseded by a later limit. Not an error —
			// the caller's intent (this limit is not in force) already holds.
			writeJSON(w, http.StatusOK, map[string]any{
				"spend_policy_id": spendPolicyID,
				"active_flag":     false,
				"withdrawn":       false,
				"detail":          "this limit was already not in force; nothing changed",
			})
			return
		}
		h.writeStoreErr(w, "failed to deactivate spend policy", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"spend_policy_id": spendPolicyID,
		"active_flag":     false,
		"withdrawn":       true,
		"detail":          "the limit is withdrawn; checks against this category are no longer evaluated against it",
	})
}

// ── GET /v1/spend-consumptions ────────────────────────────────────────────────

func (h *Handler) ListConsumptions(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	spendPolicyID := r.URL.Query().Get("spend_policy_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if !h.authorizeScope(w, r, principalID, legalEntityID, actionPolicyView) {
		return
	}

	list, err := h.store.ListConsumptions(r.Context(), legalEntityID, spendPolicyID)
	if err != nil {
		h.writeStoreErr(w, "failed to list spend consumptions", err)
		return
	}
	if list == nil {
		list = []domain.SpendConsumption{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/spend-checks ─────────────────────────────────────────────────────

// SubmitCheck evaluates whether a proposed spend would breach the caller's
// configured policy for this legal entity + category, and — only when
// allowed — records the consumption so later checks see it.
//
// Idempotent on (tenant_id, correlation_id): a retried check replays the
// stored decision rather than re-evaluating consumption, which would
// double-count the spend on a network retry. A BLOCKED decision is
// deliberately never recorded as consumption, so a rejected attempt cannot
// itself eat into the budget it was blocked from spending.
func (h *Handler) SubmitCheck(w http.ResponseWriter, r *http.Request) {
	var req domain.SpendCheckRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.LegalEntityID == "" || req.Category == "" || req.CurrencyCode == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, category, currency_code, correlation_id are required")
		return
	}
	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_amount", "amount must be > 0")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCheckSubmit); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// One call, one transaction, policy row locked. The decision and the record
	// it produces cannot be separated by a concurrent check.
	decision, err := h.store.EvaluateSpend(r.Context(), domain.SpendEvaluation{
		LegalEntityID:   req.LegalEntityID,
		Category:        req.Category,
		Amount:          req.Amount,
		CurrencyCode:    req.CurrencyCode,
		SourceReference: req.SourceReference,
		CorrelationID:   req.CorrelationID,
		PrincipalID:     principalID,
	})
	if err != nil {
		if errors.Is(err, domain.ErrCurrencyMismatch) {
			// 422 rather than 400: the request is well-formed, and the refusal is
			// a fact about the configured policy, not a malformed field.
			writeError(w, http.StatusUnprocessableEntity, "currency_mismatch",
				"this category's policy is set in a different currency, and nothing in this platform holds an FX rate, so the two amounts cannot be compared")
			return
		}
		h.writeStoreErr(w, "failed to evaluate spend check", err)
		return
	}

	// Events fire only for a fresh block. Re-publishing on a replayed decision
	// would tell every downstream consumer that a second breach occurred, when
	// the caller merely retried the first one.
	if decision.Outcome == "BLOCKED" && !decision.Replayed && decision.Policy != nil {
		correlationID := getCorrelationID(r)
		h.publisher.PublishThresholdBreached(r.Context(), correlationID, req, *decision.Policy, decision.ProjectedTotal)
		h.publisher.PublishBlockApplied(r.Context(), correlationID, req, *decision.Policy)
	}

	resp := domain.SpendCheckResponse{
		DecisionOutcome:  decision.Outcome,
		DecisionBasis:    decision.Basis,
		PriorConsumption: decision.PriorConsumption,
		ProjectedTotal:   decision.ProjectedTotal,
		ConsumptionID:    decision.ConsumptionID,
		Replayed:         decision.Replayed,
		CurrencyCode:     req.CurrencyCode,
	}
	// A replay used to answer with an outcome and nothing else — no threshold, no
	// prior total — so a retried check displayed a decision whose basis could not
	// be read. The figures come from the same policy either way.
	if decision.Policy != nil {
		resp.SpendPolicyID = decision.Policy.SpendPolicyID
		resp.ThresholdAmount = decision.Policy.ThresholdAmount
		resp.CurrencyCode = decision.Policy.CurrencyCode
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── Helpers ────────────────────────────────────────────────────────────────

// authorizeScope checks the caller against a legal entity, falling back to the
// tenant when no entity was named.
//
// The fallback is what makes the check unconditional. An unscoped list still has
// a scope — the tenant — and that is what the caller must hold the grant on.
// Returns false when it has already written the response.
func (h *Handler) authorizeScope(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, action string) bool {
	scope := legalEntityID
	if scope == "" {
		scope = svcmiddleware.TenantFromContext(r.Context())
	}
	if scope == "" {
		// No entity and no tenant: nothing to authorize against, so there is no
		// way to establish the caller may read anything. Fail closed.
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrTenantMissing))
		return false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, scope, action); err != nil {
		h.writeAuthzErr(w, err)
		return false
	}
	return true
}

// writeStoreErr keeps a missing tenant scope apart from a broken database.
//
// Every store method rejects a request with no tenant scope, and all of those
// rejections used to be reported as 503 `store_unavailable` — so a caller who
// simply had not sent X-Tenant-Id was told the service's database was down.
// The raw error text is no longer echoed either: it carried driver and query
// detail to the client for no one's benefit.
func (h *Handler) writeStoreErr(w http.ResponseWriter, msg string, err error) {
	if errors.Is(err, domain.ErrTenantMissing) {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrTenantMissing))
		return
	}
	h.log.Error(msg, zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
}

// maxRequestBytes caps a request body. A spend policy or check is a few hundred
// bytes; 64 KiB is already generous, and without a cap a single caller can
// stream an unbounded body into the decoder.
const maxRequestBytes = 64 << 10

// decodeJSON reads a JSON body strictly, and reports why it was refused.
//
// DisallowUnknownFields matters most for `source_reference`: it is optional, so a
// misspelling was silently discarded and the check succeeded with no reference to
// the thing that caused the spend — the one field that ties a consumption back to
// an order. Accepting a body and ignoring part of it is worse than rejecting it,
// because nothing downstream can tell the difference.
//
// Returns false when it has already written the response.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("body exceeds %d bytes", maxRequestBytes))
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			writeError(w, http.StatusBadRequest, "unknown_field", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		}
		return false
	}
	return true
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func getCorrelationID(r *http.Request) string {
	cid := r.Header.Get("X-Correlation-ID")
	if cid == "" {
		return uuid.NewString()
	}
	return cid
}

// writeError emits the platform's error shape: `error` for the machine code,
// `detail` for the human part.
//
// This service used to answer `{"error_code":…,"error_message":…}` — unique in
// the suite. Every other service uses `error`/`detail`, and the admin console
// parses exactly those keys (plus `field` and `message`), so every failure from
// this service arrived in the UI as a bare status code with no explanation
// whatsoever. Nothing consumed the old shape: no other backend service calls this
// one, and the console's only call was to a route that does not exist.
func writeError(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]string{"error": code}
	if detail != "" {
		body["detail"] = detail
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
