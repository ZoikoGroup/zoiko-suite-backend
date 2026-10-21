package handler

// Upload reservations — ZS-SVC-S-001 §3.4 / §3.6:
//
//	POST /v1/documents/{id}/upload-reservations   "Reserve version upload."
//	POST /v1/upload-reservations/{id}/complete    "Finalize and validate upload."
//
// The reservation is a real state machine (RESERVED -> COMMITTED | FAILED |
// QUARANTINED) and completion runs the SAME gates as the direct upload routes, in
// the same order: content-type check against the bytes (DRC-I19), fail-closed
// malware scan (DRC-I18), write to storage, then one transaction that adds the
// version, publishes its outbox event and marks the reservation COMMITTED.
//
// What the specification describes that this service cannot provide: §3.4 has the
// client upload straight to the object store with "short-lived, object-specific"
// credentials, and §10.3 resolves region before that instruction is issued. This
// service's storage is a local encrypted file backend with no credential issuance,
// so the content travels in the completion request instead (content_base64), under
// the same body limit as the direct routes. No credential, bucket or signed URL is
// invented; moving to direct object-store upload is a storage-backend change
// recorded as external in the README.
//
// Versions can be reserved only for an EXISTING document: the first version still
// arrives with POST /v1/documents.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/authz"
	"zoiko.io/document-vault-svc/internal/contenttype"
	"zoiko.io/document-vault-svc/internal/domain"
)

// ReservationStore is the reservation persistence contract.
type ReservationStore interface {
	CreateUploadReservation(ctx context.Context, p domain.CreateUploadReservationParams) (*domain.UploadReservation, bool, error)
	GetUploadReservation(ctx context.Context, reservationID string) (*domain.UploadReservation, error)
	CompleteUploadReservation(ctx context.Context, reservationID string, v *domain.DocumentVersion, correlationID string) (*domain.UploadReservation, *domain.DocumentVersion, bool, error)
	CloseUploadReservation(ctx context.Context, reservationID, status, reason string) (*domain.UploadReservation, error)
	// FindVersionByID returns the committed version when a completion is replayed.
	FindVersionByID(ctx context.Context, versionID string) (*domain.DocumentVersion, error)
}

type reservationHandler struct {
	*Handler
	store ReservationStore
}

// RegisterUploadReservationRoutes mounts the two §3.6 reservation routes.
func RegisterUploadReservationRoutes(r chi.Router, h *Handler, rs ReservationStore) {
	rh := &reservationHandler{Handler: h, store: rs}
	r.Post("/v1/documents/{documentID}/upload-reservations", rh.Reserve)
	r.Post("/v1/upload-reservations/{reservation_id}/complete", rh.Complete)
}

// maxReservedBytes is the largest content a completion request can carry: the
// request body limit, less envelope slack, decoded from base64 (4 chars -> 3 bytes).
const maxReservedBytes = int64(maxBodyBytes-1024) / 4 * 3

// maxReservationLabelLen mirrors the VARCHAR(255) columns.
const maxReservationLabelLen = 255

type reserveUploadRequest struct {
	Purpose             string `json:"purpose"`
	ExpectedSizeBytes   int64  `json:"expected_size_bytes"`
	DeclaredContentType string `json:"declared_content_type"`
}

// uploadInstructions is the "scoped upload contract" of this service: where to
// send the bytes. See the file comment for why it is not an object-store grant.
type uploadInstructions struct {
	Method       string `json:"method"`
	Path         string `json:"path"`
	ContentField string `json:"content_field"`
	MaxBytes     int64  `json:"max_bytes"`
}

type reservationView struct {
	domain.UploadReservation
	Upload uploadInstructions `json:"upload"`
}

func newReservationView(res *domain.UploadReservation) reservationView {
	return reservationView{UploadReservation: *res, Upload: uploadInstructions{
		Method: http.MethodPost, Path: "/v1/upload-reservations/" + res.ReservationID + "/complete",
		ContentField: "content_base64", MaxBytes: maxReservedBytes,
	}}
}

