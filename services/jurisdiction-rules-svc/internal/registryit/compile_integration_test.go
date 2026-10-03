//go:build integration

// ZS-JUR-001 Wave 1 end to end: compile, sign, verify, trusted keys. Real
// Postgres, real store, real handlers; tampering is done in raw SQL with the
// guard triggers temporarily disabled, which is exactly what an attacker with
// database access would try.
package registryit_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
)

type fixture struct {
	jur       *domain.Jurisdiction
	regimeID  string
	regimeCod string
	sourceID  string
	interpID  string
	ruleID    string
	packRef   string
}

func uniqueRef(prefix string) string {
	return prefix + strings.ToLower(uuid.NewString()[:6]) + ".tax.core"
}

// newFixture builds a jurisdiction, a reviewed source, an approved
// interpretation, a regime and one DRAFT rule carrying full provenance.
func newFixture(t *testing.T, e *env) *fixture {
	t.Helper()
	f := &fixture{jur: seedJurisdiction(t, "ZZ"), regimeCod: "VAT"}

	code, regime := e.do("POST", "/v1/admin/regimes", "author", map[string]any{"regime_code": f.regimeCod, "regime_name": "Value added tax"})
	require.Contains(t, []int{200, 201}, code, "%v", regime)
	f.regimeID = regime["regime_id"].(string)

	_, src := e.do("POST", "/v1/admin/sources", "author", sourceBody(f.jur.JurisdictionID))
	f.sourceID = src["source_id"].(string)
	code, r := e.do("POST", "/v1/admin/sources/"+f.sourceID+"/review", "reviewer", nil)
	mustStatus(t, 200, code, r)
	code, interp := e.do("POST", "/v1/admin/interpretations", "author", map[string]any{
		"jurisdiction_id": f.jur.JurisdictionID, "regime_id": f.regimeID, "subject": "s", "decision": "d", "rationale": "r",
		"source_ids": []string{f.sourceID}})
	mustStatus(t, 201, code, interp)
	f.interpID = interp["interpretation_id"].(string)
	code, r = e.do("POST", "/v1/admin/interpretations/"+f.interpID+"/approve", "approver", nil)
	mustStatus(t, 200, code, r)

	f.ruleID = f.newRule(t, e, "STD", time.Now().UTC().Add(-24*time.Hour), nil, 1)

	f.packRef = uniqueRef("jur.c")
	code, p := e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": f.packRef, "pack_name": "Core", "owner": "Tax Engineering"})
	mustStatus(t, 201, code, p)
	return f
}

