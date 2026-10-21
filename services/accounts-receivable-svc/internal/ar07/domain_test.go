package ar07

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

func amt(s string) kit.Amount { return kit.MustAmount(s) }

func TestApplyToResidual(t *testing.T) {
	orig := amt("100.00")
	cases := []struct {
		name     string
		residual string
		kind     string
		amount   string
		want     string
		code     string
	}{
		{"partial cash", "100.00", kit.AdjCashApplication, "40.00", "60.0000", ""},
		{"exact settle", "60.00", kit.AdjCreditNote, "60.00", "0.0000", ""},
		{"overshoot is negative residual", "60.00", kit.AdjCashApplication, "60.01", "", kit.CodeReceivableResidualInvalid},
		{"unapply reopens", "0.00", kit.AdjCashUnapply, "30.00", "30.0000", ""},
		{"unapply beyond original", "90.00", kit.AdjCashUnapply, "10.01", "", kit.CodeReceivableResidualInvalid},
		{"refund raises", "50.00", kit.AdjRefund, "10.00", "60.0000", ""},
		{"debit note is not an adjustment", "50.00", kit.AdjDebitNote, "10.00", "", kit.CodeValidationFailed},
		{"unknown kind", "50.00", "SET_BALANCE", "10.00", "", kit.CodeValidationFailed},
		{"zero amount", "50.00", kit.AdjCorrection, "0", "", kit.CodeValidationFailed},
	}
	for _, c := range cases {
		got, err := ApplyToResidual(amt(c.residual), orig, c.kind, amt(c.amount))
		if c.code == "" {
			require.NoError(t, err, c.name)
			require.Equal(t, c.want, got.String(), c.name)
			continue
		}
		var p *kit.Problem
		require.ErrorAs(t, err, &p, c.name)
		require.Equal(t, c.code, p.Code, c.name)
	}
}

func TestDeriveStateAndSettlementDimension(t *testing.T) {
	o := amt("100")
	require.Equal(t, StateOpen, DeriveState(o, o, kit.AdjCashUnapply))
	require.Equal(t, StatePartiallySettl, DeriveState(amt("1"), o, kit.AdjCashApplication))
	require.Equal(t, StateSettled, DeriveState(0, o, kit.AdjCashApplication))
	require.Equal(t, StateSettled, DeriveState(0, o, kit.AdjCreditNote))
	require.Equal(t, StateWrittenOff, DeriveState(0, o, kit.AdjWriteOff))
	require.Equal(t, StateCorrected, DeriveState(0, o, kit.AdjCorrection))

	require.Equal(t, "UNPAID", SettlementDimension(o, o, kit.AdjCashUnapply))
	require.Equal(t, "PARTIALLY_PAID", SettlementDimension(amt("5"), o, kit.AdjCashApplication))
	require.Equal(t, "PAID", SettlementDimension(0, o, kit.AdjCashApplication))
	require.Equal(t, "WRITTEN_OFF", SettlementDimension(0, o, kit.AdjWriteOff))
	require.Equal(t, "CREDITED", SettlementDimension(0, o, kit.AdjCreditNote))
	require.True(t, IsTerminal(StateWrittenOff) && IsTerminal(StateCorrected) && !IsTerminal(StateSettled))
}

func TestAgingBucket(t *testing.T) {
	for d, want := range map[int]string{-5: "current", 0: "current", 1: "1-30", 30: "1-30", 31: "31-60", 60: "31-60", 61: "61-90", 90: "61-90", 91: "90+", 400: "90+"} {
		require.Equal(t, want, AgingBucket(d), d)
	}
}

func validCreate() kit.OpenItemFromDocument {
	return kit.OpenItemFromDocument{DocumentKind: kit.DocInvoice, SourceDocumentID: uuid.NewString(), DocumentNumber: "INV-1", CustomerPartyID: uuid.NewString(),
		Currency: "USD", OriginalAmount: amt("100.00"), IssueDate: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), DueDate: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)}
}

