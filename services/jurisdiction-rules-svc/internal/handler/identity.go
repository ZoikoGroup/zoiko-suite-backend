package handler

// Governance audit J-01 / X5: X-Principal-Id was trusted as sent. The identity
// gate (identity-envelope) verifies a presented envelope, but runs in warn mode
// on this service because the tax, payroll and receivables services that read
// rule packs and resolve rules (outside this domain) do not forward one yet.
//
// Administrative changes cannot wait for them: every write under /v1/admin/ and
// every operation command must come from a caller the gate VERIFIED, a signed
// envelope (the console) or an mTLS peer workload. A caller identified only by
// headers gets 401 identity_not_verified, whatever the gate's mode. Reads and
// runtime resolutions keep the gate's mode until their callers send envelopes.
//
// The verdict is read from the gate's context value, never from a header, so a
// client cannot assert it.

import (
	"net/http"
	"strings"

	idenv "zoiko.io/identity-envelope"
)

// AdminWrite reports whether r changes administrative state: a write under
// /v1/admin/ or an API-5 operation command.
func AdminWrite(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return false
	}
	return strings.HasPrefix(r.URL.Path, "/v1/admin/") || strings.HasPrefix(r.URL.Path, "/v1/operations/")
}

// RequireVerifiedIdentity refuses the requests `applies` selects unless the
// identity gate verified the caller. Mount it after the gate.
func RequireVerifiedIdentity(applies func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if applies(r) {
				id, ok := idenv.FromContext(r.Context())
				if !ok || id == nil || (id.Basis != idenv.BasisEnvelope && id.Basis != idenv.BasisPeer) {
					w.Header().Set("WWW-Authenticate", `Bearer realm="jurisdiction-rules-svc", error="invalid_token"`)
					writeError(w, r, http.StatusUnauthorized, "identity_not_verified",
						"administrative changes need a verified identity: present the signed identity envelope (Authorization: Bearer) or call over mTLS as a registered peer")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