func (f *fixture) newRule(t *testing.T, e *env, ruleCode string, from time.Time, to *time.Time, prec int) string {
	t.Helper()
	rule, _, err := st.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionID: f.jur.JurisdictionID, RuleDomain: "TAX", RuleCode: ruleCode, RuleName: ruleCode, EffectiveFrom: from, EffectiveTo: to,
		RulePayload: []byte(`{"applies":["B2C"],"rate":"0.2000"}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "seed"})
	require.NoError(t, err)
	code, r := e.do("PUT", "/v1/admin/rules/"+rule.JurisdictionRuleID+"/provenance", "author", map[string]any{
		"regime_id": f.regimeID, "interpretation_id": f.interpID, "precedence": prec, "published_on": "2026-01-01",
		"source_ids": []string{f.sourceID}})
	mustStatus(t, 200, code, r)
	return rule.JurisdictionRuleID
}

func (f *fixture) manifest(version string, rules ...string) map[string]any {
	m := manifest(f.packRef, version, f.jur.JurisdictionCode, map[string]any{"rule_modules": rules})
	m["dependencies"] = []map[string]string{{"ref": "ref.iso4217", "version": "2026.06"}}
	return m
}

// review drafts the version and moves it to REVIEW.
func (f *fixture) review(t *testing.T, e *env, version string, rules ...string) {
	t.Helper()
	code, v := e.do("POST", "/v1/admin/packs/"+f.packRef+"/versions", "author", f.manifest(version, rules...))
	mustStatus(t, 201, code, v)
	code, v = e.do("POST", "/v1/admin/packs/"+f.packRef+"/versions/"+version+"/submit-review", "author", nil)
	mustStatus(t, 200, code, v)
}

func (f *fixture) path(version, suffix string) string {
	return "/v1/admin/packs/" + f.packRef + "/versions/" + version + suffix
}

func newSigner(t *testing.T, keyRef string) (*domain.Ed25519Signer, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	s, err := domain.NewEd25519Signer(keyRef, priv)
	require.NoError(t, err)
	return s, base64.StdEncoding.EncodeToString(pub)
}

func withoutGuards(t *testing.T, table string, fn func()) {
	t.Helper()
	_, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER USER`, table))
	require.NoError(t, err)
	defer func() {
		_, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ENABLE TRIGGER USER`, table))
		require.NoError(t, err)
	}()
	fn()
}

func TestCompileSignVerify_FullLifecycle(t *testing.T) {
	keyRef := "pack-key-" + strings.ToLower(uuid.NewString()[:6])
	signer, pubB64 := newSigner(t, keyRef)
	e := newEnvWithSigner(signer)
	f := newFixture(t, e)

	// Compile needs REVIEW (a draft is still editable).
	code, v := e.do("POST", "/v1/admin/packs/"+f.packRef+"/versions", "author", f.manifest("2026.08.1", f.ruleID))
	mustStatus(t, 201, code, v)
	code, r := e.do("POST", f.path("2026.08.1", "/compile"), "compiler", nil)
	mustStatus(t, 409, code, r)
	assert.Equal(t, "not_under_review", r["error"])
	code, _ = e.do("POST", f.path("2026.08.1", "/submit-review"), "author", nil)
	require.Equal(t, 200, code)

	// Compile: 201, reproducible replay: 200, version row carries the digest.
	code, art := e.do("POST", f.path("2026.08.1", "/compile"), "compiler", nil)
	mustStatus(t, 201, code, art)
	digest := art["artifact_digest"].(string)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, digest)
	assert.Nil(t, art["signature"], "compile never signs")
	report := art["report"].(map[string]any)
	assert.Equal(t, false, report["tests_executed"])
	code, again := e.do("POST", f.path("2026.08.1", "/compile"), "compiler", nil)
	mustStatus(t, 200, code, again)
	assert.Equal(t, digest, again["artifact_digest"])
	assert.Equal(t, 1, e.pub.count(events.EventPackVersionCompiled), "replay emits nothing")
	var recorded string
	require.NoError(t, pool.QueryRow(ctx, `SELECT artifact_digest FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id)
		WHERE p.pack_ref=$1 AND v.version='2026.08.1'`, f.packRef).Scan(&recorded))
	assert.Equal(t, digest, recorded)

	// The artifact embeds the rule, its sources and the interpretation: reproducible without live rows.
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(mustString(art["artifact"])), &body))
	assert.Len(t, body["rule_modules"], 1)
	assert.Len(t, body["sources"], 1)
	assert.Len(t, body["interpretations"], 1)

	// Unsigned: verify fails closed (JUR-NEG-04) and emits a security event.
	code, res := e.do("POST", "/v1/packs/"+f.packRef+"/versions/2026.08.1/verify", "loader", nil)
	mustStatus(t, 409, code, res)
	assert.Contains(t, res["reasons"], domain.ReasonUnsigned)
	assert.Equal(t, 1, e.pub.count(events.EventPackVerificationFailed))

	// Signing with a key that is not registered is refused (the service never signs with an untrusted key).
	code, r = e.do("POST", f.path("2026.08.1", "/sign"), "signer", nil)
	mustStatus(t, 503, code, r)
	assert.Equal(t, "signing_key_not_active", r["error"])

	// Register the PUBLIC key, then sign.
	code, k := e.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": keyRef, "public_key": pubB64})
	mustStatus(t, 201, code, k)
	code, k = e.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": keyRef, "public_key": pubB64})
	mustStatus(t, 200, code, k)
	_, otherPub := newSigner(t, "x")
	code, r = e.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": keyRef, "public_key": otherPub})
	mustStatus(t, 409, code, r)
	assert.Equal(t, "key_mismatch", r["error"], "a key_ref names one key forever")

	code, signed := e.do("POST", f.path("2026.08.1", "/sign"), "signer", nil)
	mustStatus(t, 200, code, signed)
	assert.Equal(t, keyRef, signed["signature_key_ref"])
	sig1 := signed["signature"]
	code, signed2 := e.do("POST", f.path("2026.08.1", "/sign"), "signer", nil)
	mustStatus(t, 200, code, signed2)
	assert.Equal(t, sig1, signed2["signature"])
	assert.Equal(t, 1, e.pub.count(events.EventPackVersionSigned), "signing is write-once and emits once")

	code, res = e.do("POST", "/v1/packs/"+f.packRef+"/versions/2026.08.1/verify", "loader", nil)
	mustStatus(t, 200, code, res)
	assert.Equal(t, true, res["verified"])
	code, got := e.do("GET", "/v1/packs/"+f.packRef+"/versions/2026.08.1/artifact", "", nil)
	mustStatus(t, 200, code, got)
	assert.Equal(t, true, got["verification"].(map[string]any)["verified"])

	// Tampering with the stored bytes (JUR-NEG-03): the artifact table refuses it...
	_, err := pool.Exec(ctx, `UPDATE pack_artifacts SET artifact = replace(artifact, 'B2C', 'B2B') WHERE artifact_digest=$1`, digest)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM pack_artifacts WHERE artifact_digest=$1`, digest)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `UPDATE pack_artifacts SET signature='AAAA' WHERE artifact_digest=$1`, digest)
	require.Error(t, err, "signature is write-once")
	// ...and even an attacker who disables the guard is caught by the verifier.
	withoutGuards(t, "pack_artifacts", func() {
		_, err := pool.Exec(ctx, `UPDATE pack_artifacts SET artifact = replace(artifact, 'B2C', 'B2B') WHERE artifact_digest=$1`, digest)
		require.NoError(t, err)
		code, res := e.do("POST", "/v1/packs/"+f.packRef+"/versions/2026.08.1/verify", "loader", nil)
		mustStatus(t, 409, code, res)
		assert.Contains(t, res["reasons"], domain.ReasonDigestMismatch)
		_, err = pool.Exec(ctx, `UPDATE pack_artifacts SET artifact = replace(artifact, 'B2B', 'B2C') WHERE artifact_digest=$1`, digest)
		require.NoError(t, err)
	})
	code, res = e.do("POST", "/v1/packs/"+f.packRef+"/versions/2026.08.1/verify", "loader", nil)
	mustStatus(t, 200, code, res)

	// A forged signature fails.
	withoutGuards(t, "pack_artifacts", func() {
		forged := base64.StdEncoding.EncodeToString(make([]byte, 64))
		_, err := pool.Exec(ctx, `UPDATE pack_artifacts SET signature=$2 WHERE artifact_digest=$1`, digest, forged)
		require.NoError(t, err)
		code, res := e.do("POST", "/v1/packs/"+f.packRef+"/versions/2026.08.1/verify", "loader", nil)
		mustStatus(t, 409, code, res)
		assert.Contains(t, res["reasons"], domain.ReasonSignatureInvalid)
		_, err = pool.Exec(ctx, `UPDATE pack_artifacts SET signature=$2 WHERE artifact_digest=$1`, digest, sig1)
		require.NoError(t, err)
	})

	// Retired keys still verify; revoked keys do not; status only moves forward.
	code, r = e.do("POST", "/v1/admin/pack-signing-keys/"+keyRef+"/retire", "security", map[string]any{"reason": "rotation"})
	mustStatus(t, 200, code, r)
	code, res = e.do("POST", "/v1/packs/"+f.packRef+"/versions/2026.08.1/verify", "loader", nil)
	mustStatus(t, 200, code, res)
	code, r = e.do("POST", "/v1/admin/pack-signing-keys/"+keyRef+"/revoke", "security", map[string]any{"reason": "compromised"})
	mustStatus(t, 200, code, r)
	code, res = e.do("POST", "/v1/packs/"+f.packRef+"/versions/2026.08.1/verify", "loader", nil)
	mustStatus(t, 409, code, res)
	assert.Contains(t, res["reasons"], domain.ReasonKeyRevoked)
	code, r = e.do("POST", "/v1/admin/pack-signing-keys/"+keyRef+"/retire", "security", map[string]any{"reason": "undo"})
	mustStatus(t, 409, code, r)
	code, r = e.do("POST", "/v1/admin/pack-signing-keys/"+keyRef+"/revoke", "security", map[string]any{})
	mustStatus(t, 400, code, r)

	// Separate privileged capabilities (s28).
	for _, p := range []string{"jurisdiction_pack_version.compile", "jurisdiction_pack_version.sign", "jurisdiction_pack_version.verify",
		"pack_signing_key.register", "pack_signing_key.retire", "pack_signing_key.revoke"} {
		assert.True(t, e.auth.pairs[p], p)
	}
}

