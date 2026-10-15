// Package handler is financial-statements-svc's HTTP layer: request parsing,
// response framing, and mapping service.Service's typed errors to HTTP
// status codes. No business logic and no SQL lives here.
package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-statements-svc/internal/domain"
	svcenvelope "zoiko.io/financial-statements-svc/internal/envelope"
	svcmiddleware "zoiko.io/financial-statements-svc/internal/middleware"
	"zoiko.io/financial-statements-svc/internal/service"
)

type Handler struct {
	svc *service.Service
	log *zap.Logger
}

func New(svc *service.Service, log *zap.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/statement-sets", func(r chi.Router) {
		r.Post("/", h.CreateStatementSet)
		r.Get("/", h.ListStatementSets)
		r.Get("/{id}", h.GetStatementSet)
		r.Get("/{id}/facts", h.GetStatementFacts)
		r.Get("/{id}/comparatives", h.GetComparatives)
		r.Get("/{id}/disclosures", h.GetDisclosures)
		// Colon-suffixed command segment, one route, dispatched by name —
		// same convention as accounting-period-svc's {idcmd} (its own
		// router has no clean way to combine a path param with a literal
		// suffix on one chi route per command).
		r.Post("/{idcmd}", h.StatementSetCommand)
	})
	r.Get("/v1/statement-facts/{id}/explain", h.ExplainReportedFact)
}

// ── create / read ────────────────────────────────────────────────────────

