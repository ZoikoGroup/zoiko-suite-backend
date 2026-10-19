package handler_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
)

var useSecret = []byte("provider-bearer-token-123")

const adapterSvc = "payment-initiation-adapter-svc"

func useBody(requester string) string {
	return `{"secret_path":"kv/providers/t1/bank","requested_by_principal_id":"` + requester + `","request_id":"req-1"}`
}

func doUse(t *testing.T, s *stubStore, v *stubVault, body, principal string) *httptest.ResponseRecorder {
	t.Helper()
	r := newTestRouter(s, v, &stubPublisher{})
	req := asWorkload(authed(httptest.NewRequest(http.MethodPost, "/v1/secrets/use", strings.NewReader(body))), principal)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestUse_ReturnsTheCredentialOnlyToAnAllowedWorkloadAndAuditsIt(t *testing.T) {
	for _, class := range []string{"PROVIDER_CREDENTIAL", "CALLBACK_SECRET"} {
		s := signingStore(class, `["`+adapterSvc+`"]`)
		v := &stubVault{getMaterial: append([]byte(nil), useSecret...)}
		w := doUse(t, s, v, useBody(adapterSvc), adapterSvc)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", class, w.Code, w.Body.String())
		}
		var got map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if b, _ := base64.StdEncoding.DecodeString(got["credential_base64"]); string(b) != string(useSecret) {
			t.Fatalf("credential: %v", got)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Error("a credential response must not be cacheable")
		}
		if len(s.auditEntries) != 1 || s.auditEntries[0].EventType != "USED" || s.auditEntries[0].SecretClass != class {
			t.Fatalf("every use is audited: %+v", s.auditEntries)
		}
		if a := s.auditEntries[0]; a.TenantID == nil || *a.TenantID != testTenant {
			t.Errorf("the audit entry must carry the verified tenant: %+v", a)
		}
	}
}

func TestUse_RefusesEveryOtherClass(t *testing.T) {
	for _, class := range []string{"WEBHOOK_SIGNING", "DATABASE_CREDENTIAL", "API_KEY", "provider_credential", ""} {
		s := signingStore(class, `["`+adapterSvc+`"]`)
		v := &stubVault{getMaterial: append([]byte(nil), useSecret...)}
		w := doUse(t, s, v, useBody(adapterSvc), adapterSvc)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not_a_usable_secret") {
			t.Errorf("class %q: %d %s", class, w.Code, w.Body.String())
		}
		if v.getMaterialCalls != 0 || strings.Contains(w.Body.String(), "credential_base64") {
			t.Errorf("class %q: material was read or returned for a refused class", class)
		}
		if len(s.auditEntries) != 1 || s.auditEntries[0].EventType != "DENIED" {
			t.Errorf("class %q: refusal must be audited: %+v", class, s.auditEntries)
		}
	}
}

