package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/data-ingestion-svc/internal/domain"
	customMiddleware "zoiko.io/data-ingestion-svc/internal/middleware"
)

// ZS-CONTROL-001 §9 control population endpoint — see
// docs/architecture/control-population-contract.md ("data-ingestion-svc — import-batch-tieout").

const (
	// ActionImportControlPopulationRead is the action the verified human caller needs, for the entity, to read this
	// population.
	ActionImportControlPopulationRead = "DATA_INGESTION_CONTROL_POPULATION_READ"

	maxPopulationLimit = 5000
)

var (
	tieoutLegalEntityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,254}$`)
	tieoutBatchPattern       = regexp.MustCompile(`^imb_[0-9a-f-]{36}$`)
	importBatchTieoutParams  = map[string]bool{"legal_entity_id": true, "batch_id": true, "limit": true, "cursor": true}
)

type controlPopulationResponse struct {
	Records        []domain.ControlPopulationRecord `json:"records"`
	NextCursor     string                           `json:"next_cursor"`
	Watermark      string                           `json:"watermark"`
	DeclaredTotals domain.ControlDeclaredTotals     `json:"declared_totals"`
}

// GetControlPopulation serves GET /v1/control-populations/{population}; only import-batch-tieout exists. It is a
// lifetime, single-batch population: `period_id` and any other or repeated parameter are refused (400).
func (h *Handler) GetControlPopulation(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "population") != domain.PopulationImportBatchTieout {
		h.respondError(w, http.StatusNotFound, "unknown control population")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID := customMiddleware.GetTenantID(r.Context())
	if tenantID == "" || len(tenantID) > 64 {
		h.respondError(w, http.StatusUnauthorized, "a valid X-Tenant-Id header is required")
		return
	}

	q := r.URL.Query()
	for k, vs := range q {
		if !importBatchTieoutParams[k] {
			h.respondError(w, http.StatusBadRequest, "unknown parameter: "+k)
			return
		}
		if len(vs) != 1 {
			h.respondError(w, http.StatusBadRequest, k+" must be given at most once")
			return
		}
	}

	entity := q.Get("legal_entity_id")
	if !tieoutLegalEntityPattern.MatchString(entity) {
		h.respondError(w, http.StatusBadRequest, "legal_entity_id is required (1-255 characters: letters, digits, _ . : -)")
		return
	}
	batchID := q.Get("batch_id")
	if !tieoutBatchPattern.MatchString(batchID) {
		h.respondError(w, http.StatusBadRequest, "batch_id is required (imb_<uuid>)")
		return
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n <= 0 || n > maxPopulationLimit {
			h.respondError(w, http.StatusBadRequest, "limit must be an integer between 1 and "+strconv.Itoa(maxPopulationLimit))
			return
		}
	}
	after := ""
	if q.Has("cursor") {
		b, err := base64.RawURLEncoding.DecodeString(q.Get("cursor"))
		if err != nil || len(b) == 0 || len(b) > 128 || !utf8.Valid(b) {
			h.respondError(w, http.StatusBadRequest, "cursor is not valid")
			return
		}
		after = string(b)
	}

	if !h.authorize(w, r, principalID, entity, ActionImportControlPopulationRead) {
		return
	}

	page, err := h.store.QueryImportBatchTieout(r.Context(), tenantID, domain.ImportBatchTieoutQuery{
		LegalEntityID: entity, BatchID: batchID, AfterRecordID: after})
	if err != nil {
		if errors.Is(err, domain.ErrImportBatchNotFound) {
			h.respondError(w, http.StatusNotFound, "import batch not found")
			return
		}
		h.logger.Error("GetControlPopulation: store unavailable", zap.Error(err))
		h.respondError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	resp := controlPopulationResponse{Records: page.Records, Watermark: page.Watermark, DeclaredTotals: page.DeclaredTotals}
	if resp.Records == nil {
		resp.Records = []domain.ControlPopulationRecord{}
	}
	if resp.DeclaredTotals.Totals == nil {
		resp.DeclaredTotals.Totals = map[string]string{}
	}
	if page.NextRecordID != "" {
		resp.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.NextRecordID))
	}
	h.respondJSON(w, http.StatusOK, resp)
}
