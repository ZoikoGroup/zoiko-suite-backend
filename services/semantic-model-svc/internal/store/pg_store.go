// Package store is the PgStore persistence layer for semantic-model-svc
// (DATA-05, ZS-SVC-N-001 §4). Every method runs inside one transaction
// that first declares app.tenant_id for RLS, then performs the write.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/semantic-model-svc/internal/domain"
	"zoiko.io/semantic-model-svc/internal/outbox"
)

// Store is the DATA-05 persistence contract.
type Store interface {
	CreateDraftVersion(ctx context.Context, tenantID string, req domain.CreateDraftVersionRequest, actor string, claim domain.IdempotencyClaim) (*domain.SemanticVersion, error)
	AddMetricBinding(ctx context.Context, tenantID string, req domain.AddMetricBindingRequest, actor string, claim domain.IdempotencyClaim) (*domain.MetricBinding, error)
	AddDimensionBinding(ctx context.Context, tenantID string, req domain.AddDimensionBindingRequest, actor string, claim domain.IdempotencyClaim) (*domain.DimensionBinding, error)
	AddCalculationPlan(ctx context.Context, tenantID string, req domain.AddCalculationPlanRequest, actor string, claim domain.IdempotencyClaim) (*domain.CalculationPlan, error)
	RetireMetricBinding(ctx context.Context, tenantID, metricBindingID, actor string, claim domain.IdempotencyClaim) (*domain.MetricBinding, error)
	ValidateCalculationPlan(ctx context.Context, tenantID, versionID, metricKey string) (*domain.ValidationResult, error)
	PublishSemanticVersion(ctx context.Context, tenantID, versionID, actor string, claim domain.IdempotencyClaim) (*domain.SemanticVersion, error)
	DeprecateVersion(ctx context.Context, tenantID, versionID, actor string, claim domain.IdempotencyClaim) (*domain.SemanticVersion, error)

	GetVersion(ctx context.Context, tenantID, versionID string) (*domain.SemanticVersion, error)
	GetMetricBindings(ctx context.Context, tenantID, versionID string) ([]domain.MetricBinding, error)
	GetDimensionBindings(ctx context.Context, tenantID, versionID string) ([]domain.DimensionBinding, error)
	GetCalculationPlans(ctx context.Context, tenantID, versionID string) ([]domain.CalculationPlan, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

var _ Store = (*PgStore)(nil)

func (s *PgStore) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return errors.New("tenant_id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return mapErr(err)
	}
	return tx.Commit(ctx)
}

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case strings.Contains(pgErr.Message, "Published"):
		return fmt.Errorf("%w: %s", domain.ErrVersionPublished, pgErr.Message)
	case strings.Contains(pgErr.Message, "already retired"):
		return fmt.Errorf("%w: %s", domain.ErrVersionAlreadyRetired, pgErr.Message)
	case strings.Contains(pgErr.Message, "immutable"), strings.Contains(pgErr.Message, "cannot be deleted"):
		return fmt.Errorf("%w: %s", domain.ErrImmutableViolation, pgErr.Message)
	}
	return err
}

func claimIdempotency(ctx context.Context, tx pgx.Tx, c domain.IdempotencyClaim) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_id, owner_scope, principal_id, idempotency_key, operation, request_sha256, resource_id)
		VALUES (current_setting('app.tenant_id', true), $1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, owner_scope, principal_id, idempotency_key) DO NOTHING`,
		c.OwnerScope, c.PrincipalID, c.Key, c.Operation, c.RequestSHA256, c.ResourceID)
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var reqHash, resourceID string
	if err := tx.QueryRow(ctx, `
		SELECT request_sha256, resource_id FROM idempotency_keys
		WHERE tenant_id = current_setting('app.tenant_id', true) AND owner_scope = $1 AND principal_id = $2 AND idempotency_key = $3`,
		c.OwnerScope, c.PrincipalID, c.Key).Scan(&reqHash, &resourceID); err != nil {
		return fmt.Errorf("read idempotency claim: %w", err)
	}
	if reqHash != c.RequestSHA256 {
		return domain.ErrIdempotencyKeyReused
	}
	return &domain.IdempotentReplayError{ResourceID: resourceID}
}

func newID(prefix string) string {
	return prefix + uuid.NewString()
}

// ── CreateDraftVersion ───────────────────────────────────────────────────────

const versionColumns = `version_id, tenant_id, model_id, version_number, status, created_at, created_by, published_at, published_by`

func scanVersion(row pgx.Row) (*domain.SemanticVersion, error) {
	var v domain.SemanticVersion
	if err := row.Scan(&v.VersionID, &v.TenantID, &v.ModelID, &v.VersionNumber, &v.Status, &v.CreatedAt, &v.CreatedBy,
		&v.PublishedAt, &v.PublishedBy); err != nil {
		return nil, err
	}
	return &v, nil
}

func loadVersion(ctx context.Context, tx pgx.Tx, versionID string, forUpdate bool) (*domain.SemanticVersion, error) {
	q := `SELECT ` + versionColumns + ` FROM semantic_versions WHERE version_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	v, err := scanVersion(tx.QueryRow(ctx, q, versionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrVersionNotFound
	}
	return v, err
}

