package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseInstruction() *PaymentInstruction {
	w := "w-1"
	return &PaymentInstruction{
		TenantID: "t-1", LegalEntityID: "le-1", InstructionType: PayTypeNetPay, ResultID: "res-1", WorkerID: &w, PayeeRef: "payee-1",
		DestinationID: "dest-1", DestinationVersion: "2026-01-01T00:00:00Z", DestinationFingerprint: "bank-fp", Amount: "2400.4000", Currency: "GBP",
		ValueDate: "2026-09-30", PayerAccountRef: "acct-1", PaymentMethod: PayMethodBank, PaymentReference: "Payroll 2026-09",
	}
}

func TestInstructionFingerprintBindsEveryProtectedFact(t *testing.T) {
	base := ComputeInstructionFingerprint(baseInstruction())
	assert.Equal(t, base, ComputeInstructionFingerprint(baseInstruction()), "deterministic")
	assert.Contains(t, base, "sha256:")

	changes := map[string]func(*PaymentInstruction){
		"tenant":              func(i *PaymentInstruction) { i.TenantID = "t-2" },
		"entity":              func(i *PaymentInstruction) { i.LegalEntityID = "le-2" },
		"result":              func(i *PaymentInstruction) { i.ResultID = "res-2" },
		"worker":              func(i *PaymentInstruction) { w := "w-2"; i.WorkerID = &w },
		"payee ref":           func(i *PaymentInstruction) { i.PayeeRef = "payee-2" },
		"destination":         func(i *PaymentInstruction) { i.DestinationID = "dest-2" },
		"destination version": func(i *PaymentInstruction) { i.DestinationVersion = "2026-02-01T00:00:00Z" },
		"bank detail digest":  func(i *PaymentInstruction) { i.DestinationFingerprint = "other" },
		"amount":              func(i *PaymentInstruction) { i.Amount = "2400.4001" },
		"currency":            func(i *PaymentInstruction) { i.Currency = "USD" },
		"value date":          func(i *PaymentInstruction) { i.ValueDate = "2026-10-01" },
		"paying account":      func(i *PaymentInstruction) { i.PayerAccountRef = "acct-2" },
		"reference":           func(i *PaymentInstruction) { i.PaymentReference = "Payroll 2026-10" },
		"type and subject": func(i *PaymentInstruction) {
			l := "l-1"
			i.InstructionType, i.WorkerID, i.LiabilityID = PayTypeLiab, nil, &l
		},
	}
	seen := map[string]string{base: "base"}
	for name, mutate := range changes {
		i := baseInstruction()
		mutate(i)
		fp := ComputeInstructionFingerprint(i)
		assert.NotEqual(t, base, fp, name)
		assert.NotContains(t, seen, fp, "%s collides with %s", name, seen[fp])
		seen[fp] = name
	}

	// Fields cannot bleed into each other.
	a, b := baseInstruction(), baseInstruction()
	a.PayeeRef, a.DestinationID = "ab", "c"
	b.PayeeRef, b.DestinationID = "a", "bc"
	assert.NotEqual(t, ComputeInstructionFingerprint(a), ComputeInstructionFingerprint(b))
}

func TestCreatePaymentInstructionRequestValidation(t *testing.T) {
	w, l := "w-1", "l-1"
	ok := CreatePaymentInstructionRequest{RunRefID: "r", InstructionType: PayTypeNetPay, WorkerID: &w, PayerAccountRef: "acct-1", ValueDate: "2026-09-30"}
	r := ok
	require.NoError(t, r.Validate())
	assert.Equal(t, PayMethodBank, r.PaymentMethod, "a default method")

	liab := CreatePaymentInstructionRequest{RunRefID: "r", InstructionType: PayTypeLiab, LiabilityID: &l, Amount: "10.5", PayerAccountRef: "a", ValueDate: "2026-09-30"}
	require.NoError(t, liab.Validate())

	bad := map[string]func(*CreatePaymentInstructionRequest){
		"no account":          func(r *CreatePaymentInstructionRequest) { r.PayerAccountRef = " " },
		"bad date":            func(r *CreatePaymentInstructionRequest) { r.ValueDate = "30/09/2026" },
		"bad method":          func(r *CreatePaymentInstructionRequest) { r.PaymentMethod = "CASH" },
		"no worker":           func(r *CreatePaymentInstructionRequest) { r.WorkerID = nil },
		"both subjects":       func(r *CreatePaymentInstructionRequest) { r.LiabilityID = &l },
		"net pay with amount": func(r *CreatePaymentInstructionRequest) { r.Amount = "5" },
		"unknown type":        func(r *CreatePaymentInstructionRequest) { r.InstructionType = "BONUS" },
		"long key":            func(r *CreatePaymentInstructionRequest) { r.IdempotencyKey = string(make([]byte, 129)) },
	}
	for name, mutate := range bad {
		r := ok
		mutate(&r)
		assert.Error(t, r.Validate(), name)
	}
	badLiab := liab
	badLiab.Amount = "-1"
	assert.Error(t, badLiab.Validate())
	badLiab = liab
	badLiab.WorkerID = &w
	assert.Error(t, badLiab.Validate())
}

func TestOnlyASubmittedAttemptIsAHandoff(t *testing.T) {
	assert.True(t, HandedOffStatus("SUBMITTED"))
	for _, s := range []string{"PREPARED", "PENDING_UNKNOWN", "REJECTED_BEFORE_SUBMISSION", "CANCELLED", "QUARANTINED", "UNKNOWN", "SUBMITTING", ""} {
		assert.False(t, HandedOffStatus(s), s)
	}
}

func TestPayeeDestinationChangedBy(t *testing.T) {
	d := PayeeDestination{ChangedBy: []string{"proposer", "", "approver"}}
	assert.True(t, d.ChangedByPrincipal("proposer"))
	assert.True(t, d.ChangedByPrincipal("approver"))
	assert.False(t, d.ChangedByPrincipal("someone"))
	assert.False(t, d.ChangedByPrincipal(""), "an empty principal never matches an empty recorded one")
}

func TestPaymentReferenceCarriesNoPersonalOrBankData(t *testing.T) {
	assert.Equal(t, "Payroll 2026-09", PaymentReferenceFor(PayTypeNetPay, "2026-09", ""))
	assert.Equal(t, "Payroll liability PAYE 2026-09", PaymentReferenceFor(PayTypeLiab, "2026-09", "PAYE"))
	assert.Equal(t, "wfp-payroll-instruction:abc", BankingIdempotencyKey("abc"))
}
