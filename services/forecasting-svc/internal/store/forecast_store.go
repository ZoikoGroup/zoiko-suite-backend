package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/forecasting-svc/internal/domain"
	"zoiko.io/forecasting-svc/internal/events"
	"zoiko.io/forecasting-svc/internal/outbox"
)

// ─────────────────────────────────────────────────────────────────────────────
// FIN-02 forecasts: forecast_models + forecast_projections.
// ─────────────────────────────────────────────────────────────────────────────

// forecastColumns lists the forecast_models columns in scanForecast order.
// DATE columns are cast to text so they scan into Go strings.
const forecastColumns = `
	id::text, tenant_id, legal_entity_id, model_name, domain, scenario_type, algorithm_type, granularity,
	horizon_periods, historical_data, historical_start_date::text, historical_end_date::text,
	growth_rate_adjustment, status, version, confidence_level, COALESCE(prior_forecast_id::text, ''),
	COALESCE(metadata, '{}'::jsonb), created_by, COALESCE(submitted_by, ''), COALESCE(approved_by, ''),
	approved_at, COALESCE(published_by, ''), published_at, created_at, updated_at`

func scanForecast(row pgx.Row) (*domain.Forecast, error) {
	var f domain.Forecast
	err := row.Scan(
		&f.ID, &f.TenantID, &f.LegalEntityID, &f.ModelName, &f.Domain, &f.ScenarioType, &f.AlgorithmType, &f.Granularity,
		&f.HorizonPeriods, &f.HistoricalData, &f.HistoricalStartDate, &f.HistoricalEndDate,
		&f.GrowthRateAdjustment, &f.Status, &f.Version, &f.ConfidenceLevel, &f.PriorForecastID,
		&f.Metadata, &f.CreatedBy, &f.SubmittedBy, &f.ApprovedBy,
		&f.ApprovedAt, &f.PublishedBy, &f.PublishedAt, &f.CreatedAt, &f.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if f.Metadata == nil {
		f.Metadata = map[string]interface{}{}
	}
	return &f, nil
}

// loadForecast reads one forecast (with projections) in the tenant. When
// forUpdate is true the row is locked until the transaction ends.
func loadForecast(ctx context.Context, tx pgx.Tx, tenantID, forecastID string, forUpdate bool) (*domain.Forecast, error) {
	query := `SELECT ` + forecastColumns + ` FROM forecast_models WHERE tenant_id = $1 AND id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	forecast, err := scanForecast(tx.QueryRow(ctx, query, tenantID, forecastID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrForecastNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load forecast: %w", err)
	}
	projections, err := loadProjections(ctx, tx, tenantID, forecastID)
	if err != nil {
		return nil, err
	}
	forecast.Projections = projections
	return forecast, nil
}

func loadProjections(ctx context.Context, tx pgx.Tx, tenantID, forecastID string) ([]domain.ForecastProjection, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, tenant_id, forecast_model_id::text, period_index,
		       period_start_date::text, period_end_date::text,
		       projected_amount, confidence_low, confidence_high, variance_margin, created_at
		FROM forecast_projections
		WHERE tenant_id = $1 AND forecast_model_id = $2
		ORDER BY period_index`, tenantID, forecastID)
	if err != nil {
		return nil, fmt.Errorf("load projections: %w", err)
	}
	defer rows.Close()

	projections := []domain.ForecastProjection{}
	for rows.Next() {
		var p domain.ForecastProjection
		if err := rows.Scan(&p.ID, &p.TenantID, &p.ForecastID, &p.PeriodIndex,
			&p.PeriodStartDate, &p.PeriodEndDate,
			&p.ProjectedAmount, &p.ConfidenceLow, &p.ConfidenceHigh, &p.VarianceMargin, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan projection: %w", err)
		}
		projections = append(projections, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projections: %w", err)
	}
	return projections, nil
}

func insertProjections(ctx context.Context, tx pgx.Tx, tenantID, forecastID string, projections []domain.ForecastProjection) error {
	for _, p := range projections {
		_, err := tx.Exec(ctx, `
			INSERT INTO forecast_projections (
				id, tenant_id, forecast_model_id, period_index, period_start_date, period_end_date,
				projected_amount, confidence_low, confidence_high, variance_margin
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			uuid.New(), tenantID, forecastID, p.PeriodIndex, p.PeriodStartDate, p.PeriodEndDate,
			p.ProjectedAmount, p.ConfidenceLow, p.ConfidenceHigh, p.VarianceMargin)
		if err != nil {
			return fmt.Errorf("insert projection: %w", err)
		}
	}
	return nil
}

// enqueueForecastEvent writes a finance.forecast.* event to the outbox.
func enqueueForecastEvent(ctx context.Context, tx pgx.Tx, cmd Command, eventType string, forecast *domain.Forecast) error {
	return outbox.Enqueue(ctx, tx, outbox.Record{
		AggregateType:    "forecast",
		AggregateID:      forecast.ID,
		AggregateVersion: forecast.Version,
		Params: events.PublishParams{
			EventType:     eventType,
			SubjectID:     forecast.ID,
			TenantID:      forecast.TenantID,
			LegalEntityID: forecast.LegalEntityID,
			ActorID:       cmd.ActorID,
			CorrelationID: cmd.CorrelationID,
			Payload: map[string]interface{}{
				"forecast_id":       forecast.ID,
				"status":            forecast.Status,
				"version":           forecast.Version,
				"scenario_type":     forecast.ScenarioType,
				"domain":            forecast.Domain,
				"prior_forecast_id": forecast.PriorForecastID,
			},
		},
	})
}

// newForecastRow is the data needed to insert a brand-new forecast.
type newForecastRow struct {
	LegalEntityID        string
	ModelName            string
	Domain               domain.ForecastDomain
	ScenarioType         domain.ScenarioType
	AlgorithmType        domain.AlgorithmType
	Granularity          domain.Granularity
	HorizonPeriods       int
	HistoricalData       []float64
	HistoricalStartDate  string
	GrowthRateAdjustment float64
	PriorForecastID      string
	Metadata             map[string]interface{}
}

// insertForecastWithProjections computes the projections and inserts the
// forecast in status CALCULATED, version 1. Returns the new forecast id.
func insertForecastWithProjections(ctx context.Context, tx pgx.Tx, cmd Command, row newForecastRow) (string, error) {
	projections := domain.ComputeProjections(domain.ProjectionInput{
		HistoricalData:       row.HistoricalData,
		HistoricalStartDate:  row.HistoricalStartDate,
		HorizonPeriods:       row.HorizonPeriods,
		AlgorithmType:        row.AlgorithmType,
		ScenarioType:         row.ScenarioType,
		GrowthRateAdjustment: row.GrowthRateAdjustment,
	})

	metadata := row.Metadata
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	var priorForecastID *string
	if row.PriorForecastID != "" {
		priorForecastID = &row.PriorForecastID
	}

	forecastID := uuid.New().String()
	_, err := tx.Exec(ctx, `
		INSERT INTO forecast_models (
			id, tenant_id, legal_entity_id, model_name, domain, scenario_type, algorithm_type, granularity,
			horizon_periods, historical_data, historical_start_date, historical_end_date,
			growth_rate_adjustment, status, version, confidence_level, prior_forecast_id, metadata, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'CALCULATED', 1, 95.00, $14, $15, $16)`,
		forecastID, cmd.TenantID, row.LegalEntityID, row.ModelName, string(row.Domain), string(row.ScenarioType),
		string(row.AlgorithmType), string(row.Granularity), row.HorizonPeriods, row.HistoricalData,
		row.HistoricalStartDate, domain.HistoricalEndDate(row.HistoricalStartDate, len(row.HistoricalData)),
		row.GrowthRateAdjustment, priorForecastID, metadata, cmd.ActorID)
	if err != nil {
		return "", fmt.Errorf("insert forecast: %w", err)
	}
	if err := insertProjections(ctx, tx, cmd.TenantID, forecastID, projections); err != nil {
		return "", err
	}
	return forecastID, nil
}

// CreateForecast implements POST /v1/forecasts/generate.
func (s *PgStore) CreateForecast(ctx context.Context, cmd Command, req domain.GenerateForecastRequest) (*domain.Forecast, error) {
	var created *domain.Forecast
	err := s.withTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		if err := claimIdempotencyKey(ctx, tx, cmd.TenantID, cmd.Idempotency); err != nil {
			return err
		}

		forecastID, err := insertForecastWithProjections(ctx, tx, cmd, newForecastRow{
			LegalEntityID:       req.LegalEntityID,
			ModelName:           req.ModelName,
			Domain:              req.Domain,
			ScenarioType:        req.ScenarioType,
			AlgorithmType:       req.AlgorithmType,
			Granularity:         req.Granularity,
			HorizonPeriods:      req.HorizonPeriods,
			HistoricalData:      req.HistoricalData,
			HistoricalStartDate: req.HistoricalStartDate,
			Metadata:            req.Metadata,
		})
		if err != nil {
			return err
		}

		forecast, err := loadForecast(ctx, tx, cmd.TenantID, forecastID, false)
		if err != nil {
			return err
		}
		if err := enqueueForecastEvent(ctx, tx, cmd, events.EventForecastCreated, forecast); err != nil {
			return err
		}
		if err := recordIdempotentResult(ctx, tx, cmd.TenantID, cmd.Idempotency, forecastID); err != nil {
			return err
		}
		created = forecast
		return nil
	})
	return created, err
}

// GetForecast implements GET /v1/forecasts/{id}.
func (s *PgStore) GetForecast(ctx context.Context, tenantID, forecastID string) (*domain.Forecast, error) {
	var forecast *domain.Forecast
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		loaded, err := loadForecast(ctx, tx, tenantID, forecastID, false)
		forecast = loaded
		return err
	})
	return forecast, err
}

// ListForecasts implements GET /v1/forecasts. Projections are not included in
// list rows; fetch a single forecast to see them.
func (s *PgStore) ListForecasts(ctx context.Context, tenantID string, filter domain.ForecastListFilter) ([]domain.Forecast, string, error) {
	limit := pageLimit(filter.Page)
	conditions := []string{"tenant_id = $1"}
	args := []interface{}{tenantID}
	addCondition := func(sqlTemplate string, value interface{}) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(sqlTemplate, len(args)))
	}
	if filter.LegalEntityID != "" {
		addCondition("legal_entity_id = $%d", filter.LegalEntityID)
	}
	if filter.Domain != "" {
		addCondition("domain = $%d", string(filter.Domain))
	}
	if filter.ScenarioType != "" {
		addCondition("scenario_type = $%d", string(filter.ScenarioType))
	}
	if filter.Status != "" {
		addCondition("status = $%d", string(filter.Status))
	}
	if filter.Page.HasCursorFilter {
		args = append(args, filter.Page.AfterCreatedAt, filter.Page.AfterID)
		conditions = append(conditions, fmt.Sprintf("(created_at, id::text) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	query := `SELECT ` + forecastColumns + ` FROM forecast_models WHERE ` + strings.Join(conditions, " AND ") +
		fmt.Sprintf(` ORDER BY created_at DESC, id::text DESC LIMIT $%d`, len(args))

	forecasts := []domain.Forecast{}
	err := s.withTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list forecasts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			forecast, err := scanForecast(rows)
			if err != nil {
				return fmt.Errorf("scan forecast: %w", err)
			}
			forecasts = append(forecasts, *forecast)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}

	nextCursor := ""
	if len(forecasts) > limit {
		forecasts = forecasts[:limit]
		last := forecasts[limit-1]
		nextCursor = domain.EncodeCursor(last.CreatedAt, last.ID)
	}
	return forecasts, nextCursor, nil
}

// RecalculateForecast implements POST /v1/forecasts/{id}/recalculate.
//
// Only a CALCULATED forecast may be recalculated. Projections are recomputed
// from the stored historical data (reproducible), replaced, and the version
// increments.
func (s *PgStore) RecalculateForecast(ctx context.Context, cmd Command, forecastID string, req domain.RecalculateForecastRequest) (*domain.Forecast, error) {
	var recalculated *domain.Forecast
	err := s.withTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		if err := claimIdempotencyKey(ctx, tx, cmd.TenantID, cmd.Idempotency); err != nil {
			return err
		}
		forecast, err := loadForecast(ctx, tx, cmd.TenantID, forecastID, true)
		if err != nil {
			return err
		}
		if err := checkExpectedVersion(cmd, forecast.Version); err != nil {
			return err
		}
		if forecast.Status != domain.ForecastCalculated {
			return domain.NewConflictError("forecast_not_recalculable",
				fmt.Sprintf("forecast is %s; only CALCULATED forecasts can be recalculated (create a reforecast instead)", forecast.Status))
		}

		newScenario := forecast.ScenarioType
		if req.ScenarioType != "" {
			newScenario = req.ScenarioType
		}
		newGrowth := forecast.GrowthRateAdjustment
		if req.GrowthRateAdjustment != nil {
			newGrowth = *req.GrowthRateAdjustment
		}

		projections := domain.ComputeProjections(domain.ProjectionInput{
			HistoricalData:       forecast.HistoricalData,
			HistoricalStartDate:  forecast.HistoricalStartDate,
			HorizonPeriods:       forecast.HorizonPeriods,
			AlgorithmType:        forecast.AlgorithmType,
			ScenarioType:         newScenario,
			GrowthRateAdjustment: newGrowth,
		})
		if _, err := tx.Exec(ctx, `DELETE FROM forecast_projections WHERE tenant_id = $1 AND forecast_model_id = $2`,
			cmd.TenantID, forecastID); err != nil {
			return fmt.Errorf("delete old projections: %w", err)
		}
		if err := insertProjections(ctx, tx, cmd.TenantID, forecastID, projections); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE forecast_models
			SET scenario_type = $3, growth_rate_adjustment = $4, version = version + 1, updated_at = NOW()
			WHERE tenant_id = $1 AND id = $2`,
			cmd.TenantID, forecastID, string(newScenario), newGrowth); err != nil {
			return fmt.Errorf("update forecast: %w", err)
		}

		recalculated, err = loadForecast(ctx, tx, cmd.TenantID, forecastID, false)
		if err != nil {
			return err
		}
		if err := enqueueForecastEvent(ctx, tx, cmd, events.EventForecastCalculated, recalculated); err != nil {
			return err
		}
		return recordIdempotentResult(ctx, tx, cmd.TenantID, cmd.Idempotency, forecastID)
	})
	return recalculated, err
}

