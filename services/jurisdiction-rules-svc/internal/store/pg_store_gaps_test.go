package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// Audit gap 3 (maker-checker): the approval reference is the caller's own
// assertion, so the author of a rule activating it with any string was the
// author approving their own work. The author is refused; an independent
// principal succeeds, and the reference lands on the history row it opened.
func TestPgStore_TransitionRuleStatus_ActivationNeedsAnIndependentChecker(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "MC", nil)

	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "PAYROLL",
		RuleCode:             "MC_MIN_WAGE",
		RuleName:             "Minimum wage",
		EffectiveFrom:        time.Now().UTC().Add(-time.Hour),
		RulePayload:          []byte(`{"applies": true}`),
		RuleStatus:           "DRAFT",
		CreatedByPrincipalID: "maker",
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	activate := func(actor, ref string) (*domain.JurisdictionRule, bool, error) {
		return s.TransitionRuleStatus(ctx, store.TransitionParams{
			RuleID:                  r.JurisdictionRuleID,
			NewStatus:               "ACTIVE",
			AllowedPriors:           []string{"DRAFT"},
			ActorID:                 actor,
			RequireIndependentActor: true,
			ApprovalReference:       ref,
		})
	}

	if _, _, err := activate("maker", "APR-SELF"); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("the rule's author activating it: want ErrSelfApproval, got %v", err)
	}
	if got, err := s.FindRuleByID(ctx, r.JurisdictionRuleID); err != nil || got.RuleStatus != "DRAFT" {
		t.Fatalf("a refused self-approval must leave the rule DRAFT, got %+v / %v", got, err)
	}

	updated, transitioned, err := activate("checker", "APR-42")
	if err != nil || !transitioned || updated.RuleStatus != "ACTIVE" {
		t.Fatalf("an independent checker must activate: rule=%+v transitioned=%v err=%v", updated, transitioned, err)
	}

	var reason *string
	var changedBy string
	if err := pool.QueryRow(ctx, `
		SELECT change_reason, changed_by_principal_id FROM rule_status_history
		WHERE jurisdiction_rule_id = $1 AND known_to IS NULL`, r.JurisdictionRuleID).Scan(&reason, &changedBy); err != nil {
		t.Fatalf("read open history row: %v", err)
	}
	if reason == nil || *reason != "APR-42" || changedBy != "checker" {
		t.Fatalf("history must record the approval reference and the checker, got reason=%v by=%q", reason, changedBy)
	}
}

// Audit gap 6: the overlap check is a SELECT before the UPDATE, outside any
// lock, so two concurrent activations of overlapping rules could both pass it.
// The exclusion constraint (000006) must let exactly one through.
func TestPgStore_ConcurrentOverlappingActivationsAdmitExactlyOne(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "RC", nil)

	const n = 8
	ids := make([]string, n)
	for i := range ids {
		r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
			JurisdictionRuleID:   uuid.New().String(),
			JurisdictionID:       j.JurisdictionID,
			RuleDomain:           "TAX",
			RuleCode:             "RC_VAT",
			RuleName:             "VAT",
			EffectiveFrom:        time.Now().UTC().Add(-time.Duration(i+1) * time.Hour),
			RulePayload:          []byte(`{"applies": true}`),
			RuleStatus:           "DRAFT",
			CreatedByPrincipalID: "maker",
		})
		if err != nil {
			t.Fatalf("create draft %d: %v", i, err)
		}
		ids[i] = r.JurisdictionRuleID
	}

	errs := make(chan error, n)
	start := make(chan struct{})
	for _, id := range ids {
		go func(id string) {
			<-start
			_, _, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
				RuleID: id, NewStatus: "ACTIVE", AllowedPriors: []string{"DRAFT"}, ActorID: "checker",
			})
			errs <- err
		}(id)
	}
	close(start)

	var ok, overlap int
	for range ids {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrOverlappingRule):
			overlap++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || overlap != n-1 {
		t.Fatalf("want exactly 1 activation and %d overlap refusals, got %d / %d", n-1, ok, overlap)
	}
}

