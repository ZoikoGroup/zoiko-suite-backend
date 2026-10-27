// Package problem implements the platform error contract: RFC 9457
// "application/problem+json" plus the Zoiko extensions defined by the Global
// API Command & Integration Contract Standard (§12) and the stable error
// classes in the Governance Control Plane spec (§16).
//
// Every non-2xx response emitted by this service goes through Write so the
// error body shape is identical across modules. During the migration off the
// legacy {"error","message"} body, Problem carries both `code` (the contract
// field clients should branch on) and `error` (a deprecated alias of it).
package problem

import (
	"encoding/json"
	"net/http"
)

// typeBase is the URI namespace for problem types.
const typeBase = "https://api.zoikosuite.com/problems/"

// Problem is an RFC 9457 problem-details document plus Zoiko extensions.
//
// `code` is the stable lower_snake_case machine code clients branch on; it is
// the contract field per API standard §12. `error` is retained as a byte-equal
// alias for one minor cycle so consumers still reading the legacy field keep
// working. Remove `error` once the platform has cut over to `code`.
type Problem struct {
	Type          string  `json:"type"`
	Title         string  `json:"title"`
	Status        int     `json:"status"`
	Detail        string  `json:"detail,omitempty"`
	Instance      string  `json:"instance,omitempty"`
	Code          string  `json:"code"`
	Error         string  `json:"error"`
	RequestID     string  `json:"request_id,omitempty"`
	CorrelationID string  `json:"correlation_id,omitempty"`
	Retryable     bool    `json:"retryable"`
	Errors        []Error `json:"errors,omitempty"`

	extra map[string]any
}

// Error is a field- or item-level failure inside a problem, located by JSON
// Pointer (API standard §12 `errors[]`).
type Error struct {
	Pointer string `json:"pointer"`
	Code    string `json:"code,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// classSlug maps a service error code to the platform's stable error class
// (GCP §16) rendered as a kebab-case type slug. Codes absent here fall back to
// a category slug derived from the HTTP status, because §16 only names the
// governance decisions, not every not-found or validation failure.
var classSlug = map[string]string{
	// §16 AUTHORIZATION_DENIED
	"unauthorized":          "authorization-denied",
	"authz_unavailable":     "authorization-denied",
	"missing_principal":     "authorization-denied",
	"not_assigned_reviewer": "authorization-denied",
	// §16 SOD_CONFLICT
	"segregation_of_duties": "sod-conflict",
	// §16 IDEMPOTENCY_MISMATCH
	"idempotency_conflict": "idempotency-mismatch",
	// §16 EVIDENCE_INTEGRITY_FAILED
	"pack_unverified": "evidence-integrity-failed",
	// §16 CONCURRENCY_CONFLICT
	"overlapping_rule":   "concurrency-conflict",
	"conflict":           "concurrency-conflict",
	"invalid_transition": "concurrency-conflict",
}

// classTitle is the short, stable title for each problem type. RFC 9457 wants
// `title` to summarise the problem *type*, not the occurrence; the occurrence
// specifics live in `detail`.
var classTitle = map[string]string{
	"authorization-denied":      "The request was not authorized.",
	"sod-conflict":              "The request conflicts with segregation-of-duties rules.",
	"idempotency-mismatch":      "The idempotency key was reused with a different request.",
	"evidence-integrity-failed": "Required evidence failed integrity verification.",
	"concurrency-conflict":      "The request conflicts with the current state.",
	"validation":                "The request failed validation.",
	"not-found":                 "The requested resource was not found.",
	"conflict":                  "The request conflicts with the current state.",
	"unavailable":               "The service is temporarily unavailable.",
	"error":                     "The request could not be completed.",
}

// classFor resolves the type slug for a code, falling back to a status
// category so every problem still has a well-formed type URI.
func classFor(code string, status int) string {
	if slug, ok := classSlug[code]; ok {
		return slug
	}
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusRequestEntityTooLarge:
		return "validation"
	case http.StatusNotFound:
		return "not-found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusForbidden, http.StatusUnauthorized:
		return "authorization-denied"
	case http.StatusServiceUnavailable, http.StatusInternalServerError:
		return "unavailable"
	default:
		return "error"
	}
}

// retryableFor is the advisory retry hint (API standard §12). Only transient
// transport/gateway conditions are marked retryable; a caller still has to
// respect idempotency and Retry-After.
func retryableFor(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// New builds a problem for code, deriving the type URI and title from the
// stable class. detail is occurrence-specific and may be empty.
func New(status int, code, detail string) *Problem {
	slug := classFor(code, status)
	return &Problem{
		Type:      typeBase + slug,
		Title:     classTitle[slug],
		Status:    status,
		Detail:    detail,
		Code:      code,
		Error:     code,
		Retryable: retryableFor(status),
	}
}

// With attaches a Zoiko extension field, emitted inline alongside the
// standard members (e.g. "reasons", "jurisdiction_id").
func (p *Problem) With(key string, value any) *Problem {
	if p.extra == nil {
		p.extra = make(map[string]any)
	}
	p.extra[key] = value
	return p
}

// Field attaches a field-level error located by JSON Pointer.
func (p *Problem) Field(pointer, code, detail string) *Problem {
	p.Errors = append(p.Errors, Error{Pointer: pointer, Code: code, Detail: detail})
	return p
}

// MarshalJSON flattens the extension map into the document. The alias type
// drops the method set so json.Marshal does not recurse.
func (p *Problem) MarshalJSON() ([]byte, error) {
	type alias Problem
	base, err := json.Marshal((*alias)(p))
	if err != nil {
		return nil, err
	}
	if len(p.extra) == 0 {
		return base, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(base, &fields); err != nil {
		return nil, err
	}
	for k, v := range p.extra {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		fields[k] = raw
	}
	return json.Marshal(fields)
}

// Write serialises p as application/problem+json with its own status code.
func Write(w http.ResponseWriter, p *Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		// Headers are already sent — nothing to do but drop it.
		_ = err
	}
}
