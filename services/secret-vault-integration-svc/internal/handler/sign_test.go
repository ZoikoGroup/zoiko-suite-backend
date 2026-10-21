package handler_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
)

var signKey = []byte("0123456789abcdef0123456789abcdef")

func signingStore(class string, workloads string) *stubStore {
	return &stubStore{
		applicableByPath: &domain.ApplicableSecretPolicyVersion{
			SecretPolicyVersion: domain.SecretPolicyVersion{
				SecretPolicyVersionID: "22222222-0000-4000-8000-000000000009",
				AllowedWorkloadIDs:    json.RawMessage(workloads),
			},
			SecretClass: class,
			SecretPath:  "kv/webhooks/t1/sub-1",
		},
	}
}

func signBody(message, requester string) string {
	return `{"secret_path":"kv/webhooks/t1/sub-1","requested_by_principal_id":"` + requester +
		`","request_id":"req-1","message_base64":"` + base64.StdEncoding.EncodeToString([]byte(message)) + `"}`
}

func doSign(t *testing.T, s *stubStore, v *stubVault, body, principal string) *httptest.ResponseRecorder {
	t.Helper()
	r := newTestRouter(s, v, &stubPublisher{})
	req := asWorkload(authed(httptest.NewRequest(http.MethodPost, "/v1/secrets/sign", strings.NewReader(body))), principal)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSign_ReturnsHMACAndNeverTheKey(t *testing.T) {
	s := signingStore("WEBHOOK_SIGNING", `["notification-svc"]`)
	v := &stubVault{getMaterial: append([]byte(nil), signKey...)}
	w := doSign(t, s, v, signBody("1700000000.evt-1.{}", "notification-svc"), "notification-svc")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	m := hmac.New(sha256.New, signKey)
	m.Write([]byte("1700000000.evt-1.{}"))
	want := hex.EncodeToString(m.Sum(nil))
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["signature_hex"] != want || got["algorithm"] != "HMAC-SHA256" {
		t.Fatalf("signature: %v want %s", got, want)
	}
	for _, leak := range []string{string(signKey), base64.StdEncoding.EncodeToString(signKey), hex.EncodeToString(signKey)} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatal("the response contains key material")
		}
	}
	if len(s.auditEntries) != 1 || s.auditEntries[0].EventType != "SIGNED" {
		t.Fatalf("every use must be audited: %+v", s.auditEntries)
	}
	if a := s.auditEntries[0]; a.TenantID == nil || *a.TenantID != testTenant || a.SecretClass != "WEBHOOK_SIGNING" {
		t.Errorf("the audit entry must carry the verified tenant and class: %+v", a)
	}
}

func TestSign_RefusesNonSigningSecretClasses(t *testing.T) {
	for _, class := range []string{"DATABASE_CREDENTIAL", "API_KEY", "webhook_signing", ""} {
		s := signingStore(class, `["notification-svc"]`)
		v := &stubVault{getMaterial: append([]byte(nil), signKey...)}
		w := doSign(t, s, v, signBody("m", "notification-svc"), "notification-svc")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not_a_signing_secret") {
			t.Errorf("class %q: %d %s", class, w.Code, w.Body.String())
		}
		if v.getMaterialCalls != 0 {
			t.Errorf("class %q: key material was read for a refused request", class)
		}
		if len(s.auditEntries) != 1 || s.auditEntries[0].EventType != "DENIED" {
			t.Errorf("class %q: a refusal must be audited: %+v", class, s.auditEntries)
		}
	}
}

func TestSign_RefusesWorkloadsOutsideTheAllowlist(t *testing.T) {
	s := signingStore("WEBHOOK_SIGNING", `["notification-svc"]`)
	v := &stubVault{getMaterial: append([]byte(nil), signKey...)}
	w := doSign(t, s, v, signBody("m", "other-svc"), "other-svc")
	if w.Code != http.StatusForbidden || v.getMaterialCalls != 0 {
		t.Fatalf("%d, material reads %d", w.Code, v.getMaterialCalls)
	}
}

