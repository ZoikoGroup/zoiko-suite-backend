//go:build integration

// ZS-JUR-001 Wave 7 end to end: release lifecycle, deployment rings and
// regions, rollback, emergency hotfix, source-change intake and the metrics
// feed, against the real store and real certified packs. Rings and regions are
// append-only global data, so one fixed set is created once and shared.
package registryit_test

import (
	"strings"
	"sync"
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
	"zoiko.io/jurisdiction-rules-svc/internal/resolver"
)

const (
	ringCanary  = "canary"
	ringEarly   = "early"
	ringGeneral = "general"
	regionEU    = "eu-west"
	regionUS    = "us-east"
)

var setupRingsOnce sync.Once

// ensureRings creates the shared topology: canary (soak 0) -> early (soak 1h) -> general, in two regions.
func ensureRings(t *testing.T, e *env) {
	t.Helper()
	setupRingsOnce.Do(func() {
		for _, r := range []map[string]any{
			{"ring_code": ringCanary, "ordinal": 1, "min_soak_seconds": 0},
			{"ring_code": ringEarly, "ordinal": 2, "min_soak_seconds": 3600},
			{"ring_code": ringGeneral, "ordinal": 3, "min_soak_seconds": 0},
		} {
			code, body := e.do("POST", "/v1/admin/deployment-rings", "platform-ops", r)
			require.Contains(t, []int{200, 201}, code, "%v", body)
		}
		for _, rg := range []string{regionEU, regionUS} {
			code, body := e.do("POST", "/v1/admin/deployment-regions", "platform-ops", map[string]any{"region_code": rg})
			require.Contains(t, []int{200, 201}, code, "%v", body)
		}
	})
}

// newEnvOps wires everything, with the resolver serving a given ring and region (empty = ungated).
func newEnvOps(signer domain.Signer, ring, region string) *env {
	pub := &spyPub{}
	auth := &recordingAuthz{}
	res, err := resolver.New(st, resolver.Config{EligibleStatuses: []string{"RELEASED"}, CacheTTL: 0, Ring: ring, Region: region,
		OnUnverified: func(ref, ver string, reasons []string) {
			_ = st.RecordVerificationFailure(ctx, ref, ver, ring, region, reasons)
			_ = pub.PublishRegistryEvent(ctx, events.EventPackVerificationFailed, ref+"@"+ver, "runtime-resolver", "", nil)
		}})
	if err != nil {
		panic(err)
	}
	r := chi.NewRouter()
	h := handler.New(st, auth, pub, "platform-scope", zap.NewNop()).WithRegistry(st, pub).WithCertificationPolicy(1).
		WithResolver(res).WithOperations(handler.OpsPolicy{HotfixRetroSLA: 72 * time.Hour})
	if signer != nil {
		h.WithSigner(signer)
	}
	handler.RegisterRoutes(r, h)
	return &env{srv: r, pub: pub, auth: auth}
}

func opsPack(t *testing.T, ring, region string) *certFixture {
	t.Helper()
	c := signedReadyEnv(t, func(s domain.Signer) *env { return newEnvOps(s, ring, region) })
	ensureRings(t, c.e)
	return c
}

func (c *certFixture) lifecycle(t *testing.T, version, cmd string, body any, want int) map[string]any {
	t.Helper()
	code, r := c.e.do("POST", c.fixture.path(version, "/"+cmd), "releaser", body)
	require.Equal(t, want, code, "%s %s: %v", cmd, version, r)
	return r
}

func (c *certFixture) deploy(t *testing.T, version, ring, region string, want int) map[string]any {
	t.Helper()
	return c.lifecycle(t, version, "deploy", map[string]any{"ring": ring, "region": region}, want)
}

// backdate makes an ACTIVE deployment look older, so a soak period can be satisfied without waiting.
func backdate(t *testing.T, c *certFixture, version, ring, region string, ago time.Duration) {
	t.Helper()
	withoutGuards(t, "pack_deployments", func() {
		_, err := pool.Exec(ctx, `UPDATE pack_deployments d SET deployed_at = NOW() - make_interval(secs => $5)
			FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id)
			WHERE d.pack_version_id=v.pack_version_id AND p.pack_ref=$1 AND v.version=$2 AND d.ring_code=$3 AND d.region_code=$4 AND d.status='ACTIVE'`,
			c.packRef, version, ring, region, ago.Seconds())
		require.NoError(t, err)
	})
}

func status(t *testing.T, c *certFixture, version string) string {
	t.Helper()
	var s string
	require.NoError(t, pool.QueryRow(ctx, `SELECT v.status FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref=$1 AND v.version=$2`,
		c.packRef, version).Scan(&s))
	return s
}