// forecastTransition describes one state-machine step.
type forecastTransition struct {
	name        string                  // used in error messages, e.g. "submit"
	allowedFrom []domain.ForecastStatus // statuses the forecast may be in
	to          domain.ForecastStatus
	eventType   string
	// checkRules runs extra business rules (e.g. segregation of duties) on the
	// locked forecast before the update. May be nil.
	checkRules func(forecast *domain.Forecast, cmd Command) error
	// actorColumnsSQL sets the maker/checker columns, e.g.
	// "approved_by = $4, approved_at = NOW()". $4 is always the actor.
	actorColumnsSQL string
}

// applyForecastTransition runs the shared steps of submit/approve/publish/archive.
func (s *PgStore) applyForecastTransition(ctx context.Context, cmd Command, forecastID string, transition forecastTransition,
	afterUpdate func(tx pgx.Tx, forecast *domain.Forecast) error) (*domain.Forecast, error) {
	var updated *domain.Forecast
	err := s.withTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		// Step 1: replay protection.
		if err := claimIdempotencyKey(ctx, tx, cmd.TenantID, cmd.Idempotency); err != nil {
			return err
		}
		// Step 2: lock and check the current state.
		forecast, err := loadForecast(ctx, tx, cmd.TenantID, forecastID, true)
		if err != nil {
			return err
		}
		if err := checkExpectedVersion(cmd, forecast.Version); err != nil {
			return err
		}
		if !statusIn(forecast.Status, transition.allowedFrom) {
			return domain.NewInvalidTransitionError("forecast", string(forecast.Status), string(transition.to))
		}
		if transition.checkRules != nil {
			if err := transition.checkRules(forecast, cmd); err != nil {
				return err
			}
		}
		// Step 3: write the new status (and the maker/checker column, if any).
		updateSQL := `UPDATE forecast_models SET status = $3, version = version + 1, updated_at = NOW()`
		updateArgs := []interface{}{cmd.TenantID, forecastID, string(transition.to)}
		if transition.actorColumnsSQL != "" {
			updateSQL += ", " + transition.actorColumnsSQL
			updateArgs = append(updateArgs, cmd.ActorID)
		}
		updateSQL += ` WHERE tenant_id = $1 AND id = $2`
		if _, err := tx.Exec(ctx, updateSQL, updateArgs...); err != nil {
			return fmt.Errorf("%s forecast: %w", transition.name, err)
		}
		updated, err = loadForecast(ctx, tx, cmd.TenantID, forecastID, false)
		if err != nil {
			return err
		}
		if afterUpdate != nil {
			if err := afterUpdate(tx, updated); err != nil {
				return err
			}
		}
		// Step 4: event + idempotent result.
		if err := enqueueForecastEvent(ctx, tx, cmd, transition.eventType, updated); err != nil {
			return err
		}
		return recordIdempotentResult(ctx, tx, cmd.TenantID, cmd.Idempotency, forecastID)
	})
	return updated, err
}

