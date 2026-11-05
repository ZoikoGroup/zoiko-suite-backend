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
	rep, err := check(strings.NewReader(matrix), strings.NewReader(events), Options{Priority: "P0"})
	if err != nil || len(rep.Problems) != 0 || rep.IDs != 3 || rep.Tests != 3 || len(rep.Open) != 2 {
		t.Fatalf("rep %+v err %v", rep, err)
	}
}

// A P0 ID whose mapped test failed, or never ran (renamed, deleted), fails.
func TestFailingOrMissingP0TestFails(t *testing.T) {
	events := ev("pass", "internal/httpapi", "TestA") + ev("fail", "internal/limits", "TestB") +
		ev("pass", "internal/httpapi", "TestA/subtest-fails-alone")
	rep, err := check(strings.NewReader(matrix), strings.NewReader(events), Options{Priority: "P0"})
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
	rep, _ := check(strings.NewReader(matrix), strings.NewReader(events), Options{Priority: "P0"})
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "internal/limits.TestB") {
		t.Fatalf("problems %v", rep.Problems)
	}
}

func TestSkipsAreReportedAndOptionallyFatal(t *testing.T) {
	events := ev("pass", "internal/httpapi", "TestA") + ev("skip", "internal/limits", "TestB") + ev("pass", "internal/httpapi", "TestC")
	rep, _ := check(strings.NewReader(matrix), strings.NewReader(events), Options{Priority: "P0"})
	if len(rep.Problems) != 0 || rep.Skipped != 1 {
		t.Fatalf("lenient: %+v", rep)
	}
	rep, _ = check(strings.NewReader(matrix), strings.NewReader(events), Options{Priority: "P0", FailOnSkip: true})
	if len(rep.Problems) != 1 {
		t.Fatalf("strict: %+v", rep)
	}
}

func TestMalformedReference(t *testing.T) {
	bad := "id,priority,go_tests\nSEC-T001,P0,TestNoPackage\n"
	if _, err := check(strings.NewReader(bad), strings.NewReader(""), Options{Priority: "P0"}); err == nil {
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
	rep, err := check(f, strings.NewReader(""), Options{Priority: "P0"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.IDs < 60 || rep.Tests == 0 || len(rep.Problems) != rep.Tests {
		t.Fatalf("with no test events every mapped test is missing: %+v", rep)
	}
}

// Rows not Built are listed as OPEN and fail only with FailOnOpen.
func TestOpenRowsAreListedAndOptionallyFatal(t *testing.T) {
	events := ev("pass", "internal/httpapi", "TestA") + ev("pass", "internal/limits", "TestB") + ev("pass", "internal/httpapi", "TestC")
	rep, _ := check(strings.NewReader(matrix), strings.NewReader(events), Options{Priority: "P0"})
	if len(rep.Problems) != 0 || strings.Join(rep.Open, "|") != "OPEN    SEC-T002 Partial|OPEN    SEC-T003 Blocked" {
		t.Fatalf("lenient: %+v", rep)
	}
	rep, _ = check(strings.NewReader(matrix), strings.NewReader(events), Options{Priority: "P0", FailOnOpen: true})
	if len(rep.Problems) != 2 {
		t.Fatalf("strict: %+v", rep)
	}
}

// A Built row needs evidence; frontend references must name a real test.
func TestFrontendEvidenceIsChecked(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/tests", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/tests/a.spec.ts", []byte(`test("opens with the keyboard", async () => {})`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := "id,priority,title,status,go_tests,frontend_evidence,note\n" +
		`QA-T001,P0,a,Built,,"frontend:tests/a.spec.ts ""opens with the keyboard""",` + "\n" +
		`QA-T002,P0,b,Built,,"frontend:tests/a.spec.ts ""renamed test""",` + "\n" +
		`QA-T003,P0,c,Built,,"frontend:tests/gone.spec.ts ""x""",` + "\n" +
		"QA-T004,P0,d,Built,,frontend:manual review,\n"
	rep, err := check(strings.NewReader(m), strings.NewReader(""), Options{Priority: "P0", Frontend: dir})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(rep.Problems, "\n")
	if len(rep.Problems) != 3 || rep.FrontendChecked != 3 ||
		!strings.Contains(got, `QA-T002 tests/a.spec.ts has no test "renamed test"`) ||
		!strings.Contains(got, "QA-T003 tests/gone.spec.ts (file not found)") ||
		!strings.Contains(got, "NO EVIDENCE QA-T004") {
		t.Fatalf("problems:\n%s", got)
	}
	// Without a frontend checkout the references are counted, not failed.
	rep, _ = check(strings.NewReader(m), strings.NewReader(""), Options{Priority: "P0"})
	if rep.FrontendUnchecked != 3 || len(rep.Problems) != 1 {
		t.Fatalf("unchecked: %+v", rep)
	}
}

// When the frontend repository sits next to this one, every frontend
// reference in the committed matrix names a real test.
func TestCommittedFrontendReferences(t *testing.T) {
	root := "../../../../../zoiko-suite-frontend-platform"
	if _, err := os.Stat(root + "/package.json"); err != nil {
		t.Skip("frontend repository not checked out next to this one")
	}
	for _, p := range []string{"P0", "P1"} {
		f, err := os.Open("../../../../docs/suivo/sec-t-traceability.csv")
		if err != nil {
			t.Fatal(err)
		}
		rep, err := check(f, strings.NewReader(""), Options{Priority: p, Frontend: root})
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, pr := range rep.Problems {
			if strings.HasPrefix(pr, "FRONTEND") || strings.HasPrefix(pr, "NO EVIDENCE") {
				t.Error(pr)
			}
		}
	}
}

