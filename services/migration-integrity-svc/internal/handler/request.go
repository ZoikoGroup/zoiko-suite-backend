package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"zoiko.io/migration-integrity-svc/internal/authz"
	"zoiko.io/migration-integrity-svc/internal/middleware"
	"zoiko.io/migration-integrity-svc/internal/problem"
	"zoiko.io/migration-integrity-svc/internal/store"
)

// maxBodyBytes bounds a request body (10 000 records with generous fields).
const maxBodyBytes = 16 << 20

// ─── Caller identity ──────────────────────────────────────────────────────────

// callerIdentity is who is calling, for which tenant. Both come only from
// gateway-set headers; neither is ever fabricated.
type callerIdentity struct {
	TenantID    string
	PrincipalID string
}

// requireCaller returns the verified tenant (set by TenantMiddleware) and the
// X-Principal-Id. A missing principal is a 401: the gateway sets it from the
// verified identity envelope, so its absence means the request is
// unauthenticated. ok=false means the response has been written.
func requireCaller(w http.ResponseWriter, r *http.Request) (callerIdentity, bool) {
	caller := callerIdentity{
		TenantID:    middleware.GetTenantID(r.Context()),
		PrincipalID: strings.TrimSpace(r.Header.Get(headerPrincipalID)),
	}
	if caller.PrincipalID == "" {
		problem.Write(w, r, http.StatusUnauthorized, "missing-principal", "Missing principal",
			"X-Principal-Id is required; the gateway sets it from a verified identity envelope")
		return caller, false
	}
	return caller, true
}

// ─── Path parameters ──────────────────────────────────────────────────────────

// requireUUIDParam reads a chi path parameter that must be a UUID. A
// malformed id is a 400, never a 404 or a database error.
func requireUUIDParam(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	raw := chi.URLParam(r, name)
	parsed, err := uuid.Parse(raw)
	if err != nil {
		writeBadRequest(w, r, fmt.Sprintf("path parameter %q must be a UUID, got %q", name, raw))
		return "", false
	}
	return parsed.String(), true
}

// ─── Body ─────────────────────────────────────────────────────────────────────

// readBody reads the whole body (bounded by maxBodyBytes). The raw bytes are
// needed twice: for the idempotency request hash and for decoding.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem.Write(w, r, http.StatusRequestEntityTooLarge, "body-too-large", "Request body too large",
				fmt.Sprintf("request body exceeds %d bytes; split the batch", maxBodyBytes))
			return nil, false
		}
		writeBadRequest(w, r, "request body could not be read")
		return nil, false
	}
	return body, true
}

// decodeStrict decodes JSON into target, refusing unknown fields and
// trailing content, so a misspelled field is a 400 rather than silently
// ignored.
func decodeStrict(w http.ResponseWriter, r *http.Request, body []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeBadRequest(w, r, "malformed JSON body: "+err.Error())
		return false
	}
	if decoder.More() {
		writeBadRequest(w, r, "malformed JSON body: unexpected content after the JSON document")
		return false
	}
	return true
}

// requireBodyTenantMatches enforces that a tenant named in the body agrees
// with X-Tenant-Id. The header is the only tenant authority; the body may
// only repeat it.
func requireBodyTenantMatches(w http.ResponseWriter, r *http.Request, bodyTenantID, headerTenantID string) bool {
	if bodyTenantID != "" && bodyTenantID != headerTenantID {
		writeForbidden(w, r, "tenant-mismatch", "tenant_id in the body does not match X-Tenant-Id")
		return false
	}
	return true
}

// requireLegalEntityHeaderMatches enforces that, on a write to an existing
// job, the X-Legal-Entity-Id the caller acts under is the job's entity.
func requireLegalEntityHeaderMatches(w http.ResponseWriter, r *http.Request, jobLegalEntityID string) bool {
	headerEntity := strings.TrimSpace(r.Header.Get(headerLegalEntityID))
	if headerEntity != "" && headerEntity != jobLegalEntityID {
		writeForbidden(w, r, "legal-entity-mismatch", "X-Legal-Entity-Id does not match the migration job's legal entity")
		return false
	}
	return true
}

// ─── Versioning (ETag / If-Match) ─────────────────────────────────────────────

