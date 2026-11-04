package main

import (
	"os"
	"strings"
	"testing"
)

const matrix = `id,priority,title,status,go_tests,frontend_evidence,note
SEC-T001,P0,a,Built,internal/httpapi.TestA;internal/limits.TestB,,
SEC-T002,P0,b,Partial,internal/httpapi.TestC,,
SEC-T003,P0,c,Blocked,,,needs people
QA-T001,P1,d,Built,internal/httpapi.TestFailsButP1,,
`

func ev(action, pkg, test string) string {
	return `{"Action":"` + action + `","Package":"zoiko.io/public-ai-svc/` + pkg + `","Test":"` + test + `"}` + "\n"
}

func TestAllMappedP0TestsPass(t *testing.T) {
	events := ev("run", "internal/httpapi", "TestA") + ev("pass", "internal/httpapi", "TestA") +
		ev("pass", "internal/limits", "TestB") + ev("pass", "internal/httpapi", "TestC") +
		ev("fail", "internal/httpapi", "TestFailsButP1") + "# some build output\n"
	rep, err := check(strings.NewReader(matrix), strings.NewReader(events), "P0", false)
	if err != nil || len(rep.Problems) != 0 || rep.IDs != 2 || rep.Tests != 3 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
}

// A P0 ID whose mapped test failed, or never ran (renamed, deleted), fails.
func TestFailingOrMissingP0TestFails(t *testing.T) {
	events := ev("pass", "internal/httpapi", "TestA") + ev("fail", "internal/limits", "TestB") +
		ev("pass", "internal/httpapi", "TestA/subtest-fails-alone")
	rep, err := check(strings.NewReader(matrix), strings.NewReader(events), "P0", false)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(rep.Problems, "\n")
	if len(rep.Problems) != 2 || !strings.Contains(got, "FAILED  SEC-T001 internal/limits.TestB") ||
		!strings.Contains(got, "MISSING SEC-T002 internal/httpapi.TestC") {
		t.Fatalf("problems:\n%s", got)
	}
}

// A test with the right name in another package does not count.
func TestPackageMustMatch(t *testing.T) {
	events := ev("pass", "internal/httpapi", "TestA") + ev("pass", "internal/other", "TestB") + ev("pass", "internal/httpapi", "TestC")
	rep, _ := check(strings.NewReader(matrix), strings.NewReader(events), "P0", false)
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "internal/limits.TestB") {
		t.Fatalf("problems %v", rep.Problems)
	}
}

func TestSkipsAreReportedAndOptionallyFatal(t *testing.T) {
	events := ev("pass", "internal/httpapi", "TestA") + ev("skip", "internal/limits", "TestB") + ev("pass", "internal/httpapi", "TestC")
	rep, _ := check(strings.NewReader(matrix), strings.NewReader(events), "P0", false)
	if len(rep.Problems) != 0 || rep.Skipped != 1 {
		t.Fatalf("lenient: %+v", rep)
	}
	rep, _ = check(strings.NewReader(matrix), strings.NewReader(events), "P0", true)
	if len(rep.Problems) != 1 {
		t.Fatalf("strict: %+v", rep)
	}
}

func TestMalformedReference(t *testing.T) {
	bad := "id,priority,go_tests\nSEC-T001,P0,TestNoPackage\n"
	if _, err := check(strings.NewReader(bad), strings.NewReader(""), "P0", false); err == nil {
		t.Fatal("a reference without a package must be rejected")
	}
}

// The committed matrix parses and every P0 reference is well formed.
func TestCommittedMatrixIsWellFormed(t *testing.T) {
	f, err := os.Open("../../../../docs/suivo/sec-t-traceability.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rep, err := check(f, strings.NewReader(""), "P0", false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.IDs < 60 || rep.Tests == 0 || len(rep.Problems) != rep.Tests {
		t.Fatalf("with no test events every mapped test is missing: %+v", rep.IDs)
	}
}
