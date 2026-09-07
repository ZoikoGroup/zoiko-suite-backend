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
	source_event_id, governance_decision_id,
	journal_type, transaction_date, posting_date, currency_code,
	book_id, reporting_basis, evidence_refs,
	approval_status, approval_fingerprint,
	submitted_at, submitted_by_principal_id,
	approved_at, approved_by_principal_id,
	rejected_at, rejected_by_principal_id, rejection_reason,
	posting_requested_at, posting_requested_by_principal_id,
	correction_of_journal_id`

// scanHeaderTargets returns scan destinations matching journalHeaderColumns,
// column for column. status and approvalStatus are scanned into plain
// strings and converted by the caller — domain.JournalStatus/ApprovalStatus
// have no sql.Scanner.
//
// journal_type does have one (domain.JournalType.Scan), so it scans directly
// rather than adding a second out-parameter to a signature four read paths
// already share.
func scanHeaderTargets(h *domain.JournalHeader, status, approvalStatus *string) []any {
	return []any{
		&h.JournalID, &h.TenantID, &h.LegalEntityID, &h.FiscalPeriod, status,
		&h.ReversalOfJournalID, &h.Description, &h.CreatedByPrincipalID,
		&h.ValidatedByPrincipalID, &h.PostedByPrincipalID, &h.ReversedByPrincipalID,
		&h.CorrelationID, &h.CreatedAt, &h.ValidatedAt, &h.PostedAt, &h.ReversedAt,
		&h.SourceEventID, &h.GovernanceDecisionID,
		&h.JournalType, &h.TransactionDate, &h.PostingDate, &h.CurrencyCode,
		&h.BookID, &h.ReportingBasis, &h.EvidenceRefs,
		approvalStatus, &h.ApprovalFingerprint,
		&h.SubmittedAt, &h.SubmittedByPrincipalID,
		&h.ApprovedAt, &h.ApprovedByPrincipalID,
		&h.RejectedAt, &h.RejectedByPrincipalID, &h.RejectionReason,
		&h.PostingRequestedAt, &h.PostingRequestedByPrincipalID,
		&h.CorrectionOfJournalID,
	}
}

const journalLineColumns = `
	journal_line_id, journal_id, line_number, account_code,
	debit_amount, credit_amount, COALESCE(description, ''),
	tax_code, tax_logic_snapshot_id, dimensions`

func scanLineTargets(l *domain.JournalLine) []any {
	return []any{
		&l.JournalLineID, &l.JournalID, &l.LineNumber, &l.AccountCode,
		&l.DebitAmount, &l.CreditAmount, &l.Description,
		&l.TaxCode, &l.TaxLogicSnapshotID, &l.Dimensions,
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
	// ACC-03's own approval lifecycle defaults to DRAFT — the honest
	// starting state for a proposal nobody has acted on yet. Callers that
	// mean to skip the human workflow set ApprovalStatus explicitly before
	// calling this, so the fallback here never overrides a deliberate
	// choice. A journal born FINALIZED — only a reversal is — is likewise
	// born POSTED: it is itself an authoritative posting the instant it is
	// written, never a draft proposal awaiting approval, matching the
	// PostedAt stamping just above.
	if h.ApprovalStatus == "" {
		if h.Status == domain.JournalStatusFinalized {
			h.ApprovalStatus = domain.ApprovalStatusPosted
		} else {
			h.ApprovalStatus = domain.ApprovalStatusDraft
		}
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO journal_headers (
			journal_id, tenant_id, legal_entity_id, fiscal_period, status,
			reversal_of_journal_id, description, created_by_principal_id,
			validated_by_principal_id, posted_by_principal_id, reversed_by_principal_id,
			correlation_id, created_at, validated_at, posted_at, reversed_at,
			source_event_id, governance_decision_id,
			journal_type, transaction_date, posting_date, currency_code,
			book_id, reporting_basis, evidence_refs, approval_status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
		          $19, $20, $21, $22, $23, $24, $25, $26)
		ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id != '' DO NOTHING
	`, h.JournalID, tenantID, h.LegalEntityID, h.FiscalPeriod, string(h.Status),
		h.ReversalOfJournalID, h.Description, h.CreatedByPrincipalID,
		h.ValidatedByPrincipalID, h.PostedByPrincipalID, h.ReversedByPrincipalID,
		h.CorrelationID, h.CreatedAt, h.ValidatedAt, h.PostedAt, h.ReversedAt,
		h.SourceEventID, h.GovernanceDecisionID,
		h.JournalType, h.TransactionDate, h.PostingDate, h.CurrencyCode,
		h.BookID, h.ReportingBasis, h.EvidenceRefs, string(h.ApprovalStatus))
	if err != nil {
		return nil, false, mapPgError(err)
	}

	if tag.RowsAffected() == 0 {
		// Conflict: an earlier call with this correlation_id already created a
		// journal. Resolve h to that journal in full rather than inserting a
		// duplicate.
		var status, approvalStatus string
		row := tx.QueryRow(ctx, `
			SELECT `+journalHeaderColumns+`
			FROM journal_headers WHERE tenant_id = $1 AND correlation_id = $2
		`, tenantID, h.CorrelationID)
		if err := row.Scan(scanHeaderTargets(h, &status, &approvalStatus)...); err != nil {
			return nil, false, mapPgError(err)
		}
		h.Status = domain.JournalStatus(status)
		h.ApprovalStatus = domain.ApprovalStatus(approvalStatus)

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
				tax_code, tax_logic_snapshot_id, dimensions
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, lines[i].JournalLineID, h.JournalID, tenantID, lines[i].LineNumber,
			lines[i].AccountCode, lines[i].DebitAmount, lines[i].CreditAmount, lines[i].Description,
			lines[i].TaxCode, lines[i].TaxLogicSnapshotID, lines[i].Dimensions); err != nil {
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
	var status, approvalStatus string
	var lines []domain.JournalLine

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+journalHeaderColumns+`
			FROM journal_headers WHERE journal_id = $1 AND tenant_id = $2
		`, journalID, tenantID)
		if err := row.Scan(scanHeaderTargets(&h, &status, &approvalStatus)...); err != nil {
			return mapPgError(err)
		}
		h.Status = domain.JournalStatus(status)
		h.ApprovalStatus = domain.ApprovalStatus(approvalStatus)

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
	var status, approvalStatus string
	var lines []domain.JournalLine

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+journalHeaderColumns+`
			FROM journal_headers WHERE tenant_id = $1 AND correlation_id = $2
		`, tenantID, correlationID)
		if err := row.Scan(scanHeaderTargets(&h, &status, &approvalStatus)...); err != nil {
			return mapPgError(err)
		}
		h.Status = domain.JournalStatus(status)
		h.ApprovalStatus = domain.ApprovalStatus(approvalStatus)

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
			var status, approvalStatus string
			if err := rows.Scan(scanHeaderTargets(&h, &status, &approvalStatus)...); err != nil {
				return mapPgError(err)
			}
			h.Status = domain.JournalStatus(status)
			h.ApprovalStatus = domain.ApprovalStatus(approvalStatus)
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
		if affected > 0 && toStatus == domain.JournalStatusFinalized {
			// ACC-05: every journal that reaches FINALIZED appends its
			// posted ledger entries in the SAME transaction as the status
			// flip — never a second call a future call site could forget,
			// the same bug class already hit twice this session with
			// MarkJournalPosted. See migration 000011's doc comment.
			if err := appendLedgerEntries(ctx, tx, tenantID, journalID); err != nil {
				return err
			}
		}
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

