package handler_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// withEnvelope stamps the ZoikoSuite Canonical Service Input Contract
// (ZS-ARCH-SVC-001 v2.0 §4) onto a test request.
//
// The router enforces the contract on material state changes, so a request
// built without one is refused with 401/400 before it reaches the handler under
// test — the test would then be asserting on the middleware rather than on the
// behaviour it was written for.
//
// Only absent fields are filled. A test that sets its own tenant, principal or
// idempotency key keeps it, which is what lets the negative cases — wrong
// tenant, replayed key — still say what they mean.
func withEnvelope(r *http.Request) *http.Request {
	set := func(k, v string) {
		if r.Header.Get(k) == "" {
			r.Header.Set(k, v)
		}
	}
	set("X-Tenant-Id", "tenant-test")
	set("X-Principal-Id", "principal-test")
	// A well-formed client declares the legal entity it acts for; a body
	// legal_entity_id must repeat it. Default the header to the body's value
	// so tests about other behaviour stay well-formed.
	set("X-Legal-Entity-Id", bodyLegalEntityID(r, "entity-test"))
	set("X-Request-Id", "req-test")
	set("X-Correlation-ID", "corr-test")
	set("X-Source-Channel", "api")
	set("X-Purpose-Context", "AUTOMATED_TEST")

	// A fresh key per request: reusing one across a test would make the second
	// call a replay, which is exactly what INV-08 asks the service to collapse.
	if r.Header.Get("Idempotency-Key") == "" {
		idempotencySeq++
		r.Header.Set("Idempotency-Key", "idem-test-"+itoa(idempotencySeq))
	}
	return r
}

var idempotencySeq int

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Keeps the testing import honest in packages where no other file needs it.
var _ = testing.Verbose

// bodyLegalEntityID returns the request body's legal_entity_id (restoring the
// body), or fallback when there is none.
func bodyLegalEntityID(r *http.Request, fallback string) string {
	if r.Body == nil || r.Body == http.NoBody {
		return fallback
	}
	raw, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return fallback
	}
	var probe struct {
		LegalEntityID string `json:"legal_entity_id"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.LegalEntityID == "" {
		return fallback
	}
	return probe.LegalEntityID
}
