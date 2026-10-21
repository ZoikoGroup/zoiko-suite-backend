package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
	"zoiko.io/payroll-run-svc/internal/store"
)

// Real-Postgres tests for WFP-07. The embedded superuser bypasses RLS; the restricted-role test proves
// the policies. Database guards are exercised with direct SQL.

func payEvents(_ string) store.PaymentEventBuilder {
	return func(ch store.PaymentChange) ([]domain.OutboxEvent, error) {
		return events.BuildPaymentEvents(ch.Kind, ch.Instruction, ch.Reason, "actor", "corr", "")
	}
}

func payeeFacts(version string, sigs int) store.ValidationFacts {
	return store.ValidationFacts{
		Payee:        domain.PayeeDestination{PayeeRef: "payee-1", DestinationID: "dest-1", Version: version, Fingerprint: "bank-fp-" + version},
		PolicyResult: "WITHIN_THRESHOLD", RequiredSignatures: sigs,
	}
}

// netPayDraft builds the DRAFT the handler would for the first worker of an accepted result (created by p-2;
// the result was received by p-1).
func netPayDraft(t *testing.T, s *store.PgStore, ctx context.Context, tenant string, res *store.ReceiveResultOutput) *domain.PaymentInstruction {
	t.Helper()
	ws, err := s.ListResultWorkers(ctx, res.Ref.RunRefID)
	require.NoError(t, err)
	require.NotEmpty(t, ws)
	w := ws[0].WorkerID
	in := &domain.PaymentInstruction{
		InstructionID: uuid.NewString(), TenantID: tenant, LegalEntityID: "le-1", InstructionType: domain.PayTypeNetPay,
		RunRefID: res.Ref.RunRefID, ResultID: res.Result.ResultID, WorkerID: &w, PayeeRef: "payee-1", DestinationID: "dest-1",
		DestinationVersion: "v1", DestinationFingerprint: "bank-fp-v1", Amount: ws[0].NetPay, Currency: ws[0].Currency, ValueDate: "2026-09-30",
		PayerAccountRef: "acct-1", PaymentMethod: domain.PayMethodBank, PaymentReference: "Payroll 2026-09", CorrelationID: "corr", CreatedBy: "p-2",
	}
	in.Fingerprint = domain.ComputeInstructionFingerprint(in)
	out, created, err := s.CreatePaymentInstruction(ctx, in, payEvents(tenant))
	require.NoError(t, err)
	require.True(t, created)
	return out
}

func liabDraft(t *testing.T, s *store.PgStore, ctx context.Context, tenant string, res *store.ReceiveResultOutput, l domain.PayrollLiability, amount string) (*domain.PaymentInstruction, error) {
	t.Helper()
	lid := l.LiabilityID
	in := &domain.PaymentInstruction{
		InstructionID: uuid.NewString(), TenantID: tenant, LegalEntityID: "le-1", InstructionType: domain.PayTypeLiab,
		RunRefID: res.Ref.RunRefID, ResultID: res.Result.ResultID, LiabilityID: &lid, PayeeRef: l.AuthorityCounterpartyID, DestinationID: "dest-auth",
		DestinationVersion: "v1", DestinationFingerprint: "bank-fp-auth", Amount: amount, Currency: l.Currency, ValueDate: "2026-10-20",
		PayerAccountRef: "acct-1", PaymentMethod: domain.PayMethodBank, PaymentReference: "Payroll liability " + l.ClassCode, CorrelationID: "corr", CreatedBy: "p-2",
	}
	in.Fingerprint = domain.ComputeInstructionFingerprint(in)
	out, _, err := s.CreatePaymentInstruction(ctx, in, payEvents(tenant))
	return out, err
}

// approved takes a DRAFT through validation and the approval of p-7 (one signature).
func approved(t *testing.T, s *store.PgStore, ctx context.Context, tenant string, d *domain.PaymentInstruction) *domain.PaymentInstruction {
	t.Helper()
	v, err := s.ValidatePaymentInstruction(ctx, d.InstructionID, d.Version, payeeFacts("v1", 1))
	require.NoError(t, err)
	a, err := s.ApprovePaymentInstruction(ctx, v.InstructionID, "p-7", v.Fingerprint, payEvents(tenant))
	require.NoError(t, err)
	require.Equal(t, domain.PayApproved, a.State)
	return a
}

