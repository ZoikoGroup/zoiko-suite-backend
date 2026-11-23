package store_test

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestDuplicateMigrationPrefix fails if any two *.up.sql migration files share
// the same six-digit numeric prefix. Duplicate prefixes break migration ordering
// and make `migrate.py status` report false "edited after applying" mismatches.
func TestDuplicateMigrationPrefix(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "../../deployments/migrations")

	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("reading migrations dir: %v", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	seen := make(map[string]string)
	for _, name := range names {
		if len(name) < 7 || name[6] != '_' {
			continue
		}
		prefix := name[:6]
		if other, ok := seen[prefix]; ok {
			t.Fatalf("duplicate migration prefix %q: %s and %s", prefix, other, name)
		}
		seen[prefix] = name
	}
}
