package internal_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// nonTestGoFiles walks the service and returns every non-test Go source file.
func nonTestGoFiles(t *testing.T) map[string]string {
	t.Helper()
	root := ".."
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(path)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 10 {
		t.Fatalf("expected to scan the service sources, found only %d files", len(out))
	}
	return out
}

// TestNoHardcodedCurrencyValues enforces the repo doctrine: no currency (or
// country) value as a constant, enum or switch/case in non-test code. The
// probe list lives in this TEST file only; it samples common codes and cannot
// prove absence for all of ISO 4217, which is why the data-model rule (the
// registry is populated only by imports) matters more than this check.
func TestNoHardcodedCurrencyValues(t *testing.T) {
	probe := []string{"USD", "EUR", "GBP", "JPY", "CHF", "CAD", "AUD", "CNY", "INR", "BHD", "KWD", "XAU", "XXX", "840", "978", "826", "392"}
	for path, src := range nonTestGoFiles(t) {
		for _, code := range probe {
			quoted := regexp.MustCompile(`["` + "`" + `]` + code + `["` + "`" + `]`)
			assert.Falsef(t, quoted.MatchString(src), "%s hardcodes the currency/numeric code %q", path, code)
		}
	}
}

// TestNoFloatingPoint enforces the exact-decimal doctrine for this module: a
// currency registry never does arithmetic, so float types have no business here.
func TestNoFloatingPoint(t *testing.T) {
	re := regexp.MustCompile(`\bfloat(32|64)\b|strconv\.(ParseFloat|FormatFloat)`)
	for path, src := range nonTestGoFiles(t) {
		// Prometheus gauges/histograms take float64 by API; telemetry never holds money.
		if strings.Contains(path, "/telemetry/") || strings.HasSuffix(path, "internal/outbox/relay.go") {
			continue
		}
		assert.Falsef(t, re.MatchString(src), "%s uses floating point", path)
	}
}

// TestGateNeverDefaultsOpen guards the anti-pattern this service exists to
// remove: answering "open" because nothing was found. Every path in the gate
// that finds no period must raise PERIOD_NOT_FOUND, and nothing in non-test
// code may map absence to an OPEN status.
func TestGateNeverDefaultsOpen(t *testing.T) {
	files := nonTestGoFiles(t)
	q := files["../internal/service/queries.go"]
	assert.Contains(t, q, "CodePeriodNotFound")
	assert.Equal(t, 2, strings.Count(q, "domain.CodePeriodNotFound"), "gate and status-by-key both raise PERIOD_NOT_FOUND")
	assert.NotRegexp(t, `(?i)default[^\n]*\bopen\b`, strings.ReplaceAll(q, "PERIOD_NOT_FOUND", ""))
	for path, src := range files {
		if strings.Contains(path, "/internal/handler/") || strings.Contains(path, "/internal/service/") {
			assert.NotContains(t, src, `"close_status": "OPEN"`, "%s must not hard-wire an OPEN answer", path)
		}
	}
}

// TestMigrationsEnforceImmutability keeps the database-side guarantees from
// being edited away without a test noticing.
func TestMigrationsEnforceImmutability(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "deployments", "migrations", "000002_immutability.up.sql"))
	assert.NoError(t, err)
	s := string(b)
	for _, want := range []string{"BEFORE UPDATE OR DELETE ON period_state_history", "BEFORE DELETE ON accounting_periods",
		"NEW.start_date <> OLD.start_date", "NEW.period_key <> OLD.period_key", "NEW.calendar_version_id <> OLD.calendar_version_id", "BEFORE TRUNCATE"} {
		assert.Contains(t, s, want)
	}
	m1, _ := os.ReadFile(filepath.Join("..", "deployments", "migrations", "000001_initial_schema.up.sql"))
	assert.Equal(t, 2, strings.Count(string(m1), "FORCE ROW LEVEL SECURITY;"))
}
