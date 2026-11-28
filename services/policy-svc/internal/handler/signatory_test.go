package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	"zoiko.io/identity-envelope/govdecision"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
	svcenvelope "zoiko.io/policy-svc/internal/envelope"
	"zoiko.io/policy-svc/internal/handler"
	svcmiddleware "zoiko.io/policy-svc/internal/middleware"
)

var testSigner = func() *govdecision.Signer {
	s, err := govdecision.NewRandomSigner()
	if err != nil {
		panic(err)
	}
	return s
}()

// Doc 01 §5.2 example matrix (test fixture only).
const sigMatrix = `{"currency":"GBP","valuation_basis":"GROSS","instrument_types":["SUPPLIER_CONTRACT","PAYMENT_INSTRUCTION","NDA"],` +
	`"bands":[{"band_id":"B1","above":null,"up_to":"10000","min_distinct_signers":1,"required_offices":[{"office":"FINANCE_SIGNER","min_count":1}]},` +
	`{"band_id":"B2","above":"10000","up_to":"100000","min_distinct_signers":2,"required_offices":[{"office":"DIRECTOR","min_count":1}],"eligible_offices":["FINANCE_SIGNER","DIRECTOR","CFO","CEO"]},` +
	`{"band_id":"B3","above":"100000","up_to":null,"min_distinct_signers":2,"required_offices":[{"office":"CFO","min_count":1},{"office":"CEO","min_count":1}],"separate_release_authorization":true}],` +
	`"exemptions":[{"exemption_id":"E-NDA","instrument_type":"NDA","reason":"published template"}]}`

const docHash = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func activeMatrix(id, pid string, entity *string, payload string) *domain.ApplicablePolicyVersion {
	v := activeThreshold(id, pid, strPtr(testTenant), entity, payload)
	v.PolicyCode = "SIG-" + id[:4]
	return v
}

func sigBody(amount string, mutate ...func(map[string]any)) string {
	b := map[string]any{
		"contract_version": "1.0", "request_id": "req-1", "correlation_id": "flow-1",
		"tenant_id": testTenant, "legal_entity_id": testEntity, "action": "SIGN_CONTRACT",
		"resource":        map[string]any{"type": "contract", "id": "C-1", "version": "3"},
		"money":           map[string]any{"amount": amount, "currency": "GBP", "basis": "GROSS"},
		"instrument_type": "SUPPLIER_CONTRACT", "jurisdiction": "GB", "document_hash": docHash,
		"evaluated_for_at": fixedNow.Format(time.RFC3339),
	}
	for _, m := range mutate {
		m(b)
	}
	raw, _ := json.Marshal(b)
	return string(raw)
}

func sigRig(cands ...*domain.ApplicablePolicyVersion) *rig {
	return newRig(&stubStore{}, &stubCandidates{candidates: cands}, nil)
}

func checkDecision(t *testing.T, raw []byte) govdecision.Verdict {
	t.Helper()
	v := govdecision.Check(raw, govdecision.Expectation{
		TenantID: testTenant, LegalEntityID: testEntity, ActorPrincipalID: testPrincipal, Action: "SIGN_CONTRACT",
		Resource: govdecision.Resource{Type: "contract", ID: "C-1", Version: "3"}, OwningService: "policy-svc",
		PolicyFamily: "SIGNATORY_MATRIX", Now: fixedNow, Keys: govdecision.KeySet{testSigner.KeyID: testSigner.Public()},
	})
	if !v.Trusted() {
		t.Fatalf("decision not trusted: %v\n%s", v.Problems, raw)
	}
	return v
}

