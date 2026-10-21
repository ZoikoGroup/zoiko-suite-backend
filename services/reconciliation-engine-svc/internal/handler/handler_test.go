package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/reconciliation-engine-svc/internal/authz"
	"zoiko.io/reconciliation-engine-svc/internal/domain"
	customMiddleware "zoiko.io/reconciliation-engine-svc/internal/middleware"
	"zoiko.io/reconciliation-engine-svc/internal/source"
	"zoiko.io/reconciliation-engine-svc/internal/store"
)

const (
	tenant  = "11111111-1111-1111-1111-111111111111"
	entityA = "aaaaaaaa-0000-0000-0000-00000000000a"
)

// recordingAuthz grants everything (except the actions in deny) and records
// the entity and action of each check. Decide answers with a traceable
// decision, unless untraceable is set.
type recordingAuthz struct {
	entities    []string
	actions     []string
	deny        map[string]bool
	untraceable bool
}

func (a *recordingAuthz) Decide(ctx context.Context, principalID, legalEntityID, action string) (*domain.AuthoritySnapshot, error) {
	if err := a.CheckAllowed(ctx, principalID, legalEntityID, action); err != nil {
		return nil, err
	}
	if a.untraceable {
		return nil, authz.ErrAuthorizationUnavailable
	}
	return &domain.AuthoritySnapshot{Source: "authorization-svc", PrincipalID: principalID, LegalEntityID: legalEntityID,
		ActionType: action, AccessDecisionID: "adl_1", DecisionOutcome: "GRANTED", DecisionBasis: "rbac:role=controller"}, nil
}

func (a *recordingAuthz) CheckAllowed(_ context.Context, _, legalEntityID, action string) error {
	a.entities = append(a.entities, legalEntityID)
	a.actions = append(a.actions, action)
	if a.deny[action] {
		return authz.ErrAuthorizationDenied
	}
	return nil
}

// fakeStore implements the handful of Store methods these tests reach; any
// other call panics through the nil embedded interface.
type fakeStore struct {
	store.Store
	def            *domain.ReconciliationDefinition
	run            *domain.ReconciliationRun
	version        *domain.RuleVersion
	startCalls     int
	reperformCalls int
	failures       []domain.RunFailure
	transitions    []domain.TransitionExceptionRequest
	approvals      []string
	certifiers     []string
	authorities    []*domain.AuthoritySnapshot
	revocations    []domain.RevokeCertificationRequest
	revokeErr      error
}

func (s *fakeStore) RevokeCertification(_ context.Context, _, runID string, req domain.RevokeCertificationRequest, actor string,
	authority *domain.AuthoritySnapshot, _ domain.IdempotencyClaim) (*domain.CertificationRevocation, error) {
	if s.revokeErr != nil {
		return nil, s.revokeErr
	}
	s.revocations = append(s.revocations, req)
	s.authorities = append(s.authorities, authority)
	return &domain.CertificationRevocation{RevocationID: "rcv_1", RunID: runID, Reason: req.Reason, EvidenceRefs: req.EvidenceRefs,
		Authority: *authority, RevokedBy: actor}, nil
}

func (s *fakeStore) Certify(_ context.Context, _, runID, actor string, authority *domain.AuthoritySnapshot,
	_ domain.IdempotencyClaim) (*domain.Certification, error) {
	s.certifiers = append(s.certifiers, actor)
	s.authorities = append(s.authorities, authority)
	return &domain.Certification{CertificationID: "rcf_1", RunID: runID, Authority: authority}, nil
}

func (s *fakeStore) ApproveRuleVersion(_ context.Context, _, definitionID string, version int64, approver string,
	_ domain.IdempotencyClaim) (*domain.RuleVersion, error) {
	s.approvals = append(s.approvals, approver)
	return &domain.RuleVersion{DefinitionID: definitionID, RuleVersion: version, Status: "APPROVED", ApprovedBy: &approver}, nil
}

