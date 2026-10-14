package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

func newVersionedRule(t *testing.T, s *store.PgStore, jID, code string, mut func(*domain.CreateRuleParams)) *domain.JurisdictionRule {
	t.Helper()
	p := domain.CreateRuleParams{
		JurisdictionRuleID:   uuid.New().String(),
		JurisdictionID:       jID,
		RuleDomain:           "TAX",
		RuleCode:             code,
		RuleName:             code,
		EffectiveFrom:        time.Now().UTC().Add(-time.Hour),
		RulePayload:          []byte(`{"applies": true}`),
		RuleStatus:           "DRAFT",
		CreatedByPrincipalID: "maker",
	}
	if mut != nil {
		mut(&p)
	}
	r, _, err := s.CreateRule(context.Background(), p)
	if err != nil {
		t.Fatalf("create %s: %v", code, err)
	}
	return r
}

func activateRule(t *testing.T, s *store.PgStore, id string) *domain.JurisdictionRule {
	t.Helper()
	r, _, err := s.TransitionRuleStatus(context.Background(), store.TransitionParams{
		RuleID: id, NewStatus: "ACTIVE", AllowedPriors: []string{"DRAFT"}, ActorID: "checker",
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	return r
}

// Re-audit R-1 (rows 48/49): precedence_level could not be set, so every
// rule was 0 and the pack fell through to nearest-wins. A declared higher
// precedence on the parent now beats the nearer child.
func TestPgStore_PrecedenceLevelDecidesTheWinner(t *testing.T) {
	s, _, ctx := newTestStore(t)
	country := mustCreateJurisdiction(t, s, ctx, "PC", nil)
	state := mustCreateJurisdiction(t, s, ctx, "PS", &country.JurisdictionID)

	parent := newVersionedRule(t, s, country.JurisdictionID, "P_VAT", func(p *domain.CreateRuleParams) { p.PrecedenceLevel = 1 })
	child := newVersionedRule(t, s, state.JurisdictionID, "P_VAT", func(p *domain.CreateRuleParams) { p.PrecedenceLevel = 5 })
	activateRule(t, s, parent.JurisdictionRuleID)
	activateRule(t, s, child.JurisdictionRuleID)

	if parent.PrecedenceLevel != 1 {
		t.Fatalf("precedence_level not stored: %d", parent.PrecedenceLevel)
	}
	pack, err := s.FindRulePack(ctx, state.JurisdictionID, "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Rules) != 1 || pack.Rules[0].JurisdictionRuleID != parent.JurisdictionRuleID {
		t.Fatalf("precedence 1 (parent) must beat precedence 5 (nearer child), got %+v", pack.Rules)
	}
}

// Row 48 (immutable versions, supersedes chain) and the rule_version=0 bug.
func TestPgStore_VersionChainAndImmutability(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "VC", nil)

	v1 := newVersionedRule(t, s, j.JurisdictionID, "V_RATE", nil)
	if v1.RuleVersion != 1 {
		t.Fatalf("a new rule is version 1, got %d (the create path stored 0)", v1.RuleVersion)
	}
	activateRule(t, s, v1.JurisdictionRuleID)

	v2 := newVersionedRule(t, s, j.JurisdictionID, "V_RATE", func(p *domain.CreateRuleParams) {
		p.SupersedesRuleID = &v1.JurisdictionRuleID
		p.EffectiveFrom = time.Now().UTC()
	})
	if v2.RuleVersion != 2 || v2.SupersedesRuleID == nil || *v2.SupersedesRuleID != v1.JurisdictionRuleID {
		t.Fatalf("successor must be version 2 superseding v1, got %+v", v2)
	}

	base := func(code string) domain.CreateRuleParams {
		return domain.CreateRuleParams{
			JurisdictionRuleID: uuid.New().String(), JurisdictionID: j.JurisdictionID, RuleDomain: "TAX", RuleCode: code,
			RuleName: code, EffectiveFrom: time.Now().UTC().Add(time.Hour), RulePayload: []byte(`{}`), RuleStatus: "DRAFT",
			CreatedByPrincipalID: "maker",
		}
	}

	fork := base("V_RATE")
	fork.SupersedesRuleID = &v1.JurisdictionRuleID
	if _, _, err := s.CreateRule(ctx, fork); !errors.Is(err, domain.ErrSupersedesTaken) {
		t.Fatalf("second successor: want ErrSupersedesTaken, got %v", err)
	}

	cross := base("OTHER")
	cross.SupersedesRuleID = &v2.JurisdictionRuleID
	if _, _, err := s.CreateRule(ctx, cross); !errors.Is(err, domain.ErrInvalidSupersedes) {
		t.Fatalf("cross-code supersedes: want ErrInvalidSupersedes, got %v", err)
	}

	wrongVersion := base("NEW")
	wrongVersion.RuleVersion = 7
	if _, _, err := s.CreateRule(ctx, wrongVersion); !errors.Is(err, domain.ErrRuleVersionMismatch) {
		t.Fatalf("rule_version 7 on a new rule: want ErrRuleVersionMismatch, got %v", err)
	}

	// Past DRAFT the content is immutable in the database, whoever writes it.
	if _, err := pool.Exec(ctx, `UPDATE jurisdiction_rules SET rule_payload = '{"rate":"0.99"}' WHERE jurisdiction_rule_id = $1`, v1.JurisdictionRuleID); err == nil {
		t.Fatal("an ACTIVE rule's payload must not be updatable")
	}
	if _, err := pool.Exec(ctx, `UPDATE jurisdiction_rules SET precedence_level = 9 WHERE jurisdiction_rule_id = $1`, v1.JurisdictionRuleID); err == nil {
		t.Fatal("an ACTIVE rule's precedence must not be updatable")
	}
	// A DRAFT is still editable.
	if _, err := pool.Exec(ctx, `UPDATE jurisdiction_rules SET rule_name = 'renamed' WHERE jurisdiction_rule_id = $1`, v2.JurisdictionRuleID); err != nil {
		t.Fatalf("a DRAFT must stay editable: %v", err)
	}
}

// Row 43: If-Match. The rule's row_version moves on every change; a write
// naming an old one is refused, on both transition and drift.
func TestPgStore_IfMatchRefusesAStaleVersion(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "IM", nil)
	r := newVersionedRule(t, s, j.JurisdictionID, "IM_VAT", nil)
	read := r.RowVersion

	active := activateRule(t, s, r.JurisdictionRuleID)
	if active.RowVersion <= read {
		t.Fatalf("row_version must increase on update: %d -> %d", read, active.RowVersion)
	}

	stale := read
	_, _, err := s.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID: r.JurisdictionRuleID, NewStatus: "RETIRED", AllowedPriors: []string{"ACTIVE", "SUPERSEDED"},
		EndDate: true, ActorID: "checker", ExpectedRowVersion: &stale,
	})
	if !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("transition with a stale If-Match: want ErrPreconditionFailed, got %v", err)
	}
	_, _, _, err = s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID: r.JurisdictionRuleID, ToState: "DRIFTED", RecordedByPrincipalID: "checker", ExpectedRowVersion: &stale,
	})
	if !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("drift with a stale If-Match: want ErrPreconditionFailed, got %v", err)
	}

	current := active.RowVersion
	drifted, _, changed, err := s.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID: r.JurisdictionRuleID, ToState: "DRIFTED", RecordedByPrincipalID: "checker", ExpectedRowVersion: &current,
	})
	if err != nil || !changed || drifted.RowVersion <= current {
		t.Fatalf("drift with the current If-Match must apply and bump the version: %v changed=%v", err, changed)
	}
}