func TestValidateCreate(t *testing.T) {
	require.NoError(t, ValidateCreate(validCreate()))
	mut := map[string]func(*kit.OpenItemFromDocument){
		"credit note":   func(i *kit.OpenItemFromDocument) { i.DocumentKind = kit.DocCreditNote },
		"bad currency":  func(i *kit.OpenItemFromDocument) { i.Currency = "usd" },
		"zero amount":   func(i *kit.OpenItemFromDocument) { i.OriginalAmount = 0 },
		"jpy decimals":  func(i *kit.OpenItemFromDocument) { i.Currency, i.OriginalAmount = "JPY", amt("10.50") },
		"due<issue":     func(i *kit.OpenItemFromDocument) { i.DueDate = i.IssueDate.AddDate(0, 0, -1) },
		"bad doc id":    func(i *kit.OpenItemFromDocument) { i.SourceDocumentID = "x" },
		"bad customer":  func(i *kit.OpenItemFromDocument) { i.CustomerPartyID = "" },
		"missing dates": func(i *kit.OpenItemFromDocument) { i.IssueDate = time.Time{} },
	}
	for name, f := range mut {
		in := validCreate()
		f(&in)
		require.Error(t, ValidateCreate(in), name)
	}
}

func TestValidateAdjustment(t *testing.T) {
	ok := kit.OpenItemAdjustment{OpenItemID: uuid.NewString(), Kind: kit.AdjCashApplication, Amount: amt("1"), SourceRefType: "CASH_APPLICATION", SourceRefID: "app-1", EventKey: "k"}
	require.NoError(t, ValidateAdjustment(ok))
	for name, f := range map[string]func(*kit.OpenItemAdjustment){
		"no owning type": func(a *kit.OpenItemAdjustment) { a.SourceRefType = "" },
		"free text type": func(a *kit.OpenItemAdjustment) { a.SourceRefType = "manual fix" },
		"no owning id":   func(a *kit.OpenItemAdjustment) { a.SourceRefID = " " },
		"no event key":   func(a *kit.OpenItemAdjustment) { a.EventKey = "" },
		"negative":       func(a *kit.OpenItemAdjustment) { a.Amount = amt("-1") },
		"bad item":       func(a *kit.OpenItemAdjustment) { a.OpenItemID = "x" },
	} {
		a := ok
		f(&a)
		require.Error(t, ValidateAdjustment(a), name)
	}
}

// ---- handler tests decided before any transaction opens ----

func newUnit() *Module {
	return New(testDeps(&fakeAuthz{grants: standardGrants()}, func() time.Time { return testNow }))
}

func TestHandler_Unauthorized(t *testing.T) {
	m := newUnit()
	ent, tenant := uuid.NewString(), uuid.NewString()
	item := map[string]any{"legal_entity_id": ent, "open_item_id": uuid.NewString()}
	for name, h := range map[string]http.HandlerFunc{"adjust": m.applyReceivableAdjustment, "create": m.createOpenItemFromDocument, "dispute": m.openDisputeCmd,
		"hold": m.placeCollectionHold, "close": m.closeOpenItem} {
		w := httpRec()
		body := item
		if name == "create" {
			body = map[string]any{"legal_entity_id": ent}
		}
		h(w, newReq(http.MethodPost, base+"commands/x", tenant, pNobody, body))
		require.Equal(t, http.StatusForbidden, w.Code, name)
	}
}

// Neg #20 (decidable part): no field sets a balance; the decoder rejects it.
func TestNeg20_NoBalanceFieldAccepted(t *testing.T) {
	m := newUnit()
	ent, tenant := uuid.NewString(), uuid.NewString()
	for _, field := range []string{"residual_amount", "balance", "residual", "new_balance"} {
		w := httpRec()
		m.applyReceivableAdjustment(w, newReq(http.MethodPost, base+"commands/applyReceivableAdjustment", tenant, pAdjuster,
			map[string]any{"legal_entity_id": ent, "open_item_id": uuid.NewString(), "kind": kit.AdjCorrection, "amount": "1.00", field: "0.00"}))
		require.Equal(t, http.StatusBadRequest, w.Code, field)
		require.Equal(t, kit.CodeValidationFailed, errCode(t, w.Body.Bytes()))
	}
}