func TestSignatoryResolve_SignedDecisionStoredAsEvidence(t *testing.T) {
	g := sigRig(activeMatrix(versionID, policyID, nil, sigMatrix))
	w := g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("11000.00"), header("X-Legal-Entity-Id", testEntity))
	expect(t, w, http.StatusOK, "")
	v := checkDecision(t, w.Body.Bytes())
	d := v.Decision
	if d.Outcome != govdecision.SignatureRequired || d.PolicyVersionID != versionID || d.PolicyVersion != "1" || !strings.HasSuffix(d.RuleID, "#band:B2") {
		t.Fatalf("decision %+v", d)
	}
	if len(d.RequiredActions) < 2 || d.RequiredActions[0].Kind != govdecision.ActionKindSignatureQuorum ||
		d.RequiredActions[0].MinDistinctSigners != 2 || d.RequiredActions[0].DocumentHash != docHash {
		t.Fatalf("required actions %+v", d.RequiredActions)
	}
	if !d.ValidUntil.Equal(fixedNow.Add(15 * time.Minute)) {
		t.Fatalf("valid_until %v", d.ValidUntil)
	}
	// Authorized in the legal entity as an evaluation.
	if g.az.actionType != authz.ActionPolicyEvaluate || g.az.scope != testEntity {
		t.Fatalf("authorized %s at %s", g.az.actionType, g.az.scope)
	}
	ev := g.store.saved
	if ev == nil || ev.EvaluationID != d.DecisionID || ev.DecisionID != "req-1" || ev.PolicyType != "SIGNATORY_MATRIX" ||
		ev.DecisionOutcome != govdecision.SignatureRequired || string(ev.DecisionEnvelope) != w.Body.String() ||
		d.EvidenceRef != "policy-svc:policy-evaluations/"+d.DecisionID || ev.ValidUntil == nil {
		t.Fatalf("evidence %+v", ev)
	}
}

// P0-05 through the API: 100,000 is band 2, 100,000.01 is band 3.
func TestSignatoryResolve_BandBoundary(t *testing.T) {
	for amount, band := range map[string]string{"100000": "B2", "100000.01": "B3", "10000.00": "B1"} {
		g := sigRig(activeMatrix(versionID, policyID, nil, sigMatrix))
		w := g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody(amount))
		expect(t, w, http.StatusOK, "")
		if d := checkDecision(t, w.Body.Bytes()).Decision; !strings.HasSuffix(d.RuleID, "#band:"+band) {
			t.Fatalf("%s resolved to %s", amount, d.RuleID)
		}
	}
}

// P0-01: no matrix is NOT_CONFIGURED, recorded, never PERMIT.
func TestSignatoryResolve_NoMatrixIsNotConfigured(t *testing.T) {
	g := sigRig()
	w := g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("5"))
	expect(t, w, http.StatusOK, "")
	d := checkDecision(t, w.Body.Bytes()).Decision
	if d.Outcome != govdecision.NotConfigured || d.ReasonCodes[0] != govdecision.ReasonConfigPolicyMissing || len(d.RequiredActions) != 0 || d.PolicyVersionID != "" {
		t.Fatalf("%+v", d)
	}
	if g.store.saved == nil || g.store.saved.PolicyVersionID != "" || g.store.saved.Result != govdecision.NotConfigured {
		t.Fatalf("a NOT_CONFIGURED decision is evidence too: %+v", g.store.saved)
	}
	// A high-risk instrument names the missing instrument-specific matrix.
	g = sigRig(activeMatrix(versionID, policyID, nil, sigMatrix))
	w = g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("5", func(b map[string]any) { b["risk_flags"] = []string{"RELATED_PARTY"} }))
	if d := checkDecision(t, w.Body.Bytes()).Decision; d.Outcome != govdecision.NotConfigured || d.ReasonCodes[0] != govdecision.ReasonConfigInstrumentMatrixMissing {
		t.Fatalf("%+v", d)
	}
}

// Overlapping matrices are NOT_CONFIGURED, never first-match (Doc 01 §3.2).
func TestSignatoryResolve_OverlapIsNotConfigured(t *testing.T) {
	g := sigRig(activeMatrix(versionID, policyID, nil, sigMatrix), activeMatrix(otherVersionID, otherPolicyID, nil, sigMatrix))
	w := g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("5"))
	expect(t, w, http.StatusOK, "")
	d := checkDecision(t, w.Body.Bytes()).Decision
	if d.Outcome != govdecision.NotConfigured || d.ReasonCodes[0] != govdecision.ReasonConfigRuleOverlap {
		t.Fatalf("%+v", d)
	}
	if len(g.store.events) != 1 || g.store.events[0].EventType != "policy.applicability.conflicted" {
		t.Fatalf("conflict not recorded: %+v", g.store.events)
	}
}

