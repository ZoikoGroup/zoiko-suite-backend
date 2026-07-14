// Package handler exposes document-vault-svc's REST API.
package handler

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/residency"
	"zoiko.io/document-vault-svc/internal/storage"
)

type Store interface {
	CreateDocument(ctx context.Context, doc *domain.Document, firstVersion *domain.DocumentVersion) error
	AddVersion(ctx context.Context, documentID string, v *domain.DocumentVersion) (*domain.Document, error)
	FindDocumentByID(ctx context.Context, documentID string) (*domain.Document, error)
	FindVersion(ctx context.Context, documentID string, version int) (*domain.DocumentVersion, error)
	ListVersions(ctx context.Context, documentID string) ([]domain.DocumentVersion, error)
	RecordAccess(ctx context.Context, log *domain.DocumentAccessLog) error
	ListAccessLog(ctx context.Context, documentID string) ([]domain.DocumentAccessLog, error)
}

type Handler struct {
	store     Store
	storage   storage.Backend
	residency residency.Validator
	log       *zap.Logger
}

func New(store Store, storageBackend storage.Backend, residencyValidator residency.Validator, log *zap.Logger) *Handler {
	return &Handler{store: store, storage: storageBackend, residency: residencyValidator, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/documents", func(r chi.Router) {
		r.Post("/", h.CreateDocument)
		r.Get("/{documentID}", h.GetDocument)
		r.Get("/{documentID}/content", h.GetContent)
		r.Post("/{documentID}/versions", h.AddVersion)
		r.Get("/{documentID}/versions", h.ListVersions)
		r.Get("/{documentID}/access-log", h.ListAccessLog)
	})
}

// ── POST /v1/documents ───────────────────────────────────────────────────────

func (h *Handler) CreateDocument(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	actor := actorFromHeader(r)

	if missing := requiredFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	if !req.Classification.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_classification", string(req.Classification))
		return
	}

	content, err := base64.StdEncoding.DecodeString(req.ContentBase64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_content_base64", err.Error())
		return
	}
	if len(content) == 0 {
		writeError(w, http.StatusBadRequest, "empty_content", domain.ErrEmptyContent.Error())
		return
	}

	// Jurisdiction-aware residency check (§8.3) — only when the document
	// declares a region. Fail closed on mismatch or an unreachable registry.
	if req.ResidencyRegionCode != nil && *req.ResidencyRegionCode != "" {
		if err := h.residency.CheckRegion(r.Context(), req.TenantID, *req.ResidencyRegionCode); err != nil {
			h.handleResidencyError(w, err)
			return
		}
	}

	retention := req.RetentionPolicy
	if retention == "" {
		retention = "DEFAULT"
	}

	doc := &domain.Document{
		TenantID:             req.TenantID,
		LegalEntityID:        req.LegalEntityID,
		Title:                req.Title,
		Classification:       req.Classification,
		RetentionPolicy:      retention,
		ResidencyRegionCode:  req.ResidencyRegionCode,
		CreatedByPrincipalID: actor,
	}

	// storage_key is decided before the row exists, using a random
	// placeholder tied to the document only after creation would be circular
	// — so we generate the document ID client-side isn't an option (Postgres
	// assigns it). Instead: write bytes to storage AFTER the document row
	// exists, keyed by document_id+version, inside the same logical request
	// (not the same DB transaction — storage and Postgres are different
	// systems, so this is a two-phase write: DB row first with a temporary
	// key reservation would be needlessly complex for v1; instead we insert
	// metadata with the storage key already computed from a fresh UUID we
	// mint here, then write the blob under that key. If the process crashes
	// between the two, the row would reference a missing blob — an accepted
	// v1 gap, not swept under the rug: see docs/gtrm-style "known limitations"
	// pattern used elsewhere in this repo).
	tempKey := newStorageKey()
	checksum, err := h.storage.Put(r.Context(), tempKey, content)
	if err != nil {
		h.log.Error("CreateDocument: storage write failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "")
		return
	}

	firstVersion := &domain.DocumentVersion{
		ChecksumSHA256:       checksum,
		StorageKey:           tempKey,
		SizeBytes:            int64(len(content)),
		ContentType:          req.ContentType,
		CreatedByPrincipalID: actor,
	}

	if err := h.store.CreateDocument(r.Context(), doc, firstVersion); err != nil {
		h.log.Error("CreateDocument: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	writeJSON(w, http.StatusCreated, doc)
}

// ── GET /v1/documents/{documentID} ───────────────────────────────────────────

func (h *Handler) GetDocument(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "documentID")
	doc, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}

	h.recordAccess(r, documentID, nil, domain.AccessMetadata)
	writeJSON(w, http.StatusOK, doc)
}

