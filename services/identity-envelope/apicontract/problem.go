package apicontract

import (
	"encoding/json"
	"net/http"
	"strings"
)

const (
	ProblemBase        = "https://api.zoikosuite.com/problems/"
	ContentTypeProblem = "application/problem+json"

	HeaderRequestID           = "Zoiko-Request-Id"
	HeaderLegacyRequestID     = "X-Request-Id"
	HeaderCorrelationID       = "Zoiko-Correlation-Id"
	HeaderLegacyCorrelationID = "X-Correlation-ID"
)

type FieldError struct {
	Pointer string `json:"pointer,omitempty"`
	Code    string `json:"code"`
	Detail  string `json:"detail,omitempty"`
}

type Problem struct {
	Type           string         `json:"type"`
	Title          string         `json:"title"`
	Status         int            `json:"status"`
	Detail         string         `json:"detail,omitempty"`
	Instance       string         `json:"instance,omitempty"`
	Code           string         `json:"code"`
	RequestID      string         `json:"request_id,omitempty"`
	CorrelationID  string         `json:"correlation_id,omitempty"`
	Retryable      bool           `json:"retryable"`
	Errors         []FieldError   `json:"errors,omitempty"`
	CurrentVersion *int64         `json:"current_version,omitempty"`
	Extra          map[string]any `json:"-"`
}

func (p Problem) Error() string {
	if p.Detail == "" {
		return p.Code
	}
	return p.Code + ": " + p.Detail
}

func ProblemFor(status int, code, detail string) Problem {
	return Problem{Status: status, Code: code, Detail: detail}
}

func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if p.Code == "" {
		p.Code = "internal_error"
	}
	if p.Status == 0 {
		p.Status = http.StatusInternalServerError
	}
	if p.Type == "" {
		p.Type = ProblemBase + strings.ReplaceAll(p.Code, "_", "-")
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
		if p.Title == "" {
			p.Title = "Error"
		}
	}
	if r != nil {
		if p.RequestID == "" {
			p.RequestID = RequestIDFrom(r)
		}
		if p.CorrelationID == "" {
			p.CorrelationID = firstNonEmpty(r.Header.Get(HeaderCorrelationID), r.Header.Get(HeaderLegacyCorrelationID))
		}
	}
	if p.Instance == "" && p.RequestID != "" {
		p.Instance = "urn:zoikosuite:problem:" + p.RequestID
	}

	body := map[string]any{}
	raw, _ := json.Marshal(p)
	_ = json.Unmarshal(raw, &body)
	for k, v := range p.Extra {
		if _, taken := body[k]; !taken {
			body[k] = v
		}
	}

	h := w.Header()
	h.Set("Content-Type", ContentTypeProblem)
	h.Set("Cache-Control", "private, no-store")
	if p.RequestID != "" {
		h.Set(HeaderRequestID, p.RequestID)
		h.Set(HeaderLegacyRequestID, p.RequestID)
	}
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(body)
}

func WriteError(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	WriteProblem(w, r, ProblemFor(status, code, detail))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
