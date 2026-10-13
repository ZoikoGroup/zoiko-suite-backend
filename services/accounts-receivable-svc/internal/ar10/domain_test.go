package ar10

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

func amt(s string) kit.Amount { return kit.MustAmount(s) }

func TestRefundBinding_ChangesWithEveryBoundField(t *testing.T) {
	base := RefundBinding("r1", "rc1", "USD", amt("10"), "dest-1", "fp-1", "payer-1")
	require.Equal(t, base, RefundBinding("r1", "rc1", "USD", amt("10"), "dest-1", "fp-1", "payer-1"), "deterministic")
	for name, other := range map[string]string{
		"destination": RefundBinding("r1", "rc1", "USD", amt("10"), "dest-2", "fp-1", "payer-1"),
		"fingerprint": RefundBinding("r1", "rc1", "USD", amt("10"), "dest-1", "fp-2", "payer-1"),
		"amount":      RefundBinding("r1", "rc1", "USD", amt("10.0001"), "dest-1", "fp-1", "payer-1"),
		"receipt":     RefundBinding("r1", "rc2", "USD", amt("10"), "dest-1", "fp-1", "payer-1"),
		"payer":       RefundBinding("r1", "rc1", "USD", amt("10"), "dest-1", "fp-1", "payer-2"),
		"currency":    RefundBinding("r1", "rc1", "EUR", amt("10"), "dest-1", "fp-1", "payer-1"),
	} {
		require.NotEqual(t, base, other, name)
	}
}

func TestWriteOffBinding_BoundToTaxTreatment(t *testing.T) {
	a := WriteOffBinding("w", "i", "USD", amt("100"), "BAD_DEBT", TaxReviewNeeded, 0)
	require.NotEqual(t, a, WriteOffBinding("w", "i", "USD", amt("100"), "BAD_DEBT", TaxNoneRequired, 0))
	require.NotEqual(t, WriteOffBinding("w", "i", "USD", amt("100"), "BAD_DEBT", TaxReliefClaimed, amt("10")), WriteOffBinding("w", "i", "USD", amt("100"), "BAD_DEBT", TaxReliefClaimed, amt("11")))
	require.NotEqual(t, a, WriteOffBinding("w", "i", "USD", amt("101"), "BAD_DEBT", TaxReviewNeeded, 0))
}

func TestRefundStateMachine(t *testing.T) {
	for _, s := range []string{RfDraft, RfPendingApproval, RfApproved} {
		require.True(t, RefundCanCancel(s), s)
	}
	for _, s := range []string{RfSubmitted, RfPending, RfUnknown, RfSettled, RfFailed, RfCancelled} {
		require.False(t, RefundCanCancel(s), s)
	}
	for status, want := range map[string]struct {
		state string
		final bool
	}{
		PaySettled: {RfSettled, true}, PayRejected: {RfFailed, true}, PayCancelled: {RfFailed, true}, PayReturned: {RfFailed, true},
		PayPending: {RfPending, false}, PayAccepted: {RfPending, false}, PaySubmitted: {RfPending, false}, PayPrepared: {RfPending, false},
	} {
		st, fin := RefundMapPayment(status)
		require.Equal(t, want.state, st, status)
		require.Equal(t, want.final, fin, status)
	}
}

func balanced(t *testing.T, lines []kit.PostingLine) kit.PostingEvent {
	t.Helper()
	ev := kit.PostingEvent{SourceEventID: "x", Lines: lines}
	require.NoError(t, ev.Validate(), "debits must equal credits")
	return ev
}

func TestRefundPosting(t *testing.T) {
	ev := balanced(t, BuildRefundPosting(amt("123.45")))
	require.Equal(t, KeyUnappliedCash, ev.Lines[0].MappingKey)
	require.Equal(t, amt("123.45"), ev.Lines[0].Debit)
	require.Equal(t, KeyCashReceipt, ev.Lines[1].MappingKey)
	require.Equal(t, amt("123.45"), ev.Lines[1].Credit)
}

func TestWriteOffPosting(t *testing.T) {
	w := &WriteOff{Amount: amt("1000"), ReasonCategory: "BAD_DEBT", TaxTreatment: TaxNoneRequired}
	ev := balanced(t, BuildWriteOffPosting(w))
	require.Len(t, ev.Lines, 2)
	require.Equal(t, KeyBadDebt, ev.Lines[0].MappingKey)
	require.Equal(t, KeyReceivable, ev.Lines[1].MappingKey)

	w.TaxTreatment, w.TaxReliefAmount = TaxReliefClaimed, amt("166.67")
	ev = balanced(t, BuildWriteOffPosting(w))
	require.Len(t, ev.Lines, 3)
	require.Equal(t, amt("833.33"), ev.Lines[0].Debit)
	require.Equal(t, KeyTaxOutput, ev.Lines[1].MappingKey)
	require.Equal(t, amt("166.67"), ev.Lines[1].Debit)

	w.TaxReliefAmount = w.Amount // all of it is tax: no expense line, still balanced
	ev = balanced(t, BuildWriteOffPosting(w))
	require.Len(t, ev.Lines, 2)
	require.Equal(t, KeyTaxOutput, ev.Lines[0].MappingKey)
}