// appendLedgerEntries is ACC-05's AppendPostedJournal — "(internal only)"
// per the spec's own wireframe, so it is never a Store interface method a
// handler can call; it exists purely as TransitionJournal's own side
// effect, run in the same transaction as the PENDING/VALIDATED ->
// FINALIZED flip that calls it. ON CONFLICT DO NOTHING makes it safe to
// call at most once per journal in practice while remaining idempotent in
// principle (the spec's own negative path #2, "duplicate journal append").
func appendLedgerEntries(ctx context.Context, tx pgx.Tx, tenantID, journalID string) error {
	rows, err := tx.Query(ctx, `
		SELECT jl.journal_line_id, jl.line_number, jl.account_code,
		       jl.debit_amount, jl.credit_amount, jl.dimensions,
		       jh.legal_entity_id, COALESCE(jh.book_id, ''), jh.fiscal_period,
		       jh.currency_code, jh.transaction_date, jh.posting_date,
		       jh.source_event_id, jh.correlation_id
		FROM journal_lines jl
		JOIN journal_headers jh ON jh.journal_id = jl.journal_id
		WHERE jl.journal_id = $1 AND jh.tenant_id = $2
	`, journalID, tenantID)
	if err != nil {
		return mapPgError(err)
	}
	type entryRow struct {
		lineID, accountCode, legalEntityID, bookID, fiscalPeriod, currencyCode, correlationID string
		lineNumber                                                                             int
		debit, credit                                                                          float64
		dimensions                                                                              domain.Dimensions
		transactionDate, postingDate                                                            domain.Date
		sourceEventID                                                                           *string
	}
	var entries []entryRow
	for rows.Next() {
		var e entryRow
		if err := rows.Scan(&e.lineID, &e.lineNumber, &e.accountCode, &e.debit, &e.credit, &e.dimensions,
			&e.legalEntityID, &e.bookID, &e.fiscalPeriod, &e.currencyCode, &e.transactionDate, &e.postingDate,
			&e.sourceEventID, &e.correlationID); err != nil {
			rows.Close()
			return mapPgError(err)
		}
		entries = append(entries, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return mapPgError(err)
	}

	now := time.Now().UTC()
	for _, e := range entries {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (
				ledger_entry_id, tenant_id, legal_entity_id, book_id, fiscal_period,
				journal_id, journal_line_id, line_number, account_code,
				debit_amount, credit_amount, currency_code, dimensions,
				transaction_date, posting_date, source_event_id, correlation_id, created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
			ON CONFLICT (tenant_id, journal_id, journal_line_id) DO NOTHING
		`, uuid.NewString(), tenantID, e.legalEntityID, e.bookID, e.fiscalPeriod,
			journalID, e.lineID, e.lineNumber, e.accountCode,
			e.debit, e.credit, e.currencyCode, e.dimensions,
			e.transactionDate, e.postingDate, e.sourceEventID, e.correlationID, now); err != nil {
			return mapPgError(err)
		}
	}
	return nil
}

// QueryLedger is ACC-05's own read authority over ledger_entries. Filter's
// LegalEntityID must already be validated non-empty by the caller
// (ErrLedgerScopeRequired) — the spec's own negative path #4, "cross-book
// query leakage."
func (s *PgStore) QueryLedger(ctx context.Context, tenantID string, filter domain.QueryLedgerFilter, limit int) ([]domain.LedgerEntry, error) {
	if limit <= 0 || limit > MaxListLimit {
		limit = DefaultListLimit
	}
	query := `
		SELECT ledger_entry_id, tenant_id, legal_entity_id, book_id, fiscal_period,
		       journal_id, journal_line_id, line_number, account_code,
		       debit_amount, credit_amount, currency_code, dimensions,
		       transaction_date, posting_date, source_event_id, correlation_id, entry_seq, created_at
		FROM ledger_entries
		WHERE tenant_id = $1 AND legal_entity_id = $2`
	args := []any{tenantID, filter.LegalEntityID}
	if filter.BookID != "" {
		args = append(args, filter.BookID)
		query += fmt.Sprintf(" AND book_id = $%d", len(args))
	}
	if filter.AccountCode != "" {
		args = append(args, filter.AccountCode)
		query += fmt.Sprintf(" AND account_code = $%d", len(args))
	}
	if filter.FiscalPeriod != "" {
		args = append(args, filter.FiscalPeriod)
		query += fmt.Sprintf(" AND fiscal_period = $%d", len(args))
	}
	if filter.JournalID != "" {
		args = append(args, filter.JournalID)
		query += fmt.Sprintf(" AND journal_id = $%d", len(args))
	}
	if filter.MaxEntrySeq != nil {
		args = append(args, *filter.MaxEntrySeq)
		query += fmt.Sprintf(" AND entry_seq <= $%d", len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY entry_seq ASC LIMIT $%d", len(args))

	var entries []domain.LedgerEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.LedgerEntry
			if err := rows.Scan(&e.LedgerEntryID, &e.TenantID, &e.LegalEntityID, &e.BookID, &e.FiscalPeriod,
				&e.JournalID, &e.JournalLineID, &e.LineNumber, &e.AccountCode,
				&e.DebitAmount, &e.CreditAmount, &e.CurrencyCode, &e.Dimensions,
				&e.TransactionDate, &e.PostingDate, &e.SourceEventID, &e.CorrelationID, &e.EntrySeq, &e.CreatedAt); err != nil {
				return mapPgError(err)
			}
			entries = append(entries, e)
		}
		return mapPgError(rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// QuerySourceEntries returns every ledger entry traceable to the given
// source_event_id — ACC-05's own lineage query, distinct from QueryLedger
// in that it deliberately does NOT require legal_entity_id: tracing a
// source fact to its consequences is a cross-entity question by nature
// (e.g. an intercompany event posts to two entities), so it is scoped by
// tenant + the source reference alone.
func (s *PgStore) QuerySourceEntries(ctx context.Context, tenantID, sourceEventID string) ([]domain.LedgerEntry, error) {
	var entries []domain.LedgerEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT ledger_entry_id, tenant_id, legal_entity_id, book_id, fiscal_period,
			       journal_id, journal_line_id, line_number, account_code,
			       debit_amount, credit_amount, currency_code, dimensions,
			       transaction_date, posting_date, source_event_id, correlation_id, entry_seq, created_at
			FROM ledger_entries
			WHERE tenant_id = $1 AND source_event_id = $2
			ORDER BY entry_seq ASC
		`, tenantID, sourceEventID)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.LedgerEntry
			if err := rows.Scan(&e.LedgerEntryID, &e.TenantID, &e.LegalEntityID, &e.BookID, &e.FiscalPeriod,
				&e.JournalID, &e.JournalLineID, &e.LineNumber, &e.AccountCode,
				&e.DebitAmount, &e.CreditAmount, &e.CurrencyCode, &e.Dimensions,
				&e.TransactionDate, &e.PostingDate, &e.SourceEventID, &e.CorrelationID, &e.EntrySeq, &e.CreatedAt); err != nil {
				return mapPgError(err)
			}
			entries = append(entries, e)
		}
		return mapPgError(rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// QueryAccountBalance reads the ledger_balances projection — the fast
// path, not a live recompute. Returns domain.ErrPostingExecutionNotFound's
// sibling zero-value semantics: a scope with no projection row yet (never
// posted to, or never rebuilt) is reported as a zero balance, not an
// error — an account nobody has posted to genuinely has a zero balance.
func (s *PgStore) QueryAccountBalance(ctx context.Context, tenantID string, req domain.QueryAccountBalanceRequest) (*domain.LedgerBalance, error) {
	bal := &domain.LedgerBalance{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, BookID: req.BookID,
		AccountCode: req.AccountCode, FiscalPeriod: req.FiscalPeriod,
	}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(debit_total), 0), COALESCE(SUM(credit_total), 0),
			       COALESCE(SUM(net_balance), 0), COALESCE(MAX(watermark_entry_seq), 0)
			FROM ledger_balances
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND book_id = $3
			  AND account_code = $4 AND fiscal_period = $5
		`, tenantID, req.LegalEntityID, req.BookID, req.AccountCode, req.FiscalPeriod)
		return mapPgError(row.Scan(&bal.DebitTotal, &bal.CreditTotal, &bal.NetBalance, &bal.WatermarkEntrySeq))
	})
	if err != nil {
		return nil, err
	}
	return bal, nil
}

// RebuildDerivedBalanceProjection is ACC-05's own controlled command —
// "balance projections versioned/rebuildable from entries." It recomputes
// ledger_balances entirely from ledger_entries for the given scope and
// never touches ledger_entries itself, satisfying the spec's own negative
// path #3, "balance projection corrupt while entries intact": whatever
// state ledger_balances was in, this replaces it with a value entries
// alone can reproduce.
func (s *PgStore) RebuildDerivedBalanceProjection(ctx context.Context, tenantID string, req domain.RebuildBalanceProjectionRequest) error {
	now := time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			DELETE FROM ledger_balances
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND book_id = $3 AND fiscal_period = $4
		`, tenantID, req.LegalEntityID, req.BookID, req.FiscalPeriod); err != nil {
			return mapPgError(err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO ledger_balances (
				tenant_id, legal_entity_id, book_id, account_code, fiscal_period, dimensions_key,
				debit_total, credit_total, net_balance, watermark_entry_seq, rebuilt_at
			)
			SELECT tenant_id, legal_entity_id, book_id, account_code, fiscal_period,
			       COALESCE(dimensions, '{}'::jsonb)::text,
			       SUM(debit_amount), SUM(credit_amount), SUM(debit_amount - credit_amount),
			       MAX(entry_seq), $5
			FROM ledger_entries
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND book_id = $3 AND fiscal_period = $4
			GROUP BY tenant_id, legal_entity_id, book_id, account_code, fiscal_period, COALESCE(dimensions, '{}'::jsonb)::text
		`, tenantID, req.LegalEntityID, req.BookID, req.FiscalPeriod, now)
		return mapPgError(err)
	})
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

// ── ACC-04 (Posting Engine) ───────────────────────────────────────────────────

const postingExecutionColumns = `
	execution_id, tenant_id, legal_entity_id, kind, source_event_id, idempotency_key,
	status, journal_id, calculation_trace::text, failure_reason, correlation_id,
	created_at, created_by_principal_id, committed_at`

func scanPostingExecution(row pgx.Row) (*domain.PostingExecution, error) {
	var e domain.PostingExecution
	if err := row.Scan(
		&e.ExecutionID, &e.TenantID, &e.LegalEntityID, &e.Kind, &e.SourceEventID, &e.IdempotencyKey,
		&e.Status, &e.JournalID, &e.CalculationTrace, &e.FailureReason, &e.CorrelationID,
		&e.CreatedAt, &e.CreatedByPrincipalID, &e.CommittedAt,
	); err != nil {
		return nil, err
	}
	return &e, nil
}

// CreatePostingExecution inserts a new execution in SUBMITTED status.
// Idempotent on the migration's own UNIQUE(tenant_id, source_event_id)
// partial index — a replayed PostAccountingEvent for the same
// source_event_id is caught by GetPostingExecutionBySource before this is
// ever called, so this method itself simply fails loudly (mapPgError) on
// the rare race where two concurrent callers both lost that check.
func (s *PgStore) CreatePostingExecution(ctx context.Context, e *domain.PostingExecution) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO posting_executions (
				execution_id, tenant_id, legal_entity_id, kind, source_event_id, idempotency_key,
				status, calculation_trace, correlation_id, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11)
		`, e.ExecutionID, tenantID, e.LegalEntityID, e.Kind, e.SourceEventID, e.IdempotencyKey,
			e.Status, e.CalculationTrace, e.CorrelationID, e.CreatedAt, e.CreatedByPrincipalID)
		return mapPgError(err)
	})
}

