package clock

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/jurisdiction"
	"zoiko.io/obligations-svc/internal/store"
	"zoiko.io/obligations-svc/internal/workflowclient"
)

type fakeStore struct {
	sweep     []store.WorkItem
	swept     int
	moveErr   error
	due       []store.WorkItem
	fired     int
	cases     []store.OutboxEvent
	delivered []error
	recorded  []string
}

func (f *fakeStore) ClocksNeedingSweep(context.Context, time.Time, int) ([]store.WorkItem, error) {
	return f.sweep, nil
}
func (f *fakeStore) SweepClock(context.Context, store.WorkItem, time.Time) (bool, error) {
	if f.moveErr != nil {
		return false, f.moveErr
	}
	f.swept++
	return true, nil
}
func (f *fakeStore) DueMilestones(context.Context, time.Time, int) ([]store.WorkItem, error) {
	return f.due, nil
}
func (f *fakeStore) FireMilestone(context.Context, store.WorkItem, time.Time) (bool, error) {
	f.fired++
	return true, nil
}
func (f *fakeStore) ClockStats(context.Context, time.Time) ([]store.OverdueCount, float64, error) {
	return nil, 0, nil
}
func (f *fakeStore) RelayOutboxDestination(ctx context.Context, dest string, _ int, deliver func(context.Context, store.OutboxEvent) error) (int, int, error) {
	if dest != store.DestinationWorkflowCase {
		return 0, 0, errors.New("wrong destination " + dest)
	}
	ok, bad := 0, 0
	for _, e := range f.cases {
		err := deliver(ctx, e)
		f.delivered = append(f.delivered, err)
		if err != nil {
			bad++
		} else {
			ok++
		}
	}
	return ok, bad, nil
}
func (f *fakeStore) RecordCaseOpened(_ context.Context, _ store.CaseRequest, caseID string) error {
	f.recorded = append(f.recorded, caseID)
	return nil
}

type fakeWorkflow struct {
	got []workflowclient.CaseRequest
	err error
}

func (f *fakeWorkflow) VerifyApproval(context.Context, string, string) (*workflowclient.Approval, error) {
	return nil, nil
}
func (f *fakeWorkflow) OpenCase(_ context.Context, r workflowclient.CaseRequest) (string, error) {
	f.got = append(f.got, r)
	if f.err != nil {
		return "", f.err
	}
	return "case-" + r.Milestone, nil
}

func caseEvent(t *testing.T, milestone string) store.OutboxEvent {
	raw, _ := json.Marshal(store.CaseRequest{TenantID: "t-1", LegalEntityID: "le-1", SourceType: "OBLIGATION_CLOCK", SourceID: "c-1",
		Milestone: milestone, CaseFamily: "LEGAL_COMPLIANCE", Confidentiality: "RESTRICTED", IdempotencyKey: "obligations:c-1:" + milestone})
	return store.OutboxEvent{TenantID: "t-1", EventType: "workflow.case.open_requested", EventPayload: raw}
}

func TestWFC05_WorkerTick_SweepsFiresAndOpensCases(t *testing.T) {
	st := &fakeStore{sweep: []store.WorkItem{{TenantID: "t-1", ID: "c-1"}, {TenantID: "t-2", ID: "c-2"}},
		due: []store.WorkItem{{TenantID: "t-1", ID: "m-1"}}, cases: []store.OutboxEvent{caseEvent(t, "m-1")}}
	wf := &fakeWorkflow{}
	res := NewWorker(st, wf, nil, zap.NewNop(), time.Minute).Tick(context.Background())
	if res.Moved != 2 || res.MilestonesFired != 1 || res.CasesOpened != 1 || res.CaseFailures != 0 {
		t.Fatalf("tick: %+v", res)
	}
	if len(wf.got) != 1 || wf.got[0].IdempotencyKey != "obligations:c-1:m-1" || wf.got[0].TenantID != "t-1" || wf.got[0].Confidentiality != "RESTRICTED" {
		t.Fatalf("case request sent: %+v", wf.got)
	}
	if len(st.recorded) != 1 || st.recorded[0] != "case-m-1" {
		t.Fatalf("opened case not recorded: %v", st.recorded)
	}
}

func TestWFC05_WorkerTick_ARefusedCaseStaysQueued(t *testing.T) {
	st := &fakeStore{cases: []store.OutboxEvent{caseEvent(t, "m-1")}}
	res := NewWorker(st, &fakeWorkflow{err: domain.ErrWorkflowUnavailable}, nil, zap.NewNop(), time.Minute).Tick(context.Background())
	if res.CaseFailures != 1 || len(st.recorded) != 0 || st.delivered[0] == nil {
		t.Fatalf("a refused case must fail its delivery (the relay retries it): %+v %v", res, st.recorded)
	}
	// Without workflow-svc configured the queue is left alone.
	st2 := &fakeStore{cases: []store.OutboxEvent{caseEvent(t, "m-1")}}
	if res := NewWorker(st2, nil, nil, zap.NewNop(), time.Minute).Tick(context.Background()); res.CasesOpened != 0 || len(st2.delivered) != 0 {
		t.Fatalf("no workflow client, but cases were delivered: %+v", res)
	}
}

