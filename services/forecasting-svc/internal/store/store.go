// Package store is the Postgres persistence layer.
//
// Every method runs inside ONE transaction that first declares the caller's
// tenant for row-level security:
//
//	SELECT set_config('app.tenant_id', $1, true)
//
// and every query additionally filters on tenant_id explicitly. A row from
// another tenant is therefore invisible twice over and surfaces as 404.
//
// Write commands follow the same step order everywhere:
//  1. claim the Idempotency-Key (a replay short-circuits here),
//  2. load the aggregate FOR UPDATE and check If-Match / state / SoD rules,
//  3. write the change,
//  4. enqueue the domain event in the outbox (same transaction),
//  5. record the idempotent result and commit.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/forecasting-svc/internal/domain"
)

// Command carries the server-resolved context of one write request.
type Command struct {
	TenantID      string
	LegalEntityID string // X-Legal-Entity-Id header
	ActorID       string // X-Principal-Id (or X-Workload-Id)
	CorrelationID string
	Idempotency   domain.IdempotencyClaim
	// ExpectedVersion is the version parsed from If-Match; 0 means "not sent".
	ExpectedVersion int
}

// Store is everything the HTTP layer needs from persistence.
type Store interface {
	// FIN-02 forecasts
	CreateForecast(ctx context.Context, cmd Command, req domain.GenerateForecastRequest) (*domain.Forecast, error)
	GetForecast(ctx context.Context, tenantID, forecastID string) (*domain.Forecast, error)
	ListForecasts(ctx context.Context, tenantID string, filter domain.ForecastListFilter) ([]domain.Forecast, string, error)
	RecalculateForecast(ctx context.Context, cmd Command, forecastID string, req domain.RecalculateForecastRequest) (*domain.Forecast, error)
	SubmitForecast(ctx context.Context, cmd Command, forecastID string) (*domain.Forecast, error)
	ApproveForecast(ctx context.Context, cmd Command, forecastID string) (*domain.Forecast, error)
	PublishForecast(ctx context.Context, cmd Command, forecastID string) (*domain.Forecast, error)
	ArchiveForecast(ctx context.Context, cmd Command, forecastID string) (*domain.Forecast, error)
	Reforecast(ctx context.Context, cmd Command, sourceForecastID string, req domain.ReforecastRequest) (*domain.Forecast, error)

	// AI-05 model releases
	RegisterModelRelease(ctx context.Context, cmd Command, req domain.RegisterModelReleaseRequest) (*domain.ForecastModelRelease, error)
	ApproveModelRelease(ctx context.Context, cmd Command, releaseID string, req domain.ApproveModelReleaseRequest) (*domain.ForecastModelRelease, error)
	GetModelRelease(ctx context.Context, tenantID, releaseID string) (*domain.ForecastModelRelease, error)
	ListModelReleases(ctx context.Context, tenantID string, filter domain.ModelReleaseListFilter) ([]domain.ForecastModelRelease, string, error)

	// AI-05 assist jobs
	SuggestDrivers(ctx context.Context, cmd Command, req domain.SuggestDriversRequest) (*domain.ForecastAssistJob, error)
	SuggestRange(ctx context.Context, cmd Command, req domain.SuggestRangeRequest) (*domain.ForecastAssistJob, error)
	GenerateNarrative(ctx context.Context, cmd Command, req domain.GenerateNarrativeRequest) (*domain.ForecastAssistJob, error)
	StartPlannerReview(ctx context.Context, cmd Command, jobID string) (*domain.ForecastAssistJob, error)
	AcceptSuggestion(ctx context.Context, cmd Command, jobID string, req domain.AcceptSuggestionRequest) (*domain.JobDecisionResult, error)
	RejectSuggestion(ctx context.Context, cmd Command, jobID string, req domain.RejectSuggestionRequest) (*domain.JobDecisionResult, error)
	GetAssistJob(ctx context.Context, tenantID, jobID string) (*domain.ForecastAssistJob, error)
	ListAssistJobs(ctx context.Context, tenantID string, filter domain.AssistJobListFilter) ([]domain.ForecastAssistJob, string, error)
	GetSuggestedDrivers(ctx context.Context, tenantID, jobID string) ([]domain.SuggestedDriver, error)
	GetSuggestedRanges(ctx context.Context, tenantID, jobID string) ([]domain.SuggestedRange, error)
	GetEvidenceReferences(ctx context.Context, tenantID, jobID string) ([]domain.EvidenceReference, error)
	GetPlannerDecision(ctx context.Context, tenantID, jobID string) (*domain.PlannerDecision, error)
	GetJobDecisionResult(ctx context.Context, tenantID, jobID string) (*domain.JobDecisionResult, error)
}

// PgStore implements Store on Postgres.
type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

var _ Store = (*PgStore)(nil)

// withTenantTx runs fn in one transaction scoped to tenantID for RLS. The
// transaction commits only if fn returns nil.
func (s *PgStore) withTenantTx(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	if tenantID == "" {
		return errors.New("store: tenant_id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return translatePgError(err)
	}
	return tx.Commit(ctx)
}

// translatePgError turns database rule violations into domain errors so the
// HTTP layer returns 409/422 instead of 500. Any other error is returned as-is
// and becomes a 500.
func translatePgError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "P0001": // RAISE EXCEPTION from a lifecycle trigger
		return domain.NewConflictError("invariant_violation", pgErr.Message)
	case "23505": // unique_violation
		return domain.NewConflictError("duplicate", "a resource with the same identity already exists")
	case "23514", "22P02", "22001", "22003": // check, invalid text, too long, numeric out of range
		return domain.NewValidationError("", fmt.Sprintf("value rejected by database: %s", pgErr.Message))
	default:
		return err
	}
}

// checkExpectedVersion enforces If-Match: when the caller sent a version it
// must equal the aggregate's current version.
func checkExpectedVersion(cmd Command, currentVersion int) error {
	if cmd.ExpectedVersion == 0 {
		return nil
	}
	if cmd.ExpectedVersion != currentVersion {
		return domain.NewStaleVersionError(cmd.ExpectedVersion, currentVersion)
	}
	return nil
}

// pageLimit returns the effective page size.
func pageLimit(page domain.PageRequest) int {
	if page.Limit <= 0 {
		return domain.DefaultPageLimit
	}
	if page.Limit > domain.MaxPageLimit {
		return domain.MaxPageLimit
	}
	return page.Limit
}
