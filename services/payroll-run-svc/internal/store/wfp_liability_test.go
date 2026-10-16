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

// Real-Postgres tests for WFP-06. The embedded superuser bypasses RLS, so tenant
// isolation proves the explicit tenant_id predicate; the restricted-role test proves
// the policies. Database guards are exercised with direct SQL.

func liabEvents(_ string) store.LiabilityEventBuilder {
	return func(ch store.LiabilityChange) ([]domain.OutboxEvent, error) {
		return events.BuildLiabilityEvents(ch.Kind, ch.Liability, ch.Exception, ch.Detail, "actor", "corr", "")
	}
}

func classReq(kind, class, authority, typ, dueKind string, due int, from string, required bool) domain.RegisterLiabilityClassRequest {
	return domain.RegisterLiabilityClassRequest{
		LegalEntityID: "le-1", ComponentKind: kind, ClassCode: class, LiabilityType: typ, AuthorityCounterpartyID: authority,
		JurisdictionID: "GB", DueRuleKind: dueKind, DueRuleValue: due, Required: required, RuleRef: "rule-" + class, EffectiveFrom: from,
	}
}

func registerClass(t *testing.T, s *store.PgStore, ctx context.Context, req domain.RegisterLiabilityClassRequest) *domain.LiabilityClass {
	t.Helper()
	c, err := s.RegisterLiabilityClass(ctx, req, "finance-1")
	require.NoError(t, err)
	return c
}

func registerStandardClasses(t *testing.T, s *store.PgStore, ctx context.Context) {
	registerClass(t, s, ctx, classReq(domain.KindEmployeeTax, "PAYE", "cp-hmrc", "TAX", domain.DueDayOfFollowingMonth, 22, "2026-01-01", false))
	registerClass(t, s, ctx, classReq(domain.KindEmployeePension, "PENSION", "cp-nest", "PENSION", domain.DueDaysAfterPayDate, 10, "2026-01-01", false))
	registerClass(t, s, ctx, classReq(domain.KindEmployerSocial, "NI-ER", "cp-hmrc", "SOCIAL", domain.DueDayOfFollowingMonth, 22, "2026-01-01", true))
}

// recognize composes recognition the way the handler does.
func recognize(t *testing.T, s *store.PgStore, ctx context.Context, tenant, runRefID string, review map[string]string, asOf string) *store.RecognitionOutcome {
	t.Helper()
	out, err := tryRecognize(s, ctx, tenant, runRefID, review, asOf)
	require.NoError(t, err)
	return out
}

func tryRecognize(s *store.PgStore, ctx context.Context, tenant, runRefID string, review map[string]string, asOf string) (*store.RecognitionOutcome, error) {
	ref, err := s.GetPayrollRunRef(ctx, runRefID)
	if err != nil {
		return nil, err
	}
	res, err := s.GetPayrollResultByRef(ctx, runRefID)
	if err != nil {
		return nil, err
	}
	comps, err := s.ListResultComponents(ctx, runRefID, "")
	if err != nil {
		return nil, err
	}
	classes, err := s.ListLiabilityClasses(ctx, ref.LegalEntityID)
	if err != nil {
		return nil, err
	}
	plan, err := domain.PlanRecognition(domain.PlanInput{PayDate: ref.PayDate, Flags: res.EvidenceFlags, Components: comps, Classes: classes})
	if err != nil {
		return nil, err
	}
	if review == nil {
		review = map[string]string{}
	}
	return s.ApplyRecognition(ctx, ref, res, plan, review, "p-3", "corr", asOf, liabEvents(tenant))
}

func liabOf(t *testing.T, out *store.RecognitionOutcome, class string) domain.PayrollLiability {
	t.Helper()
	for _, l := range out.Liabilities {
		if l.ClassCode == class {
			return l
		}
	}
	t.Fatalf("no liability of class %s", class)
	return domain.PayrollLiability{}
}

func countType(t *testing.T, types []string, typ string) int {
	n := 0
	for _, x := range types {
		if x == typ {
			n++
		}
	}
	return n
}

func settleReq(payment, amount string) domain.SettleLiabilityRequest {
	return domain.SettleLiabilityRequest{PaymentID: payment, Amount: amount, Currency: "GBP"}
}

var settled = store.SettlementEvidence{BankingStatus: "SETTLED"}