func TestWFP07_CreateIsADraftBoundToTheResultAndIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)

	d := netPayDraft(t, s, ctx, tenant, res)
	assert.Equal(t, domain.PayDraft, d.State)
	assert.Equal(t, "2400.4000", d.Amount, "net pay is the accepted figure, exact")
	assert.Equal(t, 1, d.RequiredSignatures)
	assert.Nil(t, d.ApprovedFingerprint)
	assert.True(t, len(d.Fingerprint) > 20)
	assert.Equal(t, []string{domain.EventPayrollPaymentInstrCreated}, rEventTypes(t, pool, tenant)[len(rEventTypes(t, pool, tenant))-1:])

	// The same worker's net pay again is the same instruction, not a second payment.
	again, created, err := s.CreatePaymentInstruction(ctx, &domain.PaymentInstruction{
		InstructionID: uuid.NewString(), TenantID: tenant, LegalEntityID: "le-1", InstructionType: domain.PayTypeNetPay, RunRefID: d.RunRefID, ResultID: d.ResultID,
		WorkerID: d.WorkerID, PayeeRef: d.PayeeRef, DestinationID: d.DestinationID, DestinationVersion: d.DestinationVersion, DestinationFingerprint: d.DestinationFingerprint,
		Amount: d.Amount, Currency: d.Currency, ValueDate: d.ValueDate, PayerAccountRef: d.PayerAccountRef, PaymentMethod: d.PaymentMethod, PaymentReference: d.PaymentReference,
		Fingerprint: d.Fingerprint, CorrelationID: "c", CreatedBy: "p-2",
	}, payEvents(tenant))
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, d.InstructionID, again.InstructionID)

	// A cancelled instruction frees the worker for a new one.
	c, err := s.CancelPaymentInstruction(ctx, d.InstructionID, d.Version, "wrong account", payEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.PayCancelled, c.State)
	d2 := netPayDraft(t, s, ctx, tenant, res)
	assert.NotEqual(t, d.InstructionID, d2.InstructionID)

	// The database holds the same lines with no store in the way.
	for _, stmt := range []string{
		`DELETE FROM wfp_payment_instructions WHERE instruction_id = $1`,
		`UPDATE wfp_payment_instructions SET created_by = 'x' WHERE instruction_id = $1`,
		`UPDATE wfp_payment_instructions SET state = 'APPROVED' WHERE instruction_id = $1`,
		`UPDATE wfp_payment_instructions SET state = 'HANDED_OFF' WHERE instruction_id = $1`,
		`UPDATE wfp_payment_instructions SET amount = 0 WHERE instruction_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), stmt, d2.InstructionID)
		assert.Error(t, err, stmt)
	}
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payment_instructions SET worker_id = NULL WHERE instruction_id = $1`, d2.InstructionID)
	assert.Error(t, err, "a NET_PAY instruction always names its worker")
}

func TestWFP07_OnlyAnAcceptedResultCanBePaid(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	v1 := acceptedResult(t, s, ctx, tenant, mid, "RUN-S", 1)
	ws, _ := s.ListResultWorkers(ctx, v1.Ref.RunRefID)
	acceptedResult(t, s, ctx, tenant, mid, "RUN-S", 2) // v1 superseded

	w := ws[0].WorkerID
	in := &domain.PaymentInstruction{
		InstructionID: uuid.NewString(), TenantID: tenant, LegalEntityID: "le-1", InstructionType: domain.PayTypeNetPay, RunRefID: v1.Ref.RunRefID, ResultID: v1.Result.ResultID,
		WorkerID: &w, PayeeRef: "p", DestinationID: "d", DestinationVersion: "v", DestinationFingerprint: "f", Amount: "10", Currency: "GBP", ValueDate: "2026-09-30",
		PayerAccountRef: "a", PaymentMethod: domain.PayMethodBank, PaymentReference: "r", Fingerprint: "sha256:x", CorrelationID: "c", CreatedBy: "p-2",
	}
	_, _, err := s.CreatePaymentInstruction(ctx, in, nil)
	assert.ErrorIs(t, err, domain.ErrInstructionResultNotActive)
}

