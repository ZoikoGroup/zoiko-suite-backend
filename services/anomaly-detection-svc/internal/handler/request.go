package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"zoiko.io/anomaly-detection-svc/internal/domain"
	"zoiko.io/anomaly-detection-svc/internal/middleware"
)

// maxBodyBytes bounds a request body (a RunDetection with 500 signals fits
// comfortably).
const maxBodyBytes = 2 << 20

// ─────────────────────────────────────────────────────────────────────────────
// Caller scope
// ─────────────────────────────────────────────────────────────────────────────

// requestScope is the verified identity of the caller, read only from the
// gateway-set headers.
type requestScope struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string
	CorrelationID string
}

// authzScope is the scope sent to authorization-svc: the legal entity when
// the caller named one, otherwise the tenant.
func (s requestScope) authzScope() string {
	if s.LegalEntityID != "" {
		return s.LegalEntityID
	}
	return s.TenantID
}

// readScope returns the caller's scope, or writes 401 and returns false when
// the principal is missing. (A missing tenant never reaches a handler: the
// TenantContext middleware already refused it with 401.)
func readScope(w http.ResponseWriter, r *http.Request) (requestScope, bool) {
	scope := requestScope{
		TenantID:      middleware.GetTenantID(r.Context()),
		PrincipalID:   strings.TrimSpace(r.Header.Get("X-Principal-Id")),
		LegalEntityID: strings.TrimSpace(r.Header.Get("X-Legal-Entity-Id")),
		CorrelationID: r.Header.Get("X-Correlation-ID"),
	}
	if scope.TenantID == "" {
		writeProblem(w, r, http.StatusUnauthorized, "missing_tenant_scope", "X-Tenant-Id is required")
		return requestScope{}, false
	}
	if scope.PrincipalID == "" {
		writeProblem(w, r, http.StatusUnauthorized, "missing_principal", "X-Principal-Id is required: the gateway sets it from a verified identity")
		return requestScope{}, false
	}
	return scope, true
}

// ─────────────────────────────────────────────────────────────────────────────
// Path parameters
// ─────────────────────────────────────────────────────────────────────────────

// pathID reads a path parameter and checks it is "<prefix><uuid>". On a bad
// id it writes 400 invalid_id and returns false.
func (h *Handler) pathID(w http.ResponseWriter, r *http.Request, param, prefix string) (string, bool) {
	id := chi.URLParam(r, param)
	if err := domain.ValidateID(prefix, id); err != nil {
		h.writeError(w, r, err)
		return "", false
	}
	return id, true
}

// ─────────────────────────────────────────────────────────────────────────────
// Body decoding
// ─────────────────────────────────────────────────────────────────────────────

// decodedBody is the raw request body plus its idempotency fingerprint.
type decodedBody struct {
	requestSHA256 string
}

// decodeBody reads the JSON body into target and checks it, writing the
// problem response itself and returning false on any failure:
//
//   - empty body when one is required          → 400 empty_body
//   - not JSON / wrong types / trailing data    → 400 malformed_json
//   - unknown field                             → 400 malformed_json
//   - "tenant_id" disagreeing with X-Tenant-Id  → 403 tenant_mismatch
//
// When allowEmpty is true an empty body is treated as {}.
func (h *Handler) decodeBody(w http.ResponseWriter, r *http.Request, scope requestScope, target any, allowEmpty bool) (decodedBody, bool) {
	rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "body_unreadable", "the request body could not be read or is larger than 2 MiB")
		return decodedBody{}, false
	}

	if len(bytes.TrimSpace(rawBody)) == 0 {
		if !allowEmpty {
			writeProblem(w, r, http.StatusBadRequest, "empty_body", "a JSON request body is required")
			return decodedBody{}, false
		}
		rawBody = []byte("{}")
	}

	decoder := json.NewDecoder(bytes.NewReader(rawBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "malformed_json", "the request body is not valid JSON for this endpoint: "+err.Error())
		return decodedBody{}, false
	}
	if decoder.More() {
		writeProblem(w, r, http.StatusBadRequest, "malformed_json", "the request body must contain exactly one JSON object")
		return decodedBody{}, false
	}

	if mismatch := bodyTenantMismatch(rawBody, scope.TenantID); mismatch {
		writeProblem(w, r, http.StatusForbidden, "tenant_mismatch",
			"tenant_id in the body does not match X-Tenant-Id; the tenant comes only from the verified header")
		return decodedBody{}, false
	}

	return decodedBody{requestSHA256: requestFingerprint(r, rawBody)}, true
}

// bodyTenantMismatch reports whether the body carries a tenant_id that
// differs from the verified header tenant.
func bodyTenantMismatch(rawBody []byte, headerTenantID string) bool {
	var scopeOnly struct {
		TenantID *string `json:"tenant_id"`
	}
	if err := json.Unmarshal(rawBody, &scopeOnly); err != nil {
		return false
	}
	if scopeOnly.TenantID == nil || *scopeOnly.TenantID == "" {
		return false
	}
	return *scopeOnly.TenantID != headerTenantID
}

// requestFingerprint hashes method, concrete path and compacted body, so the
// same key reused on another resource or with another body is detected.
func requestFingerprint(r *http.Request, rawBody []byte) string {
	var compactBody bytes.Buffer
	if err := json.Compact(&compactBody, rawBody); err != nil {
		compactBody.Reset()
		compactBody.Write(rawBody)
	}
	hasher := sha256.New()
	hasher.Write([]byte(r.Method + " " + r.URL.Path + "\n"))
	hasher.Write(compactBody.Bytes())
	return hex.EncodeToString(hasher.Sum(nil))
}

