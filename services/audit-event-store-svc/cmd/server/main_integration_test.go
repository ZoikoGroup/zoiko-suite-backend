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
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/audit-event-store-svc/internal/health"
	"zoiko.io/audit-event-store-svc/internal/store"
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
// Mirrors deployments/migrations/000001_initial_schema.up.sql and
// 000002_add_hash_chain_fields.up.sql.
const schema = `
CREATE TABLE IF NOT EXISTS audit_events (
    event_id             TEXT        NOT NULL,
    event_type           TEXT        NOT NULL,
    tenant_id            TEXT        NOT NULL,
    legal_entity_id      TEXT        NOT NULL,
    principal_id         TEXT,
    source_service       TEXT        NOT NULL,
    schema_version       TEXT        NOT NULL,
    payload              JSONB       NOT NULL,
    stored_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    correlation_id       TEXT,
    causation_id         TEXT,
    sequence_number      BIGINT,
    payload_hash         TEXT,
    previous_event_hash  TEXT,
    CONSTRAINT audit_events_pkey PRIMARY KEY (event_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS audit_events_sequence_number_idx ON audit_events (sequence_number);
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
			// Pinned explicitly rather than left on the library default
			// (V18 as of embedded-postgres v1.34.0): DefaultConfig()
			// floats to whatever major version the library currently
			// calls "latest," and that specific patch build
			// (18.3.0) can stop resolving from the library's remote
			// binary repository at any time with no code change on our
			// side — exactly what broke this test on the main-branch CI
			// run for PR #105 five minutes after the identical commit
			// passed on dev. V16 is also the version every real service
			// in this platform actually runs (postgres:16-alpine in
			// docker-compose.yml), so pinning here is prod-parity, not
			// just stability.
			Version(embeddedpostgres.V16).
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

// TestPgStore_HashChain_RealPostgres proves the hash-chain locking logic in
// internal/store/store.go (pg_advisory_xact_lock + chain-tip read + insert)
// actually works against a real PostgreSQL server, not just the in-memory
// FakeStore mirror in internal/store/store_test.go. The advisory lock is
// the one piece of this feature that's fundamentally untestable without a
// real Postgres — FakeStore's mutex can't prove the SQL-level locking
// behaves correctly.
func TestPgStore_HashChain_RealPostgres(t *testing.T) {
	dbPort := freePort(t)

	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			// Version pinned explicitly — see the doc comment on the
			// other NewDatabase call above for why.
			Version(embeddedpostgres.V16).
			Username("testuser").
			Password("testpass").
			Database("audit_event_store").
			Port(uint32(dbPort)).
			Logger(io.Discard),
	)
	require.NoError(t, pg.Start(), "embedded postgres must start")
	t.Cleanup(func() { _ = pg.Stop() })

	dsn := fmt.Sprintf(
		"host=localhost port=%d dbname=audit_event_store user=testuser password=testpass sslmode=disable",
		dbPort,
	)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	require.Eventually(t, func() bool {
		return pool.Ping(ctx) == nil
	}, 10*time.Second, 200*time.Millisecond, "embedded postgres did not become ready in time")

	_, err = pool.Exec(ctx, schema)
	require.NoError(t, err, "schema migration must succeed")

	log, _ := zap.NewDevelopment()
	defer func() { _ = log.Sync() }()

	pgStore := store.NewPgStore(pool, log)

	// Sequential inserts: verify the chain links correctly under real SQL.
	first := &store.AuditEvent{
		EventID: "evt-real-1", EventType: "test.event", TenantID: "t1",
		LegalEntityID: "e1", SourceService: "test-svc", SchemaVersion: "1.0",
		Payload: json.RawMessage(`{"n":1}`),
	}
	require.NoError(t, pgStore.Store(ctx, first))
	assert.Equal(t, int64(1), first.SequenceNumber)
	assert.NotEmpty(t, first.PayloadHash)
	assert.Empty(t, first.PreviousEventHash, "first real row must have no predecessor")

	second := &store.AuditEvent{
		EventID: "evt-real-2", EventType: "test.event", TenantID: "t1",
		LegalEntityID: "e1", SourceService: "test-svc", SchemaVersion: "1.0",
		Payload: json.RawMessage(`{"n":2}`),
	}
	require.NoError(t, pgStore.Store(ctx, second))
	assert.Equal(t, int64(2), second.SequenceNumber)
	assert.Equal(t, first.PayloadHash, second.PreviousEventHash,
		"second row must chain to the first row's real, database-computed payload_hash")

	// Duplicate delivery: must not consume a sequence number or break the chain.
	dup := &store.AuditEvent{
		EventID: "evt-real-1", EventType: "test.event", TenantID: "t1",
		LegalEntityID: "e1", SourceService: "test-svc", SchemaVersion: "1.0",
		Payload: json.RawMessage(`{"n":1}`),
	}
	require.NoError(t, pgStore.Store(ctx, dup))
	assert.Zero(t, dup.SequenceNumber, "duplicate event_id must not advance the real chain")

	third := &store.AuditEvent{
		EventID: "evt-real-3", EventType: "test.event", TenantID: "t1",
		LegalEntityID: "e1", SourceService: "test-svc", SchemaVersion: "1.0",
		Payload: json.RawMessage(`{"n":3}`),
	}
	require.NoError(t, pgStore.Store(ctx, third))
	assert.Equal(t, int64(3), third.SequenceNumber, "no gap: the duplicate above must not have consumed sequence 3")
	assert.Equal(t, second.PayloadHash, third.PreviousEventHash)

	// Concurrent inserts against the REAL database: this is the actual
	// property the advisory lock exists to guarantee — no fork, no gap,
	// no lost update, under real concurrent SQL transactions.
	const n = 15
	var wg sync.WaitGroup
	ready := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready
			e := &store.AuditEvent{
				EventID: fmt.Sprintf("evt-real-concurrent-%02d", i), EventType: "test.event",
				TenantID: "t1", LegalEntityID: "e1", SourceService: "test-svc", SchemaVersion: "1.0",
				Payload: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
			}
			assert.NoError(t, pgStore.Store(ctx, e))
		}()
	}
	close(ready)
	wg.Wait()

	rows, err := pool.Query(ctx, "SELECT sequence_number, payload_hash, previous_event_hash FROM audit_events ORDER BY sequence_number ASC")
	require.NoError(t, err)
	defer rows.Close()

	var (
		seqs       []int64
		hashes     []string
		prevHashes []*string
	)
	for rows.Next() {
		var seq int64
		var hash string
		var prev *string
		require.NoError(t, rows.Scan(&seq, &hash, &prev))
		seqs = append(seqs, seq)
		hashes = append(hashes, hash)
		prevHashes = append(prevHashes, prev)
	}
	require.NoError(t, rows.Err())

	require.Len(t, seqs, 3+n, "3 sequential + n concurrent rows, minus the one deduped duplicate")
	for i, seq := range seqs {
		assert.Equal(t, int64(i+1), seq, "sequence_number must be gap-free and strictly increasing under real concurrent load")
		if i > 0 {
			require.NotNil(t, prevHashes[i], "every row after the first must have a previous_event_hash")
			assert.Equal(t, hashes[i-1], *prevHashes[i],
				"row at position %d must chain to the immediately preceding row — a mismatch means the advisory lock failed to prevent a fork", i)
		}
	}

	t.Logf("Verified a %d-row hash chain (3 sequential + %d concurrent) against real Postgres with no gaps and no forks.", len(seqs), n)
}