func TestHandler_AdjustmentPreconditions(t *testing.T) {
	m := newUnit()
	ent, tenant, item := uuid.NewString(), uuid.NewString(), uuid.NewString()
	body := func(mut func(map[string]any)) map[string]any {
		b := map[string]any{"legal_entity_id": ent, "open_item_id": item, "kind": kit.AdjCorrection, "amount": "5.00", "expected_version": 1,
			"reason_code": "BILLING_ERROR", "evidence_id": "EV-1", "owning_document_type": "CORRECTION_CASE", "owning_document_id": "CC-1"}
		mut(b)
		return b
	}
	cases := []struct {
		name   string
		mut    func(map[string]any)
		status int
		code   string
	}{
		{"cash kind over HTTP", func(b map[string]any) { b["kind"] = kit.AdjCashApplication }, 422, kit.CodeValidationFailed},
		{"write-off over HTTP", func(b map[string]any) { b["kind"] = kit.AdjWriteOff }, 422, kit.CodeValidationFailed},
		{"no expected version", func(b map[string]any) { delete(b, "expected_version") }, 400, kit.CodeExpectedVersionMissing},
		{"no reason", func(b map[string]any) { delete(b, "reason_code") }, 400, kit.CodeReasonRequired},
		{"no evidence", func(b map[string]any) { delete(b, "evidence_id") }, 400, kit.CodeEvidenceRequired},
		{"no owning document", func(b map[string]any) { delete(b, "owning_document_id") }, 400, kit.CodeValidationFailed},
	}
	for _, c := range cases {
		w := httpRec()
		m.applyReceivableAdjustment(w, newReq(http.MethodPost, base+"commands/applyReceivableAdjustment", tenant, pAdjuster, body(c.mut)))
		require.Equal(t, c.status, w.Code, c.name+w.Body.String())
		require.Equal(t, c.code, errCode(t, w.Body.Bytes()), c.name)
	}
}

func TestHandler_ItemCommandPreconditions(t *testing.T) {
	m := newUnit()
	ent, tenant, item := uuid.NewString(), uuid.NewString(), uuid.NewString()
	w := httpRec()
	m.openDisputeCmd(w, newReq(http.MethodPost, "/", tenant, pDispute, map[string]any{"legal_entity_id": ent, "open_item_id": item, "reason_code": "R", "evidence_id": "E"}))
	require.Equal(t, kit.CodeExpectedVersionMissing, errCode(t, w.Body.Bytes()))
	w = httpRec()
	m.openDisputeCmd(w, newReq(http.MethodPost, "/", tenant, pDispute, map[string]any{"legal_entity_id": ent, "open_item_id": item, "expected_version": 1, "evidence_id": "E"}))
	require.Equal(t, kit.CodeReasonRequired, errCode(t, w.Body.Bytes()))
	w = httpRec()
	m.openDisputeCmd(w, newReq(http.MethodPost, "/", tenant, pDispute, map[string]any{"legal_entity_id": ent, "open_item_id": item, "expected_version": 1, "reason_code": "R"}))
	require.Equal(t, kit.CodeEvidenceRequired, errCode(t, w.Body.Bytes()))
	w = httpRec()
	m.resolveDisputeCmd(w, newReq(http.MethodPost, "/", tenant, pDispute, map[string]any{"legal_entity_id": ent, "open_item_id": item, "expected_version": 1, "evidence_id": "E", "resolution_code": "MAYBE"}))
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandler_QueryValidation(t *testing.T) {
	m := newUnit()
	tenant := uuid.NewString()
	h := testRouter(m)
	code, body := call(t, h, http.MethodGet, base+"getAging?legal_entity_id="+uuid.NewString(), tenant, "u", "", nil)
	require.Equal(t, http.StatusBadRequest, code, string(body)) // as_of is mandatory: aging is never "as of whenever"
	code, _ = call(t, h, http.MethodGet, base+"getBalanceAsOf?open_item_id="+uuid.NewString(), tenant, "u", "", nil)
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = call(t, h, http.MethodGet, base+"getOpenItem", tenant, "u", "", nil)
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = call(t, h, http.MethodGet, base+"getOpenItem?open_item_id=x", "", "u", "", nil)
	require.Equal(t, http.StatusUnauthorized, code)
}
