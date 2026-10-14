package domain_test

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func rat(s string) *big.Rat {
	r, err := domain.ParseDecimal(s)
	if err != nil {
		panic(err)
	}
	return r
}

// fullInput: gross 3100.50, employee deductions tax 600.10 + pension 100 = 700.10, net 2400.40, employer social 250.
func fullInput() domain.ResultPostingInput {
	return domain.ResultPostingInput{
		Gross: rat("3100.50"), Net: rat("2400.40"),
		ByKind: map[string]*big.Rat{
			domain.KindEarning: rat("3100.50"), domain.KindEmployeeTax: rat("600.10"), domain.KindEmployeePension: rat("100"),
			domain.KindEmployerSocial: rat("250"),
		},
	}
}

func lineMap(lines []domain.PostingLine) map[string]domain.PostingLine {
	out := map[string]domain.PostingLine{}
	for _, l := range lines {
		out[l.MappingKey] = l
	}
	return out
}

func TestBuildPostingLines_ABalancedProposalInMappingKeysOnly(t *testing.T) {
	lines, err := domain.BuildPostingLines(fullInput())
	require.NoError(t, err)
	m := lineMap(lines)

	assert.Equal(t, "3100.5000", m[domain.KeyGrossExpense].Debit, "gross wages expense is a debit")
	assert.Equal(t, "250.0000", m["PAYROLL_EMPLOYER_SOCIAL_EXPENSE"].Debit, "employer cost is a debit")
	assert.Equal(t, "2400.4000", m[domain.KeyNetPayable].Credit, "net pay payable is a credit")
	assert.Equal(t, "600.1000", m["PAYROLL_EMPLOYEE_TAX_PAYABLE"].Credit)
	assert.Equal(t, "100.0000", m["PAYROLL_EMPLOYEE_PENSION_PAYABLE"].Credit)
	assert.Equal(t, "250.0000", m["PAYROLL_EMPLOYER_SOCIAL_PAYABLE"].Credit, "the employer liability is a credit")
	assert.Len(t, lines, 6, "zero kinds produce no lines")

	// Exactly one side per line, and the proposal balances exactly.
	for _, l := range lines {
		d, c := rat(l.Debit), rat(l.Credit)
		assert.True(t, (d.Sign() == 0) != (c.Sign() == 0), "%s must be a debit XOR a credit", l.MappingKey)
		assert.NotEmpty(t, l.MappingKey, "no line names an account code: finance owns the mapping")
	}
	debit, credit, err := domain.SumLines(lines)
	require.NoError(t, err)
	assert.Equal(t, 0, debit.Cmp(credit))
	assert.Equal(t, "3350.5000", domain.FormatDecimal(debit), "gross + employer cost")
}

func TestBuildPostingLines_NoEmployerLiabilitiesPostsWhatExists(t *testing.T) {
	// The Zoiko path computes no employer costs: the proposal is gross / net / withholdings only.
	in := domain.ResultPostingInput{
		Gross: rat("1000"), Net: rat("800"),
		ByKind: map[string]*big.Rat{domain.KindEarning: rat("1000"), domain.KindEmployeeTax: rat("200")},
	}
	lines, err := domain.BuildPostingLines(in)
	require.NoError(t, err)
	m := lineMap(lines)
	assert.Len(t, lines, 3)
	assert.Contains(t, m, domain.KeyGrossExpense)
	assert.Contains(t, m, domain.KeyNetPayable)
	assert.Contains(t, m, "PAYROLL_EMPLOYEE_TAX_PAYABLE")
	for key := range m {
		assert.NotContains(t, key, "EMPLOYER", "nothing is invented for employer cost")
	}
}

func TestBuildPostingLines_RefusesWhatDoesNotBalanceOrHasNothing(t *testing.T) {
	// A result whose gross-to-net does not reconcile is not plugged into balance.
	bad := fullInput()
	bad.Net = rat("2400.41")
	_, err := domain.BuildPostingLines(bad)
	assert.ErrorIs(t, err, domain.ErrUnbalancedPosting)

	_, err = domain.BuildPostingLines(domain.ResultPostingInput{Gross: rat("0"), Net: rat("0"), ByKind: map[string]*big.Rat{}})
	assert.ErrorIs(t, err, domain.ErrNothingToPost)
}

func TestBuildPostingLines_IsDeterministic(t *testing.T) {
	a, err := domain.BuildPostingLines(fullInput())
	require.NoError(t, err)
	b, err := domain.BuildPostingLines(fullInput())
	require.NoError(t, err)
	assert.Equal(t, a, b, "the same result always proposes the same lines in the same order")
	assert.Equal(t, domain.KeyGrossExpense, a[0].MappingKey, "debits first")
}

