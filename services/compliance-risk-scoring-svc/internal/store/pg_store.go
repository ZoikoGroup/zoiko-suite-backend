package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
)

type Store interface {
	CreateAssessment(ctx context.Context, tenantID string, assessment *domain.RiskScoreAssessment, breakdowns []domain.RiskFactorBreakdown) error
	GetAssessmentByID(ctx context.Context, tenantID, id string) (*domain.RiskScoreAssessment, error)
	ListAssessments(ctx context.Context, tenantID, legalEntityID, tier string) ([]domain.RiskScoreAssessment, error)
	CreateThresholdRule(ctx context.Context, tenantID string, rule *domain.RiskThresholdRule) error
	ListThresholdRules(ctx context.Context, tenantID string) ([]domain.RiskThresholdRule, error)
	ArchiveAssessment(ctx context.Context, tenantID, id string) error
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) CreateAssessment(ctx context.Context, tenantID string, assessment *domain.RiskScoreAssessment, breakdowns []domain.RiskFactorBreakdown) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return err
	}

	assessment.ID = uuid.New().String()
	assessment.TenantID = tenantID
	assessment.EvaluatedAt = time.Now()
	assessment.CreatedAt = time.Now()

	queryModel := `
		INSERT INTO risk_score_assessments (
			id, tenant_id, legal_entity_id, assessment_name, composite_risk_score, risk_tier,
			open_obligations_count, policy_violations_count, audit_exceptions_count,
			privacy_incidents_count, tax_penalties_count, status, evaluated_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

	_, err = tx.Exec(ctx, queryModel,
		assessment.ID, tenantID, assessment.LegalEntityID, assessment.AssessmentName,
		assessment.CompositeRiskScore, string(assessment.RiskTier),
		assessment.OpenObligationsCount, assessment.PolicyViolationsCount, assessment.AuditExceptionsCount,
		assessment.PrivacyIncidentsCount, assessment.TaxPenaltiesCount, assessment.Status,
		assessment.EvaluatedAt, assessment.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert risk_score_assessment failed: %w", err)
	}

	queryFactor := `
		INSERT INTO risk_factor_breakdowns (
			id, tenant_id, assessment_id, risk_category, category_weight, raw_score, weighted_score, risk_driver_summary, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	for i := range breakdowns {
		b := &breakdowns[i]
		b.ID = uuid.New().String()
		b.TenantID = tenantID
		b.AssessmentID = assessment.ID
		b.CreatedAt = time.Now()

		_, err := tx.Exec(ctx, queryFactor,
			b.ID, tenantID, assessment.ID, string(b.RiskCategory), b.CategoryWeight, b.RawScore, b.WeightedScore, b.RiskDriverSummary, b.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert risk_factor_breakdown failed: %w", err)
		}
	}

	assessment.FactorBreakdowns = breakdowns
	return tx.Commit(ctx)
}

func (s *PgStore) GetAssessmentByID(ctx context.Context, tenantID, id string) (*domain.RiskScoreAssessment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return nil, err
	}

	var a domain.RiskScoreAssessment
	queryModel := `
		SELECT id, tenant_id, legal_entity_id, assessment_name, composite_risk_score, risk_tier,
		       open_obligations_count, policy_violations_count, audit_exceptions_count,
		       privacy_incidents_count, tax_penalties_count, status, evaluated_at, created_at
		FROM risk_score_assessments WHERE id = $1`

	err = tx.QueryRow(ctx, queryModel, id).Scan(
		&a.ID, &a.TenantID, &a.LegalEntityID, &a.AssessmentName, &a.CompositeRiskScore, &a.RiskTier,
		&a.OpenObligationsCount, &a.PolicyViolationsCount, &a.AuditExceptionsCount,
		&a.PrivacyIncidentsCount, &a.TaxPenaltiesCount, &a.Status, &a.EvaluatedAt, &a.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("assessment not found: %w", err)
	}

	queryFactor := `
		SELECT id, tenant_id, assessment_id, risk_category, category_weight, raw_score, weighted_score, risk_driver_summary, created_at
		FROM risk_factor_breakdowns WHERE assessment_id = $1`

	rows, err := tx.Query(ctx, queryFactor, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var b domain.RiskFactorBreakdown
			_ = rows.Scan(&b.ID, &b.TenantID, &b.AssessmentID, &b.RiskCategory, &b.CategoryWeight, &b.RawScore, &b.WeightedScore, &b.RiskDriverSummary, &b.CreatedAt)
			a.FactorBreakdowns = append(a.FactorBreakdowns, b)
		}
	}

	return &a, nil
}

func (s *PgStore) ListAssessments(ctx context.Context, tenantID, legalEntityID, tier string) ([]domain.RiskScoreAssessment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return nil, err
	}

	query := `SELECT id, tenant_id, legal_entity_id, assessment_name, composite_risk_score, risk_tier,
	                 open_obligations_count, policy_violations_count, audit_exceptions_count,
	                 privacy_incidents_count, tax_penalties_count, status, evaluated_at, created_at
	          FROM risk_score_assessments WHERE tenant_id = $1`

	args := []interface{}{tenantID}
	if legalEntityID != "" {
		args = append(args, legalEntityID)
		query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
	}
	if tier != "" {
		args = append(args, tier)
		query += fmt.Sprintf(" AND risk_tier = $%d", len(args))
	}
	query += " ORDER BY evaluated_at DESC"

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var assessments []domain.RiskScoreAssessment
	for rows.Next() {
		var a domain.RiskScoreAssessment
		err := rows.Scan(
			&a.ID, &a.TenantID, &a.LegalEntityID, &a.AssessmentName, &a.CompositeRiskScore, &a.RiskTier,
			&a.OpenObligationsCount, &a.PolicyViolationsCount, &a.AuditExceptionsCount,
			&a.PrivacyIncidentsCount, &a.TaxPenaltiesCount, &a.Status, &a.EvaluatedAt, &a.CreatedAt,
		)
		if err == nil {
			assessments = append(assessments, a)
		}
	}

	return assessments, nil
}

