package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/data-ingestion-svc/internal/authz"
	"zoiko.io/data-ingestion-svc/internal/domain"
	"zoiko.io/data-ingestion-svc/internal/imports"
	customMiddleware "zoiko.io/data-ingestion-svc/internal/middleware"
	"zoiko.io/eventing/integration"
)

// Authorization actions for INT-06 controlled import. They are new: each must be granted in the
// estate's RBAC seed before the matching command can be used.
const (
	ActionImportProfilePublish = "DATA_IMPORT_PROFILE_PUBLISH"
	ActionImportProfileCertify = "DATA_IMPORT_PROFILE_CERTIFY"
	ActionImportCreate         = "DATA_IMPORT_CREATE"
	ActionImportUpload         = "DATA_IMPORT_UPLOAD"
	ActionImportValidate       = "DATA_IMPORT_VALIDATE"
	ActionImportApprove        = "DATA_IMPORT_APPROVE"
	ActionImportExecute        = "DATA_IMPORT_EXECUTE"
	ActionImportCancel         = "DATA_IMPORT_CANCEL"
	ActionImportItemManage     = "DATA_IMPORT_ITEM_MANAGE"
	ActionImportReconcile      = "DATA_IMPORT_RECONCILE"
	ActionImportRead           = "DATA_IMPORT_READ"
)

// ImportHandler serves the INT-06 API.
type ImportHandler struct {
	svc      *imports.Service
	authz    *authz.Client
	log      *zap.Logger
	maxChunk int64
}

// NewImportHandler wires the handler. maxChunk bounds one uploaded chunk.
func NewImportHandler(svc *imports.Service, a *authz.Client, log *zap.Logger, maxChunk int64) *ImportHandler {
	if log == nil {
		log = zap.NewNop()
	}
	if maxChunk <= 0 {
		maxChunk = 4 << 20
	}
	return &ImportHandler{svc: svc, authz: a, log: log, maxChunk: maxChunk}
}

// RegisterImportRoutes mounts the API under /v1/data-ingestion.
func RegisterImportRoutes(r chi.Router, h *ImportHandler) {
	r.Post("/import-profiles", h.publishProfile)
	r.Get("/import-profiles", h.listProfiles)
	r.Get("/import-profiles/{key}", h.getProfile)
	r.Post("/import-profiles/{key}/versions/{version}:certify", h.certifyProfile)
	r.Post("/import-profiles/{key}/versions/{version}:retire", h.retireProfile)

	r.Post("/imports", h.create)
	r.Get("/imports", h.list)
	r.Get("/imports/{id}", h.get)
	r.Put("/imports/{id}/chunks/{index}", h.uploadChunk)
	r.Get("/imports/{id}/upload", h.uploadState)
	r.Post("/imports/{id}:seal", h.seal)
	r.Post("/imports/{id}:validate", h.validate)
	r.Post("/imports/{id}:preview", h.preview)
	r.Post("/imports/{id}:approve", h.approve)
	r.Post("/imports/{id}:execute", h.execute)
	r.Post("/imports/{id}:cancel", h.cancel)
	r.Post("/imports/{id}:reconcile", h.reconcile)
	r.Get("/imports/{id}/preview", h.getPreview)
	r.Get("/imports/{id}/validation-errors", h.validationErrors)
	r.Get("/imports/{id}/items", h.items)
	r.Get("/imports/{id}/control-totals", h.controlTotals)
	r.Get("/imports/{id}/reconciliation", h.getReconciliation)
	r.Get("/imports/{id}/log", h.logEntries)
	r.Post("/imports/{id}/items/{row}:quarantine", h.quarantineItem)
	r.Post("/imports/{id}/items/{row}:retry", h.retryItem)
}

// ── problems ────────────────────────────────────────────────────────────────

type problemOutcome struct {
	status int
	code   integration.Code
	detail string
	retry  int
}

