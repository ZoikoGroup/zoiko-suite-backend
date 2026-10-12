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
	"zoiko.io/metric-registry-svc/internal/outbox"
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type Store interface {
	// CreateMetricDefinition inserts the new ACTIVE row and, in the same
	// transaction, an outbox_events row for outboxEvt — so a Kafka outage
	// can never lose the event while the metric definition itself commits.
	CreateMetricDefinition(ctx context.Context, d *domain.ReportMetricDefinition, outboxEvt outbox.Event) error
	GetActiveMetricDefinition(ctx context.Context, metricCode string) (*domain.ReportMetricDefinition, error)
	ListMetricVersions(ctx context.Context, metricCode string) ([]domain.ReportMetricDefinition, error)
	// ListActiveMetricDefinitions returns one row per metric_code — whichever
	// version is currently ACTIVE — for the catalog-browsing read the
	// frontend's metric catalog depends on (previously missing entirely;
	// GET /v1/report-metrics answered 405 Method Not Allowed).
	ListActiveMetricDefinitions(ctx context.Context, limit, offset int) ([]domain.ReportMetricDefinition, error)
	// PublishNewVersion atomically supersedes whatever version is currently
	// ACTIVE for metricCode (if any), inserts newVersion as the new ACTIVE
	// row, and inserts an outbox_events row for outboxEvt — all in one
	// transaction, mirroring policy-svc's ActivateVersion.
	PublishNewVersion(ctx context.Context, newVersion *domain.ReportMetricDefinition, outboxEvt outbox.Event) error
	// FindByIdempotencyKey returns the row previously created/published for
	// (metricCode, idempotencyKey), or nil if none exists yet (not an
	// error) — used to answer "was this a replay?" for INV-08.
	FindByIdempotencyKey(ctx context.Context, metricCode, idempotencyKey string) (*domain.ReportMetricDefinition, error)
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

// FindByIdempotencyKey answers "was this a replay?" directly for INV-08,
// scoped to (metric_code, idempotency_key) the same way the unique index
// is: a retried request always names the metric_code it was trying to
// create or publish a version for.
func (s *PgStore) FindByIdempotencyKey(ctx context.Context, metricCode, idempotencyKey string) (*domain.ReportMetricDefinition, error) {
	if idempotencyKey == "" {
		return nil, nil
	}
	const query = `
		SELECT ` + metricColumns + `
		FROM report_metric_definitions
		WHERE metric_code = $1 AND idempotency_key = $2;`

	row := s.pool.QueryRow(ctx, query, metricCode, idempotencyKey)
	d, err := scanMetric(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find by idempotency key: %w", err)
	}
	return d, nil
}

func (s *PgStore) CreateMetricDefinition(ctx context.Context, d *domain.ReportMetricDefinition, outboxEvt outbox.Event) error {
	dataSources := d.DataSources
	if dataSources == nil {
		dataSources = []string{}
	}
	dataSourcesJSON, err := json.Marshal(dataSources)
	if err != nil {
		return fmt.Errorf("marshal data_sources: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin create metric definition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var idempotencyKey *string
	if d.IdempotencyKey != "" {
		idempotencyKey = &d.IdempotencyKey
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO report_metric_definitions (
			metric_definition_id, metric_code, metric_name, formula_description, data_sources,
			owner_principal_id, intelligence_disclaimer, version, effective_from, created_by_principal_id, idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, d.MetricDefinitionID, d.MetricCode, d.MetricName, d.FormulaDescription, dataSourcesJSON,
		d.OwnerPrincipalID, d.IntelligenceDisclaimer, d.Version, d.EffectiveFrom, d.CreatedByPrincipalID, idempotencyKey,
	)
	if err != nil {
		if isUniqueViolation(err) {
			// Two distinct unique constraints can produce this error (the
			// primary key / metric_code uniqueness, or the (metric_code,
			// idempotency_key) index added for INV-08), and which one
			// Postgres happens to report first is a timing accident, not a
			// reliable signal of which race this is. So the real question —
			// "was this a replay?" — is answered directly: if this call
			// carried an idempotency key and a row now exists under it,
			// this was a replay and INV-08 requires returning that ORIGINAL
			// row, not a conflict error, even though this call lost the
			// race to produce it.
			if d.IdempotencyKey != "" {
				existing, findErr := s.FindByIdempotencyKey(ctx, d.MetricCode, d.IdempotencyKey)
				if findErr != nil {
					return fmt.Errorf("re-read after insert conflict: %w", findErr)
				}
				if existing != nil {
					*d = *existing
					return nil
				}
			}
			return fmt.Errorf("%w: metric_code %s", domain.ErrConflict, d.MetricCode)
		}
		return fmt.Errorf("insert report metric definition: %w", err)
	}

	if err := outbox.Insert(ctx, tx, outboxEvt); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit create metric definition: %w", err)
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

// ListActiveMetricDefinitions returns the current catalog — the one ACTIVE
// row per metric_code — ordered by metric_code for a stable paged read.
// This is the query GET /v1/report-metrics never had: without it, the
// frontend metric catalog's only recourse was GET /{metric_code} for a
// code it would have to already know, which is not a catalog.
func (s *PgStore) ListActiveMetricDefinitions(ctx context.Context, limit, offset int) ([]domain.ReportMetricDefinition, error) {
	const query = `
		SELECT ` + metricColumns + `
		FROM report_metric_definitions
		WHERE definition_status = 'ACTIVE'
		ORDER BY metric_code ASC
		LIMIT $1 OFFSET $2;`

	rows, err := s.pool.Query(ctx, query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list active metric definitions: %w", err)
	}
	defer rows.Close()

	out := make([]domain.ReportMetricDefinition, 0)
	for rows.Next() {
		d, err := scanMetric(rows)
		if err != nil {
			return nil, fmt.Errorf("scan metric definition: %w", err)
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// PublishNewVersion supersedes whatever version is currently ACTIVE for
// this metric_code (if any) and inserts newVersion as ACTIVE, in one
// transaction, superseding first — so the one-active-per-metric_code
// unique index is never violated mid-transaction.
func (s *PgStore) PublishNewVersion(ctx context.Context, newVersion *domain.ReportMetricDefinition, outboxEvt outbox.Event) error {
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

	var idempotencyKey *string
	if newVersion.IdempotencyKey != "" {
		idempotencyKey = &newVersion.IdempotencyKey
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO report_metric_definitions (
			metric_definition_id, metric_code, metric_name, formula_description, data_sources,
			owner_principal_id, intelligence_disclaimer, version, effective_from, created_by_principal_id, idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);
	`, newVersion.MetricDefinitionID, newVersion.MetricCode, newVersion.MetricName, newVersion.FormulaDescription, dataSourcesJSON,
		newVersion.OwnerPrincipalID, newVersion.IntelligenceDisclaimer, newVersion.Version, newVersion.EffectiveFrom, newVersion.CreatedByPrincipalID, idempotencyKey,
	)
	if err != nil {
		// Defense in depth against a concurrent replay racing past the
		// handler's pre-check (see PublishNewVersion's comment): the
		// supersede above is harmless to repeat (it just matches zero
		// rows the second time), but this INSERT's (metric_code,
		// idempotency_key) unique index catches the duplicate — and the
		// same "was this a replay?" re-read CreateMetricDefinition uses
		// applies here.
		if isUniqueViolation(err) && newVersion.IdempotencyKey != "" {
			existing, findErr := s.FindByIdempotencyKey(ctx, newVersion.MetricCode, newVersion.IdempotencyKey)
			if findErr != nil {
				return fmt.Errorf("re-read after insert conflict: %w", findErr)
			}
			if existing != nil {
				*newVersion = *existing
				return nil
			}
		}
		return fmt.Errorf("insert new version: %w", err)
	}

	if err := outbox.Insert(ctx, tx, outboxEvt); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit publish new version: %w", err)
	}
	return nil
}
