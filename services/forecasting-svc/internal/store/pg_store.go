package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/forecasting-svc/internal/domain"
)

type Store interface {
	CreateForecast(ctx context.Context, tenantID string, model *domain.ForecastModel, projections []domain.ForecastProjection) error
	GetForecastByID(ctx context.Context, tenantID, id string) (*domain.ForecastModel, error)
	ListForecasts(ctx context.Context, tenantID, legalEntityID, domainName, scenario string) ([]domain.ForecastModel, error)
	RecalculateForecast(ctx context.Context, tenantID, id string, growthAdjustment float64, scenario domain.ScenarioType) (*domain.ForecastModel, error)
	ArchiveForecast(ctx context.Context, tenantID, id string) error
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

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID)); err != nil {
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

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID)); err != nil {
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

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID)); err != nil {
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

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID)); err != nil {
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

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID)); err != nil {
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