// Reserve reserves a version upload for an existing document. Authorization is
// DOCUMENT_VERSION_CREATE on the owning document's legal entity — the grant the
// direct version route needs — decided before the body is read. An Idempotency-Key
// is required (§3.4): the same key and request returns the original reservation
// (200), a different request under the key is 409 idempotency_mismatch.
func (h *reservationHandler) Reserve(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	documentID := chi.URLParam(r, "documentID")
	doc, err := h.Handler.store.FindDocumentByID(r.Context(), documentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentVersionCreate) {
		return
	}

	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key is required to reserve an upload")
		return
	}
	if len(key) > maxReservationLabelLen {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must be at most 255 characters")
		return
	}
	var req reserveUploadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Purpose = strings.TrimSpace(req.Purpose)
	req.DeclaredContentType = strings.TrimSpace(req.DeclaredContentType)
	switch {
	case req.Purpose == "":
		writeError(w, http.StatusBadRequest, "missing_field", "purpose")
		return
	case len(req.Purpose) > maxReservationLabelLen:
		writeError(w, http.StatusBadRequest, "invalid_request", "purpose must be at most 255 characters")
		return
	case req.DeclaredContentType == "":
		writeError(w, http.StatusBadRequest, "missing_field", "declared_content_type")
		return
	case req.ExpectedSizeBytes <= 0:
		writeError(w, http.StatusBadRequest, "invalid_request", "expected_size_bytes must be a positive integer")
		return
	case req.ExpectedSizeBytes > maxReservedBytes:
		writeError(w, http.StatusBadRequest, "size_too_large", "expected_size_bytes exceeds the largest upload this service accepts")
		return
	}
	if _, _, perr := mime.ParseMediaType(req.DeclaredContentType); perr != nil {
		writeError(w, http.StatusBadRequest, "invalid_content_type", contenttype.ErrInvalidDeclared.Error())
		return
	}

	res, replayed, err := h.store.CreateUploadReservation(r.Context(), domain.CreateUploadReservationParams{
		DocumentID: documentID, ReservedByPrincipal: actor, Purpose: req.Purpose,
		ExpectedSizeBytes: req.ExpectedSizeBytes, DeclaredContentType: req.DeclaredContentType,
		IdempotencyKey: key, RequestHash: reservationRequestHash(documentID, req),
	})
	if err != nil {
		h.handleReservationError(w, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replay", "true")
		writeJSON(w, http.StatusOK, newReservationView(res))
		return
	}
	writeJSON(w, http.StatusCreated, newReservationView(res))
}

type completeUploadRequest struct {
	ContentBase64 string `json:"content_base64"`
}

type completionView struct {
	ReservationID string              `json:"reservation_id"`
	DocumentID    string              `json:"document_id"`
	CommitStatus  string              `json:"commit_status"`
	Version       documentVersionView `json:"version"`
}

// Complete finalizes a reservation: validates the bytes against it and commits
// the version, or leaves a terminal FAILED / QUARANTINED reservation and no
// version. Re-authorized at completion (DOCUMENT_VERSION_CREATE on the document's
// entity). Idempotent: completing a COMMITTED reservation again — any number of
// times, concurrently or not — returns the one committed version (200) and never
// a second.
//
// Gates, in order, and what each leaves behind:
//
//	size differs from the reservation        -> FAILED       422 DRC-002
//	declared type contradicts the bytes      -> FAILED       422 DRC-002
//	malware scanner unavailable              -> stays RESERVED, 503 (retryable; never a clean verdict)
//	scanner verdict not clean                -> QUARANTINED  422 (DRC-003); no version
//	storage or database failure              -> stays RESERVED, blob removed, retryable
func (h *reservationHandler) Complete(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	reservationID := chi.URLParam(r, "reservation_id")
	res, err := h.store.GetUploadReservation(r.Context(), reservationID)
	if err != nil {
		h.handleReservationError(w, err)
		return
	}
	doc, err := h.Handler.store.FindDocumentByID(r.Context(), res.DocumentID)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	if !h.authorize(w, r, actor, doc.LegalEntityID, authz.ActionDocumentVersionCreate) {
		return
	}

	switch res.CommitStatus {
	case domain.ReservationCommitted:
		h.writeCommitted(w, r, res, doc, nil, true)
		return
	case domain.ReservationFailed, domain.ReservationQuarantined:
		h.writeReservationClosed(w, res)
		return
	}

	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	var req completeUploadRequest
	if !decodeBytes(w, raw, &req) {
		return
	}
	content, derr := base64.StdEncoding.DecodeString(req.ContentBase64)
	if derr != nil {
		writeError(w, http.StatusBadRequest, "invalid_content_base64", derr.Error())
		return
	}

	// DRC-002: the bytes must be the size that was reserved.
	if int64(len(content)) != res.ExpectedSizeBytes {
		h.closeAndRefuse(w, r, res.ReservationID, domain.ReservationFailed,
			"content length does not match the reserved expected_size_bytes", http.StatusUnprocessableEntity, "DRC-002",
			"UPLOAD_VALIDATION_FAILED: content length does not match the reserved expected_size_bytes")
		return
	}
	// DRC-I19: the declared type reserved is checked against the bytes.
	detected, cerr := contenttype.Validate(res.DeclaredContentType, content)
	if cerr != nil {
		h.closeReservation(r, res.ReservationID, domain.ReservationFailed, cerr.Error())
		h.writeContentTypeError(w, cerr)
		return
	}
	// DRC-I18: fail-closed malware scan, given the DETECTED type.
	scanResult, serr := h.scanner.Scan(r.Context(), content, detected)
	if serr != nil {
		h.log.Error("Complete reservation: scan unavailable — failing closed", zap.Error(serr))
		writeError(w, http.StatusServiceUnavailable, "scan_unavailable", "")
		return
	}
	if !scanResult.Clean {
		h.log.Warn("Complete reservation: upload quarantined", zap.String("document_id", res.DocumentID), zap.String("reason", scanResult.Reason))
		if qerr := h.Handler.store.RecordQuarantinedVersionUpload(r.Context(), res.DocumentID, actor, scanResult.Reason, r.Header.Get("X-Correlation-ID")); qerr != nil {
			h.log.Error("Complete reservation: failed to record quarantine event", zap.Error(qerr))
		}
		h.closeAndRefuse(w, r, res.ReservationID, domain.ReservationQuarantined, scanResult.Reason,
			http.StatusUnprocessableEntity, "upload_quarantined", "DRC-003: MALWARE_QUARANTINED: "+scanResult.Reason)
		return
	}

	tempKey := newStorageKey()
	checksum, perr := h.storage.Put(r.Context(), tempKey, content)
	if perr != nil {
		h.log.Error("Complete reservation: storage write failed", zap.Error(perr))
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "")
		return
	}
	v := &domain.DocumentVersion{
		ChecksumSHA256: checksum, StorageKey: tempKey, SizeBytes: int64(len(content)),
		ContentType: res.DeclaredContentType, DetectedContentType: detected, CreatedByPrincipalID: actor,
	}
	committedRes, version, already, err := h.store.CompleteUploadReservation(r.Context(), res.ReservationID, v, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.cleanupBlob("CompleteUploadReservation", tempKey)
		h.handleReservationError(w, err)
		return
	}
	if already {
		// A concurrent completion committed first; this request's blob is not
		// referenced by anything.
		h.cleanupBlob("CompleteUploadReservation", tempKey)
	}
	h.writeCommitted(w, r, committedRes, doc, version, already)
}

