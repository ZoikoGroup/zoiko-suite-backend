// Package model is the provider-neutral model gateway (Doc 05 §19,
// Doc 04 §06). It hard-codes tools=[] and no browsing, bounds every call by a
// deadline and token budget, and retries at most once on transient errors.
// Until a vendor is approved (DEC-05, ADR-003) only the Fake provider exists.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PromptTemplateVersion identifies the prompt layers below (Doc 02 §11).
const PromptTemplateVersion = "L1-PROMPT-001/L2-RESPONSE-001/L3-EVIDENCE-001@dev-1"

// Evidence is one approved public excerpt, passed as quoted data.
type Evidence struct {
	Ref   string // chunk id
	Title string
	Text  string
}

// ClaimCard is a VERIFIED P1 claim. Its statement is the only permitted
// wording for that fact.
type ClaimCard struct {
	Ref       string // claim id
	Statement string
}

// Request is everything a provider may see. There is no field for tools,
// URLs to fetch, credentials or session identifiers.
type Request struct {
	PolicyVersion   string
	IntentID        string
	Message         string // redacted visitor message (L4, untrusted)
	Evidence        []Evidence
	Claims          []ClaimCard
	MaxOutputTokens int
	RepairNote      string // set on the single repair attempt
}

// Draft is untrusted proposed copy: it is validated before use.
type Draft struct {
	Text         string
	ClaimRefs    []string
	EvidenceRefs []string
	InputTokens  int
	OutputTokens int
}

// Provider is a model backend.
type Provider interface {
	Name() string
	Generate(ctx context.Context, req Request) (Draft, error)
}

// ErrTransient marks errors that may be retried once.
var ErrTransient = errors.New("transient provider error")

// Gateway wraps a provider with the R0 controls.
type Gateway struct {
	Provider Provider
	Timeout  time.Duration
	// Breaker, when set, stops calling a failing provider (A29).
	Breaker *Breaker
}

// Generate calls the provider with a deadline and at most one retry.
func (g Gateway) Generate(ctx context.Context, req Request) (Draft, error) {
	if req.MaxOutputTokens <= 0 || req.MaxOutputTokens > 2000 {
		req.MaxOutputTokens = 400
	}
	if g.Breaker != nil && !g.Breaker.allow() {
		return Draft{}, ErrCircuitOpen
	}
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, g.Timeout)
		d, err := g.Provider.Generate(cctx, req)
		cancel()
		if err == nil {
			if g.Breaker != nil {
				g.Breaker.success()
			}
			return d, nil
		}
		last = err
		if !errors.Is(err, ErrTransient) || ctx.Err() != nil {
			break
		}
	}
	if g.Breaker != nil {
		g.Breaker.failure()
	}
	return Draft{}, last
}

// BuildPrompt renders the layered prompt a real provider adapter sends:
// L1-PROMPT-001, L2-RESPONSE-001, typed fact cards, the L3-EVIDENCE-001
// wrapper holding evidence as non-instructional data, and the visitor message
// last as untrusted input (Doc 02 §11–§14). The layers are pinned by hash in
// prompts.go; only PromptVersion is ever logged.
func BuildPrompt(req Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<system version=%q policy=%q>\n%s\n</system>\n", PromptVersion, req.PolicyVersion, L1Prompt)
	fmt.Fprintf(&b, "<compose intent=%q>\n%s\n", req.IntentID, L2ResponsePrompt)
	b.WriteString("Server contract: use verified claim statements verbatim; no URLs, emails or phone numbers; max 120 words. ")
	b.WriteString("Reply as JSON {\"text\":..., \"claim_refs\":[...], \"evidence_refs\":[...]}.\n</compose>\n")
	if req.RepairNote != "" {
		b.WriteString("[REPAIR] " + req.RepairNote + "\n")
	}
	for _, c := range req.Claims {
		fmt.Fprintf(&b, "<verified_claim id=%q>%s</verified_claim>\n", c.Ref, escape(c.Statement))
	}
	b.WriteString("<approved_public_evidence>\n" + L3EvidencePreamble + "\n")
	for _, e := range req.Evidence {
		fmt.Fprintf(&b, "<record source_id=%q title=%q approved_for_public=\"true\">%s</record>\n", e.Ref, escape(e.Title), escape(e.Text))
	}
	b.WriteString("</approved_public_evidence>\n")
	b.WriteString("<visitor_message>" + escape(req.Message) + "</visitor_message>\n")
	return b.String()
}

func escape(s string) string {
	return strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(s)
}
