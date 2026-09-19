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
		OpeningBalance: 0, ClosingBalance: 10,
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

// TestBNK03_ValidateStatement_MatchingBalance_ValidatesCleanly proves the
// real completeness check: when opening_balance + sum(lines) ==
// closing_balance, ValidateStatement transitions RECEIVED->VALIDATING as
// normal.
func TestBNK03_ValidateStatement_MatchingBalance_ValidatesCleanly(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk03-balance-ok")
	connID := seedConnection(t, ctx, s, "tenant-bnk03-balance-ok")

	result, err := s.IngestStatement(ctx, "tenant-bnk03-balance-ok", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-balance-ok",
		OpeningBalance: 1000.00, ClosingBalance: 1080.50,
		Lines: []domain.StatementLineIn{
			{PostedDate: time.Now(), Amount: 100.50, Currency: "USD", Description: "deposit"},
			{PostedDate: time.Now(), Amount: -20.00, Currency: "USD", Description: "fee"},
		},
	}, "auditor-1")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if err := s.ValidateStatement(ctx, "tenant-bnk03-balance-ok", result.Statement.StatementID); err != nil {
		t.Fatalf("expected clean validation with matching balances, got %v", err)
	}

	var status string
	if err := admin.QueryRow(ctx, `SELECT status FROM bank_statements WHERE statement_id = $1`, result.Statement.StatementID).Scan(&status); err != nil {
		t.Fatalf("get statement: %v", err)
	}
	if status != domain.StatementValidating {
		t.Fatalf("expected status VALIDATING, got %s", status)
	}
}

// TestBNK03_ValidateStatement_BrokenBalance_AutoQuarantines proves that a
// deliberately broken balance is never allowed to reach ACCEPTED — it is
// auto-quarantined with a reason by ValidateStatement itself, not merely
// rejected.
func TestBNK03_ValidateStatement_BrokenBalance_AutoQuarantines(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk03-balance-bad")
	connID := seedConnection(t, ctx, s, "tenant-bnk03-balance-bad")

	result, err := s.IngestStatement(ctx, "tenant-bnk03-balance-bad", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-balance-bad",
		OpeningBalance: 1000.00, ClosingBalance: 5000.00, // deliberately wrong — lines don't reconcile to this
		Lines: []domain.StatementLineIn{
			{PostedDate: time.Now(), Amount: 100.50, Currency: "USD", Description: "deposit"},
		},
	}, "auditor-1")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if err := s.ValidateStatement(ctx, "tenant-bnk03-balance-bad", result.Statement.StatementID); err != domain.ErrStatementBalanceMismatch {
		t.Fatalf("expected ErrStatementBalanceMismatch, got %v", err)
	}

	var status, quarantineReason string
	if err := admin.QueryRow(ctx, `SELECT status, quarantine_reason FROM bank_statements WHERE statement_id = $1`, result.Statement.StatementID).Scan(&status, &quarantineReason); err != nil {
		t.Fatalf("get statement: %v", err)
	}
	if status != domain.StatementQuarantine {
		t.Fatalf("expected status QUARANTINED, got %s", status)
	}
	if quarantineReason == "" {
		t.Fatal("expected a non-empty quarantine reason explaining the balance mismatch")
	}

	// AcceptStatement must still be impossible — there is no way to
	// bypass the auto-quarantine by calling accept directly.
	if err := s.AcceptStatement(ctx, "tenant-bnk03-balance-bad", result.Statement.StatementID); err != domain.ErrInvalidStatementTransition {
		t.Fatalf("expected ErrInvalidStatementTransition accepting a QUARANTINED statement, got %v", err)
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
	if _, err := s.CreateTransactionMapping(ctx, domain.CreateTransactionMappingParams{
		TenantID: "tenant-bnk04-a", BankCode: "WIRE-IN-CODE", Category: "WIRE_IN", ActorPrincipalID: "ops-admin",
	}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	result, err := s.IngestStatement(ctx, "tenant-bnk04-a", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-normalize",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 250, Currency: "USD", Description: "wire in", BankCode: "WIRE-IN-CODE"}},
	}, "auditor-1")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	lineID := result.Lines[0].LineID

	normResult, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-bnk04-a", StatementLineID: lineID, TransactionDate: time.Now(), Amount: 250, Currency: "USD",
		Counterparty: "Acme Corp", ActorPrincipalID: "ops-1",
	})
	if err != nil {
		t.Fatalf("first normalize: %v", err)
	}
	if normResult.Transaction == nil {
		t.Fatalf("expected a mapped bank_code to normalize, got quarantine: %+v", normResult.QuarantinedException)
	}
	txn := normResult.Transaction
	if txn.Status != domain.TxnNormalized || txn.MappingVersion != 1 || txn.Category != "WIRE_IN" {
		t.Fatalf("unexpected initial canonical transaction: %+v", txn)
	}

	// A second normalize attempt on the same still-live line must fail —
	// enforced by idx_bank_txn_canonical_active_line, not application
	// logic that could be bypassed.
	if _, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-bnk04-a", StatementLineID: lineID, TransactionDate: time.Now(), Amount: 250, Currency: "USD",
		Counterparty: "Acme Corp", ActorPrincipalID: "ops-1",
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
	if _, err := s.CreateTransactionMapping(ctx, domain.CreateTransactionMappingParams{
		TenantID: "tenant-bnk04-b", BankCode: "MISC-CODE", Category: "MISC", ActorPrincipalID: "ops-admin",
	}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	result, err := s.IngestStatement(ctx, "tenant-bnk04-b", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-immutable",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 75, Currency: "USD", BankCode: "MISC-CODE"}},
	}, "auditor-1")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	normResult, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-bnk04-b", StatementLineID: result.Lines[0].LineID, TransactionDate: time.Now(), Amount: 75, Currency: "USD",
		ActorPrincipalID: "ops-1",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if normResult.Transaction == nil {
		t.Fatalf("expected a mapped bank_code to normalize, got quarantine: %+v", normResult.QuarantinedException)
	}
	txn := normResult.Transaction
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

