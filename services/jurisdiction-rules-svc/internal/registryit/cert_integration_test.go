//go:build integration

// ZS-JUR-001 Wave 3 end to end: test bundles, the harness, independent
// reviews and signed certification, with every gate attacked both through the
// API and, where the database is the last line of defence, in raw SQL.
package registryit_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
)

func newEnvPolicy(signer domain.Signer, minReviews int) *env {
	pub := &spyPub{}
	auth := &recordingAuthz{}
	r := chi.NewRouter()
	h := handler.New(st, auth, pub, "platform-scope", zap.NewNop()).WithRegistry(st, pub).WithCertificationPolicy(minReviews)
	if signer != nil {
		h.WithSigner(signer)
	}
	handler.RegisterRoutes(r, h)
	return &env{srv: r, pub: pub, auth: auth}
}

type certFixture struct {
	*fixture
	e       *env
	version string
	keyRef  string
}

// signedReady builds a fixture whose version is compiled and signed, so the
// remaining gates (tests, reviews, certifier) can be exercised one by one.
func signedReady(t *testing.T, minReviews int) *certFixture {
	t.Helper()
	return signedReadyEnv(t, func(s domain.Signer) *env { return newEnvPolicy(s, minReviews) })
}

// signedReadyEnv is signedReady with a caller-built environment (for example one with the resolver wired).
func signedReadyEnv(t *testing.T, mk func(domain.Signer) *env) *certFixture {
	t.Helper()
	keyRef := "cert-key-" + strings.ToLower(uuid.NewString()[:6])
	signer, pubB64 := newSigner(t, keyRef)
	e := mk(signer)
	f := newFixture(t, e)
	c := &certFixture{fixture: f, e: e, version: "2026.08.1", keyRef: keyRef}

	f.review(t, e, c.version, f.ruleID) // author drafts and submits for review
	code, r := e.do("POST", f.path(c.version, "/compile"), "compiler", nil)
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": keyRef, "public_key": pubB64})
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", f.path(c.version, "/sign"), "signer", nil)
	mustStatus(t, 200, code, r)
	return c
}

func (c *certFixture) artifactDoc(t *testing.T) *domain.ArtifactDoc {
	t.Helper()
	code, got := c.e.do("GET", "/v1/packs/"+c.packRef+"/versions/"+c.version+"/artifact", "", nil)
	mustStatus(t, 200, code, got)
	art := got["artifact"].(map[string]any)["artifact"]
	raw, err := json.Marshal(art)
	require.NoError(t, err)
	doc, err := domain.ParseArtifact(raw)
	require.NoError(t, err)
	return doc
}

// completeBundleFor derives a bundle that satisfies every coverage rule for the artifact.
func (c *certFixture) completeBundleFor(t *testing.T) map[string]any {
	t.Helper()
	doc := c.artifactDoc(t)
	codeByID := map[string]string{}
	for _, j := range doc.Jurisdictions {
		codeByID[j.ID] = j.Code
	}
	var cases []map[string]any
	n := 0
	add := func(class, juris string, at time.Time, rule *domain.ArtifactRule, expect map[string]any) {
		n++
		cases = append(cases, map[string]any{"id": class + "-" + string(rune('a'+n%26)) + string(rune('a'+n/26)), "class": class,
			"input":  map[string]any{"jurisdiction": juris, "rule_domain": "TAX", "rule_code": rule.RuleCode, "effective_at": at.UTC().Format(time.RFC3339Nano)},
			"expect": expect})
	}
	for i := range doc.Rules {
		r := &doc.Rules[i]
		jc := codeByID[r.JurisdictionID]
		resolved := map[string]any{"outcome": domain.OutcomeResolved, "rule_id": r.RuleID}
		add(domain.ClassGolden, jc, r.EffectiveFrom.Add(time.Hour), r, resolved)
		add(domain.ClassBoundary, jc, r.EffectiveFrom, r, resolved)
		add(domain.ClassBoundary, jc, r.EffectiveFrom.Add(-time.Nanosecond), r, map[string]any{"outcome": domain.OutcomeNoRule})
	}
	add(domain.ClassNegative, "NOT-IN-THIS-PACK", doc.Rules[0].EffectiveFrom.Add(2*time.Hour), &doc.Rules[0], map[string]any{"outcome": domain.OutcomeUnsupported})
	return map[string]any{"bundle_version": "1", "cases": cases}
}

