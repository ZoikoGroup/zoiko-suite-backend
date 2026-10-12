package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/metric-registry-svc/internal/authz"
	"zoiko.io/metric-registry-svc/internal/domain"
	"zoiko.io/metric-registry-svc/internal/events"
	"zoiko.io/metric-registry-svc/internal/outbox"
	"zoiko.io/metric-registry-svc/internal/store"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

const (
	ReportMetricCreate  = "REPORT_METRIC_CREATE"
	ReportMetricPublish = "REPORT_METRIC_PUBLISH"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthzChecker
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, platformScopeID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		}
		return false
	}
	return true
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/report-metrics", func(r chi.Router) {
		r.Post("/", h.CreateMetricDefinition)
		r.Get("/", h.ListActiveMetricDefinitions)
		r.Get("/{metric_code}", h.GetActiveMetricDefinition)
		r.Get("/{metric_code}/versions", h.ListMetricVersions)
		r.Post("/{metric_code}/versions", h.PublishNewVersion)
	})
}

func missingCreateField(req domain.CreateReportMetricRequest) string {
	switch {
	case req.MetricCode == "":
		return "metric_code"
	case req.MetricName == "":
		return "metric_name"
	case req.FormulaDescription == "":
		return "formula_description"
	case req.OwnerPrincipalID == "":
		return "owner_principal_id"
	case req.EffectiveFrom == "":
		return "effective_from"
	default:
		return ""
	}
}

// CreateMetricDefinition handles POST /v1/report-metrics — always creates
// version 1 of a new metric_code.
func (h *Handler) CreateMetricDefinition(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateReportMetricRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if missing := missingCreateField(req); missing != "" {
		writeError(w, http.StatusBadRequest, missing+" is required")
		return
	}

	// ── Idempotent replay (ZS-ARCH-SVC-001 v2.0 §4, INV-08) ─────────────────
	//
	// The envelope middleware already refuses a request with no
	// Idempotency-Key on a material write, but presence alone is not replay
	// protection: without this check, a retried registration (e.g. after a
	// client-side timeout on a POST that actually succeeded server-side)
	// depended entirely on metric_code's own unique index to reject it with
	// a 409 — the wrong outcome for a genuine replay, which INV-08 requires
	// to return the ORIGINAL 201 response instead.
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey != "" {
		existing, err := h.store.FindByIdempotencyKey(r.Context(), req.MetricCode, idempotencyKey)
		if err != nil {
			h.logger.Error("idempotency key lookup failed", zap.Error(err), zap.String("metric_code", req.MetricCode))
			writeError(w, http.StatusInternalServerError, "failed to check idempotency key")
			return
		}
		if existing != nil {
			writeJSON(w, http.StatusCreated, existing)
			return
		}
	}

	effectiveFrom, err := time.Parse(time.RFC3339, req.EffectiveFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "effective_from must be RFC3339")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, ReportMetricCreate) {
		return
	}

	d := &domain.ReportMetricDefinition{
		MetricDefinitionID:     uuid.NewString(),
		MetricCode:             req.MetricCode,
		MetricName:             req.MetricName,
		FormulaDescription:     req.FormulaDescription,
		DataSources:            req.DataSources,
		OwnerPrincipalID:       req.OwnerPrincipalID,
		IntelligenceDisclaimer: domain.DefaultIntelligenceDisclaimer,
		Version:                1,
		DefinitionStatus:       "ACTIVE",
		EffectiveFrom:          effectiveFrom,
		CreatedAt:              time.Now().UTC(),
		CreatedByPrincipalID:   principalID,
		IdempotencyKey:         idempotencyKey,
	}

	correlationID := req.CorrelationID
	if correlationID == "" {
		correlationID = r.Header.Get("X-Correlation-ID")
	}
	outboxEvt := outbox.Event{
		AggregateType: "ReportMetricDefinition",
		AggregateID:   d.MetricDefinitionID,
		EventType:     "report_metric.created",
		Payload: events.Event{
			EventID:       "evt-" + uuid.New().String(),
			EventType:     "report_metric.created",
			EventVersion:  "1.0",
			SchemaVersion: "1.0",
			SourceService: "metric-registry-svc",
			EntityID:      d.MetricDefinitionID,
			ActorID:       principalID,
			CorrelationID: correlationID,
			OccurredAt:    d.CreatedAt,
			Payload:       d,
		},
		CorrelationID: correlationID,
		// TenantID left nil: report metric definitions are platform-wide.
	}

	// outboxEvt is inserted atomically with the domain row (same
	// transaction) — this replaces the direct, error-swallowed
	// h.publisher.Publish call that used to run here: a Kafka outage no
	// longer loses the event, it is retried by the outbox relay.
	if err := h.store.CreateMetricDefinition(r.Context(), d, outboxEvt); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "metric_code already exists")
			return
		}
		h.logger.Error("create metric definition failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create metric definition")
		return
	}

	writeJSON(w, http.StatusCreated, d)
}