func (s *PgStore) GetPostingExecution(ctx context.Context, tenantID, executionID string) (*domain.PostingExecution, error) {
	var e *domain.PostingExecution
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+postingExecutionColumns+` FROM posting_executions WHERE tenant_id = $1 AND execution_id = $2`, tenantID, executionID)
		var err error
		e, err = scanPostingExecution(row)
		return mapPgError(err)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPostingExecutionNotFound
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

// GetPostingExecutionBySource is ACC-04's own idempotency check — the
// spec's own negative path, "Duplicate source event," relies on this
// returning the PRIOR execution rather than a caller creating a second
// one for the same source_event_id.
func (s *PgStore) GetPostingExecutionBySource(ctx context.Context, tenantID, sourceEventID string) (*domain.PostingExecution, error) {
	var e *domain.PostingExecution
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+postingExecutionColumns+` FROM posting_executions WHERE tenant_id = $1 AND source_event_id = $2`, tenantID, sourceEventID)
		var err error
		e, err = scanPostingExecution(row)
		return mapPgError(err)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPostingExecutionNotFound
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

// MarkPostingExecutionCommitted records the terminal success outcome —
// the journal this execution produced. No fromStatus guard: this is
// always the one terminal write a given execution makes, called exactly
// once per successful attempt (initial or reprocessed).
func (s *PgStore) MarkPostingExecutionCommitted(ctx context.Context, tenantID, executionID, journalID string, committedAt time.Time) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE posting_executions SET status = $1, journal_id = $2, committed_at = $3, failure_reason = NULL
			WHERE tenant_id = $4 AND execution_id = $5
		`, domain.PostingExecutionStatusCommitted, journalID, committedAt, tenantID, executionID)
		return mapPgError(err)
	})
}

// MarkPostingExecutionFailed records a failed or quarantined outcome —
// never left silently in an intermediate status, per the spec's own
// state model ("no partial committed state").
func (s *PgStore) MarkPostingExecutionFailed(ctx context.Context, tenantID, executionID, status, reason string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE posting_executions SET status = $1, failure_reason = $2
			WHERE tenant_id = $3 AND execution_id = $4
		`, status, reason, tenantID, executionID)
		return mapPgError(err)
	})
}