// etagFor renders a job version as a strong ETag.
func etagFor(version int) string {
	return `"` + strconv.Itoa(version) + `"`
}

// parseIfMatch returns the version named by If-Match, or 0 when the header is
// absent or "*" (no precondition). `"3"`, `W/"3"` and `3` are accepted.
func parseIfMatch(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.Header.Get(headerIfMatch))
	if raw == "" || raw == "*" {
		return 0, true
	}
	cleaned := strings.Trim(strings.TrimPrefix(raw, "W/"), `"`)
	version, err := strconv.Atoi(cleaned)
	if err != nil || version < 1 {
		writeBadRequest(w, r, fmt.Sprintf("If-Match must name a job version such as \"3\", got %q", raw))
		return 0, false
	}
	return version, true
}

// ─── Idempotency ──────────────────────────────────────────────────────────────

// newIdempotencyClaim builds the claim for a write. The request hash covers
// the operation, method, path and body, so the same key sent with anything
// different is detected as a reuse.
func newIdempotencyClaim(r *http.Request, caller callerIdentity, operation string, body []byte) store.IdempotencyClaim {
	hasher := sha256.New()
	hasher.Write([]byte(operation + "\n" + r.Method + " " + r.URL.Path + "\n"))
	hasher.Write(body)
	return store.IdempotencyClaim{
		Key:         strings.TrimSpace(r.Header.Get(headerIdempotencyKey)),
		PrincipalID: caller.PrincipalID,
		Operation:   operation,
		RequestHash: hex.EncodeToString(hasher.Sum(nil)),
	}
}

// replayIfSeen answers a request whose Idempotency-Key already completed:
// the same request gets the stored response verbatim (with
// Idempotent-Replayed: true); a different request under the same key is a
// 409. Returns true when it wrote a response.
func (h *Handler) replayIfSeen(w http.ResponseWriter, r *http.Request, caller callerIdentity, claim store.IdempotencyClaim) bool {
	if claim.IsZero() {
		return false
	}
	stored, err := h.store.FindIdempotentResponse(r.Context(), caller.TenantID, caller.PrincipalID, claim.Key)
	if err != nil {
		h.writeError(w, r, err)
		return true
	}
	if stored == nil {
		return false
	}
	if stored.RequestHash != claim.RequestHash {
		problem.Write(w, r, http.StatusConflict, "idempotency-key-reused", "Idempotency-Key reused",
			"this Idempotency-Key was already used for a different request; use a new key")
		return true
	}
	w.Header().Set(headerReplayed, "true")
	writeRawJSON(w, stored.Status, stored.Body)
	return true
}

// handleConcurrentReplay is called when the store reports that a concurrent
// request with the same key committed first: replay its response.
func (h *Handler) handleConcurrentReplay(w http.ResponseWriter, r *http.Request, caller callerIdentity, claim store.IdempotencyClaim) {
	if !h.replayIfSeen(w, r, caller, claim) {
		problem.Write(w, r, http.StatusConflict, "idempotency-key-in-flight", "Idempotency-Key in use",
			"a request with this Idempotency-Key is still being processed; retry shortly")
	}
}

// ─── Authorization ────────────────────────────────────────────────────────────

// authorize asks authorization-svc whether the caller may perform action at
// legalEntityID. It fails closed: a denial is 403, and any failure to obtain
// a decision is 503 — never a silent allow.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, caller callerIdentity, legalEntityID, action string) bool {
	err := h.authz.CheckAllowed(r.Context(), caller.PrincipalID, legalEntityID, action)
	if err == nil {
		return true
	}
	if errors.Is(err, authz.ErrAuthorizationDenied) || errors.Is(err, authz.ErrTenantMissing) {
		h.writeError(w, r, err)
		return false
	}
	h.writeError(w, r, fmt.Errorf("%w: %v", authz.ErrAuthzServiceUnavailable, err))
	return false
}

// ─── Responses ────────────────────────────────────────────────────────────────

// writeJSON marshals payload exactly as the store does for idempotent replay
// (json.Marshal), so first response and replay are byte-identical.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		h.logger.Error("marshal response", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeRawJSON(w, status, body)
}

func writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