func mustString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func TestCompile_RejectsBrokenPacksAndStoresNothing(t *testing.T) {
	e := newEnv()
	f := newFixture(t, e)

	// A rule with no provenance at all.
	bare, _, err := st.CreateRule(ctx, domain.CreateRuleParams{JurisdictionID: f.jur.JurisdictionID, RuleDomain: "TAX", RuleCode: "BARE",
		RuleName: "bare", EffectiveFrom: time.Now().UTC().Add(-time.Hour), RulePayload: []byte(`{}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "seed"})
	require.NoError(t, err)
	f.review(t, e, "2026.08.1", bare.JurisdictionRuleID)
	code, r := e.do("POST", f.path("2026.08.1", "/compile"), "compiler", nil)
	mustStatus(t, 422, code, r)
	assert.Equal(t, "compile_failed", r["error"])
	var found []string
	for _, x := range r["report"].(map[string]any)["findings"].([]any) {
		found = append(found, x.(map[string]any)["code"].(string))
	}
	assert.Contains(t, found, "JUR-C023", "no regime")
	assert.Contains(t, found, "JUR-C024", "no source")
	assert.Contains(t, found, "JUR-C025", "no interpretation")
	n := 0
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM pack_artifacts a JOIN jurisdiction_pack_versions v USING (pack_version_id)
		JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref=$1`, f.packRef).Scan(&n))
	assert.Equal(t, 0, n, "a failed compile stores nothing")

	// Two overlapping rules with the same precedence (JUR-NEG-02).
	f2 := newFixture(t, e)
	from := time.Now().UTC().Add(-48 * time.Hour)
	a := f2.newRule(t, e, "DUP", from, nil, 5)
	b := f2.newRule(t, e, "DUP", from.Add(time.Hour), nil, 5)
	f2.review(t, e, "2026.08.1", a, b)
	code, r = e.do("POST", f2.path("2026.08.1", "/compile"), "compiler", nil)
	mustStatus(t, 422, code, r)
	assert.Contains(t, mustString(r["report"]), "JUR-C040")

	// Float arithmetic in rule content (JUR-NEG-25).
	f3 := newFixture(t, e)
	fl, _, err := st.CreateRule(ctx, domain.CreateRuleParams{JurisdictionID: f3.jur.JurisdictionID, RuleDomain: "TAX", RuleCode: "FLOAT",
		RuleName: "f", EffectiveFrom: time.Now().UTC().Add(-time.Hour), RulePayload: []byte(`{"rate":0.2}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "seed"})
	require.NoError(t, err)
	code, pr := e.do("PUT", "/v1/admin/rules/"+fl.JurisdictionRuleID+"/provenance", "author", map[string]any{
		"regime_id": f3.regimeID, "interpretation_id": f3.interpID, "source_ids": []string{f3.sourceID}})
	mustStatus(t, 200, code, pr)
	f3.review(t, e, "2026.08.1", fl.JurisdictionRuleID)
	code, r = e.do("POST", f3.path("2026.08.1", "/compile"), "compiler", nil)
	mustStatus(t, 422, code, r)
	assert.Contains(t, mustString(r["report"]), "JUR-C050")

	// An unknown rule module is refused already at draft time (the compiler's JUR-C020 is a second line of defence).
	f4 := newFixture(t, e)
	code, r = e.do("POST", "/v1/admin/packs/"+f4.packRef+"/versions", "author", f4.manifest("2026.08.1", uuid.NewString()))
	mustStatus(t, 400, code, r)
	assert.Equal(t, "invalid_manifest", r["error"])

	// Unknown pack version.
	code, _ = e.do("POST", f4.path("9.9.9", "/compile"), "compiler", nil)
	assert.Equal(t, 404, code)
	sg, _ := newSigner(t, "k"+strings.ToLower(uuid.NewString()[:5]))
	code, _ = newEnvWithSigner(sg).do("POST", f4.path("9.9.9", "/sign"), "signer", nil)
	assert.Equal(t, 404, code)
}

func TestCompile_ChangedInputsUnderSameVersionAreRefused(t *testing.T) {
	e := newEnv()
	f := newFixture(t, e)
	f.review(t, e, "2026.08.1", f.ruleID)
	code, r := e.do("POST", f.path("2026.08.1", "/compile"), "compiler", nil)
	mustStatus(t, 201, code, r)

	// The rule is still DRAFT, so its provenance can be edited. Doing so
	// changes what the artifact would contain: recompiling the SAME version
	// must be refused (JUR-NEG-20); the stored artifact is untouched.
	code, r = e.do("PUT", "/v1/admin/rules/"+f.ruleID+"/provenance", "author", map[string]any{
		"regime_id": f.regimeID, "interpretation_id": f.interpID, "precedence": 99, "source_ids": []string{f.sourceID}})
	mustStatus(t, 200, code, r)
	code, r = e.do("POST", f.path("2026.08.1", "/compile"), "compiler", nil)
	mustStatus(t, 409, code, r)
	assert.Equal(t, "already_compiled", r["error"])
}

func TestSign_FailsClosedWithoutASigner(t *testing.T) {
	e := newEnv() // no signer wired
	f := newFixture(t, e)
	f.review(t, e, "2026.08.1", f.ruleID)
	code, r := e.do("POST", f.path("2026.08.1", "/compile"), "compiler", nil)
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", f.path("2026.08.1", "/sign"), "signer", nil)
	mustStatus(t, 503, code, r)
	assert.Equal(t, "signing_not_configured", r["error"])

	// Signing before compiling is a precise error, not a crash.
	f2 := newFixture(t, e)
	signer, _ := newSigner(t, "k"+strings.ToLower(uuid.NewString()[:5]))
	e2 := newEnvWithSigner(signer)
	f2.review(t, e2, "2026.08.1", f2.ruleID)
	code, r = e2.do("POST", f2.path("2026.08.1", "/sign"), "signer", nil)
	mustStatus(t, 404, code, r)
	assert.Equal(t, "not_compiled", r["error"])

	// A version with no artifact fails verification closed.
	code, r = e2.do("POST", "/v1/packs/"+f2.packRef+"/versions/2026.08.1/verify", "loader", nil)
	mustStatus(t, 409, code, r)
}

func TestSign_RefusesAKeyWhoseRegisteredPublicKeyDiffers(t *testing.T) {
	keyRef := "swap-" + strings.ToLower(uuid.NewString()[:6])
	signer, _ := newSigner(t, keyRef)
	e := newEnvWithSigner(signer)
	f := newFixture(t, e)
	f.review(t, e, "2026.08.1", f.ruleID)
	code, r := e.do("POST", f.path("2026.08.1", "/compile"), "compiler", nil)
	mustStatus(t, 201, code, r)

	// The registry trusts a DIFFERENT key under this key_ref: the service must not sign.
	_, otherPub := newSigner(t, "other")
	code, r = e.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": keyRef, "public_key": otherPub})
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", f.path("2026.08.1", "/sign"), "signer", nil)
	mustStatus(t, 503, code, r)
	assert.Equal(t, "signing_key_not_active", r["error"])
}

func TestArtifactAndKeyTables_AreGuardedInSQL(t *testing.T) {
	e := newEnv()
	f := newFixture(t, e)
	// Artifact cannot be created for a DRAFT version, nor pre-signed.
	code, v := e.do("POST", "/v1/admin/packs/"+f.packRef+"/versions", "author", f.manifest("2026.08.1", f.ruleID))
	mustStatus(t, 201, code, v)
	_, err := pool.Exec(ctx, `INSERT INTO pack_artifacts (pack_version_id, artifact, artifact_digest, compiler_name, compiler_version, report, compiled_by_principal_id)
		SELECT pack_version_id, '{}', 'sha256:'||repeat('a',64), 'x', '1', '{}'::jsonb, 'attacker' FROM jurisdiction_pack_versions WHERE manifest_digest=$1`,
		v["manifest_digest"])
	require.Error(t, err, "no artifact for a version that is not under REVIEW")

	// Key material is immutable and keys are never deleted.
	_, pub := newSigner(t, "z")
	keyRef := "guard-" + strings.ToLower(uuid.NewString()[:6])
	code, k := e.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": keyRef, "public_key": pub})
	mustStatus(t, 201, code, k)
	_, err = pool.Exec(ctx, `UPDATE pack_signing_keys SET public_key=decode(repeat('00',32),'hex') WHERE key_ref=$1`, keyRef)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM pack_signing_keys WHERE key_ref=$1`, keyRef)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `UPDATE pack_signing_keys SET status='ACTIVE' WHERE key_ref=$1 AND status='ACTIVE'`, keyRef)
	require.NoError(t, err)

	// Registration validates shape and never accepts a private key.
	code, r := e.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": "BAD REF", "public_key": pub})
	mustStatus(t, 400, code, r)
	code, r = e.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": "k-" + keyRef, "public_key": base64.StdEncoding.EncodeToString(make([]byte, 64))})
	mustStatus(t, 400, code, r)
	code, r = e.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": "k2-" + keyRef, "public_key": pub, "algorithm": "RSA"})
	mustStatus(t, 400, code, r)
	code, r = e.do("GET", "/v1/pack-signing-keys", "", nil)
	mustStatus(t, 200, code, r)
	assert.NotContains(t, mustString(r), "private", "keys listing exposes public material only")
}
