// Package store provides the PostgreSQL implementation of general-ledger-svc's
// persistence layer.
//
// Every write is wrapped in withRLS, which sets app.tenant_id on the
// transaction before running any query — the Row-Level Security policies in
// deployments/migrations/000001_initial_schema.up.sql are real and correctly
// written. But every method ALSO filters explicitly by tenant_id in its own
// SQL, rather than relying on RLS alone: this pool connects as a Postgres
// superuser (DB_USER=postgres, same as every other service in this
// platform), and Postgres superusers unconditionally bypass Row-Level
// Security regardless of policy — see
// https://www.postgresql.org/docs/current/ddl-rowsecurity.html ("the default
// deny policy is not enforced ... for superuser roles"). Found via a genuine
// CI failure (TestPgStore_RLS_TenantIsolation caught real cross-tenant
// leakage on GetJournal, which had no explicit tenant_id filter), not a
// theoretical concern. The explicit filters here are the actual isolation
// guarantee; RLS is defense-in-depth for the day this connects as a
// non-superuser role instead.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// DefaultListLimit bounds ListJournals when the caller names no limit. A
// general ledger is the one table in this platform guaranteed to grow without
// end, and an unbounded SELECT over it is a slow read that gets slower every
// day it runs — the register that reads it only ever renders a page at a time.
const DefaultListLimit = 200

// MaxListLimit caps what a caller may ask for, so a hand-written limit cannot
// reintroduce the unbounded read this replaced.
const MaxListLimit = 1000

// journalHeaderColumns is the header SELECT list. It is a single const, and
// scanHeaderTargets below is derived from it in the same order, because the
// two drifting apart is the failure mode that hand-written pairs invite: this
// service has four read paths over the same eighteen columns.
const journalHeaderColumns = `
	journal_id, tenant_id, legal_entity_id, fiscal_period, status,
	reversal_of_journal_id, description, created_by_principal_id,
	validated_by_principal_id, posted_by_principal_id, reversed_by_principal_id,
	correlation_id, created_at, validated_at, posted_at, reversed_at,
	source_event_id, governance_decision_id`

// scanHeaderTargets returns scan destinations matching journalHeaderColumns,
// column for column. status is scanned into a plain string and converted by
// the caller — domain.JournalStatus has no sql.Scanner.
func scanHeaderTargets(h *domain.JournalHeader, status *string) []any {
	return []any{
		&h.JournalID, &h.TenantID, &h.LegalEntityID, &h.FiscalPeriod, status,
		&h.ReversalOfJournalID, &h.Description, &h.CreatedByPrincipalID,
		&h.ValidatedByPrincipalID, &h.PostedByPrincipalID, &h.ReversedByPrincipalID,
		&h.CorrelationID, &h.CreatedAt, &h.ValidatedAt, &h.PostedAt, &h.ReversedAt,
		&h.SourceEventID, &h.GovernanceDecisionID,
	}
}

const journalLineColumns = `
	journal_line_id, journal_id, line_number, account_code,
	debit_amount, credit_amount, COALESCE(description, ''),
	tax_code, tax_logic_snapshot_id`

func scanLineTargets(l *domain.JournalLine) []any {
	return []any{
		&l.JournalLineID, &l.JournalID, &l.LineNumber, &l.AccountCode,
		&l.DebitAmount, &l.CreditAmount, &l.Description,
		&l.TaxCode, &l.TaxLogicSnapshotID,
	}
}

// mapPgError translates the Postgres failures that are really caller mistakes
// into domain errors, so they stop arriving at the handler as "the store is
// unavailable".
//
// journal_id, tenant_id and legal_entity_id are all uuid columns. A mistyped
// id compared against one of them dies inside the driver as SQLSTATE 22P02
// before any row is examined, and used to reach the handler as a generic
// error and answer 503 — a status that sends an operator to look at
// infrastructure over what is a typo in a URL. Same fix, same reasoning, as
// accounts-payable-svc, purchase-request-svc, purchase-order-svc and
// evidence-requirements-svc.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	if pgErr.Code == "22P02" {
		// invalid_text_representation — not a UUID at all.
		return domain.ErrInvalidIdentifier
	}
	return err
}

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