// ── ACC-03 (Journal Entry: proposal/approval lifecycle) ──────────────────────

// SubmitJournalForApproval moves DRAFT -> PENDING_APPROVAL, stamping who
// submitted it and when.
func (s *PgStore) SubmitJournalForApproval(ctx context.Context, tenantID, journalID, principalID string) error {
	return s.transitionApproval(ctx, tenantID, journalID,
		domain.ApprovalStatusDraft, domain.ApprovalStatusPendingApproval,
		"submitted_by_principal_id", "submitted_at", principalID)
}

// ApproveJournal moves PENDING_APPROVAL -> APPROVED, recording the
// spec's own named evidence — a permanent fingerprint of exactly what
// content was approved (see migration 000010's doc comment).
func (s *PgStore) ApproveJournal(ctx context.Context, tenantID, journalID, principalID, fingerprint string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE journal_headers
			SET approval_status = $1, approved_by_principal_id = $2, approved_at = $3, approval_fingerprint = $4
			WHERE journal_id = $5 AND approval_status = $6 AND tenant_id = $7
		`, string(domain.ApprovalStatusApproved), principalID, time.Now().UTC(), fingerprint,
			journalID, string(domain.ApprovalStatusPendingApproval), tenantID)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidApprovalTransition
		}
		return nil
	})
}

// RejectJournal moves PENDING_APPROVAL -> REJECTED, a terminal state —
// "rejected/cancelled before posting," per the spec's own state model.
func (s *PgStore) RejectJournal(ctx context.Context, tenantID, journalID, principalID, reason string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE journal_headers
			SET approval_status = $1, rejected_by_principal_id = $2, rejected_at = $3, rejection_reason = $4
			WHERE journal_id = $5 AND approval_status = $6 AND tenant_id = $7
		`, string(domain.ApprovalStatusRejected), principalID, time.Now().UTC(), reason,
			journalID, string(domain.ApprovalStatusPendingApproval), tenantID)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidApprovalTransition
		}
		return nil
	})
}