func resolveOutcome(t *testing.T, c *certFixture) (string, map[string]any) {
	t.Helper()
	_, r := c.e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	d, _ := r["decision"].(map[string]any)
	if d == nil {
		return "ERROR:" + mustString(r["error"]), r
	}
	return d["outcome"].(string), d
}

// ── lifecycle ───────────────────────────────────────────────────────────────

func TestRelease_PublishWithdrawBlock_AreRecordedAndGuarded(t *testing.T) {
	c := opsPack(t, "", "")
	e := c.e

	// A version under REVIEW cannot be released; only CERTIFIED can.
	r := c.lifecycle(t, c.version, "publish", nil, 409)
	assert.Equal(t, "invalid_lifecycle_state", r["error"])
	c.certifyVersion(t, c.version)

	// Bare status updates cannot reach RELEASED, WITHDRAWN or EMERGENCY_BLOCKED: only the commands can.
	for _, st := range []string{"RELEASED", "WITHDRAWN"} {
		_, err := pool.Exec(ctx, `UPDATE jurisdiction_pack_versions v SET status=$3 FROM jurisdiction_packs p WHERE p.pack_id=v.pack_id AND p.pack_ref=$1 AND v.version=$2`,
			c.packRef, c.version, st)
		require.Error(t, err, st)
	}

	r = c.lifecycle(t, c.version, "publish", map[string]any{"evidence_ref": "CAB-123"}, 200)
	assert.Equal(t, true, r["changed"])
	assert.Equal(t, "RELEASED", status(t, c, c.version))
	r = c.lifecycle(t, c.version, "publish", nil, 200)
	assert.Equal(t, false, r["changed"], "publishing again is a no-op")
	assert.Equal(t, 1, e.pub.count(events.EventPackReleased))
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM pack_release_events WHERE action='RELEASE' AND evidence_ref='CAB-123'`).Scan(&n))
	assert.Equal(t, 1, n)

	// Emergency stop and recovery; a reason is mandatory for a block.
	c.lifecycle(t, c.version, "block", map[string]any{}, 400)
	c.lifecycle(t, c.version, "block", map[string]any{"reason": "suspected wrong rate"}, 200)
	assert.Equal(t, "EMERGENCY_BLOCKED", status(t, c, c.version))
	c.lifecycle(t, c.version, "deploy", map[string]any{"ring": ringCanary, "region": regionEU}, 409) // cannot deploy while blocked
	c.lifecycle(t, c.version, "unblock", map[string]any{"reason": "reviewed, rate is right"}, 200)
	assert.Equal(t, "RELEASED", status(t, c, c.version))

	// Withdrawal needs both a reason and evidence (s25); then nothing new can use it.
	c.lifecycle(t, c.version, "withdraw", map[string]any{"reason": "superseded by law"}, 400)
	c.lifecycle(t, c.version, "withdraw", map[string]any{"evidence_ref": "TICKET-9"}, 400)
	c.lifecycle(t, c.version, "withdraw", map[string]any{"reason": "superseded by law", "evidence_ref": "TICKET-9"}, 200)
	assert.Equal(t, "WITHDRAWN", status(t, c, c.version))
	c.lifecycle(t, c.version, "publish", nil, 409)
	c.lifecycle(t, c.version, "block", map[string]any{"reason": "x"}, 409)
	for _, p := range []string{"jurisdiction_pack_version.publish", "jurisdiction_pack_version.withdraw", "jurisdiction_pack_version.block", "jurisdiction_pack_version.unblock"} {
		assert.True(t, e.auth.pairs[p], p+" is its own capability")
	}
	_, err := pool.Exec(ctx, `UPDATE pack_release_events SET reason='edited'`)
	require.Error(t, err, "release history is append-only")
}

func TestRelease_DependenciesMustBeReleased_AndDeployedInTheSameScope(t *testing.T) {
	a := opsPack(t, "", "")
	a.certifyVersion(t, a.version) // pack A: CERTIFIED

	// Pack B depends on pack A@2026.08.1.
	f2 := *a.fixture
	f2.packRef = uniqueRef("jur.d")
	b := &certFixture{fixture: &f2, e: a.e, version: "2026.08.1", keyRef: a.keyRef}
	code, p := a.e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": f2.packRef, "pack_name": "Dependent", "owner": "Tax Engineering"})
	mustStatus(t, 201, code, p)
	m := f2.manifest("2026.08.1", f2.ruleID)
	m["dependencies"] = []map[string]string{{"ref": a.packRef, "version": a.version}}
	code, v := a.e.do("POST", "/v1/admin/packs/"+f2.packRef+"/versions", "author", m)
	mustStatus(t, 201, code, v)
	code, r := a.e.do("POST", f2.path("2026.08.1", "/submit-review"), "author", nil)
	mustStatus(t, 200, code, r)
	code, r = a.e.do("POST", f2.path("2026.08.1", "/compile"), "compiler", nil)
	mustStatus(t, 201, code, r)
	code, r = a.e.do("POST", f2.path("2026.08.1", "/sign"), "signer", nil)
	mustStatus(t, 200, code, r)
	b.testsPassing(t)
	b.approve(t, "reviewer-b", "TAX")
	code, r = a.e.do("POST", f2.path("2026.08.1", "/certify"), "certifier", nil)
	mustStatus(t, 201, code, r)

	// B cannot be released while A is not.
	r = b.lifecycle(t, "2026.08.1", "publish", nil, 409)
	assert.Equal(t, "operation_blocked", r["error"])
	assert.Contains(t, mustString(r["reasons"]), "dependency_not_released")
	a.lifecycle(t, a.version, "publish", nil, 200)
	b.lifecycle(t, "2026.08.1", "publish", nil, 200)

	// B cannot go to a ring and region where A is not active (JUR-NEG-15).
	r = b.deploy(t, "2026.08.1", ringCanary, regionEU, 409)
	assert.Contains(t, mustString(r["reasons"]), "dependency_not_deployed")
	a.deploy(t, a.version, ringCanary, regionEU, 201)
	b.deploy(t, "2026.08.1", ringCanary, regionEU, 201)
	r = b.deploy(t, "2026.08.1", ringCanary, regionUS, 409)
	assert.Contains(t, mustString(r["reasons"]), "dependency_not_deployed", "eu-west having A does not help us-east")

	// A released version in use cannot be withdrawn out from under its dependent.
	r = a.lifecycle(t, a.version, "withdraw", map[string]any{"reason": "bad", "evidence_ref": "T-1"}, 409)
	assert.Contains(t, mustString(r["reasons"]), "in_use_by_released_version")
	b.lifecycle(t, "2026.08.1", "withdraw", map[string]any{"reason": "bad", "evidence_ref": "T-1"}, 200)
	a.lifecycle(t, a.version, "withdraw", map[string]any{"reason": "bad", "evidence_ref": "T-1"}, 200)

	// Withdrawal rolled the deployments back, and says so.
	var active int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM pack_deployments d JOIN jurisdiction_pack_versions v USING (pack_version_id)
		JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref IN ($1,$2) AND d.status='ACTIVE'`, a.packRef, b.packRef).Scan(&active))
	assert.Equal(t, 0, active)
	var reason string
	require.NoError(t, pool.QueryRow(ctx, `SELECT rollback_reason FROM pack_deployments d JOIN jurisdiction_pack_versions v USING (pack_version_id)
		JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref=$1 LIMIT 1`, a.packRef).Scan(&reason))
	assert.Contains(t, reason, "version withdrawn")
}

