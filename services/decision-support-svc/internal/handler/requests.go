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
	"github.com/google/uuid"

	"zoiko.io/decision-support-svc/internal/domain"
	svcmiddleware "zoiko.io/decision-support-svc/internal/middleware"
)

// maxRequestBytes caps a JSON request body.
const maxRequestBytes = 256 << 10 // 256 KiB

// callerFromRequest reads who is calling. Tenant and principal are set by
// gateway-auth-svc; neither is ever defaulted or fabricated here.
func callerFromRequest(r *http.Request) (domain.CallerContext, error) {
	tenantID := strings.TrimSpace(svcmiddleware.TenantFromContext(r.Context()))
	if tenantID == "" {
		return domain.CallerContext{}, domain.ErrTenantMissing
	}
	principalID := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if principalID == "" {
		return domain.CallerContext{}, domain.ErrPrincipalMissing
	}
	return domain.CallerContext{
		TenantID:      tenantID,
		PrincipalID:   principalID,
		LegalEntityID: strings.TrimSpace(r.Header.Get("X-Legal-Entity-Id")),
		CorrelationID: strings.TrimSpace(r.Header.Get("X-Correlation-ID")),
		RequestID:     strings.TrimSpace(r.Header.Get("X-Request-Id")),
	}, nil
}

// readBody reads the raw request body (capped). The raw bytes are kept
// because the idempotency hash is taken over exactly what was sent.
func readBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		return nil, domain.NewFieldError("body", "could not be read")
	}
	if len(raw) > maxRequestBytes {
		return nil, domain.NewFieldError("body", "exceeds 256 KiB")
	}
	return raw, nil
}

// decodeJSON decodes raw into target, refusing malformed JSON, unknown
// fields and trailing data. An empty body is allowed only when emptyOK.
func decodeJSON(raw []byte, target any, emptyOK bool) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		if emptyOK {
			return nil
		}
		return domain.NewFieldError("body", "is required")
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return domain.NewFieldError("body", "is not valid JSON for this request: "+err.Error())
	}
	if decoder.More() {
		return domain.NewFieldError("body", "must contain a single JSON object")
	}
	return nil
}

// idempotencyFromRequest reads Idempotency-Key and hashes the request it
// came with: method, path, principal, If-Match and the raw body. The
// envelope middleware has already refused a write without the key; the
// check here keeps the handler correct even if that middleware is
// configured in observe mode.
func idempotencyFromRequest(r *http.Request, principalID string, rawBody []byte) (domain.IdempotencyInput, error) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		return domain.IdempotencyInput{}, domain.NewFieldError("Idempotency-Key", "header is required for writes")
	}
	if len(key) > 255 {
		return domain.IdempotencyInput{}, domain.NewFieldError("Idempotency-Key", "must be at most 255 characters")
	}

	hasher := sha256.New()
	for _, part := range []string{r.Method, r.URL.Path, principalID, r.Header.Get("If-Match")} {
		hasher.Write([]byte(part))
		hasher.Write([]byte{0})
	}
	hasher.Write(rawBody)

	return domain.IdempotencyInput{Key: key, RequestHash: hex.EncodeToString(hasher.Sum(nil))}, nil
}

// recommendationIDFromPath reads and validates {recommendation_id}.
func recommendationIDFromPath(r *http.Request) (string, error) {
	id := chi.URLParam(r, "recommendation_id")
	if _, err := uuid.Parse(id); err != nil || len(id) != 36 {
		return "", domain.NewFieldError("recommendation_id", "must be a UUID")
	}
	return strings.ToLower(id), nil
}

// expectedVersionFromRequest reads If-Match. Decisions require it.
func expectedVersionFromRequest(r *http.Request) (int, error) {
	value := strings.TrimSpace(r.Header.Get("If-Match"))
	if value == "" {
		return 0, domain.ErrPreconditionRequired
	}
	version, ok := domain.ParseETag(value)
	if !ok {
		return 0, domain.NewFieldError("If-Match", `must be the recommendation's ETag, e.g. "1"`)
	}
	return version, nil
}

// listFilterFromQuery reads and validates the GET /v1/recommendations query.
func listFilterFromQuery(r *http.Request) (domain.ListFilter, error) {
	query := r.URL.Query()
	filter := domain.ListFilter{
		LegalEntityID:    strings.TrimSpace(query.Get("legal_entity_id")),
		SubjectType:      strings.TrimSpace(query.Get("subject_type")),
		SubjectReference: strings.TrimSpace(query.Get("subject_reference")),
		ActionType:       strings.TrimSpace(query.Get("action_type")),
		Limit:            domain.DefaultPageSize,
	}

	if filter.LegalEntityID != "" {
		if _, err := uuid.Parse(filter.LegalEntityID); err != nil {
			return domain.ListFilter{}, domain.NewFieldError("legal_entity_id", "must be a UUID")
		}
	}

	if raw := query.Get("status"); raw != "" {
		status, known := domain.ParseStatus(raw)
		if !known {
			return domain.ListFilter{}, domain.NewFieldError("status", "must be one of REVIEW_REQUIRED, ACCEPTED, REJECTED, EXPIRED, SUPERSEDED")
		}
		filter.Status = status
	}

	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > domain.MaxPageSize {
			return domain.ListFilter{}, domain.NewFieldError("limit", "must be an integer from 1 to 200")
		}
		filter.Limit = limit
	}

	if raw := query.Get("cursor"); raw != "" {
		cursor, ok := domain.DecodeCursor(raw)
		if !ok {
			return domain.ListFilter{}, domain.NewFieldError("cursor", "is not a cursor returned by this API")
		}
		filter.After = cursor
	}
	return filter, nil
}
