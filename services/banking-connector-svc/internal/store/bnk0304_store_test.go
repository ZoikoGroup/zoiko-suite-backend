package store_test

import (
	"context"
	"testing"
	"time"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

func seedConnection(t *testing.T, ctx context.Context, s *store.PgStore, tenantID string) string {
	t.Helper()
	conn, _, err := s.InitiateConnection(ctx, domain.InitiateConnectionParams{
		TenantID: tenantID, LegalEntityID: "le-x", BankAccountID: "acct-x", BankName: "Test Bank",
		CreatedByPrincipalID: "auditor-1", CorrelationID: "corr-seed-" + tenantID,
	})
	if err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	return conn.ConnectionID
}

// TestBNK03_IngestStatement_IdempotentOnContentHash proves a re-uploaded
// statement (same connection, same bytes) returns the original import
// rather than creating a duplicate.
func TestBNK03_IngestStatement_IdempotentOnContentHash(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk03-a")
	connID := seedConnection(t, ctx, s, "tenant-bnk03-a")

	req := domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(),
		ContentHash: "hash-abc", SourceID: "src-1", ImportBatchID: "batch-1",
		Lines: []domain.StatementLineIn{
			{PostedDate: time.Now(), Amount: 100.50, Currency: "USD", Description: "deposit"},
			{PostedDate: time.Now(), Amount: -20.00, Currency: "USD", Description: "fee"},
		},
	}

	first, err := s.IngestStatement(ctx, "tenant-bnk03-a", req, "auditor-1")
	if err != nil || !first.Created || len(first.Lines) != 2 {
		t.Fatalf("first ingest: created=%v lines=%d err=%v", first.Created, len(first.Lines), err)
	}

	second, err := s.IngestStatement(ctx, "tenant-bnk03-a", req, "auditor-1")
	if err != nil {
		t.Fatalf("retried ingest: %v", err)
	}
	if second.Created {
		t.Fatal("expected created=false on the retried ingest — this is a duplicate-import bug if true")
	}
	if second.Statement.StatementID != first.Statement.StatementID {
		t.Fatalf("expected the retried ingest to return the ORIGINAL statement id %s, got %s", first.Statement.StatementID, second.Statement.StatementID)
	}
	if len(second.Lines) != 2 {
		t.Fatalf("expected the retried ingest to return the original 2 lines, got %d", len(second.Lines))
	}
}

// TestBNK03_StatementLifecycle_CAS_Transitions proves the
// RECEIVED->VALIDATING->ACCEPTED path is CAS-enforced, and that a
// terminal (ACCEPTED) statement can never be re-validated.
func TestBNK03_StatementLifecycle_CAS_Transitions(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk03-b")
	connID := seedConnection(t, ctx, s, "tenant-bnk03-b")

	result, err := s.IngestStatement(ctx, "tenant-bnk03-b", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-lifecycle",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 10, Currency: "USD"}},
	}, "auditor-1")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	stmtID := result.Statement.StatementID

	// Accepting before validation must be rejected.
	if err := s.AcceptStatement(ctx, "tenant-bnk03-b", stmtID); err != domain.ErrInvalidStatementTransition {
		t.Fatalf("expected ErrInvalidStatementTransition accepting a RECEIVED statement, got %v", err)
	}

	if err := s.ValidateStatement(ctx, "tenant-bnk03-b", stmtID); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := s.AcceptStatement(ctx, "tenant-bnk03-b", stmtID); err != nil {
		t.Fatalf("accept: %v", err)
	}

	// Terminal: re-validating an ACCEPTED statement must be rejected.
	if err := s.ValidateStatement(ctx, "tenant-bnk03-b", stmtID); err != domain.ErrInvalidStatementTransition {
		t.Fatalf("expected ErrInvalidStatementTransition re-validating an ACCEPTED statement, got %v", err)
	}

	// DB-layer negative control: a raw UPDATE on the ACCEPTED header must
	// be refused by migration 004's own trigger. Run over admin
	// (superuser) deliberately — the trigger fires regardless of role.
	if _, err := admin.Exec(ctx, `UPDATE bank_statements SET status = 'RECEIVED' WHERE statement_id = $1`, stmtID); err == nil {
		t.Fatal("expected the trigger to refuse mutating an ACCEPTED statement via a raw UPDATE")
	}
	if _, err := admin.Exec(ctx, `ALTER TABLE bank_statements DISABLE TRIGGER trg_reject_terminal_statement_mutation`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE bank_statements SET status = 'RECEIVED' WHERE statement_id = $1`, stmtID); err != nil {
		t.Fatalf("expected the UPDATE to succeed with the trigger disabled, proving it was the real mechanism: %v", err)
	}
	if _, err := admin.Exec(ctx, `ALTER TABLE bank_statements ENABLE TRIGGER trg_reject_terminal_statement_mutation`); err != nil {
		t.Fatalf("re-enable trigger: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE bank_statements SET status = 'ACCEPTED' WHERE statement_id = $1`, stmtID); err != nil {
		t.Fatalf("restore accepted state: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE bank_statements SET status = 'RECEIVED' WHERE statement_id = $1`, stmtID); err == nil {
		t.Fatal("expected re-enabling the trigger to restore the refusal")
	}
}

