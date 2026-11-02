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
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
	customMiddleware "zoiko.io/compliance-risk-scoring-svc/internal/middleware"
	"zoiko.io/compliance-risk-scoring-svc/internal/service"
	"zoiko.io/compliance-risk-scoring-svc/internal/store"
)

// maxBodyBytes caps request bodies; larger bodies get 413.
const maxBodyBytes = 1 << 20

// maxPrincipalIDLength matches the VARCHAR(128) actor columns.
const maxPrincipalIDLength = 128

// requirePrincipal reads X-Principal-Id. Missing -> 401: the gateway sets it
// from the verified identity, so its absence means authentication failed.
func requirePrincipal(r *http.Request) (string, error) {
	principalID := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if principalID == "" {
		return "", &domain.Error{Kind: domain.KindUnauthenticated, Code: "missing_principal",
			Detail: "X-Principal-Id header is required"}
	}
	if len(principalID) > maxPrincipalIDLength {
		return "", domain.BadRequest("invalid_principal_id", "X-Principal-Id must be at most 128 characters")
	}
	return principalID, nil
}

// queryContext builds the caller context for a read: tenant (already
// verified by TenantMiddleware) plus a mandatory principal.
func queryContext(r *http.Request) (service.QueryContext, error) {
	if _, err := requirePrincipal(r); err != nil {
		return service.QueryContext{}, err
	}
	return service.QueryContext{TenantID: customMiddleware.GetTenantID(r.Context())}, nil
}

// commandContext builds the caller context for a write from the envelope
// headers and the raw body (for the idempotency request hash).
func commandContext(r *http.Request, rawBody []byte) (service.CommandContext, error) {
	principalID, err := requirePrincipal(r)
	if err != nil {
		return service.CommandContext{}, err
	}

	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		return service.CommandContext{}, domain.BadRequest("idempotency_key_required",
			"Idempotency-Key header is required for this write")
	}
	if len(idempotencyKey) > 255 {
		return service.CommandContext{}, domain.BadRequest("invalid_idempotency_key", "Idempotency-Key must be at most 255 characters")
	}

	cmd := service.CommandContext{
		TenantID:         customMiddleware.GetTenantID(r.Context()),
		PrincipalID:      principalID,
		LegalEntityScope: strings.TrimSpace(r.Header.Get("X-Legal-Entity-Id")),
		RequestID:        r.Header.Get("X-Request-Id"),
		CorrelationID:    r.Header.Get("X-Correlation-ID"),
		IdempotencyKey:   idempotencyKey,
		RequestHash:      requestHash(principalID, r.Method, r.URL.Path, rawBody),
	}

	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if ifMatch != "" {
		cmd.IfMatchPresent = true
		if ifMatch != "*" {
			version, parseErr := parseETagVersion(ifMatch)
			if parseErr != nil {
				return service.CommandContext{}, domain.PreconditionFailed("If-Match is not an ETag issued by this service")
			}
			cmd.IfMatchVersion = &version
		}
	}
	return cmd, nil
}

// requestHash fingerprints a write so a reused Idempotency-Key with a
// different principal, route or body is detected. JSON bodies are compacted
// first so whitespace differences do not count as a different request.
func requestHash(principalID, method, path string, rawBody []byte) string {
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, rawBody); err != nil {
		compacted.Reset()
		compacted.Write(rawBody)
	}
	digest := sha256.New()
	digest.Write([]byte(principalID + "\n" + method + " " + path + "\n"))
	digest.Write(compacted.Bytes())
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

// parseETagVersion accepts `"3"`, `W/"3"` or `3`.
func parseETagVersion(etag string) (int, error) {
	text := strings.TrimPrefix(etag, "W/")
	text = strings.Trim(text, `"`)
	version, err := strconv.Atoi(text)
	if err != nil || version < 1 {
		return 0, errors.New("invalid etag")
	}
	return version, nil
}

// readBody reads the raw body (bounded). An empty body is returned as nil.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	limited := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	raw, err := io.ReadAll(limited)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, &domain.Error{Kind: domain.KindValidation, Code: "body_too_large", Detail: "request body exceeds 1 MiB"}
		}
		return nil, domain.BadRequest("unreadable_body", "request body could not be read")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	return raw, nil
}

