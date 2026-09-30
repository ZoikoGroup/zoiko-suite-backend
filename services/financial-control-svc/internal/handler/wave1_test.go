package handler_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/engine"
	"zoiko.io/financial-control-svc/internal/store"
)

type wave1State struct {
	beginRun    *domain.ControlRun
	beginReplay bool
	beginErr    error
	beginExpect int
	snaps       []domain.PopulationSnapshot
	records     []store.PopulationRecordRow
	exceptions  []domain.ControlException
	exception   *domain.ControlException
	assignErr   error
	assignedVer int
	assignReq   domain.AssignExceptionRequest
	evidence    *domain.EvidencePackage
	evidenceErr error
}

func (f *fakeStore) BeginExecution(_ context.Context, _, _, _, _, _ string, expected int) (*domain.ControlRun, bool, error) {
	f.calls = append(f.calls, "BeginExecution")
	f.w1.beginExpect = expected
	return f.w1.beginRun, f.w1.beginReplay, f.w1.beginErr
}
func (f *fakeStore) ListPopulationSnapshots(context.Context, string, string) ([]domain.PopulationSnapshot, error) {
	return f.w1.snaps, nil
}
func (f *fakeStore) ListPopulationRecords(context.Context, string, string, domain.Side, int64, int) ([]store.PopulationRecordRow, error) {
	return f.w1.records, nil
}
func (f *fakeStore) ListExceptions(context.Context, string, string, store.ListExceptionsFilter) ([]domain.ControlException, error) {
	return f.w1.exceptions, nil
}
func (f *fakeStore) GetException(context.Context, string, string) (*domain.ControlException, error) {
	if f.w1.exception == nil {
		return nil, domain.ErrNotFound
	}
	c := *f.w1.exception
	return &c, nil
}
func (f *fakeStore) AssignException(_ context.Context, _, _, _, _ string, expected int, req domain.AssignExceptionRequest) (*domain.ControlException, error) {
	f.calls = append(f.calls, "AssignException")
	f.w1.assignedVer, f.w1.assignReq = expected, req
	if f.w1.assignErr != nil {
		return nil, f.w1.assignErr
	}
	c := *f.w1.exception
	c.State, c.Version = domain.ExAssigned, c.Version+1
	owner := req.OwnerPrincipalID
	c.OwnerPrincipal = &owner
	return &c, nil
}
func (f *fakeStore) GetLatestEvidence(context.Context, string, string) (*domain.EvidencePackage, error) {
	return f.w1.evidence, f.w1.evidenceErr
}

type fakeExecutor struct {
	result *domain.ControlRun
	err    error
	called int
	in     engine.Input
}

func (e *fakeExecutor) Execute(_ context.Context, in engine.Input) (*domain.ControlRun, error) {
	e.called++
	e.in = in
	return e.result, e.err
}

func preparing() *domain.ControlRun {
	r := sampleRun()
	r.LifecycleState, r.Version = domain.LifecyclePreparing, 2
	return r
}

func TestExecute_RequiresIdempotencyKey(t *testing.T) {
	s, ex := &fakeStore{run: sampleRun()}, &fakeExecutor{}
	rr := do(newServerExec(s, &fakeAuthz{}, ex), "POST", "/controls/v1/runs/r1/execute", "", auth)
	assert.Equal(t, 400, rr.Code)
	assert.Contains(t, rr.Body.String(), "missing_idempotency_key")
	assert.Zero(t, ex.called)
}

func TestExecute_HappyPath_AuthorizesRunEntity_RunsPipelineOnce(t *testing.T) {
	final := sampleRun()
	final.LifecycleState, final.ResultState, final.Version = domain.LifecycleReadyToCertify, domain.ResultPass, 6
	s := &fakeStore{run: sampleRun(), w1: wave1State{beginRun: preparing()}}
	ex := &fakeExecutor{result: final}
	a := &fakeAuthz{}
	rr := do(newServerExec(s, a, ex), "POST", "/controls/v1/runs/r1/execute", "",
		with(map[string]string{"Idempotency-Key": "k1", "If-Match": `"3"`, "X-Legal-Entity-Id": "spoofed"}))
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assert.Equal(t, `"6"`, rr.Header().Get("ETag"))
	assert.Equal(t, 1, ex.called)
	assert.Equal(t, "alice", ex.in.Actor, "executes as the VERIFIED principal")
	assert.Equal(t, 3, s.w1.beginExpect, "If-Match is applied to the start")
	assert.Equal(t, []string{"FINCTRL_EXECUTE"}, a.actions)
	assert.Equal(t, []string{"e1"}, a.entity, "authorized against the run's own entity, not the header")
	var got domain.ControlRun
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Equal(t, domain.ResultPass, got.ResultState)
}