func TestWFP06_RegistryIsAppendOnlyAndEffectiveDated(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	_, ctx := wfpTenant()

	v1 := registerClass(t, s, ctx, classReq(domain.KindEmployeeTax, "PAYE", "cp-hmrc", "TAX", domain.DueDayOfFollowingMonth, 22, "2026-01-01", false))
	assert.Equal(t, 1, v1.ClassVersion)
	assert.Equal(t, "*", v1.ComponentCode)
	assert.True(t, v1.Active)

	// A change is a new row with a LATER effective date; the same or an earlier date is refused.
	_, err := s.RegisterLiabilityClass(ctx, classReq(domain.KindEmployeeTax, "PAYE", "cp-hmrc", "TAX", domain.DueDayOfFollowingMonth, 19, "2026-01-01", false), "f")
	assert.ErrorIs(t, err, domain.ErrLiabilityClassOrder)
	_, err = s.RegisterLiabilityClass(ctx, classReq(domain.KindEmployeeTax, "PAYE", "cp-hmrc", "TAX", domain.DueDayOfFollowingMonth, 19, "2025-12-01", false), "f")
	assert.ErrorIs(t, err, domain.ErrLiabilityClassOrder)
	v2 := registerClass(t, s, ctx, classReq(domain.KindEmployeeTax, "PAYE", "cp-hmrc", "TAX", domain.DueDayOfFollowingMonth, 19, "2026-07-01", false))
	assert.Equal(t, 2, v2.ClassVersion)

	all, err := s.ListLiabilityClasses(ctx, "le-1")
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, []int{1, 2}, []int{all[0].ClassVersion, all[1].ClassVersion})

	for _, stmt := range []string{
		`UPDATE wfp_liability_classes SET due_rule_value = 1 WHERE class_row_id = $1`,
		`DELETE FROM wfp_liability_classes WHERE class_row_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), stmt, v1.ClassRowID)
		assert.Error(t, err, stmt)
	}
	// An earning cannot create a liability, even by direct SQL.
	_, err = pool.Exec(context.Background(), `INSERT INTO wfp_liability_classes (class_row_id, tenant_id, legal_entity_id, component_kind, class_version,
		class_code, liability_type, authority_counterparty_id, jurisdiction_id, due_rule_kind, due_rule_value, rule_ref, effective_from, registered_by)
		VALUES (gen_random_uuid(), $1, 'le-1', 'EARNING', 1, 'X', 'TAX', 'a', 'GB', 'DAYS_AFTER_PAY_DATE', 1, 'r', '2026-01-01', 'f')`, v1.TenantID)
	assert.Error(t, err)

	other, otherCtx := wfpTenant()
	_ = other
	none, err := s.ListLiabilityClasses(otherCtx, "le-1")
	require.NoError(t, err)
	assert.Empty(t, none, "another tenant sees no registry")
}

func TestWFP06_RecognitionCreatesExactLiabilitiesIdempotently(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)

	out := recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	assert.Equal(t, 3, out.NewCount)
	require.Len(t, out.Liabilities, 3)

	paye := liabOf(t, out, "PAYE")
	assert.Equal(t, "600.1000", paye.Amount)
	assert.Equal(t, domain.LiabRecognized, paye.State)
	assert.Equal(t, "2026-10-22", paye.DueDate)
	assert.Equal(t, domain.DueDayOfFollowingMonth, paye.DueRuleKind)
	assert.Equal(t, "cp-hmrc", paye.AuthorityCounterpartyID)
	assert.Equal(t, domain.SideEmployeeWithheld, paye.PartySide)
	assert.Equal(t, "GBP", paye.Currency)
	assert.Equal(t, "2026-09", paye.PayrollPeriodID)
	assert.Equal(t, "600.1000", paye.OutstandingAmount)
	assert.Equal(t, map[string]string{domain.KindEmployeeTax: "600.1000"}, paye.KindAmounts)
	assert.Equal(t, "rule-PAYE", paye.RuleRef, "due-date and authority provenance is stored")

	ni := liabOf(t, out, "NI-ER")
	assert.Equal(t, "250.0000", ni.Amount)
	assert.Equal(t, domain.SideEmployer, ni.PartySide)
	pen := liabOf(t, out, "PENSION")
	assert.Equal(t, "100.0000", pen.Amount)
	assert.Equal(t, "2026-10-10", pen.DueDate)
	assert.Empty(t, out.Exceptions)

	// Recognizing again changes nothing and emits nothing.
	before := rEventTypes(t, pool, tenant)
	again := recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	assert.Equal(t, 0, again.NewCount)
	require.Len(t, again.Liabilities, 3)
	assert.Equal(t, paye.LiabilityID, liabOf(t, again, "PAYE").LiabilityID)
	assert.Equal(t, before, rEventTypes(t, pool, tenant))
	assert.Equal(t, 3, countType(t, before, domain.EventPayrollLiabilityRecognized))

	// The liabilities never change in place, and are never deleted.
	for _, stmt := range []string{
		`UPDATE wfp_payroll_liabilities SET amount = 1 WHERE liability_id = $1`,
		`UPDATE wfp_payroll_liabilities SET due_date = '2030-01-01' WHERE liability_id = $1`,
		`UPDATE wfp_payroll_liabilities SET authority_counterparty_id = 'cp-evil' WHERE liability_id = $1`,
		`UPDATE wfp_payroll_liabilities SET kind_amounts = '{}'::jsonb WHERE liability_id = $1`,
		`DELETE FROM wfp_payroll_liabilities WHERE liability_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), stmt, paye.LiabilityID)
		assert.Error(t, err, stmt)
	}

}

