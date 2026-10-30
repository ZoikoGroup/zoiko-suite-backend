//go:build integration

// ZS-JUR-001 s9 end to end: a jurisdiction is determined from the entity's authorised assignments (tenant data), a claimed
// jurisdiction never widens them, the evidence ledger holds no tenant or entity ids, and the opt-in resolver gate serves only
// jurisdictions whose rollout is LAUNCHED.
package registryit_test

import (
	"context"
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
	"zoiko.io/jurisdiction-rules-svc/internal/resolver"
)

const decisionPath = "/v1/jurisdiction-decisions:resolve"

// newEnvDetermination wires the entity applicability projection as main.go does, which mounts the determination route.
func newEnvDetermination() *env {
	pub := &spyPub{}
	auth := &recordingAuthz{}
	r := chi.NewRouter()
	h := handler.New(st, auth, pub, "platform-scope", zap.NewNop()).WithRegistry(st, pub).WithEntityApplicability(st)
	handler.RegisterRoutes(r, h)
	return &env{srv: r, pub: pub, auth: auth}
}

// assign records one consumed assignment in the projection, as the entity event consumer would.
func assign(t *testing.T, tenant, entity string, j *domain.Jurisdiction, typ string, from time.Time, to *time.Time) {
	t.Helper()
	id := uuid.NewString()
	applied, err := st.ApplyEntityApplicability(ctx, id, "entity.jurisdiction.changed", domain.EntityJurisdictionApplicability{
		ApplicabilityID: id, TenantID: tenant, LegalEntityID: entity, JurisdictionID: j.JurisdictionID, ApplicabilityType: typ,
		EffectiveFrom: from, EffectiveTo: to, SourceObjectVersion: 1, SourceEventType: "entity.jurisdiction.changed", SourceEventID: id})
	require.NoError(t, err)
	require.True(t, applied)
}

func decide(t *testing.T, e *env, tenant string, body map[string]any, want int) map[string]any {
	t.Helper()
	code, r := e.doWithHeaders("POST", decisionPath, "tax-ops", body, map[string]string{"X-Tenant-Id": tenant})
	require.Equal(t, want, code, "%v", r)
	return r
}

func determination(r map[string]any) map[string]any { m, _ := r["determination"].(map[string]any); return m }

func TestJurisdictionDecision_IsDeterminedFromAuthorisedAssignmentsAndAClaimNeverWidensThem(t *testing.T) {
	e := newEnvDetermination()
	tenant, entity := "tenant-"+uuid.NewString()[:8], "entity-"+uuid.NewString()[:8]
	gb, fr := seedJurisdiction(t, "GB"), seedJurisdiction(t, "FR")
	start := time.Now().UTC().Add(-24 * time.Hour)
	body := func(extra map[string]any) map[string]any {
		b := map[string]any{"legal_entity_id": entity, "purpose": "TAX", "effective_at": time.Now().UTC().Format(time.RFC3339)}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}

	// No assignment: nothing applies, and a claimed jurisdiction is never used instead.
	r := decide(t, e, tenant, body(map[string]any{"claimed_jurisdiction": gb.JurisdictionCode}), 422)
	assert.Equal(t, "NO_APPLICABLE_JURISDICTION", determination(r)["outcome"])
	assert.Nil(t, determination(r)["jurisdiction"])

	// One authorised jurisdiction.
	assign(t, tenant, entity, gb, "PRIMARY", start, nil)
	r = decide(t, e, tenant, body(nil), 200)
	d := determination(r)
	require.Equal(t, "JURISDICTION_DETERMINED", d["outcome"])
	assert.Equal(t, "SINGLE_AUTHORISED_JURISDICTION", d["basis"])
	j := d["jurisdiction"].(map[string]any)
	assert.Equal(t, gb.JurisdictionCode, j["jurisdiction_code"])
	assert.Equal(t, "NO_ROLLOUT", j["support"], "the answer says whether the jurisdiction is supported for production, and here it is not")
	assert.NotEmpty(t, j["evidence"].([]any)[0].(map[string]any)["source_event_id"], "the decision names the assignment it rests on")
	// Every recorded decision is announced, refusals included (the earlier NO_APPLICABLE answer and this one).
	assert.Equal(t, 2, e.pub.count(events.EventJurisdictionDecisionResolved))

	// JUR-NEG-05: a claim outside the authorised set is refused, even though FR exists as a jurisdiction.
	r = decide(t, e, tenant, body(map[string]any{"claimed_jurisdiction": fr.JurisdictionCode}), 422)
	assert.Equal(t, "CLAIM_NOT_AUTHORISED", determination(r)["outcome"])
	assert.Len(t, determination(r)["candidates"], 1, "the authorised set is shown so the caller can correct the claim")
	r = decide(t, e, tenant, body(map[string]any{"claimed_jurisdiction": strings.ToLower(gb.JurisdictionCode)}), 200)
	assert.Equal(t, "CONFIRMED", determination(r)["claim"].(map[string]any)["status"])

	// A second authorised jurisdiction: ambiguous until a claim selects inside the set.
	assign(t, tenant, entity, fr, "SECONDARY", start, nil)
	r = decide(t, e, tenant, body(nil), 409)
	assert.Equal(t, "AMBIGUOUS_JURISDICTION", determination(r)["outcome"])
	assert.Len(t, determination(r)["candidates"], 2)
	r = decide(t, e, tenant, body(map[string]any{"claimed_jurisdiction": fr.JurisdictionCode}), 200)
	d = determination(r)
	assert.Equal(t, "CLAIM_WITHIN_AUTHORISED_SET", d["basis"])
	assert.Equal(t, fr.JurisdictionCode, d["jurisdiction"].(map[string]any)["jurisdiction_code"])
	assert.Equal(t, "SELECTED_AMONG_AUTHORISED", d["claim"].(map[string]any)["status"])

	// Purpose: a filing-only assignment authorises FILING, not TAX.
	de := seedJurisdiction(t, "DE")
	assign(t, tenant, entity, de, "FILING_ONLY", start, nil)
	r = decide(t, e, tenant, body(map[string]any{"claimed_jurisdiction": de.JurisdictionCode}), 422)
	assert.Equal(t, "CLAIM_NOT_AUTHORISED", determination(r)["outcome"], "DE is authorised for filing only")
	r = decide(t, e, tenant, body(map[string]any{"purpose": "FILING", "claimed_jurisdiction": de.JurisdictionCode}), 200)
	assert.Equal(t, de.JurisdictionCode, determination(r)["jurisdiction"].(map[string]any)["jurisdiction_code"])
}

