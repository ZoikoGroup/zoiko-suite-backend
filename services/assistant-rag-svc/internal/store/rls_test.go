package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/assistant-rag-svc/internal/domain"
	"zoiko.io/assistant-rag-svc/internal/store"
)

// Real-Postgres tests for assistant-rag-svc (AI-03), run as a
// purpose-created NOSUPERUSER NOBYPASSRLS role.

const (
	orgA = "11111111-1111-1111-1111-111111111111"
	orgB = "22222222-2222-2222-2222-222222222222"
)

func openAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)

	_, _ = pool.Exec(ctx, `
		DROP TABLE IF EXISTS outbox_events CASCADE;
		DROP TABLE IF EXISTS idempotency_keys CASCADE;
		DROP TABLE IF EXISTS tool_proposals CASCADE;
		DROP TABLE IF EXISTS ai_response_evidence CASCADE;
		DROP TABLE IF EXISTS citations CASCADE;
		DROP TABLE IF EXISTS prompt_executions CASCADE;
		DROP TABLE IF EXISTS retrieved_items CASCADE;
		DROP TABLE IF EXISTS retrieval_sets CASCADE;
		DROP TABLE IF EXISTS assistant_sessions CASCADE;
		DROP TABLE IF EXISTS tool_policies CASCADE;
		DROP TABLE IF EXISTS source_grants CASCADE;
		DROP FUNCTION IF EXISTS enforce_tool_proposal_lifecycle() CASCADE;
		DROP FUNCTION IF EXISTS reject_immutable_row() CASCADE;
		DROP FUNCTION IF EXISTS enforce_session_lifecycle() CASCADE;
		DROP FUNCTION IF EXISTS reject_delete() CASCADE;`)

	_, thisFile, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var migs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			migs = append(migs, e.Name())
		}
	}
	if len(migs) == 0 {
		t.Fatalf("no migrations found in %s", migDir)
	}
	sort.Strings(migs)
	for _, name := range migs {
		sqlBytes, err := os.ReadFile(filepath.Join(migDir, name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
	return pool
}

func appRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	const appRole = "zoiko_app_test"
	const appPassword = "zoiko_app_test_pw"

	if _, err := admin.Exec(ctx, `DO $do$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+appRole+`') THEN
			CREATE ROLE `+appRole+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
		END IF;
	END $do$;`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	for _, stmt := range []string{
		`ALTER ROLE ` + appRole + ` WITH LOGIN PASSWORD '` + appPassword + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE ON SCHEMA public TO ` + appRole,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + appRole,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("grant (%s): %v", stmt, err)
		}
	}

	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.User = url.UserPassword(appRole, appPassword)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as app role: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

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
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	return &fixture{t: t, ctx: context.Background(), admin: admin, app: app, s: store.NewPgStore(app)}
}

func (f *fixture) claim(op, resource string) domain.IdempotencyClaim {
	f.mu.Lock()
	f.keys++
	n := f.keys
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", op, resource, n)))
	return domain.IdempotencyClaim{
		OwnerScope: domain.SellerScope, PrincipalID: "test-operator", Key: fmt.Sprintf("key-%d", n),
		Operation: op, RequestSHA256: hex.EncodeToString(sum[:]), ResourceID: resource,
	}
}

func (f *fixture) registerSourceGrant(org, sourceRef, purpose string) *domain.SourceGrant {
	f.t.Helper()
	g, err := f.s.RegisterSourceGrant(f.ctx, org, domain.RegisterSourceGrantRequest{SourceRef: sourceRef, Purpose: purpose}, "test-operator")
	if err != nil {
		f.t.Fatalf("register source grant: %v", err)
	}
	return g
}

func (f *fixture) registerToolPolicy(org, toolName, targetDomain string, protected bool) *domain.ToolPolicy {
	f.t.Helper()
	p, err := f.s.RegisterToolPolicy(f.ctx, org, domain.RegisterToolPolicyRequest{
		ToolName: toolName, TargetDomain: targetDomain, Protected: protected,
	}, "test-operator")
	if err != nil {
		f.t.Fatalf("register tool policy: %v", err)
	}
	return p
}

func (f *fixture) startSession(org, subjectID, purpose string) *domain.AssistantSession {
	f.t.Helper()
	sess, err := f.s.StartSession(f.ctx, org, domain.StartSessionRequest{SubjectID: subjectID, Purpose: purpose},
		"test-operator", f.claim("StartSession", subjectID))
	if err != nil {
		f.t.Fatalf("start session: %v", err)
	}
	return sess
}

func (f *fixture) retrieve(org string, req domain.RetrieveRequest) *domain.RetrievalSet {
	f.t.Helper()
	set, err := f.s.Retrieve(f.ctx, org, req, "test-operator", f.claim("Retrieve", req.SessionID+req.Query))
	if err != nil {
		f.t.Fatalf("retrieve: %v", err)
	}
	return set
}
