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

	"zoiko.io/forecasting-svc/internal/domain"
	svcenvelope "zoiko.io/forecasting-svc/internal/envelope"
	customMiddleware "zoiko.io/forecasting-svc/internal/middleware"
	"zoiko.io/forecasting-svc/internal/store"
)

// Request helpers shared by every handler. Each returns ok=false after it has
// already written the problem response, so handlers read top-to-bottom:
//
//	actorID, ok := h.requireActor(w, r)
//	if !ok { return }

// maxRequestBodyBytes bounds every request body.
const maxRequestBodyBytes = 1 << 20 // 1 MiB

// requireActor returns the acting principal (X-Principal-Id, or X-Workload-Id
// for a workload). A request without one never passed identity verification.
func (h *Handler) requireActor(w http.ResponseWriter, r *http.Request) (string, bool) {
	actor := svcenvelope.Parse(r).Actor()
	if actor == "" {
		writeProblem(w, r, http.StatusUnauthorized, "missing_principal", "X-Principal-Id (or X-Workload-Id) is required", "")
		return "", false
	}
	return actor, true
}

// tenantOf returns the gateway-verified tenant placed on the context by
// TenantMiddleware. Routes are only mounted behind that middleware, so it is
// never empty here.
func tenantOf(r *http.Request) string {
	return customMiddleware.GetTenantID(r.Context())
}

// legalEntityHeader returns X-Legal-Entity-Id ("" when absent).
func legalEntityHeader(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(svcenvelope.HeaderLegalEntityID))
}

// authorizationScope is the legal entity sent to authorization-svc: the
// resource's own legal entity when known, else the header, else the tenant
// (tenant-wide configuration such as model releases).
func authorizationScope(r *http.Request, resourceLegalEntityID string) string {
	if resourceLegalEntityID != "" {
		return resourceLegalEntityID
	}
	if header := legalEntityHeader(r); header != "" {
		return header
	}
	return tenantOf(r)
}

// requireUUIDPathParam validates a path id that must be a UUID (400 otherwise).
func requireUUIDPathParam(w http.ResponseWriter, r *http.Request, name, value string) bool {
	if !domain.IsValidUUID(value) {
		writeProblem(w, r, http.StatusBadRequest, "invalid_identifier", name+" must be a UUID", "")
		return false
	}
	return true
}

// requirePrefixedPathParam validates a path id like "faj_<uuid>" (400 otherwise).
func requirePrefixedPathParam(w http.ResponseWriter, r *http.Request, name, value, prefix string) bool {
	if !domain.IsValidPrefixedID(value, prefix) {
		writeProblem(w, r, http.StatusBadRequest, "invalid_identifier", name+" must look like "+prefix+"<uuid>", "")
		return false
	}
	return true
}

// readBody reads the raw request body (bounded). required=true refuses an
// empty body with 400.
func readBody(w http.ResponseWriter, r *http.Request, required bool) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "body_unreadable", "request body could not be read or exceeds 1 MiB", "")
		return nil, false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 && required {
		writeProblem(w, r, http.StatusBadRequest, "body_required", "a JSON request body is required", "")
		return nil, false
	}
	return raw, true
}

// decodeJSON parses raw into dest.
//
//   - Invalid JSON syntax -> 400 malformed_json.
//   - Valid JSON with a wrong field type -> 422 with the field pointer.
//   - A body "tenant_id" that differs from X-Tenant-Id -> 403: the tenant comes
//     only from the verified header; a body may at most agree with it.
//
// An empty raw body leaves dest untouched (used by optional-body commands).
func decodeJSON(w http.ResponseWriter, r *http.Request, raw []byte, dest interface{}) bool {
	if len(raw) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			writeProblem(w, r, http.StatusUnprocessableEntity, "invalid_field_type",
				fmt.Sprintf("field %q has the wrong type", typeErr.Field), typeErr.Field)
			return false
		}
		writeProblem(w, r, http.StatusBadRequest, "malformed_json", "request body is not valid JSON", "")
		return false
	}

	var tenantProbe struct {
		TenantID *string `json:"tenant_id"`
	}
	if err := json.Unmarshal(raw, &tenantProbe); err == nil && tenantProbe.TenantID != nil {
		if *tenantProbe.TenantID != tenantOf(r) {
			writeProblem(w, r, http.StatusForbidden, "tenant_mismatch",
				"body tenant_id does not match the verified X-Tenant-Id; the tenant is taken from the header only", "tenant_id")
			return false
		}
	}
	return true
}

