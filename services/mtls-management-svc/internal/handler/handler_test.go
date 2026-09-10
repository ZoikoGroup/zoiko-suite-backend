package handler_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	authzpkg "zoiko.io/mtls-management-svc/internal/authz"
	internalca "zoiko.io/mtls-management-svc/internal/ca"
	"zoiko.io/mtls-management-svc/internal/domain"
	"zoiko.io/mtls-management-svc/internal/handler"
	"zoiko.io/mtls-management-svc/internal/siem"
	"zoiko.io/mtls-management-svc/internal/store"
)

// stubAuthz is a test double for handler.AuthzChecker. It GRANTS by default
// so the existing behavioural tests keep exercising the real path; tests that
// need the deny or unavailable branch set err.
type stubAuthz struct {
	err error
}

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	return s.err
}

// newRouterWithAuthz is newRouter with an injectable authorization decision.
// The bootstrap path is disabled ("") — these tests exercise only the
// normal human/admin principal + authorize flow.
func newRouterWithAuthz(t *testing.T, az handler.AuthzChecker) http.Handler {
	t.Helper()
	c, err := internalca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create test CA: %v", err)
	}
	return handler.NewRouter(handler.New(store.NewMemoryStore(), c, siem.New("", "mtls-management-svc", zap.NewNop()), az, zap.NewNop(), ""))
}

// newRouter builds a router backed by a real CA persisted under a per-test
// temp directory — every test gets its own isolated CA, so tests cannot
// interfere with each other's issued certificates. Authorization GRANTS by
// default here; see newRouterWithAuthz to inject a decision. The bootstrap
// path is disabled ("") — see newRouterWithBootstrapToken to enable it.
func newRouter(t *testing.T) http.Handler {
	t.Helper()
	c, err := internalca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create test CA: %v", err)
	}
	return handler.NewRouter(handler.New(store.NewMemoryStore(), c, siem.New("", "mtls-management-svc", zap.NewNop()), &stubAuthz{}, zap.NewNop(), ""))
}

// newRouterWithBootstrapToken enables the self-provisioning bootstrap path
// with the given shared token. Authorization DENIES by default here — the
// whole point of these tests is to prove the bootstrap path never needs to
// reach the authorize() call at all.
func newRouterWithBootstrapToken(t *testing.T, token string) http.Handler {
	t.Helper()
	c, err := internalca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create test CA: %v", err)
	}
	denyingAuthz := &stubAuthz{err: authzpkg.ErrAuthorizationDenied}
	return handler.NewRouter(handler.New(store.NewMemoryStore(), c, siem.New("", "mtls-management-svc", zap.NewNop()), denyingAuthz, zap.NewNop(), token))
}

// mustParsePEMCertificate fails the test if pemBytes is not a real,
// parseable X.509 certificate — the whole point of this fix is that these
// are no longer fabricated strings.
func mustParsePEMCertificate(t *testing.T, pemBytes string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(pemBytes))
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("certificate_pem did not decode to a PEM CERTIFICATE block: %q", pemBytes)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("certificate_pem did not parse as a valid X.509 certificate: %v", err)
	}
	return cert
}

func mustParsePEMPrivateKey(t *testing.T, pemBytes string) {
	t.Helper()
	block, _ := pem.Decode([]byte(pemBytes))
	if block == nil || block.Type != "EC PRIVATE KEY" {
		t.Fatalf("private_key_pem did not decode to a PEM EC PRIVATE KEY block: %q", pemBytes)
	}
	if _, err := x509.ParseECPrivateKey(block.Bytes); err != nil {
		t.Fatalf("private_key_pem did not parse as a valid EC private key: %v", err)
	}
}

