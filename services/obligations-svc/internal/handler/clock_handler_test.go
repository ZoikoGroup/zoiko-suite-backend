package handler_test

// R-001 WFC-05 clock routes (tracker OB-S6, OB-S9, OB-S10, OB-S12).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/evidencereq"
	"zoiko.io/obligations-svc/internal/handler"
	"zoiko.io/obligations-svc/internal/jurisdiction"
	svcmiddleware "zoiko.io/obligations-svc/internal/middleware"
	"zoiko.io/obligations-svc/internal/workflowclient"
)

const (
	tOblID   = "11111111-0000-0000-0000-000000000001"
	tClockID = "cccccccc-0000-0000-0000-000000000001"
	tLE      = "le-1"
)

// actionAuthz denies the named actions and grants the rest.
type actionAuthz struct{ deny map[string]bool }

func (a *actionAuthz) CheckAllowed(_ context.Context, _, _, _, action, _ string) error {
	if a.deny[action] {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

type stubCalc struct {
	calc *domain.Calculation
	err  error
	reqs []jurisdiction.CalculationRequest
}

func (s *stubCalc) Calculate(_ context.Context, req jurisdiction.CalculationRequest, _ string) (*domain.Calculation, error) {
	s.reqs = append(s.reqs, req)
	return s.calc, s.err
}

type stubWorkflow struct {
	approval *workflowclient.Approval
	err      error
	fps      []string
}

func (s *stubWorkflow) VerifyApproval(_ context.Context, _, fp string) (*workflowclient.Approval, error) {
	s.fps = append(s.fps, fp)
	return s.approval, s.err
}
func (s *stubWorkflow) OpenCase(context.Context, workflowclient.CaseRequest) (string, error) {
	return "case-1", nil
}

type stubEvidence struct {
	res *evidencereq.EvaluateResponse
	err error
	got []evidencereq.EvaluateParams
}

func (s *stubEvidence) Evaluate(_ context.Context, p evidencereq.EvaluateParams) (*evidencereq.EvaluateResponse, error) {
	s.got = append(s.got, p)
	return s.res, s.err
}

type stubClocks struct {
	clock      *domain.Clock
	created    bool
	startErr   error
	started    []domain.StartClockParams
	commands   []domain.ClockCommandParams
	cmdErr     error
	decision   *domain.ApplicabilityDecision
	superseded bool
	successor  *domain.Clock
}

func (s *stubClocks) StartClock(_ context.Context, p domain.StartClockParams) (*domain.Clock, bool, error) {
	s.started = append(s.started, p)
	if s.startErr != nil {
		return nil, false, s.startErr
	}
	c := *s.clock
	return &c, s.created, nil
}
func (s *stubClocks) GetClock(_ context.Context, id string) (*domain.Clock, error) {
	if s.clock == nil || s.clock.ClockID != id {
		return nil, domain.ErrClockNotFound
	}
	c := *s.clock
	return &c, nil
}
func (s *stubClocks) ListClocks(context.Context, domain.ListClocksFilter) ([]*domain.Clock, error) {
	c := *s.clock
	return []*domain.Clock{&c}, nil
}
func (s *stubClocks) LiveClocksForObligation(context.Context, string) ([]*domain.Clock, error) {
	c := *s.clock
	return []*domain.Clock{&c}, nil
}
func (s *stubClocks) ApplyClockCommand(_ context.Context, p domain.ClockCommandParams) (*domain.Clock, error) {
	s.commands = append(s.commands, p)
	if s.cmdErr != nil {
		return nil, s.cmdErr
	}
	c := *s.clock
	c.RowVersion++
	return &c, nil
}
func (s *stubClocks) SupersedeClock(_ context.Context, _ string, _ int, p domain.StartClockParams) (*domain.Clock, *domain.Clock, bool, error) {
	s.started = append(s.started, p)
	c := *s.clock
	return &c, s.successor, s.superseded, nil
}
func (s *stubClocks) ListClockTransitions(context.Context, string) ([]*domain.ClockTransition, error) {
	return nil, nil
}
func (s *stubClocks) ListClockMilestones(context.Context, string) ([]*domain.Milestone, error) {
	return nil, nil
}
func (s *stubClocks) ListEscalationEvents(context.Context, string) ([]*domain.EscalationEvent, error) {
	return nil, nil
}
func (s *stubClocks) ClockCalculationRequest(context.Context, string, string) (json.RawMessage, error) {
	return json.RawMessage(`{"jurisdiction":"GB","obligation_code":"VAT_RETURN","period_end":"2026-11-30"}`), nil
}
func (s *stubClocks) GetApplicabilityDecision(context.Context, string) (*domain.ApplicabilityDecision, error) {
	if s.decision == nil {
		return nil, domain.ErrApplicabilityMismatch
	}
	return s.decision, nil
}
func (s *stubClocks) CreateEscalationPolicy(_ context.Context, code string, m []domain.MilestoneSpec, _, _ string) (*domain.EscalationPolicy, error) {
	return &domain.EscalationPolicy{EscalationPolicyID: "p-1", PolicyCode: code, Version: 1, Milestones: m, RowVersion: 1}, nil
}
func (s *stubClocks) GetEscalationPolicy(context.Context, string) (*domain.EscalationPolicy, error) {
	return nil, domain.ErrEscalationPolicyNotFound
}
func (s *stubClocks) ListEscalationPolicies(context.Context, string, int, string, int) ([]*domain.EscalationPolicy, error) {
	return nil, nil
}
func (s *stubClocks) RetireEscalationPolicy(context.Context, string, int, string) (*domain.EscalationPolicy, error) {
	return nil, domain.ErrEscalationPolicyNotFound
}
func (s *stubClocks) Summary(_ context.Context, now time.Time) (*domain.ObligationSummary, error) {
	return &domain.ObligationSummary{AsOf: now}, nil
}

type clockRig struct {
	clocks *stubClocks
	calc   *stubCalc
	wf     *stubWorkflow
	ev     *stubEvidence
	authz  *actionAuthz
	router chi.Router
	now    time.Time
}

func calculated(due time.Time) *domain.Calculation {
	return &domain.Calculation{DecisionID: "dec-1", Outcome: domain.CalculationOutcomeCalculated, ObligationRuleID: "rule-1", RuleVersion: 2,
		DueDate: due.Format("2006-01-02"), DueAt: &due, Timezone: "UTC", CalendarCode: "gb-hmrc", CalendarVersion: 1, CalendarVersionID: "cv-1"}
}

func newClockRig(t *testing.T) *clockRig {
	t.Helper()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	due := now.AddDate(0, 1, 0)
	rig := &clockRig{
		clocks: &stubClocks{created: true, clock: &domain.Clock{ClockID: tClockID, ObligationID: tOblID, LegalEntityID: tLE,
			ClockClass: domain.ClockClassHardLegal, StoredState: domain.ClockStateActive, DueAt: due, LocalDueDate: due.Format("2006-01-02"),
			Timezone: "UTC", RowVersion: 3}},
		calc:  &stubCalc{calc: calculated(due)},
		wf:    &stubWorkflow{approval: &workflowclient.Approval{WorkflowType: "OBLIGATION_WAIVER", WorkflowStatus: "APPROVED", SubjectType: "OBLIGATION_CLOCK", SubjectID: tClockID, Deciders: []string{"checker-2"}}},
		ev:    &stubEvidence{res: &evidencereq.EvaluateResponse{EvaluationID: "ev-1", Outcome: "SATISFIED"}},
		authz: &actionAuthz{deny: map[string]bool{}},
		now:   now,
	}
	st := &stubStore{findObligation: &domain.Obligation{ObligationID: tOblID, LegalEntityID: tLE, ObligationStatus: "OPEN", ObligationType: "FILING", Version: 4}}
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(st, &stubValidator{}, rig.authz, zap.NewNop()).WithEvidenceRequirementsClient(rig.ev).
		WithClocks(rig.clocks, rig.calc, rig.wf).WithClock(func() time.Time { return rig.now })
	handler.RegisterRoutes(r, h)
	rig.router = r
	return rig
}

func (rig *clockRig) do(t *testing.T, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := withIdentity(httptest.NewRequest(method, path, bytes.NewBufferString(body)))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	rig.router.ServeHTTP(w, req)
	return w
}

func code(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	c, _ := m["code"].(string)
	return strings.ToLower(c)
}

const startBody = `{"clock_class":"HARD_LEGAL","jurisdiction_code":"GB","rule_obligation_code":"VAT_RETURN","facts":{"period_end":"2026-11-30"},
 "trigger":{"event_id":"evt-1","event_type":"period.closed","event_time":"2026-11-23T10:00:00Z","source_object_type":"ACCOUNTING_PERIOD","source_object_id":"p-11"}}`

var key = map[string]string{"Idempotency-Key": "k-1"}

func keyIfMatch(v string) map[string]string {
	return map[string]string{"Idempotency-Key": "k-1", "If-Match": `"` + v + `"`}
}

// OB-S6: a clock is made only from a CALCULATED answer; anything else names
// the outcome (422); jurisdiction unreachable is 503.
func TestWF3_StartClock_OnlyACalculatedAnswerMakesAClock(t *testing.T) {
	path := "/v1/obligations/" + tOblID + "/clocks"
	for _, tc := range []struct {
		name   string
		calc   *domain.Calculation
		err    error
		status int
		code   string
	}{
		{"ambiguous", &domain.Calculation{Outcome: "AMBIGUOUS", Message: "two rules"}, nil, 422, "calculation_refused"},
		{"no calendar", &domain.Calculation{Outcome: "NO_CALENDAR_VERSION"}, nil, 422, "calculation_refused"},
		{"no rule", &domain.Calculation{Outcome: "NO_OBLIGATION_RULE"}, nil, 422, "calculation_refused"},
		{"jurisdiction down", nil, domain.ErrJurisdictionServiceUnavailable, 503, "jurisdiction_service_unavailable"},
		{"caller may not calculate", nil, domain.ErrAuthorizationDenied, 403, "calculation_forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newClockRig(t)
			rig.calc.calc, rig.calc.err = tc.calc, tc.err
			w := rig.do(t, http.MethodPost, path, startBody, key)
			if w.Code != tc.status || code(t, w) != tc.code {
				t.Fatalf("got %d %s, want %d %s: %s", w.Code, code(t, w), tc.status, tc.code, w.Body.String())
			}
			if len(rig.clocks.started) != 0 {
				t.Fatal("a clock was started from a refused calculation")
			}
		})
	}
	rig := newClockRig(t)
	w := rig.do(t, http.MethodPost, path, startBody, key)
	if w.Code != http.StatusCreated || w.Header().Get("Location") == "" || w.Header().Get("ETag") != `"3"` {
		t.Fatalf("calculated: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	p := rig.clocks.started[0]
	if p.Calc.DecisionID != "dec-1" || p.ClockKey != "k-1" || p.Trigger.EventID != "evt-1" || p.Trigger.SourceObjectID != "p-11" || p.DueSoonAt == nil {
		t.Fatalf("start params: %+v", p)
	}
	if rig.calc.reqs[0].Jurisdiction != "GB" || rig.calc.reqs[0].ObligationCode != "VAT_RETURN" || rig.calc.reqs[0].PeriodEnd != "2026-11-30" {
		t.Fatalf("calculation request: %+v", rig.calc.reqs[0])
	}
	rig.clocks.created = false
	if w := rig.do(t, http.MethodPost, path, startBody, key); w.Code != http.StatusOK {
		t.Fatalf("replay: %d", w.Code)
	}
}

// R-INV-15 / NP-15: no caller due date, in any spelling.
func TestWF3_StartClock_ACallerDueDateIsRefused(t *testing.T) {
	path := "/v1/obligations/" + tOblID + "/clocks"
	for _, extra := range []string{`"due_at":"2030-01-01T00:00:00Z"`, `"computed_deadline":"2030-01-01T00:00:00Z"`, `"due_date":"2030-01-01"`} {
		rig := newClockRig(t)
		body := strings.Replace(startBody, `{"clock_class"`, `{`+extra+`,"clock_class"`, 1)
		if w := rig.do(t, http.MethodPost, path, body, key); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", extra, w.Code, w.Body.String())
		}
		if len(rig.calc.reqs) != 0 || len(rig.clocks.started) != 0 {
			t.Fatalf("%s reached the calculation", extra)
		}
	}
}

func TestWF3_StartClock_NeedsTriggerKeyAndGrant(t *testing.T) {
	path := "/v1/obligations/" + tOblID + "/clocks"
	rig := newClockRig(t)
	if w := rig.do(t, http.MethodPost, path, startBody, nil); w.Code != http.StatusBadRequest || code(t, w) != "idempotency_key_required" {
		t.Fatalf("no key: %d %s", w.Code, w.Body.String())
	}
	noTrigger := strings.Replace(startBody, `"event_id":"evt-1",`, ``, 1)
	if w := rig.do(t, http.MethodPost, path, noTrigger, key); w.Code != http.StatusBadRequest {
		t.Fatalf("no trigger event id: %d", w.Code)
	}
	future := strings.Replace(startBody, `2026-11-23T10:00:00Z`, `2027-01-01T00:00:00Z`, 1)
	if w := rig.do(t, http.MethodPost, path, future, key); w.Code != http.StatusBadRequest {
		t.Fatalf("trigger in the future: %d", w.Code)
	}
	badClass := strings.Replace(startBody, `HARD_LEGAL`, `SOFT_INTERNAL`, 1)
	if w := rig.do(t, http.MethodPost, path, badClass, key); w.Code != http.StatusBadRequest {
		t.Fatalf("SOFT_INTERNAL is no longer a class: %d", w.Code)
	}
	rig.authz.deny["OBLIGATION_CLOCK_CREATE"] = true
	if w := rig.do(t, http.MethodPost, path, startBody, key); w.Code != http.StatusForbidden {
		t.Fatalf("no grant: %d", w.Code)
	}
}

// OB-S7: the read is computed at read time.
func TestWF3_GetClock_ReadsOverdueWithoutASweep(t *testing.T) {
	rig := newClockRig(t)
	rig.clocks.clock.DueAt = rig.now.Add(-time.Minute)
	rig.clocks.clock.LocalDueDate = rig.now.Format("2006-01-02")
	w := rig.do(t, http.MethodGet, "/v1/obligations/"+tOblID+"/clocks/"+tClockID, "", nil)
	var c domain.Clock
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if w.Code != http.StatusOK || c.ClockState != domain.ClockStateOverdue || c.StoredState != domain.ClockStateActive {
		t.Fatalf("got %d state %s stored %s", w.Code, c.ClockState, c.StoredState)
	}
	if w := rig.do(t, http.MethodGet, "/v1/obligations/22222222-0000-0000-0000-000000000002/clocks/"+tClockID, "", nil); w.Code != http.StatusNotFound {
		// the stub store returns the same obligation for any id; a clock of
		// another obligation must still be 404
		t.Fatalf("clock under another obligation: %d", w.Code)
	}
}

// OB-S9 / R-INV-14 / NP-17.
func TestWF3_SatisfyClock_NeedsAcceptedEvidence(t *testing.T) {
	path := "/v1/obligations/" + tOblID + "/clocks/" + tClockID + "/satisfy"
	rig := newClockRig(t)
	if w := rig.do(t, http.MethodPost, path, `{"evidence_refs":["SUPPORTING_DOCUMENT:d-1"]}`, key); w.Code != http.StatusPreconditionRequired {
		t.Fatalf("no If-Match: %d", w.Code)
	}
	if w := rig.do(t, http.MethodPost, path, `{}`, keyIfMatch("3")); w.Code != http.StatusConflict || code(t, w) != "evidence_missing" {
		t.Fatalf("no evidence: %d %s", w.Code, w.Body.String())
	}
	if w := rig.do(t, http.MethodPost, path, `{"evidence_refs":["no-type"]}`, keyIfMatch("3")); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed ref: %d", w.Code)
	}
	rig.ev.res = &evidencereq.EvaluateResponse{EvaluationID: "ev-2", Outcome: "MISSING", Unmet: []evidencereq.Unmet{{EvidenceType: "RECEIPT"}}}
	if w := rig.do(t, http.MethodPost, path, `{"evidence_refs":["SUPPORTING_DOCUMENT:d-1"]}`, keyIfMatch("3")); w.Code != http.StatusConflict || code(t, w) != "evidence_missing" {
		t.Fatalf("MISSING: %d %s", w.Code, w.Body.String())
	}
	rig.ev.res, rig.ev.err = nil, domain.ErrEvidenceUnavailable
	if w := rig.do(t, http.MethodPost, path, `{"evidence_refs":["SUPPORTING_DOCUMENT:d-1"]}`, keyIfMatch("3")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("evidence-requirements down: %d", w.Code)
	}
	if len(rig.clocks.commands) != 0 {
		t.Fatal("a refused satisfy reached the store")
	}
	rig.ev.err = nil
	for _, outcome := range []string{"SATISFIED", "NO_REQUIREMENTS_DEFINED"} {
		rig.ev.res = &evidencereq.EvaluateResponse{EvaluationID: "ev-3", Outcome: outcome}
		if w := rig.do(t, http.MethodPost, path, `{"evidence_refs":["SUPPORTING_DOCUMENT:d-1:INVOICE"]}`, keyIfMatch("3")); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", outcome, w.Code, w.Body.String())
		}
	}
	got := rig.ev.got[len(rig.ev.got)-1]
	if got.LegalEntityID != tLE || got.PresentArtifacts[0].EvidenceType != "SUPPORTING_DOCUMENT" || got.PresentArtifacts[0].ArtifactSubtype != "INVOICE" {
		t.Fatalf("evaluation request: %+v", got)
	}
	cmd := rig.clocks.commands[len(rig.clocks.commands)-1]
	if cmd.ExpectedVersion != 3 || cmd.Command != domain.ClockCommandSatisfy || *cmd.EvidenceEvaluationID != "ev-3" {
		t.Fatalf("command: %+v", cmd)
	}
	rig.authz.deny["OBLIGATION_CLOCK_SATISFY"] = true
	if w := rig.do(t, http.MethodPost, path, `{"evidence_refs":["SUPPORTING_DOCUMENT:d-1"]}`, keyIfMatch("3")); w.Code != http.StatusForbidden {
		t.Fatalf("no grant: %d", w.Code)
	}
}

