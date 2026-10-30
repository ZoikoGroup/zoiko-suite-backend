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

func scanCheck(row pgx.Row, c *domain.VendorDDCheck) error {
	return row.Scan(
		&c.CheckID, &c.TenantID, &c.LegalEntityID, &c.CounterpartyID, &c.VendorName,
		&c.Status, &c.RiskOutcome, &c.ScreeningBasis, &c.ScreeningSource,
		&c.CorrelationID, &c.InitiatedByPrincipalID, &c.StartedAt, &c.CompletedAt,
	)
}

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

func tenantScope(ctx context.Context) (string, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return "", domain.ErrTenantMissing
	}
	return tenantID, nil
}

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
		if mapped := mapPgError(err); errors.Is(mapped, domain.ErrInvalidIdentifier) {
			return nil, domain.ErrCheckNotFound
		}
		return nil, err
	}
	return &c, nil
}

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
			return []domain.VendorDDEvidence{}, nil
		}
		return nil, err
	}
	return out, nil
}

func (s *PgStore) CreateVendorProfile(ctx context.Context, p *domain.VendorProfile) error {
	tenantID, err := tenantScope(ctx)
	if err != nil {
		return err
	}

	if p.VendorProfileID == "" {
		p.VendorProfileID = uuid.NewString()
	}
	p.TenantID = tenantID
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now
	if p.VendorStatus == "" {
		p.VendorStatus = "ONBOARDING"
	}
	if p.DueDiligenceStatus == "" {
		p.DueDiligenceStatus = "PENDING"
	}
	if p.RiskRating == "" {
		p.RiskRating = "UNRATED"
	}

	return mapPgError(s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO vendor_profiles (
				vendor_profile_id, tenant_id, legal_entity_id, counterparty_id,
				vendor_name, vendor_status, due_diligence_status, risk_rating,
				approved_at, approved_by, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, p.VendorProfileID, p.TenantID, p.LegalEntityID, p.CounterpartyID,
			p.VendorName, p.VendorStatus, p.DueDiligenceStatus, p.RiskRating,
			p.ApprovedAt, p.ApprovedBy, p.CreatedAt, p.UpdatedAt,
		)
		return err
	}))
}

func (s *PgStore) GetVendorProfileByID(ctx context.Context, id string) (*domain.VendorProfile, error) {
	tenantID, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}

	var p domain.VendorProfile
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT vendor_profile_id, tenant_id, legal_entity_id, counterparty_id,
			       vendor_name, vendor_status, due_diligence_status, risk_rating,
			       approved_at, approved_by, created_at, updated_at
			FROM vendor_profiles
			WHERE vendor_profile_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&p.VendorProfileID, &p.TenantID, &p.LegalEntityID, &p.CounterpartyID,
			&p.VendorName, &p.VendorStatus, &p.DueDiligenceStatus, &p.RiskRating,
			&p.ApprovedAt, &p.ApprovedBy, &p.CreatedAt, &p.UpdatedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrVendorProfileNotFound
		}
		return nil, mapPgError(err)
	}
	return &p, nil
}

func (s *PgStore) ListVendorProfiles(ctx context.Context, legalEntityID, counterpartyID string) ([]domain.VendorProfile, error) {
	tenantID, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}

	var out []domain.VendorProfile
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT vendor_profile_id, tenant_id, legal_entity_id, counterparty_id,
			       vendor_name, vendor_status, due_diligence_status, risk_rating,
			       approved_at, approved_by, created_at, updated_at
			FROM vendor_profiles
			WHERE tenant_id = $1
		`
		args := []any{tenantID}
		idx := 2
		if legalEntityID != "" {
			query += fmt.Sprintf(" AND legal_entity_id = $%d", idx)
			args = append(args, legalEntityID)
			idx++
		}
		if counterpartyID != "" {
			query += fmt.Sprintf(" AND counterparty_id = $%d", idx)
			args = append(args, counterpartyID)
			idx++
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var p domain.VendorProfile
			if err := rows.Scan(
				&p.VendorProfileID, &p.TenantID, &p.LegalEntityID, &p.CounterpartyID,
				&p.VendorName, &p.VendorStatus, &p.DueDiligenceStatus, &p.RiskRating,
				&p.ApprovedAt, &p.ApprovedBy, &p.CreatedAt, &p.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return out, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
