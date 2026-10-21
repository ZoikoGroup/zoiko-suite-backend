package handler_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"go.uber.org/zap"
	authzpkg "zoiko.io/mtls-management-svc/internal/authz"
	internalca "zoiko.io/mtls-management-svc/internal/ca"
	"zoiko.io/mtls-management-svc/internal/domain"
	"zoiko.io/mtls-management-svc/internal/handler"
	"zoiko.io/mtls-management-svc/internal/siem"
	"zoiko.io/mtls-management-svc/internal/store"
)

// Regression tests for the 13 Oct 2026 pass (Security & Trust §14,
// Governance register M-01 / M-03): revocation that a verifier can see and
// that survives a restart, rotation that cannot un-revoke, per-service
// bootstrap tokens, and an envelope exemption that no longer swallows the
// human provisioning path.

type fixture struct {
	router    http.Handler
	ca        *internalca.CA
	ledger    *internalca.Ledger
	ledgerDir string
}

func newFixture(t *testing.T, az handler.AuthzChecker, tokens handler.BootstrapTokens) fixture {
	t.Helper()
	c, err := internalca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("CA: %v", err)
	}
	dir := t.TempDir()
	l, err := internalca.OpenLedger(dir)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	h := handler.New(store.NewMemoryStore(), c, l, siem.New("", "mtls-management-svc", zap.NewNop()), az, zap.NewNop(), tokens)
	return fixture{router: handler.NewRouter(h), ca: c, ledger: l, ledgerDir: dir}
}

func (f fixture) do(t *testing.T, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

func (f fixture) provision(t *testing.T) domain.MtlsCertificate {
	t.Helper()
	w := f.do(t, http.MethodPost, "/v1/mtls/certificates",
		domain.ProvisionCertRequest{LegalEntityID: "LE-1", ServiceName: "ledger-svc", CommonName: "ledger-svc", RotationDays: 30}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("provision: %d %s", w.Code, w.Body)
	}
	var res domain.ProvisionCertResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return res.Certificate
}

func (f fixture) rotate(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodPost, "/v1/mtls/certificates/"+id+"/rotate", nil, nil)
}

// fetchCRL reads GET /v1/mtls/crl with NO identity headers at all — verifiers
// call it with none — and checks the CA's signature on it.
func (f fixture) fetchCRL(t *testing.T) *x509.RevocationList {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, handler.CRLPath, nil)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s without identity: %d %s", handler.CRLPath, w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/pkix-crl" {
		t.Fatalf("CRL content type = %q", ct)
	}
	crl, err := x509.ParseRevocationList(w.Body.Bytes())
	if err != nil {
		t.Fatalf("parse CRL: %v", err)
	}
	if err := crl.CheckSignatureFrom(f.ca.Certificate()); err != nil {
		t.Fatalf("CRL is not signed by the CA: %v", err)
	}
	return crl
}

func crlHas(crl *x509.RevocationList, serialHex string) bool {
	want, _ := new(big.Int).SetString(serialHex, 16)
	for _, e := range crl.RevokedCertificateEntries {
		if e.SerialNumber.Cmp(want) == 0 {
			return true
		}
	}
	return false
}