// TestBNK04_NormalizeTransaction_KnownCode_NormalizesAutomatically proves
// the real code-mapping dictionary: a line whose bank_code has a
// registered mapping normalizes straight to NORMALIZED with the mapped
// category, never a caller-supplied guess (Category is not even a field
// on NormalizeTransactionParams any more).
func TestBNK04_NormalizeTransaction_KnownCode_NormalizesAutomatically(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk04-map-known")
	connID := seedConnection(t, ctx, s, "tenant-bnk04-map-known")

	if _, err := s.CreateTransactionMapping(ctx, domain.CreateTransactionMappingParams{
		TenantID: "tenant-bnk04-map-known", BankCode: "ACH-CREDIT", Category: "ACH_CREDIT", ActorPrincipalID: "ops-admin",
	}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	result, err := s.IngestStatement(ctx, "tenant-bnk04-map-known", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-map-known",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 500, Currency: "USD", Description: "payroll", BankCode: "ACH-CREDIT"}},
	}, "auditor-1")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	normResult, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-bnk04-map-known", StatementLineID: result.Lines[0].LineID, TransactionDate: time.Now(), Amount: 500, Currency: "USD",
		Counterparty: "Employer Inc", ActorPrincipalID: "ops-1",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if normResult.QuarantinedException != nil {
		t.Fatalf("expected a known bank_code to normalize, got a quarantine instead: %+v", normResult.QuarantinedException)
	}
	if normResult.Transaction == nil || normResult.Transaction.Category != "ACH_CREDIT" || normResult.Transaction.Status != domain.TxnNormalized {
		t.Fatalf("expected the mapped category ACH_CREDIT on a NORMALIZED row, got %+v", normResult.Transaction)
	}
}

// TestBNK04_NormalizeTransaction_UnknownCode_AutoQuarantines is the
// negative control: a bank_code with no dictionary entry is never
// accepted with a guessed category — it is automatically routed to a
// mapping exception instead, and a second normalize attempt on the same
// still-quarantined line returns the same exception rather than erroring
// or creating a duplicate OPEN exception (idx_mapping_exceptions_open_line).
func TestBNK04_NormalizeTransaction_UnknownCode_AutoQuarantines(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk04-map-unknown")
	connID := seedConnection(t, ctx, s, "tenant-bnk04-map-unknown")

	result, err := s.IngestStatement(ctx, "tenant-bnk04-map-unknown", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-map-unknown",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 999, Currency: "USD", Description: "mystery", BankCode: "XYZ-UNKNOWN"}},
	}, "auditor-1")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	lineID := result.Lines[0].LineID

	first, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-bnk04-map-unknown", StatementLineID: lineID, TransactionDate: time.Now(), Amount: 999, Currency: "USD",
		Counterparty: "Unknown Corp", ActorPrincipalID: "ops-1",
	})
	if err != nil {
		t.Fatalf("first normalize: %v", err)
	}
	if first.Transaction != nil {
		t.Fatalf("expected an unmapped bank_code to be quarantined, not accepted with a guessed category: %+v", first.Transaction)
	}
	if first.QuarantinedException == nil || first.QuarantinedException.Status != "OPEN" {
		t.Fatalf("expected an OPEN mapping exception, got %+v", first.QuarantinedException)
	}

	second, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-bnk04-map-unknown", StatementLineID: lineID, TransactionDate: time.Now(), Amount: 999, Currency: "USD",
		Counterparty: "Unknown Corp", ActorPrincipalID: "ops-1",
	})
	if err != nil {
		t.Fatalf("second normalize: %v", err)
	}
	if second.QuarantinedException == nil || second.QuarantinedException.ExceptionID != first.QuarantinedException.ExceptionID {
		t.Fatalf("expected the second attempt to return the SAME exception %s, got %+v", first.QuarantinedException.ExceptionID, second.QuarantinedException)
	}
}

// TestBNK04_CreateTransactionMapping_RejectsDuplicateBankCode proves
// idx_bank_transaction_mappings_tenant_code: a second mapping for the same
// tenant+bank_code is rejected rather than silently overwriting one
// NormalizeTransaction may already be relying on.
func TestBNK04_CreateTransactionMapping_RejectsDuplicateBankCode(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := middleware.WithTenant(context.Background(), "tenant-bnk04-map-dup")

	if _, err := s.CreateTransactionMapping(ctx, domain.CreateTransactionMappingParams{
		TenantID: "tenant-bnk04-map-dup", BankCode: "DUP-CODE", Category: "MISC", ActorPrincipalID: "ops-admin",
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := s.CreateTransactionMapping(ctx, domain.CreateTransactionMappingParams{
		TenantID: "tenant-bnk04-map-dup", BankCode: "DUP-CODE", Category: "SOMETHING_ELSE", ActorPrincipalID: "ops-admin",
	}); err != domain.ErrMappingAlreadyExists {
		t.Fatalf("expected ErrMappingAlreadyExists, got %v", err)
	}
}
