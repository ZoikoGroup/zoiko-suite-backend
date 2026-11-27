package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/anomaly-detection-svc/internal/domain"
)

// AI-04 AnomalyModel registry and drift metrics.

const resourceTypeModel = "anomaly_model"

const modelColumns = `model_id, tenant_id, domain_name, model_provider, model_version,
	review_threshold_bp, max_drift_bp, drift_policy, status, version,
	created_at, created_by, updated_at, updated_by`

func scanModel(row pgx.Row) (*domain.AnomalyModel, error) {
	var model domain.AnomalyModel
	var driftPolicy, status string
	err := row.Scan(
		&model.ModelID, &model.TenantID, &model.DomainName, &model.ModelProvider, &model.ModelVersion,
		&model.ReviewThresholdBP, &model.MaxDriftBP, &driftPolicy, &status, &model.Version,
		&model.CreatedAt, &model.CreatedBy, &model.UpdatedAt, &model.UpdatedBy,
	)
	if err != nil {
		return nil, err
	}
	model.DriftPolicy = domain.DriftPolicy(driftPolicy)
	model.Status = domain.ModelStatus(status)
	return &model, nil
}

func loadModelByID(ctx context.Context, tx pgx.Tx, tenantID, modelID string, forUpdate bool) (*domain.AnomalyModel, error) {
	query := `SELECT ` + modelColumns + ` FROM anomaly_models WHERE tenant_id = $1 AND model_id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	model, err := scanModel(tx.QueryRow(ctx, query, tenantID, modelID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAnomalyModelNotFound
	}
	return model, err
}

// loadModelByIdentity finds the model a RunDetection names by
// (domain, provider, model_version). Unknown is 422: the run references a
// model that was never registered.
func loadModelByIdentity(ctx context.Context, tx pgx.Tx, tenantID, domainName, provider, modelVersion string) (*domain.AnomalyModel, error) {
	model, err := scanModel(tx.QueryRow(ctx, `
		SELECT `+modelColumns+`
		FROM anomaly_models
		WHERE tenant_id = $1 AND domain_name = $2 AND model_provider = $3 AND model_version = $4
		FOR SHARE`,
		tenantID, domainName, provider, modelVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAnomalyModelNotRegistered
	}
	return model, err
}

// RegisterAnomalyModel creates a model registration. Registering the same
// (domain, provider, model_version) twice is 409: a registered model is
// changed only through UpdateAnomalyModel with If-Match, never silently
// overwritten.
func (s *PgStore) RegisterAnomalyModel(ctx context.Context, cmd domain.CommandContext, req domain.RegisterAnomalyModelRequest) (domain.CommandResult[domain.AnomalyModel], error) {
	var result domain.CommandResult[domain.AnomalyModel]

	err := s.inTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		modelID := newID(domain.PrefixAnomalyModel)

		claim, err := claimIdempotencyKey(ctx, tx, cmd, resourceTypeModel, modelID)
		if err != nil {
			return err
		}
		if claim.IsReplay {
			original, err := loadModelByID(ctx, tx, cmd.TenantID, claim.OriginalResourceID, false)
			if err != nil {
				return err
			}
			result = domain.CommandResult[domain.AnomalyModel]{Resource: original, Replayed: true}
			return nil
		}

		created, err := scanModel(tx.QueryRow(ctx, `
			INSERT INTO anomaly_models
				(model_id, tenant_id, domain_name, model_provider, model_version,
				 review_threshold_bp, max_drift_bp, drift_policy, status, version, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'Active', 1, $9, $9)
			ON CONFLICT (tenant_id, domain_name, model_provider, model_version) DO NOTHING
			RETURNING `+modelColumns,
			modelID, cmd.TenantID, req.DomainName, req.ModelProvider, req.ModelVersion,
			*req.ReviewThresholdBP, *req.MaxDriftBP, string(req.DriftPolicy), cmd.PrincipalID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAnomalyModelExists
		}
		if err != nil {
			return fmt.Errorf("register anomaly model: %w", err)
		}
		result = domain.CommandResult[domain.AnomalyModel]{Resource: created}
		return nil
	})
	return result, err
}

// UpdateAnomalyModel replaces the mutable configuration of a model under
// If-Match. The updater becomes the model's publisher (updated_by) for
// segregation-of-duties purposes.
func (s *PgStore) UpdateAnomalyModel(ctx context.Context, cmd domain.CommandContext, modelID string, req domain.UpdateAnomalyModelRequest) (domain.CommandResult[domain.AnomalyModel], error) {
	var result domain.CommandResult[domain.AnomalyModel]

	err := s.inTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		current, err := loadModelByID(ctx, tx, cmd.TenantID, modelID, true)
		if err != nil {
			return err
		}

		claim, err := claimIdempotencyKey(ctx, tx, cmd, resourceTypeModel, modelID)
		if err != nil {
			return err
		}
		if claim.IsReplay {
			result = domain.CommandResult[domain.AnomalyModel]{Resource: current, Replayed: true}
			return nil
		}

		if err := checkExpectedVersion(cmd.ExpectedVersion, current.Version); err != nil {
			return err
		}

		updated, err := scanModel(tx.QueryRow(ctx, `
			UPDATE anomaly_models
			SET review_threshold_bp = $3, max_drift_bp = $4, drift_policy = $5, status = $6,
			    version = version + 1, updated_at = NOW(), updated_by = $7
			WHERE tenant_id = $1 AND model_id = $2
			RETURNING `+modelColumns,
			cmd.TenantID, modelID, *req.ReviewThresholdBP, *req.MaxDriftBP,
			string(req.DriftPolicy), string(req.Status), cmd.PrincipalID))
		if err != nil {
			return fmt.Errorf("update anomaly model: %w", err)
		}
		result = domain.CommandResult[domain.AnomalyModel]{Resource: updated}
		return nil
	})
	return result, err
}

// disableModelAfterDrift flips a model to Disabled in its own transaction.
// It is used by RunDetection under DriftPolicyDisable: the run itself is
// refused (rolled back), but the disable must persist.
func (s *PgStore) disableModelAfterDrift(ctx context.Context, tenantID, modelID, actor string) error {
	return s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE anomaly_models
			SET status = 'Disabled', version = version + 1, updated_at = NOW(), updated_by = $3
			WHERE tenant_id = $1 AND model_id = $2 AND status = 'Active'`,
			tenantID, modelID, actor)
		if err != nil {
			return fmt.Errorf("disable anomaly model after drift: %w", err)
		}
		return nil
	})
}

