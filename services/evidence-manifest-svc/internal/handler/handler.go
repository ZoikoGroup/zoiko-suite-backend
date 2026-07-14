// Package handler exposes evidence-manifest-svc's REST API.
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/aggregator"
	"zoiko.io/evidence-manifest-svc/internal/domain"
)

type Store interface {
	CreateManifest(ctx context.Context, m *domain.EvidenceManifest) error
	AddRecord(ctx context.Context, r *domain.ManifestRecord) error
	FinalizeGenerated(ctx context.Context, manifestID, checksumSHA256 string) (*domain.EvidenceManifest, error)
	FinalizeFailed(ctx context.Context, manifestID, reason string) (*domain.EvidenceManifest, error)
	FindManifestByID(ctx context.Context, manifestID string) (*domain.EvidenceManifest, error)
	ListRecords(ctx context.Context, manifestID string) ([]domain.ManifestRecord, error)
}

type Publisher interface {
	PublishManifestGenerated(ctx context.Context, m *domain.EvidenceManifest) error
}

// GovernanceSource, AccessSource, WorkflowSource are the narrow interfaces the
// handler depends on — satisfied by aggregator's real HTTP clients, and
// stubbable in tests.
type GovernanceSource interface {
	ListByEntityAndDateRange(ctx context.Context, legalEntityID string, from, to *time.Time) ([]aggregator.SourceRecord, error)
	GetByID(ctx context.Context, decisionID string) (*aggregator.SourceRecord, error)
}

type AccessSource interface {
	GetByID(ctx context.Context, accessDecisionID string) (*aggregator.SourceRecord, error)
}

type WorkflowSource interface {
	GetByID(ctx context.Context, workflowInstanceID string) (*aggregator.SourceRecord, error)
}

type Handler struct {
	store      Store
	governance GovernanceSource
	access     AccessSource
	workflow   WorkflowSource
	publisher  Publisher
	log        *zap.Logger
}

func New(store Store, governance GovernanceSource, access AccessSource,
	workflow WorkflowSource, publisher Publisher, log *zap.Logger) *Handler {
	return &Handler{store: store, governance: governance, access: access, workflow: workflow, publisher: publisher, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/evidence-manifests", func(r chi.Router) {
		r.Post("/", h.GenerateManifest)
		r.Get("/{manifestID}", h.GetManifest)
		r.Get("/{manifestID}/records", h.ListRecords)
	})
}

// ── POST /v1/evidence-manifests ──────────────────────────────────────────────