// A more specific matrix that does not govern the instrument does not shadow
// one that does.
func TestSignatoryResolve_OnlyCoveringMatricesCompete(t *testing.T) {
	ndaOnly := strings.Replace(sigMatrix, `"instrument_types":["SUPPLIER_CONTRACT","PAYMENT_INSTRUCTION","NDA"]`, `"instrument_types":["NDA"]`, 1)
	ndaOnly = strings.Replace(ndaOnly, `,"exemptions":[{"exemption_id":"E-NDA","instrument_type":"NDA","reason":"published template"}]`, "", 1)
	g := sigRig(activeMatrix(versionID, policyID, nil, sigMatrix), activeMatrix(otherVersionID, otherPolicyID, strPtr(testEntity), ndaOnly))
	w := g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("5"))
	if d := checkDecision(t, w.Body.Bytes()).Decision; d.Outcome != govdecision.SignatureRequired || d.PolicyVersionID != versionID {
		t.Fatalf("%+v", d)
	}
}

func TestSignatoryResolve_ExplicitExemptionPermits(t *testing.T) {
	g := sigRig(activeMatrix(versionID, policyID, nil, sigMatrix))
	w := g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("0", func(b map[string]any) { b["instrument_type"] = "NDA" }))
	if d := checkDecision(t, w.Body.Bytes()).Decision; d.Outcome != govdecision.Permit || d.ReasonCodes[0] != govdecision.ReasonSignatureExempt {
		t.Fatalf("%+v", d)
	}
}

// C02-P0-03 and the identity rules: the body never widens the verified scope.
func TestSignatoryResolve_ScopeAndActorComeFromIdentity(t *testing.T) {
	cases := map[string]struct {
		body string
		opts []reqOpt
	}{
		"foreign tenant": {body: sigBody("5", func(b map[string]any) { b["tenant_id"] = otherTenant })},
		"foreign entity": {body: sigBody("5"), opts: []reqOpt{header("X-Legal-Entity-Id", otherEntity)}},
		"impersonation":  {body: sigBody("5", func(b map[string]any) { b["actor_principal_id"] = "someone-else" })},
	}
	for name, c := range cases {
		g := sigRig(activeMatrix(versionID, policyID, nil, sigMatrix))
		w := g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", c.body, c.opts...)
		expect(t, w, http.StatusForbidden, "CONTEXT_UNRESOLVED")
		if g.store.saveCalls != 0 || g.cands.calls != 0 {
			t.Fatalf("%s: refused request reached the store", name)
		}
	}
	g := sigRig(activeMatrix(versionID, policyID, nil, sigMatrix))
	expect(t, g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("5"), noPrincipal), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
	g = newRig(&stubStore{}, &stubCandidates{}, &stubAuthz{err: domain.ErrAuthorizationDenied})
	expect(t, g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("5")), http.StatusForbidden, "AUTHORIZATION_DENIED")
}

