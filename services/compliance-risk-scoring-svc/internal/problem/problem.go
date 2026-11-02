// Package problem writes RFC 9457 "application/problem+json" error bodies.
//
// Shape (Global API Command & Integration Contract Standard §12):
//
//	{
//	  "type":           "https://api.zoikosuite.com/problems/<code-with-dashes>",
//	  "title":          "<short summary of the status>",
//	  "status":         409,
//	  "detail":         "<what happened in this occurrence>",
//	  "instance":       "/v1/risk-scores/…",
//	  "code":           "invalid_transition",   // stable, clients branch on this
//	  "error":          "invalid_transition",   // legacy alias of code
//	  "request_id":     "…", "correlation_id": "…",
//	  "errors":         [{"pointer": "/field", "detail": "…"}]
//	}
package problem

import (
	"encoding/json"
	"net/http"
	"strings"

	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
)

const typeBase = "https://api.zoikosuite.com/problems/"

// FieldProblem locates one invalid field by JSON Pointer.
type FieldProblem struct {
	Pointer string `json:"pointer"`
	Detail  string `json:"detail"`
}

// Problem is the RFC 9457 document.
type Problem struct {
	Type          string         `json:"type"`
	Title         string         `json:"title"`
	Status        int            `json:"status"`
	Detail        string         `json:"detail,omitempty"`
	Instance      string         `json:"instance,omitempty"`
	Code          string         `json:"code"`
	Error         string         `json:"error"`
	RequestID     string         `json:"request_id,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	Errors        []FieldProblem `json:"errors,omitempty"`
}

// Write sends a problem response.
func Write(w http.ResponseWriter, r *http.Request, status int, code, detail string, fields []domain.FieldError) {
	body := Problem{
		Type:     typeBase + strings.ReplaceAll(code, "_", "-"),
		Title:    http.StatusText(status),
		Status:   status,
		Detail:   detail,
		Code:     code,
		Error:    code,
		Instance: "",
	}
	if r != nil {
		body.Instance = r.URL.Path
		body.RequestID = r.Header.Get("X-Request-Id")
		body.CorrelationID = r.Header.Get("X-Correlation-ID")
	}
	for _, field := range fields {
		body.Errors = append(body.Errors, FieldProblem{Pointer: "/" + field.Field, Detail: field.Detail})
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
