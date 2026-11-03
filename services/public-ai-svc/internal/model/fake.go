package model

import (
	"context"
	"regexp"
	"sort"
	"strings"
)

// Fault makes the Fake provider misbehave, for tests of the controls around
// it. The zero value behaves well.
type Fault string

const (
	FaultNone             Fault = ""
	FaultTimeout          Fault = "timeout"           // blocks until the deadline
	FaultError            Fault = "error"             // permanent failure
	FaultTransientOnce    Fault = "transient_once"    // fails once, then succeeds
	FaultInjectURL        Fault = "inject_url"        // appends a URL, email and phone number
	FaultInventPrice      Fault = "invent_price"      // states a price that is in no claim
	FaultHTML             Fault = "html"              // emits markup
	FaultProhibitedPhrase Fault = "prohibited_phrase" // promises a guarantee
	FaultUnknownRef       Fault = "unknown_ref"       // cites evidence it was not given
	FaultToolCall         Fault = "tool_call"         // emits a tool instruction (CT-033)
)

// Fake is a deterministic, offline provider (profile "fake-v0"). It builds an
// extractive answer from the given claims and evidence and never adds facts.
type Fake struct {
	Fault Fault
	calls int
}

func (f *Fake) Name() string { return "fake-v0" }

// Calls reports how many times Generate ran (tests prove refusals and
// sensitive input never reach the model).
func (f *Fake) Calls() int { return f.calls }

var (
	sentenceRe = regexp.MustCompile(`[^.!?]+[.!?]`)
	headingRe  = regexp.MustCompile(`^\x{00a7}([^\x{00a7}]*)\x{00a7}\s*(.*)$`)
	// qualifierRe marks availability caveats that must stay with the answer
	// they qualify (Doc 02 T02, G01: no claim of universal availability).
	qualifierRe = regexp.MustCompile(`(?i)\b(?:depends? on|depending on|varies|vary by|subject to|not available everywhere)\b`)
)

func (f *Fake) Generate(ctx context.Context, req Request) (Draft, error) {
	f.calls++
	switch f.Fault {
	case FaultTimeout:
		<-ctx.Done()
		return Draft{}, ctx.Err()
	case FaultError:
		return Draft{}, errPermanent
	case FaultTransientOnce:
		if f.calls == 1 {
			return Draft{}, ErrTransient
		}
	}

	d := Draft{InputTokens: (len(BuildPrompt(req)) + 3) / 4}
	var parts []string
	if len(req.Claims) > 0 {
		for _, c := range req.Claims {
			parts = append(parts, c.Statement)
			d.ClaimRefs = append(d.ClaimRefs, c.Ref)
		}
	} else if len(req.Evidence) > 0 {
		// Extractive answer from the best-ranked evidence only (evidence
		// arrives in rank order): up to two sentences that share words with
		// the question, in document order, else the section's first sentence.
		q := queryWords(req.Message)
		top := req.Evidence[0]
		var sentences, headings []string
		heading := top.Title
		for _, s := range sentenceRe.FindAllString(top.Text, -1) {
			s = strings.TrimSpace(s)
			if m := headingRe.FindStringSubmatch(s); m != nil {
				heading, s = m[1], strings.TrimSpace(m[2])
			}
			if strings.HasPrefix(s, "TEST-ONLY") || strings.HasPrefix(s, "Not ZoikoSuite fact") || s == "" {
				continue
			}
			sentences = append(sentences, s)
			headings = append(headings, heading)
		}
		type cand struct {
			s     string
			score int
			order int
		}
		var cands []cand
		for j, s := range sentences {
			sc := 0
			for w := range words(s + " " + headings[j]) {
				if q[w] {
					sc++
				}
			}
			cands = append(cands, cand{s, sc, j})
		}
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
		var picked []cand
		for _, c := range cands {
			if len(picked) == 2 || c.score == 0 {
				break
			}
			picked = append(picked, c)
		}
		if len(picked) == 0 && len(cands) > 0 {
			picked = append(picked, cand{sentences[0], 0, 0})
		}
		// A short passage is one approved statement: use all of it, so a
		// caveat in its last sentence is never cut off.
		if len(picked) > 0 && len(sentences) <= shortPassage {
			picked = picked[:0]
			for j, s := range sentences {
				picked = append(picked, cand{s, 0, j})
			}
		}
		// Never drop the passage's availability qualifier.
		for j, s := range sentences {
			kept := false
			for _, c := range picked {
				kept = kept || c.order == j
			}
			if !kept && len(picked) > 0 && qualifierRe.MatchString(s) {
				picked = append(picked, cand{s, 0, j})
			}
		}
		sort.Slice(picked, func(i, j int) bool { return picked[i].order < picked[j].order })
		focused := false
		for _, c := range picked {
			s, f := shape(c.s, q)
			focused = focused || f
			parts = append(parts, s)
		}
		if len(parts) > 0 {
			d.EvidenceRefs = append(d.EvidenceRefs, top.Ref)
			// The next section of the same article adds its opening sentence,
			// so a one-section answer is not cut short (not when the answer
			// was narrowed to what the visitor asked about).
			if next := req.Evidence[1:]; !focused && len(next) > 0 && next[0].SourceID != "" && next[0].SourceID == top.SourceID {
				if s := passageText(next[0].Text); s != "" && !strings.Contains(d.Text+strings.Join(parts, " "), s) {
					parts = append(parts, s)
					d.EvidenceRefs = append(d.EvidenceRefs, next[0].Ref)
				}
			}
		}
	}
	if len(req.Claims) > 0 {
		d.Text = ListStatements(parts) // same shape the model contract asks for
	} else {
		d.Text = joinParts(parts)
	}

	switch f.Fault {
	case FaultInjectURL:
		d.Text += " Visit https://evil.example/buy or email sales@evil.example or call +1 415 555 0100."
	case FaultInventPrice:
		d.Text += " It costs XTS 9.99 per month with a 50% discount."
	case FaultHTML:
		d.Text += ` <img src=x onerror=alert(1)>`
	case FaultProhibitedPhrase:
		d.Text += " This is guaranteed and fully compliant."
	case FaultToolCall:
		d.Text += ` {"tool_call": {"name": "pay_invoice", "arguments": {"id": "1"}}}`
	case FaultUnknownRef:
		d.EvidenceRefs = append(d.EvidenceRefs, "chunk-not-provided#001")
	}
	d.OutputTokens = (len(d.Text) + 3) / 4
	return d, nil
}