func TestConstantTierPolicy_RequiredAndHeld(t *testing.T) {
	az := &fakeAuthz{grants: standardGrants()}
	p := NewConstantTierPolicy(az)
	for amount, tier := range map[string]int{"0.0001": 1, "500": 1, "500.0001": 2, "5000": 2, "5000.01": 3, "50000": 3, "50000.01": 4, "9999999": 4} {
		require.Equal(t, tier, p.Required(amt(amount), "USD").Level, amount)
	}
	for who, want := range map[string]int{pTier1: 1, pTier2: 2, pTier3: 3, pTier4: 4, pRequester: 0, pNobody: 0, pWOAll: 3} {
		got, err := p.Held(context.Background(), kit.Caller{PrincipalID: who, LegalEntityID: "e"})
		require.NoError(t, err)
		require.Equal(t, want, got, who)
	}
}

func TestCheckDestination(t *testing.T) {
	party := uuid.NewString()
	ok := &DestinationInfo{Status: "ACTIVE", Fingerprint: "fp", PartyRef: party, Currency: "USD"}
	require.NoError(t, checkDestination(ok, party, "USD"))
	require.NoError(t, checkDestination(&DestinationInfo{Status: "ACTIVE", Fingerprint: "fp"}, party, "USD"), "party / currency unknown at the source are not blockers")
	for name, d := range map[string]*DestinationInfo{
		"not active":     {Status: "VERIFIED", Fingerprint: "fp"},
		"suspended":      {Status: "SUSPENDED", Fingerprint: "fp"},
		"no fingerprint": {Status: "ACTIVE"},
		"other party":    {Status: "ACTIVE", Fingerprint: "fp", PartyRef: uuid.NewString()},
		"other currency": {Status: "ACTIVE", Fingerprint: "fp", Currency: "EUR"},
	} {
		err := checkDestination(d, party, "USD")
		require.Error(t, err, name)
		require.Equal(t, http.StatusUnprocessableEntity, err.(*kit.Problem).Status, name)
	}
}

func TestValidTax(t *testing.T) {
	require.NoError(t, validTax(TaxNoneRequired, 0, amt("100"), "USD"))
	require.Error(t, validTax(TaxNoneRequired, amt("1"), amt("100"), "USD"))
	require.NoError(t, validTax(TaxReliefClaimed, amt("16.67"), amt("100"), "USD"))
	require.Error(t, validTax(TaxReliefClaimed, 0, amt("100"), "USD"), "relief claimed needs an amount")
	require.Error(t, validTax(TaxReliefClaimed, amt("101"), amt("100"), "USD"), "relief cannot exceed the write-off")
	require.Error(t, validTax(TaxReliefClaimed, amt("1.001"), amt("100"), "USD"), "relief must fit the currency")
	require.NoError(t, validTax(TaxReviewNeeded, 0, amt("100"), "USD"))
	require.Error(t, validTax("MAYBE", 0, amt("100"), "USD"))
}

// ── handler tests that need no database: every refusal happens before storage ──

func notWired() *Module {
	return New(testDeps(&fakeAuthz{grants: standardGrants()}), nil, nil, nil, nil, nil, nil, nil)
}

func TestRoutes_MissingIdempotencyKeyIsRefused(t *testing.T) {
	h := testRouter(notWired())
	for _, p := range []string{"createRefundRequest", "changeRefundDestination", "approveRefund", "submitRefundForPayment", "reconcileRefundState", "cancelUnsubmittedRefund",
		"createWriteOffRequest", "resolveWriteOffTaxTreatment", "approveWriteOff", "commitWriteOff"} {
		req := httptest.NewRequest("POST", base+"commands/"+p, nil)
		req.Header.Set("X-Tenant-Id", "tenant-1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, p)
		require.Equal(t, "IDEMPOTENCY_KEY_REQUIRED", errCode(t, rec.Body.Bytes()), p)
	}
}

func createRefund(m *Module, principal string, body map[string]any) (int, []byte) {
	rec := httptest.NewRecorder()
	m.createRefundRequest(rec, newReq("POST", base+"commands/createRefundRequest", "tenant-1", principal, body))
	return rec.Code, rec.Body.Bytes()
}

func refundBody(over map[string]any) map[string]any {
	b := map[string]any{"legal_entity_id": uuid.NewString(), "receipt_id": uuid.NewString(), "amount": "100.00", "destination_ref": "dest-1", "payer_account_ref": "payer-1",
		"reason_code": "OVERPAYMENT", "evidence_id": "ev-1"}
	for k, v := range over {
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
	}
	return b
}

func TestCreateRefund_RefusalsBeforeStorage(t *testing.T) {
	m := notWired()
	code, body := createRefund(m, pNobody, refundBody(nil))
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, "FORBIDDEN", errCode(t, body))
	code, body = createRefund(m, pApprover, refundBody(nil))
	require.Equal(t, http.StatusForbidden, code, "approving is not creating")
	require.Equal(t, "FORBIDDEN", errCode(t, body))
	for name, c := range map[string]struct {
		over map[string]any
		code string
	}{
		"no reason":          {map[string]any{"reason_code": ""}, "REASON_REQUIRED"},
		"no evidence":        {map[string]any{"evidence_id": ""}, "EVIDENCE_REQUIRED"},
		"zero amount":        {map[string]any{"amount": "0"}, "VALIDATION_FAILED"},
		"no destination":     {map[string]any{"destination_ref": ""}, "VALIDATION_FAILED"},
		"bad receipt id":     {map[string]any{"receipt_id": "nope"}, "VALIDATION_FAILED"},
		"raw bank details":   {map[string]any{"iban": "GB00"}, "VALIDATION_FAILED"},
		"unknown amount fmt": {map[string]any{"amount": "1.123456789"}, "VALIDATION_FAILED"},
	} {
		code, body := createRefund(m, pRequester, refundBody(c.over))
		require.Equal(t, c.code, errCode(t, body), name)
		require.GreaterOrEqual(t, code, 400, name)
	}
	code, body = createRefund(m, pRequester, refundBody(nil))
	require.Equal(t, http.StatusServiceUnavailable, code, "AR-08 / destination verification not wired: fail closed, no state change")
	require.Equal(t, "DEPENDENCY_UNAVAILABLE", errCode(t, body))
}

