package health_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/schema-registry-svc/internal/health"
)

func TestLiveness_AlwaysOK(t *testing.T) {
	h := health.New(nil, zap.NewNop())
	rec := httptest.NewRecorder()
	h.Liveness(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}

// Readiness is exercised against a real database (skipped without
// TEST_DATABASE_URL): 200 while the pool answers, 503 once it cannot.
func TestReadiness_ReflectsTheDatabase(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	h := health.New(pool, zap.NewNop())

	rec := httptest.NewRecorder()
	h.Readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"status":"ok"}`, rec.Body.String())

	pool.Close() // the database is no longer reachable through this pool
	rec = httptest.NewRecorder()
	h.Readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
