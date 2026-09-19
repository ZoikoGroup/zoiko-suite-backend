// BNK-03 Statement Ingestion + BNK-04 Transaction Normalization
// persistence. Kept in its own file, same "self-contained composed
// interface" pattern used for every other capability added to an
// existing service in this build.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/banking-connector-svc/internal/domain"
)

const statementColumns = `
	statement_id, connection_id, tenant_id, statement_format, statement_date, opening_balance, closing_balance, transaction_count, ingested_at,
	content_hash, source_id, import_batch_id, status, quarantine_reason, created_by_principal_id`

func scanStatement(row pgx.Row, s *domain.BankStatement, contentHash, sourceID, importBatchID, status, quarantineReason, createdBy *string) error {
	return row.Scan(&s.StatementID, &s.ConnectionID, &s.TenantID, &s.StatementFormat, &s.StatementDate, &s.OpeningBalance, &s.ClosingBalance, &s.TransactionCount, &s.IngestedAt,
		contentHash, sourceID, importBatchID, status, quarantineReason, createdBy)
}

// IngestStatement is BNK-03's real entry point: a RECEIVED-status header
// plus its immutable evidence lines, written in one transaction.
// Idempotent on (connection_id, content_hash) — a re-uploaded statement
// returns the original import rather than double-ingesting it.
func (p *PgStore) IngestStatement(ctx context.Context, tenantID string, req domain.IngestStatementLinesRequest, actorPrincipalID string) (*domain.IngestStatementResult, error) {
	result := &domain.IngestStatementResult{}
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		statementID := uuid.New().String()
		var s domain.BankStatement
		var contentHash, sourceID, importBatchID, status, quarantineReason, createdBy string
		row := tx.QueryRow(ctx, `
			INSERT INTO bank_statements (
				statement_id, connection_id, tenant_id, statement_format, statement_date, opening_balance, closing_balance, transaction_count, ingested_at,
				content_hash, source_id, import_batch_id, status, created_by_principal_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now(),$9,$10,$11,'RECEIVED',$12)
			ON CONFLICT (connection_id, content_hash) WHERE content_hash <> '' DO NOTHING
			RETURNING `+statementColumns,
			statementID, req.ConnectionID, tenantID, req.StatementFormat, req.StatementDate, req.OpeningBalance, req.ClosingBalance, len(req.Lines),
			req.ContentHash, req.SourceID, req.ImportBatchID, actorPrincipalID)
		err := scanStatement(row, &s, &contentHash, &sourceID, &importBatchID, &status, &quarantineReason, &createdBy)
		if err == nil {
			result.Created = true
			for i, line := range req.Lines {
				var l domain.StatementLine
				lrow := tx.QueryRow(ctx, `
					INSERT INTO bank_statement_lines (line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency, description, raw_reference, bank_code)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
					RETURNING line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency, description, raw_reference, bank_code, created_at`,
					uuid.New().String(), s.StatementID, tenantID, i+1, line.PostedDate, line.Amount, line.Currency, line.Description, line.RawReference, line.BankCode)
				if err := lrow.Scan(&l.LineID, &l.StatementID, &l.TenantID, &l.LineSeq, &l.PostedDate, &l.Amount, &l.Currency, &l.Description, &l.RawReference, &l.BankCode, &l.CreatedAt); err != nil {
					return err
				}
				result.Lines = append(result.Lines, l)
			}
			result.Statement = s
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Conflict: return the original import and its existing lines.
		row = tx.QueryRow(ctx, `SELECT `+statementColumns+` FROM bank_statements WHERE connection_id=$1 AND content_hash=$2`, req.ConnectionID, req.ContentHash)
		if err := scanStatement(row, &s, &contentHash, &sourceID, &importBatchID, &status, &quarantineReason, &createdBy); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency, description, raw_reference, bank_code, created_at
			FROM bank_statement_lines WHERE statement_id=$1 ORDER BY line_seq ASC`, s.StatementID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.StatementLine
			if err := rows.Scan(&l.LineID, &l.StatementID, &l.TenantID, &l.LineSeq, &l.PostedDate, &l.Amount, &l.Currency, &l.Description, &l.RawReference, &l.BankCode, &l.CreatedAt); err != nil {
				return err
			}
			result.Lines = append(result.Lines, l)
		}
		result.Statement = s
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (p *PgStore) notFoundOrInvalidStatement(ctx context.Context, tenantID, statementID string) error {
	var exists bool
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bank_statements WHERE statement_id=$1 AND tenant_id=$2)`, statementID, tenantID).Scan(&exists)
	})
	if err != nil {
		return err
	}
	if !exists {
		return domain.ErrStatementNotFound
	}
	return domain.ErrInvalidStatementTransition
}