func TestWFP06_OnlyAnAcceptedResultCanCreateALiability(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)

	v1 := acceptedResult(t, s, ctx, tenant, mid, "RUN-S", 1)
	acceptedResult(t, s, ctx, tenant, mid, "RUN-S", 2) // v1 is now SUPERSEDED

	// A fresh recognition against the superseded version is refused by the database.
	_, err := tryRecognize(s, ctx, tenant, v1.Ref.RunRefID, nil, "2026-09-30")
	assert.ErrorIs(t, err, domain.ErrLiabilityResultNotActive)

	// A rejected result has no liabilities either.
	w := resultWorker(uuid.NewString())
	w.NetPay = "2400.41"
	rejReq := resultRequest(mid, "RUN-REJ", 1, w)
	rejReq.RunTotals.NetPay = "2400.41"
	rej, err := s.ReceivePayrollResult(ctx, prepare(tenant, rejReq), nil)
	require.NoError(t, err)
	require.Equal(t, domain.ResultRejected, rej.Result.Outcome)
	_, err = pool.Exec(context.Background(), `INSERT INTO wfp_payroll_liabilities
		(liability_id, tenant_id, legal_entity_id, run_ref_id, result_id, payroll_period_id, class_code, class_row_id, liability_type, party_side,
		 authority_counterparty_id, jurisdiction_id, rule_ref, component_kinds, source_component_count, amount, currency, due_date, due_rule_kind,
		 due_rule_value, state, correlation_id, created_by)
		SELECT gen_random_uuid(), $1, 'le-1', $2, $3, '2026-09', 'PAYE', class_row_id, 'TAX', 'EMPLOYEE_WITHHELD', 'cp', 'GB', 'r', '{}', 1, 10, 'GBP',
		       '2026-10-22', 'DAYS_AFTER_PAY_DATE', 1, 'RECOGNIZED', 'c', 'x' FROM wfp_liability_classes WHERE tenant_id = $1 LIMIT 1`,
		tenant, rej.Ref.RunRefID, rej.Result.ResultID)
	assert.Error(t, err, "a rejected result cannot be the source of a liability")
}

func TestWFP06_UnmappedComponentsAndMissingClassesAreVisibleExceptions(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerClass(t, s, ctx, classReq(domain.KindEmployeeTax, "PAYE", "cp-hmrc", "TAX", domain.DueDayOfFollowingMonth, 22, "2026-01-01", false))
	registerClass(t, s, ctx, classReq(domain.KindEmployerSocial, "NI-ER", "cp-hmrc", "SOCIAL", domain.DueDayOfFollowingMonth, 22, "2026-01-01", true))
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)

	out := recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	require.Len(t, out.Liabilities, 2, "what the registry covers is recognized")
	require.Len(t, out.Exceptions, 1)
	e := out.Exceptions[0]
	assert.Equal(t, domain.ExcUnmappedClass, e.Code)
	assert.Equal(t, "EMPLOYEE_PENSION:PEN", e.SubjectKey)
	require.NotNil(t, e.Amount)
	assert.Equal(t, "100.0000", *e.Amount)
	assert.Equal(t, domain.ExcOpen, e.State)

	// Recognizing again does not duplicate the exception or its event.
	recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	open, err := s.ListLiabilityExceptions(ctx, res.Ref.RunRefID, "", "OPEN")
	require.NoError(t, err)
	assert.Len(t, open, 1)
	assert.Equal(t, 1, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollReconExceptionCreated))

	// Registering the class and recognizing again fixes the cause: the exception resolves
	// (it is never dismissed by hand) and the liability appears.
	registerClass(t, s, ctx, classReq(domain.KindEmployeePension, "PENSION", "cp-nest", "PENSION", domain.DueDaysAfterPayDate, 10, "2026-01-01", false))
	fixed := recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	assert.Equal(t, 1, fixed.NewCount)
	require.Len(t, fixed.Exceptions, 1)
	assert.Equal(t, domain.ExcResolved, fixed.Exceptions[0].State)
	require.NotNil(t, fixed.Exceptions[0].Resolution)
	assert.Equal(t, "CAUSE_CLEARED", *fixed.Exceptions[0].Resolution)

	// An exception is never deleted and its identity never changes.
	for _, stmt := range []string{
		`DELETE FROM wfp_liability_exceptions WHERE exception_id = $1`,
		`UPDATE wfp_liability_exceptions SET detail = 'changed' WHERE exception_id = $1`,
		`UPDATE wfp_liability_exceptions SET code = 'OTHER' WHERE exception_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), stmt, fixed.Exceptions[0].ExceptionID)
		assert.Error(t, err, stmt)
	}
}

func TestWFP06_ZoikoResultWithoutEmployerLiabilitiesRaisesACompletenessException(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx) // NI-ER is required
	mid := registerManifest(t, s, ctx)

	// A Zoiko-style result: employee side only, flagged; no employer component at all.
	w := resultWorker(uuid.NewString())
	w.Components = []domain.ResultComponentInput{
		rcomp("BASE", "EARNING", "3000"), rcomp("HRA", "EARNING", "100.5"), rcomp("TAX", "EMPLOYEE_TAX", "600.10"), rcomp("PEN", "EMPLOYEE_PENSION", "100"),
	}
	w.EmployerLiabilities = "0"
	req := resultRequest(mid, "RUN-Z", 1, w)
	req.EvidenceFlags = []string{domain.FlagPlaceholderTaxRate, domain.FlagEmployerLiabilitiesNotComputed}
	req.RunTotals.EmployerLiabilities = "0"
	out, err := s.ReceivePayrollResult(ctx, prepare(tenant, req), resultEvents(tenant))
	require.NoError(t, err)
	require.Equal(t, domain.ResultAccepted, out.Result.Outcome)

	got := recognize(t, s, ctx, tenant, out.Ref.RunRefID, nil, "2026-09-30")
	require.Len(t, got.Liabilities, 2, "employee-side liabilities are recognized")
	codes := map[string]bool{}
	for _, e := range got.Exceptions {
		codes[e.Code+"/"+e.SubjectKey] = true
		assert.Equal(t, domain.ExcOpen, e.State)
	}
	assert.True(t, codes[domain.ExcEmployerNotComputed+"/RESULT"], "the gap is visible, not silent")
	assert.True(t, codes[domain.ExcMissingExpected+"/EMPLOYER_SOCIAL:*"])
	assert.Equal(t, 2, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollReconExceptionCreated))
}

func TestWFP06_AnInvalidAuthorityIsRecognizedUnderReviewAndCannotBeScheduled(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)

	out := recognize(t, s, ctx, tenant, res.Ref.RunRefID, map[string]string{"cp-hmrc": domain.ErrAuthorityInvalid.Error()}, "2026-09-30")
	paye := liabOf(t, out, "PAYE")
	assert.Equal(t, domain.LiabExcReviewRequired, paye.ExceptionState)
	assert.Equal(t, domain.LiabExcNone, liabOf(t, out, "PENSION").ExceptionState)
	var authExc int
	for _, e := range out.Exceptions {
		if e.Code == domain.ExcAuthorityInvalid {
			authExc++
			assert.Equal(t, "cp-hmrc", e.SubjectKey)
		}
	}
	assert.Equal(t, 1, authExc, "one exception per bad authority, however many liabilities use it")

	_, err := s.ScheduleLiability(ctx, paye.LiabilityID, paye.Version, "2026-10-15", "p-4", liabEvents(tenant))
	assert.ErrorIs(t, err, domain.ErrLiabilityNeedsReview)
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payroll_liabilities SET state = 'SCHEDULED' WHERE liability_id = $1`, paye.LiabilityID)
	assert.Error(t, err, "the database also refuses to schedule a liability under review")

	// The authority becomes valid: recognition clears the review and the exception.
	fixed := recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	paye = liabOf(t, fixed, "PAYE")
	assert.Equal(t, domain.LiabExcNone, paye.ExceptionState)
	for _, e := range fixed.Exceptions {
		if e.Code == domain.ExcAuthorityInvalid {
			assert.Equal(t, domain.ExcResolved, e.State)
		}
	}
	sched, err := s.ScheduleLiability(ctx, paye.LiabilityID, paye.Version, "2026-10-15", "p-4", liabEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.LiabScheduled, sched.State)
	require.NotNil(t, sched.ScheduledFor)
	assert.Equal(t, "2026-10-15", *sched.ScheduledFor)
}