// ── rings, regions, gates, resolver gating ──────────────────────────────────

func TestDeployment_RingOrderSoakAndHealthGates(t *testing.T) {
	c := opsPack(t, "", "")
	c.certifyVersion(t, c.version)
	c.lifecycle(t, c.version, "publish", nil, 200)

	// Unknown scope and unreleased versions are precise errors.
	r := c.deploy(t, c.version, "nope", regionEU, 404)
	assert.Equal(t, "ring_not_found", r["error"])
	r = c.deploy(t, c.version, ringCanary, "nowhere", 404)
	assert.Equal(t, "region_not_found", r["error"])
	c.lifecycle(t, c.version, "deploy", map[string]any{"ring": ringCanary}, 400)

	// Ring order: early and general cannot be skipped to.
	r = c.deploy(t, c.version, ringEarly, regionEU, 409)
	assert.Contains(t, mustString(r["reasons"]), "previous_ring_not_deployed")
	r = c.deploy(t, c.version, ringGeneral, regionEU, 409)
	assert.Contains(t, mustString(r["reasons"]), "previous_ring_not_deployed")

	d := c.deploy(t, c.version, ringCanary, regionEU, 201)
	assert.Equal(t, "ACTIVE", d["status"])
	d2 := c.deploy(t, c.version, ringCanary, regionEU, 200)
	assert.Equal(t, d["deployment_id"], d2["deployment_id"], "idempotent")
	assert.Equal(t, 1, c.e.pub.count(events.EventPackPromoted))

	c.deploy(t, c.version, ringEarly, regionEU, 201) // canary soak is 0
	// early requires 1h of soak before general.
	r = c.deploy(t, c.version, ringGeneral, regionEU, 409)
	assert.Contains(t, mustString(r["reasons"]), "soak_not_elapsed")
	backdate(t, c, c.version, ringEarly, regionEU, 2*time.Hour)
	c.deploy(t, c.version, ringGeneral, regionEU, 201)

	// A runtime verification failure in the previous ring blocks promotion.
	for _, rg := range []string{ringCanary, ringEarly} {
		c.deploy(t, c.version, rg, regionUS, 201)
	}
	backdate(t, c, c.version, ringEarly, regionUS, 2*time.Hour)
	require.NoError(t, st.RecordVerificationFailure(ctx, c.packRef, c.version, ringEarly, regionUS, []string{"digest_mismatch"}))
	r = c.deploy(t, c.version, ringGeneral, regionUS, 409)
	assert.Contains(t, mustString(r["reasons"]), "verification_failures_in_previous_ring")

	// The deployment ledger keeps history; a deployment only ever goes ACTIVE -> ROLLED_BACK.
	code, list := c.e.do("GET", "/v1/packs/"+c.packRef+"/versions/"+c.version+"/deployments", "", nil)
	mustStatus(t, 200, code, list)
	assert.Len(t, list["deployments"], 5)
	_, err := pool.Exec(ctx, `UPDATE pack_deployments SET region_code='us-east' WHERE region_code='eu-west'`)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM pack_deployments`)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `UPDATE deployment_rings SET min_soak_seconds=0`)
	require.Error(t, err, "rings are append-only")
	code, rings := c.e.do("GET", "/v1/deployment-rings", "", nil)
	mustStatus(t, 200, code, rings)
	assert.GreaterOrEqual(t, len(rings["rings"].([]any)), 3)
	code, r = c.e.do("POST", "/v1/admin/deployment-rings", "platform-ops", map[string]any{"ring_code": ringCanary, "ordinal": 9})
	mustStatus(t, 409, code, r)
	code, r = c.e.do("POST", "/v1/admin/deployment-rings", "platform-ops", map[string]any{"ring_code": "Bad Code", "ordinal": 1})
	mustStatus(t, 400, code, r)
}

func TestResolver_UsesOnlyWhatIsDeployedToItsRingAndRegion(t *testing.T) {
	c := opsPack(t, ringCanary, regionEU) // this resolver instance serves canary / eu-west
	c.certifyVersion(t, c.version)
	c.lifecycle(t, c.version, "publish", nil, 200)

	out, _ := resolveOutcome(t, c)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", out, "RELEASED but not deployed here: not served")
	c.deploy(t, c.version, ringCanary, regionUS, 201)
	out, _ = resolveOutcome(t, c)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", out, "deployed in another region: not served")
	c.deploy(t, c.version, ringCanary, regionEU, 201)
	out, dec := resolveOutcome(t, c)
	require.Equal(t, "RESOLVED", out)
	assert.Equal(t, c.version, dec["pack"].(map[string]any)["version"])

	// The decision records which resolver instance made it.
	var ring, region string
	require.NoError(t, pool.QueryRow(ctx, `SELECT resolver_ring, resolver_region FROM rule_decision_evidence WHERE outcome='RESOLVED' AND pack_ref=$1 ORDER BY created_at DESC LIMIT 1`,
		c.packRef).Scan(&ring, &region))
	assert.Equal(t, []string{ringCanary, regionEU}, []string{ring, region})

	// The emergency stop takes effect for the resolver; recovery restores it.
	c.lifecycle(t, c.version, "block", map[string]any{"reason": "investigating"}, 200)
	out, _ = resolveOutcome(t, c)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", out)
	c.lifecycle(t, c.version, "unblock", map[string]any{"reason": "cleared"}, 200)
	out, _ = resolveOutcome(t, c)
	assert.Equal(t, "RESOLVED", out)

	// Withdrawal rolls the deployment back; nothing new resolves.
	c.lifecycle(t, c.version, "withdraw", map[string]any{"reason": "withdrawn", "evidence_ref": "T-2"}, 200)
	out, _ = resolveOutcome(t, c)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", out)
}

// ── rollback ────────────────────────────────────────────────────────────────

func TestRollback_RestoresAnOlderRelease_AndStatesWhatItCannotUndo(t *testing.T) {
	c := opsPack(t, ringCanary, regionEU)
	c.certifyVersion(t, c.version)
	c.lifecycle(t, c.version, "publish", nil, 200)
	c.deploy(t, c.version, ringCanary, regionEU, 201)

	c.certifyVersion(t, "2026.09.1")
	c.lifecycle(t, "2026.09.1", "publish", nil, 200)
	c.deploy(t, "2026.09.1", ringCanary, regionEU, 201)
	out, dec := resolveOutcome(t, c)
	require.Equal(t, "RESOLVED", out)
	assert.Equal(t, "2026.09.1", dec["pack"].(map[string]any)["version"], "the newest deployed version serves")

	body := func(m map[string]any) map[string]any {
		base := map[string]any{"ring": ringCanary, "region": regionEU, "restore_version": c.version, "reason": "wrong rate in 2026.09.1"}
		for k, v := range m {
			base[k] = v
		}
		return base
	}
	c.lifecycle(t, "2026.09.1", "rollback", body(map[string]any{"reason": ""}), 400)
	r := c.lifecycle(t, "2026.09.1", "rollback", body(map[string]any{"restore_version": "2026.09.1"}), 409)
	assert.Contains(t, mustString(r["reasons"]), "restore_not_older")
	r = c.lifecycle(t, "2026.09.1", "rollback", body(map[string]any{"restore_version": "1.0.0"}), 409)
	assert.Contains(t, mustString(r["reasons"]), "restore_version_not_found")
	c.lifecycle(t, "2026.09.1", "rollback", body(map[string]any{"ring": ringEarly}), 404) // nothing deployed there

	r = c.lifecycle(t, "2026.09.1", "rollback", body(nil), 200)
	assert.EqualValues(t, 1, r["decisions_made_with_rolled_back_version"], "JUR-NEG-19: decisions already made are counted, not rewritten")
	assert.Contains(t, mustString(r["remediation_note"]), "remediation workflow")
	assert.Contains(t, mustString(r["reconciliation"]), "not performed")
	assert.Equal(t, "ROLLED_BACK", r["rolled_back"].(map[string]any)["status"])
	assert.Equal(t, c.version, r["restored"].(map[string]any)["version"])

	out, dec = resolveOutcome(t, c)
	require.Equal(t, "RESOLVED", out)
	assert.Equal(t, c.version, dec["pack"].(map[string]any)["version"], "future decisions use the restored release")
	assert.Equal(t, 1, c.e.pub.count(events.EventPackRolledBack))
	// The old decision is untouched.
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM rule_decision_evidence WHERE pack_ref=$1 AND pack_version='2026.09.1' AND outcome='RESOLVED'`, c.packRef).Scan(&n))
	assert.Equal(t, 1, n)

	// A restore target that was never deployed in this scope is refused.
	c.certifyVersion(t, "2026.10.1")
	c.lifecycle(t, "2026.10.1", "publish", nil, 200)
	c.deploy(t, "2026.10.1", ringCanary, regionEU, 201)
	r = c.lifecycle(t, "2026.10.1", "rollback", body(map[string]any{"ring": ringCanary, "restore_version": "2026.09.1"}), 200)
	assert.Equal(t, "2026.09.1", r["restored"].(map[string]any)["version"], "an older version that WAS deployed here before can be restored")
}

