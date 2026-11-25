package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	"zoiko.io/identity-envelope/govdecision"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/handler"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
	"zoiko.io/workflow-svc/internal/signatory"
)

const (
	sigTenant   = "11111111-1111-4111-8111-111111111111"
	sigEntity   = "22222222-2222-4222-8222-222222222222"
	sigDecision = "33333333-3333-4333-8333-333333333333"
	sigHash     = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

type stubVerifier struct {
	d   *govdecision.Decision
	err error
	got []signatory.Request
}

func (v *stubVerifier) Verify(_ context.Context, r signatory.Request) (*govdecision.Decision, error) {
	v.got = append(v.got, r)
	return v.d, v.err
}

func sigDecisionFixture(outcome string) *govdecision.Decision {
	return &govdecision.Decision{
		ContractVersion: "1.0", DecisionID: sigDecision, Outcome: outcome, OwningService: "policy-svc",
		PolicyFamily: "SIGNATORY_MATRIX", RuleID: "pv#band:B2", PolicyVersionID: "44444444-4444-4444-8444-444444444444",
		TenantID: sigTenant, LegalEntityID: sigEntity, ActorPrincipalID: "preparer-1", Action: "SIGN_CONTRACT",
		Resource: govdecision.Resource{Type: "contract", ID: "C-1", Version: "3"}, EvaluatedAt: time.Now().UTC(),
		ValidUntil: time.Now().UTC().Add(time.Minute), ReasonCodes: []string{"SIGNATURE_QUORUM_INCOMPLETE"},
		RequiredActions: []govdecision.RequiredAction{{Kind: govdecision.ActionKindSignatureQuorum, MinDistinctSigners: 2,
			RequiredOffices: []govdecision.OfficeRequirement{{Office: "DIRECTOR", MinCount: 1}},
			EligibleOffices: []string{"DIRECTOR", "FINANCE_SIGNER"}, DocumentHash: sigHash}},
	}
}

// sigStore adds the signatory reads and records the quorum vote.
type sigStore struct {
	*stubStore
	binding *domain.SignatoryBinding
	signers []domain.Signer
	vote    domain.SubmitQuorumVoteParams
}

func (s *sigStore) FindSignatoryBinding(context.Context, string) (*domain.SignatoryBinding, error) {
	return s.binding, nil
}
func (s *sigStore) SignaturesFor(context.Context, string) ([]domain.Signer, error) {
	return s.signers, nil
}
func (s *sigStore) SubmitQuorumVote(ctx context.Context, p domain.SubmitQuorumVoteParams) (*domain.WorkflowInstance, *domain.WorkflowStage, bool, error) {
	s.vote = p
	return s.stubStore.SubmitQuorumVote(ctx, p)
}

// officeAuthz grants the signer offices in held (by action) and approval
// authority to everyone.
type officeAuthz struct {
	stubAuthz
	held map[string]bool
}

func (a *officeAuthz) CheckAllowed(_ context.Context, _, _, action, _ string) error {
	a.asked = append(a.asked, action)
	if strings.HasPrefix(action, domain.SignatoryActionPrefix) && !a.held[action] {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func sigRouter(s *sigStore, az *officeAuthz, v *stubVerifier) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, &stubPublisher{}, az, &stubDocuments{}, zap.NewNop())
	if v != nil {
		h = h.WithSignatoryVerifier(v)
	}
	handler.RegisterRoutes(r, h)
	return r
}

func sigCreate(t *testing.T, r chi.Router, stages string, extra string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"tenant_id":"` + sigTenant + `","legal_entity_id":"` + sigEntity + `","workflow_type":"CONTRACT_SIGNATURE",` +
		`"signatory_decision_id":"` + sigDecision + `"` + extra + `,"stages":` + stages + `}`
	req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/workflows", bytes.NewBufferString(body)), "requester-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

const twoSigners = `[{"stage_type":"QUORUM","required_approvals":2,"quorum_approvers":["signer-a","signer-b","signer-c"]}]`

func newSigStore() *sigStore {
	return &sigStore{stubStore: &stubStore{
		instance: &domain.WorkflowInstance{WorkflowInstanceID: testWorkflowID, WorkflowStatus: "PENDING", RowVersion: 1},
		stages:   []*domain.WorkflowStage{{WorkflowStageID: "s-1", StageOrder: 1}},
	}}
}

// Doc 02 §4.2: a signature workflow is created only from a verified
// SIGNATURE_REQUIRED decision, bound to its resource and document.
func TestSignatureWorkflow_CreatedFromVerifiedDecision(t *testing.T) {
	s := newSigStore()
	v := &stubVerifier{d: sigDecisionFixture(govdecision.SignatureRequired)}
	w := sigCreate(t, sigRouter(s, &officeAuthz{}, v), twoSigners, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	p := s.createdParams
	if p.Signatory == nil || p.Signatory.DecisionID != sigDecision || p.Signatory.DocumentHash != sigHash ||
		p.SubjectType == nil || *p.SubjectType != "contract" || *p.SubjectID != "C-1" || *p.SubjectVersion != 3 || *p.SubjectFingerprint != sigHash {
		t.Fatalf("binding %+v subject %v %v %v %v", p.Signatory, p.SubjectType, p.SubjectID, p.SubjectVersion, p.SubjectFingerprint)
	}
	if got := v.got[0]; got.TenantID != sigTenant || got.LegalEntityID != sigEntity || got.DecisionID != sigDecision || !got.RequireValid || got.PrincipalID != "requester-1" {
		t.Fatalf("verifier asked %+v", got)
	}
	var req domain.SignatureRequirement
	if json.Unmarshal(p.Signatory.Requirement, &req) != nil || req.MinDistinctSigners != 2 {
		t.Fatalf("requirement %s", p.Signatory.Requirement)
	}
	// DL-2: the signatory decision is the governance decision the workflow
	// resolves when the caller names no other.
	if p.CausationDecisionID != sigDecision {
		t.Fatalf("causation = %q, want the signatory decision %q", p.CausationDecisionID, sigDecision)
	}
}

func TestSignatureWorkflow_RefusedUnlessVerifiedAndSufficient(t *testing.T) {
	cases := map[string]struct {
		verifier *stubVerifier
		stages   string
		extra    string
		status   int
		code     string
	}{
		"no verifier":      {nil, twoSigners, "", http.StatusServiceUnavailable, "signatory_verifier_unavailable"},
		"policy-svc down":  {&stubVerifier{err: signatory.ErrUnavailable}, twoSigners, "", http.StatusServiceUnavailable, "policy_service_unavailable"},
		"not found":        {&stubVerifier{err: signatory.ErrNotFound}, twoSigners, "", http.StatusUnprocessableEntity, "signatory_decision_not_found"},
		"untrusted":        {&stubVerifier{err: &signatory.UntrustedError{Problems: []string{"signature"}}}, twoSigners, "", http.StatusUnprocessableEntity, "signatory_decision_untrusted"},
		"not configured":   {&stubVerifier{d: sigDecisionFixture(govdecision.NotConfigured)}, twoSigners, "", http.StatusUnprocessableEntity, "signatory_decision_not_actionable"},
		"exempt":           {&stubVerifier{d: sigDecisionFixture(govdecision.Permit)}, twoSigners, "", http.StatusUnprocessableEntity, "signatory_decision_not_actionable"},
		"quorum too small": {&stubVerifier{d: sigDecisionFixture(govdecision.SignatureRequired)}, `[{"stage_type":"QUORUM","required_approvals":1,"quorum_approvers":["signer-a","signer-b"]}]`, "", http.StatusUnprocessableEntity, "signature_quorum_insufficient"},
		"no quorum stage":  {&stubVerifier{d: sigDecisionFixture(govdecision.SignatureRequired)}, `[{"approver_principal_id":"signer-a"}]`, "", http.StatusUnprocessableEntity, "signature_quorum_stage_required"},
		"preparer signs":   {&stubVerifier{d: sigDecisionFixture(govdecision.SignatureRequired)}, `[{"stage_type":"QUORUM","required_approvals":2,"quorum_approvers":["signer-a","preparer-1"]}]`, "", http.StatusBadRequest, "preparer_cannot_sign"},
		"other document":   {&stubVerifier{d: sigDecisionFixture(govdecision.SignatureRequired)}, twoSigners, `,"subject_fingerprint":"sha256:` + strings.Repeat("f", 64) + `"`, http.StatusConflict, "signatory_subject_mismatch"},
		"other version":    {&stubVerifier{d: sigDecisionFixture(govdecision.SignatureRequired)}, twoSigners, `,"subject_version":4`, http.StatusConflict, "signatory_subject_mismatch"},
		"not a uuid":       {&stubVerifier{}, twoSigners, "", 0, ""},
	}
	for name, c := range cases {
		s := newSigStore()
		var v *stubVerifier
		if c.verifier != nil {
			v = c.verifier
		}
		r := chi.NewRouter()
		r.Use(svcmiddleware.TenantContext())
		h := handler.New(s, &stubPublisher{}, &officeAuthz{}, &stubDocuments{}, zap.NewNop())
		if v != nil {
			h = h.WithSignatoryVerifier(v)
		}
		handler.RegisterRoutes(r, h)
		if name == "not a uuid" {
			body := `{"tenant_id":"` + sigTenant + `","legal_entity_id":"` + sigEntity + `","workflow_type":"X","signatory_decision_id":"nope","stages":` + twoSigners + `}`
			req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/workflows", bytes.NewBufferString(body)), "requester-1")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: %d", name, w.Code)
			}
			continue
		}
		w := sigCreate(t, r, c.stages, c.extra)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.code) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		if s.createdParams.WorkflowType != "" {
			t.Fatalf("%s: the workflow was created anyway", name)
		}
	}
}

func boundStore() *sigStore {
	s := newSigStore()
	s.findInstance = &domain.WorkflowInstance{WorkflowInstanceID: testWorkflowID, TenantID: sigTenant, LegalEntityID: sigEntity,
		InitiatedBy: "requester-1", WorkflowStatus: "PENDING", RowVersion: 1}
	s.currentStage = &domain.WorkflowStage{WorkflowStageID: "s-1", StageOrder: 1, StageType: domain.StageTypeQuorum}
	s.submitInstance = s.findInstance
	s.submitStage = s.currentStage
	s.submitTransitioned = true
	req, _ := json.Marshal(sigDecisionFixture(govdecision.SignatureRequired).RequiredActions[0])
	s.binding = &domain.SignatoryBinding{WorkflowInstanceID: testWorkflowID, DecisionID: sigDecision, DocumentHash: sigHash, Requirement: req}
	return s
}

func vote(t *testing.T, r chi.Router, principal, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/workflows/"+testWorkflowID+"/quorum-votes", bytes.NewBufferString(body)), principal)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Doc 02 §4.2: Authorization says which offices a signer holds at the moment
// of signing; one with none of the accepted offices cannot sign.
func TestSignatureVote_OfficesFromAuthorizationAtSigning(t *testing.T) {
	s := boundStore()
	az := &officeAuthz{held: map[string]bool{"SIGNATORY_SIGN_AS_DIRECTOR": true}}
	w := vote(t, sigRouter(s, az, &stubVerifier{}), "signer-a", `{"action":"APPROVE","document_hash":"`+sigHash+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(s.vote.SignerOffices) != 1 || s.vote.SignerOffices[0] != "DIRECTOR" || s.vote.DocumentHash != sigHash {
		t.Fatalf("vote %+v", s.vote)
	}
	for _, want := range []string{"SIGNATORY_SIGN_AS_DIRECTOR", "SIGNATORY_SIGN_AS_FINANCE_SIGNER"} {
		found := false
		for _, a := range az.asked {
			found = found || a == want
		}
		if !found {
			t.Fatalf("authorization not asked %s: %v", want, az.asked)
		}
	}

	s = boundStore()
	w = vote(t, sigRouter(s, &officeAuthz{}, &stubVerifier{}), "signer-b", `{"action":"APPROVE","document_hash":"`+sigHash+`"}`)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "signer_not_eligible") || s.vote.ActorPrincipalID != "" {
		t.Fatalf("an officeless signer must be refused before the store: %d %s", w.Code, w.Body.String())
	}

	// The store's refusals map to the contract's codes.
	for err, code := range map[error]string{
		domain.ErrSignatureDocumentChanged: "signature_document_changed",
		domain.ErrSignatureOutOfOrder:      "signature_out_of_order",
	} {
		s = boundStore()
		s.submitErr = err
		w = vote(t, sigRouter(s, az, &stubVerifier{}), "signer-a", `{"action":"APPROVE","document_hash":"`+sigHash+`"}`)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), code) {
			t.Fatalf("%v: %d %s", err, w.Code, w.Body.String())
		}
	}
}