func TestWFC05_WorkerTick_AFailedMoveDoesNotStopTheSweep(t *testing.T) {
	st := &fakeStore{sweep: []store.WorkItem{{ID: "a"}, {ID: "b"}}, moveErr: errors.New("db down"), due: []store.WorkItem{{ID: "m"}}}
	res := NewWorker(st, nil, nil, zap.NewNop(), time.Minute).Tick(context.Background())
	if res.Moved != 0 || res.MilestonesFired != 1 {
		t.Fatalf("tick after failed moves: %+v", res)
	}
}

// ── calendar consumer (OB-S8) ────────────────────────────────────────────────

type fakeCalStore struct {
	consumed   map[string]bool
	older      []store.WorkItem
	superseded []domain.StartClockParams
	failIDs    map[string]bool
	marks      int
}

func (f *fakeCalStore) EventConsumed(_ context.Context, id string) (bool, error) {
	return f.consumed[id], nil
}
func (f *fakeCalStore) MarkEventConsumed(_ context.Context, id, _ string, _ map[string]any) (bool, error) {
	f.marks++
	f.consumed[id] = true
	return true, nil
}
func (f *fakeCalStore) ClocksOnOlderCalendar(context.Context, string, int, int) ([]store.WorkItem, error) {
	return f.older, nil
}
func (f *fakeCalStore) GetClockForTenant(_ context.Context, tenant, id string) (*domain.Clock, error) {
	return &domain.Clock{ClockID: id, TenantID: tenant, RowVersion: 2, DueAt: time.Date(2027, 1, 7, 23, 59, 59, 0, time.UTC)}, nil
}
func (f *fakeCalStore) ClockCalculationRequest(context.Context, string, string) (json.RawMessage, error) {
	return json.RawMessage(`{"jurisdiction":"GB","obligation_code":"VAT_RETURN","period_end":"2026-11-30","pack_ref":"gb-tax","pack_version":"1.0.0"}`), nil
}
func (f *fakeCalStore) SupersedeClock(_ context.Context, id string, _ int, p domain.StartClockParams) (*domain.Clock, *domain.Clock, bool, error) {
	if f.failIDs[id] {
		return nil, nil, false, errors.New("boom")
	}
	f.superseded = append(f.superseded, p)
	return &domain.Clock{}, &domain.Clock{}, true, nil
}

type fakeCalc struct {
	sys  []jurisdiction.CalculationRequest
	keys []string
}

func (f *fakeCalc) Calculate(_ context.Context, req jurisdiction.CalculationRequest, key string) (*domain.Calculation, error) {
	f.sys = append(f.sys, req)
	f.keys = append(f.keys, key)
	due := time.Date(2027, 1, 8, 23, 59, 59, 0, time.UTC)
	return &domain.Calculation{Outcome: domain.CalculationOutcomeCalculated, DueAt: &due, CalendarVersion: 2, CalendarVersionID: "cv-2"}, nil
}

func calendarEvent(id, typ string) []byte {
	raw, _ := json.Marshal(map[string]any{"event_id": id, "event_type": typ,
		"payload": map[string]any{"calendar_code": "gb-hmrc", "calendar_version": 2, "calendar_version_id": "cv-2"}})
	return raw
}

func TestWFC05_CalendarCorrection_SupersedesOnceAndIsReplaySafe(t *testing.T) {
	st := &fakeCalStore{consumed: map[string]bool{}, older: []store.WorkItem{{TenantID: "t-1", ID: "c-1"}, {TenantID: "t-2", ID: "c-2"}}}
	calc := &fakeCalc{}
	c := NewCalendarConsumer(st, calc, "svc-obligations", nil, zap.NewNop())
	res, err := c.Handle(context.Background(), calendarEvent("evt-1", EventBusinessCalendarUpdated))
	if err != nil || res.Superseded != 2 {
		t.Fatalf("first delivery: %+v %v", res, err)
	}
	for _, p := range st.superseded {
		if *p.SupersessionReason != "CALENDAR_CORRECTED" || p.SystemTenantID == "" || p.CausationID != "evt-1" {
			t.Fatalf("successor params: %+v", p)
		}
	}
	for _, r := range calc.sys {
		if r.PackRef != "" || r.PackVersion != "" {
			t.Fatalf("a correction must be asked of the pack in force, not the pinned one: %+v", r)
		}
	}
	res, err = c.Handle(context.Background(), calendarEvent("evt-1", EventBusinessCalendarUpdated))
	if err != nil || !res.Replayed || len(st.superseded) != 2 {
		t.Fatalf("replay applied again: %+v %v", res, err)
	}
}

func TestWFC05_CalendarCorrection_AFailureIsRetriedNotConsumed(t *testing.T) {
	st := &fakeCalStore{consumed: map[string]bool{}, older: []store.WorkItem{{TenantID: "t-1", ID: "c-1"}}, failIDs: map[string]bool{"c-1": true}}
	c := NewCalendarConsumer(st, &fakeCalc{}, "svc-obligations", nil, zap.NewNop())
	if _, err := c.Handle(context.Background(), calendarEvent("evt-9", EventCalendarChanged)); err == nil {
		t.Fatal("a failed recalculation must be retried")
	}
	if st.consumed["evt-9"] || st.marks != 0 {
		t.Fatal("a failed event was marked consumed")
	}
	if res, err := c.Handle(context.Background(), calendarEvent("evt-x", "jurisdiction.rule.updated")); err != nil || !res.Ignored {
		t.Fatalf("another event type: %+v %v", res, err)
	}
	if res, _ := c.Handle(context.Background(), []byte("not json")); !res.Ignored {
		t.Fatal("a poison message must be skipped")
	}
}
