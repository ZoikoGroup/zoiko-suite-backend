// BNK-03 Statement Ingestion + BNK-04 Transaction Normalization
// persistence. Kept in its own file, same "self-contained composed
// interface" pattern used for every other capability added to an
// existing service in this build.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/banking-connector-svc/internal/domain"
)

const statementColumns = `
	statement_id, connection_id, tenant_id, statement_format, statement_date, opening_balance, closing_balance, transaction_count, ingested_at,
	content_hash, source_id, import_batch_id, status, quarantine_reason, created_by_principal_id`

func scanStatement(row pgx.Row, s *domain.BankStatement) error {
	return row.Scan(&s.StatementID, &s.ConnectionID, &s.TenantID, &s.StatementFormat, &s.StatementDate, &s.OpeningBalance, &s.ClosingBalance, &s.TransactionCount, &s.IngestedAt,
		&s.ContentHash, &s.SourceID, &s.ImportBatchID, &s.Status, &s.QuarantineReason, &s.CreatedByPrincipalID)
}

// IngestStatement is BNK-03's real entry point: a RECEIVED-status header
// plus its immutable evidence lines, written in one transaction.
// Idempotent on (connection_id, content_hash) — a re-uploaded statement
// returns the original import rather than double-ingesting it.
func (p *PgStore) IngestStatement(ctx context.Context, tenantID string, req domain.IngestStatementLinesRequest, actorPrincipalID, correlationID string) (*domain.IngestStatementResult, error) {
	result := &domain.IngestStatementResult{}
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		statementID := uuid.New().String()
		var s domain.BankStatement
		row := tx.QueryRow(ctx, `
			INSERT INTO bank_statements (
				statement_id, connection_id, tenant_id, statement_format, statement_date, opening_balance, closing_balance, transaction_count, ingested_at,
				content_hash, source_id, import_batch_id, status, created_by_principal_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now(),$9,$10,$11,'RECEIVED',$12)
			ON CONFLICT (connection_id, content_hash) WHERE content_hash <> '' DO NOTHING
			RETURNING `+statementColumns,
			statementID, req.ConnectionID, tenantID, req.StatementFormat, req.StatementDate, req.OpeningBalance, req.ClosingBalance, len(req.Lines),
			req.ContentHash, req.SourceID, req.ImportBatchID, actorPrincipalID)
		err := scanStatement(row, &s)
		if err == nil {
			result.Created = true
			// One set-based INSERT for the whole immutable line population.
			// The previous per-line INSERT ... RETURNING loop was a true
			// N+1: one network round trip per statement line, inside the
			// ingest transaction.
			lineIDs := make([]string, len(req.Lines))
			statementIDs := make([]string, len(req.Lines))
			tenantIDs := make([]string, len(req.Lines))
			seqs := make([]int32, len(req.Lines))
			postedDates := make([]time.Time, len(req.Lines))
			amounts := make([]float64, len(req.Lines))
			currencies := make([]string, len(req.Lines))
			descriptions := make([]string, len(req.Lines))
			rawReferences := make([]string, len(req.Lines))
			bankCodes := make([]string, len(req.Lines))
			for i, line := range req.Lines {
				lineIDs[i] = uuid.New().String()
				statementIDs[i] = s.StatementID
				tenantIDs[i] = tenantID
				seqs[i] = int32(i + 1)
				postedDates[i] = line.PostedDate
				amounts[i] = line.Amount
				currencies[i] = line.Currency
				descriptions[i] = line.Description
				rawReferences[i] = line.RawReference
				bankCodes[i] = line.BankCode
			}
			rows, err := tx.Query(ctx, `
				WITH input AS (
					SELECT *
					FROM unnest(
						$1::text[], $2::text[], $3::text[], $4::int4[], $5::timestamptz[],
						$6::float8[], $7::text[], $8::text[], $9::text[], $10::text[]
					) AS t(line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency, description, raw_reference, bank_code)
				), inserted AS (
					INSERT INTO bank_statement_lines (line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency, description, raw_reference, bank_code)
					SELECT line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency, description, raw_reference, bank_code
					FROM input
					RETURNING line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency, description, raw_reference, bank_code, created_at
				)
				SELECT line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency, description, raw_reference, bank_code, created_at
				FROM inserted ORDER BY line_seq ASC`,
				lineIDs, statementIDs, tenantIDs, seqs, postedDates, amounts, currencies, descriptions, rawReferences, bankCodes)
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
			if err := rows.Err(); err != nil {
				return err
			}
			result.Statement = s
			return p.enqueueEvent(ctx, tx, "com.zoikosuite.banking.bank-statement.ingested", "banking.statement.ingested",
				"bank-statement", s.StatementID, tenantID, "", actorPrincipalID, correlationID, result)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Conflict: return the original import and its existing lines.
		row = tx.QueryRow(ctx, `SELECT `+statementColumns+` FROM bank_statements WHERE connection_id=$1 AND content_hash=$2`, req.ConnectionID, req.ContentHash)
		if err := scanStatement(row, &s); err != nil {
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

// loadStatementLine reads the immutable raw evidence for one line. Every
// canonical write is derived from this row, not from the request body.
func loadStatementLine(ctx context.Context, tx pgx.Tx, tenantID, lineID string) (*domain.StatementLine, error) {
	var l domain.StatementLine
	err := tx.QueryRow(ctx, `
		SELECT line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency, description, raw_reference, bank_code, created_at
		FROM bank_statement_lines WHERE line_id=$1 AND tenant_id=$2`, lineID, tenantID).
		Scan(&l.LineID, &l.StatementID, &l.TenantID, &l.LineSeq, &l.PostedDate, &l.Amount, &l.Currency, &l.Description, &l.RawReference, &l.BankCode, &l.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrStatementLineNotFound
		}
		return nil, err
	}
	return &l, nil
}

// assertRawEvidence refuses a caller-supplied date/amount/currency that
// contradicts the raw line. The parameters are optional assertions; the
// canonical insert always uses the raw values returned by loadStatementLine.
func assertRawEvidence(line *domain.StatementLine, date *time.Time, amount *float64, currency string) error {
	if date != nil && !date.Equal(line.PostedDate) {
		return domain.ErrTransactionEvidenceMismatch
	}
	if amount != nil && *amount != line.Amount {
		return domain.ErrTransactionEvidenceMismatch
	}
	if currency != "" && !strings.EqualFold(strings.TrimSpace(currency), line.Currency) {
		return domain.ErrTransactionEvidenceMismatch
	}
	return nil
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

// ValidateStatement is BNK-03's balance check: it verifies
// opening_balance + sum(all ingested line amounts) equals closing_balance
// before letting the statement reach a state from which it can be ACCEPTED.
// It is not a full completeness check: it does not compare the declared line
// count, account, currency, period or watermark. A mismatch auto-transitions
// the statement straight to QUARANTINED with a reason and is then returned as
// ErrStatementBalanceMismatch, the same fail-closed posture as every other
// evidence-integrity check in this service, so a caller cannot retry their
// way past a bad statement by calling AcceptStatement directly.
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
		line, err := loadStatementLine(ctx, tx, params.TenantID, params.StatementLineID)
		if err != nil {
			return err
		}
		if err := assertRawEvidence(line, params.TransactionDate, params.Amount, params.Currency); err != nil {
			return err
		}
		bankCode := line.BankCode

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
				// A second normalize attempt on an already-quarantined line
				// reuses the existing exception rather than raising a
				// duplicate -- no new fact, so no new event either.
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
			return p.enqueueEvent(ctx, tx, "com.zoikosuite.banking.mapping-exception.raised", "banking.mapping_exception.raised",
				"mapping-exception", e.ExceptionID, params.TenantID, "", params.ActorPrincipalID, params.CorrelationID, e)
		}

		var t domain.CanonicalTransaction
		row := tx.QueryRow(ctx, `
			INSERT INTO bank_transactions_canonical (transaction_id, tenant_id, statement_line_id, transaction_date, amount, currency, category, counterparty, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING `+canonicalTxnColumns,
			uuid.New().String(), params.TenantID, params.StatementLineID, line.PostedDate, line.Amount, line.Currency, category, params.Counterparty, params.ActorPrincipalID)
		if err := scanCanonicalTxn(row, &t); err != nil {
			return err
		}
		result.Transaction = &t
		return p.enqueueEvent(ctx, tx, "com.zoikosuite.banking.canonical-transaction.normalized", "banking.transaction.normalized",
			"canonical-transaction", t.TransactionID, params.TenantID, "", params.ActorPrincipalID, params.CorrelationID, t)
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
		line, err := loadStatementLine(ctx, tx, params.TenantID, prior.StatementLineID)
		if err != nil {
			return err
		}
		if err := assertRawEvidence(line, params.TransactionDate, params.Amount, params.Currency); err != nil {
			return err
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
			newID, params.TenantID, prior.StatementLineID, prior.MappingVersion+1, line.PostedDate, line.Amount, line.Currency, params.Category, params.Counterparty, params.ActorPrincipalID)
		if err := scanCanonicalTxn(row, &newTxn); err != nil {
			return err
		}
		return p.enqueueEvent(ctx, tx, "com.zoikosuite.banking.canonical-transaction.renormalized", "banking.transaction.renormalized",
			"canonical-transaction", newTxn.TransactionID, params.TenantID, "", params.ActorPrincipalID, params.CorrelationID, newTxn)
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
		if err := row.Scan(&e.ExceptionID, &e.TenantID, &e.StatementLineID, &e.Reason, &e.Status, &e.RaisedByPrincipalID, &resolvedTxnID, &resolvedBy, &resolvedAt, &e.CreatedAt); err != nil {
			return err
		}
		return p.enqueueEvent(ctx, tx, "com.zoikosuite.banking.mapping-exception.raised", "banking.mapping_exception.raised",
			"mapping-exception", e.ExceptionID, params.TenantID, "", params.ActorPrincipalID, params.CorrelationID, e)
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
		line, err := loadStatementLine(ctx, tx, params.TenantID, e.StatementLineID)
		if err != nil {
			return err
		}
		if err := assertRawEvidence(line, params.TransactionDate, params.Amount, params.Currency); err != nil {
			return err
		}

		txnRow := tx.QueryRow(ctx, `
			INSERT INTO bank_transactions_canonical (transaction_id, tenant_id, statement_line_id, transaction_date, amount, currency, category, counterparty, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING `+canonicalTxnColumns,
			uuid.New().String(), params.TenantID, e.StatementLineID, line.PostedDate, line.Amount, line.Currency, params.Category, params.Counterparty, params.ApproverPrincipalID)
		if err := scanCanonicalTxn(txnRow, &t); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE mapping_exceptions SET status='APPROVED', resolved_transaction_id=$2, resolved_by_principal_id=$3, resolved_at=now() WHERE exception_id=$1`,
			e.ExceptionID, t.TransactionID, params.ApproverPrincipalID); err != nil {
			return err
		}
		return p.enqueueEvent(ctx, tx, "com.zoikosuite.banking.mapping-exception.approved", "banking.mapping_exception.approved",
			"mapping-exception", e.ExceptionID, params.TenantID, "", params.ApproverPrincipalID, params.CorrelationID, t)
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