// ValidateStatement is BNK-03's actual completeness/balance check: it
// verifies opening_balance + sum(all ingested line amounts) equals
// closing_balance before letting the statement reach a state from which it
// can be ACCEPTED. A mismatch never surfaces as a plain error to the
// caller — it auto-transitions the statement straight to QUARANTINED with
// a reason, the same fail-closed posture as every other evidence-integrity
// check in this service, so a caller cannot retry their way past a bad
// statement by calling AcceptStatement directly.
func (p *PgStore) ValidateStatement(ctx context.Context, tenantID, statementID string) error {
	var mismatchReason string
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var opening, closing float64
		var status string
		if err := tx.QueryRow(ctx, `SELECT opening_balance, closing_balance, status FROM bank_statements WHERE statement_id=$1 AND tenant_id=$2`, statementID, tenantID).Scan(&opening, &closing, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrStatementNotFound
			}
			return err
		}
		if !domain.CanValidateStatement(status) {
			return domain.ErrInvalidStatementTransition
		}
		var lineSum float64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0) FROM bank_statement_lines WHERE statement_id=$1 AND tenant_id=$2`, statementID, tenantID).Scan(&lineSum); err != nil {
			return err
		}
		const epsilon = 0.005 // half a cent — floating-point tolerance, not a business allowance
		if diff := opening + lineSum - closing; diff > epsilon || diff < -epsilon {
			mismatchReason = fmt.Sprintf("opening_balance (%.2f) + sum(lines) (%.2f) = %.2f does not equal closing_balance (%.2f)", opening, lineSum, opening+lineSum, closing)
			tag, err := tx.Exec(ctx, `UPDATE bank_statements SET status='QUARANTINED', quarantine_reason=$3 WHERE statement_id=$1 AND tenant_id=$2 AND status='RECEIVED'`, statementID, tenantID, mismatchReason)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return domain.ErrInvalidStatementTransition
			}
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE bank_statements SET status='VALIDATING' WHERE statement_id=$1 AND tenant_id=$2 AND status='RECEIVED'`, statementID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidStatementTransition
		}
		return nil
	})
	if err != nil {
		return err
	}
	if mismatchReason != "" {
		return domain.ErrStatementBalanceMismatch
	}
	return nil
}

func (p *PgStore) AcceptStatement(ctx context.Context, tenantID, statementID string) error {
	tag, err := p.execWithTenant(ctx, `UPDATE bank_statements SET status='ACCEPTED' WHERE statement_id=$1 AND tenant_id=$2 AND status='VALIDATING'`, statementID, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return p.notFoundOrInvalidStatement(ctx, tenantID, statementID)
	}
	return nil
}

func (p *PgStore) QuarantineStatement(ctx context.Context, tenantID, statementID, reason string) error {
	tag, err := p.execWithTenant(ctx, `UPDATE bank_statements SET status='QUARANTINED', quarantine_reason=$3 WHERE statement_id=$1 AND tenant_id=$2 AND status IN ('RECEIVED','VALIDATING')`, statementID, tenantID, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return p.notFoundOrInvalidStatement(ctx, tenantID, statementID)
	}
	return nil
}