// Doc 02 §8.4: release re-checks the requirement in force now against the
// signatures given.
func TestSignatureRelease_RechecksCurrentRequirement(t *testing.T) {
	approved := func() *sigStore {
		s := boundStore()
		v := 3
		typ, id := "contract", "C-1"
		s.findInstance.WorkflowStatus = "APPROVED"
		s.findInstance.SubjectType, s.findInstance.SubjectID, s.findInstance.SubjectVersion = &typ, &id, &v
		s.verifyReleaseResult = &domain.ReleaseVerificationResult{WorkflowInstanceID: testWorkflowID, CanRelease: true, Status: "VALID"}
		return s
	}
	release := func(s *sigStore, v *stubVerifier, body string) *httptest.ResponseRecorder {
		req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/workflows/"+testWorkflowID+"/verify-release", bytes.NewBufferString(body)), "releaser-1")
		w := httptest.NewRecorder()
		sigRouter(s, &officeAuthz{}, v).ServeHTTP(w, req)
		return w
	}
	body := `{"current_subject_fingerprint":"` + sigHash + `","current_signatory_decision_id":"` + sigDecision + `"}`

	s := approved()
	s.signers = []domain.Signer{{PrincipalID: "signer-a", Offices: []string{"DIRECTOR"}}, {PrincipalID: "signer-b", Offices: []string{"FINANCE_SIGNER"}}}
	v := &stubVerifier{d: sigDecisionFixture(govdecision.SignatureRequired)}
	if w := release(s, v, body); w.Code != http.StatusOK {
		t.Fatalf("met requirement must release: %d %s", w.Code, w.Body.String())
	}
	if got := v.got[0]; got.Resource.Version != "3" || got.Resource.ID != "C-1" {
		t.Fatalf("release re-check must pin the workflow subject: %+v", got)
	}

	// The requirement tightened (now a CFO is needed): no release.
	s = approved()
	s.signers = []domain.Signer{{PrincipalID: "signer-a", Offices: []string{"DIRECTOR"}}, {PrincipalID: "signer-b", Offices: []string{"FINANCE_SIGNER"}}}
	tight := sigDecisionFixture(govdecision.SignatureRequired)
	tight.RequiredActions[0].RequiredOffices = append(tight.RequiredActions[0].RequiredOffices, govdecision.OfficeRequirement{Office: "CFO", MinCount: 1})
	if w := release(s, &stubVerifier{d: tight}, body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not met") {
		t.Fatalf("tightened requirement must hold release: %d %s", w.Code, w.Body.String())
	}
	// The policy is gone (NOT_CONFIGURED): no release.
	if w := release(approved(), &stubVerifier{d: sigDecisionFixture(govdecision.NotConfigured)}, body); w.Code != http.StatusConflict {
		t.Fatalf("NOT_CONFIGURED must hold release: %d", w.Code)
	}
	// Without a fresh decision: refused.
	if w := release(approved(), &stubVerifier{}, `{"current_subject_fingerprint":"`+sigHash+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing fresh decision: %d", w.Code)
	}
	// policy-svc down: fail closed.
	if w := release(approved(), &stubVerifier{err: signatory.ErrUnavailable}, body); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("policy-svc down: %d", w.Code)
	}
}
