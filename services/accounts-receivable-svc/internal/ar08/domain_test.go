package ar08

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

func d(s string) time.Time { t, _ := time.Parse("2006-01-02", s); return t }

func rcpt(amount string) *Receipt {
	a := kit.MustAmount(amount)
	return &Receipt{ReceiptID: uuid.NewString(), Currency: "USD", Amount: a, UnappliedAmount: a, State: RcptUnapplied, RegisteredBy: "reg"}
}

func view(amount, due string) kit.OpenItemView {
	return kit.OpenItemView{OpenItemID: uuid.NewString(), SourceDocumentID: uuid.NewString(), CustomerPartyID: "cust", Currency: "USD",
		Residual: kit.MustAmount(amount), OriginalAmount: kit.MustAmount(amount), DueDate: d(due)}
}

func TestScoring_ReferenceAmountAndOrdering(t *testing.T) {
	a, b, c := view("100.00", "2026-10-01"), view("500.00", "2026-10-05"), view("500.00", "2026-10-02")
	r := rcpt("500.00")
	r.RemittanceText = "paying " + b.SourceDocumentID
	got := ScoreCandidates(r, []kit.OpenItemView{a, b, c}, BasisCustomer)
	require.Len(t, got, 3)
	require.Equal(t, b.OpenItemID, got[0].OpenItemID, "reference + amount + customer wins")
	require.GreaterOrEqual(t, got[0].Score, AutoApplyMinScore)
	require.Contains(t, got[0].Basis, BasisReference)
	require.Equal(t, c.OpenItemID, got[1].OpenItemID, "equal amount beats a smaller item")
	require.Equal(t, 1, got[0].Rank)
	// deterministic: shuffled input, same output
	again := ScoreCandidates(r, []kit.OpenItemView{c, a, b}, BasisCustomer)
	require.Equal(t, got, again)
}

func TestScoring_DisputedAndHeldAreNeverAutoEligible(t *testing.T) {
	a := view("500.00", "2026-10-01")
	a.Disputed = true
	r := rcpt("500.00")
	r.RemittanceText = a.SourceDocumentID
	got := ScoreCandidates(r, []kit.OpenItemView{a}, BasisCustomer)
	require.True(t, got[0].Disputed)
	require.GreaterOrEqual(t, got[0].Score, AutoApplyMinScore)
	require.False(t, got[0].Eligible())
	require.Empty(t, AllocateAuto(r.Amount, got))
}

func TestAllocateAuto_CapsAtResidualAndKeepsRemainderUnapplied(t *testing.T) {
	a, b := view("300.00", "2026-10-01"), view("900.00", "2026-10-02")
	r := rcpt("500.00")
	r.RemittanceText = a.SourceDocumentID + " " + b.SourceDocumentID
	got := ScoreCandidates(r, []kit.OpenItemView{a, b}, BasisCustomer)
	lines := AllocateAuto(r.Amount, got)
	var sum kit.Amount
	for _, l := range lines {
		sum = sum.Add(l.Amount)
	}
	require.Equal(t, r.Amount, sum)
	require.Len(t, lines, 2)
	require.Equal(t, 1, lines[0].LineNo)
	require.LessOrEqual(t, lines[0].OpenItemID, lines[1].OpenItemID, "lines are in lock order")
}

