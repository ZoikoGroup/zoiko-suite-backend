// Package model is the provider-neutral model gateway (Doc 05 §19,
// Doc 04 §06). It hard-codes tools=[] and no browsing, bounds every call by a
// deadline and token budget, and retries at most once on transient errors.
// Providers: the deterministic Fake (dev/test) and Groq (groq.go).
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"zoiko.io/public-ai-svc/internal/telemetry"
)

// PromptTemplateVersion identifies the prompt layers below (Doc 02 §11).
const PromptTemplateVersion = "L1-PROMPT-001/L2-RESPONSE-001/L3-EVIDENCE-001@dev-1"

// Evidence is one approved public excerpt, passed as quoted data. Only
// records that already passed the backend's eligibility checks (audience,
// approval, validity, market, locale, revocation) become Evidence; the
// metadata travels with the excerpt so the model can weigh and cite it.
type Evidence struct {
	Ref   string // chunk id (the only citation id the model may return)
	Title string
	Text  string
	// SourceID is the source registry id; Authority is its precedence tier
	// (P0-P4); Approval is APPROVED or DRAFT_PENDING_REVIEW (dev only);
	// ValidUntil is RFC 3339; Markets is the scope ("ALL" or market codes).
	SourceID   string
	Authority  string
	Approval   string
	ValidUntil string
	Markets    string
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
	// ModelID is recorded on the model span (gen_ai.request.model).
	ModelID string
}

// Generate calls the provider with a deadline and at most one retry, inside
// a model span that carries usage and outcome only, never messages.
func (g Gateway) Generate(ctx context.Context, req Request) (Draft, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "chat "+g.ModelID, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	span.SetAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.request.model", g.ModelID))
	d, err := g.generate(ctx, req)
	switch {
	case err == nil:
		span.SetAttributes(attribute.Int("gen_ai.usage.input_tokens", d.InputTokens), attribute.Int("gen_ai.usage.output_tokens", d.OutputTokens))
	case errors.Is(err, ErrCircuitOpen):
		telemetry.Fail(span, "circuit_open")
	case errors.Is(err, context.DeadlineExceeded):
		telemetry.Fail(span, "timeout")
	case errors.Is(err, ErrTransient):
		telemetry.Fail(span, "transient")
	default:
		telemetry.Fail(span, "provider")
	}
	return d, err
}

func (g Gateway) generate(ctx context.Context, req Request) (Draft, error) {
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

// BuildMessages renders what a real provider receives: a system message (the
// pinned Suivo system prompt, L2-RESPONSE-001 and the server's output
// contract) and a user message holding the verified claim cards, the
// L3-EVIDENCE-001 wrapper with each eligible record and its metadata as
// non-instructional data, and the visitor's redacted question last as
// untrusted input (Doc 02 §11–§14). Only PromptVersion is ever logged.
func BuildMessages(req Request) (system, user string) {
	var s strings.Builder
	fmt.Fprintf(&s, "%s\n\n<compose version=%q policy=%q intent=%q>\n%s\n", SystemPrompt, PromptVersion, req.PolicyVersion, req.IntentID, L2ResponsePrompt)
	s.WriteString(ServerContract + "\n</compose>")

	var u strings.Builder
	if req.RepairNote != "" {
		u.WriteString("[REPAIR] " + req.RepairNote + "\n")
	}
	for _, c := range req.Claims {
		fmt.Fprintf(&u, "<verified_claim id=%q>%s</verified_claim>\n", c.Ref, escape(c.Statement))
	}
	u.WriteString("<approved_public_evidence>\n" + L3EvidencePreamble + "\n")
	for _, e := range req.Evidence {
		fmt.Fprintf(&u, "<record id=%q source_id=%q title=%q authority=%q approval=%q valid_until=%q markets=%q>%s</record>\n",
			e.Ref, e.SourceID, escape(e.Title), e.Authority, e.Approval, e.ValidUntil, e.Markets, escape(e.Text))
	}
	u.WriteString("</approved_public_evidence>\n")
	u.WriteString("<visitor_message>" + escape(req.Message) + "</visitor_message>\n")
	return s.String(), u.String()
}

// BuildPrompt is BuildMessages as one string (token estimates and tests).
func BuildPrompt(req Request) string {
	system, user := BuildMessages(req)
	return system + "\n" + user
}

func escape(s string) string {
	return strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(s)
}
