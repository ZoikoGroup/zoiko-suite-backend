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
)

// Fake is a deterministic, offline provider (profile "fake-v0"). It builds an
// extractive answer from the given claims and evidence and never adds facts.
type Fake struct {
	Fault Fault
	calls int
}

func (f *Fake) Name() string { return "fake-v0" }

var (
	sentenceRe = regexp.MustCompile(`[^.!?]+[.!?]`)
	headingRe  = regexp.MustCompile(`^\x{00a7}([^\x{00a7}]*)\x{00a7}\s*(.*)$`)
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
		q := words(req.Message)
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
		sort.Slice(picked, func(i, j int) bool { return picked[i].order < picked[j].order })
		for _, c := range picked {
			parts = append(parts, c.s)
		}
		if len(parts) > 0 {
			d.EvidenceRefs = append(d.EvidenceRefs, top.Ref)
		}
	}
	d.Text = strings.Join(parts, " ")

	switch f.Fault {
	case FaultInjectURL:
		d.Text += " Visit https://evil.example/buy or email sales@evil.example or call +1 415 555 0100."
	case FaultInventPrice:
		d.Text += " It costs XTS 9.99 per month with a 50% discount."
	case FaultHTML:
		d.Text += ` <img src=x onerror=alert(1)>`
	case FaultProhibitedPhrase:
		d.Text += " This is guaranteed and fully compliant."
	case FaultUnknownRef:
		d.EvidenceRefs = append(d.EvidenceRefs, "chunk-not-provided#001")
	}
	d.OutputTokens = (len(d.Text) + 3) / 4
	return d, nil
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
