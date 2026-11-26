package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Hundredths is a fixed-point decimal with exactly two fractional digits,
// stored as an integer count of hundredths: 42.50 is Hundredths(4250).
//
// Scores, weights and thresholds are all NUMERIC(5,2) in Postgres. Doing the
// arithmetic in float64 (as the 000001-era code did) made results depend on
// binary rounding — 3 privacy incidents x 33.3 gave 99.9, not 100 — so every
// calculation in this service is integer arithmetic on Hundredths instead.
//
// JSON: marshalled as a string ("42.50") so no client parses it into a float.
// Unmarshal accepts either a string or a JSON number with at most two decimals.
type Hundredths int64

// OneHundredPercent is 100.00.
const OneHundredPercent Hundredths = 10000

// String renders the value with exactly two decimals, e.g. "42.50", "-3.05".
func (h Hundredths) String() string {
	sign := ""
	value := int64(h)
	if value < 0 {
		sign = "-"
		value = -value
	}
	return fmt.Sprintf("%s%d.%02d", sign, value/100, value%100)
}

// ParseHundredths parses "42", "42.5" or "42.50". More than two decimals,
// exponents, or anything non-numeric is an error — precision is never
// silently dropped.
func ParseHundredths(raw string) (Hundredths, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, fmt.Errorf("empty decimal")
	}

	negative := false
	if strings.HasPrefix(text, "-") {
		negative = true
		text = text[1:]
	}

	wholePart, fractionPart, hasFraction := strings.Cut(text, ".")
	if wholePart == "" || !isAllDigits(wholePart) {
		return 0, fmt.Errorf("invalid decimal %q", raw)
	}
	if hasFraction {
		if fractionPart == "" || len(fractionPart) > 2 || !isAllDigits(fractionPart) {
			return 0, fmt.Errorf("invalid decimal %q: at most two decimal places", raw)
		}
	}
	if len(wholePart) > 12 {
		return 0, fmt.Errorf("decimal %q out of range", raw)
	}

	whole, err := strconv.ParseInt(wholePart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid decimal %q", raw)
	}

	fraction := int64(0)
	if hasFraction {
		if len(fractionPart) == 1 {
			fractionPart += "0"
		}
		fraction, _ = strconv.ParseInt(fractionPart, 10, 64)
	}

	value := whole*100 + fraction
	if negative {
		value = -value
	}
	return Hundredths(value), nil
}

func isAllDigits(text string) bool {
	for _, character := range text {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// MarshalJSON writes the value as a JSON string.
func (h Hundredths) MarshalJSON() ([]byte, error) {
	return json.Marshal(h.String())
}

// UnmarshalJSON accepts "42.50" or 42.50.
func (h *Hundredths) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "null" {
		return fmt.Errorf("decimal must not be null")
	}
	if strings.HasPrefix(text, "\"") {
		var quoted string
		if err := json.Unmarshal(data, &quoted); err != nil {
			return err
		}
		text = quoted
	}
	parsed, err := ParseHundredths(text)
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}

// divideRoundHalfUp returns numerator/denominator rounded half away from
// zero. Both arguments must be non-negative (all scoring inputs are).
func divideRoundHalfUp(numerator, denominator int64) int64 {
	return (numerator*2 + denominator) / (denominator * 2)
}
