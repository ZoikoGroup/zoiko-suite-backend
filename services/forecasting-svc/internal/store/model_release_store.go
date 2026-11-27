package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/forecasting-svc/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// AI-05 model releases: forecast_model_releases.
//
// Lifecycle: PendingEvaluation --approve (different principal)--> Approved.
// Only Approved releases may be used by suggestion commands.
// ─────────────────────────────────────────────────────────────────────────────

const modelReleaseColumns = `
	release_id, tenant_id, domain_name, model_provider, model_version, status, version,
	COALESCE(evaluation_reference, ''), created_by, created_at, COALESCE(approved_by, ''), approved_at`

func scanModelRelease(row pgx.Row) (*domain.ForecastModelRelease, error) {
	var release domain.ForecastModelRelease
	err := row.Scan(&release.ReleaseID, &release.TenantID, &release.DomainName, &release.ModelProvider,
		&release.ModelVersion, &release.Status, &release.Version, &release.EvaluationReference,
		&release.CreatedBy, &release.CreatedAt, &release.ApprovedBy, &release.ApprovedAt)
	if err != nil {
		return nil, err
	}
	return &release, nil
}

func loadModelRelease(ctx context.Context, tx pgx.Tx, tenantID, releaseID string, forUpdate bool) (*domain.ForecastModelRelease, error) {
	query := `SELECT ` + modelReleaseColumns + ` FROM forecast_model_releases WHERE tenant_id = $1 AND release_id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	release, err := scanModelRelease(tx.QueryRow(ctx, query, tenantID, releaseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrModelReleaseNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load model release: %w", err)
	}
	return release, nil
}

// RegisterModelRelease implements POST /v1/forecast-assist/model-releases.
// A (domain, provider, version) combination can be registered once per tenant;
// registering it again is a 409.
func (s *PgStore) RegisterModelRelease(ctx context.Context, cmd Command, req domain.RegisterModelReleaseRequest) (*domain.ForecastModelRelease, error) {
	var registered *domain.ForecastModelRelease
	err := s.withTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		if err := claimIdempotencyKey(ctx, tx, cmd.TenantID, cmd.Idempotency); err != nil {
			return err
		}

		var existingReleaseID string
		err := tx.QueryRow(ctx, `
			SELECT release_id FROM forecast_model_releases
			WHERE tenant_id = $1 AND domain_name = $2 AND model_provider = $3 AND model_version = $4`,
			cmd.TenantID, req.DomainName, req.ModelProvider, req.ModelVersion).Scan(&existingReleaseID)
		if err == nil {
			return domain.NewConflictError("model_release_already_registered",
				"this domain/provider/version is already registered as "+existingReleaseID)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check existing model release: %w", err)
		}

		releaseID := domain.PrefixForecastModelRelease + uuid.New().String()
		registered, err = scanModelRelease(tx.QueryRow(ctx, `
			INSERT INTO forecast_model_releases (release_id, tenant_id, domain_name, model_provider, model_version, status, created_by)
			VALUES ($1, $2, $3, $4, $5, 'PendingEvaluation', $6)
			RETURNING `+modelReleaseColumns,
			releaseID, cmd.TenantID, req.DomainName, req.ModelProvider, req.ModelVersion, cmd.ActorID))
		if err != nil {
			return fmt.Errorf("insert model release: %w", err)
		}
		return recordIdempotentResult(ctx, tx, cmd.TenantID, cmd.Idempotency, releaseID)
	})
	return registered, err
}

// ApproveModelRelease implements POST /v1/forecast-assist/model-releases/{releaseID}:approve.
//
// Segregation of duties: the principal who registered (published) the release
// may not approve its evaluation (spec: "Configuration/model/prompt/rule
// publication separated from evaluation/release approval").
func (s *PgStore) ApproveModelRelease(ctx context.Context, cmd Command, releaseID string, req domain.ApproveModelReleaseRequest) (*domain.ForecastModelRelease, error) {
	var approved *domain.ForecastModelRelease
	err := s.withTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		if err := claimIdempotencyKey(ctx, tx, cmd.TenantID, cmd.Idempotency); err != nil {
			return err
		}
		release, err := loadModelRelease(ctx, tx, cmd.TenantID, releaseID, true)
		if err != nil {
			return err
		}
		if err := checkExpectedVersion(cmd, release.Version); err != nil {
			return err
		}
		if release.Status != domain.ModelReleasePendingEvaluation {
			return domain.NewInvalidTransitionError("model release", string(release.Status), string(domain.ModelReleaseApproved))
		}
		if release.CreatedBy == cmd.ActorID {
			return domain.NewForbiddenError("segregation_of_duties",
				"the principal who registered a model release cannot approve its evaluation")
		}

		approved, err = scanModelRelease(tx.QueryRow(ctx, `
			UPDATE forecast_model_releases
			SET status = 'Approved', version = version + 1, evaluation_reference = $3, approved_by = $4, approved_at = NOW()
			WHERE tenant_id = $1 AND release_id = $2
			RETURNING `+modelReleaseColumns,
			cmd.TenantID, releaseID, req.EvaluationReference, cmd.ActorID))
		if err != nil {
			return fmt.Errorf("approve model release: %w", err)
		}
		return recordIdempotentResult(ctx, tx, cmd.TenantID, cmd.Idempotency, releaseID)
	})
	return approved, err
}

// GetModelRelease implements GET /v1/forecast-assist/model-releases/{releaseID}.
func (s *PgStore) GetModelRelease(ctx context.Context, tenantID, releaseID string) (*domain.ForecastModelRelease, error) {
	var release *domain.ForecastModelRelease
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		loaded, err := loadModelRelease(ctx, tx, tenantID, releaseID, false)
		release = loaded
		return err
	})
	return release, err
}

// ListModelReleases implements GET /v1/forecast-assist/model-releases.
func (s *PgStore) ListModelReleases(ctx context.Context, tenantID string, filter domain.ModelReleaseListFilter) ([]domain.ForecastModelRelease, string, error) {
	limit := pageLimit(filter.Page)
	conditions := []string{"tenant_id = $1"}
	args := []interface{}{tenantID}
	if filter.Status != "" {
		args = append(args, string(filter.Status))
		conditions = append(conditions, fmt.Sprintf("status = $%d", len(args)))
	}
	if filter.Page.HasCursorFilter {
		args = append(args, filter.Page.AfterCreatedAt, filter.Page.AfterID)
		conditions = append(conditions, fmt.Sprintf("(created_at, release_id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	query := `SELECT ` + modelReleaseColumns + ` FROM forecast_model_releases WHERE ` + strings.Join(conditions, " AND ") +
		fmt.Sprintf(` ORDER BY created_at DESC, release_id DESC LIMIT $%d`, len(args))

	releases := []domain.ForecastModelRelease{}
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list model releases: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			release, err := scanModelRelease(rows)
			if err != nil {
				return fmt.Errorf("scan model release: %w", err)
			}
			releases = append(releases, *release)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}

	nextCursor := ""
	if len(releases) > limit {
		releases = releases[:limit]
		last := releases[limit-1]
		nextCursor = domain.EncodeCursor(last.CreatedAt, last.ReleaseID)
	}
	return releases, nextCursor, nil
}

// requireApprovedModelRelease is called before a new assist job is created.
// Unregistered or not-yet-evaluated models are refused with 422.
func requireApprovedModelRelease(ctx context.Context, tx pgx.Tx, tenantID, domainName, provider, version string) error {
	var status string
	err := tx.QueryRow(ctx, `
		SELECT status FROM forecast_model_releases
		WHERE tenant_id = $1 AND domain_name = $2 AND model_provider = $3 AND model_version = $4`,
		tenantID, domainName, provider, version).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return &domain.Error{Kind: domain.KindValidation, Code: "model_release_not_registered", Field: "model_version",
			Message: "this domain/provider/version is not registered; register it and approve its evaluation before use"}
	}
	if err != nil {
		return fmt.Errorf("check model release: %w", err)
	}
	if domain.ModelReleaseStatus(status) != domain.ModelReleaseApproved {
		return &domain.Error{Kind: domain.KindValidation, Code: "model_release_not_evaluated", Field: "model_version",
			Message: "this model release has not passed evaluation approval; it cannot be used yet"}
	}
	return nil
}