// Doc 02 §9: schema violations are 400 problems, never decisions.
func TestSignatoryResolve_SchemaViolationsAre400(t *testing.T) {
	cases := map[string]string{
		"float money":      strings.Replace(sigBody("5"), `"amount":"5"`, `"amount":5`, 1),
		"unknown member":   sigBody("5", func(b map[string]any) { b["auto_approve"] = true }),
		"no document hash": sigBody("5", func(b map[string]any) { delete(b, "document_hash") }),
		"bad hash":         sigBody("5", func(b map[string]any) { b["document_hash"] = "md5:abc" }),
		"contract 2.0":     sigBody("5", func(b map[string]any) { b["contract_version"] = "2.0" }),
		"no money":         sigBody("5", func(b map[string]any) { delete(b, "money") }),
		"negative":         sigBody("-1"),
		"no basis":         sigBody("5", func(b map[string]any) { b["money"] = map[string]any{"amount": "5", "currency": "GBP"} }),
		"lower-case act":   sigBody("5", func(b map[string]any) { b["action"] = "sign" }),
		"no resource ver":  sigBody("5", func(b map[string]any) { b["resource"] = map[string]any{"type": "contract", "id": "C-1"} }),
		"no jurisdiction":  sigBody("5", func(b map[string]any) { delete(b, "jurisdiction") }),
		"no evaluated_for": sigBody("5", func(b map[string]any) { delete(b, "evaluated_for_at") }),
	}
	for name, body := range cases {
		g := sigRig(activeMatrix(versionID, policyID, nil, sigMatrix))
		w := g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		if g.store.saveCalls != 0 {
			t.Fatalf("%s: recorded", name)
		}
	}
}

// Doc 02 §9.1: the same request_id replays the stored bytes; other content is 409.
func TestSignatoryResolve_ReplayAndConflict(t *testing.T) {
	g := sigRig(activeMatrix(versionID, policyID, nil, sigMatrix))
	first := g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("11000"))
	expect(t, first, http.StatusOK, "")
	g.store.evaluation = g.store.saved
	again := g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("11000"))
	expect(t, again, http.StatusOK, "")
	if again.Body.String() != first.Body.String() || again.Header().Get("X-Decision-Replayed") != "true" || g.store.saveCalls != 1 {
		t.Fatalf("replay differs or re-evaluated (saves %d)", g.store.saveCalls)
	}
	expect(t, g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("11000.01")), http.StatusConflict, "IDEMPOTENCY_MISMATCH")
}

// C02-P0-15: no durable evidence, no decision. A tampered matrix is not used.
func TestSignatoryResolve_FailsClosed(t *testing.T) {
	g := newRig(&stubStore{saveErr: errors.New("db down")}, &stubCandidates{candidates: []*domain.ApplicablePolicyVersion{activeMatrix(versionID, policyID, nil, sigMatrix)}}, nil)
	expect(t, g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("5")), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")

	tampered := activeMatrix(versionID, policyID, nil, sigMatrix)
	tampered.ArtifactDigest = digestOf("something else")
	g = sigRig(tampered)
	body := expect(t, g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("5")), http.StatusInternalServerError, "EVIDENCE_INTEGRITY_FAILED")
	if body["outcome"] != govdecision.Error || g.store.saveCalls != 0 {
		t.Fatalf("%v", body)
	}

	g = newRig(&stubStore{}, &stubCandidates{err: errors.New("cache down")}, nil)
	expect(t, g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("5")), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
}

func TestDecisions_ServedSignedAndVerifiableByPublishedKey(t *testing.T) {
	g := sigRig(activeMatrix(versionID, policyID, nil, sigMatrix))
	w := g.do(http.MethodPost, "/internal/v1/policy/signatory-matrix/resolve", sigBody("5"))
	d := checkDecision(t, w.Body.Bytes()).Decision

	g.store.evaluation = g.store.saved
	got := g.do(http.MethodGet, "/internal/v1/policy/decisions/"+d.DecisionID, "")
	expect(t, got, http.StatusOK, "")
	if got.Body.String() != w.Body.String() {
		t.Fatal("the stored decision differs from the one answered")
	}
	keys := g.do(http.MethodGet, "/v1/policy-decision-keys", "")
	expect(t, keys, http.StatusOK, "")
	set, err := govdecision.ParseJWKS(keys.Body.Bytes())
	if err != nil || set.Verify(got.Body.Bytes()) != nil {
		t.Fatalf("published key does not verify: %v", err)
	}

	g.store.evaluation = &domain.Evaluation{EvaluationID: evidenceID, TenantID: testTenant}
	expect(t, g.do(http.MethodGet, "/internal/v1/policy/decisions/"+evidenceID, ""), http.StatusNotFound, "NOT_FOUND")
	expect(t, g.do(http.MethodGet, "/internal/v1/policy/decisions/not-a-uuid", ""), http.StatusBadRequest, "INVALID_REQUEST")
}

