// Package problem writes RFC 9457 problem+json error responses using the
// public error catalogue of Doc 05 §13. Details never contain stack traces,
// vendor names or internal architecture.
package problem

import (
	"encoding/json"
	"net/http"
)

// Code is the stable public error suffix; the wire value is "publicai.<code>".
type Code string

const (
	InvalidRequest            Code = "invalid_request"
	UnsupportedSchema         Code = "unsupported_schema"
	IdempotencyHeaderMismatch Code = "idempotency_header_mismatch"
	SessionExpired            Code = "session_expired"
	OriginDenied              Code = "origin_denied"
	ScopeDenied               Code = "scope_denied"
	NotFound                  Code = "not_found"
	TurnConflict              Code = "turn_conflict"
	PayloadTooLarge           Code = "payload_too_large"
	UnsupportedMediaType      Code = "unsupported_media_type"
	ValidationFailed          Code = "validation_failed"
	IdempotencyMismatch       Code = "idempotency_mismatch"
	RateLimited               Code = "rate_limited"
	DependencyFailed          Code = "dependency_failed"
	ServiceUnavailable        Code = "service_unavailable"
	Timeout                   Code = "timeout"
	Internal                  Code = "internal_error"
)

var catalogue = map[Code]struct {
	status    int
	title     string
	retryable bool
}{
	InvalidRequest:            {http.StatusBadRequest, "Invalid request", false},
	UnsupportedSchema:         {http.StatusBadRequest, "Unsupported schema version", false},
	IdempotencyHeaderMismatch: {http.StatusBadRequest, "Idempotency key does not match client_turn_id", false},
	SessionExpired:            {http.StatusUnauthorized, "Session expired", false},
	OriginDenied:              {http.StatusForbidden, "Origin not allowed", false},
	ScopeDenied:               {http.StatusForbidden, "Not permitted", false},
	NotFound:                  {http.StatusNotFound, "Not found", false},
	TurnConflict:              {http.StatusConflict, "A turn is already in progress", true},
	PayloadTooLarge:           {http.StatusRequestEntityTooLarge, "Payload too large", false},
	UnsupportedMediaType:      {http.StatusUnsupportedMediaType, "Content-Type must be application/json", false},
	ValidationFailed:          {http.StatusUnprocessableEntity, "Validation failed", false},
	IdempotencyMismatch:       {http.StatusUnprocessableEntity, "Idempotency key reused with a different request", false},
	RateLimited:               {http.StatusTooManyRequests, "Too many requests", true},
	DependencyFailed:          {http.StatusBadGateway, "Dependency failed", true},
	ServiceUnavailable:        {http.StatusServiceUnavailable, "Service unavailable", true},
	Timeout:                   {http.StatusGatewayTimeout, "Timed out", true},
	Internal:                  {http.StatusInternalServerError, "Internal error", true},
}

// TypeBase is illustrative (Doc 05 §13) and must resolve before production.
const TypeBase = "https://developers.zoikosuite.com/problems/"

type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance"`
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

// Status returns the HTTP status for a code.
func Status(c Code) int {
	if e, ok := catalogue[c]; ok {
		return e.status
	}
	return http.StatusInternalServerError
}

// Write emits the problem for code. detail must be safe for the public.
func Write(w http.ResponseWriter, r *http.Request, c Code, detail string) {
	e, ok := catalogue[c]
	if !ok {
		c, e = Internal, catalogue[Internal]
	}
	rid := w.Header().Get("X-Request-ID")
	p := Problem{
		Type:      TypeBase + string(c),
		Title:     e.title,
		Status:    e.status,
		Detail:    detail,
		Instance:  "/public-ai/errors/" + rid,
		Code:      "publicai." + string(c),
		RequestID: rid,
		Retryable: e.retryable,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(p)
}