func (p *PgStore) ReprocessQuarantinedStatement(ctx context.Context, tenantID, statementID string) error {
	tag, err := p.execWithTenant(ctx, `UPDATE bank_statements SET status='RECEIVED', quarantine_reason='' WHERE statement_id=$1 AND tenant_id=$2 AND status='QUARANTINED'`, statementID, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return p.notFoundOrInvalidStatement(ctx, tenantID, statementID)
	}
	return nil
}

// execWithTenant is a thin CAS-style helper for the handful of BNK-03/04
// commands that are plain status-column UPDATEs with no extra RETURNING
// payload to scan.
func (p *PgStore) execWithTenant(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	var tag pgconn.CommandTag
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		tag, err = tx.Exec(ctx, sql, args...)
		return err
	})
	return tag, err
}

const canonicalTxnColumns = `
	transaction_id, tenant_id, statement_line_id, mapping_version, transaction_date, amount, currency, category, counterparty, status, superseded_by, created_by_principal_id, created_at`

func scanCanonicalTxn(row pgx.Row, t *domain.CanonicalTransaction) error {
	var supersededBy *string
	err := row.Scan(&t.TransactionID, &t.TenantID, &t.StatementLineID, &t.MappingVersion, &t.TransactionDate, &t.Amount, &t.Currency, &t.Category, &t.Counterparty, &t.Status, &supersededBy, &t.CreatedByPrincipalID, &t.CreatedAt)
	if err != nil {
		return err
	}
	if supersededBy != nil {
		t.SupersededBy = *supersededBy
	}
	return nil
}

// NormalizeTransaction is BNK-04's real entry point — one NORMALIZED
// canonical row per statement line. idx_bank_txn_canonical_active_line
// (migration 004) enforces at most one live (non-SUPERSEDED) row per
// line, so a second attempt on an already-normalized line fails here
// rather than silently creating a duplicate.
//
// The line's category is never a caller-supplied guess: it is resolved
// against bank_transaction_mappings (migration 005) using the line's own
// bank_code evidence. A bank_code with no mapping is automatically routed
// to a mapping exception — idx_mapping_exceptions_open_line (migration
// 004) already guarantees at most one OPEN exception per line, so a
// second normalize attempt on an already-quarantined line reuses the
// existing exception rather than raising a duplicate.
func (p *PgStore) NormalizeTransaction(ctx context.Context, params domain.NormalizeTransactionParams) (*domain.NormalizeTransactionResult, error) {
	var result domain.NormalizeTransactionResult
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var bankCode string
		err := tx.QueryRow(ctx, `SELECT bank_code FROM bank_statement_lines WHERE line_id=$1 AND tenant_id=$2`, params.StatementLineID, params.TenantID).Scan(&bankCode)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrStatementLineNotFound
			}
			return err
		}

		var category string
		err = tx.QueryRow(ctx, `SELECT category FROM bank_transaction_mappings WHERE tenant_id=$1 AND bank_code=$2`, params.TenantID, bankCode).Scan(&category)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			// Unknown bank_code — auto-quarantine, never guess a category.
			// Checked before inserting (not insert-then-recover): a unique
			// violation would abort the whole transaction, and there is no
			// savepoint here to roll back to.
			var e domain.MappingException
			var resolvedTxnID, resolvedBy *string
			var resolvedAt *time.Time
			existingRow := tx.QueryRow(ctx, `SELECT exception_id, tenant_id, statement_line_id, reason, status, raised_by_principal_id, resolved_transaction_id, resolved_by_principal_id, resolved_at, created_at
				FROM mapping_exceptions WHERE statement_line_id=$1 AND status='OPEN'`, params.StatementLineID)
			err = existingRow.Scan(&e.ExceptionID, &e.TenantID, &e.StatementLineID, &e.Reason, &e.Status, &e.RaisedByPrincipalID, &resolvedTxnID, &resolvedBy, &resolvedAt, &e.CreatedAt)
			if err == nil {
				result.QuarantinedException = &e
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			reason := fmt.Sprintf("no code mapping found for bank_code %q", bankCode)
			excRow := tx.QueryRow(ctx, `
				INSERT INTO mapping_exceptions (exception_id, tenant_id, statement_line_id, reason, raised_by_principal_id)
				VALUES ($1,$2,$3,$4,$5)
				RETURNING exception_id, tenant_id, statement_line_id, reason, status, raised_by_principal_id, resolved_transaction_id, resolved_by_principal_id, resolved_at, created_at`,
				uuid.New().String(), params.TenantID, params.StatementLineID, reason, params.ActorPrincipalID)
			if err := excRow.Scan(&e.ExceptionID, &e.TenantID, &e.StatementLineID, &e.Reason, &e.Status, &e.RaisedByPrincipalID, &resolvedTxnID, &resolvedBy, &resolvedAt, &e.CreatedAt); err != nil {
				return err
			}
			result.QuarantinedException = &e
			return nil
		}

		var t domain.CanonicalTransaction
		row := tx.QueryRow(ctx, `
			INSERT INTO bank_transactions_canonical (transaction_id, tenant_id, statement_line_id, transaction_date, amount, currency, category, counterparty, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING `+canonicalTxnColumns,
			uuid.New().String(), params.TenantID, params.StatementLineID, params.TransactionDate, params.Amount, params.Currency, category, params.Counterparty, params.ActorPrincipalID)
		if err := scanCanonicalTxn(row, &t); err != nil {
			return err
		}
		result.Transaction = &t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// CreateTransactionMapping adds one bank_code -> category dictionary entry
