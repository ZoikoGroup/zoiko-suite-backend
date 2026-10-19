package clients

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The caller's envelope reaches authorization-svc only on a call made in that
// caller's name; a call for anyone else goes without it (delegate path).
func TestAuthzAdminClient_ForwardsTheCallersEnvelopeOnlyForThemselves(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("X-Principal-Id")+"|"+r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := NewAuthzAdminClientWithHTTP(srv.URL, srv.Client())

	// What ForwardBearer records for a request from P-ALICE.
	var ctx context.Context
	ForwardBearer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { ctx = r.Context() })).
		ServeHTTP(httptest.NewRecorder(), func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/x", nil)
			r.Header.Set("Authorization", "Bearer alice-envelope")
			r.Header.Set("X-Principal-Id", "P-ALICE")
			return r
		}())

	scope := func(p string) Scope {
		return Scope{PrincipalID: p, TenantID: "t", LegalEntityID: "e", CorrelationID: "c"}
	}
	if err := c.post(ctx, "/v1/admin/roles", []byte(`{}`), scope("P-ALICE")); err != nil {
		t.Fatal(err)
	}
	if err := c.post(ctx, "/v1/admin/roles", []byte(`{}`), scope("P-BOB")); err != nil {
		t.Fatal(err)
	}
	if err := c.post(context.Background(), "/v1/admin/roles", []byte(`{}`), scope("P-ALICE")); err != nil {
		t.Fatal(err)
	}
	want := []string{"P-ALICE|Bearer alice-envelope", "P-BOB|", "P-ALICE|"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d: got %q want %q", i, got[i], want[i])
		}
	}
}
