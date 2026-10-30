//go:build integration

// ZS-JUR-001 s10 and s23 end to end: a pack that would collide with another released pack is refused at release, and a released version is
// superseded only when nothing it served is left without a servant.
package registryit_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/events"
)

func blockReasons(r map[string]any) string {
	var out []string
	if l, ok := r["reasons"].([]any); ok {
		for _, x := range l {
			out = append(out, fmt.Sprint(x))
		}
	}
	return strings.Join(out, " | ")
}

func versionStatus(t *testing.T, ref, version string) string {
	t.Helper()
	var st string
	require.NoError(t, pool.QueryRow(ctx, `SELECT v.status FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id)
		WHERE p.pack_ref=$1 AND v.version=$2`, ref, version).Scan(&st))
	return st
}

func TestSupersede_OnlyWhenNothingLosesService(t *testing.T) {
	c := releasedPack(t, 0)
	e := c.e
	sup := func(version string, body map[string]any) (int, map[string]any) {
		return e.do("POST", c.fixture.path(version, "/supersede"), "releaser", body)
	}

	c.certifyVersion(t, "2026.09.1") // a newer version, certified but not yet released

	// Request validation.
	code, _ := sup("2026.08.1", map[string]any{"reason": "x"})
	assert.Equal(t, 400, code, "superseded_by is required")
	code, _ = sup("2026.08.1", map[string]any{"superseded_by": "2026.09.1"})
	assert.Equal(t, 400, code, "a reason is required")
	code, _ = sup("2026.08.1", map[string]any{"superseded_by": "9.9.9", "reason": "x"})
	assert.Equal(t, 404, code, "the newer version must exist")

	// A newer version that is not released cannot take over: the older one would leave no servant.
	code, r := sup("2026.08.1", map[string]any{"superseded_by": "2026.09.1", "reason": "corrected rates"})
	mustStatus(t, 409, code, r)
	assert.Contains(t, blockReasons(r), "newer_version_not_released")
	assert.Equal(t, "RELEASED", versionStatus(t, c.packRef, "2026.08.1"), "a refused supersession changes nothing")

	// Release the newer version; now the older can be superseded.
	setStatus(t, c, "2026.09.1", "RELEASED")
	code, r = sup("2026.08.1", map[string]any{"superseded_by": "2026.09.1", "reason": "corrected rates", "evidence_ref": "CHG-42"})
	mustStatus(t, 200, code, r)
	assert.Equal(t, true, r["changed"])
	assert.Equal(t, "SUPERSEDED", r["to_status"])
	assert.Equal(t, "SUPERSEDED", versionStatus(t, c.packRef, "2026.08.1"))
	assert.Equal(t, 1, e.pub.count(events.EventPackSuperseded))
	var action, reason string
	require.NoError(t, pool.QueryRow(ctx, `SELECT e.action, e.reason FROM pack_release_events e JOIN jurisdiction_pack_versions v USING (pack_version_id)
		JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref=$1 AND v.version='2026.08.1' AND e.action='SUPERSEDE'`, c.packRef).Scan(&action, &reason))
	assert.Contains(t, reason, "superseded by 2026.09.1", "the release history says by what and why")

	// A repeat is a no-op and is not announced twice.
	code, r = sup("2026.08.1", map[string]any{"superseded_by": "2026.09.1", "reason": "again"})
	mustStatus(t, 200, code, r)
	assert.Equal(t, false, r["changed"])
	assert.Equal(t, 1, e.pub.count(events.EventPackSuperseded))

	// Historical resolution stays permitted: a pinned replay still reaches the superseded version, while new decisions use the newer one.
	pinned := c.resolveReq(time.Now())
	pinned["pack_ref"], pinned["pack_version"] = c.packRef, "2026.08.1"
	code, r = e.do("POST", "/v1/rule-resolutions:resolve", "auditor", pinned)
	mustStatus(t, 200, code, r)
	assert.Equal(t, "2026.08.1", r["decision"].(map[string]any)["pack"].(map[string]any)["version"])
	code, r = e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
	mustStatus(t, 200, code, r)
	assert.Equal(t, "2026.09.1", r["decision"].(map[string]any)["pack"].(map[string]any)["version"])

	// The newer version cannot be superseded by the older (not newer, and the older is no longer released), reported together.
	code, r = sup("2026.09.1", map[string]any{"superseded_by": "2026.08.1", "reason": "backwards"})
	mustStatus(t, 409, code, r)
	assert.Contains(t, blockReasons(r), "not_newer")
	assert.Contains(t, blockReasons(r), "newer_version_not_released")
	assert.Equal(t, "RELEASED", versionStatus(t, c.packRef, "2026.09.1"))

	// Only a released version can be superseded.
	c.certifyVersion(t, "2026.10.1")
	code, r = sup("2026.10.1", map[string]any{"superseded_by": "2026.09.1", "reason": "x"})
	mustStatus(t, 409, code, r)
	assert.Equal(t, "invalid_lifecycle_state", r["error"])
}

