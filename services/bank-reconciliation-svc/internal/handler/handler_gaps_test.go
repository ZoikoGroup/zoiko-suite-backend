package handler_test

// Regression tests for the defects closed in the 13 Aug 2026 gap-closing
// pass. Each one fails against the code as it was.
//
// They live in their own file only to keep the original suite readable; they
// share its stubs and helpers.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	"zoiko.io/bank-reconciliation-svc/internal/ledger"
)

// ── tenant scope ─────────────────────────────────────────────────────────────

// The register was scoped by ?tenant_id, so naming another tenant returned
// their entire bank statement history — every amount, bank reference and
// reconciliation state. The store's explicit tenant filter, which its package
// doc is careful to explain, was filtering by a value the caller chose.
func TestListStatementLines_IgnoresTenantQueryParamAndUsesVerifiedScope(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	rec := doRequestAs(r, http.MethodGet, "/v1/statement-lines/?tenant_id=someone-else", nil, "principal-1", "t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lastListFilter.TenantID != "t1" {
		t.Fatalf("register was scoped to %q; the caller's verified tenant is %q — a query parameter must never widen the scope",
			s.lastListFilter.TenantID, "t1")
	}
}

// The row's tenant_id column was written from the request BODY while only the
// RLS scope came from the verified header. This pool connects as a superuser,
// which bypasses RLS unconditionally, so the mismatch was not caught anywhere:
// a caller could land a statement line in another tenant's register.
func TestCreateStatementLine_BodyTenantOtherThanVerifiedScope_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	req := validCreateReq()
	req.TenantID = "some-other-tenant"
	rec := doRequestAs(r, http.MethodPost, "/v1/statement-lines/", req, "principal-1", "t1")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the body names a tenant other than the verified scope, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.lines) != 0 {
		t.Fatal("a refused cross-tenant write still reached the store")
	}
}

func TestCreateStatementLine_WritesVerifiedTenantNotBodyTenant(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	req := validCreateReq()
	req.TenantID = "" // caller omits it entirely; the header decides
	rec := doRequestAs(r, http.MethodPost, "/v1/statement-lines/", req, "principal-1", "t9")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	for _, l := range s.lines {
		if l.TenantID != "t9" {
			t.Fatalf("row stored under tenant %q, want the verified scope %q", l.TenantID, "t9")
		}
	}
}

func TestGetStatementLine_NoVerifiedTenantScope_Returns401NotNotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequestAs(r, http.MethodGet, "/v1/statement-lines/l1", nil, "principal-1", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no verified tenant scope, got %d: %s — a caller who was never scoped was being told the row does not exist", rec.Code, rec.Body.String())
	}
}

// ── direction: the defect this service exists to not have ────────────────────

// A bank statement line of -1000.00 is money LEAVING the account. A journal
// that debits the cash account by 1000.00 records money ARRIVING. These are
// opposites and must never reconcile — but the old check compared
// abs(journal net) with abs(statement amount), so they did.
func TestMatchStatementLine_OppositeDirection_Refused(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{
		StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1",
		Amount: -1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct(),
	}
	// finalizedJournal(+1000) debits cash: money in.
	l := &stubLedger{journal: finalizedJournal("e1", 1000)}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, l)

	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "j1"}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a 1000.00 payment OUT was reconciled against a journal recording 1000.00 IN (got %d: %s)", rec.Code, rec.Body.String())
	}
	if s.lines["l1"].Status != domain.StatementLineStatusUnmatched {
		t.Fatalf("line was transitioned to %s despite the direction mismatch", s.lines["l1"].Status)
	}
}

func TestMatchStatementLine_SameDirection_Succeeds(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{
		StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1",
		Amount: -1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct(),
	}
	// finalizedJournal(-1000) credits cash: money out, matching the line.
	l := &stubLedger{journal: finalizedJournal("e1", -1000)}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, l)

	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "j1"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for a correctly-directed match, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A journal can be real, FINALIZED, for the right entity and the right total,
// and still have nothing to do with this bank account.
func TestMatchStatementLine_JournalNeverTouchesCashAccount_Refused(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{
		StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1",
		Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct(),
	}
	l := &stubLedger{journal: &ledger.Journal{
		JournalID: "j1", LegalEntityID: "e1", Status: "FINALIZED",
		Lines: []ledger.JournalLine{
			{AccountCode: "5000", DebitAmount: 1000},
			{AccountCode: "4000", CreditAmount: 1000},
		},
	}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, l)

	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "j1"}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a journal that posts nothing to this bank account, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A line ingested before migration 000003 has no cash account code, so
// direction cannot be verified. It is refused rather than falling back to the
// magnitude comparison — verifying something weaker and calling it a match is
// the defect, not the absence of the column.
func TestMatchStatementLine_NoCashAccountCode_RefusedNotWeakened(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{
		StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1",
		Amount: 1000, Status: domain.StatementLineStatusUnmatched, // no GLCashAccountCode
	}
	l := &stubLedger{journal: finalizedJournal("e1", 1000)}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, l)

	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "j1"}, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 cash_account_unknown, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lines["l1"].Status != domain.StatementLineStatusUnmatched {
		t.Fatal("line was matched despite the direction being unverifiable")
	}
}