func (h *Handler) GenerateManifest(w http.ResponseWriter, r *http.Request) {
	var req domain.GenerateManifestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if missing := requiredFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	if !req.ScenarioType.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_scenario_type", string(req.ScenarioType))
		return
	}
	if req.GovernanceDecisionsFrom == nil && req.GovernanceDecisionsTo == nil &&
		len(req.GovernanceDecisionIDs) == 0 && len(req.AccessDecisionIDs) == 0 && len(req.WorkflowInstanceIDs) == 0 {
		writeError(w, http.StatusBadRequest, "no_records_requested", domain.ErrNoRecordsRequested.Error())
		return
	}

	requestedBy := req.RequestedBy
	if requestedBy == "" {
		requestedBy = actorFromHeader(r)
	}

	manifest := &domain.EvidenceManifest{
		TenantID: req.TenantID, LegalEntityID: req.LegalEntityID,
		ScenarioType: req.ScenarioType, RequestedBy: requestedBy,
	}
	if err := h.store.CreateManifest(r.Context(), manifest); err != nil {
		h.log.Error("GenerateManifest: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	records, err := h.collectRecords(r.Context(), req)
	if err != nil {
		// Fail closed: a manifest that can't fully assemble is FAILED, not
		// silently partial — a partial manifest that LOOKS complete is worse
		// than no manifest at all for audit/legal-discovery use.
		reason := err.Error()
		if _, ferr := h.store.FinalizeFailed(r.Context(), manifest.ManifestID, reason); ferr != nil {
			h.log.Error("GenerateManifest: failed to record FAILED status", zap.Error(ferr))
		}
		h.log.Error("GenerateManifest: source aggregation failed — manifest marked FAILED",
			zap.String("manifest_id", manifest.ManifestID), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "source_service_unavailable", domain.ErrSourceServiceUnavailable.Error())
		return
	}

	hasher := sha256.New()
	for _, rec := range records {
		mr := &domain.ManifestRecord{
			ManifestID: manifest.ManifestID, SourceType: rec.SourceType,
			SourceRecordID: rec.SourceRecordID, RecordSnapshot: rec.RawJSON,
		}
		if err := h.store.AddRecord(r.Context(), mr); err != nil {
			h.log.Error("GenerateManifest: failed to persist record", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
			return
		}
		hasher.Write([]byte(rec.SourceType))
		hasher.Write([]byte(rec.SourceRecordID))
		hasher.Write(rec.RawJSON)
	}
	checksum := hex.EncodeToString(hasher.Sum(nil))

	finalManifest, err := h.store.FinalizeGenerated(r.Context(), manifest.ManifestID, checksum)
	if err != nil {
		h.log.Error("GenerateManifest: failed to finalize", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	if pubErr := h.publisher.PublishManifestGenerated(r.Context(), finalManifest); pubErr != nil {
		h.log.Error("GenerateManifest: failed to publish evidence.manifest.generated",
			zap.String("manifest_id", finalManifest.ManifestID), zap.Error(pubErr))
	}

	writeJSON(w, http.StatusCreated, finalManifest)
}

// collectRecords fetches every requested source record. Fails closed on the
// first source error — a manifest is all-or-nothing.
func (h *Handler) collectRecords(ctx context.Context, req domain.GenerateManifestRequest) ([]aggregator.SourceRecord, error) {
	var out []aggregator.SourceRecord

	if req.GovernanceDecisionsFrom != nil || req.GovernanceDecisionsTo != nil {
		recs, err := h.governance.ListByEntityAndDateRange(ctx, req.LegalEntityID, req.GovernanceDecisionsFrom, req.GovernanceDecisionsTo)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	for _, id := range req.GovernanceDecisionIDs {
		rec, err := h.governance.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	for _, id := range req.AccessDecisionIDs {
		rec, err := h.access.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	for _, id := range req.WorkflowInstanceIDs {
		rec, err := h.workflow.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, nil
}

// ── GET /v1/evidence-manifests/{manifestID} ──────────────────────────────────

func (h *Handler) GetManifest(w http.ResponseWriter, r *http.Request) {
	manifestID := chi.URLParam(r, "manifestID")
	m, err := h.store.FindManifestByID(r.Context(), manifestID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ── GET /v1/evidence-manifests/{manifestID}/records ──────────────────────────

func (h *Handler) ListRecords(w http.ResponseWriter, r *http.Request) {
	manifestID := chi.URLParam(r, "manifestID")
	if _, err := h.store.FindManifestByID(r.Context(), manifestID); err != nil {
		h.handleStoreError(w, err)
		return
	}
	records, err := h.store.ListRecords(r.Context(), manifestID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, records)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) handleStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrManifestNotFound):
		writeError(w, http.StatusNotFound, "manifest_not_found", "")
	default:
		h.log.Error("store error", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

func requiredFieldMissing(req domain.GenerateManifestRequest) string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.ScenarioType == "":
		return "scenario_type"
	default:
		return ""
	}
}

func actorFromHeader(r *http.Request) string {
	if a := r.Header.Get("X-Actor-Principal-ID"); a != "" {
		return a
	}
	if a := r.Header.Get("X-Principal-Id"); a != "" {
		return a
	}
	return "unknown"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type errorResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorResponse{Error: code, Detail: detail})
}
