package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/privacy-transfer-svc/internal/domain"
	"zoiko.io/privacy-transfer-svc/internal/handler"
	"zoiko.io/privacy-transfer-svc/internal/middleware"
	"zoiko.io/privacy-transfer-svc/internal/store"
	"zoiko.io/privacy-transfer-svc/internal/testdb"
)

// EvaluateTransfer and the §17.1 trigger endpoint share computeTriggers AND the
// stored evidence it measures (latest assessment, subprocessors, mechanism).
// assessment_required decides only whether the assessment GATE is enforced; it
// must not decide which triggers are visible. These tests pin that, including
// the scenario that used to disagree: an approved assessment followed by a newly
// attached subprocessor, evaluated with assessment_required=false.

type triggerEnv struct {
	r    http.Handler
	rel  *domain.ProcessorRelationship
	mech *domain.TransferMechanism
}

func tcPause() { time.Sleep(3 * time.Millisecond) } // created_at ordering is what triggers 3 and 4 compare

func tcRecord(t *testing.T, r http.Handler, rel string, outcome domain.AssessmentOutcome, reviewAt *time.Time, mut ...func(*domain.RecordTransferAssessmentRequest)) {
	t.Helper()
	req := domain.RecordTransferAssessmentRequest{RelationshipID: rel, Outcome: outcome, ReviewTriggerAt: reviewAt}
	for _, m := range mut {
		m(&req)
	}
	if w := doRequest(r, http.MethodPost, "/privacy/transfer-assessments", req, testTenant); w.Code != http.StatusCreated {
		t.Fatalf("record assessment: %d %s", w.Code, w.Body.String())
	}
}

func tcAttachSub(t *testing.T, r http.Handler, rel string) {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/processor-relationships/"+rel+"/subprocessors", domain.AttachSubprocessorRequest{
		ProviderIdentity: "sub-1", Service: "storage", NotificationApprovalModel: "PRIOR_NOTICE",
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("attach subprocessor: %d %s", w.Code, w.Body.String())
	}
}

func tcEvaluate(t *testing.T, r http.Handler, env *triggerEnv, required bool, mut ...func(*domain.EvaluateTransferRequest)) domain.TransferDecision {
	t.Helper()
	req := domain.EvaluateTransferRequest{RelationshipID: env.rel.RelationshipID, TransferMechanismID: env.mech.MechanismID, AssessmentRequired: required}
	for _, m := range mut {
		m(&req)
	}
	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", req, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("evaluate: %d %s", w.Code, w.Body.String())
	}
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	return d
}

// triggers asks the trigger endpoint (POST body and, where the inputs fit the
// query string, GET) the same question and requires both to agree.
func tcTriggers(t *testing.T, r http.Handler, env *triggerEnv, mut ...func(*domain.EvaluateTriggersRequest)) domain.EvaluateTriggersResponse {
	t.Helper()
	req := domain.EvaluateTriggersRequest{RelationshipID: env.rel.RelationshipID, TransferMechanismID: env.mech.MechanismID}
	for _, m := range mut {
		m(&req)
	}
	w := doRequest(r, http.MethodPost, "/privacy/transfer-assessments/evaluate-triggers", req, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("evaluate-triggers: %d %s", w.Code, w.Body.String())
	}
	var resp domain.EvaluateTriggersResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if len(req.DataCategories) == 0 && len(req.SubjectClasses) == 0 {
		q := "?relationship_id=" + req.RelationshipID + "&transfer_mechanism_id=" + req.TransferMechanismID
		if req.DestinationJurisdiction != "" {
			q += "&destination_jurisdiction=" + req.DestinationJurisdiction
		}
		wg := doRequest(r, http.MethodGet, "/privacy/transfer-assessments/triggers"+q, nil, testTenant)
		var respGet domain.EvaluateTriggersResponse
		_ = json.Unmarshal(wg.Body.Bytes(), &respGet)
		if wg.Code != http.StatusOK || respGet.ReassessmentNeeded != resp.ReassessmentNeeded || strings.Join(respGet.ActiveTriggers, ",") != strings.Join(resp.ActiveTriggers, ",") {
			t.Fatalf("GET and POST trigger endpoints disagree: %v vs %v", respGet, resp)
		}
	}
	return resp
}