func TestWFP06_DueSweepScheduleAndVersioning(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	out := recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")

	// Before any due date nothing is DUE; on 2026-10-10 only the pension liability is.
	due, err := s.DueSweep(ctx, "le-1", "2026-10-09", liabEvents(tenant))
	require.NoError(t, err)
	assert.Empty(t, due)
	due, err = s.DueSweep(ctx, "le-1", "2026-10-10", liabEvents(tenant))
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, "PENSION", due[0].ClassCode)
	assert.Equal(t, domain.LiabDue, due[0].State)
	again, _ := s.DueSweep(ctx, "le-1", "2026-10-10", liabEvents(tenant))
	assert.Len(t, again, 1, "the sweep is idempotent: it returns DUE liabilities without moving them twice")
	assert.Equal(t, 1, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollLiabilityDue))

	// Recognition after the due date marks things DUE in the same transaction.
	res2 := acceptedResult(t, s, ctx, tenant, mid, "RUN-2", 1)
	late := recognize(t, s, ctx, tenant, res2.Ref.RunRefID, nil, "2026-11-01")
	for _, l := range late.Liabilities {
		assert.Equal(t, domain.LiabDue, l.State, l.ClassCode)
	}

	pen := due[0]
	_, err = s.ScheduleLiability(ctx, pen.LiabilityID, pen.Version+5, "2026-10-12", "p-4", nil)
	assert.ErrorIs(t, err, domain.ErrLiabilityVersionConflict)
	sched, err := s.ScheduleLiability(ctx, pen.LiabilityID, pen.Version, "2026-10-12", "p-4", liabEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.LiabScheduled, sched.State)
	assert.Equal(t, "0.0000", sched.SettledAmount, "scheduling pays nothing")
	assert.Equal(t, "100.0000", sched.OutstandingAmount, "still fully outstanding")
	_, err = s.ScheduleLiability(ctx, pen.LiabilityID, sched.Version, "2026-10-13", "p-4", nil)
	assert.ErrorIs(t, err, domain.ErrLiabilityState, "already scheduled")
	_ = out
}