// GetActiveMetricDefinition handles GET /v1/report-metrics/{metric_code}.
//
// Identity-only, no further authz: this catalog is platform-wide reference
// data with no owning tenant, same posture as capability-registry-svc's and
// schema-registry-svc's reads — the bar is "the gateway verified who you
// are," not a granted permission. This read (and ListMetricVersions below)
// used to require neither, letting any caller on the internal network
// enumerate the full metric definition catalog — including
// owner_principal_id and formula_description — anonymously.
func (h *Handler) GetActiveMetricDefinition(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	d, err := h.store.GetActiveMetricDefinition(r.Context(), chi.URLParam(r, "metric_code"))
	if err != nil {
		if errors.Is(err, domain.ErrMetricNotFound) {
			writeError(w, http.StatusNotFound, "report metric not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get metric definition")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ListMetricVersions handles GET /v1/report-metrics/{metric_code}/versions.
// See GetActiveMetricDefinition's comment: identity-only, same as every
// other read in this handler.
func (h *Handler) ListMetricVersions(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	versions, err := h.store.ListMetricVersions(r.Context(), chi.URLParam(r, "metric_code"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list metric versions")
		return
	}
	if versions == nil {
		versions = []domain.ReportMetricDefinition{}
	}
	writeJSON(w, http.StatusOK, versions)
}

// PublishNewVersion handles POST /v1/report-metrics/{metric_code}/versions
// — publishes a new version, atomically superseding whatever was ACTIVE.
func (h *Handler) PublishNewVersion(w http.ResponseWriter, r *http.Request) {
	metricCode := chi.URLParam(r, "metric_code")

	// ── Idempotent replay (ZS-ARCH-SVC-001 v2.0 §4, INV-08) ─────────────────
	//
	// Checked before anything else, including decoding the body: unlike
	// CreateMetricDefinition (where metric_code's own unique index coincides
	// with a replay and naturally rejects a duplicate INSERT), the supersede
	// step below is NOT self-protecting — it unconditionally marks whatever
	// row is currently ACTIVE as SUPERSEDED. If a retried call were allowed
	// to reach that statement, it would supersede whatever a legitimate
	// LATER publish had since made ACTIVE, corrupting the version history
	// instead of safely no-opping. So a replay must be caught here, before
	// any read or write of report_metric_definitions, and must return the
	// ORIGINAL version row unconditionally.
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey != "" {
		existing, err := h.store.FindByIdempotencyKey(r.Context(), metricCode, idempotencyKey)
		if err != nil {
			h.logger.Error("idempotency key lookup failed", zap.Error(err), zap.String("metric_code", metricCode))
			writeError(w, http.StatusInternalServerError, "failed to check idempotency key")
			return
		}
		if existing != nil {
			writeJSON(w, http.StatusCreated, existing)
			return
		}
	}

	var req domain.PublishMetricVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.MetricName == "" || req.FormulaDescription == "" || req.OwnerPrincipalID == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "metric_name, formula_description, owner_principal_id, and effective_from are required")
		return
	}
	effectiveFrom, err := time.Parse(time.RFC3339, req.EffectiveFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "effective_from must be RFC3339")
		return
	}

	current, err := h.store.GetActiveMetricDefinition(r.Context(), metricCode)
	if err != nil {
		if errors.Is(err, domain.ErrMetricNotFound) {
			writeError(w, http.StatusNotFound, "report metric not found — create version 1 first via POST /v1/report-metrics")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch current metric definition")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, ReportMetricPublish) {
		return
	}

	next := &domain.ReportMetricDefinition{
		MetricDefinitionID:     uuid.NewString(),
		MetricCode:             metricCode,
		MetricName:             req.MetricName,
		FormulaDescription:     req.FormulaDescription,
		DataSources:            req.DataSources,
		OwnerPrincipalID:       req.OwnerPrincipalID,
		IntelligenceDisclaimer: domain.DefaultIntelligenceDisclaimer,
		Version:                current.Version + 1,
		DefinitionStatus:       "ACTIVE",
		EffectiveFrom:          effectiveFrom,
		CreatedAt:              time.Now().UTC(),
		CreatedByPrincipalID:   principalID,
		IdempotencyKey:         idempotencyKey,
	}

	correlationID := req.CorrelationID
	if correlationID == "" {
		correlationID = r.Header.Get("X-Correlation-ID")
	}
	outboxEvt := outbox.Event{
		AggregateType: "ReportMetricDefinition",
		AggregateID:   next.MetricDefinitionID,
		EventType:     "report_metric.version_published",
		Payload: events.Event{
			EventID:       "evt-" + uuid.New().String(),
			EventType:     "report_metric.version_published",
			EventVersion:  "1.0",
			SchemaVersion: "1.0",
			SourceService: "metric-registry-svc",
			EntityID:      next.MetricDefinitionID,
			ActorID:       principalID,
			CorrelationID: correlationID,
			OccurredAt:    next.CreatedAt,
			Payload:       next,
		},
		CorrelationID: correlationID,
	}

	// See CreateMetricDefinition's comment: outboxEvt is inserted atomically
	// with the supersede+insert transaction, replacing the direct publish
	// that used to run here unconditionally after commit.
	if err := h.store.PublishNewVersion(r.Context(), next, outboxEvt); err != nil {
		h.logger.Error("publish new metric version failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to publish new metric version")
		return
	}

	writeJSON(w, http.StatusCreated, next)
}

// ListActiveMetricDefinitions handles GET /v1/report-metrics — the catalog
// read that was missing entirely (MET-02): the router had no handler for a
// bare GET on this path, so it answered 405 Method Not Allowed and the
// frontend metric catalog rendered an empty table.
func (h *Handler) ListActiveMetricDefinitions(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	defs, err := h.store.ListActiveMetricDefinitions(r.Context(), limit, offset)
	if err != nil {
		h.logger.Error("list active metric definitions failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to list metric definitions")
		return
	}
	writeJSON(w, http.StatusOK, defs)
}

const (
	defaultLimit = 100
	maxLimit     = 500
)

// parsePaging bounds a register read — same discipline as this platform's
// other registries (schema-registry-svc, retention-registry-svc): an
// unbounded or malformed limit/offset must not reach the database.
func parsePaging(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset = defaultLimit, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and "+strconv.Itoa(maxLimit))
			return 0, 0, false
		}
		limit = n
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
