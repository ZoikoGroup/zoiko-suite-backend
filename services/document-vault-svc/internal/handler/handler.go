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

	"zoiko.io/document-vault-svc/internal/authz"
	"zoiko.io/document-vault-svc/internal/domain"
	svcmiddleware "zoiko.io/document-vault-svc/internal/middleware"
	"zoiko.io/document-vault-svc/internal/residency"
	"zoiko.io/document-vault-svc/internal/storage"
)

type Store interface {
	CreateDocument(ctx context.Context, doc *domain.Document, firstVersion *domain.DocumentVersion) error
	AddVersion(ctx context.Context, documentID string, v *domain.DocumentVersion) (*domain.Document, error)
	FindDocumentByID(ctx context.Context, documentID string) (*domain.Document, error)
	FindVersion(ctx context.Context, documentID string, version int) (*domain.DocumentVersion, error)
	ListVersions(ctx context.Context, documentID string) ([]domain.DocumentVersion, error)
	ListDocuments(ctx context.Context, legalEntityID string, limit, offset int) ([]domain.Document, error)
	RecordAccess(ctx context.Context, log *domain.DocumentAccessLog) error
	ListAccessLog(ctx context.Context, documentID string, limit, offset int) ([]domain.DocumentAccessLog, error)
}

type Handler struct {
	store     Store
	storage   storage.Backend
	residency residency.Validator
	authz     authz.Client
	log       *zap.Logger
}

func New(store Store, storageBackend storage.Backend, residencyValidator residency.Validator, authzClient authz.Client, log *zap.Logger) *Handler {
	return &Handler{store: store, storage: storageBackend, residency: residencyValidator, authz: authzClient, log: log}
}

// maxBodyBytes caps a request body.
//
// This service takes base64 content inline, so an unbounded body is read whole
// into memory and then decoded into a second copy before anything validates
// it. 12 MiB of base64 is roughly 9 MiB of document.
const maxBodyBytes = 12 << 20

const (
	defaultPageLimit = 100
	maxPageLimit     = 500
)

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/documents", func(r chi.Router) {
		r.Post("/", h.CreateDocument)
		// The register. There was no list route at all, so every one of the
		// routes below needed a document_id the caller already had — the vault
		// could be written to and read from, but never browsed.
		r.Get("/", h.ListDocuments)
		r.Get("/{documentID}", h.GetDocument)
		r.Get("/{documentID}/content", h.GetContent)
		r.Post("/{documentID}/versions", h.AddVersion)
		r.Get("/{documentID}/versions", h.ListVersions)
		r.Get("/{documentID}/access-log", h.ListAccessLog)
	})
}

// ── POST /v1/documents ───────────────────────────────────────────────────────

func (h *Handler) CreateDocument(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req domain.CreateDocumentRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// tenant_id in the body is no longer what scopes the write — the header is,
	// because that is the value the gateway verified. A body naming a different
	// tenant is refused rather than ignored: a caller that believed it was
	// filing into another tenant should be told it cannot, not quietly have the
	// document land somewhere else.
	if req.TenantID != "" && req.TenantID != tenantID {
		writeError(w, http.StatusBadRequest, "tenant_mismatch",
			"tenant_id in the body does not match the request's tenant")
		return
	}
	req.TenantID = tenantID

	if missing := requiredFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	if !req.Classification.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_classification", string(req.Classification))
		return
	}
	if !h.authorize(w, r, actor, req.LegalEntityID, authz.ActionDocumentCreate) {
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
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	documentID := chi.URLParam(r, "documentID")
	doc, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	// Authorized against the document's OWN legal entity, which is why the
	// lookup comes first. The document is already tenant-scoped by the store,
	// so this cannot be used to probe another tenant's ids.
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentRead) {
		return
	}

	h.recordAccess(r, actor, documentID, nil, domain.AccessMetadata)
	writeJSON(w, http.StatusOK, doc)
}

// ── GET /v1/documents ────────────────────────────────────────────────────────

// ListDocuments is the tenant's register for one legal entity.
//
// legal_entity_id is required rather than optional. This service authorizes
// per legal entity, so a register spanning every entity in the tenant would
// have no single scope to authorize against — and defaulting to "all entities
// the tenant owns" is how the unscoped reads elsewhere in this platform came
// about.
func (h *Handler) ListDocuments(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_field",
			"legal_entity_id is required — documents are authorized per legal entity")
		return
	}
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, actor, legalEntityID, authz.ActionDocumentRead) {
		return
	}

	docs, err := h.store.ListDocuments(r.Context(), legalEntityID, limit, offset)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if docs == nil {
		docs = []domain.Document{}
	}
	writeJSON(w, http.StatusOK, docs)
}

// ── GET /v1/documents/{documentID}/content?version=N ────────────────────────

