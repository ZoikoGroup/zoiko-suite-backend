//go:build integration

package store_test

// SPEC_DEVIATIONS A3/B8: certification, resubmission and exception
// transitions are idempotent on Idempotency-Key, through command_idempotency.

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/catalogue"
	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/store"
)

func countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, testPool.QueryRow(ctx, sql, args...).Scan(&n))
	return n
}

func readyRun(t *testing.T, w *wave2) *domain.ControlRun {
	t.Helper()
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	_, final := w.run("FIN-CTRL-020", fiscal)
	require.Equal(t, domain.LifecycleReadyToCertify, final.LifecycleState)
	return final
}

// The deviation's own case: a client retrying a CERTIFY whose response was lost
// gets the original result, not a 409/412 on the run that has moved on.
func TestIdempotency_CertifyRetryReplaysTheOriginalResult(t *testing.T) {
	w := newWave5(t)
	run := readyRun(t, w)
	key := "cert-" + uuid.NewString()
	req := domain.CertifyRequest{Decision: "CERTIFY", Reason: "reviewed"}

	first, replay, err := testStore.CertifyRun(ctx, w.tenant, run.RunID, "checker", "c", key, run.Version, req, testAuthority("checker", w.tenant))
	require.NoError(t, err)
	require.False(t, replay)
	transitions := countRows(t, `SELECT count(*) FROM control_run_transitions WHERE run_id = $1`, run.RunID)

	// The retry carries the If-Match the client saw before the first attempt.
	again, replay, err := testStore.CertifyRun(ctx, w.tenant, run.RunID, "checker", "c", key, run.Version, req, testAuthority("checker", w.tenant))
	require.NoError(t, err, "a retry is not a version conflict")
	assert.True(t, replay)
	assert.Equal(t, first.Version, again.Version)
	assert.Equal(t, domain.CertCertified, again.CertificationState)
	assert.Equal(t, transitions, countRows(t, `SELECT count(*) FROM control_run_transitions WHERE run_id = $1`, run.RunID),
		"nothing is applied twice")
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM control_certifications WHERE run_id = $1`, run.RunID))
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'control.run.certified'`, run.RunID))

	// The same key for the other decision, or another run, is a conflict.
	_, _, err = testStore.CertifyRun(ctx, w.tenant, run.RunID, "checker", "c", key, store.SkipVersionCheck,
		domain.CertifyRequest{Decision: "REJECT", Reason: "changed my mind"}, nil)
	require.ErrorIs(t, err, domain.ErrConflict)
	other := readyRun(t, w)
	_, _, err = testStore.CertifyRun(ctx, w.tenant, other.RunID, "checker", "c", key, other.Version, req, testAuthority("checker", w.tenant))
	require.ErrorIs(t, err, domain.ErrConflict)
}

// A refused command never consumes its key.
func TestIdempotency_RefusedCertificationDoesNotConsumeTheKey(t *testing.T) {
	w := newWave5(t)
	run := readyRun(t, w)
	key := "cert-" + uuid.NewString()
	_, _, err := testStore.CertifyRun(ctx, w.tenant, run.RunID, "prep", "c", key, run.Version,
		domain.CertifyRequest{Decision: "CERTIFY"}, testAuthority("prep", w.tenant))
	require.ErrorIs(t, err, domain.ErrSegregation)
	assert.Zero(t, countRows(t, `SELECT count(*) FROM command_idempotency WHERE idempotency_key = $1`, key))

	_, replay, err := testStore.CertifyRun(ctx, w.tenant, run.RunID, "checker", "c", key, run.Version,
		domain.CertifyRequest{Decision: "CERTIFY"}, testAuthority("checker", w.tenant))
	require.NoError(t, err)
	assert.False(t, replay, "the key was never claimed, so this is a first attempt")
}