func TestHealthCheck(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	withEnvelope(r)
	w := httptest.NewRecorder()
	newRouter(t).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestProvisionCert_BootstrapToken_SkipsAuthorization proves a caller with
// no principal at all can self-provision by presenting the correct shared
// bootstrap token — even though authorization-svc would DENY this exact
// request on its own merits (newRouterWithBootstrapToken wires a denying
// authz stub), proving the bootstrap path genuinely bypasses that check
// rather than happening to agree with it.
func TestProvisionCert_BootstrapToken_SkipsAuthorization(t *testing.T) {
	router := newRouterWithBootstrapToken(t, "shared-secret-123")

	body, _ := json.Marshal(domain.ProvisionCertRequest{LegalEntityID: "LE-1", ServiceName: "authorization-svc", CommonName: "authorization-svc.zoiko.internal", RotationDays: 90})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Mtls-Bootstrap-Token", "shared-secret-123")
	// Deliberately NO X-Principal-Id — the whole point of this defense.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 201 {
		t.Fatalf("expected 201 via bootstrap token with no principal, got %d: %s", w.Code, w.Body)
	}
	var result domain.ProvisionCertResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to decode provision response: %v", err)
	}
	mustParsePEMCertificate(t, result.Certificate.CertificatePEM)
}

// TestProvisionCert_WrongBootstrapToken_FallsBackToDeniedAuthz proves a
// caller presenting an INCORRECT token gets no special treatment — it
// falls through to the normal principal/authorize path, which the test's
// denying authz stub then rejects.
func TestProvisionCert_WrongBootstrapToken_FallsBackToDeniedAuthz(t *testing.T) {
	router := newRouterWithBootstrapToken(t, "shared-secret-123")

	body, _ := json.Marshal(domain.ProvisionCertRequest{LegalEntityID: "LE-1", ServiceName: "authorization-svc", CommonName: "authorization-svc.zoiko.internal", RotationDays: 90})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Mtls-Bootstrap-Token", "wrong-token")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (denied via normal path), got %d: %s", w.Code, w.Body)
	}
}

// TestProvisionCert_NoBootstrapTokenHeader_FallsBackToDeniedAuthz proves
// omitting the header entirely (the normal case for every caller that
// isn't self-provisioning) behaves exactly as before this defense existed.
func TestProvisionCert_NoBootstrapTokenHeader_FallsBackToDeniedAuthz(t *testing.T) {
	router := newRouterWithBootstrapToken(t, "shared-secret-123")

	body, _ := json.Marshal(domain.ProvisionCertRequest{LegalEntityID: "LE-1", ServiceName: "ledger-svc", CommonName: "ledger-svc.zoiko.internal", RotationDays: 90})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (denied via normal path), got %d: %s", w.Code, w.Body)
	}
}

