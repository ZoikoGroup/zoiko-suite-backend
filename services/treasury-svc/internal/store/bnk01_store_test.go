//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

// TestPgStore_VerifyBankAccountOwnership_SupersedesPriorEvidence is the
// real proof of "ownership verification state is orthogonal and
// versioned": a second verification call supersedes the first rather than
// creating an unrelated, ambiguous second record, and IsOwnershipVerified
// still reports true throughout (never flips to false between the two
// calls).
func TestPgStore_VerifyBankAccountOwnership_SupersedesPriorEvidence(t *testing.T) {
	cleanTables(t)
	s := testStore

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	acct := newTestAccount(tenantID, uuid.New().String())
	if _, err := s.CreateBankAccount(ctx, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}

	verified, err := s.IsOwnershipVerified(ctx, tenantID, acct.BankAccountID)
	if err != nil {
		t.Fatalf("IsOwnershipVerified (before): %v", err)
	}
	if verified {
		t.Fatal("expected a freshly created account to be unverified before any evidence is recorded")
	}

	first, err := s.VerifyBankAccountOwnership(ctx, domain.VerifyOwnershipParams{
		BankAccountID: acct.BankAccountID, TenantID: tenantID, VerificationMethod: "MICRO_DEPOSIT",
		EvidenceRef: "evidence-1", VerifiedByPrincipalID: "auditor-1",
	})
	if err != nil {
		t.Fatalf("first verify: %v", err)
	}

	verified, err = s.IsOwnershipVerified(ctx, tenantID, acct.BankAccountID)
	if err != nil || !verified {
		t.Fatalf("expected verified=true after first evidence, got verified=%v err=%v", verified, err)
	}

	second, err := s.VerifyBankAccountOwnership(ctx, domain.VerifyOwnershipParams{
		BankAccountID: acct.BankAccountID, TenantID: tenantID, VerificationMethod: "DOCUMENT_UPLOAD",
		EvidenceRef: "evidence-2", VerifiedByPrincipalID: "auditor-2",
	})
	if err != nil {
		t.Fatalf("second verify: %v", err)
	}

	verified, err = s.IsOwnershipVerified(ctx, tenantID, acct.BankAccountID)
	if err != nil || !verified {
		t.Fatalf("expected verified=true after re-verification, got verified=%v err=%v", verified, err)
	}

	history, err := s.ListOwnershipEvidence(ctx, tenantID, acct.BankAccountID)
	if err != nil {
		t.Fatalf("list evidence: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 evidence rows (append-only), got %d", len(history))
	}
	for _, e := range history {
		if e.EvidenceID == first.EvidenceID {
			if e.SupersededBy == nil || *e.SupersededBy != second.EvidenceID {
				t.Fatalf("expected the first evidence row to be superseded by the second, got %+v", e)
			}
		}
		if e.EvidenceID == second.EvidenceID && e.SupersededBy != nil {
			t.Fatalf("expected the second (latest) evidence row to remain unsuperseded, got %+v", e)
		}
	}
}

// TestPgStore_BankAccountLifecycle_CAS_Transitions is the real proof of
// the operational-status state machine's CAS enforcement: illegal
// transitions (suspend a non-ACTIVE account, reactivate a non-SUSPENDED
// one) are rejected, not silently applied.
func TestPgStore_BankAccountLifecycle_CAS_Transitions(t *testing.T) {
	cleanTables(t)
	s := testStore

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	acct := newTestAccount(tenantID, uuid.New().String())
	if _, err := s.CreateBankAccount(ctx, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}

	// Reactivating an ACTIVE (not SUSPENDED) account must be rejected.
	if _, err := s.ReactivateBankAccount(ctx, domain.ReactivateAccountParams{BankAccountID: acct.BankAccountID, TenantID: tenantID, ActorPrincipalID: "ops-1"}); err != domain.ErrInvalidTransition {
		t.Fatalf("expected ErrInvalidTransition reactivating an ACTIVE account, got %v", err)
	}

	suspended, err := s.SuspendBankAccount(ctx, domain.SuspendAccountParams{BankAccountID: acct.BankAccountID, TenantID: tenantID, Reason: "suspicious activity", ActorPrincipalID: "ops-1"})
	if err != nil || suspended.AccountStatus != "SUSPENDED" {
		t.Fatalf("suspend: status=%v err=%v", suspended, err)
	}

	// Suspending an already-SUSPENDED account must be rejected — not a
	// silent no-op that would hide a duplicate suspend request.
	if _, err := s.SuspendBankAccount(ctx, domain.SuspendAccountParams{BankAccountID: acct.BankAccountID, TenantID: tenantID, Reason: "again", ActorPrincipalID: "ops-1"}); err != domain.ErrInvalidTransition {
		t.Fatalf("expected ErrInvalidTransition double-suspending, got %v", err)
	}

	reactivated, err := s.ReactivateBankAccount(ctx, domain.ReactivateAccountParams{BankAccountID: acct.BankAccountID, TenantID: tenantID, ActorPrincipalID: "ops-1"})
	if err != nil || reactivated.AccountStatus != "ACTIVE" {
		t.Fatalf("reactivate: status=%v err=%v", reactivated, err)
	}

	closed, err := s.CloseBankAccount(ctx, domain.CloseAccountParams{BankAccountID: acct.BankAccountID, TenantID: tenantID, Reason: "account closed by client", ActorPrincipalID: "ops-1"})
	if err != nil || closed.AccountStatus != "CLOSED" {
		t.Fatalf("close: status=%v err=%v", closed, err)
	}

	// A CLOSED account can never transition again.
	if _, err := s.SuspendBankAccount(ctx, domain.SuspendAccountParams{BankAccountID: acct.BankAccountID, TenantID: tenantID, Reason: "x", ActorPrincipalID: "ops-1"}); err != domain.ErrInvalidTransition {
		t.Fatalf("expected ErrInvalidTransition suspending a CLOSED account, got %v", err)
	}
}

