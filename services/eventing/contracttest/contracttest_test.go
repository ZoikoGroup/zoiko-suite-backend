package contracttest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// outboxWith gives the test a private schema holding a minimal eventing_outbox
// (CheckOutbox reads only event_type and payload) filled with the given rows.
func outboxWith(t *testing.T, rows map[string][]string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") != "" || os.Getenv("REQUIRE_DB_TESTS") != "" {
			t.Fatal("TEST_DATABASE_URL must be set in CI")
		}
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "contracttest_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE TABLE eventing_outbox (event_type text, payload jsonb)`); err != nil {
		t.Fatal(err)
	}
	for typ, payloads := range rows {
		for _, p := range payloads {
			if _, err := pool.Exec(ctx, `INSERT INTO eventing_outbox VALUES ($1, $2::jsonb)`, typ, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	return pool
}

func schemaDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const itemSchema = `{"type":"object","required":["id"],"properties":{"id":{"type":"string"},"qty":{"type":"integer"}}}`

func TestConformingEventsPass(t *testing.T) {
	pool := outboxWith(t, map[string][]string{
		"com.zoikosuite.ar.item.created": {`{"data":{"id":"a","qty":1}}`, `{"data":{"id":"b"}}`},
	})
	dir := schemaDir(t, map[string]string{"ar.item.created.schema.json": itemSchema})
	problems, checked, err := CheckOutbox(context.Background(), pool, dir)
	if err != nil || len(problems) != 0 || checked != 2 {
		t.Fatalf("problems=%v checked=%d err=%v", problems, checked, err)
	}
}

func TestDriftMissingSchemaAndNoDataAreReported(t *testing.T) {
	pool := outboxWith(t, map[string][]string{
		"com.zoikosuite.ar.item.created": {`{"data":{"qty":"many"}}`, `{"other":1}`},
		"com.zoikosuite.ar.item.removed": {`{"data":{"id":"x"}}`},
		"LegacyEvent":                    {`{"data":{}}`},
	})
	dir := schemaDir(t, map[string]string{"ar.item.created.schema.json": itemSchema})
	problems, checked, err := CheckOutbox(context.Background(), pool, dir)
	if err != nil {
		t.Fatal(err)
	}
	if checked != 3 {
		t.Fatalf("legacy rows are skipped, canonical ones counted: checked=%d", checked)
	}
	all := strings.Join(problems, "\n")
	for _, want := range []string{"ar.item.created", "no data member", "ar.item.removed: emitted but has no published schema"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if len(problems) < 3 {
		t.Errorf("want a wrong-type, a no-data and a missing-schema problem, got %v", problems)
	}
}

func TestViolationsOfOneTypeAreCapped(t *testing.T) {
	var bad []string
	for i := 0; i < 10; i++ {
		bad = append(bad, `{"data":{"qty":"x"}}`)
	}
	pool := outboxWith(t, map[string][]string{"com.zoikosuite.ar.item.created": bad})
	dir := schemaDir(t, map[string]string{"ar.item.created.schema.json": itemSchema})
	problems, _, _ := CheckOutbox(context.Background(), pool, dir)
	if !strings.Contains(strings.Join(problems, "\n"), "more violation(s) not shown") {
		t.Fatalf("expected a cap note, got %v", problems)
	}
}