func tenantFromCtxOrFallback(ctx context.Context, fallback string) string {
	if t := svcmiddleware.TenantFromContext(ctx); t != "" {
		return t
	}
	return fallback
}

// insertJournal writes one journal header and its lines inside an existing
// transaction, returning created=false if this (tenant_id, correlation_id)
// already produced a journal.
//
// Every nullable header column is written here, not just the ones an ordinary
// PENDING create happens to populate. That was the defect this replaced: the
// INSERT named eleven columns and omitted reversal_of_journal_id and the
// posted_* pair, so a reversing journal — which is born FINALIZED, posted by
// its creator, and pointing at the journal it reverses — silently lost all
// three. The link that makes a reversal traceable to its original was
// generated in memory, returned in the POST response, and then dropped on the
// floor; a GET of the same journal a second later disagreed with the response
// the caller had just been handed.
//
// On conflict h is resolved to the FULL stored header, not just its id: a
// retry must answer with the journal that exists, and the caller's own
// unstored description/status would otherwise be echoed back attached to a
// stranger's journal_id.
func insertJournal(ctx context.Context, tx pgx.Tx, tenantID string, h *domain.JournalHeader, lines []domain.JournalLine) (resultLines []domain.JournalLine, created bool, err error) {
	now := time.Now().UTC()
	if h.CreatedAt.IsZero() {
		h.CreatedAt = now
	}
	// A journal born FINALIZED — only a reversal is — is posted at the moment
	// it is written. Stamped here so the invariant "no FINALIZED row has a null
	// posted_at" holds at the one place FINALIZED rows are created, and so the
	// timestamp in the response is the one in the table.
	if h.Status == domain.JournalStatusFinalized && h.PostedAt == nil {
		postedAt := now
		h.PostedAt = &postedAt
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO journal_headers (
			journal_id, tenant_id, legal_entity_id, fiscal_period, status,
			reversal_of_journal_id, description, created_by_principal_id,
			validated_by_principal_id, posted_by_principal_id, reversed_by_principal_id,
			correlation_id, created_at, validated_at, posted_at, reversed_at,
			source_event_id, governance_decision_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
	`, h.JournalID, tenantID, h.LegalEntityID, h.FiscalPeriod, string(h.Status),
		h.ReversalOfJournalID, h.Description, h.CreatedByPrincipalID,
		h.ValidatedByPrincipalID, h.PostedByPrincipalID, h.ReversedByPrincipalID,
		h.CorrelationID, h.CreatedAt, h.ValidatedAt, h.PostedAt, h.ReversedAt,
		h.SourceEventID, h.GovernanceDecisionID)
	if err != nil {
		return nil, false, mapPgError(err)
	}

	if tag.RowsAffected() == 0 {
		// Conflict: an earlier call with this correlation_id already created a
		// journal. Resolve h to that journal in full rather than inserting a
		// duplicate.
		var status string
		row := tx.QueryRow(ctx, `
			SELECT `+journalHeaderColumns+`
			FROM journal_headers WHERE tenant_id = $1 AND correlation_id = $2
		`, tenantID, h.CorrelationID)
		if err := row.Scan(scanHeaderTargets(h, &status)...); err != nil {
			return nil, false, mapPgError(err)
		}
		h.Status = domain.JournalStatus(status)

		existing, err := queryLines(ctx, tx, tenantID, h.JournalID)
		if err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}

	h.TenantID = tenantID
	for i := range lines {
		lines[i].JournalLineID = uuid.NewString()
		lines[i].JournalID = h.JournalID
		lines[i].LineNumber = i + 1
		if _, err := tx.Exec(ctx, `
			INSERT INTO journal_lines (
				journal_line_id, journal_id, tenant_id, line_number,
				account_code, debit_amount, credit_amount, description,
				tax_code, tax_logic_snapshot_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, lines[i].JournalLineID, h.JournalID, tenantID, lines[i].LineNumber,
			lines[i].AccountCode, lines[i].DebitAmount, lines[i].CreditAmount, lines[i].Description,
			lines[i].TaxCode, lines[i].TaxLogicSnapshotID); err != nil {
			return nil, false, mapPgError(err)
		}
	}
	return lines, true, nil
}

func queryLines(ctx context.Context, tx pgx.Tx, tenantID, journalID string) ([]domain.JournalLine, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+journalLineColumns+`
		FROM journal_lines WHERE journal_id = $1 AND tenant_id = $2 ORDER BY line_number ASC
	`, journalID, tenantID)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()

	var lines []domain.JournalLine
	for rows.Next() {
		var l domain.JournalLine
		if err := rows.Scan(scanLineTargets(&l)...); err != nil {
			return nil, mapPgError(err)
		}
		lines = append(lines, l)
	}
	return lines, mapPgError(rows.Err())
}

