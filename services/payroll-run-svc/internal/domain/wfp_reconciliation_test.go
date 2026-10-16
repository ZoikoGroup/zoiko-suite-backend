package domain

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rat(s string) *big.Rat {
	r, err := ParseDecimal(s)
	if err != nil {
		panic(err)
	}
	return r
}

func lines() []PostingLine {
	l, err := BuildPostingLines(ResultPostingInput{
		Gross: rat("3100.50"), Net: rat("2400.40"),
		ByKind: map[string]*big.Rat{KindEmployeeTax: rat("600.10"), KindEmployeePension: rat("100"), KindEmployerSocial: rat("250")},
	})
	if err != nil {
		panic(err)
	}
	return l
}

func TestReconcileResultToGL_MatchMismatchAndNotPosted(t *testing.T) {
	exp := lines()
	ok := ReconcileResultToGL(exp, &AccountingBridge{Lines: exp})
	assert.True(t, ok.Matched)
	assert.Empty(t, ok.Exceptions)
	assert.Equal(t, CtrlPayrollToGL, ok.Control)

	changed := append([]PostingLine(nil), exp...)
	for i := range changed {
		if changed[i].MappingKey == KeyGrossExpense {
			changed[i].Debit = "3100.4900"
		}
	}
	bad := ReconcileResultToGL(exp, &AccountingBridge{Lines: changed})
	assert.False(t, bad.Matched)
	require.Len(t, bad.Exceptions, 1)
	assert.Equal(t, ReconGLMismatch, bad.Exceptions[0].Code)
	assert.Equal(t, KeyGrossExpense+"|DR", bad.Exceptions[0].SubjectKey)
	assert.Equal(t, SeverityBlocking, bad.Exceptions[0].Severity)
	require.NotNil(t, bad.Exceptions[0].Amount)
	assert.Equal(t, "-0.0100", *bad.Exceptions[0].Amount, "the exact difference, never rounded")

	missing := ReconcileResultToGL(exp, nil)
	assert.False(t, missing.Matched)
	require.Len(t, missing.Exceptions, 1)
	assert.Equal(t, ReconResultNotPosted, missing.Exceptions[0].Code)

	extra := append(append([]PostingLine(nil), exp...), PostingLine{MappingKey: "PAYROLL_OTHER_PAYABLE", Debit: "0.0000", Credit: "5.0000"})
	e := ReconcileResultToGL(exp, &AccountingBridge{Lines: extra})
	assert.False(t, e.Matched, "a line posted that the result does not imply is a difference too")
	assert.Equal(t, "PAYROLL_OTHER_PAYABLE|CR", e.Exceptions[0].SubjectKey)
}

func TestReconcileResultToLiabilities_ComparesEveryPayableExceptNetPay(t *testing.T) {
	exp := lines()
	rec := map[string]*big.Rat{
		EmployeePayableKey(KindEmployeeTax): rat("600.10"), EmployeePayableKey(KindEmployeePension): rat("100"), EmployerPayableKey(KindEmployerSocial): rat("250"),
	}
	assert.True(t, ReconcileResultToLiabilities(exp, rec).Matched, "net pay is not a liability")

	delete(rec, EmployeePayableKey(KindEmployeePension))
	a := ReconcileResultToLiabilities(exp, rec)
	assert.False(t, a.Matched)
	require.Len(t, a.Exceptions, 1)
	assert.Equal(t, ReconLiabilityMismatch, a.Exceptions[0].Code)
	assert.Equal(t, EmployeePayableKey(KindEmployeePension), a.Exceptions[0].SubjectKey)
	assert.Equal(t, "-100.0000", *a.Exceptions[0].Amount)

	rec[EmployeePayableKey(KindEmployeePension)] = rat("100")
	rec["PAYROLL_EMPLOYEE_GARNISHMENT_PAYABLE"] = rat("9")
	b := ReconcileResultToLiabilities(exp, rec)
	assert.False(t, b.Matched, "a liability the result never implied is a difference too")
}

func wid(s string) *string { return &s }

func TestReconcileNetPay(t *testing.T) {
	net := map[string]*big.Rat{"w1": rat("100"), "w2": rat("50.5"), "w0": rat("0")}
	ins := []InstructionFact{
		{InstructionID: "i1", Type: PayTypeNetPay, WorkerID: wid("w1"), State: PayApproved, Amount: "100.0000"},
		{InstructionID: "i2", Type: PayTypeNetPay, WorkerID: wid("w2"), State: PayHandedOff, Amount: "50.5000"},
	}
	assert.True(t, ReconcileNetPay(net, ins).Matched, "a worker owed nothing needs no instruction")

	ins[1].Amount = "50.4000"
	a := ReconcileNetPay(net, ins)
	require.Len(t, a.Exceptions, 1)
	assert.Equal(t, ReconNetPayAmountMismatch, a.Exceptions[0].Code)
	assert.Equal(t, "w2", a.Exceptions[0].SubjectKey)

	for _, st := range []string{PayCancelled, PaySuperseded, PayReturned} {
		ins[1].State, ins[1].Amount = st, "50.5000"
		b := ReconcileNetPay(net, ins)
		require.Len(t, b.Exceptions, 1, st)
		assert.Equal(t, ReconNetPayNotInstructed, b.Exceptions[0].Code, st)
	}

	ins = []InstructionFact{
		{InstructionID: "a", Type: PayTypeNetPay, WorkerID: wid("w1"), State: PayDraft, Amount: "60.0000"},
		{InstructionID: "b", Type: PayTypeNetPay, WorkerID: wid("w1"), State: PayDraft, Amount: "40.0000"},
		{InstructionID: "c", Type: PayTypeNetPay, WorkerID: wid("w2"), State: PayDraft, Amount: "50.5000"},
		{InstructionID: "d", Type: PayTypeLiab, State: PayDraft, Amount: "999.0000"},
	}
	assert.True(t, ReconcileNetPay(net, ins).Matched, "two live instructions that add up are fine; liability instructions are not net pay")
}