func TestSupersede_NewerVersionMustBeDeployedWhereTheOlderIs(t *testing.T) {
	c := releasedPack(t, 0)
	e := c.e
	ring, region := "sup-ring-"+strings.ToLower(c.packRef[len(c.packRef)-6:]), "sup-eu-"+strings.ToLower(c.packRef[len(c.packRef)-6:])
	_, err := pool.Exec(ctx, `INSERT INTO deployment_rings (ring_code, ordinal, min_soak_seconds, created_by_principal_id) VALUES ($1, $2, 0, 'seed')`, ring, 100000+time.Now().Nanosecond()%800000)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO deployment_regions (region_code, created_by_principal_id) VALUES ($1, 'seed')`, region)
	require.NoError(t, err)
	deploy := func(version string) {
		_, err := pool.Exec(ctx, `INSERT INTO pack_deployments (pack_version_id, ring_code, region_code, deployed_by)
			SELECT v.pack_version_id, $3, $4, 'seed' FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref=$1 AND v.version=$2`,
			c.packRef, version, ring, region)
		require.NoError(t, err)
	}
	c.certifyVersion(t, "2026.09.1")
	setStatus(t, c, "2026.09.1", "RELEASED")
	deploy("2026.08.1") // the older version is live in this ring and region; the newer one is not

	code, r := e.do("POST", c.fixture.path("2026.08.1", "/supersede"), "releaser", map[string]any{"superseded_by": "2026.09.1", "reason": "corrected"})
	mustStatus(t, 409, code, r)
	assert.Contains(t, blockReasons(r), "newer_not_deployed")
	assert.Contains(t, blockReasons(r), ring)
	assert.Equal(t, "RELEASED", versionStatus(t, c.packRef, "2026.08.1"), "the ring keeps being served")

	deploy("2026.09.1")
	code, r = e.do("POST", c.fixture.path("2026.08.1", "/supersede"), "releaser", map[string]any{"superseded_by": "2026.09.1", "reason": "corrected"})
	mustStatus(t, 200, code, r)
	assert.Equal(t, "SUPERSEDED", versionStatus(t, c.packRef, "2026.08.1"))
}

// releaseOnly builds, certifies and tries to publish a pack of only the given rules, returning the publish result without asserting it.
func (s *payrollScenario) releaseOnly(t *testing.T, ruleIDs []string) (ref string, publishCode int, publishBody map[string]any) {
	t.Helper()
	e := s.e
	ref = uniqueRef("jur.p")
	s.packRef = ref
	code, r := e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": ref, "pack_name": "Subset", "owner": "Payroll Compliance"})
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", "/v1/admin/packs/"+ref+"/versions", "author", manifest(ref, "2026.08.1", s.jur.JurisdictionCode, map[string]any{"rule_modules": ruleIDs, "effective_from": "2025-01-01"}))
	mustStatus(t, 201, code, r)
	for _, step := range []string{"/submit-review", "/compile", "/sign"} {
		code, r = e.do("POST", s.path(step), "author", nil)
		require.Contains(t, []int{200, 201}, code, "%s: %v", step, r)
	}
	code, r = e.do("POST", s.path("/test-bundle"), "tester", s.autoBundle(t))
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", s.path("/test-runs"), "tester", nil)
	mustStatus(t, 201, code, r)
	require.Equal(t, true, r["passed"], "%v", r["result"])
	code, r = e.do("POST", s.path("/reviews"), "reviewer-pay", map[string]any{"role": "PAYROLL", "decision": "APPROVE"})
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", s.path("/certify"), "certifier", nil)
	mustStatus(t, 201, code, r)
	publishCode, publishBody = e.do("POST", s.path("/publish"), "releaser", nil)
	return ref, publishCode, publishBody
}

func TestRelease_RefusesAPackThatCollidesWithAnotherReleasedPack(t *testing.T) {
	// Two rule versions of one code whose periods overlap from April 2026, in two different packs of the same jurisdiction.
	s := newRulesScenario(t, []payrollSpec{
		{"PERSONAL_ALLOWANCE", pyStart2025, nil, pyAllowance25, ""},
		{"PERSONAL_ALLOWANCE", pyStart2026, nil, pyAllowance26, ""},
	})
	a, b := s.rules["PERSONAL_ALLOWANCE@2025-04-06"], s.rules["PERSONAL_ALLOWANCE@2026-04-06"]
	require.NotEmpty(t, a)
	require.NotEmpty(t, b)

	refA, code, r := s.releaseOnly(t, []string{a})
	mustStatus(t, 200, code, r)
	refB, code, r := s.releaseOnly(t, []string{b})
	mustStatus(t, 409, code, r)
	assert.Equal(t, "operation_blocked", r["error"])
	reasons := blockReasons(r)
	assert.Contains(t, reasons, "cross_pack_conflict")
	assert.Contains(t, reasons, "PAYROLL/PERSONAL_ALLOWANCE")
	assert.Contains(t, reasons, refA+"@2026.08.1", "the other pack is named")
	assert.Contains(t, reasons, "2026-04-06 onward", "and the overlapping period")
	assert.Equal(t, "CERTIFIED", versionStatus(t, refB, "2026.08.1"), "a refused release changes nothing")

	// The block is about packs that are live: once the other pack is withdrawn, the release goes through.
	code, r = s.e.do("POST", "/v1/admin/packs/"+refA+"/versions/2026.08.1/withdraw", "releaser", map[string]any{"reason": "replaced by a corrected pack", "evidence_ref": "CHG-7"})
	mustStatus(t, 200, code, r)
	s.packRef = refB
	code, r = s.e.do("POST", s.path("/publish"), "releaser", nil)
	mustStatus(t, 200, code, r)
	assert.Equal(t, "RELEASED", versionStatus(t, refB, "2026.08.1"))
}

func TestRelease_AllowsPacksWhosePeriodsDoNotOverlap(t *testing.T) {
	// The same rule code in two packs is fine when the rules never apply at the same instant (the end is exclusive).
	s := newRulesScenario(t, []payrollSpec{
		{"PERSONAL_ALLOWANCE", pyStart2025, &pyStart2026, pyAllowance25, ""},
		{"PERSONAL_ALLOWANCE", pyStart2026, nil, pyAllowance26, ""},
	})
	_, code, r := s.releaseOnly(t, []string{s.rules["PERSONAL_ALLOWANCE@2025-04-06"]})
	mustStatus(t, 200, code, r)
	_, code, r = s.releaseOnly(t, []string{s.rules["PERSONAL_ALLOWANCE@2026-04-06"]})
	mustStatus(t, 200, code, r)
}
