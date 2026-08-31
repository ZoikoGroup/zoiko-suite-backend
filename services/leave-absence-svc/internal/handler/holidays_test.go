package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/leave-absence-svc/internal/domain"
)

// ── holiday store stubs ────────────────────────────────────────────────────────

func (s *stubStore) CreateHoliday(_ context.Context, h *domain.Holiday) error {
	// Mirrors idx_holidays_tenant_entity_date: unique per entity per date, but
	// only across ACTIVE rows.
	for _, existing := range s.holidays {
		if existing.LegalEntityID == h.LegalEntityID &&
			existing.Date == h.Date &&
			existing.Status == "ACTIVE" {
			return domain.ErrHolidayDateExists
		}
	}
	s.holidays[h.HolidayID] = h
	return nil
}

func (s *stubStore) ListHolidays(_ context.Context, f domain.HolidayFilter) ([]domain.Holiday, error) {
	var out []domain.Holiday
	for _, h := range s.holidays {
		if f.LegalEntityID != "" && h.LegalEntityID != f.LegalEntityID {
			continue
		}
		if !f.IncludeInactive && h.Status != "ACTIVE" {
			continue
		}
		if f.From != "" && h.Date < f.From {
			continue
		}
		if f.To != "" && h.Date > f.To {
			continue
		}
		out = append(out, *h)
	}
	// The real store orders by date; tests depend on that.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Date < out[j-1].Date; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func (s *stubStore) GetHoliday(_ context.Context, id string) (*domain.Holiday, error) {
	h, ok := s.holidays[id]
	if !ok {
		return nil, domain.ErrHolidayNotFound
	}
	return h, nil
}

func (s *stubStore) DeactivateHoliday(_ context.Context, id string) error {
	h, ok := s.holidays[id]
	if !ok || h.Status != "ACTIVE" {
		return domain.ErrHolidayNotFound
	}
	h.Status = "INACTIVE"
	return nil
}

// ── helpers ────────────────────────────────────────────────────────────────────

// daysFromNow renders a date offset from today, so notice-period tests do not
// go stale as the calendar advances.
func daysFromNow(n int) string {
	return time.Now().UTC().AddDate(0, 0, n).Format("2006-01-02")
}

// ── holiday endpoint tests ─────────────────────────────────────────────────────

func TestCreateHoliday_HappyPath(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/leave/holidays", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Republic Day",
		"date":            "2026-01-26",
		"holiday_type":    "PUBLIC",
	}, "hr-admin")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var h domain.Holiday
	if err := json.NewDecoder(rr.Body).Decode(&h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.Status != "ACTIVE" {
		t.Errorf("expected ACTIVE got %q", h.Status)
	}
	if h.HolidayType != "PUBLIC" {
		t.Errorf("expected PUBLIC got %q", h.HolidayType)
	}
}

func TestCreateHoliday_DefaultsToPublic(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/leave/holidays", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Founders Day",
		"date":            "2026-04-02",
	}, "hr-admin")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var h domain.Holiday
	_ = json.NewDecoder(rr.Body).Decode(&h)
	if h.HolidayType != "PUBLIC" {
		t.Errorf("expected default PUBLIC got %q", h.HolidayType)
	}
}

func TestCreateHoliday_InvalidType_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/leave/holidays", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Odd Day",
		"date":            "2026-04-02",
		"holiday_type":    "BANK",
	}, "hr-admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateHoliday_MalformedDate_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/leave/holidays", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Bad Date",
		"date":            "26-01-2026",
	}, "hr-admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateHoliday_DuplicateDate_Returns409(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	body := map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Republic Day",
		"date":            "2026-01-26",
	}
	if rr := doReq(r, http.MethodPost, "/v1/leave/holidays", body, "hr-admin"); rr.Code != http.StatusCreated {
		t.Fatalf("seed: expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	body["name"] = "Republic Day (duplicate)"
	rr := doReq(r, http.MethodPost, "/v1/leave/holidays", body, "hr-admin")
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 got %d: %s", rr.Code, rr.Body.String())
	}
}