func TestWFP06_SettlementIsEvidencedPartialIdempotentAndBounded(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	paye := liabOf(t, recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30"), "PAYE") // owes 600.1000

	// Partial settlement keeps the residual.
	p1, err := s.SettleLiability(ctx, paye.LiabilityID, settleReq("pay-1", "200.10"), settled, "p-5", liabEvents(tenant))
	require.NoError(t, err)
	assert.False(t, p1.Replay)
	assert.Equal(t, domain.LiabPartiallySettled, p1.Liability.State)
	assert.Equal(t, "200.1000", p1.Liability.SettledAmount)
	assert.Equal(t, "400.0000", p1.Liability.OutstandingAmount)
	assert.Equal(t, domain.AmountDeclared, p1.Settlement.AmountEvidence, "the amount is disclosed as declared, not bank-verified")

	// The same payment again is the same settlement, not counted twice.
	rep, err := s.SettleLiability(ctx, paye.LiabilityID, settleReq("pay-1", "200.10"), settled, "p-5", liabEvents(tenant))
	require.NoError(t, err)
	assert.True(t, rep.Replay)
	assert.Equal(t, "200.1000", rep.Liability.SettledAmount)
	assert.Equal(t, p1.Settlement.SettlementID, rep.Settlement.SettlementID)
	assert.Equal(t, 1, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollLiabilitySettled))
	// ...but not with a different amount.
	_, err = s.SettleLiability(ctx, paye.LiabilityID, settleReq("pay-1", "300"), settled, "p-5", nil)
	assert.ErrorIs(t, err, domain.ErrSettlementConflict)

	// Never beyond what is owed; never in another currency.
	_, err = s.SettleLiability(ctx, paye.LiabilityID, settleReq("pay-2", "400.0001"), settled, "p-5", nil)
	assert.ErrorIs(t, err, domain.ErrOverSettlement)
	_, err = s.SettleLiability(ctx, paye.LiabilityID, domain.SettleLiabilityRequest{PaymentID: "pay-2", Amount: "10", Currency: "USD"}, settled, "p-5", nil)
	assert.ErrorIs(t, err, domain.ErrSettlementCurrency)
	got, _ := s.GetLiability(ctx, paye.LiabilityID)
	assert.Equal(t, "200.1000", got.SettledAmount, "refused settlements changed nothing")

	// The remainder settles it.
	p2, err := s.SettleLiability(ctx, paye.LiabilityID, settleReq("pay-2", "400"), settled, "p-5", liabEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.LiabSettled, p2.Liability.State)
	assert.Equal(t, "0.0000", p2.Liability.OutstandingAmount)
	_, err = s.SettleLiability(ctx, paye.LiabilityID, settleReq("pay-3", "1"), settled, "p-5", nil)
	assert.ErrorIs(t, err, domain.ErrLiabilityState, "a settled liability takes no more")
	_, err = s.ScheduleLiability(ctx, paye.LiabilityID, p2.Liability.Version, "2026-10-20", "p-4", nil)
	assert.ErrorIs(t, err, domain.ErrLiabilityState)

	detail, err := s.GetLiability(ctx, paye.LiabilityID)
	require.NoError(t, err)
	require.Len(t, detail.Settlements, 2)
	assert.Equal(t, "pay-1", detail.Settlements[0].PaymentID)
	assert.Equal(t, "SETTLED", detail.Settlements[0].BankingStatus)

	// The database holds the same lines with no store in the way.
	for _, stmt := range []string{
		`UPDATE wfp_payroll_liabilities SET settled_amount = 700 WHERE liability_id = $1`,                  // beyond what is owed
		`UPDATE wfp_payroll_liabilities SET state = 'SETTLED', settled_amount = 1 WHERE liability_id = $1`, // settled without the full amount
		`UPDATE wfp_payroll_liabilities SET state = 'RECOGNIZED' WHERE liability_id = $1`,                  // backwards
		`UPDATE wfp_liability_settlements SET amount = 1 WHERE liability_id = $1`,                          // evidence is immutable
		`DELETE FROM wfp_liability_settlements WHERE liability_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), stmt, paye.LiabilityID)
		assert.Error(t, err, stmt)
	}
	_, err = pool.Exec(context.Background(), `INSERT INTO wfp_liability_settlements (settlement_id, tenant_id, liability_id, payment_id, amount, currency, declared_by, banking_status)
		VALUES (gen_random_uuid(), $1, $2, 'pay-x', 1, 'GBP', 'x', 'PENDING')`, tenant, paye.LiabilityID)
	assert.Error(t, err, "only a SETTLED Banking status can be recorded")
}

func TestWFP06_AdjustmentsAreExplicitAndBoundedBySettlement(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	out := recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	pen := liabOf(t, out, "PENSION") // owes 100

	adj := func(l domain.PayrollLiability, delta string) (*domain.PayrollLiability, error) {
		return s.AdjustLiability(ctx, l.LiabilityID, domain.AdjustLiabilityRequest{ExpectedVersion: l.Version, Delta: delta, Reason: "provider invoice", EvidenceRef: "inv-1"}, "p-6", liabEvents(tenant))
	}
	up, err := adj(pen, "25.5")
	require.NoError(t, err)
	assert.Equal(t, "100.0000", up.Amount, "the original amount is never edited")
	assert.Equal(t, "25.5000", up.AdjustmentTotal)
	assert.Equal(t, "125.5000", up.OutstandingAmount)

	_, err = s.AdjustLiability(ctx, pen.LiabilityID, domain.AdjustLiabilityRequest{ExpectedVersion: 1, Delta: "1", Reason: "r", EvidenceRef: "e"}, "p-6", nil)
	assert.ErrorIs(t, err, domain.ErrLiabilityVersionConflict)

	// Settle most of it, then try to cut what is owed below what is settled.
	part, err := s.SettleLiability(ctx, pen.LiabilityID, settleReq("pay-1", "120"), settled, "p-5", liabEvents(tenant))
	require.NoError(t, err)
	_, err = adj(*part.Liability, "-6")
	assert.ErrorIs(t, err, domain.ErrAdjustBelowSettled)
	closed, err := adj(*part.Liability, "-5.5") // owed 120 = settled 120
	require.NoError(t, err)
	assert.Equal(t, domain.LiabSettled, closed.State, "the adjustment closed the residual")

	// An increase on a settled liability reopens the residual.
	reopened, err := adj(*closed, "10")
	require.NoError(t, err)
	assert.Equal(t, domain.LiabPartiallySettled, reopened.State)
	assert.Equal(t, "10.0000", reopened.OutstandingAmount)

	detail, _ := s.GetLiability(ctx, pen.LiabilityID)
	require.Len(t, detail.Adjustments, 3, "every adjustment is its own evidenced record")
	assert.Equal(t, 3, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollLiabilityAdjusted))

	// An unsettled liability adjusted to nothing is superseded, not left at zero.
	paye := liabOf(t, out, "PAYE")
	gone, err := adj(paye, "-600.10")
	require.NoError(t, err)
	assert.Equal(t, domain.LiabSuperseded, gone.State)
	_, err = adj(*gone, "1")
	assert.ErrorIs(t, err, domain.ErrLiabilityState)

	_, err = pool.Exec(context.Background(), `UPDATE wfp_liability_adjustments SET delta = 99 WHERE liability_id = $1`, pen.LiabilityID)
	assert.Error(t, err, "adjustments are immutable")
	_, err = pool.Exec(context.Background(), `DELETE FROM wfp_liability_adjustments WHERE liability_id = $1`, pen.LiabilityID)
	assert.Error(t, err)
}

func TestWFP06_DisputeIsOrthogonalAndBlocksScheduling(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	paye := liabOf(t, recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30"), "PAYE")

	d, err := s.DisputeLiability(ctx, paye.LiabilityID, paye.Version, "authority claims a different amount", liabEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.LiabExcDisputed, d.ExceptionState)
	assert.Equal(t, domain.LiabRecognized, d.State, "the lifecycle state is untouched")
	require.NotNil(t, d.DisputeReason)

	_, err = s.ScheduleLiability(ctx, paye.LiabilityID, d.Version, "2026-10-15", "p-4", nil)
	assert.ErrorIs(t, err, domain.ErrLiabilityNeedsReview)
	_, err = s.DisputeLiability(ctx, paye.LiabilityID, d.Version, "again", nil)
	assert.ErrorIs(t, err, domain.ErrLiabilityState, "already disputed")
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payroll_liabilities SET exception_state = 'DISPUTED', dispute_reason = NULL WHERE liability_id = $1`, paye.LiabilityID)
	assert.Error(t, err)

	c, err := s.ClearLiabilityDispute(ctx, paye.LiabilityID, d.Version, liabEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.LiabExcNone, c.ExceptionState)
	assert.Nil(t, c.DisputeReason)
	_, err = s.ScheduleLiability(ctx, paye.LiabilityID, c.Version, "2026-10-15", "p-4", nil)
	assert.NoError(t, err)
	assert.Equal(t, 2, countType(t, rEventTypes(t, pool, tenant), domain.EventPayrollLiabilityDisputed))
}