func TestWFP07_LiabilityInstructionsCannotPayMoreThanIsOutstanding(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	out := recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	paye := liabOf(t, out, "PAYE") // owes 600.1000

	a, err := liabDraft(t, s, ctx, tenant, res, paye, "300")
	require.NoError(t, err)
	reserved, err := s.ReservedForLiability(ctx, paye.LiabilityID)
	require.NoError(t, err)
	assert.Equal(t, "300.0000", reserved)

	_, err = liabDraft(t, s, ctx, tenant, res, paye, "300.1001")
	assert.ErrorIs(t, err, domain.ErrExceedsOutstanding, "live instructions plus this one would exceed what is owed")
	b, err := liabDraft(t, s, ctx, tenant, res, paye, "300.1")
	require.NoError(t, err, "the remainder fits exactly")
	_, err = liabDraft(t, s, ctx, tenant, res, paye, "0.0001")
	assert.ErrorIs(t, err, domain.ErrExceedsOutstanding)

	// Cancelling frees the reservation.
	_, err = s.CancelPaymentInstruction(ctx, a.InstructionID, a.Version, "re-plan", payEvents(tenant))
	require.NoError(t, err)
	_, err = liabDraft(t, s, ctx, tenant, res, paye, "300")
	require.NoError(t, err)

	// A disputed or under-review liability is not paid.
	pen := liabOf(t, out, "PENSION")
	d, err := s.DisputeLiability(ctx, pen.LiabilityID, pen.Version, "wrong amount", liabEvents(tenant))
	require.NoError(t, err)
	_, err = liabDraft(t, s, ctx, tenant, res, *d, "10")
	assert.ErrorIs(t, err, domain.ErrSubjectNotPayable)

	// A liability of another result is not payable through this one.
	other := acceptedResult(t, s, ctx, tenant, mid, "RUN-2", 1)
	_, err = liabDraft(t, s, ctx, tenant, other, paye, "10")
	assert.ErrorIs(t, err, domain.ErrSubjectNotPayable)
	_ = b
}

func TestWFP07_ApprovalNeedsADifferentPrincipalAtTheFingerprintTheyAreShown(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1) // received by p-1
	d := netPayDraft(t, s, ctx, tenant, res)                  // prepared by p-2

	_, err := s.ApprovePaymentInstruction(ctx, d.InstructionID, "p-7", d.Fingerprint, nil)
	assert.ErrorIs(t, err, domain.ErrInstructionState, "a DRAFT cannot be approved: it must be validated first")

	_, err = s.ValidatePaymentInstruction(ctx, d.InstructionID, 99, payeeFacts("v1", 1))
	assert.ErrorIs(t, err, domain.ErrInstructionVersionConflict)
	v, err := s.ValidatePaymentInstruction(ctx, d.InstructionID, d.Version, payeeFacts("v1", 1))
	require.NoError(t, err)
	assert.Equal(t, domain.PayValidated, v.State)
	_, err = s.ValidatePaymentInstruction(ctx, d.InstructionID, v.Version, payeeFacts("v1", 1))
	assert.ErrorIs(t, err, domain.ErrInstructionState, "already validated")

	for _, who := range []string{"p-2", "p-1"} { // the preparer, then the principal who received the result
		_, err = s.ApprovePaymentInstruction(ctx, v.InstructionID, who, v.Fingerprint, nil)
		assert.ErrorIs(t, err, domain.ErrSoDPreparerOrReceiver, who)
	}
	_, err = s.ApprovePaymentInstruction(ctx, v.InstructionID, "p-7", "sha256:something-else", nil)
	assert.ErrorIs(t, err, domain.ErrApprovalInvalidated, "approving a fingerprint that is not the current one")

	// Nor can anyone approve by editing state directly.
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payment_instructions SET state = 'APPROVED', approved_fingerprint = fingerprint, approved_at = now() WHERE instruction_id = $1`, v.InstructionID)
	assert.Error(t, err, "no quorum, no approval")
	_, err = pool.Exec(context.Background(), `INSERT INTO wfp_payment_approvals (approval_id, tenant_id, instruction_id, approver, fingerprint) VALUES (gen_random_uuid(), $1, $2, 'p-2', $3)`,
		tenant, v.InstructionID, v.Fingerprint)
	assert.Error(t, err, "the preparer cannot approve even by direct insert")

	a, err := s.ApprovePaymentInstruction(ctx, v.InstructionID, "p-7", v.Fingerprint, payEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.PayApproved, a.State)
	require.NotNil(t, a.ApprovedFingerprint)
	assert.Equal(t, a.Fingerprint, *a.ApprovedFingerprint)
	assert.Equal(t, 1, a.SignaturesCollected)
	assert.Equal(t, 1, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollPaymentInstrApproved))

	state, fp, err := s.PaymentInstructionFingerprint(ctx, a.InstructionID)
	require.NoError(t, err)
	assert.Equal(t, domain.PayApproved, state)
	assert.Equal(t, a.Fingerprint, fp)
	state, fp, err = s.PaymentInstructionFingerprint(ctx, d.InstructionID)
	require.NoError(t, err)
	assert.Equal(t, domain.PayApproved, state)
	_ = fp

	// What is approved is frozen.
	for _, stmt := range []string{
		`UPDATE wfp_payment_instructions SET amount = 1 WHERE instruction_id = $1`,
		`UPDATE wfp_payment_instructions SET destination_id = 'other' WHERE instruction_id = $1`,
		`UPDATE wfp_payment_instructions SET payer_account_ref = 'other' WHERE instruction_id = $1`,
		`UPDATE wfp_payment_instructions SET fingerprint = 'sha256:forged' WHERE instruction_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), stmt, a.InstructionID)
		assert.Error(t, err, stmt)
	}
}

