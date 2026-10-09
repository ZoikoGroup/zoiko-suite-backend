package idempotency_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/idempotency"
	"zoiko.io/authorization-svc/internal/jurisdiction"
	"zoiko.io/authorization-svc/internal/siem"
	"zoiko.io/authorization-svc/internal/store"
)

// The audit's scenario, end to end on a real database: a retried
// POST /v1/admin/role-assignments created a second grant, because nothing read
// Idempotency-Key. Runs only with AUTHZ_IT_DSN (a database migrated to 000021,
// connected as a NOBYPASSRLS role); it only adds rows under fresh ids.

type noPublisher struct{ handler.EventPublisher }

type noValidator struct{}

func (noValidator) ValidateExists(context.Context, string) error { return nil }

var _ jurisdiction.Validator = noValidator{}

func TestIdempotencyIT_RetriedAssignmentGrantsOnce(t *testing.T) {
	dsn := os.Getenv("AUTHZ_IT_DSN")
	if dsn == "" {
		t.Skip("AUTHZ_IT_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	tenant := uuid.NewString()
	adminRole, grantRole := uuid.NewString(), uuid.NewString()
	admin, target := "admin-"+uuid.NewString()[:8], "target-"+uuid.NewString()[:8]

	// Fixtures, written under the tenant so RLS admits them.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"SELECT set_config('app.tenant_id', $1, true)", []any{tenant}},
		{"INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_by_principal_id) VALUES ($1,$2,'IT_ADMIN','it','LEGAL_ENTITY',true,'it'),($3,$2,'IT_GRANT','it','LEGAL_ENTITY',true,'it')", []any{adminRole, tenant, grantRole}},
		{"INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions, active_flag) VALUES ($1,'IAM','[\"iam.assignment.grant\"]',true),($2,'BIZ','[\"report.view\"]',true)", []any{adminRole, grantRole}},
		{"INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by) VALUES ($1,$2,$3,now() - interval '1 minute','it')", []any{admin, adminRole, tenant}},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("fixture %q: %v", q.sql, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	pg := store.New(pool, zap.NewNop())
	h := handler.New(pg, noPublisher{}, noValidator{}, siem.New("", "authorization-svc", zap.NewNop()), "", false, zap.NewNop())
	r := chi.NewRouter()
	r.Use(idempotency.Middleware(pg, handler.MaterialWrite, zap.NewNop()))
	handler.RegisterRoutes(r, h)

	post := func(key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/role-assignments", bytes.NewBufferString(body))
		req.Header.Set("X-Principal-Id", admin)
		req.Header.Set("X-Tenant-Id", tenant)
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	body := `{"principal_id":"` + target + `","role_id":"` + grantRole + `","legal_entity_id":"` + uuid.NewString() + `","effective_from":"2026-01-01T00:00:00Z"}`
	key := uuid.NewString()

	first := post(key, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first call: want 201, got %d: %s", first.Code, first.Body.String())
	}
	retry := post(key, body)
	if retry.Code != http.StatusOK || retry.Header().Get(idempotency.HeaderReplayed) != "true" {
		t.Fatalf("retry: want 200 replayed, got %d (replayed=%q): %s", retry.Code, retry.Header().Get(idempotency.HeaderReplayed), retry.Body.String())
	}
	var a, b any
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(retry.Body.Bytes(), &b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("retry answered differently from the first call:\n%s\n%s", first.Body.String(), retry.Body.String())
	}

	held, err := pg.ListRoleAssignments(ctx, tenant, target, grantRole, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 {
		t.Fatalf("retried assignment created %d grants, want 1", len(held))
	}

	// The same key for a different request is refused, and grants nothing.
	other := `{"principal_id":"` + target + `","role_id":"` + grantRole + `","legal_entity_id":"` + uuid.NewString() + `","effective_from":"2026-01-01T00:00:00Z"}`
	if w := post(key, other); w.Code != http.StatusConflict {
		t.Fatalf("key reused for a different request: want 409, got %d: %s", w.Code, w.Body.String())
	}
	held, _ = pg.ListRoleAssignments(ctx, tenant, target, grantRole, false)
	if len(held) != 1 {
		t.Fatalf("mismatched key still granted: %d grants", len(held))
	}
}

var _ = domain.AccessDecisionLog{}
