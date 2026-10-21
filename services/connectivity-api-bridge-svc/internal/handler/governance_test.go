package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
	"zoiko.io/eventing/integration"
)

type problemBody struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Detail        string `json:"detail"`
	Error         string `json:"error"`
	Code          string `json:"code"`
	Instance      string `json:"instance"`
	CorrelationID string `json:"correlation_id"`
}

func problemOf(t *testing.T, w *httptest.ResponseRecorder) problemBody {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != integration.ProblemContentType {
		t.Fatalf("content type %q, want %q (body %s)", ct, integration.ProblemContentType, w.Body.String())
	}
	var p problemBody
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func ver(b domain.ApiBridge) map[string]any { return map[string]any{"expected_version": b.Version} }

// ---- problem details -------------------------------------------------------------

func TestErrorsAreProblemDetailsThatKeepTheLegacyErrorMember(t *testing.T) {
	_, h := setupTestRouter(t)
	w := h.call("POST", "/v1/bridges", "t1", "maker", map[string]any{"legal_entity_id": "le-1"}, "X-Correlation-ID", "corr-9")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	p := problemOf(t, w)
	if p.Code != "INT-SCHEMA-004" || p.Type != "urn:zoikosuite:problem:INT-SCHEMA-004" || p.Status != 400 ||
		p.Instance != "/v1/bridges" || p.CorrelationID != "corr-9" {
		t.Fatalf("%+v", p)
	}
	if p.Error == "" || p.Error != p.Detail {
		t.Fatalf("clients reading the old {\"error\": ...} member must still get the message: %+v", p)
	}
}

func TestInternalFaultsAreOpaque(t *testing.T) {
	w := httptest.NewRecorder()
	writeProblem(w, nil, assertErr("pq: connection to 10.0.0.5 refused: password authentication failed"))
	if w.Code != 500 || strings.Contains(w.Body.String(), "10.0.0.5") || strings.Contains(w.Body.String(), "password") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

func TestMissingPrincipalIs401(t *testing.T) {
	_, h := setupTestRouter(t)
	w := h.call("POST", "/v1/bridges", "t1", "", fullBody())
	if w.Code != http.StatusUnauthorized || problemOf(t, w).Code != "INT-AUTH-003" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// ---- lifecycle over HTTP ---------------------------------------------------------

func TestFullLifecycleOverHTTP(t *testing.T) {
	_, h := setupTestRouter(t)
	b := h.activeBridge(t, "t1")
	if b.CreatedBy != "maker" || b.ApprovedBy != "checker" || b.ApprovedFingerprint == "" || b.Version != 4 {
		t.Fatalf("%+v", b)
	}
	if !strings.HasPrefix(b.ConfigFingerprint, "sha256:") {
		t.Fatalf("%+v", b)
	}
	// every step is on the record
	w := h.call("GET", "/v1/bridges/"+b.BridgeID+"/changes", "t1", "maker", nil)
	var ch struct {
		Changes []domain.Change `json:"changes"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &ch)
	kinds := make([]string, 0)
	for _, c := range ch.Changes {
		kinds = append(kinds, c.Kind)
	}
	if strings.Join(kinds, ",") != "CREATE,SUBMIT,APPROVE,ACTIVATE" {
		t.Fatalf("change history = %v", kinds)
	}
	w = h.call("GET", "/v1/bridges/"+b.BridgeID+"/approvals", "t1", "maker", nil)
	var ap struct {
		Approvals []domain.Approval `json:"approvals"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &ap)
	if len(ap.Approvals) != 1 || ap.Approvals[0].DecidedBy != "checker" || ap.Approvals[0].RequestedBy != "maker" {
		t.Fatalf("%+v", ap)
	}
}

func TestSelfApprovalIs403OverHTTP(t *testing.T) {
	_, h := setupTestRouter(t)
	b := decodeBridge(t, h.call("POST", "/v1/bridges", "t1", "maker", fullBody()))
	b = decodeBridge(t, h.call("POST", "/v1/bridges/"+b.BridgeID+"/submit", "t1", "maker", ver(b)))
	w := h.call("POST", "/v1/bridges/"+b.BridgeID+"/approve", "t1", "maker", ver(b))
	if w.Code != http.StatusForbidden || problemOf(t, w).Code != "INT-AUTH-003" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got := decodeBridge(t, h.call("GET", "/v1/bridges/"+b.BridgeID, "t1", "maker", nil)); got.Status != domain.StatusPendingApproval || got.ApprovedBy != "" {
		t.Fatalf("a refused approval must change nothing: %+v", got)
	}
}

func TestExpectedVersionIsRequiredAndChecked(t *testing.T) {
	_, h := setupTestRouter(t)
	b := decodeBridge(t, h.call("POST", "/v1/bridges", "t1", "maker", fullBody()))
	for _, path := range []string{"submit", "approve", "reject", "activate", "suspend", "revoke", "archive"} {
		w := h.call("POST", "/v1/bridges/"+b.BridgeID+"/"+path, "t1", "maker", map[string]any{})
		if w.Code != http.StatusBadRequest || problemOf(t, w).Code != "INT-SCHEMA-004" {
			t.Errorf("%s without expected_version: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := h.call("POST", "/v1/bridges/"+b.BridgeID+"/submit", "t1", "maker", map[string]any{"expected_version": b.Version + 7})
	if w.Code != http.StatusConflict || problemOf(t, w).Code != "INT-IDEM-005" {
		t.Fatalf("stale version: %d %s", w.Code, w.Body.String())
	}
	w = h.call("PATCH", "/v1/bridges/"+b.BridgeID, "t1", "maker", map[string]any{"purpose": "x-purpose"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PATCH without expected_version: %d", w.Code)
	}
}

func TestSubmitRefusalsAreStableProblems(t *testing.T) {
	_, h := setupTestRouter(t)
	create := func(mutate func(map[string]any)) domain.ApiBridge {
		body := fullBody()
		mutate(body)
		w := h.call("POST", "/v1/bridges", "t1", "maker", body)
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		return decodeBridge(t, w)
	}
	for name, tc := range map[string]struct {
		mutate func(map[string]any)
		status int
		code   string
	}{
		"region outside the deployment": {func(b map[string]any) { b["destination_region"] = "us-east" }, 403, "INT-REG-002"},
		"scope the profile lacks":       {func(b map[string]any) { b["scopes"] = []string{"ingest", "admin.all"} }, 403, "INT-SCOPE-013"},
		"host not on the allowlist": {func(b map[string]any) {
			b["endpoint_url"] = "https://api.evil.example/x"
			h.svc.Egress.Resolver = fakeDNS{"api.evil.example": {"93.184.216.50"}, "api.bank-x.example": {"93.184.216.34"}}
		}, 403, "INT-REG-002"},
		"credential unknown":       {func(b map[string]any) { b["credential_ref"] = "0a000000-0000-4000-8000-0000000000ee" }, 422, "INT-CRED-010"},
		"no such profile version":  {func(b map[string]any) { b["provider_profile_version"] = 9 }, 409, "INT-VERS-012"},
		"incomplete configuration": {func(b map[string]any) { delete(b, "purpose") }, 400, "INT-SCHEMA-004"},
	} {
		t.Run(name, func(t *testing.T) {
			b := create(tc.mutate)
			w := h.call("POST", "/v1/bridges/"+b.BridgeID+"/submit", "t1", "maker", ver(b))
			if w.Code != tc.status || problemOf(t, w).Code != tc.code {
				t.Fatalf("got %d %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.code)
			}
		})
	}
	t.Run("vault outage is a 503 with Retry-After", func(t *testing.T) {
		b := create(func(map[string]any) {})
		h.svc.Vault = unavailableVault{}
		defer func() { h.svc.Vault = h.vault }()
		w := h.call("POST", "/v1/bridges/"+b.BridgeID+"/submit", "t1", "maker", ver(b))
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
			t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body.String())
		}
	})
}

type unavailableVault struct{}

func (unavailableVault) Verify(_ context.Context, _ string, _ vault.Caller) error {
	return vault.ErrUnavailable
}

// ---- authorization --------------------------------------------------------------

func TestEveryCommandChecksItsOwnAuthorizationAction(t *testing.T) {
	_, h := setupTestRouter(t)
	b := h.activeBridge(t, "t1")
	h.authz.mu.Lock()
	h.authz.actions = nil
	h.authz.mu.Unlock()

	cmds := []struct {
		method, path, action string
		body                 map[string]any
	}{
		{"PATCH", "", ActionUpdate, map[string]any{"bridge_name": "x"}},
		{"POST", "/suspend", ActionSuspend, map[string]any{"reason": "r"}},
		{"POST", "/consent", ActionConsent, map[string]any{"consent_expires_at": h.now.Add(24 * time.Hour)}},
		{"POST", "/upgrade-profile", ActionProfileUpgrade, map[string]any{"provider_profile_version": 3}},
		{"POST", "/health", ActionHealthReport, map[string]any{"success": true}},
		{"POST", "/checkpoints/s1/advance", ActionCheckpoint, map[string]any{"cursor": "c"}},
		{"POST", "/checkpoints/s1/reset-requests", ActionResetRequest, map[string]any{"to_cursor": "c", "reason": "r"}},
		{"POST", "/ingest", BRIDGE_INGEST, map[string]any{"payload_summary": "x"}},
	}
	for _, c := range cmds {
		h.authz.deny("denied-user", c.action)
		body := map[string]any{"expected_version": b.Version}
		for k, v := range c.body {
			body[k] = v
		}
		w := h.call(c.method, "/v1/bridges/"+b.BridgeID+c.path, "t1", "denied-user", body)
		if w.Code != http.StatusForbidden || problemOf(t, w).Code != "INT-AUTH-003" {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	if got := decodeBridge(t, h.call("GET", "/v1/bridges/"+b.BridgeID, "t1", "maker", nil)); got.Version != b.Version || got.BridgeName != b.BridgeName {
		t.Fatalf("a denied command must not change anything: %+v", got)
	}
	h.authz.mu.Lock()
	seen := strings.Join(h.authz.actions, ",")
	h.authz.mu.Unlock()
	for _, c := range cmds {
		if !strings.Contains(seen, c.action) {
			t.Errorf("action %s was never asked of authorization-svc", c.action)
		}
	}
}

func TestApproveAndRejectShareTheApprovalAction(t *testing.T) {
	_, h := setupTestRouter(t)
	b := decodeBridge(t, h.call("POST", "/v1/bridges", "t1", "maker", fullBody()))
	b = decodeBridge(t, h.call("POST", "/v1/bridges/"+b.BridgeID+"/submit", "t1", "maker", ver(b)))
	h.authz.deny("clerk", ActionApprove)
	for _, p := range []string{"approve", "reject"} {
		body := ver(b)
		body["reason"] = "r"
		if w := h.call("POST", "/v1/bridges/"+b.BridgeID+"/"+p, "t1", "clerk", body); w.Code != http.StatusForbidden {
			t.Errorf("%s by a principal without %s: %d", p, ActionApprove, w.Code)
		}
	}
}

func TestAuthorizationOutageFailsClosedAs503(t *testing.T) {
	_, h := setupTestRouter(t)
	h.authz.mu.Lock()
	h.authz.down = true
	h.authz.mu.Unlock()
	w := h.call("POST", "/v1/bridges", "t1", "maker", fullBody())
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if list := decodeList(t, h.call("GET", "/v1/bridges", "t1", "maker", nil)); len(list) != 0 {
		t.Fatal("nothing may be created while authorization cannot be confirmed")
	}
}

func decodeList(t *testing.T, w *httptest.ResponseRecorder) []domain.ApiBridge {
	t.Helper()
	var out struct {
		Bridges []domain.ApiBridge `json:"bridges"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Bridges
}

// ---- secrets ---------------------------------------------------------------------

func TestSecretsAreRefusedAndNeverEchoed_INT_NP_019(t *testing.T) {
	_, h := setupTestRouter(t)
	for name, mutate := range map[string]func(map[string]any){
		"userinfo":   func(b map[string]any) { b["endpoint_url"] = "https://u:hunter2@api.bank-x.example/x" },
		"query key":  func(b map[string]any) { b["endpoint_url"] = "https://api.bank-x.example/x?api_key=sk_live_ABC" },
		"raw secret": func(b map[string]any) { b["credential_ref"] = "sk_live_TOPSECRET" },
	} {
		body := fullBody()
		mutate(body)
		w := h.call("POST", "/v1/bridges", "t1", "maker", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, w.Code)
		}
		for _, leak := range []string{"hunter2", "sk_live", "TOPSECRET"} {
			if strings.Contains(w.Body.String(), leak) {
				t.Errorf("%s: response echoes %q: %s", name, leak, w.Body.String())
			}
		}
	}
	if n := len(decodeList(t, h.call("GET", "/v1/bridges", "t1", "maker", nil))); n != 0 {
		t.Fatalf("refused creates must store nothing: %d", n)
	}
	// and an update cannot smuggle one in either
	b := h.activeBridge(t, "t1")
	w := h.call("PATCH", "/v1/bridges/"+b.BridgeID, "t1", "editor", map[string]any{"expected_version": b.Version, "endpoint_url": "https://u:p@api.bank-x.example/x"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("%d", w.Code)
	}
	if got := decodeBridge(t, h.call("GET", "/v1/bridges/"+b.BridgeID, "t1", "maker", nil)); !strings.HasPrefix(got.EndpointURL, "https://api.bank-x.example") || got.Status != domain.StatusActive {
		t.Fatalf("%+v", got)
	}
}

// ---- idempotent create -------------------------------------------------------------

func TestCreateIsIdempotentOnTheHeader(t *testing.T) {
	_, h := setupTestRouter(t)
	w1 := h.call("POST", "/v1/bridges", "t1", "maker", fullBody(), "Idempotency-Key", "k-1")
	w2 := h.call("POST", "/v1/bridges", "t1", "maker", fullBody(), "Idempotency-Key", "k-1")
	if w1.Code != http.StatusCreated || w2.Code != http.StatusOK {
		t.Fatalf("%d then %d", w1.Code, w2.Code)
	}
	if decodeBridge(t, w1).BridgeID != decodeBridge(t, w2).BridgeID {
		t.Fatal("a replay must return the original connector")
	}
	diff := fullBody()
	diff["bridge_name"] = "A different connector"
	w3 := h.call("POST", "/v1/bridges", "t1", "maker", diff, "Idempotency-Key", "k-1")
	if w3.Code != http.StatusConflict || problemOf(t, w3).Code != "INT-IDEM-005" {
		t.Fatalf("%d %s", w3.Code, w3.Body.String())
	}
	if n := len(decodeList(t, h.call("GET", "/v1/bridges", "t1", "maker", nil))); n != 1 {
		t.Fatalf("%d connectors", n)
	}
}

// ---- ingest ------------------------------------------------------------------------

func TestIngestRules(t *testing.T) {
	_, h := setupTestRouter(t)
	b := h.activeBridge(t, "t1")
	if w := h.call("POST", "/v1/bridges/"+b.BridgeID+"/ingest", "t1", "u", map[string]any{"payload_summary": "x", "scope": "statements.read"}); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	w := h.call("POST", "/v1/bridges/"+b.BridgeID+"/ingest", "t1", "u", map[string]any{"payload_summary": "x", "scope": "payments.write"})
	if w.Code != http.StatusForbidden || problemOf(t, w).Code != "INT-SCOPE-013" {
		t.Fatalf("an unapproved scope: %d %s", w.Code, w.Body.String())
	}
	b2 := decodeBridge(t, h.call("POST", "/v1/bridges/"+b.BridgeID+"/suspend", "t1", "maker", map[string]any{"expected_version": b.Version, "reason": "maintenance"}))
	w = h.call("POST", "/v1/bridges/"+b.BridgeID+"/ingest", "t1", "u", map[string]any{"payload_summary": "x"})
	if w.Code != http.StatusConflict || problemOf(t, w).Code != "INT-SCOPE-013" {
		t.Fatalf("a suspended connector: %d %s", w.Code, w.Body.String())
	}
	_ = b2
	logs := h.call("GET", "/v1/bridges/"+b.BridgeID+"/logs", "t1", "u", nil)
	if !strings.Contains(logs.Body.String(), `"total":1`) {
		t.Fatalf("only the one successful ingestion is logged: %s", logs.Body.String())
	}
}

// ---- tenant isolation over every route ------------------------------------------------

func TestOtherTenantGets404OnEveryConnectorRoute(t *testing.T) {
	_, h := setupTestRouter(t)
	b := h.activeBridge(t, "tenant-a")
	routes := []struct{ method, path string }{
		{"GET", ""}, {"PATCH", ""}, {"POST", "/submit"}, {"POST", "/approve"}, {"POST", "/reject"}, {"POST", "/activate"},
		{"POST", "/suspend"}, {"POST", "/revoke"}, {"POST", "/archive"}, {"POST", "/consent"}, {"POST", "/upgrade-profile"},
		{"POST", "/ingest"}, {"GET", "/logs"}, {"GET", "/changes"}, {"GET", "/approvals"}, {"GET", "/profile-status"},
		{"GET", "/health"}, {"POST", "/health"}, {"GET", "/health/transitions"}, {"GET", "/checkpoints"},
		{"POST", "/checkpoints/s1/advance"}, {"POST", "/checkpoints/s1/reset-requests"},
	}
	for _, rt := range routes {
		body := map[string]any{"expected_version": b.Version, "reason": "x", "cursor": "c", "to_cursor": "c", "success": true,
			"provider_profile_version": 3, "payload_summary": "x"}
		w := h.call(rt.method, "/v1/bridges/"+b.BridgeID+rt.path, "tenant-b", "maker", body)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s as another tenant: %d, want 404", rt.method, rt.path, w.Code)
		}
	}
	if got := decodeBridge(t, h.call("GET", "/v1/bridges/"+b.BridgeID, "tenant-a", "maker", nil)); !got.Live() || got.Version != b.Version {
		t.Fatalf("the owner's connector was disturbed: %+v", got)
	}
}

// ---- health ---------------------------------------------------------------------------

func TestHealthEndpoints(t *testing.T) {
	_, h := setupTestRouter(t)
	b := h.activeBridge(t, "t1")
	for i := 0; i < domain.FailureThreshold; i++ {
		w := h.call("POST", "/v1/bridges/"+b.BridgeID+"/health", "t1", "ops", map[string]any{"success": false, "reason_code": "PROVIDER_OUTAGE", "detail": "503s"})
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	var hl domain.Health
	_ = json.Unmarshal(h.call("GET", "/v1/bridges/"+b.BridgeID+"/health", "t1", "ops", nil).Body.Bytes(), &hl)
	if hl.State != domain.HealthDegraded || hl.ReasonCode != "PROVIDER_OUTAGE" || hl.ConsecutiveFailures != domain.FailureThreshold {
		t.Fatalf("%+v", hl)
	}
	tr := h.call("GET", "/v1/bridges/"+b.BridgeID+"/health/transitions", "t1", "ops", nil)
	if !strings.Contains(tr.Body.String(), `"to_state":"DEGRADED"`) {
		t.Fatalf("%s", tr.Body.String())
	}
	if w := h.call("POST", "/v1/bridges/"+b.BridgeID+"/health", "t1", "ops", map[string]any{"success": false, "reason_code": "BOGUS"}); w.Code != 400 {
		t.Fatalf("an unknown reason: %d", w.Code)
	}
}

// ---- checkpoints -----------------------------------------------------------------------

func TestCheckpointFlowOverHTTP(t *testing.T) {
	_, h := setupTestRouter(t)
	b := h.activeBridge(t, "t1")
	base := "/v1/bridges/" + b.BridgeID + "/checkpoints/statements"
	w1, w2 := h.now.Add(-2*time.Hour), h.now.Add(-time.Hour)

	if w := h.call("POST", base+"/advance", "t1", "sync", map[string]any{"expected_version": 0, "cursor": "c1", "watermark": w1}); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := h.call("POST", base+"/advance", "t1", "sync", map[string]any{"expected_version": 0, "cursor": "c1", "watermark": w1}); w.Code != http.StatusConflict {
		t.Fatalf("creating twice: %d", w.Code)
	}
	if w := h.call("POST", base+"/advance", "t1", "sync", map[string]any{"expected_version": 1, "cursor": "c2", "watermark": w2}); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	old := h.now.Add(-5 * time.Hour)
	w := h.call("POST", base+"/advance", "t1", "sync", map[string]any{"expected_version": 2, "cursor": "old", "watermark": old})
	if w.Code != http.StatusConflict || problemOf(t, w).Code != "INT-CB-015" {
		t.Fatalf("rollback: %d %s", w.Code, w.Body.String())
	}

	// reset needs a second person
	w = h.call("POST", base+"/reset-requests", "t1", "sync", map[string]any{"to_cursor": "c1", "to_watermark": w1, "reason": "provider re-sent"})
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var req domain.ResetRequest
	_ = json.Unmarshal(w.Body.Bytes(), &req)
	if w := h.call("POST", "/v1/checkpoint-reset-requests/"+req.RequestID+"/approve", "t1", "sync", nil); w.Code != http.StatusForbidden {
		t.Fatalf("self-approved reset: %d", w.Code)
	}
	if w := h.call("POST", "/v1/checkpoint-reset-requests/"+req.RequestID+"/approve", "t2", "lead", nil); w.Code != http.StatusNotFound {
		t.Fatalf("another tenant: %d", w.Code)
	}
	if w := h.call("POST", "/v1/checkpoint-reset-requests/"+req.RequestID+"/approve", "t1", "lead", nil); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	list := h.call("GET", "/v1/bridges/"+b.BridgeID+"/checkpoints", "t1", "sync", nil)
	if !strings.Contains(list.Body.String(), `"cursor":"c1"`) || !strings.Contains(list.Body.String(), `"checkpoint_version":3`) {
		t.Fatalf("%s", list.Body.String())
	}
}

// ---- provider profiles ------------------------------------------------------------------

func TestProfileRoutes(t *testing.T) {
	_, h := setupTestRouter(t)
	body := map[string]any{"provider_key": "bank-y", "provider_type": "BANK", "version": 1, "allowed_regions": []string{"uk"},
		"allowed_host_suffixes": []string{"bank-y.example"}, "supported_scopes": []string{"ingest"}}

	if w := h.call("POST", "/v1/external-profiles", "tenant-a", "admin", body); w.Code != http.StatusForbidden {
		t.Fatalf("a tenant publishing a platform profile: %d %s", w.Code, w.Body.String())
	}
	if w := h.call("POST", "/v1/external-profiles", store.PlatformScopeID, "admin", body); w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := h.call("POST", "/v1/external-profiles", store.PlatformScopeID, "admin", body); w.Code != http.StatusConflict {
		t.Fatalf("a version is never replaced: %d", w.Code)
	}
	// every tenant can read profiles
	if w := h.call("GET", "/v1/external-profiles?provider_key=bank-y", "tenant-a", "u", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "bank-y") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := h.call("GET", "/v1/external-profiles/bank-y/versions/1", "tenant-a", "u", nil); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	for _, bad := range []string{"0", "x", "-1"} {
		if w := h.call("GET", "/v1/external-profiles/bank-y/versions/"+bad, "tenant-a", "u", nil); w.Code != 400 {
			t.Errorf("version %q: %d", bad, w.Code)
		}
	}
	if w := h.call("GET", "/v1/external-profiles/bank-y/versions/9", "tenant-a", "u", nil); w.Code != 404 {
		t.Fatalf("%d", w.Code)
	}
	// status: platform only, forward only
	st := func(tenant, status string) *httptest.ResponseRecorder {
		return h.call("POST", "/v1/external-profiles/bank-y/versions/1/status", tenant, "admin", map[string]any{"status": status})
	}
	if w := st("tenant-a", "CERTIFIED"); w.Code != http.StatusForbidden {
		t.Fatalf("%d", w.Code)
	}
	if w := st(store.PlatformScopeID, "CERTIFIED"); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := st(store.PlatformScopeID, "DEPRECATED"); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	if w := st(store.PlatformScopeID, "CERTIFIED"); w.Code != http.StatusConflict {
		t.Fatalf("a profile never moves backwards: %d", w.Code)
	}
	if w := st(store.PlatformScopeID, "DRAFT"); w.Code != 400 {
		t.Fatalf("%d", w.Code)
	}
}

func TestProfileStatusForAConnector(t *testing.T) {
	_, h := setupTestRouter(t)
	b := h.activeBridge(t, "t1")
	w := h.call("GET", "/v1/bridges/"+b.BridgeID+"/profile-status", "t1", "u", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"CERTIFIED"`) || !strings.Contains(w.Body.String(), `"upgrade_required":false`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// ---- compatibility ----------------------------------------------------------------------

func TestPostmanAliasRoutesStillWork(t *testing.T) {
	_, h := setupTestRouter(t)
	w := h.call("POST", "/v1/api-bridge/connections", "t1", "maker", fullBody())
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	b := decodeBridge(t, w)
	if g := h.call("GET", "/v1/api-bridge/connections/"+b.BridgeID, "t1", "maker", nil); g.Code != 200 {
		t.Fatalf("%d", g.Code)
	}
	if l := h.call("GET", "/v1/api-bridge/connections", "t1", "maker", nil); !strings.Contains(l.Body.String(), `"connections"`) {
		t.Fatal("the alias list keeps both envelope keys")
	}
}

func TestBodyLimitsAndMalformedJSON(t *testing.T) {
	_, h := setupTestRouter(t)
	if w := h.call("POST", "/v1/bridges", "t1", "maker", []byte(`{not json`)); w.Code != http.StatusBadRequest {
		t.Fatalf("%d", w.Code)
	}
	huge := []byte(`{"bridge_name":"` + strings.Repeat("a", 2<<20) + `"}`)
	if w := h.call("POST", "/v1/bridges", "t1", "maker", huge); w.Code != http.StatusBadRequest {
		t.Fatalf("an oversized body: %d", w.Code)
	}
}

// ---- X-Expected-Version -------------------------------------------------------------

func TestExpectedVersionMayComeFromTheHeader(t *testing.T) {
	_, h := setupTestRouter(t)
	b := decodeBridge(t, h.call("POST", "/v1/bridges", "t1", "maker", fullBody()))
	v := strconv.FormatInt(b.Version, 10)

	// header only
	w := h.call("POST", "/v1/bridges/"+b.BridgeID+"/submit", "t1", "maker", map[string]any{}, "X-Expected-Version", v)
	if w.Code != http.StatusOK {
		t.Fatalf("header only: %d %s", w.Code, w.Body.String())
	}
	b = decodeBridge(t, w)

	// header and body agree
	w = h.call("PATCH", "/v1/bridges/"+b.BridgeID, "t1", "editor", map[string]any{"expected_version": b.Version, "bridge_name": "n1"},
		"X-Expected-Version", strconv.FormatInt(b.Version, 10))
	if w.Code != http.StatusOK {
		t.Fatalf("agreeing: %d %s", w.Code, w.Body.String())
	}
	b = decodeBridge(t, w)

	// header and body disagree: refuse rather than guess which was meant
	w = h.call("PATCH", "/v1/bridges/"+b.BridgeID, "t1", "editor", map[string]any{"expected_version": b.Version, "bridge_name": "n2"},
		"X-Expected-Version", strconv.FormatInt(b.Version+1, 10))
	if w.Code != http.StatusBadRequest || problemOf(t, w).Code != "INT-SCHEMA-004" {
		t.Fatalf("disagreeing: %d %s", w.Code, w.Body.String())
	}
	// a malformed header is refused, not ignored
	for _, bad := range []string{"abc", "0", "-3", "1.5"} {
		w = h.call("POST", "/v1/bridges/"+b.BridgeID+"/suspend", "t1", "maker", map[string]any{"reason": "r"}, "X-Expected-Version", bad)
		if w.Code != http.StatusBadRequest {
			t.Errorf("X-Expected-Version %q: %d", bad, w.Code)
		}
	}
	// a stale header value is a conflict like a stale body value
	w = h.call("POST", "/v1/bridges/"+b.BridgeID+"/activate", "t1", "checker", map[string]any{}, "X-Expected-Version", "1")
	if w.Code != http.StatusConflict {
		t.Fatalf("stale header: %d", w.Code)
	}
}

func TestEmptyBodyIsFineWhenTheHeaderCarriesTheVersion(t *testing.T) {
	_, h := setupTestRouter(t)
	b := decodeBridge(t, h.call("POST", "/v1/bridges", "t1", "maker", fullBody()))
	w := h.call("POST", "/v1/bridges/"+b.BridgeID+"/submit", "t1", "maker", nil, "X-Expected-Version", strconv.FormatInt(b.Version, 10))
	if w.Code != http.StatusOK {
		t.Fatalf("an empty body with the version in the header: %d %s", w.Code, w.Body.String())
	}
	// an empty body with nothing carrying the version is a 400 that says so
	w = h.call("POST", "/v1/bridges/"+b.BridgeID+"/approve", "t1", "checker", nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "expected_version is required") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// create with no body still fails on the missing fields, not on parsing
	w = h.call("POST", "/v1/bridges", "t1", "maker", nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "legal_entity_id") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