// for a tenant. idx_bank_transaction_mappings_tenant_code (migration 005)
// enforces uniqueness — a duplicate bank_code for the same tenant is
// rejected rather than silently overwriting a mapping NormalizeTransaction
// may already be relying on.
func (p *PgStore) CreateTransactionMapping(ctx context.Context, params domain.CreateTransactionMappingParams) (*domain.TransactionMapping, error) {
	var m domain.TransactionMapping
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO bank_transaction_mappings (mapping_id, tenant_id, bank_code, category, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5)
			RETURNING mapping_id, tenant_id, bank_code, category, created_by_principal_id, created_at`,
			uuid.New().String(), params.TenantID, params.BankCode, params.Category, params.ActorPrincipalID)
		return row.Scan(&m.MappingID, &m.TenantID, &m.BankCode, &m.Category, &m.CreatedByPrincipalID, &m.CreatedAt)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, domain.ErrMappingAlreadyExists
		}
		return nil, err
	}
	return &m, nil
}

// ReNormalizeTransaction supersedes an existing NORMALIZED transaction
// with a corrected one — never edits the prior row's fields (migration
// 004's trigger blocks that), only its superseded_by pointer, in the same
// transaction as inserting the replacement.
func (p *PgStore) ReNormalizeTransaction(ctx context.Context, params domain.ReNormalizeTransactionParams) (*domain.CanonicalTransaction, error) {
	var newTxn domain.CanonicalTransaction
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var prior domain.CanonicalTransaction
		if err := scanCanonicalTxn(tx.QueryRow(ctx, `SELECT `+canonicalTxnColumns+` FROM bank_transactions_canonical WHERE transaction_id=$1 AND tenant_id=$2`, params.PriorTransactionID, params.TenantID), &prior); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrTransactionNotFound
			}
			return err
		}
		if prior.Status != domain.TxnNormalized {
			return domain.ErrInvalidTransactionTransition
		}
		newID := uuid.New().String()
		// The active-line uniqueness index is on statement_line_id, so the
		// old row must be marked SUPERSEDED first, freeing the slot, before
		// the new row can claim it with the same statement_line_id.
		if _, err := tx.Exec(ctx, `UPDATE bank_transactions_canonical SET status='SUPERSEDED', superseded_by=$2 WHERE transaction_id=$1`, prior.TransactionID, newID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO bank_transactions_canonical (transaction_id, tenant_id, statement_line_id, mapping_version, transaction_date, amount, currency, category, counterparty, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			RETURNING `+canonicalTxnColumns,
			newID, params.TenantID, prior.StatementLineID, prior.MappingVersion+1, params.TransactionDate, params.Amount, params.Currency, params.Category, params.Counterparty, params.ActorPrincipalID)
		return scanCanonicalTxn(row, &newTxn)
	})
	if err != nil {
		return nil, err
	}
	return &newTxn, nil
}

