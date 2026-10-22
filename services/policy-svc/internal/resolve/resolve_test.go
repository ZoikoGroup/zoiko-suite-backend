package resolve

import (
	"testing"
	"time"

	"zoiko.io/policy-svc/internal/domain"
)

var (
	jan1  = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	jan10 = time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	feb10 = time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	feb15 = time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
	feb20 = time.Date(2026, 2, 20, 0, 0, 0, 0, time.UTC)
	mar1  = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	mar5  = time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	mar10 = time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	apr1  = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	now   = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

const tenant = "11111111-1111-1111-1111-111111111111"
const entity = "33333333-3333-3333-3333-333333333333"

func ptr(t time.Time) *time.Time { return &t }

type vopt func(*domain.ApplicablePolicyVersion)

func scope(t, e string) vopt {
	return func(v *domain.ApplicablePolicyVersion) {
		if t != "" {
			v.TenantID = &[]string{t}[0]
		}
		if e != "" {
			v.LegalEntityID = &[]string{e}[0]
		}
	}
}
func until(t time.Time) vopt { return func(v *domain.ApplicablePolicyVersion) { v.EffectiveTo = &t } }
func withdrawn(t time.Time) vopt {
	return func(v *domain.ApplicablePolicyVersion) { v.WithdrawnAt = &t }
}
func number(n int) vopt { return func(v *domain.ApplicablePolicyVersion) { v.VersionNumber = n } }

func version(id, policy string, effective time.Time, activated *time.Time, opts ...vopt) *domain.ApplicablePolicyVersion {
	v := &domain.ApplicablePolicyVersion{PolicyVersion: domain.PolicyVersion{
		PolicyVersionID: id, PolicyID: policy, EffectiveFrom: effective, ActivatedAt: activated, VersionNumber: 1,
	}}
	for _, o := range opts {
		o(v)
	}
	return v
}

func selected(t *testing.T, r Result) string {
	t.Helper()
	if r.Status != Resolved {
		t.Fatalf("expected RESOLVED, got %s (trace %+v)", r.Status, r.Trace)
	}
	return r.Selected.PolicyVersionID
}

func reasonOf(r Result, id string) string {
	for _, e := range r.Trace {
		if e.PolicyVersionID == id {
			return e.Reason
		}
	}
	return ""
}

// GOV-05 negative path #1: a future policy cannot apply before its time.
func TestFutureVersionDoesNotApplyEarly(t *testing.T) {
	v := version("v1", "p", mar1, ptr(feb10))
	r := Resolve([]*domain.ApplicablePolicyVersion{v}, feb15, now)
	if r.Status != NotApplicable || reasonOf(r, "v1") != ReasonNotYetEffective {
		t.Fatalf("expected NOT_APPLICABLE / not_yet_effective, got %s / %s", r.Status, reasonOf(r, "v1"))
	}
	if got := selected(t, Resolve([]*domain.ApplicablePolicyVersion{v}, mar5, now)); got != "v1" {
		t.Fatalf("expected v1 once effective, got %s", got)
	}
}

// Audit row "Expiry (effective_to) is honoured".
func TestExpiredVersionStopsDeciding(t *testing.T) {
	v := version("v1", "p", jan1, ptr(jan1), until(mar1))
	r := Resolve([]*domain.ApplicablePolicyVersion{v}, mar5, now)
	if r.Status != NotApplicable || reasonOf(r, "v1") != ReasonExpired {
		t.Fatalf("expected expired, got %s / %s", r.Status, reasonOf(r, "v1"))
	}
	if got := selected(t, Resolve([]*domain.ApplicablePolicyVersion{v}, feb15, now)); got != "v1" {
		t.Fatalf("expected v1 inside its window, got %s", got)
	}
}

// A future-dated successor takes over from its own effective_from; its
// predecessor keeps governing the time before.
func TestScheduledSuccessorTakesOverAtItsEffectiveTime(t *testing.T) {
	v0 := version("v0", "p", jan1, ptr(jan1))
	v1 := version("v1", "p", mar1, ptr(feb10), number(2))
	cands := []*domain.ApplicablePolicyVersion{v0, v1}
	if got := selected(t, Resolve(cands, feb15, now)); got != "v0" {
		t.Fatalf("15 Feb is v0's, got %s", got)
	}
	if got := selected(t, Resolve(cands, mar5, now)); got != "v1" {
		t.Fatalf("5 Mar is v1's, got %s", got)
	}
}

// GOV-05 negative path #4: a historical request resolves the version that
// was active then, as the platform knew it then.
func TestHistoricalRequestResolvesOriginalVersion(t *testing.T) {
	v0 := version("v0", "p", jan1, ptr(jan1))
	// v1 is a backdated correction, effective 1 Jan, recorded on 10 Feb.
	v1 := version("v1", "p", jan1, ptr(feb10), number(2))
	cands := []*domain.ApplicablePolicyVersion{v0, v1}

	asOf, knownAt, ok := Times(ptr(jan10), nil, now)
	if !ok || !knownAt.Equal(jan10) {
		t.Fatalf("a past as_of must default known_at to itself, got %v", knownAt)
	}
	if got := selected(t, Resolve(cands, asOf, knownAt)); got != "v0" {
		t.Fatalf("as known on 10 Jan, v0 governed 10 Jan; got %s", got)
	}
	if got := selected(t, Resolve(cands, jan10, now)); got != "v1" {
		t.Fatalf("as known now, the correction governs 10 Jan; got %s", got)
	}
	if reasonOf(Resolve(cands, jan10, jan10), "v1") != ReasonActivatedAfterKnownAt {
		t.Fatal("v1 must be explained as not yet known")
	}
}

// V-001 PDC-I-28: suspending a live version does not hand control back to
// the version it replaced.
func TestSuspensionDoesNotResurrectPredecessor(t *testing.T) {
	v0 := version("v0", "p", jan1, ptr(jan1))
	v1 := version("v1", "p", mar1, ptr(feb10), number(2), withdrawn(mar10))
	cands := []*domain.ApplicablePolicyVersion{v0, v1}

	r := Resolve(cands, apr1, now)
	if r.Status != NotApplicable || reasonOf(r, "v1") != ReasonWithdrawn || reasonOf(r, "v0") != ReasonSupersededInChain {
		t.Fatalf("expected fail-closed after suspension, got %s (v0 %s, v1 %s)", r.Status, reasonOf(r, "v0"), reasonOf(r, "v1"))
	}
	// Before the suspension was recorded, v1 governed.
	if got := selected(t, Resolve(cands, mar5, mar5)); got != "v1" {
		t.Fatalf("as known on 5 Mar, v1 governed; got %s", got)
	}
	// February still belongs to v0.
	if got := selected(t, Resolve(cands, feb15, now)); got != "v0" {
		t.Fatalf("15 Feb is still v0's; got %s", got)
	}
}

// A scheduled successor withdrawn before going live never displaced anything.
func TestCancelledScheduleLeavesCurrentVersion(t *testing.T) {
	v0 := version("v0", "p", jan1, ptr(jan1))
	v1 := version("v1", "p", mar1, ptr(feb10), number(2), withdrawn(feb20))
	cands := []*domain.ApplicablePolicyVersion{v0, v1}
	if got := selected(t, Resolve(cands, mar5, now)); got != "v0" {
		t.Fatalf("the cancelled schedule must leave v0 in force, got %s", got)
	}
	if reasonOf(Resolve(cands, mar5, now), "v1") != ReasonCancelledBeforeLive {
		t.Fatal("v1 must be explained as cancelled before it went live")
	}
	// As known before the cancellation, v1 was going to govern March.
	if got := selected(t, Resolve(cands, mar5, feb15)); got != "v1" {
		t.Fatalf("as known on 15 Feb, v1 was scheduled for March; got %s", got)
	}
}

func TestNeverActivatedVersionsAreIgnored(t *testing.T) {
	draft := version("d", "p", jan1, nil)
	r := Resolve([]*domain.ApplicablePolicyVersion{draft}, now, now)
	if r.Status != NotApplicable || reasonOf(r, "d") != ReasonNeverActivated {
		t.Fatalf("a draft must never decide, got %s / %s", r.Status, reasonOf(r, "d"))
	}
}

// GOV-05 failure semantics / V-001 PDC-I-08: never latest-by-timestamp across
// distinct policies at the same specificity.
func TestTiedPoliciesAreConflicted(t *testing.T) {
	a := version("a", "p1", jan1, ptr(jan1), scope(tenant, ""))
	b := version("b", "p2", jan10, ptr(feb10), scope(tenant, ""))
	r := Resolve([]*domain.ApplicablePolicyVersion{a, b}, now, now)
	if r.Status != Conflicted || len(r.Conflicting) != 2 || r.Selected != nil {
		t.Fatalf("expected CONFLICTED with both candidates, got %s %+v", r.Status, r.Conflicting)
	}
}

func TestMostSpecificScopeWins(t *testing.T) {
	global := version("g", "p1", jan1, ptr(jan1))
	tenantWide := version("t", "p2", jan1, ptr(jan1), scope(tenant, ""))
	entityLevel := version("e", "p3", jan1, ptr(jan1), scope(tenant, entity))
	r := Resolve([]*domain.ApplicablePolicyVersion{global, tenantWide, entityLevel}, now, now)
	if selected(t, r) != "e" || reasonOf(r, "t") != ReasonLessSpecific || reasonOf(r, "g") != ReasonLessSpecific {
		t.Fatalf("expected the entity version to win, got trace %+v", r.Trace)
	}
	if len(r.Applicable) != 3 || r.Applicable[0].PolicyVersionID != "e" || r.Applicable[2].PolicyVersionID != "g" {
		t.Fatalf("expected the applicable set most specific first, got %v", r.Applicable)
	}
	// A conflict at a less specific tier does not matter when a more
	// specific version governs.
	g2 := version("g2", "p4", jan1, ptr(jan1))
	if selected(t, Resolve([]*domain.ApplicablePolicyVersion{global, g2, entityLevel}, now, now)) != "e" {
		t.Fatal("a lower-tier tie must not block a more specific version")
	}
}

func TestTimesRejectsFutureKnowledge(t *testing.T) {
	if _, _, ok := Times(nil, ptr(now.Add(time.Hour)), now); ok {
		t.Fatal("known_at in the future must be refused")
	}
	a, k, ok := Times(ptr(now.Add(48*time.Hour)), nil, now)
	if !ok || !a.Equal(now.Add(48*time.Hour)) || !k.Equal(now) {
		t.Fatalf("a future as_of is answered with what is known now, got %v / %v", a, k)
	}
}
