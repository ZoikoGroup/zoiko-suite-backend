// Package problem writes this service's error responses as RFC 9457 problem
// details (application/problem+json), as the Global API Command & Integration
// Contract Standard §12 requires.
//
// The body is a superset of the previous {"error": ..., "message": ...} shape:
// `error` still carries the stable code and `message` the human text, so
// callers that read those keys keep working while new callers read `type`,
// `title`, `status`, `detail` and `code`.
package problem

import (
	"encoding/json"
	"net/http"
)

const typeBase = "https://api.zoikosuite.com/problems/reconciliation-intelligence/"

// Body is the problem-details document. Extra holds extension members such as
// an envelope refusal's `violations`.
type Body struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Detail        string `json:"detail,omitempty"`
	Code          string `json:"code"`
	Instance      string `json:"instance,omitempty"`
	RequestID     string `json:"request_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
	// Legacy members kept for existing callers.
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// Write sends status with a problem body. r may be nil (no request context).
func Write(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	WriteWith(w, r, status, code, detail, nil)
}

// WriteWith is Write plus extension members merged into the body.
func WriteWith(w http.ResponseWriter, r *http.Request, status int, code, detail string, extra map[string]any) {
	b := Body{
		Type:    typeBase + code,
		Title:   http.StatusText(status),
		Status:  status,
		Detail:  detail,
		Code:    code,
		Error:   code,
		Message: detail,
	}
	if r != nil {
		b.Instance = r.URL.Path
		b.RequestID = r.Header.Get("X-Request-Id")
		b.CorrelationID = r.Header.Get("X-Correlation-ID")
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if len(extra) == 0 {
		_ = json.NewEncoder(w).Encode(b)
		return
	}
	raw, _ := json.Marshal(b)
	merged := map[string]any{}
	_ = json.Unmarshal(raw, &merged)
	for k, v := range extra {
		merged[k] = v
	}
	_ = json.NewEncoder(w).Encode(merged)
}