// TestPgStore_BankAccount_ClosedIsImmutable is the real, negative-
// controlled proof that a CLOSED account cannot be mutated at all —
// disabling the trigger and re-enabling it confirms the trigger itself,
// not something else, is what refuses the raw UPDATE.
func TestPgStore_BankAccount_ClosedIsImmutable(t *testing.T) {
	cleanTables(t)
	s := testStore

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	acct := newTestAccount(tenantID, uuid.New().String())
	if _, err := s.CreateBankAccount(ctx, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if _, err := s.CloseBankAccount(ctx, domain.CloseAccountParams{BankAccountID: acct.BankAccountID, TenantID: tenantID, Reason: "done", ActorPrincipalID: "ops-1"}); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := testPool.Exec(ctx, `UPDATE bank_accounts SET account_name = 'tampered' WHERE bank_account_id = $1`, acct.BankAccountID); err == nil {
		t.Fatal("expected the CLOSED-account trigger to refuse a raw UPDATE")
	}

	// Negative control: disable the trigger, confirm the same UPDATE now
	// succeeds (proving the trigger — not something else — was refusing
	// it), then re-enable and confirm refusal returns.
	if _, err := testPool.Exec(ctx, `ALTER TABLE bank_accounts DISABLE TRIGGER trg_reject_bank_account_mutation`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE bank_accounts SET account_name = 'tampered-while-disabled' WHERE bank_account_id = $1`, acct.BankAccountID); err != nil {
		t.Fatalf("expected the UPDATE to succeed with the trigger disabled, proving it was the real mechanism: %v", err)
	}
	if _, err := testPool.Exec(ctx, `ALTER TABLE bank_accounts ENABLE TRIGGER trg_reject_bank_account_mutation`); err != nil {
		t.Fatalf("re-enable trigger: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE bank_accounts SET account_name = 'tampered-again' WHERE bank_account_id = $1`, acct.BankAccountID); err == nil {
		t.Fatal("expected re-enabling the trigger to restore the refusal")
	}
}

// TestPgStore_RotateAccountIdentifierToken_RequiresVersionBump is the
// real, negative-controlled proof of the token-rotation trigger: a raw
// UPDATE changing masked_account_number without bumping token_version is
// refused, while RotateAccountIdentifierToken's own controlled path (which
// bumps the version atomically) succeeds.
func TestPgStore_RotateAccountIdentifierToken_RequiresVersionBump(t *testing.T) {
	cleanTables(t)
	s := testStore

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	acct := newTestAccount(tenantID, uuid.New().String())
	if _, err := s.CreateBankAccount(ctx, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if acct.TokenVersion != 1 {
		t.Fatalf("expected a freshly created account to start at token_version=1, got %d", acct.TokenVersion)
	}

	// A raw UPDATE that changes the identifier WITHOUT bumping token_version
	// must be refused by the trigger.
	if _, err := testPool.Exec(ctx, `UPDATE bank_accounts SET masked_account_number = '****9999' WHERE bank_account_id = $1`, acct.BankAccountID); err == nil {
		t.Fatal("expected the trigger to refuse an identifier change with no token_version bump")
	}

	rotated, err := s.RotateAccountIdentifierToken(ctx, domain.RotateAccountTokenParams{
		BankAccountID: acct.BankAccountID, TenantID: tenantID, NewMaskedAccountNumber: "****9999",
		NewBankIdentifier: acct.BankIdentifier, ActorPrincipalID: "ops-1",
	})
	if err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if rotated.MaskedAccountNumber != "****9999" {
		t.Fatalf("expected masked_account_number updated, got %s", rotated.MaskedAccountNumber)
	}
	if rotated.TokenVersion != 2 {
		t.Fatalf("expected token_version bumped to 2, got %d", rotated.TokenVersion)
	}
}

// TestPgStore_CreateBankAccount_IdempotentOnCorrelationID proves a retried
// CreateBankAccount call with the same correlation_id returns the
// original row (created=false) rather than a second account.
func TestPgStore_CreateBankAccount_IdempotentOnCorrelationID(t *testing.T) {
	cleanTables(t)
	s := testStore

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	legalEntityID := uuid.New().String()

	first := newTestAccount(tenantID, legalEntityID)
	first.CorrelationID = "corr-create-1"
	if created, err := s.CreateBankAccount(ctx, first); err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}

	retry := &domain.BankAccount{
		BankAccountID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		AccountName: "Different Name", MaskedAccountNumber: "****0000", BankIdentifier: "OTHER",
		CurrencyCode: "USD", AccountStatus: "ACTIVE", CorrelationID: "corr-create-1",
	}
	created, err := s.CreateBankAccount(ctx, retry)
	if err != nil {
		t.Fatalf("retried create: %v", err)
	}
	if created {
		t.Fatal("expected created=false on the retried call — this is a duplicate-account bug if true")
	}
	if retry.BankAccountID != first.BankAccountID {
		t.Fatalf("expected the retried call to return the ORIGINAL account id %s, got %s", first.BankAccountID, retry.BankAccountID)
	}

	list, err := s.ListBankAccounts(ctx, legalEntityID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 bank account for this correlation_id, got %d", len(list))
	}
}