func (c *certFixture) path(suffix string) string { return c.fixture.path(c.version, suffix) }

func (c *certFixture) testsPassing(t *testing.T) {
	t.Helper()
	code, r := c.e.do("POST", c.path("/test-bundle"), "tester", c.completeBundleFor(t))
	require.Contains(t, []int{200, 201}, code, "%v", r)
	code, r = c.e.do("POST", c.path("/test-runs"), "tester", nil)
	mustStatus(t, 201, code, r)
	require.Equal(t, true, r["passed"], "%v", r)
}

func (c *certFixture) approve(t *testing.T, reviewer, role string) {
	t.Helper()
	code, r := c.e.do("POST", c.path("/reviews"), reviewer, map[string]any{"role": role, "decision": "APPROVE"})
	mustStatus(t, 201, code, r)
}

func reasonsOf(r map[string]any) string {
	b, _ := json.Marshal(r["reasons"])
	return string(b)
}

func TestCertification_FullLifecycle(t *testing.T) {
	c := signedReady(t, 1)
	e := c.e

	// Nothing done yet: every unmet gate is reported in one answer.
	code, r := e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 409, code, r)
	assert.Equal(t, "certification_blocked", r["error"])
	assert.Contains(t, reasonsOf(r), "no_test_bundle")
	assert.Contains(t, reasonsOf(r), "insufficient_reviews")

	c.testsPassing(t)
	code, r = e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 409, code, r)
	assert.NotContains(t, reasonsOf(r), "tests_", "the test gate is now satisfied")
	assert.Contains(t, reasonsOf(r), "insufficient_reviews")

	c.approve(t, "tax-reviewer", "tax")
	code, cert := e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 201, code, cert)
	assert.Equal(t, c.keyRef, cert["signature_key_ref"])
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, cert["report_digest"])
	report := cert["report"].(map[string]any)
	assert.Contains(t, report["classes_not_implemented"], "REGRESSION_CORPUS", "the certificate states what it did not prove")
	assert.Equal(t, "certifier", report["certified_by"])

	// The version is now CERTIFIED and carries the bundle digest.
	var status string
	var bundleDigest *string
	require.NoError(t, pool.QueryRow(ctx, `SELECT v.status, v.test_bundle_digest FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id)
		WHERE p.pack_ref=$1 AND v.version=$2`, c.packRef, c.version).Scan(&status, &bundleDigest))
	assert.Equal(t, "CERTIFIED", status)
	require.NotNil(t, bundleDigest)
	assert.Equal(t, cert["bundle_digest"], *bundleDigest)

	// Replay is idempotent; one event.
	code, again := e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 200, code, again)
	assert.Equal(t, cert["certification_id"], again["certification_id"])
	assert.Equal(t, 1, e.pub.count(events.EventPackCertified))

	code, got := e.do("GET", "/v1/packs/"+c.packRef+"/versions/"+c.version+"/certification", "", nil)
	mustStatus(t, 200, code, got)
	assert.Equal(t, true, got["verification"].(map[string]any)["verified"])

	// Evidence is frozen: nothing more can be added once CERTIFIED.
	code, r = e.do("POST", c.path("/test-bundle"), "tester", c.completeBundleFor(t))
	assert.Equal(t, 409, code, "%v", r)
	code, r = e.do("POST", c.path("/reviews"), "late-reviewer", map[string]any{"role": "LEGAL", "decision": "APPROVE"})
	assert.Equal(t, 409, code, "%v", r)
	code, r = e.do("POST", c.path("/test-runs"), "tester", nil)
	assert.Equal(t, 409, code, "%v", r)

	// Tampering with the signed report is caught by the verifier even with the guard off.
	withoutGuards(t, "pack_certifications", func() {
		_, err := pool.Exec(ctx, `UPDATE pack_certifications SET report = jsonb_set(report, '{certified_by}', '"someone-else"') WHERE certification_id=$1`, cert["certification_id"])
		require.NoError(t, err)
		_, got := e.do("GET", "/v1/packs/"+c.packRef+"/versions/"+c.version+"/certification", "", nil)
		assert.Equal(t, false, got["verification"].(map[string]any)["verified"])
		_, err = pool.Exec(ctx, `UPDATE pack_certifications SET report = jsonb_set(report, '{certified_by}', '"certifier"') WHERE certification_id=$1`, cert["certification_id"])
		require.NoError(t, err)
	})
	_, got = e.do("GET", "/v1/packs/"+c.packRef+"/versions/"+c.version+"/certification", "", nil)
	assert.Equal(t, true, got["verification"].(map[string]any)["verified"])

	// Append-only in SQL.
	_, err := pool.Exec(ctx, `UPDATE pack_certifications SET certified_by='x' WHERE certification_id=$1`, cert["certification_id"])
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM pack_reviews WHERE pack_version_id IN (SELECT pack_version_id FROM pack_certifications WHERE certification_id=$1)`, cert["certification_id"])
	require.Error(t, err)
	_, err = pool.Exec(ctx, `UPDATE pack_test_runs SET passed=true`)
	require.Error(t, err)

	for _, p := range []string{"jurisdiction_pack_version.author_tests", "jurisdiction_pack_version.execute_tests",
		"jurisdiction_pack_version.review", "jurisdiction_pack_version.certify"} {
		assert.True(t, e.auth.pairs[p], p+" is a separate capability")
	}
}

func TestCertification_IndependenceIsEnforced(t *testing.T) {
	c := signedReady(t, 1) // author "author", compiler "compiler", signer irrelevant
	e := c.e

	// Test author "tester" can run tests but not review or certify.
	c.testsPassing(t)
	for _, builder := range []string{"author", "compiler", "tester"} {
		code, r := e.do("POST", c.path("/reviews"), builder, map[string]any{"role": "TAX", "decision": "APPROVE"})
		mustStatus(t, 403, code, r)
		assert.Equal(t, "segregation_of_duties", r["error"], builder)
	}

	// A reviewer cannot also certify.
	c.approve(t, "reviewer-1", "TAX")
	code, r := e.do("POST", c.path("/certify"), "reviewer-1", nil)
	mustStatus(t, 409, code, r)
	assert.Contains(t, reasonsOf(r), "certifier_not_independent")
	for _, builder := range []string{"author", "compiler", "tester"} {
		code, r = e.do("POST", c.path("/certify"), builder, nil)
		mustStatus(t, 409, code, r)
		assert.Contains(t, reasonsOf(r), "certifier_not_independent", builder)
	}

	// The database refuses the same self-certification even if the API were bypassed.
	var vid, runID, artDigest, bundleDigest string
	require.NoError(t, pool.QueryRow(ctx, `SELECT v.pack_version_id::text, (SELECT run_id::text FROM pack_test_runs WHERE pack_version_id=v.pack_version_id LIMIT 1),
			a.artifact_digest, (SELECT bundle_digest FROM pack_test_bundles WHERE pack_version_id=v.pack_version_id LIMIT 1)
		FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id) JOIN pack_artifacts a USING (pack_version_id)
		WHERE p.pack_ref=$1 AND v.version=$2`, c.packRef, c.version).Scan(&vid, &runID, &artDigest, &bundleDigest))
	for _, who := range []string{"author", "compiler", "tester", "reviewer-1"} {
		_, err := pool.Exec(ctx, `INSERT INTO pack_certifications (pack_version_id, artifact_digest, bundle_digest, test_run_id, min_reviews, report, report_digest, signature, signature_key_ref, certified_by)
			VALUES ($1,$2,$3,$4,1,'{}'::jsonb,$5,'x',$6,$7)`, vid, artDigest, bundleDigest, runID, "sha256:"+strings.Repeat("0", 64), c.keyRef, who)
		require.Error(t, err, "%s cannot certify, even in SQL", who)
	}
	_, err := pool.Exec(ctx, `INSERT INTO pack_reviews (pack_version_id, artifact_digest, reviewer, role, decision) VALUES ($1,$2,'author','TAX','APPROVE')`, vid, artDigest)
	require.Error(t, err, "the author cannot review, even in SQL")
	_, err = pool.Exec(ctx, `UPDATE jurisdiction_pack_versions SET status='CERTIFIED' WHERE pack_version_id=$1`, vid)
	require.Error(t, err, "CERTIFIED is reachable only through a certification record")

	code, r = e.do("POST", c.path("/certify"), "independent-certifier", nil)
	mustStatus(t, 201, code, r)
}

func TestCertification_RejectionsAndFailingTestsBlock(t *testing.T) {
	c := signedReady(t, 1)
	e := c.e

	// A failing run is recorded as evidence and blocks.
	bad := c.completeBundleFor(t)
	cases := bad["cases"].([]map[string]any)
	cases[0]["expect"] = map[string]any{"outcome": domain.OutcomeNoRule} // golden now expects the wrong thing
	code, r := e.do("POST", c.path("/test-bundle"), "tester", bad)
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", c.path("/test-runs"), "tester", nil)
	mustStatus(t, 201, code, r)
	assert.Equal(t, false, r["passed"])
	c.approve(t, "reviewer-1", "TAX")
	code, r = e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 409, code, r)
	assert.Contains(t, reasonsOf(r), "tests_failed")

	// A corrected bundle is a new revision; until it is run, the old run no longer counts.
	code, r = e.do("POST", c.path("/test-bundle"), "tester", c.completeBundleFor(t))
	mustStatus(t, 201, code, r)
	assert.EqualValues(t, 2, r["revision"])
	code, r = e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 409, code, r)
	assert.Contains(t, reasonsOf(r), "tests_not_run")
	code, r = e.do("POST", c.path("/test-bundle"), "tester", c.completeBundleFor(t))
	mustStatus(t, 200, code, r)
	assert.EqualValues(t, 2, r["revision"], "an identical bundle is not a new revision")
	code, r = e.do("POST", c.path("/test-runs"), "tester", nil)
	mustStatus(t, 201, code, r)
	require.Equal(t, true, r["passed"])

	// A rejection blocks until the same reviewer changes their decision.
	code, r = e.do("POST", c.path("/reviews"), "reviewer-2", map[string]any{"role": "legal", "decision": "REJECT"})
	mustStatus(t, 400, code, r) // findings are mandatory for a rejection
	code, r = e.do("POST", c.path("/reviews"), "reviewer-2", map[string]any{"role": "legal", "decision": "REJECT", "findings": "s.4 reading is wrong"})
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 409, code, r)
	assert.Contains(t, reasonsOf(r), "review_rejected")
	c.approve(t, "reviewer-2", "LEGAL") // the latest decision wins
	code, r = e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 201, code, r)

	code, rv := e.do("GET", "/v1/packs/"+c.packRef+"/versions/"+c.version+"/reviews", "", nil)
	mustStatus(t, 200, code, rv)
	assert.Len(t, rv["reviews"], 3, "the full review history is kept, including the rejection")
}

func TestCertification_CoverageGapsFailTheRun(t *testing.T) {
	c := signedReady(t, 1)
	// One golden case only: every case passes, but coverage is incomplete.
	doc := c.artifactDoc(t)
	only := map[string]any{"bundle_version": "1", "cases": []map[string]any{{
		"id": "g1", "class": "GOLDEN",
		"input":  map[string]any{"jurisdiction": c.jur.JurisdictionCode, "rule_domain": "TAX", "rule_code": doc.Rules[0].RuleCode, "effective_at": doc.Rules[0].EffectiveFrom.Add(time.Hour).Format(time.RFC3339Nano)},
		"expect": map[string]any{"outcome": domain.OutcomeResolved, "rule_id": doc.Rules[0].RuleID}}}}
	code, r := c.e.do("POST", c.path("/test-bundle"), "tester", only)
	mustStatus(t, 201, code, r)
	code, r = c.e.do("POST", c.path("/test-runs"), "tester", nil)
	mustStatus(t, 201, code, r)
	assert.Equal(t, false, r["passed"])
	res := r["result"].(map[string]any)
	assert.EqualValues(t, 0, res["failed"])
	assert.Equal(t, false, res["coverage_complete"])
	assert.Contains(t, mustString(res["coverage_gaps"]), "JUR-T002")
	assert.Contains(t, mustString(res["coverage_gaps"]), "JUR-T004")
}

func TestCertification_MinReviewsPolicy(t *testing.T) {
	c := signedReady(t, 2)
	c.testsPassing(t)
	c.approve(t, "reviewer-1", "TAX")
	code, r := c.e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 409, code, r)
	assert.Contains(t, reasonsOf(r), "insufficient_reviews")
	// The same person approving under a second role is still one reviewer.
	c.approve(t, "reviewer-1", "LEGAL")
	code, r = c.e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 409, code, r)
	assert.Contains(t, reasonsOf(r), "insufficient_reviews")
	c.approve(t, "reviewer-2", "LEGAL")
	code, r = c.e.do("POST", c.path("/certify"), "certifier", nil)
	mustStatus(t, 201, code, r)
	assert.EqualValues(t, 2, r["min_reviews"])
}

func TestCertification_RequiresSignedVerifiedArtifactAndASigner(t *testing.T) {
	// No signer configured: certification fails closed.
	e := newEnvPolicy(nil, 1)
	f := newFixture(t, e)
	f.review(t, e, "2026.08.1", f.ruleID)
	code, r := e.do("POST", f.path("2026.08.1", "/certify"), "certifier", nil)
	mustStatus(t, 503, code, r)
	assert.Equal(t, "signing_not_configured", r["error"])

	// Compiled but never signed, then reviewed and tested: still blocked.
	keyRef := "unsigned-" + strings.ToLower(uuid.NewString()[:6])
	signer, pub := newSigner(t, keyRef)
	e2 := newEnvPolicy(signer, 1)
	f2 := newFixture(t, e2)
	f2.review(t, e2, "2026.08.1", f2.ruleID)
	code, r = e2.do("POST", f2.path("2026.08.1", "/compile"), "compiler", nil)
	mustStatus(t, 201, code, r)
	c := &certFixture{fixture: f2, e: e2, version: "2026.08.1", keyRef: keyRef}
	c.testsPassing(t)
	c.approve(t, "reviewer-1", "TAX")
	code, r = e2.do("POST", f2.path("2026.08.1", "/certify"), "certifier", nil)
	mustStatus(t, 409, code, r)
	assert.Contains(t, reasonsOf(r), "artifact_unverified")
	assert.Contains(t, reasonsOf(r), domain.ReasonUnsigned)

	// Signed, but the signing key is not registered/ACTIVE for the certifying service: 503, nothing certified.
	code, r = e2.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": keyRef, "public_key": pub})
	mustStatus(t, 201, code, r)
	code, r = e2.do("POST", f2.path("2026.08.1", "/sign"), "signer", nil)
	mustStatus(t, 200, code, r)
	code, r = e2.do("POST", "/v1/admin/pack-signing-keys/"+keyRef+"/retire", "security", map[string]any{"reason": "rotation"})
	mustStatus(t, 200, code, r)
	code, r = e2.do("POST", f2.path("2026.08.1", "/certify"), "certifier", nil)
	mustStatus(t, 503, code, r)
	assert.Equal(t, "signing_key_not_active", r["error"], "a retired key cannot sign new certifications")

	// Unknown version and not-compiled paths are precise errors.
	code, _ = e2.do("POST", f2.path("9.9.9", "/certify"), "certifier", nil)
	assert.Equal(t, 404, code)
	code, _ = e2.do("GET", "/v1/packs/"+f2.packRef+"/versions/2026.08.1/certification", "", nil)
	assert.Equal(t, 404, code)
}

func TestCertification_TestBundleValidationAndAuth(t *testing.T) {
	c := signedReady(t, 1)
	e := c.e
	for name, body := range map[string]any{
		"unknown class": map[string]any{"bundle_version": "1", "cases": []map[string]any{{"id": "x", "class": "PERFORMANCE"}}},
		"empty":         map[string]any{"bundle_version": "1", "cases": []map[string]any{}},
		"unknown field": map[string]any{"bundle_version": "1", "cases": []map[string]any{}, "extra": 1},
		"wrong version": map[string]any{"bundle_version": "9", "cases": []map[string]any{}},
	} {
		code, r := e.do("POST", c.path("/test-bundle"), "tester", body)
		assert.Equal(t, 400, code, "%s: %v", name, r)
	}
	code, _ := e.do("POST", c.path("/test-bundle"), "", c.completeBundleFor(t))
	assert.Equal(t, 401, code)
	e.auth.deny = "jurisdiction_pack_version.certify"
	code, _ = e.do("POST", c.path("/certify"), "certifier", nil)
	assert.Equal(t, 403, code)
	code, r := e.do("POST", c.path("/test-runs"), "tester", nil)
	assert.Equal(t, 409, code, "running before any bundle exists: %v", r)
	assert.Equal(t, "no_test_bundle", r["error"])
	code, _ = e.do("GET", c.path("/test-bundle")[len("/v1/admin"):], "", nil)
	_ = code
	code, _ = e.do("GET", "/v1/packs/"+c.packRef+"/versions/"+c.version+"/test-bundle", "", nil)
	assert.Equal(t, 409, code, "no bundle yet")
}