// Amounts are NUMERIC(18,2) at both ends and only float64 in transit. Summed
// across lines they must still land on the exact cent.
func TestMatchStatementLine_MultiLineCashMovementIsExactCents(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{
		StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1",
		Amount: 100.30, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct(),
	}
	// 60.10 + 40.20 == 100.30 is false in binary floating point.
	l := &stubLedger{journal: &ledger.Journal{
		JournalID: "j1", LegalEntityID: "e1", Status: "FINALIZED",
		Lines: []ledger.JournalLine{
			{AccountCode: "1000", DebitAmount: 60.10},
			{AccountCode: "1000", DebitAmount: 40.20},
			{AccountCode: "4000", CreditAmount: 100.30},
		},
	}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, l)

	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "j1"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── CompleteStatement ────────────────────────────────────────────────────────

// The authz check passed for the legal entity the CALLER named, and nothing
// tied that entity to the bank account being completed. So a principal holding
// the action over any one entity could complete — and publish
// reconciliation.completed for — a statement belonging to another.
func TestCompleteStatement_BankAccountBelongsToAnotherEntity_Returns403(t *testing.T) {
	s := newStubStore()
	s.countUnmatched = 0
	s.legalEntities = []string{"e2"} // the lines really belong to e2

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubLedger{})

	// ...but the caller authorizes against e1, which it does hold rights over.
	rec := doRequest(r, http.MethodPost, "/v1/bank-accounts/b1/statements/2026-07-01/complete?legal_entity_id=e1", nil, "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the authorized entity is not the one the statement belongs to, got %d: %s", rec.Code, rec.Body.String())
	}
	if pub.completed != 0 {
		t.Fatal("reconciliation.completed was published for another entity's bank account")
	}
}

// Zero unmatched lines and zero lines at all are not the same thing. The
// second announced that a statement nobody ingested had been reconciled.
func TestCompleteStatement_NoLinesAtAll_Returns404AndPublishesNothing(t *testing.T) {
	s := newStubStore()
	s.countUnmatched = 0
	s.legalEntities = []string{}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubLedger{})

	rec := doRequest(r, http.MethodPost, "/v1/bank-accounts/b1/statements/2026-07-01/complete?legal_entity_id=e1", nil, "principal-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a bank account and date with no statement lines, got %d: %s", rec.Code, rec.Body.String())
	}
	if pub.completed != 0 {
		t.Fatal("reconciliation.completed was published for a statement that does not exist")
	}
}

// ── input handling and error mapping ─────────────────────────────────────────

// A malformed uuid reached the caller as 503 store_unavailable: a database
// outage reported for a caller's typo, which also misleads anything watching
// this service's error rate.
func TestGetStatementLine_MalformedIdentifier_Returns400NotOutage(t *testing.T) {
	s := newStubStore()
	s.getErr = domain.ErrInvalidIdentifier
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	rec := doRequest(r, http.MethodGet, "/v1/statement-lines/not-a-uuid", nil, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed identifier, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateStatementLine_UnknownField_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	body := `{"legal_entity_id":"e1","bank_account_id":"b1","statement_date":"2026-07-01T00:00:00Z",` +
		`"amount":100,"currency_code":"USD","bank_reference":"ACH-1","gl_cash_account_code":"1000",` +
		`"correlation_id":"c1","tenant_idd":"typo"}`
	rec := doRawRequest(r, http.MethodPost, "/v1/statement-lines/", body, "principal-1", testTenant)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unrecognised field, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateStatementLine_MissingCashAccountCode_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	req := validCreateReq()
	req.GLCashAccountCode = ""
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without gl_cash_account_code, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gl_cash_account_code") {
		t.Fatalf("error should name the missing field, got %s", rec.Body.String())
	}
}

// currency_code is VARCHAR(3); a longer value used to reach Postgres and come
// back as SQLSTATE 22001, reported to the caller as an outage.
func TestCreateStatementLine_OverlongCurrencyCode_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	req := validCreateReq()
	req.CurrencyCode = "DOLLARS"
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-ISO-4217 currency code, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFlagException_OverlongReason_Returns400(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{
		StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1",
		Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct(),
	}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/exception",
		domain.FlagExceptionRequest{Reason: strings.Repeat("x", 501)}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a reason longer than the column allows, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── paging ───────────────────────────────────────────────────────────────────

// The register returned every line ever ingested. A bank account accrues one
// row per transaction, so that is an entire statement history in one response.
func TestListStatementLines_LimitIsPassedThrough(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})

	rec := doRequest(r, http.MethodGet, "/v1/statement-lines/?limit=25", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lastListFilter.Limit != 25 {
		t.Fatalf("limit reached the store as %d, want 25", s.lastListFilter.Limit)
	}
}

// Refused rather than silently defaulted: a caller asking for 5000 and
// receiving 200 rows would read the short page as the whole register.
func TestListStatementLines_InvalidLimit_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	for _, bad := range []string{"abc", "0", "-5", "100000"} {
		rec := doRequest(r, http.MethodGet, "/v1/statement-lines/?limit="+bad, nil, "principal-1")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("limit=%s: expected 400, got %d: %s", bad, rec.Code, rec.Body.String())
		}
	}
}

// An empty register encoded as JSON null, which every consumer then has to
// special-case against an empty array.
func TestListStatementLines_EmptyRegisterIsArrayNotNull(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodGet, "/v1/statement-lines/", nil, "principal-1")

	body := strings.TrimSpace(rec.Body.String())
	if body == "null" {
		t.Fatal("empty register encoded as JSON null, want []")
	}
	var lines []domain.StatementLine
	if err := json.Unmarshal([]byte(body), &lines); err != nil {
		t.Fatalf("register did not decode as an array: %v (%s)", err, body)
	}
}