// ── GET /v1/documents/{documentID}/content?version=N ────────────────────────

func (h *Handler) GetContent(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "documentID")
	doc, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}

	version := doc.CurrentVersion
	if q := r.URL.Query().Get("version"); q != "" {
		v, err := strconv.Atoi(q)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_version", q)
			return
		}
		version = v
	}

	v, err := h.store.FindVersion(r.Context(), documentID, version)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}

	content, err := h.storage.Get(r.Context(), v.StorageKey, v.ChecksumSHA256)
	if errors.Is(err, storage.ErrIntegrityFailure) {
		h.log.Error("GetContent: INTEGRITY FAILURE", zap.String("document_id", documentID), zap.Int("version", version))
		writeError(w, http.StatusConflict, "integrity_check_failed", domain.ErrChecksumMismatch.Error())
		return
	}
	if err != nil {
		h.log.Error("GetContent: storage unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "")
		return
	}

	h.recordAccess(r, documentID, &v.DocumentVersionID, domain.AccessDownload)

	w.Header().Set("Content-Type", v.ContentType)
	w.Header().Set("X-Checksum-SHA256", v.ChecksumSHA256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// ── POST /v1/documents/{documentID}/versions ─────────────────────────────────

func (h *Handler) AddVersion(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "documentID")
	actor := actorFromHeader(r)

	var req domain.CreateDocumentVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	content, err := base64.StdEncoding.DecodeString(req.ContentBase64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_content_base64", err.Error())
		return
	}
	if len(content) == 0 {
		writeError(w, http.StatusBadRequest, "empty_content", domain.ErrEmptyContent.Error())
		return
	}

	tempKey := newStorageKey()
	checksum, err := h.storage.Put(r.Context(), tempKey, content)
	if err != nil {
		h.log.Error("AddVersion: storage write failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "")
		return
	}

	v := &domain.DocumentVersion{
		ChecksumSHA256:       checksum,
		StorageKey:           tempKey,
		SizeBytes:            int64(len(content)),
		ContentType:          req.ContentType,
		CreatedByPrincipalID: actor,
	}

	doc, err := h.store.AddVersion(r.Context(), documentID, v)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, doc)
}

// ── GET /v1/documents/{documentID}/versions ──────────────────────────────────

func (h *Handler) ListVersions(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "documentID")
	versions, err := h.store.ListVersions(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, versions)
}

// ── GET /v1/documents/{documentID}/access-log ────────────────────────────────

func (h *Handler) ListAccessLog(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "documentID")
	logEntries, err := h.store.ListAccessLog(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, logEntries)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) recordAccess(r *http.Request, documentID string, versionID *string, accessType domain.AccessType) {
	corrID := r.Header.Get("X-Correlation-ID")
	var corrPtr *string
	if corrID != "" {
		corrPtr = &corrID
	}
	entry := &domain.DocumentAccessLog{
		DocumentID:            documentID,
		DocumentVersionID:     versionID,
		AccessedByPrincipalID: actorFromHeader(r),
		AccessType:            accessType,
		CorrelationID:         corrPtr,
	}
	if err := h.store.RecordAccess(r.Context(), entry); err != nil {
		// Access logging must never silently vanish — log loudly even though
		// we don't fail the read itself (the read already succeeded by the
		// time logging runs).
		h.log.Error("FAILED TO RECORD ACCESS LOG ENTRY", zap.String("document_id", documentID), zap.Error(err))
	}
}

func (h *Handler) handleStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrDocumentNotFound):
		writeError(w, http.StatusNotFound, "document_not_found", "")
	case errors.Is(err, domain.ErrDocumentVersionNotFound):
		writeError(w, http.StatusNotFound, "version_not_found", "")
	default:
		h.log.Error("store error", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

func (h *Handler) handleResidencyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, residency.ErrMismatch):
		writeError(w, http.StatusConflict, "residency_violation", domain.ErrResidencyViolation.Error())
	default:
		h.log.Error("residency check failed — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "residency_service_unavailable", "")
	}
}

func requiredFieldMissing(req domain.CreateDocumentRequest) string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.Title == "":
		return "title"
	case req.Classification == "":
		return "classification"
	case req.ContentType == "":
		return "content_type"
	case req.ContentBase64 == "":
		return "content_base64"
	default:
		return ""
	}
}

func actorFromHeader(r *http.Request) string {
	if a := r.Header.Get("X-Actor-Principal-ID"); a != "" {
		return a
	}
	// Prefer the identity gateway propagates once fronted by gateway-auth-svc.
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

// newStorageKey mints a random storage key for a new blob. Storage keys are
// never derived from user input — they're an internal detail the store layer
// records in document_versions.storage_key.
func newStorageKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