func (s *fakeStore) GetException(context.Context, string, string) (*domain.ReconciliationException, error) {
	return &domain.ReconciliationException{ExceptionID: "rex_1", RunID: "rcr_1"}, nil
}
func (s *fakeStore) TransitionException(_ context.Context, _, _ string, req domain.TransitionExceptionRequest, _ string,
	_ domain.IdempotencyClaim) (*domain.ReconciliationException, error) {
	s.transitions = append(s.transitions, req)
	return &domain.ReconciliationException{ExceptionID: "rex_1", Status: req.ToState}, nil
}

func (s *fakeStore) GetDefinition(context.Context, string, string) (*domain.ReconciliationDefinition, error) {
	if s.def == nil {
		return nil, domain.ErrDefinitionNotFound
	}
	return s.def, nil
}
func (s *fakeStore) GetRun(context.Context, string, string) (*domain.ReconciliationRun, error) {
	if s.run == nil {
		return nil, domain.ErrRunNotFound
	}
	return s.run, nil
}
func (s *fakeStore) GetMatchResults(context.Context, string, string) ([]domain.MatchResult, error) {
	return nil, nil
}
func (s *fakeStore) CheckIdempotency(context.Context, string, domain.IdempotencyClaim) error {
	return nil
}
func (s *fakeStore) StartRun(_ context.Context, _ string, _ domain.StartRunRequest, _ int64, _ []domain.FrozenPopulation,
	_ string, _ domain.IdempotencyClaim) (*domain.ReconciliationRun, error) {
	s.startCalls++
	return &domain.ReconciliationRun{RunID: "rcr_x", LegalEntityID: s.def.LegalEntityID}, nil
}

func (s *fakeStore) RecordFailedRun(_ context.Context, _ string, _ domain.StartRunRequest, _ int64, f domain.RunFailure,
	_ string, _ domain.IdempotencyClaim) (*domain.ReconciliationRun, error) {
	s.failures = append(s.failures, f)
	code := f.Code
	return &domain.ReconciliationRun{RunID: "rcr_failed", LegalEntityID: s.def.LegalEntityID, Status: domain.RunFailed,
		ResultState: domain.ResultIndeterminate, CertificationState: domain.CertUncertified, FailureCode: &code}, nil
}

func (s *fakeStore) GetRuleVersion(context.Context, string, string, int64) (*domain.RuleVersion, error) {
	if s.version == nil {
		return nil, domain.ErrRuleVersionNotFound
	}
	return s.version, nil
}

func (s *fakeStore) Reperform(_ context.Context, _, _ string, _ []domain.FrozenPopulation, _ string,
	_ domain.IdempotencyClaim) (*domain.ReconciliationRun, error) {
	s.reperformCalls++
	return &domain.ReconciliationRun{RunID: "rcr_1"}, nil
}

type failingFetcher struct {
	err   error
	calls *int
}

func (f failingFetcher) Fetch(context.Context, domain.PopulationSpec, source.Scope) (*source.Fetched, error) {
	if f.calls != nil {
		*f.calls++
	}
	return nil, f.err
}

func definition() *domain.ReconciliationDefinition {
	return &domain.ReconciliationDefinition{DefinitionID: "rdf_1", LegalEntityID: entityA, AmountScale: 2, Version: 3,
		Populations: []domain.PopulationSpec{{Side: "A", System: "ar", Population: "p"}, {Side: "B", System: "gl", Population: "p"}},
		Materiality: &domain.MaterialityPolicy{}, ExceptionPolicy: &domain.ExceptionPolicy{OwnerRole: "o"},
		CertificationProfile: &domain.CertificationProfile{}}
}

