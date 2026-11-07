package release

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/claims"
	"zoiko.io/public-ai-svc/internal/knowledge"
	"zoiko.io/public-ai-svc/internal/signing"
	"zoiko.io/public-ai-svc/internal/store"
)

var (
	ctx      = context.Background()
	fixtures = filepath.Join("..", "..", "testdata", "fixtures")
	now      = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
)

type env struct {
	st       *store.Memory
	rt       *Runtime
	pub, key string
}

func setup(t *testing.T) env {
	t.Helper()
	pub, key, err := signing.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	kb, kbSig, _, err := BuildKB(filepath.Join(fixtures, "kb"), "kb-test-1", now, key)
	if err != nil {
		t.Fatal(err)
	}
	cl, clSig, err := BuildClaims(filepath.Join(fixtures, "claims.json"), "claims-test-1", now, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		id, kind string
		m, s     []byte
	}{{"kb-test-1", "kb", kb, kbSig}, {"claims-test-1", "claims", cl, clSig}} {
		if err := Publish(ctx, st, pub, p.id, p.kind, p.m, p.s); err != nil {
			t.Fatal(err)
		}
		if err := Promote(ctx, st, p.kind, p.id, nil); err != nil {
			t.Fatal(err)
		}
	}
	rt := New(st, pub)
	if err := rt.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	return env{st, rt, pub, key}
}

// KB-001..: the ingestion pipeline stages approved sources and quarantines
// every unsafe one with the right state.
func TestIngestionStates(t *testing.T) {
	_, reports, err := knowledge.IngestDir(filepath.Join(fixtures, "kb"), "kb-x", now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"src-overview": knowledge.StateStaged, "src-integrations": knowledge.StateStaged, "src-trust": knowledge.StateStaged,
		"src-pricing": knowledge.StateStaged, "src-annual-offer": knowledge.StateStaged, "src-migration": knowledge.StateStaged,
		"src-poisoned": knowledge.StateQuarantineIndex, "src-private": knowledge.StateQuarantinePrivate,
		"src-draft": knowledge.StateHoldApproval, "src-offsite": knowledge.StateQuarantineFetch,
	}
	got := map[string]string{}
	for _, r := range reports {
		got[r.SourceID] = r.State
		t.Logf("%s %s %s", r.SourceID, r.State, r.Reason)
	}
	for id, state := range want {
		if got[id] != state {
			t.Errorf("%s: got %s want %s", id, got[id], state)
		}
	}
}

func TestRoutesRejectUnsafeURLs(t *testing.T) {
	m, _, _ := knowledge.IngestDir(filepath.Join(fixtures, "kb"), "kb-x", now)
	expectedKeys := []string{"pricing", "demo", "support", "core_modules", "trust", "platform-overview", "sign-in", "signup_or_trial"}
	keySet := map[string]bool{}
	for _, r := range m.Routes {
		keySet[r.RouteKey] = true
		if !strings.HasPrefix(r.URL, "https://example.invalid/") {
			t.Fatalf("unsafe route accepted: %+v", r)
		}
	}
	for _, k := range expectedKeys {
		if !keySet[k] {
			t.Fatalf("expected route key %q not present", k)
		}
	}
	// ensure that no route with a javascript: URL slipped through
	for _, r := range m.Routes {
		if strings.HasPrefix(r.URL, "javascript:") {
			t.Fatalf("unsafe javascript route was not rejected: %+v", r)
		}
	}
}

func search(t *testing.T, e env, q string, market string) knowledge.Result {
	t.Helper()
	snap, err := e.rt.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap.KB.Search(knowledge.Query{Text: q, Market: market, Locale: "en-US", Now: now}, snap)
}

func TestRetrievalFindsApprovedEvidence(t *testing.T) {
	e := setup(t)
	r := search(t, e, "Which finance modules does it include, accounts payable?", "")
	if r.Answerability != knowledge.Answerable || r.Hits[0].Chunk.SourceID != "src-overview" {
		t.Fatalf("got %+v", r)
	}
}