func TestExecute_ReplayDoesNotRunPipelineAgain(t *testing.T) {
	s := &fakeStore{run: sampleRun(), w1: wave1State{beginRun: preparing(), beginReplay: true}}
	ex := &fakeExecutor{result: sampleRun()}
	rr := do(newServerExec(s, &fakeAuthz{}, ex), "POST", "/controls/v1/runs/r1/execute", "", with(map[string]string{"Idempotency-Key": "k1"}))
	assert.Equal(t, 200, rr.Code)
	assert.Equal(t, "true", rr.Header().Get("Idempotent-Replay"))
	assert.Zero(t, ex.called, "a replay must not start a second execution")
	assert.Equal(t, store.SkipVersionCheck, s.w1.beginExpect, "no If-Match means no version check")
}

func TestExecute_DeniedNeverStartsAnything(t *testing.T) {
	for _, err := range []error{domain.ErrAuthorizationDenied, domain.ErrAuthorizationServiceUnavailable} {
		s, ex := &fakeStore{run: sampleRun(), w1: wave1State{beginRun: preparing()}}, &fakeExecutor{}
		rr := do(newServerExec(s, &fakeAuthz{err: err}, ex), "POST", "/controls/v1/runs/r1/execute", "", with(map[string]string{"Idempotency-Key": "k"}))
		assert.Contains(t, []int{403, 503}, rr.Code)
		assert.NotContains(t, s.calls, "BeginExecution", "no state change for a caller who is not authorized (fail closed)")
		assert.Zero(t, ex.called)
	}
}

func TestExecute_ErrorMapping(t *testing.T) {
	key := with(map[string]string{"Idempotency-Key": "k"})
	for err, want := range map[error]int{domain.ErrInvalidTransition: 422, domain.ErrConflict: 409, domain.ErrNotFound: 404} {
		s := &fakeStore{run: sampleRun(), w1: wave1State{beginErr: err}}
		rr := do(newServerExec(s, &fakeAuthz{}, &fakeExecutor{}), "POST", "/controls/v1/runs/r1/execute", "", key)
		assert.Equal(t, want, rr.Code, "%v", err)
	}
	// Executor could not even record the failure => 503, not a fake success.
	s := &fakeStore{run: sampleRun(), w1: wave1State{beginRun: preparing()}}
	rr := do(newServerExec(s, &fakeAuthz{}, &fakeExecutor{err: context.DeadlineExceeded}), "POST", "/controls/v1/runs/r1/execute", "", key)
	assert.Equal(t, 503, rr.Code)
	// Malformed If-Match.
	rr = do(newServerExec(&fakeStore{run: sampleRun()}, &fakeAuthz{}, &fakeExecutor{}), "POST", "/controls/v1/runs/r1/execute", "",
		with(map[string]string{"Idempotency-Key": "k", "If-Match": "abc"}))
	assert.Equal(t, 400, rr.Code)
}

func TestExecute_TechnicalFailureIsReportedAs200WithIndeterminateState(t *testing.T) {
	failed := sampleRun()
	failed.LifecycleState, failed.ResultState = domain.LifecycleFailed, domain.ResultIndeterminate
	s := &fakeStore{run: sampleRun(), w1: wave1State{beginRun: preparing()}}
	rr := do(newServerExec(s, &fakeAuthz{}, &fakeExecutor{result: failed}), "POST", "/controls/v1/runs/r1/execute", "", with(map[string]string{"Idempotency-Key": "k"}))
	require.Equal(t, 200, rr.Code)
	var got domain.ControlRun
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Equal(t, domain.ResultIndeterminate, got.ResultState)
	assert.NotEqual(t, domain.ResultPass, got.ResultState)
	assert.Contains(t, got.Attention, "CONTROL_FAILED")
}