func TestReconcileBankingOutcomes(t *testing.T) {
	mk := func(id, state, outcome string) InstructionFact {
		return InstructionFact{InstructionID: id, State: state, Amount: "10.0000", LatestOutcome: outcome}
	}
	a := ReconcileBankingOutcomes([]InstructionFact{
		mk("settled", PayHandedOff, OutcomeSettled), mk("returned", PayHandedOff, OutcomeReturned), mk("failed", PaySubmitted, OutcomeFailed),
		mk("unknown", PayHandedOff, OutcomeUnresolved), mk("flight", PayHandedOff, OutcomeInFlight), mk("none", PaySubmitted, ""),
		mk("draft", PayDraft, ""), mk("approved", PayApproved, ""), mk("cancelled", PayCancelled, ""),
	})
	assert.False(t, a.Matched)
	got := map[string][2]string{}
	for _, e := range a.Exceptions {
		got[e.SubjectKey] = [2]string{e.Code, e.Severity}
	}
	assert.Equal(t, map[string][2]string{
		"returned": {ReconPaymentReturned, SeverityBlocking}, "failed": {ReconPaymentFailed, SeverityBlocking},
		"unknown": {ReconOutcomeUnresolved, SeverityBlocking},
		"flight":  {ReconPaymentInFlight, SeverityWarning}, "none": {ReconNoBankingObservation, SeverityWarning},
	}, got, "only instructions that reached Banking are compared; only SETTLED matches")
	assert.Len(t, a.Lines, 6)
}

func TestRunOutcomeCollectAndHash(t *testing.T) {
	m := Assertion{Matched: true}
	assert.Equal(t, ReconOutcomeMatched, RunOutcome([]Assertion{m}))
	assert.Equal(t, ReconOutcomeBreak, RunOutcome([]Assertion{m, {Matched: false}}))
	assert.Equal(t, ReconOutcomeBreak, RunOutcome([]Assertion{{Matched: true, Exceptions: []ReconExceptionDraft{{Code: "X"}}}}), "an exception, even a warning, is a break")

	dup := []Assertion{
		{Exceptions: []ReconExceptionDraft{{Code: "A", SubjectKey: "1"}, {Code: "A", SubjectKey: "2"}}},
		{Exceptions: []ReconExceptionDraft{{Code: "A", SubjectKey: "1"}}},
	}
	assert.Len(t, CollectExceptions(dup), 2, "the same code and subject is one exception")

	p := ReconPopulation{
		ResultID: "r", ResultFingerprint: "f", WorkerNet: map[string]string{"b": "2", "a": "1"},
		Liabilities:  []LiabilityFact{{LiabilityID: "2"}, {LiabilityID: "1"}},
		Instructions: []InstructionFact{{InstructionID: "y"}, {InstructionID: "x"}},
	}
	h1, raw, err := p.Hash()
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"a":"1","b":"2"`, "map keys are sorted")
	q := ReconPopulation{
		ResultID: "r", ResultFingerprint: "f", WorkerNet: map[string]string{"a": "1", "b": "2"},
		Liabilities:  []LiabilityFact{{LiabilityID: "1"}, {LiabilityID: "2"}},
		Instructions: []InstructionFact{{InstructionID: "x"}, {InstructionID: "y"}},
	}
	h2, _, err := q.Hash()
	require.NoError(t, err)
	assert.Equal(t, h1, h2, "the hash does not depend on the order things were read in")
	q.Instructions[0].LatestOutcome = OutcomeReturned
	h3, _, _ := q.Hash()
	assert.NotEqual(t, h1, h3, "a changed Banking outcome changes the population")
}

func TestOutcomeMapping(t *testing.T) {
	for in, want := range map[string]string{
		"PREPARED": OutcomeInFlight, "SUBMITTED": OutcomeInFlight, "REJECTED_BEFORE_SUBMISSION": OutcomeFailed, "CANCELLED": OutcomeFailed,
		"PENDING_UNKNOWN": OutcomeUnresolved, "QUARANTINED": OutcomeUnresolved, "SOMETHING_NEW": OutcomeUnresolved, "": OutcomeUnresolved, " submitted ": OutcomeInFlight,
	} {
		assert.Equal(t, want, OutcomeOfAttempt(in), in)
	}
	for in, want := range map[string]string{
		"SETTLED": OutcomeSettled, "RETURNED": OutcomeReturned, "REJECTED": OutcomeFailed, "CANCELLED": OutcomeFailed,
		"PREPARED": OutcomeInFlight, "SUBMITTED": OutcomeInFlight, "ACCEPTED": OutcomeInFlight, "PENDING": OutcomeUnresolved, "WEIRD": OutcomeUnresolved,
	} {
		assert.Equal(t, want, OutcomeOfPayment(in), in)
	}
	assert.True(t, InstructionIsLive(PayHandedOff))
	assert.False(t, InstructionIsLive(PayReturned))
}
