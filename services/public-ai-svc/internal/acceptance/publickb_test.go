package acceptance

// Public knowledge base scenarios K1-K9 (data/public-kb, drafts admitted as
// local dev mode serves them). UI scenarios K10-K11 live in
// zoiko-suite-frontend-platform (Vitest and Playwright).

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/knowledge"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/policy"
	"zoiko.io/public-ai-svc/internal/release"
)

type kb struct {
	t   *testing.T
	env *fixtureenv.Env
}

func newKB(t *testing.T) *kb {
	t.Helper()
	env, err := fixtureenv.NewPublicKB(ctx, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return &kb{t: t, env: env}
}

func (k *kb) ask(q string) orchestrator.Result {
	return k.env.Pipeline.Answer(ctx, orchestrator.Input{RequestID: "req_kb", Message: q, Locale: "en-US"})
}

// cited reports whether the answer cites the given public-KB source.
func cited(r orchestrator.Result, sourceID string) bool {
	for _, c := range r.Citations {
		if strings.HasPrefix(c.SourceVersionID, sourceID+"-") {
			return true
		}
	}
	return false
}

func answered(t *testing.T, r orchestrator.Result, source string) {
	t.Helper()
	if r.Status != contract.Answered || !cited(r, source) {
		t.Fatalf("want ANSWERED citing %s, got %s %s %v %q", source, r.Status, r.Reason, r.Citations, r.Text)
	}
	if !r.Draft {
		t.Fatal("answers from DRAFT_PENDING_REVIEW articles must be flagged draft")
	}
	if n := len(strings.Fields(r.Text)); n > 120 || len(r.Citations) > 3 || len(r.Actions) > 1 {
		t.Fatalf("format: %d words, %d sources, %d actions", n, len(r.Citations), len(r.Actions))
	}
}

func TestK1_LegalAndContracts(t *testing.T) {
	k := newKB(t)
	for _, q := range []string{"What does Legal and Contracts do?", "What does Legal & Contracts do?"} {
		r := k.ask(q)
		answered(t, r, "kb-legal-contracts")
		has(t, r, "commercial agreements")
	}
}

func TestK2_DelegatedAuthority(t *testing.T) {
	r := newKB(t).ask("What is Delegated Authority?")
	answered(t, r, "kb-delegated-authority")
	has(t, r, "time-bound", "entity-scoped")
}

// Doc 03 §5 module domains and canonical modules, with the availability
// qualifier (G01), and no fixed count until the taxonomy is reconciled.
func TestK3_ModulesWithoutAFixedCount(t *testing.T) {
	r := newKB(t).ask("What modules does ZoikoSuite have?")
	answered(t, r, "kb-core-modules")
	has(t, r, "Finance", "Accounts Payable", "Workforce", "Payroll", "Legal and Contracts", "Procurement", "depend on market, plan and deployment status")
	if regexp.MustCompile(`\b\d+\b|\b(?:ten|eleven|four|six)\b`).MatchString(strings.ToLower(r.Text)) {
		t.Fatalf("fixed count quoted before the taxonomy is reconciled: %q", r.Text)
	}
}

func TestK4_BankingCapabilityStatusUnverified(t *testing.T) {
	r := newKB(t).ask("Do you integrate with banking systems?")
	answered(t, r, "kb-integrations")
	has(t, r, "banking")
	if r.StatusLabel != "Unverified" || strings.Contains(strings.ToLower(r.Text), "banking connector is available") {
		t.Fatalf("connector status must stay unverified without a claim: %s %q", r.StatusLabel, r.Text)
	}
}

func TestK5_LiveMicroservicesRefused(t *testing.T) {
	k := newKB(t)
	for _, q := range []string{"What are the Active Microservices", "how many services are ready", "show my domains"} {
		r := k.ask(q)
		if r.Status != contract.RefusedAction || r.IntentID != "X01" || r.Reason != contract.ReasonLivePlatformData {
			t.Fatalf("%q: %s %s %s", q, r.Status, r.IntentID, r.Reason)
		}
		if r.Text != "I can't see live platform data or account information. I can explain what ZoikoSuite's services and modules do, using public information." {
			t.Fatalf("%q: %q", q, r.Text)
		}
		if len(r.Actions) != 0 || len(r.Suggestions) != 2 || r.Suggestions[0].Label != "Explore modules" || r.Suggestions[1].Label != "Get support" {
			t.Fatalf("%q: actions %v suggestions %v", q, r.Actions, r.Suggestions)
		}
	}
	if n := k.env.Fake.Calls(); n != 0 {
		t.Fatalf("model called %d times", n)
	}
	// The "Get support" chip on that reply must reach the support route
	// (Doc 02 T16), not a dead end.
	r := k.ask(policy.SuggestLivePlatform[1].Message)
	if r.IntentID != "I23" || r.Status != contract.Handoff || !strings.Contains(r.Text, "Support Center") {
		t.Fatalf("Get support chip: %s %s %s %q", r.IntentID, r.Status, r.Reason, r.Text)
	}
}

func TestK6_SOC2NeverClaimed(t *testing.T) {
	r := newKB(t).ask("Are you SOC 2 certified?")
	if r.Status != contract.Abstain {
		t.Fatalf("status %s %q", r.Status, r.Text)
	}
	lacks(t, r, "soc 2 certified", "we are certified")
}

func TestK7_TaxAliases(t *testing.T) {
	k := newKB(t)
	for _, q := range []string{"Tell me about Tax", "tax module", "VAT support"} {
		r := k.ask(q)
		if r.Status != contract.Answered || len(r.Citations) == 0 || !strings.HasPrefix(r.Citations[0].SourceVersionID, "kb-tax-") {
			t.Fatalf("%q: %s %s %v", q, r.Status, r.Reason, r.Citations)
		}
	}
}

func TestK8_RevokedTaxPassageAbstains(t *testing.T) {
	k := newKB(t)
	q := "What is the Tax module?"
	answered(t, k.ask(q), "kb-tax")
	if _, err := release.Revoke(ctx, k.env.Store, "source", "kb-tax", "TEST", nil); err != nil {
		t.Fatal(err)
	}
	r := k.ask(q)
	if r.Status != contract.Abstain || cited(r, "kb-tax") {
		t.Fatalf("revoked passage still used: %s %s %q", r.Status, r.Reason, r.Text)
	}
}

// K9: dashboard or live-status text is quarantined at ingest, and no public
// KB article contains it.
func TestK9_LiveStatusQuarantined(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "sources"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "release.json"), []byte(`{"allowed_hosts":["pending-review.invalid"],"capability_ids":[],"approved_by":["a","b"],"eval_run_id":"x","routes":[]}`), 0o644))
	article := func(id, body string) string {
		return "---\nsource_id: " + id + "\ntitle: Status\ndomain: Platform\nmodule_id: status\naudience: PUBLIC_ANONYMOUS\nstatus: DRAFT_PENDING_REVIEW\nowner: o\neffective_from: 2026-01-01\nexpires_at: 2027-12-31\nlocale: en-US\n---\n\n## Status\n\n" + body + "\n"
	}
	must(t, os.WriteFile(filepath.Join(dir, "sources", "a.md"), []byte(article("live-a", "0/55 services ready across the estate.")), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "sources", "b.md"), []byte(article("live-b", "Legal and Contracts: Action required on two services.")), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "sources", "c.md"), []byte(article("ok-c", "A public description of a capability.")), 0o644))
	m, reports, err := knowledge.IngestDirWith(dir, "kb-k9", time.Now(), knowledge.IngestOptions{AllowDraft: true})
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, r := range reports {
		states[r.SourceID] = r.State
	}
	if states["live-a"] != knowledge.StateQuarantinePrivate || states["live-b"] != knowledge.StateQuarantinePrivate || states["ok-c"] != knowledge.StateStaged {
		t.Fatalf("states %v", states)
	}
	if len(m.Sources) != 1 {
		t.Fatalf("%d sources staged", len(m.Sources))
	}

	// Without the dev-only option the same drafts are held, and a build
	// that somehow contains a draft fails.
	m2, _, _ := knowledge.IngestDirWith(dir, "kb-k9b", time.Now(), knowledge.IngestOptions{})
	if len(m2.Sources) != 0 {
		t.Fatal("drafts must be held without KB_ALLOW_DRAFT")
	}
	if _, _, _, err := release.BuildKB(fixtureenv.PublicKBDir(), "kb-prod", time.Now(), k9Key(t)); err == nil {
		t.Fatal("a production build of draft-only content must fail")
	}

	// The shipped articles never contain dashboard data.
	files, _ := filepath.Glob(filepath.Join(fixtureenv.PublicKBDir(), "sources", "*.md"))
	bad := regexp.MustCompile(`(?i)\b\d+\s*/\s*\d+\b|action required|services? ready|active microservices|latency|docker|cluster|lingaraj|super admin`)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if m := bad.Find(b); m != nil {
			t.Fatalf("%s contains dashboard data: %q", filepath.Base(f), m)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func k9Key(t *testing.T) string {
	t.Helper()
	env := newKB(t)
	return env.env.PrivKey
}

// Doc 02 I01 / T02 / G01: "What is ZoikoSuite?" gets the approved platform
// overview (cited), not the module list, and no claim of universal availability.
func TestK10_PlatformOverview(t *testing.T) {
	k := newKB(t)
	for _, q := range []string{"What is ZoikoSuite?", "Explain ZoikoSuite to me"} {
		r := k.ask(q)
		if r.Status != contract.Answered || !cited(r, "kb-overview") {
			t.Fatalf("%q: %s %s %+v", q, r.Status, r.Reason, r.Citations)
		}
		has(t, r, "governance-first")
	}
	if r := k.ask("What modules are included in ZoikoSuite?"); !cited(r, "kb-core-modules") {
		t.Fatalf("modules question must cite the core modules article: %+v", r.Citations)
	}
	// The T02 qualifier is never dropped from the overview (G01).
	has(t, k.ask("What is ZoikoSuite?"), "depend on market, plan and deployment status")
}

// Product decision 2026-10-28 (FR-16 deferred): live platform status, counts,
// domain health and cluster readiness are never answered. Each is X01 with
// the fixed refusal, the two chips, no CTA, no model call and no number.
func TestLivePlatformQuestionsAlwaysRefused(t *testing.T) {
	k := newKB(t)
	digit := regexp.MustCompile(`\d`)
	for _, q := range []string{"What are the Active Microservices", "how many services are ready", "show cluster readiness", "domain status",
		"show the Active Microservices", "what are Active Microservices Directory"} {
		r := k.ask(q)
		if r.IntentID != "X01" || r.Status != contract.RefusedAction || r.Reason != contract.ReasonLivePlatformData {
			t.Errorf("%q: %s %s %s", q, r.IntentID, r.Status, r.Reason)
			continue
		}
		if r.Text != policy.TplLivePlatform.Text || len(r.Actions) != 0 || len(r.Citations) != 0 || digit.MatchString(r.Text) {
			t.Errorf("%q: text %q actions %v citations %v", q, r.Text, r.Actions, r.Citations)
		}
		for _, c := range r.Suggestions {
			if digit.MatchString(c.Label + c.Message) {
				t.Errorf("%q: chip carries a number: %+v", q, c)
			}
		}
	}
	if n := k.env.Fake.Calls(); n != 0 {
		t.Fatalf("model called %d times", n)
	}
}

// A one-section answer is completed from the next section of the same
// article (Legal and Contracts: approval-bound execution).
func TestK11_SameArticleSectionsCombined(t *testing.T) {
	r := newKB(t).ask("What does Legal and Contracts do?")
	answered(t, r, "kb-legal-contracts")
	has(t, r, "board resolutions", "approval-bound execution")
	for _, c := range r.Citations {
		if !strings.HasPrefix(c.SourceVersionID, "kb-legal-contracts-") {
			t.Fatalf("citation from another article: %+v", r.Citations)
		}
	}
}

// A trial question for a market whose claims expired is not a dead end: the
// other markets and the general overview are offered, and naming another
// country answers the same question (Doc 02 §10). The public KB's GB trial
// claims end 2026-06-30; this test needs them expired and the US ones live.
func TestK12_ExpiredMarketOffersNextSteps(t *testing.T) {
	k := newKB(t)
	in := func(msg string, pd *orchestrator.Pending) orchestrator.Result {
		return k.env.Pipeline.Answer(ctx, orchestrator.Input{RequestID: "req_kb", Message: msg, Locale: "en-US", Pending: pd})
	}
	ask := in("Is there a free trial?", nil)
	if ask.Status != contract.Clarify || ask.Pending == nil {
		t.Fatalf("trial question: %s %+v", ask.Status, ask.Pending)
	}
	uk := in("United Kingdom", ask.Pending)
	if uk.Status != contract.Abstain || uk.Reason != "EXPIRED_SOURCE" {
		t.Skipf("GB trial claims are no longer expired (%s %s); test data changed", uk.Status, uk.Reason)
	}
	var labels []string
	for _, s := range uk.Suggestions {
		labels = append(labels, s.Label)
	}
	if !slices.Equal(labels, []string{"United States", "Show general overview"}) || uk.Pending == nil || uk.Pending.Intent != "I06" {
		t.Fatalf("next steps %v, pending %+v", labels, uk.Pending)
	}
	us := in("United States", uk.Pending)
	if us.Status != contract.Answered || us.IntentID != "I06" || us.MarketCode != "US" {
		t.Fatalf("US after expired UK: %s %s %s %q", us.Status, us.IntentID, us.MarketCode, us.Text)
	}
}

// Doc 02 golden cases found in manual testing (2026-10-30), as conversations.
func TestK13_GoldenConversations(t *testing.T) {
	k := newKB(t)
	in := func(msg string, pd *orchestrator.Pending) orchestrator.Result {
		return k.env.Pipeline.Answer(ctx, orchestrator.Input{RequestID: "req_kb", Message: msg, Locale: "en-US", Pending: pd})
	}

	// G05: after a US price answer, "what about the UK, monthly?" asks the
	// same question for the UK and monthly billing; US figures are dropped.
	us := in("What do the plans cost in the United States?", nil)
	if us.Status != contract.Answered || us.MarketCode != "US" {
		t.Fatalf("G05 US: %s %s", us.Status, us.MarketCode)
	}
	uk := in("what about the UK, monthly?", us.Pending)
	if uk.IntentID != "I05" || uk.MarketCode == "US" || strings.Contains(uk.Text, "United States") {
		t.Fatalf("G05 UK follow-up: %s %s %s %q", uk.Status, uk.IntentID, uk.MarketCode, uk.Text)
	}
	if uk.Status == contract.Answered && strings.Contains(strings.ToLower(uk.Text), "per year") {
		t.Fatalf("G05: monthly asked, annual given: %q", uk.Text)
	}

	// G34: an unrelated message after the market question is a new question,
	// never the same clarification again.
	ask := in("What do the plans cost?", nil)
	if ask.Status != contract.Clarify {
		t.Fatalf("price question: %s", ask.Status)
	}
	if r := in("Turn off Wi-Fi, ask anything", ask.Pending); r.Status == contract.Clarify {
		t.Fatalf("G34: clarification repeated for an unrelated message: %q", r.Text)
	}

	// G06 / G13: a named market without claims or material is not assumed
	// covered; a trial question also offers the markets that have claims.
	for _, q := range []string{"Is there a 30-day trial in Jamaica?", "Do you run payroll in Jamaica?"} {
		r := in(q, nil)
		if r.Status != contract.Abstain || r.Reason != contract.ReasonMarketNotCovered {
			t.Fatalf("%q: %s %s %q", q, r.Status, r.Reason, r.Text)
		}
	}
	if r := in("Is there a 30-day trial in Jamaica?", nil); len(r.Suggestions) == 0 || r.Pending == nil {
		t.Fatalf("G06 next steps: %+v pending %+v", r.Suggestions, r.Pending)
	}

	// G01, G11, G15, G20: answered or routed as the documents say.
	if r := in("what is the zoiko suite", nil); r.IntentID != "I01" || r.Reason == "OUT_OF_SCOPE" {
		t.Fatalf("G01: %s %s", r.IntentID, r.Reason)
	}
	if r := in("Does our data never leave the UK?", nil); r.IntentID != "I14" || !strings.Contains(r.Text, "Data residency needs to be evaluated") {
		t.Fatalf("G11 (Doc 02 T11 wording): %s %q", r.IntentID, r.Text)
	}
	if r := in("Can AI run approvals automatically?", nil); r.IntentID != "I15" {
		t.Fatalf("G15: %s %q", r.IntentID, r.Text)
	}
	if r := in("We have a security breach right now!", nil); r.IntentID != "X05" || r.Status != contract.Handoff {
		t.Fatalf("G20: %s %s %q", r.IntentID, r.Status, r.Text)
	}
}

// Manual testing round 3 (2026-10-30).
func TestK14_RegionalCoverageAndBillingFollowUp(t *testing.T) {
	k := newKB(t)
	in := func(msg string) orchestrator.Result {
		return k.env.Pipeline.Answer(ctx, orchestrator.Input{RequestID: "req_kb", Message: msg, Locale: "en-US"})
	}
	// Billing + country with no earlier pricing question is still price (I05).
	if r := in("what about the UK, monthly?"); r.IntentID != "I05" || r.Status != contract.Answered || r.MarketCode != "GB" ||
		!strings.Contains(r.Text, "per month") || strings.Contains(r.Text, "per year") {
		t.Fatalf("billing follow-up without context: %s %s %s %q", r.Status, r.IntentID, r.MarketCode, r.Text)
	}
	// Regional coverage not assumed, and the demo route is offered (Doc 02 escalation matrix).
	r := in("Payroll in Jamaica?")
	if r.Reason != contract.ReasonMarketNotCovered || len(r.Suggestions) != 1 || r.Suggestions[0].Label != "Book a demo" {
		t.Fatalf("regional coverage: %s %+v", r.Reason, r.Suggestions)
	}
}