// P0-01: the families other services decide answer NOT_IMPLEMENTED on the
// evaluate route, name their owner, and never consult the policy set, so a
// configured reference version cannot turn into a permit.
func TestEvaluate_ReferenceFamiliesNeverPermit(t *testing.T) {
	for typ, owner := range map[string]string{"SPEND_CONTROL": "spend-controls-svc", "SOD_RULE": "authorization-svc", "LEGAL_BASIS": "privacy-decision-svc", "SIGNATORY_MATRIX": "policy-svc", "UNHEARD_OF": ""} {
		g := sigRig(activeThreshold(versionID, policyID, strPtr(testTenant), nil, thresholdPayload))
		body := `{"policy_type":"` + typ + `","tenant_id":"` + testTenant + `","decision_id":"d","action_context":{"amount":1}}`
		got := expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", body), http.StatusNotImplemented, "NOT_IMPLEMENTED")
		if got["outcome"] != govdecision.NotImplemented || (owner != "" && got["owning_service"] != owner) {
			t.Fatalf("%s: %v", typ, got)
		}
		if g.cands.calls != 0 || g.store.saveCalls != 0 {
			t.Fatalf("%s: evaluated anyway", typ)
		}
	}
}

func TestEvaluate_CarriesTheSharedOutcome(t *testing.T) {
	g := evalRig(nil)
	got := expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody("")), http.StatusOK, "")
	if got["outcome"] != govdecision.ApprovalRequired || got["owning_service"] != "policy-svc" || got["contract_version"] != "1.0" {
		t.Fatalf("%v", got)
	}
	g = newRig(&stubStore{}, &stubCandidates{}, nil)
	nf := expect(t, g.do(http.MethodPost, "/v1/policies/evaluate", evalBody("")), http.StatusNotFound, "POLICY_NOT_EFFECTIVE")
	if nf["outcome"] != govdecision.NotConfigured {
		t.Fatalf("%v", nf)
	}
}

func TestCreateVersion_FamilyPayloadRules(t *testing.T) {
	cases := []struct {
		policyType, payload string
		status              int
	}{
		{"SIGNATORY_MATRIX", sigMatrix, http.StatusCreated},
		{"SIGNATORY_MATRIX", strings.Replace(sigMatrix, `"above":"10000"`, `"above":"9000"`, 1), http.StatusBadRequest},
		{"SIGNATORY_MATRIX", `{"threshold_amount":5000}`, http.StatusBadRequest},
		{"SPEND_CONTROL", `{"rule_set_ref":"spend-controls:policy/MKT-UK","rule_set_version":"2"}`, http.StatusCreated},
		{"SPEND_CONTROL", `{"limit_amount":"50000"}`, http.StatusBadRequest},
		{"SPEND_CONTROL", `{"rule_set_ref":"x","rule_set_version":"1","threshold_amount":5}`, http.StatusBadRequest},
		{"SOD_RULE", `{"rule_set_ref":"authorization:sod/conflict-set","rule_set_version":"7","owning_service":"authorization-svc"}`, http.StatusCreated},
		{"SOD_RULE", `{"rule_set_ref":"x","rule_set_version":"1","owning_service":"policy-svc"}`, http.StatusBadRequest},
		{"LEGAL_BASIS", `{"jurisdiction":"GB","rule_reference":"UK GDPR Art 6(1)(c)"}`, http.StatusCreated},
		{"LEGAL_BASIS", `{"legal_basis":"consent"}`, http.StatusBadRequest},
		{"LEGAL_BASIS", ``, http.StatusBadRequest},
	}
	for _, c := range cases {
		s := &stubStore{
			policy:         &domain.Policy{PolicyID: policyID, PolicyType: c.policyType, TenantID: strPtr(testTenant)},
			version:        &domain.PolicyVersion{PolicyVersionID: versionID, PolicyID: policyID, RowVersion: 1},
			versionCreated: true,
		}
		g := newRig(s, nil, nil)
		body := `{"effective_from":"2026-11-01T00:00:00Z"`
		if c.payload != "" {
			body += `,"rule_payload":` + c.payload
		}
		w := g.do(http.MethodPost, "/v1/policies/"+policyID+"/versions", body+"}")
		if w.Code != c.status {
			t.Fatalf("%s %s: %d %s", c.policyType, c.payload, w.Code, w.Body.String())
		}
	}
}

