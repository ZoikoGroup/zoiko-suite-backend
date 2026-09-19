// Package store provides the PostgreSQL implementation of
// bank-reconciliation-svc's persistence layer.
//
// Every write is wrapped in withRLS, which sets app.tenant_id on the
// transaction — the Row-Level Security policy is real and correctly written.
// But every method ALSO filters explicitly by tenant_id in its own SQL,
// rather than relying on RLS alone: this pool connects as a Postgres
// superuser (DB_USER=postgres, same as every other service in this
// platform), and Postgres superusers unconditionally bypass Row-Level
// Security regardless of policy. This was found via genuine CI failures in
// general-ledger-svc and tenant-entity-registry-svc, so this service is
// built with the explicit filter from day one rather than discovering the
// same gap a third time.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// mapPgError translates driver-level errors that are really caller mistakes
// into domain errors, so they can be answered 400 instead of 503.
//
// 22P02 is an invalid text representation (a malformed UUID), 22007/22008 an
// invalid datetime, 22001 a string longer than the column allows. Without
// this every one of them reached the handler as an opaque error and was
// reported as "store unavailable" — telling the caller the database is down
// when the database is fine and the request was wrong. The same defect was
// found in accounts-payable, purchase-request, purchase-order,
// evidence-requirements, general-ledger and financial-close.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "22P02", "22007", "22008", "22001":
			return domain.ErrInvalidIdentifier
		}
	}
	return err
}

func tenantFromCtxOrFallback(ctx context.Context, fallback string) string {
	if t := svcmiddleware.TenantFromContext(ctx); t != "" {
		return t
	}
	return fallback
}

// selectColumns and scanLine are kept adjacent and in the same order on
// purpose: they are one list split in two, and a column added to one but not
// the other is a scan error at runtime rather than a compile error.
const selectColumns = `
	statement_line_id, tenant_id, legal_entity_id, bank_account_id, statement_date,
	amount, currency_code, bank_reference, status,
	matched_journal_id, matched_transaction_id, matched_by_principal_id, matched_at,
	exception_reason, flagged_by_principal_id, flagged_at,
	gl_cash_account_code, correlation_id, created_at,
	proposed_journal_id, proposed_transaction_id, proposed_by_principal_id, proposed_at
`

func scanLine(row interface{ Scan(...any) error }, l *domain.StatementLine) error {
	var status string
	if err := row.Scan(
		&l.StatementLineID, &l.TenantID, &l.LegalEntityID, &l.BankAccountID, &l.StatementDate,
		&l.Amount, &l.CurrencyCode, &l.BankReference, &status,
		&l.MatchedJournalID, &l.MatchedTransactionID, &l.MatchedByPrincipalID, &l.MatchedAt,
		&l.ExceptionReason, &l.FlaggedByPrincipalID, &l.FlaggedAt,
		&l.GLCashAccountCode, &l.CorrelationID, &l.CreatedAt,
		&l.ProposedJournalID, &l.ProposedTransactionID, &l.ProposedByPrincipalID, &l.ProposedAt,
	); err != nil {
		return err
	}
	l.Status = domain.StatementLineStatus(status)
	return nil
}