// Two concurrent first attempts with one key: one applies, the other waits on the
// run's row lock and then replays it — it is not turned into a version conflict.
func TestIdempotency_ConcurrentFirstAttemptsReplayRatherThanConflict(t *testing.T) {
	w := newWave5(t)
	run := readyRun(t, w)
	key := "cert-" + uuid.NewString()
	var wg sync.WaitGroup
	replays := make([]bool, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, replays[i], errs[i] = testStore.CertifyRun(ctx, w.tenant, run.RunID, "checker", "c", key, run.Version,
				domain.CertifyRequest{Decision: "CERTIFY"}, testAuthority("checker", w.tenant))
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.True(t, replays[0] != replays[1], "exactly one attempt applies, the other replays: %v", replays)
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM control_certifications WHERE run_id = $1`, run.RunID))
}

func TestIdempotency_SubmitForCertificationReplays(t *testing.T) {
	w := newWave5(t)
	run := readyRun(t, w)
	rej, err := certifyRun(ctx, w.tenant, run.RunID, "checker", "c", run.Version,
		domain.CertifyRequest{Decision: "REJECT", Reason: "attach the memo"}, nil)
	require.NoError(t, err)

	key := "submit-" + uuid.NewString()
	sub, replay, err := testStore.SubmitForCertification(ctx, w.tenant, run.RunID, "prep", "c", key, rej.Version, "memo attached")
	require.NoError(t, err)
	require.False(t, replay)
	again, replay, err := testStore.SubmitForCertification(ctx, w.tenant, run.RunID, "prep", "c", key, rej.Version, "memo attached")
	require.NoError(t, err)
	assert.True(t, replay)
	assert.Equal(t, sub.Version, again.Version, "the resubmission is not applied twice")
	assert.Equal(t, domain.CertPending, again.CertificationState)
}

// Exception transitions are keyed to the exception and target state, so the
// same key on another exception of the same run is a conflict, not a replay.
// REPERFORMED — reperformance, mandatorily idempotent — replays like the rest.
func TestIdempotency_ExceptionTransitionsAreKeyedToTheException(t *testing.T) {
	w := newWave5(t)
	_, exc := failedRunWithTwoExceptions(t, w) // both assigned to olivia
	x, y := exc[0], exc[1]

	key := "ex-" + uuid.NewString()
	inv, replay, err := testStore.ResolveException(ctx, w.tenant, x.ExceptionID, "olivia", "c", key, x.Version,
		domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "looking"})
	require.NoError(t, err)
	require.False(t, replay)
	again, replay, err := testStore.ResolveException(ctx, w.tenant, x.ExceptionID, "olivia", "c", key, x.Version,
		domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "looking"})
	require.NoError(t, err)
	assert.True(t, replay)
	assert.Equal(t, inv.Version, again.Version)
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM exception_transitions WHERE exception_id = $1 AND to_state = 'INVESTIGATING'`, x.ExceptionID))

	_, _, err = testStore.ResolveException(ctx, w.tenant, y.ExceptionID, "olivia", "c", key, y.Version,
		domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "looking"})
	require.ErrorIs(t, err, domain.ErrConflict, "same run, other exception")
	_, _, err = testStore.ResolveException(ctx, w.tenant, x.ExceptionID, "olivia", "c", key, inv.Version,
		domain.ResolveExceptionRequest{ToState: "AWAITING_ADJUSTMENT", Reason: "need a journal"})
	require.ErrorIs(t, err, domain.ErrConflict, "same exception, other transition")

	// Through remediation to reperformance, then retry the reperformance.
	cur := *inv
	for _, s := range []struct{ actor, to, ev string }{
		{"olivia", "AWAITING_ADJUSTMENT", ""}, {"olivia", "REMEDIATED", "JE-9001"},
	} {
		out, err := resolveException(ctx, w.tenant, x.ExceptionID, s.actor, "c", cur.Version,
			domain.ResolveExceptionRequest{ToState: s.to, Reason: "r", EvidenceRef: s.ev})
		require.NoError(t, err, s.to)
		cur = *out
	}
	rkey := "reperf-" + uuid.NewString()
	rep := domain.ResolveExceptionRequest{ToState: "REPERFORMED", Reason: "rerun clean", EvidenceRef: "run-2"}
	_, replay, err = testStore.ResolveException(ctx, w.tenant, x.ExceptionID, "rita", "c", rkey, cur.Version, rep)
	require.NoError(t, err)
	require.False(t, replay)
	_, replay, err = testStore.ResolveException(ctx, w.tenant, x.ExceptionID, "rita", "c", rkey, cur.Version, rep)
	require.NoError(t, err)
	assert.True(t, replay)
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM exception_transitions WHERE exception_id = $1 AND to_state = 'REPERFORMED'`, x.ExceptionID))
}

// One key, one mechanism: a key already used to execute a run cannot be reused
// to certify it.
func TestIdempotency_ExecutionKeyCannotBeReusedForCertification(t *testing.T) {
	w := newWave5(t)
	run := readyRun(t, w)
	var execKey string
	require.NoError(t, testPool.QueryRow(ctx, `SELECT idempotency_key FROM command_idempotency
		WHERE run_id = $1 AND command = 'EXECUTE'`, run.RunID).Scan(&execKey))
	_, _, err := testStore.CertifyRun(ctx, w.tenant, run.RunID, "checker", "c", execKey, run.Version,
		domain.CertifyRequest{Decision: "CERTIFY"}, testAuthority("checker", w.tenant))
	require.ErrorIs(t, err, domain.ErrConflict)
}
