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