// CreateStatementLine inserts an ingested bank statement line in UNMATCHED status.
// CreateStatementLine inserts an ingested bank statement line in UNMATCHED
// status.
//
// Idempotent on (tenant_id, correlation_id): a retried call (e.g. a client
// timeout on a POST that actually succeeded server-side) hits the partial
// unique index added in 000002 and resolves to the ORIGINAL line — mutating
// *l in place to reflect it — rather than creating a duplicate. Returns
// created=false when the row already existed.
// The tenant the row is written under is the one resolved here, and the SAME
// value is used for the RLS scope, the tenant_id column, and the idempotency
// lookup. It used to be resolved twice: withRLS got the context's verified
// tenant while the INSERT wrote l.TenantID, which the handler took straight
// from the request body. Because this pool connects as a superuser, RLS does
// not stop the mismatch — so a caller could set the header to its own tenant
// and the body to somebody else's and land a row in a register it has no
// rights to. One value, used everywhere, is the fix.
func (s *PgStore) CreateStatementLine(ctx context.Context, l *domain.StatementLine) (created bool, err error) {
	tenantID := tenantFromCtxOrFallback(ctx, l.TenantID)
	l.TenantID = tenantID

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			INSERT INTO statement_lines (
				statement_line_id, tenant_id, legal_entity_id, bank_account_id, statement_date,
				amount, currency_code, bank_reference, status, gl_cash_account_code,
				correlation_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
		`, l.StatementLineID, tenantID, l.LegalEntityID, l.BankAccountID, l.StatementDate,
			l.Amount, l.CurrencyCode, l.BankReference, string(l.Status), l.GLCashAccountCode,
			l.CorrelationID, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Replay of a prior request with this correlation_id. Re-read the
			// stored row in full — including statement_line_id, which the
			// handler had already generated a fresh uuid for. Returning that
			// unstored id would hand the caller an identifier that 404s on
			// every subsequent call.
			row := tx.QueryRow(ctx, `SELECT `+selectColumns+`
				FROM statement_lines WHERE tenant_id = $1 AND correlation_id = $2
			`, tenantID, l.CorrelationID)
			if err := scanLine(row, l); err != nil {
				return err
			}
			created = false
			return nil
		}
		l.CreatedAt = now
		created = true
		return nil
	})
	return created, mapPgError(err)
}

// GetStatementLine returns (nil, nil) if not found — including when the
// caller's tenant scope doesn't match the line's tenant (explicit filter,
// not RLS-only — see package doc).
//
// A request carrying NO verified tenant scope is an error, not an absence.
// It used to return (nil, nil) too, so the handler answered 404
// statement_line_not_found — telling a caller who had never been scoped that
// the row does not exist, which is both wrong and quietly reassuring.
func (s *PgStore) GetStatementLine(ctx context.Context, statementLineID string) (*domain.StatementLine, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantScopeMissing
	}

	var l domain.StatementLine
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+selectColumns+`
			FROM statement_lines WHERE statement_line_id = $1 AND tenant_id = $2
		`, statementLineID, tenantID)
		return scanLine(row, &l)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return &l, nil
}

// ListStatementLines returns statement lines matching the given filter.
//
// filter.TenantID must be the caller's VERIFIED scope — the handler no longer
// reads it from ?tenant_id. The explicit tenant filter this package's doc
// comment is careful about was, until now, filtering by a value the caller
// supplied, so ?tenant_id=<somebody else> returned their entire bank
// register: every amount, bank reference and reconciliation state.
//
// The result is always non-nil so an empty register encodes as [] and not
// JSON null, and the page is bounded — see domain.DefaultListLimit.
func (s *PgStore) ListStatementLines(ctx context.Context, filter domain.ListStatementLinesFilter) ([]domain.StatementLine, error) {
	if filter.TenantID == "" {
		return nil, domain.ErrTenantScopeMissing
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = domain.DefaultListLimit
	}
	if limit > domain.MaxListLimit {
		limit = domain.MaxListLimit
	}

	out := make([]domain.StatementLine, 0)
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+selectColumns+`
			FROM statement_lines
			WHERE tenant_id = $1
			  AND ($2 = '' OR bank_account_id::text = $2)
			  AND ($3 = '' OR statement_date = $3::date)
			  AND ($4 = '' OR status = $4)
			ORDER BY created_at DESC
			LIMIT $5
		`, filter.TenantID, filter.BankAccountID, filter.StatementDate, filter.Status, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.StatementLine
			if err := scanLine(rows, &l); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return out, nil
}

// MatchStatementLine transitions a line from UNMATCHED or EXCEPTION to
// MATCHED, atomically — the fromStatus check, the transition, and the
// tenant scope are one statement, no separate read, no race window. The
// journal itself must already have been verified against general-ledger-svc
// by the caller (internal/handler) before this is invoked; this method only
// persists the outcome.
func (s *PgStore) MatchStatementLine(ctx context.Context, tenantID, statementLineID, journalID, actorPrincipalID string) error {
	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE statement_lines
			SET status = 'MATCHED', matched_journal_id = $1, matched_by_principal_id = $2, matched_at = $3
			WHERE statement_line_id = $4 AND status IN ('UNMATCHED', 'EXCEPTION') AND tenant_id = $5
		`, journalID, actorPrincipalID, time.Now().UTC(), statementLineID, tenantID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return mapPgError(err)
	}
	if affected == 0 {
		return domain.ErrInvalidTransition
	}
	return nil
}

// FlagException transitions a line from UNMATCHED to EXCEPTION, atomically.
func (s *PgStore) FlagException(ctx context.Context, tenantID, statementLineID, reason, actorPrincipalID string) error {
	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE statement_lines
			SET status = 'EXCEPTION', exception_reason = $1, flagged_by_principal_id = $2, flagged_at = $3
			WHERE statement_line_id = $4 AND status = 'UNMATCHED' AND tenant_id = $5
		`, reason, actorPrincipalID, time.Now().UTC(), statementLineID, tenantID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return mapPgError(err)
	}
	if affected == 0 {
		return domain.ErrInvalidTransition
	}
	return nil
}

// CountUnmatched returns how many lines are still UNMATCHED for the given
// bank account + statement date — used to decide whether the statement can
// be marked complete. tenantID must be the caller's verified scope.
func (s *PgStore) CountUnmatched(ctx context.Context, tenantID, bankAccountID, statementDate string) (int, error) {
	var count int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM statement_lines
			WHERE tenant_id = $1 AND bank_account_id = $2::uuid AND statement_date = $3::date AND status = 'UNMATCHED'
		`, tenantID, bankAccountID, statementDate).Scan(&count)
	})
	if err != nil {
		return 0, mapPgError(err)
	}
	return count, nil
}

