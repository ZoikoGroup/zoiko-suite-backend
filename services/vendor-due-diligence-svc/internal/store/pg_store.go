package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/vendor-due-diligence-svc/internal/domain"
	svcmiddleware "zoiko.io/vendor-due-diligence-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// checkColumns is the single source of truth for the SELECT list.
//
// Derived from one slice rather than hand-copied into each query so the list and
// its scan targets cannot drift apart — the policy-svc bug where a SELECT named
// 10 of 12 columns was exactly this shape. COALESCE lives in selectCheckSQL so
// the nullable columns scan into plain strings.
var checkColumns = []string{
	"check_id",
	"tenant_id",
	"legal_entity_id",
	"counterparty_id",
	"vendor_name",
	"status",
	"COALESCE(risk_outcome, '')",
	"COALESCE(screening_basis, '')",
	"COALESCE(screening_source, '')",
	"correlation_id",
	"initiated_by_principal_id",
	"started_at",
	"completed_at",
}

func selectCheckSQL() string {
	return "SELECT " + strings.Join(checkColumns, ", ") + " FROM vendor_dd_checks"
}

// scanCheck reads one row in checkColumns order. The one place that knows the
// order, paired with the one place that knows the list.
func scanCheck(row pgx.Row, c *domain.VendorDDCheck) error {
	return row.Scan(
		&c.CheckID, &c.TenantID, &c.LegalEntityID, &c.CounterpartyID, &c.VendorName,
		&c.Status, &c.RiskOutcome, &c.ScreeningBasis, &c.ScreeningSource,
		&c.CorrelationID, &c.InitiatedByPrincipalID, &c.StartedAt, &c.CompletedAt,
	)
}

// mapPgError turns the Postgres failures that are really caller mistakes into
// domain errors, so they stop arriving at the client as "the store is
// unavailable".
//
// 22P02 is the one that mattered here: check_id is a uuid column, so
// GET /v1/vendor-checks/not-a-uuid died inside the driver before any row was
// examined and answered 503. A mistyped id in a URL is not an outage, and
// reporting it as one sends an operator to look at Docker instead of at the id.
// The existing handler test did not catch it because the stub store is a map and
// never sees the driver.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	if pgErr.Code == "22P02" {
		return domain.ErrInvalidIdentifier
	}
	return err
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// tenantScope resolves the tenant every query is filtered by, refusing rather
// than running unscoped.
//
// The returned error is ErrTenantMissing and not a generic one on purpose: the
// handler distinguishes it from a broken database, because a caller who omitted
// X-Tenant-Id was previously told the service's database was down.
func tenantScope(ctx context.Context) (string, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return "", domain.ErrTenantMissing
	}
	return tenantID, nil
}

// CreateCheck inserts a new check in STARTED status, idempotent on
// (tenant_id, correlation_id): a retry finds the existing row instead of
// starting a second check for the same request.
//
// On conflict the caller's struct is overwritten with the stored row, so the
// response describes what is actually on record — including a prior attempt left
// in STARTED, which the handler reports rather than presenting as a fresh answer.
func (s *PgStore) CreateCheck(ctx context.Context, c *domain.VendorDDCheck) (created bool, err error) {
	tenantID, err := tenantScope(ctx)
	if err != nil {
		return false, err
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO vendor_dd_checks (
				check_id, tenant_id, legal_entity_id, counterparty_id, vendor_name,
				status, correlation_id, initiated_by_principal_id, started_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, c.CheckID, tenantID, c.LegalEntityID, c.CounterpartyID, c.VendorName,
			c.Status, c.CorrelationID, c.InitiatedByPrincipalID, c.StartedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}

		// Conflict: a check for this (tenant_id, correlation_id) already
		// exists — fetch its current state so the caller returns the
		// existing check rather than a second, divergent one.
		row := tx.QueryRow(ctx,
			selectCheckSQL()+" WHERE tenant_id = $1 AND correlation_id = $2",
			tenantID, c.CorrelationID)
		return scanCheck(row, c)
	})
	if err != nil {
		return false, mapPgError(err)
	}
	return created, nil
}

func (s *PgStore) GetCheck(ctx context.Context, id string) (*domain.VendorDDCheck, error) {
	tenantID, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}

	var c domain.VendorDDCheck
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			selectCheckSQL()+" WHERE check_id = $1 AND tenant_id = $2", id, tenantID)
		return scanCheck(row, &c)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCheckNotFound
	}
	if err != nil {
		// A non-UUID path param means no such check can exist, which is what the
		// caller asked about — so it reads as absent, not as a fault.
		if mapped := mapPgError(err); errors.Is(mapped, domain.ErrInvalidIdentifier) {
			return nil, domain.ErrCheckNotFound
		}
		return nil, err
	}
	return &c, nil
}

// ListFilter narrows the register. Both filters are applied by the service and
// compose with AND.
//
// Limit and Offset are required rather than optional: the route previously had no
// pagination at all, so every read returned the tenant's entire screening history
// and the response grew without bound as checks accumulated.
type ListFilter struct {
	LegalEntityID  string
	CounterpartyID string
	Limit          int
	Offset         int
}

