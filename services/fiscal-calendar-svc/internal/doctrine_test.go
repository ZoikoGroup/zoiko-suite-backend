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

// sourceFiles walks the service and returns every non-test source file with
// one of the given suffixes.
func sourceFiles(t *testing.T, suffixes ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, s := range suffixes {
			if strings.HasSuffix(path, s) {
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				out[filepath.ToSlash(path)] = string(b)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestNoHardcodedJurisdictionValues enforces the repo doctrine: no country,
// jurisdiction or currency value as a constant, enum or switch/case in non-test
// code or migrations. Calendar patterns, fiscal-year starts and book/basis
// scopes are DATA. The probe list lives in this TEST file only; it samples
// common values and cannot prove absence for every jurisdiction, which is why
// the data-model rule (scope is an opaque string; patterns are JSONB) matters
// more than this check.
func TestNoHardcodedJurisdictionValues(t *testing.T) {
	probe := []string{
		"US", "GB", "UK", "DE", "FR", "IN", "JP", "CN", "AU", "CA", "SG", "AE", "BR", "ZA",
		"USA", "GBR", "DEU", "FRA", "IND", "JPN", "CHN", "AUS", "CAN",
		"USD", "EUR", "GBP", "JPY", "CHF", "CNY", "INR", "AUD", "CAD",
		"IFRS", "GAAP", "US-GAAP", "UK-GAAP", "HMRC", "IRS",
	}
	files := sourceFiles(t, ".go", ".sql")
	if len(files) < 20 {
		t.Fatalf("expected to scan the service sources, found only %d files", len(files))
	}
	for path, src := range files {
		for _, code := range probe {
			quoted := regexp.MustCompile(`["` + "`" + `']` + regexp.QuoteMeta(code) + `["` + "`" + `']`)
			assert.Falsef(t, quoted.MatchString(src), "%s hardcodes the jurisdiction/currency/standard value %q", path, code)
		}
	}
}

// TestNoFloatingPoint enforces the exact-arithmetic doctrine: calendar maths is
// integer days and weeks.
func TestNoFloatingPoint(t *testing.T) {
	re := regexp.MustCompile(`\bfloat(32|64)\b|strconv\.(ParseFloat|FormatFloat)`)
	for path, src := range sourceFiles(t, ".go") {
		// Prometheus gauges/histograms take float64 by API; telemetry never holds calendar data.
		if strings.Contains(path, "/telemetry/") || strings.HasSuffix(path, "internal/outbox/relay.go") {
			continue
		}
		assert.Falsef(t, re.MatchString(src), "%s uses floating point", path)
	}
}

// TestNoSoftDelete: material objects are never soft-deleted; they are
// superseded by status transition and end-dating.
func TestNoSoftDelete(t *testing.T) {
	re := regexp.MustCompile(`(?i)\b(deleted_at|is_deleted|soft_delete)\b`)
	for path, src := range sourceFiles(t, ".go", ".sql") {
		assert.Falsef(t, re.MatchString(src), "%s looks like soft delete", path)
	}
}

// TestEveryTableForcesRowLevelSecurity: every table the migrations create is
// tenant-scoped and has FORCE RLS.
func TestEveryTableForcesRowLevelSecurity(t *testing.T) {
	create := regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS (\w+)`)
	var all strings.Builder
	var tables []string
	for path, src := range sourceFiles(t, ".up.sql") {
		all.WriteString(src)
		for _, m := range create.FindAllStringSubmatch(src, -1) {
			tables = append(tables, m[1])
		}
		_ = path
	}
	if len(tables) < 6 {
		t.Fatalf("expected the service's tables, found %v", tables)
	}
	sqlText := all.String()
	for _, tbl := range tables {
		assert.Containsf(t, sqlText, "ALTER TABLE "+tbl+" FORCE ROW LEVEL SECURITY;", "table %s must FORCE row-level security", tbl)
	}
}