func serve(h *Handler, method, path, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.With(customMiddleware.TenantMiddleware).Route("/v1/reconciliation", h.routes)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", "alice")
	req.Header.Set("Idempotency-Key", "k1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// A10: authorization is checked against the stored record's legal entity —
// never the tenant id, never a value the caller chose.
func TestAuthorization_UsesTheRecordsLegalEntity(t *testing.T) {
	az := &recordingAuthz{}
	st := &fakeStore{run: &domain.ReconciliationRun{RunID: "rcr_1", LegalEntityID: entityA}}
	h := NewHandler(st, az, failingFetcher{}, zap.NewNop())

	if w := serve(h, http.MethodGet, "/v1/reconciliation/runs/rcr_1", ""); w.Code != http.StatusOK {
		t.Fatalf("get run: %d %s", w.Code, w.Body)
	}
	if w := serve(h, http.MethodGet, "/v1/reconciliation/runs/rcr_1/matches", ""); w.Code != http.StatusOK {
		t.Fatalf("get matches: %d %s", w.Code, w.Body)
	}
	for _, e := range az.entities {
		if e != entityA {
			t.Fatalf("authorization made against %q, want the run's entity %q (all: %v)", e, entityA, az.entities)
		}
	}
	if len(az.entities) != 2 {
		t.Fatalf("authorization calls: %v", az.entities)
	}
}

// A record without an entity (written before migration 000004) is refused
// without any authorization call, rather than checked against a broader scope.
func TestAuthorization_LegacyRecordIsRefused(t *testing.T) {
	az := &recordingAuthz{}
	st := &fakeStore{run: &domain.ReconciliationRun{RunID: "rcr_1"}}
	h := NewHandler(st, az, failingFetcher{}, zap.NewNop())
	if w := serve(h, http.MethodGet, "/v1/reconciliation/runs/rcr_1", ""); w.Code != http.StatusConflict {
		t.Fatalf("legacy run: %d %s", w.Code, w.Body)
	}
	if len(az.entities) != 0 {
		t.Fatalf("a legacy record reached authorization: %v", az.entities)
	}
}

// A1/A6: StartRun authorizes against the definition's entity; a population
// that cannot be proven complete freezes nothing but is recorded as a Failed /
// INDETERMINATE run with the failure's code, returned with 502 or 422.
func TestStartRun_SourceFailureRecordsFailedRun(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code int
		want domain.FailureCode
	}{
		"source down":      {source.ErrSourceUnavailable, http.StatusBadGateway, domain.FailureSourceUnavailable},
		"no declared tots": {source.ErrSourceInconsistent, http.StatusUnprocessableEntity, domain.FailureSourceInconsistent},
		"unregistered":     {source.ErrSourceUnknown, http.StatusUnprocessableEntity, domain.FailureSourceUnregistered},
	} {
		az := &recordingAuthz{}
		st := &fakeStore{def: definition()}
		h := NewHandler(st, az, failingFetcher{err: tc.err}, zap.NewNop())
		w := serve(h, http.MethodPost, "/v1/reconciliation/runs", `{"definition_id":"rdf_1","period_id":"2026-09"}`)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), `"result_state":"INDETERMINATE"`) ||
			!strings.Contains(w.Body.String(), `"rcr_failed"`) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
		if st.startCalls != 0 {
			t.Fatalf("%s: populations were frozen for an unproven population", name)
		}
		if len(st.failures) != 1 || st.failures[0].Code != tc.want || st.failures[0].Reason == "" {
			t.Fatalf("%s: failed runs recorded %+v, want one with code %s", name, st.failures, tc.want)
		}
		if len(az.entities) != 1 || az.entities[0] != entityA {
			t.Fatalf("%s: StartRun authorized against %v, want the definition's entity", name, az.entities)
		}
	}
}

// A request error — a scope value the run does not carry — is the caller's,
// not a finding about the populations: 400, and no run of any kind.
func TestStartRun_RequestErrorRecordsNothing(t *testing.T) {
	def := definition()
	def.Populations[0].Params = map[string]string{"as_of": "${scope.as_of}"}
	st := &fakeStore{def: def}
	h := NewHandler(st, &recordingAuthz{}, failingFetcher{err: source.ErrSourceUnavailable}, zap.NewNop())
	w := serve(h, http.MethodPost, "/v1/reconciliation/runs", `{"definition_id":"rdf_1","period_id":"2026-09"}`)
	if w.Code != http.StatusBadRequest || st.startCalls != 0 || len(st.failures) != 0 {
		t.Fatalf("missing scope: %d %s, starts %d, failures %v", w.Code, w.Body, st.startCalls, st.failures)
	}
}

