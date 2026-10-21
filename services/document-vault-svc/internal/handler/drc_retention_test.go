package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/handler"
)

// POST /v1/retention:evaluate, POST /v1/dispositions and
// POST /v1/dispositions/{id}:approve (ZS-SVC-S-001 §9.2) over the real handlers.

type retStub struct {
	handler.RetentionStore
	entities   map[string]string
	entitiesEr error
	evalCalls  [][]string
	evalAsOf   []time.Time
	evalActors []string
	evalResult []domain.RetentionEvaluation
	candidate  []domain.CreateDispositionCandidateParams
	candErr    error
	approved   []domain.ApproveForDispositionParams
	approveErr error
}

func (s *retStub) RecordLegalEntities(_ context.Context, ids []string) (map[string]string, error) {
	if s.entitiesEr != nil {
		return nil, s.entitiesEr
	}
	out := map[string]string{}
	for _, id := range ids {
		e, ok := s.entities[id]
		if !ok {
			return nil, domain.ErrRecordNotFound
		}
		out[id] = e
	}
	return out, nil
}

func (s *retStub) EvaluateRetentionAt(_ context.Context, p domain.EvaluateRetentionParams) ([]domain.RetentionEvaluation, error) {
	ids := p.RecordIDs
	s.evalAsOf = append(s.evalAsOf, p.AsOf)
	s.evalActors = append(s.evalActors, p.ActorPrincipalID)
	s.evalCalls = append(s.evalCalls, ids)
	if s.evalResult != nil {
		return s.evalResult, nil
	}
	out := make([]domain.RetentionEvaluation, 0, len(ids))
	for _, id := range ids {
		ev := domain.RetentionEvaluation{RecordID: id, RecordClass: "LEGAL_CONTRACT", JurisdictionScope: "US-DE"}
		domain.ResolveRetention(&ev)
		out = append(out, ev)
	}
	return out, nil
}

func (s *retStub) CreateDispositionCandidate(_ context.Context, p domain.CreateDispositionCandidateParams) (*domain.RecordRetentionState, error) {
	if s.candErr != nil {
		return nil, s.candErr
	}
	s.candidate = append(s.candidate, p)
	return &domain.RecordRetentionState{RetentionStateID: "rs-1", RecordID: p.RecordID, State: domain.RetentionReviewRequired}, nil
}

func (s *retStub) ApproveForDisposition(_ context.Context, p domain.ApproveForDispositionParams) (*domain.RecordRetentionState, error) {
	if s.approveErr != nil {
		return nil, s.approveErr
	}
	s.approved = append(s.approved, p)
	ref := p.ApprovalRef
	st := &domain.RecordRetentionState{RetentionStateID: p.RetentionStateID, RecordID: "rec-9", State: domain.RetentionApprovedForDisposition}
	if ref != "" {
		st.DispositionApprovalRef = &ref
	}
	return st, nil
}

// entityActionAuthz records (legal entity, action) for every decision.
type entityActionAuthz struct {
	denyEntity string
	denyAll    bool
	err        error
	asked      []string
}