func TestWFP07_AHighValueInstructionNeedsAQuorumOfDistinctApprovers(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	d := netPayDraft(t, s, ctx, tenant, res)
	f := payeeFacts("v1", 2)
	f.PolicyResult, f.PolicyVersionID = domain.PolicyApproval, "pol-1"
	v, err := s.ValidatePaymentInstruction(ctx, d.InstructionID, d.Version, f)
	require.NoError(t, err)
	assert.Equal(t, 2, v.RequiredSignatures)
	require.NotNil(t, v.PolicyResult)
	assert.Equal(t, domain.PolicyApproval, *v.PolicyResult)

	first, err := s.ApprovePaymentInstruction(ctx, v.InstructionID, "p-7", v.Fingerprint, payEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.PayValidated, first.State, "one of two signatures")
	assert.Equal(t, 1, first.SignaturesCollected)
	_, err = s.ApprovePaymentInstruction(ctx, v.InstructionID, "p-7", v.Fingerprint, nil)
	assert.ErrorIs(t, err, domain.ErrAlreadyApproved, "the same principal cannot sign twice")
	assert.Equal(t, 0, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollPaymentInstrApproved), "no approval event before the quorum")

	second, err := s.ApprovePaymentInstruction(ctx, v.InstructionID, "p-8", v.Fingerprint, payEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.PayApproved, second.State)
	assert.Equal(t, 2, second.SignaturesCollected)
	assert.Equal(t, 1, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollPaymentInstrApproved))
}