func (h *Handler) GetContent(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	documentID := chi.URLParam(r, "documentID")
	doc, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	// DOWNLOAD, not READ. Knowing a document exists and reading its bytes are
	// different disclosures — the access log has recorded them as different
	// access types since day one, and authorization now agrees.
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentDownload) {
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

	h.recordAccess(r, actor, documentID, &v.DocumentVersionID, domain.AccessDownload)

	w.Header().Set("Content-Type", v.ContentType)
	w.Header().Set("X-Checksum-SHA256", v.ChecksumSHA256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// ── POST /v1/documents/{documentID}/versions ─────────────────────────────────

func (h *Handler) AddVersion(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	documentID := chi.URLParam(r, "documentID")

	// The document is read before the body so the new version can be authorized
	// against the entity that owns it. A version is an amendment to an existing
	// governed record, so the grant that matters is the one on that record.
	existing, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, actor, existing.LegalEntityID, authz.ActionDocumentVersionCreate) {
		return
	}

	var req domain.CreateDocumentVersionRequest
	if !decodeJSON(w, r, &req) {
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
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	documentID := chi.URLParam(r, "documentID")
	doc, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentRead) {
		return
	}
	versions, err := h.store.ListVersions(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if versions == nil {
		versions = []domain.DocumentVersion{}
	}
	writeJSON(w, http.StatusOK, versions)
}

// ── GET /v1/documents/{documentID}/access-log ────────────────────────────────

func (h *Handler) ListAccessLog(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	documentID := chi.URLParam(r, "documentID")
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	doc, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	// A separate grant from reading the document. The log says who read what
	// and when; on a governed vault that is the record an investigator
	// consults, and it should not fall out of ordinary read access.
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentAccessLogRead) {
		return
	}
	logEntries, err := h.store.ListAccessLog(r.Context(), documentID, limit, offset)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if logEntries == nil {
		logEntries = []domain.DocumentAccessLog{}
	}
	writeJSON(w, http.StatusOK, logEntries)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// recordAccess takes the actor as a parameter rather than re-deriving it from
// headers. It used to call actorFromHeader, which preferred a forgeable
// X-Actor-Principal-ID and fell back to the literal "unknown" — so the
// append-only record of who read a RESTRICTED document could be attributed to
// anyone, or to nobody.
func (h *Handler) recordAccess(r *http.Request, actor, documentID string, versionID *string, accessType domain.AccessType) {
	corrID := r.Header.Get("X-Correlation-ID")
	var corrPtr *string
	if corrID != "" {
		corrPtr = &corrID
	}
	entry := &domain.DocumentAccessLog{
		DocumentID:            documentID,
		DocumentVersionID:     versionID,
		AccessedByPrincipalID: actor,
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
	case errors.Is(err, domain.ErrTenantMissing):
		writeError(w, http.StatusUnauthorized, "tenant_missing", err.Error())
	case errors.Is(err, domain.ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, "identity_missing", err.Error())
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
	// tenant_id is not checked here any more: it is taken from the verified
	// X-Tenant-Id header before this runs, so it can never be empty by the time
	// we reach this point.
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

// requirePrincipal returns the gateway-verified principal, or refuses.
//
// This replaces actorFromHeader, which was three bugs in nine lines. It read
// X-Actor-Principal-ID FIRST — a header nothing in this platform sets and
// anything may send, taking precedence over the one the gateway verifies — so a
// caller could attribute their own download to a colleague. Failing both, it
// returned the literal string "unknown", so an unidentified caller was not
// refused but RECORDED, and the append-only log that exists to answer "who
// downloaded this" could answer "unknown" and read as though it had answered.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", domain.ErrIdentityMissing.Error())
		return "", false
	}
	return principalID, true
}

// requireTenant refuses a request carrying no X-Tenant-Id. Without it the
// store's old predicate widened to every tenant rather than refusing.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_missing", domain.ErrTenantMissing.Error())
		return "", false
	}
	return tenantID, true
}

// authorize asks authorization-svc and writes the refusal itself. Returns true
// only on an explicit GRANTED.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, action string) bool {
	err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, action)
	if err == nil {
		return true
	}
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", domain.ErrAuthorizationDenied.Error())
		return false
	}
	h.log.Error("authorization check failed — failing closed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "authz_unavailable", domain.ErrAuthzServiceUnavailable.Error())
	return false
}

// decodeJSON caps the body and refuses unknown fields. A misspelled field used
// to be discarded in silence — on a create that means a document stored with a
// classification the caller believed they had set.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func parsePaging(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset = defaultPageLimit, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageLimit {
			writeError(w, http.StatusBadRequest, "invalid_paging", domain.ErrInvalidPaging.Error())
			return 0, 0, false
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_paging", domain.ErrInvalidPaging.Error())
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
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
