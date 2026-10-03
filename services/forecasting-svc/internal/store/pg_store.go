package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/forecasting-svc/internal/domain"
)

type Store interface {
	CreateForecast(ctx context.Context, tenantID string, model *domain.ForecastModel, projections []domain.ForecastProjection) error
	GetForecastByID(ctx context.Context, tenantID, id string) (*domain.ForecastModel, error)
	ListForecasts(ctx context.Context, tenantID, legalEntityID, domainName, scenario string) ([]domain.ForecastModel, error)
	RecalculateForecast(ctx context.Context, tenantID, id string, growthAdjustment float64, scenario domain.ScenarioType) (*domain.ForecastModel, error)
	ArchiveForecast(ctx context.Context, tenantID, id string) error

	// AI-05 governed advisory layer (additive; see domain/types.go).
	RegisterForecastModelRelease(ctx context.Context, tenantID string, req domain.RegisterForecastModelReleaseRequest, actor string) (*domain.ForecastModelRelease, error)
	SuggestDrivers(ctx context.Context, tenantID string, req domain.SuggestDriversRequest, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error)
	SuggestRange(ctx context.Context, tenantID string, req domain.SuggestRangeRequest, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error)
	GenerateNarrative(ctx context.Context, tenantID string, req domain.GenerateNarrativeRequest, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error)
	StartPlannerReview(ctx context.Context, tenantID, jobID string, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error)
	AcceptSuggestion(ctx context.Context, tenantID, jobID string, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error)
	RejectSuggestion(ctx context.Context, tenantID, jobID string, req domain.RejectForecastSuggestionRequest, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error)
	GetForecastAssistJob(ctx context.Context, tenantID, jobID string) (*domain.ForecastAssistJob, error)
	GetSuggestedDrivers(ctx context.Context, tenantID, jobID string) ([]domain.SuggestedDriver, error)
	GetSuggestedRanges(ctx context.Context, tenantID, jobID string) ([]domain.SuggestedRange, error)
	GetEvidenceReferences(ctx context.Context, tenantID, jobID string) ([]domain.EvidenceReference, error)
	GetPlannerDecision(ctx context.Context, tenantID, jobID string) (*domain.PlannerDecision, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) CreateForecast(ctx context.Context, tenantID string, model *domain.ForecastModel, projections []domain.ForecastProjection) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}

	model.ID = uuid.New().String()
	model.TenantID = tenantID
	model.CreatedAt = time.Now()
	model.UpdatedAt = time.Now()

	queryModel := `
		INSERT INTO forecast_models (
			id, tenant_id, legal_entity_id, model_name, domain, scenario_type, algorithm_type,
			granularity, horizon_periods, historical_start_date, historical_end_date, status, confidence_level, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`

	hStart := model.HistoricalStartDate
	if hStart == "" {
		hStart = time.Now().AddDate(-1, 0, 0).Format("2006-01-02")
	}
	hEnd := model.HistoricalEndDate
	if hEnd == "" {
		hEnd = time.Now().Format("2006-01-02")
	}

	_, err = tx.Exec(ctx, queryModel,
		model.ID, tenantID, model.LegalEntityID, model.ModelName, string(model.Domain),
		string(model.ScenarioType), string(model.AlgorithmType), string(model.Granularity),
		model.HorizonPeriods, hStart, hEnd, model.Status, model.ConfidenceLevel,
		model.CreatedAt, model.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert forecast_model failed: %w", err)
	}

	queryProj := `
		INSERT INTO forecast_projections (
			id, tenant_id, forecast_model_id, period_index, period_start_date, period_end_date,
			projected_amount, confidence_low, confidence_high, variance_margin, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

	for i := range projections {
		proj := &projections[i]
		proj.ID = uuid.New().String()
		proj.TenantID = tenantID
		proj.ForecastModelID = model.ID
		proj.CreatedAt = time.Now()

		_, err := tx.Exec(ctx, queryProj,
			proj.ID, tenantID, model.ID, proj.PeriodIndex, proj.PeriodStartDate, proj.PeriodEndDate,
			proj.ProjectedAmount, proj.ConfidenceLow, proj.ConfidenceHigh, proj.VarianceMargin, proj.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert forecast_projection failed: %w", err)
		}
	}

	model.Projections = projections
	return tx.Commit(ctx)
}

func (s *PgStore) GetForecastByID(ctx context.Context, tenantID, id string) (*domain.ForecastModel, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	var m domain.ForecastModel
	var hStart, hEnd time.Time
	queryModel := `
		SELECT id, tenant_id, legal_entity_id, model_name, domain, scenario_type, algorithm_type,
		       granularity, horizon_periods, historical_start_date, historical_end_date, status, confidence_level, created_at, updated_at
		FROM forecast_models WHERE id = $1`

	err = tx.QueryRow(ctx, queryModel, id).Scan(
		&m.ID, &m.TenantID, &m.LegalEntityID, &m.ModelName, &m.Domain, &m.ScenarioType, &m.AlgorithmType,
		&m.Granularity, &m.HorizonPeriods, &hStart, &hEnd, &m.Status, &m.ConfidenceLevel, &m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("forecast not found: %w", err)
	}
	m.HistoricalStartDate = hStart.Format("2006-01-02")
	m.HistoricalEndDate = hEnd.Format("2006-01-02")

	queryProj := `
		SELECT id, tenant_id, forecast_model_id, period_index, period_start_date, period_end_date,
		       projected_amount, confidence_low, confidence_high, variance_margin, created_at
		FROM forecast_projections WHERE forecast_model_id = $1 ORDER BY period_index ASC`

	rows, err := tx.Query(ctx, queryProj, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var p domain.ForecastProjection
			var pStart, pEnd time.Time
			_ = rows.Scan(&p.ID, &p.TenantID, &p.ForecastModelID, &p.PeriodIndex, &pStart, &pEnd,
				&p.ProjectedAmount, &p.ConfidenceLow, &p.ConfidenceHigh, &p.VarianceMargin, &p.CreatedAt)
			p.PeriodStartDate = pStart.Format("2006-01-02")
			p.PeriodEndDate = pEnd.Format("2006-01-02")
			m.Projections = append(m.Projections, p)
		}
	}

	return &m, nil
}

func (s *PgStore) ListForecasts(ctx context.Context, tenantID, legalEntityID, domainName, scenario string) ([]domain.ForecastModel, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	query := `SELECT id, tenant_id, legal_entity_id, model_name, domain, scenario_type, algorithm_type,
	                 granularity, horizon_periods, historical_start_date, historical_end_date, status, confidence_level, created_at, updated_at
	          FROM forecast_models WHERE tenant_id = $1`

	args := []interface{}{tenantID}
	if legalEntityID != "" {
		args = append(args, legalEntityID)
		query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
	}
	if domainName != "" {
		args = append(args, domainName)
		query += fmt.Sprintf(" AND domain = $%d", len(args))
	}
	if scenario != "" {
		args = append(args, scenario)
		query += fmt.Sprintf(" AND scenario_type = $%d", len(args))
	}
	query += " ORDER BY created_at DESC"

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var models []domain.ForecastModel
	for rows.Next() {
		var m domain.ForecastModel
		var hStart, hEnd time.Time
		err := rows.Scan(
			&m.ID, &m.TenantID, &m.LegalEntityID, &m.ModelName, &m.Domain, &m.ScenarioType, &m.AlgorithmType,
			&m.Granularity, &m.HorizonPeriods, &hStart, &hEnd, &m.Status, &m.ConfidenceLevel, &m.CreatedAt, &m.UpdatedAt,
		)
		if err == nil {
			m.HistoricalStartDate = hStart.Format("2006-01-02")
			m.HistoricalEndDate = hEnd.Format("2006-01-02")
			models = append(models, m)
		}
	}

	return models, nil
}

func (s *PgStore) RecalculateForecast(ctx context.Context, tenantID, id string, growthAdjustment float64, scenario domain.ScenarioType) (*domain.ForecastModel, error) {
	model, err := s.GetForecastByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}

	if scenario != "" {
		model.ScenarioType = scenario
	}

	mult := 1.0 + growthAdjustment
	for i := range model.Projections {
		model.Projections[i].ProjectedAmount = mathRound(model.Projections[i].ProjectedAmount * mult)
		model.Projections[i].ConfidenceLow = mathRound(model.Projections[i].ConfidenceLow * mult)
		model.Projections[i].ConfidenceHigh = mathRound(model.Projections[i].ConfidenceHigh * mult)
	}

	model.UpdatedAt = time.Now()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, err
	}

	_, _ = tx.Exec(ctx, "UPDATE forecast_models SET scenario_type = $1, updated_at = $2 WHERE id = $3", string(model.ScenarioType), model.UpdatedAt, id)
	for _, p := range model.Projections {
		_, _ = tx.Exec(ctx, "UPDATE forecast_projections SET projected_amount = $1, confidence_low = $2, confidence_high = $3 WHERE id = $4", p.ProjectedAmount, p.ConfidenceLow, p.ConfidenceHigh, p.ID)
	}

	_ = tx.Commit(ctx)
	return model, nil
}

func (s *PgStore) ArchiveForecast(ctx context.Context, tenantID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}

	cmd, err := tx.Exec(ctx, "UPDATE forecast_models SET status = 'ARCHIVED', updated_at = NOW() WHERE id = $1", id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return fmt.Errorf("forecast not found")
	}

	return tx.Commit(ctx)
}

func mathRound(val float64) float64 {
	return float64(int(val*100+0.5)) / 100
}

// ─────────────────────────────────────────────────────────────────────────────
// AI-05 governed advisory layer — additive, does not touch the legacy
// CreateForecast/RecalculateForecast/ArchiveForecast methods above.
// Every method runs inside one transaction that first declares
// app.tenant_id for RLS.
// ─────────────────────────────────────────────────────────────────────────────

func (s *PgStore) withAssistTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
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
		return mapAssistErr(err)
	}
	return tx.Commit(ctx)
}

func mapAssistErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	return fmt.Errorf("forecasting-svc: %s", pgErr.Message)
}

func claimAssistIdempotency(ctx context.Context, tx pgx.Tx, c domain.AssistIdempotencyClaim) error {
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
		return domain.ErrAssistIdempotencyKeyReused
	}
	return &domain.AssistIdempotentReplayError{ResourceID: resourceID}
}

func newAssistID(prefix string) string {
	return prefix + uuid.New().String()
}

const forecastModelReleaseColumns = `release_id, tenant_id, domain_name, model_provider, model_version, created_at, created_by`

func scanForecastModelRelease(row pgx.Row) (*domain.ForecastModelRelease, error) {
	var m domain.ForecastModelRelease
	if err := row.Scan(&m.ReleaseID, &m.TenantID, &m.DomainName, &m.ModelProvider, &m.ModelVersion, &m.CreatedAt, &m.CreatedBy); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *PgStore) RegisterForecastModelRelease(ctx context.Context, tenantID string, req domain.RegisterForecastModelReleaseRequest, actor string) (*domain.ForecastModelRelease, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ForecastModelRelease
	err := s.withAssistTenant(ctx, tenantID, func(tx pgx.Tx) error {
		id := newAssistID(domain.PrefixForecastModelRelease)
		got, err := scanForecastModelRelease(tx.QueryRow(ctx, `
			INSERT INTO forecast_model_releases (release_id, tenant_id, domain_name, model_provider, model_version, created_by)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, domain_name, model_provider, model_version) DO UPDATE SET created_by = EXCLUDED.created_by
			RETURNING `+forecastModelReleaseColumns,
			id, tenantID, req.DomainName, req.ModelProvider, req.ModelVersion, actor))
		if err != nil {
			return fmt.Errorf("register forecast model release: %w", err)
		}
		out = got
		return nil
	})
	return out, err
}

func loadForecastModelRelease(ctx context.Context, tx pgx.Tx, tenantID, domainName, provider, version string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM forecast_model_releases
		WHERE tenant_id = $1 AND domain_name = $2 AND model_provider = $3 AND model_version = $4)`,
		tenantID, domainName, provider, version).Scan(&exists); err != nil {
		return fmt.Errorf("check forecast model release: %w", err)
	}
	if !exists {
		return domain.ErrForecastModelReleaseNotFound
	}
	return nil
}

