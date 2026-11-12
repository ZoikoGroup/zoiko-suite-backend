package handler_test

// Input-contract negative paths of the admin and runtime routes, at the
// handler: each case sends a real request body and asserts the RFC 9457 status
// and problem code, and that a refused request never reached the store.
//
// The specification's negative-path scenarios (JUR-NEG-01…30, REF-01 #22/#23,
// REF-10 #40/#41, PDC-02 NP-11…NP-20) need the real store and run in
// internal/registryit/negative_paths_integration_test.go, one test per id.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

type negCase struct {
	name       string
	method     string
	path       string
	body       string
	headers    map[string]string
	noIdentity bool
	wantStatus int
	wantCode   string
}

func runNegCases(t *testing.T, cases []negCase, newStore func() *stubStore) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			router := newTestRouter(st)
			var body *bytes.Reader
			if tc.body != "" {
				body = bytes.NewReader([]byte(tc.body))
			} else {
				body = bytes.NewReader(nil)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Correlation-ID", "neg-"+strings.ReplaceAll(tc.name, " ", "-"))
			if !tc.noIdentity {
				req.Header.Set("X-Principal-Id", testPrincipal)
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rr := executeRequest(router, req)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Fatalf("Content-Type = %q, want application/problem+json", ct)
			}
			var p map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
				t.Fatalf("problem body is not JSON: %v (%s)", err, rr.Body.String())
			}
			if p["code"] != tc.wantCode {
				t.Fatalf("code = %v, want %q; body: %s", p["code"], tc.wantCode, rr.Body.String())
			}
			if int(p["status"].(float64)) != tc.wantStatus {
				t.Fatalf("problem status = %v, want %d", p["status"], tc.wantStatus)
			}
			if st.createJurisdictionArgs.JurisdictionCode != "" || st.createRuleArgs.RuleCode != "" ||
				st.transitionArgs.NewStatus != "" || st.driftArgs.ToState != "" {
				t.Fatalf("a refused request reached the store")
			}
		})
	}
}