// OB-S10 / NP-18: a waiver needs the grant, an approval workflow about this
// clock, approved for exactly this subject, by somebody else.
func TestWF3_WaiveClock_AuthorityAndASecondPerson(t *testing.T) {
	path := "/v1/obligations/" + tOblID + "/clocks/" + tClockID + "/waive"
	body := `{"reason_code":"COURT_ORDER","evidence_refs":["SUPPORTING_DOCUMENT:order-1"],"approval_reference":"wf-1"}`
	rig := newClockRig(t)
	rig.authz.deny["OBLIGATION_CLOCK_WAIVE"] = true
	if w := rig.do(t, http.MethodPost, path, body, keyIfMatch("3")); w.Code != http.StatusForbidden {
		t.Fatalf("NP-18 waiver without authority: %d", w.Code)
	}
	if len(rig.wf.fps) != 0 {
		t.Fatal("the approval was consulted before the grant")
	}
	delete(rig.authz.deny, "OBLIGATION_CLOCK_WAIVE")
	for _, tc := range []struct {
		name     string
		body     string
		approval *workflowclient.Approval
		err      error
		status   int
		code     string
	}{
		{"no approval", `{"reason_code":"COURT_ORDER","evidence_refs":["DOC:1"],"approval_reference":""}`, nil, nil, 409, "approval_required"},
		{"no evidence", `{"reason_code":"COURT_ORDER","evidence_refs":[],"approval_reference":"wf-1"}`, nil, nil, 400, "missing_field"},
		{"no reason", `{"reason_code":"","evidence_refs":["DOC:1"],"approval_reference":"wf-1"}`, nil, nil, 400, "missing_field"},
		{"requester approved", body, &workflowclient.Approval{WorkflowType: "OBLIGATION_WAIVER", SubjectType: "OBLIGATION_CLOCK", SubjectID: tClockID, Deciders: []string{testPrincipal}}, nil, 409, "approval_required"},
		{"another clock", body, &workflowclient.Approval{WorkflowType: "OBLIGATION_WAIVER", SubjectType: "OBLIGATION_CLOCK", SubjectID: "other", Deciders: []string{"checker-2"}}, nil, 409, "approval_required"},
		{"wrong workflow type", body, &workflowclient.Approval{WorkflowType: "PURCHASE_APPROVAL", SubjectType: "OBLIGATION_CLOCK", SubjectID: tClockID, Deciders: []string{"checker-2"}}, nil, 409, "approval_required"},
		{"not released", body, nil, domain.ErrApprovalRequired, 409, "approval_required"},
		{"workflow down", body, nil, domain.ErrWorkflowUnavailable, 503, "workflow_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig.wf.approval, rig.wf.err = tc.approval, tc.err
			n := len(rig.clocks.commands)
			w := rig.do(t, http.MethodPost, path, tc.body, keyIfMatch("3"))
			if w.Code != tc.status || code(t, w) != tc.code {
				t.Fatalf("got %d %s, want %d %s: %s", w.Code, code(t, w), tc.status, tc.code, w.Body.String())
			}
			if len(rig.clocks.commands) != n {
				t.Fatal("a refused waiver reached the store")
			}
		})
	}
	rig.wf.approval, rig.wf.err = &workflowclient.Approval{WorkflowType: "OBLIGATION_WAIVER", SubjectType: "OBLIGATION_CLOCK", SubjectID: tClockID, Deciders: []string{"checker-2"}}, nil
	if w := rig.do(t, http.MethodPost, path, body, keyIfMatch("3")); w.Code != http.StatusOK {
		t.Fatalf("approved waiver: %d %s", w.Code, w.Body.String())
	}
	cmd := rig.clocks.commands[len(rig.clocks.commands)-1]
	if *cmd.ApprovedBy != "checker-2" || cmd.Actor != testPrincipal || *cmd.ApprovalReference != "wf-1" || *cmd.ReasonCode != "COURT_ORDER" {
		t.Fatalf("waiver command: %+v", cmd)
	}
	want := domain.AuthoritySubjectFingerprint(tClockID, "WAIVE", "COURT_ORDER", []string{"SUPPORTING_DOCUMENT:order-1"})
	if rig.wf.fps[len(rig.wf.fps)-1] != want {
		t.Fatal("verify-release was not asked about this exact subject")
	}
}

