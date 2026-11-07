package policy

import (
	"encoding/base64"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	unorm "golang.org/x/text/unicode/norm"
)

// SafetyFold is the text every safety check reads (Doc 06 SEC-T016, Doc 07
// QA-T040/T043): NFKC turns fullwidth and other compatibility forms into
// plain characters ("４１１１" → "4111", "ｉｇｎｏｒｅ" → "ignore"), invisible
// format characters (zero-width spaces, joiners, direction marks) are
// dropped, and common Cyrillic and Greek look-alikes of Latin letters are
// mapped to Latin, so a detector cannot be bypassed by how text is encoded.
// The folded text is also what reaches retrieval and the model.
func SafetyFold(s string) string {
	s = unorm.NFKC.String(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		if l, ok := confusables[r]; ok {
			r = l
		}
		b.WriteRune(r)
	}
	return b.String()
}

// confusables maps Cyrillic and Greek letters that render like Latin ones.
var confusables = map[rune]rune{
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'у': 'y', 'х': 'x', 'і': 'i', 'ј': 'j', 'ѕ': 's', 'һ': 'h', 'ԁ': 'd', 'ԛ': 'q', 'ԝ': 'w',
	'А': 'A', 'В': 'B', 'Е': 'E', 'К': 'K', 'М': 'M', 'Н': 'H', 'О': 'O', 'Р': 'P', 'С': 'C', 'Т': 'T', 'Х': 'X', 'У': 'Y', 'І': 'I', 'Ј': 'J', 'Ѕ': 'S',
	'α': 'a', 'ο': 'o', 'ρ': 'p', 'ν': 'v', 'ι': 'i', 'κ': 'k', 'τ': 't', 'υ': 'u', 'χ': 'x',
	'Α': 'A', 'Β': 'B', 'Ε': 'E', 'Ζ': 'Z', 'Η': 'H', 'Ι': 'I', 'Κ': 'K', 'Μ': 'M', 'Ν': 'N', 'Ο': 'O', 'Ρ': 'P', 'Τ': 'T', 'Υ': 'Y', 'Χ': 'X',
}

var encodedRunRe = regexp.MustCompile(`[A-Za-z0-9+/_-]{24,}={0,2}`)

// encodedInstruction reports whether a long base64 run decodes to text that
// trips the prompt-injection rules (Doc 06 §08 "Instruction hidden in
// encoded text").
func encodedInstruction(text string) bool {
	for _, run := range encodedRunRe.FindAllString(text, 8) {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			dec, err := enc.DecodeString(run)
			if err != nil || !utf8.Valid(dec) || !printable(dec) {
				continue
			}
			plain := SafetyFold(string(dec))
			for _, re := range exceptionRules[X06PromptInjection] {
				if re.MatchString(plain) {
					return true
				}
			}
		}
	}
	return false
}

func printable(b []byte) bool {
	for _, r := range string(b) {
		if !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// Contact details (Doc 10 §21, Doc 09 §26): the public chat never collects
// them. They are masked before retrieval and the model, without refusing the
// question, and the reply says they were not kept.
var (
	emailRe = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	phoneRe = regexp.MustCompile(`(?:\+\d{1,3}[\s.-]?)?\(?\d{2,5}\)?[\s.-]?\d{2,5}[\s.-]?\d{2,6}(?:[\s.-]?\d{2,5})?`)
)

// ContactNotice is appended to a reply when contact details were removed.
const ContactNotice = "For your privacy, I didn't keep the contact details you shared. Please use the official contact or demo form if you'd like the team to reach you."

const contactMask = "[contact removed]"

// maskContacts replaces emails and phone numbers (9–15 digits, so dates
// like 2026-10-29 and short amounts are left alone).
func maskContacts(text string) (string, bool) {
	found := false
	text = emailRe.ReplaceAllStringFunc(text, func(string) string { found = true; return contactMask })
	text = phoneRe.ReplaceAllStringFunc(text, func(m string) string {
		if n := digits(m); n >= 9 && n <= 15 {
			found = true
			return contactMask
		}
		return m
	})
	return text, found
}

func digits(s string) int {
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n++
		}
	}
	return n
}

// searchNoiseRe matches what masking leaves behind ("my email is
// [contact removed]", "[REDACTED:...]"): words that would steer retrieval
// toward unrelated articles.
var searchNoiseRe = regexp.MustCompile(`(?i)(?:,?\s*(?:and\s+)?(?:my|our)\s+(?:e-?mail(?:\s+address)?|phone(?:\s+number)?|mobile(?:\s+number)?|number|contact(?:\s+details)?)\s+is\s*)?\[(?:contact removed|REDACTED:[a-z_]+)\]`)

// SearchText is the redacted question with masking leftovers removed, for
// retrieval only; the model still receives the masked text.
func SearchText(redacted string) string {
	return strings.TrimSpace(searchNoiseRe.ReplaceAllString(redacted, " "))
}

// HasContact reports whether a message contains an email or phone number.
func HasContact(msg string) bool {
	_, found := maskContacts(SafetyFold(msg))
	return found
}
