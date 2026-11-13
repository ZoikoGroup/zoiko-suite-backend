package handler_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	idenv "zoiko.io/identity-envelope"

	"zoiko.io/jurisdiction-rules-svc/internal/handler"
)

// J-01: an administrative write needs the gate's verified identity; headers alone are refused.
func TestJ01_AdminWritesNeedAVerifiedIdentity(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mw := handler.RequireVerifiedIdentity(handler.AdminWrite)(ok)
	run := func(method, path string, id *idenv.Identity, forgedBasis string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Principal-Id", "someone")
		if forgedBasis != "" {
			req.Header.Set(idenv.HeaderIdentityBasis, forgedBasis)
		}
		if id != nil {
			req = req.WithContext(idenv.WithIdentity(req.Context(), id))
		}
		rr := httptest.NewRecorder()
		mw.ServeHTTP(rr, req)
		return rr.Code
	}
	const admin = "/v1/admin/jurisdictions"
	cases := []struct {
		name   string
		method string
		path   string
		id     *idenv.Identity
		forged string
		want   int
	}{
		{"headers only", http.MethodPost, admin, nil, "", http.StatusUnauthorized},
		{"a forged basis header is not a verification", http.MethodPost, admin, nil, string(idenv.BasisEnvelope), http.StatusUnauthorized},
		{"asserted (warn mode) identity", http.MethodPost, admin, &idenv.Identity{Basis: idenv.BasisAsserted, PrincipalID: "p"}, "", http.StatusUnauthorized},
		{"verified envelope", http.MethodPost, admin, &idenv.Identity{Basis: idenv.BasisEnvelope, PrincipalID: "p"}, "", http.StatusNoContent},
		{"verified mTLS peer", http.MethodPut, "/v1/admin/rules/r-1/provenance", &idenv.Identity{Basis: idenv.BasisPeer, PrincipalID: "p"}, "", http.StatusNoContent},
		{"operation cancel is administrative", http.MethodPost, "/v1/operations/op_1/cancel", nil, "", http.StatusUnauthorized},
		{"an admin read keeps the gate's mode", http.MethodGet, admin, nil, "", http.StatusNoContent},
		{"a runtime resolution keeps the gate's mode", http.MethodPost, "/v1/rule-resolutions:resolve", nil, "", http.StatusNoContent},
	}
	for _, c := range cases {
		if got := run(c.method, c.path, c.id, c.forged); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}