// newRigWithStore is newRig over any store (one that adds optional reads).
func newRigWithStore(s handler.PolicyStore) *rig {
	o := handler.Options{PlatformScopeID: testAuthzScopeID, Now: func() time.Time { return fixedNow }, DecisionSigner: testSigner}
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))
	h := handler.New(s, &stubCandidates{}, &stubAuthz{}, o, zap.NewNop())
	handler.RegisterRoutes(r, h)
	handler.RegisterPolicyTypeRoutes(r, h)
	return &rig{store: &stubStore{}, cands: &stubCandidates{}, az: &stubAuthz{}, r: r}
}

type statusStore struct {
	*stubStore
	statuses map[string]*domain.FamilyStatus
}

func (s *statusStore) FamilyStatuses(context.Context) (map[string]*domain.FamilyStatus, error) {
	return s.statuses, nil
}

// Doc 01 §9: one card per family; not-implemented and unconfigured are
// blocking states, never success.
func TestPolicyTypes_StatusCards(t *testing.T) {
	st := &statusStore{stubStore: &stubStore{}, statuses: map[string]*domain.FamilyStatus{
		"APPROVAL_THRESHOLD": {PolicyType: "APPROVAL_THRESHOLD", Families: 1, VersionsByStatus: map[string]int64{"ACTIVE": 1},
			GoverningVersions: []domain.GoverningVersion{{PolicyVersionID: versionID}}, Decisions24h: map[string]int64{"PERMIT": 3}},
		"MYSTERY": {PolicyType: "MYSTERY", Families: 1, VersionsByStatus: map[string]int64{}},
	}}
	r := newRigWithStore(st)
	w := r.do(http.MethodGet, "/v1/policy-types", "")
	expect(t, w, http.StatusOK, "")
	var resp struct {
		Items []struct {
			PolicyType    string `json:"policy_type"`
			OwningService string `json:"owning_service"`
			DisplayState  string `json:"display_state"`
			Blocking      bool   `json:"blocking"`
			EvaluableHere bool   `json:"evaluable_here"`
		} `json:"items"`
		Outcomes []string `json:"outcomes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"APPROVAL_THRESHOLD": {"EVALUABLE", "policy-svc"},
		"SIGNATORY_MATRIX":   {"CONFIGURATION_REQUIRED", "policy-svc"},
		"SPEND_CONTROL":      {"DECIDED_BY_OWNING_SERVICE", "spend-controls-svc"},
		"SOD_RULE":           {"DECIDED_BY_OWNING_SERVICE", "authorization-svc"},
		"LEGAL_BASIS":        {"DECIDED_BY_OWNING_SERVICE", "privacy-decision-svc"},
		"MYSTERY":            {"NOT_IMPLEMENTED", ""},
	}
	for _, it := range resp.Items {
		w, ok := want[it.PolicyType]
		if !ok {
			continue
		}
		if it.DisplayState != w[0] || it.OwningService != w[1] || it.Blocking != (w[0] != "EVALUABLE") {
			t.Fatalf("%s: %+v", it.PolicyType, it)
		}
		delete(want, it.PolicyType)
	}
	if len(want) != 0 || len(resp.Outcomes) != 8 {
		t.Fatalf("missing cards %v / outcomes %v", want, resp.Outcomes)
	}
}
