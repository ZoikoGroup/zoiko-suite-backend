package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// ProblemEnvelopeRefusals renders the §4 envelope middleware's refusals as RFC
// 9457 problem+json, the shape every other error from this service already has
// (API standard §12; GCP §16 stable classes).
//
// The envelope middleware is generated code shared by every service
// (services/_contract/rollout.sh) and writes `{"error","detail","violations"}`
// with Content-Type application/json. Changing the generator is a platform
// decision (audit X9), so this service converts its own refusals instead, in
// front of the middleware. The original members are kept as RFC 9457 extension
// members, so a caller reading `error`, `detail` or `violations` sees no change.
//
// Only a refusal is rewritten: the middleware also marks requests it ADMITS
// while out of contract (incomplete reads under write-strict) with the same
// X-Envelope-Contract header, and those answers pass through untouched.
func ProblemEnvelopeRefusals(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &refusalRewriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		rw.finish(r)
	})
}

type refusalRewriter struct {
	http.ResponseWriter
	capturing bool
	status    int
	buf       bytes.Buffer
}

func (rw *refusalRewriter) WriteHeader(status int) {
	h := rw.Header()
	if status >= 400 &&
		h.Get("X-Envelope-Contract") == "violated" &&
		strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		rw.capturing, rw.status = true, status
		return
	}
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *refusalRewriter) Write(b []byte) (int, error) {
	if rw.capturing {
		return rw.buf.Write(b)
	}
	return rw.ResponseWriter.Write(b)
}

func (rw *refusalRewriter) finish(r *http.Request) {
	if !rw.capturing {
		return
	}
	var refusal struct {
		Error      string          `json:"error"`
		Detail     string          `json:"detail"`
		Service    string          `json:"service,omitempty"`
		Violations json.RawMessage `json:"violations,omitempty"`
	}
	_ = json.Unmarshal(rw.buf.Bytes(), &refusal)

	class, title := "ENVELOPE_INCOMPLETE", "Envelope Incomplete"
	if rw.status == http.StatusUnauthorized {
		class, title = "UNAUTHORIZED", "Caller Identity Missing"
	}
	rw.Header().Set("Content-Type", "application/problem+json")
	rw.ResponseWriter.WriteHeader(rw.status)
	_ = json.NewEncoder(rw.ResponseWriter).Encode(struct {
		Type       string          `json:"type"`
		Title      string          `json:"title"`
		Status     int             `json:"status"`
		Detail     string          `json:"detail,omitempty"`
		Instance   string          `json:"instance,omitempty"`
		Error      string          `json:"error,omitempty"`
		Service    string          `json:"service,omitempty"`
		Violations json.RawMessage `json:"violations,omitempty"`
	}{class, title, rw.status, refusal.Detail, r.URL.Path, refusal.Error, refusal.Service, refusal.Violations})
}