// classifyImport maps an error to its presentation: a status, a stable INT-* code and a detail that
// names the rule and never echoes caller input.
func classifyImport(err error) problemOutcome {
	var replay *domain.IdempotentReplayError
	switch {
	case errors.Is(err, authz.ErrAuthorizationDenied):
		return problemOutcome{http.StatusForbidden, integration.CodeAuthDenied, "authorization denied", 0}
	case errors.Is(err, errNoPrincipal):
		return problemOutcome{http.StatusUnauthorized, integration.CodeAuthDenied, "X-Principal-Id header is required", 0}
	case errors.Is(err, errBadImportBody):
		return problemOutcome{http.StatusBadRequest, integration.CodeSchemaMismatch, err.Error(), 0}
	case errors.As(err, &replay):
		return problemOutcome{http.StatusConflict, integration.CodeIdempotencyConflict, "idempotent replay", 0}
	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		return problemOutcome{http.StatusConflict, integration.CodeIdempotencyConflict, err.Error(), 0}
	case errors.Is(err, imports.ErrNotFound), errors.Is(err, imports.ErrProfileNotFound), errors.Is(err, imports.ErrItemNotFound):
		return problemOutcome{http.StatusNotFound, "", err.Error(), 0}
	case errors.Is(err, imports.ErrHashMismatch), errors.Is(err, imports.ErrChunkConflict), errors.Is(err, imports.ErrIncompleteUpload),
		errors.Is(err, imports.ErrPreviewMismatch):
		return problemOutcome{http.StatusConflict, integration.CodeFileIntegrity, err.Error(), 0}
	case errors.Is(err, imports.ErrTooLarge):
		return problemOutcome{http.StatusRequestEntityTooLarge, integration.CodeFileIntegrity, err.Error(), 0}
	case errors.Is(err, imports.ErrDuplicateBatch), errors.Is(err, imports.ErrVersionConflict):
		return problemOutcome{http.StatusConflict, integration.CodeIdempotencyConflict, err.Error(), 0}
	case errors.Is(err, imports.ErrReconciliationFailed):
		return problemOutcome{http.StatusConflict, integration.CodeReconciliationBreak, err.Error(), 0}
	case errors.Is(err, imports.ErrSelfApproval), errors.Is(err, imports.ErrProfileSelfCertify), errors.Is(err, imports.ErrNotPlatformScope):
		return problemOutcome{http.StatusForbidden, integration.CodeAuthDenied, err.Error(), 0}
	case errors.Is(err, imports.ErrNoScanner), errors.Is(err, imports.ErrScannerUnavailable):
		return problemOutcome{http.StatusServiceUnavailable, integration.CodeQuarantined, err.Error(), 30}
	case errors.Is(err, imports.ErrInvalidState), errors.Is(err, imports.ErrProfileNotCertified), errors.Is(err, imports.ErrTargetNotConfigured),
		errors.Is(err, imports.ErrRetryNotAllowed):
		return problemOutcome{http.StatusConflict, integration.CodeScopeDenied, err.Error(), 0}
	case errors.Is(err, imports.ErrValidation), errors.Is(err, imports.ErrSchemaMismatch):
		return problemOutcome{http.StatusBadRequest, integration.CodeSchemaMismatch, err.Error(), 0}
	}
	return problemOutcome{http.StatusInternalServerError, "", "internal error", 0}
}

var (
	errNoPrincipal   = errors.New("principal required")
	errBadImportBody = errors.New("the request body is not valid JSON for this operation")
)

// problem answers with RFC 9457 problem details. The pre-existing {"error": "..."} member rides
// beside them so clients written against this service's old error shape keep reading it.
func (h *ImportHandler) problem(w http.ResponseWriter, r *http.Request, err error) {
	o := classifyImport(err)
	if o.status >= 500 && o.status != http.StatusServiceUnavailable {
		h.log.Error("import API: internal error", zap.String("path", r.URL.Path), zap.Error(err))
	}
	body := map[string]any{
		"type": "about:blank", "title": http.StatusText(o.status), "status": o.status, "detail": o.detail, "error": o.detail,
		"instance": r.URL.Path,
	}
	if c := r.Header.Get("X-Correlation-ID"); c != "" {
		body["correlation_id"] = c
	}
	if o.code != "" {
		info := o.code.Info()
		body["type"] = integration.ProblemTypePrefix + string(o.code)
		body["title"] = info.Title
		body["code"] = string(o.code)
		body["retryable"] = info.Retryable
	}
	if o.retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(o.retry))
	}
	w.Header().Set("Content-Type", integration.ProblemContentType)
	w.WriteHeader(o.status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeImportJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ── plumbing ────────────────────────────────────────────────────────────────

func (h *ImportHandler) actor(r *http.Request) (imports.Actor, error) {
	p := r.Header.Get("X-Principal-Id")
	if p == "" {
		return imports.Actor{}, errNoPrincipal
	}
	corr := r.Header.Get("X-Correlation-ID")
	return imports.Actor{TenantID: customMiddleware.GetTenantID(r.Context()), PrincipalID: p, CorrelationID: corr}, nil
}

func (h *ImportHandler) allow(r *http.Request, principal, legalEntityID, action string) error {
	return h.authz.CheckAllowed(r.Context(), principal, legalEntityID, action)
}

const maxJSONBody = 256 << 10

func decodeImportBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return errBadImportBody
	}
	return nil
}