func (p *PgStore) QuarantineTransaction(ctx context.Context, params domain.QuarantineTransactionParams) (*domain.MappingException, error) {
	var e domain.MappingException
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO mapping_exceptions (exception_id, tenant_id, statement_line_id, reason, raised_by_principal_id)
			VALUES ($1,$2,$3,$4,$5)
			RETURNING exception_id, tenant_id, statement_line_id, reason, status, raised_by_principal_id, resolved_transaction_id, resolved_by_principal_id, resolved_at, created_at`,
			uuid.New().String(), params.TenantID, params.StatementLineID, params.Reason, params.ActorPrincipalID)
		var resolvedTxnID, resolvedBy *string
		var resolvedAt *time.Time
		return row.Scan(&e.ExceptionID, &e.TenantID, &e.StatementLineID, &e.Reason, &e.Status, &e.RaisedByPrincipalID, &resolvedTxnID, &resolvedBy, &resolvedAt, &e.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ApproveMappingException is BNK-04's maker-checker resolution: it
// creates the real canonical transaction for the previously-quarantined
// line and closes the exception in the same transaction. The approver
// must differ from whoever raised the exception — enforced here, at the
// point of write, not just checked by the caller.
func (p *PgStore) ApproveMappingException(ctx context.Context, params domain.ApproveMappingExceptionParams) (*domain.CanonicalTransaction, error) {
	var t domain.CanonicalTransaction
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var e domain.MappingException
		var resolvedTxnID, resolvedBy *string
		var resolvedAt *time.Time
		err := tx.QueryRow(ctx, `SELECT exception_id, tenant_id, statement_line_id, reason, status, raised_by_principal_id, resolved_transaction_id, resolved_by_principal_id, resolved_at, created_at
			FROM mapping_exceptions WHERE exception_id=$1 AND tenant_id=$2`, params.ExceptionID, params.TenantID).
			Scan(&e.ExceptionID, &e.TenantID, &e.StatementLineID, &e.Reason, &e.Status, &e.RaisedByPrincipalID, &resolvedTxnID, &resolvedBy, &resolvedAt, &e.CreatedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrMappingExceptionNotFound
			}
			return err
		}
		if e.Status != "OPEN" {
			return domain.ErrMappingExceptionNotOpen
		}
		if e.RaisedByPrincipalID == params.ApproverPrincipalID {
			return domain.ErrMappingExceptionSelfApproval
		}

		txnRow := tx.QueryRow(ctx, `
			INSERT INTO bank_transactions_canonical (transaction_id, tenant_id, statement_line_id, transaction_date, amount, currency, category, counterparty, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING `+canonicalTxnColumns,
			uuid.New().String(), params.TenantID, e.StatementLineID, params.TransactionDate, params.Amount, params.Currency, params.Category, params.Counterparty, params.ApproverPrincipalID)
		if err := scanCanonicalTxn(txnRow, &t); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE mapping_exceptions SET status='APPROVED', resolved_transaction_id=$2, resolved_by_principal_id=$3, resolved_at=now() WHERE exception_id=$1`,
			e.ExceptionID, t.TransactionID, params.ApproverPrincipalID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (p *PgStore) GetCanonicalTransaction(ctx context.Context, tenantID, transactionID string) (*domain.CanonicalTransaction, error) {
	var t domain.CanonicalTransaction
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+canonicalTxnColumns+` FROM bank_transactions_canonical WHERE transaction_id=$1 AND tenant_id=$2`, transactionID, tenantID)
		return scanCanonicalTxn(row, &t)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTransactionNotFound
		}
		return nil, err
	}
	return &t, nil
}

