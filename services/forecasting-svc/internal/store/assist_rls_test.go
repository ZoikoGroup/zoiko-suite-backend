package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/forecasting-svc/internal/domain"
	"zoiko.io/forecasting-svc/internal/store"
	"zoiko.io/forecasting-svc/internal/testdb"
)

// Real-Postgres tests for forecasting-svc, run as a purpose-created
// NOSUPERUSER NOBYPASSRLS role in a dedicated schema with every up
// migration applied (internal/testdb).
//
// Ported from the pre-rewrite API: the old store took (tenant, req, actor,
// AssistIdempotencyClaim) and returned AssistIdempotentReplayError; the
// rewrite takes a store.Command (tenant, legal entity, actor, idempotency
// claim, If-Match version) and returns *domain.IdempotentReplay. Model
// releases now need an evaluation approval by a second principal before use.

const (
	orgA = "11111111-1111-1111-1111-111111111111"
	orgB = "22222222-2222-2222-2222-222222222222"

	requester = "test-operator"
	evaluator = "evaluator-eve"
	planner   = "reviewer-bob"
)

type fixture struct {
	t     *testing.T
	ctx   context.Context
	admin *pgxpool.Pool
	app   *pgxpool.Pool
	s     *store.PgStore
	mu    sync.Mutex
	keys  int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	admin, app := testdb.Open(t, "forecast_store_it")
	return &fixture{t: t, ctx: context.Background(), admin: admin, app: app, s: store.NewPgStore(app)}
}

// cmd builds a write command with a fresh Idempotency-Key.
func (f *fixture) cmd(org, actor, operation string, expectedVersion int) store.Command {
	f.mu.Lock()
	f.keys++
	n := f.keys
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", operation, n)))
	return store.Command{
		TenantID: org, LegalEntityID: "LE-1001", ActorID: actor, CorrelationID: "corr-test",
		Idempotency: domain.IdempotencyClaim{
			Key: fmt.Sprintf("key-%d", n), PrincipalID: actor, Operation: operation, RequestSHA256: hex.EncodeToString(sum[:]),
		},
		ExpectedVersion: expectedVersion,
	}
}

// errCode returns the domain error code of err ("" when not a domain error).
func errCode(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// registerRelease registers a model release and has a second principal
// approve its evaluation, so it can be used.
func (f *fixture) registerRelease(org, domainName, provider, version string) *domain.ForecastModelRelease {
	f.t.Helper()
	rel, err := f.s.RegisterModelRelease(f.ctx, f.cmd(org, requester, "model_release.register", 0), domain.RegisterModelReleaseRequest{
		DomainName: domainName, ModelProvider: provider, ModelVersion: version,
	})
	if err != nil {
		f.t.Fatalf("register model release: %v", err)
	}
	approved, err := f.s.ApproveModelRelease(f.ctx, f.cmd(org, evaluator, "model_release.approve", rel.Version), rel.ReleaseID,
		domain.ApproveModelReleaseRequest{EvaluationReference: "eval-report-1"})
	if err != nil {
		f.t.Fatalf("approve model release: %v", err)
	}
	return approved
}

func baseDriversRequest(domainName string) domain.SuggestDriversRequest {
	return domain.SuggestDriversRequest{
		JobContext: domain.JobContext{
			DomainName: domainName, ModelProvider: "internal-forecast", ModelVersion: "v1.0",
			PlanVersion: "plan-2026-q1", PlanningPurpose: "quarterly-planning",
		},
		Drivers:  []domain.DriverInput{{DriverName: "headcount_growth", SuggestedValue: 0.05, Rationale: "hiring plan trend"}},
		Evidence: []domain.EvidenceInput{{SourceRef: "hr-system-export-2026-01", Description: "headcount trend export"}},
	}
}

func (f *fixture) suggestDrivers(org string, req domain.SuggestDriversRequest) *domain.ForecastAssistJob {
	f.t.Helper()
	job, err := f.s.SuggestDrivers(f.ctx, f.cmd(org, requester, "assist.suggest_drivers", 0), req)
	if err != nil {
		f.t.Fatalf("suggest drivers: %v", err)
	}
	return job
}
