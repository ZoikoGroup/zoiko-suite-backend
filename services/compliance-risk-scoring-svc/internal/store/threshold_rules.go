package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
)

// thresholdColumns is the SELECT list scanThresholdRule expects, in order.
// DATE columns are read as ::text (YYYY-MM-DD).
const thresholdColumns = `
	id::text, tenant_id, rule_name, risk_category,
	moderate_threshold::text, high_threshold::text, critical_threshold::text,
	COALESCE(notification_channel, ''), status, version,
	effective_from::text, effective_to::text,
	created_by, updated_by, approved_by, approved_at, retired_by, retired_at,
	supersedes_rule_id::text, created_at, updated_at`

func scanThresholdRule(row pgx.Row) (*domain.RiskThresholdRule, error) {
	var rule domain.RiskThresholdRule
	err := row.Scan(
		&rule.ID, &rule.TenantID, &rule.RuleName, &rule.RiskCategory,
		scanDecimal(&rule.ModerateThreshold), scanDecimal(&rule.HighThreshold), scanDecimal(&rule.CriticalThreshold),
		&rule.NotificationChannel, &rule.Status, &rule.Version,
		&rule.EffectiveFrom, &rule.EffectiveTo,
		&rule.CreatedBy, &rule.UpdatedBy, &rule.ApprovedBy, &rule.ApprovedAt, &rule.RetiredBy, &rule.RetiredAt,
		&rule.SupersedesRuleID, &rule.CreatedAt, &rule.UpdatedAt,
	)
	if err != nil {
		return nil, translatePgError(err)
	}
	return &rule, nil
}

// InsertThresholdRule writes a new DRAFT rule. ID and timestamps must be set.
func (q *TenantQueries) InsertThresholdRule(ctx context.Context, rule *domain.RiskThresholdRule) error {
	_, err := q.tx.Exec(ctx, `
		INSERT INTO risk_threshold_rules (
			id, tenant_id, rule_name, risk_category,
			moderate_threshold, high_threshold, critical_threshold,
			notification_channel, status, version, effective_from,
			created_by, updated_by, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7::numeric, $8, $9, $10, $11::date, $12, $13, $14, $15)`,
		rule.ID, q.TenantID, rule.RuleName, string(rule.RiskCategory),
		rule.ModerateThreshold.String(), rule.HighThreshold.String(), rule.CriticalThreshold.String(),
		rule.NotificationChannel, string(rule.Status), rule.Version, rule.EffectiveFrom,
		rule.CreatedBy, rule.UpdatedBy, rule.CreatedAt, rule.UpdatedAt,
	)
	if err != nil {
		return translatePgError(fmt.Errorf("insert threshold rule: %w", err))
	}
	return nil
}

// GetThresholdRule loads one rule; lockForUpdate takes a row lock.
func (q *TenantQueries) GetThresholdRule(ctx context.Context, id string, lockForUpdate bool) (*domain.RiskThresholdRule, error) {
	query := `SELECT ` + thresholdColumns + ` FROM risk_threshold_rules WHERE tenant_id = $1 AND id = $2::uuid`
	if lockForUpdate {
		query += ` FOR UPDATE`
	}
	return scanThresholdRule(q.tx.QueryRow(ctx, query, q.TenantID, id))
}