func TestJurisdictionDecision_IsTenantScopedTemporalAndIgnoresDeactivatedJurisdictions(t *testing.T) {
	e := newEnvDetermination()
	tenant, other, entity := "tenant-"+uuid.NewString()[:8], "tenant-"+uuid.NewString()[:8], "entity-"+uuid.NewString()[:8]
	j := seedJurisdiction(t, "NZ")
	now := time.Now().UTC()
	assign(t, tenant, entity, j, "PRIMARY", now.Add(-48*time.Hour), nil)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	b := func(effective string) map[string]any {
		return map[string]any{"legal_entity_id": entity, "purpose": "ANY", "effective_at": effective}
	}

	// Another tenant's entity id is indistinguishable from an unknown one.
	assert.Equal(t, "NO_APPLICABLE_JURISDICTION", determination(decide(t, e, other, b(at(0)), 422))["outcome"])
	assert.Equal(t, "JURISDICTION_DETERMINED", determination(decide(t, e, tenant, b(at(0)), 200))["outcome"])

	// Temporal: before the assignment began nothing applied.
	assert.Equal(t, "NO_APPLICABLE_JURISDICTION", determination(decide(t, e, tenant, b(at(-72*time.Hour)), 422))["outcome"])

	// A period that has ended no longer applies (the end instant is exclusive).
	end := now.Add(-time.Hour).Truncate(time.Second)
	endEntity := "entity-" + uuid.NewString()[:8]
	assign(t, tenant, endEntity, j, "PRIMARY", now.Add(-48*time.Hour), &end)
	eb := func(effective time.Time) map[string]any {
		return map[string]any{"legal_entity_id": endEntity, "purpose": "TAX", "effective_at": effective.Format(time.RFC3339Nano)}
	}
	assert.Equal(t, "JURISDICTION_DETERMINED", determination(decide(t, e, tenant, eb(end.Add(-time.Nanosecond)), 200))["outcome"])
	assert.Equal(t, "NO_APPLICABLE_JURISDICTION", determination(decide(t, e, tenant, eb(end), 422))["outcome"])

	// A deactivated jurisdiction is not an authorised one.
	dead := seedJurisdiction(t, "SG")
	deadEntity := "entity-" + uuid.NewString()[:8]
	assign(t, tenant, deadEntity, dead, "PRIMARY", now.Add(-48*time.Hour), nil)
	_, _, err := st.DeactivateJurisdiction(ctx, dead.JurisdictionID, "ops")
	require.NoError(t, err)
	assert.Equal(t, "NO_APPLICABLE_JURISDICTION", determination(decide(t, e, tenant,
		map[string]any{"legal_entity_id": deadEntity, "purpose": "ANY", "effective_at": at(0)}, 422))["outcome"])
}

