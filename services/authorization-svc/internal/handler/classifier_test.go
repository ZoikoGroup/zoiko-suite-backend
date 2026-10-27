package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
)

// Governance audit N-16: the verb classifiers matched substrings, so a read
// such as ADDRESS_POSTCODE_READ counted as a posting and was refused on a
// CLOSED resource.
func TestTerminalStateGuard_MatchesTokensNotSubstrings(t *testing.T) {
	for action, want := range map[string]string{
		"ADDRESS_POSTCODE_READ": "GRANTED", // "post" inside a word
		"GL_JOURNAL_POST":       "DENIED",
		"invoice.approve":       "DENIED",
		"PRERELEASE_NOTES_VIEW": "GRANTED", // "release" inside a word
	} {
		s := &stubStore{rbacActions: []string{action}, rbacBasis: "rbac:role=R"}
		body := `{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"` + action +
			`","tenant_id":"11111111-1111-4111-8111-111111111111","attributes":{"status":"CLOSED"}}`
		req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		newTestRouter(s).ServeHTTP(w, req)
		if got, basis := decodeDecision(t, w); got != want {
			t.Errorf("%s on a CLOSED resource: %s (%s), want %s", action, got, basis, want)
		}
	}
}

// The canonical decision API now carries the break-glass and support session
// layers, as /v1/authorize does.
func TestCanonical_AcceptsBreakGlassAndSupportSessions(t *testing.T) {
	future := time.Now().Add(time.Hour)
	cases := map[string]*stubStore{
		`"break_glass_session_id":"bg-1"`: {breakGlassSession: &domain.BreakGlassSession{SessionID: "bg-1", PrincipalID: "p-1",
			IncidentID: "INC-1", Status: domain.BreakGlassSessionStatusActive, ExpiresAt: future, RequestedActions: []string{"ledger.view"}}},
		`"support_session_id":"ss-1"`: {supportSession: &domain.SupportSession{SessionID: "ss-1", SupportOperatorID: "p-1",
			Status: domain.SupportSessionStatusActive, ExpiresAt: future, ReadOnly: true, TenantConsentObtained: true}},
	}
	for field, s := range cases {
		req := httptest.NewRequest(http.MethodPost, handler.CanonicalDecisionsPath, bytes.NewBufferString(
			`{"subject_id":"p-1","action":"ledger.view","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","tenant_id":"11111111-1111-4111-8111-111111111111",`+field+`}`))
		w := httptest.NewRecorder()
		newTestRouter(s).ServeHTTP(w, req)
		if !bytes.Contains(w.Body.Bytes(), []byte(`"decision":"PERMIT"`)) {
			t.Errorf("%s: want PERMIT through the session, got %d %s", field, w.Code, w.Body.String())
		}
	}
}