func TestJUR_ValidateAction_NegativePaths(t *testing.T) {
	const p = "/v1/validate-action"
	runNegCases(t, []negCase{
		{name: "no caller identity", method: http.MethodPost, path: p, noIdentity: true,
			body: `{"action_type":"TAX.STD","jurisdiction_id":"j-1","entity_type":"INVOICE"}`,
			wantStatus: http.StatusUnauthorized, wantCode: "missing_principal"},
		{name: "malformed JSON", method: http.MethodPost, path: p, body: `{not json}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_json"},
		{name: "unknown field", method: http.MethodPost, path: p,
			body:       `{"action_type":"TAX.STD","jurisdiction_id":"j-1","entity_type":"INVOICE","country":"GB"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_json"},
		{name: "missing jurisdiction_id", method: http.MethodPost, path: p,
			body:       `{"action_type":"TAX.STD","entity_type":"INVOICE"}`,
			wantStatus: http.StatusBadRequest, wantCode: "missing_field"},
		{name: "missing entity_type", method: http.MethodPost, path: p,
			body:       `{"action_type":"TAX.STD","jurisdiction_id":"j-1"}`,
			wantStatus: http.StatusBadRequest, wantCode: "missing_field"},
		{name: "action_type without a domain", method: http.MethodPost, path: p,
			body:       `{"action_type":"STD","jurisdiction_id":"j-1","entity_type":"INVOICE"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_action_type"},
		{name: "payload that is not an object", method: http.MethodPost, path: p,
			body:       `{"action_type":"TAX.STD","jurisdiction_id":"j-1","entity_type":"INVOICE","payload":[1]}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_field"},
		{name: "malformed effective_at", method: http.MethodPost, path: p,
			body:       `{"action_type":"TAX.STD","jurisdiction_id":"j-1","entity_type":"INVOICE","effective_at":"tomorrow"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_json"},
	}, func() *stubStore { return &stubStore{} })
}

func TestJUR_ValidateAction_UnknownJurisdictionIs404(t *testing.T) {
	st := &stubStore{rulePackErr: domain.ErrJurisdictionNotFound}
	req := httptest.NewRequest(http.MethodPost, "/v1/validate-action",
		strings.NewReader(`{"action_type":"TAX.STD","jurisdiction_id":"nowhere","entity_type":"INVOICE"}`))
	req.Header.Set("X-Principal-Id", testPrincipal)
	rr := executeRequest(newTestRouter(st), req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (REF-01 #22: an unknown jurisdiction is refused, never guessed); body %s", rr.Code, rr.Body.String())
	}
}

func TestJUR_CreateJurisdiction_NegativePaths(t *testing.T) {
	const p = "/v1/admin/jurisdictions"
	ok := `"jurisdiction_name":"Test","jurisdiction_type":"COUNTRY","authority_type":"NATIONAL","effective_from":"2026-01-01T00:00:00Z"`
	runNegCases(t, []negCase{
		{name: "no caller identity", method: http.MethodPost, path: p, noIdentity: true,
			body: `{"jurisdiction_code":"TT",` + ok + `}`, wantStatus: http.StatusUnauthorized, wantCode: "missing_principal"},
		{name: "empty body", method: http.MethodPost, path: p, body: ``,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request_body"},
		{name: "two JSON objects", method: http.MethodPost, path: p, body: `{"jurisdiction_code":"TT",` + ok + `}{}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request_body"},
		{name: "unknown field", method: http.MethodPost, path: p, body: `{"jurisdiction_code":"TT","iso":"x",` + ok + `}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request_body"},
		{name: "missing jurisdiction_code", method: http.MethodPost, path: p, body: `{` + ok + `}`,
			wantStatus: http.StatusBadRequest, wantCode: "missing_field"},
		{name: "missing effective_from", method: http.MethodPost, path: p,
			body:       `{"jurisdiction_code":"TT","jurisdiction_name":"Test","jurisdiction_type":"COUNTRY","authority_type":"NATIONAL"}`,
			wantStatus: http.StatusBadRequest, wantCode: "missing_field"},
		{name: "jurisdiction_code over 32 characters", method: http.MethodPost, path: p,
			body:       `{"jurisdiction_code":"` + strings.Repeat("X", 33) + `",` + ok + `}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_jurisdiction_code"},
		{name: "effective_to before effective_from", method: http.MethodPost, path: p,
			body:       `{"jurisdiction_code":"TT",` + ok + `,"effective_to":"2025-01-01T00:00:00Z"}`,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_effective_period"},
	}, func() *stubStore { return &stubStore{} })
}

func TestJUR_IfMatch_NegativePaths(t *testing.T) {
	runNegCases(t, []negCase{
		{name: "rule transition with a malformed If-Match", method: http.MethodPost,
			path: "/v1/admin/rules/r-1/transition", body: `{"new_status":"ACTIVE"}`,
			headers: map[string]string{"If-Match": `"abc"`}, wantStatus: http.StatusBadRequest, wantCode: "invalid_if_match"},
		{name: "drift with a malformed If-Match", method: http.MethodPost,
			path: "/v1/admin/rules/r-1/drift", body: `{"drift_state":"DRIFTED","reason":"x"}`,
			headers: map[string]string{"If-Match": `"0"`}, wantStatus: http.StatusBadRequest, wantCode: "invalid_if_match"},
		{name: "deactivate with a malformed If-Match", method: http.MethodPost,
			path: "/v1/admin/jurisdictions/j-1/deactivate", body: `{"reason":"retired by ISO","evidence_ref":"iso-3166:2026-1"}`,
			headers: map[string]string{"If-Match": `W/"v"`}, wantStatus: http.StatusBadRequest, wantCode: "invalid_if_match"},
	}, func() *stubStore { return &stubStore{} })
}