func TestProvisionRotateRevoke(t *testing.T) {
	router := newRouter(t)
	// Provision
	body, _ := json.Marshal(domain.ProvisionCertRequest{LegalEntityID: "LE-1", ServiceName: "ledger-svc", CommonName: "ledger-svc.zoiko.internal", RotationDays: 90, AutoRotate: true})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates", bytes.NewBuffer(body))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body)
	}
	var result domain.ProvisionCertResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to decode provision response: %v", err)
	}
	cert := result.Certificate
	if cert.ID == "" {
		t.Fatal("expected cert ID")
	}
	if cert.Status != domain.CertStatusActive {
		t.Fatalf("expected ACTIVE, got %s", cert.Status)
	}

	// The core of this fix: the returned certificate and private key must
	// be REAL, parseable X.509 material, not fabricated strings.
	leafCert := mustParsePEMCertificate(t, cert.CertificatePEM)
	if leafCert.Subject.CommonName != "ledger-svc.zoiko.internal" {
		t.Fatalf("expected CommonName ledger-svc.zoiko.internal, got %s", leafCert.Subject.CommonName)
	}
	mustParsePEMPrivateKey(t, result.PrivateKeyPEM)
	caCert := mustParsePEMCertificate(t, result.CACertPEM)

	// Verify the leaf is actually signed by the returned CA cert — proving
	// this isn't just "two unrelated valid certificates," but a real chain.
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leafCert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		t.Fatalf("issued leaf certificate does not verify against the returned CA certificate: %v", err)
	}

	// Get — GetCert must return the public cert but must NEVER leak a
	// private key field.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/mtls/certificates/"+cert.ID, nil)
	withEnvelope(req2)
	req2.Header.Set("X-Tenant-ID", "t1")
	req2.Header.Set("X-Principal-Id", "principal-test-01")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("expected 200 on get, got %d", w2.Code)
	}
	if bytes.Contains(w2.Body.Bytes(), []byte("PRIVATE KEY")) {
		t.Fatal("GetCert response must never contain private key material")
	}

	// List
	req3 := httptest.NewRequest(http.MethodGet, "/v1/mtls/certificates?legal_entity_id=LE-1", nil)
	withEnvelope(req3)
	req3.Header.Set("X-Tenant-ID", "t1")
	req3.Header.Set("X-Principal-Id", "principal-test-01")
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)
	if w3.Code != 200 {
		t.Fatalf("expected 200 on list, got %d", w3.Code)
	}
	var listResp map[string]interface{}
	json.Unmarshal(w3.Body.Bytes(), &listResp)
	if int(listResp["count"].(float64)) < 1 {
		t.Fatal("expected at least 1 cert")
	}
	if bytes.Contains(w3.Body.Bytes(), []byte("PRIVATE KEY")) {
		t.Fatal("ListCerts response must never contain private key material")
	}

	// Rotate — must issue a genuinely different key pair and certificate.
	req4 := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates/"+cert.ID+"/rotate", nil)
	withEnvelope(req4)
	req4.Header.Set("X-Tenant-ID", "t1")
	req4.Header.Set("X-Principal-Id", "principal-test-01")
	w4 := httptest.NewRecorder()
	router.ServeHTTP(w4, req4)
	if w4.Code != 200 {
		t.Fatalf("expected 200 on rotate, got %d", w4.Code)
	}
	var rotateResult domain.ProvisionCertResult
	json.Unmarshal(w4.Body.Bytes(), &rotateResult)
	rotated := rotateResult.Certificate
	if rotated.Fingerprint == cert.Fingerprint {
		t.Fatal("fingerprint should change after rotation")
	}
	if rotated.SerialNumber == cert.SerialNumber {
		t.Fatal("serial number should change after rotation")
	}
	if rotateResult.PrivateKeyPEM == result.PrivateKeyPEM {
		t.Fatal("rotation must issue a genuinely new private key, not reuse the old one")
	}
	rotatedLeaf := mustParsePEMCertificate(t, rotated.CertificatePEM)
	if _, err := rotatedLeaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Fatalf("rotated leaf certificate does not verify against the CA: %v", err)
	}

	// Policy
	polBody, _ := json.Marshal(domain.CreatePolicyRequest{PolicyName: "ledger-to-treasury", SourceService: "ledger-svc", TargetService: "treasury-svc", Action: domain.PolicyAllow, RequiresMtls: true})
	req5 := httptest.NewRequest(http.MethodPost, "/v1/mtls/policies", bytes.NewBuffer(polBody))
	withEnvelope(req5)
	req5.Header.Set("X-Tenant-ID", "t1")
	req5.Header.Set("X-Principal-Id", "principal-test-01")
	w5 := httptest.NewRecorder()
	router.ServeHTTP(w5, req5)
	if w5.Code != 201 {
		t.Fatalf("expected 201 on policy create, got %d", w5.Code)
	}

	// Revoke
	req6 := httptest.NewRequest(http.MethodDelete, "/v1/mtls/certificates/"+cert.ID, nil)
	withEnvelope(req6)
	req6.Header.Set("X-Tenant-ID", "t1")
	req6.Header.Set("X-Principal-Id", "principal-test-01")
	w6 := httptest.NewRecorder()
	router.ServeHTTP(w6, req6)
	if w6.Code != 200 {
		t.Fatalf("expected 200 on revoke, got %d", w6.Code)
	}
}

func TestValidationError(t *testing.T) {
	body, _ := json.Marshal(domain.ProvisionCertRequest{ServiceName: "svc"})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates", bytes.NewBuffer(body))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	newRouter(t).ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestCA_LoadOrCreate_PersistsAcrossRestarts verifies that reloading a CA
// from the same directory returns the SAME root certificate — the property
// that makes certificate rotation safe across service restarts, unlike the
// old design where every restart would have generated a fresh CA (this
// service never generated a CA at all before this fix, but the property
// matters going forward).
func TestCA_LoadOrCreate_PersistsAcrossRestarts(t *testing.T) {
	dir := t.TempDir()

	first, err := internalca.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreate failed: %v", err)
	}
	second, err := internalca.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreate (reload) failed: %v", err)
	}

	if string(first.CertificatePEM()) != string(second.CertificatePEM()) {
		t.Fatal("reloading the CA from the same directory must return the same root certificate")
	}
}