// writeCommitted answers with the committed version: 201 on the completion that
// committed it, 200 (with Idempotency-Replay) on every later one.
func (h *reservationHandler) writeCommitted(w http.ResponseWriter, r *http.Request, res *domain.UploadReservation, doc *domain.Document, v *domain.DocumentVersion, replay bool) {
	if v == nil {
		if res.CommittedVersionID == nil {
			writeError(w, http.StatusInternalServerError, "store_unavailable", "")
			return
		}
		found, err := h.store.FindVersionByID(r.Context(), *res.CommittedVersionID)
		if err != nil {
			h.handleStoreError(w, err)
			return
		}
		v = found
	}
	view, err := h.buildVersionView(r.Context(), v, doc)
	if err != nil {
		h.handleStoreError(w, err)
		return
	}
	status := http.StatusCreated
	if replay {
		w.Header().Set("Idempotency-Replay", "true")
		status = http.StatusOK
	}
	writeJSON(w, status, completionView{ReservationID: res.ReservationID, DocumentID: res.DocumentID, CommitStatus: res.CommitStatus, Version: view})
}

func (h *reservationHandler) writeReservationClosed(w http.ResponseWriter, res *domain.UploadReservation) {
	prefix := "DRC-002: UPLOAD_VALIDATION_FAILED: "
	if res.CommitStatus == domain.ReservationQuarantined {
		prefix = "DRC-003: MALWARE_QUARANTINED: "
	}
	writeError(w, http.StatusConflict, "reservation_closed", prefix+res.FailureReason)
}

// closeReservation records a terminal failure; a failure to record it is logged
// and does not change the response (the reservation stays RESERVED and the
// caller can still be refused again on retry).
func (h *reservationHandler) closeReservation(r *http.Request, id, status, reason string) {
	if _, err := h.store.CloseUploadReservation(r.Context(), id, status, reason); err != nil {
		h.log.Error("failed to close upload reservation", zap.String("reservation_id", id), zap.String("status", status), zap.Error(err))
	}
}

func (h *reservationHandler) closeAndRefuse(w http.ResponseWriter, r *http.Request, id, status, reason string, httpStatus int, code, detail string) {
	h.closeReservation(r, id, status, reason)
	writeError(w, httpStatus, code, detail)
}

func (h *reservationHandler) handleReservationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrUploadReservationNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrUploadReservationClosed):
		writeError(w, http.StatusConflict, "reservation_closed", err.Error())
	case errors.Is(err, domain.ErrIdempotencyMismatch):
		writeError(w, http.StatusConflict, "idempotency_mismatch", domain.ErrIdempotencyMismatch.Error())
	default:
		h.handleStoreError(w, err)
	}
}

// reservationRequestHash fingerprints a reservation request. The principal is not
// part of it: the idempotency key is already scoped to (tenant, principal).
func reservationRequestHash(documentID string, req reserveUploadRequest) string {
	sum := sha256.Sum256([]byte(documentID + "\x00" + req.Purpose + "\x00" + strconv.FormatInt(req.ExpectedSizeBytes, 10) + "\x00" + req.DeclaredContentType))
	return hex.EncodeToString(sum[:])
}