func TestWFP07_ChangingTheProtectedSubjectInvalidatesTheApproval(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	a := approved(t, s, ctx, tenant, netPayDraft(t, s, ctx, tenant, res))

	inv, err := s.InvalidatePaymentApproval(ctx, a.InstructionID, "PROTECTED_SUBJECT_CHANGED", payEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.PayDraft, inv.State)
	assert.Nil(t, inv.ApprovedFingerprint)
	assert.Equal(t, 0, inv.SignaturesCollected)
	require.NotNil(t, inv.InvalidationReason)
	assert.Equal(t, 1, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollPaymentInstrInvalidated))
	_, err = s.InvalidatePaymentApproval(ctx, a.InstructionID, "again", nil)
	assert.ErrorIs(t, err, domain.ErrInstructionState, "nothing left to invalidate")

	// The payee's destination changed (a new version and bank-detail digest): re-validation re-pins it,
	// the fingerprint changes, and the old approval cannot count toward the new one.
	v, err := s.ValidatePaymentInstruction(ctx, inv.InstructionID, inv.Version, payeeFacts("v2", 1))
	require.NoError(t, err)
	assert.NotEqual(t, a.Fingerprint, v.Fingerprint, "the payee version is part of what is approved")
	assert.Equal(t, "v2", v.DestinationVersion)
	assert.Equal(t, 0, v.SignaturesCollected)
	_, err = s.ApprovePaymentInstruction(ctx, v.InstructionID, "p-7", a.Fingerprint, nil)
	assert.ErrorIs(t, err, domain.ErrApprovalInvalidated, "the old fingerprint no longer applies")
	re, err := s.ApprovePaymentInstruction(ctx, v.InstructionID, "p-7", v.Fingerprint, payEvents(tenant))
	require.NoError(t, err, "the same principal can approve the NEW fingerprint")
	assert.Equal(t, domain.PayApproved, re.State)

	det, err := s.GetPaymentInstruction(ctx, re.InstructionID)
	require.NoError(t, err)
	require.Len(t, det.Approvals, 2)
	assert.NotNil(t, det.Approvals[0].InvalidatedAt, "the first approval is kept as history, marked invalid")
	assert.Nil(t, det.Approvals[1].InvalidatedAt)
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payment_approvals SET approver = 'x' WHERE instruction_id = $1`, re.InstructionID)
	assert.Error(t, err, "approvals are evidence")
	_, err = pool.Exec(context.Background(), `DELETE FROM wfp_payment_approvals WHERE instruction_id = $1`, re.InstructionID)
	assert.Error(t, err)
}

func TestWFP07_SubmissionToBankingIsPendingUntilBankingSaysSubmitted(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	d := netPayDraft(t, s, ctx, tenant, res)

	_, _, err := s.BeginSubmitToBanking(ctx, d.InstructionID, d.Version, "p-9")
	assert.ErrorIs(t, err, domain.ErrNotApproved)
	a := approved(t, s, ctx, tenant, d)
	_, _, err = s.BeginSubmitToBanking(ctx, a.InstructionID, 99, "p-9")
	assert.ErrorIs(t, err, domain.ErrInstructionVersionConflict)

	sub, retry, err := s.BeginSubmitToBanking(ctx, a.InstructionID, a.Version, "p-9")
	require.NoError(t, err)
	assert.False(t, retry)
	assert.Equal(t, domain.PaySubmitted, sub.State)
	again, retry, err := s.BeginSubmitToBanking(ctx, a.InstructionID, 0, "p-9")
	require.NoError(t, err)
	assert.True(t, retry, "calling again for a submitted instruction is a retry of the same attempt")
	assert.Equal(t, sub.InstructionID, again.InstructionID)

	// Cancelling is for instructions that never reached Banking.
	_, err = s.CancelPaymentInstruction(ctx, a.InstructionID, again.Version, "oops", nil)
	assert.ErrorIs(t, err, domain.ErrInstructionState)
	_, err = s.InvalidatePaymentApproval(ctx, a.InstructionID, "oops", nil)
	assert.ErrorIs(t, err, domain.ErrInstructionState)

	// Banking timed out: unknown, not failed, not paid.
	unk, err := s.RecordBankingOutcome(ctx, a.InstructionID, "", "UNKNOWN", "adapter unreachable", payEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.PaySubmitted, unk.State)
	assert.Nil(t, unk.HandedOffAt)
	// The attempt exists but its submission is unconfirmed.
	pend, err := s.RecordBankingOutcome(ctx, a.InstructionID, "attempt-1", "PENDING_UNKNOWN", "", payEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.PaySubmitted, pend.State)
	require.NotNil(t, pend.AttemptID)
	assert.Equal(t, 1, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollPaymentSubmitted), "the attempt is announced once")
	_, err = s.ReleaseRefusedSubmission(ctx, a.InstructionID, "x")
	assert.ErrorIs(t, err, domain.ErrInstructionState, "an instruction with a Banking attempt is never released")
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payment_instructions SET attempt_id = 'attempt-2' WHERE instruction_id = $1`, a.InstructionID)
	assert.Error(t, err, "the Banking attempt never changes")

	done, err := s.RecordBankingOutcome(ctx, a.InstructionID, "attempt-1", "SUBMITTED", "", payEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.PayHandedOff, done.State)
	assert.NotNil(t, done.HandedOffAt)
	assert.Equal(t, 2, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollPaymentSubmitted))
	_, err = s.RecordBankingOutcome(ctx, a.InstructionID, "attempt-1", "SUBMITTED", "", nil)
	assert.ErrorIs(t, err, domain.ErrInstructionState)
}

