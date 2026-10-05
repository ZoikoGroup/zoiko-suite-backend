//go:build integration

// ZS-JUR-001 Wave 2 end to end: the runtime resolver against the real store
// and real, signed, certified, released pack artifacts. Release (Wave 7) does
// not exist yet, so the tests move a CERTIFIED version to RELEASED with a
// direct update, which the database permits as a legal edge.
package registryit_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
	"zoiko.io/jurisdiction-rules-svc/internal/resolver"
)

func newEnvResolver(signer domain.Signer, statuses []string, ttl time.Duration) *env {
	pub := &spyPub{}
	auth := &recordingAuthz{}
	res, err := resolver.New(st, resolver.Config{
		EligibleStatuses: statuses, CacheTTL: ttl,
		OnUnverified: func(ref, ver string, reasons []string) {
			_ = pub.PublishRegistryEvent(ctx, events.EventPackVerificationFailed, ref+"@"+ver, "runtime-resolver", "", map[string]any{"reasons": reasons})
		},
	})
	if err != nil {
		panic(err)
	}
	r := chi.NewRouter()
	h := handler.New(st, auth, pub, "platform-scope", zap.NewNop()).WithRegistry(st, pub).WithCertificationPolicy(1).WithResolver(res)
	if signer != nil {
		h.WithSigner(signer)
	}
	handler.RegisterRoutes(r, h)
	return &env{srv: r, pub: pub, auth: auth}
}

// certifyVersion takes an already-drafted-and-reviewed version through compile,
// sign, tests, one independent review and certification.
func (c *certFixture) certifyVersion(t *testing.T, version string) {
	t.Helper()
	p := func(suffix string) string { return c.fixture.path(version, suffix) }
	if version != c.version { // the first version is prepared by signedReady
		c.fixture.review(t, c.e, version, c.ruleID)
		code, r := c.e.do("POST", p("/compile"), "compiler", nil)
		mustStatus(t, 201, code, r)
		code, r = c.e.do("POST", p("/sign"), "signer", nil)
		mustStatus(t, 200, code, r)
	}
	old := c.version
	c.version = version
	defer func() { c.version = old }()
	c.testsPassing(t)
	c.approve(t, "reviewer-"+version, "TAX")
	code, r := c.e.do("POST", p("/certify"), "certifier", nil)
	mustStatus(t, 201, code, r)
}

// setStatus drives a version to a status through the real lifecycle commands
// (RELEASED = publish, WITHDRAWN = withdraw); SUPERSEDED has no command yet and
// is set directly, which the database allows (it guards only the other three).
func setStatus(t *testing.T, c *certFixture, version, status string) {
	t.Helper()
	switch status {
	case "RELEASED":
		code, r := c.e.do("POST", c.fixture.path(version, "/publish"), "releaser", nil)
		require.Equal(t, 200, code, "publish %s: %v", version, r)
		return
	case "WITHDRAWN":
		code, r := c.e.do("POST", c.fixture.path(version, "/withdraw"), "releaser", map[string]any{"reason": "test withdrawal", "evidence_ref": "TICKET-1"})
		require.Equal(t, 200, code, "withdraw %s: %v", version, r)
		return
	}
	_, err := pool.Exec(ctx, `UPDATE jurisdiction_pack_versions v SET status=$3 FROM jurisdiction_packs p
		WHERE p.pack_id=v.pack_id AND p.pack_ref=$1 AND v.version=$2`, c.packRef, version, status)
	require.NoError(t, err, "%s -> %s", version, status)
}

// releasedPack returns a fixture with one RELEASED, certified pack version and an env with the resolver.
func releasedPack(t *testing.T, ttl time.Duration) *certFixture {
	t.Helper()
	c := signedReadyEnv(t, func(s domain.Signer) *env { return newEnvResolver(s, []string{"RELEASED"}, ttl) })
	c.certifyVersion(t, c.version)
	setStatus(t, c, c.version, "RELEASED")
	return c
}

func (c *certFixture) resolveReq(at time.Time) map[string]any {
	return map[string]any{"jurisdiction": c.jur.JurisdictionCode, "rule_domain": "TAX", "rule_code": "STD", "effective_at": at.UTC().Format(time.RFC3339Nano)}
}

