package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	authzpkg "zoiko.io/privacy-rights-svc/internal/authz"
	"zoiko.io/privacy-rights-svc/internal/domain"
	"zoiko.io/privacy-rights-svc/internal/handler"
)

// recAuthz records every authorization question and can deny a single action.
type recAuthz struct {
	denyAction string
	calls      []authzCall
}

type authzCall struct{ Principal, Scope, Action string }

func (a *recAuthz) CheckAllowed(_ context.Context, principal, scope, action string) error {
	a.calls = append(a.calls, authzCall{principal, scope, action})
	if a.denyAction != "" && a.denyAction == action {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

// ── Gap 2: read routes are authenticated, tenant-scoped and authorized ──────

func TestReads_RequirePrincipal(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{})
	c := createRequest(t, r)
	for _, path := range []string{
		"/privacy/rights-requests/" + c.RequestID,
		"/privacy/rights-requests?subject_ref=subject-1",
		"/privacy/rights-requests/" + c.RequestID + "/discovery-manifests",
		"/v1/privacy/rights-requests/" + c.RequestID,
	} {
		w := doRawRequest(r, http.MethodGet, path, nil, map[string]string{"X-Tenant-Id": testTenant, "X-Principal-Id": " "})
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"AUTHENTICATION_FAILED"`) {
			t.Fatalf("%s: expected 401 AUTHENTICATION_FAILED, got %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestReads_RequireTenant(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{})
	w := doRawRequest(r, http.MethodGet, "/privacy/rights-requests?subject_ref=x", nil, nil)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"TENANT_SCOPE_REQUIRED"`) {
		t.Fatalf("expected 401 TENANT_SCOPE_REQUIRED, got %d %s", w.Code, w.Body.String())
	}
}

func TestReads_AuthorizedBeforeLookup_NoExistenceLeak(t *testing.T) {
	st := newStubStore()
	az := &recAuthz{}
	r := newTestRouter(st, az)
	c := createRequest(t, r)

	az.denyAction = handler.PrivacyRightsRequestProcess
	for _, id := range []string{c.RequestID, uuid.NewString()} { // existing and non-existing id answer identically
		for _, suffix := range []string{"", "/discovery-manifests"} {
			w := doRequest(r, http.MethodGet, "/privacy/rights-requests/"+id+suffix, nil, testTenant)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"AUTHORIZATION_DENIED"`) {
				t.Fatalf("unauthorized read of %s%s must be 403 whether or not it exists, got %d %s", id, suffix, w.Code, w.Body.String())
			}
		}
	}
	w := doRequest(r, http.MethodGet, "/privacy/rights-requests?subject_ref=subject-1", nil, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthorized list must be 403, got %d", w.Code)
	}

	az.denyAction = ""
	w = doRequest(r, http.MethodGet, "/privacy/rights-requests/"+uuid.NewString(), nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("authorized read of a missing case must be 404, got %d", w.Code)
	}
	w = doRequest(r, http.MethodGet, "/privacy/rights-requests/"+c.RequestID, nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("authorized read must be 200, got %d", w.Code)
	}
	last := az.calls[len(az.calls)-1]
	if last.Action != handler.PrivacyRightsRequestProcess || last.Scope != testTenant {
		t.Fatalf("read must be authorized for the existing action on the verified tenant, got %+v", last)
	}
}

func TestReads_CrossTenantIsNotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{})
	c := createRequest(t, r)
	for _, suffix := range []string{"", "/discovery-manifests"} {
		w := doRequest(r, http.MethodGet, "/privacy/rights-requests/"+c.RequestID+suffix, nil, "another-tenant")
		if suffix == "" && w.Code != http.StatusNotFound {
			t.Fatalf("cross-tenant read must be 404, got %d %s", w.Code, w.Body.String())
		}
		if suffix != "" && strings.Contains(w.Body.String(), "manifest_id") {
			t.Fatal("cross-tenant manifest read leaked data")
		}
	}
	w := doRequest(r, http.MethodGet, "/privacy/rights-requests?subject_ref=subject-1", nil, "another-tenant")
	var list struct{ Count int }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if list.Count != 0 {
		t.Fatalf("cross-tenant list must be empty, got %d", list.Count)
	}
}

func TestMutations_AuthorizedBeforeLookup_NoExistenceLeak(t *testing.T) {
	az := &recAuthz{}
	r := newTestRouter(newStubStore(), az)
	c := createRequest(t, r)
	az.denyAction = handler.PrivacyRightsRequestProcess
	az.calls = nil
	for _, tc := range mutationCases(c.RequestID) {
		if tc.name == "close" || tc.name == "create" {
			continue
		}
		missing := strings.Replace(tc.path, c.RequestID, uuid.NewString(), 1)
		for _, p := range []string{tc.path, missing} {
			w := doRequest(r, http.MethodPost, p, tc.body, testTenant)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s %s: unauthorized mutation must be 403 for existing and missing ids alike, got %d", tc.name, p, w.Code)
			}
		}
	}
}

// ── Gap 6: validation never precedes authentication/authorization ───────────

func TestValidationFollowsAuthentication(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{})
	c := createRequest(t, r)
	for _, tc := range mutationCases(c.RequestID) {
		w := doRawRequest(r, http.MethodPost, tc.path, "{not json", map[string]string{
			"X-Tenant-Id": testTenant, "X-Principal-Id": " ", "X-Correlation-ID": "c", "Idempotency-Key": "k-" + uuid.NewString(),
		})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: unauthenticated malformed body must be 401, got %d", tc.name, w.Code)
		}
	}
	az := &stubAuthz{deny: true}
	r2 := newTestRouter(newStubStore(), az)
	w := doRequest(r2, http.MethodPost, "/privacy/rights-requests", "{not json", testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthorized malformed body must be 403, got %d", w.Code)
	}
}

func TestErrorBodiesCarryErrorCode(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{})
	c := createRequest(t, r)
	cases := []struct {
		name   string
		method string
		path   string
		body   interface{}
		hdr    map[string]string
		status int
		code   string
	}{
		{"missing principal", http.MethodPost, "/privacy/rights-requests", map[string]string{}, map[string]string{"X-Principal-Id": " ", "X-Tenant-Id": testTenant, "X-Correlation-ID": "c", "Idempotency-Key": "k1"}, 401, "AUTHENTICATION_FAILED"},
		{"missing correlation", http.MethodPost, "/privacy/rights-requests", map[string]string{}, map[string]string{"X-Tenant-Id": testTenant, "Idempotency-Key": "k2"}, 400, "CORRELATION_ID_REQUIRED"},
		{"missing key", http.MethodPost, "/privacy/rights-requests", map[string]string{}, map[string]string{"X-Tenant-Id": testTenant, "X-Correlation-ID": "c"}, 400, "IDEMPOTENCY_KEY_REQUIRED"},
		{"tenant mismatch", http.MethodPost, "/privacy/rights-requests", map[string]string{"tenant_id": "other", "subject_ref": "s", "right_family": "ACCESS"}, map[string]string{"X-Tenant-Id": testTenant, "X-Correlation-ID": "c", "Idempotency-Key": "k3"}, 403, "TENANT_SCOPE_MISMATCH"},
		{"invalid body", http.MethodPost, "/privacy/rights-requests", "x", map[string]string{"X-Tenant-Id": testTenant, "X-Correlation-ID": "c", "Idempotency-Key": "k4"}, 400, "VALIDATION_FAILED"},
		{"not found", http.MethodGet, "/privacy/rights-requests/" + uuid.NewString(), nil, map[string]string{"X-Tenant-Id": testTenant}, 404, "NOT_FOUND"},
		{"read missing principal", http.MethodGet, "/privacy/rights-requests/" + c.RequestID, nil, map[string]string{"X-Tenant-Id": testTenant, "X-Principal-Id": " "}, 401, "AUTHENTICATION_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRawRequest(r, tc.method, tc.path, tc.body, tc.hdr)
			if w.Code != tc.status {
				t.Fatalf("status want %d got %d (%s)", tc.status, w.Code, w.Body.String())
			}
			var b map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &b)
			if b["error_code"] != tc.code || b["error"] == "" {
				t.Fatalf("want error_code %s with message, got %v", tc.code, b)
			}
		})
	}

	// §32 codes keep their meaning in both the message and error_code.
	for _, tc := range []struct {
		body   interface{}
		status int
		code   string
		msg    string
	}{
		{domain.CloseRequestRequest{Outcome: domain.OutcomeFulfilled}, 422, "PRV-012", domain.PRV012IdentityAssuranceInsufficient},
	} {
		w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+c.RequestID+"/close", tc.body, testTenant)
		var b map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		if w.Code != tc.status || b["error_code"] != tc.code || b["error"] != tc.msg {
			t.Fatalf("got %d %v", w.Code, b)
		}
	}
}

// ── Gap 3: idempotency ───────────────────────────────────────────────────────

func TestIdempotency_LookupFailureRefusesRequest(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{})
	st.failLookup = true
	w := doRequest(r, http.MethodPost, "/privacy/rights-requests",
		domain.CreateRightsRequestRequest{SubjectRef: "s", RightFamily: domain.RightAccess}, testTenant)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"STORE_UNAVAILABLE"`) {
		t.Fatalf("a failed idempotency lookup must refuse the request with 503, got %d %s", w.Code, w.Body.String())
	}
	if st.execs != 0 || len(st.requests) != 0 {
		t.Fatal("nothing may execute when the idempotency lookup fails")
	}
}

func TestIdempotency_CommitFailureLeavesNothing_RetryCreatesOne(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{})
	hdr := map[string]string{"Idempotency-Key": "k-atomic"}
	payload := domain.CreateRightsRequestRequest{SubjectRef: "s", RightFamily: domain.RightAccess}

	st.failCommit = true
	w := doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests", payload, testTenant, hdr)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
	if len(st.requests) != 0 || len(st.idempotency) != 0 {
		t.Fatal("a failed commit must persist no case and no idempotency record")
	}
	st.failCommit = false
	w = doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests", payload, testTenant, hdr)
	if w.Code != http.StatusCreated || w.Header().Get("Idempotency-Replay") != "" {
		t.Fatalf("retry must be a fresh success, got %d replay=%q", w.Code, w.Header().Get("Idempotency-Replay"))
	}
	w = doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests", payload, testTenant, hdr)
	if w.Header().Get("Idempotency-Replay") != "true" || len(st.requests) != 1 {
		t.Fatalf("replay must not re-execute: replay=%q cases=%d", w.Header().Get("Idempotency-Replay"), len(st.requests))
	}
}

func TestIdempotency_AliasHeaderIsHonoured(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{})
	payload := domain.CreateRightsRequestRequest{SubjectRef: "s-alias", RightFamily: domain.RightAccess}
	h := map[string]string{"X-Tenant-Id": testTenant, "X-Correlation-ID": "c", "X-Idempotency-Key": "alias-1"}
	w1 := doRawRequest(r, http.MethodPost, "/privacy/rights-requests", payload, h)
	w2 := doRawRequest(r, http.MethodPost, "/privacy/rights-requests", payload, h)
	if w1.Code != http.StatusCreated || w2.Header().Get("Idempotency-Replay") != "true" || len(st.requests) != 1 {
		t.Fatalf("X-Idempotency-Key must dedupe exactly like Idempotency-Key: %d / replay=%q / cases=%d", w1.Code, w2.Header().Get("Idempotency-Replay"), len(st.requests))
	}
}

