// Package store provides the PostgreSQL implementation of
// metric-registry-svc's persistence layer.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/metric-registry-svc/internal/domain"
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type Store interface {
	CreateMetricDefinition(ctx context.Context, d *domain.ReportMetricDefinition) error
	GetActiveMetricDefinition(ctx context.Context, metricCode string) (*domain.ReportMetricDefinition, error)
	ListMetricVersions(ctx context.Context, metricCode string) ([]domain.ReportMetricDefinition, error)
	// PublishNewVersion atomically supersedes whatever version is currently
	// ACTIVE for metricCode (if any) and inserts newVersion as the new
	// ACTIVE row, in one transaction — mirrors policy-svc's ActivateVersion.
	PublishNewVersion(ctx context.Context, newVersion *domain.ReportMetricDefinition) error
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

const metricColumns = `
	metric_definition_id, metric_code, metric_name, formula_description, data_sources,
	owner_principal_id, intelligence_disclaimer, version, definition_status,
	effective_from, created_at, created_by_principal_id`

func scanMetric(row pgx.Row) (*domain.ReportMetricDefinition, error) {
	d := &domain.ReportMetricDefinition{}
	var dataSources []byte
	err := row.Scan(
		&d.MetricDefinitionID, &d.MetricCode, &d.MetricName, &d.FormulaDescription, &dataSources,
		&d.OwnerPrincipalID, &d.IntelligenceDisclaimer, &d.Version, &d.DefinitionStatus,
		&d.EffectiveFrom, &d.CreatedAt, &d.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(dataSources, &d.DataSources)
	return d, nil
}

func (s *PgStore) CreateMetricDefinition(ctx context.Context, d *domain.ReportMetricDefinition) error {
	dataSources := d.DataSources
	if dataSources == nil {
		dataSources = []string{}
	}
	dataSourcesJSON, err := json.Marshal(dataSources)
	if err != nil {
		return fmt.Errorf("marshal data_sources: %w", err)
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO report_metric_definitions (
			metric_definition_id, metric_code, metric_name, formula_description, data_sources,
			owner_principal_id, intelligence_disclaimer, version, effective_from, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, d.MetricDefinitionID, d.MetricCode, d.MetricName, d.FormulaDescription, dataSourcesJSON,
		d.OwnerPrincipalID, d.IntelligenceDisclaimer, d.Version, d.EffectiveFrom, d.CreatedByPrincipalID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: metric_code %s", domain.ErrConflict, d.MetricCode)
		}
		return fmt.Errorf("insert report metric definition: %w", err)
	}
	return nil
}

func (s *PgStore) GetActiveMetricDefinition(ctx context.Context, metricCode string) (*domain.ReportMetricDefinition, error) {
	const query = `
		SELECT ` + metricColumns + `
		FROM report_metric_definitions
		WHERE metric_code = $1 AND definition_status = 'ACTIVE';`

	row := s.pool.QueryRow(ctx, query, metricCode)
	d, err := scanMetric(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrMetricNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get active metric definition: %w", err)
	}
	return d, nil
}

func (s *PgStore) ListMetricVersions(ctx context.Context, metricCode string) ([]domain.ReportMetricDefinition, error) {
	const query = `
		SELECT ` + metricColumns + `
		FROM report_metric_definitions
		WHERE metric_code = $1
		ORDER BY version DESC;`

	rows, err := s.pool.Query(ctx, query, metricCode)
	if err != nil {
		return nil, fmt.Errorf("list metric versions: %w", err)
	}
	defer rows.Close()

	var out []domain.ReportMetricDefinition
	for rows.Next() {
		d, err := scanMetric(rows)
		if err != nil {
			return nil, fmt.Errorf("scan metric version: %w", err)
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// PublishNewVersion supersedes whatever version is currently ACTIVE for
// this metric_code (if any) and inserts newVersion as ACTIVE, in one
// transaction, superseding first — so the one-active-per-metric_code
// unique index is never violated mid-transaction.
func (s *PgStore) PublishNewVersion(ctx context.Context, newVersion *domain.ReportMetricDefinition) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin publish new version: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		UPDATE report_metric_definitions
		SET definition_status = 'SUPERSEDED'
		WHERE metric_code = $1 AND definition_status = 'ACTIVE';
	`, newVersion.MetricCode)
	if err != nil {
		return fmt.Errorf("supersede prior active version: %w", err)
	}

	dataSources := newVersion.DataSources
	if dataSources == nil {
		dataSources = []string{}
	}
	dataSourcesJSON, err := json.Marshal(dataSources)
	if err != nil {
		return fmt.Errorf("marshal data_sources: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO report_metric_definitions (
			metric_definition_id, metric_code, metric_name, formula_description, data_sources,
			owner_principal_id, intelligence_disclaimer, version, effective_from, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);
	`, newVersion.MetricDefinitionID, newVersion.MetricCode, newVersion.MetricName, newVersion.FormulaDescription, dataSourcesJSON,
		newVersion.OwnerPrincipalID, newVersion.IntelligenceDisclaimer, newVersion.Version, newVersion.EffectiveFrom, newVersion.CreatedByPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("insert new version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit publish new version: %w", err)
	}
	return nil
}