// CreateJournal inserts a journal header (status PENDING) and all of its
// lines in a single transaction. Balance validation (sum debits == sum
// credits) happens at ValidateJournal, not here — PENDING is deliberately
// allowed to be unbalanced, matching the Tri-Phase Commit spec's intent that
// Pending is a draft state.
//
// The row is written under the tenant this request is scoped to, which is the
// verified X-Tenant-Id when there is one — NOT the tenant_id in the request
// body. The two used to be able to disagree: withRLS set app.tenant_id from
// the context while the INSERT wrote h.TenantID from the body, so a caller
// verified as tenant A could file a journal owned by tenant B. Under a
// superuser connection RLS does not stop that (see the package comment), and
// the row was then invisible to the tenant who created it and live in a ledger
// they had no relationship with. The handler additionally refuses the mismatch
// outright; this is the second lock on the same door.
//
// Idempotent on (tenant_id, correlation_id): a retried call with the same
// correlation_id resolves h to the original journal and returns its actual
// lines (created=false) instead of inserting a duplicate — a client retry
// after a network timeout must not double-post a journal. The returned
// lines slice reflects whichever journal (new or pre-existing) the call
// resolved to; it is not necessarily the same length as the input lines.
func (s *PgStore) CreateJournal(ctx context.Context, h *domain.JournalHeader, lines []domain.JournalLine) (resultLines []domain.JournalLine, created bool, err error) {
	tenantID := tenantFromCtxOrFallback(ctx, h.TenantID)

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var innerErr error
		resultLines, created, innerErr = insertJournal(ctx, tx, tenantID, h, lines)
		return innerErr
	})
	if err != nil {
		return nil, false, err
	}
	return resultLines, created, nil
}

// ReverseJournal creates the reversing journal AND marks the original REVERSED
// in one transaction.
//
// These were two independent calls — CreateJournal, then TransitionJournal —
// and the gap between them is a double-counted ledger. The reversing journal
// is born FINALIZED, so if the transition that follows it failed (a dropped
// connection, a pod evicted mid-request, or simply the original no longer
// being FINALIZED), the books were left holding both the original posting and
// its inverse as live, final entries, with the original still reversible
// again. Nothing in the service ever reconciled that state, and no error the
// caller saw distinguished it from the reversal having been refused outright.
//
// Committing both in one transaction makes the outcome binary: either the
// original is REVERSED and its inverse exists, or neither happened. The UPDATE
// keeps the atomic WHERE status = 'FINALIZED' guard, so a concurrent second
// reversal loses the race and rolls back rather than posting an orphan.
//
// created=false means this correlation_id already reversed something — a
// retry. reversing is then resolved to the stored reversing journal and the
// original is left exactly as it is (already REVERSED), rather than reporting
// a successful retry as an invalid transition.
func (s *PgStore) ReverseJournal(
	ctx context.Context,
	tenantID, originalJournalID string,
	reversing *domain.JournalHeader,
	reversingLines []domain.JournalLine,
	actorPrincipalID string,
) (resultLines []domain.JournalLine, created bool, err error) {
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var innerErr error
		resultLines, created, innerErr = insertJournal(ctx, tx, tenantID, reversing, reversingLines)
		if innerErr != nil {
			return innerErr
		}
		if !created {
			return nil
		}

		tag, err := tx.Exec(ctx, `
			UPDATE journal_headers
			SET status = $1, reversed_by_principal_id = $2, reversed_at = $3
			WHERE journal_id = $4 AND status = $5 AND tenant_id = $6
		`, string(domain.JournalStatusReversed), actorPrincipalID, time.Now().UTC(),
			originalJournalID, string(domain.JournalStatusFinalized), tenantID)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			// The original is not FINALIZED any more. Returning an error rolls
			// the transaction back, taking the reversing journal with it.
			return domain.ErrInvalidTransition
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return resultLines, created, nil
}

