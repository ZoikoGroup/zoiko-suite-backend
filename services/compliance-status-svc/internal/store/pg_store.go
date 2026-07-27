package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/compliance-status-svc/internal/domain"
	"zoiko.io/compliance-status-svc/internal/middleware"
)

type Store interface {
	Evaluate(ctx context.Context, c *domain.ComplianceHealth) error
	GetByID(ctx context.Context, id string) (*domain.ComplianceHealth, error)
	List(ctx context.Context, legalEntityID, jurisdictionID, domainName, status string) ([]domain.ComplianceHealth, error)
	CreateGap(ctx context.Context, g *domain.ComplianceGap) error
	ListGaps(ctx context.Context, legalEntityID, domainName, severity, status string) ([]domain.ComplianceGap, error)
	ResolveGap(ctx context.Context, id string, req *domain.ResolveGapRequest) (*domain.ComplianceGap, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID)
	return err
}

func (s *PgStore) Evaluate(ctx context.Context, c *domain.ComplianceHealth) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if c.StatusID == "" {
		c.StatusID = "cstat-" + uuid.New().String()
	}
	c.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	c.LastEvaluatedAt = now
	c.CreatedAt = now
	c.UpdatedAt = now
	if c.DomainName == "" {
		c.DomainName = "OVERALL"
	}
	if c.EffectiveFrom == "" {
		c.EffectiveFrom = now.Format("2006-01-02")
	}
	c.CalculateHealthScore()

	_, err = tx.Exec(ctx, `
		INSERT INTO compliance_status_records
			(status_id, tenant_id, legal_entity_id, jurisdiction_id, domain_name,
			 overall_status, health_score, total_obligations, fulfilled_obligations,
			 pending_obligations, overdue_obligations, open_exceptions, last_evaluated_at,
			 notes, effective_from, effective_to, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		ON CONFLICT (tenant_id, legal_entity_id, jurisdiction_id, domain_name)
		DO UPDATE SET
			overall_status = EXCLUDED.overall_status,
			health_score = EXCLUDED.health_score,
			total_obligations = EXCLUDED.total_obligations,
			fulfilled_obligations = EXCLUDED.fulfilled_obligations,
			pending_obligations = EXCLUDED.pending_obligations,
			overdue_obligations = EXCLUDED.overdue_obligations,
			open_exceptions = EXCLUDED.open_exceptions,
			last_evaluated_at = EXCLUDED.last_evaluated_at,
			notes = EXCLUDED.notes,
			updated_at = EXCLUDED.updated_at`,
		c.StatusID, c.TenantID, c.LegalEntityID, c.JurisdictionID, c.DomainName,
		string(c.OverallStatus), c.HealthScore, c.TotalObligations, c.FulfilledObligations,
		c.PendingObligations, c.OverdueObligations, c.OpenExceptions, c.LastEvaluatedAt,
		c.Notes, c.EffectiveFrom, c.EffectiveTo, c.CreatedBy, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert compliance status: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) GetByID(ctx context.Context, id string) (*domain.ComplianceHealth, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var c domain.ComplianceHealth
	var statusStr string
	err = tx.QueryRow(ctx, `
		SELECT status_id, tenant_id, legal_entity_id, jurisdiction_id, domain_name,
		       overall_status, health_score, total_obligations, fulfilled_obligations,
		       pending_obligations, overdue_obligations, open_exceptions, last_evaluated_at,
		       notes, effective_from, effective_to, created_by, created_at, updated_at
		FROM compliance_status_records WHERE status_id = $1`, id,
	).Scan(
		&c.StatusID, &c.TenantID, &c.LegalEntityID, &c.JurisdictionID, &c.DomainName,
		&statusStr, &c.HealthScore, &c.TotalObligations, &c.FulfilledObligations,
		&c.PendingObligations, &c.OverdueObligations, &c.OpenExceptions, &c.LastEvaluatedAt,
		&c.Notes, &c.EffectiveFrom, &c.EffectiveTo, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrStatusRecordNotFound
		}
		return nil, err
	}
	c.OverallStatus = domain.OverallStatus(statusStr)
	_ = tx.Commit(ctx)
	return &c, nil
}

