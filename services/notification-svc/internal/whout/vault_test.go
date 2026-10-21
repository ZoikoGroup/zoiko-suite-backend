package whout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const sigHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func policies(path, class, status string, tenant *string, workloads ...string) string {
	w, _ := json.Marshal(workloads)
	t := "null"
	if tenant != nil {
		t = `"` + *tenant + `"`
	}
	return `[{"secret_path":"` + path + `","secret_class":"` + class + `","version_status":"` + status + `","tenant_id":` + t + `,"allowed_workload_ids":` + string(w) + `}]`
}

func newVault(t *testing.T, h http.HandlerFunc) *VaultClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewVaultClient(srv.URL, nil, "notification-svc")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVaultVerify(t *testing.T) {
	tenant := "t1"
	other := "t2"
	caller := VaultCaller{PrincipalID: "approver", TenantID: "t1", CorrelationID: "c1"}
	cases := map[string]struct {
		status int
		body   string
		want   error
	}{
		"active signing policy":        {200, policies("kv/k", SigningSecretClass, "ACTIVE", &tenant, "notification-svc"), nil},
		"global policy":                {200, policies("kv/k", SigningSecretClass, "ACTIVE", nil, "notification-svc"), nil},
		"another tenant's policy":      {200, policies("kv/k", SigningSecretClass, "ACTIVE", &other, "notification-svc"), ErrKeyUnavailable},
		"a different path":             {200, policies("kv/other", SigningSecretClass, "ACTIVE", &tenant, "notification-svc"), ErrKeyUnavailable},
		"not active":                   {200, policies("kv/k", SigningSecretClass, "DRAFT", &tenant, "notification-svc"), ErrKeyUnavailable},
		"not a signing class":          {200, policies("kv/k", "DATABASE_CREDENTIAL", "ACTIVE", &tenant, "notification-svc"), ErrKeyUnavailable},
		"we are not an allowed signer": {200, policies("kv/k", SigningSecretClass, "ACTIVE", &tenant, "someone-else"), ErrKeyUnavailable},
		"empty":                        {200, `[]`, ErrKeyUnavailable},
		"vault refuses the caller":     {403, ``, ErrKeyUnavailable},
		"vault unauthenticated":        {401, ``, ErrKeyUnavailable},
		"vault down":                   {503, ``, ErrDependency},
		"vault error":                  {500, ``, ErrDependency},
		"garbage":                      {200, `not json`, ErrDependency},
	}
	for name, c := range cases {
		var gotPrincipal, gotTenant, gotQuery string
		v := newVault(t, func(w http.ResponseWriter, r *http.Request) {
			gotPrincipal, gotTenant, gotQuery = r.Header.Get("X-Principal-Id"), r.Header.Get("X-Tenant-Id"), r.URL.RawQuery
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		})
		err := v.Verify(context.Background(), "kv/k", caller)
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		if gotPrincipal != "approver" || gotTenant != "t1" || !strings.Contains(gotQuery, "secret_class=WEBHOOK_SIGNING") {
			t.Errorf("%s: the confirmation must be made as the caller, for the signing class: %q %q %q", name, gotPrincipal, gotTenant, gotQuery)
		}
	}
	// an unreachable vault fails closed
	c, _ := NewVaultClient("http://127.0.0.1:1", &http.Client{Timeout: 200 * time.Millisecond}, "notification-svc")
	if err := c.Verify(context.Background(), "kv/k", caller); !errors.Is(err, ErrDependency) {
		t.Errorf("unreachable: %v", err)
	}
}

