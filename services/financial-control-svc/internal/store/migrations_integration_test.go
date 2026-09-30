//go:build integration

package store_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every migration must have a DOWN script that reverses it cleanly, and the UP scripts must
// apply again afterwards. Runs in a scratch schema so the shared test data is untouched.
func TestMigrations_DownScriptsReverseCleanlyAndUpReapplies(t *testing.T) {
	ups, _ := filepath.Glob("../../deployments/migrations/*.up.sql")
	downs, _ := filepath.Glob("../../deployments/migrations/*.down.sql")
	sort.Strings(ups)
	sort.Strings(downs)
	require.NotEmpty(t, ups)
	require.Equal(t, len(ups), len(downs), "every UP migration needs a DOWN migration")

	conn, err := testPool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, `DROP SCHEMA IF EXISTS mig_roundtrip CASCADE; CREATE SCHEMA mig_roundtrip; SET search_path TO mig_roundtrip`)
	require.NoError(t, err)
	defer func() {
		_, _ = conn.Exec(ctx, `SET search_path TO public; DROP SCHEMA IF EXISTS mig_roundtrip CASCADE`)
	}()

	run := func(files []string) {
		for _, f := range files {
			b, err := os.ReadFile(f)
			require.NoError(t, err)
			_, err = conn.Exec(ctx, string(b))
			require.NoError(t, err, f)
		}
	}
	run(ups)
	for i, j := 0, len(downs)-1; i < j; i, j = i+1, j-1 {
		downs[i], downs[j] = downs[j], downs[i]
	}
	run(downs)
	var left int
	require.NoError(t, conn.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'mig_roundtrip'`).Scan(&left))
	require.Zero(t, left, "the DOWN scripts must leave no table behind")
	run(ups)
}
