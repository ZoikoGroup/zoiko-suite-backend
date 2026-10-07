package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestParseSubject_AcceptsTextAndPlainPlaceholdersOnly(t *testing.T) {
	vars, err := ParseSubject("Your payslip for {{.period}} is ready, {{.first_name}}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(vars, ",") != "first_name,period" {
		t.Errorf("vars = %v", vars)
	}
	if v, err := ParseSubject("Plain subject with no variables"); err != nil || len(v) != 0 {
		t.Errorf("plain text: %v %v", v, err)
	}
}

func TestParseSubject_RefusesLogicAndAnythingThatIsNotAPlaceholder(t *testing.T) {
	for name, s := range map[string]string{
		"empty":           "   ",
		"too long":        strings.Repeat("a", 201),
		"newline":         "line one\nline two",
		"carriage return": "a\rb",
		"conditional":     "{{if .x}}a{{end}}",
		"range":           "{{range .x}}a{{end}}",
		"function call":   `{{printf "%s" .x}}`,
		"pipeline":        "{{.x | len}}",
		"nested field":    "{{.a.b}}",
		"variable decl":   "{{$x := .a}}{{$x}}",
		"template call":   `{{template "x"}}`,
		"unterminated":    "Hello {{.name",
		"dot only":        "{{.}}",
	} {
		if _, err := ParseSubject(s); !errors.Is(err, ErrSubjectInvalid) {
			t.Errorf("%s: want ErrSubjectInvalid, got %v", name, err)
		}
	}
}

// The author must declare which variables are safe for a subject, and only those.
func TestCheckSubjectVariables(t *testing.T) {
	schema := []string{"first_name", "salary", "period"}
	if err := CheckSubjectVariables("Payslip for {{.period}}", []string{"period"}, schema); err != nil {
		t.Fatalf("a declared, safe variable: %v", err)
	}
	if err := CheckSubjectVariables("Hello {{.first_name}}", []string{"period"}, schema); !errors.Is(err, ErrSubjectInvalid) {
		t.Errorf("a variable not listed as subject-safe must be refused: %v", err)
	}
	if err := CheckSubjectVariables("Salary {{.salary}}", []string{"period"}, schema); !errors.Is(err, ErrSubjectInvalid) {
		t.Errorf("the sensitive variable is not in subject_variables, so it must be refused: %v", err)
	}
	if err := CheckSubjectVariables("Hello", []string{"not_in_schema"}, schema); !errors.Is(err, ErrSubjectInvalid) {
		t.Errorf("subject_variables must be a subset of the schema: %v", err)
	}
}

// NP-08: a value that would break the header is refused, not silently cleaned.
func TestRenderSubject(t *testing.T) {
	out, err := RenderSubject("Payslip for {{.period}}", map[string]string{"period": "October 2026"})
	if err != nil || out != "Payslip for October 2026" {
		t.Fatalf("got %q %v", out, err)
	}
	for name, v := range map[string]string{
		"newline":         "Oct\nBcc: attacker@example.com",
		"carriage return": "Oct\rX",
		"tab":             "Oct\tX",
		"nul":             "Oct\x00X",
	} {
		if _, err := RenderSubject("Payslip for {{.period}}", map[string]string{"period": v}); !errors.Is(err, ErrSubjectInvalid) {
			t.Errorf("%s: want ErrSubjectInvalid, got %v", name, err)
		}
	}
	if _, err := RenderSubject("Payslip for {{.period}}", map[string]string{}); !errors.Is(err, ErrSubjectInvalid) {
		t.Errorf("a missing variable must not render as blank text: %v", err)
	}
	if _, err := RenderSubject("{{.p}}", map[string]string{"p": strings.Repeat("a", 250)}); !errors.Is(err, ErrSubjectInvalid) {
		t.Errorf("an over-long result must be refused: %v", err)
	}
}
