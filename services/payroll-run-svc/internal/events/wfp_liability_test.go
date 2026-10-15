package events

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func TestBuildLiabilityEvents(t *testing.T) {
	l := &domain.PayrollLiability{
		LiabilityID: "l-1", TenantID: "t-1", LegalEntityID: "le-1", RunRefID: "rr-1", ResultID: "res-1", ClassCode: "PAYE", LiabilityType: "TAX",
		AuthorityCounterpartyID: "cp-1", Currency: "GBP", DueDate: "2026-10-22", State: domain.LiabSettled, ExceptionState: domain.LiabExcNone, Version: 4,
	}
	want := map[string]string{
		"RECOGNIZED": domain.EventPayrollLiabilityRecognized, "DUE": domain.EventPayrollLiabilityDue, "SCHEDULED": domain.EventPayrollLiabilityScheduled,
		"SETTLED": domain.EventPayrollLiabilitySettled, "ADJUSTED": domain.EventPayrollLiabilityAdjusted, "SUPERSEDED": domain.EventPayrollLiabilityAdjusted,
		"DISPUTED": domain.EventPayrollLiabilityDisputed, "DISPUTE_CLEARED": domain.EventPayrollLiabilityDisputed,
		"RECONCILIATION_FAILED": domain.EventPayrollLiabilityReconciliationFailed,
	}
	for kind, typ := range want {
		evs, err := BuildLiabilityEvents(kind, l, nil, map[string]any{"payment_id": "pay-1"}, "p-1", "corr-1", "")
		require.NoError(t, err, kind)
		require.Len(t, evs, 1)
		assert.Equal(t, typ, evs[0].EventType, kind)
		assert.True(t, domain.EventCatalogue[evs[0].EventType], "%s is in the catalogue", typ)
		assert.Equal(t, "res-1", evs[0].ObjectID, "keyed by result so one result's liabilities stay in order")
		assert.Equal(t, "t-1", evs[0].TenantID)
		var env map[string]any
		require.NoError(t, json.Unmarshal(evs[0].Payload, &env))
		assert.NotContains(t, string(evs[0].Payload), `"amount"`, "no payroll amounts travel on the event")
		assert.Contains(t, string(evs[0].Payload), "pay-1", "detail is merged")
	}

	exc := &domain.LiabilityException{ExceptionID: "e-1", TenantID: "t-1", LegalEntityID: "le-1", ResultID: "res-1", Code: domain.ExcUnmappedClass, SubjectKey: "EMPLOYEE_PENSION:PEN"}
	evs, err := BuildLiabilityEvents("EXCEPTION", nil, exc, nil, "p-1", "corr-1", "")
	require.NoError(t, err)
	assert.Equal(t, domain.EventPayrollReconExceptionCreated, evs[0].EventType)

	_, err = BuildLiabilityEvents("BOGUS", l, nil, nil, "p", "c", "")
	assert.Error(t, err)
	_, err = BuildLiabilityEvents("SETTLED", nil, nil, nil, "p", "c", "")
	assert.Error(t, err)
	_, err = BuildLiabilityEvents("EXCEPTION", nil, nil, nil, "p", "c", "")
	assert.Error(t, err)
}
