package acceptance

// Public knowledge base scenarios K1-K9 (data/public-kb, drafts admitted as
// local dev mode serves them). UI scenarios K10-K11 live in
// zoiko-suite-frontend-platform (Vitest and Playwright).

import (
	"os"
	"path/filepath"
	"regexp"
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
