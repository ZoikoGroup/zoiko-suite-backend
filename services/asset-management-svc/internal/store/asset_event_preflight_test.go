package store_test

import (
	"errors"
	"testing"

	"zoiko.io/asset-management-svc/internal/domain"
)

// PreflightApplyAssetEvent must (1) pass for an applicable event WITHOUT
// mutating anything, and (2) surface the same refusals ApplyAssetEvent would,
// so the handler can refuse before posting a GL journal.
func TestPreflightApply_ValidEvent_PassesAndMutatesNothing(t *testing.T) {
	e := newRebaseEnv(t)
	e.runPeriod(true)
	e.runPeriod(true)
	e.runPeriod(true) // carrying 9000, remaining 9

	ev := e.newEvent(domain.AssetEventTypeImpairment, "book-1", 1800)
	if err := e.s.PreflightApplyAssetEvent(e.ctx, ev.EventID, e.tick()); err != nil {
		t.Fatalf("preflight of an applicable event must pass, got %v", err)
	}

	e.expectSchedule("after preflight", 12000, 12, 1) // untouched: still version 1, original basis and life
	got, err := e.s.GetAssetEvent(e.ctx, ev.EventID)
	if err != nil || got.Status != domain.AssetEventStatusApproved {
		t.Fatalf("event must stay APPROVED after a preflight, got %+v (err %v)", got, err)
	}
	if e.effectCount(ev.EventID) != 0 {
		t.Fatalf("a preflight must write no effects row")
	}

	// The real apply still works afterwards (preflight left nothing behind).
	if err := e.apply(ev); err != nil {
		t.Fatalf("apply after preflight failed: %v", err)
	}
	e.expectSchedule("after apply", 7200, 9, 2)
}

func TestPreflightApply_SurfacesTheSameRefusalsAsApply(t *testing.T) {
	e := newRebaseEnv(t)
	e.runPeriod(true)
	e.runPeriod(true)
	e.runPeriod(true) // carrying 9000

	tooBig := e.newEvent(domain.AssetEventTypeImpairment, "book-1", 9500)
	if err := e.s.PreflightApplyAssetEvent(e.ctx, tooBig.EventID, e.tick()); !errors.Is(err, domain.ErrImpairmentExceedsCarrying) {
		t.Fatalf("expected ErrImpairmentExceedsCarrying from preflight, got %v", err)
	}

	e.runPeriod(false) // a run now in flight against the current schedule version
	ok := e.newEvent(domain.AssetEventTypeAddition, "book-1", 100)
	if err := e.s.PreflightApplyAssetEvent(e.ctx, ok.EventID, e.tick()); !errors.Is(err, domain.ErrDepreciationRunInFlight) {
		t.Fatalf("expected ErrDepreciationRunInFlight from preflight, got %v", err)
	}
	e.expectSchedule("refusals leave schedule untouched", 12000, 12, 1)
}
