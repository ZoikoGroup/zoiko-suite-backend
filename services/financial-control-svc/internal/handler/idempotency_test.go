package handler_test

// SPEC_DEVIATIONS A3/B8: Idempotency-Key is mandatory on certification,
// resubmission and exception transitions.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/domain"
)

type idemCase struct {
	path, body, storeCall string
	store                 func() *fakeStore
	ifMatch               string
}

func idemCases() map[string]idemCase {
	return map[string]idemCase{
		"certification": {"/controls/v1/runs/r1/certification", `{"decision":"CERTIFY"}`, "CertifyRun",
			func() *fakeStore { return &fakeStore{run: sampleRun()} }, `"3"`},
		"submit-for-certification": {"/controls/v1/runs/r1/submit-for-certification", `{"reason":"memo attached"}`, "SubmitForCertification",
			func() *fakeStore { return &fakeStore{run: sampleRun()} }, `"3"`},
		"exception transition (reperformance)": {"/controls/v1/exceptions/x1/transition",
			`{"to_state":"REPERFORMED","reason":"rerun clean","evidence_ref":"run-2"}`, "ResolveException",
			func() *fakeStore { return &fakeStore{w1: wave1State{exception: openExc()}} }, `"2"`},
	}
}

func TestIdempotencyKey_RequiredBeforeAnythingIsRead(t *testing.T) {
	for name, c := range idemCases() {
		s := c.store()
		rr := do(newServer(s, &fakeAuthz{}), "POST", c.path, c.body, with(map[string]string{"If-Match": c.ifMatch}))
		assert.Equal(t, 400, rr.Code, name)
		assert.Contains(t, rr.Body.String(), "missing_idempotency_key", name)
		assert.NotContains(t, s.calls, c.storeCall, name)

		rr = do(newServer(s, &fakeAuthz{}), "POST", c.path, c.body,
			with(map[string]string{"If-Match": c.ifMatch, "Idempotency-Key": strings.Repeat("k", 256)}))
		assert.Equal(t, 400, rr.Code, name+": an over-long key")
	}
}

func TestIdempotencyKey_ForwardedAndReplayMarked(t *testing.T) {
	for name, c := range idemCases() {
		s := c.store()
		hdr := with(map[string]string{"If-Match": c.ifMatch, "Idempotency-Key": "key-123"})
		rr := do(newServer(s, &fakeAuthz{}), "POST", c.path, c.body, hdr)
		require.Equal(t, 200, rr.Code, name+": "+rr.Body.String())
		assert.Equal(t, "key-123", s.idemKey, name)
		assert.Empty(t, rr.Header().Get("Idempotent-Replay"), name)

		s = c.store()
		s.idemReplay = true
		rr = do(newServer(s, &fakeAuthz{}), "POST", c.path, c.body, hdr)
		require.Equal(t, 200, rr.Code, name)
		assert.Equal(t, "true", rr.Header().Get("Idempotent-Replay"), name)
		assert.NotEmpty(t, rr.Header().Get("ETag"), name)
	}
}

// A key reused for something else is the store's ErrConflict: 409.
func TestIdempotencyKey_ReuseIsAConflict(t *testing.T) {
	s := &fakeStore{run: sampleRun()}
	s.w5.certErr = domain.ErrConflict
	rr := do(newServer(s, &fakeAuthz{}), "POST", "/controls/v1/runs/r1/certification", `{"decision":"CERTIFY"}`,
		with(map[string]string{"If-Match": `"3"`, "Idempotency-Key": "used-elsewhere"}))
	assert.Equal(t, 409, rr.Code)
}
