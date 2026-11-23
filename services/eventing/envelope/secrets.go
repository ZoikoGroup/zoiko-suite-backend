package envelope

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// ZS-EVENT-001 E-15 and section 13: "never include access tokens, passwords,
// private keys, full card data or provider secrets" in an event. EV-T09: "an
// event payload that contains an access token is blocked from publication".
//
// ScanSecrets is deliberately narrow. A scanner that flags ordinary business
// fields gets bypassed, so it looks only for (a) values that are secret
// material wherever they sit (a JWT, a PEM private key, a bearer token, an
// AWS key id, a connection string with a password, a Luhn-valid card number)
// and (b) a small set of key names that name a credential and hold a
// non-empty string. It reports WHERE it found something, never the value.

var (
	jwtValue    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
	pemKeyValue = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	bearerValue = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`)
	awsKeyValue = regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)
	urlPassword = regexp.MustCompile(`[a-z][a-z0-9+.-]*://[^\s/:@]+:[^\s/@]+@`)
	cardDigits  = regexp.MustCompile(`^[0-9][0-9 -]{11,22}[0-9]$`)
)

// credentialKeys are normalised (lower-case letters and digits only) key names
// that denote a credential. Matching is exact on the whole key, so
// "secret_ref" or "token_type" are not flagged.
var credentialKeys = map[string]bool{
	"password": true, "passwd": true, "passphrase": true, "secret": true, "clientsecret": true,
	"apikey": true, "accesstoken": true, "refreshtoken": true, "idtoken": true, "bearertoken": true,
	"privatekey": true, "secretkey": true, "cvv": true, "cvc": true, "cardnumber": true, "pan": true,
}

// ScanSecrets returns the JSON paths (for example "data.auth.access_token") at
// which data holds prohibited secret material, sorted. It never returns values.
func ScanSecrets(data []byte) []string {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil
	}
	var found []string
	walk("data", v, &found)
	sort.Strings(found)
	return found
}

func walk(path string, v any, found *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			p := path + "." + k
			if s, ok := child.(string); ok && strings.TrimSpace(s) != "" && credentialKeys[normalizeKey(k)] {
				*found = append(*found, p)
				continue
			}
			walk(p, child, found)
		}
	case []any:
		for i, child := range x {
			walk(path+"["+itoa(i)+"]", child, found)
		}
	case string:
		if secretValue(x) {
			*found = append(*found, path)
		}
	}
}

func normalizeKey(k string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(k) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func secretValue(s string) bool {
	if len(s) < 12 {
		return false
	}
	if jwtValue.MatchString(s) || pemKeyValue.MatchString(s) || bearerValue.MatchString(s) ||
		awsKeyValue.MatchString(s) || urlPassword.MatchString(s) {
		return true
	}
	return cardDigits.MatchString(strings.TrimSpace(s)) && luhn(s)
}

// luhn reports whether the digits of s (ignoring spaces and dashes) form a
// 13-19 digit number passing the Luhn check, i.e. plausibly a card number.
func luhn(s string) bool {
	var digits []int
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits = append(digits, int(r-'0'))
		case r == ' ' || r == '-':
		default:
			return false
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