// A8: Reperform re-reads the run's populations through the same path StartRun
// uses. A re-read that cannot be proven complete and reproducible freezes
// nothing and fails with StartRun's classification (502 unreachable, 422
// inconsistent); the run itself is left exactly as it was — unlike StartRun,
// there is no attempt-record to write, because the run already exists.
func TestReperform_UnprovenSourceChangeIsRefused(t *testing.T) {
	run := &domain.ReconciliationRun{RunID: "rcr_1", LegalEntityID: entityA, DefinitionID: "rdf_1", DefinitionVersion: 3,
		Status: domain.RunExceptionsOpen, Tolerance: &domain.TolerancePolicy{}, RulesSHA256: strings.Repeat("a", 64)}
	for name, tc := range map[string]struct {
		err  error
		code int
	}{
		"source down":      {source.ErrSourceUnavailable, http.StatusBadGateway},
		"no declared tots": {source.ErrSourceInconsistent, http.StatusUnprocessableEntity},
	} {
		st := &fakeStore{run: run, version: &domain.RuleVersion{RuleVersion: 3,
			Rules: domain.RuleSet{Populations: definition().Populations, AmountScale: 2}}}
		h := NewHandler(st, &recordingAuthz{}, failingFetcher{err: tc.err}, zap.NewNop())
		w := serve(h, http.MethodPost, "/v1/reconciliation/runs/rcr_1:reperform", "")
		if w.Code != tc.code || st.reperformCalls != 0 || !strings.Contains(w.Body.String(), `"failure_code"`) {
			t.Fatalf("%s: %d %s, store reperform calls %d", name, w.Code, w.Body, st.reperformCalls)
		}
	}
}

// DefineReconciliation authorizes against the entity in the request only
// after it is validated as a UUID, so the tenant id cannot be smuggled in.
func TestDefine_RefusesNonEntityBeforeAuthorization(t *testing.T) {
	az := &recordingAuthz{}
	h := NewHandler(&fakeStore{}, az, failingFetcher{}, zap.NewNop())
	w := serve(h, http.MethodPost, "/v1/reconciliation/definitions", `{"legal_entity_id":"not-a-uuid","name":"n"}`)
	if w.Code != http.StatusBadRequest || len(az.entities) != 0 {
		t.Fatalf("define with a non-UUID entity: %d %s, authz calls %v", w.Code, w.Body, az.entities)
	}
}