func TestWF3_NotApplicable_NeedsANotApplicableDecisionOfThisObligation(t *testing.T) {
	path := "/v1/obligations/" + tOblID + "/clocks/" + tClockID + "/not-applicable"
	base := `{"reason_code":"OUT_OF_SCOPE","evidence_refs":["DOC:1"],"approval_reference":"wf-1"`
	rig := newClockRig(t)
	rig.wf.approval = &workflowclient.Approval{WorkflowType: "OBLIGATION_NOT_APPLICABLE", SubjectType: "OBLIGATION_CLOCK", SubjectID: tClockID, Deciders: []string{"checker-2"}}
	if w := rig.do(t, http.MethodPost, path, base+`}`, keyIfMatch("3")); w.Code != http.StatusBadRequest {
		t.Fatalf("no decision id: %d", w.Code)
	}
	rig.clocks.decision = &domain.ApplicabilityDecision{ObligationID: tOblID, Decision: "APPLICABLE"}
	if w := rig.do(t, http.MethodPost, path, base+`,"applicability_decision_id":"ad-1"}`, keyIfMatch("3")); w.Code != http.StatusConflict || code(t, w) != "applicability_mismatch" {
		t.Fatalf("APPLICABLE decision: %d %s", w.Code, w.Body.String())
	}
	rig.clocks.decision = &domain.ApplicabilityDecision{ObligationID: "another", Decision: "NOT_APPLICABLE"}
	if w := rig.do(t, http.MethodPost, path, base+`,"applicability_decision_id":"ad-1"}`, keyIfMatch("3")); w.Code != http.StatusConflict {
		t.Fatalf("another obligation's decision: %d", w.Code)
	}
	rig.clocks.decision = &domain.ApplicabilityDecision{ObligationID: tOblID, Decision: "NOT_APPLICABLE"}
	if w := rig.do(t, http.MethodPost, path, base+`,"applicability_decision_id":"ad-1"}`, keyIfMatch("3")); w.Code != http.StatusOK {
		t.Fatalf("valid N/A: %d %s", w.Code, w.Body.String())
	}
}