func (h *Handler) CreateStatementSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LegalEntityID        string `json:"legal_entity_id"`
		ReportingPeriod      string `json:"reporting_period"`
		Framework            string `json:"framework"`
		PresentationCurrency string `json:"presentation_currency"`
		SourceSnapshotID     string `json:"source_snapshot_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	set, err := h.svc.CreateStatementSet(r.Context(), tenantID, principalID, correlationID(r), service.CreateSetRequest{
		LegalEntityID:        req.LegalEntityID,
		ReportingPeriod:      req.ReportingPeriod,
		Framework:            req.Framework,
		PresentationCurrency: req.PresentationCurrency,
		SourceSnapshotID:     req.SourceSnapshotID,
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, set)
}

func (h *Handler) GetStatementSet(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	set, err := h.svc.GetStatementSet(r.Context(), tenantID, principalID, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

func (h *Handler) ListStatementSets(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	sets, err := h.svc.ListStatementSets(r.Context(), tenantID, principalID, q.Get("legal_entity_id"), q.Get("status"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if sets == nil {
		sets = []domain.FinancialStatementSet{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": sets, "count": len(sets)})
}

func (h *Handler) GetStatementFacts(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	facts, err := h.svc.GetStatementFacts(r.Context(), tenantID, principalID, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if facts == nil {
		facts = []domain.StatementFact{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": facts, "count": len(facts)})
}

// GetComparatives: GET /v1/statement-sets/{id}/comparatives?prior_set_id=…
// The prior set is named explicitly rather than resolved from a period:
// reporting_period is a free-form label and this service has no fiscal
// calendar to say which period is "immediately before" another.
func (h *Handler) GetComparatives(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	view, err := h.svc.GetComparatives(r.Context(), tenantID, principalID, chi.URLParam(r, "id"), r.URL.Query().Get("prior_set_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// GetDisclosures: GET /v1/statement-sets/{id}/disclosures (the spec's
// GetDisclosure), every note on the set in sequence order.
func (h *Handler) GetDisclosures(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	disclosures, err := h.svc.GetDisclosures(r.Context(), tenantID, principalID, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if disclosures == nil {
		disclosures = []domain.DisclosureInstance{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": disclosures, "count": len(disclosures)})
}

func (h *Handler) ExplainReportedFact(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	explained, err := h.svc.ExplainReportedFact(r.Context(), tenantID, principalID, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, explained)
}

// ── commands ─────────────────────────────────────────────────────────────

func splitCommand(seg string) (id, cmd string, ok bool) {
	i := strings.LastIndex(seg, ":")
	if i <= 0 || i == len(seg)-1 {
		return "", "", false
	}
	return seg[:i], seg[i+1:], true
}

// StatementSetCommand dispatches every colon-suffixed command on one
// existing set: :refresh-draft, :validate, :submit-for-review,
// :request-changes, :approve, :certify, :publish, :supersede,
// :add-disclosure.
func (h *Handler) StatementSetCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd, ok := splitCommand(chi.URLParam(r, "idcmd"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_command",
			"expected {id}:refresh-draft, :validate, :submit-for-review, :request-changes, :approve, :certify, :publish or :supersede")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	corr := correlationID(r)

	switch cmd {
	case "refresh-draft":
		set, err := h.svc.RefreshDraft(r.Context(), tenantID, principalID, corr, id)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, set)

	case "validate":
		res, err := h.svc.ValidateStatements(r.Context(), tenantID, principalID, corr, id)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)

	case "submit-for-review":
		if err := h.svc.SubmitForReview(r.Context(), tenantID, principalID, corr, id); err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": string(domain.StateInReview)})

	case "request-changes":
		if err := h.svc.RequestChanges(r.Context(), tenantID, principalID, corr, id); err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": string(domain.StateDraft)})

	case "approve":
		if err := h.svc.Approve(r.Context(), tenantID, principalID, corr, id); err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": string(domain.StateApproved)})

	case "certify":
		var body struct {
			Reason string `json:"reason"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.Reason == "" {
			writeError(w, http.StatusBadRequest, "missing_field", "reason is required")
			return
		}
		if err := h.svc.Certify(r.Context(), tenantID, principalID, corr, id, body.Reason); err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": string(domain.StateCertified)})

	case "publish":
		if err := h.svc.Publish(r.Context(), tenantID, principalID, corr, id); err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": string(domain.StatePublished)})

	case "supersede":
		var body struct {
			NewSourceSnapshotID string `json:"new_source_snapshot_id"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.NewSourceSnapshotID == "" {
			writeError(w, http.StatusBadRequest, "missing_field", "new_source_snapshot_id is required")
			return
		}
		newSet, err := h.svc.SupersedeStatementSet(r.Context(), tenantID, principalID, corr, id, body.NewSourceSnapshotID)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, newSet)

	case "add-disclosure":
		var body struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		d, replayed, err := h.svc.AddDisclosure(r.Context(), tenantID, principalID, corr, idempotencyKey(r), id, body.Title, body.Body)
		if err != nil {
			h.fail(w, err)
			return
		}
		status := http.StatusCreated
		if replayed {
			status = http.StatusOK
		}
		writeJSON(w, status, d)

	default:
		writeError(w, http.StatusNotFound, "unknown_command",
			"expected {id}:refresh-draft, :validate, :submit-for-review, :request-changes, :approve, :certify, :publish, :supersede or :add-disclosure")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_missing", "X-Tenant-Id header is required")
		return "", false
	}
	return tenantID, true
}

// idempotencyKey reads the request's Idempotency-Key the same way
// correlationID reads its field: the validated envelope first, then the raw
// header. Empty means the caller sent none (the envelope middleware requires
// one on writes in production).
func idempotencyKey(r *http.Request) string {
	if env, ok := svcenvelope.FromContext(r.Context()); ok && env.IdempotencyKey != "" {
		return env.IdempotencyKey
	}
	return strings.TrimSpace(r.Header.Get(svcenvelope.HeaderIdempotencyKey))
}

func correlationID(r *http.Request) string {
	if env, ok := svcenvelope.FromContext(r.Context()); ok && env.CorrelationID != "" {
		return env.CorrelationID
	}
	if cid := r.Header.Get("X-Correlation-ID"); cid != "" {
		return cid
	}
	return uuid.NewString()
}

// statusFor maps a typed service error to its HTTP status.
func statusFor(code domain.Code) int {
	switch code {
	case domain.CodeContextInvalid, domain.CodeSourceUnverified, domain.CodeValidationFailed:
		return http.StatusUnprocessableEntity
	case domain.CodeVersionConflict, domain.CodeInvalidTransition:
		return http.StatusConflict
	case domain.CodeSoDDenied, domain.CodeForbidden:
		return http.StatusForbidden
	case domain.CodeNotFound:
		return http.StatusNotFound
	case domain.CodeDependencyUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// fail renders a service error. A typed domain.Error keeps its code; a
// bare error (e.g. ErrStatementSetNotFound, a store failure) never leaks
// its text to the client.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	if de, ok := domain.AsError(err); ok {
		writeError(w, statusFor(de.Code), string(de.Code), de.Message)
		return
	}
	switch err {
	case domain.ErrStatementSetNotFound:
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case domain.ErrFactNotFound:
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		h.log.Error("unhandled service error", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error_code": code, "error_message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// correlationIDMiddleware propagates X-Correlation-ID through every request.
func CorrelationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Correlation-ID") == "" {
			r.Header.Set("X-Correlation-ID", middleware.GetReqID(r.Context()))
		}
		w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
		next.ServeHTTP(w, r)
	})
}