// aliases expand short visitor terms the word set would drop (dev composer
// only; retrieval has its own signed synonym lexicon).
var aliases = map[string][]string{"hr": {"human", "resource"}, "ap": {"payable"}, "ar": {"receivable"}, "gl": {"ledger"}}

// queryWords is the question's word set plus alias expansions.
func queryWords(s string) map[string]bool {
	q := words(s)
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		for _, a := range aliases[w] {
			q[a] = true
		}
	}
	return q
}

// shape turns a long list sentence ("Lead: Label (items), Label (items), and
// Label (items).") into what the visitor asked: only the matching group(s)
// when the question names one (focused), else a bulleted list. It reorders
// the passage's own words and adds none but "include".
func shape(s string, q map[string]bool) (string, bool) {
	colon := strings.Index(s, ":")
	if colon < 0 {
		return s, false
	}
	lead := strings.TrimSpace(s[:colon])
	rest := strings.TrimSuffix(strings.TrimSpace(s[colon+1:]), ".")
	type group struct{ label, items string }
	var gs []group
	for {
		open := strings.Index(rest, "(")
		if open < 0 {
			break
		}
		end := strings.Index(rest[open:], ")")
		if end < 0 {
			return s, false
		}
		end += open
		label := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest[:open]), ","))
		label = strings.TrimSpace(strings.TrimPrefix(label, "and "))
		gs = append(gs, group{label, strings.TrimSpace(rest[open+1 : end])})
		rest = rest[end+1:]
	}
	if len(gs) < 3 || strings.Trim(rest, ",. ") != "" {
		return s, false
	}
	var hit []string
	for _, g := range gs {
		for w := range words(g.label + " " + g.items) {
			if q[w] {
				hit = append(hit, g.label+" ("+g.items+")")
				break
			}
		}
	}
	if len(hit) > 0 && len(hit) < len(gs) {
		subject := lead
		if i := strings.Index(lead, " are "); i > 0 {
			subject = lead[:i]
		}
		return subject + " include " + strings.Join(hit, " and ") + ".", true
	}
	lines := []string{lead + ":"}
	for _, g := range gs {
		lines = append(lines, "- **"+g.label+":** "+g.items)
	}
	return strings.Join(lines, "\n"), false
}

// joinParts joins answer sentences; text after a list starts on its own line
// so it is not read as part of the last list item.
func joinParts(parts []string) string {
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			if strings.Contains(parts[i-1], "\n") {
				b.WriteString("\n")
			} else {
				b.WriteString(" ")
			}
		}
		b.WriteString(p)
	}
	return b.String()
}

// shortPassage is the most sentences a passage may have to be used whole.
const shortPassage = 3

// passageText is a supporting section without its heading marker: all of it
// when it is short, else its opening sentence.
func passageText(text string) string {
	var out []string
	for _, s := range sentenceRe.FindAllString(text, -1) {
		s = strings.TrimSpace(s)
		if m := headingRe.FindStringSubmatch(s); m != nil {
			s = strings.TrimSpace(m[2])
		}
		if s != "" && !strings.HasPrefix(s, "TEST-ONLY") && !strings.HasPrefix(s, "Not ZoikoSuite fact") {
			out = append(out, s)
		}
	}
	if len(out) > shortPassage {
		out = out[:1]
	}
	return strings.Join(out, " ")
}

type permanentError struct{}

func (permanentError) Error() string { return "provider failure" }

var errPermanent = permanentError{}

var wordRe = regexp.MustCompile(`[a-z0-9]+`)

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		if len(w) > 2 && !commonWords[w] {
			out[strings.TrimSuffix(w, "s")] = true
		}
	}
	return out
}

var commonWords = map[string]bool{"the": true, "and": true, "for": true, "does": true, "what": true, "which": true, "with": true,
	"how": true, "are": true, "you": true, "your": true, "can": true, "this": true, "that": true, "from": true, "zoikosuite": true}