// GetJournal returns a journal header plus its lines. Returns (nil, nil, nil)
// if not found — including when the caller's tenant scope doesn't match the
// journal's tenant.
//
// The tenant_id column is filtered explicitly here, not left to RLS alone:
// the pool connects as a Postgres superuser (same posture as every other
// service in this platform), and Postgres superusers unconditionally bypass
// Row-Level Security regardless of policy — RLS alone provides no real
// isolation guarantee under this connection. Found via a genuine CI failure
// (TestPgStore_RLS_TenantIsolation caught real cross-tenant leakage), not
// theoretical.
func (s *PgStore) GetJournal(ctx context.Context, journalID string) (*domain.JournalHeader, []domain.JournalLine, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil, nil
	}

	var h domain.JournalHeader
	var status string
	var lines []domain.JournalLine

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+journalHeaderColumns+`
			FROM journal_headers WHERE journal_id = $1 AND tenant_id = $2
		`, journalID, tenantID)
		if err := row.Scan(scanHeaderTargets(&h, &status)...); err != nil {
			return mapPgError(err)
		}
		h.Status = domain.JournalStatus(status)

		// Read in the same transaction as the header. Two transactions could
		// see a journal's header from before a write and its lines from after.
		var err error
		lines, err = queryLines(ctx, tx, tenantID, journalID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		// Not a UUID, so it cannot name a journal that exists. Absent, not
		// broken — the same answer an unknown id gets, which is also what
		// another tenant's journal gets.
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &h, lines, nil
}

// GetJournalByCorrelationID resolves an idempotency key to the journal it
// created, within one tenant. Returns (nil, nil, nil) when the key has not been
// used. Backed by the same partial unique index that makes creation idempotent,
// so at most one row can match.
func (s *PgStore) GetJournalByCorrelationID(ctx context.Context, tenantID, correlationID string) (*domain.JournalHeader, []domain.JournalLine, error) {
	if tenantID == "" || correlationID == "" {
		return nil, nil, nil
	}

	var h domain.JournalHeader
	var status string
	var lines []domain.JournalLine

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+journalHeaderColumns+`
			FROM journal_headers WHERE tenant_id = $1 AND correlation_id = $2
		`, tenantID, correlationID)
		if err := row.Scan(scanHeaderTargets(&h, &status)...); err != nil {
			return mapPgError(err)
		}
		h.Status = domain.JournalStatus(status)

		var err error
		lines, err = queryLines(ctx, tx, tenantID, h.JournalID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &h, lines, nil
}

// ListJournals returns journal headers matching the given filter (tenant_id
// is required; the others are optional), newest first, bounded by
// filter.Limit.
func (s *PgStore) ListJournals(ctx context.Context, filter domain.ListJournalsFilter) ([]domain.JournalHeader, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	var out []domain.JournalHeader
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+journalHeaderColumns+`
			FROM journal_headers
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR fiscal_period = $3)
			  AND ($4 = '' OR status = $4)
			ORDER BY created_at DESC, journal_id DESC
			LIMIT $5
		`, filter.TenantID, filter.LegalEntityID, filter.FiscalPeriod, filter.Status, limit)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var h domain.JournalHeader
			var status string
			if err := rows.Scan(scanHeaderTargets(&h, &status)...); err != nil {
				return mapPgError(err)
			}
			h.Status = domain.JournalStatus(status)
			out = append(out, h)
		}
		return mapPgError(rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TransitionJournal atomically moves a journal from fromStatus to toStatus,
// stamping the actor and timestamp column appropriate to toStatus. Uses
// WHERE status = $fromStatus so the transition and the state-machine check
// are one atomic UPDATE — no separate read, no race window (same pattern as
// tenant-entity-registry-svc's TransitionEntityStatus). Returns
// domain.ErrInvalidTransition if zero rows were affected (either the journal
// doesn't exist or wasn't in fromStatus).
//
// REVERSED is not reachable through here — a reversal must also create the
// reversing journal, and the two are one transaction; see ReverseJournal.
func (s *PgStore) TransitionJournal(ctx context.Context, tenantID, journalID string, fromStatus, toStatus domain.JournalStatus, actorPrincipalID string) error {
	actorColumn, timeColumn := transitionColumns(toStatus)
	query := fmt.Sprintf(`
		UPDATE journal_headers
		SET status = $1, %s = $2, %s = $3
		WHERE journal_id = $4 AND status = $5 AND tenant_id = $6
	`, actorColumn, timeColumn)

	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, query, string(toStatus), actorPrincipalID, time.Now().UTC(), journalID, string(fromStatus), tenantID)
		if err != nil {
			return mapPgError(err)
		}
		affected = tag.RowsAffected()
		return nil
	})
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		// A malformed journal_id names nothing, so nothing moved. Reported as a
		// refused transition rather than a dead store, and never as success.
		return domain.ErrInvalidTransition
	}
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.ErrInvalidTransition
	}
	return nil
}

