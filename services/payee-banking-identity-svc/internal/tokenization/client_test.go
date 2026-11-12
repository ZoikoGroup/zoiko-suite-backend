package tokenization

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

var scope = Scope{TenantID: "tenant-a", PrincipalID: "alice", LegalEntityID: "le-1", CorrelationID: "corr-1"}

// server answers /v1/tokenize and /v1/detokenize with handler, recording the
// last request's tenant header.
func server(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}

// envelopeHeaders are the ZS-ARCH-SVC-001 §4 headers key-management-svc's
// write-strict envelope refuses a POST without.
var envelopeHeaders = []string{"X-Tenant-Id", "X-Principal-Id", "X-Legal-Entity-Id", "X-Correlation-ID",
	"X-Request-Id", "X-Source-Channel", "X-Purpose-Context", "Idempotency-Key"}

func TestClient_TokenizeSendsTheContract(t *testing.T) {
	var gotPath, gotTenant, gotPrincipal, gotCorr string
	var gotBody tokenizeRequest
	var got http.Header
	s := server(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTenant, got = r.URL.Path, r.Header.Get("X-Tenant-Id"), r.Header.Clone()
		gotPrincipal, gotCorr = r.Header.Get("X-Principal-Id"), r.Header.Get("X-Correlation-ID")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(tokenizeResponse{Token: "tok_123"})
	})
	tok, err := NewClient(s.URL).Tokenize(context.Background(), scope, ClassificationRestricted, "GB29NWBK60161331926819")
	if err != nil || tok != "tok_123" {
		t.Fatalf("Tokenize = %q, %v", tok, err)
	}
	if gotPath != "/v1/tokenize" || gotTenant != "tenant-a" || gotPrincipal != "alice" || gotCorr != "corr-1" {
		t.Fatalf("request: path=%s tenant=%s principal=%s corr=%s", gotPath, gotTenant, gotPrincipal, gotCorr)
	}
	if gotBody.Value != "GB29NWBK60161331926819" || gotBody.Classification != "S3_RESTRICTED" {
		t.Fatalf("body: %+v", gotBody)
	}
	for _, h := range envelopeHeaders {
		if got.Get(h) == "" {
			t.Errorf("tokenize sent no %s; key-management-svc refuses the request without it", h)
		}
	}
	if got.Get("X-Legal-Entity-Id") != "le-1" || got.Get("X-Source-Channel") != "system" ||
		got.Get("X-Purpose-Context") != "bank_detail_tokenization" {
		t.Fatalf("envelope: %v", got)
	}
}

func TestClient_DetokenizeSendsTheContract(t *testing.T) {
	var gotPath string
	var gotBody detokenizeRequest
	var got http.Header
	s := server(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, got = r.URL.Path, r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(detokenizeResponse{Value: "raw-value"})
	})
	v, err := NewClient(s.URL).Detokenize(context.Background(), scope, "tok_123")
	if err != nil || v != "raw-value" || gotPath != "/v1/detokenize" || gotBody.Token != "tok_123" {
		t.Fatalf("Detokenize = %q, %v (path %s, body %+v)", v, err, gotPath, gotBody)
	}
	for _, h := range envelopeHeaders {
		if got.Get(h) == "" {
			t.Errorf("detokenize sent no %s; key-management-svc refuses the request without it", h)
		}
	}
	if got.Get("X-Purpose-Context") != "bank_detail_detokenization" {
		t.Fatalf("purpose: %q", got.Get("X-Purpose-Context"))
	}
}

// Every way the service can fail to give a usable answer is a refusal, never
// a pass-through of the raw value.
func TestClient_FailsClosed(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"500":               func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"404 (no endpoint)": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		"undecodable body":  func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) },
		"empty token":       func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"token":""}`)) },
		"token is the raw value": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"token":"GB29NWBK60161331926819"}`))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			tok, err := NewClient(server(t, h).URL).Tokenize(context.Background(), scope, ClassificationRestricted, "GB29NWBK60161331926819")
			if !errors.Is(err, ErrTokenizationServiceUnavailable) || tok != "" {
				t.Fatalf("Tokenize = %q, %v; want ErrTokenizationServiceUnavailable and no token", tok, err)
			}
		})
	}
}