func TestRevoke_SerialPublishedOnSignedCRL(t *testing.T) {
	f := newFixture(t, &stubAuthz{}, nil)
	cert := f.provision(t)
	if crl := f.fetchCRL(t); crlHas(crl, cert.SerialNumber) {
		t.Fatal("an active certificate is on the CRL")
	}
	if w := f.do(t, http.MethodDelete, "/v1/mtls/certificates/"+cert.ID, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if crl := f.fetchCRL(t); !crlHas(crl, cert.SerialNumber) {
		t.Fatal("a revoked certificate is missing from the CRL — peers would keep admitting it (M-03)")
	}
}

// Rotation keeps the record and replaces its serial. The superseded
// certificate stays valid until it expires, so a later revocation must list
// it too — the record used to forget it on rotation.
func TestRevokeAfterRotate_RevokesEverySerial(t *testing.T) {
	f := newFixture(t, &stubAuthz{}, nil)
	cert := f.provision(t)
	w := f.rotate(t, cert.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", w.Code, w.Body)
	}
	var rotated domain.ProvisionCertResult
	_ = json.Unmarshal(w.Body.Bytes(), &rotated)
	if rotated.Certificate.SerialNumber == cert.SerialNumber {
		t.Fatal("rotation did not change the serial")
	}
	if w := f.do(t, http.MethodDelete, "/v1/mtls/certificates/"+cert.ID, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	crl := f.fetchCRL(t)
	for _, s := range []string{cert.SerialNumber, rotated.Certificate.SerialNumber} {
		if !crlHas(crl, s) {
			t.Fatalf("serial %s not revoked; CRL holds %d entries", s, len(crl.RevokedCertificateEntries))
		}
	}
}

// Rotating a revoked certificate used to set it back to ACTIVE and hand out
// fresh key material for a withdrawn identity.
func TestRotateRevoked_RefusedAndStaysRevoked(t *testing.T) {
	f := newFixture(t, &stubAuthz{}, nil)
	cert := f.provision(t)
	if w := f.do(t, http.MethodDelete, "/v1/mtls/certificates/"+cert.ID, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	w := f.rotate(t, cert.ID)
	if w.Code != http.StatusConflict || !bytes.Contains(w.Body.Bytes(), []byte("certificate_revoked")) {
		t.Fatalf("rotate after revoke: want 409 certificate_revoked, got %d %s", w.Code, w.Body)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("PRIVATE KEY")) {
		t.Fatal("a refused rotation leaked key material")
	}
	g := f.do(t, http.MethodGet, "/v1/mtls/certificates/"+cert.ID, nil, nil)
	var got domain.MtlsCertificate
	_ = json.Unmarshal(g.Body.Bytes(), &got)
	if got.Status != domain.CertStatusRevoked || got.SerialNumber != cert.SerialNumber {
		t.Fatalf("after refused rotation: status %s serial %s", got.Status, got.SerialNumber)
	}
}

// A revocation that cannot be made durable is not reported as done, and the
// record is left ACTIVE so the operator's retry still has something to do.
func TestRevoke_LedgerWriteFailure_500AndNotRevoked(t *testing.T) {
	f := newFixture(t, &stubAuthz{}, nil)
	cert := f.provision(t)
	if err := os.RemoveAll(f.ledgerDir); err != nil {
		t.Fatal(err)
	}
	w := f.do(t, http.MethodDelete, "/v1/mtls/certificates/"+cert.ID, nil, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("revoke with unwritable ledger: want 500, got %d %s", w.Code, w.Body)
	}
	g := f.do(t, http.MethodGet, "/v1/mtls/certificates/"+cert.ID, nil, nil)
	var got domain.MtlsCertificate
	_ = json.Unmarshal(g.Body.Bytes(), &got)
	if got.Status != domain.CertStatusActive {
		t.Fatalf("status after failed revoke = %s, want ACTIVE", got.Status)
	}
}

// A denied revoke must not reach the ledger either.
func TestRevokeDenied_NotOnCRL(t *testing.T) {
	az := &switchAuthz{}
	f := newFixture(t, az, nil)
	cert := f.provision(t)
	az.set(authzpkg.ErrAuthorizationDenied)
	if w := f.do(t, http.MethodDelete, "/v1/mtls/certificates/"+cert.ID, nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("denied revoke: %d %s", w.Code, w.Body)
	}
	if f.ledger.IsRevoked(cert.SerialNumber) {
		t.Fatal("a denied revoke reached the ledger")
	}
}

type switchAuthz struct {
	mu  sync.Mutex
	err error
}

func (s *switchAuthz) set(err error) { s.mu.Lock(); s.err = err; s.mu.Unlock() }
func (s *switchAuthz) CheckAllowed(context.Context, string, string, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// ── M-01: per-service bootstrap tokens ────────────────────────────────────

const (
	tokGateway = "gateway-token-0123456789abcdef0123456789"
	tokAuthz   = "authz-token-0123456789abcdef0123456789ab"
)

func bootstrapReq(t *testing.T, f fixture, token, service, cn string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(domain.ProvisionCertRequest{LegalEntityID: "PLATFORM", ServiceName: service, CommonName: cn, RotationDays: 90})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates", bytes.NewReader(body))
	req.Header.Set("X-Tenant-ID", "platform")
	req.Header.Set("X-Mtls-Bootstrap-Token", token)
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

func TestBootstrap_TokenProvisionsOnlyItsOwnService(t *testing.T) {
	// Authorization DENIES: the bootstrap path must not depend on it.
	f := newFixture(t, &stubAuthz{err: authzpkg.ErrAuthorizationDenied},
		handler.BootstrapTokens{tokGateway: "gateway-auth-svc", tokAuthz: "authorization-svc"})

	if w := bootstrapReq(t, f, tokGateway, "gateway-auth-svc", "gateway-auth-svc"); w.Code != http.StatusCreated {
		t.Fatalf("own service: want 201, got %d %s", w.Code, w.Body)
	}
	cases := []struct{ name, service, cn string }{
		{"another service's name", "authorization-svc", "authorization-svc"},
		{"own service_name, another CN", "gateway-auth-svc", "authorization-svc"},
		{"another service_name, own CN", "authorization-svc", "gateway-auth-svc"},
		{"decorated CN", "gateway-auth-svc", "gateway-auth-svc.zoiko.internal"},
	}
	for _, c := range cases {
		w := bootstrapReq(t, f, tokGateway, c.service, c.cn)
		if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte("bootstrap_identity_mismatch")) {
			t.Errorf("%s: want 403 bootstrap_identity_mismatch, got %d %s", c.name, w.Code, w.Body)
		}
	}
	if w := bootstrapReq(t, f, tokAuthz, "authorization-svc", "authorization-svc"); w.Code != http.StatusCreated {
		t.Fatalf("second token, own service: want 201, got %d %s", w.Code, w.Body)
	}
}

func TestBootstrap_NoTokensConfigured_PresentedTokenRefused(t *testing.T) {
	f := newFixture(t, &stubAuthz{}, nil)
	if w := bootstrapReq(t, f, tokGateway, "gateway-auth-svc", "gateway-auth-svc"); w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 when bootstrap is disabled, got %d %s", w.Code, w.Body)
	}
}

// ── envelope exemption narrowed ───────────────────────────────────────────

// The generated policy exempted the whole /v1/mtls/certificates path, so a
// human provisioning call carried no envelope — no idempotency key, and no
// tenant for the authz client to forward. Only a token-bearing POST is exempt.
func TestHumanProvision_RequiresEnvelope(t *testing.T) {
	f := newFixture(t, &stubAuthz{}, nil)
	body, _ := json.Marshal(domain.ProvisionCertRequest{LegalEntityID: "LE-1", ServiceName: "ledger-svc", CommonName: "ledger-svc"})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates", bytes.NewReader(body))
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("human provisioning without an envelope: want a 4xx refusal, got %d %s", w.Code, w.Body)
	}
	if w.Code == http.StatusCreated {
		t.Fatal("certificate issued without an envelope")
	}
}

// End to end through the real authz client: a human provisioning call now
// forwards its tenant to authorization-svc.
func TestHumanProvision_ForwardsTenantToAuthorization(t *testing.T) {
	var gotTenant, gotAction string
	authzSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("X-Tenant-Id")
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotAction = body["action_type"]
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	defer authzSrv.Close()
	f := newFixture(t, authzpkg.NewClient(authzSrv.URL), nil)
	f.provision(t)
	if gotAction != handler.MtlsCertProvision || gotTenant != "t1" {
		t.Fatalf("authorization-svc saw action %q tenant %q; want %q and the caller's tenant t1", gotAction, gotTenant, handler.MtlsCertProvision)
	}
}

// authorization-svc's pre-evaluation refusals are denials, not outages.
func TestAuthzRefusal403_Is403Not503(t *testing.T) {
	authzSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "tenant_scope_mismatch"})
	}))
	defer authzSrv.Close()
	f := newFixture(t, authzpkg.NewClient(authzSrv.URL), nil)
	w := f.do(t, http.MethodGet, "/v1/mtls/certificates?legal_entity_id=LE-1", nil, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("authz 403 tenant_scope_mismatch: want 403, got %d %s", w.Code, w.Body)
	}
}