// load fetches the batch and authorizes the caller against ITS legal entity.
func (h *ImportHandler) load(w http.ResponseWriter, r *http.Request, action string) (imports.Actor, *imports.Batch, bool) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return a, nil, false
	}
	b, err := h.svc.Get(r.Context(), a.TenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.problem(w, r, err)
		return a, nil, false
	}
	if err := h.allow(r, a.PrincipalID, b.LegalEntityID, action); err != nil {
		h.problem(w, r, err)
		return a, nil, false
	}
	return a, b, true
}

func limitOf(r *http.Request) int { n, _ := strconv.Atoi(r.URL.Query().Get("limit")); return n }

// ── DTOs ────────────────────────────────────────────────────────────────────

type batchDTO struct {
	BatchID             string     `json:"batch_id"`
	LegalEntityID       string     `json:"legal_entity_id"`
	ProfileKey          string     `json:"profile_key"`
	ProfileVersion      int        `json:"profile_version"`
	Material            bool       `json:"material"`
	SourceSystem        string     `json:"source_system"`
	FileName            string     `json:"file_name,omitempty"`
	DeclaredContentType string     `json:"declared_content_type"`
	DeclaredSHA256      string     `json:"declared_sha256"`
	DeclaredRows        *int       `json:"declared_rows,omitempty"`
	DeclaredControl     string     `json:"declared_control_total,omitempty"`
	Status              string     `json:"status"`
	StatusReason        string     `json:"status_reason,omitempty"`
	Version             int        `json:"version"`
	ChunkCount          int        `json:"chunk_count"`
	SizeBytes           int64      `json:"size_bytes"`
	ContentSHA256       string     `json:"content_sha256,omitempty"`
	ScanEngine          string     `json:"scan_engine,omitempty"`
	ScanVerdict         string     `json:"scan_verdict,omitempty"`
	ScanSignature       string     `json:"scan_signature,omitempty"`
	RowCount            int        `json:"row_count"`
	ValidRows           int        `json:"valid_rows"`
	InvalidRows         int        `json:"invalid_rows"`
	ComputedControl     string     `json:"computed_control_total,omitempty"`
	PreviewFingerprint  string     `json:"preview_fingerprint,omitempty"`
	ApprovedBy          string     `json:"approved_by,omitempty"`
	ApprovedAt          *time.Time `json:"approved_at,omitempty"`
	ExecutedBy          string     `json:"executed_by,omitempty"`
	ImportedItems       int        `json:"imported_items"`
	QuarantinedItems    int        `json:"quarantined_items"`
	FailedItems         int        `json:"failed_items"`
	SkippedItems        int        `json:"skipped_items"`
	// Outcome keeps delivery, validation, mapping, the owning domain and reconciliation apart; Overall is the pessimistic
	// roll-up. A batch that was delivered but not reconciled is PENDING, never a success (INT-NP-048).
	Outcome   integration.Outcome `json:"outcome"`
	Overall   string              `json:"overall"`
	CreatedBy string              `json:"created_by"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

func toBatchDTO(b *imports.Batch) batchDTO {
	o := imports.OutcomeOf(b)
	return batchDTO{Outcome: o, Overall: o.Overall(),
		BatchID: b.BatchID, LegalEntityID: b.LegalEntityID, ProfileKey: b.ProfileKey, ProfileVersion: b.ProfileVersion, Material: b.Material,
		SourceSystem: b.SourceSystem, FileName: b.FileName, DeclaredContentType: b.DeclaredContentType, DeclaredSHA256: b.DeclaredSHA256,
		DeclaredRows: b.DeclaredRows, DeclaredControl: b.DeclaredControl, Status: string(b.Status), StatusReason: b.StatusReason, Version: b.Version,
		ChunkCount: b.ChunkCount, SizeBytes: b.SizeBytes, ContentSHA256: b.ContentSHA256, ScanEngine: b.ScanEngine, ScanVerdict: b.ScanVerdict,
		ScanSignature: b.ScanSignature, RowCount: b.RowCount, ValidRows: b.ValidRows, InvalidRows: b.InvalidRows, ComputedControl: b.ComputedControl,
		PreviewFingerprint: b.PreviewFingerprint, ApprovedBy: b.ApprovedBy, ApprovedAt: b.ApprovedAt, ExecutedBy: b.ExecutedBy,
		ImportedItems: b.ImportedItems, QuarantinedItems: b.QuarantinedItems, FailedItems: b.FailedItems, SkippedItems: b.SkippedItems,
		CreatedBy: b.CreatedBy, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
	}
}

type itemDTO struct {
	RowNo            int                `json:"row_no"`
	Status           string             `json:"status"`
	Values           map[string]any     `json:"values,omitempty"`
	Errors           []imports.RowError `json:"errors,omitempty"`
	IdempotencyKey   string             `json:"idempotency_key"`
	Attempts         int                `json:"attempts"`
	DomainStatus     *int               `json:"domain_status,omitempty"`
	ResultRef        string             `json:"result_ref,omitempty"`
	LastErrorClass   string             `json:"last_error_class,omitempty"`
	QuarantineReason string             `json:"quarantine_reason,omitempty"`
	QuarantineOwner  string             `json:"quarantine_owner,omitempty"`
}

func toItemDTO(it imports.Item, withValues bool) itemDTO {
	d := itemDTO{RowNo: it.RowNo, Status: string(it.Status), Errors: it.ErrorCodes, IdempotencyKey: it.IdempotencyKey, Attempts: it.Attempts,
		DomainStatus: it.DomainStatus, ResultRef: it.ResultRef, LastErrorClass: it.LastErrorClass, QuarantineReason: it.QuarantineReason,
		QuarantineOwner: it.QuarantineOwner}
	if withValues {
		d.Values = it.Values
	}
	return d
}

// ── profiles ────────────────────────────────────────────────────────────────

type publishProfileRequest struct {
	ProfileKey string       `json:"profile_key"`
	Spec       imports.Spec `json:"spec"`
}

func (h *ImportHandler) publishProfile(w http.ResponseWriter, r *http.Request) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	if err := h.allow(r, a.PrincipalID, imports.PlatformScopeID, ActionImportProfilePublish); err != nil {
		h.problem(w, r, err)
		return
	}
	var req publishProfileRequest
	if err := decodeImportBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	p, err := h.svc.PublishProfile(r.Context(), a, req.ProfileKey, req.Spec)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusCreated, profileDTO(p))
}

func profileDTO(p *imports.Profile) map[string]any {
	return map[string]any{"profile_key": p.Key, "version": p.Version, "status": string(p.Status), "spec": p.Spec, "spec_hash": p.SpecHash,
		"created_by": p.CreatedBy, "certified_by": p.CertifiedBy, "certified_at": p.CertifiedAt}
}

func (h *ImportHandler) getProfile(w http.ResponseWriter, r *http.Request) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	if err := h.allow(r, a.PrincipalID, r.Header.Get("X-Legal-Entity-Id"), ActionImportRead); err != nil {
		h.problem(w, r, err)
		return
	}
	v, _ := strconv.Atoi(r.URL.Query().Get("version"))
	p, err := h.svc.GetProfile(r.Context(), chi.URLParam(r, "key"), v)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, profileDTO(p))
}

func (h *ImportHandler) listProfiles(w http.ResponseWriter, r *http.Request) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	if err := h.allow(r, a.PrincipalID, r.Header.Get("X-Legal-Entity-Id"), ActionImportRead); err != nil {
		h.problem(w, r, err)
		return
	}
	ps, err := h.svc.ListProfiles(r.Context(), r.URL.Query().Get("key"))
	if err != nil {
		h.problem(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(ps))
	for i := range ps {
		out = append(out, profileDTO(&ps[i]))
	}
	writeImportJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

func (h *ImportHandler) certifyProfile(w http.ResponseWriter, r *http.Request) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	if err := h.allow(r, a.PrincipalID, imports.PlatformScopeID, ActionImportProfileCertify); err != nil {
		h.problem(w, r, err)
		return
	}
	v, _ := strconv.Atoi(chi.URLParam(r, "version"))
	p, err := h.svc.CertifyProfile(r.Context(), a, chi.URLParam(r, "key"), v)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, profileDTO(p))
}

func (h *ImportHandler) retireProfile(w http.ResponseWriter, r *http.Request) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	if err := h.allow(r, a.PrincipalID, imports.PlatformScopeID, ActionImportProfileCertify); err != nil {
		h.problem(w, r, err)
		return
	}
	var req struct {
		To string `json:"to"`
	}
	if err := decodeImportBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	v, _ := strconv.Atoi(chi.URLParam(r, "version"))
	p, err := h.svc.RetireProfile(r.Context(), a, chi.URLParam(r, "key"), v, imports.ProfileStatus(req.To))
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, profileDTO(p))
}

// ── batches ─────────────────────────────────────────────────────────────────

type createImportRequest struct {
	LegalEntityID   string `json:"legal_entity_id"`
	ProfileKey      string `json:"profile_key"`
	ProfileVersion  int    `json:"profile_version"`
	SourceSystem    string `json:"source_system"`
	FileName        string `json:"file_name"`
	ContentType     string `json:"content_type"`
	SHA256          string `json:"sha256"`
	DeclaredRows    *int   `json:"declared_rows"`
	DeclaredControl string `json:"declared_control_total"`
}

func (h *ImportHandler) create(w http.ResponseWriter, r *http.Request) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		h.problem(w, r, errBadImportBody)
		return
	}
	var req createImportRequest
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		h.problem(w, r, errBadImportBody)
		return
	}
	if err := h.allow(r, a.PrincipalID, req.LegalEntityID, ActionImportCreate); err != nil {
		h.problem(w, r, err)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		h.problem(w, r, imports.ErrValidation)
		return
	}
	sum := sha256.Sum256(raw)
	claim := domain.IdempotencyClaim{OwnerScope: domain.SellerScope, PrincipalID: a.PrincipalID, Key: key, Operation: "CreateImportBatch",
		RequestSHA256: hex.EncodeToString(sum[:])}
	b, err := h.svc.CreateBatch(r.Context(), a, imports.CreateRequest{
		LegalEntityID: req.LegalEntityID, ProfileKey: req.ProfileKey, ProfileVersion: req.ProfileVersion, SourceSystem: req.SourceSystem,
		FileName: req.FileName, ContentType: req.ContentType, SHA256: req.SHA256, DeclaredRows: req.DeclaredRows, DeclaredControl: req.DeclaredControl,
		Jurisdiction: r.Header.Get("X-Jurisdiction-Context"),
	}, claim)
	var replay *domain.IdempotentReplayError
	if errors.As(err, &replay) {
		prev, gerr := h.svc.Get(r.Context(), a.TenantID, replay.ResourceID)
		if gerr != nil {
			h.problem(w, r, gerr)
			return
		}
		w.Header().Set("Idempotent-Replayed", "true")
		writeImportJSON(w, http.StatusOK, toBatchDTO(prev))
		return
	}
	if err != nil {
		h.problem(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/data-ingestion/imports/"+b.BatchID)
	writeImportJSON(w, http.StatusCreated, toBatchDTO(b))
}

func (h *ImportHandler) list(w http.ResponseWriter, r *http.Request) {
	a, err := h.actor(r)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	if err := h.allow(r, a.PrincipalID, r.Header.Get("X-Legal-Entity-Id"), ActionImportRead); err != nil {
		h.problem(w, r, err)
		return
	}
	bs, err := h.svc.List(r.Context(), a.TenantID, imports.Status(r.URL.Query().Get("status")), limitOf(r))
	if err != nil {
		h.problem(w, r, err)
		return
	}
	out := make([]batchDTO, 0, len(bs))
	for i := range bs {
		out = append(out, toBatchDTO(&bs[i]))
	}
	writeImportJSON(w, http.StatusOK, map[string]any{"imports": out})
}

func (h *ImportHandler) get(w http.ResponseWriter, r *http.Request) {
	if _, b, ok := h.load(w, r, ActionImportRead); ok {
		writeImportJSON(w, http.StatusOK, toBatchDTO(b))
	}
}

func (h *ImportHandler) uploadChunk(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportUpload)
	if !ok {
		return
	}
	idx, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil {
		h.problem(w, r, imports.ErrValidation)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxChunk+1))
	if err != nil || int64(len(data)) > h.maxChunk {
		h.problem(w, r, imports.ErrTooLarge)
		return
	}
	stored, err := h.svc.UploadChunk(r.Context(), a, b.BatchID, idx, data, r.Header.Get("X-Chunk-SHA256"))
	if err != nil {
		h.problem(w, r, err)
		return
	}
	code := http.StatusCreated
	if !stored {
		code = http.StatusOK // the same chunk again: an upload that resumed
	}
	writeImportJSON(w, code, map[string]any{"batch_id": b.BatchID, "chunk_index": idx, "stored": stored})
}

func (h *ImportHandler) uploadState(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportRead)
	if !ok {
		return
	}
	chunks, err := h.svc.UploadState(r.Context(), a.TenantID, b.BatchID)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	type chunk struct {
		Index  int    `json:"index"`
		Size   int    `json:"size_bytes"`
		SHA256 string `json:"sha256"`
	}
	out := make([]chunk, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, chunk{c.Index, c.Size, c.SHA256})
	}
	writeImportJSON(w, http.StatusOK, map[string]any{"batch_id": b.BatchID, "status": string(b.Status), "chunks": out})
}

func (h *ImportHandler) seal(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportUpload)
	if !ok {
		return
	}
	var req struct {
		ChunkCount int `json:"chunk_count"`
	}
	if err := decodeImportBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	out, err := h.svc.Seal(r.Context(), a, b.BatchID, req.ChunkCount)
	if err != nil {
		if out != nil { // a quarantined upload: say what happened to it
			h.problemWithBatch(w, r, err, out)
			return
		}
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, toBatchDTO(out))
}

// problemWithBatch answers a refused command that nevertheless changed the batch (it was
// quarantined): the problem, plus the batch as it now stands.
func (h *ImportHandler) problemWithBatch(w http.ResponseWriter, r *http.Request, err error, b *imports.Batch) {
	o := classifyImport(err)
	body := map[string]any{
		"type": integration.ProblemTypePrefix + string(o.code), "title": http.StatusText(o.status), "status": o.status, "detail": o.detail,
		"error": o.detail, "instance": r.URL.Path, "code": string(o.code), "batch": toBatchDTO(b),
	}
	w.Header().Set("Content-Type", integration.ProblemContentType)
	w.WriteHeader(o.status)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *ImportHandler) validate(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportValidate)
	if !ok {
		return
	}
	out, err := h.svc.RequestValidation(r.Context(), a, b.BatchID)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusAccepted, toBatchDTO(out))
}

func (h *ImportHandler) preview(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportValidate)
	if !ok {
		return
	}
	out, err := h.svc.Preview(r.Context(), a, b.BatchID)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, map[string]any{"batch": toBatchDTO(out), "preview": out.Preview, "preview_fingerprint": out.PreviewFingerprint})
}

func (h *ImportHandler) approve(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportApprove)
	if !ok {
		return
	}
	var req struct {
		PreviewFingerprint string `json:"preview_fingerprint"`
		Reason             string `json:"reason"`
	}
	if err := decodeImportBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	out, err := h.svc.Approve(r.Context(), a, b.BatchID, req.PreviewFingerprint, req.Reason)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, toBatchDTO(out))
}

func (h *ImportHandler) execute(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportExecute)
	if !ok {
		return
	}
	out, err := h.svc.Execute(r.Context(), a, b.BatchID)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusAccepted, toBatchDTO(out))
}

func (h *ImportHandler) cancel(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportCancel)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeImportBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	out, err := h.svc.Cancel(r.Context(), a, b.BatchID, req.Reason)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, toBatchDTO(out))
}

func (h *ImportHandler) reconcile(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportReconcile)
	if !ok {
		return
	}
	out, err := h.svc.Reconcile(r.Context(), a, b.BatchID)
	if err != nil {
		if out != nil && errors.Is(err, imports.ErrReconciliationFailed) {
			h.problemWithBatch(w, r, err, out)
			return
		}
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, map[string]any{"batch": toBatchDTO(out), "reconciliation": out.Reconciliation})
}

func (h *ImportHandler) getPreview(w http.ResponseWriter, r *http.Request) {
	_, b, ok := h.load(w, r, ActionImportRead)
	if !ok {
		return
	}
	if b.Preview == nil {
		h.problem(w, r, imports.ErrNotFound)
		return
	}
	writeImportJSON(w, http.StatusOK, map[string]any{"preview": b.Preview, "preview_fingerprint": b.PreviewFingerprint})
}

func (h *ImportHandler) getReconciliation(w http.ResponseWriter, r *http.Request) {
	_, b, ok := h.load(w, r, ActionImportRead)
	if !ok {
		return
	}
	if b.Reconciliation == nil {
		h.problem(w, r, imports.ErrNotFound)
		return
	}
	writeImportJSON(w, http.StatusOK, b.Reconciliation)
}

func (h *ImportHandler) listItems(w http.ResponseWriter, r *http.Request, status imports.ItemStatus, withValues bool, key string) {
	a, b, ok := h.load(w, r, ActionImportRead)
	if !ok {
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	items, err := h.svc.Items(r.Context(), a.TenantID, b.BatchID, status, after, limitOf(r))
	if err != nil {
		h.problem(w, r, err)
		return
	}
	out := make([]itemDTO, 0, len(items))
	next := 0
	for _, it := range items {
		out = append(out, toItemDTO(it, withValues))
		next = it.RowNo
	}
	writeImportJSON(w, http.StatusOK, map[string]any{key: out, "next_after": next})
}

func (h *ImportHandler) validationErrors(w http.ResponseWriter, r *http.Request) {
	// Errors name the column and a stable code; the invalid row's values are not returned.
	h.listItems(w, r, imports.ItemInvalid, false, "errors")
}

func (h *ImportHandler) items(w http.ResponseWriter, r *http.Request) {
	h.listItems(w, r, imports.ItemStatus(r.URL.Query().Get("status")), true, "items")
}

func (h *ImportHandler) controlTotals(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportRead)
	if !ok {
		return
	}
	ct, err := h.svc.ControlTotals(r.Context(), a.TenantID, b.BatchID)
	if err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, ct)
}

func (h *ImportHandler) logEntries(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportRead)
	if !ok {
		return
	}
	entries, err := h.svc.Log(r.Context(), a.TenantID, b.BatchID, limitOf(r))
	if err != nil {
		h.problem(w, r, err)
		return
	}
	type entry struct {
		Kind       string    `json:"kind"`
		Actor      string    `json:"actor"`
		FromStatus string    `json:"from_status,omitempty"`
		ToStatus   string    `json:"to_status,omitempty"`
		Reason     string    `json:"reason,omitempty"`
		CreatedAt  time.Time `json:"created_at"`
	}
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, entry{e.Kind, e.Actor, string(e.FromStatus), string(e.ToStatus), e.Reason, e.CreatedAt})
	}
	writeImportJSON(w, http.StatusOK, map[string]any{"log": out})
}

func (h *ImportHandler) quarantineItem(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportItemManage)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
		Owner  string `json:"owner"`
	}
	if err := decodeImportBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	row, err := strconv.Atoi(chi.URLParam(r, "row"))
	if err != nil {
		h.problem(w, r, imports.ErrValidation)
		return
	}
	if err := h.svc.QuarantineItem(r.Context(), a, b.BatchID, row, req.Reason, req.Owner); err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusOK, map[string]any{"batch_id": b.BatchID, "row_no": row, "status": "QUARANTINED"})
}

func (h *ImportHandler) retryItem(w http.ResponseWriter, r *http.Request) {
	a, b, ok := h.load(w, r, ActionImportItemManage)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeImportBody(w, r, &req); err != nil {
		h.problem(w, r, err)
		return
	}
	row, err := strconv.Atoi(chi.URLParam(r, "row"))
	if err != nil {
		h.problem(w, r, imports.ErrValidation)
		return
	}
	if err := h.svc.RetryItem(r.Context(), a, b.BatchID, row, req.Reason); err != nil {
		h.problem(w, r, err)
		return
	}
	writeImportJSON(w, http.StatusAccepted, map[string]any{"batch_id": b.BatchID, "row_no": row, "status": "PENDING"})
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