func TestGetPopulation_SnapshotsAlwaysRecordsOnRequest(t *testing.T) {
	s := &fakeStore{run: sampleRun(), w1: wave1State{
		snaps:   []domain.PopulationSnapshot{{Side: domain.SideA, RowCount: 2, PopulationHash: "sha256:x", Watermark: "wm"}},
		records: []store.PopulationRecordRow{{Seq: 7, Record: domain.PopulationRecord{RecordID: "a1"}}},
	}}
	h := newServerExec(s, &fakeAuthz{}, &fakeExecutor{})
	rr := do(h, "GET", "/controls/v1/runs/r1/population", "", auth)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `"source_watermark"`)
	assert.NotContains(t, rr.Body.String(), `"records"`)

	rr = do(h, "GET", "/controls/v1/runs/r1/population?side=a&limit=1", "", auth)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `"records"`)
	assert.Contains(t, rr.Body.String(), `"next_after":7`)

	assert.Equal(t, 400, do(h, "GET", "/controls/v1/runs/r1/population?side=C", "", auth).Code)
	denied := newServerExec(s, &fakeAuthz{err: domain.ErrAuthorizationDenied}, &fakeExecutor{})
	assert.Equal(t, 403, do(denied, "GET", "/controls/v1/runs/r1/population", "", auth).Code)
}

func openException() *domain.ControlException {
	return &domain.ControlException{ExceptionID: "x1", RunID: "r1", LegalEntityID: "e1", State: domain.ExOpen, Version: 1,
		Severity: domain.SeverityHigh, OwnerRole: "AR_CONTROLLER", DueAt: time.Now().Add(-time.Hour)}
}