func TestValidateLines_ExactReconciliation(t *testing.T) {
	r := rcpt("100.00")
	l := []Line{{OpenItemID: uuid.NewString(), Amount: kit.MustAmount("60.00")}}
	s, err := ValidateLines(r, l, 0, DiffNone, nil)
	require.NoError(t, err)
	require.Equal(t, Split{Applied: kit.MustAmount("60"), Unapplied: kit.MustAmount("40")}, s)
	require.Equal(t, RcptPartial, ReceiptState(s))

	s, err = ValidateLines(r, l, kit.MustAmount("10"), DiffBankFee, nil)
	require.NoError(t, err)
	require.Equal(t, r.Amount, s.Applied+s.Unapplied+s.Difference)

	for name, tc := range map[string]struct {
		lines []Line
		diff  kit.Amount
		kind  string
		decl  *kit.Amount
		code  string
	}{
		"over the receipt":     {[]Line{{OpenItemID: uuid.NewString(), Amount: kit.MustAmount("100.01")}}, 0, "", nil, kit.CodeApplicationOutOfBalance},
		"declared mismatch":    {l, 0, "", amt("39.00"), kit.CodeApplicationOutOfBalance},
		"difference too large": {l, kit.MustAmount("60"), DiffBankFee, nil, kit.CodeApplicationOutOfBalance},
		"kind without amount":  {l, 0, DiffFX, nil, kit.CodeValidationFailed},
		"amount without kind":  {l, kit.MustAmount("1"), "", nil, kit.CodeValidationFailed},
		"no lines":             {nil, 0, "", nil, kit.CodeValidationFailed},
		"zero line":            {[]Line{{OpenItemID: uuid.NewString(), Amount: 0}}, 0, "", nil, kit.CodeValidationFailed},
		"too many decimals":    {[]Line{{OpenItemID: uuid.NewString(), Amount: kit.MustAmount("1.0001")}}, 0, "", nil, kit.CodeValidationFailed},
	} {
		_, err := ValidateLines(r, tc.lines, tc.diff, tc.kind, tc.decl)
		var p *kit.Problem
		require.ErrorAs(t, err, &p, name)
		require.Equal(t, tc.code, p.Code, name)
	}
	dup := uuid.NewString()
	_, err = ValidateLines(r, []Line{{OpenItemID: dup, Amount: 1}, {OpenItemID: dup, Amount: 1}}, 0, "", nil)
	require.Error(t, err)
}

func amt(s string) *kit.Amount { a := kit.MustAmount(s); return &a }

func TestApprovalRequirement(t *testing.T) {
	a := view("500.00", "2026-10-01")
	r := rcpt("500.00")
	r.RemittanceText = a.SourceDocumentID
	cands := ScoreCandidates(r, []kit.OpenItemView{a}, BasisCustomer)
	lines := AllocateAuto(r.Amount, cands)
	require.Empty(t, ApprovalRequirement(r, ModeAuto, r.Amount, 0, cands, lines))
	require.Equal(t, "MANUAL_MATCH", ApprovalRequirement(r, ModeManual, r.Amount, 0, cands, lines))
	require.Equal(t, "AUTHORIZED_DIFFERENCE", ApprovalRequirement(r, ModeAuto, r.Amount, 1, cands, lines))
	require.Equal(t, "HIGH_VALUE", ApprovalRequirement(r, ModeAuto, AutoApplyHighValueLimit+1, 0, cands, lines))
	r.RelatedParty = true
	require.Equal(t, "RELATED_PARTY", ApprovalRequirement(r, ModeAuto, r.Amount, 0, cands, lines))
	r.RelatedParty = false
	require.Equal(t, "LOW_CONFIDENCE_OR_DISPUTED", ApprovalRequirement(r, ModeAuto, r.Amount, 0, nil, lines))
}

func TestBuildPosting_BalancedBothDirections(t *testing.T) {
	a := &Application{ReceiptAmount: kit.MustAmount("100"), AppliedAmount: kit.MustAmount("70"), UnappliedAmount: kit.MustAmount("25"), DifferenceAmount: kit.MustAmount("5"), DifferenceKind: DiffFX}
	for _, rev := range []bool{false, true} {
		ev := kit.PostingEvent{Lines: BuildPosting(a, rev)}
		require.NoError(t, ev.Validate())
		keys := map[string]bool{}
		for _, l := range ev.Lines {
			keys[l.MappingKey] = true
		}
		require.True(t, keys[KeyCashReceipt] && keys[KeyReceivable] && keys[KeyUnappliedCash] && keys[KeyFXDifference])
	}
	fwd := BuildPosting(a, false)
	require.Equal(t, kit.MustAmount("100"), fwd[0].Debit, "forward debits the cash receipt")
	require.Equal(t, kit.MustAmount("100"), BuildPosting(a, true)[0].Credit, "reverse credits it")
	a.DifferenceKind = DiffBankFee
	require.Equal(t, KeyBankFee, BuildPosting(a, false)[3].MappingKey)
}