func TestWFP06_SupersessionFollowsTheCorrectionChain(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	v1 := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	out := recognize(t, s, ctx, tenant, v1.Ref.RunRefID, nil, "2026-09-30")
	paye := liabOf(t, out, "PAYE")

	_, err := s.SupersedeLiabilitiesForRun(ctx, v1.Ref.RunRefID, nil)
	assert.ErrorIs(t, err, domain.ErrResultNotSuperseded, "an ACCEPTED result's liabilities are not superseded")

	// Money already moved against the pension liability.
	pen := liabOf(t, out, "PENSION")
	_, err = s.SettleLiability(ctx, pen.LiabilityID, settleReq("pay-1", "40"), settled, "p-5", nil)
	require.NoError(t, err)

	v2 := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 2) // corrects v1
	sup, err := s.SupersedeLiabilitiesForRun(ctx, v1.Ref.RunRefID, liabEvents(tenant))
	require.NoError(t, err)
	require.Len(t, sup.Superseded, 2, "the unsettled ones are superseded")
	require.Len(t, sup.NeedsAdjustment, 1, "the one with a settlement needs an explicit adjustment")
	assert.Equal(t, pen.LiabilityID, sup.NeedsAdjustment[0].LiabilityID)
	got, _ := s.GetLiability(ctx, paye.LiabilityID)
	assert.Equal(t, domain.LiabSuperseded, got.State)
	assert.Equal(t, "600.1000", got.Amount, "the superseded liability is kept as it was")

	again, err := s.SupersedeLiabilitiesForRun(ctx, v1.Ref.RunRefID, liabEvents(tenant))
	require.NoError(t, err)
	assert.Empty(t, again.Superseded, "idempotent")
	assert.Len(t, again.NeedsAdjustment, 1)

	// The pension liability is corrected by evidence, then it is fully handled.
	adj, err := s.AdjustLiability(ctx, pen.LiabilityID, domain.AdjustLiabilityRequest{ExpectedVersion: sup.NeedsAdjustment[0].Version, Delta: "-60", Reason: "result superseded", EvidenceRef: "chain-1"}, "p-6", liabEvents(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.LiabSettled, adj.State, "40 settled = 40 owed after the adjustment")

	// The replacement result gets its own liabilities.
	fresh := recognize(t, s, ctx, tenant, v2.Ref.RunRefID, nil, "2026-09-30")
	assert.Equal(t, 3, fresh.NewCount)
	assert.NotEqual(t, paye.LiabilityID, liabOf(t, fresh, "PAYE").LiabilityID)
}