func TestListExceptions_DerivesAttentionAndPages(t *testing.T) {
	s := &fakeStore{run: sampleRun(), w1: wave1State{exceptions: []domain.ControlException{*openException()}}}
	rr := do(newServerExec(s, &fakeAuthz{}, &fakeExecutor{}), "GET", "/controls/v1/runs/r1/exceptions?limit=1", "", auth)
	require.Equal(t, 200, rr.Code)
	var got struct {
		Items     []domain.ControlException `json:"items"`
		NextAfter string                    `json:"next_after"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got.Items, 1)
	assert.ElementsMatch(t, []string{"OVERDUE", "SLA_BREACH", "MATERIAL"}, got.Items[0].Attention)
	assert.Equal(t, domain.ExOpen, got.Items[0].State, "derived signals never overwrite authoritative state")
	assert.Equal(t, "x1", got.NextAfter)
}

func TestAssignException_RequiresIfMatchAndReason(t *testing.T) {
	s := &fakeStore{w1: wave1State{exception: openException()}}
	h := newServerExec(s, &fakeAuthz{}, &fakeExecutor{})
	body := `{"owner_principal_id":"bob","reason":"AR lead"}`

	assert.Equal(t, 428, do(h, "POST", "/controls/v1/exceptions/x1/assign", body, auth).Code, "no If-Match => 428")
	assert.Equal(t, 400, do(h, "POST", "/controls/v1/exceptions/x1/assign", body, with(map[string]string{"If-Match": "x"})).Code)
	assert.Equal(t, 400, do(h, "POST", "/controls/v1/exceptions/x1/assign", `{"owner_principal_id":"bob"}`, with(map[string]string{"If-Match": `"1"`})).Code, "reason required")
	assert.Equal(t, 400, do(h, "POST", "/controls/v1/exceptions/x1/assign", `{"reason":"r"}`, with(map[string]string{"If-Match": `"1"`})).Code, "owner required")
	assert.Equal(t, 400, do(h, "POST", "/controls/v1/exceptions/x1/assign", `{"owner_principal_id":"b","reason":"r","due_date":"tomorrow"}`, with(map[string]string{"If-Match": `"1"`})).Code)
	assert.NotContains(t, s.calls, "AssignException")
}

func TestAssignException_HappyPath(t *testing.T) {
	s := &fakeStore{w1: wave1State{exception: openException()}}
	a := &fakeAuthz{}
	rr := do(newServerExec(s, a, &fakeExecutor{}), "POST", "/controls/v1/exceptions/x1/assign",
		`{"owner_principal_id":"bob","reason":"AR lead"}`, with(map[string]string{"If-Match": `"1"`}))
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assert.Equal(t, `"2"`, rr.Header().Get("ETag"))
	assert.Equal(t, 1, s.w1.assignedVer)
	assert.Equal(t, []string{"FINCTRL_EXCEPTION_ASSIGN"}, a.actions)
	assert.Equal(t, []string{"e1"}, a.entity, "authorized against the exception's own legal entity")
	assert.Contains(t, rr.Body.String(), `"state":"ASSIGNED"`)
}

func TestAssignException_ErrorMapping(t *testing.T) {
	h := func(err error) int {
		s := &fakeStore{w1: wave1State{exception: openException(), assignErr: err}}
		return do(newServerExec(s, &fakeAuthz{}, &fakeExecutor{}), "POST", "/controls/v1/exceptions/x1/assign",
			`{"owner_principal_id":"bob","reason":"r"}`, with(map[string]string{"If-Match": `"1"`})).Code
	}
	assert.Equal(t, 409, h(domain.ErrConflict), "stale ETag")
	assert.Equal(t, 422, h(domain.ErrInvalidTransition))
	assert.Equal(t, 400, h(domain.ErrInvalidArgument), "SLA extension refused")
	// Unknown exception and denied caller.
	assert.Equal(t, 404, do(newServerExec(&fakeStore{}, &fakeAuthz{}, &fakeExecutor{}), "POST", "/controls/v1/exceptions/x9/assign",
		`{"owner_principal_id":"bob","reason":"r"}`, with(map[string]string{"If-Match": `"1"`})).Code)
	s := &fakeStore{w1: wave1State{exception: openException()}}
	rr := do(newServerExec(s, &fakeAuthz{err: domain.ErrAuthorizationDenied}, &fakeExecutor{}), "POST", "/controls/v1/exceptions/x1/assign",
		`{"owner_principal_id":"bob","reason":"r"}`, with(map[string]string{"If-Match": `"1"`}))
	assert.Equal(t, 403, rr.Code)
	assert.NotContains(t, s.calls, "AssignException")
}

func TestGetEvidence_ReportsIntegrity(t *testing.T) {
	raw, digest, err := domain.SealEvidence(domain.EvidenceContent{Run: domain.RunEvidence{RunID: "r1"}})
	require.NoError(t, err)
	ok := domain.VerifyEvidence(raw, digest)
	s := &fakeStore{run: sampleRun(), w1: wave1State{evidence: &domain.EvidencePackage{PackageID: "p1", RunID: "r1", Content: raw, Digest: digest, Verified: &ok}}}
	rr := do(newServerExec(s, &fakeAuthz{}, &fakeExecutor{}), "GET", "/controls/v1/runs/r1/evidence", "", auth)
	require.Equal(t, 200, rr.Code)
	assert.Contains(t, rr.Body.String(), `"integrity_verified":true`)

	missing := &fakeStore{run: sampleRun(), w1: wave1State{evidenceErr: domain.ErrNotFound}}
	assert.Equal(t, 404, do(newServerExec(missing, &fakeAuthz{}, &fakeExecutor{}), "GET", "/controls/v1/runs/r1/evidence", "", auth).Code)
}

func TestSeedCatalogue(t *testing.T) {
	body := `{"wave":2,"ar_control_accounts":"1200","ap_control_accounts":"2000","cash_accounts":"1000","effective_from":"2026-09-01"}`
	s := &fakeStore{}
	a := &fakeAuthz{}
	rr := do(newServerExec(s, a, &fakeExecutor{}), "POST", "/controls/v1/catalogue/seed", body, auth)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	var got struct {
		Controls []struct{ ControlCode, Status string } `json:"controls"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got.Controls, 6)
	assert.Equal(t, "created", got.Controls[0].Status)
	assert.Equal(t, []string{"FINCTRL_DEFINE"}, a.actions, "seeding needs the same authority as defining a control")

	for name, b := range map[string]string{
		"wrong wave":     `{"wave":9,"ar_control_accounts":"1","ap_control_accounts":"2","cash_accounts":"3","effective_from":"2026-09-01"}`,
		"missing inputs": `{"wave":2}`,
		"unknown field":  `{"wave":2,"ar_control_accounts":"1","ap_control_accounts":"2","cash_accounts":"3","effective_from":"2026-09-01","x":1}`,
	} {
		s := &fakeStore{}
		assert.Equal(t, 400, do(newServerExec(s, &fakeAuthz{}, &fakeExecutor{}), "POST", "/controls/v1/catalogue/seed", b, auth).Code, name)
		assert.NotContains(t, s.calls, "CreateDefinition", name)
	}
	denied := &fakeStore{}
	assert.Equal(t, 403, do(newServerExec(denied, &fakeAuthz{err: domain.ErrAuthorizationDenied}, &fakeExecutor{}), "POST", "/controls/v1/catalogue/seed", body, auth).Code)
	assert.NotContains(t, denied.calls, "CreateDefinition")
}

