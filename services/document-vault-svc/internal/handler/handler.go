// Package handler exposes document-vault-svc's REST API.
package handler

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/authz"
	"zoiko.io/document-vault-svc/internal/domain"
	svcmiddleware "zoiko.io/document-vault-svc/internal/middleware"
	"zoiko.io/document-vault-svc/internal/residency"
	"zoiko.io/document-vault-svc/internal/scan"
	"zoiko.io/document-vault-svc/internal/storage"
)

type Store interface {
	CreateDocument(ctx context.Context, doc *domain.Document, firstVersion *domain.DocumentVersion, correlationID string) error
	AddVersion(ctx context.Context, documentID string, v *domain.DocumentVersion, correlationID string) (*domain.Document, error)
	DeclareRecord(ctx context.Context, p domain.DeclareRecordParams, correlationID string) (*domain.Document, error)
	SupersedeDocument(ctx context.Context, p domain.SupersedeDocumentParams, correlationID string) (*domain.Document, error)
	MoveToArchive(ctx context.Context, p domain.MoveToArchiveParams, correlationID string) (*domain.Document, error)
	RequestDisposition(ctx context.Context, p domain.RequestDispositionParams, correlationID string) (*domain.Document, error)
	GetAsOfDocument(ctx context.Context, documentID string, asOf time.Time) (*domain.Document, *domain.DocumentVersion, error)
	LinkDocument(ctx context.Context, p domain.LinkDocumentParams) (*domain.DocumentLink, error)
	ListDocumentLinks(ctx context.Context, documentID string) ([]domain.DocumentLink, error)
	ClassifyRecord(ctx context.Context, p domain.ClassifyRecordParams) (*domain.RecordClassification, error)
	FindClassificationByID(ctx context.Context, classificationID string) (*domain.RecordClassification, error)
	ConfirmClassification(ctx context.Context, p domain.ConfirmClassificationParams) (*domain.RecordClassification, error)
	GetClassification(ctx context.Context, documentID string) (*domain.RecordClassification, error)
	FindDocumentByID(ctx context.Context, documentID string) (*domain.Document, error)
	FindVersion(ctx context.Context, documentID string, version int) (*domain.DocumentVersion, error)
	ListVersions(ctx context.Context, documentID string) ([]domain.DocumentVersion, error)
	ListDocuments(ctx context.Context, legalEntityID string, limit, offset int) ([]domain.Document, error)
	RecordAccess(ctx context.Context, log *domain.DocumentAccessLog) error
	ListAccessLog(ctx context.Context, documentID string, limit, offset int) ([]domain.DocumentAccessLog, error)
	RecordQuarantinedVersionUpload(ctx context.Context, documentID, attemptedByPrincipalID, reason, correlationID string) error
}

type Handler struct {
	store     Store
	storage   storage.Backend
	residency residency.Validator
	authz     authz.Client
	scanner   scan.Scanner
	log       *zap.Logger
}

