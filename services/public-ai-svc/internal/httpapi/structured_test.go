package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/policy"
	"zoiko.io/public-ai-svc/internal/session"
	"zoiko.io/public-ai-svc/internal/store"
)

type fixedFlags struct{ v flags.Values }

func (f fixedFlags) Values() flags.Values { return f.v }

type linkConfig struct{ staticConfig }

func (linkConfig) PublicConfig() contract.ConfigResponse {
	c := staticConfig{}.PublicConfig()
	c.CitationLinks = []contract.CitationLink{{URLKey: "kb-overview", Title: "ZoikoSuite overview"}}
	return c
}

type routeMap map[string]string

func (m routeMap) ResolveRoute(key, _ string) (string, bool) {
	u, ok := m[key]
	return u, ok
}

func structuredHarness(t *testing.T, on bool, res orchestrator.Result) *harness {
	t.Helper()
	cfg := testConfig()
	cfg.PolicyVersion, cfg.ModelTimeout = "p", time.Second
	st := store.NewMemory()
	v := flags.Defaults()
	v.StructuredResponse = on
	ans := answererFunc(func(context.Context, orchestrator.Input) orchestrator.Result { return res })
	srv := New(cfg, nil, Deps{Store: st, Sessions: session.New(st, time.Hour), Answerer: ans, Config: linkConfig{},
		Routes: routeMap{"pricing": "https://www.zoikosuite.com/pricing", "trust": "https://pending.invalid/trust"},
		Flags:  fixedFlags{v}, Ready: func() bool { return true }})
	return &harness{t: t, h: srv.Router(), store: st}
}

var pricingAnswer = orchestrator.Result{
	Status: contract.Answered, IntentID: "I05", Text: "Starter, Growth and Enterprise plans are available.", KnowledgeReleaseID: "kb",
	Citations: []contract.Citation{{SourceVersionID: "claims-1", ClaimIDs: []string{"c1"}, URLKey: "pricing"}, {SourceVersionID: "sv-1", URLKey: "kb-overview"}},
	Actions:   []contract.Action{{Type: contract.ActionOpenRoute, RouteKey: "pricing"}, {Type: contract.ActionOpenRoute, RouteKey: "trust"}},
}

// ADR-008: with the flag off, /menu is absent and turns carry none of the
// structured fields, so existing clients see the unchanged contract.
func TestStructuredOffKeepsContract(t *testing.T) {
	h := structuredHarness(t, false, pricingAnswer)
	if w := h.req(http.MethodGet, "/public-ai/v1/menu", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("/menu with flag off: %d", w.Code)
	}
	s := h.newSession("key-structured-off")
	body := h.turn(s, turnUUID, "What plans are available?").Body.String()
	for _, f := range []string{`"outcome"`, `"followups"`, `"cta"`, `"title"`} {
		if strings.Contains(body, f) {
			t.Errorf("flag off but %s present: %s", f, body)
		}
	}
}

func TestMenuServesDoc02Families(t *testing.T) {
	h := structuredHarness(t, true, pricingAnswer)
	w := h.req(http.MethodGet, "/public-ai/v1/menu", "", nil)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("/menu: %d %s", w.Code, w.Body.String())
	}
	var m contract.MenuResponse
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != contract.SchemaVersion || len(m.Categories) != len(policy.Menu) {
		t.Fatalf("menu: %+v", m)
	}
	for i, c := range m.Categories {
		if c.ID != policy.Menu[i].ID || c.Label != policy.Menu[i].Label || len(c.Items) != len(policy.Menu[i].Items) {
			t.Errorf("category %d: %+v", i, c)
		}
		for _, it := range c.Items {
			if it.Message == "" || it.IntentID == "" {
				t.Errorf("item without message or intent: %+v", it)
			}
		}
	}
}

