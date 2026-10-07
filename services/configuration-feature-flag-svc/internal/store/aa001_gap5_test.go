package store_test

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// ── Audit 2026-09-23, gap 5: rollout_percentage is evaluated, deterministically
//
// The audit: the percentage was stored and never evaluated, so each consumer
// bucketed on its own and two services disagreed about the same principal.
// Evaluation is now server-side; these tests hold it to INV-07 — the same
// subject always gets the same answer, the share enabled tracks the
// percentage, and raising a rollout never takes the feature away from anyone
// who already had it.

func setRollout(t *testing.T, s *store.PgStore, pct int) {
	t.Helper()
	if _, _, err := s.UpsertFeatureFlag(context.Background(), domain.UpsertFeatureFlagParams{
		Key: "checkout.new_ui", Enabled: true, RolloutPercentage: pct, Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil {
		t.Fatalf("set rollout %d: %v", pct, err)
	}
}

func enabledSet(t *testing.T, s *store.PgStore, n int) map[string]bool {
	t.Helper()
	on := map[string]bool{}
	for i := 0; i < n; i++ {
		subject := fmt.Sprintf("user-%d", i)
		ev, err := s.EvaluateFlag(context.Background(), domain.EvaluateFlagParams{
			Key: "checkout.new_ui", Environment: "staging", SubjectKey: subject,
		})
		if err != nil {
			t.Fatalf("evaluate %s: %v", subject, err)
		}
		if ev.Enabled {
			on[subject] = true
		}
	}
	return on
}

func TestGap5_RolloutIsDeterministicProportionalAndMonotonic(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedFlag(t, pool, "checkout.new_ui")
	const n = 2000

	setRollout(t, s, 30)
	at30 := enabledSet(t, s, n)
	if again := enabledSet(t, s, n); len(again) != len(at30) {
		t.Fatalf("the same subjects must get the same answers: %d then %d enabled", len(at30), len(again))
	}
	if share := float64(len(at30)) / n; share < 0.25 || share > 0.35 {
		t.Errorf("30%% rollout enabled %.1f%% of subjects", share*100)
	}

	setRollout(t, s, 60)
	at60 := enabledSet(t, s, n)
	lost := 0
	for subject := range at30 {
		if !at60[subject] {
			lost++
		}
	}
	if lost != 0 {
		t.Errorf("raising the rollout 30%%→60%% took the feature away from %d of %d subjects who had it", lost, len(at30))
	}
	if share := float64(len(at60)) / n; share < 0.55 || share > 0.65 {
		t.Errorf("60%% rollout enabled %.1f%% of subjects", share*100)
	}

	setRollout(t, s, 0)
	if on := enabledSet(t, s, 200); len(on) != 0 {
		t.Errorf("0%% rollout enabled %d subjects", len(on))
	}
	setRollout(t, s, 100)
	if on := enabledSet(t, s, 200); len(on) != 200 {
		t.Errorf("100%% rollout enabled only %d of 200 subjects", len(on))
	}
}
