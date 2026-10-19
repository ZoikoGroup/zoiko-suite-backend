package imports

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// INT-CTRL-021 / INT-NP-031: the import engine reaches a domain only through a named domain command over HTTP. It has
// no database access of its own to the authoritative tables it feeds, and the build says so: this package may not import
// a database driver, database/sql, or the service's own store. Persistence is behind the Repo interface the store
// implements; delivery is HTTPTargets.
func TestTheImportEngineHasNoDatabaseAccessOfItsOwn(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources found: %v", err)
	}
	var usesHTTP bool
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range src.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			switch {
			case p == "database/sql", strings.Contains(p, "pgx"), strings.Contains(p, "lib/pq"), strings.Contains(p, "gorm"),
				strings.HasSuffix(p, "/internal/store"):
				t.Errorf("%s imports %s: the import engine must not touch a database directly", f, p)
			case p == "net/http":
				usesHTTP = true
			}
		}
	}
	if !usesHTTP {
		t.Fatal("the engine's delivery to a domain is an HTTP command; if that changed this guard must be re-thought, not skipped")
	}
	if _, err := os.Stat("execute.go"); err != nil {
		t.Fatalf("execute.go moved: %v", err)
	}
}
