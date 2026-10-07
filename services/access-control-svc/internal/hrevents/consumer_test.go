package hrevents_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/hrevents"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
)

type fakeLinks struct {
	links    map[string]*domain.SubjectLink // by employee id
	observed int
}

func (f *fakeLinks) FindSubjectLink(ctx context.Context, emp string) (*domain.SubjectLink, error) {
	if svcmiddleware.TenantFromContext(ctx) == "" {
		return nil, errors.New("no tenant on the context")
	}
	l, ok := f.links[emp]
	if !ok {
		return nil, nil
	}
	cp := *l
	return &cp, nil
}

func (f *fakeLinks) ObserveSubject(_ context.Context, emp, mgr, status string) (*domain.SubjectLink, error) {
	l, ok := f.links[emp]
	if !ok {
		return nil, nil
	}
	before := *l
	f.observed++
	if mgr != "" {
		l.LastManagerEmployeeID = mgr
	}
	if status != "" {
		l.LastStatus = status
	}
	return &before, nil
}

type fakeTrigger struct {
	calls []domain.ReviewTrigger
	err   error
}

func (f *fakeTrigger) TriggerReview(_ context.Context, t domain.ReviewTrigger) (*domain.ReviewCampaign, bool, error) {
	f.calls = append(f.calls, t)
	if f.err != nil {
		return nil, false, f.err
	}
	return &domain.ReviewCampaign{CampaignID: "c-" + t.CorrelationID}, true, nil
}

var errOutage = errors.New("authorization-svc unavailable")

func isTransient(err error) bool { return errors.Is(err, errOutage) }

func rig() (*fakeLinks, *fakeTrigger, *hrevents.Handler) {
	links := &fakeLinks{links: map[string]*domain.SubjectLink{
		"E-1":  {EmployeeID: "E-1", PrincipalID: "user-1", LegalEntityID: "le-1", LastManagerEmployeeID: "E-M1", LastStatus: "ACTIVE"},
		"E-M1": {EmployeeID: "E-M1", PrincipalID: "mgr-1", LegalEntityID: "le-1"},
		"E-M2": {EmployeeID: "E-M2", PrincipalID: "mgr-2", LegalEntityID: "le-1"},
	}}
	tr := &fakeTrigger{}
	return links, tr, hrevents.NewHandler(links, tr, isTransient, zap.NewNop())
}

func event(t *testing.T, id, typ string, payload map[string]any) []byte {
	t.Helper()
	raw, _ := json.Marshal(payload)
	b, _ := json.Marshal(map[string]any{"event_id": id, "event_type": typ, "tenant_id": "tenant-abc", "legal_entity_id": "le-1", "payload": json.RawMessage(raw)})
	return b
}

func TestMoverOpensReviewByNewManager(t *testing.T) {
	links, tr, h := rig()
	out, err := h.Handle(context.Background(), event(t, "ev-1", "employee.updated", map[string]any{"employee_id": "E-1", "manager_employee_id": "E-M2", "status": "ACTIVE"}))
	if err != nil || out != hrevents.OutcomeOpened {
		t.Fatalf("outcome = %s %v", out, err)
	}
	c := tr.calls[0]
	if c.SubjectPrincipalID != "user-1" || c.ReviewerPrincipalID != "mgr-2" || c.CorrelationID != "hr-ev-1" || !strings.HasPrefix(c.Reason, "mover:") || c.TenantID != "tenant-abc" {
		t.Fatalf("trigger = %+v", c)
	}
	if links.links["E-1"].LastManagerEmployeeID != "E-M2" {
		t.Errorf("the new manager must be recorded")
	}
	// The same manager again is not a move.
	out, _ = h.Handle(context.Background(), event(t, "ev-2", "employee.updated", map[string]any{"employee_id": "E-1", "manager_employee_id": "E-M2"}))
	if out != hrevents.OutcomeNoTrigger || len(tr.calls) != 1 {
		t.Errorf("an unchanged manager opened a review: %s %d", out, len(tr.calls))
	}
}

// employee.updated carries only the new manager: the first one seen is a
// baseline, not a change.
func TestFirstSightingOfManagerIsABaseline(t *testing.T) {
	links, tr, h := rig()
	links.links["E-1"].LastManagerEmployeeID = ""
	out, _ := h.Handle(context.Background(), event(t, "ev-1", "employee.updated", map[string]any{"employee_id": "E-1", "manager_employee_id": "E-M2"}))
	if out != hrevents.OutcomeNoTrigger || len(tr.calls) != 0 || links.links["E-1"].LastManagerEmployeeID != "E-M2" {
		t.Fatalf("outcome = %s, calls = %d, manager = %q", out, len(tr.calls), links.links["E-1"].LastManagerEmployeeID)
	}
}

