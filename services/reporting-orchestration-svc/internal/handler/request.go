package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"zoiko.io/reporting-orchestration-svc/internal/domain"
	"zoiko.io/reporting-orchestration-svc/internal/middleware"
	"zoiko.io/reporting-orchestration-svc/internal/store"
)

// Request-parsing helpers shared by every handler. Each helper writes the
// problem response itself and returns ok=false, so handlers read as a flat
// list of "if !ok { return }" steps.

const (
	maxRequestBodyBytes = 1 << 20 // 1 MiB
	defaultPageLimit    = 50
	maxPageLimit        = 200
	maxPageOffset       = 100000
)

// requestContext is the verified caller context every handler starts from.
type requestContext struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string // X-Legal-Entity-Id (may be empty on reads)
	CorrelationID string
}

// requireCaller reads the gateway-verified tenant (set by TenantMiddleware)
// and principal. A request without a principal never passed identity
// verification: 401, never a fabricated identity.
func (h *Handler) requireCaller(w http.ResponseWriter, r *http.Request) (requestContext, bool) {
	tenantID := middleware.GetTenantID(r.Context())
	if tenantID == "" {
		writeProblem(w, r, http.StatusUnauthorized, CodeTenantRequired, "X-Tenant-Id header is required")
		return requestContext{}, false
	}
	principalID := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if principalID == "" {
		writeProblem(w, r, http.StatusUnauthorized, CodePrincipalRequired, "X-Principal-Id header is required")
		return requestContext{}, false
	}
	return requestContext{
		TenantID:      tenantID,
		PrincipalID:   principalID,
		LegalEntityID: strings.TrimSpace(r.Header.Get("X-Legal-Entity-Id")),
		CorrelationID: r.Header.Get("X-Correlation-ID"),
	}, true
}

// requireUUIDParam reads a chi URL parameter that must be a UUID (400 if not).
func requireUUIDParam(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	value := chi.URLParam(r, name)
	parsed, err := uuid.Parse(value)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, CodeInvalidID, fmt.Sprintf("%s must be a UUID", name))
		return "", false
	}
	return parsed.String(), true
}

// errEmptyBody is returned by readJSONBody when the body is empty.
var errEmptyBody = errors.New("request body is empty")

// readJSONBody strictly decodes the request body into target: unknown fields,
// trailing data and malformed JSON are all rejected.
func readJSONBody(r *http.Request, target interface{}) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		return fmt.Errorf("could not read request body: %w", err)
	}
	if len(raw) > maxRequestBodyBytes {
		return fmt.Errorf("request body exceeds %d bytes", maxRequestBodyBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return errEmptyBody
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

// requireJSONBody decodes a mandatory JSON body (400 MALFORMED_JSON on error).
func requireJSONBody(w http.ResponseWriter, r *http.Request, target interface{}) bool {
	if err := readJSONBody(r, target); err != nil {
		writeProblem(w, r, http.StatusBadRequest, CodeMalformedJSON, "request body is not valid JSON for this operation: "+err.Error())
		return false
	}
	return true
}

// optionalJSONBody decodes a body that may be empty.
func optionalJSONBody(w http.ResponseWriter, r *http.Request, target interface{}) bool {
	err := readJSONBody(r, target)
	if err == nil || errors.Is(err, errEmptyBody) {
		return true
	}
	writeProblem(w, r, http.StatusBadRequest, CodeMalformedJSON, "request body is not valid JSON for this operation: "+err.Error())
	return false
}

// requireTenantAgreement enforces "tenant only from X-Tenant-Id": a body
// tenant_id may be omitted, or must equal the header (403 otherwise).
func requireTenantAgreement(w http.ResponseWriter, r *http.Request, caller requestContext, bodyTenantID string) bool {
	if bodyTenantID != "" && bodyTenantID != caller.TenantID {
		writeProblem(w, r, http.StatusForbidden, CodeTenantMismatch, "tenant_id in the body does not match X-Tenant-Id")
		return false
	}
	return true
}

// resolveLegalEntity returns the legal entity for a create: the body value,
// defaulting to X-Legal-Entity-Id. When both are present they must agree.
func resolveLegalEntity(w http.ResponseWriter, r *http.Request, caller requestContext, bodyLegalEntityID string) (string, bool) {
	bodyLegalEntityID = strings.TrimSpace(bodyLegalEntityID)
	if bodyLegalEntityID == "" {
		return caller.LegalEntityID, true
	}
	if caller.LegalEntityID != "" && caller.LegalEntityID != bodyLegalEntityID {
		writeProblem(w, r, http.StatusForbidden, CodeLegalEntityMismatch, "legal_entity_id in the body does not match X-Legal-Entity-Id")
		return "", false
	}
	return bodyLegalEntityID, true
}

// parseIfMatch returns the expected version from If-Match, or 0 when the
// header is absent or "*". A malformed value is 400.
func parseIfMatch(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" || raw == "*" {
		return 0, true
	}
	raw = strings.TrimPrefix(raw, "W/")
	raw = strings.Trim(raw, `"`)
	version, err := strconv.Atoi(raw)
	if err != nil || version < 1 {
		writeProblem(w, r, http.StatusBadRequest, CodeInvalidIfMatch, `If-Match must be an ETag previously returned by this service, e.g. "3"`)
		return 0, false
	}
	return version, true
}

// setETag sets the ETag for a versioned resource.
func setETag(w http.ResponseWriter, version int) {
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, version))
}

// parsePage reads ?limit= and ?offset= with bounds (400 when out of range).
func parsePage(w http.ResponseWriter, r *http.Request) (store.Page, bool) {
	page := store.Page{Limit: defaultPageLimit, Offset: 0}
	query := r.URL.Query()

	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxPageLimit {
			writeProblem(w, r, http.StatusBadRequest, CodeInvalidQuery, fmt.Sprintf("limit must be an integer between 1 and %d", maxPageLimit))
			return page, false
		}
		page.Limit = limit
	}
	if raw := query.Get("offset"); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 || offset > maxPageOffset {
			writeProblem(w, r, http.StatusBadRequest, CodeInvalidQuery, fmt.Sprintf("offset must be an integer between 0 and %d", maxPageOffset))
			return page, false
		}
		page.Offset = offset
	}
	return page, true
}

// listResponse is the envelope for every list endpoint.
type listResponse struct {
	Data   interface{} `json:"data"`
	Count  int         `json:"count"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

// newEvent builds an outbox event stamped with the caller's context.
func newEvent(caller requestContext, eventType, aggregateType, aggregateID, legalEntityID string) domain.OutboxEvent {
	return domain.OutboxEvent{
		EventType:     eventType,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		TenantID:      caller.TenantID,
		LegalEntityID: legalEntityID,
		ActorID:       caller.PrincipalID,
		CorrelationID: caller.CorrelationID,
	}
}

// validationProblem writes a 400 VALIDATION_FAILED.
func validationProblem(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblem(w, r, http.StatusBadRequest, CodeValidationFailed, detail)
}
