package ar08

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Handler tests that need no database: every refusal below happens before the module touches storage.

func regModule(src ReceiptSource) *Module {
	return New(testDeps(&fakeAuthz{grants: standardGrants()}), src, nil, nil, nil)
}

func register(m *Module, principal string, body map[string]any) (int, []byte) {
	rec := httptest.NewRecorder()
	m.registerConfirmedReceipt(rec, newReq("POST", base+"commands/registerConfirmedReceipt", "tenant-1", principal, body))
	return rec.Code, rec.Body.Bytes()
}

func TestRoutes_MissingIdempotencyKeyIsRefused(t *testing.T) {
	m := regModule(newFakeSource())
	h := testRouter(m)
	for _, p := range []string{"registerConfirmedReceipt", "createApplicationBatch", "generateMatchCandidates", "applyReceipt", "approveManualMatch", "unapplyReceipt", "reapplyReceipt", "reconcileApplicationBatch"} {
		req := httptest.NewRequest("POST", base+"commands/"+p, nil)
		req.Header.Set("X-Tenant-Id", "tenant-1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, p)
		require.Equal(t, "IDEMPOTENCY_KEY_REQUIRED", errCode(t, rec.Body.Bytes()), p)
	}
}

func TestRegister_RefusesWithoutAuthorityOrTenant(t *testing.T) {
	m := regModule(newFakeSource())
	entity := uuid.NewString()
	code, body := register(m, pNobody, map[string]any{"legal_entity_id": entity, "bank_transaction_id": uuid.NewString()})
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, "FORBIDDEN", errCode(t, body))
	code, _ = register(m, pClerk, map[string]any{"legal_entity_id": entity, "bank_transaction_id": uuid.NewString()})
	require.Equal(t, http.StatusForbidden, code, "applying cash is not registering a receipt")
	code, body = register(m, pRegistrar, map[string]any{"legal_entity_id": entity, "bank_transaction_id": uuid.NewString(), "amount": "5.00"})
	require.Equal(t, http.StatusBadRequest, code, "the caller cannot supply an amount: it comes from Banking")
	require.Equal(t, "VALIDATION_FAILED", errCode(t, body))
}

// TestNeg22_UnconfirmedPaymentIsNeverRegistered: every non-confirmed Banking answer is SETTLEMENT_NOT_CONFIRMED.
func TestNeg22_UnconfirmedPaymentIsNeverRegistered(t *testing.T) {
	tenant, entity := "tenant-1", uuid.NewString()
	src := newFakeSource()
	m := regModule(src)
	body := func(id string) map[string]any {
		return map[string]any{"legal_entity_id": entity, "bank_transaction_id": id}
	}
	quar, sup, out, bad := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	src.put(tenant, quar, "100.00", "USD", BankQuarantined)
	src.put(tenant, sup, "100.00", "USD", BankSuperseded)
	src.put(tenant, out, "-100.00", "USD", BankNormalized)
	src.put(tenant, bad, "100.00", "usd", BankNormalized)
	for name, id := range map[string]string{"quarantined": quar, "superseded": sup, "outflow": out, "bad currency": bad, "unknown": uuid.NewString()} {
		code, b := register(m, pRegistrar, body(id))
		require.Equal(t, http.StatusUnprocessableEntity, code, name)
		require.Equal(t, "SETTLEMENT_NOT_CONFIRMED", errCode(t, b), name)
	}
	src.setErr(ErrSourceUnavailable)
	code, b := register(m, pRegistrar, body(uuid.NewString()))
	require.Equal(t, http.StatusServiceUnavailable, code)
	require.Equal(t, "SETTLEMENT_NOT_CONFIRMED", errCode(t, b), "Banking down fails closed")
	src.setErr(errors.New("boom"))
	code, b = register(m, pRegistrar, body(uuid.NewString()))
	require.Equal(t, http.StatusServiceUnavailable, code)
	require.Equal(t, "SETTLEMENT_NOT_CONFIRMED", errCode(t, b))
	nosrc := regModule(nil)
	code, b = register(nosrc, pRegistrar, body(uuid.NewString()))
	require.Equal(t, http.StatusServiceUnavailable, code)
	require.Equal(t, "SETTLEMENT_NOT_CONFIRMED", errCode(t, b), "an unwired settlement source never confirms")
}

func TestRegister_RejectsMalformedIdentifiers(t *testing.T) {
	m := regModule(newFakeSource())
	entity := uuid.NewString()
	code, _ := register(m, pRegistrar, map[string]any{"legal_entity_id": entity, "bank_transaction_id": "not-a-uuid"})
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = register(m, pRegistrar, map[string]any{"legal_entity_id": entity, "bank_transaction_id": uuid.NewString(), "customer_party_id": "x"})
	require.Equal(t, http.StatusBadRequest, code)
}
