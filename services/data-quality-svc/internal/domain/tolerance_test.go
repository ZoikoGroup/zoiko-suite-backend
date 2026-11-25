package domain_test

import (
	"testing"

	"zoiko.io/data-quality-svc/internal/domain"
)

func tol(kind, value string, scale int, mode string) *domain.Tolerance {
	return &domain.Tolerance{Kind: kind, Value: value, Scale: scale, Rounding: mode, Population: "all"}
}

func eval(t *testing.T, tl *domain.Tolerance, pass, fail int64, sum string) domain.DQResultStatus {
	t.Helper()
	got, err := domain.EvaluateTolerance(tl, "r", pass, fail, sum)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return got
}

func TestTolerance_NoEvidenceNoFailureNoTolerance(t *testing.T) {
	if got := eval(t, tol("ABSOLUTE", "5", 2, "HALF_UP"), 0, 0, "0"); got != domain.ResultIndeterminate {
		t.Fatalf("no records: %s", got)
	}
	if got := eval(t, tol("ABSOLUTE", "5", 2, "HALF_UP"), 10, 0, "0"); got != domain.ResultPass {
		t.Fatalf("no failures: %s", got)
	}
	if got := eval(t, nil, 99, 1, "0"); got != domain.ResultFail {
		t.Fatalf("a failure with no tolerance: %s", got)
	}
}

// Rounding happens before the comparison, to the tolerance's own scale and by its own mode.
func TestTolerance_RoundingModesAtTheBoundary(t *testing.T) {
	cases := []struct {
		name  string
		mode  string
		scale int
		limit string
		sum   string
		want  domain.DQResultStatus
	}{
		// scale 0, limit 50
		{"HALF_UP 50.4 rounds to 50", "HALF_UP", 0, "50", "50.4", domain.ResultPass},
		{"HALF_UP 50.5 rounds to 51", "HALF_UP", 0, "50", "50.5", domain.ResultFail},
		{"DOWN 50.9 rounds to 50", "DOWN", 0, "50", "50.9", domain.ResultPass},
		{"DOWN 51.0 is 51", "DOWN", 0, "50", "51.0", domain.ResultFail},
		{"HALF_EVEN 50.5 rounds to the even 50", "HALF_EVEN", 0, "50", "50.5", domain.ResultPass},
		{"HALF_EVEN 51.5 rounds to the even 52", "HALF_EVEN", 0, "51", "51.5", domain.ResultFail},
		{"HALF_EVEN 50.6 rounds up to 51", "HALF_EVEN", 0, "50", "50.6", domain.ResultFail},
		{"HALF_EVEN 50.4 rounds down to 50", "HALF_EVEN", 0, "50", "50.4", domain.ResultPass},
		// scale 2, limit 1.00
		{"scale 2 HALF_UP 1.004 is 1.00", "HALF_UP", 2, "1.00", "1.004", domain.ResultPass},
		{"scale 2 HALF_UP 1.005 is 1.01", "HALF_UP", 2, "1.00", "1.005", domain.ResultFail},
		{"scale 2 DOWN 1.009 is 1.00", "DOWN", 2, "1.00", "1.009", domain.ResultPass},
		{"scale 2 HALF_EVEN 1.005 is 1.00", "HALF_EVEN", 2, "1.00", "1.005", domain.ResultPass},
		{"scale 2 HALF_EVEN 1.015 is 1.02", "HALF_EVEN", 2, "1.01", "1.015", domain.ResultFail},
		// exactly at the limit
		{"at the limit passes", "HALF_UP", 2, "1.00", "1.00", domain.ResultPass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := eval(t, tol("ABSOLUTE", c.limit, c.scale, c.mode), 10, 1, c.sum); got != c.want {
				t.Fatalf("%s %v vs %s at scale %d: got %s, want %s", c.mode, c.sum, c.limit, c.scale, got, c.want)
			}
		})
	}
}

func TestTolerance_PercentIsFailuresOverEvaluatedRecords(t *testing.T) {
	// 1 failure in 8 evaluated = 12.5%.
	if got := eval(t, tol("PERCENT", "12.5", 1, "HALF_UP"), 7, 1, "0"); got != domain.ResultPass {
		t.Fatalf("12.5%% vs 12.5: %s", got)
	}
	if got := eval(t, tol("PERCENT", "12", 0, "HALF_UP"), 7, 1, "0"); got != domain.ResultFail {
		t.Fatalf("12.5%% rounds HALF_UP to 13 at scale 0, over 12: %s", got)
	}
	if got := eval(t, tol("PERCENT", "12", 0, "DOWN"), 7, 1, "0"); got != domain.ResultPass {
		t.Fatalf("12.5%% rounds DOWN to 12 at scale 0, within 12: %s", got)
	}
	if got := eval(t, tol("PERCENT", "12", 0, "HALF_EVEN"), 7, 1, "0"); got != domain.ResultPass {
		t.Fatalf("12.5%% rounds HALF_EVEN to 12, within 12: %s", got)
	}
	// 1 in 3 = 33.333...%, which is a repeating decimal: exact arithmetic, no float error.
	if got := eval(t, tol("PERCENT", "33.33", 2, "HALF_UP"), 2, 1, "0"); got != domain.ResultPass {
		t.Fatalf("33.333...%% rounds to 33.33 at scale 2: %s", got)
	}
	if got := eval(t, tol("PERCENT", "33.32", 2, "HALF_UP"), 2, 1, "0"); got != domain.ResultFail {
		t.Fatalf("33.333...%% rounds to 33.33 at scale 2, over 33.32: %s", got)
	}
}

func TestTolerance_BadInputIsAnErrorNeverAGuess(t *testing.T) {
	bad := []struct {
		name string
		tl   *domain.Tolerance
		sum  string
	}{
		{"value not a decimal", tol("ABSOLUTE", "abc", 2, "HALF_UP"), "1"},
		{"sum not a decimal", tol("ABSOLUTE", "1", 2, "HALF_UP"), "abc"},
		{"unknown kind", tol("RATIO", "1", 2, "HALF_UP"), "1"},
		{"unknown rounding", tol("ABSOLUTE", "1", 2, "CEILING"), "1"},
		{"scale too large", tol("ABSOLUTE", "1", 19, "HALF_UP"), "1"},
		{"negative scale", tol("ABSOLUTE", "1", -1, "HALF_UP"), "1"},
		{"negative sum", tol("ABSOLUTE", "1", 2, "HALF_UP"), "-1"},
	}
	for _, c := range bad {
		if _, err := domain.EvaluateTolerance(c.tl, "r", 9, 1, c.sum); err == nil {
			t.Fatalf("%s: no error", c.name)
		}
	}
}