func (s *PgStore) List(ctx context.Context, legalEntityID, jurisdictionID, domainName, status string) ([]domain.ComplianceHealth, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT status_id, tenant_id, legal_entity_id, jurisdiction_id, domain_name,
		       overall_status, health_score, total_obligations, fulfilled_obligations,
		       pending_obligations, overdue_obligations, open_exceptions, last_evaluated_at,
		       notes, effective_from, effective_to, created_by, created_at, updated_at
		FROM compliance_status_records
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR jurisdiction_id = $2)
		  AND ($3 = '' OR domain_name = $3)
		  AND ($4 = '' OR overall_status = $4)
		ORDER BY health_score ASC, last_evaluated_at DESC`,
		legalEntityID, jurisdictionID, domainName, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ComplianceHealth
	for rows.Next() {
		var c domain.ComplianceHealth
		var statusStr string
		if err := rows.Scan(
			&c.StatusID, &c.TenantID, &c.LegalEntityID, &c.JurisdictionID, &c.DomainName,
			&statusStr, &c.HealthScore, &c.TotalObligations, &c.FulfilledObligations,
			&c.PendingObligations, &c.OverdueObligations, &c.OpenExceptions, &c.LastEvaluatedAt,
			&c.Notes, &c.EffectiveFrom, &c.EffectiveTo, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, err
		}
		c.OverallStatus = domain.OverallStatus(statusStr)
		out = append(out, c)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) CreateGap(ctx context.Context, g *domain.ComplianceGap) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if g.GapID == "" {
		g.GapID = "cgap-" + uuid.New().String()
	}
	g.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	g.DetectedAt = now
	g.CreatedAt = now
	g.UpdatedAt = now
	if g.Status == "" {
		g.Status = domain.GapOpen
	}
	if g.Severity == "" {
		g.Severity = domain.SeverityMedium
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO compliance_gaps
			(gap_id, tenant_id, legal_entity_id, jurisdiction_id, domain_name,
			 gap_type, severity, source_reference, description, remediation_plan,
			 status, detected_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		g.GapID, g.TenantID, g.LegalEntityID, g.JurisdictionID, g.DomainName,
		g.GapType, string(g.Severity), g.SourceReference, g.Description, g.RemediationPlan,
		string(g.Status), g.DetectedAt, g.CreatedAt, g.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert compliance gap: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) ListGaps(ctx context.Context, legalEntityID, domainName, severity, status string) ([]domain.ComplianceGap, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT gap_id, tenant_id, legal_entity_id, jurisdiction_id, domain_name,
		       gap_type, severity, source_reference, description, remediation_plan,
		       status, detected_at, resolved_at, created_at, updated_at
		FROM compliance_gaps
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR domain_name = $2)
		  AND ($3 = '' OR severity = $3)
		  AND ($4 = '' OR status = $4)
		ORDER BY detected_at DESC`,
		legalEntityID, domainName, severity, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ComplianceGap
	for rows.Next() {
		var g domain.ComplianceGap
		var sevStr, statStr string
		if err := rows.Scan(
			&g.GapID, &g.TenantID, &g.LegalEntityID, &g.JurisdictionID, &g.DomainName,
			&g.GapType, &sevStr, &g.SourceReference, &g.Description, &g.RemediationPlan,
			&statStr, &g.DetectedAt, &g.ResolvedAt, &g.CreatedAt, &g.UpdatedAt,
		); err != nil {
			return nil, err
		}
		g.Severity = domain.GapSeverity(sevStr)
		g.Status = domain.GapStatus(statStr)
		out = append(out, g)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) ResolveGap(ctx context.Context, id string, req *domain.ResolveGapRequest) (*domain.ComplianceGap, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var g domain.ComplianceGap
	var sevStr, statStr string
	err = tx.QueryRow(ctx, `
		SELECT gap_id, tenant_id, legal_entity_id, jurisdiction_id, domain_name,
		       gap_type, severity, source_reference, description, remediation_plan,
		       status, detected_at, resolved_at, created_at, updated_at
		FROM compliance_gaps WHERE gap_id = $1`, id,
	).Scan(
		&g.GapID, &g.TenantID, &g.LegalEntityID, &g.JurisdictionID, &g.DomainName,
		&g.GapType, &sevStr, &g.SourceReference, &g.Description, &g.RemediationPlan,
		&statStr, &g.DetectedAt, &g.ResolvedAt, &g.CreatedAt, &g.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrGapNotFound
		}
		return nil, err
	}
	g.Severity = domain.GapSeverity(sevStr)
	g.Status = domain.GapStatus(statStr)
	if g.Status == domain.GapResolved {
		return nil, domain.ErrGapAlreadyResolved
	}

	now := time.Now().UTC()
	g.Status = domain.GapResolved
	g.ResolvedAt = &now
	if req.RemediationNotes != "" {
		g.RemediationPlan = g.RemediationPlan + " | Resolved: " + req.RemediationNotes
	}
	g.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE compliance_gaps
		SET status=$1, remediation_plan=$2, resolved_at=$3, updated_at=$4
		WHERE gap_id=$5`,
		string(g.Status), g.RemediationPlan, g.ResolvedAt, g.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve compliance gap: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &g, nil
}