const forecastAssistJobColumns = `job_id, tenant_id, domain_name, model_provider, model_version, plan_version, planning_purpose,
	COALESCE(narrative, ''), status, created_at, created_by`

func scanForecastAssistJob(row pgx.Row) (*domain.ForecastAssistJob, error) {
	var j domain.ForecastAssistJob
	if err := row.Scan(&j.JobID, &j.TenantID, &j.DomainName, &j.ModelProvider, &j.ModelVersion, &j.PlanVersion, &j.PlanningPurpose,
		&j.Narrative, &j.Status, &j.CreatedAt, &j.CreatedBy); err != nil {
		return nil, err
	}
	return &j, nil
}

func loadAssistJobForUpdate(ctx context.Context, tx pgx.Tx, tenantID, jobID string) (*domain.ForecastAssistJob, error) {
	j, err := scanForecastAssistJob(tx.QueryRow(ctx, `SELECT `+forecastAssistJobColumns+` FROM forecast_assist_jobs WHERE tenant_id = $1 AND job_id = $2 FOR UPDATE`,
		tenantID, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrForecastAssistJobNotFound
	}
	return j, err
}

// ensureAssistJob either creates a new job (jobID == "") against a
// registered model release, or loads an existing one for update and
// confirms it is still open for new suggestions (Queued or Generated).
// When loading an existing job, the caller's own domain/provider/
// version/plan/purpose fields are ignored — the job's own frozen
// context is authoritative.
func ensureAssistJob(ctx context.Context, tx pgx.Tx, tenantID, jobID, domainName, provider, version, planVersion, purpose, actor string) (*domain.ForecastAssistJob, bool, error) {
	if jobID != "" {
		job, err := loadAssistJobForUpdate(ctx, tx, tenantID, jobID)
		if err != nil {
			return nil, false, err
		}
		if job.Status != domain.AssistJobQueued && job.Status != domain.AssistJobGenerated {
			return nil, false, domain.ErrForecastJobNotOpen
		}
		return job, false, nil
	}
	if err := loadForecastModelRelease(ctx, tx, tenantID, domainName, provider, version); err != nil {
		return nil, false, err
	}
	newJobID := newAssistID(domain.PrefixForecastAssistJob)
	job, err := scanForecastAssistJob(tx.QueryRow(ctx, `
		INSERT INTO forecast_assist_jobs (job_id, tenant_id, domain_name, model_provider, model_version, plan_version, planning_purpose, status, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'Generated', $8)
		RETURNING `+forecastAssistJobColumns,
		newJobID, tenantID, domainName, provider, version, planVersion, purpose, actor))
	if err != nil {
		return nil, false, fmt.Errorf("create forecast assist job: %w", err)
	}
	return job, true, nil
}

func insertEvidence(ctx context.Context, tx pgx.Tx, tenantID, jobID string, evidence []domain.EvidenceInput) error {
	for _, e := range evidence {
		evID := newAssistID(domain.PrefixEvidenceReference)
		if _, err := tx.Exec(ctx, `
			INSERT INTO evidence_references (evidence_id, tenant_id, job_id, source_ref, description)
			VALUES ($1, $2, $3, $4, $5)`,
			evID, tenantID, jobID, e.SourceRef, e.Description); err != nil {
			return fmt.Errorf("insert evidence reference: %w", err)
		}
	}
	return nil
}

// SuggestDrivers either starts a new job or appends drivers to an
// existing open one. Refuses outright against an unregistered model
// release before any job is created.
func (s *PgStore) SuggestDrivers(ctx context.Context, tenantID string, req domain.SuggestDriversRequest, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ForecastAssistJob
	err := s.withAssistTenant(ctx, tenantID, func(tx pgx.Tx) error {
		job, created, err := ensureAssistJob(ctx, tx, tenantID, req.JobID, req.DomainName, req.ModelProvider, req.ModelVersion, req.PlanVersion, req.PlanningPurpose, actor)
		if err != nil {
			return err
		}
		claim.ResourceID = job.JobID
		if err := claimAssistIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		_ = created
		for _, d := range req.Drivers {
			driverID := newAssistID(domain.PrefixSuggestedDriver)
			if _, err := tx.Exec(ctx, `
				INSERT INTO suggested_drivers (driver_id, tenant_id, job_id, driver_name, suggested_value, rationale)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				driverID, tenantID, job.JobID, d.DriverName, d.SuggestedValue, d.Rationale); err != nil {
				return fmt.Errorf("insert suggested driver: %w", err)
			}
		}
		if err := insertEvidence(ctx, tx, tenantID, job.JobID, req.Evidence); err != nil {
			return err
		}
		out = job
		return nil
	})
	return out, err
}

// SuggestRange either starts a new job or appends ranges to an
// existing open one.
func (s *PgStore) SuggestRange(ctx context.Context, tenantID string, req domain.SuggestRangeRequest, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ForecastAssistJob
	err := s.withAssistTenant(ctx, tenantID, func(tx pgx.Tx) error {
		job, _, err := ensureAssistJob(ctx, tx, tenantID, req.JobID, req.DomainName, req.ModelProvider, req.ModelVersion, req.PlanVersion, req.PlanningPurpose, actor)
		if err != nil {
			return err
		}
		claim.ResourceID = job.JobID
		if err := claimAssistIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		for _, rg := range req.Ranges {
			rangeID := newAssistID(domain.PrefixSuggestedRange)
			if _, err := tx.Exec(ctx, `
				INSERT INTO suggested_ranges (range_id, tenant_id, job_id, period_label, low_value, high_value, confidence_note)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				rangeID, tenantID, job.JobID, rg.PeriodLabel, rg.LowValue, rg.HighValue, rg.ConfidenceNote); err != nil {
				return fmt.Errorf("insert suggested range: %w", err)
			}
		}
		if err := insertEvidence(ctx, tx, tenantID, job.JobID, req.Evidence); err != nil {
			return err
		}
		out = job
		return nil
	})
	return out, err
}

// GenerateNarrative sets a job's narrative exactly once.
func (s *PgStore) GenerateNarrative(ctx context.Context, tenantID string, req domain.GenerateNarrativeRequest, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ForecastAssistJob
	err := s.withAssistTenant(ctx, tenantID, func(tx pgx.Tx) error {
		job, _, err := ensureAssistJob(ctx, tx, tenantID, req.JobID, req.DomainName, req.ModelProvider, req.ModelVersion, req.PlanVersion, req.PlanningPurpose, actor)
		if err != nil {
			return err
		}
		claim.ResourceID = job.JobID
		if err := claimAssistIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if job.Narrative != "" {
			return domain.ErrNarrativeAlreadySet
		}
		updated, err := scanForecastAssistJob(tx.QueryRow(ctx, `UPDATE forecast_assist_jobs SET narrative = $2 WHERE job_id = $1 RETURNING `+forecastAssistJobColumns,
			job.JobID, req.NarrativeText))
		if err != nil {
			return fmt.Errorf("generate narrative: %w", err)
		}
		if err := insertEvidence(ctx, tx, tenantID, job.JobID, req.Evidence); err != nil {
			return err
		}
		out = updated
		return nil
	})
	return out, err
}

// StartPlannerReview moves a Generated job into PlannerReview — a
// human has started looking at it.
func (s *PgStore) StartPlannerReview(ctx context.Context, tenantID, jobID string, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error) {
	var out *domain.ForecastAssistJob
	err := s.withAssistTenant(ctx, tenantID, func(tx pgx.Tx) error {
		job, err := loadAssistJobForUpdate(ctx, tx, tenantID, jobID)
		if err != nil {
			return err
		}
		claim.ResourceID = jobID
		if err := claimAssistIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if job.Status != domain.AssistJobGenerated {
			return domain.ErrForecastJobNotGenerated
		}
		updated, err := scanForecastAssistJob(tx.QueryRow(ctx, `UPDATE forecast_assist_jobs SET status = 'PlannerReview' WHERE job_id = $1 RETURNING `+forecastAssistJobColumns, jobID))
		if err != nil {
			return fmt.Errorf("start planner review: %w", err)
		}
		out = updated
		return nil
	})
	return out, err
}

// AcceptSuggestion records the planner's acceptance. It writes only
// this layer's own planner_decisions row — never forecast_models,
// forecast_projections, or any accounting table: "accepted suggestions
// create normal FIN candidates, never accounting entries" holds
// because this store has no command capable of writing either.
func (s *PgStore) AcceptSuggestion(ctx context.Context, tenantID, jobID string, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error) {
	var out *domain.ForecastAssistJob
	err := s.withAssistTenant(ctx, tenantID, func(tx pgx.Tx) error {
		job, err := loadAssistJobForUpdate(ctx, tx, tenantID, jobID)
		if err != nil {
			return err
		}
		claim.ResourceID = jobID
		if err := claimAssistIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if job.Status != domain.AssistJobPlannerReview {
			return domain.ErrForecastJobNotInReview
		}
		updated, err := scanForecastAssistJob(tx.QueryRow(ctx, `UPDATE forecast_assist_jobs SET status = 'Accepted' WHERE job_id = $1 RETURNING `+forecastAssistJobColumns, jobID))
		if err != nil {
			return fmt.Errorf("accept suggestion: %w", err)
		}
		if err := recordPlannerDecision(ctx, tx, tenantID, jobID, domain.PlannerDecisionAccepted, "", actor); err != nil {
			return err
		}
		out = updated
		return nil
	})
	return out, err
}

// RejectSuggestion records the planner's rejection, with a mandatory
// reason.
func (s *PgStore) RejectSuggestion(ctx context.Context, tenantID, jobID string, req domain.RejectForecastSuggestionRequest, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ForecastAssistJob
	err := s.withAssistTenant(ctx, tenantID, func(tx pgx.Tx) error {
		job, err := loadAssistJobForUpdate(ctx, tx, tenantID, jobID)
		if err != nil {
			return err
		}
		claim.ResourceID = jobID
		if err := claimAssistIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if job.Status != domain.AssistJobPlannerReview {
			return domain.ErrForecastJobNotInReview
		}
		updated, err := scanForecastAssistJob(tx.QueryRow(ctx, `UPDATE forecast_assist_jobs SET status = 'Rejected' WHERE job_id = $1 RETURNING `+forecastAssistJobColumns, jobID))
		if err != nil {
			return fmt.Errorf("reject suggestion: %w", err)
		}
		if err := recordPlannerDecision(ctx, tx, tenantID, jobID, domain.PlannerDecisionRejected, req.Reason, actor); err != nil {
			return err
		}
		out = updated
		return nil
	})
	return out, err
}

func recordPlannerDecision(ctx context.Context, tx pgx.Tx, tenantID, jobID string, decision domain.PlannerDecisionType, reason, actor string) error {
	decID := newAssistID(domain.PrefixPlannerDecision)
	var reasonVal *string
	if reason != "" {
		reasonVal = &reason
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO planner_decisions (decision_id, tenant_id, job_id, decision, reason, actor)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		decID, tenantID, jobID, decision, reasonVal, actor)
	if err != nil {
		return fmt.Errorf("record planner decision: %w", err)
	}
	return nil
}

func (s *PgStore) GetForecastAssistJob(ctx context.Context, tenantID, jobID string) (*domain.ForecastAssistJob, error) {
	var out *domain.ForecastAssistJob
	err := s.withAssistTenant(ctx, tenantID, func(tx pgx.Tx) error {
		j, err := scanForecastAssistJob(tx.QueryRow(ctx, `SELECT `+forecastAssistJobColumns+` FROM forecast_assist_jobs WHERE tenant_id = $1 AND job_id = $2`, tenantID, jobID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrForecastAssistJobNotFound
		}
		out = j
		return err
	})
	return out, err
}

func (s *PgStore) GetSuggestedDrivers(ctx context.Context, tenantID, jobID string) ([]domain.SuggestedDriver, error) {
	var out []domain.SuggestedDriver
	err := s.withAssistTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT driver_id, tenant_id, job_id, driver_name, suggested_value, rationale, created_at
			FROM suggested_drivers WHERE tenant_id = $1 AND job_id = $2 ORDER BY created_at`, tenantID, jobID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.SuggestedDriver
			if err := rows.Scan(&d.DriverID, &d.TenantID, &d.JobID, &d.DriverName, &d.SuggestedValue, &d.Rationale, &d.CreatedAt); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) GetSuggestedRanges(ctx context.Context, tenantID, jobID string) ([]domain.SuggestedRange, error) {
	var out []domain.SuggestedRange
	err := s.withAssistTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT range_id, tenant_id, job_id, period_label, low_value, high_value, confidence_note, created_at
			FROM suggested_ranges WHERE tenant_id = $1 AND job_id = $2 ORDER BY created_at`, tenantID, jobID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.SuggestedRange
			if err := rows.Scan(&r.RangeID, &r.TenantID, &r.JobID, &r.PeriodLabel, &r.LowValue, &r.HighValue, &r.ConfidenceNote, &r.CreatedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) GetEvidenceReferences(ctx context.Context, tenantID, jobID string) ([]domain.EvidenceReference, error) {
	var out []domain.EvidenceReference
	err := s.withAssistTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT evidence_id, tenant_id, job_id, source_ref, description, created_at
			FROM evidence_references WHERE tenant_id = $1 AND job_id = $2 ORDER BY created_at`, tenantID, jobID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.EvidenceReference
			if err := rows.Scan(&e.EvidenceID, &e.TenantID, &e.JobID, &e.SourceRef, &e.Description, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) GetPlannerDecision(ctx context.Context, tenantID, jobID string) (*domain.PlannerDecision, error) {
	var out *domain.PlannerDecision
	err := s.withAssistTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var d domain.PlannerDecision
		var reason *string
		err := tx.QueryRow(ctx, `SELECT decision_id, tenant_id, job_id, decision, reason, actor, decided_at
			FROM planner_decisions WHERE tenant_id = $1 AND job_id = $2`, tenantID, jobID).
			Scan(&d.DecisionID, &d.TenantID, &d.JobID, &d.Decision, &reason, &d.Actor, &d.DecidedAt)
		if err != nil {
			return err
		}
		if reason != nil {
			d.Reason = *reason
		}
		out = &d
		return nil
	})
	return out, err
}

// In-Memory Store for Testing & Fallback
type MemoryStore struct {
	mu     sync.RWMutex
	models map[string]*domain.ForecastModel
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		models: make(map[string]*domain.ForecastModel),
	}
}

func (m *MemoryStore) CreateForecast(ctx context.Context, tenantID string, model *domain.ForecastModel, projections []domain.ForecastProjection) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	model.ID = uuid.New().String()
	model.TenantID = tenantID
	model.CreatedAt = time.Now()
	model.UpdatedAt = time.Now()

	for i := range projections {
		projections[i].ID = uuid.New().String()
		projections[i].TenantID = tenantID
		projections[i].ForecastModelID = model.ID
		projections[i].CreatedAt = time.Now()
	}

	model.Projections = projections
	m.models[model.ID] = model
	return nil
}

func (m *MemoryStore) GetForecastByID(ctx context.Context, tenantID, id string) (*domain.ForecastModel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	model, exists := m.models[id]
	if !exists || model.TenantID != tenantID {
		return nil, fmt.Errorf("forecast not found")
	}
	return model, nil
}

func (m *MemoryStore) ListForecasts(ctx context.Context, tenantID, legalEntityID, domainName, scenario string) ([]domain.ForecastModel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []domain.ForecastModel
	for _, model := range m.models {
		if model.TenantID != tenantID {
			continue
		}
		if legalEntityID != "" && model.LegalEntityID != legalEntityID {
			continue
		}
		if domainName != "" && string(model.Domain) != domainName {
			continue
		}
		if scenario != "" && string(model.ScenarioType) != scenario {
			continue
		}
		result = append(result, *model)
	}
	return result, nil
}

func (m *MemoryStore) RecalculateForecast(ctx context.Context, tenantID, id string, growthAdjustment float64, scenario domain.ScenarioType) (*domain.ForecastModel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	model, exists := m.models[id]
	if !exists || model.TenantID != tenantID {
		return nil, fmt.Errorf("forecast not found")
	}

	if scenario != "" {
		model.ScenarioType = scenario
	}

	mult := 1.0 + growthAdjustment
	for i := range model.Projections {
		model.Projections[i].ProjectedAmount = mathRound(model.Projections[i].ProjectedAmount * mult)
		model.Projections[i].ConfidenceLow = mathRound(model.Projections[i].ConfidenceLow * mult)
		model.Projections[i].ConfidenceHigh = mathRound(model.Projections[i].ConfidenceHigh * mult)
	}

	model.UpdatedAt = time.Now()
	return model, nil
}

func (m *MemoryStore) ArchiveForecast(ctx context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	model, exists := m.models[id]
	if !exists || model.TenantID != tenantID {
		return fmt.Errorf("forecast not found")
	}

	model.Status = "ARCHIVED"
	model.UpdatedAt = time.Now()
	return nil
}

// AI-05 governed advisory layer — not implemented in MemoryStore, which
// exists only as an in-memory fallback/test double for the legacy
// forecast engine above. These stubs exist only to satisfy store.Store.
func (m *MemoryStore) RegisterForecastModelRelease(ctx context.Context, tenantID string, req domain.RegisterForecastModelReleaseRequest, actor string) (*domain.ForecastModelRelease, error) {
	return nil, errors.New("not implemented in MemoryStore")
}
func (m *MemoryStore) SuggestDrivers(ctx context.Context, tenantID string, req domain.SuggestDriversRequest, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error) {
	return nil, errors.New("not implemented in MemoryStore")
}
func (m *MemoryStore) SuggestRange(ctx context.Context, tenantID string, req domain.SuggestRangeRequest, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error) {
	return nil, errors.New("not implemented in MemoryStore")
}
func (m *MemoryStore) GenerateNarrative(ctx context.Context, tenantID string, req domain.GenerateNarrativeRequest, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error) {
	return nil, errors.New("not implemented in MemoryStore")
}
func (m *MemoryStore) StartPlannerReview(ctx context.Context, tenantID, jobID string, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error) {
	return nil, errors.New("not implemented in MemoryStore")
}
func (m *MemoryStore) AcceptSuggestion(ctx context.Context, tenantID, jobID string, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error) {
	return nil, errors.New("not implemented in MemoryStore")
}
func (m *MemoryStore) RejectSuggestion(ctx context.Context, tenantID, jobID string, req domain.RejectForecastSuggestionRequest, actor string, claim domain.AssistIdempotencyClaim) (*domain.ForecastAssistJob, error) {
	return nil, errors.New("not implemented in MemoryStore")
}
func (m *MemoryStore) GetForecastAssistJob(ctx context.Context, tenantID, jobID string) (*domain.ForecastAssistJob, error) {
	return nil, errors.New("not implemented in MemoryStore")
}
func (m *MemoryStore) GetSuggestedDrivers(ctx context.Context, tenantID, jobID string) ([]domain.SuggestedDriver, error) {
	return nil, errors.New("not implemented in MemoryStore")
}
func (m *MemoryStore) GetSuggestedRanges(ctx context.Context, tenantID, jobID string) ([]domain.SuggestedRange, error) {
	return nil, errors.New("not implemented in MemoryStore")
}
func (m *MemoryStore) GetEvidenceReferences(ctx context.Context, tenantID, jobID string) ([]domain.EvidenceReference, error) {
	return nil, errors.New("not implemented in MemoryStore")
}
func (m *MemoryStore) GetPlannerDecision(ctx context.Context, tenantID, jobID string) (*domain.PlannerDecision, error) {
	return nil, errors.New("not implemented in MemoryStore")
}
