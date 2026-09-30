package mtls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIssuer stands in for mtls-management-svc: it signs a fresh leaf per
// call, and records the lifetime and auto-rotate flag it was asked for.
type fakeIssuer struct {
	srv       *httptest.Server
	calls     atomic.Int32
	down      atomic.Bool
	lastReq   provisionRequest
	lastHdr   http.Header
	notBefore time.Time
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	f := &fakeIssuer{notBefore: time.Now()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		f.lastHdr = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&f.lastReq)
		n := f.calls.Add(1)
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(int64(n + 1)), Subject: pkix.Name{CommonName: f.lastReq.CommonName},
			NotBefore: f.notBefore, NotAfter: f.notBefore.Add(time.Duration(f.lastReq.RotationDays) * 24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, caTmpl, &key.PublicKey, caKey)
		keyDER, _ := x509.MarshalECPrivateKey(key)
		var res provisionResult
		res.Certificate.CertificatePEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		res.PrivateKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
		res.CACertPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(res)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newRenewing(t *testing.T, f *fakeIssuer, now *time.Time) *renewingCert {
	t.Helper()
	p := provisioner{url: f.srv.URL, serviceName: "secret-vault-integration-svc", client: f.srv.Client()}
	cert, _, err := p.provision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &renewingCert{p: p, cert: cert, now: func() time.Time { return *now }}
}

func TestProvision_RequestsShortLivedAutoRotatingCert(t *testing.T) {
	f := newFakeIssuer(t)
	now := time.Now()
	rc := newRenewing(t, f, &now)
	if f.lastReq.RotationDays != 1 || !f.lastReq.AutoRotate {
		t.Fatalf("must request a 1-day auto-rotating cert, asked for %+v", f.lastReq)
	}
	if life := rc.cert.Leaf.NotAfter.Sub(rc.cert.Leaf.NotBefore); life > 24*time.Hour {
		t.Fatalf("leaf lifetime %v exceeds a day", life)
	}
}

func TestRenewingCert_RenewsAtHalfLife(t *testing.T) {
	f := newFakeIssuer(t)
	now := f.notBefore.Add(time.Hour)
	rc := newRenewing(t, f, &now)
	first := rc.cert.Leaf.SerialNumber

	c, _ := rc.get(context.Background())
	if c.Leaf.SerialNumber.Cmp(first) != 0 || f.calls.Load() != 1 {
		t.Fatal("before half-life the current leaf must be reused")
	}
	now = f.notBefore.Add(13 * time.Hour)
	c, err := rc.get(context.Background())
	if err != nil || c.Leaf.SerialNumber.Cmp(first) == 0 {
		t.Fatalf("past half-life the leaf must be renewed without a restart: %v", err)
	}
}

func TestRenewingCert_IssuerOutage(t *testing.T) {
	f := newFakeIssuer(t)
	now := f.notBefore.Add(13 * time.Hour)
	rc := newRenewing(t, f, &now)
	f.down.Store(true)
	if _, err := rc.get(context.Background()); err != nil {
		t.Fatalf("a still-valid leaf must be kept through an issuer outage: %v", err)
	}
	now = f.notBefore.Add(25 * time.Hour)
	if _, err := rc.get(context.Background()); err == nil {
		t.Fatal("an expired leaf must never be presented")
	}
}

// Without the bootstrap token and envelope, mtls-management-svc refused every
// provisioning call, so AUTHZ_MTLS_ENABLED=true could never boot.
func TestProvision_SendsBootstrapTokenAndEnvelope(t *testing.T) {
	f := newFakeIssuer(t)
	tokFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokFile, []byte("boot-tok\n"), 0o600)
	p := provisioner{url: f.srv.URL, serviceName: "secret-vault-integration-svc", platformScopeID: "scope-1", bootstrapTokenPath: tokFile, client: f.srv.Client()}
	if _, _, err := p.provision(context.Background()); err != nil {
		t.Fatal(err)
	}
	for h, want := range map[string]string{
		"X-Mtls-Bootstrap-Token": "boot-tok",
		"X-Workload-Id":          "secret-vault-integration-svc",
		"X-Tenant-Id":            "scope-1",
		"X-Source-Channel":       "system",
	} {
		if got := f.lastHdr.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	if f.lastHdr.Get("Idempotency-Key") == "" || f.lastHdr.Get("X-Request-Id") == "" {
		t.Error("a provisioning write must carry Idempotency-Key and X-Request-Id")
	}
}
