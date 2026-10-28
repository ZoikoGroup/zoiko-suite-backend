// Package validate is the deterministic post-generation gate (Doc 04 §08
// step 7, Doc 06 §10). Model output is untrusted proposed copy: nothing
// reaches a visitor until it passes here.
package validate

import (
	"errors"
	"regexp"
	"strings"

	"zoiko.io/public-ai-svc/internal/model"
)

// MaxWords is the answer length before "expand" (Doc 01 §12).
const MaxWords = 120

var (
	ErrEmpty            = errors.New("empty answer")
	ErrUnknownReference = errors.New("draft cites claims or evidence it was not given")
	ErrMarkup           = errors.New("markup or script in output")
	ErrProhibitedPhrase = errors.New("prohibited commitment phrase")
	ErrUnsupportedFact  = errors.New("number not present in verified claims or evidence")
	ErrInstructionLeak  = errors.New("output discusses instructions")
	ErrToolInstruction  = errors.New("output contains a tool or function instruction")
)

var (
	urlRe    = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+|\b[a-z0-9-]+\.(?:com|net|org|io|ai|co|uk|example|invalid)(?:/\S*)?`)
	emailRe  = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	phoneRe  = regexp.MustCompile(`\+?\d[\d\s().-]{7,}\d`)
	markupRe = regexp.MustCompile(`(?i)<\s*/?\s*[a-z][^>]*>|javascript:|on[a-z]+\s*=|\]\(|!\[`)
	numberRe = regexp.MustCompile(`\d+(?:[.,]\d+)*%?`)
	toolRe   = regexp.MustCompile(`(?i)tool_call|function_call|"tool"\s*:|<\s*tool|call_tool|execute_action|"arguments"\s*:`)
	leakRe   = regexp.MustCompile(`(?i)system prompt|my instructions|approved_public_evidence|verified_claim|L1-PROMPT`)
	spaceRe  = regexp.MustCompile(`\s{2,}`)
)

// Prohibited unless a verified claim statement contains the exact phrase
// (Doc 02 §16, Doc 10 §08).
var prohibited = []string{"guarantee", "guaranteed", "fully compliant", "100%", "unlimited", "instant", "soc 2 certified",
	"i booked you", "i have booked", "ticket created", "i opened your account", "automatically enrolled", "an agent will call",
	// Master prompt §4 step 10: banned absolutes and claims of having acted.
	"100% compliant", "24/7", "round-the-clock", "works with every", "works with all", "i've booked", "i've submitted",
	"i have submitted", "i've created", "i have created", "i've sent", "i have sent", "i've scheduled", "i have scheduled"}

// Check validates a draft against the claims and evidence actually supplied.
// It returns the cleaned text: model-produced URLs, emails and phone numbers
// are removed (links come only from the route registry).
func Check(d model.Draft, claims []model.ClaimCard, evidence []model.Evidence) (string, error) {
	allowedClaim := map[string]bool{}
	var grounded strings.Builder
	for _, c := range claims {
		allowedClaim[c.Ref] = true
		grounded.WriteString(c.Statement + " ")
	}
	allowedEv := map[string]bool{}
	for _, e := range evidence {
		allowedEv[e.Ref] = true
		grounded.WriteString(e.Title + " " + e.Text + " ")
	}
	for _, r := range d.ClaimRefs {
		if !allowedClaim[r] {
			return "", ErrUnknownReference
		}
	}
	for _, r := range d.EvidenceRefs {
		if !allowedEv[r] {
			return "", ErrUnknownReference
		}
	}
	if len(d.ClaimRefs)+len(d.EvidenceRefs) == 0 {
		return "", ErrUnknownReference // an answer must be grounded in something it was given
	}

	text := d.Text
	if toolRe.MatchString(text) {
		return "", ErrToolInstruction
	}
	if markupRe.MatchString(text) {
		return "", ErrMarkup
	}
	text = emailRe.ReplaceAllString(text, "") // before URLs, or the domain part is stripped first
	text = urlRe.ReplaceAllString(text, "")
	text = phoneRe.ReplaceAllString(text, "")
	text = strings.TrimSpace(spaceRe.ReplaceAllString(text, " "))
	if text == "" {
		return "", ErrEmpty
	}
	if leakRe.MatchString(text) {
		return "", ErrInstructionLeak
	}

	lowerText := strings.ToLower(text)
	lowerGround := strings.ToLower(grounded.String())
	for _, p := range prohibited {
		if strings.Contains(lowerText, p) && !claimAllows(claims, p) {
			return "", ErrProhibitedPhrase
		}
	}
	for _, n := range numberRe.FindAllString(text, -1) {
		if !strings.Contains(lowerGround, strings.ToLower(n)) {
			return "", ErrUnsupportedFact
		}
	}
	return truncateWords(text, MaxWords), nil
}

func claimAllows(claims []model.ClaimCard, phrase string) bool {
	for _, c := range claims {
		if strings.Contains(strings.ToLower(c.Statement), phrase) {
			return true
		}
	}
	return false
}

// truncateWords keeps whole sentences within max words.
func truncateWords(s string, max int) string {
	if len(strings.Fields(s)) <= max {
		return s
	}
	var out []string
	n := 0
	for _, sent := range regexp.MustCompile(`[^.!?]+[.!?]?`).FindAllString(s, -1) {
		w := len(strings.Fields(sent))
		if n+w > max {
			break
		}
		out = append(out, strings.TrimSpace(sent))
		n += w
	}
	if len(out) == 0 {
		return strings.Join(strings.Fields(s)[:max], " ") + "…"
	}
	return strings.Join(out, " ")
}