// RequestJournalPosting moves APPROVED -> POSTING_REQUESTED — the
// handoff signal to ACC-04: only a POSTING_REQUESTED journal is eligible
// for PostApprovedJournal to actually commit.
func (s *PgStore) RequestJournalPosting(ctx context.Context, tenantID, journalID, principalID string) error {
	return s.transitionApproval(ctx, tenantID, journalID,
		domain.ApprovalStatusApproved, domain.ApprovalStatusPostingRequested,
		"posting_requested_by_principal_id", "posting_requested_at", principalID)
}

// MarkJournalPosted moves POSTING_REQUESTED -> POSTED — called by ACC-04's
// own PostApprovedJournal once it actually finalizes the journal, closing
// the loop between the two capabilities. No actor/timestamp columns of
// its own: PostedByPrincipalID/PostedAt (JournalStatus's own fields,
// already stamped by TransitionJournal) already answer who/when.
func (s *PgStore) MarkJournalPosted(ctx context.Context, tenantID, journalID string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE journal_headers SET approval_status = $1
			WHERE journal_id = $2 AND approval_status = $3 AND tenant_id = $4
		`, string(domain.ApprovalStatusPosted), journalID, string(domain.ApprovalStatusPostingRequested), tenantID)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidApprovalTransition
		}
		return nil
	})
}

// transitionApproval is the shared guarded UPDATE every simple
// ApprovalStatus move uses — mirrors TransitionJournal's own pattern.
func (s *PgStore) transitionApproval(ctx context.Context, tenantID, journalID string, from, to domain.ApprovalStatus, actorColumn, timeColumn, principalID string) error {
	query := fmt.Sprintf(`
		UPDATE journal_headers
		SET approval_status = $1, %s = $2, %s = $3
		WHERE journal_id = $4 AND approval_status = $5 AND tenant_id = $6
	`, actorColumn, timeColumn)
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, query, string(to), principalID, time.Now().UTC(), journalID, string(from), tenantID)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidApprovalTransition
		}
		return nil
	})
}

// AmendDraftJournal replaces a journal's editable header fields and every
// line, in one transaction. Only succeeds from DRAFT or PENDING_APPROVAL
// (enforced by the WHERE clause, not just the handler) — the spec's own
// negative paths #2 ("journal changed after approval") and #4 ("attempt
// edit after posting") are satisfied by there being no other status this
// UPDATE ever matches. Amending a PENDING_APPROVAL journal always resets
// it to DRAFT (see ValidApprovalTransitions's own doc comment) —
// withdrawing a stale approval request rather than leaving one pending
// against content an approver never actually saw.
func (s *PgStore) AmendDraftJournal(ctx context.Context, tenantID, journalID string, h *domain.JournalHeader, lines []domain.JournalLine) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE journal_headers
			SET description = $1, journal_type = $2, transaction_date = $3, posting_date = $4,
			    currency_code = $5, book_id = $6, reporting_basis = $7, evidence_refs = $8,
			    approval_status = $9
			WHERE journal_id = $10 AND tenant_id = $11 AND approval_status IN ($12, $13)
		`, h.Description, h.JournalType, h.TransactionDate, h.PostingDate,
			h.CurrencyCode, h.BookID, h.ReportingBasis, h.EvidenceRefs,
			string(domain.ApprovalStatusDraft),
			journalID, tenantID, string(domain.ApprovalStatusDraft), string(domain.ApprovalStatusPendingApproval))
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidApprovalTransition
		}

		if _, err := tx.Exec(ctx, `DELETE FROM journal_lines WHERE journal_id = $1 AND tenant_id = $2`, journalID, tenantID); err != nil {
			return mapPgError(err)
		}
		for i := range lines {
			lines[i].JournalLineID = uuid.NewString()
			lines[i].JournalID = journalID
			lines[i].LineNumber = i + 1
			if _, err := tx.Exec(ctx, `
				INSERT INTO journal_lines (
					journal_line_id, journal_id, tenant_id, line_number,
					account_code, debit_amount, credit_amount, description,
					tax_code, tax_logic_snapshot_id, dimensions
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			`, lines[i].JournalLineID, journalID, tenantID, lines[i].LineNumber,
				lines[i].AccountCode, lines[i].DebitAmount, lines[i].CreditAmount, lines[i].Description,
				lines[i].TaxCode, lines[i].TaxLogicSnapshotID, lines[i].Dimensions); err != nil {
				return mapPgError(err)
			}
		}
		return nil
	})
}