// ST-3: the old free-target route is deprecated and names the command.
func TestWF3_ClockStatusRoute_IsADeprecatedAlias(t *testing.T) {
	path := "/v1/obligations/" + tOblID + "/clocks/" + tClockID + "/status"
	rig := newClockRig(t)
	for state, want := range map[string]string{"FULFILLED": "use_named_command", "WAIVED": "use_named_command", "OVERDUE": "computed_state", "DUE_SOON": "computed_state"} {
		w := rig.do(t, http.MethodPost, path, `{"clock_state":"`+state+`"}`, keyIfMatch("3"))
		if w.Code != http.StatusConflict || code(t, w) != want || w.Header().Get("Deprecation") != "true" {
			t.Fatalf("%s: %d %s %v", state, w.Code, w.Body.String(), w.Header())
		}
	}
	if len(rig.clocks.commands) != 0 {
		t.Fatal("the alias set a state")
	}
}

func TestWF3_Recalculate_SupersedesOnlyWhenTheAnswerChanges(t *testing.T) {
	path := "/v1/obligations/" + tOblID + "/clocks/" + tClockID + "/recalculate"
	rig := newClockRig(t)
	if w := rig.do(t, http.MethodPost, path, `{}`, keyIfMatch("3")); w.Code != http.StatusBadRequest {
		t.Fatalf("no reason: %d", w.Code)
	}
	rig.clocks.superseded = true
	rig.clocks.successor = &domain.Clock{ClockID: "succ", ObligationID: tOblID, StoredState: domain.ClockStateActive, DueAt: rig.now.AddDate(0, 2, 0)}
	w := rig.do(t, http.MethodPost, path, `{"reason_code":"CALENDAR_CORRECTED"}`, keyIfMatch("3"))
	var body struct {
		Superseded bool          `json:"superseded"`
		Successor  *domain.Clock `json:"successor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusOK || !body.Superseded || body.Successor == nil || body.Successor.ClockID != "succ" {
		t.Fatalf("recalculate: %d %s", w.Code, w.Body.String())
	}
	p := rig.clocks.started[len(rig.clocks.started)-1]
	if *p.SupersessionReason != "CALENDAR_CORRECTED" || !strings.HasPrefix(p.ClockKey, "recalc:"+tClockID) {
		t.Fatalf("successor params: %+v", p)
	}
	rig.authz.deny["OBLIGATION_CLOCK_RECALCULATE"] = true
	if w := rig.do(t, http.MethodPost, path, `{"reason_code":"X"}`, keyIfMatch("3")); w.Code != http.StatusForbidden {
		t.Fatalf("no grant: %d", w.Code)
	}
}

func TestWF3_ListClocks_ForgedCursorIs400(t *testing.T) {
	rig := newClockRig(t)
	if w := rig.do(t, http.MethodGet, "/v1/obligations/"+tOblID+"/clocks?cursor=not-a-cursor", "", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("forged cursor: %d", w.Code)
	}
	if w := rig.do(t, http.MethodGet, "/v1/obligations/"+tOblID+"/clocks?limit=1", "", nil); w.Code != http.StatusOK || w.Header().Get("X-Next-Cursor") == "" {
		t.Fatalf("first page: %d %v", w.Code, w.Header())
	}
}

func TestWF3_AvailableActions_FollowStateAndGrants(t *testing.T) {
	rig := newClockRig(t)
	rig.authz.deny["OBLIGATION_CLOCK_WAIVE"] = true
	w := rig.do(t, http.MethodGet, "/v1/obligations/"+tOblID+"/clocks/"+tClockID+"/available-actions", "", nil)
	var body struct {
		Actions []struct {
			Command string `json:"command"`
			Allowed bool   `json:"allowed"`
		} `json:"actions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	got := map[string]bool{}
	for _, a := range body.Actions {
		got[a.Command] = a.Allowed
	}
	if _, has := got["activate"]; has || !got["satisfy"] || got["waive"] || !got["recalculate"] {
		t.Fatalf("ACTIVE clock actions: %v", got)
	}
	rig.clocks.clock.StoredState = domain.ClockStateFulfilled
	w = rig.do(t, http.MethodGet, "/v1/obligations/"+tOblID+"/clocks/"+tClockID+"/available-actions", "", nil)
	body.Actions = nil
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Actions) != 0 {
		t.Fatalf("a FULFILLED clock offers %+v", body.Actions)
	}
	w = rig.do(t, http.MethodGet, "/v1/obligations/"+tOblID+"/available-actions", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status:CLOSED"`) {
		t.Fatalf("obligation actions: %d %s", w.Code, w.Body.String())
	}
}