func TestResolver_EndToEnd_DecisionIsExplainableAndRecorded(t *testing.T) {
	c := releasedPack(t, 0)
	e := c.e

	code, r := e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	mustStatus(t, 200, code, r)
	dec := r["decision"].(map[string]any)
	assert.Equal(t, "RESOLVED", dec["outcome"])
	pack := dec["pack"].(map[string]any)
	assert.Equal(t, c.packRef, pack["pack_ref"])
	assert.Equal(t, c.version, pack["version"])
	assert.NotEmpty(t, pack["certification_id"])
	rule := dec["rule"].(map[string]any)
	assert.Equal(t, c.ruleID, rule["rule_id"])
	assert.Equal(t, "0.2000", rule["payload"].(map[string]any)["rate"])
	assert.Len(t, dec["sources"], 1)
	assert.Equal(t, "SINGLE_CANDIDATE", dec["basis"])
	assert.NotEmpty(t, dec["explanation"])
	id := r["decision_id"].(string)

	// Evidence: the exact pack release and rule content are recorded and immutable.
	var artDigest, ruleID, outcome string
	require.NoError(t, pool.QueryRow(ctx, `SELECT artifact_digest, rule_id::text, outcome FROM rule_decision_evidence WHERE decision_id=$1`, id).Scan(&artDigest, &ruleID, &outcome))
	assert.Equal(t, pack["artifact_digest"], artDigest)
	assert.Equal(t, c.ruleID, ruleID)
	code, got := e.do("GET", "/v1/rule-decisions/"+id, "auditor", nil)
	mustStatus(t, 200, code, got)
	assert.Equal(t, id, got["decision_id"])
	_, err := pool.Exec(ctx, `UPDATE rule_decision_evidence SET outcome='NO_RULE' WHERE decision_id=$1`, id)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM rule_decision_evidence WHERE decision_id=$1`, id)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO rule_decision_evidence (requested_by, request_digest, request, effective_at, outcome, response)
		VALUES ('x','sha256:`+strings.Repeat("0", 64)+`','{}'::jsonb,NOW(),'RESOLVED','{}'::jsonb)`)
	require.Error(t, err, "a RESOLVED decision without its pack and rule basis cannot be stored")

	// Definitive non-answers are explicit, and recorded too.
	code, r = e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now().Add(-90*24*time.Hour)))
	mustStatus(t, 200, code, r)
	assert.Equal(t, "NO_RULE", r["decision"].(map[string]any)["outcome"], "before the rule starts: the old rate is not invented")
	bad := c.resolveReq(time.Now())
	bad["jurisdiction"] = "ATLANTIS"
	code, r = e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", bad)
	mustStatus(t, 422, code, r)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", r["decision"].(map[string]any)["outcome"], "JUR-NEG-22")
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM rule_decision_evidence WHERE requested_by='tax-engine' AND pack_version_id IS NULL`).Scan(&n))
	assert.GreaterOrEqual(t, n, 2)

	// Auth and request hygiene.
	code, _ = e.do("POST", "/v1/rule-resolutions:resolve", "", c.resolveReq(time.Now()))
	assert.Equal(t, 401, code)
	e.auth.deny = "rule_resolution.resolve"
	code, _ = e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	assert.Equal(t, 403, code)
	e.auth.deny = ""
	code, _ = e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", map[string]any{"jurisdiction": "x"})
	assert.Equal(t, 400, code)
	badAt := c.resolveReq(time.Now())
	badAt["effective_at"] = "yesterday"
	code, _ = e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", badAt)
	assert.Equal(t, 400, code)
	code, _ = e.do("GET", "/v1/rule-decisions/00000000-0000-0000-0000-000000000000", "auditor", nil)
	assert.Equal(t, 404, code)
	code, _ = e.do("GET", "/v1/rule-decisions/not-a-uuid", "auditor", nil)
	assert.Equal(t, 404, code)
}

func TestResolver_IdempotencyKeyBindsOneQuestion(t *testing.T) {
	c := releasedPack(t, 0)
	e := c.e
	do := func(key string, body map[string]any) (int, map[string]any) {
		return e.doWithHeaders("POST", "/v1/rule-resolutions:resolve", "tax-engine", body, map[string]string{"Idempotency-Key": key})
	}
	at := time.Now()
	code, first := do("k-1", c.resolveReq(at))
	mustStatus(t, 200, code, first)
	assert.Equal(t, false, first["replayed"])
	code, again := do("k-1", c.resolveReq(at))
	mustStatus(t, 200, code, again)
	assert.Equal(t, true, again["replayed"])
	assert.Equal(t, first["decision_id"], again["decision_id"], "a retry returns the same decision, not a second record")
	other := c.resolveReq(at.Add(time.Hour))
	code, r := do("k-1", other)
	mustStatus(t, 409, code, r)
	assert.Equal(t, "idempotency_conflict", r["error"])
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM rule_decision_evidence WHERE idempotency_key='k-1'`).Scan(&n))
	assert.Equal(t, 1, n)
	// The key is scoped to the caller.
	code, r = e.doWithHeaders("POST", "/v1/rule-resolutions:resolve", "other-engine", c.resolveReq(at), map[string]string{"Idempotency-Key": "k-1"})
	mustStatus(t, 200, code, r)
	assert.Equal(t, false, r["replayed"])
}