func TestIdempotency_BoundToMethodPathAndBody(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{})
	c := createRequest(t, r)
	hdr := map[string]string{"Idempotency-Key": "shared-key"}
	body := domain.RecordIdentityVerificationRequest{Verified: true, Method: "GOVT_ID"}
	w1 := doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests/"+c.RequestID+"/identity-verification", body, testTenant, hdr)
	if w1.Code != http.StatusCreated {
		t.Fatalf("got %d", w1.Code)
	}
	// Same key, different route, body that happens to parse: must be a conflict, never a replay of the wrong answer.
	w2 := doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests/"+c.RequestID+"/wfc-process-ref",
		domain.AttachWFCProcessRefRequest{WFCProcessRef: "wf-1"}, testTenant, hdr)
	if w2.Code != http.StatusConflict || !strings.Contains(w2.Body.String(), `"PRV-020"`) {
		t.Fatalf("same key on a different operation must be 409 PRV-020, got %d %s", w2.Code, w2.Body.String())
	}
	// The /v1 alias of the SAME operation and body is the same logical request: a replay.
	w3 := doRequestWithHeaders(r, http.MethodPost, "/v1/privacy/rights-requests/"+c.RequestID+"/identity-verification", body, testTenant, hdr)
	if w3.Header().Get("Idempotency-Replay") != "true" {
		t.Fatalf("the /v1 alias of the same request must replay, got %d replay=%q", w3.Code, w3.Header().Get("Idempotency-Replay"))
	}
}