// UpdateThresholdDraft rewrites the editable fields of a DRAFT rule, but
// only if it is still at expectedVersion (optimistic concurrency).
func (q *TenantQueries) UpdateThresholdDraft(ctx context.Context, rule *domain.RiskThresholdRule, expectedVersion int) error {
	tag, err := q.tx.Exec(ctx, `
		UPDATE risk_threshold_rules
		SET rule_name = $3, risk_category = $4,
		    moderate_threshold = $5::numeric, high_threshold = $6::numeric, critical_threshold = $7::numeric,
		    notification_channel = $8, effective_from = $9::date,
		    updated_by = $10, updated_at = $11, version = version + 1
		WHERE tenant_id = $1 AND id = $2::uuid AND status = 'DRAFT' AND version = $12`,
		q.TenantID, rule.ID, rule.RuleName, string(rule.RiskCategory),
		rule.ModerateThreshold.String(), rule.HighThreshold.String(), rule.CriticalThreshold.String(),
		rule.NotificationChannel, rule.EffectiveFrom, rule.UpdatedBy, rule.UpdatedAt, expectedVersion,
	)
	if err != nil {
		return translatePgError(fmt.Errorf("update threshold rule: %w", err))
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// FindActiveThresholdRule returns the ACTIVE rule for category, locked, or
// ErrNotFound.
func (q *TenantQueries) FindActiveThresholdRule(ctx context.Context, category domain.RiskCategory) (*domain.RiskThresholdRule, error) {
	return scanThresholdRule(q.tx.QueryRow(ctx, `SELECT `+thresholdColumns+`
		FROM risk_threshold_rules
		WHERE tenant_id = $1 AND risk_category = $2 AND status = 'ACTIVE'
		FOR UPDATE`, q.TenantID, string(category)))
}

// SupersedeThresholdRule closes an ACTIVE rule: status SUPERSEDED, in force
// until effectiveTo (exclusive).
func (q *TenantQueries) SupersedeThresholdRule(ctx context.Context, id, effectiveTo string, now time.Time) error {
	tag, err := q.tx.Exec(ctx, `
		UPDATE risk_threshold_rules
		SET status = 'SUPERSEDED', effective_to = $3::date, updated_at = $4, version = version + 1
		WHERE tenant_id = $1 AND id = $2::uuid AND status = 'ACTIVE'`,
		q.TenantID, id, effectiveTo, now)
	if err != nil {
		return translatePgError(fmt.Errorf("supersede threshold rule: %w", err))
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// ApproveThresholdRule moves a DRAFT to ACTIVE.
func (q *TenantQueries) ApproveThresholdRule(ctx context.Context, id, approverID string, supersedesRuleID *string, now time.Time) error {
	tag, err := q.tx.Exec(ctx, `
		UPDATE risk_threshold_rules
		SET status = 'ACTIVE', approved_by = $3, approved_at = $4, supersedes_rule_id = $5::uuid,
		    updated_at = $4, version = version + 1
		WHERE tenant_id = $1 AND id = $2::uuid AND status = 'DRAFT'`,
		q.TenantID, id, approverID, now, supersedesRuleID)
	if err != nil {
		return translatePgError(fmt.Errorf("approve threshold rule: %w", err))
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// RetireThresholdRule moves a DRAFT or ACTIVE rule to RETIRED. effectiveTo
// is set only for a rule that was in force (nil leaves it unchanged).
func (q *TenantQueries) RetireThresholdRule(ctx context.Context, id, retiredBy string, effectiveTo *string, now time.Time) error {
	tag, err := q.tx.Exec(ctx, `
		UPDATE risk_threshold_rules
		SET status = 'RETIRED', retired_by = $3, retired_at = $4,
		    effective_to = COALESCE($5::date, effective_to),
		    updated_at = $4, version = version + 1
		WHERE tenant_id = $1 AND id = $2::uuid AND status IN ('DRAFT', 'ACTIVE')`,
		q.TenantID, id, retiredBy, now, effectiveTo)
	if err != nil {
		return translatePgError(fmt.Errorf("retire threshold rule: %w", err))
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// ThresholdFilter narrows ListThresholdRules.
type ThresholdFilter struct {
	Status       string
	RiskCategory string
}

// ListThresholdRules returns one page of rules, newest first.
func (q *TenantQueries) ListThresholdRules(ctx context.Context, filter ThresholdFilter, page PageRequest) ([]domain.RiskThresholdRule, *PageCursor, error) {
	conditions := []string{"tenant_id = $1"}
	args := []any{q.TenantID}

	if filter.Status != "" {
		args = append(args, filter.Status)
		conditions = append(conditions, fmt.Sprintf("status = $%d", len(args)))
	}
	if filter.RiskCategory != "" {
		args = append(args, filter.RiskCategory)
		conditions = append(conditions, fmt.Sprintf("risk_category = $%d", len(args)))
	}
	if page.Cursor != nil {
		args = append(args, page.Cursor.CreatedAt, page.Cursor.ID)
		conditions = append(conditions, fmt.Sprintf("(created_at, id) < ($%d, $%d::uuid)", len(args)-1, len(args)))
	}
	args = append(args, page.Limit+1)

	rows, err := q.tx.Query(ctx, `SELECT `+thresholdColumns+`
		FROM risk_threshold_rules
		WHERE `+strings.Join(conditions, " AND ")+`
		ORDER BY created_at DESC, id DESC
		LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list threshold rules: %w", err)
	}
	defer rows.Close()

	rules := []domain.RiskThresholdRule{}
	for rows.Next() {
		rule, err := scanThresholdRule(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("scan threshold rule: %w", err)
		}
		rules = append(rules, *rule)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate threshold rules: %w", err)
	}

	return trimPage(rules, page.Limit, func(r domain.RiskThresholdRule) PageCursor {
		return PageCursor{CreatedAt: r.CreatedAt, ID: r.ID}
	})
}

// EffectiveThresholdRules returns, per category, the approved rule in force
// on asOfDate: effective_from <= date AND (effective_to IS NULL OR date <
// effective_to). Categories without a rule are absent from the map.
func (q *TenantQueries) EffectiveThresholdRules(ctx context.Context, asOfDate string) (map[domain.RiskCategory]domain.RiskThresholdRule, error) {
	rows, err := q.tx.Query(ctx, `
		SELECT DISTINCT ON (risk_category) `+thresholdColumns+`
		FROM risk_threshold_rules
		WHERE tenant_id = $1
		  AND approved_at IS NOT NULL
		  AND effective_from <= $2::date
		  AND (effective_to IS NULL OR effective_to > $2::date)
		ORDER BY risk_category, effective_from DESC, approved_at DESC`,
		q.TenantID, asOfDate)
	if err != nil {
		return nil, fmt.Errorf("effective threshold rules: %w", err)
	}
	defer rows.Close()

	effective := map[domain.RiskCategory]domain.RiskThresholdRule{}
	for rows.Next() {
		rule, err := scanThresholdRule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan threshold rule: %w", err)
		}
		effective[rule.RiskCategory] = *rule
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate threshold rules: %w", err)
	}
	return effective, nil
}
