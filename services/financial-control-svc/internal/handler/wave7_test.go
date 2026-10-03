package handler_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
)

func openExc() *domain.ControlException {
	owner := "olivia"
	return &domain.ControlException{ExceptionID: "x1", LegalEntityID: "e1", RunID: "r1", Version: 2,
		State: domain.ExInvestigating, OwnerPrincipal: &owner, Severity: domain.SeverityMedium}
}

func TestResolve_RequiresIfMatchAndACompleteRequest(t *testing.T) {
	s := &fakeStore{w1: wave1State{exception: openExc()}}
	h := newServer(s, &fakeAuthz{})
	p := "/controls/v1/exceptions/x1/transition"
	assert.Equal(t, 428, do(h, "POST", p, `{"to_state":"INVESTIGATING","reason":"r"}`, auth).Code)
	hdr := with(map[string]string{"If-Match": `"2"`})
	assert.Equal(t, 400, do(h, "POST", p, `{"to_state":"WAIVED_UNDER_AUTHORITY","reason":"r"}`, hdr).Code, "a waiver needs its authority")
	assert.Equal(t, 400, do(h, "POST", p, `{"to_state":"REMEDIATED","reason":"r"}`, hdr).Code, "a remediation needs its evidence")
	assert.NotContains(t, s.calls, "ResolveException")
}

func TestResolve_ActionDependsOnTheTargetState(t *testing.T) {
	cases := map[string]struct{ body, action string }{
		"work":    {`{"to_state":"AWAITING_EVIDENCE","reason":"r"}`, "FINCTRL_EXCEPTION_RESOLVE"},
		"waive":   {`{"to_state":"WAIVED_UNDER_AUTHORITY","reason":"r","authority_ref":"CFO-1"}`, "FINCTRL_EXCEPTION_WAIVE"},
		"reperf":  {`{"to_state":"REPERFORMED","reason":"r","evidence_ref":"run-2"}`, "FINCTRL_REPERFORM"},
		"carry":   {`{"to_state":"CARRIED_FORWARD_UNDER_AUTHORITY","reason":"r","authority_ref":"CFO-1","carry_to_period":"2026-10"}`, "FINCTRL_EXCEPTION_WAIVE"},
		"closing": {`{"to_state":"CLOSED","reason":"r"}`, "FINCTRL_EXCEPTION_RESOLVE"},
	}
	for name, c := range cases {
		s := &fakeStore{w1: wave1State{exception: openExc()}}
		a := &fakeAuthz{}
		rr := do(newServer(s, a), "POST", "/controls/v1/exceptions/x1/transition", c.body, with(map[string]string{"If-Match": `"2"`}))
		require.Equal(t, 200, rr.Code, name+": "+rr.Body.String())
		assert.Equal(t, []string{c.action}, a.actions, name)
		assert.Equal(t, []string{"e1"}, a.entity, name)
		assert.Equal(t, 2, s.w7.resolveExpect, name)
		assert.Equal(t, `"3"`, rr.Header().Get("ETag"), name)
	}
}

func TestResolve_DeniedNeverReachesTheStore(t *testing.T) {
	s := &fakeStore{w1: wave1State{exception: openExc()}}
	rr := do(newServer(s, &fakeAuthz{err: domain.ErrAuthorizationDenied}), "POST", "/controls/v1/exceptions/x1/transition",
		`{"to_state":"CLOSED","reason":"r"}`, with(map[string]string{"If-Match": `"2"`}))
	assert.Equal(t, 403, rr.Code)
	assert.NotContains(t, s.calls, "ResolveException")
}

func TestResolve_IndependenceAndConflictMapping(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
	}{{domain.ErrNotOwner, 403}, {domain.ErrNotIndependent, 403}, {domain.ErrConflict, 409}, {domain.ErrInvalidTransition, 422}, {domain.ErrNotFound, 404}} {
		s := &fakeStore{w1: wave1State{exception: openExc()}, w7: wave7State{resolveErr: c.err}}
		rr := do(newServer(s, &fakeAuthz{}), "POST", "/controls/v1/exceptions/x1/transition",
			`{"to_state":"CLOSED","reason":"r"}`, with(map[string]string{"If-Match": `"2"`}))
		assert.Equal(t, c.code, rr.Code, c.err.Error())
	}
}

func TestSubmitForCertification_NeedsReasonAndIfMatch(t *testing.T) {
	s := &fakeStore{run: sampleRun()}
	h := newServer(s, &fakeAuthz{})
	p := "/controls/v1/runs/r1/submit-for-certification"
	assert.Equal(t, 428, do(h, "POST", p, `{"reason":"fixed"}`, auth).Code)
	hdr := with(map[string]string{"If-Match": `"3"`})
	assert.Equal(t, 400, do(h, "POST", p, `{}`, hdr).Code)
	rr := do(h, "POST", p, `{"reason":"evidence added"}`, hdr)
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assert.Contains(t, s.calls, "SubmitForCertification")
}
