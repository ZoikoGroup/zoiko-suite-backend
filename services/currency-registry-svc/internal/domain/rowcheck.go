package domain

import (
	"fmt"
	"strconv"
)

// MaxNameLen bounds the currency name.
const MaxNameLen = 200

func isUpperAlpha3(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

func isDigits3(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ParseMinorUnit parses the submitted exponent as an integer in 0..6. It never
// goes through a float: "2.0", "2.5", "1e0" and "" are all refused.
func ParseMinorUnit(r ImportRow) (int, error) {
	s := r.MinorUnit.String()
	if s == "" {
		return 0, fmt.Errorf("minor_unit is required")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("minor_unit must be an integer, got %q", s)
	}
	if n < MinorUnitMin || n > MinorUnitMax {
		return 0, fmt.Errorf("minor_unit %d outside %d..%d", n, MinorUnitMin, MinorUnitMax)
	}
	return n, nil
}

// CheckRowShape returns every shape problem with a single row (no registry
// lookups): code lengths/charset, name, minor_unit range.
func CheckRowShape(r ImportRow) []string {
	var out []string
	if !isUpperAlpha3(r.AlphaCode) {
		out = append(out, fmt.Sprintf("alpha_code must be exactly 3 uppercase letters, got %q", r.AlphaCode))
	}
	if !isDigits3(r.NumericCode) {
		out = append(out, fmt.Sprintf("numeric_code must be exactly 3 digits, got %q", r.NumericCode))
	}
	if r.Name == "" || len(r.Name) > MaxNameLen {
		out = append(out, "name must be 1..200 characters")
	}
	if _, err := ParseMinorUnit(r); err != nil {
		out = append(out, err.Error())
	}
	return out
}
