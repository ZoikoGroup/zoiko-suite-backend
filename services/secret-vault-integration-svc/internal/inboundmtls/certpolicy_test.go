package inboundmtls

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func reqWithLeaf(leaf *x509.Certificate) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/secrets/broker", nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	return r
}

func serve(t *testing.T, mw func(http.Handler) http.Handler, r *http.Request) (int, bool) {
	t.Helper()
	called := false
	w := httptest.NewRecorder()
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(w, r)
	return w.Code, called
}

func TestCertPolicy_LongLivedCertRefused(t *testing.T) {
	now := time.Now()
	leaf := &x509.Certificate{NotBefore: now, NotAfter: now.Add(90 * 24 * time.Hour)}
	code, called := serve(t, CertPolicy(24*time.Hour, "", nil), reqWithLeaf(leaf))
	if called || code != http.StatusForbidden {
		t.Fatalf("a 90-day certificate must be refused under a 24h maximum, got %d", code)
	}
}

func TestCertPolicy_CertForAnotherEnvironmentRefused(t *testing.T) {
	now := time.Now()
	staging, _ := url.Parse("spiffe://zoiko/staging/workload/svc-a")
	leaf := &x509.Certificate{NotBefore: now, NotAfter: now.Add(time.Hour), URIs: []*url.URL{staging}}
	code, called := serve(t, CertPolicy(0, "spiffe://zoiko/production/", nil), reqWithLeaf(leaf))
	if called || code != http.StatusForbidden {
		t.Fatalf("a staging certificate must not authenticate in production, got %d", code)
	}
}

func TestCertPolicy_BoundShortLivedCertPasses(t *testing.T) {
	now := time.Now()
	prod, _ := url.Parse("spiffe://zoiko/production/workload/svc-a")
	leaf := &x509.Certificate{NotBefore: now, NotAfter: now.Add(time.Hour), URIs: []*url.URL{prod}}
	if _, called := serve(t, CertPolicy(24*time.Hour, "spiffe://zoiko/production/", nil), reqWithLeaf(leaf)); !called {
		t.Fatal("a short-lived certificate bound to this environment must pass")
	}
}

func TestCertPolicy_UnsetIsNoop(t *testing.T) {
	now := time.Now()
	leaf := &x509.Certificate{NotBefore: now, NotAfter: now.Add(900 * 24 * time.Hour)}
	if _, called := serve(t, CertPolicy(0, "", nil), reqWithLeaf(leaf)); !called {
		t.Fatal("with no policy configured the middleware must pass through")
	}
}
