package handler_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
)

type wave5State struct {
	certErr    error
	certExpect int
	certReq    domain.CertifyRequest
	gate       *domain.CloseGate
	summary    *domain.ExceptionSummary
}

func (f *fakeStore) CertifyRun(_ context.Context, _, _, _, _ string, expected int, req domain.CertifyRequest) (*domain.ControlRun, error) {
	f.calls = append(f.calls, "CertifyRun")
	f.w5.certExpect, f.w5.certReq = expected, req
	if f.w5.certErr != nil {
		return nil, f.w5.certErr
	}
	c := *f.run
	c.Version++
	c.LifecycleState, c.CertificationState = domain.LifecycleCertified, domain.CertCertified
	return &c, nil
}
func (f *fakeStore) CloseGate(context.Context, string, string, string) (*domain.CloseGate, error) {
	f.calls = append(f.calls, "CloseGate")
	return f.w5.gate, nil
}
func (f *fakeStore) ExceptionSummary(context.Context, string, string, string) (*domain.ExceptionSummary, error) {
	f.calls = append(f.calls, "ExceptionSummary")
	return f.w5.summary, nil
}

const entityUUID = "22222222-2222-2222-2222-222222222222"

func TestCertify_RequiresIfMatchAndValidDecision(t *testing.T) {
	s := &fakeStore{run: sampleRun()}
	h := newServer(s, &fakeAuthz{})
	assert.Equal(t, 428, do(h, "POST", "/controls/v1/runs/r1/certification", `{"decision":"CERTIFY"}`, auth).Code)
	hdr := with(map[string]string{"If-Match": `"3"`})
	assert.Equal(t, 400, do(h, "POST", "/controls/v1/runs/r1/certification", `{"decision":"MAYBE"}`, hdr).Code)
	assert.Equal(t, 400, do(h, "POST", "/controls/v1/runs/r1/certification", `{"decision":"REJECT"}`, hdr).Code, "a rejection needs a reason")
	assert.NotContains(t, s.calls, "CertifyRun")
}

func TestCertify_AuthorizedAgainstTheRunsEntityWithCertifyAction(t *testing.T) {
	s := &fakeStore{run: sampleRun()}
	a := &fakeAuthz{}
	rr := do(newServer(s, a), "POST", "/controls/v1/runs/r1/certification", `{"decision":"CERTIFY"}`, with(map[string]string{"If-Match": `"3"`}))
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assert.Equal(t, []string{"FINCTRL_CERTIFY"}, a.actions)
	assert.Equal(t, []string{"e1"}, a.entity)
	assert.Equal(t, 3, s.w5.certExpect)
	assert.Equal(t, `"4"`, rr.Header().Get("ETag"))
}

func TestCertify_DeniedNeverReachesTheStore(t *testing.T) {
	s := &fakeStore{run: sampleRun()}
	rr := do(newServer(s, &fakeAuthz{err: domain.ErrAuthorizationDenied}), "POST", "/controls/v1/runs/r1/certification",
		`{"decision":"CERTIFY"}`, with(map[string]string{"If-Match": `"3"`}))
	assert.Equal(t, 403, rr.Code)
	assert.NotContains(t, s.calls, "CertifyRun")
}

func TestCertify_SegregationConflictAndTransitionMapping(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
	}{{domain.ErrSegregation, 403}, {domain.ErrConflict, 409}, {domain.ErrInvalidTransition, 422}} {
		s := &fakeStore{run: sampleRun(), w5: wave5State{certErr: c.err}}
		rr := do(newServer(s, &fakeAuthz{}), "POST", "/controls/v1/runs/r1/certification", `{"decision":"CERTIFY"}`, with(map[string]string{"If-Match": `"3"`}))
		assert.Equal(t, c.code, rr.Code, c.err.Error())
	}
}

func TestPeriodReads_ValidateAndAuthorizeOnTheEntity(t *testing.T) {
	s := &fakeStore{w5: wave5State{gate: &domain.CloseGate{Items: []domain.CloseGateItem{}}, summary: &domain.ExceptionSummary{}}}
	a := &fakeAuthz{}
	h := newServer(s, a)
	for _, p := range []string{"/controls/v1/close-gate", "/controls/v1/exception-summary"} {
		assert.Equal(t, 400, do(h, "GET", p, "", auth).Code)
		assert.Equal(t, 400, do(h, "GET", p+"?legal_entity_id=not-a-uuid&period_id=2026-09", "", auth).Code)
		assert.Equal(t, 400, do(h, "GET", p+"?legal_entity_id="+entityUUID, "", auth).Code)
		rr := do(h, "GET", p+"?legal_entity_id="+entityUUID+"&period_id=2026-09", "", auth)
		require.Equal(t, 200, rr.Code, rr.Body.String())
	}
	assert.Equal(t, []string{"FINCTRL_READ", "FINCTRL_READ"}, a.actions)
	assert.Equal(t, []string{entityUUID, entityUUID}, a.entity)
}

func TestPeriodReads_DeniedNeverReachesTheStore(t *testing.T) {
	s := &fakeStore{}
	rr := do(newServer(s, &fakeAuthz{err: domain.ErrAuthorizationDenied}), "GET",
		"/controls/v1/close-gate?legal_entity_id="+entityUUID+"&period_id=2026-09", "", auth)
	assert.Equal(t, 403, rr.Code)
	assert.Empty(t, s.calls)
}