func TestIdempotency_ScopedByPrincipal(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{})
	payload := domain.CreateRightsRequestRequest{SubjectRef: "s", RightFamily: domain.RightAccess}
	a := doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests", payload, testTenant, map[string]string{"Idempotency-Key": "k", "X-Principal-Id": "principal-A"})
	b := doRequestWithHeaders(r, http.MethodPost, "/privacy/rights-requests", payload, testTenant, map[string]string{"Idempotency-Key": "k", "X-Principal-Id": "principal-B"})
	if a.Code != http.StatusCreated || b.Code != http.StatusCreated || b.Header().Get("Idempotency-Replay") == "true" || len(st.requests) != 2 {
		t.Fatalf("principal B must not replay principal A's response: %d %d replay=%q cases=%d", a.Code, b.Code, b.Header().Get("Idempotency-Replay"), len(st.requests))
	}
}

// ── Gap 4: disclosure gate ───────────────────────────────────────────────────

func TestManifest_HashMustBeSHA256(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{})
	c := createRequest(t, r)
	doRequest(r, http.MethodPost, "/privacy/rights-requests/"+c.RequestID+"/identity-verification",
		domain.RecordIdentityVerificationRequest{Verified: true, Method: "GOVT_ID"}, testTenant)
	path := "/privacy/rights-requests/" + c.RequestID + "/discovery-manifests"
	for _, bad := range []string{"abc", "sha256:abc", strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 65), "   "} {
		w := doRequest(r, http.MethodPost, path, domain.AttachDiscoveryManifestRequest{Domain: "crm", ContentHash: bad, CandidateCount: 1}, testTenant)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "VALIDATION_FAILED") {
			t.Fatalf("hash %q must be refused with 400, got %d", bad, w.Code)
		}
	}
	w := doRequest(r, http.MethodPost, path, domain.AttachDiscoveryManifestRequest{Domain: "crm", ContentHash: hashABC, CandidateCount: -1}, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("negative candidate_count must be refused, got %d", w.Code)
	}
	for _, ok := range []string{hashCRM, strings.ToUpper(hashCRM), hashABC} {
		w := doRequest(r, http.MethodPost, path, domain.AttachDiscoveryManifestRequest{Domain: "crm", ContentHash: ok, CandidateCount: 0}, testTenant)
		if w.Code != http.StatusCreated {
			t.Fatalf("valid SHA-256 digest %q refused: %d %s", ok, w.Code, w.Body.String())
		}
	}
}