// CountMatched returns how many lines are MATCHED for the given bank
// account + statement date — CompleteStatement's certificate records this
// as matched_line_count.
func (s *PgStore) CountMatched(ctx context.Context, tenantID, bankAccountID, statementDate string) (int, error) {
	var count int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM statement_lines
			WHERE tenant_id = $1 AND bank_account_id = $2::uuid AND statement_date = $3::date AND status = 'MATCHED'
		`, tenantID, bankAccountID, statementDate).Scan(&count)
	})
	if err != nil {
		return 0, mapPgError(err)
	}
	return count, nil
}

// StatementLegalEntities returns the distinct legal entities the lines for
// this bank account + statement date belong to.
//
// CompleteStatement authorizes against a legal_entity_id taken from the query
// string, and nothing previously connected that value to the bank account
// being completed. So a caller could name an entity it legitimately holds
// BANKREC_COMPLETE_STATEMENT over, and then complete — and publish
// reconciliation.completed for — a bank account belonging to a different
// entity entirely. An authorization check that is not bound to the resource
// it guards is decoration.
//
// Returned as a set rather than a single value because nothing in the schema
// constrains a bank account to one legal entity; if the data disagrees with
// that assumption the handler must see it rather than silently read the first
// row.
func (s *PgStore) StatementLegalEntities(ctx context.Context, tenantID, bankAccountID, statementDate string) ([]string, error) {
	out := make([]string, 0, 1)
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT legal_entity_id::text FROM statement_lines
			WHERE tenant_id = $1 AND bank_account_id = $2::uuid AND statement_date = $3::date
		`, tenantID, bankAccountID, statementDate)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return out, nil
}

// ProposeMatch is the maker-checker manual-match path's "maker" step:
// UNMATCHED/EXCEPTION -> PENDING_CONFIRMATION, recording who proposed
// which journal. It does not touch the matched_* columns at all — those
// are only ever written by ConfirmMatch.
func (s *PgStore) ProposeMatch(ctx context.Context, tenantID, statementLineID, journalID, proposedByPrincipalID string) error {
	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE statement_lines
			SET status = 'PENDING_CONFIRMATION', proposed_journal_id = $1, proposed_by_principal_id = $2, proposed_at = $3
			WHERE statement_line_id = $4 AND status IN ('UNMATCHED', 'EXCEPTION') AND tenant_id = $5
		`, journalID, proposedByPrincipalID, time.Now().UTC(), statementLineID, tenantID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return mapPgError(err)
	}
	if affected == 0 {
		return domain.ErrInvalidTransition
	}
	return nil
}

// ConfirmMatch is the "checker" step: PENDING_CONFIRMATION -> MATCHED,
// only if confirmingPrincipalID differs from the principal who proposed
// it — fetched and compared before the CAS update runs, the same
// fetch-then-compare self-approval idiom used everywhere else in this
// codebase's maker-checker flows.
func (s *PgStore) ConfirmMatch(ctx context.Context, tenantID, statementLineID, confirmingPrincipalID string) (*domain.StatementLine, error) {
	var l domain.StatementLine
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+selectColumns+`
			FROM statement_lines WHERE statement_line_id = $1 AND tenant_id = $2 FOR UPDATE
		`, statementLineID, tenantID)
		if err := scanLine(row, &l); err != nil {
			return err
		}
		if l.Status != domain.StatementLineStatusPendingConfirmation {
			return domain.ErrInvalidTransition
		}
		if l.ProposedByPrincipalID != nil && *l.ProposedByPrincipalID == confirmingPrincipalID {
			return domain.ErrMatchSelfConfirmation
		}
		journalID := ""
		if l.ProposedJournalID != nil {
			journalID = *l.ProposedJournalID
		}
		var matchedTxnID *string
		if l.ProposedTransactionID != nil {
			matchedTxnID = l.ProposedTransactionID
		}
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			UPDATE statement_lines
			SET status = 'MATCHED', matched_journal_id = NULLIF($1, '')::uuid, matched_transaction_id = $2, matched_by_principal_id = $3, matched_at = $4
			WHERE statement_line_id = $5 AND status = 'PENDING_CONFIRMATION' AND tenant_id = $6
		`, journalID, matchedTxnID, confirmingPrincipalID, now, statementLineID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidTransition
		}
		l.Status = domain.StatementLineStatusMatched
		if journalID != "" {
			l.MatchedJournalID = &journalID
		}
		l.MatchedTransactionID = matchedTxnID
		l.MatchedByPrincipalID = &confirmingPrincipalID
		l.MatchedAt = &now
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrStatementLineNotFound
		}
		if errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrMatchSelfConfirmation) {
			return nil, err
		}
		return nil, mapPgError(err)
	}
	return &l, nil
}