func transitionColumns(to domain.JournalStatus) (actorColumn, timeColumn string) {
	switch to {
	case domain.JournalStatusValidated:
		return "validated_by_principal_id", "validated_at"
	case domain.JournalStatusFinalized:
		return "posted_by_principal_id", "posted_at"
	case domain.JournalStatusReversed:
		return "reversed_by_principal_id", "reversed_at"
	default:
		return "posted_by_principal_id", "posted_at"
	}
}

// SumLines returns the total debit and credit amounts for a journal's lines —
// used by the service layer to enforce the double-entry balance invariant
// before allowing a PENDING -> VALIDATED transition.
//
// Both sums are computed by Postgres over NUMERIC(18,2) columns and returned
// as exact minor units (cents), not as float64. Comparing two float64 sums for
// equality is the classic way to reject a journal that balances perfectly well
// in decimal, and the caller's decision here is exact equality.
func (s *PgStore) SumLines(ctx context.Context, tenantID, journalID string) (debitTotal, creditTotal int64, err error) {
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT (COALESCE(SUM(debit_amount), 0) * 100)::bigint,
			       (COALESCE(SUM(credit_amount), 0) * 100)::bigint
			FROM journal_lines WHERE journal_id = $1 AND tenant_id = $2
		`, journalID, tenantID)
		return mapPgError(row.Scan(&debitTotal, &creditTotal))
	})
	if err != nil {
		return 0, 0, err
	}
	return debitTotal, creditTotal, nil
}

// CompileTrialBalance is ACC-15's real, durable trial-balance capability —
// see migration 000006's doc comment. It computes the watermark (the
// highest journal_seq among the FINALIZED/REVERSED journals actually
// included) and each account's net balance in ONE query each, inside the
// same transaction the snapshot is written in — never N+1 per-journal
// fetches, and never a page-size limit to be truncated by (unlike
// ListJournals, which exists for a paginated UI, this reads everything
// that matches directly from the store it already owns).
func (s *PgStore) CompileTrialBalance(ctx context.Context, tenantID, legalEntityID, fiscalPeriod, principalID string) (*domain.TrialBalanceSnapshot, error) {
	snap := &domain.TrialBalanceSnapshot{
		TrialBalanceSnapshotID: uuid.NewString(),
		TenantID:               tenantID,
		LegalEntityID:          legalEntityID,
		FiscalPeriod:           fiscalPeriod,
		CompiledAt:             time.Now().UTC(),
		CompiledByPrincipalID:  principalID,
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(journal_seq), 0)
			FROM journal_headers
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_period = $3
			  AND status IN ('FINALIZED', 'REVERSED')
		`, tenantID, legalEntityID, fiscalPeriod).Scan(&snap.LedgerWatermark); err != nil {
			return mapPgError(err)
		}

		rows, err := tx.Query(ctx, `
			SELECT jl.account_code, SUM(jl.debit_amount - jl.credit_amount) AS net_balance
			FROM journal_lines jl
			JOIN journal_headers jh ON jh.journal_id = jl.journal_id
			WHERE jh.tenant_id = $1 AND jh.legal_entity_id = $2 AND jh.fiscal_period = $3
			  AND jh.status IN ('FINALIZED', 'REVERSED') AND jl.tenant_id = $1
			GROUP BY jl.account_code
			ORDER BY jl.account_code ASC
		`, tenantID, legalEntityID, fiscalPeriod)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var line domain.TrialBalanceLine
			if err := rows.Scan(&line.AccountCode, &line.NetBalance); err != nil {
				return mapPgError(err)
			}
			snap.Lines = append(snap.Lines, line)
		}
		if err := mapPgError(rows.Err()); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO trial_balance_snapshots (
				trial_balance_snapshot_id, tenant_id, legal_entity_id, fiscal_period,
				ledger_watermark, compiled_at, compiled_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, snap.TrialBalanceSnapshotID, tenantID, legalEntityID, fiscalPeriod,
			snap.LedgerWatermark, snap.CompiledAt, principalID); err != nil {
			return mapPgError(err)
		}

		for _, line := range snap.Lines {
			if _, err := tx.Exec(ctx, `
				INSERT INTO trial_balance_lines (trial_balance_snapshot_id, tenant_id, account_code, net_balance)
				VALUES ($1, $2, $3, $4)
			`, snap.TrialBalanceSnapshotID, tenantID, line.AccountCode, line.NetBalance); err != nil {
				return mapPgError(err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// GetTrialBalance returns one previously-compiled snapshot by id, with its
// lines, or domain.ErrTrialBalanceNotFound.
func (s *PgStore) GetTrialBalance(ctx context.Context, tenantID, snapshotID string) (*domain.TrialBalanceSnapshot, error) {
	var snap domain.TrialBalanceSnapshot
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT trial_balance_snapshot_id, tenant_id, legal_entity_id, fiscal_period,
			       ledger_watermark, compiled_at, compiled_by_principal_id
			FROM trial_balance_snapshots
			WHERE trial_balance_snapshot_id = $1 AND tenant_id = $2
		`, snapshotID, tenantID).Scan(
			&snap.TrialBalanceSnapshotID, &snap.TenantID, &snap.LegalEntityID, &snap.FiscalPeriod,
			&snap.LedgerWatermark, &snap.CompiledAt, &snap.CompiledByPrincipalID,
		); err != nil {
			return mapPgError(err)
		}

		rows, err := tx.Query(ctx, `
			SELECT account_code, net_balance FROM trial_balance_lines
			WHERE trial_balance_snapshot_id = $1 AND tenant_id = $2
			ORDER BY account_code ASC
		`, snapshotID, tenantID)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var line domain.TrialBalanceLine
			if err := rows.Scan(&line.AccountCode, &line.NetBalance); err != nil {
				return mapPgError(err)
			}
			snap.Lines = append(snap.Lines, line)
		}
		return mapPgError(rows.Err())
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTrialBalanceNotFound
	}
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// ── ACC-01 Chart of Accounts ─────────────────────────────────────────────────

const accountColumns = `account_id, tenant_id, account_code, account_name, account_type,
	parent_account_id, is_control_account, direct_posting_restricted, status,
	created_at, created_by_principal_id`

func scanAccount(row pgx.Row) (*domain.Account, error) {
	var a domain.Account
	var accountType, status string
	if err := row.Scan(
		&a.AccountID, &a.TenantID, &a.AccountCode, &a.AccountName, &accountType,
		&a.ParentAccountID, &a.IsControlAccount, &a.DirectPostingRestricted, &status,
		&a.CreatedAt, &a.CreatedByPrincipalID,
	); err != nil {
		return nil, err
	}
	a.AccountType = domain.AccountType(accountType)
	a.Status = status
	return &a, nil
}

func (s *PgStore) CreateAccount(ctx context.Context, a *domain.Account) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// The parent, if named, must actually exist in this tenant's chart
		// — a dangling parent_account_id would make the hierarchy this
		// invariant exists to model unreliable from the moment it's created.
		if a.ParentAccountID != nil {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chart_of_accounts WHERE account_id = $1 AND tenant_id = $2)`,
				*a.ParentAccountID, tenantID).Scan(&exists); err != nil {
				return mapPgError(err)
			}
			if !exists {
				return domain.ErrParentAccountNotFound
			}
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO chart_of_accounts (
				account_id, tenant_id, account_code, account_name, account_type,
				parent_account_id, is_control_account, direct_posting_restricted, status,
				created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, a.AccountID, tenantID, a.AccountCode, a.AccountName, string(a.AccountType),
			a.ParentAccountID, a.IsControlAccount, a.DirectPostingRestricted, a.Status,
			a.CreatedAt, a.CreatedByPrincipalID)
		return mapPgError(err)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.ErrAccountAlreadyExists
		}
		return err
	}
	return nil
}

