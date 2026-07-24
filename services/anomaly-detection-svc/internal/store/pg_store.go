package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/anomaly-detection-svc/internal/domain"
	"zoiko.io/anomaly-detection-svc/internal/middleware"
)

type Store interface {
	Detect(ctx context.Context, rec *domain.AnomalyRecord) error
	GetByID(ctx context.Context, id string) (*domain.AnomalyRecord, error)
	ListAnomalies(ctx context.Context, legalEntityID, domainName, severity, status string) ([]domain.AnomalyRecord, error)
	UpdateStatus(ctx context.Context, id string, req *domain.UpdateStatusRequest) (*domain.AnomalyRecord, error)
	CreateRule(ctx context.Context, rule *domain.AnomalyRule) error
	ListRules(ctx context.Context, domainName string) ([]domain.AnomalyRule, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID))
	return err
}

func (s *PgStore) Detect(ctx context.Context, rec *domain.AnomalyRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if rec.AnomalyID == "" {
		rec.AnomalyID = "anom-" + uuid.New().String()
	}
	rec.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	rec.DetectedAt = now
	rec.CreatedAt = now
	rec.UpdatedAt = now
	if rec.Status == "" {
		rec.Status = domain.StatusOpen
	}

	var ruleID *string
	if rec.RuleID != "" {
		ruleID = &rec.RuleID
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO anomaly_records
			(anomaly_id, tenant_id, legal_entity_id, domain_name, source_entity_id,
			 rule_id, severity, anomaly_score, observed_value, expected_value,
			 description, status, detected_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		rec.AnomalyID, rec.TenantID, rec.LegalEntityID, rec.DomainName, rec.SourceEntityID,
		ruleID, string(rec.Severity), rec.AnomalyScore, rec.ObservedValue, rec.ExpectedValue,
		rec.Description, string(rec.Status), rec.DetectedAt, rec.CreatedAt, rec.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert anomaly record: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetByID(ctx context.Context, id string) (*domain.AnomalyRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var rec domain.AnomalyRecord
	var severityStr, statusStr string
	var ruleID *string

	err = tx.QueryRow(ctx, `
		SELECT anomaly_id, tenant_id, legal_entity_id, domain_name, source_entity_id,
		       rule_id, severity, anomaly_score, observed_value, expected_value,
		       description, status, investigated_by, investigated_at, resolution_notes,
		       detected_at, created_at, updated_at
		FROM anomaly_records WHERE anomaly_id = $1`, id,
	).Scan(
		&rec.AnomalyID, &rec.TenantID, &rec.LegalEntityID, &rec.DomainName, &rec.SourceEntityID,
		&ruleID, &severityStr, &rec.AnomalyScore, &rec.ObservedValue, &rec.ExpectedValue,
		&rec.Description, &statusStr, &rec.InvestigatedBy, &rec.InvestigatedAt, &rec.ResolutionNotes,
		&rec.DetectedAt, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, domain.ErrAnomalyRecordNotFound
		}
		return nil, err
	}

	if ruleID != nil {
		rec.RuleID = *ruleID
	}
	rec.Severity = domain.Severity(severityStr)
	rec.Status = domain.AnomalyStatus(statusStr)
	_ = tx.Commit(ctx)
	return &rec, nil
}

func (s *PgStore) ListAnomalies(ctx context.Context, legalEntityID, domainName, severity, status string) ([]domain.AnomalyRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT anomaly_id, tenant_id, legal_entity_id, domain_name, source_entity_id,
		       rule_id, severity, anomaly_score, observed_value, expected_value,
		       description, status, investigated_by, investigated_at, resolution_notes,
		       detected_at, created_at, updated_at
		FROM anomaly_records
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR domain_name = $3)
		  AND ($3 = '' OR severity = $3)
		  AND ($4 = '' OR status = $4)
		ORDER BY anomaly_score DESC, detected_at DESC`,
		legalEntityID, domainName, severity, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.AnomalyRecord
	for rows.Next() {
		var rec domain.AnomalyRecord
		var severityStr, statusStr string
		var ruleID *string

		if err := rows.Scan(
			&rec.AnomalyID, &rec.TenantID, &rec.LegalEntityID, &rec.DomainName, &rec.SourceEntityID,
			&ruleID, &severityStr, &rec.AnomalyScore, &rec.ObservedValue, &rec.ExpectedValue,
			&rec.Description, &statusStr, &rec.InvestigatedBy, &rec.InvestigatedAt, &rec.ResolutionNotes,
			&rec.DetectedAt, &rec.CreatedAt, &rec.UpdatedAt,
		); err != nil {
			return nil, err
		}

		if ruleID != nil {
			rec.RuleID = *ruleID
		}
		rec.Severity = domain.Severity(severityStr)
		rec.Status = domain.AnomalyStatus(statusStr)
		out = append(out, rec)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) UpdateStatus(ctx context.Context, id string, req *domain.UpdateStatusRequest) (*domain.AnomalyRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rec, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	rec.Status = req.Status
	rec.InvestigatedBy = req.InvestigatedBy
	rec.InvestigatedAt = &now
	if req.ResolutionNotes != "" {
		rec.ResolutionNotes = req.ResolutionNotes
	}
	rec.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE anomaly_records
		SET status = $1, investigated_by = $2, investigated_at = $3, resolution_notes = $4, updated_at = $5
		WHERE anomaly_id = $6`,
		string(rec.Status), rec.InvestigatedBy, rec.InvestigatedAt, rec.ResolutionNotes, rec.UpdatedAt, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update anomaly status: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *PgStore) CreateRule(ctx context.Context, rule *domain.AnomalyRule) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if rule.RuleID == "" {
		rule.RuleID = "arule-" + uuid.New().String()
	}
	rule.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	rule.CreatedAt = now
	rule.UpdatedAt = now
	rule.IsActive = true

	_, err = tx.Exec(ctx, `
		INSERT INTO anomaly_detection_rules
			(rule_id, tenant_id, rule_name, domain_name, metric_type, threshold_value, z_score_cutoff, is_active, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		rule.RuleID, rule.TenantID, rule.RuleName, rule.DomainName, rule.MetricType,
		rule.ThresholdValue, rule.ZScoreCutoff, rule.IsActive, rule.CreatedAt, rule.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert anomaly rule: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *PgStore) ListRules(ctx context.Context, domainName string) ([]domain.AnomalyRule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT rule_id, tenant_id, rule_name, domain_name, metric_type, threshold_value, z_score_cutoff, is_active, created_at, updated_at
		FROM anomaly_detection_rules
		WHERE ($1 = '' OR domain_name = $1)
		ORDER BY created_at DESC`, domainName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.AnomalyRule
	for rows.Next() {
		var r domain.AnomalyRule
		if err := rows.Scan(
			&r.RuleID, &r.TenantID, &r.RuleName, &r.DomainName, &r.MetricType,
			&r.ThresholdValue, &r.ZScoreCutoff, &r.IsActive, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	_ = tx.Commit(ctx)
	return out, nil
}
