package handler_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/migration-integrity-svc/internal/handler"
	"zoiko.io/migration-integrity-svc/internal/health"
	"zoiko.io/migration-integrity-svc/internal/store"
)

type fakePinger struct{ err error }

func (p fakePinger) Ping(context.Context) error { return p.err }

// /readyz is public (no tenant, principal or envelope headers) and reflects
// the database: 200 when it answers, 503 when it does not or is absent.
func TestReadyz_PublicAndReflectsDatabase(t *testing.T) {
	h := handler.NewHandler(store.NewMemoryStore(), nil, zap.NewNop(), 95)
	for _, tc := range []struct {
		name   string
		router http.Handler
		want   int
	}{
		{"db up", handler.NewRouterWithReadiness(h, health.ReadyzHandler(fakePinger{})), http.StatusOK},
		{"db down", handler.NewRouterWithReadiness(h, health.ReadyzHandler(fakePinger{err: errors.New("down")})), http.StatusServiceUnavailable},
		{"no db", handler.NewRouter(h), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil)) // no headers at all
			if rec.Code != tc.want {
				t.Fatalf("expected %d, got %d: %s", tc.want, rec.Code, rec.Body.String())
			}
		})
	}
}
