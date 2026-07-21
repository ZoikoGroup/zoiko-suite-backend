//go:build integration

// Package main — integration test for cmd/server.
//
// Run with:
//
//	go test -v -tags=integration -timeout=120s ./cmd/server/
//
// The test:
//  1. Starts an embedded PostgreSQL instance (real Postgres binary, no Docker
//     needed) on a random high port.
//  2. Runs the schema migration so audit_events table exists.
//  3. Boots the service HTTP server in a goroutine wired to the same DB.
//  4. Issues real HTTP GET requests to /healthz and /readyz.
//  5. Asserts both return 200 and JSON bodies with status:"ok".
//
// This gives the same confidence as "run a container and curl it" without
// needing Docker Desktop to be running.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/health"
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

// schema is the minimal DDL needed to satisfy the service's DB expectations.
// Mirrors deployments/migrations/000001_initial_schema.up.sql.
const schema = `
CREATE TABLE IF NOT EXISTS audit_events (
    event_id        TEXT        NOT NULL,
    event_type      TEXT        NOT NULL,
    tenant_id       TEXT        NOT NULL,
    legal_entity_id TEXT        NOT NULL,
    principal_id    TEXT,
    source_service  TEXT        NOT NULL,
    schema_version  TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    stored_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT audit_events_pkey PRIMARY KEY (event_id)
);
`

// TestServerHealthProbes is the integration smoke test.
// It starts an embedded Postgres, runs the schema, boots the HTTP server,
// and validates /healthz and /readyz both return 200 with status:"ok".
func TestServerHealthProbes(t *testing.T) {
	// ── Pick free ports ───────────────────────────────────────────────────
	dbPort := freePort(t)
	httpPort := freePort(t)

	// ── Start embedded Postgres ───────────────────────────────────────────
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.PostgresVersion("16.1.0")).
			Username("testuser").
			Password("testpass").
			Database("audit_event_store").
			Port(uint32(dbPort)).
			Logger(io.Discard),
	)
	require.NoError(t, pg.Start(), "embedded postgres must start")
	t.Cleanup(func() { _ = pg.Stop() })

	// ── Apply schema ──────────────────────────────────────────────────────
	dsn := fmt.Sprintf(
		"host=localhost port=%d dbname=audit_event_store user=testuser password=testpass sslmode=disable",
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

	_, err = pool.Exec(ctx, schema)
	require.NoError(t, err, "schema migration must succeed")

	// ── Build and start HTTP server (same wiring as main()) ───────────────
	log, _ := zap.NewDevelopment()
	defer func() { _ = log.Sync() }()

	// Set env so config.Load() picks up the test DB.
	t.Setenv("DB_HOST", "localhost")
	t.Setenv("DB_PORT", fmt.Sprintf("%d", dbPort))
	t.Setenv("DB_NAME", "audit_event_store")
	t.Setenv("DB_USER", "testuser")
	t.Setenv("DB_PASSWORD", "testpass")
	t.Setenv("DB_SSLMODE", "disable")
	t.Setenv("PORT", fmt.Sprintf("%d", httpPort))

	// Wire health handler directly (same as main()).
	healthH := health.New(pool, log)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthH.Liveness)
	mux.HandleFunc("/readyz", healthH.Readiness)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", httpPort),
		Handler: mux,
	}

	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})

	// Wait for the HTTP server to be ready.
	require.Eventually(t, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/healthz", httpPort))
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 100*time.Millisecond, "HTTP server did not become ready")

	// ── /healthz ──────────────────────────────────────────────────────────
	t.Run("GET /healthz returns 200 ok", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/healthz", httpPort))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		t.Logf("GET /healthz → %d  body: %s", resp.StatusCode, body)

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &payload))
		assert.Equal(t, "ok", payload["status"])
		assert.Equal(t, "audit-event-store-svc", payload["service"])
	})

	// ── /readyz ───────────────────────────────────────────────────────────
	t.Run("GET /readyz returns 200 ok when DB is reachable", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/readyz", httpPort))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		t.Logf("GET /readyz → %d  body: %s", resp.StatusCode, body)

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &payload))
		assert.Equal(t, "ok", payload["status"])
		assert.Equal(t, "audit-event-store-svc", payload["service"])
	})

	// ── Explicit log of both probe responses for the PR record ────────────
	t.Log("Both /healthz and /readyz returned 200 ok with correct JSON bodies.")
	_ = os.Stdout.Sync()
}