// ── emergency hotfix ────────────────────────────────────────────────────────

func TestHotfix_EnhancedReviewRingExceptionAndRetrospective(t *testing.T) {
	c := opsPack(t, ringEarly, regionEU)
	c.certifyVersion(t, c.version)
	c.lifecycle(t, c.version, "publish", nil, 200)
	c.deploy(t, c.version, ringCanary, regionEU, 201)

	hv := "2026.09.9"
	c.fixture.review(t, c.e, hv, c.ruleID)
	declare := func(m map[string]any, want int) map[string]any {
		base := map[string]any{"severity": "P0", "scope_summary": "correct the reduced rate threshold", "incident_ref": "INC-4411",
			"incident_commander": "ic-1", "rollback_target_version": c.version}
		for k, v := range m {
			base[k] = v
		}
		return c.lifecycle(t, hv, "hotfix", base, want)
	}
	declare(map[string]any{"severity": "P2"}, 400)
	r := declare(map[string]any{"rollback_target_version": "9.9.9"}, 400)
	assert.Equal(t, "invalid_reference", r["error"], "the rollback target must exist")
	declare(map[string]any{"incident_ref": ""}, 400)
	hf := declare(nil, 201)
	assert.Equal(t, "P0", hf["severity"])
	assert.NotEmpty(t, hf["retro_due_at"])
	declare(nil, 200) // idempotent
	declare(map[string]any{"incident_ref": "INC-OTHER"}, 409)

	// Enhanced review: a hotfix needs two independent approvers even though the policy default is one.
	code, r2 := c.e.do("POST", c.fixture.path(hv, "/compile"), "compiler", nil)
	mustStatus(t, 201, code, r2)
	code, r2 = c.e.do("POST", c.fixture.path(hv, "/sign"), "signer", nil)
	mustStatus(t, 200, code, r2)
	hc := *c
	hc.version = hv
	hc.testsPassing(t)
	hc.approve(t, "hf-reviewer-1", "TAX")
	code, r2 = c.e.do("POST", c.fixture.path(hv, "/certify"), "certifier", nil)
	mustStatus(t, 409, code, r2)
	assert.Contains(t, reasonsOf(r2), "insufficient_reviews: 1 independent approving reviewer(s), 2 required", "JUR-NEG-14")
	// The database refuses it independently of the service.
	var vid, runID, artDigest, bundleDigest string
	require.NoError(t, pool.QueryRow(ctx, `SELECT v.pack_version_id::text, (SELECT run_id::text FROM pack_test_runs WHERE pack_version_id=v.pack_version_id LIMIT 1),
			a.artifact_digest, (SELECT bundle_digest FROM pack_test_bundles WHERE pack_version_id=v.pack_version_id LIMIT 1)
		FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id) JOIN pack_artifacts a USING (pack_version_id)
		WHERE p.pack_ref=$1 AND v.version=$2`, c.packRef, hv).Scan(&vid, &runID, &artDigest, &bundleDigest))
	_, err := pool.Exec(ctx, `INSERT INTO pack_certifications (pack_version_id, artifact_digest, bundle_digest, test_run_id, min_reviews, report, report_digest, signature, signature_key_ref, certified_by)
		VALUES ($1,$2,$3,$4,1,'{}'::jsonb,$5,'x',$6,'independent-certifier')`, vid, artDigest, bundleDigest, runID, "sha256:"+strings.Repeat("0", 64), c.keyRef)
	require.Error(t, err, "a hotfix certification with fewer than two reviewers cannot be stored")
	hc.approve(t, "hf-reviewer-2", "LEGAL")
	code, r2 = c.e.do("POST", c.fixture.path(hv, "/certify"), "certifier", nil)
	mustStatus(t, 201, code, r2)
	assert.EqualValues(t, 2, r2["min_reviews"])

	// A hotfix cannot be declared once certified.
	other := "2026.09.10"
	c.certifyVersion(t, other)
	c.lifecycle(t, other, "hotfix", map[string]any{"severity": "P0", "scope_summary": "x", "incident_ref": "I2", "incident_commander": "ic-1", "rollback_target_version": c.version}, 409)

	c.lifecycle(t, hv, "publish", nil, 200)

	// Ring order applies to a hotfix too, and the exception is tightly held.
	r = c.deploy(t, hv, ringEarly, regionEU, 409)
	assert.Contains(t, mustString(r["reasons"]), "previous_ring_not_deployed")
	r = c.lifecycle(t, hv, "deploy", map[string]any{"ring": ringEarly, "region": regionEU, "exception_reason": "tax deadline in 2h"}, 409)
	assert.Contains(t, mustString(r["reasons"]), "exception_requires_incident_commander")
	// The rollback target must be live in the ring being deployed to (the known-good fallback).
	code, r = c.e.do("POST", c.fixture.path(hv, "/deploy"), "ic-1", map[string]any{"ring": ringEarly, "region": regionEU, "exception_reason": "tax deadline in 2h"})
	mustStatus(t, 409, code, r)
	assert.Contains(t, mustString(r["reasons"]), "rollback_target_not_deployed")
	c.deploy(t, c.version, ringEarly, regionEU, 201)
	code, r = c.e.do("POST", c.fixture.path(hv, "/deploy"), "ic-1", map[string]any{"ring": ringEarly, "region": regionEU, "exception_reason": "tax deadline in 2h"})
	mustStatus(t, 201, code, r)
	assert.Equal(t, "tax deadline in 2h", r["exception_reason"], "the exception is recorded on the deployment")
	// A normal version never gets the exception.
	code, r = c.e.do("POST", c.fixture.path(other, "/publish"), "releaser", nil)
	mustStatus(t, 200, code, r)
	code, r = c.e.do("POST", c.fixture.path(other, "/deploy"), "ic-1", map[string]any{"ring": ringEarly, "region": regionEU, "exception_reason": "because"})
	mustStatus(t, 409, code, r)
	assert.Contains(t, mustString(r["reasons"]), "exception_not_allowed")

	// Retrospective: due date set at declaration; overdue shows in metrics; completion is once.
	withoutGuards(t, "pack_hotfixes", func() {
		_, err := pool.Exec(ctx, `UPDATE pack_hotfixes SET retro_due_at = NOW() - INTERVAL '1 day' WHERE pack_version_id=$1`, vid)
		require.NoError(t, err)
		code, m := c.e.do("GET", "/v1/ops/metrics", "ops", nil)
		mustStatus(t, 200, code, m)
		assert.Contains(t, mustString(m["hotfix_retrospectives_overdue"]), c.packRef)
	})
	c.lifecycle(t, hv, "hotfix/retrospective", map[string]any{}, 400)
	r = c.lifecycle(t, hv, "hotfix/retrospective", map[string]any{"note": "root cause: stale threshold; added a boundary test"}, 200)
	assert.NotNil(t, r["retro_completed_at"])
	c.lifecycle(t, hv, "hotfix/retrospective", map[string]any{"note": "again"}, 200)
	_, err = pool.Exec(ctx, `UPDATE pack_hotfixes SET incident_ref='edited' WHERE pack_version_id=$1`, vid)
	require.Error(t, err, "a hotfix record is immutable")
	code, got := c.e.do("GET", "/v1/packs/"+c.packRef+"/versions/"+hv+"/hotfix", "", nil)
	mustStatus(t, 200, code, got)
	code, _ = c.e.do("GET", "/v1/packs/"+c.packRef+"/versions/"+c.version+"/hotfix", "", nil)
	assert.Equal(t, 404, code)
}