// The private and poisoned sources never become retrievable (QA-T022/T036).
func TestPrivateAndPoisonedNeverRetrieved(t *testing.T) {
	e := setup(t)
	for _, q := range []string{"payroll run checklist staff", "release notes plans free forever"} {
		r := search(t, e, q, "")
		for _, h := range r.Hits {
			if h.Chunk.SourceID == "src-private" || h.Chunk.SourceID == "src-poisoned" {
				t.Fatalf("%q retrieved forbidden source %s", q, h.Chunk.SourceID)
			}
		}
	}
}

// Expired evidence is not answerable and is reported as stale.
func TestExpiredSourceIsStale(t *testing.T) {
	e := setup(t)
	r := search(t, e, "annual billing discount spring campaign promotion", "")
	if r.Answerability != knowledge.Stale {
		t.Fatalf("got %s %+v", r.Answerability, r.Hits)
	}
}

// Revocation denies retrieval immediately, and rolling the release pointer
// back does not resurrect a revoked source (Doc 08 RB-02, OPR-005).
func TestRevocationSurvivesRollback(t *testing.T) {
	e := setup(t)
	q := "data migration spreadsheets import templates"
	if r := search(t, e, q, ""); r.Answerability != knowledge.Answerable {
		t.Fatalf("before revoke: %s", r.Answerability)
	}
	if _, err := Revoke(ctx, e.st, "source", "src-migration", "TEST_REVOKE", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if r := search(t, e, q, ""); r.Answerability != knowledge.Revoked {
		t.Fatalf("after revoke: %s", r.Answerability)
	}
	// Publish a second release and roll back to the first: still revoked.
	kb2, sig2, _, _ := BuildKB(filepath.Join(fixtures, "kb"), "kb-test-2", now, e.key)
	if err := Publish(ctx, e.st, e.pub, "kb-test-2", "kb", kb2, sig2); err != nil {
		t.Fatal(err)
	}
	_ = Promote(ctx, e.st, "kb", "kb-test-2", nil)
	_ = e.rt.Refresh(ctx)
	_ = Promote(ctx, e.st, "kb", "kb-test-1", nil)
	_ = e.rt.Refresh(ctx)
	if r := search(t, e, q, ""); r.Answerability != knowledge.Revoked {
		t.Fatalf("after rollback: %s", r.Answerability)
	}
	snap, _ := e.rt.Snapshot()
	if snap.KB.ReleaseID() != "kb-test-1" {
		t.Fatalf("rollback did not move pointer: %s", snap.KB.ReleaseID())
	}
}

func TestRevokedSinceAtCommit(t *testing.T) {
	e := setup(t)
	snap, _ := e.rt.Snapshot()
	if hit, _ := e.rt.RevokedSince(ctx, snap.Watermark, []string{"src-pricing"}, []string{"c-price-us-growth-m"}); hit {
		t.Fatal("nothing revoked yet")
	}
	_, _ = Revoke(ctx, e.st, "claim", "c-price-us-growth-m", "TEST", nil)
	if hit, _ := e.rt.RevokedSince(ctx, snap.Watermark, nil, []string{"c-price-us-growth-m"}); !hit {
		t.Fatal("commit-time check must see the new revocation")
	}
}

// A tampered or wrongly-signed release is never published or loaded.
func TestSignatureEnforced(t *testing.T) {
	e := setup(t)
	kb, sig, _, _ := BuildKB(filepath.Join(fixtures, "kb"), "kb-test-3", now, e.key)
	tampered := []byte(strings.Replace(string(kb), "general ledger", "free forever", 1))
	if err := Publish(ctx, e.st, e.pub, "kb-test-3", "kb", tampered, sig); err == nil {
		t.Fatal("tampered manifest must be rejected")
	}
	otherPub, _, _ := signing.GenerateKey()
	if err := Publish(ctx, e.st, otherPub, "kb-test-3", "kb", kb, sig); err == nil {
		t.Fatal("signature from another key must be rejected")
	}
	// Even if a bad release is forced into the store, the runtime fails closed.
	_ = e.st.PutRelease(ctx, store.Release{ReleaseID: "kb-bad", Kind: "kb", Manifest: tampered, Signature: sig, CreatedAt: now})
	_ = e.st.SetPointer(ctx, "kb", "kb-bad", nil)
	if err := e.rt.Refresh(ctx); err == nil {
		t.Fatal("refresh must fail on an invalid signature")
	}
	if e.rt.Ready(time.Minute) {
		t.Fatal("runtime must not be ready with an invalid release")
	}
	if _, err := e.rt.Snapshot(); err == nil {
		t.Fatal("no snapshot may be served from an invalid release")
	}
}

func resolve(t *testing.T, e env, q claims.Request) claims.Decision {
	t.Helper()
	snap, _ := e.rt.Snapshot()
	q.AsOf = now
	return snap.Claims.Resolve(q, snap)
}

func TestClaimDecisions(t *testing.T) {
	e := setup(t)
	cases := []struct {
		name string
		q    claims.Request
		want string
	}{
		{"verified price", claims.Request{Subject: claims.Price, SubjectID: "TEST_GROWTH", Market: "US", BillingTerm: "monthly"}, claims.Verified},
		{"contradicting IE prices", claims.Request{Subject: claims.Price, SubjectID: "TEST_GROWTH", Market: "IE"}, claims.Contradicted},
		{"consistent GB price", claims.Request{Subject: claims.Price, SubjectID: "TEST_GROWTH", Market: "GB"}, claims.Verified},
		{"expired annual price", claims.Request{Subject: claims.Price, SubjectID: "TEST_GROWTH", Market: "US", BillingTerm: "annual"}, claims.ExpiredDecision},
		{"no market", claims.Request{Subject: claims.Price, SubjectID: "TEST_GROWTH"}, claims.MarketRequired},
		{"unpublished market", claims.Request{Subject: claims.Price, SubjectID: "TEST_GROWTH", Market: "DE"}, claims.InsufficientEvidence},
		{"SOC 2 readiness is never certification", claims.Request{Subject: claims.Certification, SubjectID: "SOC2"}, claims.InsufficientEvidence},
		{"ISO certified", claims.Request{Subject: claims.Certification, SubjectID: "ISO27001"}, claims.Verified},
		{"trial US", claims.Request{Subject: claims.Trial, Market: "US"}, claims.Verified},
		{"trial DE", claims.Request{Subject: claims.Trial, Market: "DE"}, claims.InsufficientEvidence},
		{"integration", claims.Request{Subject: claims.IntegrationStatus, SubjectID: "fixturebooks"}, claims.Verified},
	}
	for _, c := range cases {
		if got := resolve(t, e, c.q).Decision; got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
	// A revoked claim is blocked.
	_, _ = Revoke(ctx, e.st, "claim", "c-cert-iso27001", "TEST", nil)
	_ = e.rt.Refresh(ctx)
	if got := resolve(t, e, claims.Request{Subject: claims.Certification, SubjectID: "ISO27001"}).Decision; got != claims.RevokedDecision {
		t.Fatalf("revoked claim: %s", got)
	}
}

func TestVerifiedClaimCarriesStatementAndProvenance(t *testing.T) {
	e := setup(t)
	d := resolve(t, e, claims.Request{Subject: claims.Price, SubjectID: "TEST_GROWTH", Market: "US", BillingTerm: "monthly"})
	if len(d.Verified) != 1 || !strings.Contains(d.Verified[0].PublicStatement, "XTS 49.00") || d.Verified[0].CitationURLKey == "" {
		t.Fatalf("got %+v", d)
	}
}

// signup_or_trial is only offered where a trial claim is VERIFIED.
func TestTrialRouteGatedByClaim(t *testing.T) {
	e := setup(t)
	snap, _ := e.rt.Snapshot()
	if _, ok := snap.Route("signup_or_trial", "US", now); !ok {
		t.Fatal("US trial route should be offered")
	}
	if _, ok := snap.Route("signup_or_trial", "DE", now); ok {
		t.Fatal("DE has no verified trial: route must be hidden")
	}
	if _, ok := snap.Route("evil", "US", now); ok {
		t.Fatal("javascript: route must never exist")
	}
	cfg := e.rt.PublicConfig()
	for _, l := range cfg.CitationLinks {
		// Fixture sources live on a placeholder host: the title is kept, the URL is never exposed.
		if l.URL != "" || l.URLKey == "" || l.Title == "" {
			t.Fatalf("citation link exposes a placeholder URL or lacks a title: %+v", l)
		}
		if l.URLKey == "internal-payroll" || l.URLKey == "release-notes" {
			t.Fatalf("quarantined source exposed as a citation link: %+v", l)
		}
	}
	if len(cfg.CitationLinks) == 0 {
		t.Fatal("citation links missing")
	}
	if cfg.ConfigRevision == "" || len(cfg.MarketChoices) != 3 {
		t.Fatalf("config: %+v", cfg)
	}
}

func TestClaimsValidationRequiresTwoApprovals(t *testing.T) {
	m := claims.Manifest{Claims: []claims.Claim{{ClaimID: "c1", ClaimType: claims.Price, Status: claims.StatusActive, ApprovalRefs: []string{"one"}, ValidFrom: now, ValidUntil: now.Add(time.Hour)}}}
	if len(m.Validate()) == 0 {
		t.Fatal("single-approval ACTIVE claim must be rejected")
	}
}

// OPR-004: a release evaluated with another policy version is refused at
// load, and outside dev/test a release must declare its range.
func TestIncompatibleReleaseRefusedAtLoad(t *testing.T) {
	withRange := func(t *testing.T, e env, id string, versions []string) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(fixtures, "claims.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		m["compatible_policy_versions"] = versions
		path := filepath.Join(t.TempDir(), "claims.json")
		b, _ := json.Marshal(m)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		cl, sig, err := BuildClaims(path, id, now, e.key)
		if err != nil {
			t.Fatal(err)
		}
		if err := Publish(ctx, e.st, e.pub, id, "claims", cl, sig); err != nil {
			t.Fatal(err)
		}
		if err := Promote(ctx, e.st, "claims", id, nil); err != nil {
			t.Fatal(err)
		}
	}

	e := setup(t)
	e.rt.PolicyVersion = "pub-policy-1.0"
	withRange(t, e, "claims-compat-ok", []string{"pub-policy-0.9", "pub-policy-1.0"})
	if err := e.rt.Refresh(ctx); err != nil {
		t.Fatalf("compatible release refused: %v", err)
	}
	withRange(t, e, "claims-compat-bad", []string{"pub-policy-2.0"})
	if err := e.rt.Refresh(ctx); !errors.Is(err, ErrIncompatibleRelease) {
		t.Fatalf("incompatible release: got %v", err)
	}
	if _, err := e.rt.Snapshot(); err == nil {
		t.Fatal("no snapshot may be served from an incompatible release")
	}

	strict := setup(t) // the fixture releases declare no range
	strict.rt = New(strict.st, strict.pub)
	strict.rt.PolicyVersion, strict.rt.RequireCompatibility = "pub-policy-1.0", true
	if err := strict.rt.Refresh(ctx); !errors.Is(err, ErrIncompatibleRelease) {
		t.Fatalf("release without a range outside dev/test: got %v", err)
	}
}

// Doc 03 §11: a KB release names its product, audience and signing time.
func TestKBReleaseIdentityFields(t *testing.T) {
	_, key, _ := signing.GenerateKey()
	body, _, _, err := BuildKB(filepath.Join(fixtures, "kb"), "kb-identity-1", now, key)
	if err != nil {
		t.Fatal(err)
	}
	var m knowledge.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.ProductID != "zoikosuite" || m.Audience != knowledge.Audience || !m.SignedAt.Equal(now) {
		t.Fatalf("identity: product=%q audience=%q signed=%v", m.ProductID, m.Audience, m.SignedAt)
	}
}
