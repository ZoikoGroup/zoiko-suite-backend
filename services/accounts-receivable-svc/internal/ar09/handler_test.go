package ar09

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func newUnit() *Module {
	return New(testDeps(&fakeAuthz{grants: standardGrants()}, nil), newFakeRecv(), &fakeProfiles{}, &FakeDeliverer{})
}

// Handlers invoked directly (no idempotency wrapper, no DB): everything decidable before a transaction.
func TestHandlersRefuseBeforeAnyWork(t *testing.T) {
	m := newUnit()
	tenant, entity, cs := uuid.NewString(), uuid.NewString(), uuid.NewString()
	type tc struct {
		name      string
		h         http.HandlerFunc
		principal string
		body      map[string]any
		status    int
		code      string
	}
	for _, c := range []tc{
		{"open without permission", m.openCollectionCase, pNobody, map[string]any{"legal_entity_id": entity}, 403, "FORBIDDEN"},
		{"send needs dunning.send", m.sendDunningNotice, pHold, map[string]any{"legal_entity_id": entity, "case_id": cs}, 403, "FORBIDDEN"},
		{"escalate needs its own permission", m.escalateCollectionCase, pCollector, map[string]any{"legal_entity_id": entity, "case_id": cs}, 403, "FORBIDDEN"},
		{"hold needs hold permission", m.placeCollectionHold, pCollector, map[string]any{"legal_entity_id": entity, "case_id": cs, "reason_code": "X"}, 403, "FORBIDDEN"},
		{"policy needs policy permission", m.publishDunningPolicy, pCollector, map[string]any{"legal_entity_id": entity}, 403, "FORBIDDEN"},
		{"legal entity required", m.advanceDunningStage, pCollector, map[string]any{"case_id": cs}, 400, "VALIDATION_FAILED"},
		{"unknown field rejected", m.advanceDunningStage, pCollector, map[string]any{"legal_entity_id": entity, "case_id": cs, "balance": "0"}, 400, "VALIDATION_FAILED"},
		{"hold reason required", m.placeCollectionHold, pHold, map[string]any{"legal_entity_id": entity, "case_id": cs}, 400, "REASON_REQUIRED"},
		{"resume reason required", m.resumeCollections, pHold, map[string]any{"legal_entity_id": entity, "case_id": cs}, 400, "REASON_REQUIRED"},
		{"escalate reason required", m.escalateCollectionCase, pEscalate, map[string]any{"legal_entity_id": entity, "case_id": cs}, 400, "REASON_REQUIRED"},
		{"escalate referral required", m.escalateCollectionCase, pEscalate, map[string]any{"legal_entity_id": entity, "case_id": cs, "reason_code": "X"}, 400, "VALIDATION_FAILED"},
		{"close outcome required", m.closeCollectionCase, pCollector, map[string]any{"legal_entity_id": entity, "case_id": cs}, 400, "VALIDATION_FAILED"},
		{"abandon needs reason", m.closeCollectionCase, pCollector, map[string]any{"legal_entity_id": entity, "case_id": cs, "outcome": "ABANDONED"}, 400, "REASON_REQUIRED"},
		{"promise date", m.recordPromiseToPay, pCollector, map[string]any{"legal_entity_id": entity, "case_id": cs, "promised_date": "tomorrow", "promised_amount": "1"}, 400, "VALIDATION_FAILED"},
		{"promise amount", m.recordPromiseToPay, pCollector, map[string]any{"legal_entity_id": entity, "case_id": cs, "promised_date": "2026-11-01", "promised_amount": "0"}, 400, "VALIDATION_FAILED"},
		{"open currency", m.openCollectionCase, pCollector, map[string]any{"legal_entity_id": entity, "customer_party_id": uuid.NewString(), "currency": "usd"}, 400, "VALIDATION_FAILED"},
		{"open customer uuid", m.openCollectionCase, pCollector, map[string]any{"legal_entity_id": entity, "customer_party_id": "x", "currency": "USD"}, 400, "VALIDATION_FAILED"},
		{"policy invalid", m.publishDunningPolicy, pPolicy, map[string]any{"legal_entity_id": entity, "dunning_profile": "STD", "effective_from": "2026-01-01"}, 400, "VALIDATION_FAILED"},
	} {
		rec := httptest.NewRecorder()
		c.h(rec, newReq(http.MethodPost, base+"commands/x", tenant, c.principal, c.body))
		require.Equal(t, c.status, rec.Code, c.name+": "+rec.Body.String())
		require.Equal(t, c.code, errCode(t, rec.Body.Bytes()), c.name)
	}
}

func TestQueriesRequireTenantAndPrincipal(t *testing.T) {
	m := newUnit()
	for _, h := range []http.HandlerFunc{m.getCollectionCase, m.getDunningSchedule, m.getCustomerCollectionHistory, m.getAvailableActions, m.getPromisesDue} {
		rec := httptest.NewRecorder()
		h(rec, newReq(http.MethodGet, base+"q", "", "p", nil))
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		rec = httptest.NewRecorder()
		h(rec, newReq(http.MethodGet, base+"q", uuid.NewString(), "", nil))
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}
}

func TestCommandsRequireIdempotencyKey(t *testing.T) {
	m := newUnit()
	req := newReq(http.MethodPost, base+"commands/sendDunningNotice", uuid.NewString(), pCollector, map[string]any{})
	rec := httptest.NewRecorder()
	testRouter(m).ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "IDEMPOTENCY_KEY_REQUIRED", errCode(t, rec.Body.Bytes()))
}

func TestSelectOverdue(t *testing.T) {
	r := newFakeRecv()
	_ = r
	all := mkViews()
	got, err := selectOverdue(all, nil, testNow)
	require.NoError(t, err)
	require.Len(t, got, 1)
	_, err = selectOverdue(all, []string{"current"}, testNow)
	require.Error(t, err, "a not-yet-due item cannot join a case")
	_, err = selectOverdue(all, []string{"missing"}, testNow)
	require.Error(t, err)
	_, err = selectOverdue(all[1:], nil, testNow)
	require.Error(t, err, "no overdue receivable")
}