func (s *PgStore) CreateThresholdRule(ctx context.Context, tenantID string, rule *domain.RiskThresholdRule) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return err
	}

	rule.ID = uuid.New().String()
	rule.TenantID = tenantID
	rule.CreatedAt = time.Now()

	query := `INSERT INTO risk_threshold_rules (
		id, tenant_id, rule_name, risk_category, high_threshold, critical_threshold, notification_channel, is_active, created_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	_, err = tx.Exec(ctx, query, rule.ID, tenantID, rule.RuleName, string(rule.RiskCategory), rule.HighThreshold, rule.CriticalThreshold, rule.NotificationChannel, rule.IsActive, rule.CreatedAt)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *PgStore) ListThresholdRules(ctx context.Context, tenantID string) ([]domain.RiskThresholdRule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return nil, err
	}

	query := `SELECT id, tenant_id, rule_name, risk_category, high_threshold, critical_threshold, notification_channel, is_active, created_at
	          FROM risk_threshold_rules WHERE tenant_id = $1 ORDER BY created_at DESC`

	rows, err := tx.Query(ctx, query, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []domain.RiskThresholdRule
	for rows.Next() {
		var r domain.RiskThresholdRule
		if err := rows.Scan(&r.ID, &r.TenantID, &r.RuleName, &r.RiskCategory, &r.HighThreshold, &r.CriticalThreshold, &r.NotificationChannel, &r.IsActive, &r.CreatedAt); err == nil {
			rules = append(rules, r)
		}
	}

	return rules, nil
}

func (s *PgStore) ArchiveAssessment(ctx context.Context, tenantID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID); err != nil {
		return err
	}

	cmd, err := tx.Exec(ctx, "UPDATE risk_score_assessments SET status = 'ARCHIVED' WHERE id = $1", id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return fmt.Errorf("assessment not found")
	}

	return tx.Commit(ctx)
}

// In-Memory Store for Testing & Fallback
type MemoryStore struct {
	mu          sync.RWMutex
	assessments map[string]*domain.RiskScoreAssessment
	rules       map[string]*domain.RiskThresholdRule
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		assessments: make(map[string]*domain.RiskScoreAssessment),
		rules:       make(map[string]*domain.RiskThresholdRule),
	}
}

func (m *MemoryStore) CreateAssessment(ctx context.Context, tenantID string, assessment *domain.RiskScoreAssessment, breakdowns []domain.RiskFactorBreakdown) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	assessment.ID = uuid.New().String()
	assessment.TenantID = tenantID
	assessment.EvaluatedAt = time.Now()
	assessment.CreatedAt = time.Now()

	for i := range breakdowns {
		breakdowns[i].ID = uuid.New().String()
		breakdowns[i].TenantID = tenantID
		breakdowns[i].AssessmentID = assessment.ID
		breakdowns[i].CreatedAt = time.Now()
	}

	assessment.FactorBreakdowns = breakdowns
	m.assessments[assessment.ID] = assessment
	return nil
}

func (m *MemoryStore) GetAssessmentByID(ctx context.Context, tenantID, id string) (*domain.RiskScoreAssessment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	a, exists := m.assessments[id]
	if !exists || a.TenantID != tenantID {
		return nil, fmt.Errorf("assessment not found")
	}
	return a, nil
}

func (m *MemoryStore) ListAssessments(ctx context.Context, tenantID, legalEntityID, tier string) ([]domain.RiskScoreAssessment, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []domain.RiskScoreAssessment
	for _, a := range m.assessments {
		if a.TenantID != tenantID {
			continue
		}
		if legalEntityID != "" && a.LegalEntityID != legalEntityID {
			continue
		}
		if tier != "" && string(a.RiskTier) != tier {
			continue
		}
		result = append(result, *a)
	}
	return result, nil
}

func (m *MemoryStore) CreateThresholdRule(ctx context.Context, tenantID string, rule *domain.RiskThresholdRule) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rule.ID = uuid.New().String()
	rule.TenantID = tenantID
	rule.CreatedAt = time.Now()
	m.rules[rule.ID] = rule
	return nil
}

func (m *MemoryStore) ListThresholdRules(ctx context.Context, tenantID string) ([]domain.RiskThresholdRule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []domain.RiskThresholdRule
	for _, r := range m.rules {
		if r.TenantID == tenantID {
			result = append(result, *r)
		}
	}
	return result, nil
}

func (m *MemoryStore) ArchiveAssessment(ctx context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, exists := m.assessments[id]
	if !exists || a.TenantID != tenantID {
		return fmt.Errorf("assessment not found")
	}

	a.Status = "ARCHIVED"
	return nil
}