func (s *PgStore) GetAccountByCode(ctx context.Context, tenantID, accountCode string) (*domain.Account, error) {
	var a *domain.Account
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM chart_of_accounts WHERE tenant_id = $1 AND account_code = $2`,
			tenantID, accountCode)
		var err error
		a, err = scanAccount(row)
		return mapPgError(err)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAccountNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *PgStore) ListAccounts(ctx context.Context, tenantID string) ([]domain.Account, error) {
	var out []domain.Account
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+accountColumns+` FROM chart_of_accounts WHERE tenant_id = $1 ORDER BY account_code ASC`, tenantID)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanAccount(rows)
			if err != nil {
				return mapPgError(err)
			}
			out = append(out, *a)
		}
		return mapPgError(rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── ACC-02 Account Mapping ───────────────────────────────────────────────────

// SetAccountMapping records a new effective-dated mapping for mappingKey,
// atomically superseding any prior current mapping for the same key —
// never a destructive overwrite (migration 000008's doc comment). Fails
// if accountCode does not name an existing ACTIVE account: ACC-02 must
// never map a business concept onto an account that can't be posted to.
func (s *PgStore) SetAccountMapping(ctx context.Context, m *domain.AccountMapping) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM chart_of_accounts WHERE tenant_id = $1 AND account_code = $2`,
			tenantID, m.AccountCode).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrMappingTargetAccountInvalid
		}
		if err != nil {
			return mapPgError(err)
		}
		if status != "ACTIVE" {
			return domain.ErrMappingTargetAccountInvalid
		}

		if _, err := tx.Exec(ctx, `
			UPDATE account_mappings SET effective_to = $1
			WHERE tenant_id = $2 AND mapping_key = $3 AND effective_to IS NULL
		`, m.EffectiveFrom, tenantID, m.MappingKey); err != nil {
			return mapPgError(err)
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO account_mappings (
				account_mapping_id, tenant_id, mapping_key, account_code,
				effective_from, effective_to, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, NULL, $6, $7)
		`, m.AccountMappingID, tenantID, m.MappingKey, m.AccountCode,
			m.EffectiveFrom, m.CreatedAt, m.CreatedByPrincipalID)
		return mapPgError(err)
	})
}

func (s *PgStore) GetCurrentAccountMapping(ctx context.Context, tenantID, mappingKey string) (*domain.AccountMapping, error) {
	var m domain.AccountMapping
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return mapPgError(tx.QueryRow(ctx, `
			SELECT account_mapping_id, tenant_id, mapping_key, account_code,
			       effective_from, effective_to, created_at, created_by_principal_id
			FROM account_mappings
			WHERE tenant_id = $1 AND mapping_key = $2 AND effective_to IS NULL
		`, tenantID, mappingKey).Scan(
			&m.AccountMappingID, &m.TenantID, &m.MappingKey, &m.AccountCode,
			&m.EffectiveFrom, &m.EffectiveTo, &m.CreatedAt, &m.CreatedByPrincipalID,
		))
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAccountMappingNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *PgStore) ListAccountMappings(ctx context.Context, tenantID string) ([]domain.AccountMapping, error) {
	var out []domain.AccountMapping
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT account_mapping_id, tenant_id, mapping_key, account_code,
			       effective_from, effective_to, created_at, created_by_principal_id
			FROM account_mappings
			WHERE tenant_id = $1 AND effective_to IS NULL
			ORDER BY mapping_key ASC
		`, tenantID)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.AccountMapping
			if err := rows.Scan(
				&m.AccountMappingID, &m.TenantID, &m.MappingKey, &m.AccountCode,
				&m.EffectiveFrom, &m.EffectiveTo, &m.CreatedAt, &m.CreatedByPrincipalID,
			); err != nil {
				return mapPgError(err)
			}
			out = append(out, m)
		}
		return mapPgError(rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) DeactivateAccount(ctx context.Context, tenantID, accountCode string) error {
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `UPDATE chart_of_accounts SET status = 'INACTIVE' WHERE tenant_id = $1 AND account_code = $2`,
			tenantID, accountCode)
		if err != nil {
			return mapPgError(err)
		}
		if res.RowsAffected() == 0 {
			return domain.ErrAccountNotFound
		}
		return nil
	})
	return err
}
