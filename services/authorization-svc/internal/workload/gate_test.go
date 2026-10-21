package workload_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/mtls"
	"zoiko.io/authorization-svc/internal/workload"
)

// testCA mirrors mtls-management-svc's CA closely enough for the handshake:
// leaves carry CN = service_name and both server and client auth usages.
type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

func newCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (c *testCA) leaf(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn, "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

// echo answers 200 with the X-Workload-Id the handler saw.
var echo = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, r.Header.Get(workload.HeaderWorkloadID))
})

// mtlsServer runs the gate behind the production server tls.Config.
func mtlsServer(t *testing.T, ca *testCA, gate *workload.Gate) *httptest.Server {
	t.Helper()
	certPEM, keyPEM := ca.leaf(t, "authorization-svc")
	cfg, err := mtls.ServerTLSConfig(&mtls.ServerIdentity{CertPEM: certPEM, KeyPEM: keyPEM, CACertPEM: ca.certPEM})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(gate.Middleware(echo))
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func clientFor(t *testing.T, serverCA, issuer *testCA, cn string) *http.Client {
	t.Helper()
	certPEM, keyPEM := issuer.leaf(t, cn)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(serverCA.certPEM)
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{pair}, RootCAs: roots, ServerName: "authorization-svc",
	}}}
}

func post(t *testing.T, c *http.Client, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRequireMode_PlaintextEvaluationRefused401_OtherRoutesUntouched(t *testing.T) {
	gate := workload.New(workload.ModeRequire, []string{"access-control-svc"}, nil)
	srv := httptest.NewServer(gate.Middleware(echo))
	defer srv.Close()

	for _, p := range []string{"/v1/authorize", "/internal/authorization/decisions", "/v1/sod/evaluate",
		"/v1/sod/validate", "/v1/entity-scope/validate", "/v1/delegated-access/evaluate",
		"/internal/v1/gov03/commands/invalidateAuthorizationCache"} {
		code, body := post(t, srv.Client(), srv.URL+p, map[string]string{"X-Workload-Id": "access-control-svc"})
		if code != http.StatusUnauthorized || !strings.Contains(body, "client_certificate_required") {
			t.Errorf("%s over plaintext with a claimed workload: want 401 client_certificate_required, got %d %s", p, code, body)
		}
	}
	for _, p := range []string{"/v1/admin/roles", "/healthz", "/v1/me/capabilities"} {
		if code, _ := post(t, srv.Client(), srv.URL+p, nil); code != http.StatusOK {
			t.Errorf("%s is not an evaluation route and must pass the gate, got %d", p, code)
		}
	}
}

func TestPermitMode_PlaintextEvaluationAdmitted(t *testing.T) {
	gate := workload.New(workload.ModePermit, nil, nil)
	srv := httptest.NewServer(gate.Middleware(echo))
	defer srv.Close()
	if code, _ := post(t, srv.Client(), srv.URL+"/v1/authorize", nil); code != http.StatusOK {
		t.Fatalf("permit mode must admit an un-migrated plaintext caller, got %d", code)
	}
}

func TestRegisteredPEP_Admitted_WorkloadHeaderIsCertificateName(t *testing.T) {
	ca := newCA(t, "platform-ca")
	srv := mtlsServer(t, ca, workload.New(workload.ModeRequire, []string{"access-control-svc"}, nil))
	c := clientFor(t, ca, ca, "access-control-svc")

	code, body := post(t, c, srv.URL+"/v1/authorize", nil)
	if code != http.StatusOK || body != "access-control-svc" {
		t.Fatalf("registered PEP: want 200 with X-Workload-Id=access-control-svc set from the cert, got %d %q", code, body)
	}
}

func TestUnregisteredWorkload_Refused403(t *testing.T) {
	ca := newCA(t, "platform-ca")
	srv := mtlsServer(t, ca, workload.New(workload.ModeRequire, []string{"access-control-svc"}, nil))
	c := clientFor(t, ca, ca, "payroll-svc")

	code, body := post(t, c, srv.URL+"/internal/authorization/decisions", nil)
	if code != http.StatusForbidden || !strings.Contains(body, "workload_not_registered") {
		t.Fatalf("a CA-issued cert for a service not on the list: want 403 workload_not_registered, got %d %s", code, body)
	}
}

func TestUnregisteredWorkload_RefusedInPermitModeToo(t *testing.T) {
	ca := newCA(t, "platform-ca")
	srv := mtlsServer(t, ca, workload.New(workload.ModePermit, []string{"access-control-svc"}, nil))
	c := clientFor(t, ca, ca, "payroll-svc")
	if code, _ := post(t, c, srv.URL+"/v1/authorize", nil); code != http.StatusForbidden {
		t.Fatalf("permit mode relaxes only the plaintext path; a presented cert is still checked, got %d", code)
	}
}

func TestClaimedWorkloadDiffersFromCertificate_Refused403(t *testing.T) {
	ca := newCA(t, "platform-ca")
	srv := mtlsServer(t, ca, workload.New(workload.ModeRequire, []string{"access-control-svc", "payment-svc"}, nil))
	c := clientFor(t, ca, ca, "access-control-svc")

	code, body := post(t, c, srv.URL+"/v1/authorize", map[string]string{"X-Workload-Id": "payment-svc"})
	if code != http.StatusForbidden || !strings.Contains(body, "workload_identity_mismatch") {
		t.Fatalf("cert for access-control-svc claiming payment-svc: want 403 workload_identity_mismatch, got %d %s", code, body)
	}
}

func TestCertificateFromAnotherCA_FailsHandshake(t *testing.T) {
	ca := newCA(t, "platform-ca")
	rogue := newCA(t, "rogue-ca")
	srv := mtlsServer(t, ca, workload.New(workload.ModeRequire, []string{"access-control-svc"}, nil))
	c := clientFor(t, ca, rogue, "access-control-svc")

	if code, _ := post(t, c, srv.URL+"/v1/authorize", nil); code != 0 {
		t.Fatalf("a registered name on a certificate from another CA must not complete the handshake, got HTTP %d", code)
	}
}
