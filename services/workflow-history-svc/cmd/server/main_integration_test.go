//go:build integration

// Package main integration test for workflow-history-svc.
//
// This test boots a single real embedded Postgres, applies the migration,
// starts an HTTP test server, and exercises health probes and all read API
// endpoints. Subtests are used to run everything against a single database
// startup to avoid hitting the 2-minute timeout on slow environments.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/workflow-history-svc/internal/handler"
	"zoiko.io/workflow-history-svc/internal/health"
	"zoiko.io/workflow-history-svc/internal/store"
)

// freePort returns an OS-assigned free TCP port on localhost.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func TestIntegration(t *testing.T) {
	// ── 1. Start embedded Postgres once ──────────────────────────────────────
	dbPort := uint32(freePort(t))
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.PostgresVersion("16.0.0")).
			Port(dbPort).
			Database("workflow_history_test").
			Username("postgres").
			Password("postgres"),
	)
	require.NoError(t, pg.Start(), "embedded postgres failed to start")
	defer func() { _ = pg.Stop() }()

	dsn := fmt.Sprintf(
		"host=localhost port=%d dbname=workflow_history_test user=postgres password=postgres sslmode=disable",
		dbPort,
	)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	// Wait for the embedded DB to be ready (up to 10s).
	require.Eventually(t, func() bool {
		return pool.Ping(ctx) == nil
	}, 10*time.Second, 200*time.Millisecond, "embedded postgres did not become ready in time")

	// Apply the migration SQL from the service's own migrations directory.
	migrationSQL, err := os.ReadFile("../../deployments/migrations/000001_initial_schema.up.sql")
	require.NoError(t, err, "could not read migration file")

	_, err = pool.Exec(ctx, string(migrationSQL))
	require.NoError(t, err, "migration failed")

	// ── 2. Build test server once ────────────────────────────────────────────
	log := zap.NewNop()
	pgStore := store.NewPgStore(pool, log)
	healthH := health.New(pool, log)
	historyH := handler.New(pgStore, log)

	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.Recoverer)
	router.Get("/healthz", healthH.Liveness)
	router.Get("/readyz", healthH.Readiness)
	router.Get("/v1/workflows/history", historyH.GetCrossWorkflowHistory)
	router.Get("/v1/workflows/{workflow_instance_id}/history", historyH.GetInstanceHistory)

	srv := httptest.NewServer(router)
	defer srv.Close()

	// ── 3. Run subtests ──────────────────────────────────────────────────────

	t.Run("GET /healthz returns 200 ok", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/healthz")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("GET /readyz returns 200 ok", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/readyz")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("GET /v1/workflows/{id}/history NotFound", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/v1/workflows/wf-does-not-exist/history")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("GET /v1/workflows/{id}/history Returns Stored Events", func(t *testing.T) {
		// Directly insert events via the store layer.
		events := []store.WorkflowHistoryEvent{
			{
				EventID:            "evt-001",
				WorkflowInstanceID: "wf-001",
				EventType:          "workflow.started",
				CorrelationID:      "corr-001",
				TenantID:           "t-001",
				LegalEntityID:      "e-001",
				Payload:            json.RawMessage(`{"workflow_instance_id":"wf-001","tenant_id":"t-001","legal_entity_id":"e-001"}`),
			},
			{
				EventID:            "evt-002",
				WorkflowInstanceID: "wf-001",
				EventType:          "approval.granted",
				CorrelationID:      "corr-001",
				TenantID:           "t-001",
				LegalEntityID:      "e-001",
				Payload:            json.RawMessage(`{"workflow_instance_id":"wf-001","stage_order":1,"approver_principal_id":"u-002"}`),
			},
			{
				EventID:            "evt-003",
				WorkflowInstanceID: "wf-001",
				EventType:          "workflow.completed",
				CorrelationID:      "corr-001",
				TenantID:           "t-001",
				LegalEntityID:      "e-001",
				Payload:            json.RawMessage(`{"workflow_instance_id":"wf-001","workflow_status":"approved"}`),
			},
		}
		for _, e := range events {
			require.NoError(t, pgStore.Append(ctx, e))
		}

		resp, err := http.Get(srv.URL + "/v1/workflows/wf-001/history")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var body []map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		require.Len(t, body, 3)
		assert.Equal(t, "workflow.started", body[0]["event_type"])
		assert.Equal(t, "approval.granted", body[1]["event_type"])
		assert.Equal(t, "workflow.completed", body[2]["event_type"])
		for _, row := range body {
			assert.Equal(t, "t-001", row["tenant_id"])
			assert.Equal(t, "e-001", row["legal_entity_id"])
		}
	})

	t.Run("GET /v1/workflows/{id}/history Idempotency", func(t *testing.T) {
		e := store.WorkflowHistoryEvent{
			EventID:            "evt-idem-01",
			WorkflowInstanceID: "wf-idem",
			EventType:          "workflow.started",
			CorrelationID:      "corr-001",
			TenantID:           "t-001",
			LegalEntityID:      "e-001",
			Payload:            json.RawMessage(`{"workflow_instance_id":"wf-idem","tenant_id":"t-001","legal_entity_id":"e-001"}`),
		}
		require.NoError(t, pgStore.Append(ctx, e))
		require.NoError(t, pgStore.Append(ctx, e)) // duplicate

		resp, err := http.Get(srv.URL + "/v1/workflows/wf-idem/history")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var body []map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		assert.Len(t, body, 1)
	})

	t.Run("GET /v1/workflows/history returns filtered events", func(t *testing.T) {
		events := []store.WorkflowHistoryEvent{
			{
				EventID: "evt-a1", WorkflowInstanceID: "wf-a", EventType: "workflow.started",
				CorrelationID: "corr-a", TenantID: "t-001", LegalEntityID: "e-001",
				Payload: json.RawMessage(`{}`),
			},
			{
				EventID: "evt-b1", WorkflowInstanceID: "wf-b", EventType: "workflow.started",
				CorrelationID: "corr-b", TenantID: "t-001", LegalEntityID: "e-001",
				Payload: json.RawMessage(`{}`),
			},
			{
				EventID: "evt-c1", WorkflowInstanceID: "wf-c", EventType: "workflow.started",
				CorrelationID: "corr-c", TenantID: "t-999", LegalEntityID: "e-999",
				Payload: json.RawMessage(`{}`),
			},
		}
		for _, e := range events {
			require.NoError(t, pgStore.Append(ctx, e))
		}

		from := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
		to := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
		url := fmt.Sprintf("%s/v1/workflows/history?tenant_id=t-001&legal_entity_id=e-001&from=%s&to=%s",
			srv.URL, from, to)

		resp, err := http.Get(url)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var body []map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		// The workflowwf-001 was also added with tenant t-001, legal e-001 in a previous test,
		// plus wf-a and wf-b which have that context too. That's 3 started events + any subsequent
		// transitions (granted, completed for wf-001).
		// So total count should be at least 2.
		assert.GreaterOrEqual(t, len(body), 2)
		for _, event := range body {
			assert.Equal(t, "t-001", event["tenant_id"])
			assert.Equal(t, "e-001", event["legal_entity_id"])
		}
	})

	t.Run("GET /v1/workflows/history Missing Params", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/v1/workflows/history?tenant_id=t-001")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("GET /v1/workflows/history Invalid Time Range", func(t *testing.T) {
		from := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
		to := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
		url := fmt.Sprintf("%s/v1/workflows/history?tenant_id=t-001&legal_entity_id=e-001&from=%s&to=%s",
			srv.URL, from, to)

		resp, err := http.Get(url)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("GetTenantContext", func(t *testing.T) {
		tc, found, err := pgStore.GetTenantContext(ctx, "wf-missing")
		require.NoError(t, err)
		assert.False(t, found)
		assert.Empty(t, tc.TenantID)

		require.NoError(t, pgStore.Append(ctx, store.WorkflowHistoryEvent{
			EventID:            "evt-ctx-01",
			WorkflowInstanceID: "wf-ctx",
			EventType:          "workflow.started",
			CorrelationID:      "corr-001",
			TenantID:           "t-ctx",
			LegalEntityID:      "e-ctx",
			Payload:            json.RawMessage(`{}`),
		}))

		tc, found, err = pgStore.GetTenantContext(ctx, "wf-ctx")
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, "t-ctx", tc.TenantID)
		assert.Equal(t, "e-ctx", tc.LegalEntityID)
	})
}
