package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
	svcenvelope "zoiko.io/policy-svc/internal/envelope"
	"zoiko.io/policy-svc/internal/handler"
	svcmiddleware "zoiko.io/policy-svc/internal/middleware"
)

// REF-07 accounting policy at the HTTP layer: the caller's tenant and identity
// are the verified ones, every refusal is its own answer, and a request that
// exactly one version does not answer blocks calculation instead of guessing.
// The SQL, the guards and the ranking against real data are proven in
// internal/store.

const (
	bookGB       = "55555555-5555-5555-5555-555555555555"
	bookUS       = "66666666-6666-6666-6666-666666666666"
	jurisdiction = "77777777-7777-7777-7777-777777777777"
)

type acctStub struct {
	*stubStore

	drafted     *domain.AccountingPolicyVersion
	draftCreate bool
	draftErr    error
	gotDraft    domain.DraftAccountingPolicyParams
	draftCalls  int

	version    *domain.AccountingPolicyVersion
	versionErr error

	candidates []*domain.AccountingCandidate
	candErr    error
	gotQuery   domain.AccountingCandidateQuery

	impact    *domain.AccountingImpact
	impactErr error

	isAccounting    bool
	isAccountingErr error
	checkedPolicy   string
}

func (s *acctStub) CreateAccountingDraft(_ context.Context, p domain.DraftAccountingPolicyParams) (*domain.AccountingPolicyVersion, bool, error) {
	s.draftCalls++
	s.gotDraft = p
	return s.drafted, s.draftCreate, s.draftErr
}

func (s *acctStub) GetAccountingPolicyVersion(_ context.Context, id string) (*domain.AccountingPolicyVersion, error) {
	if s.versionErr != nil {
		return nil, s.versionErr
	}
	if s.version != nil {
		return s.version, nil
	}
	for _, c := range s.candidates {
		if c.PolicyVersionID == id {
			return &domain.AccountingPolicyVersion{PolicyVersion: c.PolicyVersion, PolicyCode: c.PolicyCode, Applicability: c.Applicability,
				Details: domain.AccountingVersionDetails{SemanticVersion: c.SemanticVersion, Precedence: c.Precedence}}, nil
		}
	}
	return nil, domain.ErrPolicyVersionNotFound
}

func (s *acctStub) AccountingCandidates(_ context.Context, q domain.AccountingCandidateQuery) ([]*domain.AccountingCandidate, error) {
	s.gotQuery = q
	return s.candidates, s.candErr
}

func (s *acctStub) AccountingImpactOf(_ context.Context, _ string) (*domain.AccountingImpact, error) {
	return s.impact, s.impactErr
}

func (s *acctStub) IsAccountingPolicy(_ context.Context, policyID string) (bool, error) {
	s.checkedPolicy = policyID
	return s.isAccounting, s.isAccountingErr
}

type acctRig struct {
	st *acctStub
	az *stubAuthz
	r  chi.Router
}

func newAcctRig(st *acctStub, az *stubAuthz, opts ...rigOption) *acctRig {
	if az == nil {
		az = &stubAuthz{}
	}
	rig := newAcctRigClient(st, az, opts...)
	rig.az = az
	return rig
}

// newAcctRigClient is newAcctRig with any authorization client, for the tests
// that need one with more than CheckAllowed.
func newAcctRigClient(st *acctStub, client authz.Client, opts ...rigOption) *acctRig {
	if st == nil {
		st = &acctStub{}
	}
	if st.stubStore == nil {
		st.stubStore = &stubStore{}
	}
	o := handler.Options{PlatformScopeID: testAuthzScopeID, Now: func() time.Time { return fixedNow }}
	for _, f := range opts {
		f(&o)
	}
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))
	handler.RegisterRoutes(r, handler.New(st, &stubCandidates{}, client, o, zap.NewNop()))
	return &acctRig{st: st, r: r}
}