func tcHasTrigger(resp domain.EvaluateTriggersResponse, trigger string) bool {
	for _, a := range resp.ActiveTriggers {
		if a == trigger {
			return true
		}
	}
	return false
}

func tcReasonsContain(d domain.TransferDecision, sub string) bool {
	for _, c := range d.ReasonCodes {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func tcNewEnv(t *testing.T, r http.Handler, jurisdictions []string) *triggerEnv {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/processor-relationships", domain.CreateProcessorRelationshipRequest{
		ControllerRef: "controller-1", ProcessorRef: "processor-1", Service: "hosting",
		ProcessingInstructions: "host data only as instructed", Jurisdictions: jurisdictions,
		DataCategories: []string{"CONTACT_INFO"}, SubjectClasses: []string{"CUSTOMER"},
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("create relationship: %d %s", w.Code, w.Body.String())
	}
	var rel domain.ProcessorRelationship
	_ = json.Unmarshal(w.Body.Bytes(), &rel)
	tcPause()
	return &triggerEnv{r: r, rel: &rel, mech: createValidMechanism(t, r)}
}

// The scenario the audit reproduced: approved assessment, then a new subprocessor.
func TestTriggerConsistency_NewSubprocessorAfterAssessment_BothPathsAgree(t *testing.T) {
	for _, required := range []bool{false, true} {
		r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
		env := tcNewEnv(t, r, nil)
		tcPause()
		tcRecord(t, r, env.rel.RelationshipID, domain.AssessmentApprove, nil)
		tcPause()
		tcAttachSub(t, r, env.rel.RelationshipID)

		tr := tcTriggers(t, r, env)
		if !tr.ReassessmentNeeded || !tcHasTrigger(tr, domain.TriggerNewProcessorOrJurisdiction) {
			t.Fatalf("required=%v: the trigger endpoint must report the new-subprocessor trigger, got %+v", required, tr)
		}
		d := tcEvaluate(t, r, env, required)
		if d.Result != domain.ResultReviewRequired || !tcReasonsContain(d, "REASSESSMENT_TRIGGER_NEW_SUBPROCESSOR") {
			t.Fatalf("required=%v: EvaluateTransfer must agree with the trigger endpoint, got %s %v", required, d.Result, d.ReasonCodes)
		}
	}
}

// Property: for the same inputs and stored evidence, EvaluateTransfer is
// REVIEW_REQUIRED because of a trigger exactly when the trigger endpoint
// reports reassessment needed — whatever assessment_required says.
func TestTriggerConsistency_EvaluateAndEndpointNeverContradict(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(24 * time.Hour)
	cases := []struct {
		name      string
		setup     func(t *testing.T, r http.Handler, env *triggerEnv)
		eval      func(*domain.EvaluateTransferRequest)
		trig      func(*domain.EvaluateTriggersRequest)
		wantTrig  bool
		wantToken string // fragment of the evaluate reason when wantTrig
	}{
		{"clean: assessment on file, nothing changed", func(t *testing.T, r http.Handler, e *triggerEnv) {
			tcRecord(t, r, e.rel.RelationshipID, domain.AssessmentApprove, &future)
		}, nil, nil, false, ""},
		{"no assessment on file", func(t *testing.T, r http.Handler, e *triggerEnv) {}, nil, nil, false, ""},
		{"new subprocessor after the assessment", func(t *testing.T, r http.Handler, e *triggerEnv) {
			tcRecord(t, r, e.rel.RelationshipID, domain.AssessmentApprove, &future)
			tcPause()
			tcAttachSub(t, r, e.rel.RelationshipID)
		}, nil, nil, true, "NEW_SUBPROCESSOR"},
		{"mechanism recorded after the assessment", func(t *testing.T, r http.Handler, e *triggerEnv) {
			// assessment on file BEFORE the mechanism: re-point env.mech to a newer mechanism
			tcRecord(t, r, e.rel.RelationshipID, domain.AssessmentApprove, &future)
			tcPause()
			e.mech = createValidMechanism(t, r)
		}, nil, nil, true, "CHANGED_TRANSFER_MECHANISM"},
		{"assessment past its review date", func(t *testing.T, r http.Handler, e *triggerEnv) {
			tcRecord(t, r, e.rel.RelationshipID, domain.AssessmentApprove, &past)
		}, nil, nil, true, "ASSESSMENT_EXPIRED"},
		{"destination outside the relationship's jurisdictions", func(t *testing.T, r http.Handler, e *triggerEnv) {
			tcRecord(t, r, e.rel.RelationshipID, domain.AssessmentApprove, &future) // so only the trigger, not the missing-assessment gate, decides
		},
			func(q *domain.EvaluateTransferRequest) { q.DestinationJurisdiction = "AU-PRIVACY" },
			func(q *domain.EvaluateTriggersRequest) { q.DestinationJurisdiction = "AU-PRIVACY" }, true, "NEW_DESTINATION_JURISDICTION"},
		{"minors subject class the relationship does not cover", func(t *testing.T, r http.Handler, e *triggerEnv) {
			tcRecord(t, r, e.rel.RelationshipID, domain.AssessmentApprove, &future) // so only the trigger, not the missing-assessment gate, decides
		},
			func(q *domain.EvaluateTransferRequest) { q.SubjectClasses = []string{"MINOR"} },
			func(q *domain.EvaluateTriggersRequest) { q.SubjectClasses = []string{"MINOR"} }, true, "NEW_MINORS_CONTEXT"},
		{"sensitive category the relationship was not registered for", func(t *testing.T, r http.Handler, e *triggerEnv) {
			tcRecord(t, r, e.rel.RelationshipID, domain.AssessmentApprove, &future) // so only the trigger, not the missing-assessment gate, decides
		},
			func(q *domain.EvaluateTransferRequest) { q.DataCategories = []string{"HEALTH"} },
			func(q *domain.EvaluateTriggersRequest) { q.DataCategories = []string{"HEALTH"} }, true, "NEW_PURPOSE_OR_SENSITIVE_CATEGORY"},
	}
	for _, tc := range cases {
		for _, required := range []bool{false, true} {
			r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
			env := tcNewEnv(t, r, []string{"EU-GDPR", "UK-GDPR"})
			tcPause()
			tc.setup(t, r, env)
			tcPause()

			var evalMut []func(*domain.EvaluateTransferRequest)
			if tc.eval != nil {
				evalMut = append(evalMut, tc.eval)
			}
			var trigMut []func(*domain.EvaluateTriggersRequest)
			if tc.trig != nil {
				trigMut = append(trigMut, tc.trig)
			}
			tr := tcTriggers(t, r, env, trigMut...)
			d := tcEvaluate(t, r, env, required, evalMut...)

			if tr.ReassessmentNeeded != tc.wantTrig {
				t.Errorf("%s (required=%v): trigger endpoint needed=%v, want %v (%v)", tc.name, required, tr.ReassessmentNeeded, tc.wantTrig, tr.ActiveTriggers)
			}
			// "No assessment on file" with required=true is the assessment GATE, not a trigger.
			if tc.name == "no assessment on file" && required {
				if d.Result != domain.ResultReviewRequired || !tcReasonsContain(d, "ASSESSMENT_REQUIRED_NOT_FOUND") {
					t.Errorf("%s: gate behaviour changed: %s %v", tc.name, d.Result, d.ReasonCodes)
				}
				continue
			}
			if tc.wantTrig {
				if d.Result != domain.ResultReviewRequired || !tcReasonsContain(d, tc.wantToken) {
					t.Errorf("%s (required=%v): endpoint reports a trigger but EvaluateTransfer returned %s %v (want REVIEW_REQUIRED containing %s)",
						tc.name, required, d.Result, d.ReasonCodes, tc.wantToken)
				}
			} else if d.Result != domain.ResultAuthorized {
				t.Errorf("%s (required=%v): no trigger is active but EvaluateTransfer returned %s %v", tc.name, required, d.Result, d.ReasonCodes)
			}
		}
	}
}

// assessment_required still controls the assessment GATE and what enters the
// decision; the semantics the specification and the existing tests rely on.
func TestTriggerConsistency_AssessmentRequiredSemanticsUnchanged(t *testing.T) {
	future := time.Now().UTC().Add(24 * time.Hour)
	newRouter := func() (http.Handler, *triggerEnv) {
		r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
		env := tcNewEnv(t, r, nil)
		tcPause()
		return r, env
	}

	t.Run("rejected assessment is ignored when not required, blocks when required", func(t *testing.T) {
		r, env := newRouter()
		tcRecord(t, r, env.rel.RelationshipID, domain.AssessmentReject, nil)
		if d := tcEvaluate(t, r, env, false); d.Result != domain.ResultAuthorized || d.AssessmentID != nil {
			t.Fatalf("not required: want AUTHORIZED with no assessment recorded, got %s %v assessment=%v", d.Result, d.ReasonCodes, d.AssessmentID)
		}
		if d := tcEvaluate(t, r, env, true); d.Result != domain.ResultBlocked || !tcReasonsContain(d, "ASSESSMENT_REJECTED") {
			t.Fatalf("required: want BLOCKED/ASSESSMENT_REJECTED, got %s %v", d.Result, d.ReasonCodes)
		}
	})
	t.Run("remediate outcome reviews only when required", func(t *testing.T) {
		r, env := newRouter()
		tcRecord(t, r, env.rel.RelationshipID, domain.AssessmentRemediate, nil)
		if d := tcEvaluate(t, r, env, false); d.Result != domain.ResultAuthorized {
			t.Fatalf("not required: want AUTHORIZED, got %s %v", d.Result, d.ReasonCodes)
		}
		if d := tcEvaluate(t, r, env, true); d.Result != domain.ResultReviewRequired || !tcReasonsContain(d, "ASSESSMENT_REQUIRES_REMEDIATION") {
			t.Fatalf("required: want REVIEW_REQUIRED/REQUIRES_REMEDIATION, got %s %v", d.Result, d.ReasonCodes)
		}
	})
	t.Run("assessment measures become conditions only when required", func(t *testing.T) {
		r, env := newRouter()
		tcRecord(t, r, env.rel.RelationshipID, domain.AssessmentApprove, &future, func(q *domain.RecordTransferAssessmentRequest) {
			q.TechnicalMeasures = "encryption at rest"
		})
		if d := tcEvaluate(t, r, env, false); d.Result != domain.ResultAuthorized || d.Conditions != "" {
			t.Fatalf("not required: want AUTHORIZED without conditions, got %s %q", d.Result, d.Conditions)
		}
		d := tcEvaluate(t, r, env, true)
		if d.Result != domain.ResultConditional || !strings.Contains(d.Conditions, "encryption at rest") || d.AssessmentID == nil {
			t.Fatalf("required: want CONDITIONAL with the measure and the assessment id, got %s %q %v", d.Result, d.Conditions, d.AssessmentID)
		}
	})
	t.Run("approved assessment, nothing changed: AUTHORIZED both ways", func(t *testing.T) {
		r, env := newRouter()
		tcRecord(t, r, env.rel.RelationshipID, domain.AssessmentApprove, &future)
		for _, required := range []bool{false, true} {
			if d := tcEvaluate(t, r, env, required); d.Result != domain.ResultAuthorized {
				t.Fatalf("required=%v: want AUTHORIZED, got %s %v", required, d.Result, d.ReasonCodes)
			}
		}
	})
	t.Run("inactive relationship still blocks, mechanism checks unchanged", func(t *testing.T) {
		r, env := newRouter()
		tcRecord(t, r, env.rel.RelationshipID, domain.AssessmentApprove, &future)
		doRequest(r, http.MethodPost, "/privacy/processor-relationships/"+env.rel.RelationshipID+"/status", domain.UpdateRelationshipStatusRequest{Status: domain.RelationshipInactive}, testTenant)
		if d := tcEvaluate(t, r, env, false); d.Result != domain.ResultBlocked || !tcReasonsContain(d, "PROCESSOR_RELATIONSHIP_NOT_ACTIVE") {
			t.Fatalf("want BLOCKED/NOT_ACTIVE, got %s %v", d.Result, d.ReasonCodes)
		}
	})
}

// The trigger evidence is read even when no assessment is required, so a failing
// assessment lookup must fail closed — as REVIEW_REQUIRED/PRV-019 — never
// "no trigger".
type failingAssessmentStore struct {
	store.Store
}

func (failingAssessmentStore) FindLatestAssessment(context.Context, string) (*domain.TransferAssessment, error) {
	return nil, domain.ErrStoreUnavailable
}

func TestTriggerConsistency_AssessmentLookupFailureFailsClosed(t *testing.T) {
	st := newStubStore()
	st.pub = &stubPublisher{}
	good := newTestRouter(st, st.pub, &stubAuthz{}, newStubPurposeRegistry())
	env := tcNewEnv(t, good, nil)

	h := handler.New(failingAssessmentStore{Store: st}, &stubAuthz{}, newStubPurposeRegistry(), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)

	for _, required := range []bool{false, true} {
		d := tcEvaluate(t, r, env, required)
		if d.Result != domain.ResultReviewRequired || !tcReasonsContain(d, "PRV-019") {
			t.Fatalf("required=%v: want REVIEW_REQUIRED/PRV-019 on a failing lookup, got %s %v", required, d.Result, d.ReasonCodes)
		}
	}
}

// Real PostgreSQL: the reproduced scenario through the real store and RLS.
func TestTriggerConsistency_RealDB_NewSubprocessorScenario(t *testing.T) {
	db := testdb.Setup(t)
	pgStore := store.NewPgStore(db.App, zap.NewNop())
	h := handler.New(pgStore, &stubAuthz{}, newStubPurposeRegistry(), zap.NewNop())
	router := chi.NewRouter()
	router.Use(middleware.TenantContext())
	handler.RegisterRoutes(router, h)
	tenant := "00000000-0000-0000-0000-0000000000c1"

	call := func(method, path string, body interface{}) *httptest.ResponseRecorder {
		return doRequest(router, method, path, body, tenant)
	}
	mustCreate := func(path string, body interface{}, into interface{}) {
		t.Helper()
		w := call(http.MethodPost, path, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
		}
		_ = json.Unmarshal(w.Body.Bytes(), into)
	}

	var rel domain.ProcessorRelationship
	mustCreate("/privacy/processor-relationships", domain.CreateProcessorRelationshipRequest{
		ControllerRef: "c", ProcessorRef: "p", Service: "s", ProcessingInstructions: "as instructed",
	}, &rel)
	tcPause()
	var mech domain.TransferMechanism
	mustCreate("/privacy/transfer-mechanisms", domain.CreateTransferMechanismRequest{MechanismType: "SCC"}, &mech)
	tcPause()
	var as domain.TransferAssessment
	mustCreate("/privacy/transfer-assessments", domain.RecordTransferAssessmentRequest{RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentApprove}, &as)
	tcPause()
	var sp domain.Subprocessor
	mustCreate("/privacy/processor-relationships/"+rel.RelationshipID+"/subprocessors", domain.AttachSubprocessorRequest{
		ProviderIdentity: "sub", Service: "svc", NotificationApprovalModel: "PRIOR_NOTICE",
	}, &sp)

	w := call(http.MethodGet, "/privacy/transfer-assessments/triggers?relationship_id="+rel.RelationshipID+"&transfer_mechanism_id="+mech.MechanismID, nil)
	var tr domain.EvaluateTriggersResponse
	_ = json.Unmarshal(w.Body.Bytes(), &tr)
	if w.Code != http.StatusOK || !tr.ReassessmentNeeded || !tcHasTrigger(tr, domain.TriggerNewProcessorOrJurisdiction) {
		t.Fatalf("trigger endpoint: %d %s", w.Code, w.Body.String())
	}
	for _, required := range []bool{false, true} {
		w := call(http.MethodPost, "/privacy/transfer-decisions", domain.EvaluateTransferRequest{
			RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID, AssessmentRequired: required,
		})
		var d domain.TransferDecision
		_ = json.Unmarshal(w.Body.Bytes(), &d)
		if w.Code != http.StatusOK || d.Result != domain.ResultReviewRequired || !tcReasonsContain(d, "NEW_SUBPROCESSOR") {
			t.Fatalf("required=%v: want REVIEW_REQUIRED/NEW_SUBPROCESSOR, got %d %s %v", required, w.Code, d.Result, d.ReasonCodes)
		}
	}
}