func TestWF3_Evaluate_RefusesADueDateAndNeedsADefinition(t *testing.T) {
	rig := newClockRig(t)
	clock := strings.TrimSpace(startBody)
	for name, body := range map[string]string{
		"due_date sent":    `{"obligation":{"obligation_definition_id":"d-1","legal_entity_id":"le-1","obligation_code":"OBL-1","due_date":"2030-01-01T00:00:00Z"},"clock":` + clock + `}`,
		"no definition":    `{"obligation":{"legal_entity_id":"le-1","obligation_code":"OBL-1"},"clock":` + clock + `}`,
		"no trigger event": `{"obligation":{"obligation_definition_id":"d-1"},"clock":` + strings.Replace(clock, `"event_id":"evt-1",`, ``, 1) + `}`,
	} {
		if w := rig.do(t, http.MethodPost, "/v1/obligations/evaluate", body, key); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if len(rig.calc.reqs) != 0 {
		t.Fatal("a refused evaluate reached the calculation")
	}
}

func TestWF3_Summary_IsPlatformScopedRead(t *testing.T) {
	rig := newClockRig(t)
	if w := rig.do(t, http.MethodGet, "/v1/obligations/summary", "", nil); w.Code != http.StatusOK {
		t.Fatalf("summary: %d", w.Code)
	}
	rig.authz.deny["OBLIGATION_READ"] = true
	if w := rig.do(t, http.MethodGet, "/v1/obligations/summary", "", nil); w.Code != http.StatusForbidden {
		t.Fatalf("summary without read: %d", w.Code)
	}
}

func TestWF3_ClockRoutesAnswer503WithoutClocks(t *testing.T) {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(&stubStore{}, &stubValidator{}, &stubAuthz{}, zap.NewNop()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations/summary", nil)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("summary without clocks: %d", w.Code)
	}
	_ = errors.New
}