// do sends a request; the first string after the path is the body, any reqOpt
// after it adjusts the request.
func (g *acctRig) do(method, path string, args ...any) *httptest.ResponseRecorder {
	body := ""
	var opts []reqOpt
	for _, a := range args {
		switch v := a.(type) {
		case string:
			body = v
		case reqOpt:
			opts = append(opts, v)
		}
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Principal-Id", testPrincipal)
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("X-Correlation-ID", "corr-1")
	for _, o := range opts {
		o(req)
	}
	w := httptest.NewRecorder()
	g.r.ServeHTTP(w, req)
	return w
}

// cand builds an activated candidate. Each is its own policy, as each family is.
func cand(id, policyDomain string, book, juris, entity *string, precedence int) *domain.AccountingCandidate {
	activated := fixedNow.Add(-48 * time.Hour)
	c := &domain.AccountingCandidate{
		Applicability:   domain.AccountingApplicability{PolicyID: "p-" + id, PolicyDomain: policyDomain, Framework: "IFRS", BookID: book, JurisdictionID: juris},
		SemanticVersion: "1.0.0", Precedence: precedence,
	}
	c.PolicyVersion = domain.PolicyVersion{
		PolicyVersionID: id, PolicyID: "p-" + id, TenantID: strPtr(testTenant), LegalEntityID: entity,
		ScopeType: domain.DeriveScopeType(strPtr(testTenant), entity), VersionStatus: domain.StatusActive, VersionNumber: 1,
		EffectiveFrom: fixedNow.Add(-72 * time.Hour), ActivatedAt: &activated, ArtifactDigest: "sha256:" + id, RowVersion: 4,
	}
	c.PolicyCode = "ACCT:" + policyDomain + ":" + id
	return c
}

const draftBody = `{
  "policy_domain": "REVENUE_RECOGNITION", "framework": "IFRS", "book_id": "` + bookGB + `", "jurisdiction_id": "` + jurisdiction + `",
  "legal_entity_id": "` + testEntity + `", "semantic_version": "2.1.0", "precedence": 5,
  "framework_source": "IFRS 15 (2018) paragraph 31", "policy_text_ref": "policy-manual/rev-rec/v2.1",
  "rule_mappings": [{"ref_kind": "JURISDICTION_PACK", "ref": "pack-gb-vat", "ref_version": "2026.3", "rationale": "VAT point of supply", "maps_to": "output tax"}],
  "impact_analysis": {"summary": "applies to contracts signed from 2027", "affected_periods": "FUTURE_ONLY", "closed_history_unchanged": true},
  "effective_from": "2027-01-01T00:00:00Z", "rationale": "annual policy review"
}`

func draftedVersion() *domain.AccountingPolicyVersion {
	return &domain.AccountingPolicyVersion{PolicyVersion: domain.PolicyVersion{PolicyVersionID: versionID, PolicyID: policyID, RowVersion: 1, VersionStatus: domain.StatusDraft}}
}

// ── DraftAccountingPolicy ───────────────────────────────────────────────────

func TestDraftAccounting_IsInTheVerifiedTenantAsTheVerifiedCaller(t *testing.T) {
	st := &acctStub{drafted: draftedVersion(), draftCreate: true}
	g := newAcctRig(st, nil)
	w := g.do(http.MethodPost, "/v1/accounting-policies", strings.Replace(draftBody, `"policy_domain"`, `"tenant_id": "`+otherTenant+`", "policy_domain"`, 1), header("Idempotency-Key", "key-1"))
	expect(t, w, http.StatusCreated, "")
	if w.Header().Get("ETag") != `"1"` {
		t.Errorf("ETag = %q, want the row_version", w.Header().Get("ETag"))
	}
	p := st.gotDraft
	if p.TenantID != testTenant || p.CreatedByPrincipalID != testPrincipal {
		t.Errorf("tenant %q actor %q: a draft is in the caller's tenant as the caller, whatever the body says", p.TenantID, p.CreatedByPrincipalID)
	}
	if p.PolicyDomain != "REVENUE_RECOGNITION" || p.Framework != "IFRS" || p.SemanticVersion != "2.1.0" || p.Precedence != 5 ||
		p.BookID == nil || *p.BookID != bookGB || p.JurisdictionID == nil || p.LegalEntityID == nil || *p.LegalEntityID != testEntity ||
		len(p.RuleMappings) != 1 || p.RuleMappings[0].RefKind != "JURISDICTION_PACK" || p.RuleMappings[0].RefVersion != "2026.3" ||
		p.Impact.AffectedPeriods != "FUTURE_ONLY" || p.RequestKey != "key-1" {
		t.Errorf("params = %+v", p)
	}
	if g.az.actionType != authz.ActionAccountingPolicyDraft || g.az.scope != testEntity {
		t.Errorf("asked %s at %s, want ACCOUNTING_POLICY_DRAFT in the entity's scope", g.az.actionType, g.az.scope)
	}
}

func TestDraftAccounting_ReplayIs200(t *testing.T) {
	g := newAcctRig(&acctStub{drafted: draftedVersion(), draftCreate: false}, nil)
	expect(t, g.do(http.MethodPost, "/v1/accounting-policies", draftBody), http.StatusOK, "")
}

func TestDraftAccounting_ListsEveryProblemAndNeverReachesTheStore(t *testing.T) {
	st := &acctStub{drafted: draftedVersion(), draftCreate: true}
	g := newAcctRig(st, nil)
	w := g.do(http.MethodPost, "/v1/accounting-policies", `{"policy_domain":"rev","framework":"","semantic_version":"2","precedence":-1,"impact_analysis":{}}`)
	body := expect(t, w, http.StatusBadRequest, "INVALID_REQUEST")
	errs, _ := body["errors"].([]any)
	if len(errs) < 6 {
		t.Errorf("errors = %v, want every problem at once", errs)
	}
	if st.draftCalls != 0 || g.az.calls != 0 {
		t.Error("an invalid draft reached the store or authorization")
	}

	// Taking effect in the past reaches open periods and must say so.
	retro := strings.Replace(draftBody, `"effective_from": "2027-01-01T00:00:00Z"`, `"effective_from": "2026-01-01T00:00:00Z"`, 1)
	body = expect(t, g.do(http.MethodPost, "/v1/accounting-policies", retro), http.StatusBadRequest, "INVALID_REQUEST")
	if !strings.Contains(w.Body.String(), "impact_analysis") || !strings.Contains(toJSON(body), "retroactive_justification") {
		t.Errorf("a retroactive draft was not asked to justify itself: %v", body)
	}
	if st.draftCalls != 0 {
		t.Error("a retroactive draft with no justification reached the store")
	}
}

func toJSON(m map[string]any) string {
	var sb strings.Builder
	for k, v := range m {
		sb.WriteString(k)
		if s, ok := v.(string); ok {
			sb.WriteString(s)
		}
		if l, ok := v.([]any); ok {
			for _, e := range l {
				if s, ok := e.(string); ok {
					sb.WriteString(s)
				}
			}
		}
	}
	return sb.String()
}

func TestDraftAccounting_NeedsAnAuthenticatedAuthorizedCaller(t *testing.T) {
	st := &acctStub{drafted: draftedVersion(), draftCreate: true}
	g := newAcctRig(st, &stubAuthz{byAction: map[string]error{authz.ActionAccountingPolicyDraft: domain.ErrAuthorizationDenied}})
	expect(t, g.do(http.MethodPost, "/v1/accounting-policies", draftBody), http.StatusForbidden, "AUTHORIZATION_DENIED")
	if st.draftCalls != 0 {
		t.Error("a denied draft reached the store")
	}

	g = newAcctRig(st, &stubAuthz{err: domain.ErrAuthorizationServiceUnavailable})
	expect(t, g.do(http.MethodPost, "/v1/accounting-policies", draftBody), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")

	g = newAcctRig(st, nil)
	expect(t, g.do(http.MethodPost, "/v1/accounting-policies", draftBody, noPrincipal), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
	expect(t, g.do(http.MethodPost, "/v1/accounting-policies", draftBody, noTenant), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
	if st.draftCalls != 0 {
		t.Error("an unauthenticated draft reached the store")
	}
}

func TestDraftAccounting_StoreRefusalsAreMapped(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"a mapping depends on nothing": {domain.ErrAccountingDependencyNotFound, http.StatusNotFound, "NOT_FOUND"},
		"same semantic version, other": {domain.ErrIdempotencyMismatch, http.StatusConflict, "IDEMPOTENCY_MISMATCH"},
		"a different framework":        {domain.ErrConflict, http.StatusConflict, "IDEMPOTENCY_MISMATCH"},
		"store down":                   {domain.ErrStoreUnavailable, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"},
	} {
		g := newAcctRig(&acctStub{draftErr: c.err}, nil)
		expect(t, g.do(http.MethodPost, "/v1/accounting-policies", draftBody), c.status, c.code)
		_ = name
	}
}

// ── the generic endpoints stand aside ───────────────────────────────────────

func TestGenericEndpointsRefuseAccountingPolicies(t *testing.T) {
	st := &acctStub{isAccounting: true, stubStore: &stubStore{findVersion: &domain.PolicyVersion{PolicyVersionID: versionID, PolicyID: policyID, RowVersion: 1}}}
	g := newAcctRig(st, nil)

	expect(t, g.do(http.MethodPost, "/v1/policies", `{"policy_code":"A","policy_name":"A","policy_type":"ACCOUNTING_POLICY"}`), http.StatusBadRequest, "ACCOUNTING_POLICY_ENDPOINT_REQUIRED")
	expect(t, g.do(http.MethodPost, "/v1/policies", `{"policy_code":"A","policy_name":"A","policy_type":"accounting_policy"}`), http.StatusBadRequest, "ACCOUNTING_POLICY_ENDPOINT_REQUIRED")
	if st.createPolicyCalls != 0 {
		t.Error("an accounting family was created through the generic endpoint")
	}

	st.policy = &domain.Policy{PolicyID: policyID, PolicyType: domain.PolicyTypeAccounting, TenantID: strPtr(testTenant)}
	expect(t, g.do(http.MethodPost, "/v1/policies/"+policyID+"/versions", `{"effective_from":"2027-01-01T00:00:00Z"}`), http.StatusBadRequest, "ACCOUNTING_POLICY_ENDPOINT_REQUIRED")
	if st.createVersionCalls != 0 {
		t.Error("an accounting version was created through the generic endpoint")
	}

	for _, command := range []string{"submit-review", "approve", "activate", "schedule", "suspend", "retire", "review-suspension"} {
		w := g.do(http.MethodPost, "/v1/policies/"+policyID+"/versions/"+versionID+"/"+command, `{"reason_code":"x","outcome":"UPHELD"}`, header("If-Match", `"1"`))
		expect(t, w, http.StatusBadRequest, "ACCOUNTING_POLICY_ENDPOINT_REQUIRED")
	}
	if st.transitionCalls != 0 {
		t.Error("an accounting version was moved through the generic lifecycle")
	}

	expect(t, g.do(http.MethodGet, "/v1/policies/resolve?policy_type=ACCOUNTING_POLICY"), http.StatusBadRequest, "ACCOUNTING_POLICY_ENDPOINT_REQUIRED")
	expect(t, g.do(http.MethodGet, "/v1/policies/explain?policy_type=accounting_policy"), http.StatusBadRequest, "ACCOUNTING_POLICY_ENDPOINT_REQUIRED")
	expect(t, g.do(http.MethodGet, "/v1/policies?policy_type=ACCOUNTING_POLICY"), http.StatusBadRequest, "ACCOUNTING_POLICY_ENDPOINT_REQUIRED")
}

// Other policies are untouched: the lifecycle of an approval threshold works as before.
func TestGenericEndpointsStillServeOtherPolicies(t *testing.T) {
	st := &acctStub{isAccounting: false,
		stubStore: &stubStore{findVersion: &domain.PolicyVersion{PolicyVersionID: versionID, PolicyID: policyID, TenantID: strPtr(testTenant), RowVersion: 1},
			transitionResult: &domain.TransitionResult{Version: &domain.PolicyVersion{PolicyVersionID: versionID, PolicyID: policyID, RowVersion: 2}, Transitioned: true}}}
	g := newAcctRig(st, nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/"+policyID+"/versions/"+versionID+"/submit-review", `{}`, header("If-Match", `"1"`)), http.StatusOK, "")
	if st.transitionCalls != 1 {
		t.Error("the generic lifecycle no longer serves other policies")
	}
}

func TestGenericEndpointsFailClosedWhenTheAccountingCheckFails(t *testing.T) {
	st := &acctStub{isAccountingErr: domain.ErrStoreUnavailable, stubStore: &stubStore{findVersion: &domain.PolicyVersion{PolicyVersionID: versionID, PolicyID: policyID, RowVersion: 1}}}
	g := newAcctRig(st, nil)
	expect(t, g.do(http.MethodPost, "/v1/policies/"+policyID+"/versions/"+versionID+"/approve", `{}`, header("If-Match", `"1"`)), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	if st.transitionCalls != 0 {
		t.Error("a version was approved although it could not be told whether it is an accounting policy")
	}
}

// ── resolution ──────────────────────────────────────────────────────────────

const resolvePath = "/v1/accounting-policies/resolve?policy_domain=REVENUE_RECOGNITION"

func TestResolveAccounting_OneGoverningVersionIsPinned(t *testing.T) {
	st := &acctStub{candidates: []*domain.AccountingCandidate{cand("v1", "REVENUE_RECOGNITION", nil, nil, nil, 0)}}
	g := newAcctRig(st, nil)
	body := expect(t, g.do(http.MethodGet, resolvePath+"&book_id="+bookGB+"&jurisdiction_id="+jurisdiction+"&legal_entity_id="+testEntity), http.StatusOK, "")
	if body["status"] != "RESOLVED" || body["blocks_calculation"] != false {
		t.Fatalf("body = %v", body)
	}
	pin, _ := body["pin"].(map[string]any)
	if pin["policy_version_id"] != "v1" || pin["artifact_digest"] != "sha256:v1" || pin["semantic_version"] != "1.0.0" || pin["row_version"] != float64(4) {
		t.Errorf("pin = %v, want the version, its digest and row version", pin)
	}
	if pol, _ := body["policy"].(map[string]any); pol["policy_code"] == nil {
		t.Errorf("the governing policy itself is missing: %v", body)
	}
	q := st.gotQuery
	if q.TenantID != testTenant || q.PolicyDomain != "REVENUE_RECOGNITION" || q.BookID == nil || *q.BookID != bookGB ||
		q.JurisdictionID == nil || *q.JurisdictionID != jurisdiction || q.LegalEntityID == nil || *q.LegalEntityID != testEntity {
		t.Errorf("candidate query = %+v", q)
	}
	if g.az.actionType != authz.ActionPolicyRead {
		t.Errorf("asked %s, want POLICY_READ", g.az.actionType)
	}
}

func TestResolveAccounting_NothingGoverningBlocksCalculation(t *testing.T) {
	// Nothing bound at all, and a candidate bound only for a later date.
	for name, cands := range map[string][]*domain.AccountingCandidate{
		"no candidates":  nil,
		"another domain": {cand("v1", "LEASES", nil, nil, nil, 0)},
		"not yet effective": func() []*domain.AccountingCandidate {
			c := cand("v1", "REVENUE_RECOGNITION", nil, nil, nil, 0)
			c.EffectiveFrom = fixedNow.Add(24 * time.Hour)
			return []*domain.AccountingCandidate{c}
		}(),
	} {
		g := newAcctRig(&acctStub{candidates: cands}, nil)
		body := expect(t, g.do(http.MethodGet, resolvePath), http.StatusNotFound, "NO_APPLICABLE_POLICY")
		if body["blocks_calculation"] != true || body["resolution_status"] != "NOT_APPLICABLE" {
			t.Errorf("%s: body = %v, want a refusal that blocks calculation", name, body)
		}
	}
}

func TestResolveAccounting_AmbiguityBlocksAndNamesTheCandidates(t *testing.T) {
	// Equally specific (one narrows by book, one by jurisdiction), equal precedence.
	g := newAcctRig(&acctStub{candidates: []*domain.AccountingCandidate{
		cand("by-book", "REVENUE_RECOGNITION", strPtr(bookGB), nil, nil, 0),
		cand("by-jurisdiction", "REVENUE_RECOGNITION", nil, strPtr(jurisdiction), nil, 0),
	}}, nil)
	body := expect(t, g.do(http.MethodGet, resolvePath+"&book_id="+bookGB+"&jurisdiction_id="+jurisdiction), http.StatusConflict, "POLICY_CONFLICTED")
	if body["blocks_calculation"] != true || body["resolution_status"] != "CONFLICTED" {
		t.Errorf("body = %v", body)
	}
	if c, _ := body["conflicting"].([]any); len(c) != 2 {
		t.Errorf("conflicting = %v, want both candidates named", body["conflicting"])
	}
}

func TestResolveAccounting_PrecedenceAndSpecificityDecide(t *testing.T) {
	// Equal specificity, different precedence: the higher governs.
	g := newAcctRig(&acctStub{candidates: []*domain.AccountingCandidate{
		cand("by-book", "REVENUE_RECOGNITION", strPtr(bookGB), nil, nil, 1),
		cand("by-jurisdiction", "REVENUE_RECOGNITION", nil, strPtr(jurisdiction), nil, 9),
	}}, nil)
	body := expect(t, g.do(http.MethodGet, resolvePath+"&book_id="+bookGB+"&jurisdiction_id="+jurisdiction), http.StatusOK, "")
	if pin, _ := body["pin"].(map[string]any); pin["policy_version_id"] != "by-jurisdiction" {
		t.Errorf("pin = %v, want the higher precedence to govern", pin)
	}

	// Narrower scope beats precedence: a policy for the book and jurisdiction
	// governs over a general one, however high the general one's precedence.
	g = newAcctRig(&acctStub{candidates: []*domain.AccountingCandidate{
		cand("general", "REVENUE_RECOGNITION", nil, nil, nil, 1000),
		cand("narrow", "REVENUE_RECOGNITION", strPtr(bookGB), strPtr(jurisdiction), nil, 0),
		cand("entity", "REVENUE_RECOGNITION", nil, nil, strPtr(testEntity), 0),
	}}, nil)
	body = expect(t, g.do(http.MethodGet, resolvePath+"&book_id="+bookGB+"&jurisdiction_id="+jurisdiction+"&legal_entity_id="+testEntity), http.StatusOK, "")
	if pin, _ := body["pin"].(map[string]any); pin["policy_version_id"] != "narrow" {
		t.Errorf("pin = %v, want the narrowest scope to govern", pin)
	}
}

func TestResolveAccounting_AsOfPicksTheVersionEffectiveThen(t *testing.T) {
	old := cand("old", "REVENUE_RECOGNITION", nil, nil, nil, 0)
	old.EffectiveFrom, old.EffectiveTo = fixedNow.Add(-400*24*time.Hour), ptrTime(fixedNow.Add(-30*24*time.Hour))
	old.ActivatedAt = ptrTime(fixedNow.Add(-401 * 24 * time.Hour))
	old.VersionStatus = domain.StatusSuperseded
	neu := cand("new", "REVENUE_RECOGNITION", nil, nil, nil, 0)
	neu.EffectiveFrom = fixedNow.Add(-30 * 24 * time.Hour)
	neu.ActivatedAt = ptrTime(fixedNow.Add(-31 * 24 * time.Hour))
	g := newAcctRig(&acctStub{candidates: []*domain.AccountingCandidate{old, neu}}, nil)

	for asOf, want := range map[string]string{
		fixedNow.Add(-100 * 24 * time.Hour).Format(time.RFC3339): "old",
		fixedNow.Add(-5 * 24 * time.Hour).Format(time.RFC3339):   "new",
	} {
		body := expect(t, g.do(http.MethodGet, resolvePath+"&as_of="+asOf), http.StatusOK, "")
		if pin, _ := body["pin"].(map[string]any); pin["policy_version_id"] != want {
			t.Errorf("as of %s: %v, want %s", asOf, pin, want)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestResolveAccounting_InputRefusals(t *testing.T) {
	st := &acctStub{}
	g := newAcctRig(st, nil)
	for name, path := range map[string]string{
		"no domain":        "/v1/accounting-policies/resolve",
		"over-long domain": "/v1/accounting-policies/resolve?policy_domain=" + strings.Repeat("A", 65),
		"bad book id":      resolvePath + "&book_id=GB",
		"bad jurisdiction": resolvePath + "&jurisdiction_id=x",
		"bad entity":       resolvePath + "&legal_entity_id=x",
		"bad as_of":        resolvePath + "&as_of=yesterday",
		"bad known_at":     resolvePath + "&known_at=yesterday",
		"future known_at":  resolvePath + "&known_at=" + fixedNow.Add(time.Hour).Format(time.RFC3339),
	} {
		if w := g.do(http.MethodGet, path); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400 (%s)", name, w.Code, w.Body.String())
		}
	}
	if st.gotQuery.TenantID != "" {
		t.Error("an invalid request reached the store")
	}
	expect(t, g.do(http.MethodGet, resolvePath, noPrincipal), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
	expect(t, g.do(http.MethodGet, resolvePath, noTenant), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
	g = newAcctRig(&acctStub{candErr: domain.ErrStoreUnavailable}, nil)
	expect(t, g.do(http.MethodGet, resolvePath), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
	g = newAcctRig(&acctStub{}, &stubAuthz{err: domain.ErrAuthorizationDenied})
	expect(t, g.do(http.MethodGet, resolvePath), http.StatusForbidden, "AUTHORIZATION_DENIED")
}

// ── explain and list ────────────────────────────────────────────────────────

func TestExplainAccounting_TracesWhyEachCandidateDidOrDidNotGovern(t *testing.T) {
	never := cand("never", "REVENUE_RECOGNITION", nil, nil, nil, 0)
	never.ActivatedAt = nil
	g := newAcctRig(&acctStub{candidates: []*domain.AccountingCandidate{
		cand("general", "REVENUE_RECOGNITION", nil, nil, nil, 0),
		cand("narrow", "REVENUE_RECOGNITION", strPtr(bookGB), nil, nil, 0),
		cand("by-book", "REVENUE_RECOGNITION", strPtr(bookGB), nil, nil, 0),
		never,
	}}, nil)
	// narrow and by-book are the same dimensions and precedence: ambiguous.
	body := expect(t, g.do(http.MethodGet, "/v1/accounting-policies/explain?policy_domain=REVENUE_RECOGNITION&book_id="+bookGB), http.StatusOK, "")
	if body["status"] != "CONFLICTED" || body["blocks_calculation"] != true {
		t.Fatalf("body = %v", body)
	}
	reasons := map[string]string{}
	for _, e := range body["trace"].([]any) {
		m := e.(map[string]any)
		reasons[m["policy_version_id"].(string)] = m["reason"].(string)
	}
	if reasons["general"] != "overridden_by_more_specific_scope" || reasons["narrow"] != "conflicts_with_another_policy" ||
		reasons["by-book"] != "conflicts_with_another_policy" || reasons["never"] != "never_activated" {
		t.Errorf("reasons = %v", reasons)
	}

	// Lower precedence at equal specificity is its own reason.
	g = newAcctRig(&acctStub{candidates: []*domain.AccountingCandidate{
		cand("hi", "REVENUE_RECOGNITION", strPtr(bookGB), nil, nil, 5),
		cand("lo", "REVENUE_RECOGNITION", strPtr(bookGB), nil, nil, 1),
	}}, nil)
	body = expect(t, g.do(http.MethodGet, "/v1/accounting-policies/explain?policy_domain=REVENUE_RECOGNITION&book_id="+bookGB), http.StatusOK, "")
	for _, e := range body["trace"].([]any) {
		m := e.(map[string]any)
		if m["policy_version_id"] == "lo" && m["reason"] != "overridden_by_higher_precedence" {
			t.Errorf("reason = %v, want overridden_by_higher_precedence", m["reason"])
		}
		if m["policy_domain"] != "REVENUE_RECOGNITION" || m["semantic_version"] != "1.0.0" {
			t.Errorf("trace entry lacks its accounting context: %v", m)
		}
	}

	// Nothing to explain is still an answer, with an empty trace.
	g = newAcctRig(&acctStub{}, nil)
	body = expect(t, g.do(http.MethodGet, "/v1/accounting-policies/explain?policy_domain=LEASES"), http.StatusOK, "")
	if body["status"] != "NOT_APPLICABLE" || body["blocks_calculation"] != true {
		t.Errorf("body = %v", body)
	}
}

func TestListApplicableAccounting_OneEntryPerDomainConflictsIncluded(t *testing.T) {
	g := newAcctRig(&acctStub{candidates: []*domain.AccountingCandidate{
		cand("rev", "REVENUE_RECOGNITION", nil, nil, nil, 0),
		cand("lease-a", "LEASES", strPtr(bookGB), nil, nil, 0),
		cand("lease-b", "LEASES", nil, strPtr(jurisdiction), nil, 0),
		cand("fx", "FX_TRANSLATION", nil, nil, nil, 0),
	}}, nil)
	w := g.do(http.MethodGet, "/v1/accounting-policies?book_id="+bookGB+"&jurisdiction_id="+jurisdiction)
	body := expect(t, w, http.StatusOK, "")
	list, _ := body["policies"].([]any)
	got := map[string]string{}
	blocks := map[string]bool{}
	var order []string
	for _, e := range list {
		m := e.(map[string]any)
		d := m["policy_domain"].(string)
		order = append(order, d)
		got[d], blocks[d] = m["status"].(string), m["blocks_calculation"].(bool)
	}
	if strings.Join(order, ",") != "FX_TRANSLATION,LEASES,REVENUE_RECOGNITION" {
		t.Errorf("domains = %v, want one entry each in a stable order", order)
	}
	if got["REVENUE_RECOGNITION"] != "RESOLVED" || got["FX_TRANSLATION"] != "RESOLVED" || got["LEASES"] != "CONFLICTED" || !blocks["LEASES"] || blocks["FX_TRANSLATION"] {
		t.Errorf("statuses = %v blocks = %v: an ambiguous domain must be listed as such, not dropped", got, blocks)
	}
	if _, has := list[0].(map[string]any)["trace"]; has {
		t.Error("a list carries traces; explain is for that")
	}

	// One domain only, and none at all.
	st := &acctStub{}
	g = newAcctRig(st, nil)
	body = expect(t, g.do(http.MethodGet, "/v1/accounting-policies?policy_domain=LEASES"), http.StatusOK, "")
	if l, _ := body["policies"].([]any); len(l) != 0 || st.gotQuery.PolicyDomain != "LEASES" {
		t.Errorf("policies = %v, domain filter %q", body["policies"], st.gotQuery.PolicyDomain)
	}
}

// ── GetPolicyVersion ────────────────────────────────────────────────────────

func TestGetAccountingVersion(t *testing.T) {
	v := draftedVersion()
	v.PolicyCode = "ACCT:X:1"
	g := newAcctRig(&acctStub{version: v}, nil)
	w := g.do(http.MethodGet, "/v1/accounting-policies/versions/"+versionID)
	body := expect(t, w, http.StatusOK, "")
	if body["policy_code"] != "ACCT:X:1" || w.Header().Get("ETag") != `"1"` {
		t.Errorf("body = %v etag %q", body, w.Header().Get("ETag"))
	}
	expect(t, g.do(http.MethodGet, "/v1/accounting-policies/versions/not-a-uuid"), http.StatusBadRequest, "INVALID_REQUEST")
	g = newAcctRig(&acctStub{versionErr: domain.ErrPolicyVersionNotFound}, nil)
	expect(t, g.do(http.MethodGet, "/v1/accounting-policies/versions/"+versionID), http.StatusNotFound, "NOT_FOUND")
	g = newAcctRig(&acctStub{versionErr: domain.ErrStoreUnavailable}, nil)
	expect(t, g.do(http.MethodGet, "/v1/accounting-policies/versions/"+versionID), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
}

// A store without accounting support answers 503 rather than pretending.
func TestAccountingRoutesNeedAnAccountingStore(t *testing.T) {
	g := newRig(&stubStore{}, nil, nil)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/accounting-policies"},
		{http.MethodGet, "/v1/accounting-policies"},
		{http.MethodGet, resolvePath},
		{http.MethodGet, "/v1/accounting-policies/explain?policy_domain=X"},
		{http.MethodGet, "/v1/accounting-policies/versions/" + versionID},
	} {
		if w := g.do(c.method, c.path, `{}`); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: %d, want 503", c.method, c.path, w.Code)
		}
	}
}
