package model

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// SEC-T050 / OPR-015: the model client never reaches cloud metadata or a
// link-local address, whatever BaseURL says; ordinary hosts still work.
func TestEgressGuardRefusesMetadata(t *testing.T) {
	for _, a := range []string{"169.254.169.254", "169.254.170.2", "fe80::1", "fd00:ec2::254", "0.0.0.0", "::", "224.0.0.1", "::ffff:169.254.169.254", "100.100.100.200"} {
		if !forbiddenEgress(netip.MustParseAddr(a)) {
			t.Errorf("%s must be refused", a)
		}
	}
	for _, a := range []string{"127.0.0.1", "203.0.113.10", "2001:db8::1"} {
		if forbiddenEgress(netip.MustParseAddr(a)) {
			t.Errorf("%s must be allowed", a)
		}
	}
	g := &Groq{APIKey: testKey, BaseURL: "http://169.254.169.254/latest"}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, g.BaseURL+"/meta-data/", nil)
	if _, err := g.httpClient().Do(req); !errors.Is(err, ErrForbiddenEgress) {
		t.Fatalf("metadata request error = %v, want ErrForbiddenEgress", err)
	}
	// A caller-supplied client gets the guard too.
	g.Client = &http.Client{}
	if _, err := g.httpClient().Do(req.Clone(context.Background())); !errors.Is(err, ErrForbiddenEgress) {
		t.Fatalf("caller client: error = %v, want ErrForbiddenEgress", err)
	}
	if _, err := g.Generate(context.Background(), req1); err == nil {
		t.Fatal("generation against a metadata address must fail")
	}
	// The provider path itself is unaffected.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	ok, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := (&Groq{}).httpClient().Do(ok)
	if err != nil {
		t.Fatalf("loopback test server: %v", err)
	}
	resp.Body.Close()
}