func TestStructuredTurnFields(t *testing.T) {
	h := structuredHarness(t, true, pricingAnswer)
	s := h.newSession("key-structured-on")
	e := decodeEnvelope(t, h.turn(s, turnUUID, "What plans are available?"))
	if e.Outcome != contract.OutcomeGrounded {
		t.Errorf("outcome %q", e.Outcome)
	}
	if len(e.FollowUps) == 0 || len(e.FollowUps) > contract.MaxFollowUps || e.FollowUps[0].IntentID != "I06" {
		t.Errorf("followups (Doc 02 §08 pricing → trial first): %+v", e.FollowUps)
	}
	// Route-keyed claim citation gets the route's title and real URL; a KB
	// citation gets the source title.
	if c := e.Citations[0]; c.Title != "Pricing and plans" || c.RouteKey != "pricing" || c.URL != "https://www.zoikosuite.com/pricing" {
		t.Errorf("claim citation: %+v", c)
	}
	if c := e.Citations[1]; c.Title != "ZoikoSuite overview" {
		t.Errorf("kb citation: %+v", c)
	}
	// Primary CTA uses the intent's Doc 02 §06 label; a placeholder URL is never sent.
	if e.CTA == nil || e.CTA.Primary.Label != "Compare plans" || e.CTA.Primary.URL == "" {
		t.Fatalf("primary cta: %+v", e.CTA)
	}
	if e.CTA.Secondary == nil || e.CTA.Secondary.RouteKey != "trust" || e.CTA.Secondary.URL != "" {
		t.Errorf("secondary cta: %+v", e.CTA.Secondary)
	}
}

func TestStructuredRefusalHasNoFollowUps(t *testing.T) {
	refused := orchestrator.Result{Status: contract.RefusedAction, IntentID: "X01", Text: "I can't see account data.", KnowledgeReleaseID: "kb",
		Suggestions: []contract.Chip{{Label: "Get support", Message: "I need help from support"}}}
	h := structuredHarness(t, true, refused)
	s := h.newSession("key-structured-refused")
	e := decodeEnvelope(t, h.turn(s, turnUUID, "show my invoices"))
	if e.Outcome != contract.OutcomeRefused || len(e.FollowUps) != 0 || len(e.Suggestions) != 1 {
		t.Fatalf("refusal: %+v", e)
	}
}

func TestEnvelopeRejectsBadStructuredFields(t *testing.T) {
	base := contract.Envelope{SchemaVersion: "1.0", TurnSequence: 1, Status: contract.Answered, IntentID: "I05", KnowledgeReleaseID: "kb",
		PolicyVersion: "p", AnswerClass: contract.ProcessExplanation, Citations: []contract.Citation{{SourceVersionID: "s", URLKey: "k"}}}
	bad := []func(*contract.Envelope){
		func(e *contract.Envelope) { e.Outcome = contract.OutcomeRefused }, // does not match status
		func(e *contract.Envelope) { e.FollowUps = make([]contract.FollowUp, 5) },
		func(e *contract.Envelope) {
			e.FollowUps = []contract.FollowUp{{ID: "x", Label: "x", IntentID: "Z99", Message: "x"}}
		},
		func(e *contract.Envelope) {
			e.CTA = &contract.CTA{Secondary: &contract.CTALink{RouteKey: "r", Label: "l"}}
		},
		func(e *contract.Envelope) {
			e.CTA = &contract.CTA{Primary: &contract.CTALink{RouteKey: "", Label: "l"}}
		},
	}
	for i, mut := range bad {
		e := base
		mut(&e)
		if e.Validate() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	ok := base
	ok.Outcome = contract.OutcomeGrounded
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Doc 02 X05: a security report stays on its official route; no follow-ups.
func TestNoFollowUpsAfterSafetyReply(t *testing.T) {
	report := orchestrator.Result{Status: contract.Handoff, IntentID: "X05", Text: "Please use the official security reporting channel.", KnowledgeReleaseID: "kb",
		Actions: []contract.Action{{Type: contract.ActionOpenRoute, RouteKey: "trust"}}}
	h := structuredHarness(t, true, report)
	s := h.newSession("key-structured-x05")
	if e := decodeEnvelope(t, h.turn(s, turnUUID, "We have a security breach right now!")); len(e.FollowUps) != 0 {
		t.Fatalf("follow-ups after X05: %+v", e.FollowUps)
	}
}
