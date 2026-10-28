package apicontract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// EnvelopeRefusals renders the §4 envelope middleware's refusals as RFC 9457
// problem+json (ZS-API-001 §12).
//
// Every service carries a vendored copy of that middleware
// (services/_contract/envelope), and the copy writes
// `{"error","detail","violations"}` as application/json. Rewriting the
// vendored copy in each service would fork it from the generator, so a service
// mounts this in front of the middleware instead. The original members stay as
// extension members, so a caller reading `error`, `detail` or `violations`
// sees no change.
//
// Only a refusal is rewritten: the middleware also marks requests it admits
// while out of contract with the same X-Envelope-Contract header, and those
// answers pass through untouched.
func EnvelopeRefusals(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &envelopeRefusalWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		rw.finish(r)
	})
}

type envelopeRefusalWriter struct {
	http.ResponseWriter
	capturing bool
	status    int
	buf       bytes.Buffer
}

func (rw *envelopeRefusalWriter) WriteHeader(status int) {
	h := rw.Header()
	if status >= 400 &&
		h.Get("X-Envelope-Contract") == "violated" &&
		strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		rw.capturing, rw.status = true, status
		return
	}
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *envelopeRefusalWriter) Write(b []byte) (int, error) {
	if rw.capturing {
		return rw.buf.Write(b)
	}
	return rw.ResponseWriter.Write(b)
}

func (rw *envelopeRefusalWriter) finish(r *http.Request) {
	if !rw.capturing {
		return
	}
	var body map[string]any
	_ = json.Unmarshal(rw.buf.Bytes(), &body)

	// 401 is the middleware's answer to a missing tenant or actor.
	code := "envelope_incomplete"
	if rw.status == http.StatusUnauthorized {
		code = "identity_missing"
	}
	p := ProblemFor(rw.status, code, "")
	if s, ok := body["detail"].(string); ok {
		p.Detail = s
	}
	p.Extra = map[string]any{}
	for k, v := range body {
		if k != "detail" {
			p.Extra[k] = v
		}
	}
	WriteProblem(rw.ResponseWriter, r, p)
}
