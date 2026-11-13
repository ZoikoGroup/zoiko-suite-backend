//go:build integration

// Migration 000037 converges a database migrated on the pre-registry lineage, against a real Postgres.
package registryit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// convergedState counts what 000037 guarantees: the six rule columns, the ten
// foreign keys (validated) and the provenance guard trigger.
func convergedState(t *testing.T) (columns, validFKs, triggers int) {
	t.Helper()
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'jurisdiction_rules'
		AND column_name IN ('regime_id','interpretation_id','supersedes_rule_id','precedence','published_on','rule_parameters')`).Scan(&columns))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype = 'f' AND convalidated AND conname IN (
		'jurisdiction_rules_regime_id_fkey','jurisdiction_rules_interpretation_id_fkey','interpretation_records_jurisdiction_id_fkey',
		'obligation_rules_jurisdiction_id_fkey','pack_version_jurisdictions_jurisdiction_id_fkey','payment_terms_jurisdiction_id_fkey',
		'regulatory_calendars_jurisdiction_id_fkey','regulatory_sources_jurisdiction_id_fkey','rule_sources_jurisdiction_rule_id_fkey',
		'source_change_notices_jurisdiction_id_fkey')`).Scan(&validFKs))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname = 'trg_jurisdiction_rules_provenance_guard'
		AND tgrelid = 'jurisdiction_rules'::regclass`).Scan(&triggers))
	return
}

func TestMigration000037_ConvergesAnOldLineageDatabase(t *testing.T) {
	up := readMigration(t, "000037_converge_rule_provenance_schema.up.sql")

	// A database built from this directory already has everything: no change.
	c, f, g := convergedState(t)
	require.Equal(t, []int{6, 10, 1}, []int{c, f, g}, "fresh schema")
	_, err := pool.Exec(ctx, up)
	require.NoError(t, err, "a no-op on a fresh database")
	c, f, g = convergedState(t)
	assert.Equal(t, []int{6, 10, 1}, []int{c, f, g})

	// The pre-registry lineage: 000005 recorded without its ALTER of
	// jurisdiction_rules, and the foreign keys to jurisdictions missing.
	_, err = pool.Exec(ctx, `
		DROP TRIGGER trg_jurisdiction_rules_provenance_guard ON jurisdiction_rules;
		ALTER TABLE jurisdiction_rules DROP COLUMN regime_id, DROP COLUMN interpretation_id,
			DROP COLUMN precedence, DROP COLUMN published_on, DROP COLUMN rule_parameters;
		ALTER TABLE payment_terms DROP CONSTRAINT payment_terms_jurisdiction_id_fkey;
		ALTER TABLE regulatory_sources DROP CONSTRAINT regulatory_sources_jurisdiction_id_fkey;
		ALTER TABLE rule_sources DROP CONSTRAINT rule_sources_jurisdiction_rule_id_fkey;`)
	require.NoError(t, err, "simulate the old lineage")
	c, f, g = convergedState(t)
	require.Equal(t, []int{1, 5, 0}, []int{c, f, g}, "old lineage: only supersedes_rule_id and five foreign keys")

	_, err = pool.Exec(ctx, up)
	require.NoError(t, err, "converge")
	c, f, g = convergedState(t)
	assert.Equal(t, []int{6, 10, 1}, []int{c, f, g}, "converged to the fresh schema, foreign keys validated")

	_, err = pool.Exec(ctx, up)
	require.NoError(t, err, "a rerun is a no-op")
	c, f, g = convergedState(t)
	assert.Equal(t, []int{6, 10, 1}, []int{c, f, g})
}
