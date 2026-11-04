package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"zoiko.io/policy-svc/internal/domain"
)

// REF-07 lifecycle against a real Postgres: replacing an active policy is
// stated, never a side effect; each change is announced in the transaction that
// makes it; and the impact report says what activating a version would touch.

type outboxRow struct {
	eventType string
	payload   map[string]any
}

func (e *testEnv) accountingEvents(t *testing.T, eventType string) []outboxRow {
	t.Helper()
	rows, err := e.admin.Query(context.Background(), `SELECT event_type, payload FROM outbox WHERE event_type = $1 ORDER BY created_at, event_id`, eventType)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var raw []byte
		if err := rows.Scan(&r.eventType, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &r.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (e *testEnv) approved(t *testing.T, tenant string, mutate ...func(*domain.DraftAccountingPolicyParams)) *domain.AccountingPolicyVersion {
	t.Helper()
	v := e.acctDraft(t, tenant, mutate...)
	for _, step := range []struct{ cmd, actor string }{{domain.CommandSubmitReview, "author"}, {domain.CommandApprove, "approver"}} {
		if _, err := e.cmd(t, tenant, &v.PolicyVersion, step.cmd, step.actor); err != nil {
			t.Fatalf("%s: %v", step.cmd, err)
		}
	}
	return v
}

// ── supersession is stated ──────────────────────────────────────────────────

func TestAccountingSupersession_ReplacingAnActiveVersionMustNameIt(t *testing.T) {
	e := newEnv(t)
	v1 := e.released(t, tenantA, book(bookGB), semver("1.0.0"))
	v2 := e.approved(t, tenantA, book(bookGB), semver("2.0.0"))

	// Silent replacement is refused, and nothing changed.
	if _, err := e.activate(tenantA, v2, "activator", ""); !errors.Is(err, domain.ErrSupersessionRequired) {
		t.Fatalf("activating over an active version without naming it: %v, want ErrSupersessionRequired", err)
	}
	if !strings.Contains(errString(e.activate(tenantA, v2, "activator", "")), v1.PolicyVersionID) {
		t.Error("the refusal does not say which version is active")
	}
	if got := e.statusOf(t, v1); got != domain.StatusActive {
		t.Errorf("v1 is %s after a refused activation", got)
	}

	// Naming the wrong version, or one that is not active, is refused.
	other := e.released(t, tenantA, book(bookUS))
	if _, err := e.activate(tenantA, v2, "activator", other.PolicyVersionID); !errors.Is(err, domain.ErrInvalidSupersedes) {
		t.Errorf("naming another policy's active version: %v, want ErrInvalidSupersedes", err)
	}
	if _, err := e.activate(tenantA, v2, "activator", v2.PolicyVersionID); !errors.Is(err, domain.ErrInvalidSupersedes) {
		t.Errorf("naming itself: %v, want ErrInvalidSupersedes", err)
	}

	// Naming the active version replaces it, keeping it.
	res, err := e.activate(tenantA, v2, "activator", v1.PolicyVersionID)
	if err != nil || !res.Transitioned || len(res.Superseded) != 1 || res.Superseded[0].PolicyVersionID != v1.PolicyVersionID {
		t.Fatalf("superseding: %+v err %v", res, err)
	}
	old, _ := e.s.GetAccountingPolicyVersion(as(tenantA), v1.PolicyVersionID)
	if old.VersionStatus != domain.StatusSuperseded || old.SupersededByVersionID == nil || *old.SupersededByVersionID != v2.PolicyVersionID || old.Details.SemanticVersion != "1.0.0" {
		t.Errorf("v1 = %+v, want it kept, superseded by v2", old)
	}
}

func TestAccountingSupersession_NamingAVersionWhereNothingIsActiveIsRefused(t *testing.T) {
	e := newEnv(t)
	v := e.approved(t, tenantA, book(bookGB))
	if _, err := e.activate(tenantA, v, "activator", "99999999-9999-9999-9999-999999999999"); !errors.Is(err, domain.ErrInvalidSupersedes) {
		t.Errorf("superseding a version that is not active: %v, want ErrInvalidSupersedes", err)
	}
	if _, err := e.activate(tenantA, v, "activator", ""); err != nil {
		t.Errorf("a first activation needs no predecessor: %v", err)
	}

	// Only accounting policies have the command: naming a version for any other
	// policy is refused rather than ignored.
	generic := e.draft(t, tenantA, e.family(t, tenantA, sp(tenantA), "AP-1"), sp(tenantA), nil, e.clock, `{"threshold_amount": 1}`)
	for _, step := range []struct{ cmd, actor string }{{domain.CommandSubmitReview, "author"}, {domain.CommandApprove, "approver"}} {
		if _, err := e.cmd(t, tenantA, generic, step.cmd, step.actor); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.Transition(as(tenantA), domain.TransitionCommand{PolicyVersionID: generic.PolicyVersionID, Command: domain.CommandActivate,
		ActorID: "activator", SupersedesVersionID: v.PolicyVersionID}); !errors.Is(err, domain.ErrInvalidSupersedes) {
		t.Errorf("a generic policy given a supersedes id: %v, want ErrInvalidSupersedes", err)
	}
	// A generic policy is activated, and replaced, exactly as before.
	if _, err := e.cmd(t, tenantA, generic, domain.CommandActivate, "activator"); err != nil {
		t.Errorf("a generic activation: %v", err)
	}
}

func (e *testEnv) statusOf(t *testing.T, v *domain.AccountingPolicyVersion) string {
	t.Helper()
	got, err := e.s.GetAccountingPolicyVersion(as(tenantA), v.PolicyVersionID)
	if err != nil {
		t.Fatal(err)
	}
	return got.VersionStatus
}

func errString(_ *domain.TransitionResult, err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The maker-checker rules are the GOV-05 ones, applied to accounting versions.
func TestAccountingLifecycle_TheAuthorIsNeverTheCheckerAndEveryStepIsOrdered(t *testing.T) {
	e := newEnv(t)
	v := e.acctDraft(t, tenantA, book(bookGB))
	if _, err := e.activate(tenantA, v, "activator", ""); !errors.Is(err, domain.ErrApprovalRequired) {
		t.Errorf("activating a draft: %v, want ErrApprovalRequired", err)
	}
	if _, err := e.cmd(t, tenantA, &v.PolicyVersion, domain.CommandSubmitReview, "author"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cmd(t, tenantA, &v.PolicyVersion, domain.CommandApprove, "author"); !errors.Is(err, domain.ErrSoDConflict) {
		t.Errorf("the author approving: %v, want ErrSoDConflict", err)
	}
	if _, err := e.cmd(t, tenantA, &v.PolicyVersion, domain.CommandApprove, "approver"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.activate(tenantA, v, "author", ""); !errors.Is(err, domain.ErrSoDConflict) {
		t.Errorf("the author activating: %v, want ErrSoDConflict", err)
	}
	if _, err := e.activate(tenantA, v, "activator", ""); err != nil {
		t.Errorf("an independent activator: %v", err)
	}
	// Repeating a command that has taken effect changes nothing and says so.
	again, err := e.activate(tenantA, v, "activator", "")
	if err != nil || again.Transitioned {
		t.Errorf("a repeat activation: %+v err %v, want an idempotent no-op", again, err)
	}
}

// ── events ──────────────────────────────────────────────────────────────────

func TestAccountingEvents_EachChangeIsAnnouncedWhereItApplies(t *testing.T) {
	e := newEnv(t)
	v1 := e.released(t, tenantA, book(bookGB), juris(jurUK), semver("1.0.0"), precedence(4))
	approvedEvs := e.accountingEvents(t, domain.EventAccountingPolicyApproved)
	activatedEvs := e.accountingEvents(t, domain.EventAccountingPolicyActivated)
	if len(approvedEvs) != 1 || len(activatedEvs) != 1 {
		t.Fatalf("%d approved, %d activated events, want one each", len(approvedEvs), len(activatedEvs))
	}
	p := activatedEvs[0].payload["payload"]
	if p == nil {
		p = activatedEvs[0].payload
	}
	pm, _ := p.(map[string]any)
	if pm["policy_version_id"] != v1.PolicyVersionID || pm["policy_domain"] != "REVENUE_RECOGNITION" || pm["framework"] != "IFRS" ||
		pm["book_id"] != bookGB || pm["jurisdiction_id"] != jurUK || pm["semantic_version"] != "1.0.0" || pm["precedence"] != float64(4) ||
		pm["artifact_digest"] != v1.ArtifactDigest {
		t.Errorf("activated payload = %v, want where the policy applies and what it is", pm)
	}

	// A superseding activation announces the new version and the old one's end.
	v2 := e.approved(t, tenantA, book(bookGB), juris(jurUK), semver("2.0.0"), precedence(4))
	if _, err := e.activate(tenantA, v2, "activator", v1.PolicyVersionID); err != nil {
		t.Fatal(err)
	}
	sup := e.accountingEvents(t, domain.EventAccountingPolicySuperseded)
	if len(sup) != 1 {
		t.Fatalf("%d superseded events, want 1", len(sup))
	}
	sm := inner(sup[0].payload)
	if sm["policy_version_id"] != v1.PolicyVersionID || sm["superseded_by_version_id"] != v2.PolicyVersionID || sm["semantic_version"] != "1.0.0" {
		t.Errorf("superseded payload = %v", sm)
	}
	if got := len(e.accountingEvents(t, domain.EventAccountingPolicyActivated)); got != 2 {
		t.Errorf("%d activated events, want 2", got)
	}

	// An emergency suspension is announced with its reason and session.
	e.advance(time.Minute)
	res, err := e.s.Transition(as(tenantA), domain.TransitionCommand{PolicyVersionID: v2.PolicyVersionID, Command: domain.CommandSuspend,
		ActorID: "ops", Reason: "wrong rate; break_glass_session=sess-7", BreakGlassSessionID: "sess-7",
		Meta: domain.EventMeta{CorrelationID: "corr", IdempotencyKey: "k-suspend"}})
	if err != nil || !res.Transitioned {
		t.Fatalf("suspend: %+v err %v", res, err)
	}
	susp := e.accountingEvents(t, domain.EventAccountingPolicySuspended)
	if len(susp) != 1 {
		t.Fatalf("%d suspended events, want 1", len(susp))
	}
	um := inner(susp[0].payload)
	if um["policy_version_id"] != v2.PolicyVersionID || um["break_glass_session_id"] != "sess-7" || !strings.Contains(um["reason"].(string), "wrong rate") {
		t.Errorf("suspended payload = %v", um)
	}

	// A repeat that changes nothing announces nothing.
	before := e.count(t, `SELECT COUNT(*) FROM outbox`)
	if _, err := e.s.Transition(as(tenantA), domain.TransitionCommand{PolicyVersionID: v2.PolicyVersionID, Command: domain.CommandSuspend, ActorID: "ops", Reason: "again"}); err != nil {
		t.Fatal(err)
	}
	if after := e.count(t, `SELECT COUNT(*) FROM outbox`); after != before {
		t.Errorf("a repeated suspension wrote %d events", after-before)
	}
}

func inner(m map[string]any) map[string]any {
	if p, ok := m["payload"].(map[string]any); ok {
		return p
	}
	return m
}

// Policies that are not accounting policies announce nothing of the kind.
func TestAccountingEvents_OtherPoliciesAnnounceNoAccountingEvents(t *testing.T) {
	e := newEnv(t)
	fam := e.family(t, tenantA, sp(tenantA), "AP-1")
	v := e.draft(t, tenantA, fam, sp(tenantA), nil, e.clock, `{"threshold_amount": 5}`)
	e.release(t, tenantA, v)
	if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE event_type LIKE 'accounting.%'`); n != 0 {
		t.Errorf("%d accounting events for a generic policy", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE event_type = 'policy.version.activated'`); n != 1 {
		t.Errorf("the generic activation event is gone: %d", n)
	}
}

// Scheduling names the predecessor at once, and the promotion, when the time
// comes, supersedes it and announces both.
func TestAccountingSupersession_AScheduledReplacementPromotesAndAnnounces(t *testing.T) {
	e := newEnv(t)
	v1 := e.released(t, tenantA, book(bookGB), semver("1.0.0"))
	v2 := e.approved(t, tenantA, book(bookGB), semver("2.0.0"), period(e.clock.Add(48*time.Hour), nil))

	if _, err := e.activate(tenantA, v2, "activator", ""); !errors.Is(err, domain.ErrSupersessionRequired) {
		t.Fatalf("scheduling over an active version without naming it: %v", err)
	}
	res, err := e.activate(tenantA, v2, "activator", v1.PolicyVersionID)
	if err != nil || res.Version.VersionStatus != domain.StatusScheduled {
		t.Fatalf("scheduling: %+v err %v", res, err)
	}
	if got := e.statusOf(t, v1); got != domain.StatusActive {
		t.Errorf("v1 is %s while its replacement is only scheduled", got)
	}
	if n := len(e.accountingEvents(t, domain.EventAccountingPolicyActivated)); n != 1 {
		t.Errorf("%d activated events before promotion, want only v1's", n)
	}

	e.advance(72 * time.Hour)
	if n, err := e.s.PromoteDueActivations(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("promote: %d err %v", n, err)
	}
	if got := e.statusOf(t, v1); got != domain.StatusSuperseded {
		t.Errorf("v1 is %s after promotion, want SUPERSEDED", got)
	}
	if n := len(e.accountingEvents(t, domain.EventAccountingPolicyActivated)); n != 2 {
		t.Errorf("%d activated events after promotion, want 2", n)
	}
	if n := len(e.accountingEvents(t, domain.EventAccountingPolicySuperseded)); n != 1 {
		t.Errorf("%d superseded events after promotion, want 1", n)
	}
}

// ── impact ──────────────────────────────────────────────────────────────────

func TestAccountingImpact_SaysWhatActivatingWouldTouch(t *testing.T) {
	e := newEnv(t)
	v1 := e.released(t, tenantA, book(bookGB), semver("1.0.0"))
	// A policy narrowed by jurisdiction, with precedence 1: not ambiguous with v1
	// (precedence 0), but it would be with a v2 of precedence 1.
	other := e.released(t, tenantA, juris(jurUK), precedence(1))

	// v2: the next version of v1's policy, retroactive to before it is drafted.
	v2 := e.approved(t, tenantA, book(bookGB), semver("2.0.0"), precedence(1), period(e.clock.Add(-96*time.Hour), nil))
	// v3 depends on v2.
	e.acctDraft(t, tenantA, domainOf("LEASES"), func(p *domain.DraftAccountingPolicyParams) {
		p.RuleMappings = []domain.RuleMapping{{RefKind: domain.MappingKindPolicyVersion, Ref: v2.PolicyVersionID, RefVersion: "2.0.0", Rationale: "builds on revenue policy"}}
	})

	imp, err := e.s.AccountingImpactOf(as(tenantA), v2.PolicyVersionID)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Version.PolicyVersionID != v2.PolicyVersionID {
		t.Errorf("version = %s", imp.Version.PolicyVersionID)
	}
	if !imp.Retroactive {
		t.Error("a version effective before it was drafted is not reported retroactive")
	}
	if imp.WouldSupersede == nil || imp.WouldSupersede.PolicyVersionID != v1.PolicyVersionID ||
		imp.PredecessorGovernsUntil == nil || !imp.PredecessorGovernsUntil.Equal(v2.EffectiveFrom) {
		t.Errorf("would supersede %+v until %v, want v1 until v2's effective_from", imp.WouldSupersede, imp.PredecessorGovernsUntil)
	}
	if len(imp.DependedOnBy) != 1 || imp.DependedOnBy[0].PolicyDomain != "LEASES" {
		t.Errorf("depended on by %+v, want the leases version", imp.DependedOnBy)
	}
	if len(imp.AmbiguousWith) != 1 || imp.AmbiguousWith[0].PolicyVersionID != other.PolicyVersionID {
		t.Errorf("ambiguous with %+v, want the jurisdiction-narrowed policy", imp.AmbiguousWith)
	}
	blockers := strings.Join(imp.ActivationBlockers, " | ")
	if !strings.Contains(blockers, v1.PolicyVersionID) || !strings.Contains(blockers, other.PolicyVersionID) || !strings.Contains(blockers, "supersed") {
		t.Errorf("blockers = %q, want the active version and the ambiguous one named", blockers)
	}

	// The report agrees with what activation does.
	if _, err := e.activate(tenantA, v2, "activator", ""); !errors.Is(err, domain.ErrSupersessionRequired) {
		t.Errorf("activation without naming: %v", err)
	}
	if _, err := e.activate(tenantA, v2, "activator", v1.PolicyVersionID); !errors.Is(err, domain.ErrPolicyConflicted) {
		t.Errorf("activation named but ambiguous: %v, want ErrPolicyConflicted, as the impact said", err)
	}
}

func TestAccountingImpact_ACleanVersionHasNoBlockers(t *testing.T) {
	e := newEnv(t)
	v := e.approved(t, tenantA, book(bookGB), period(e.clock.Add(24*time.Hour), nil))
	imp, err := e.s.AccountingImpactOf(as(tenantA), v.PolicyVersionID)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Retroactive || imp.WouldSupersede != nil || imp.PendingSchedule != nil || len(imp.AmbiguousWith) != 0 ||
		len(imp.DependedOnBy) != 0 || len(imp.ActivationBlockers) != 0 {
		t.Errorf("impact = %+v, want a clean report", imp)
	}
}

func TestAccountingImpact_ASchedulePendingInTheScopeIsReported(t *testing.T) {
	e := newEnv(t)
	v1 := e.released(t, tenantA, book(bookGB), semver("1.0.0"))
	v2 := e.approved(t, tenantA, book(bookGB), semver("2.0.0"), period(e.clock.Add(48*time.Hour), nil))
	if _, err := e.activate(tenantA, v2, "activator", v1.PolicyVersionID); err != nil {
		t.Fatal(err)
	}
	v3 := e.approved(t, tenantA, book(bookGB), semver("3.0.0"), period(e.clock.Add(96*time.Hour), nil))
	imp, err := e.s.AccountingImpactOf(as(tenantA), v3.PolicyVersionID)
	if err != nil {
		t.Fatal(err)
	}
	if imp.PendingSchedule == nil || imp.PendingSchedule.PolicyVersionID != v2.PolicyVersionID {
		t.Errorf("pending schedule = %+v, want v2", imp.PendingSchedule)
	}
	if !strings.Contains(strings.Join(imp.ActivationBlockers, " "), "already scheduled") {
		t.Errorf("blockers = %v", imp.ActivationBlockers)
	}
}

func TestAccountingImpact_IsTenantScopedAndOnlyForAccountingVersions(t *testing.T) {
	e := newEnv(t)
	v := e.approved(t, tenantA, book(bookGB))
	if _, err := e.s.AccountingImpactOf(as(tenantB), v.PolicyVersionID); !errors.Is(err, domain.ErrPolicyVersionNotFound) {
		t.Errorf("another tenant's impact: %v, want not found", err)
	}
	generic := e.draft(t, tenantA, e.family(t, tenantA, sp(tenantA), "AP-2"), sp(tenantA), nil, e.clock, `{"threshold_amount": 5}`)
	if _, err := e.s.AccountingImpactOf(as(tenantA), generic.PolicyVersionID); !errors.Is(err, domain.ErrPolicyVersionNotFound) {
		t.Errorf("a generic version's impact: %v, want not found", err)
	}
	if _, err := e.s.AccountingImpactOf(as(tenantA), "not-a-uuid"); !errors.Is(err, domain.ErrPolicyVersionNotFound) {
		t.Errorf("a bad id: %v", err)
	}
}