func TestResolver_OnlyReleasedPacksResolve_CertifiedNeedsExplicitNonProductionOptIn(t *testing.T) {
	// Default eligibility: CERTIFIED is "not yet production eligible" (s23).
	c := signedReadyEnv(t, func(s domain.Signer) *env { return newEnvResolver(s, []string{"RELEASED"}, 0) })
	c.certifyVersion(t, c.version) // CERTIFIED, not released
	code, r := c.e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	mustStatus(t, 422, code, r)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", r["decision"].(map[string]any)["outcome"])
	code, r = c.e.do("GET", "/v1/pack-coverage?jurisdiction="+c.jur.JurisdictionCode, "", nil)
	mustStatus(t, 200, code, r)
	assert.Empty(t, r["packs"], "an unreleased pack is not advertised as supported")

	// A lower environment may opt in to CERTIFIED.
	c2 := signedReadyEnv(t, func(s domain.Signer) *env { return newEnvResolver(s, []string{"RELEASED", "CERTIFIED"}, 0) })
	c2.certifyVersion(t, c2.version)
	code, r = c2.e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c2.resolveReq(time.Now()))
	mustStatus(t, 200, code, r)
	assert.Equal(t, "RESOLVED", r["decision"].(map[string]any)["outcome"])

	// Released: coverage shows it.
	setStatus(t, c, c.version, "RELEASED")
	code, r = c.e.do("GET", "/v1/pack-coverage?jurisdiction="+c.jur.JurisdictionCode, "", nil)
	mustStatus(t, 200, code, r)
	require.Len(t, r["packs"], 1)
	assert.Equal(t, true, r["packs"].([]any)[0].(map[string]any)["in_force"])
	code, _ = c.e.do("GET", "/v1/pack-coverage", "", nil)
	assert.Equal(t, 400, code)
}

