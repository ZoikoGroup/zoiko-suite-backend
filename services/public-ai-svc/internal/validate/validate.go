// Package validate is the deterministic post-generation gate (Doc 04 §08
// step 7, Doc 06 §10). Model output is untrusted proposed copy: nothing
// reaches a visitor until it passes here.
package validate

import (
	"errors"
	"regexp"
	"slices"
	"strings"

	claimreg "zoiko.io/public-ai-svc/internal/claims"
	"zoiko.io/public-ai-svc/internal/model"
)

// MaxWords is the answer length shown before "expand" (Doc 01 §12): the
// first MaxWords words carry the complete core answer. MaxFullWords bounds
// the whole answer, including the detail behind "Show more".
const (
	MaxWords     = 120
	MaxFullWords = 240
)

var (
	ErrEmpty            = errors.New("empty answer")
	ErrUnknownReference = errors.New("draft cites claims or evidence it was not given")
	ErrMarkup           = errors.New("markup or script in output")
	ErrProhibitedPhrase = errors.New("prohibited commitment phrase")
	ErrUnsupportedFact  = errors.New("number not present in verified claims or evidence")
	ErrInstructionLeak  = errors.New("output discusses instructions")
	ErrToolInstruction  = errors.New("output contains a tool or function instruction")
	ErrPlaceholder      = errors.New("output contains an unfilled placeholder")
)