// TestBNK03_StatementLines_AreAppendOnly is the negative-controlled proof
// that ingested evidence lines can never be edited or deleted.
func TestBNK03_StatementLines_AreAppendOnly(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk03-c")
	connID := seedConnection(t, ctx, s, "tenant-bnk03-c")
	result, err := s.IngestStatement(ctx, "tenant-bnk03-c", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-lines",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 5, Currency: "USD", Description: "orig"}},
	}, "auditor-1")
	if err != nil || len(result.Lines) != 1 {
		t.Fatalf("ingest: lines=%d err=%v", len(result.Lines), err)
	}
	lineID := result.Lines[0].LineID

	if _, err := admin.Exec(ctx, `UPDATE bank_statement_lines SET description = 'tampered' WHERE line_id = $1`, lineID); err == nil {
		t.Fatal("expected the trigger to refuse mutating a statement line")
	}
	if _, err := admin.Exec(ctx, `DELETE FROM bank_statement_lines WHERE line_id = $1`, lineID); err == nil {
		t.Fatal("expected statement lines to never be deletable")
	}
}

// TestBNK04_NormalizeTransaction_ActiveLineUniqueness proves that a
// second attempt to normalize the same statement line while the first
// canonical transaction is still live is rejected — a correction must go
// through ReNormalizeTransaction (supersession), not a second live row.
func TestBNK04_NormalizeTransaction_ActiveLineUniqueness(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk04-a")
	connID := seedConnection(t, ctx, s, "tenant-bnk04-a")
	result, err := s.IngestStatement(ctx, "tenant-bnk04-a", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-normalize",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 250, Currency: "USD", Description: "wire in"}},
	}, "auditor-1")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	lineID := result.Lines[0].LineID

	txn, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-bnk04-a", StatementLineID: lineID, TransactionDate: time.Now(), Amount: 250, Currency: "USD",
		Category: "WIRE_IN", Counterparty: "Acme Corp", ActorPrincipalID: "ops-1",
	})
	if err != nil {
		t.Fatalf("first normalize: %v", err)
	}
	if txn.Status != domain.TxnNormalized || txn.MappingVersion != 1 {
		t.Fatalf("unexpected initial canonical transaction: %+v", txn)
	}

	// A second normalize attempt on the same still-live line must fail —
	// enforced by idx_bank_txn_canonical_active_line, not application
	// logic that could be bypassed.
	if _, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-bnk04-a", StatementLineID: lineID, TransactionDate: time.Now(), Amount: 250, Currency: "USD",
		Category: "WIRE_IN", Counterparty: "Acme Corp", ActorPrincipalID: "ops-1",
	}); err == nil {
		t.Fatal("expected a second normalize on an already-normalized line to fail")
	}

	// Correction via supersession must succeed, preserve the prior row
	// (now SUPERSEDED, pointing at the new one), and bump mapping_version.
	corrected, err := s.ReNormalizeTransaction(ctx, domain.ReNormalizeTransactionParams{
		TenantID: "tenant-bnk04-a", PriorTransactionID: txn.TransactionID, TransactionDate: time.Now(), Amount: 250, Currency: "USD",
		Category: "WIRE_IN_CORRECTED", Counterparty: "Acme Corporation", ActorPrincipalID: "ops-2",
	})
	if err != nil {
		t.Fatalf("re-normalize: %v", err)
	}
	if corrected.Status != domain.TxnNormalized || corrected.MappingVersion != 2 || corrected.StatementLineID != lineID {
		t.Fatalf("unexpected corrected canonical transaction: %+v", corrected)
	}

	// Re-normalizing the now-SUPERSEDED prior row must be rejected.
	if _, err := s.ReNormalizeTransaction(ctx, domain.ReNormalizeTransactionParams{
		TenantID: "tenant-bnk04-a", PriorTransactionID: txn.TransactionID, TransactionDate: time.Now(), Amount: 250, Currency: "USD",
		Category: "X", Counterparty: "Y", ActorPrincipalID: "ops-2",
	}); err != domain.ErrInvalidTransactionTransition {
		t.Fatalf("expected ErrInvalidTransactionTransition re-normalizing an already-SUPERSEDED row, got %v", err)
	}
}