// The same date in a different legal entity is a different calendar.
func TestCreateHoliday_SameDateDifferentEntity_Allowed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	if rr := doReq(r, http.MethodPost, "/v1/leave/holidays", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Republic Day",
		"date":            "2026-01-26",
	}, "hr-admin"); rr.Code != http.StatusCreated {
		t.Fatalf("seed: expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/leave/holidays", map[string]any{
		"legal_entity_id": "le-in",
		"name":            "Republic Day",
		"date":            "2026-01-26",
	}, "hr-admin")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateHoliday_AuthzDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/leave/holidays", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Republic Day",
		"date":            "2026-01-26",
	}, "hr-admin")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListHolidays_RequiresLegalEntity(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	rr := doReq(r, http.MethodGet, "/v1/leave/holidays", nil, "hr-admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListHolidays_FiltersByDateRange(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	for _, d := range []struct{ name, date string }{
		{"New Year", "2026-01-01"},
		{"Republic Day", "2026-01-26"},
		{"Independence Day", "2026-08-15"},
	} {
		rr := doReq(r, http.MethodPost, "/v1/leave/holidays", map[string]any{
			"legal_entity_id": "le-in",
			"name":            d.name,
			"date":            d.date,
		}, "hr-admin")
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %s: expected 201 got %d: %s", d.name, rr.Code, rr.Body.String())
		}
	}

	rr := doReq(r, http.MethodGet, "/v1/leave/holidays?legal_entity_id=le-in&from=2026-01-01&to=2026-02-01", nil, "hr-admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}

	var list []domain.Holiday
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 holidays in January, got %d", len(list))
	}
	if list[0].Date != "2026-01-01" || list[1].Date != "2026-01-26" {
		t.Errorf("expected date-ordered results, got %s then %s", list[0].Date, list[1].Date)
	}
}

func TestDeactivateHoliday_RetiresRatherThanDeletes(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rrCreate := doReq(r, http.MethodPost, "/v1/leave/holidays", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Cancelled Day",
		"date":            "2026-05-04",
	}, "hr-admin")
	var h domain.Holiday
	_ = json.NewDecoder(rrCreate.Body).Decode(&h)

	rr := doReq(r, http.MethodDelete, "/v1/leave/holidays/"+h.HolidayID, nil, "hr-admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}

	// Gone from the default listing.
	rrList := doReq(r, http.MethodGet, "/v1/leave/holidays?legal_entity_id=le-us", nil, "hr-admin")
	var list []domain.Holiday
	_ = json.NewDecoder(rrList.Body).Decode(&list)
	if len(list) != 0 {
		t.Errorf("expected inactive holiday to be hidden, got %d", len(list))
	}

	// But still on record.
	rrAll := doReq(r, http.MethodGet, "/v1/leave/holidays?legal_entity_id=le-us&include_inactive=true", nil, "hr-admin")
	var all []domain.Holiday
	_ = json.NewDecoder(rrAll.Body).Decode(&all)
	if len(all) != 1 || all[0].Status != "INACTIVE" {
		t.Errorf("expected 1 INACTIVE holiday on record, got %+v", all)
	}
}

// A retired date can be declared again; the uniqueness index is partial on ACTIVE.
func TestCreateHoliday_AfterDeactivation_Allowed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	body := map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Moved Day",
		"date":            "2026-05-04",
	}
	rrCreate := doReq(r, http.MethodPost, "/v1/leave/holidays", body, "hr-admin")
	var h domain.Holiday
	_ = json.NewDecoder(rrCreate.Body).Decode(&h)

	if rr := doReq(r, http.MethodDelete, "/v1/leave/holidays/"+h.HolidayID, nil, "hr-admin"); rr.Code != http.StatusOK {
		t.Fatalf("deactivate: expected 200 got %d", rr.Code)
	}

	rr := doReq(r, http.MethodPost, "/v1/leave/holidays", body, "hr-admin")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDeactivateHoliday_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	rr := doReq(r, http.MethodDelete, "/v1/leave/holidays/does-not-exist", nil, "hr-admin")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── leave policy tests ─────────────────────────────────────────────────────────

// seedPolicyType creates a leave type with the given policy and accrues enough
// balance for the employee to submit against it.
func seedPolicyType(t *testing.T, s *stubStore, r chi.Router, policy map[string]any) string {
	t.Helper()

	body := map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Policy Leave",
		"code":            fmt.Sprintf("POLICY_%d", len(s.types)),
		"is_paid":         true,
	}
	for k, v := range policy {
		body[k] = v
	}

	rr := doReq(r, http.MethodPost, "/v1/leave/types", body, "hr-admin")
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed leave type: expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var lt domain.LeaveType
	_ = json.NewDecoder(rr.Body).Decode(&lt)

	rrAccrue := doReq(r, http.MethodPost, "/v1/leave/balances/accrue", map[string]any{
		"employee_id":   "emp-1",
		"leave_type_id": lt.LeaveTypeID,
		"hours":         400.0,
	}, "hr-admin")
	if rrAccrue.Code != http.StatusOK && rrAccrue.Code != http.StatusCreated {
		t.Fatalf("seed balance: got %d: %s", rrAccrue.Code, rrAccrue.Body.String())
	}

	return lt.LeaveTypeID
}

func submitLeave(r chi.Router, leaveTypeID, start, end string, hours float64, correlationID string) *httptest.ResponseRecorder {
	return doReq(r, http.MethodPost, "/v1/leave/requests", map[string]any{
		"employee_id":    "emp-1",
		"leave_type_id":  leaveTypeID,
		"start_date":     start,
		"end_date":       end,
		"total_hours":    hours,
		"correlation_id": correlationID,
	}, "hr-admin")
}

