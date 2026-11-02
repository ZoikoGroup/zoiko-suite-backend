package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/anomaly-detection-svc/internal/domain"
	"zoiko.io/anomaly-detection-svc/internal/store"
	"zoiko.io/anomaly-detection-svc/internal/testdb"
)

// Real-Postgres tests for anomaly-detection-svc. The store runs as a
// purpose-created NOSUPERUSER NOBYPASSRLS role (internal/testdb), in its own
// schema with every up migration applied.
//
// Ported from the pre-rewrite API: the old store took (tenant, req, actor,
// claim) and signalled replays with an error; the rewrite takes a
// domain.CommandContext (tenant, principal, idempotency claim, If-Match
// version) and returns a domain.CommandResult. The behaviour each test
// asserts is unchanged.

const (
	orgA = "11111111-1111-1111-1111-111111111111"
	orgB = "22222222-2222-2222-2222-222222222222"

	submitter = "test-operator"
	reviewer  = "reviewer-bob"
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
	admin, app := testdb.Open(t, "anomaly_store_it")
	return &fixture{t: t, ctx: context.Background(), admin: admin, app: app, s: store.NewPgStore(app)}
}

// cmd builds a command with a fresh Idempotency-Key. expectedVersion is the
// If-Match value (nil for creates).
func (f *fixture) cmd(org, principal, route string, expectedVersion *int) domain.CommandContext {
	f.mu.Lock()
	f.keys++
	n := f.keys
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", route, n)))
	return domain.CommandContext{
		TenantID: org, PrincipalID: principal, CorrelationID: "corr-test",
		Idempotency: domain.IdempotencyClaim{
			RouteTemplate: route, Key: fmt.Sprintf("key-%d", n), RequestSHA256: hex.EncodeToString(sum[:]),
		},
		ExpectedVersion: expectedVersion,
	}
}

func i32(v int32) *int32 { return &v }
func ver(v int) *int     { return &v }

func (f *fixture) registerModel(org, domainName, provider, version string, reviewThresholdBP, maxDriftBP int32) *domain.AnomalyModel {
	f.t.Helper()
	req := domain.RegisterAnomalyModelRequest{
		DomainName: domainName, ModelProvider: provider, ModelVersion: version,
		ReviewThresholdBP: i32(reviewThresholdBP), MaxDriftBP: i32(maxDriftBP),
	}
	if err := req.Validate(); err != nil {
		f.t.Fatalf("model request invalid: %v", err)
	}
	res, err := f.s.RegisterAnomalyModel(f.ctx, f.cmd(org, submitter, "POST /v1/anomaly-governance/models", nil), req)
	if err != nil {
		f.t.Fatalf("register anomaly model: %v", err)
	}
	return res.Resource
}

func (f *fixture) runDetection(org string, req domain.RunDetectionRequest) *domain.DetectionRun {
	f.t.Helper()
	res, err := f.s.RunDetection(f.ctx, f.cmd(org, submitter, "POST /v1/anomaly-governance/runs", nil), req)
	if err != nil {
		f.t.Fatalf("run detection: %v", err)
	}
	return res.Resource
}

func (f *fixture) closeSignal(org string, sig domain.GovernedAnomalySignal, outcome domain.SignalStatus, reasonCode, notes string) (*domain.GovernedAnomalySignal, error) {
	res, err := f.s.CloseSignal(f.ctx, f.cmd(org, reviewer, "POST /v1/anomaly-governance/signals/{signalID}:close", ver(sig.Version)),
		sig.SignalID, domain.CloseSignalRequest{Outcome: outcome, ReasonCode: reasonCode, Notes: notes})
	return res.Resource, err
}