func TestWFP07_ARefusalBeforeAnyAttemptReleasesTheInstruction(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	a := approved(t, s, ctx, tenant, netPayDraft(t, s, ctx, tenant, res))
	_, _, err := s.BeginSubmitToBanking(ctx, a.InstructionID, a.Version, "p-9")
	require.NoError(t, err)

	rel, err := s.ReleaseRefusedSubmission(ctx, a.InstructionID, "payer account closed")
	require.NoError(t, err)
	assert.Equal(t, domain.PayApproved, rel.State)
	require.NotNil(t, rel.BankingError)
	assert.Equal(t, "payer account closed", *rel.BankingError, "the refusal stays visible")
	// It can be resubmitted or cancelled.
	_, _, err = s.BeginSubmitToBanking(ctx, a.InstructionID, rel.Version, "p-9")
	require.NoError(t, err)
}

func TestWFP07_SupersessionStopsUnsubmittedPaymentsAndFlagsSubmittedOnes(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	v1 := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	out := recognize(t, s, ctx, tenant, v1.Ref.RunRefID, nil, "2026-09-30")

	net := approved(t, s, ctx, tenant, netPayDraft(t, s, ctx, tenant, v1)) // APPROVED
	draftLiab, err := liabDraft(t, s, ctx, tenant, v1, liabOf(t, out, "PAYE"), "100")
	require.NoError(t, err) // DRAFT
	sentLiab, err := liabDraft(t, s, ctx, tenant, v1, liabOf(t, out, "PENSION"), "100")
	require.NoError(t, err)
	sentLiab = approved(t, s, ctx, tenant, sentLiab)
	_, _, err = s.BeginSubmitToBanking(ctx, sentLiab.InstructionID, sentLiab.Version, "p-9")
	require.NoError(t, err)

	_, err = s.SupersedeInstructionsForRun(ctx, v1.Ref.RunRefID, nil)
	assert.ErrorIs(t, err, domain.ErrResultNotSuperseded)

	// A validated-but-not-approved instruction cannot be approved once its result is superseded.
	pend, err := liabDraft(t, s, ctx, tenant, v1, liabOf(t, out, "NI-ER"), "50")
	require.NoError(t, err)
	pv, err := s.ValidatePaymentInstruction(ctx, pend.InstructionID, pend.Version, payeeFacts("v1", 1))
	require.NoError(t, err)

	acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 2) // supersedes v1
	_, err = s.ApprovePaymentInstruction(ctx, pv.InstructionID, "p-7", pv.Fingerprint, nil)
	assert.ErrorIs(t, err, domain.ErrInstructionResultNotActive, "no approval of a superseded result's payment")
	_, _, err = s.BeginSubmitToBanking(ctx, net.InstructionID, net.Version, "p-9")
	assert.ErrorIs(t, err, domain.ErrInstructionResultNotActive, "nor a submission")

	sup, err := s.SupersedeInstructionsForRun(ctx, v1.Ref.RunRefID, payEvents(tenant))
	require.NoError(t, err)
	assert.Len(t, sup.Superseded, 3, "net pay, the draft and the validated liability instruction")
	require.Len(t, sup.NeedsAttention, 1)
	assert.Equal(t, sentLiab.InstructionID, sup.NeedsAttention[0].InstructionID, "what reached Banking is resolved with Banking")
	got, _ := s.GetPaymentInstruction(ctx, draftLiab.InstructionID)
	assert.Equal(t, domain.PaySuperseded, got.State)
	got, _ = s.GetPaymentInstruction(ctx, net.InstructionID)
	assert.Equal(t, domain.PaySuperseded, got.State)
	assert.Nil(t, got.ApprovedFingerprint)
	assert.Equal(t, 3, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollPaymentInstrSuperseded))
	again, err := s.SupersedeInstructionsForRun(ctx, v1.Ref.RunRefID, nil)
	require.NoError(t, err)
	assert.Empty(t, again.Superseded, "idempotent")
	assert.Len(t, again.NeedsAttention, 1)
}