func createWriteOff(m *Module, principal string, body map[string]any) (int, []byte) {
	rec := httptest.NewRecorder()
	m.createWriteOffRequest(rec, newReq("POST", base+"commands/createWriteOffRequest", "tenant-1", principal, body))
	return rec.Code, rec.Body.Bytes()
}

func writeOffBody(over map[string]any) map[string]any {
	b := map[string]any{"legal_entity_id": uuid.NewString(), "open_item_id": uuid.NewString(), "amount": "100.00", "reason_category": "BAD_DEBT",
		"reason_code": "UNCOLLECTIBLE", "evidence_id": "ev-1"}
	for k, v := range over {
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
	}
	return b
}

// TestCreateWriteOff_RefusalsBeforeStorage: a write-off without a controlled reason, evidence or a tax decision is refused.
func TestCreateWriteOff_RefusalsBeforeStorage(t *testing.T) {
	m := notWired()
	code, body := createWriteOff(m, pNobody, writeOffBody(nil))
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, "FORBIDDEN", errCode(t, body))
	for name, c := range map[string]struct {
		over map[string]any
		code string
	}{
		"free-text category": {map[string]any{"reason_category": "I felt like it"}, "VALIDATION_FAILED"},
		"no category":        {map[string]any{"reason_category": ""}, "VALIDATION_FAILED"},
		"no reason":          {map[string]any{"reason_code": ""}, "REASON_REQUIRED"},
		"no evidence":        {map[string]any{"evidence_id": ""}, "EVIDENCE_REQUIRED"},
		"zero amount":        {map[string]any{"amount": "0"}, "VALIDATION_FAILED"},
		"bad item id":        {map[string]any{"open_item_id": "x"}, "VALIDATION_FAILED"},
		"unknown tax value":  {map[string]any{"tax_treatment": "MAYBE"}, "VALIDATION_FAILED"},
		"relief w/o review":  {map[string]any{"tax_treatment": "REVIEW_REQUIRED", "tax_relief_amount": "5"}, "VALIDATION_FAILED"},
	} {
		_, body := createWriteOff(m, pRequester, writeOffBody(c.over))
		require.Equal(t, c.code, errCode(t, body), name)
	}
	// an explicit tax decision needs the tax-review authority (the requester cannot self-certify the tax treatment) and evidence
	code, body = createWriteOff(m, pRequester, writeOffBody(map[string]any{"tax_treatment": "NONE_REQUIRED", "tax_evidence_id": "t-1"}))
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, "FORBIDDEN", errCode(t, body))
	m2 := New(testDeps(&fakeAuthz{grants: map[string]map[string]bool{"both": {ActionWriteOffCreate: true, ActionWriteOffTaxReview: true}}}), nil, nil, nil, nil, nil, nil, nil)
	_, body = createWriteOff(m2, "both", writeOffBody(map[string]any{"tax_treatment": "NONE_REQUIRED"}))
	require.Equal(t, "EVIDENCE_REQUIRED", errCode(t, body))
	code, body = createWriteOff(m, pRequester, writeOffBody(nil))
	require.Equal(t, http.StatusServiceUnavailable, code, "AR-07 not wired: 503, nothing recorded")
	require.Equal(t, "DEPENDENCY_UNAVAILABLE", errCode(t, body))
}

func TestHTTPGateway_WireIsPascalCase(t *testing.T) {
	// payment-initiation-adapter-svc and payment-status-svc decode WITHOUT json tags: the wire keys are the Go field names.
	b, err := json.Marshal(prepareWire{LegalEntityID: "e", IdempotencyKey: "k", Amount: 1.5})
	require.NoError(t, err)
	require.Contains(t, string(b), `"LegalEntityID":"e"`)
	require.Contains(t, string(b), `"IdempotencyKey":"k"`)
	require.Contains(t, string(b), `"PayerAccountVerified":false`)
}
