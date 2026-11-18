package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/handler"
)

func registerRouter(s *stubStore, az *stubAuthz) http.Handler {
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, az, &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))
	return r
}

func withBody(extra string) string {
	return strings.TrimSuffix(strings.TrimSpace(validBody()), "}") + "," + extra + "}"
}

func post(r http.Handler, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := authed(httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func get(r http.Handler, path string) *httptest.ResponseRecorder {
	req := authed(httptest.NewRequest(http.MethodGet, path, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// The register fields are trimmed, upper-cased where they are codes, deduped,
// stored, and carried by the recorded event together with the chain link.
func TestCreateDecision_RegisterFieldsNormalisedStoredAndEvented(t *testing.T) {
	s := &stubStore{created: true}
	w := post(registerRouter(s, testAuthz()), "/v1/decisions",
		withBody(`"title":"  Approve vendor contract renewal ","domain":"legal_contracts","priority":"high","tags":["Vendor","vendor"," Renewal "],"related_obligation_id":"OB-2026-1123"`), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	d := s.got
	if ptr(d.Title) != "Approve vendor contract renewal" || d.Domain != "LEGAL_CONTRACTS" || ptr(d.Priority) != "HIGH" {
		t.Fatalf("register fields not normalised: title=%q domain=%q priority=%q", ptr(d.Title), d.Domain, ptr(d.Priority))
	}
	if strings.Join(d.Tags, ",") != "Vendor,Renewal" {
		t.Fatalf("tags must be trimmed and deduped case-insensitively, got %v", d.Tags)
	}
	var env struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(s.gotRec.Event, &env); err != nil {
		t.Fatalf("event: %v", err)
	}
	if env.Payload["domain"] != "LEGAL_CONTRACTS" || env.Payload["related_obligation_id"] != "OB-2026-1123" || env.Payload["chain_seq"] == nil {
		t.Fatalf("event payload lacks register fields or the chain link: %v", env.Payload)
	}
}

func ptr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestCreateDecision_400_InvalidRegisterFields(t *testing.T) {
	for name, extra := range map[string]string{
		"priority":    `"priority":"URGENT"`,
		"too many":    `"tags":["a","b","c","d","e","f","g","h","i","j","k"]`,
		"long tag":    `"tags":["` + strings.Repeat("x", 33) + `"]`,
		"long title":  `"title":"` + strings.Repeat("t", 201) + `"`,
		"long domain": `"domain":"` + strings.Repeat("D", 33) + `"`,
	} {
		t.Run(name, func(t *testing.T) {
			s := &stubStore{created: true}
			w := post(registerRouter(s, testAuthz()), "/v1/decisions", withBody(extra), nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			if s.inserts != 0 {
				t.Fatal("a refused request must write nothing")
			}
		})
	}
}

func TestCreateDecision_400_UnknownDomainFromStore(t *testing.T) {
	s := &stubStore{err: domain.ErrUnknownDomain}
	w := post(registerRouter(s, testAuthz()), "/v1/decisions", withBody(`"domain":"NOPE"`), nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "UNKNOWN_DOMAIN") {
		t.Fatalf("expected 400 UNKNOWN_DOMAIN, got %d: %s", w.Code, w.Body.String())
	}
}

// A follow-up names its cause in the body or as X-Causation-Id (policy-svc's
// relay); the two may not disagree.
func TestCreateDecision_CausationFromHeaderAndMismatch(t *testing.T) {
	s := &stubStore{created: true}
	w := post(registerRouter(s, testAuthz()), "/v1/decisions", validBody(), map[string]string{"X-Causation-Id": "dec-parent"})
	if w.Code != http.StatusCreated || ptr(s.got.CausationID) != "dec-parent" {
		t.Fatalf("expected the header causation to be recorded, got %d causation=%q", w.Code, ptr(s.got.CausationID))
	}
	s = &stubStore{created: true}
	w = post(registerRouter(s, testAuthz()), "/v1/decisions", withBody(`"causation_id":"dec-a"`), map[string]string{"X-Causation-Id": "dec-b"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "CAUSATION_MISMATCH") {
		t.Fatalf("expected 400 CAUSATION_MISMATCH, got %d: %s", w.Code, w.Body.String())
	}
}

// A repeated decision_id with a changed register field is a different
// decision, not a replay.
func TestCreateDecision_409_SameIDChangedRegisterField(t *testing.T) {
	title := "Original"
	s := &stubStore{created: false, existing: &domain.GovernanceDecision{
		DecisionID: "dec-001", TenantID: testTenant, LegalEntityID: "55555555-5555-5555-5555-555555555555",
		ActorID: testPrincipal, ActionType: "PAYROLL_RELEASE", Outcome: "DENIED", RuleBasis: "policy-v3-sod",
		CorrelationID: "corr-001", Title: &title, Domain: "PEOPLE_PAYROLL",
	}}
	w := post(registerRouter(s, testAuthz()), "/v1/decisions", withBody(`"title":"Changed"`), nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListDecisions_RegisterFiltersMapToParams(t *testing.T) {
	s := &stubStore{}
	w := get(registerRouter(s, testAuthz()),
		"/v1/decisions?domain=finance_tax&status=pending&priority=high&jurisdiction=GB&causation=dec-1&workflow=wf-1&q=GD-2026-0042&offset=20&limit=10")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	p := s.listGotParams
	if p.Domain != "FINANCE_TAX" || p.Category != "PENDING" || p.Priority != "HIGH" || p.Jurisdiction != "GB" ||
		p.CausationID != "dec-1" || p.WorkflowInstanceID != "wf-1" || p.Query != "GD-2026-0042" || p.Offset != 20 || p.Limit != 10 {
		t.Fatalf("filters not passed through: %+v", p)
	}
	var body struct {
		TotalCount *int64 `json:"total_count"`
		Limit      int    `json:"limit"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.TotalCount == nil {
		t.Fatalf("response must carry total_count: %s", w.Body.String())
	}
}

// The "audit" query dimension of Doc 03 §8.7 is the correlation id
// (decision D2-2.F11): ?correlation= reaches the store; over 64 chars is 400.
func TestListDecisions_AuditDimensionIsTheCorrelationID(t *testing.T) {
	s := &stubStore{}
	w := get(registerRouter(s, testAuthz()), "/v1/decisions?correlation=corr-audit-7&entity=entity-1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.listGotParams.CorrelationID != "corr-audit-7" || s.listGotParams.LegalEntityID != "entity-1" {
		t.Fatalf("correlation filter not passed through: %+v", s.listGotParams)
	}
	if w := get(registerRouter(&stubStore{}, testAuthz()), "/v1/decisions?correlation="+strings.Repeat("c", 65)); w.Code != http.StatusBadRequest {
		t.Fatalf("over-long correlation: expected 400, got %d", w.Code)
	}
}

func TestListDecisions_400_UnknownStatusOrPriorityOrCursor(t *testing.T) {
	for _, q := range []string{"status=MAYBE", "priority=URGENT", "cursor=not-a-cursor"} {
		w := get(registerRouter(&stubStore{}, testAuthz()), "/v1/decisions?"+q)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d", q, w.Code)
		}
	}
}

func TestDecisionSummary_Window(t *testing.T) {
	s := &stubStore{}
	r := registerRouter(s, testAuthz())

	if w := get(r, "/v1/decisions/summary"); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if span := s.summaryGot.To.Sub(s.summaryGot.From); span != 30*24*time.Hour {
		t.Fatalf("default window must be 30 days, got %v", span)
	}
	if w := get(r, "/v1/decisions/summary?days=7&entity=55555555-5555-5555-5555-555555555555"); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if span := s.summaryGot.To.Sub(s.summaryGot.From); span != 7*24*time.Hour || s.summaryGot.LegalEntityID == "" {
		t.Fatalf("days=7 with entity not applied: %+v", s.summaryGot)
	}
	for _, q := range []string{"days=0", "days=400", "from=2026-11-10T00:00:00Z&to=2026-11-01T00:00:00Z", "from=garbage"} {
		if w := get(r, "/v1/decisions/summary?"+q); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d", q, w.Code)
		}
	}
}

// Sealing needs GOVERNANCE_EVIDENCE_SEAL on the platform scope — not the
// principal-id string the first version checked as if it were an action.
func TestSealPackage_AuthorizedOnSealActionAtPlatformScope(t *testing.T) {
	s := &stubStore{}
	az := testAuthz()
	w := post(registerRouter(s, az), "/v1/evidence/packages/seal", `{"label":"November close"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if az.actionType != handler.ActionSealEvidence || az.scope != testAuthzScopeID {
		t.Fatalf("authz asked %q on %q, want %q on the platform scope", az.actionType, az.scope, handler.ActionSealEvidence)
	}
	if ptr(s.sealGot.Label) != "November close" || s.sealGot.PrincipalID != testPrincipal {
		t.Fatalf("seal request not passed through: %+v", s.sealGot)
	}
}

func TestSealPackage_RetryAndMismatch(t *testing.T) {
	s := &stubStore{}
	r := registerRouter(s, testAuthz())
	hdr := map[string]string{"Idempotency-Key": "seal-1"}
	if w := post(r, "/v1/evidence/packages/seal", `{"label":"a"}`, hdr); w.Code != http.StatusCreated {
		t.Fatalf("first seal: %d %s", w.Code, w.Body.String())
	}
	if w := post(r, "/v1/evidence/packages/seal", `{"label":"a"}`, hdr); w.Code != http.StatusOK {
		t.Fatalf("retry with the same key and body must be 200, got %d", w.Code)
	}
	if w := post(r, "/v1/evidence/packages/seal", `{"label":"b"}`, hdr); w.Code != http.StatusConflict {
		t.Fatalf("same key, different body must be 409, got %d", w.Code)
	}
}

func TestSealPackage_StoreRefusals(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
		code string
	}{
		{domain.ErrNothingToSeal, http.StatusConflict, "NOTHING_TO_SEAL"},
		{domain.ErrChainBroken, http.StatusConflict, "CHAIN_BROKEN"},
		{domain.ErrDecisionNotFound, http.StatusNotFound, "NOT_FOUND"},
		{domain.ErrStoreUnavailable, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"},
	} {
		w := post(registerRouter(&stubStore{sealErr: tc.err}, testAuthz()), "/v1/evidence/packages/seal", `{}`, nil)
		if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.code) {
			t.Fatalf("%v: expected %d %s, got %d: %s", tc.err, tc.want, tc.code, w.Code, w.Body.String())
		}
	}
}

func TestSealPackage_400_WithoutIdempotencyKey(t *testing.T) {
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/evidence/packages/seal", strings.NewReader(`{}`)))
	req.Header.Del("Idempotency-Key")
	w := httptest.NewRecorder()
	registerRouter(&stubStore{}, testAuthz()).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// A package is read tenant-wide: the first version authorized it on the
// tenant id as though that were a legal entity.
func TestPackages_ReadTenantWide(t *testing.T) {
	s := &stubStore{}
	r := registerRouter(s, testAuthz())
	if w := post(r, "/v1/evidence/packages/seal", `{}`, nil); w.Code != http.StatusCreated {
		t.Fatal(w.Body.String())
	}
	az := testAuthz()
	r = registerRouter(s, az)
	for _, path := range []string{"/v1/evidence/packages/" + s.sealed.PackageID, "/v1/evidence/packages/" + s.sealed.PackageID + "/verify"} {
		if w := get(r, path); w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", path, w.Code)
		}
		if az.scope != testAuthzScopeID || az.actionType != handler.ActionReadDecision {
			t.Fatalf("%s: authorized %q on %q, want READ on the platform scope", path, az.actionType, az.scope)
		}
	}
	if w := get(r, "/v1/evidence/packages/00000000-0000-0000-0000-000000000000"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown package: expected 404, got %d", w.Code)
	}
}

func TestExportChain_IsAnAttachment(t *testing.T) {
	w := get(registerRouter(&stubStore{}, testAuthz()), "/v1/evidence/chain/export")
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("expected an attachment, got %d %q", w.Code, w.Header().Get("Content-Disposition"))
	}
}

// policy-svc's relay records the whole evaluation, with the facts under
// action_context. Replay read amount only at the top level and answered 400
// for every decision policy-svc wrote (found by the dependency e2e, 11 Nov).
func TestReplayDecision_ReadsPolicySvcActionContext(t *testing.T) {
	s, pc := replayFixture()
	s.findByIDResult.EvaluationContext = []byte(`{"policy_type":"APPROVAL_THRESHOLD","action_context":{"amount":7500},"result":"APPROVAL_REQUIRED"}`)
	w := httptest.NewRecorder()
	newTestRouterWithPolicyClient(s, pc).ServeHTTP(w, replayRequest(`{}`, "replay-ctx-1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var m domain.ReplayManifest
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || m.ReplayedOutcome != "ESCALATED" || !m.OutcomesMatch {
		t.Fatalf("expected a reproduced ESCALATED, got %+v (%v)", m, err)
	}
}