func New(store Store, storageBackend storage.Backend, residencyValidator residency.Validator, authzClient authz.Client, scanner scan.Scanner, log *zap.Logger) *Handler {
	return &Handler{store: store, storage: storageBackend, residency: residencyValidator, authz: authzClient, scanner: scanner, log: log}
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
		r.Post("/{documentID}/declare-record", h.DeclareRecord)
		r.Post("/{documentID}/supersede", h.SupersedeDocument)
		r.Post("/{documentID}/archive", h.MoveToArchive)
		r.Post("/{documentID}/request-disposition", h.RequestDisposition)
		r.Get("/{documentID}/verify-digest", h.VerifyDigest)
		r.Get("/{documentID}/as-of", h.GetAsOfDocument)
		r.Post("/{documentID}/links", h.LinkDocument)
		r.Get("/{documentID}/links", h.GetLinkedObjects)
		r.Post("/{documentID}/classify", h.ClassifyRecord)
		r.Post("/classifications/{classificationID}/confirm", h.ConfirmClassification)
		r.Get("/{documentID}/classification", h.GetClassification)
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

	// Malware/type scan gate (BIZ-01's own "Malware/type/hash failure
	// quarantines upload" failure semantics) — before anything is
	// persisted. No document exists yet at this point, so a quarantine
	// here is reject-only: there is no aggregate to tie a recorded event
	// to (see internal/scan's own package doc on the current NoOpScanner).
	if result, err := h.scanner.Scan(r.Context(), content, req.ContentType); err != nil {
		h.log.Error("CreateDocument: scan unavailable — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "scan_unavailable", "")
		return
	} else if !result.Clean {
		h.log.Warn("CreateDocument: upload quarantined", zap.String("reason", result.Reason))
		writeError(w, http.StatusUnprocessableEntity, "upload_quarantined", result.Reason)
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

	if err := h.store.CreateDocument(r.Context(), doc, firstVersion, r.Header.Get("X-Correlation-ID")); err != nil {
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

// ── POST /v1/documents/{documentID}/declare-record ──────────────────────────

// DeclareRecord marks the document's current version the authoritative
// declared record — BIZ-01's own central concept, distinct from ordinary
// versioning. See domain.CanDeclareRecord's own doc comment: this is a
// one-time action, not a repeatable one.
func (h *Handler) DeclareRecord(w http.ResponseWriter, r *http.Request) {
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
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentDeclareRecord) {
		return
	}

	updated, err := h.store.DeclareRecord(r.Context(), domain.DeclareRecordParams{
		DocumentID: documentID, DeclaredByPrincipalID: actor,
	}, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/documents/{documentID}/supersede ────────────────────────────────

type supersedeDocumentRequest struct {
	SupersededByDocumentID string `json:"superseded_by_document_id"`
}

// SupersedeDocument marks documentID as superseded by an already-existing
// document — the replacement is created first via the normal
// CreateDocument path, then linked here. Forward link only, set exactly
// once (migration 000005's own trigger).
func (h *Handler) SupersedeDocument(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req supersedeDocumentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SupersededByDocumentID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "superseded_by_document_id")
		return
	}
	documentID := chi.URLParam(r, "documentID")
	doc, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentSupersede) {
		return
	}

	updated, err := h.store.SupersedeDocument(r.Context(), domain.SupersedeDocumentParams{
		DocumentID: documentID, SupersededByDocumentID: req.SupersededByDocumentID, ActorPrincipalID: actor,
	}, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/documents/{documentID}/archive ──────────────────────────────────

type moveToArchiveRequest struct {
	Reason string `json:"reason,omitempty"`
}

// MoveToArchive marks the document ARCHIVED — see domain.CanArchive.
func (h *Handler) MoveToArchive(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req moveToArchiveRequest
	_ = decodeJSONOptional(r, &req)
	documentID := chi.URLParam(r, "documentID")
	doc, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentArchive) {
		return
	}

	updated, err := h.store.MoveToArchive(r.Context(), domain.MoveToArchiveParams{
		DocumentID: documentID, ArchivedByPrincipalID: actor, Reason: req.Reason,
	}, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/documents/{documentID}/request-disposition ─────────────────────

type requestDispositionRequest struct {
	Reason string `json:"reason,omitempty"`
}

// RequestDisposition records a disposition request only — see
// domain.Document.DispositionRequestedAt's own doc comment. It never
// purges anything; DATA-GOV owns that decision.
func (h *Handler) RequestDisposition(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req requestDispositionRequest
	_ = decodeJSONOptional(r, &req)
	documentID := chi.URLParam(r, "documentID")
	doc, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentRequestDisposition) {
		return
	}

	updated, err := h.store.RequestDisposition(r.Context(), domain.RequestDispositionParams{
		DocumentID: documentID, RequestedByPrincipalID: actor, Reason: req.Reason,
	}, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── GET /v1/documents/{documentID}/verify-digest ─────────────────────────────

// VerifyDigest re-verifies a version's stored checksum against the actual
// bytes on disk — a pass/fail integrity check, never the content itself
// (that stays GetContent's own DOWNLOAD-gated disclosure). Defaults to
// the document's current version; ?version=N checks a specific one.
func (h *Handler) VerifyDigest(w http.ResponseWriter, r *http.Request) {
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

	_, err = h.storage.Get(r.Context(), v.StorageKey, v.ChecksumSHA256)
	result := domain.DigestVerification{DocumentID: documentID, Version: version, ChecksumSHA256: v.ChecksumSHA256, Verified: true}
	if errors.Is(err, storage.ErrIntegrityFailure) {
		h.log.Error("VerifyDigest: INTEGRITY FAILURE", zap.String("document_id", documentID), zap.Int("version", version))
		result.Verified = false
		writeJSON(w, http.StatusOK, result)
		return
	}
	if err != nil {
		h.log.Error("VerifyDigest: storage unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ── GET /v1/documents/{documentID}/as-of ─────────────────────────────────────

type asOfDocumentResponse struct {
	Document domain.Document        `json:"document"`
	Version  domain.DocumentVersion `json:"version_as_of"`
}

// GetAsOfDocument reconstructs which version was current as of a given
// time — see store.PgStore.GetAsOfDocument's own doc comment on the real
// limit: only the version lineage is reconstructable, not historical
// status/declaration/supersession (no history table backs those fields
// yet).
func (h *Handler) GetAsOfDocument(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	documentID := chi.URLParam(r, "documentID")
	asOfRaw := r.URL.Query().Get("as_of")
	if asOfRaw == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "as_of")
		return
	}
	asOf, err := time.Parse(time.RFC3339, asOfRaw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_as_of", "as_of must be an RFC3339 timestamp")
		return
	}

	doc, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentRead) {
		return
	}

	asOfDoc, asOfVersion, err := h.store.GetAsOfDocument(r.Context(), documentID, asOf)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, asOfDocumentResponse{Document: *asOfDoc, Version: *asOfVersion})
}

// ── POST /v1/documents/{documentID}/links ────────────────────────────────────

type linkDocumentRequest struct {
	LinkedObjectType string `json:"linked_object_type"`
	LinkedObjectID   string `json:"linked_object_id"`
}

// LinkDocument records a link to another business object — see
// domain.DocumentLink's own doc comment. The caller (whatever service
// attached this document to something) invokes this explicitly; nothing
// here infers a link on its own.
func (h *Handler) LinkDocument(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req linkDocumentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LinkedObjectType == "" || req.LinkedObjectID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "linked_object_type and linked_object_id are required")
		return
	}
	documentID := chi.URLParam(r, "documentID")
	doc, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentLink) {
		return
	}

	link, err := h.store.LinkDocument(r.Context(), domain.LinkDocumentParams{
		DocumentID: documentID, LinkedObjectType: req.LinkedObjectType, LinkedObjectID: req.LinkedObjectID,
		LinkedByPrincipalID: actor, CorrelationID: r.Header.Get("X-Correlation-ID"),
	})
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, link)
}

// ── GET /v1/documents/{documentID}/links ─────────────────────────────────────

// GetLinkedObjects handles the doc's own GetLinkedObjects query — every
// business object this document has ever been linked to.
func (h *Handler) GetLinkedObjects(w http.ResponseWriter, r *http.Request) {
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

	links, err := h.store.ListDocumentLinks(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if links == nil {
		links = []domain.DocumentLink{}
	}
	writeJSON(w, http.StatusOK, links)
}

// ── POST /v1/documents/{documentID}/classify ─────────────────────────────────

type classifyRecordRequest struct {
	ClassificationValue domain.Classification       `json:"classification_value"`
	Source              domain.ClassificationSource `json:"source"`
	Confidence          *float64                    `json:"confidence,omitempty"`
	RuleModelVersion    string                      `json:"rule_model_version,omitempty"`
	SourceEvidence      string                      `json:"source_evidence,omitempty"`
}

// ClassifyRecord proposes a classification for a document — BIZ-02's own
// ClassifyRecord command. Lands CANDIDATE; requires ConfirmClassification
// (by a different principal, for a human proposal) before it governs
// anything.
func (h *Handler) ClassifyRecord(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req classifyRecordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !req.ClassificationValue.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_classification", string(req.ClassificationValue))
		return
	}
	documentID := chi.URLParam(r, "documentID")
	doc, err := h.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionClassifyRecord) {
		return
	}

	classification, err := h.store.ClassifyRecord(r.Context(), domain.ClassifyRecordParams{
		DocumentID: documentID, ClassificationValue: req.ClassificationValue, Source: req.Source,
		Confidence: req.Confidence, RuleModelVersion: req.RuleModelVersion, SourceEvidence: req.SourceEvidence,
		ProposedByPrincipalID: actor, CorrelationID: r.Header.Get("X-Correlation-ID"),
	})
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, classification)
}