func TestWFP07_ASettlementIsBoundToTheInstructionThatPaidIt(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	out := recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	paye, pen := liabOf(t, out, "PAYE"), liabOf(t, out, "PENSION")

	mk := func(l domain.PayrollLiability, amount string, submit bool) *domain.PaymentInstruction {
		d, err := liabDraft(t, s, ctx, tenant, res, l, amount)
		require.NoError(t, err)
		a := approved(t, s, ctx, tenant, d)
		if submit {
			_, _, err = s.BeginSubmitToBanking(ctx, a.InstructionID, a.Version, "p-9")
			require.NoError(t, err)
		}
		return a
	}
	instr := mk(paye, "600.10", true)
	notSent := mk(pen, "100", false)
	set := func(l domain.PayrollLiability, payment, amount, instruction string) error {
		_, err := s.SettleLiability(ctx, l.LiabilityID, domain.SettleLiabilityRequest{PaymentID: payment, Amount: amount, Currency: "GBP", InstructionID: instruction}, settled, "p-5", liabEvents(tenant))
		return err
	}

	assert.ErrorIs(t, set(paye, "pay-1", "600.10", uuid.NewString()), domain.ErrInstructionMismatch, "no such instruction")
	assert.ErrorIs(t, set(paye, "pay-1", "500", instr.InstructionID), domain.ErrInstructionMismatch, "the approved amount is the amount")
	assert.ErrorIs(t, set(pen, "pay-1", "100", instr.InstructionID), domain.ErrInstructionMismatch, "another liability's instruction")
	assert.ErrorIs(t, set(pen, "pay-2", "100", notSent.InstructionID), domain.ErrInstructionMismatch, "an instruction that never reached Banking paid nothing")

	require.NoError(t, set(paye, "pay-1", "600.10", instr.InstructionID))
	got, err := s.GetLiability(ctx, paye.LiabilityID)
	require.NoError(t, err)
	assert.Equal(t, domain.LiabSettled, got.State)
	require.Len(t, got.Settlements, 1)
	assert.Equal(t, domain.AmountInstructionBound, got.Settlements[0].AmountEvidence, "the amount is the approved, fingerprinted figure")
	require.NotNil(t, got.Settlements[0].InstructionID)
	assert.Equal(t, instr.InstructionID, *got.Settlements[0].InstructionID)

	// Without an instruction the amount stays a declaration, and says so.
	require.NoError(t, set(pen, "pay-9", "40", ""))
	got, _ = s.GetLiability(ctx, pen.LiabilityID)
	assert.Equal(t, domain.AmountDeclared, got.Settlements[0].AmountEvidence)

	// One instruction settles once.
	again := mk(liabOf(t, out, "NI-ER"), "250", true)
	require.NoError(t, set(liabOf(t, out, "NI-ER"), "pay-3", "250", again.InstructionID))
	_, err = pool.Exec(context.Background(), `INSERT INTO wfp_liability_settlements (settlement_id, tenant_id, liability_id, payment_id, amount, currency, declared_by, banking_status, instruction_id)
		VALUES (gen_random_uuid(), $1, $2, 'pay-dup', 1, 'GBP', 'x', 'SETTLED', $3)`, tenant, pen.LiabilityID, again.InstructionID)
	assert.Error(t, err, "a second payment cannot claim the same instruction")
}