func (a *entityActionAuthz) CheckAllowed(_ context.Context, _, legalEntityID, action string) error {
	a.asked = append(a.asked, legalEntityID+"|"+action)
	if a.err != nil {
		return a.err
	}
	if a.denyAll || (a.denyEntity != "" && legalEntityID == a.denyEntity) {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func retRouter(t *testing.T, az *entityActionAuthz, ret *retStub, identity bool) http.Handler {
	return drcRouter(t, newStubStore(), az, nil, nil, ret, identity)
}

// ── retention:evaluate ───────────────────────────────────────────────────────

func TestRetentionEvaluate_ReturnsResolutionPerRecord(t *testing.T) {
	ret := &retStub{entities: map[string]string{"rec-1": "ent-A", "rec-2": "ent-A"}}
	az := &entityActionAuthz{}
	r := retRouter(t, az, ret, true)

	w := call(r, http.MethodPost, "/v1/retention:evaluate", `{"record_ids":["rec-1","rec-2","rec-1"," "]}`, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Results []struct {
			RecordID   string `json:"record_id"`
			Resolution string `json:"resolution"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Results, 2, "duplicates and blanks are dropped")
	assert.Equal(t, "NO_APPLICABLE_RULE", body.Results[0].Resolution)
	assert.Equal(t, [][]string{{"rec-1", "rec-2"}}, ret.evalCalls)
	assert.Equal(t, []string{"ent-A|RETENTION_RULE_RESOLVE"}, az.asked, "one decision per distinct owning entity, on the registered action")
}

func TestRetentionEvaluate_AuthorizesEveryOwningEntity_BeforeAnyResult(t *testing.T) {
	ret := &retStub{entities: map[string]string{"rec-1": "ent-A", "rec-2": "ent-B"}}
	az := &entityActionAuthz{denyEntity: "ent-B"}
	r := retRouter(t, az, ret, true)

	w := call(r, http.MethodPost, "/v1/retention:evaluate", `{"record_ids":["rec-1","rec-2"]}`, nil)
	assert.Equal(t, http.StatusForbidden, w.Code, "denied on one record's entity refuses the whole call")
	assert.Empty(t, ret.evalCalls, "no rule or schedule is read for a call that is not fully authorized")
	assert.ElementsMatch(t, []string{"ent-A|RETENTION_RULE_RESOLVE", "ent-B|RETENTION_RULE_RESOLVE"}, az.asked)
}

func TestRetentionEvaluate_FailsClosed(t *testing.T) {
	// authorization outage
	ret := &retStub{entities: map[string]string{"rec-1": "ent-A"}}
	az := &entityActionAuthz{err: domain.ErrAuthzServiceUnavailable}
	w := call(retRouter(t, az, ret, true), http.MethodPost, "/v1/retention:evaluate", `{"record_ids":["rec-1"]}`, nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Empty(t, ret.evalCalls)

	// a record outside the tenant: the whole call is "not found", nothing evaluated
	ret = &retStub{entities: map[string]string{"rec-1": "ent-A"}}
	az = &entityActionAuthz{}
	w = call(retRouter(t, az, ret, true), http.MethodPost, "/v1/retention:evaluate", `{"record_ids":["rec-1","rec-foreign"]}`, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, ret.evalCalls)
	assert.Empty(t, az.asked, "authorization is not asked about records that do not resolve")

	// identity required
	ret = &retStub{entities: map[string]string{"rec-1": "ent-A"}}
	w = call(retRouter(t, &entityActionAuthz{}, ret, false), http.MethodPost, "/v1/retention:evaluate", `{"record_ids":["rec-1"]}`, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, ret.evalCalls)
}

func TestRetentionEvaluate_ValidatesTheRequest(t *testing.T) {
	ret := &retStub{entities: map[string]string{"rec-1": "ent-A"}}
	r := retRouter(t, &entityActionAuthz{}, ret, true)
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/retention:evaluate", `{}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/retention:evaluate", `{"record_ids":[]}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/retention:evaluate", `{"record_ids":["rec-1"],"as_of":"x"}`, nil).Code, "unknown fields are refused")

	many := make([]string, 101)
	for i := range many {
		many[i] = fmt.Sprintf("rec-%d", i)
	}
	b, _ := json.Marshal(map[string]any{"record_ids": many})
	w := call(r, http.MethodPost, "/v1/retention:evaluate", string(b), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "too_many_records")
	assert.Empty(t, ret.evalCalls)
}

func TestRetentionEvaluate_ConflictIsSurfacedNotResolved(t *testing.T) {
	ev := domain.RetentionEvaluation{RecordID: "rec-1", RecordClass: "EMPLOYMENT_RECORD", JurisdictionScope: "GB",
		ApplicableRules: []domain.RetentionRuleSummary{
			{RetentionRuleVersionID: "r-short", DurationDays: 365, TriggerType: domain.TriggerEmploymentEnd, DispositionAction: domain.DispositionDelete},
			{RetentionRuleVersionID: "r-long", DurationDays: 2190, TriggerType: domain.TriggerEmploymentEnd, DispositionAction: domain.DispositionDelete}}}
	domain.ResolveRetention(&ev)
	ret := &retStub{entities: map[string]string{"rec-1": "ent-A"}, evalResult: []domain.RetentionEvaluation{ev}}
	w := call(retRouter(t, &entityActionAuthz{}, ret, true), http.MethodPost, "/v1/retention:evaluate", `{"record_ids":["rec-1"]}`, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"resolution":"RETENTION_CONFLICT"`)
	assert.Contains(t, w.Body.String(), `"conflict_code":"RETENTION_CONFLICT"`)
	assert.Contains(t, w.Body.String(), "r-short")
	assert.Contains(t, w.Body.String(), "r-long")
}

// ── dispositions ─────────────────────────────────────────────────────────────

const candidateBody = `{"record_id":"rec-9","expected_retention_state":"DUE","review_notes":"elapsed"}`

func TestDispositionCreate_CreatesACandidate(t *testing.T) {
	ret := &retStub{}
	az := &entityActionAuthz{}
	w := call(retRouter(t, az, ret, true), http.MethodPost, "/v1/dispositions", candidateBody, nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var v map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v))
	assert.Equal(t, "rs-1", v["disposition_id"])
	assert.Equal(t, "rec-9", v["record_id"])
	assert.Equal(t, "REVIEW_REQUIRED", v["state"])
	require.Len(t, ret.candidate, 1)
	assert.Equal(t, domain.RetentionDue, ret.candidate[0].ExpectedState)
	assert.Equal(t, testPrincipal, ret.candidate[0].CreatedByPrincipalID)
	assert.Equal(t, []string{"|RETENTION_STATE_MANAGE"}, az.asked)
}

func TestDispositionCreate_RefusalsAndErrors(t *testing.T) {
	// required inputs
	ret := &retStub{}
	r := retRouter(t, &entityActionAuthz{}, ret, true)
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/dispositions", `{"expected_retention_state":"DUE"}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/dispositions", `{"record_id":"rec-9"}`, nil).Code, "the expected retention state is required")
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/dispositions", `{"record_id":"rec-9","expected_retention_state":"BOGUS"}`, nil).Code)
	assert.Empty(t, ret.candidate)

	// authorization and identity
	ret = &retStub{}
	assert.Equal(t, http.StatusForbidden, call(retRouter(t, &entityActionAuthz{denyAll: true}, ret, true), http.MethodPost, "/v1/dispositions", candidateBody, nil).Code)
	assert.Equal(t, http.StatusServiceUnavailable, call(retRouter(t, &entityActionAuthz{err: domain.ErrAuthzServiceUnavailable}, ret, true), http.MethodPost, "/v1/dispositions", candidateBody, nil).Code)
	assert.Equal(t, http.StatusUnauthorized, call(retRouter(t, &entityActionAuthz{}, ret, false), http.MethodPost, "/v1/dispositions", candidateBody, nil).Code)
	assert.Empty(t, ret.candidate)

	// legal hold: DRC-008, naming the hold
	ret = &retStub{candErr: fmt.Errorf("%w (hold hold-77)", domain.ErrRecordUnderLegalHold)}
	w := call(retRouter(t, &entityActionAuthz{}, ret, true), http.MethodPost, "/v1/dispositions", candidateBody, nil)
	assert.Equal(t, http.StatusConflict, w.Code)
	code, detail := errorCode(t, w)
	assert.Equal(t, "legal_hold_block", code)
	assert.Contains(t, detail, "DRC-008")
	assert.Contains(t, detail, "hold-77")

	// expected-state mismatch, not due, no state
	for name, tc := range map[string]struct {
		err  error
		want int
		code string
	}{
		"mismatch": {domain.ErrRetentionStateMismatch, http.StatusConflict, "retention_state_mismatch"},
		"not due":  {domain.ErrRetentionStateNotDue, http.StatusConflict, "conflict"},
		"no state": {domain.ErrRetentionStateNotFound, http.StatusNotFound, "not_found"},
	} {
		ret = &retStub{candErr: tc.err}
		w = call(retRouter(t, &entityActionAuthz{}, ret, true), http.MethodPost, "/v1/dispositions", candidateBody, nil)
		assert.Equal(t, tc.want, w.Code, name)
		code, _ = errorCode(t, w)
		assert.Equal(t, tc.code, code, name)
	}
}

func TestDispositionApprove_ConsumesTheApprovalDecision(t *testing.T) {
	ret := &retStub{}
	w := call(retRouter(t, &entityActionAuthz{}, ret, true), http.MethodPost, "/v1/dispositions/rs-5:approve", `{"approval_ref":"wfc-decision-42"}`, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, ret.approved, 1)
	assert.Equal(t, "rs-5", ret.approved[0].RetentionStateID, "the id before ':approve' is the disposition id")
	assert.Equal(t, "wfc-decision-42", ret.approved[0].ApprovalRef)
	assert.Equal(t, testPrincipal, ret.approved[0].ApprovedByPrincipalID)
	assert.Contains(t, w.Body.String(), `"disposition_approval_ref":"wfc-decision-42"`)
	assert.Contains(t, w.Body.String(), `"state":"APPROVED_FOR_DISPOSITION"`)
}

func TestDispositionApprove_RefusalsAndErrors(t *testing.T) {
	ret := &retStub{}
	r := retRouter(t, &entityActionAuthz{}, ret, true)
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/dispositions/rs-5:approve", `{}`, nil).Code, "the approval decision reference is required")
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/dispositions/rs-5:approve", `{"approval_ref":"`+strings.Repeat("x", 256)+`"}`, nil).Code)
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/dispositions/rs-5:approve", `{"approval_ref":"a","approved_by":"me"}`, nil).Code, "no self-declared approver is accepted")
	assert.Empty(t, ret.approved)

	ret = &retStub{}
	assert.Equal(t, http.StatusForbidden, call(retRouter(t, &entityActionAuthz{denyAll: true}, ret, true), http.MethodPost, "/v1/dispositions/rs-5:approve", `{"approval_ref":"a"}`, nil).Code)
	assert.Equal(t, http.StatusServiceUnavailable, call(retRouter(t, &entityActionAuthz{err: domain.ErrAuthzServiceUnavailable}, ret, true), http.MethodPost, "/v1/dispositions/rs-5:approve", `{"approval_ref":"a"}`, nil).Code)
	assert.Equal(t, http.StatusUnauthorized, call(retRouter(t, &entityActionAuthz{}, ret, false), http.MethodPost, "/v1/dispositions/rs-5:approve", `{"approval_ref":"a"}`, nil).Code)
	assert.Empty(t, ret.approved)

	ret = &retStub{approveErr: fmt.Errorf("%w (hold hold-3)", domain.ErrRecordUnderLegalHold)}
	w := call(retRouter(t, &entityActionAuthz{}, ret, true), http.MethodPost, "/v1/dispositions/rs-5:approve", `{"approval_ref":"a"}`, nil)
	assert.Equal(t, http.StatusConflict, w.Code)
	_, detail := errorCode(t, w)
	assert.Contains(t, detail, "DRC-008")
	assert.Contains(t, detail, "hold-3")

	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"not review required": {domain.ErrRetentionStateNotReviewRequired, http.StatusConflict},
		"unknown id":          {domain.ErrRetentionStateNotFound, http.StatusNotFound},
	} {
		ret = &retStub{approveErr: tc.err}
		w = call(retRouter(t, &entityActionAuthz{}, ret, true), http.MethodPost, "/v1/dispositions/rs-5:approve", `{"approval_ref":"a"}`, nil)
		assert.Equal(t, tc.want, w.Code, name)
	}
}

func TestDispositionRoutes_DoNotShadowTheExistingRetentionRoutes(t *testing.T) {
	ret := &retStub{}
	r := retRouter(t, &entityActionAuthz{}, ret, true)
	w := call(r, http.MethodPost, "/v1/retention-states/rs-7/approve-for-disposition", `{}`, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, ret.approved, 1)
	assert.Equal(t, "rs-7", ret.approved[0].RetentionStateID)
	assert.Empty(t, ret.approved[0].ApprovalRef, "the older route sends no reference")
	_ = httptest.NewRecorder
}