// Audit gap 7: status and effective_to are mutated in place, so a query could
// not answer "what did the platform know at T about date D". A supersede that
// end-dated a rule at now-1h hid it from every pack for a date after that,
// even when asked as of a moment before the supersede was recorded.
func TestPgStore_ResolveRulePack_KnownAtReplaysWhatWasKnown(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	now := time.Now().UTC()
	j := mustCreateJurisdiction(t, s, ctx, "BT", nil)

	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "BT_VAT",
		RuleName:             "VAT",
		EffectiveFrom:        now.Add(-6 * time.Hour),
		RulePayload:          []byte(`{"applies": true}`),
		RuleStatus:           "DRAFT",
		CreatedByPrincipalID: "maker",
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if _, _, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID: r.JurisdictionRuleID, NewStatus: "ACTIVE", AllowedPriors: []string{"DRAFT"}, ActorID: "checker",
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	// Pin the knowledge timeline: DRAFT known [now-5h, now-4h), ACTIVE known
	// from now-4h. The runtime rows all carry ~now, which no `known_at` in the
	// past could tell apart.
	if _, err := pool.Exec(ctx, `
		UPDATE rule_status_history SET
		  known_from = CASE rule_status WHEN 'DRAFT' THEN $2::timestamptz ELSE $3::timestamptz END,
		  known_to   = CASE rule_status WHEN 'DRAFT' THEN $3::timestamptz ELSE NULL END
		WHERE jurisdiction_rule_id = $1`, r.JurisdictionRuleID, now.Add(-5*time.Hour), now.Add(-4*time.Hour)); err != nil {
		t.Fatalf("pin history: %v", err)
	}
	// Supersede it now, end-dated at now-1h (back-dated, recorded now).
	end := now.Add(-time.Hour)
	if _, _, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID: r.JurisdictionRuleID, NewStatus: "SUPERSEDED", AllowedPriors: []string{"ACTIVE"},
		EndDate: true, EffectiveTo: &end, ActorID: "checker",
	}); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	at := now.Add(-30 * time.Minute) // after the back-dated end
	resolve := func(knownAt *time.Time) *domain.RulePack {
		t.Helper()
		p, err := s.ResolveRulePack(ctx, domain.RulePackQuery{JurisdictionID: j.JurisdictionID, At: at, KnownAt: knownAt})
		if err != nil {
			t.Fatalf("ResolveRulePack known_at=%v: %v", knownAt, err)
		}
		return p
	}
	has := func(p *domain.RulePack) bool {
		for _, rule := range p.Rules {
			if rule.JurisdictionRuleID == r.JurisdictionRuleID {
				return true
			}
		}
		return false
	}

	if has(resolve(nil)) {
		t.Error("current knowledge: the rule ended at now-1h and must not govern now-30m")
	}
	before := now.Add(-2 * time.Hour)
	if p := resolve(&before); !has(p) || p.Outcome != domain.RulePackResolved || p.KnownAt == nil {
		t.Errorf("known_at=now-2h: the platform then knew the rule ACTIVE and open-ended; want it RESOLVED, got %+v", p)
	}
	whileDraft := now.Add(-4*time.Hour - 30*time.Minute)
	if p := resolve(&whileDraft); has(p) || p.Outcome != domain.RulePackInsufficient {
		t.Errorf("known_at while DRAFT: the rule must not govern and the empty pack is INSUFFICIENT, got %+v", p)
	}
	unknown := now.Add(-7 * time.Hour)
	if p := resolve(&unknown); has(p) {
		t.Error("known_at before the rule was recorded: the rule must not exist")
	}
}

// Audit gap 8: an empty pack and a complete pack were indistinguishable. Each
// now carries an outcome, and a required code that resolves to nothing is
// listed and makes the pack INSUFFICIENT.
func TestPgStore_ResolveRulePack_OutcomeAndRequiredCodes(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "OC", nil)

	empty, err := s.ResolveRulePack(ctx, domain.RulePackQuery{JurisdictionID: j.JurisdictionID})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Outcome != domain.RulePackInsufficient {
		t.Fatalf("an empty pack must be INSUFFICIENT, got %q", empty.Outcome)
	}

	if _, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "OC_VAT",
		RuleName:             "VAT",
		EffectiveFrom:        time.Now().UTC().Add(-time.Hour),
		RulePayload:          []byte(`{"applies": true}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "maker",
	}); err != nil {
		t.Fatal(err)
	}

	full, err := s.ResolveRulePack(ctx, domain.RulePackQuery{JurisdictionID: j.JurisdictionID, RequiredCodes: []string{"OC_VAT"}})
	if err != nil {
		t.Fatal(err)
	}
	if full.Outcome != domain.RulePackResolved || len(full.Insufficient) != 0 {
		t.Fatalf("every required code resolved: want RESOLVED, got %q %v", full.Outcome, full.Insufficient)
	}

	partial, err := s.ResolveRulePack(ctx, domain.RulePackQuery{JurisdictionID: j.JurisdictionID, RequiredCodes: []string{"OC_VAT", "OC_WHT"}})
	if err != nil {
		t.Fatal(err)
	}
	if partial.Outcome != domain.RulePackInsufficient || len(partial.Insufficient) != 1 || partial.Insufficient[0] != "OC_WHT" {
		t.Fatalf("OC_WHT resolves to nothing: want INSUFFICIENT [OC_WHT], got %q %v", partial.Outcome, partial.Insufficient)
	}
	if len(partial.Rules) != 1 {
		t.Fatalf("the resolved rule must still be returned alongside the gap, got %d rules", len(partial.Rules))
	}
}