func TestJurisdictionDecision_EvidenceHoldsNoTenantOrEntityIdsAndReplaysAreIdempotent(t *testing.T) {
	e := newEnvDetermination()
	tenant, entity := "tenant-secret-"+uuid.NewString()[:8], "entity-secret-"+uuid.NewString()[:8]
	j := seedJurisdiction(t, "IE")
	assign(t, tenant, entity, j, "PRIMARY", time.Now().UTC().Add(-time.Hour), nil)
	// A PLANNED rollout makes the support answer concrete.
	code, rr := e.do("POST", "/v1/admin/rollouts", "platform", map[string]any{"family_ref": "zz." + strings.ToLower(uuid.NewString()[:8]), "display_name": "IE", "layer": "COUNTRY",
		"jurisdiction_code": j.JurisdictionCode, "owner": "owner-1"})
	mustStatus(t, 201, code, rr)

	body := map[string]any{"legal_entity_id": entity, "purpose": "TAX", "effective_at": time.Now().UTC().Format(time.RFC3339)}
	hdr := map[string]string{"X-Tenant-Id": tenant, "Idempotency-Key": "dec-1"}
	code, a := e.doWithHeaders("POST", decisionPath, "tax-ops", body, hdr)
	mustStatus(t, 200, code, a)
	assert.Equal(t, "NOT_YET_SUPPORTED", determination(a)["jurisdiction"].(map[string]any)["support"], "a PLANNED rollout is not support")
	before := e.pub.count(events.EventJurisdictionDecisionResolved)

	var kind, outcome, reqText, respText string
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision_kind, outcome, request::text, response::text FROM rule_decision_evidence WHERE decision_id=$1`, a["decision_id"]).
		Scan(&kind, &outcome, &reqText, &respText))
	assert.Equal(t, "JURISDICTION", kind)
	assert.Equal(t, "JURISDICTION_DETERMINED", outcome)
	assert.Contains(t, reqText, "entity_ref_digest")
	for _, forbidden := range []string{tenant, entity} {
		assert.NotContains(t, reqText, forbidden, "the platform-wide ledger must not hold tenant or entity ids")
		assert.NotContains(t, respText, forbidden)
	}

	// Replay: same decision, no second event. A different question under the same key is a conflict.
	code, b := e.doWithHeaders("POST", decisionPath, "tax-ops", body, hdr)
	mustStatus(t, 200, code, b)
	assert.Equal(t, a["decision_id"], b["decision_id"])
	assert.Equal(t, true, b["replayed"])
	assert.Equal(t, before, e.pub.count(events.EventJurisdictionDecisionResolved), "a replay is not announced again")
	body["purpose"] = "FILING"
	code, c := e.doWithHeaders("POST", decisionPath, "tax-ops", body, hdr)
	mustStatus(t, 409, code, c)
}

func TestJurisdictionDecision_RequestValidationAndRouteWiring(t *testing.T) {
	e := newEnvDetermination()
	ok := map[string]any{"legal_entity_id": "e1", "purpose": "TAX", "effective_at": time.Now().UTC().Format(time.RFC3339)}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range ok {
			m[kk] = vv
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	code, r := e.do("POST", decisionPath, "tax-ops", ok)
	assert.Equal(t, 401, code, "X-Tenant-Id is required: %v", r)
	decide(t, e, "t1", with("legal_entity_id", nil), 400)
	decide(t, e, "t1", with("purpose", "PAYROLL"), 400)
	decide(t, e, "t1", with("purpose", nil), 400)
	decide(t, e, "t1", with("effective_at", "yesterday"), 400)
	decide(t, e, "t1", with("legal_entity_id", strings.Repeat("e", 129)), 400)

	// Without the entity projection the route does not exist, so an unwired deployment exposes exactly what it did before.
	plain := newEnv()
	code, _ = plain.doWithHeaders("POST", decisionPath, "tax-ops", ok, map[string]string{"X-Tenant-Id": "t1"})
	assert.Equal(t, 404, code)
}

// newEnvGated is a resolver that serves only jurisdictions whose rollout is LAUNCHED with a released pack (RESOLVER_REQUIRE_LAUNCHED).
func newEnvGated() *env {
	pub := &spyPub{}
	auth := &recordingAuthz{}
	res, err := resolver.New(st, resolver.Config{EligibleStatuses: []string{"RELEASED"}, CacheTTL: 0,
		SupportGate: func(c2 context.Context, code string) (resolver.SupportAnswer, error) {
			v, err := st.JurisdictionSupport(c2, code)
			if err != nil {
				return resolver.SupportAnswer{}, err
			}
			return resolver.SupportAnswer{Supported: v.Supported, Outcome: v.Outcome}, nil
		}})
	if err != nil {
		panic(err)
	}
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(st, auth, pub, "platform-scope", zap.NewNop()).WithRegistry(st, pub).WithCertificationPolicy(1).WithResolver(res))
	return &env{srv: r, pub: pub, auth: auth}
}

func TestResolverGate_ServesOnlyLaunchedJurisdictions(t *testing.T) {
	s := newRecordsScenario(t)
	s.release(t)
	open, gated := s.e, newEnvGated()
	ask := func(e *env, want int) map[string]any {
		s.e = e
		return s.retention(t, nil, want)
	}

	// Without the gate (the default) a released pack is served even though no rollout was ever launched.
	assert.Equal(t, "RETENTION_RESOLVED", outcomeOf(ask(open, 200), "retention"))
	// With the gate the same question is refused, explicitly and with the reason, for a jurisdiction that is not yet supported.
	r := ask(gated, 422)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", outcomeOf(r, "retention"))
	assert.Contains(t, strings.Join(toStrings(r["retention"].(map[string]any)["resolution"].(map[string]any)["explanation"]), " "), "NO_ROLLOUT")

	// Launch the rollout through the governance gate (the normal env), and the gated resolver serves it.
	s.e = open
	code, rr := s.e.do("POST", "/v1/admin/rollouts", "platform", map[string]any{"family_ref": "zz." + strings.ToLower(s.packRef[len(s.packRef)-6:]), "display_name": "Gate land",
		"layer": "COUNTRY", "jurisdiction_code": s.jur.JurisdictionCode, "owner": "owner-1", "support_owner": "support-1"})
	mustStatus(t, 201, code, rr)
	id := rr["rollout_id"].(string)
	transition(t, s, id, "owner-1", "AUTHORING", "", 200)
	attestAll(t, s, id, "READY", "DOR", 10, "attester-1")
	transition(t, s, id, "owner-1", "READY", "", 200)
	attestAll(t, s, id, "DONE", "DOD", 11, "attester-2")
	code, rr = s.e.do("POST", "/v1/admin/rollouts/"+id+"/expert-approvals", "expert-1", map[string]any{"qualification": "Chartered tax adviser", "scope": "VAT", "decision": "APPROVE"})
	mustStatus(t, 201, code, rr)
	code, rr = s.e.do("PUT", "/v1/admin/rollouts/"+id+"/packs", "platform", map[string]any{"pack_refs": []string{s.packRef}})
	mustStatus(t, 200, code, rr)
	assert.Equal(t, "NOT_YET_SUPPORTED", outcomeFromSupport(t, s, s.jur.JurisdictionCode), "READY is not LAUNCHED")
	ask(gated, 422)
	transition(t, s, id, "launcher-1", "LAUNCHED", "", 200)
	assert.Equal(t, "RETENTION_RESOLVED", outcomeOf(ask(gated, 200), "retention"), "LAUNCHED with a released pack is served")

	// Suspension stops service at once (the cache TTL is zero here; the TTL behaviour is unit-tested).
	s.e = open
	transition(t, s, id, "ops", "SUSPENDED", "authority changed the schema", 200)
	r = ask(gated, 422)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", outcomeOf(r, "retention"))
	assert.Contains(t, strings.Join(toStrings(r["retention"].(map[string]any)["resolution"].(map[string]any)["explanation"]), " "), "SUSPENDED")

	// The other resolver entry points are gated too, not only rule resolution.
	s.e = gated
	code, c := s.e.do("POST", "/v1/rule-resolutions:resolve", "caller", map[string]any{"jurisdiction": s.jur.JurisdictionCode, "rule_domain": "RECORDS_RETENTION",
		"rule_code": "PAYROLL_RECORDS", "effective_at": "2026-03-10T00:00:00Z"})
	assert.Equal(t, 422, code, "%v", c)
}

func outcomeFromSupport(t *testing.T, s *payrollScenario, code string) string {
	t.Helper()
	return supportOf(t, s, code)["outcome"].(string)
}