func statusIn(status domain.ForecastStatus, allowed []domain.ForecastStatus) bool {
	for _, candidate := range allowed {
		if status == candidate {
			return true
		}
	}
	return false
}

// SubmitForecast implements POST /v1/forecasts/{id}/submit: CALCULATED -> REVIEWED.
func (s *PgStore) SubmitForecast(ctx context.Context, cmd Command, forecastID string) (*domain.Forecast, error) {
	return s.applyForecastTransition(ctx, cmd, forecastID, forecastTransition{
		name:            "submit",
		allowedFrom:     []domain.ForecastStatus{domain.ForecastCalculated},
		to:              domain.ForecastReviewed,
		eventType:       events.EventForecastSubmitted,
		actorColumnsSQL: "submitted_by = $4",
	}, nil)
}

// ApproveForecast implements POST /v1/forecasts/{id}/approve: REVIEWED -> APPROVED.
//
// Segregation of duties (maker/checker): the approver must be neither the
// principal who created the forecast nor the one who submitted it.
func (s *PgStore) ApproveForecast(ctx context.Context, cmd Command, forecastID string) (*domain.Forecast, error) {
	return s.applyForecastTransition(ctx, cmd, forecastID, forecastTransition{
		name:            "approve",
		allowedFrom:     []domain.ForecastStatus{domain.ForecastReviewed},
		to:              domain.ForecastApproved,
		eventType:       events.EventForecastApproved,
		actorColumnsSQL: "approved_by = $4, approved_at = NOW()",
		checkRules:      requireApproverIsIndependent,
	}, nil)
}

