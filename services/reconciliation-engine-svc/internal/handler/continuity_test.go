package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
	"zoiko.io/reconciliation-engine-svc/internal/source"
)

// ZS-SVC-Z-001 DQC-03, cross-period roll-forward (step I): a population whose declared opening is
// not the previous certified period's closing is not reproducible, and nothing is frozen.

type rollFetcher struct{ rf *domain.DeclaredRollForward }

func (f rollFetcher) Fetch(_ context.Context, spec domain.PopulationSpec, _ source.Scope) (*source.Fetched, error) {
	got := &source.Fetched{Watermark: "wm", DeclaredRowCount: 1, DeclaredTotals: map[string]string{"USD": "10.00"},
		Records: []domain.PopulationRecord{{RecordID: "r1", Reference: "R-" + spec.Side, Amount: "10.00", Currency: "USD", Date: "2026-09-02"}}}
	if spec.Side == "A" && f.rf != nil {
		got.Controls.RollForward = f.rf
		got.Outcomes = []domain.ControlOutcome{{Control: domain.ControlRollForward, Status: domain.OutcomeVerified}}
	}
	return got, nil
}

type continuityStore struct {
	*fakeStore
	prior *domain.PriorClosing
	asked []string
}

func (s *continuityStore) PriorClosing(_ context.Context, _, entity, specRef, side, periodStart string) (*domain.PriorClosing, error) {
	s.asked = append(s.asked, entity+"|"+specRef+"|"+side+"|"+periodStart)
	return s.prior, nil
}

func rollForward(open string) *domain.DeclaredRollForward {
	return &domain.DeclaredRollForward{Opening: map[string]string{"USD": open}, Closing: map[string]string{"USD": "110.00"},
		PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30"}
}

func startOn(t *testing.T, rf *domain.DeclaredRollForward, prior *domain.PriorClosing) (*continuityStore, int, string) {
	t.Helper()
	st := &continuityStore{fakeStore: &fakeStore{def: definition()}, prior: prior}
	h := NewHandler(st, &recordingAuthz{}, rollFetcher{rf: rf}, zap.NewNop())
	w := serve(h, http.MethodPost, "/v1/reconciliation/runs", `{"definition_id":"rdf_1","period_id":"2026-09"}`)
	return st, w.Code, w.Body.String()
}

func TestStartRun_OpeningThatContinuesThePreviousClosingProceeds(t *testing.T) {
	st, code, body := startOn(t, rollForward("100.00"), &domain.PriorClosing{RunID: "rcr_aug", PeriodEnd: "2026-08-31", Closing: map[string]string{"USD": "100"}})
	if code != http.StatusCreated || st.startCalls != 1 || len(st.failures) != 0 {
		t.Fatalf("%d %s, starts %d, failures %v", code, body, st.startCalls, st.failures)
	}
	if len(st.asked) != 1 || !strings.Contains(st.asked[0], entityA) || !strings.HasSuffix(st.asked[0], "|A|2026-09-01") {
		t.Fatalf("the prior closing was asked for %v", st.asked)
	}
}

func TestStartRun_BrokenContinuityFailsTheRunClosed(t *testing.T) {
	for name, prior := range map[string]*domain.PriorClosing{
		"opening differs from the previous closing": {RunID: "rcr_aug", PeriodEnd: "2026-08-31", Closing: map[string]string{"USD": "99.99"}},
		"a period is missing in between":           {RunID: "rcr_jul", PeriodEnd: "2026-07-31", Closing: map[string]string{"USD": "100"}},
		"a currency appears from nowhere":          {RunID: "rcr_aug", PeriodEnd: "2026-08-31", Closing: map[string]string{"USD": "100", "EUR": "5"}},
	} {
		st, code, body := startOn(t, rollForward("100.00"), prior)
		if code != http.StatusUnprocessableEntity || st.startCalls != 0 || len(st.failures) != 1 ||
			st.failures[0].Code != domain.FailureSourceInconsistent || !strings.Contains(body, `"result_state":"INDETERMINATE"`) {
			t.Fatalf("%s: %d %s, starts %d, failures %+v", name, code, body, st.startCalls, st.failures)
		}
	}
}

func TestStartRun_NoPriorPeriodIsRecordedNotPassed(t *testing.T) {
	st, code, _ := startOn(t, rollForward("100.00"), nil)
	if code != http.StatusCreated || st.startCalls != 1 {
		t.Fatalf("a first period must be allowed: %d", code)
	}
	// What was frozen says the opening was not verified: NO_PRIOR_PERIOD, never VERIFIED.
	h := &Handler{store: st}
	pops := []domain.FrozenPopulation{{Side: "A", SpecRef: "ar/p", Controls: domain.DeclaredControls{RollForward: rollForward("100.00")}}}
	if err := h.checkContinuity(context.Background(), "t", entityA, pops); err != nil {
		t.Fatal(err)
	}
	if o := pops[0].ControlOutcomes; len(o) != 1 || o[0].Control != domain.ControlContinuity || o[0].Status != domain.OutcomeNoPriorPeriod {
		t.Fatalf("outcomes = %+v", o)
	}
}

func TestStartRun_ASourceWithoutARollForwardIsNotAskedForAPriorClosing(t *testing.T) {
	st, code, _ := startOn(t, nil, &domain.PriorClosing{RunID: "x", PeriodEnd: "2026-08-31", Closing: map[string]string{"USD": "1"}})
	if code != http.StatusCreated || len(st.asked) != 0 {
		t.Fatalf("%d, prior closing asked for %v", code, st.asked)
	}
}
