package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/currency-registry-svc/internal/domain"
)

// TestStateMachine is table-driven over EVERY (from, to) pair of the status
// vocabulary, so a transition added by accident cannot pass unnoticed.
func TestStateMachine(t *testing.T) {
	legal := map[[2]domain.Status]bool{
		{domain.StatusKnown, domain.StatusSupported}:      true,
		{domain.StatusKnown, domain.StatusRestricted}:     true,
		{domain.StatusSupported, domain.StatusRestricted}: true,
		{domain.StatusRestricted, domain.StatusSupported}: true,
		{domain.StatusSupported, domain.StatusRetired}:    true,
		{domain.StatusRestricted, domain.StatusRetired}:   true,
	}
	for _, from := range domain.AllStatuses {
		for _, to := range domain.AllStatuses {
			from, to := from, to
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				err := domain.ValidateTransition(from, to)
				if legal[[2]domain.Status{from, to}] {
					assert.NoError(t, err)
					assert.True(t, domain.CanTransition(from, to))
					return
				}
				require.Error(t, err)
				de, ok := domain.AsError(err)
				require.True(t, ok)
				assert.Equal(t, domain.CodeInvalidTransition, de.Code)
				assert.False(t, domain.CanTransition(from, to))
			})
		}
	}
}

func TestStateMachine_UnknownStatusIsInvalidTransition(t *testing.T) {
	for _, pair := range [][2]domain.Status{{"BOGUS", domain.StatusSupported}, {domain.StatusKnown, "BOGUS"}, {"", ""}} {
		de, ok := domain.AsError(domain.ValidateTransition(pair[0], pair[1]))
		require.True(t, ok)
		assert.Equal(t, domain.CodeInvalidTransition, de.Code)
	}
}

func TestStateMachine_RetiredIsTerminal(t *testing.T) {
	for _, to := range domain.AllStatuses {
		assert.False(t, domain.CanTransition(domain.StatusRetired, to))
	}
}

func TestCheckRowShape(t *testing.T) {
	num := func(s string) json.Number { return json.Number(s) }
	cases := []struct {
		name string
		row  domain.ImportRow
		bad  bool
	}{
		{"valid", domain.ImportRow{AlphaCode: "ABC", NumericCode: "123", Name: "n", MinorUnit: num("2")}, false},
		{"valid zero exponent", domain.ImportRow{AlphaCode: "ABC", NumericCode: "001", Name: "n", MinorUnit: num("0")}, false},
		{"valid upper bound", domain.ImportRow{AlphaCode: "ABC", NumericCode: "123", Name: "n", MinorUnit: num("6")}, false},
		{"alpha 2", domain.ImportRow{AlphaCode: "AB", NumericCode: "123", Name: "n", MinorUnit: num("2")}, true},
		{"alpha 4", domain.ImportRow{AlphaCode: "ABCD", NumericCode: "123", Name: "n", MinorUnit: num("2")}, true},
		{"alpha digits", domain.ImportRow{AlphaCode: "A1C", NumericCode: "123", Name: "n", MinorUnit: num("2")}, true},
		{"numeric letters", domain.ImportRow{AlphaCode: "ABC", NumericCode: "12A", Name: "n", MinorUnit: num("2")}, true},
		{"numeric 2", domain.ImportRow{AlphaCode: "ABC", NumericCode: "12", Name: "n", MinorUnit: num("2")}, true},
		{"minor 7", domain.ImportRow{AlphaCode: "ABC", NumericCode: "123", Name: "n", MinorUnit: num("7")}, true},
		{"minor -1", domain.ImportRow{AlphaCode: "ABC", NumericCode: "123", Name: "n", MinorUnit: num("-1")}, true},
		{"minor fractional", domain.ImportRow{AlphaCode: "ABC", NumericCode: "123", Name: "n", MinorUnit: num("2.5")}, true},
		{"minor 2.0 is not an integer literal", domain.ImportRow{AlphaCode: "ABC", NumericCode: "123", Name: "n", MinorUnit: num("2.0")}, true},
		{"minor exponent form", domain.ImportRow{AlphaCode: "ABC", NumericCode: "123", Name: "n", MinorUnit: num("1e0")}, true},
		{"minor missing", domain.ImportRow{AlphaCode: "ABC", NumericCode: "123", Name: "n"}, true},
		{"name empty", domain.ImportRow{AlphaCode: "ABC", NumericCode: "123", MinorUnit: num("2")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := domain.CheckRowShape(tc.row)
			if tc.bad {
				assert.NotEmpty(t, errs)
			} else {
				assert.Empty(t, errs)
			}
		})
	}
}

func TestManifestHash(t *testing.T) {
	a := domain.ImportRow{AlphaCode: "AAA", NumericCode: "001", Name: "A", MinorUnit: "2"}
	b := domain.ImportRow{AlphaCode: "BBB", NumericCode: "002", Name: "B|tricky\nname", MinorUnit: "0", FundOrMetalFlag: true}

	h := domain.ManifestHash([]domain.ImportRow{a, b})
	assert.Len(t, h, 64)
	assert.Equal(t, h, domain.ManifestHash([]domain.ImportRow{b, a}), "row order must not matter")
	assert.Equal(t, h, domain.ManifestHash([]domain.ImportRow{a, b}), "deterministic")

	changed := b
	changed.MinorUnit = "1"
	assert.NotEqual(t, h, domain.ManifestHash([]domain.ImportRow{a, changed}), "any field change changes the hash")
	flag := a
	flag.FundOrMetalFlag = true
	assert.NotEqual(t, h, domain.ManifestHash([]domain.ImportRow{flag, b}))

	// A separator inside a name cannot forge another row's boundary.
	x := domain.ImportRow{AlphaCode: "AAA", NumericCode: "001", Name: "A|2|false\nBBB|002|\"x\"", MinorUnit: "2"}
	assert.NotEqual(t, domain.ManifestHash([]domain.ImportRow{x}), domain.ManifestHash([]domain.ImportRow{a}))

	assert.Equal(t, h, domain.NormalizeHash("  SHA256:"+h+" "))
}