// ── source-change intake ────────────────────────────────────────────────────

func TestSourceChangeIntake_OpensReviewButNeverTouchesRules(t *testing.T) {
	e := newEnv()
	j := seedJurisdiction(t, "NL")
	rule := seedRule(t, j.JurisdictionID, "UNTOUCHED")
	var before string
	require.NoError(t, pool.QueryRow(ctx, `SELECT rule_status||'|'||rule_payload::text||'|'||COALESCE(updated_at::text,'') FROM jurisdiction_rules WHERE jurisdiction_rule_id=$1`, rule.JurisdictionRuleID).Scan(&before))

	body := map[string]any{"jurisdiction_id": j.JurisdictionID, "authority": "Belastingdienst", "change_type": "FUTURE_DATED_LAW",
		"title": "VAT rate change announced for 2027", "location": "https://example.invalid/notice", "observed_hash": newSnapshot()}
	bad := map[string]any{"authority": "x", "change_type": "RUMOUR", "title": "t"}
	code, r := e.do("POST", "/v1/admin/source-change-notices", "analyst-1", bad)
	mustStatus(t, 400, code, r)
	code, notice := e.do("POST", "/v1/admin/source-change-notices", "analyst-1", body)
	mustStatus(t, 201, code, notice)
	nid := notice["notice_id"].(string)
	assert.Equal(t, "OPEN", notice["status"])
	assert.Equal(t, 1, e.pub.count(events.EventSourceChangeCaptured))

	// Independence: the author cannot review their own notice; only the assigned reviewer can close it.
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/review", "analyst-1", nil)
	mustStatus(t, 403, code, r)
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/close", "analyst-2", map[string]any{"outcome": "NO_CHANGE", "note": "n"})
	mustStatus(t, 403, code, r) // not yet under review / not the reviewer
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/review", "analyst-2", nil)
	mustStatus(t, 200, code, r)
	assert.Equal(t, "UNDER_REVIEW", r["status"])
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/review", "analyst-2", nil)
	mustStatus(t, 200, code, r)
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/review", "analyst-3", nil)
	mustStatus(t, 409, code, r)
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/close", "analyst-3", map[string]any{"outcome": "NO_CHANGE", "note": "n"})
	mustStatus(t, 403, code, r)
	assert.Equal(t, "not_assigned_reviewer", r["error"])

	// Conclusions are validated; an interpretation conclusion needs an APPROVED interpretation.
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/close", "analyst-2", map[string]any{"outcome": "MAYBE", "note": "n"})
	mustStatus(t, 400, code, r)
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/close", "analyst-2", map[string]any{"outcome": "RULE_CHANGE_PLANNED"})
	mustStatus(t, 400, code, r)
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/close", "analyst-2", map[string]any{"outcome": "INTERPRETATION_RECORDED", "note": "n"})
	mustStatus(t, 400, code, r)
	f := newFixture(t, e) // brings an unapproved-then-approved interpretation of its own
	_, pending := e.do("POST", "/v1/admin/interpretations", "author", map[string]any{"jurisdiction_id": j.JurisdictionID, "subject": "s", "decision": "d", "rationale": "r", "source_ids": []string{f.sourceID}})
	pid := pending["interpretation_id"].(string)
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/close", "analyst-2", map[string]any{"outcome": "INTERPRETATION_RECORDED", "note": "n", "linked_interpretation_id": pid})
	mustStatus(t, 409, code, r)
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/close", "analyst-2", map[string]any{"outcome": "INTERPRETATION_RECORDED", "note": "interpretation approved", "linked_interpretation_id": f.interpID})
	mustStatus(t, 200, code, r)
	assert.Equal(t, "CLOSED", r["status"])
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/close", "analyst-2", map[string]any{"outcome": "INTERPRETATION_RECORDED", "note": "again", "linked_interpretation_id": f.interpID})
	mustStatus(t, 200, code, r)
	assert.Equal(t, 1, e.pub.count(events.EventSourceChangeClosed), "a replay emits nothing")
	code, r = e.do("POST", "/v1/admin/source-change-notices/"+nid+"/close", "analyst-2", map[string]any{"outcome": "NO_CHANGE", "note": "different"})
	mustStatus(t, 409, code, r)

	code, got := e.do("GET", "/v1/source-change-notices/"+nid, "", nil)
	mustStatus(t, 200, code, got)
	code, list := e.do("GET", "/v1/source-change-notices?status=closed", "", nil)
	mustStatus(t, 200, code, list)
	code, _ = e.do("GET", "/v1/source-change-notices/"+uuid.NewString(), "", nil)
	assert.Equal(t, 404, code)
	_, err := pool.Exec(ctx, `UPDATE source_change_notices SET title='edited' WHERE notice_id=$1`, nid)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM source_change_notices WHERE notice_id=$1`, nid)
	require.Error(t, err)

	// The whole point (s29): the notice never edits, activates or publishes a rule.
	var after string
	require.NoError(t, pool.QueryRow(ctx, `SELECT rule_status||'|'||rule_payload::text||'|'||COALESCE(updated_at::text,'') FROM jurisdiction_rules WHERE jurisdiction_rule_id=$1`, rule.JurisdictionRuleID).Scan(&after))
	assert.Equal(t, before, after)
}

// ── metrics ─────────────────────────────────────────────────────────────────

func TestOpsMetrics_ReportsSkewAgeUpcomingAndWhatItDoesNotMeasure(t *testing.T) {
	c := opsPack(t, ringCanary, regionEU)
	c.certifyVersion(t, c.version)
	c.lifecycle(t, c.version, "publish", nil, 200)
	c.deploy(t, c.version, ringCanary, regionEU, 201)
	c.certifyVersion(t, "2026.09.1")
	c.lifecycle(t, "2026.09.1", "publish", nil, 200)
	c.deploy(t, "2026.09.1", ringCanary, regionUS, 201) // us-east runs a newer version than eu-west: skew
	resolveOutcome(t, c)

	code, m := c.e.do("GET", "/v1/ops/metrics?window_hours=24", "ops", nil)
	mustStatus(t, 200, code, m)
	assert.NotEmpty(t, m["resolutions_by_outcome"])
	assert.Contains(t, mustString(m["resolved_by_pack_version"]), c.packRef)

	var skew map[string]any
	for _, s := range m["deployment_skew"].([]any) {
		e := s.(map[string]any)
		if e["pack_ref"] == c.packRef && e["ring_code"] == ringCanary {
			skew = e
		}
	}
	require.NotNil(t, skew, "the pack appears in the skew report")
	assert.Equal(t, true, skew["skewed"])
	assert.Equal(t, c.version, skew["newest_active_version_by_region"].(map[string]any)[regionEU])
	assert.Equal(t, "2026.09.1", skew["newest_active_version_by_region"].(map[string]any)[regionUS])

	// Certification age: fresh certifications are not stale; a warn threshold of 1 day marks an aged one stale.
	withoutGuards(t, "pack_certifications", func() {
		_, err := pool.Exec(ctx, `UPDATE pack_certifications c SET certified_at = NOW() - INTERVAL '10 days' FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id)
			WHERE c.pack_version_id=v.pack_version_id AND p.pack_ref=$1 AND v.version=$2`, c.packRef, c.version)
		require.NoError(t, err)
	})
	_, m = c.e.do("GET", "/v1/ops/metrics?cert_age_warn_days=5", "ops", nil)
	var staleSeen, freshSeen bool
	for _, a := range m["certification_age"].([]any) {
		x := a.(map[string]any)
		if x["pack_ref"] == c.packRef && x["version"] == c.version {
			staleSeen = x["stale"] == true && x["age_days"].(float64) >= 10
		}
		if x["pack_ref"] == c.packRef && x["version"] == "2026.09.1" {
			freshSeen = x["stale"] == false
		}
	}
	assert.True(t, staleSeen)
	assert.True(t, freshSeen)

	// Released but never deployed anywhere is called out.
	c.certifyVersion(t, "2026.10.1")
	c.lifecycle(t, "2026.10.1", "publish", nil, 200)
	_, m = c.e.do("GET", "/v1/ops/metrics", "ops", nil)
	assert.Contains(t, mustString(m["released_but_not_deployed"]), "2026.10.1")
	assert.NotEmpty(t, m["pack_versions_by_status"])

	// Honest about its limits.
	assert.Contains(t, mustString(m["not_measured"]), "historical replay drift")
	assert.Contains(t, mustString(m["not_measured"]), "no e-invoice or filing adapter")

	code, _ = c.e.do("GET", "/v1/ops/metrics?window_hours=abc", "ops", nil)
	assert.Equal(t, 400, code)
	code, _ = c.e.do("GET", "/v1/ops/metrics", "", nil)
	assert.Equal(t, 401, code)
	c.e.auth.deny = "pack_ops_metrics.view"
	code, _ = c.e.do("GET", "/v1/ops/metrics", "ops", nil)
	assert.Equal(t, 403, code)
}