func TestVaultSign(t *testing.T) {
	var got map[string]string
	var gotPrincipal, gotTenant string
	v := newVault(t, func(w http.ResponseWriter, r *http.Request) {
		gotPrincipal, gotTenant = r.Header.Get("X-Principal-Id"), r.Header.Get("X-Tenant-Id")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"signature_hex":"` + sigHex + `"}`))
	})
	sig, err := v.Sign(context.Background(), "t1", "kv/k", "req-1", "corr", []byte("1.evt.body"))
	if err != nil || sig != sigHex {
		t.Fatalf("%q %v", sig, err)
	}
	msg, _ := base64.StdEncoding.DecodeString(got["message_base64"])
	if string(msg) != "1.evt.body" || got["secret_path"] != "kv/k" || got["request_id"] != "req-1" ||
		got["requested_by_principal_id"] != "notification-svc" || gotPrincipal != "notification-svc" || gotTenant != "t1" || got["algorithm"] != "HMAC-SHA256" {
		t.Fatalf("request: %v %s %s", got, gotPrincipal, gotTenant)
	}
	for name, h := range map[string]http.HandlerFunc{
		"vault 403":       func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) },
		"vault 503":       func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) },
		"short signature": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"signature_hex":"abcd"}`)) },
		"no signature":    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
		"garbage":         func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`x`)) },
	} {
		sig, err := newVault(t, h).Sign(context.Background(), "t1", "kv/k", "r", "", []byte("m"))
		if !errors.Is(err, ErrSignerUnavailable) || sig != "" {
			t.Errorf("%s: %q %v (a failure must never yield a signature)", name, sig, err)
		}
	}
	c, _ := NewVaultClient("http://127.0.0.1:1", &http.Client{Timeout: 200 * time.Millisecond}, "notification-svc")
	if _, err := c.Sign(context.Background(), "t1", "kv/k", "r", "", []byte("m")); !errors.Is(err, ErrSignerUnavailable) {
		t.Errorf("unreachable: %v", err)
	}
}

func TestNewVaultClientRefusesBadSetup(t *testing.T) {
	if _, err := NewVaultClient("not a url", nil, "p"); err == nil {
		t.Error("a relative URL was accepted")
	}
	if _, err := NewVaultClient("http://v", nil, " "); err == nil {
		t.Error("a missing principal was accepted")
	}
	if _, err := NewVaultClient("http://v", &http.Client{}, "p"); err == nil {
		t.Error("a client with no timeout (an infinite wait) was accepted")
	}
}

// The real vault refuses a governed request that lacks its canonical input contract
// (400 envelope_incomplete). A stub does not, which is how a client without these
// headers passes every test and then fails closed on every call in production.
func TestVaultRequestsCarryTheCanonicalContract(t *testing.T) {
	var verify, sign http.Header
	v := newVault(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			verify = r.Header.Clone()
			_, _ = w.Write([]byte(`[]`))
			return
		}
		sign = r.Header.Clone()
		_, _ = w.Write([]byte(`{"signature_hex":"` + sigHex + `"}`))
	})
	_ = v.Verify(context.Background(), "kv/k", VaultCaller{PrincipalID: "approver", TenantID: "t1", CorrelationID: "corr-1"})
	if _, err := v.Sign(context.Background(), "t1", "kv/k", "delivery:1:0", "corr-2", []byte("m")); err != nil {
		t.Fatal(err)
	}
	validChannels := map[string]bool{"api": true, "import": true, "integration": true, "mobile": true, "scheduled_job": true, "system": true, "web": true}
	for name, h := range map[string]http.Header{"verify": verify, "sign": sign} {
		for _, k := range []string{"X-Principal-Id", "X-Tenant-Id", "X-Request-Id", "X-Source-Channel", "X-Purpose-Context"} {
			if h.Get(k) == "" {
				t.Errorf("%s: %s is missing", name, k)
			}
		}
		if !validChannels[h.Get("X-Source-Channel")] {
			t.Errorf("%s: %q is not a channel the contract accepts", name, h.Get("X-Source-Channel"))
		}
	}
	if sign.Get("Idempotency-Key") != "delivery:1:0" || sign.Get("X-Request-Id") != "delivery:1:0" {
		t.Errorf("a signing request is a write: it needs its idempotency key (%q) and trace id (%q)", sign.Get("Idempotency-Key"), sign.Get("X-Request-Id"))
	}
	if sign.Get("X-Purpose-Context") == verify.Get("X-Purpose-Context") {
		t.Error("verification and signing are different purposes")
	}
}