func TestWFP07_ListsAndTenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenantA, ctxA := wfpTenant()
	tenantB, ctxB := wfpTenant()
	mA, mB := registerManifest(t, s, ctxA), registerManifest(t, s, ctxB)
	resA := acceptedResult(t, s, ctxA, tenantA, mA, "RUN-1", 1)
	resB := acceptedResult(t, s, ctxB, tenantB, mB, "RUN-1", 1)
	dA := netPayDraft(t, s, ctxA, tenantA, resA)
	netPayDraft(t, s, ctxB, tenantB, resB)

	_, err := s.GetPaymentInstruction(ctxB, dA.InstructionID)
	assert.ErrorIs(t, err, domain.ErrInstructionNotFound)
	_, err = s.ValidatePaymentInstruction(ctxB, dA.InstructionID, dA.Version, payeeFacts("v1", 1))
	assert.ErrorIs(t, err, domain.ErrInstructionNotFound)
	_, err = s.CancelPaymentInstruction(ctxB, dA.InstructionID, dA.Version, "x", nil)
	assert.ErrorIs(t, err, domain.ErrInstructionNotFound)
	_, _, err = s.PaymentInstructionFingerprint(ctxB, dA.InstructionID)
	assert.ErrorIs(t, err, domain.ErrInstructionNotFound, "the adapter's lookup is tenant-scoped too")

	listA, err := s.ListPaymentInstructions(ctxA, store.InstructionFilter{LegalEntityID: "le-1", PendingOnly: true})
	require.NoError(t, err)
	require.Len(t, listA, 1)
	assert.Equal(t, tenantA, listA[0].TenantID)
	c, err := s.CancelPaymentInstruction(ctxA, dA.InstructionID, dA.Version, "x", nil)
	require.NoError(t, err)
	assert.Equal(t, domain.PayCancelled, c.State)
	listA, _ = s.ListPaymentInstructions(ctxA, store.InstructionFilter{LegalEntityID: "le-1", PendingOnly: true})
	assert.Empty(t, listA, "a cancelled instruction is no longer pending")
}

func TestWFP07_WorksUnderRowLevelSecurity(t *testing.T) {
	admin := openTestPool(t)
	app := appPool(t, admin)
	s := store.New(app, "")
	tenantA, ctxA := wfpTenant()
	tenantB, ctxB := wfpTenant()
	mA, mB := registerManifest(t, s, ctxA), registerManifest(t, s, ctxB)
	resA := acceptedResult(t, s, ctxA, tenantA, mA, "RUN-1", 1)
	resB := acceptedResult(t, s, ctxB, tenantB, mB, "RUN-1", 1)

	a := approved(t, s, ctxA, tenantA, netPayDraft(t, s, ctxA, tenantA, resA))
	_, _, err := s.BeginSubmitToBanking(ctxA, a.InstructionID, a.Version, "p-9")
	require.NoError(t, err)
	done, err := s.RecordBankingOutcome(ctxA, a.InstructionID, "attempt-1", "SUBMITTED", "", payEvents(tenantA))
	require.NoError(t, err)
	assert.Equal(t, domain.PayHandedOff, done.State)

	_, err = s.GetPaymentInstruction(ctxB, a.InstructionID)
	assert.ErrorIs(t, err, domain.ErrInstructionNotFound)
	netPayDraft(t, s, ctxB, tenantB, resB)
	var n int
	require.NoError(t, app.QueryRow(context.Background(), `SELECT count(*) FROM wfp_payment_instructions`).Scan(&n))
	assert.Equal(t, 0, n, "no tenant context, no rows")
	require.NoError(t, app.QueryRow(context.Background(), `SELECT count(*) FROM wfp_payment_approvals`).Scan(&n))
	assert.Equal(t, 0, n)
}

func TestWFP07_EventsAreAtomicWithTheChange(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	d := netPayDraft(t, s, ctx, tenant, res)
	v, err := s.ValidatePaymentInstruction(ctx, d.InstructionID, d.Version, payeeFacts("v1", 1))
	require.NoError(t, err)

	boom := func(store.PaymentChange) ([]domain.OutboxEvent, error) {
		return nil, errors.New("event builder failed")
	}
	_, err = s.ApprovePaymentInstruction(ctx, v.InstructionID, "p-7", v.Fingerprint, boom)
	require.Error(t, err)
	got, _ := s.GetPaymentInstruction(ctx, v.InstructionID)
	assert.Equal(t, domain.PayValidated, got.State, "a failed event rolls the approval back")
	assert.Empty(t, got.Approvals, "including the recorded signature")
}
