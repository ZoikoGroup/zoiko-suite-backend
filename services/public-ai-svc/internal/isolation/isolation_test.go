package isolation

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// SEC-T049 / F02: the whole module (not just this package) must not depend
// on any other ZoikoSuite service module.
func TestSECT049_NoOtherZoikoModules(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list failed: %v\n%s", err, out)
	}
	pkgs := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	if len(pkgs) < 10 {
		t.Fatalf("suspiciously short dependency list (%d lines); the check is not scanning the module", len(pkgs))
	}
	if bad := ForbiddenDeps(pkgs); len(bad) > 0 {
		t.Fatalf("forbidden dependencies on other services: %v", bad)
	}
}

// Proves the check actually rejects a cross-service import.
func TestForbiddenDepsRejectsOtherServices(t *testing.T) {
	in := []string{
		"net/http",
		"zoiko.io/public-ai-svc/internal/config",
		"zoiko.io/public-ai-svc",
		"zoiko.io/assistant-rag-svc/internal/store",
		"zoiko.io/general-ledger-svc",
		"zoiko.io/public-ai-svc-evil/x",
	}
	got := ForbiddenDeps(in)
	want := []string{"zoiko.io/assistant-rag-svc/internal/store", "zoiko.io/general-ledger-svc", "zoiko.io/public-ai-svc-evil/x"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}
