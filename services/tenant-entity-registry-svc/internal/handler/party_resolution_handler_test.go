package handler_test

// MDM-05 routes. partySvc offers them too, so the OpenAPI parity checks cover them with the rest.

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const caseID = "0190a000-0000-7000-8000-00000000cccc"

type resolutionRec struct {
	merge  domain.MergePartiesRequest
	split  domain.SplitPartiesRequest
	decide domain.CaseDecisionRequest
	match  domain.MatchPartiesRequest

	status, caseType, after string
	limit                   int
}

var resRec = map[*partySvc]*resolutionRec{}

func (p *partySvc) rec() *resolutionRec {
	if resRec[p] == nil {
		resRec[p] = &resolutionRec{}
	}
	return resRec[p]
}

func (p *partySvc) MatchParties(_ context.Context, req domain.MatchPartiesRequest) (*domain.MatchPartiesResult, error) {
	p.rec().match = req
	return &domain.MatchPartiesResult{Verdict: "NO_MATCH", Candidates: []domain.MatchCandidate{}}, p.err
}

func (p *partySvc) ListResolutionCases(_ context.Context, status, caseType string, limit int, after string) ([]domain.ResolutionCase, string, error) {
	r := p.rec()
	r.status, r.caseType, r.limit, r.after = status, caseType, limit, after
	return nil, "n", p.err
}

func (p *partySvc) DecideResolutionCase(_ context.Context, id string, req domain.CaseDecisionRequest) (*registry.CaseDecisionOutcome, error) {
	p.gotID, p.rec().decide = id, req
	return &registry.CaseDecisionOutcome{}, p.err
}

func (p *partySvc) MergeParties(_ context.Context, id string, req domain.MergePartiesRequest) (*registry.MergeOutcome, error) {
	p.gotID, p.rec().merge = id, req
	return &registry.MergeOutcome{}, p.err
}

func (p *partySvc) SplitParties(_ context.Context, id string, req domain.SplitPartiesRequest) (*registry.SplitOutcome, error) {
	p.gotID, p.rec().split = id, req
	return &registry.SplitOutcome{}, p.err
}

func (p *partySvc) ExplainMergeHistory(_ context.Context, id string) (*domain.MergeHistory, error) {
	p.gotID = id
	return &domain.MergeHistory{PartyID: id, AliasIDs: []string{}, Lineage: []domain.LineageEvent{}}, p.err
}

func TestResolutionRoutes_ColonSuffixedCommandsReachTheService(t *testing.T) {
	p := &partySvc{}
	rec := do(newPartyRouter(p), http.MethodPost, "/v1/resolution/"+caseID+":merge",
		`{"survivor_party_id":"s","merged_party_ids":["m"],"expected_master_versions":{"s":2,"m":3},"reason":"r","evidence_ref":"e"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, caseID, p.gotID, "the case id is the path segment before the colon")
	assert.Equal(t, "s", p.rec().merge.SurvivorPartyID)
	assert.Equal(t, int64(3), p.rec().merge.ExpectedMasterVersions["m"])
	assert.Equal(t, corrID, p.rec().merge.CorrelationID)

	p = &partySvc{}
	rec = do(newPartyRouter(p), http.MethodPost, "/v1/resolution/"+caseID+":split",
		`{"merged_party_id":"m","expected_master_versions":{"m":3},"reason":"r","evidence_ref":"e"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, caseID, p.gotID)
	assert.Equal(t, "m", p.rec().split.MergedPartyID)

	assert.Equal(t, http.StatusNotFound, do(newPartyRouter(&partySvc{}), http.MethodPost, "/v1/resolution/"+caseID+":unknown", `{}`).Code)
}

func TestResolutionRoutes_RefusalsAreMapped(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
		code string
	}{
		{registry.ErrSelfApproval, http.StatusForbidden, "SOD_DENIED"},
		{registry.ErrMergeProhibited, http.StatusConflict, "MERGE_PROHIBITED"},
		{registry.ErrVersionConflict, http.StatusConflict, "VERSION_CONFLICT"},
		{registry.ErrStateConflict, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrInvalidInput, http.StatusBadRequest, "VALIDATION_FAILED"},
	} {
		rec := do(newPartyRouter(&partySvc{stubSvc: &stubSvc{err: tc.err}}), http.MethodPost, "/v1/resolution/"+caseID+":merge", `{}`)
		assert.Equal(t, tc.want, rec.Code, tc.err.Error())
		assert.Contains(t, rec.Body.String(), tc.code, tc.err.Error())
	}
}

func TestResolutionRoutes_MatchDecisionListAndHistory(t *testing.T) {
	p := &partySvc{}
	rec := do(newPartyRouter(p), http.MethodPost, "/v1/resolution:match", `{"party_kind":"LEGAL_ENTITY","names":[{"type":"LEGAL_NAME","value":"Nova"}]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, domain.PartyLegalEntity, p.rec().match.PartyKind)
	assert.Contains(t, rec.Body.String(), `"verdict":"NO_MATCH"`)

	p = &partySvc{}
	rec = do(newPartyRouter(p), http.MethodPost, "/v1/resolution-cases/"+caseID+"/decision",
		`{"decision":"DISMISS","note":"n","expected_case_version":1}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, caseID, p.gotID)
	assert.Equal(t, "DISMISS", p.rec().decide.Decision)

	p = &partySvc{}
	rec = do(newPartyRouter(p), http.MethodGet, "/v1/resolution-cases?status=OPEN&case_type=FACT_CONFLICT&limit=5&cursor=c", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "OPEN", p.rec().status)
	assert.Equal(t, "FACT_CONFLICT", p.rec().caseType)
	assert.Equal(t, 5, p.rec().limit)
	assert.Equal(t, "c", p.rec().after)
	assert.Contains(t, rec.Body.String(), `"items":[]`)
	for _, bad := range []string{"limit=0", "limit=201", "limit=x"} {
		assert.Equal(t, http.StatusBadRequest, do(newPartyRouter(&partySvc{}), http.MethodGet, "/v1/resolution-cases?"+bad, "").Code, bad)
	}

	p = &partySvc{}
	rec = do(newPartyRouter(p), http.MethodGet, "/v1/parties/"+partyID+"/merge-history", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, partyID, p.gotID)
	assert.Contains(t, rec.Body.String(), `"lineage":[]`)
}