// ── POST /v1/documents/classifications/{classificationID}/confirm ───────────

// ConfirmClassification moves a CANDIDATE classification to CONFIRMED —
// BIZ-02's own ConfirmClassification command. Fetched (read-only) BEFORE
// authorization and BEFORE the mutation — the same fetch-then-authorize
// order every other handler in this service uses — so an unauthorized
// caller can never cause the confirm to actually run before being
// refused. The self-confirmation (maker-checker) check itself happens
// in the store layer, where the proposal's own principal is compared.
func (h *Handler) ConfirmClassification(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	classificationID := chi.URLParam(r, "classificationID")

	existing, err := h.store.FindClassificationByID(r.Context(), classificationID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, actor, existing.LegalEntityID, authz.ActionConfirmClassification) {
		return
	}

	updated, err := h.store.ConfirmClassification(r.Context(), domain.ConfirmClassificationParams{
		ClassificationID: classificationID, ConfirmedByPrincipalID: actor,
	})
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── GET /v1/documents/{documentID}/classification ────────────────────────────

// GetClassification returns the document's current (non-superseded)
// classification — BIZ-02's own GetClassification query.
func (h *Handler) GetClassification(w http.ResponseWriter, r *http.Request) {
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

	classification, err := h.store.GetClassification(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, classification)
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

	// Malware/type scan gate — same as CreateDocument's, but this time
	// the document already exists, so a quarantine is recorded as a real
	// event (document.version_upload_quarantined) rather than only
	// rejected. Still nothing is persisted to storage or document_versions.
	if result, err := h.scanner.Scan(r.Context(), content, req.ContentType); err != nil {
		h.log.Error("AddVersion: scan unavailable — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "scan_unavailable", "")
		return
	} else if !result.Clean {
		h.log.Warn("AddVersion: upload quarantined", zap.String("document_id", documentID), zap.String("reason", result.Reason))
		if err := h.store.RecordQuarantinedVersionUpload(r.Context(), documentID, actor, result.Reason, r.Header.Get("X-Correlation-ID")); err != nil {
			h.log.Error("AddVersion: failed to record quarantine event", zap.Error(err))
		}
		writeError(w, http.StatusUnprocessableEntity, "upload_quarantined", result.Reason)
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

	doc, err := h.store.AddVersion(r.Context(), documentID, v, r.Header.Get("X-Correlation-ID"))
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
	case errors.Is(err, domain.ErrDocumentAlreadyDeclared):
		writeError(w, http.StatusConflict, "already_declared", err.Error())
	case errors.Is(err, domain.ErrDocumentNotActive):
		writeError(w, http.StatusConflict, "document_not_active", err.Error())
	case errors.Is(err, domain.ErrDocumentAlreadySuperseded):
		writeError(w, http.StatusConflict, "already_superseded", err.Error())
	case errors.Is(err, domain.ErrSupersedingDocumentNotFound):
		writeError(w, http.StatusBadRequest, "superseding_document_not_found", err.Error())
	case errors.Is(err, domain.ErrCannotSupersedeSelf):
		writeError(w, http.StatusBadRequest, "cannot_supersede_self", err.Error())
	case errors.Is(err, domain.ErrDocumentNotArchivable):
		writeError(w, http.StatusConflict, "not_archivable", err.Error())
	case errors.Is(err, domain.ErrDispositionAlreadyRequested):
		writeError(w, http.StatusConflict, "disposition_already_requested", err.Error())
	case errors.Is(err, domain.ErrDuplicateLink):
		writeError(w, http.StatusConflict, "duplicate_link", err.Error())
	case errors.Is(err, domain.ErrClassificationNotFound):
		writeError(w, http.StatusNotFound, "classification_not_found", "")
	case errors.Is(err, domain.ErrInvalidClassificationSource):
		writeError(w, http.StatusBadRequest, "invalid_source", err.Error())
	case errors.Is(err, domain.ErrAIConfidenceRequired):
		writeError(w, http.StatusBadRequest, "confidence_required", err.Error())
	case errors.Is(err, domain.ErrHumanConfidenceNotAllowed):
		writeError(w, http.StatusBadRequest, "confidence_not_allowed", err.Error())
	case errors.Is(err, domain.ErrClassificationSelfConfirmation):
		writeError(w, http.StatusForbidden, "self_confirmation_forbidden", err.Error())
	case errors.Is(err, domain.ErrClassificationNotCandidate):
		writeError(w, http.StatusConflict, "not_candidate", err.Error())
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

// decodeJSONOptional is decodeJSON for a command whose body is entirely
// optional (e.g. an archive/disposition reason) — an empty body is not an
// error, malformed JSON still is.
func decodeJSONOptional(r *http.Request, dst any) error {
	if r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
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