// GetAnomalyModel returns one model of the tenant (404 otherwise).
func (s *PgStore) GetAnomalyModel(ctx context.Context, tenantID, modelID string) (*domain.AnomalyModel, error) {
	var model *domain.AnomalyModel
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		loaded, err := loadModelByID(ctx, tx, tenantID, modelID, false)
		model = loaded
		return err
	})
	return model, err
}

// ListAnomalyModels returns one page of models, newest first.
func (s *PgStore) ListAnomalyModels(ctx context.Context, tenantID string, filter domain.ModelFilter, page domain.PageRequest) ([]domain.AnomalyModel, string, error) {
	var models []domain.AnomalyModel
	cursorTime, cursorID := cursorArgs(page)

	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+modelColumns+`
			FROM anomaly_models
			WHERE tenant_id = $1
			  AND ($2 = '' OR domain_name = $2)
			  AND ($3 = '' OR status = $3)
			  AND ($4::timestamptz IS NULL OR (created_at, model_id) < ($4::timestamptz, $5::text))
			ORDER BY created_at DESC, model_id DESC
			LIMIT $6`,
			tenantID, filter.DomainName, string(filter.Status), cursorTime, cursorID, pageLimitPlusOne(page))
		if err != nil {
			return fmt.Errorf("list anomaly models: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			model, err := scanModel(rows)
			if err != nil {
				return fmt.Errorf("scan anomaly model: %w", err)
			}
			models = append(models, *model)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}

	pageRows, nextCursor := splitPage(models, page, func(model domain.AnomalyModel) string {
		return domain.EncodeCursor(model.CreatedAt, model.ModelID)
	})
	return pageRows, nextCursor, nil
}

// GetModelDriftMetrics aggregates the drift observed across the model's runs
// and the review outcomes of its signals (false-positive feedback).
func (s *PgStore) GetModelDriftMetrics(ctx context.Context, tenantID, modelID string) (*domain.DriftMetrics, error) {
	var metrics *domain.DriftMetrics

	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		model, err := loadModelByID(ctx, tx, tenantID, modelID, false)
		if err != nil {
			return err
		}

		result := domain.DriftMetrics{
			ModelID:         model.ModelID,
			ModelVersion:    model.ModelVersion,
			MaxDriftBP:      model.MaxDriftBP,
			DriftPolicy:     model.DriftPolicy,
			ModelStatus:     model.Status,
			SignalsByStatus: map[string]int64{},
		}

		// Run-level drift statistics.
		err = tx.QueryRow(ctx, `
			SELECT COUNT(*),
			       COUNT(*) FILTER (WHERE drift_breached),
			       MAX(observed_drift_bp),
			       AVG(observed_drift_bp)::float8,
			       (SELECT observed_drift_bp FROM detection_runs
			         WHERE tenant_id = $1 AND model_id = $2
			         ORDER BY created_at DESC, run_id DESC LIMIT 1)
			FROM detection_runs
			WHERE tenant_id = $1 AND model_id = $2`,
			tenantID, modelID).Scan(
			&result.RunCount, &result.DriftBreachedRuns,
			&result.MaxObservedDriftBP, &result.AvgObservedDriftBP, &result.LastObservedDriftBP)
		if err != nil {
			return fmt.Errorf("aggregate run drift: %w", err)
		}

		// Signal outcomes, used for false-positive monitoring.
		rows, err := tx.Query(ctx, `
			SELECT s.status, COUNT(*)
			FROM anomaly_signals s
			JOIN detection_runs r ON r.run_id = s.run_id AND r.tenant_id = s.tenant_id
			WHERE s.tenant_id = $1 AND r.model_id = $2
			GROUP BY s.status`,
			tenantID, modelID)
		if err != nil {
			return fmt.Errorf("aggregate signal outcomes: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var status string
			var count int64
			if err := rows.Scan(&status, &count); err != nil {
				return fmt.Errorf("scan signal outcome: %w", err)
			}
			result.SignalsByStatus[status] = count
			result.SignalCount += count
		}
		if err := rows.Err(); err != nil {
			return err
		}

		result.ConfirmedCount = result.SignalsByStatus[string(domain.SignalConfirmed)]
		result.DismissedCount = result.SignalsByStatus[string(domain.SignalDismissed)]
		result.EscalatedCount = result.SignalsByStatus[string(domain.SignalEscalated)]
		closedCount := result.ConfirmedCount + result.DismissedCount
		if closedCount > 0 {
			rate := result.DismissedCount * 10000 / closedCount
			result.FalsePositiveRateBP = &rate
		}

		metrics = &result
		return nil
	})
	return metrics, err
}
