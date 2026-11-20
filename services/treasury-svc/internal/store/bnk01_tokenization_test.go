//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

// TestPgStore_CreateBankAccount_Tokenized_StoresTokenRowNotRawNumber proves
// the real store-level contract behind TOKENIZATION_ENFORCED: when the
// handler hands CreateBankAccount an account with AccountNumberToken set, the
// store writes bank_account_identifier_tokens in the SAME transaction as the
// account row, bank_accounts.masked_account_number stays the safe masked
// form only, and the live read reports Tokenized()=true with the token value
// never equal to the masked display string.
func TestPgStore_CreateBankAccount_Tokenized_StoresTokenRowNotRawNumber(t *testing.T) {
	cleanTables(t)
	s := testStore

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	acct := newTestAccount(tenantID, uuid.New().String())
	acct.MaskedAccountNumber = "****6789" // computed server-side by the handler, as it would be
	acct.AccountNumberToken = "faketok_realvaultwouldneverlooklikethis"

	created, err := s.CreateBankAccount(ctx, acct)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if !created {
		t.Fatalf("expected a new account to be created")
	}
	if !acct.Tokenized() {
		t.Fatalf("expected the returned account to report Tokenized()=true")
	}

	// The raw token never reaches bank_accounts.masked_account_number — only
	// the safe masked display form does.
	var storedMasked string
	if err := testPool.QueryRow(ctx, `SELECT masked_account_number FROM bank_accounts WHERE bank_account_id = $1`,
		acct.BankAccountID).Scan(&storedMasked); err != nil {
		t.Fatalf("read bank_accounts: %v", err)
	}
	if storedMasked != "****6789" {
		t.Fatalf("expected masked_account_number to stay the masked display form, got %q", storedMasked)
	}

	// The real token lives ONLY in the side table, as its own current (not
	// superseded) row, in the same transaction as account creation.
	var token, source string
	var supersededBy *string
	if err := testPool.QueryRow(ctx, `
		SELECT account_number_token, source, superseded_by
		FROM bank_account_identifier_tokens WHERE bank_account_id = $1`, acct.BankAccountID).
		Scan(&token, &source, &supersededBy); err != nil {
		t.Fatalf("read bank_account_identifier_tokens: %v", err)
	}
	if token != acct.AccountNumberToken || source != "WRITE" || supersededBy != nil {
		t.Fatalf("unexpected token row: token=%q source=%q superseded_by=%v", token, source, supersededBy)
	}

	// A fresh read reports the same thing — the current token survives a
	// round trip through GetBankAccount, not just the create response.
	reread, err := s.GetBankAccount(ctx, acct.BankAccountID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if !reread.Tokenized() || reread.AccountNumberToken != acct.AccountNumberToken {
		t.Fatalf("expected GetBankAccount to also report the token, got Tokenized()=%v token=%q", reread.Tokenized(), reread.AccountNumberToken)
	}
}

// TestPgStore_CreateBankAccount_NotTokenized_PreservesOldBehaviorExactly is
// the TOKENIZATION_ENFORCED=false regression guard: an account created with
// no AccountNumberToken (every existing caller, today) gets no token row at
// all and Tokenized() reports false — the exact pre-existing behavior.
func TestPgStore_CreateBankAccount_NotTokenized_PreservesOldBehaviorExactly(t *testing.T) {
	cleanTables(t)
	s := testStore

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	acct := newTestAccount(tenantID, uuid.New().String()) // AccountNumberToken left empty, as every pre-existing caller does

	if _, err := s.CreateBankAccount(ctx, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if acct.Tokenized() {
		t.Fatalf("expected a non-tokenized account, got Tokenized()=true")
	}
	var count int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM bank_account_identifier_tokens WHERE bank_account_id = $1`,
		acct.BankAccountID).Scan(&count); err != nil {
		t.Fatalf("count token rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected zero token rows for a non-tokenized account, got %d", count)
	}
	if acct.MaskedAccountNumber != "****1234" {
		t.Fatalf("expected the caller-supplied masked value unchanged, got %q", acct.MaskedAccountNumber)
	}
}

// TestPgStore_RotateAccountIdentifierToken_Tokenized_SupersedesThePriorToken
// proves rotation's append-and-supersede contract: a second token row is
// written, the PREVIOUS token row is marked superseded (never deleted, for
// audit), and the live account reports only the NEW token as current.
func TestPgStore_RotateAccountIdentifierToken_Tokenized_SupersedesThePriorToken(t *testing.T) {
	cleanTables(t)
	s := testStore

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	acct := newTestAccount(tenantID, uuid.New().String())
	acct.MaskedAccountNumber = "****0001"
	acct.AccountNumberToken = "faketok_original"
	if _, err := s.CreateBankAccount(ctx, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}

	rotated, err := s.RotateAccountIdentifierToken(ctx, domain.RotateAccountTokenParams{
		BankAccountID: acct.BankAccountID, TenantID: tenantID,
		NewMaskedAccountNumber: "****9999", NewBankIdentifier: acct.BankIdentifier,
		ActorPrincipalID: "ops-1", CorrelationID: "corr-rotate-1",
		NewAccountNumberToken: "faketok_rotated",
	})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.AccountNumberToken != "faketok_rotated" || rotated.MaskedAccountNumber != "****9999" {
		t.Fatalf("expected the rotated account to carry the NEW token/masked value, got token=%q masked=%q",
			rotated.AccountNumberToken, rotated.MaskedAccountNumber)
	}

	rows, err := testPool.Query(ctx, `
		SELECT account_number_token, superseded_by IS NOT NULL
		FROM bank_account_identifier_tokens WHERE bank_account_id = $1 ORDER BY created_at`, acct.BankAccountID)
	if err != nil {
		t.Fatalf("query tokens: %v", err)
	}
	defer rows.Close()
	type row struct {
		token       string
		superseded  bool
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.token, &r.superseded); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 token rows (original + rotated), got %d: %+v", len(got), got)
	}
	if got[0].token != "faketok_original" || !got[0].superseded {
		t.Fatalf("expected the original token to be superseded, got %+v", got[0])
	}
	if got[1].token != "faketok_rotated" || got[1].superseded {
		t.Fatalf("expected the rotated token to be the current one, got %+v", got[1])
	}
}