func TestPayrollMappingKeys(t *testing.T) {
	keys := domain.AllPayrollMappingKeys()
	assert.Len(t, keys, 17, "gross + net + 5 employee payables + 5 employer expenses + 5 employer payables")
	seen := map[string]bool{}
	for _, k := range keys {
		assert.False(t, seen[k], "duplicate key %s", k)
		seen[k] = true
		assert.Regexp(t, `^PAYROLL_[A-Z_]+$`, k)
	}
	for _, k := range []string{"PAYROLL_EMPLOYEE_DEDUCTION_PAYABLE", "PAYROLL_EMPLOYEE_BENEFIT_PAYABLE", "PAYROLL_EMPLOYER_OTHER_EXPENSE", "PAYROLL_EMPLOYER_PENSION_PAYABLE"} {
		assert.True(t, seen[k], k)
	}

	// Every component kind the result interface accepts can be posted.
	in := domain.ResultPostingInput{Gross: rat("0"), Net: rat("0"), ByKind: map[string]*big.Rat{}}
	total := new(big.Rat)
	for _, kind := range []string{
		domain.KindEmployeeDeduction, domain.KindEmployeeTax, domain.KindEmployeeSocial, domain.KindEmployeePension, domain.KindEmployeeBenefit,
	} {
		in.ByKind[kind] = rat("1")
		total.Add(total, rat("1"))
	}
	for _, kind := range []string{
		domain.KindEmployerTax, domain.KindEmployerSocial, domain.KindEmployerPension, domain.KindEmployerBenefit, domain.KindEmployerOther,
	} {
		in.ByKind[kind] = rat("1")
	}
	in.Gross, in.Net = rat("5"), rat("0")
	lines, err := domain.BuildPostingLines(in)
	require.NoError(t, err)
	assert.Len(t, lines, 1+5+5+5, "gross + 5 employer expenses + 5 employee payables + 5 employer payables (net is zero)")
	for _, l := range lines {
		assert.Contains(t, keys, l.MappingKey)
	}
}

func TestSourceEventIDAndPeriod(t *testing.T) {
	a := domain.SourceEventID("res-1", "PRIMARY", domain.PurposeResult)
	assert.Equal(t, a, domain.SourceEventID("res-1", "PRIMARY", domain.PurposeResult), "deterministic: a retry is the same posting")
	assert.NotEqual(t, a, domain.SourceEventID("res-2", "PRIMARY", domain.PurposeResult))
	assert.NotEqual(t, a, domain.SourceEventID("res-1", "OTHER", domain.PurposeResult))
	assert.NotEqual(t, a, domain.SourceEventID("res-1", "PRIMARY", domain.PurposeReversal), "a reversal is its own subject")

	assert.Equal(t, "2026-09", domain.FiscalPeriodOf("2026-09-30"))
	assert.Equal(t, "2026-01", domain.FiscalPeriodOf("2026-01-01"))
}

func TestLedgerEventPayload(t *testing.T) {
	lines, err := domain.BuildPostingLines(fullInput())
	require.NoError(t, err)
	b := &domain.AccountingBridge{
		LegalEntityID: "le-1", FiscalPeriod: "2026-09", Description: "Payroll", SourceEventID: "wfp-payroll:x", CorrelationID: "corr-1",
		TransactionCurrency: "GBP", DocumentDate: "2026-09-30", PostingDate: "2026-09-30", Lines: lines,
	}
	raw, err := b.LedgerEventPayload()
	require.NoError(t, err)

	var got struct {
		LegalEntityID string `json:"legal_entity_id"`
		FiscalPeriod  string `json:"fiscal_period"`
		SourceEventID string `json:"source_event_id"`
		CorrelationID string `json:"correlation_id"`
		Currency      string `json:"transaction_currency"`
		DocumentDate  string `json:"document_date"`
		PostingDate   string `json:"posting_date"`
		Lines         []struct {
			MappingKey  string  `json:"mapping_key"`
			AccountCode *string `json:"account_code"`
			Debit       float64 `json:"debit_amount"`
			Credit      float64 `json:"credit_amount"`
		} `json:"lines"`
	}
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, "2026-09", got.FiscalPeriod)
	assert.Equal(t, "wfp-payroll:x", got.SourceEventID)
	assert.Equal(t, "GBP", got.Currency)
	assert.Equal(t, "2026-09-30", got.DocumentDate)
	require.Len(t, got.Lines, 6)

	var debit, credit float64
	for _, l := range got.Lines {
		assert.NotEmpty(t, l.MappingKey)
		assert.Nil(t, got.Lines[0].AccountCode, "WFP never sends an account code")
		debit += l.Debit
		credit += l.Credit
	}
	assert.InDelta(t, debit, credit, 0.00001)
	assert.NotContains(t, string(raw), "book", "the book is carried, not sent")
}

func TestLedgerEventPayload_RejectsAnUnconvertibleAmount(t *testing.T) {
	b := &domain.AccountingBridge{Lines: []domain.PostingLine{{MappingKey: "K", Debit: "not-a-number", Credit: "0.0000"}}}
	_, err := b.LedgerEventPayload()
	assert.Error(t, err)
}
