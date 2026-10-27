// Package problem writes RFC 9457 problem details with the stable ZS-SVC-M-001
// §2.2 integration error codes.
//
// The canonical taxonomy lives in zoiko.io/eventing/integration. This service
// carries the few codes it uses itself rather than importing that module: the
// gateway is built from its own directory, and a build-time dependency on a
// sibling module would put the one service every request passes through at the
// mercy of that module's build context. problem_test.go pins these codes and
// titles to the canonical file, so the two cannot drift apart unnoticed.
package problem

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Code is a stable INT error code.
type Code string

const (
	CodeContextInvalid     Code = "INT-CTX-001"
	CodeAuthDenied         Code = "INT-AUTH-003"
	CodeSchemaMismatch     Code = "INT-SCHEMA-004"
	CodeRateLimited        Code = "INT-RATE-007"
	CodeVersionUnsupported Code = "INT-VERS-012"
	CodeBackpressure       Code = "INT-BP-016"
)

// Titles are fixed per code (the canonical file's wording).
var Titles = map[Code]string{
	CodeContextInvalid:     "Invalid or unresolved tenant/context",
	CodeAuthDenied:         "Authentication or authorization denied",
	CodeSchemaMismatch:     "Contract, schema or profile mismatch",
	CodeRateLimited:        "Rate limited",
	CodeVersionUnsupported: "Unsupported or deprecated version",
	CodeBackpressure:       "Backpressure or admission protection active",
}

// TypePrefix prefixes the RFC 9457 type URI (a URN: an identifier, not a page).
const TypePrefix = "urn:zoikosuite:problem:"

// ContentType is the RFC 9457 media type.
const ContentType = "application/problem+json"

// Problem is the response body.
type Problem struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Detail        string `json:"detail,omitempty"`
	Instance      string `json:"instance,omitempty"`
	Code          Code   `json:"code"`
	Retryable     bool   `json:"retryable"`
	CorrelationID string `json:"correlation_id,omitempty"`
	DecisionID    string `json:"decision_id,omitempty"`
}

// Spec describes one refusal.
type Spec struct {
	Code          Code
	Status        int
	Detail        string
	Instance      string
	Retryable     bool
	RetryAfter    int // seconds; 0 = not advised
	CorrelationID string
	DecisionID    string
}

// Write sends the refusal as application/problem+json. Detail must name a rule,
// never echo caller input.
func Write(w http.ResponseWriter, s Spec) {
	title := Titles[s.Code]
	if title == "" {
		title = http.StatusText(s.Status)
	}
	p := Problem{
		Type:          TypePrefix + string(s.Code),
		Title:         title,
		Status:        s.Status,
		Detail:        s.Detail,
		Instance:      s.Instance,
		Code:          s.Code,
		Retryable:     s.Retryable,
		CorrelationID: s.CorrelationID,
		DecisionID:    s.DecisionID,
	}
	w.Header().Set("Content-Type", ContentType)
	if s.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(s.RetryAfter))
	}
	w.WriteHeader(s.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteStatus answers with a problem that carries no integration code, for
// conditions outside the §2.2 taxonomy (a missing resource, a bad method). The
// type is about:blank, as RFC 9457 §4.2.1 prescribes.
func WriteStatus(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": detail,
	})
}
