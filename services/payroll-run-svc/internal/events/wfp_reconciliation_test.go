package events

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func TestBuildReconEvents(t *testing.T) {
	run := &domain.ReconRun{ReconRunID: "rr-1", TenantID: "t-1", LegalEntityID: "le-1", RunRefID: "ref-1", ResultID: "res-1", Outcome: domain.ReconOutcomeBreak, PopulationHash: "h", ExceptionCount: 2}
	amt := "-1.0000"
	exc := &domain.ReconException{ExceptionID: "e-1", TenantID: "t-1", LegalEntityID: "le-1", RunRefID: "ref-1", ResultID: "res-1", ReconRunID: "rr-1",
		Code: domain.ReconGLMismatch, SubjectKey: "K|DR", Severity: domain.SeverityBlocking, Amount: &amt, State: domain.ReconStateOpen}
	ins := &domain.PaymentInstruction{InstructionID: "i-1", TenantID: "t-1", LegalEntityID: "le-1", RunRefID: "ref-1", ResultID: "res-1",
		InstructionType: domain.PayTypeNetPay, State: domain.PayReturned, PayeeRef: "SECRET-PAYEE", DestinationID: "SECRET-DEST", Amount: "99.0000"}
	obs := &domain.BankingObservation{Source: domain.ObsSourcePayment, RawStatus: "RETURNED", Outcome: domain.OutcomeReturned}

	cases := []struct {
		kind string
		typ  string
	}{
		{"RUN_COMPLETED", domain.EventPayrollReconRunCompleted},
		{"RUN_CERTIFIED", domain.EventPayrollReconRunCertified},
		{"EXCEPTION_CREATED", domain.EventPayrollReconExceptionCreated},
		{"EXCEPTION_RESOLVED", domain.EventPayrollReconExceptionResolved},
		{"EXCEPTION_WAIVED", domain.EventPayrollReconExceptionWaived},
		{"BANKING_OBSERVED", domain.EventPayrollBankingOutcomeObserved},
		{"INSTRUCTION_RETURNED", domain.EventPayrollPaymentInstrReturned},
	}
	for _, c := range cases {
		evs, err := BuildReconEvents(c.kind, run, exc, ins, obs, "actor", "corr", "cause")
		require.NoError(t, err, c.kind)
		require.Len(t, evs, 1, c.kind)
		assert.Equal(t, c.typ, evs[0].EventType, c.kind)
		assert.Equal(t, "t-1", evs[0].TenantID, c.kind)
		assert.Equal(t, "res-1", evs[0].ObjectID, "%s is keyed by the payroll result so its stream stays in order", c.kind)
		assert.True(t, domain.EventCatalogue[evs[0].EventType], "%s is in the catalogue", c.kind)
		var env map[string]any
		require.NoError(t, json.Unmarshal(evs[0].Payload, &env), c.kind)
		assert.Equal(t, c.typ, env["event_type"])
		assert.Equal(t, "cause", env["causation_id"])
		for _, secret := range []string{"SECRET-PAYEE", "SECRET-DEST", "99.0000"} {
			assert.NotContains(t, string(evs[0].Payload), secret, "%s carries no payee, destination or payment amount", c.kind)
		}
	}

	_, err := BuildReconEvents("NOPE", run, exc, ins, obs, "a", "c", "")
	assert.Error(t, err)
	_, err = BuildReconEvents("RUN_COMPLETED", nil, nil, nil, nil, "a", "c", "")
	assert.Error(t, err)
	_, err = BuildReconEvents("EXCEPTION_WAIVED", nil, nil, nil, nil, "a", "c", "")
	assert.Error(t, err)
	_, err = BuildReconEvents("BANKING_OBSERVED", nil, nil, ins, nil, "a", "c", "")
	assert.Error(t, err)
	_, err = BuildReconEvents("INSTRUCTION_RETURNED", nil, nil, nil, nil, "a", "c", "")
	assert.Error(t, err)
}