// Both producers publish employee.terminated for one exit: one review.
func TestTerminationFromTwoProducersOpensOneReview(t *testing.T) {
	_, tr, h := rig()
	out, _ := h.Handle(context.Background(), event(t, "ev-em", "employee.terminated", map[string]any{"employee_id": "E-1"}))
	if out != hrevents.OutcomeOpened || tr.calls[0].ReviewerPrincipalID != "mgr-1" {
		t.Fatalf("first: %s %+v", out, tr.calls)
	}
	out, _ = h.Handle(context.Background(), event(t, "ev-off", "employee.terminated", map[string]any{"employee_id": "E-1", "termination_type": "VOLUNTARY"}))
	if out != hrevents.OutcomeNoTrigger || len(tr.calls) != 1 {
		t.Fatalf("second producer opened another review: %s %d", out, len(tr.calls))
	}
}

func TestContractorEnd(t *testing.T) {
	_, tr, h := rig()
	out, _ := h.Handle(context.Background(), event(t, "ev-1", "employee.status.changed", map[string]any{
		"employee_id": "E-1", "old_status": "ACTIVE", "new_status": "INACTIVE", "worker_type": "CONTRACTOR"}))
	if out != hrevents.OutcomeOpened || !strings.HasPrefix(tr.calls[0].Reason, "contractor end:") {
		t.Fatalf("outcome = %s %+v", out, tr.calls)
	}
}

// Temporary states are not exits.
func TestLeaveIsNotALeaver(t *testing.T) {
	_, tr, h := rig()
	out, _ := h.Handle(context.Background(), event(t, "ev-1", "employee.status.changed", map[string]any{
		"employee_id": "E-1", "old_status": "ACTIVE", "new_status": "ON_LEAVE"}))
	if out != hrevents.OutcomeNoTrigger || len(tr.calls) != 0 {
		t.Fatalf("ON_LEAVE opened a review: %s", out)
	}
}

// Never guessed: an unlinked employee opens nothing.
func TestUnlinkedEmployeeIsSkipped(t *testing.T) {
	_, tr, h := rig()
	out, err := h.Handle(context.Background(), event(t, "ev-1", "employee.terminated", map[string]any{"employee_id": "E-404"}))
	if err != nil || out != hrevents.OutcomeUnlinked || len(tr.calls) != 0 {
		t.Fatalf("outcome = %s %v calls=%d", out, err, len(tr.calls))
	}
}

// An outage is retried, and nothing is recorded first: the retry must see
// the same state and open the same (idempotent) review.
func TestOutageIsRetriedWithoutRecording(t *testing.T) {
	links, tr, h := rig()
	tr.err = errOutage
	msg := event(t, "ev-1", "employee.terminated", map[string]any{"employee_id": "E-1"})
	if _, err := h.Handle(context.Background(), msg); err == nil {
		t.Fatal("an outage must surface as a retryable error")
	}
	if links.observed != 0 || links.links["E-1"].LastStatus != "ACTIVE" {
		t.Fatalf("observation recorded before the review was opened")
	}
	tr.err = nil
	out, err := h.Handle(context.Background(), msg)
	if err != nil || out != hrevents.OutcomeOpened || tr.calls[1].CorrelationID != tr.calls[0].CorrelationID {
		t.Fatalf("retry: %s %v %+v", out, err, tr.calls)
	}
}

// A refusal redelivery cannot fix is final: recorded, not retried forever.
func TestRefusalIsFinal(t *testing.T) {
	links, tr, h := rig()
	tr.err = domain.ErrEscalationReviewerReq
	out, err := h.Handle(context.Background(), event(t, "ev-1", "employee.terminated", map[string]any{"employee_id": "E-1"}))
	if err != nil || out != hrevents.OutcomeRefused || links.observed != 1 {
		t.Fatalf("outcome = %s %v observed=%d", out, err, links.observed)
	}
}

func TestMalformedAndForeignEvents(t *testing.T) {
	_, _, h := rig()
	if out, _ := h.Handle(context.Background(), []byte("{")); out != hrevents.OutcomeMalformed {
		t.Errorf("garbage = %s", out)
	}
	if out, _ := h.Handle(context.Background(), event(t, "ev-1", "employee.hired", map[string]any{"employee_id": "E-1"})); out != hrevents.OutcomeIgnored {
		t.Errorf("hired = %s", out)
	}
	if out, _ := h.Handle(context.Background(), event(t, "ev-1", "employee.terminated", map[string]any{})); out != hrevents.OutcomeMalformed {
		t.Errorf("no employee = %s", out)
	}
}