func TestResolver_FailsClosed_OnRevokedKey_AndOnTamperedArtifact(t *testing.T) {
	// Tampered stored artifact (JUR-NEG-03): refused and a security event is raised.
	c := releasedPack(t, 0)
	e := c.e
	withoutGuards(t, "pack_artifacts", func() {
		_, err := pool.Exec(ctx, `UPDATE pack_artifacts a SET artifact = replace(artifact, '0.2000', '0.0000') FROM jurisdiction_pack_versions v
			JOIN jurisdiction_packs p USING (pack_id) WHERE a.pack_version_id=v.pack_version_id AND p.pack_ref=$1`, c.packRef)
		require.NoError(t, err)
		code, r := e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
		mustStatus(t, 503, code, r)
		assert.Equal(t, "pack_unverified", r["error"])
		assert.Contains(t, mustString(r["failures"]), domain.ReasonDigestMismatch)
		assert.GreaterOrEqual(t, e.pub.count(events.EventPackVerificationFailed), 1)
		_, err = pool.Exec(ctx, `UPDATE pack_artifacts a SET artifact = replace(artifact, '0.0000', '0.2000') FROM jurisdiction_pack_versions v
			JOIN jurisdiction_packs p USING (pack_id) WHERE a.pack_version_id=v.pack_version_id AND p.pack_ref=$1`, c.packRef)
		require.NoError(t, err)
	})
	code, r := e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	mustStatus(t, 200, code, r)

	// Revoked signing key: every pack signed with it stops resolving.
	code, r = e.do("POST", "/v1/admin/pack-signing-keys/"+c.keyRef+"/revoke", "security", map[string]any{"reason": "compromised"})
	mustStatus(t, 200, code, r)
	code, r = e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	mustStatus(t, 503, code, r)
	assert.Contains(t, mustString(r["failures"]), domain.ReasonKeyRevoked)
	// No evidence is recorded for a refused resolution: there was no decision.
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM rule_decision_evidence WHERE outcome='RESOLVED' AND pack_version_id IN (
		SELECT v.pack_version_id FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref=$1)`, c.packRef).Scan(&n))
	assert.Equal(t, 1, n, "only the one good decision from before the revocation")
}

func TestResolver_CacheStalenessIsBounded_AndInvalidationIsImmediate(t *testing.T) {
	c := releasedPack(t, time.Minute)
	e := c.e
	code, r := e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	mustStatus(t, 200, code, r)

	code, r = e.do("POST", "/v1/admin/pack-signing-keys/"+c.keyRef+"/revoke", "security", map[string]any{"reason": "compromised"})
	mustStatus(t, 200, code, r)
	code, r = e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	mustStatus(t, 200, code, r) // inside the TTL: the documented staleness window

	code, r = e.do("POST", "/v1/admin/resolver-cache:invalidate", "operator", nil)
	mustStatus(t, 200, code, r)
	assert.Equal(t, true, r["invalidated"])
	code, r = e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	mustStatus(t, 503, code, r)
	assert.True(t, e.auth.pairs["resolver_cache.invalidate"], "invalidation is its own privileged action")
}

func TestResolver_PicksNewestReleasedVersion_AndPinnedReplayReproducesHistory(t *testing.T) {
	c := releasedPack(t, 0)
	e := c.e
	_, first := e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	firstPack := first["decision"].(map[string]any)["pack"].(map[string]any)
	assert.Equal(t, "2026.08.1", firstPack["version"])

	// A corrected version is built and released.
	c.certifyVersion(t, "2026.09.1") // drafts, submits, compiles, signs, tests, reviews and certifies
	setStatus(t, c, "2026.09.1", "RELEASED")
	code, r := e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	mustStatus(t, 200, code, r)
	assert.Equal(t, "2026.09.1", r["decision"].(map[string]any)["pack"].(map[string]any)["version"], "new decisions use the newest released version")

	// The old one is superseded; a past decision can still be reproduced against it.
	setStatus(t, c, "2026.08.1", "SUPERSEDED")
	pinned := c.resolveReq(time.Now())
	pinned["pack_ref"], pinned["pack_version"] = c.packRef, "2026.08.1"
	code, r = e.do("POST", "/v1/rule-resolutions:resolve", "auditor", pinned)
	mustStatus(t, 200, code, r)
	assert.Equal(t, "2026.08.1", r["decision"].(map[string]any)["pack"].(map[string]any)["version"], "JUR-NEG-07: replay uses the original version")
	assert.Equal(t, firstPack["artifact_digest"], r["decision"].(map[string]any)["pack"].(map[string]any)["artifact_digest"])

	// Once withdrawn it never resolves, even when pinned; unknown versions are 404.
	setStatus(t, c, "2026.08.1", "WITHDRAWN")
	code, r = e.do("POST", "/v1/rule-resolutions:resolve", "auditor", pinned)
	mustStatus(t, 409, code, r)
	assert.Equal(t, "pinned_pack_not_eligible", r["error"])
	pinned["pack_version"] = "9.9.9"
	code, _ = e.do("POST", "/v1/rule-resolutions:resolve", "auditor", pinned)
	assert.Equal(t, 404, code)
	bad := c.resolveReq(time.Now())
	bad["pack_version"] = "2026.09.1"
	code, _ = e.do("POST", "/v1/rule-resolutions:resolve", "auditor", bad)
	assert.Equal(t, 400, code, "a version pin needs a pack_ref")
}

func TestResolver_RoutesAreAbsentUnlessWired(t *testing.T) {
	e := newEnv() // registry only
	code, _ := e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", map[string]any{})
	assert.Equal(t, 404, code, "the resolver is opt-in; the original surface is unchanged")
	code, _ = e.do("GET", "/v1/pack-coverage?jurisdiction=GB", "", nil)
	assert.Equal(t, 404, code)
	_ = fmt.Sprint
}
