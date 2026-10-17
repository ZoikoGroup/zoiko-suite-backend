package store_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tax-authority-interface-svc/internal/authority"
)

func cbIn(event, ref, raw, mapped string) authority.CallbackInput {
	return authority.CallbackInput{Provider: "taxauth", Scheme: "hmac-sha256-hex", EventRef: event, SubmissionRef: ref, RawStatus: raw, Mapped: mapped,
		BodyHash: strings.Repeat("ab", 32), CorrelationID: "c"}
}

// sentFiling returns a filing the authority holds (SUBMITTED) or may hold (PENDING_UNKNOWN).
func (f *fixture) sentFiling(tenant string, unknown bool) *authority.Filing {
	f.t.Helper()
	fl := f.filing(tenant, f.iface(tenant, nil))
	ctx := tc(tenant)
	_, err := f.st.MarkValidated(ctx, fl.SubmissionID, "op", 1)
	require.NoError(f.t, err)
	_, err = f.st.BeginSend(ctx, fl.SubmissionID, authority.StatusValidated, 1, "h", "op")
	require.NoError(f.t, err)
	o := authority.Outcome{Kind: authority.OutSubmitted, From: authority.StatusSubmitting, AuthorityRef: "RC-" + fl.SubmissionID, Transport: entry("SUBMIT", "ANSWERED", "ACKNOWLEDGED")}
	if unknown {
		o = authority.Outcome{Kind: authority.OutUnknown, From: authority.StatusSubmitting, Failure: "no_answer", Transport: entry("SUBMIT", "UNKNOWN", "")}
	}
	got, err := f.st.Finish(ctx, fl.SubmissionID, o, "op", "c")
	require.NoError(f.t, err)
	return got
}

func (f *fixture) cbCount(tenant, event, outcome string) int {
	var n int
	require.NoError(f.t, f.admin.QueryRow(context.Background(),
		`SELECT count(*) FROM tax_authority_callbacks WHERE tenant_id = $1 AND event_ref = $2 AND outcome = $3`, tenant, event, outcome).Scan(&n))
	return n
}

func TestCallback_AcceptanceByTheAuthorityAndTheWholeTrail(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()
	fl := f.sentFiling(tenant, false)

	// by the authority's reference, or by our id
	res, err := f.st.ApplyCallback(tc(tenant), cbIn("E1", fl.AuthorityRef, "PROCESSING", "PENDING"))
	require.NoError(t, err)
	assert.Equal(t, authority.OutcomeStaleIgnored, res.Outcome, "still received: nothing changes")
	res, err = f.st.ApplyCallback(tc(tenant), cbIn("E2", fl.SubmissionID, "ACCEPTED", "ACCEPTED"))
	require.NoError(t, err)
	assert.Equal(t, authority.OutcomeApplied, res.Outcome)
	assert.Equal(t, authority.StatusAccepted, res.Filing.Status)
	assert.Equal(t, fl.SubmissionID, f.ack(fl.SubmissionID), "acceptance acknowledges the filing")
	assert.Equal(t, 1, f.outbox(tenant, fl.SubmissionID, "authority-attempt.response-received"))
	assert.Equal(t, 1, f.cbCount(tenant, "E2", authority.OutcomeApplied))
}

// acceptance: callback replay
func TestCallback_ReplayAndConcurrentCopiesApplyOnce(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()
	fl := f.sentFiling(tenant, false)
	res, _ := f.st.ApplyCallback(tc(tenant), cbIn("E1", fl.AuthorityRef, "ACCEPTED", "ACCEPTED"))
	require.Equal(t, authority.OutcomeApplied, res.Outcome)
	before := f.outbox(tenant, fl.SubmissionID, "")
	for i := 0; i < 3; i++ {
		res, err := f.st.ApplyCallback(tc(tenant), cbIn("E1", fl.AuthorityRef, "ACCEPTED", "ACCEPTED"))
		require.NoError(t, err)
		assert.Equal(t, authority.OutcomeDuplicate, res.Outcome)
	}
	assert.Equal(t, before, f.outbox(tenant, fl.SubmissionID, ""), "a replayed callback produces no further event")
	assert.Equal(t, 3, f.cbCount(tenant, "E1", authority.OutcomeDuplicate))

	other := f.sentFiling(tenant, false)
	var applied, dup atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.st.ApplyCallback(tc(tenant), cbIn("E-race", other.AuthorityRef, "ACCEPTED", "ACCEPTED"))
			if err != nil {
				t.Errorf("%v", err)
				return
			}
			switch r.Outcome {
			case authority.OutcomeApplied:
				applied.Add(1)
			case authority.OutcomeDuplicate:
				dup.Add(1)
			default:
				t.Errorf("unexpected %s", r.Outcome)
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, applied.Load())
	assert.EqualValues(t, 9, dup.Load())
}