// The pre-000005 tolerance_amount_minor_units is refused rather than silently
// dropped: dropping it would give the caller exact matching without saying so.
func TestDefine_RefusesUnknownFields(t *testing.T) {
	az := &recordingAuthz{}
	h := NewHandler(&fakeStore{}, az, failingFetcher{}, zap.NewNop())
	w := serve(h, http.MethodPost, "/v1/reconciliation/definitions",
		`{"legal_entity_id":"`+entityA+`","name":"n","tolerance_amount_minor_units":5}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "tolerance_amount_minor_units") || len(az.entities) != 0 {
		t.Fatalf("legacy tolerance field: %d %s, authz calls %v", w.Code, w.Body, az.entities)
	}
}

// DQC-05 / scenario 24: accepting risk is checked against its own permission,
// on the run's entity; without it the command is rejected and nothing changes.
func TestTransitionException_AcceptRiskNeedsItsOwnPermission(t *testing.T) {
	run := &domain.ReconciliationRun{RunID: "rcr_1", LegalEntityID: entityA}
	accept := `{"to_state":"ACCEPTED_RISK","reason":"immaterial","authority_ref":"controller-approval-7"}`

	az := &recordingAuthz{}
	st := &fakeStore{run: run}
	w := serve(NewHandler(st, az, failingFetcher{}, zap.NewNop()), http.MethodPost, "/v1/reconciliation/exceptions/rex_1:transition", accept)
	if w.Code != http.StatusOK || len(az.actions) != 1 || az.actions[0] != ActionReconciliationAcceptRisk || az.entities[0] != entityA {
		t.Fatalf("accept: %d %s, checks %v on %v", w.Code, w.Body, az.actions, az.entities)
	}

	az = &recordingAuthz{deny: map[string]bool{ActionReconciliationAcceptRisk: true}}
	st = &fakeStore{run: run}
	w = serve(NewHandler(st, az, failingFetcher{}, zap.NewNop()), http.MethodPost, "/v1/reconciliation/exceptions/rex_1:transition", accept)
	if w.Code != http.StatusForbidden || len(st.transitions) != 0 {
		t.Fatalf("accept without authority: %d %s, store calls %v", w.Code, w.Body, st.transitions)
	}

	az = &recordingAuthz{deny: map[string]bool{ActionReconciliationAcceptRisk: true}}
	st = &fakeStore{run: run}
	w = serve(NewHandler(st, az, failingFetcher{}, zap.NewNop()), http.MethodPost, "/v1/reconciliation/exceptions/rex_1:transition",
		`{"to_state":"INVESTIGATING","reason":"looking"}`)
	if w.Code != http.StatusOK || az.actions[0] != ActionReconciliationManage {
		t.Fatalf("investigate needs only MANAGE: %d %s, checks %v", w.Code, w.Body, az.actions)
	}
}

// A9 / Invariant 12: certifying is its own permission, checked on the run's
// entity; the permission that prepares a run is not enough to seal it.
func TestCertify_NeedsCertifyPermission(t *testing.T) {
	run := &domain.ReconciliationRun{RunID: "rcr_1", LegalEntityID: entityA}

	az := &recordingAuthz{}
	st := &fakeStore{run: run}
	w := serve(NewHandler(st, az, failingFetcher{}, zap.NewNop()), http.MethodPost, "/v1/reconciliation/runs/rcr_1:certify", "")
	if w.Code != http.StatusCreated || len(az.actions) != 1 || az.actions[0] != ActionReconciliationCertify || az.entities[0] != entityA ||
		len(st.certifiers) != 1 {
		t.Fatalf("certify: %d %s, checks %v on %v, certifiers %v", w.Code, w.Body, az.actions, az.entities, st.certifiers)
	}

	az = &recordingAuthz{deny: map[string]bool{ActionReconciliationCertify: true}}
	st = &fakeStore{run: run}
	w = serve(NewHandler(st, az, failingFetcher{}, zap.NewNop()), http.MethodPost, "/v1/reconciliation/runs/rcr_1:certify", "")
	if w.Code != http.StatusForbidden || len(st.certifiers) != 0 {
		t.Fatalf("certify with only MANAGE: %d %s, certifiers %v", w.Code, w.Body, st.certifiers)
	}
}

// A12: the certification is handed the certifier's own live decision, for
// this action on the run's entity; a grant that cannot be traced certifies
// nothing.
func TestCertify_RecordsTheCertifiersDecision(t *testing.T) {
	run := &domain.ReconciliationRun{RunID: "rcr_1", LegalEntityID: entityA}

	st := &fakeStore{run: run}
	w := serve(NewHandler(st, &recordingAuthz{}, failingFetcher{}, zap.NewNop()), http.MethodPost, "/v1/reconciliation/runs/rcr_1:certify", "")
	if w.Code != http.StatusCreated || len(st.authorities) != 1 {
		t.Fatalf("certify: %d %s, authorities %v", w.Code, w.Body, st.authorities)
	}
	a := st.authorities[0]
	if a.AccessDecisionID != "adl_1" || a.PrincipalID != "alice" || a.LegalEntityID != entityA || a.ActionType != ActionReconciliationCertify {
		t.Fatalf("authority handed to the store: %+v", a)
	}

	st = &fakeStore{run: run}
	w = serve(NewHandler(st, &recordingAuthz{untraceable: true}, failingFetcher{}, zap.NewNop()), http.MethodPost,
		"/v1/reconciliation/runs/rcr_1:certify", "")
	if w.Code != http.StatusServiceUnavailable || len(st.certifiers) != 0 {
		t.Fatalf("untraceable grant: %d %s, certifiers %v", w.Code, w.Body, st.certifiers)
	}
}

const revokeBody = `{"reason":"bank statement restated after certification","evidence_refs":["bank-restatement-2026-10"]}`

// B34: revoking is its own permission, checked on the run's entity through
// Decide, and the store is handed the revoker's own decision for it; the
// permission to certify is not enough.
func TestRevokeCertification_NeedsRevokePermissionAndRecordsTheDecision(t *testing.T) {
	run := &domain.ReconciliationRun{RunID: "rcr_1", LegalEntityID: entityA, Status: domain.RunCertified,
		CertificationState: domain.CertCertified}
	const path = "/v1/reconciliation/runs/rcr_1:revoke-certification"

	az := &recordingAuthz{}
	st := &fakeStore{run: run}
	w := serve(NewHandler(st, az, failingFetcher{}, zap.NewNop()), http.MethodPost, path, revokeBody)
	if w.Code != http.StatusCreated || len(az.actions) != 1 || az.actions[0] != ActionReconciliationRevoke || az.entities[0] != entityA ||
		len(st.revocations) != 1 || st.revocations[0].EvidenceRefs[0] != "bank-restatement-2026-10" {
		t.Fatalf("revoke: %d %s, checks %v on %v, revocations %v", w.Code, w.Body, az.actions, az.entities, st.revocations)
	}
	if a := st.authorities[0]; a.AccessDecisionID != "adl_1" || a.PrincipalID != "alice" || a.ActionType != ActionReconciliationRevoke {
		t.Fatalf("authority handed to the store: %+v", a)
	}

	az = &recordingAuthz{deny: map[string]bool{ActionReconciliationRevoke: true}}
	st = &fakeStore{run: run}
	w = serve(NewHandler(st, az, failingFetcher{}, zap.NewNop()), http.MethodPost, path, revokeBody)
	if w.Code != http.StatusForbidden || len(st.revocations) != 0 {
		t.Fatalf("revoke with only CERTIFY: %d %s, revocations %v", w.Code, w.Body, st.revocations)
	}

	st = &fakeStore{run: run}
	w = serve(NewHandler(st, &recordingAuthz{untraceable: true}, failingFetcher{}, zap.NewNop()), http.MethodPost, path, revokeBody)
	if w.Code != http.StatusServiceUnavailable || len(st.revocations) != 0 {
		t.Fatalf("untraceable grant: %d %s, revocations %v", w.Code, w.Body, st.revocations)
	}

	// The store's independence refusal is a 403.
	st = &fakeStore{run: run, revokeErr: domain.ErrRevokerIsCertifier}
	w = serve(NewHandler(st, &recordingAuthz{}, failingFetcher{}, zap.NewNop()), http.MethodPost, path, revokeBody)
	if w.Code != http.StatusForbidden {
		t.Fatalf("certifier revokes: %d %s", w.Code, w.Body)
	}
}

// NP-47: a revocation with no reason or evidence is refused before anything
// is authorized or written, and so is an unknown field.
func TestRevokeCertification_RequiresReasonAndEvidence(t *testing.T) {
	run := &domain.ReconciliationRun{RunID: "rcr_1", LegalEntityID: entityA}
	for name, body := range map[string]string{
		"empty body":    ``,
		"no reason":     `{"evidence_refs":["x"]}`,
		"no evidence":   `{"reason":"restated"}`,
		"blank ref":     `{"reason":"restated","evidence_refs":[" "]}`,
		"unknown field": `{"reason":"restated","evidence_ref":"x"}`,
	} {
		az := &recordingAuthz{}
		st := &fakeStore{run: run}
		w := serve(NewHandler(st, az, failingFetcher{}, zap.NewNop()), http.MethodPost,
			"/v1/reconciliation/runs/rcr_1:revoke-certification", body)
		if w.Code != http.StatusBadRequest || len(az.actions) != 0 || len(st.revocations) != 0 {
			t.Errorf("%s: %d %s, checks %v, revocations %v", name, w.Code, w.Body, az.actions, st.revocations)
		}
	}
}

// A11: approving a rule version is its own permission, checked on the
// definition's entity at the moment of approval; publishing is not enough.
func TestApproveRuleVersion_NeedsApprovalPermission(t *testing.T) {
	az := &recordingAuthz{}
	st := &fakeStore{def: definition()}
	w := serve(NewHandler(st, az, failingFetcher{}, zap.NewNop()), http.MethodPost, "/v1/reconciliation/definitions/rdf_1/rule-versions/2/approve", "")
	if w.Code != http.StatusOK || len(az.actions) != 1 || az.actions[0] != ActionReconciliationApproveRules || az.entities[0] != entityA ||
		len(st.approvals) != 1 || st.approvals[0] != "alice" {
		t.Fatalf("approve: %d %s, checks %v on %v, approvals %v", w.Code, w.Body, az.actions, az.entities, st.approvals)
	}

	az = &recordingAuthz{deny: map[string]bool{ActionReconciliationApproveRules: true}}
	st = &fakeStore{def: definition()}
	w = serve(NewHandler(st, az, failingFetcher{}, zap.NewNop()), http.MethodPost, "/v1/reconciliation/definitions/rdf_1/rule-versions/2/approve", "")
	if w.Code != http.StatusForbidden || len(st.approvals) != 0 {
		t.Fatalf("approve without permission: %d %s, approvals %v", w.Code, w.Body, st.approvals)
	}

	w = serve(NewHandler(&fakeStore{def: definition()}, &recordingAuthz{}, failingFetcher{}, zap.NewNop()), http.MethodPost,
		"/v1/reconciliation/definitions/rdf_1/rule-versions/zero/approve", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad version: %d", w.Code)
	}
}

func TestGetDefinition_AuthorizesReadOnItsEntity(t *testing.T) {
	az := &recordingAuthz{}
	w := serve(NewHandler(&fakeStore{def: definition()}, az, failingFetcher{}, zap.NewNop()), http.MethodGet, "/v1/reconciliation/definitions/rdf_1", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"definition_id":"rdf_1"`) ||
		len(az.actions) != 1 || az.actions[0] != ActionReconciliationRead || az.entities[0] != entityA {
		t.Fatalf("get definition: %d %s, checks %v on %v", w.Code, w.Body, az.actions, az.entities)
	}
}

// A definition with no approved rule version in effect starts nothing and
// reads no source.
func TestStartRun_RefusesUnapprovedRulesBeforeReadingSources(t *testing.T) {
	def := definition()
	def.Version, def.Materiality, def.ExceptionPolicy, def.CertificationProfile = 0, nil, nil, nil
	calls := 0
	st := &fakeStore{def: def}
	w := serve(NewHandler(st, &recordingAuthz{}, failingFetcher{err: source.ErrSourceUnavailable, calls: &calls}, zap.NewNop()),
		http.MethodPost, "/v1/reconciliation/runs", `{"definition_id":"rdf_1","period_id":"2026-09"}`)
	if w.Code != http.StatusConflict || calls != 0 || st.startCalls != 0 || len(st.failures) != 0 {
		t.Fatalf("unapproved rules: %d %s, source reads %d, starts %d, failed runs %d", w.Code, w.Body, calls, st.startCalls, len(st.failures))
	}
}