var (
	urlRe   = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+|\b[a-z0-9-]+\.(?:com|net|org|io|ai|co|uk|example|invalid)(?:/\S*)?`)
	emailRe = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	// phoneRe never spans a line break: "10000).\n5. Select" is the end of
	// one numbered step and the start of the next, not a phone number.
	phoneRe  = regexp.MustCompile(`\+?\d[\d \t().-]{7,}\d`)
	markupRe = regexp.MustCompile(`(?i)<\s*/?\s*[a-z][^>]*>|javascript:|on[a-z]+\s*=|\]\(|!\[`)
	numberRe = regexp.MustCompile(`\d+(?:[.,]\d+)*%?`)
	toolRe   = regexp.MustCompile(`(?i)tool_call|function_call|"tool"\s*:|<\s*tool|call_tool|execute_action|"arguments"\s*:`)
	// Also the uncleared working name: public UI says "ZoikoSuite Assistant".
	leakRe  = regexp.MustCompile(`(?i)system prompt|my instructions|approved_public_evidence|verified_claim|L1-PROMPT|\bsuivo\b`)
	spaceRe = regexp.MustCompile(`\s{2,}`)
)

// Prohibited unless a verified claim statement contains the exact phrase
// (Doc 02 §16, Doc 10 §08).
var prohibited = []string{"guarantee", "guaranteed", "fully compliant", "100%", "unlimited", "instant", "soc 2 certified",
	"i booked you", "i have booked", "ticket created", "i opened your account", "automatically enrolled", "an agent will call",
	// Master prompt §4 step 10: banned absolutes and claims of having acted.
	"100% compliant", "24/7", "round-the-clock", "works with every", "works with all", "i've booked", "i've submitted",
	"i have submitted", "i've created", "i have created", "i've sent", "i have sent", "i've scheduled", "i have scheduled"}

// ErrFalseConfirmation: the draft confirms an action or promises a response
// that no receiving system has confirmed (Doc 09 §11, §13; ADM-T008/T010).
var ErrFalseConfirmation = errors.New("validate: unconfirmed action or response promise")

// falseConfirmationRe matches confirmed bookings, forwarding or escalation,
// "the team will contact you", response-time promises, case or ticket
// numbers, and "your data was deleted".
var falseConfirmationRe = regexp.MustCompile(`(?i)\b(?:` +
	`(?:i|we)(?:'ve| have)? (?:booked|scheduled|forwarded|escalated|passed on|sent|created|logged|opened|submitted|deleted|erased|removed)\b|` +
	`(?:your|the) (?:request|enquiry|inquiry|ticket|case|demo|meeting|message|details) (?:has|have) been (?:booked|scheduled|forwarded|escalated|passed on|sent|created|logged|submitted|received)|` +
	`(?:someone|the team|a specialist|an agent|sales|support|our team) will (?:contact|call|email|reach out to|get back to|be in touch with) you|` +
	`(?:will|should) (?:respond|reply|get back to you|hear back) within\b|within \d+ (?:minutes|hours|business days|working days|days)\b|` +
	`(?:case|ticket|reference|ref) (?:number|no\.?|id|#)\s*(?:is\s*)?[:#]?\s*[A-Z0-9-]{4,}|` +
	`(?:your|the) (?:personal )?data (?:has been|was|is now) (?:deleted|erased|removed|exported)` +
	`)`)

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
	if claimreg.HasPlaceholder(text) {
		return "", ErrPlaceholder // Doc 02 §17: never show "[price]" or "{{x}}"
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
	if falseConfirmationRe.MatchString(text) {
		// Doc 09 §11/§13: no case, booking, forwarding, contact or deletion
		// is confirmed, and no response time promised, unless a receiving
		// system returned it, which a public chat answer never has.
		return "", ErrFalseConfirmation
	}
	// Doc 03 §13: every figure must appear as a whole number in what the
	// model was given ("4" is not supported by "49").
	groundNums := numberSet(lowerGround)
	for _, n := range numberRe.FindAllString(text, -1) {
		if !groundNums[canonNumber(n)] {
			return "", ErrUnsupportedFact
		}
	}
	// Doc 03 §17: a price or trial length is a P1 fact. It may only repeat a
	// verified claim card, never evidence text or the model's own wording.
	claimNums := numberSet(claimText(claims))
	for _, n := range p1Figures(text) {
		if !claimNums[canonNumber(n)] {
			return "", ErrUnsupportedFact
		}
	}
	// Doc 03 §19 claim-to-span check (citation laundering): each substantive
	// sentence must be supported by what the draft itself cites, not merely
	// by something else it was shown.
	if !spansSupported(text, citedText(d, claims, evidence)) {
		return "", ErrUnsupportedSpan
	}
	return truncateWords(text, MaxFullWords), nil
}

// ErrUnsupportedSpan: a sentence is not supported by the sources it cites.
var ErrUnsupportedSpan = errors.New("a sentence is not supported by its cited sources")

func citedText(d model.Draft, claims []model.ClaimCard, evidence []model.Evidence) string {
	var b strings.Builder
	for _, c := range claims {
		if slices.Contains(d.ClaimRefs, c.Ref) {
			b.WriteString(c.Statement + " ")
		}
	}
	for _, e := range evidence {
		if slices.Contains(d.EvidenceRefs, e.Ref) {
			b.WriteString(e.Title + " " + e.Text + " ")
		}
	}
	return b.String()
}

var (
	sentenceSplitRe = regexp.MustCompile(`[^.!?\n]+[.!?]?`)
	wordRe          = regexp.MustCompile(`[\p{L}\p{N}]+`)
	stopwords       = map[string]bool{"about": true, "after": true, "also": true, "because": true, "before": true, "being": true,
		"between": true, "could": true, "does": true, "each": true, "from": true, "have": true, "into": true, "more": true,
		"most": true, "other": true, "over": true, "such": true, "than": true, "that": true, "their": true, "them": true,
		"then": true, "there": true, "these": true, "they": true, "this": true, "those": true, "through": true, "under": true,
		"what": true, "when": true, "where": true, "which": true, "while": true, "with": true, "within": true, "would": true,
		"your": true, "zoikosuite": true, "will": true, "can't": true, "cannot": true, "should": true, "some": true, "only": true}
)

// stem is a crude prefix stem: "approvals", "approved" and "approval" match.
func stem(w string) string {
	if len(w) > 5 {
		return w[:5]
	}
	return w
}

// spansSupported reports whether every sentence with at least five content
// words has at least half of them in the cited text.
func spansSupported(text, cited string) bool {
	have := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(cited), -1) {
		have[stem(w)] = true
	}
	for _, sent := range sentenceSplitRe.FindAllString(text, -1) {
		var words []string
		for _, w := range wordRe.FindAllString(strings.ToLower(sent), -1) {
			if len(w) >= 4 && !stopwords[w] {
				words = append(words, w)
			}
		}
		if len(words) < 5 {
			continue
		}
		hit := 0
		for _, w := range words {
			if have[stem(w)] {
				hit++
			}
		}
		if hit*2 < len(words) {
			return false
		}
	}
	return true
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

var (
	// currencyRe is an amount with a currency symbol or code on either side.
	currencyRe = regexp.MustCompile(`(?i)(?:[$€£]|\b(?:XTS|USD|GBP|EUR)\b)\s?(\d+(?:[.,]\d+)*)|(\d+(?:[.,]\d+)*)\s?(?:\b(?:XTS|USD|GBP|EUR)\b|[€£])`)
	// trialRe is a trial length: "14-day trial", "30 days free trial",
	// "trial of 30 days", "trial lasts 14 days".
	// capacityRe: user, seat or entity counts (Doc 03 §17 "included
	// users/entities": avoid an absolute capacity promise without a claim).
	capacityRe = regexp.MustCompile(`(?i)\b(\d[\d,]*)\s+(?:legal\s+)?(?:entities|entity|users|seats|companies|subsidiaries)\b`)
	trialRe    = regexp.MustCompile(`(?i)\b(\d+)[\s-]*days?[\s-]+(?:(?:free|self-serve)[\s-]+)?trial\b|\btrial\s+(?:period\s+)?(?:of|for|lasts?|is)\s+(\d+)\s+days?\b`)
)

// p1Figures returns the numbers in currency amounts, trial lengths and
// user/entity capacities: only a signed claim card may state them.
func p1Figures(text string) []string {
	var out []string
	for _, re := range []*regexp.Regexp{currencyRe, trialRe, capacityRe} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			for _, g := range m[1:] {
				if g != "" {
					out = append(out, g)
				}
			}
		}
	}
	return out
}

func claimText(claims []model.ClaimCard) string {
	var b strings.Builder
	for _, c := range claims {
		b.WriteString(strings.ToLower(c.Statement) + " ")
	}
	return b.String()
}

// numberSet holds every whole number token in s, canonicalised.
func numberSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, n := range numberRe.FindAllString(s, -1) {
		out[canonNumber(n)] = true
	}
	return out
}

// canonNumber makes "49.00" and "49", or "1,000" and "1000", the same token;
// a percent sign stays significant.
func canonNumber(n string) string {
	pct := strings.HasSuffix(n, "%")
	n = strings.TrimSuffix(n, "%")
	if i := strings.LastIndexByte(n, '.'); i >= 0 && strings.Trim(n[i+1:], "0") == "" {
		n = n[:i]
	}
	n = strings.ReplaceAll(n, ",", "")
	if pct {
		n += "%"
	}
	return n
}
