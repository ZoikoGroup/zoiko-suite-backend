package domain

import (
	"regexp"
	"strconv"
	"strings"
)

// ibanLengths is the real IBAN-registry fixed length (country code + check
// digits + BBAN) for every IBAN-using country this service is likely to see
// a destination proposed for. Values taken from the published IBAN
// registry; a country code not in this map is treated as non-IBAN — see
// ValidateAccountFormat's fallback rule.
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16,
	"BG": 22, "BH": 22, "BR": 29, "CH": 21, "CR": 22, "CY": 28, "CZ": 24,
	"DE": 22, "DK": 18, "DO": 28, "EE": 20, "EG": 29, "ES": 24, "FI": 18,
	"FO": 18, "FR": 27, "GB": 22, "GE": 22, "GI": 23, "GL": 18, "GR": 27,
	"GT": 28, "HR": 21, "HU": 28, "IE": 22, "IL": 23, "IQ": 23, "IS": 26,
	"IT": 27, "JO": 30, "KW": 30, "KZ": 20, "LB": 28, "LC": 32, "LI": 21,
	"LT": 20, "LU": 20, "LV": 21, "LY": 25, "MC": 27, "MD": 24, "ME": 22,
	"MK": 19, "MR": 27, "MT": 31, "MU": 30, "NL": 18, "NO": 15, "PK": 24,
	"PL": 28, "PS": 29, "PT": 25, "QA": 29, "RO": 24, "RS": 22, "SA": 24,
	"SC": 31, "SE": 24, "SI": 19, "SK": 24, "SM": 27, "ST": 25, "SV": 28,
	"TL": 23, "TN": 24, "TR": 26, "UA": 29, "VA": 22, "VG": 24, "XK": 20,
}

var ibanStructure = regexp.MustCompile(`^[A-Z]{2}[0-9]{2}[A-Z0-9]+$`)

// nonIBANAccount is a minimal sanity rule for a country this service has no
// IBAN-format rule for (e.g. the US, which uses routing+account numbers, or
// Canada): an account identifier must be a plausible alphanumeric account
// reference, not empty, not absurdly short or long. This is not a
// country-specific routing/account-number validator — none exists anywhere
// in this codebase (see SPEC_DEVIATIONS) — but it is a real structural
// check, not merely "non-empty".
var nonIBANAccount = regexp.MustCompile(`^[A-Za-z0-9]{4,34}$`)

// ErrInvalidAccountFormat is returned by ValidateAccountFormat.
const ErrInvalidAccountFormat = sentinel("account_identifier is not a valid format for the stated country_code")

// ValidateAccountFormat is the literal fix for §4.10's "server-resolved
// context" line's "Format validation; account-country rules." For a country
// this service knows uses IBAN, the identifier must be exactly that
// country's IBAN length, have IBAN structure (two letters, two check
// digits, then alphanumeric BBAN), start with the stated country code, and
// pass the IBAN mod-97 checksum (ISO 7064). For every other country, a
// minimal alphanumeric-length sanity rule applies — this service has no
// per-country routing/account-number validator to call (none exists
// anywhere in this platform; see SPEC_DEVIATIONS A3/A14).
func ValidateAccountFormat(countryCode, accountIdentifier string) error {
	cc := strings.ToUpper(strings.TrimSpace(countryCode))
	acct := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(accountIdentifier), " ", ""))
	if cc == "" || acct == "" {
		return ErrInvalidAccountFormat
	}

	if length, ok := ibanLengths[cc]; ok {
		if len(acct) != length {
			return ErrInvalidAccountFormat
		}
		if !ibanStructure.MatchString(acct) {
			return ErrInvalidAccountFormat
		}
		if acct[:2] != cc {
			return ErrInvalidAccountFormat
		}
		if !ibanChecksumValid(acct) {
			return ErrInvalidAccountFormat
		}
		return nil
	}

	if !nonIBANAccount.MatchString(acct) {
		return ErrInvalidAccountFormat
	}
	return nil
}

// ibanChecksumValid implements the ISO 7064 mod-97-10 check digit algorithm
// (IBAN's own validation rule): move the first four characters to the end,
// convert letters to numbers (A=10 .. Z=35), and the resulting decimal
// string must be congruent to 1 mod 97. Computed digit-by-digit so it never
// needs a bignum for even the longest (34-character) IBAN.
func ibanChecksumValid(iban string) bool {
	rearranged := iban[4:] + iban[:4]
	var sb strings.Builder
	for _, r := range rearranged {
		switch {
		case r >= '0' && r <= '9':
			sb.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			sb.WriteString(strconv.Itoa(int(r-'A') + 10))
		default:
			return false
		}
	}
	remainder := 0
	digits := sb.String()
	for i := 0; i < len(digits); i++ {
		remainder = (remainder*10 + int(digits[i]-'0')) % 97
	}
	return remainder == 1
}