func TestWFP06_ReconciliationComparesLiabilitiesWithThePostedJournal(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	// The pension class is missing at first, so the PENSION payable the journal posts has no liability.
	registerClass(t, s, ctx, classReq(domain.KindEmployeeTax, "PAYE", "cp-hmrc", "TAX", domain.DueDayOfFollowingMonth, 22, "2026-01-01", false))
	registerClass(t, s, ctx, classReq(domain.KindEmployerSocial, "NI-ER", "cp-hmrc", "SOCIAL", domain.DueDayOfFollowingMonth, 22, "2026-01-01", true))
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)
	recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")

	_, err := s.ReconcileLiabilities(ctx, res.Ref.RunRefID, liabEvents(tenant))
	assert.ErrorIs(t, err, domain.ErrNoPostedAccounting, "nothing to compare against until the journal is posted")

	b, _, err := s.CreateAccountingBridge(ctx, buildBridge(t, s, ctx, res.Ref.RunRefID, "p-9"), accEvents(tenant))
	require.NoError(t, err)
	_, err = s.SubmitAccountingBridge(ctx, b.BridgeID, b.Version, "p-2", accEvents(tenant))
	require.NoError(t, err)
	fl := &fakeLedger{outcome: posted()}
	n, err := s.DispatchPending(ctx, 10, fl.post, accEvents(tenant))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	rec, err := s.ReconcileLiabilities(ctx, res.Ref.RunRefID, liabEvents(tenant))
	require.NoError(t, err)
	assert.False(t, rec.Matched, "100 of employee pension was posted as a payable but no liability covers it")
	var diff []string
	for _, l := range rec.Lines {
		if !l.Matches {
			diff = append(diff, l.MappingKey)
			assert.Equal(t, "0.0000", l.Liabilities)
			assert.Equal(t, "100.0000", l.Posted)
		}
	}
	assert.Equal(t, []string{"PAYROLL_EMPLOYEE_PENSION_PAYABLE"}, diff)
	open, err := s.ListLiabilityExceptions(ctx, res.Ref.RunRefID, "", "OPEN")
	require.NoError(t, err)
	var codes []string
	for _, e := range open {
		codes = append(codes, e.Code)
	}
	assert.Contains(t, codes, domain.ExcGLMismatch)
	list, _ := s.ListLiabilities(ctx, store.LiabilityFilter{RunRefID: res.Ref.RunRefID})
	for _, l := range list {
		assert.Equal(t, "MISMATCH", l.ReconciliationState)
	}
	types := rEventTypes(t, pool, tenant)
	assert.Equal(t, 2, countType(t, types, domain.EventPayrollLiabilityReconciliationFailed), "one per liability of the result")

	// Fix the registry, recognize, pay PAYE in full, reconcile again.
	registerClass(t, s, ctx, classReq(domain.KindEmployeePension, "PENSION", "cp-nest", "PENSION", domain.DueDaysAfterPayDate, 10, "2026-01-01", false))
	fixed := recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	_, err = s.SettleLiability(ctx, liabOf(t, fixed, "PAYE").LiabilityID, settleReq("pay-1", "600.10"), settled, "p-5", liabEvents(tenant))
	require.NoError(t, err)

	rec, err = s.ReconcileLiabilities(ctx, res.Ref.RunRefID, liabEvents(tenant))
	require.NoError(t, err)
	assert.True(t, rec.Matched)
	for _, l := range rec.Lines {
		assert.True(t, l.Matches, l.MappingKey)
	}
	list, _ = s.ListLiabilities(ctx, store.LiabilityFilter{RunRefID: res.Ref.RunRefID})
	require.Len(t, list, 3)
	for _, l := range list {
		assert.Equal(t, "MATCHED", l.ReconciliationState)
		if l.ClassCode == "PAYE" {
			assert.Equal(t, domain.LiabReconciled, l.State, "settled AND reconciled")
		} else {
			assert.Equal(t, domain.LiabRecognized, l.State, "unsettled liabilities are matched, not reconciled")
		}
	}
	closed, err := s.ListLiabilityExceptions(ctx, res.Ref.RunRefID, "", "RESOLVED")
	require.NoError(t, err)
	var resolvedGL bool
	for _, e := range closed {
		if e.Code == domain.ExcGLMismatch {
			resolvedGL = true
		}
	}
	assert.True(t, resolvedGL, "the mismatch closed because its cause was fixed")
}

func TestWFP06_EventsAreAtomicWithTheChange(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	registerStandardClasses(t, s, ctx)
	mid := registerManifest(t, s, ctx)
	res := acceptedResult(t, s, ctx, tenant, mid, "RUN-1", 1)

	ref, _ := s.GetPayrollRunRef(ctx, res.Ref.RunRefID)
	result, _ := s.GetPayrollResultByRef(ctx, res.Ref.RunRefID)
	comps, _ := s.ListResultComponents(ctx, res.Ref.RunRefID, "")
	classes, _ := s.ListLiabilityClasses(ctx, "le-1")
	plan, err := domain.PlanRecognition(domain.PlanInput{PayDate: ref.PayDate, Components: comps, Classes: classes})
	require.NoError(t, err)
	boom := func(store.LiabilityChange) ([]domain.OutboxEvent, error) {
		return nil, errors.New("event builder failed")
	}
	_, err = s.ApplyRecognition(ctx, ref, result, plan, map[string]string{}, "p-3", "corr", "2026-09-30", boom)
	require.Error(t, err)
	list, _ := s.ListLiabilities(ctx, store.LiabilityFilter{RunRefID: res.Ref.RunRefID})
	assert.Empty(t, list, "a failed event rolls the recognition back")

	out := recognize(t, s, ctx, tenant, res.Ref.RunRefID, nil, "2026-09-30")
	paye := liabOf(t, out, "PAYE")
	_, err = s.ScheduleLiability(ctx, paye.LiabilityID, paye.Version, "2026-10-15", "p-4", boom)
	require.Error(t, err)
	got, _ := s.GetLiability(ctx, paye.LiabilityID)
	assert.Equal(t, domain.LiabRecognized, got.State, "and so does a failed event on a later command")
	assert.Equal(t, paye.Version, got.Version)
}

