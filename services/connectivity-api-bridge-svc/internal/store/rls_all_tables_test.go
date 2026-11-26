package store_test

import (
	"context"
	"testing"
)

// TestRLS_EveryTenantTableIsForced walks the catalog instead of a hand-kept
// list: every public table that carries a tenant_id column must have row
// level security ENABLED and FORCED, so a table added by a later migration
// without RLS fails here instead of shipping unprotected.
func TestRLS_EveryTenantTableIsForced(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)

	rows, err := admin.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
		   AND EXISTS (SELECT 1 FROM pg_attribute a
		                WHERE a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped)
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("query catalog: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name string
		var enabled, forced bool
		if err := rows.Scan(&name, &enabled, &forced); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if !enabled || !forced {
			t.Errorf("%s: tenant table must ENABLE and FORCE row level security (enabled=%v forced=%v)", name, enabled, forced)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if n == 0 {
		t.Fatal("no tenant tables found: migrations were not applied")
	}
	t.Logf("%d tenant tables checked", n)
}