func TestSeedCatalogue_Wave3(t *testing.T) {
	s := &fakeStore{}
	rr := do(newServerExec(s, &fakeAuthz{}, &fakeExecutor{}), "POST", "/controls/v1/catalogue/seed", `{"wave":3,"effective_from":"2026-09-01"}`, auth)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	var got struct {
		Controls []struct {
			ControlCode string `json:"control_code"`
			Status      string `json:"status"`
		} `json:"controls"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	codes := []string{}
	for _, c := range got.Controls {
		codes = append(codes, c.ControlCode)
	}
	assert.ElementsMatch(t, []string{"FIN-CTRL-013", "PAY-CTRL-001", "PAY-CTRL-002", "PAY-CTRL-003"}, codes)

	assert.Equal(t, 400, do(newServerExec(&fakeStore{}, &fakeAuthz{}, &fakeExecutor{}), "POST", "/controls/v1/catalogue/seed", `{"wave":3}`, auth).Code, "effective_from is required")
	assert.Equal(t, 400, do(newServerExec(&fakeStore{}, &fakeAuthz{}, &fakeExecutor{}), "POST", "/controls/v1/catalogue/seed", `{"wave":4,"effective_from":"2026-09-01"}`, auth).Code)
}

func TestCatalogueStatus(t *testing.T) {
	a := &fakeAuthz{}
	rr := do(newServerExec(&fakeStore{}, a, &fakeExecutor{}), "GET", "/controls/v1/catalogue/status", "", auth)
	require.Equal(t, 200, rr.Code)
	assert.Equal(t, []string{"FINCTRL_READ"}, a.actions)
	var got struct {
		Summary  map[string]int `json:"summary"`
		Controls []struct {
			Code      string   `json:"control_code"`
			State     string   `json:"state"`
			BlockedBy []string `json:"blocked_by"`
		} `json:"controls"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Equal(t, 48, len(got.Controls))
	assert.Equal(t, 25, got.Summary["BLOCKED"])
	assert.Equal(t, 11, got.Summary["PARTIAL"])
	for _, c := range got.Controls {
		if c.Code == "FIN-CTRL-007" {
			assert.Equal(t, "BLOCKED", c.State)
			assert.NotEmpty(t, c.BlockedBy)
		}
	}
	assert.Equal(t, 403, do(newServerExec(&fakeStore{}, &fakeAuthz{err: domain.ErrAuthorizationDenied}, &fakeExecutor{}), "GET", "/controls/v1/catalogue/status", "", auth).Code)
	assert.Equal(t, 401, do(newServerExec(&fakeStore{}, &fakeAuthz{}, &fakeExecutor{}), "GET", "/controls/v1/catalogue/status", "", map[string]string{"X-Principal-Id": "a"}).Code)
}

func TestSeedCatalogue_Wave4(t *testing.T) {
	body := `{"wave":4,"effective_from":"2026-09-01","ic_receivable_accounts":"1300","ic_payable_accounts":"2300"}`
	rr := do(newServerExec(&fakeStore{}, &fakeAuthz{}, &fakeExecutor{}), "POST", "/controls/v1/catalogue/seed", body, auth)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	var got struct {
		Controls []struct {
			ControlCode string `json:"control_code"`
		} `json:"controls"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	codes := []string{}
	for _, c := range got.Controls {
		codes = append(codes, c.ControlCode)
	}
	assert.ElementsMatch(t, []string{"FIN-CTRL-014", "FIN-CTRL-015", "FIN-CTRL-017"}, codes)

	for name, b := range map[string]string{
		"no accounts":  `{"wave":4,"effective_from":"2026-09-01"}`,
		"no effective": `{"wave":4,"ic_receivable_accounts":"1","ic_payable_accounts":"2"}`,
	} {
		assert.Equal(t, 400, do(newServerExec(&fakeStore{}, &fakeAuthz{}, &fakeExecutor{}), "POST", "/controls/v1/catalogue/seed", b, auth).Code, name)
	}
}