func requireApproverIsIndependent(forecast *domain.Forecast, cmd Command) error {
	if cmd.ActorID == forecast.CreatedBy || cmd.ActorID == forecast.SubmittedBy {
		return domain.NewForbiddenError("segregation_of_duties",
			"the approver must differ from the principal who prepared or submitted the forecast")
	}
	return nil
}

// PublishForecast implements POST /v1/forecasts/{id}/publish: APPROVED -> PUBLISHED.
// When the forecast is a reforecast of a PUBLISHED forecast, that prior
// forecast becomes SUPERSEDED in the same transaction.
func (s *PgStore) PublishForecast(ctx context.Context, cmd Command, forecastID string) (*domain.Forecast, error) {
	supersedePriorForecast := func(tx pgx.Tx, published *domain.Forecast) error {
		if published.PriorForecastID == "" {
			return nil
		}
		tag, err := tx.Exec(ctx, `
			UPDATE forecast_models SET status = 'SUPERSEDED', version = version + 1, updated_at = NOW()
			WHERE tenant_id = $1 AND id = $2 AND status = 'PUBLISHED'`,
			cmd.TenantID, published.PriorForecastID)
		if err != nil {
			return fmt.Errorf("supersede prior forecast: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil // prior was never published (e.g. still APPROVED): nothing to supersede
		}
		prior, err := loadForecast(ctx, tx, cmd.TenantID, published.PriorForecastID, false)
		if err != nil {
			return err
		}
		return enqueueForecastEvent(ctx, tx, cmd, events.EventForecastSuperseded, prior)
	}

	return s.applyForecastTransition(ctx, cmd, forecastID, forecastTransition{
		name:            "publish",
		allowedFrom:     []domain.ForecastStatus{domain.ForecastApproved},
		to:              domain.ForecastPublished,
		eventType:       events.EventForecastPublished,
		actorColumnsSQL: "published_by = $4, published_at = NOW()",
	}, supersedePriorForecast)
}

// ArchiveForecast implements DELETE /v1/forecasts/{id}: CALCULATED/REVIEWED -> ARCHIVED.
// Approved and published versions are records and cannot be archived.
func (s *PgStore) ArchiveForecast(ctx context.Context, cmd Command, forecastID string) (*domain.Forecast, error) {
	return s.applyForecastTransition(ctx, cmd, forecastID, forecastTransition{
		name:        "archive",
		allowedFrom: []domain.ForecastStatus{domain.ForecastCalculated, domain.ForecastReviewed},
		to:          domain.ForecastArchived,
		eventType:   events.EventForecastArchived,
	}, nil)
}

// Reforecast implements POST /v1/forecasts/{id}/reforecast.
//
// The source forecast (APPROVED or PUBLISHED) is never modified. A new
// CALCULATED forecast is created with prior_forecast_id pointing at the
// source; it must go through submit/approve/publish like any other forecast.
func (s *PgStore) Reforecast(ctx context.Context, cmd Command, sourceForecastID string, req domain.ReforecastRequest) (*domain.Forecast, error) {
	var created *domain.Forecast
	err := s.withTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		if err := claimIdempotencyKey(ctx, tx, cmd.TenantID, cmd.Idempotency); err != nil {
			return err
		}
		source, err := loadForecast(ctx, tx, cmd.TenantID, sourceForecastID, false)
		if err != nil {
			return err
		}
		if source.Status != domain.ForecastApproved && source.Status != domain.ForecastPublished {
			return domain.NewConflictError("forecast_not_reforecastable",
				fmt.Sprintf("forecast is %s; only APPROVED or PUBLISHED forecasts can be reforecast (recalculate a CALCULATED one instead)", source.Status))
		}

		row := newForecastRow{
			LegalEntityID:        source.LegalEntityID,
			ModelName:            source.ModelName,
			Domain:               source.Domain,
			ScenarioType:         source.ScenarioType,
			AlgorithmType:        source.AlgorithmType,
			Granularity:          source.Granularity,
			HorizonPeriods:       source.HorizonPeriods,
			HistoricalData:       source.HistoricalData,
			HistoricalStartDate:  source.HistoricalStartDate,
			GrowthRateAdjustment: source.GrowthRateAdjustment,
			PriorForecastID:      source.ID,
			Metadata:             source.Metadata,
		}
		if req.ModelName != "" {
			row.ModelName = req.ModelName
		}
		if req.ScenarioType != "" {
			row.ScenarioType = req.ScenarioType
		}
		if req.GrowthRateAdjustment != nil {
			row.GrowthRateAdjustment = *req.GrowthRateAdjustment
		}
		if req.HistoricalData != nil {
			row.HistoricalData = req.HistoricalData
		}

		newForecastID, err := insertForecastWithProjections(ctx, tx, cmd, row)
		if err != nil {
			return err
		}
		created, err = loadForecast(ctx, tx, cmd.TenantID, newForecastID, false)
		if err != nil {
			return err
		}
		if err := enqueueForecastEvent(ctx, tx, cmd, events.EventForecastCreated, created); err != nil {
			return err
		}
		return recordIdempotentResult(ctx, tx, cmd.TenantID, cmd.Idempotency, newForecastID)
	})
	return created, err
}
