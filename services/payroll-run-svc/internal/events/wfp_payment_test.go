package events

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func TestBuildPaymentEvents(t *testing.T) {
	w := "w-1"
	i := &domain.PaymentInstruction{
		InstructionID: "i-1", TenantID: "t-1", LegalEntityID: "le-1", InstructionType: domain.PayTypeNetPay, RunRefID: "rr-1", ResultID: "res-1", WorkerID: &w,
		PayeeRef: "payee-secret-ref", DestinationID: "dest-secret", DestinationFingerprint: "bank-digest", Amount: "2400.4000", Currency: "GBP",
		ValueDate: "2026-09-30", Fingerprint: "sha256:abc", State: domain.PayApproved, RequiredSignatures: 2, Version: 4,
	}
	want := map[string]string{
		"CREATED": domain.EventPayrollPaymentInstrCreated, "APPROVED": domain.EventPayrollPaymentInstrApproved,
		"INVALIDATED": domain.EventPayrollPaymentInstrInvalidated, "SUBMITTED": domain.EventPayrollPaymentSubmitted,
		"CANCELLED": domain.EventPayrollPaymentInstrCancelled, "SUPERSEDED": domain.EventPayrollPaymentInstrSuperseded,
	}
	for kind, typ := range want {
		evs, err := BuildPaymentEvents(kind, i, "because", "p-1", "corr-1", "")
		require.NoError(t, err, kind)
		require.Len(t, evs, 1)
		assert.Equal(t, typ, evs[0].EventType, kind)
		assert.True(t, domain.EventCatalogue[typ], "%s is in the catalogue", typ)
		assert.Equal(t, "res-1", evs[0].ObjectID, "keyed by result")
		body := string(evs[0].Payload)
		assert.Contains(t, body, "sha256:abc")
		for _, secret := range []string{"payee-secret-ref", "dest-secret", "bank-digest", "2400.4"} {
			assert.NotContains(t, body, secret, "no payee, destination or amount travels on the event")
		}
	}
	_, err := BuildPaymentEvents("BOGUS", i, "", "p", "c", "")
	assert.Error(t, err)
	_, err = BuildPaymentEvents("CREATED", nil, "", "p", "c", "")
	assert.Error(t, err)
}