// RejectProposedMatch lets a checker send a proposed match back for
// correction rather than confirming it — PENDING_CONFIRMATION ->
// EXCEPTION, with the rejection reason recorded in exception_reason so
// it becomes a normal queue item again.
func (s *PgStore) RejectProposedMatch(ctx context.Context, tenantID, statementLineID, reason, actorPrincipalID string) error {
	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE statement_lines
			SET status = 'EXCEPTION', exception_reason = $1, flagged_by_principal_id = $2, flagged_at = $3,
				proposed_journal_id = NULL, proposed_transaction_id = NULL, proposed_by_principal_id = NULL, proposed_at = NULL
			WHERE statement_line_id = $4 AND status = 'PENDING_CONFIRMATION' AND tenant_id = $5
		`, reason, actorPrincipalID, time.Now().UTC(), statementLineID, tenantID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return mapPgError(err)
	}
	if affected == 0 {
		return domain.ErrInvalidTransition
	}
	return nil
}

// CertifyStatement is CompleteStatement's real persisted evidence step.
// Idempotent on (tenant_id, bank_account_id, statement_date): a repeat
// call returns the ORIGINAL certificate rather than creating a second
// one for the same statement.
func (s *PgStore) CertifyStatement(ctx context.Context, tenantID, legalEntityID, bankAccountID, statementDate, certifiedByPrincipalID, correlationID string, matchedLineCount int) (*domain.ReconciliationCertificate, bool, error) {
	created := false
	var c domain.ReconciliationCertificate
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO reconciliation_certificates (
				certificate_id, tenant_id, legal_entity_id, bank_account_id, statement_date,
				matched_line_count, certified_by_principal_id, correlation_id
			) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (tenant_id, bank_account_id, statement_date) WHERE run_id IS NULL DO NOTHING
			RETURNING certificate_id, tenant_id, legal_entity_id, bank_account_id, statement_date,
				matched_line_count, certified_by_principal_id, certified_at, correlation_id
		`, tenantID, legalEntityID, bankAccountID, statementDate, matchedLineCount, certifiedByPrincipalID, correlationID)
		var stmtDate time.Time
		err := row.Scan(&c.CertificateID, &c.TenantID, &c.LegalEntityID, &c.BankAccountID, &stmtDate,
			&c.MatchedLineCount, &c.CertifiedByPrincipalID, &c.CertifiedAt, &c.CorrelationID)
		if err == nil {
			c.StatementDate = stmtDate.Format("2006-01-02")
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		row = tx.QueryRow(ctx, `
			SELECT certificate_id, tenant_id, legal_entity_id, bank_account_id, statement_date,
				matched_line_count, certified_by_principal_id, certified_at, correlation_id
			FROM reconciliation_certificates WHERE tenant_id = $1 AND bank_account_id = $2 AND statement_date = $3
		`, tenantID, bankAccountID, statementDate)
		if err := row.Scan(&c.CertificateID, &c.TenantID, &c.LegalEntityID, &c.BankAccountID, &stmtDate,
			&c.MatchedLineCount, &c.CertifiedByPrincipalID, &c.CertifiedAt, &c.CorrelationID); err != nil {
			return err
		}
		c.StatementDate = stmtDate.Format("2006-01-02")
		return nil
	})
	if err != nil {
		return nil, false, mapPgError(err)
	}
	return &c, created, nil
}

func (s *PgStore) MatchStatementLineWithCanonical(ctx context.Context, tenantID, statementLineID, transactionID, actorPrincipalID string) error {
	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE statement_lines
			SET status = 'MATCHED', matched_transaction_id = $1, matched_by_principal_id = $2, matched_at = $3
			WHERE statement_line_id = $4 AND status IN ('UNMATCHED', 'EXCEPTION') AND tenant_id = $5
		`, transactionID, actorPrincipalID, time.Now().UTC(), statementLineID, tenantID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return mapPgError(err)
	}
	if affected == 0 {
		return domain.ErrInvalidTransition
	}
	return nil
}

func (s *PgStore) ProposeMatchWithCanonical(ctx context.Context, tenantID, statementLineID, transactionID, proposedByPrincipalID string) error {
	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE statement_lines
			SET status = 'PENDING_CONFIRMATION', proposed_transaction_id = $1, proposed_by_principal_id = $2, proposed_at = $3
			WHERE statement_line_id = $4 AND status IN ('UNMATCHED', 'EXCEPTION') AND tenant_id = $5
		`, transactionID, proposedByPrincipalID, time.Now().UTC(), statementLineID, tenantID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return mapPgError(err)
	}
	if affected == 0 {
		return domain.ErrInvalidTransition
	}
	return nil
}