func (s *PgStore) ListChecks(ctx context.Context, f ListFilter) ([]domain.VendorDDCheck, error) {
	tenantID, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}

	var out []domain.VendorDDCheck
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := selectCheckSQL() + " WHERE tenant_id = $1"
		args := []any{tenantID}

		if f.LegalEntityID != "" {
			args = append(args, f.LegalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if f.CounterpartyID != "" {
			args = append(args, f.CounterpartyID)
			query += fmt.Sprintf(" AND counterparty_id = $%d", len(args))
		}

		// started_at is not unique — the stub screening is fast enough that two
		// checks can share a timestamp — so check_id breaks the tie. Without it
		// two pages can repeat a row and skip another.
		args = append(args, f.Limit, f.Offset)
		query += fmt.Sprintf(" ORDER BY started_at DESC, check_id DESC LIMIT $%d OFFSET $%d",
			len(args)-1, len(args))

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c domain.VendorDDCheck
			if err := scanCheck(rows, &c); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return out, nil
}

// ConcludeCheck records the outcome AND the evidence supporting it, in one
// transaction, guarded on the check still being STARTED.
//
// This replaces a separate AddEvidence + CompleteCheck pair, and the split was a
// real defect rather than a tidiness problem. The evidence write's failure was
// logged and swallowed, the completion then ran anyway, and the handler returned
// the evidence record in its response — so a caller was handed a COMPLETED/CLEAR
// check together with the evidence for it while the store held the check and no
// evidence at all. A later GET returned the clean outcome with an empty evidence
// list. For a service whose purpose is due-diligence evidence, a conclusion that
// can outlive its evidence is the worst available failure: it is an unevidenced
// compliance pass that reads exactly like an evidenced one.
//
// Both rows now land together or neither does. The STARTED guard is what stops a
// second conclusion overwriting a terminal one — including overwriting FLAGGED
// with CLEAR, which the unguarded UPDATE permitted.
func (s *PgStore) ConcludeCheck(
	ctx context.Context,
	checkID, riskOutcome, screeningBasis, screeningSource string,
	evidence *domain.VendorDDEvidence,
) error {
	tenantID, err := tenantScope(ctx)
	if err != nil {
		return err
	}

	return mapPgError(s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()

		res, err := tx.Exec(ctx, `
			UPDATE vendor_dd_checks
			SET status = $1, risk_outcome = $2, screening_basis = $3,
			    screening_source = $4, completed_at = $5
			WHERE check_id = $6 AND tenant_id = $7 AND status = $8
		`, domain.StatusCompleted, riskOutcome, screeningBasis, screeningSource, now,
			checkID, tenantID, domain.StatusStarted)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			// Either no such check for this tenant, or it is no longer STARTED.
			// Distinguished by a read, so a replay is not reported as a missing
			// record — and it is inside this transaction, so the answer cannot be
			// stale by the time it is returned.
			var status string
			err := tx.QueryRow(ctx,
				"SELECT status FROM vendor_dd_checks WHERE check_id = $1 AND tenant_id = $2",
				checkID, tenantID).Scan(&status)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCheckNotFound
			}
			if err != nil {
				return err
			}
			return domain.ErrCheckAlreadyConcluded
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO vendor_dd_evidence (
				evidence_id, check_id, tenant_id, evidence_type, description,
				document_reference, recorded_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7)
		`, evidence.EvidenceID, checkID, tenantID, evidence.EvidenceType,
			evidence.Description, nullIfEmpty(evidence.DocumentReference), evidence.RecordedAt)
		return err
	}))
}

// MarkFailed records that a check could not be concluded, with the reason.
//
// Before this existed, FAILED was a status no code path could ever write and
// vendor.dd.failed was an event declared in the spec (§12.10) that could never be
// emitted. A check whose conclusion failed was simply left in STARTED — where it
// stayed forever, indistinguishable in the register from anything else, with no
// route to retry it and nothing downstream told that a screening had been
// attempted and lost.
//
// Guarded on STARTED like ConcludeCheck: a check that already concluded must not
// be retrospectively marked failed.
func (s *PgStore) MarkFailed(ctx context.Context, checkID, reason string) error {
	tenantID, err := tenantScope(ctx)
	if err != nil {
		return err
	}

	return mapPgError(s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		res, err := tx.Exec(ctx, `
			UPDATE vendor_dd_checks
			SET status = $1, screening_basis = $2, completed_at = $3
			WHERE check_id = $4 AND tenant_id = $5 AND status = $6
		`, domain.StatusFailed, reason, now, checkID, tenantID, domain.StatusStarted)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrCheckNotFound
		}
		return nil
	}))
}

func (s *PgStore) ListEvidence(ctx context.Context, checkID string) ([]domain.VendorDDEvidence, error) {
	tenantID, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}

	var out []domain.VendorDDEvidence
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT evidence_id, check_id, tenant_id, evidence_type, description,
			       COALESCE(document_reference, ''), recorded_at
			FROM vendor_dd_evidence
			WHERE check_id = $1 AND tenant_id = $2
			ORDER BY recorded_at ASC, evidence_id ASC
		`, checkID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var e domain.VendorDDEvidence
			if err := rows.Scan(
				&e.EvidenceID, &e.CheckID, &e.TenantID, &e.EvidenceType, &e.Description,
				&e.DocumentReference, &e.RecordedAt,
			); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		if mapped := mapPgError(err); errors.Is(mapped, domain.ErrInvalidIdentifier) {
			// No check with a non-UUID id exists, so it has no evidence.
			return []domain.VendorDDEvidence{}, nil
		}
		return nil, err
	}
	return out, nil
}

// nullIfEmpty keeps "no value" out of the database as NULL rather than "".
// document_reference held "" for every row it had ever been written, making
// "no supporting document" and "a document whose reference is blank" the same
// value.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