// decodeJSON decodes raw into target strictly: malformed JSON, unknown
// fields, wrong types and trailing data are all 400s. An empty body is an
// error unless optional is true.
func decodeJSON(raw []byte, target any, optional bool) error {
	if raw == nil {
		if optional {
			return nil
		}
		return domain.BadRequest("missing_body", "a JSON request body is required")
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return describeDecodeError(err)
	}
	if decoder.More() {
		return domain.BadRequest("malformed_json", "request body must contain exactly one JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return domain.BadRequest("malformed_json", "request body must contain exactly one JSON object")
	}
	return nil
}

func describeDecodeError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return domain.ValidationError([]domain.FieldError{{Field: typeErr.Field, Detail: "has the wrong type: expected " + typeErr.Type.String()}})
	}
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return domain.ValidationError([]domain.FieldError{{Field: field, Detail: "is not a recognised field"}})
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) || errors.Is(err, io.ErrUnexpectedEOF) {
		return domain.BadRequest("malformed_json", "request body is not valid JSON")
	}
	// Errors from custom UnmarshalJSON (e.g. a decimal with 3 places).
	return domain.BadRequest("invalid_value", err.Error())
}

// readCommand reads the body, decodes it into target and builds the
// command context — the common prologue of every write handler.
func readCommand(w http.ResponseWriter, r *http.Request, target any, bodyOptional bool) (service.CommandContext, error) {
	raw, err := readBody(w, r)
	if err != nil {
		return service.CommandContext{}, err
	}
	cmd, err := commandContext(r, raw)
	if err != nil {
		return service.CommandContext{}, err
	}
	if err := decodeJSON(raw, target, bodyOptional); err != nil {
		return service.CommandContext{}, err
	}
	return cmd, nil
}

// uuidParam reads a {name} path parameter that must be a UUID.
func uuidParam(r *http.Request, name string) (string, error) {
	value := chi.URLParam(r, name)
	parsed, err := uuid.Parse(value)
	if err != nil {
		return "", domain.BadRequest("invalid_id", name+" must be a UUID")
	}
	return parsed.String(), nil
}

// pageRequest parses ?limit= (1..200, default 50) and ?cursor=.
func pageRequest(r *http.Request) (store.PageRequest, error) {
	page := store.PageRequest{Limit: store.DefaultPageLimit}

	if limitText := r.URL.Query().Get("limit"); limitText != "" {
		limit, err := strconv.Atoi(limitText)
		if err != nil || limit < 1 || limit > store.MaxPageLimit {
			return page, domain.BadRequest("invalid_limit", "limit must be an integer between 1 and 200")
		}
		page.Limit = limit
	}

	if cursorText := r.URL.Query().Get("cursor"); cursorText != "" {
		cursor, err := store.DecodeCursor(cursorText)
		if err != nil {
			return page, domain.BadRequest("invalid_cursor", "cursor is not valid; use next_cursor from a previous page")
		}
		page.Cursor = cursor
	}
	return page, nil
}

// enumQuery reads an optional, upper-cased enum query parameter and checks
// it with isValid.
func enumQuery(r *http.Request, name string, isValid func(string) bool, allowed string) (string, error) {
	value := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get(name)))
	if value == "" {
		return "", nil
	}
	if !isValid(value) {
		return "", domain.BadRequest("invalid_filter", name+" must be one of "+allowed)
	}
	return value, nil
}

// dateQuery reads an optional YYYY-MM-DD query parameter.
func dateQuery(r *http.Request, name string) (string, error) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return "", nil
	}
	if _, err := time.Parse(domain.DateLayout, value); err != nil {
		return "", domain.BadRequest("invalid_date", name+" must be a date in YYYY-MM-DD format")
	}
	return value, nil
}

// writeCommandResult writes a write's result (fresh or replayed).
func writeCommandResult(w http.ResponseWriter, result *service.CommandResult) {
	w.Header().Set("Content-Type", "application/json")
	if result.ETag != "" {
		w.Header().Set("ETag", result.ETag)
	}
	if result.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.WriteHeader(result.StatusCode)
	_, _ = w.Write(result.Body)
}