func (s *PgStore) GetVersion(ctx context.Context, tenantID, versionID string) (*domain.SemanticVersion, error) {
	var out *domain.SemanticVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		v, err := loadVersion(ctx, tx, versionID, false)
		out = v
		return err
	})
	return out, err
}

// CreateDraftVersion finds-or-creates the named model, then always
// creates a NEW version row (Draft) — bindings are added to it via
// AddMetricBinding/AddDimensionBinding/AddCalculationPlan afterward.
func (s *PgStore) CreateDraftVersion(ctx context.Context, tenantID string, req domain.CreateDraftVersionRequest, actor string, claim domain.IdempotencyClaim) (*domain.SemanticVersion, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.SemanticVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		modelID := newID(domain.PrefixModel)
		tag, err := tx.Exec(ctx, `
			INSERT INTO semantic_models (model_id, tenant_id, name, created_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant_id, name) DO NOTHING`,
			modelID, tenantID, req.ModelName, actor)
		if err != nil {
			return fmt.Errorf("find-or-create model: %w", err)
		}
		if tag.RowsAffected() == 0 {
			if err := tx.QueryRow(ctx, `SELECT model_id FROM semantic_models WHERE tenant_id = $1 AND name = $2`,
				tenantID, req.ModelName).Scan(&modelID); err != nil {
				return fmt.Errorf("look up model: %w", err)
			}
		}

		versionID := newID(domain.PrefixVersion)
		claim.ResourceID = versionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		var nextVersion int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_number), 0) + 1 FROM semantic_versions WHERE tenant_id = $1 AND model_id = $2`,
			tenantID, modelID).Scan(&nextVersion); err != nil {
			return fmt.Errorf("compute next version number: %w", err)
		}

		got, err := scanVersion(tx.QueryRow(ctx, `
			INSERT INTO semantic_versions (version_id, tenant_id, model_id, version_number, status, created_by)
			VALUES ($1, $2, $3, $4, 'Draft', $5)
			RETURNING `+versionColumns,
			versionID, tenantID, modelID, nextVersion, actor))
		if err != nil {
			return fmt.Errorf("insert semantic version: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "semantic_version", AggregateID: got.VersionID,
			EventType: "DATA.SemanticVersionDrafted", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

// ── Bindings ─────────────────────────────────────────────────────────────────

const metricBindingColumns = `metric_binding_id, tenant_id, version_id, metric_key, dataset_version_id, source_column,
	aggregation, unit, currency, sign, retired, created_at, created_by`

func scanMetricBinding(row pgx.Row) (*domain.MetricBinding, error) {
	var m domain.MetricBinding
	if err := row.Scan(&m.MetricBindingID, &m.TenantID, &m.VersionID, &m.MetricKey, &m.DatasetVersionID, &m.SourceColumn,
		&m.Aggregation, &m.Unit, &m.Currency, &m.Sign, &m.Retired, &m.CreatedAt, &m.CreatedBy); err != nil {
		return nil, err
	}
	return &m, nil
}

// AddMetricBinding attaches one metric binding to a Draft/Validating
// version. Duplicate metric_keys within a version ARE allowed to be
// added — the doc's own control is that collisions "block PUBLISH," not
// that they're refused at add-time — so conflict detection lives in
// PublishSemanticVersion, checked against every non-retired binding.
func (s *PgStore) AddMetricBinding(ctx context.Context, tenantID string, req domain.AddMetricBindingRequest, actor string, claim domain.IdempotencyClaim) (*domain.MetricBinding, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.MetricBinding
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		version, err := loadVersion(ctx, tx, req.VersionID, false)
		if err != nil {
			return err
		}
		if version.Status != domain.SemVerDraft && version.Status != domain.SemVerValidating {
			return fmt.Errorf("%w: version is %s", domain.ErrVersionPublished, version.Status)
		}

		id := newID(domain.PrefixMetric)
		claim.ResourceID = id
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		got, err := scanMetricBinding(tx.QueryRow(ctx, `
			INSERT INTO metric_bindings (metric_binding_id, tenant_id, version_id, metric_key, dataset_version_id,
				source_column, aggregation, unit, currency, sign, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING `+metricBindingColumns,
			id, tenantID, req.VersionID, req.MetricKey, req.DatasetVersionID, req.SourceColumn, req.Aggregation,
			req.Unit, req.Currency, req.Sign, actor))
		if err != nil {
			return fmt.Errorf("insert metric binding: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "metric_binding", AggregateID: got.MetricBindingID,
			EventType: "DATA.MetricBindingAdded", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetMetricBindings(ctx context.Context, tenantID, versionID string) ([]domain.MetricBinding, error) {
	var out []domain.MetricBinding
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+metricBindingColumns+` FROM metric_bindings WHERE version_id = $1 ORDER BY created_at`, versionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMetricBinding(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return rows.Err()
	})
	return out, err
}

// RetireMetricBinding marks a binding retired — never deleted, preserving
// evidence — and is only permitted while the owning version is still
// Draft/Validating (once Published, the version's own immutability
// trigger transitively blocks this too).
func (s *PgStore) RetireMetricBinding(ctx context.Context, tenantID, metricBindingID, actor string, claim domain.IdempotencyClaim) (*domain.MetricBinding, error) {
	var out *domain.MetricBinding
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var versionID string
		var retired bool
		if err := tx.QueryRow(ctx, `SELECT version_id, retired FROM metric_bindings WHERE metric_binding_id = $1 FOR UPDATE`,
			metricBindingID).Scan(&versionID, &retired); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrMetricBindingNotFound
			}
			return err
		}
		if retired {
			return domain.ErrVersionAlreadyRetired
		}
		version, err := loadVersion(ctx, tx, versionID, false)
		if err != nil {
			return err
		}
		if version.Status != domain.SemVerDraft && version.Status != domain.SemVerValidating {
			return fmt.Errorf("%w: version is %s", domain.ErrVersionPublished, version.Status)
		}

		claim.ResourceID = metricBindingID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		got, err := scanMetricBinding(tx.QueryRow(ctx, `
			UPDATE metric_bindings SET retired = TRUE WHERE metric_binding_id = $1
			RETURNING `+metricBindingColumns, metricBindingID))
		if err != nil {
			return fmt.Errorf("retire metric binding: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "metric_binding", AggregateID: got.MetricBindingID,
			EventType: "DATA.MetricBindingRetired", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

const dimensionBindingColumns = `dimension_binding_id, tenant_id, version_id, dimension_key, dataset_version_id,
	source_column, hierarchy_level, parent_dimension_key, created_at, created_by`

func scanDimensionBinding(row pgx.Row) (*domain.DimensionBinding, error) {
	var d domain.DimensionBinding
	if err := row.Scan(&d.DimensionBindingID, &d.TenantID, &d.VersionID, &d.DimensionKey, &d.DatasetVersionID,
		&d.SourceColumn, &d.HierarchyLevel, &d.ParentDimensionKey, &d.CreatedAt, &d.CreatedBy); err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *PgStore) AddDimensionBinding(ctx context.Context, tenantID string, req domain.AddDimensionBindingRequest, actor string, claim domain.IdempotencyClaim) (*domain.DimensionBinding, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.DimensionBinding
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		version, err := loadVersion(ctx, tx, req.VersionID, false)
		if err != nil {
			return err
		}
		if version.Status != domain.SemVerDraft && version.Status != domain.SemVerValidating {
			return fmt.Errorf("%w: version is %s", domain.ErrVersionPublished, version.Status)
		}

		id := newID(domain.PrefixDimension)
		claim.ResourceID = id
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		got, err := scanDimensionBinding(tx.QueryRow(ctx, `
			INSERT INTO dimension_bindings (dimension_binding_id, tenant_id, version_id, dimension_key,
				dataset_version_id, source_column, hierarchy_level, parent_dimension_key, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+dimensionBindingColumns,
			id, tenantID, req.VersionID, req.DimensionKey, req.DatasetVersionID, req.SourceColumn, req.HierarchyLevel,
			req.ParentDimensionKey, actor))
		if err != nil {
			return fmt.Errorf("insert dimension binding: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dimension_binding", AggregateID: got.DimensionBindingID,
			EventType: "DATA.DimensionBindingAdded", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetDimensionBindings(ctx context.Context, tenantID, versionID string) ([]domain.DimensionBinding, error) {
	var out []domain.DimensionBinding
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+dimensionBindingColumns+` FROM dimension_bindings WHERE version_id = $1 ORDER BY created_at`, versionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDimensionBinding(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	return out, err
}

// ── Calculation plans ────────────────────────────────────────────────────────

const calcPlanColumns = `calc_plan_id, tenant_id, version_id, metric_key, expression, created_at, created_by`

func scanCalcPlan(row pgx.Row) (*domain.CalculationPlan, error) {
	var c domain.CalculationPlan
	var exprJSON []byte
	if err := row.Scan(&c.CalcPlanID, &c.TenantID, &c.VersionID, &c.MetricKey, &exprJSON, &c.CreatedAt, &c.CreatedBy); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(exprJSON, &c.Expression); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PgStore) AddCalculationPlan(ctx context.Context, tenantID string, req domain.AddCalculationPlanRequest, actor string, claim domain.IdempotencyClaim) (*domain.CalculationPlan, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.CalculationPlan
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		version, err := loadVersion(ctx, tx, req.VersionID, false)
		if err != nil {
			return err
		}
		if version.Status != domain.SemVerDraft && version.Status != domain.SemVerValidating {
			return fmt.Errorf("%w: version is %s", domain.ErrVersionPublished, version.Status)
		}

		id := newID(domain.PrefixCalcPlan)
		claim.ResourceID = id
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		exprJSON, err := json.Marshal(req.Expression)
		if err != nil {
			return err
		}
		got, err := scanCalcPlan(tx.QueryRow(ctx, `
			INSERT INTO calculation_plans (calc_plan_id, tenant_id, version_id, metric_key, expression, created_by)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6)
			RETURNING `+calcPlanColumns,
			id, tenantID, req.VersionID, req.MetricKey, exprJSON, actor))
		if err != nil {
			return fmt.Errorf("insert calculation plan: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "calculation_plan", AggregateID: got.CalcPlanID,
			EventType: "DATA.CalculationPlanAdded", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetCalculationPlans(ctx context.Context, tenantID, versionID string) ([]domain.CalculationPlan, error) {
	var out []domain.CalculationPlan
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+calcPlanColumns+` FROM calculation_plans WHERE version_id = $1 ORDER BY created_at`, versionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCalcPlan(rows)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	return out, err
}

// ValidateCalculationPlan is a pure read: a dry-run check callable any
// time before publish, never mutating state. It checks every operand
// referenced by the named plan resolves to a non-retired MetricBinding in
// the same version.
func (s *PgStore) ValidateCalculationPlan(ctx context.Context, tenantID, versionID, metricKey string) (*domain.ValidationResult, error) {
	var out *domain.ValidationResult
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var exprJSON []byte
		err := tx.QueryRow(ctx, `SELECT expression FROM calculation_plans WHERE version_id = $1 AND metric_key = $2`,
			versionID, metricKey).Scan(&exprJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCalcPlanNotFound
		}
		if err != nil {
			return err
		}
		var expr domain.PlanExpression
		if err := json.Unmarshal(exprJSON, &expr); err != nil {
			return err
		}
		activeKeys, err := loadActiveMetricKeys(ctx, tx, versionID)
		if err != nil {
			return err
		}
		var missing []string
		for _, op := range expr.Operands {
			if !activeKeys[op] {
				missing = append(missing, op)
			}
		}
		out = &domain.ValidationResult{MetricKey: metricKey, Valid: len(missing) == 0, MissingOperands: missing}
		return nil
	})
	return out, err
}

func loadActiveMetricKeys(ctx context.Context, tx pgx.Tx, versionID string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT metric_key FROM metric_bindings WHERE version_id = $1 AND NOT retired`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// ── PublishSemanticVersion ───────────────────────────────────────────────────

// PublishSemanticVersion is where collision detection and calculation
// plan validation are actually enforced — the doc's own "block publish"
// framing, not "block add."
func (s *PgStore) PublishSemanticVersion(ctx context.Context, tenantID, versionID, actor string, claim domain.IdempotencyClaim) (*domain.SemanticVersion, error) {
	var out *domain.SemanticVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		version, err := loadVersion(ctx, tx, versionID, true)
		if err != nil {
			return err
		}
		if version.Status != domain.SemVerDraft && version.Status != domain.SemVerValidating {
			return fmt.Errorf("%w: version is %s", domain.ErrVersionPublished, version.Status)
		}

		bindings, err := loadActiveMetricBindings(ctx, tx, versionID)
		if err != nil {
			return err
		}
		if len(bindings) == 0 {
			return domain.ErrNoBindings
		}
		if conflict := detectCollision(bindings); conflict != "" {
			if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "semantic_version", AggregateID: versionID,
				EventType: "DATA.SemanticConflictDetected", TenantID: &tenantID,
				Payload: map[string]string{"version_id": versionID, "metric_key": conflict}}); err != nil {
				return err
			}
			return fmt.Errorf("%w: metric_key %q", domain.ErrMetricCollision, conflict)
		}

		plans, err := loadCalcPlans(ctx, tx, versionID)
		if err != nil {
			return err
		}
		activeKeys := map[string]bool{}
		for _, b := range bindings {
			activeKeys[b.MetricKey] = true
		}
		for _, p := range plans {
			for _, op := range p.Expression.Operands {
				if !activeKeys[op] {
					return fmt.Errorf("%w: plan for %q references %q", domain.ErrCalcPlanInvalid, p.MetricKey, op)
				}
			}
		}

		claim.ResourceID = versionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		now := time.Now().UTC()
		got, err := scanVersion(tx.QueryRow(ctx, `
			UPDATE semantic_versions SET status = 'Published', published_at = $2, published_by = $3 WHERE version_id = $1
			RETURNING `+versionColumns, versionID, now, actor))
		if err != nil {
			return fmt.Errorf("publish version: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "semantic_version", AggregateID: got.VersionID,
			EventType: "DATA.SemanticVersionPublished", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

func loadActiveMetricBindings(ctx context.Context, tx pgx.Tx, versionID string) ([]domain.MetricBinding, error) {
	rows, err := tx.Query(ctx, `SELECT `+metricBindingColumns+` FROM metric_bindings WHERE version_id = $1 AND NOT retired ORDER BY created_at`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MetricBinding
	for rows.Next() {
		m, err := scanMetricBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func loadCalcPlans(ctx context.Context, tx pgx.Tx, versionID string) ([]domain.CalculationPlan, error) {
	rows, err := tx.Query(ctx, `SELECT `+calcPlanColumns+` FROM calculation_plans WHERE version_id = $1`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CalculationPlan
	for rows.Next() {
		c, err := scanCalcPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// detectCollision returns the first metric_key bound more than once with
// differing unit/aggregation/sign among active bindings — "conflicting
// KPI/metric semantics" (the doc's own named acceptance test). Two
// bindings sharing a metric_key with IDENTICAL semantics are not a
// collision (e.g. the same metric legitimately sourced from two dataset
// versions during a migration window is out of scope here — only
// semantic disagreement blocks).
func detectCollision(bindings []domain.MetricBinding) string {
	type sig struct {
		unit string
		agg  domain.AggregationMethod
		sign domain.MetricSign
	}
	seen := map[string]sig{}
	for _, b := range bindings {
		s := sig{unit: b.Unit, agg: b.Aggregation, sign: b.Sign}
		if prior, ok := seen[b.MetricKey]; ok {
			if prior != s {
				return b.MetricKey
			}
			continue
		}
		seen[b.MetricKey] = s
	}
	return ""
}

// ── DeprecateVersion ─────────────────────────────────────────────────────────

func (s *PgStore) DeprecateVersion(ctx context.Context, tenantID, versionID, actor string, claim domain.IdempotencyClaim) (*domain.SemanticVersion, error) {
	var out *domain.SemanticVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		version, err := loadVersion(ctx, tx, versionID, true)
		if err != nil {
			return err
		}
		if version.Status != domain.SemVerPublished {
			return fmt.Errorf("%w: version is %s, must be Published to deprecate", domain.ErrVersionNotDraft, version.Status)
		}
		claim.ResourceID = versionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := scanVersion(tx.QueryRow(ctx, `
			UPDATE semantic_versions SET status = 'Deprecated' WHERE version_id = $1
			RETURNING `+versionColumns, versionID))
		if err != nil {
			return fmt.Errorf("deprecate version: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "semantic_version", AggregateID: got.VersionID,
			EventType: "DATA.SemanticVersionDeprecated", TenantID: &tenantID, Payload: got})
	})
	return out, err
}