func TestClient_FailsClosedWhenUnreachable(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	url := s.URL
	s.Close() // nothing listens there any more
	if tok, err := NewClient(url).Tokenize(context.Background(), scope, ClassificationRestricted, "12345678"); !errors.Is(err, ErrTokenizationServiceUnavailable) || tok != "" {
		t.Fatalf("Tokenize against a dead service = %q, %v", tok, err)
	}
	if v, err := NewClient(url).Detokenize(context.Background(), scope, "tok"); !errors.Is(err, ErrTokenizationServiceUnavailable) || v != "" {
		t.Fatalf("Detokenize against a dead service = %q, %v", v, err)
	}
}

func TestClient_DetokenizeDeniedIsDistinct(t *testing.T) {
	s := server(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if _, err := NewClient(s.URL).Detokenize(context.Background(), scope, "tok"); !errors.Is(err, ErrDetokenizationDenied) {
		t.Fatalf("403 on detokenize = %v, want ErrDetokenizationDenied", err)
	}
	// A 403 on tokenize is not a "denial" of something the caller may retry
	// elsewhere — it is just a refusal, like any other non-200.
	if _, err := NewClient(s.URL).Tokenize(context.Background(), scope, ClassificationRestricted, "v"); !errors.Is(err, ErrTokenizationServiceUnavailable) {
		t.Fatalf("403 on tokenize = %v, want ErrTokenizationServiceUnavailable", err)
	}
}

func TestClient_RefusesUnbindableRequestsWithoutCalling(t *testing.T) {
	called := false
	s := server(t, func(w http.ResponseWriter, _ *http.Request) { called = true })
	c := NewClient(s.URL)
	if _, err := c.Tokenize(context.Background(), Scope{}, ClassificationRestricted, "v"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("no tenant: %v", err)
	}
	if _, err := c.Detokenize(context.Background(), Scope{TenantID: "t", LegalEntityID: "le"}, "tok"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("no principal on detokenize: %v", err)
	}
	if _, err := c.Tokenize(context.Background(), Scope{TenantID: "t", LegalEntityID: "le"}, ClassificationRestricted, "v"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("no principal on tokenize: %v", err)
	}
	if _, err := c.Tokenize(context.Background(), Scope{TenantID: "t", PrincipalID: "p"}, ClassificationRestricted, "v"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("no legal entity on tokenize: %v", err)
	}
	if called {
		t.Fatal("an unbindable request reached the service")
	}
}

func TestFake_IsDeterministicTenantBoundAndCanFail(t *testing.T) {
	f := &Fake{}
	ctx := context.Background()
	t1, err := f.Tokenize(ctx, scope, ClassificationRestricted, "12345678")
	if err != nil || t1 == "12345678" || t1 == "" {
		t.Fatalf("Tokenize = %q, %v", t1, err)
	}
	if t2, _ := f.Tokenize(ctx, scope, ClassificationRestricted, "12345678"); t2 != t1 {
		t.Fatalf("not deterministic: %q vs %q", t1, t2)
	}
	if v, err := f.Detokenize(ctx, scope, t1); err != nil || v != "12345678" {
		t.Fatalf("round trip = %q, %v", v, err)
	}
	other := Scope{TenantID: "tenant-b", PrincipalID: "bob", LegalEntityID: "le-1"}
	if _, err := f.Detokenize(ctx, other, t1); !errors.Is(err, ErrDetokenizationDenied) {
		t.Fatalf("another tenant detokenizing (SEC-T29) = %v, want ErrDetokenizationDenied", err)
	}
	f.Err = ErrTokenizationServiceUnavailable
	if tok, err := f.Tokenize(ctx, scope, ClassificationRestricted, "12345678"); !errors.Is(err, ErrTokenizationServiceUnavailable) || tok != "" {
		t.Fatalf("failing fake = %q, %v", tok, err)
	}
	if f.Calls() != 5 {
		t.Fatalf("Calls = %d, want 5", f.Calls())
	}
}
