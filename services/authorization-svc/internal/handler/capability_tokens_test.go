package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
)

// N-16, second instance (13 Oct 2026 pass 2): /v1/me/capabilities derived its
// feature flags from substrings, so a postcode editor was told it could post
// journals and a SIEM exporter reader that it could export data.
func TestCapabilities_FeatureFlagsMatchTokensNotSubstrings(t *testing.T) {
	cases := []struct {
		action string
		want   map[string]bool
	}{
		{"ADDRESS_POSTCODE_UPDATE", map[string]bool{}},
		{"SIEM_EXPORTER_READ", map[string]bool{}},
		{"PRERELEASE_NOTES_VIEW", map[string]bool{}},
		{"GL_JOURNAL_POST", map[string]bool{"can_post_journals": true}},
		{"PAYMENT_RELEASE", map[string]bool{"can_release_payments": true}},
		{"invoice.approve", map[string]bool{"can_approve": true}},
		{"REPORT_EXPORT", map[string]bool{"can_export": true}},
	}
	for _, c := range cases {
		s := &stubStore{rbacActions: []string{c.action}, entityTenant: tenantA}
		req := httptest.NewRequest(http.MethodGet, "/v1/me/capabilities", nil)
		req.Header.Set("X-Principal-Id", "p-1")
		req.Header.Set("X-Tenant-Id", tenantA)
		req.Header.Set("X-Legal-Entity-Id", "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
		w := httptest.NewRecorder()
		newTestRouter(s).ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.action, w.Code, w.Body.String())
		}
		var resp domain.CapabilitiesResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		for _, flag := range []string{"can_post_journals", "can_release_payments", "can_approve", "can_export"} {
			if resp.Features[flag] != c.want[flag] {
				t.Errorf("%s: %s = %v, want %v", c.action, flag, resp.Features[flag], c.want[flag])
			}
		}
	}
}

// A support session without bulk export may still use an action that merely
// names an exporter; one that exports is refused.
func TestSupportSession_ExportClassifiedByToken(t *testing.T) {
	future := time.Now().Add(time.Hour)
	for action, wantPermit := range map[string]bool{
		"SIEM_EXPORTER_READ": true,
		"LEDGER_EXPORT_READ": false,
	} {
		s := &stubStore{supportSession: &domain.SupportSession{SessionID: "ss-1", SupportOperatorID: "p-1",
			Status: domain.SupportSessionStatusActive, ExpiresAt: future, ReadOnly: true, TenantConsentObtained: true}}
		req := httptest.NewRequest(http.MethodPost, handler.CanonicalDecisionsPath, bytes.NewBufferString(
			`{"subject_id":"p-1","action":"`+action+`","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","tenant_id":"11111111-1111-4111-8111-111111111111","support_session_id":"ss-1"}`))
		w := httptest.NewRecorder()
		newTestRouter(s).ServeHTTP(w, req)
		permitted := bytes.Contains(w.Body.Bytes(), []byte(`"decision":"PERMIT"`))
		if permitted != wantPermit {
			t.Errorf("%s in a read-only support session without export: permit=%v, want %v (%s)", action, permitted, wantPermit, w.Body.String())
		}
	}
}