// Row 56: the pack digest pins an answer — stable across identical calls,
// different once a rule in it changes.
func TestPgStore_PackDigestPinsTheAnswer(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "PD", nil)
	r := newVersionedRule(t, s, j.JurisdictionID, "PD_VAT", nil)
	activateRule(t, s, r.JurisdictionRuleID)
	at := time.Now().UTC()

	a, err := s.FindRulePack(ctx, j.JurisdictionID, "", at)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.FindRulePack(ctx, j.JurisdictionID, "", at)
	if len(a.Rules) != 1 {
		t.Fatalf("the active rule must be in the pack at %s, got %d rules (outcome %s)", at, len(a.Rules), a.Outcome)
	}
	if a.PackDigest == "" || a.PackDigest != b.PackDigest {
		t.Fatalf("the same question must give the same digest: %q vs %q", a.PackDigest, b.PackDigest)
	}
	if _, _, _, err := s.RecordDrift(ctx, domain.RecordDriftParams{JurisdictionRuleID: r.JurisdictionRuleID, ToState: "DRIFTED", RecordedByPrincipalID: "x"}); err != nil {
		t.Fatal(err)
	}
	c, _ := s.FindRulePack(ctx, j.JurisdictionID, "", at)
	if c.PackDigest == a.PackDigest {
		t.Fatal("a change to a rule in the pack must change the digest")
	}
}
