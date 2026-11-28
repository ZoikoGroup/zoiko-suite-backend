package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyDuplicateNumbers are migration numbers that two files already share.
// deployments/migrate.py orders and tracks migrations by file name, so they
// apply in a fixed order; renaming them would orphan the applied-migration
// records of every existing database. They are recorded in SPEC_DEVIATIONS.md
// and are the only ones allowed.
var legacyDuplicateNumbers = map[string]bool{"000003": true, "000009": true}

// Every migration has a down file, and no new migration reuses a number: a
// tool that keys on the number (golang-migrate, most CI runners) would apply
// only one of the two.
func TestMigrations_NumberedOnceWithDownFiles(t *testing.T) {
	ups, err := filepath.Glob("../../deployments/migrations/*.up.sql")
	if err != nil || len(ups) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	seen := map[string][]string{}
	for _, up := range ups {
		name := filepath.Base(up)
		num, _, ok := strings.Cut(name, "_")
		if !ok || len(num) != 6 {
			t.Errorf("%s: want a six-digit number prefix", name)
			continue
		}
		seen[num] = append(seen[num], name)
		down := strings.TrimSuffix(up, ".up.sql") + ".down.sql"
		if _, err := os.Stat(down); err != nil {
			t.Errorf("%s has no %s", name, filepath.Base(down))
		}
	}
	for num, names := range seen {
		if len(names) > 1 && !legacyDuplicateNumbers[num] {
			t.Errorf("migration number %s is used by %v: give the new one the next free number", num, names)
		}
	}
}