func TestSign_RefusesAnUnverifiedIdentity(t *testing.T) {
	s := signingStore("WEBHOOK_SIGNING", `["notification-svc"]`)
	v := &stubVault{getMaterial: append([]byte(nil), signKey...)}
	// the verified caller is "other-svc" but the body claims the allowed workload
	w := doSign(t, s, v, signBody("m", "notification-svc"), "other-svc")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "workload_identity_mismatch") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if v.getMaterialCalls != 0 {
		t.Fatal("key material was read for an unproven identity")
	}
	d := s.auditEntries[0]
	if d.ActedByPrincipalID == nil || *d.ActedByPrincipalID != "other-svc" || d.RequestedByPrincipalID != "notification-svc" {
		t.Errorf("actor and subject must stay apart: %+v", d)
	}
}

func TestSign_IsPinnedToTheCallersTenant(t *testing.T) {
	s := signingStore("WEBHOOK_SIGNING", `["notification-svc"]`)
	v := &stubVault{getMaterial: append([]byte(nil), signKey...)}
	body := `{"secret_path":"kv/x","tenant_id":"` + otherTenant + `","requested_by_principal_id":"notification-svc","request_id":"r","message_base64":"bQ=="}`
	if w := doSign(t, s, v, body, "notification-svc"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "tenant_scope_mismatch") {
		t.Fatalf("a foreign tenant_id must be refused: %d %s", w.Code, w.Body.String())
	}
	// omitting tenant_id must not widen the lookup to the global policy set
	s = signingStore("WEBHOOK_SIGNING", `["notification-svc"]`)
	_ = doSign(t, s, v, signBody("m", "notification-svc"), "notification-svc")
	if s.auditEntries[0].TenantID == nil || *s.auditEntries[0].TenantID != testTenant {
		t.Fatalf("lookup/audit must be scoped to the verified tenant: %+v", s.auditEntries[0])
	}
}

func TestSign_NoPolicyAndOutages(t *testing.T) {
	s := &stubStore{applicableByPathErr: domain.ErrSecretPolicyNotFound}
	if w := doSign(t, s, &stubVault{}, signBody("m", "notification-svc"), "notification-svc"); w.Code != http.StatusNotFound {
		t.Errorf("no policy: %d", w.Code)
	}
	s = signingStore("WEBHOOK_SIGNING", `["notification-svc"]`)
	v := &stubVault{getMaterialErr: errors.New("backend down")}
	if w := doSign(t, s, v, signBody("m", "notification-svc"), "notification-svc"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("vault backend down: %d (a signature must never be fabricated)", w.Code)
	}
	s = signingStore("WEBHOOK_SIGNING", `["notification-svc"]`)
	s.auditErr = errors.New("audit store down")
	v = &stubVault{getMaterial: append([]byte(nil), signKey...)}
	w := doSign(t, s, v, signBody("m", "notification-svc"), "notification-svc")
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "signature_hex") {
		t.Errorf("a signature no one can account for must not be returned: %d %s", w.Code, w.Body.String())
	}
}

func TestSign_ValidatesInput(t *testing.T) {
	s := signingStore("WEBHOOK_SIGNING", `["notification-svc"]`)
	v := &stubVault{getMaterial: append([]byte(nil), signKey...)}
	for name, body := range map[string]string{
		"missing message":   `{"secret_path":"kv/x","requested_by_principal_id":"notification-svc","request_id":"r"}`,
		"bad base64":        `{"secret_path":"kv/x","requested_by_principal_id":"notification-svc","request_id":"r","message_base64":"!!!"}`,
		"other algorithm":   `{"secret_path":"kv/x","requested_by_principal_id":"notification-svc","request_id":"r","message_base64":"bQ==","algorithm":"MD5"}`,
		"missing path":      `{"requested_by_principal_id":"notification-svc","request_id":"r","message_base64":"bQ=="}`,
		"missing request":   `{"secret_path":"kv/x","requested_by_principal_id":"notification-svc","message_base64":"bQ=="}`,
		"oversized message": signBody(strings.Repeat("x", 190<<10), "notification-svc"),
	} {
		w := doSign(t, s, v, body, "notification-svc")
		if w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if v.getMaterialCalls != 0 {
		t.Error("key material was read for an invalid request")
	}
}