func TestClose_Fulfilled_RequiresVerifiedIdentityEventNotJustFlag(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{})
	c := createRequest(t, r)
	// A case whose denormalised flag is set but which has no verified event
	// must not pass the gate.
	st.requests[c.RequestID].IdentityVerified = true
	st.manifests[c.RequestID] = []domain.DiscoveryManifest{{ManifestID: "m", ContentHash: hashABC}}
	w := doRequest(r, http.MethodPost, "/privacy/rights-requests/"+c.RequestID+"/close",
		domain.CloseRequestRequest{Outcome: domain.OutcomeFulfilled}, testTenant)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "PRV-012") {
		t.Fatalf("expected 422 PRV-012, got %d %s", w.Code, w.Body.String())
	}
}

// ── Gap 7: events ────────────────────────────────────────────────────────────
//
// §19 of ZS-SVC-W-001 declares no rights-intake or rights-closure event, so
// this service must emit none. Guard against an undeclared event (or a direct,
// non-transactional publisher) being reintroduced.
func TestNoEventsOrPublisherInService(t *testing.T) {
	var offenders []string
	for _, root := range []string{"../../internal", "../../cmd"} {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, _ := os.ReadFile(p)
			low := strings.ToLower(string(b))
			if strings.Contains(low, "kafka") || strings.Contains(low, "privacy.rights_request") || strings.Contains(low, "event_outbox") {
				offenders = append(offenders, filepath.ToSlash(p))
			}
			return nil
		})
	}
	for _, o := range offenders {
		if !strings.HasSuffix(o, "internal/config/config.go") { // Kafka settings are parsed but unused
			t.Fatalf("unexpected event machinery in %s", o)
		}
	}
}

func TestTenantMismatchBodyRefused(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{})
	w := doRequest(r, http.MethodPost, "/privacy/rights-requests",
		domain.CreateRightsRequestRequest{TenantID: "someone-else", SubjectRef: "s", RightFamily: domain.RightAccess}, testTenant)
	if w.Code != http.StatusForbidden || len(st.requests) != 0 {
		t.Fatalf("got %d, cases=%d", w.Code, len(st.requests))
	}
}