func TestCreateLeaveType_DefaultsToRequiringApproval(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/leave/types", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Annual Vacation",
		"code":            "VACATION",
	}, "hr-admin")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var lt domain.LeaveType
	_ = json.NewDecoder(rr.Body).Decode(&lt)
	if !lt.RequiresApproval {
		t.Error("a leave type with requires_approval omitted must still require approval")
	}
}

func TestCreateLeaveType_DuplicateCode_Returns409(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	body := map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Annual Vacation",
		"code":            "VACATION",
	}
	if rr := doReq(r, http.MethodPost, "/v1/leave/types", body, "hr-admin"); rr.Code != http.StatusCreated {
		t.Fatalf("seed: expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/leave/types", body, "hr-admin")
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateLeaveType_CarryForwardWithoutCap_Rejected(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/leave/types", map[string]any{
		"legal_entity_id":         "le-us",
		"name":                    "Annual Vacation",
		"code":                    "VACATION",
		"carry_forward_allowed":   true,
		"carry_forward_max_hours": 0,
	}, "hr-admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateLeaveType_NegativePolicy_Rejected(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/leave/types", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Annual Vacation",
		"code":            "VACATION",
		"min_notice_days": -1,
	}, "hr-admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSubmitLeave_NoticeTooShort_Rejected(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	ltID := seedPolicyType(t, s, r, map[string]any{"min_notice_days": 14})

	rr := submitLeave(r, ltID, daysFromNow(3), daysFromNow(4), 16, "corr-notice-short")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSubmitLeave_SufficientNotice_Accepted(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	ltID := seedPolicyType(t, s, r, map[string]any{"min_notice_days": 14})

	rr := submitLeave(r, ltID, daysFromNow(20), daysFromNow(21), 16, "corr-notice-ok")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
}

// Sick leave is configured with zero notice so it can be booked same-day, and
// retroactively for a day already taken.
func TestSubmitLeave_ZeroNotice_AllowsRetroactive(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	ltID := seedPolicyType(t, s, r, map[string]any{"min_notice_days": 0})

	rr := submitLeave(r, ltID, daysFromNow(-2), daysFromNow(-2), 8, "corr-retro")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSubmitLeave_SpanTooLong_Rejected(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	ltID := seedPolicyType(t, s, r, map[string]any{"max_consecutive_days": 5})

	rr := submitLeave(r, ltID, daysFromNow(30), daysFromNow(40), 80, "corr-span-long")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

// The span is inclusive of both endpoints: start+4 is the fifth day, not the sixth.
func TestSubmitLeave_SpanExactlyAtLimit_Accepted(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	ltID := seedPolicyType(t, s, r, map[string]any{"max_consecutive_days": 5})

	rr := submitLeave(r, ltID, daysFromNow(30), daysFromNow(34), 40, "corr-span-exact")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSubmitLeave_ZeroMaxConsecutive_MeansUnlimited(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	ltID := seedPolicyType(t, s, r, map[string]any{"max_consecutive_days": 0})

	rr := submitLeave(r, ltID, daysFromNow(30), daysFromNow(120), 300, "corr-span-unlimited")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSubmitLeave_EndBeforeStart_Rejected(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	ltID := seedPolicyType(t, s, r, nil)

	rr := submitLeave(r, ltID, daysFromNow(40), daysFromNow(30), 16, "corr-reversed")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSubmitLeave_UnknownLeaveType_Returns404(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := submitLeave(r, "no-such-type", daysFromNow(10), daysFromNow(11), 16, "corr-unknown")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSubmitLeave_AutoApprovesWhenPolicySaysSo(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	ltID := seedPolicyType(t, s, r, map[string]any{"requires_approval": false})

	rr := submitLeave(r, ltID, daysFromNow(10), daysFromNow(11), 16, "corr-auto")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var lr domain.LeaveRequest
	if err := json.NewDecoder(rr.Body).Decode(&lr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if lr.Status != "APPROVED" {
		t.Fatalf("expected APPROVED got %q", lr.Status)
	}
	// The approval is attributed to the policy, never to the submitter.
	if lr.ReviewerID == nil || *lr.ReviewerID == "hr-admin" {
		t.Errorf("auto-approval must not be attributed to the submitting principal, got %v", lr.ReviewerID)
	}
}

func TestSubmitLeave_StaysSubmittedWhenApprovalRequired(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	ltID := seedPolicyType(t, s, r, map[string]any{"requires_approval": true})

	rr := submitLeave(r, ltID, daysFromNow(10), daysFromNow(11), 16, "corr-manual")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var lr domain.LeaveRequest
	_ = json.NewDecoder(rr.Body).Decode(&lr)
	if lr.Status != "SUBMITTED" {
		t.Fatalf("expected SUBMITTED got %q", lr.Status)
	}
}