func TestBatchStateOf_And_RequestHash(t *testing.T) {
	require.Equal(t, BatchUnapplied, BatchStateOf(2, 0, 0, false))
	require.Equal(t, BatchProposed, BatchStateOf(2, 0, 0, true))
	require.Equal(t, BatchPartial, BatchStateOf(2, 1, 1, false))
	require.Equal(t, BatchFull, BatchStateOf(2, 2, 2, false))
	require.Equal(t, BatchPartial, BatchStateOf(2, 2, 1, false))
	x, y := uuid.NewString(), uuid.NewString()
	h1 := RequestHash("b", "r", ModeManual, []Line{{OpenItemID: x, Amount: 1}, {OpenItemID: y, Amount: 2}}, 0, "")
	h2 := RequestHash("b", "r", ModeManual, []Line{{OpenItemID: y, Amount: 2}, {OpenItemID: x, Amount: 1}}, 0, "")
	require.Equal(t, h1, h2)
	require.NotEqual(t, h1, RequestHash("b", "r", ModeAuto, []Line{{OpenItemID: x, Amount: 1}, {OpenItemID: y, Amount: 2}}, 0, ""))
}

func TestInferCustomer(t *testing.T) {
	id := uuid.NewString()
	require.Equal(t, id, InferCustomer("payer", "ref CUST-"+id+" thanks"))
	require.Empty(t, InferCustomer("no ids here", "12345"))
}

func TestVerifyConfirmed(t *testing.T) {
	tid, tenant := uuid.NewString(), uuid.NewString()
	ok := func() *BankTransaction {
		return &BankTransaction{TransactionID: tid, TenantID: tenant, Status: BankNormalized, Amount: kit.MustAmount("10.00"), Currency: "USD", TransactionDate: d("2026-10-01")}
	}
	require.NoError(t, VerifyConfirmed(ok(), tid, tenant))
	for name, mut := range map[string]func(*BankTransaction){
		"quarantined": func(x *BankTransaction) { x.Status = BankQuarantined },
		"superseded":  func(x *BankTransaction) { x.Status = BankSuperseded },
		"outflow":     func(x *BankTransaction) { x.Amount = kit.MustAmount("-1") },
		"zero":        func(x *BankTransaction) { x.Amount = 0 },
		"bad ccy":     func(x *BankTransaction) { x.Currency = "us" },
		"wrong id":    func(x *BankTransaction) { x.TransactionID = uuid.NewString() },
		"wrong tnt":   func(x *BankTransaction) { x.TenantID = uuid.NewString() },
		"jpy decimal": func(x *BankTransaction) { x.Currency = "JPY"; x.Amount = kit.MustAmount("10.50") },
		"no date":     func(x *BankTransaction) { x.TransactionDate = time.Time{} },
	} {
		x := ok()
		mut(x)
		var p *kit.Problem
		require.ErrorAs(t, VerifyConfirmed(x, tid, tenant), &p, name)
		require.Equal(t, kit.CodeSettlementNotConfirmed, p.Code, name)
	}
}

func TestHTTPReceiptSource_MapsBankingAnswers(t *testing.T) {
	tid := uuid.NewString()
	var status int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/banking/canonical-transactions/"+tid, r.URL.Path)
		require.Equal(t, "tenant-1", r.Header.Get("X-Tenant-Id"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	src := NewHTTPReceiptSource(srv.URL)
	c := kit.Caller{TenantID: "tenant-1", PrincipalID: "p"}
	status, body = 200, `{"transaction_id":"`+tid+`","tenant_id":"tenant-1","statement_line_id":"l1","transaction_date":"2026-10-10T00:00:00Z","amount":1234.5,"currency":"usd","counterparty":"ACME","status":"NORMALIZED"}`
	got, err := src.Get(context.Background(), c, tid)
	require.NoError(t, err)
	require.Equal(t, kit.MustAmount("1234.50"), got.Amount, "the decimal text is parsed exactly, never through arithmetic")
	require.Equal(t, "USD", got.Currency)
	status = 404
	_, err = src.Get(context.Background(), c, tid)
	require.ErrorIs(t, err, ErrBankTxnNotFound)
	status = 500
	_, err = src.Get(context.Background(), c, tid)
	require.ErrorIs(t, err, ErrSourceUnavailable)
	status, body = 200, `not json`
	_, err = src.Get(context.Background(), c, tid)
	require.ErrorIs(t, err, ErrSourceUnavailable)
	status, body = 200, `{"transaction_id":"`+tid+`","amount":1e+30,"currency":"USD","status":"NORMALIZED"}`
	_, err = src.Get(context.Background(), c, tid)
	require.ErrorIs(t, err, ErrSourceUnavailable, "an unrepresentable amount is ambiguous: fail closed")
	srv.Close()
	_, err = src.Get(context.Background(), c, tid)
	require.ErrorIs(t, err, ErrSourceUnavailable)
	_ = svcclient.ErrUnavailable
}
