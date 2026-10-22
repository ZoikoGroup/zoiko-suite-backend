package handler_test

// SPEC_DEVIATIONS A1/B6: revoking a certification.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
)

type revokeState struct {
	err      error
	replay   bool
	expected int
	key      string
	req      domain.RevokeCertificationRequest
}

func revokeOf(f *fakeStore) *revokeState { return &f.rv }

func (f *fakeStore) RevokeCertification(_ context.Context, _, _, _, _, key string, expected int, req domain.RevokeCertificationRequest) (*domain.ControlRun, bool, error) {
	f.calls = append(f.calls, "RevokeCertification")
	st := revokeOf(f)
	st.key, st.expected, st.req = key, expected, req
	if st.err != nil {
		return nil, false, st.err
	}
	c := *f.run
	c.Version++
	c.LifecycleState, c.CertificationState = domain.LifecycleCertified, domain.CertRevoked
	return &c, st.replay, nil
}

const revokePath = "/controls/v1/runs/r1/revoke-certification"
const revokeBody = `{"reason":"source totals restated after certification","evidence_refs":["doc:restatement-123"]}`

func revokeHeaders(kv ...string) map[string]string {
	m := map[string]string{"If-Match": `"3"`, "Idempotency-Key": "k-1"}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return with(m)
}

func TestRevokeCertification_RequiresKeyIfMatchReasonAndEvidence(t *testing.T) {
	s := &fakeStore{run: sampleRun()}
	h := newServer(s, &fakeAuthz{})
	assert.Equal(t, 400, do(h, "POST", revokePath, revokeBody, revokeHeaders("Idempotency-Key", "")).Code, "Idempotency-Key is mandatory")
	assert.Equal(t, 428, do(h, "POST", revokePath, revokeBody, revokeHeaders("If-Match", "")).Code, "If-Match is mandatory")
	for name, body := range map[string]string{
		"no reason":          `{"evidence_refs":["doc:1"]}`,
		"blank reason":       `{"reason":"  ","evidence_refs":["doc:1"]}`,
		"no evidence":        `{"reason":"wrong"}`,
		"empty evidence":     `{"reason":"wrong","evidence_refs":[]}`,
		"blank evidence ref": `{"reason":"wrong","evidence_refs":[" "]}`,
	} {
		assert.Equal(t, 400, do(h, "POST", revokePath, body, revokeHeaders()).Code, name)
	}
	assert.NotContains(t, s.calls, "RevokeCertification")
}

// Its own action, not FINCTRL_CERTIFY, checked against the run's entity.
func TestRevokeCertification_AuthorizedWithRevokeActionOnTheRunsEntity(t *testing.T) {
	s := &fakeStore{run: sampleRun()}
	a := &fakeAuthz{}
	rr := do(newServer(s, a), "POST", revokePath, revokeBody, revokeHeaders())
	require.Equal(t, 200, rr.Code, rr.Body.String())
	assert.Equal(t, []string{"FINCTRL_REVOKE"}, a.actions)
	assert.Equal(t, []string{"e1"}, a.entity)
	st := revokeOf(s)
	assert.Equal(t, 3, st.expected)
	assert.Equal(t, "k-1", st.key)
	assert.Equal(t, []string{"doc:restatement-123"}, st.req.EvidenceRefs)
	assert.Equal(t, `"4"`, rr.Header().Get("ETag"))
	assert.Empty(t, rr.Header().Get("Idempotent-Replay"))

	s2 := &fakeStore{run: sampleRun()}
	rr = do(newServer(s2, &fakeAuthz{err: domain.ErrAuthorizationDenied}), "POST", revokePath, revokeBody, revokeHeaders())
	assert.Equal(t, 403, rr.Code)
	assert.NotContains(t, s2.calls, "RevokeCertification")
}

func TestRevokeCertification_ReplayIsMarked(t *testing.T) {
	s := &fakeStore{run: sampleRun()}
	revokeOf(s).replay = true
	rr := do(newServer(s, &fakeAuthz{}), "POST", revokePath, revokeBody, revokeHeaders())
	require.Equal(t, 200, rr.Code)
	assert.Equal(t, "true", rr.Header().Get("Idempotent-Replay"))
}

func TestRevokeCertification_ErrorMapping(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
	}{
		{domain.ErrRevokerIsCertifier, 403},
		{domain.ErrConflict, 409},
		{domain.ErrInvalidTransition, 422},
		{domain.ErrNotFound, 404},
	} {
		s := &fakeStore{run: sampleRun()}
		revokeOf(s).err = c.err
		assert.Equal(t, c.code, do(newServer(s, &fakeAuthz{}), "POST", revokePath, revokeBody, revokeHeaders()).Code, c.err.Error())
	}
}

func TestCheckRevokerIndependence(t *testing.T) {
	assert.ErrorIs(t, domain.CheckRevokerIndependence("cert-1", []string{"cert-1"}), domain.ErrRevokerIsCertifier)
	assert.NoError(t, domain.CheckRevokerIndependence("auditor", []string{"cert-1"}))
}

// The revocation guard moves only the certification dimension, only from a
// CERTIFIED run, and REVOKED has no way out.
func TestValidateCertificationRevocation(t *testing.T) {
	assert.NoError(t, domain.ValidateCertificationRevocation(domain.LifecycleCertified, domain.CertCertified))
	assert.ErrorIs(t, domain.ValidateCertificationRevocation(domain.LifecycleCertified, domain.CertRevoked), domain.ErrInvalidTransition)
	assert.ErrorIs(t, domain.ValidateCertificationRevocation(domain.LifecycleReadyToCertify, domain.CertPending), domain.ErrInvalidTransition)
	assert.ErrorIs(t, domain.ValidateCertificationRevocation(domain.LifecycleSuperseded, domain.CertSuperseded), domain.ErrInvalidTransition)
	for _, to := range []domain.CertificationState{domain.CertPending, domain.CertCertified, domain.CertSuperseded, domain.CertRejected} {
		assert.Error(t, domain.ValidateCertificationTransition(domain.CertRevoked, to), "REVOKED is terminal")
	}
}

func TestEvaluateGate_RevokedCertificationBlocksWithItsOwnReason(t *testing.T) {
	g := domain.EvaluateGate("e1", "2026-09", []domain.GateInput{
		{ControlCode: "C1", RunID: "r1", Lifecycle: "CERTIFIED", Result: "PASS", Certification: "CERTIFIED"},
		{ControlCode: "C2", RunID: "r2", Lifecycle: "CERTIFIED", Result: "PASS", Certification: "REVOKED",
			RevokedAt: "2026-10-19T10:00:00.000000Z", RevocationReason: "restated"},
	})
	assert.False(t, g.Open)
	assert.Equal(t, 1, g.Blocking)
	it := g.Items[1]
	assert.Equal(t, domain.GateBlocking, it.Status)
	assert.Equal(t, "CERTIFICATION_REVOKED", it.Reason)
	assert.Equal(t, "2026-10-19T10:00:00.000000Z", it.CertificationRevokedAt)
	assert.Equal(t, "restated", it.RevocationReason)
	assert.Equal(t, "REVOKED", it.Certificatio)
}