// TestBNK04_CanonicalTransaction_SupersededIsImmutable is the
// negative-controlled proof of migration 004's reject_canonical_txn_mutation
// trigger: a SUPERSEDED row can never be field-edited, even by a raw
// UPDATE, and even the one legitimate write (a NULL->value superseded_by)
// can never happen twice.
func TestBNK04_CanonicalTransaction_SupersededIsImmutable(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk04-b")
	connID := seedConnection(t, ctx, s, "tenant-bnk04-b")
	result, err := s.IngestStatement(ctx, "tenant-bnk04-b", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-immutable",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 75, Currency: "USD"}},
	}, "auditor-1")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	txn, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-bnk04-b", StatementLineID: result.Lines[0].LineID, TransactionDate: time.Now(), Amount: 75, Currency: "USD",
		Category: "MISC", ActorPrincipalID: "ops-1",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if _, err := s.ReNormalizeTransaction(ctx, domain.ReNormalizeTransactionParams{
		TenantID: "tenant-bnk04-b", PriorTransactionID: txn.TransactionID, TransactionDate: time.Now(), Amount: 75, Currency: "USD",
		Category: "MISC_CORRECTED", ActorPrincipalID: "ops-2",
	}); err != nil {
		t.Fatalf("re-normalize: %v", err)
	}

	// txn.TransactionID is now SUPERSEDED. A raw UPDATE trying to change
	// its amount must be refused — run over admin, since the trigger
	// fires regardless of role.
	if _, err := admin.Exec(ctx, `UPDATE bank_transactions_canonical SET amount = 999 WHERE transaction_id = $1`, txn.TransactionID); err == nil {
		t.Fatal("expected the trigger to refuse editing a SUPERSEDED canonical transaction")
	}

	if _, err := admin.Exec(ctx, `ALTER TABLE bank_transactions_canonical DISABLE TRIGGER trg_reject_canonical_txn_mutation`); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE bank_transactions_canonical SET amount = 999 WHERE transaction_id = $1`, txn.TransactionID); err != nil {
		t.Fatalf("expected the UPDATE to succeed with the trigger disabled, proving it was the real mechanism: %v", err)
	}
	if _, err := admin.Exec(ctx, `ALTER TABLE bank_transactions_canonical ENABLE TRIGGER trg_reject_canonical_txn_mutation`); err != nil {
		t.Fatalf("re-enable trigger: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE bank_transactions_canonical SET amount = 1000 WHERE transaction_id = $1`, txn.TransactionID); err == nil {
		t.Fatal("expected re-enabling the trigger to restore the refusal")
	}
}

// TestBNK04_ApproveMappingException_RejectsSelfApproval is the real proof
// of BNK-04's maker-checker resolution: the principal who raised a
// mapping exception cannot also be the one who approves it.
func TestBNK04_ApproveMappingException_RejectsSelfApproval(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk04-c")
	connID := seedConnection(t, ctx, s, "tenant-bnk04-c")
	result, err := s.IngestStatement(ctx, "tenant-bnk04-c", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-exception",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 42, Currency: "USD", Description: "unknown code"}},
	}, "auditor-1")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	lineID := result.Lines[0].LineID

	exc, err := s.QuarantineTransaction(ctx, domain.QuarantineTransactionParams{
		TenantID: "tenant-bnk04-c", StatementLineID: lineID, Reason: "unrecognized bank code", ActorPrincipalID: "ops-1",
	})
	if err != nil || exc.Status != "OPEN" {
		t.Fatalf("quarantine: status=%v err=%v", exc, err)
	}

	// Same principal approving their own exception must be rejected.
	if _, err := s.ApproveMappingException(ctx, domain.ApproveMappingExceptionParams{
		TenantID: "tenant-bnk04-c", ExceptionID: exc.ExceptionID, TransactionDate: time.Now(), Amount: 42, Currency: "USD",
		Category: "MISC", ApproverPrincipalID: "ops-1",
	}); err != domain.ErrMappingExceptionSelfApproval {
		t.Fatalf("expected ErrMappingExceptionSelfApproval, got %v", err)
	}

	// A different principal approving must succeed, create the real
	// canonical transaction, and close the exception.
	txn, err := s.ApproveMappingException(ctx, domain.ApproveMappingExceptionParams{
		TenantID: "tenant-bnk04-c", ExceptionID: exc.ExceptionID, TransactionDate: time.Now(), Amount: 42, Currency: "USD",
		Category: "MISC", ApproverPrincipalID: "ops-2",
	})
	if err != nil || txn.StatementLineID != lineID {
		t.Fatalf("approve: txn=%+v err=%v", txn, err)
	}

	// Approving an already-resolved exception a second time must fail.
	if _, err := s.ApproveMappingException(ctx, domain.ApproveMappingExceptionParams{
		TenantID: "tenant-bnk04-c", ExceptionID: exc.ExceptionID, TransactionDate: time.Now(), Amount: 42, Currency: "USD",
		Category: "MISC", ApproverPrincipalID: "ops-3",
	}); err != domain.ErrMappingExceptionNotOpen {
		t.Fatalf("expected ErrMappingExceptionNotOpen re-approving a resolved exception, got %v", err)
	}
}