func TestUse_RefusesOutsideTheAllowlistAndUnprovenIdentity(t *testing.T) {
	s := signingStore("PROVIDER_CREDENTIAL", `["`+adapterSvc+`"]`)
	v := &stubVault{getMaterial: append([]byte(nil), useSecret...)}
	if w := doUse(t, s, v, useBody("other-svc"), "other-svc"); w.Code != http.StatusForbidden || v.getMaterialCalls != 0 {
		t.Fatalf("not allow-listed: %d reads=%d", w.Code, v.getMaterialCalls)
	}
	// claims the allowed workload, but the verified caller is someone else
	s = signingStore("PROVIDER_CREDENTIAL", `["`+adapterSvc+`"]`)
	w := doUse(t, s, v, useBody(adapterSvc), "other-svc")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "workload_identity_mismatch") || v.getMaterialCalls != 0 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestUse_TenantPinAndFailuresFailClosed(t *testing.T) {
	s := signingStore("PROVIDER_CREDENTIAL", `["`+adapterSvc+`"]`)
	v := &stubVault{getMaterial: append([]byte(nil), useSecret...)}
	body := `{"secret_path":"kv/x","tenant_id":"` + otherTenant + `","requested_by_principal_id":"` + adapterSvc + `","request_id":"r"}`
	if w := doUse(t, s, v, body, adapterSvc); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "tenant_scope_mismatch") {
		t.Fatalf("foreign tenant: %d %s", w.Code, w.Body.String())
	}
	s = signingStore("PROVIDER_CREDENTIAL", `["`+adapterSvc+`"]`)
	s.auditErr = errors.New("audit down")
	w := doUse(t, s, &stubVault{getMaterial: append([]byte(nil), useSecret...)}, useBody(adapterSvc), adapterSvc)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "credential_base64") {
		t.Fatalf("an unaccountable credential must be withheld: %d %s", w.Code, w.Body.String())
	}
	s = signingStore("PROVIDER_CREDENTIAL", `["`+adapterSvc+`"]`)
	if w := doUse(t, s, &stubVault{getMaterialErr: errors.New("down")}, useBody(adapterSvc), adapterSvc); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("backend down: %d", w.Code)
	}
	for name, b := range map[string]string{
		"no path":    `{"requested_by_principal_id":"` + adapterSvc + `","request_id":"r"}`,
		"no request": `{"secret_path":"kv/x","requested_by_principal_id":"` + adapterSvc + `"}`,
	} {
		if w := doUse(t, s, &stubVault{}, b, adapterSvc); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
}

func TestUse_ByPolicyID(t *testing.T) {
	s := signingStore("PROVIDER_CREDENTIAL", `["`+adapterSvc+`"]`)
	s.findPolicyResult = &domain.SecretPolicy{SecretPolicyID: "11111111-0000-4000-8000-000000000001", SecretPath: "kv/providers/t1/bank"}
	v := &stubVault{getMaterial: append([]byte(nil), useSecret...)}
	body := `{"secret_policy_id":"11111111-0000-4000-8000-000000000001","requested_by_principal_id":"` + adapterSvc + `","request_id":"r"}`
	w := doUse(t, s, v, body, adapterSvc)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "credential_base64") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// an unknown id is "not found" and is audited, and nothing is read
	s = signingStore("PROVIDER_CREDENTIAL", `["`+adapterSvc+`"]`)
	s.findPolicyErr = domain.ErrSecretPolicyNotFound
	v = &stubVault{getMaterial: append([]byte(nil), useSecret...)}
	w = doUse(t, s, v, body, adapterSvc)
	if w.Code != http.StatusNotFound || v.getMaterialCalls != 0 || len(s.auditEntries) != 1 || s.auditEntries[0].EventType != "DENIED" {
		t.Fatalf("%d reads=%d audit=%+v", w.Code, v.getMaterialCalls, s.auditEntries)
	}
	// neither path nor id
	w = doUse(t, s, v, `{"requested_by_principal_id":"`+adapterSvc+`","request_id":"r"}`, adapterSvc)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("%d", w.Code)
	}
}

func TestSign_ByPolicyID(t *testing.T) {
	s := signingStore("WEBHOOK_SIGNING", `["notification-svc"]`)
	s.findPolicyResult = &domain.SecretPolicy{SecretPolicyID: "11111111-0000-4000-8000-000000000001", SecretPath: "kv/webhooks/t1/sub-1"}
	v := &stubVault{getMaterial: append([]byte(nil), signKey...)}
	body := `{"secret_policy_id":"11111111-0000-4000-8000-000000000001","requested_by_principal_id":"notification-svc","request_id":"r","message_base64":"bQ=="}`
	if w := doSign(t, s, v, body, "notification-svc"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "signature_hex") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	s.findPolicyErr = domain.ErrSecretPolicyNotFound
	if w := doSign(t, s, v, body, "notification-svc"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown policy id: %d", w.Code)
	}
}
