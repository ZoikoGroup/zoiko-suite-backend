// Package policy is the deterministic safety layer that runs before any
// retrieval or model call (Doc 02 §04/§07, Doc 06 §08): sensitive-input
// interception, exception classes X01–X10, intent rules I01–I30, slots and
// response templates.
package policy

import (
	"math/big"
	"regexp"
	"sort"
	"strings"
)

// Sensitive data kinds the chat must never accept, echo or store (Doc 06 §15).
const (
	KindCard     = "payment_card"
	KindIBAN     = "iban"
	KindNationID = "national_or_tax_id"
	KindSecret   = "credential_or_key"
	KindPassword = "password"
	KindOTP      = "one_time_code"
	KindBank     = "bank_account"
)

type detector struct {
	kind  string
	re    *regexp.Regexp
	check func(match string) bool
}

var detectors = []detector{
	{KindSecret, regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), nil},
	{KindSecret, regexp.MustCompile(`\b(?:sk|pk|rk)[-_](?:live|test|proj)?[-_]?[A-Za-z0-9]{16,}\b`), nil},
	{KindSecret, regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), nil},
	{KindSecret, regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`), nil},
	{KindSecret, regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`), nil},
	{KindSecret, regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}\b`), nil},
	{KindSecret, regexp.MustCompile(`(?i)\b(?:api[ _-]?key|secret|access[ _-]?token|bearer)\b\s*(?:is|=|:)?\s*['"]?[A-Za-z0-9_\-./+]{12,}`), nil},
	{KindPassword, regexp.MustCompile(`(?i)\b(?:password|passcode|passwd|pwd)\b\s*(?:is|=|:)\s*\S+`), nil},
	{KindOTP, regexp.MustCompile(`(?i)\b(?:otp|one[- ]time (?:code|password|passcode)|verification code|2fa code|auth(?:entication)? code|security code)\b\D{0,12}\d{4,8}\b`), nil},
	{KindIBAN, regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]){11,30}\b`), validIBAN},
	{KindCard, regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`), luhn},
	{KindNationID, regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), nil},                                               // US SSN
	{KindNationID, regexp.MustCompile(`(?i)\b[A-CEGHJ-PR-TW-Z]{2} ?\d{2} ?\d{2} ?\d{2} ?[A-D]\b`), nil},            // UK NINO
	{KindNationID, regexp.MustCompile(`(?i)\b(?:ein|fein|tin|utr|ssn|nino|pan)\b\D{0,10}\d[\d -]{6,12}\d\b`), nil}, // labelled tax IDs
	{KindBank, regexp.MustCompile(`(?i)\b(?:account|acct|routing|sort code|aba)\b\s*(?:number|no\.?|#)?\s*[:#]?\s*\d[\d -]{5,20}\d\b`), nil},
}

// Redaction is the result of scanning a message.
type Redaction struct {
	Text  string   // message with every detected value replaced
	Kinds []string // distinct kinds found, sorted
}

// Found reports whether any sensitive value was detected.
func (r Redaction) Found() bool { return len(r.Kinds) > 0 }

// Redact masks sensitive values. The returned text is the only form of the
// message that may be passed on (to a model, never to logs).
func Redact(msg string) Redaction {
	kinds := map[string]bool{}
	out := msg
	for _, d := range detectors {
		out = d.re.ReplaceAllStringFunc(out, func(m string) string {
			if d.check != nil && !d.check(m) {
				return m
			}
			kinds[d.kind] = true
			return "[REDACTED:" + d.kind + "]"
		})
	}
	r := Redaction{Text: out}
	for k := range kinds {
		r.Kinds = append(r.Kinds, k)
	}
	sort.Strings(r.Kinds)
	return r
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func luhn(s string) bool {
	d := digitsOnly(s)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	sum, double := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if double {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

func validIBAN(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	rearranged := s[4:] + s[:4]
	var b strings.Builder
	for _, c := range rearranged {
		switch {
		case c >= '0' && c <= '9':
			b.WriteRune(c)
		case c >= 'A' && c <= 'Z':
			b.WriteString(big.NewInt(int64(c - 'A' + 10)).String())
		default:
			return false
		}
	}
	n, ok := new(big.Int).SetString(b.String(), 10)
	return ok && new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}