// validate runs a request's Validate method and writes 422 on failure.
func (h *Handler) validate(w http.ResponseWriter, r *http.Request, validatable interface{ Validate() error }) bool {
	if err := validatable.Validate(); err != nil {
		h.writeError(w, r, err)
		return false
	}
	return true
}

// ── Idempotency and concurrency ─────────────────────────────────────────────

// buildCommand assembles the store.Command for a write request.
//
// The idempotency fingerprint covers the operation name, the target resource
// and the exact body, so the same Idempotency-Key replayed with a different
// body (or against a different resource) is refused with 409.
func buildCommand(r *http.Request, actorID, operation, targetID string, rawBody []byte, expectedVersion int) store.Command {
	fingerprint := sha256.New()
	fingerprint.Write([]byte(operation))
	fingerprint.Write([]byte{0})
	fingerprint.Write([]byte(targetID))
	fingerprint.Write([]byte{0})
	fingerprint.Write(rawBody)

	return store.Command{
		TenantID:      tenantOf(r),
		LegalEntityID: legalEntityHeader(r),
		ActorID:       actorID,
		CorrelationID: r.Header.Get(svcenvelope.HeaderCorrelationID),
		Idempotency: domain.IdempotencyClaim{
			Key:           strings.TrimSpace(r.Header.Get(svcenvelope.HeaderIdempotencyKey)),
			PrincipalID:   actorID,
			Operation:     operation,
			RequestSHA256: hex.EncodeToString(fingerprint.Sum(nil)),
		},
		ExpectedVersion: expectedVersion,
	}
}

// asReplay reports whether err is an idempotent replay and marks the response.
func asReplay(w http.ResponseWriter, err error) (*domain.IdempotentReplay, bool) {
	var replay *domain.IdempotentReplay
	if errors.As(err, &replay) {
		w.Header().Set("Idempotent-Replayed", "true")
		return replay, true
	}
	return nil, false
}

// etagFor renders an aggregate version as a strong ETag: "v<version>".
func etagFor(version int) string {
	return fmt.Sprintf(`"v%d"`, version)
}

// requireIfMatch parses the mandatory If-Match header for a state transition.
// Missing -> 428; unparseable -> 400. Accepted forms: "v3", v3, "3", W/"v3".
func requireIfMatch(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		writeProblem(w, r, http.StatusPreconditionRequired, "precondition_required",
			"If-Match is required for this state transition; send the ETag from the latest GET", "")
		return 0, false
	}
	version, ok := parseETagVersion(raw)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "invalid_if_match", `If-Match must be an ETag like "v3"`, "")
		return 0, false
	}
	return version, true
}

// optionalIfMatch parses If-Match when present (0 when absent).
func optionalIfMatch(w http.ResponseWriter, r *http.Request) (int, bool) {
	if strings.TrimSpace(r.Header.Get("If-Match")) == "" {
		return 0, true
	}
	return requireIfMatch(w, r)
}

func parseETagVersion(raw string) (int, bool) {
	value := strings.TrimPrefix(raw, "W/")
	value = strings.Trim(value, `"`)
	value = strings.TrimPrefix(value, "v")
	version, err := strconv.Atoi(value)
	if err != nil || version < 1 {
		return 0, false
	}
	return version, true
}

// ── Pagination ──────────────────────────────────────────────────────────────

// parsePageRequest reads ?limit= (1..200, default 50) and ?cursor=.
func parsePageRequest(w http.ResponseWriter, r *http.Request) (domain.PageRequest, bool) {
	page := domain.PageRequest{Limit: domain.DefaultPageLimit}
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > domain.MaxPageLimit {
			writeProblem(w, r, http.StatusBadRequest, "invalid_query", "limit must be an integer between 1 and 200", "")
			return page, false
		}
		page.Limit = limit
	}
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		createdAt, id, err := domain.DecodeCursor(cursor)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "invalid_query", "cursor is not a valid page cursor", "")
			return page, false
		}
		page.AfterCreatedAt = createdAt
		page.AfterID = id
		page.HasCursorFilter = true
	}
	return page, true
}

// invalidQuery writes the 400 for a bad query parameter.
func invalidQuery(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblem(w, r, http.StatusBadRequest, "invalid_query", detail, "")
}

// ── Responses ───────────────────────────────────────────────────────────────

func respondJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// listResponse is the body of every list endpoint.
type listResponse struct {
	Data       interface{} `json:"data"`
	Count      int         `json:"count"`
	NextCursor string      `json:"next_cursor,omitempty"`
}