// validatable is implemented by every request type in the domain package.
type validatable interface {
	Validate() error
}

// validate runs the request's domain validation, writing 422 on failure.
func (h *Handler) validate(w http.ResponseWriter, r *http.Request, request validatable) bool {
	if err := request.Validate(); err != nil {
		h.writeError(w, r, err)
		return false
	}
	return true
}

// ─────────────────────────────────────────────────────────────────────────────
// Command context: Idempotency-Key and If-Match
// ─────────────────────────────────────────────────────────────────────────────

// requireIdempotencyKey returns the Idempotency-Key header, writing 400 when
// it is missing (the envelope middleware normally refuses first).
func requireIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeProblem(w, r, http.StatusBadRequest, "idempotency_key_required",
			"Idempotency-Key is required for every write so a retry cannot duplicate the effect")
		return "", false
	}
	if len(key) > 255 {
		writeProblem(w, r, http.StatusBadRequest, "idempotency_key_invalid", "Idempotency-Key must be at most 255 characters")
		return "", false
	}
	return key, true
}

// requireIfMatch parses If-Match into a version number. Missing → 428;
// not a version we ever issued → 412 (it cannot match).
func requireIfMatch(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		writeProblem(w, r, http.StatusPreconditionRequired, "precondition_required",
			"If-Match is required when changing an existing resource; send the ETag from the last GET")
		return 0, false
	}
	cleaned := strings.TrimPrefix(raw, "W/")
	cleaned = strings.Trim(cleaned, `"`)
	version, err := strconv.Atoi(cleaned)
	if err != nil || version < 1 {
		writeProblem(w, r, http.StatusPreconditionFailed, "stale_version",
			"If-Match "+raw+" does not match any version of this resource")
		return 0, false
	}
	return version, true
}

// newCommand assembles the store's CommandContext for one write.
func newCommand(r *http.Request, scope requestScope, idempotencyKey string, body decodedBody, expectedVersion *int) domain.CommandContext {
	return domain.CommandContext{
		TenantID:      scope.TenantID,
		LegalEntityID: scope.LegalEntityID,
		PrincipalID:   scope.PrincipalID,
		CorrelationID: scope.CorrelationID,
		Idempotency: domain.IdempotencyClaim{
			RouteTemplate: r.Method + " " + chi.RouteContext(r.Context()).RoutePattern(),
			Key:           idempotencyKey,
			RequestSHA256: body.requestSHA256,
		},
		ExpectedVersion: expectedVersion,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Query parameters
// ─────────────────────────────────────────────────────────────────────────────

// readPage parses ?limit= and ?cursor=, writing 400 on a bad value.
func (h *Handler) readPage(w http.ResponseWriter, r *http.Request) (domain.PageRequest, bool) {
	page := domain.PageRequest{Limit: domain.DefaultPageLimit}

	rawLimit := r.URL.Query().Get("limit")
	if rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > domain.MaxPageLimit {
			writeProblem(w, r, http.StatusBadRequest, "invalid_limit", "limit must be an integer between 1 and 200",
				ProblemField{Pointer: "limit", Code: "invalid_limit", Detail: "1..200"})
			return domain.PageRequest{}, false
		}
		page.Limit = limit
	}

	rawCursor := r.URL.Query().Get("cursor")
	if rawCursor != "" {
		cursor, err := domain.DecodeCursor(rawCursor)
		if err != nil {
			h.writeError(w, r, err)
			return domain.PageRequest{}, false
		}
		page.Cursor = cursor
	}
	return page, true
}

// queryEnum reads an optional enum query parameter, writing 400 when the
// value is not one of the allowed ones.
func queryEnum(w http.ResponseWriter, r *http.Request, name string, isValid func(string) bool) (string, bool) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return "", true
	}
	if !isValid(value) {
		writeProblem(w, r, http.StatusBadRequest, "invalid_query_parameter", name+" has an unsupported value: "+value,
			ProblemField{Pointer: name, Code: "invalid_value", Detail: value})
		return "", false
	}
	return value, true
}

// ─────────────────────────────────────────────────────────────────────────────
// Responses
// ─────────────────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// setETag sets the strong ETag "<version>" used by If-Match.
func setETag(w http.ResponseWriter, version int) {
	w.Header().Set("ETag", `"`+strconv.Itoa(version)+`"`)
}

// writeCommandResult writes the outcome of a write command: the given
// status (201 for creates, 200 for transitions) for both the first execution
// and an exact replay, plus Idempotent-Replayed: true on replay.
func writeCommandResult(w http.ResponseWriter, status int, replayed bool, version int, location string, body any) {
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	if version > 0 {
		setETag(w, version)
	}
	if location != "" {
		w.Header().Set("Location", location)
	}
	writeJSON(w, status, body)
}

// listResponse is the shape of every collection response.
type listResponse[T any] struct {
	Data       []T    `json:"data"`
	Count      int    `json:"count"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func writeList[T any](w http.ResponseWriter, items []T, nextCursor string) {
	if items == nil {
		items = []T{}
	}
	writeJSON(w, http.StatusOK, listResponse[T]{Data: items, Count: len(items), NextCursor: nextCursor})
}
