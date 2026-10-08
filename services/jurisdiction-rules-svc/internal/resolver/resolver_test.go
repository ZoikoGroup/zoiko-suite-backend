package resolver

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

func ptr[T any](v T) *T { return &v }

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// fakeSource is an in-memory registry with real, signed artifacts.
type fakeSource struct {
	mu       sync.Mutex
	packs    map[string]domain.EligiblePack // by pack_version_id
	loaded   map[string]*domain.LoadedPack
	listCall int
	loadCall int
}

func (f *fakeSource) ListEligiblePacks(_ context.Context, statuses []string, _, _ string) ([]domain.EligiblePack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCall++
	var out []domain.EligiblePack
	for _, p := range f.packs {
		for _, s := range statuses {
			if p.Status == s {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

func (f *fakeSource) FindPackVersion(_ context.Context, ref, version string) (*domain.EligiblePack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.packs {
		if p.PackRef == ref && p.Version == version {
			c := p
			return &c, nil
		}
	}
	return nil, ErrPackNotFound
}

func (f *fakeSource) LoadPack(_ context.Context, id string) (*domain.LoadedPack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadCall++
	lp, ok := f.loaded[id]
	if !ok {
		return nil, ErrPackNotFound
	}
	c := *lp
	return &c, nil
}

type world struct {
	src    *fakeSource
	pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
	key    *domain.TrustedKey
	signer *domain.Ed25519Signer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	s, err := domain.NewEd25519Signer("k1", priv)
	require.NoError(t, err)
	return &world{
		src: &fakeSource{packs: map[string]domain.EligiblePack{}, loaded: map[string]*domain.LoadedPack{}},
		pub: pub, priv: priv, signer: s,
		key: &domain.TrustedKey{KeyRef: "k1", Algorithm: domain.AlgorithmEd25519, PublicKey: pub, Status: domain.KeyActive},
	}
}

type ruleSpec struct {
	id         string
	code       string
	from       time.Time
	to         *time.Time
	precedence *int
	supersedes *string
}

// addPack compiles a real artifact, signs it and certifies it, then registers it.
func (w *world) addPack(t *testing.T, ref, version, status string, from time.Time, to *time.Time, rules ...ruleSpec) string {
	t.Helper()
	id := ref + "@" + version
	in := domain.CompileInput{
		PackRef: ref, Version: version, ManifestDigest: "sha256:" + strings.Repeat("a", 64),
		Manifest: domain.PackManifest{PackID: ref, PackVersion: version, JurisdictionIDs: []string{"GB", "GB-ENG"}, Regimes: []string{"VAT"},
			EffectiveFrom: from.Format("2006-01-02")},
		Jurisdictions: []domain.Named{{ID: "j-gb", Code: "GB"}, {ID: "j-eng", Code: "GB-ENG", ParentID: ptr("j-gb")}},
		Regimes:       []domain.Named{{ID: "r-vat", Code: "VAT"}},
		Sources: []domain.CompileSource{{SourceID: "s-1", JurisdictionID: "j-gb", Authority: "HMRC", SourceType: "STATUTE", AuthorityLevel: "BINDING_LAW",
			Title: "VAT Act", Location: "https://x", SnapshotHash: "sha256:" + strings.Repeat("b", 64), Language: "en", Reviewed: true}},
		Interpretations: []domain.CompileInterpretation{{InterpretationID: "i-1", JurisdictionID: "j-gb", Subject: "s", Decision: "d", Rationale: "r", Approved: true, SourceIDs: []string{"s-1"}}},
	}
	if to != nil {
		in.Manifest.EffectiveTo = ptr(to.Format("2006-01-02"))
	}
	for _, r := range rules {
		in.Rules = append(in.Rules, domain.CompileRule{RuleID: r.id, JurisdictionID: "j-gb", RuleDomain: "TAX", RuleCode: r.code, RuleName: "Rule " + r.id,
			EffectiveFrom: r.from, EffectiveTo: r.to, Payload: json.RawMessage(`{"rate":"0.2000"}`), RuleStatus: "ACTIVE", RegimeID: ptr("r-vat"),
			InterpretationID: ptr("i-1"), SourceIDs: []string{"s-1"}, Precedence: r.precedence, SupersedesRuleID: r.supersedes})
	}
	out := domain.Compile(in)
	require.False(t, out.Report.HasErrors(), "%+v", out.Report.Findings)

	sig, _ := w.signer.Sign(domain.SigningMessage(out.ArtifactDigest))
	b64 := base64.StdEncoding.EncodeToString(sig)

	report := `{"artifact_digest":"` + out.ArtifactDigest + `","certified_by":"certifier"}`
	canon, _ := domain.CanonicalJSON([]byte(report))
	rd, _ := domain.DigestOf(canon)
	csig, _ := w.signer.Sign(domain.CertificationSigningMessage(rd))

	w.src.packs[id] = domain.EligiblePack{PackVersionID: id, PackRef: ref, Version: version, Status: status, EffectiveFrom: from, EffectiveTo: to,
		ScopeCodes: []string{"GB", "GB-ENG"}}
	w.src.loaded[id] = &domain.LoadedPack{PackVersionID: id, ArtifactJSON: string(out.ArtifactJSON), ArtifactDigest: out.ArtifactDigest,
		VersionDigest: out.ArtifactDigest, Signature: &b64, KeyRef: ptr("k1"), Key: w.key,
		Cert: &domain.LoadedCertification{CertificationID: "cert-" + id, ArtifactDigest: out.ArtifactDigest, ReportJSON: string(canon),
			ReportDigest: rd, Signature: base64.StdEncoding.EncodeToString(csig), KeyRef: "k1", Key: w.key}}
	return id
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func (w *world) resolver(t *testing.T, ttl time.Duration, c *clock, onBad func(string, string, []string)) *Resolver {
	t.Helper()
	cfg := Config{EligibleStatuses: []string{"RELEASED"}, CacheTTL: ttl, OnUnverified: onBad}
	if c != nil {
		cfg.Now = c.now
	}
	r, err := New(w.src, cfg)
	require.NoError(t, err)
	return r
}

func req(at time.Time) Request {
	return Request{Jurisdiction: "GB", RuleDomain: "TAX", RuleCode: "STD", At: at}
}

func TestResolve_ReturnsAnExplainableDecisionFromAVerifiedArtifact(t *testing.T) {
	w := newWorld(t)
	w.addPack(t, "jur.gb.tax.core", "2026.08.1", "RELEASED", day(2026, 1, 1), nil, ruleSpec{id: "r1", code: "STD", from: day(2026, 1, 1)})
	d, err := w.resolver(t, time.Minute, nil, nil).Resolve(context.Background(), req(day(2026, 9, 1)))
	require.NoError(t, err)
	assert.Equal(t, domain.OutcomeResolved, d.Outcome)
	require.NotNil(t, d.Pack)
	assert.Equal(t, "jur.gb.tax.core", d.Pack.PackRef)
	assert.Equal(t, "2026.08.1", d.Pack.Version)
	assert.Equal(t, "cert-jur.gb.tax.core@2026.08.1", d.Pack.CertificationID)
	require.NotNil(t, d.Rule)
	assert.Equal(t, "r1", d.Rule.RuleID)
	assert.JSONEq(t, `{"rate":"0.2000"}`, string(d.Rule.Payload))
	require.Len(t, d.Sources, 1)
	assert.Equal(t, "HMRC", d.Sources[0].Authority)
	assert.Equal(t, "SINGLE_CANDIDATE", d.Basis)
	assert.NotEmpty(t, d.Explanation)
	eng := mustResolve(t, w, Request{Jurisdiction: "GB-ENG", RuleDomain: "TAX", RuleCode: "STD", At: day(2026, 9, 1)})
	assert.Equal(t, domain.OutcomeResolved, eng.Outcome)
	assert.Equal(t, "r1", eng.Rule.RuleID, "a subdivision inside the pack scope inherits through the artifact's own parent links")
	assert.Equal(t, "GB", eng.Rule.JurisdictionCode, "and the evidence names the jurisdiction the rule belongs to")
}

func mustResolve(t *testing.T, w *world, rq Request) *Decision {
	t.Helper()
	d, err := w.resolver(t, time.Minute, nil, nil).Resolve(context.Background(), rq)
	require.NoError(t, err)
	return d
}

func TestResolve_OnlyEligibleStatusesAreUsed(t *testing.T) {
	w := newWorld(t)
	for _, st := range []string{"DRAFT", "REVIEW", "CERTIFIED", "WITHDRAWN", "SUPERSEDED", "EMERGENCY_BLOCKED"} {
		w.addPack(t, "jur.gb.tax."+strings.ToLower(strings.ReplaceAll(st, "_", "")), "1.0", st, day(2026, 1, 1), nil, ruleSpec{id: "r-" + st, code: "STD", from: day(2026, 1, 1)})
	}
	d := mustResolve(t, w, req(day(2026, 9, 1)))
	assert.Equal(t, domain.OutcomeUnsupported, d.Outcome, "nothing but RELEASED resolves: CERTIFIED is not production eligible (s23)")
	assert.Nil(t, d.Rule)
}

func TestResolve_UnsupportedAndNotInForceAreDistinctExplicitAnswers(t *testing.T) {
	w := newWorld(t)
	w.addPack(t, "jur.gb.tax.core", "1.0", "RELEASED", day(2026, 6, 1), nil, ruleSpec{id: "r1", code: "STD", from: day(2026, 6, 1)})
	r := w.resolver(t, time.Minute, nil, nil)

	d, err := r.Resolve(context.Background(), Request{Jurisdiction: "FR", RuleDomain: "TAX", RuleCode: "STD", At: day(2026, 9, 1)})
	require.NoError(t, err)
	assert.Equal(t, domain.OutcomeUnsupported, d.Outcome, "JUR-NEG-22")

	d, err = r.Resolve(context.Background(), req(day(2026, 3, 1))) // before the pack's window
	require.NoError(t, err)
	assert.Equal(t, domain.OutcomeNoRule, d.Outcome)
	assert.Contains(t, strings.Join(d.Explanation, " "), "none is in force")

	d, err = r.Resolve(context.Background(), Request{Jurisdiction: "GB", RuleDomain: "TAX", RuleCode: "OTHER", At: day(2026, 9, 1)})
	require.NoError(t, err)
	assert.Equal(t, domain.OutcomeNoRule, d.Outcome)
}

func TestResolve_PicksTheHighestVersionInForce_AndPinnedReplayReproducesHistory(t *testing.T) {
	w := newWorld(t)
	w.addPack(t, "jur.gb.tax.core", "2026.01.1", "SUPERSEDED", day(2026, 1, 1), nil, ruleSpec{id: "old", code: "STD", from: day(2026, 1, 1)})
	w.addPack(t, "jur.gb.tax.core", "2026.06.1", "RELEASED", day(2026, 1, 1), nil, ruleSpec{id: "new", code: "STD", from: day(2026, 1, 1)})
	r := w.resolver(t, time.Minute, nil, nil)

	d, err := r.Resolve(context.Background(), req(day(2026, 9, 1)))
	require.NoError(t, err)
	assert.Equal(t, "new", d.Rule.RuleID, "new decisions use the current released version")

	// JUR-NEG-07 / s23: a past decision is replayed against the version it used.
	rq := req(day(2026, 9, 1))
	rq.PackRef, rq.PackVersion = "jur.gb.tax.core", "2026.01.1"
	d, err = r.Resolve(context.Background(), rq)
	require.NoError(t, err)
	assert.Equal(t, "old", d.Rule.RuleID)
	assert.Equal(t, "2026.01.1", d.Pack.Version)

	// A withdrawn version never resolves, even when pinned.
	w.addPack(t, "jur.gb.tax.core", "2026.03.1", "WITHDRAWN", day(2026, 1, 1), nil, ruleSpec{id: "bad", code: "STD", from: day(2026, 1, 1)})
	rq.PackVersion = "2026.03.1"
	_, err = r.Resolve(context.Background(), rq)
	assert.ErrorIs(t, err, ErrPinnedNotEligible)
	rq.PackVersion = "9.9.9"
	_, err = r.Resolve(context.Background(), rq)
	assert.ErrorIs(t, err, ErrPackNotFound)
	_, err = r.Resolve(context.Background(), Request{Jurisdiction: "GB", RuleDomain: "TAX", RuleCode: "STD", At: day(2026, 9, 1), PackVersion: "1"})
	assert.ErrorIs(t, err, ErrBadRequest)
}

func TestResolve_PackWindowsUseTheEffectiveDateNotTheLatestVersion(t *testing.T) {
	w := newWorld(t)
	to := day(2026, 7, 1)
	w.addPack(t, "jur.gb.tax.core", "1.0", "RELEASED", day(2026, 1, 1), &to, ruleSpec{id: "h1", code: "STD", from: day(2026, 1, 1)})
	w.addPack(t, "jur.gb.tax.core", "2.0", "RELEASED", to, nil, ruleSpec{id: "h2", code: "STD", from: to})
	r := w.resolver(t, time.Minute, nil, nil)
	for at, want := range map[time.Time]string{day(2026, 3, 1): "h1", to.Add(-time.Nanosecond): "h1", to: "h2", day(2027, 1, 1): "h2"} {
		d, err := r.Resolve(context.Background(), req(at))
		require.NoError(t, err)
		assert.Equal(t, want, d.Rule.RuleID, at.String())
	}
}

func TestResolve_AmbiguityIsBlockedNotGuessed(t *testing.T) {
	// Within one pack: overlap with no declared precedence. (The compiler refuses to build this, so
	// the artifact is assembled to prove the resolver also refuses at run time.)
	w := newWorld(t)
	id := w.addPack(t, "jur.gb.tax.core", "1.0", "RELEASED", day(2026, 1, 1), nil,
		ruleSpec{id: "a", code: "STD", from: day(2026, 1, 1), precedence: ptr(1)},
		ruleSpec{id: "b", code: "STD", from: day(2026, 2, 1), precedence: ptr(2)})
	lp := w.src.loaded[id]
	// Forge equal precedence in the artifact and re-sign it, as a faulty producer might.
	forged := strings.Replace(lp.ArtifactJSON, `"precedence":2`, `"precedence":1`, 1)
	require.NotEqual(t, lp.ArtifactJSON, forged)
	canon, _ := domain.CanonicalJSON([]byte(forged))
	dg, _ := domain.DigestOf(canon)
	sig, _ := w.signer.Sign(domain.SigningMessage(dg))
	b64 := base64.StdEncoding.EncodeToString(sig)
	lp.ArtifactJSON, lp.ArtifactDigest, lp.VersionDigest, lp.Signature = string(canon), dg, dg, &b64
	lp.Cert.ArtifactDigest = dg
	d := mustResolveWith(t, w, req(day(2026, 3, 1)))
	assert.Equal(t, domain.OutcomeAmbiguous, d.Outcome)
	assert.Nil(t, d.Rule)
	assert.ElementsMatch(t, []string{"a", "b"}, d.Considered)

	// Across packs: two packs both define the rule for the same jurisdiction and instant (JUR-NEG-24).
	w2 := newWorld(t)
	w2.addPack(t, "jur.gb.tax.core", "1.0", "RELEASED", day(2026, 1, 1), nil, ruleSpec{id: "p1", code: "STD", from: day(2026, 1, 1)})
	w2.addPack(t, "jur.gb.vat.extra", "1.0", "RELEASED", day(2026, 1, 1), nil, ruleSpec{id: "p2", code: "STD", from: day(2026, 1, 1)})
	d = mustResolveWith(t, w2, req(day(2026, 3, 1)))
	assert.Equal(t, domain.OutcomeAmbiguous, d.Outcome)
	assert.Len(t, d.PacksConsulted, 2)
	assert.Contains(t, strings.Join(d.Explanation, " "), "cross-pack conflict")

	// Restricting to one pack is an explicit choice, and then it resolves.
	rq := req(day(2026, 3, 1))
	rq.PackRef = "jur.gb.vat.extra"
	d = mustResolveWith(t, w2, rq)
	assert.Equal(t, "p2", d.Rule.RuleID)
}

func mustResolveWith(t *testing.T, w *world, rq Request) *Decision {
	t.Helper()
	d, err := w.resolver(t, time.Minute, nil, nil).Resolve(context.Background(), rq)
	require.NoError(t, err)
	return d
}

func TestResolve_FailsClosedWhenACoveringPackIsNotTrustworthy(t *testing.T) {
	type mut func(*world, *domain.LoadedPack)
	cases := map[string]struct {
		mutate mut
		reason string
	}{
		"tampered artifact (JUR-NEG-03)": {func(w *world, lp *domain.LoadedPack) {
			lp.ArtifactJSON = strings.Replace(lp.ArtifactJSON, "0.2000", "0.0000", 1)
		}, domain.ReasonDigestMismatch},
		"unsigned (JUR-NEG-04)": {func(w *world, lp *domain.LoadedPack) { lp.Signature, lp.KeyRef = nil, nil }, domain.ReasonUnsigned},
		"revoked key": {func(w *world, lp *domain.LoadedPack) {
			k := *lp.Key
			k.Status = domain.KeyRevoked
			lp.Key = &k
		}, domain.ReasonKeyRevoked},
		"unknown key":      {func(w *world, lp *domain.LoadedPack) { lp.Key = nil }, domain.ReasonKeyUnknown},
		"no certification": {func(w *world, lp *domain.LoadedPack) { lp.Cert = nil }, "not_certified"},
		"certification for other content": {func(w *world, lp *domain.LoadedPack) {
			lp.Cert.ArtifactDigest = "sha256:" + strings.Repeat("0", 64)
		}, "certification_artifact_mismatch"},
		"certification report tampered": {func(w *world, lp *domain.LoadedPack) {
			lp.Cert.ReportJSON = strings.Replace(lp.Cert.ReportJSON, "certifier", "someone", 1)
		}, "certification_" + domain.ReasonDigestMismatch},
		"recorded digest differs": {func(w *world, lp *domain.LoadedPack) {
			lp.VersionDigest = "sha256:" + strings.Repeat("1", 64)
		}, domain.ReasonRecordedDigestBad},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			id := w.addPack(t, "jur.gb.tax.core", "1.0", "RELEASED", day(2026, 1, 1), nil, ruleSpec{id: "r1", code: "STD", from: day(2026, 1, 1)})
			// A second, healthy pack for ANOTHER jurisdiction must not mask the failure.
			c.mutate(w, w.src.loaded[id])
			var gotRef string
			var gotReasons []string
			r := w.resolver(t, time.Minute, nil, func(ref, _ string, reasons []string) { gotRef, gotReasons = ref, reasons })
			d, err := r.Resolve(context.Background(), req(day(2026, 9, 1)))
			require.Error(t, err)
			assert.Nil(t, d, "no answer at all: not a different answer")
			var ue *UnverifiedError
			require.True(t, errors.As(err, &ue))
			require.Len(t, ue.Failures, 1)
			assert.Contains(t, ue.Failures[0].Reasons, c.reason)
			assert.Equal(t, "jur.gb.tax.core", gotRef, "a security event is raised")
			assert.Contains(t, gotReasons, c.reason)
		})
	}
}

func TestResolve_AnUnverifiedPackIsNeverSkippedInFavourOfAnOlderOne(t *testing.T) {
	w := newWorld(t)
	w.addPack(t, "jur.gb.tax.core", "1.0", "RELEASED", day(2026, 1, 1), nil, ruleSpec{id: "old", code: "STD", from: day(2026, 1, 1)})
	bad := w.addPack(t, "jur.gb.tax.core", "2.0", "RELEASED", day(2026, 1, 1), nil, ruleSpec{id: "new", code: "STD", from: day(2026, 1, 1)})
	w.src.loaded[bad].Signature, w.src.loaded[bad].KeyRef = nil, nil
	_, err := w.resolver(t, time.Minute, nil, nil).Resolve(context.Background(), req(day(2026, 9, 1)))
	var ue *UnverifiedError
	require.True(t, errors.As(err, &ue), "falling back to 1.0 would apply an outdated rate silently")
}

func TestCache_ReusesWithinTTL_AndRevocationTakesEffectAfterTTLOrInvalidate(t *testing.T) {
	w := newWorld(t)
	id := w.addPack(t, "jur.gb.tax.core", "1.0", "RELEASED", day(2026, 1, 1), nil, ruleSpec{id: "r1", code: "STD", from: day(2026, 1, 1)})
	c := &clock{t: day(2026, 9, 1)}
	r := w.resolver(t, 30*time.Second, c, nil)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		d, err := r.Resolve(ctx, req(day(2026, 9, 1)))
		require.NoError(t, err)
		require.Equal(t, domain.OutcomeResolved, d.Outcome)
	}
	assert.Equal(t, 1, w.src.listCall, "the eligible list is read once inside the TTL")
	assert.Equal(t, 1, w.src.loadCall, "the artifact is loaded and verified once inside the TTL")
	hits, misses, refreshes := r.Stats()
	assert.Equal(t, 4, hits)
	assert.Equal(t, 1, misses)
	assert.Equal(t, 1, refreshes)

	// The key is revoked in the registry. Within the TTL the cached verdict still stands...
	revoked := *w.key
	revoked.Status = domain.KeyRevoked
	w.src.loaded[id].Key = &revoked
	_, err := r.Resolve(ctx, req(day(2026, 9, 1)))
	require.NoError(t, err, "bounded staleness: this is the documented window")

	// ...an explicit invalidation closes it immediately...
	r.Invalidate()
	_, err = r.Resolve(ctx, req(day(2026, 9, 1)))
	var ue *UnverifiedError
	require.True(t, errors.As(err, &ue))

	// ...and so does the TTL expiring.
	w.src.loaded[id].Key = w.key
	r.Invalidate()
	_, err = r.Resolve(ctx, req(day(2026, 9, 1)))
	require.NoError(t, err)
	revoked2 := *w.key
	revoked2.Status = domain.KeyRevoked
	w.src.loaded[id].Key = &revoked2
	c.t = c.t.Add(31 * time.Second)
	_, err = r.Resolve(ctx, req(day(2026, 9, 1)))
	require.True(t, errors.As(err, &ue), "after the TTL the artifact is re-verified")
}

func TestCache_ZeroTTLNeverCaches(t *testing.T) {
	w := newWorld(t)
	w.addPack(t, "jur.gb.tax.core", "1.0", "RELEASED", day(2026, 1, 1), nil, ruleSpec{id: "r1", code: "STD", from: day(2026, 1, 1)})
	r := w.resolver(t, 0, nil, nil)
	for i := 0; i < 3; i++ {
		_, err := r.Resolve(context.Background(), req(day(2026, 9, 1)))
		require.NoError(t, err)
	}
	assert.Equal(t, 3, w.src.loadCall)
}

func TestResolve_ValidatesTheRequest(t *testing.T) {
	w := newWorld(t)
	r := w.resolver(t, time.Minute, nil, nil)
	for _, bad := range []Request{
		{RuleDomain: "TAX", RuleCode: "STD", At: day(2026, 1, 1)},
		{Jurisdiction: "GB", RuleCode: "STD", At: day(2026, 1, 1)},
		{Jurisdiction: "GB", RuleDomain: "TAX", At: day(2026, 1, 1)},
		{Jurisdiction: "GB", RuleDomain: "TAX", RuleCode: "STD"},
	} {
		_, err := r.Resolve(context.Background(), bad)
		assert.ErrorIs(t, err, ErrBadRequest)
	}
	_, err := New(w.src, Config{})
	assert.Error(t, err, "a resolver with no eligible status would silently answer nothing")
}