func TestWFP06_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenantA, ctxA := wfpTenant()
	tenantB, ctxB := wfpTenant()
	registerStandardClasses(t, s, ctxA)
	registerStandardClasses(t, s, ctxB)
	mA, mB := registerManifest(t, s, ctxA), registerManifest(t, s, ctxB)
	resA := acceptedResult(t, s, ctxA, tenantA, mA, "RUN-1", 1)
	resB := acceptedResult(t, s, ctxB, tenantB, mB, "RUN-1", 1)
	outA := recognize(t, s, ctxA, tenantA, resA.Ref.RunRefID, nil, "2026-09-30")
	recognize(t, s, ctxB, tenantB, resB.Ref.RunRefID, nil, "2026-09-30")
	payeA := liabOf(t, outA, "PAYE")

	_, err := s.GetLiability(ctxB, payeA.LiabilityID)
	assert.ErrorIs(t, err, domain.ErrLiabilityNotFound)
	_, err = s.SettleLiability(ctxB, payeA.LiabilityID, settleReq("pay-1", "1"), settled, "p", nil)
	assert.ErrorIs(t, err, domain.ErrLiabilityNotFound)
	_, err = s.ScheduleLiability(ctxB, payeA.LiabilityID, payeA.Version, "2026-10-15", "p", nil)
	assert.ErrorIs(t, err, domain.ErrLiabilityNotFound)
	_, err = s.AdjustLiability(ctxB, payeA.LiabilityID, domain.AdjustLiabilityRequest{ExpectedVersion: 1, Delta: "1", Reason: "r", EvidenceRef: "e"}, "p", nil)
	assert.ErrorIs(t, err, domain.ErrLiabilityNotFound)
	_, err = s.ReconcileLiabilities(ctxB, resA.Ref.RunRefID, nil)
	assert.ErrorIs(t, err, domain.ErrResultNotFound)
	_, err = s.SupersedeLiabilitiesForRun(ctxB, resA.Ref.RunRefID, nil)
	assert.ErrorIs(t, err, domain.ErrRunRefNotFound)

	listB, err := s.ListLiabilities(ctxB, store.LiabilityFilter{LegalEntityID: "le-1"})
	require.NoError(t, err)
	require.Len(t, listB, 3)
	for _, l := range listB {
		assert.Equal(t, tenantB, l.TenantID)
	}
}

func TestWFP06_WorksUnderRowLevelSecurity(t *testing.T) {
	admin := openTestPool(t)
	app := appPool(t, admin)
	s := store.New(app, "")
	tenantA, ctxA := wfpTenant()
	tenantB, ctxB := wfpTenant()
	registerStandardClasses(t, s, ctxA)
	registerStandardClasses(t, s, ctxB)
	mA, mB := registerManifest(t, s, ctxA), registerManifest(t, s, ctxB)
	resA := acceptedResult(t, s, ctxA, tenantA, mA, "RUN-1", 1)
	resB := acceptedResult(t, s, ctxB, tenantB, mB, "RUN-1", 1)

	outA := recognize(t, s, ctxA, tenantA, resA.Ref.RunRefID, nil, "2026-10-30")
	outB := recognize(t, s, ctxB, tenantB, resB.Ref.RunRefID, nil, "2026-09-30")
	require.Len(t, outA.Liabilities, 3)
	for _, l := range outA.Liabilities {
		assert.Equal(t, domain.LiabDue, l.State, "swept under the restricted role")
		assert.Equal(t, tenantA, l.TenantID)
	}

	paye := liabOf(t, outA, "PAYE")
	set, err := s.SettleLiability(ctxA, paye.LiabilityID, settleReq("pay-1", "600.10"), settled, "p-5", liabEvents(tenantA))
	require.NoError(t, err)
	assert.Equal(t, domain.LiabSettled, set.Liability.State)

	// Tenant B cannot see or touch tenant A's rows, and a connection with no tenant sees none.
	_, err = s.GetLiability(ctxB, paye.LiabilityID)
	assert.ErrorIs(t, err, domain.ErrLiabilityNotFound)
	var n int
	require.NoError(t, app.QueryRow(context.Background(), `SELECT count(*) FROM wfp_payroll_liabilities`).Scan(&n))
	assert.Equal(t, 0, n, "no tenant context, no rows")
	require.NoError(t, app.QueryRow(context.Background(), `SELECT count(*) FROM wfp_liability_classes`).Scan(&n))
	assert.Equal(t, 0, n)
	_, err = app.Exec(context.Background(), `UPDATE wfp_payroll_liabilities SET created_by = 'x'`)
	assert.NoError(t, err, "an UPDATE with no tenant context matches nothing")
	got, _ := s.GetLiability(ctxA, paye.LiabilityID)
	assert.Equal(t, "p-3", got.CreatedBy)
	assert.Len(t, outB.Liabilities, 3)
}