func TestCallback_NeverMovesAFilingBackwardsOrChangesAFinalOne(t *testing.T) {
	f := newFixture(t)
	tenant := uuid.NewString()

	fin := f.sentFiling(tenant, false)
	_, _ = f.st.ApplyCallback(tc(tenant), cbIn("A1", fin.AuthorityRef, "REJECTED", "REJECTED"))
	for i, m := range []string{"ACCEPTED", "PENDING", "REJECTED"} {
		res, err := f.st.ApplyCallback(tc(tenant), cbIn("A-late-"+string(rune('a'+i)), fin.AuthorityRef, m, m))
		require.NoError(t, err)
		if m == "REJECTED" {
			assert.Equal(t, authority.OutcomeStaleIgnored, res.Outcome, "the same final state again changes nothing")
		} else {
			assert.Equal(t, authority.OutcomeRegressionBlocked, res.Outcome, m)
		}
	}
	cur, _ := f.st.LoadFiling(tc(tenant), fin.SubmissionID)
	assert.Equal(t, authority.StatusRejected, cur.Status)

	// an unknown-outcome filing is found by a callback: PENDING moves it to SUBMITTED, never backwards
	unk := f.sentFiling(tenant, true)
	res, err := f.st.ApplyCallback(tc(tenant), cbIn("B1", unk.SubmissionID, "PROCESSING", "PENDING"))
	require.NoError(t, err)
	assert.Equal(t, authority.OutcomeApplied, res.Outcome)
	assert.Equal(t, authority.StatusSubmitted, res.Filing.Status)
	res, _ = f.st.ApplyCallback(tc(tenant), cbIn("B2", unk.SubmissionID, "PROCESSING", "PENDING"))
	assert.Equal(t, authority.OutcomeStaleIgnored, res.Outcome)
	assert.Equal(t, 1, f.outbox(tenant, unk.SubmissionID, "authority-attempt.submitted")-f.outbox(tenant, unk.SubmissionID, "x-none"))

	// a filing that was never sent cannot be answered for
	fresh := f.filing(tenant, f.iface(tenant, nil))
	res, _ = f.st.ApplyCallback(tc(tenant), cbIn("C1", fresh.SubmissionID, "ACCEPTED", "ACCEPTED"))
	assert.Equal(t, authority.OutcomeStaleIgnored, res.Outcome)
	cur, _ = f.st.LoadFiling(tc(tenant), fresh.SubmissionID)
	assert.Equal(t, authority.StatusPrepared, cur.Status)
}

func TestCallback_UnknownAmbiguousAndCrossTenant(t *testing.T) {
	f := newFixture(t)
	owner, other := uuid.NewString(), uuid.NewString()
	fl := f.sentFiling(owner, false)

	// the authority calls before we know the reference: not decided, so the same event can apply later
	res, err := f.st.ApplyCallback(tc(owner), cbIn("U1", "RC-not-yet", "ACCEPTED", "ACCEPTED"))
	require.NoError(t, err)
	assert.Equal(t, authority.OutcomeUnknownFiling, res.Outcome)
	res, _ = f.st.ApplyCallback(tc(owner), cbIn("U1", fl.AuthorityRef, "ACCEPTED", "ACCEPTED"))
	assert.Equal(t, authority.OutcomeApplied, res.Outcome)

	// another tenant's scope finds nothing of the owner's
	fl2 := f.sentFiling(owner, false)
	res, _ = f.st.ApplyCallback(tc(other), cbIn("X1", fl2.AuthorityRef, "ACCEPTED", "ACCEPTED"))
	assert.Equal(t, authority.OutcomeUnknownFiling, res.Outcome)
	res, _ = f.st.ApplyCallback(tc(other), cbIn("X2", fl2.SubmissionID, "ACCEPTED", "ACCEPTED"))
	assert.Equal(t, authority.OutcomeUnknownFiling, res.Outcome)
	cur, _ := f.st.LoadFiling(tc(owner), fl2.SubmissionID)
	assert.Equal(t, authority.StatusSubmitted, cur.Status)

	// receipts are tenant-scoped, listable and append-only
	mine, err := f.st.ListCallbacks(tc(owner), "", 50) // both: the unmatched one carries no submission id
	require.NoError(t, err)
	assert.Len(t, mine, 2)
	theirs, _ := f.st.ListCallbacks(tc(other), fl.SubmissionID, 50) // the other tenant sees none of the owner's
	assert.Empty(t, theirs)
	_, err = f.app.Exec(tc(other), `INSERT INTO tax_authority_callbacks (tenant_id, provider_key, event_ref, submission_ref, scheme, reported_status, body_hash, outcome)
		VALUES ($1, 'taxauth', 'forged', 'x', 'hmac-sha256-hex', 'ACCEPTED', repeat('a', 64), 'APPLIED')`, owner)
	assert.Error(t, err)
	var id string
	require.NoError(t, f.admin.QueryRow(context.Background(), `SELECT receipt_id::text FROM tax_authority_callbacks WHERE tenant_id = $1 LIMIT 1`, owner).Scan(&id))
	_, err = f.admin.Exec(context.Background(), `UPDATE tax_authority_callbacks SET outcome = 'APPLIED' WHERE receipt_id = $1`, id)
	assert.Error(t, err)
	_, err = f.admin.Exec(context.Background(), `DELETE FROM tax_authority_callbacks WHERE receipt_id = $1`, id)
	assert.Error(t, err)
}
