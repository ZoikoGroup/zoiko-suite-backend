package handler

// REF-10 Business Calendar (ZS-SVC-I-001 s4.20): holiday import, IsBusinessDay
// and AddBusinessDays.
//
//	POST /v1/admin/calendars/{code}/imports                  ImportHolidayCalendar (a draft version, with its evidence)
//	GET  /v1/calendars/{code}/is-business-day?date=          IsBusinessDay
//	GET  /v1/calendars/{code}/add-business-days?date=&days=  AddBusinessDays
//
// The reads are deterministic and say which calendar version answered, with its
// content digest and the evidence of its import, so an answer can be reproduced.
// Without a pin the version is the one in force for the date and published at
// known_at; with calendar_version it is that version exactly. A due date worked
// out today is therefore never silently changed by a calendar amended tomorrow.

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// businessCalendarStore is the registry's REF-10 capability. It is optional so a
// registry without it still satisfies RegistryStore; the routes then answer 503.
type businessCalendarStore interface {
	ImportHolidayCalendar(ctx context.Context, p domain.ImportCalendarParams) (*store.CalendarVersionRecord, bool, error)
	ResolveCalendarVersion(ctx context.Context, code, on string, knownAt *time.Time, pin *int) (*store.CalendarVersionRecord, bool, error)
	CalendarHolidayChanges(ctx context.Context, code string, version int) ([]domain.HolidayChange, error)
}

func (h *Handler) businessCalendars(w http.ResponseWriter, r *http.Request) (businessCalendarStore, bool) {
	s, ok := h.registry.(businessCalendarStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "the registry does not support business calendars")
	}
	return s, ok
}

// ImportCalendarRequest is the body of POST /v1/admin/calendars/{code}/imports.
type ImportCalendarRequest struct {
	SourceAuthority string           `json:"source_authority"`
	SourceReference string           `json:"source_reference"`
	SourceVersion   string           `json:"source_version"`
	EffectiveFrom   string           `json:"effective_from"`
	Timezone        string           `json:"timezone"`
	WeekendDays     *[]int           `json:"weekend_days"`
	CutoffTime      *string          `json:"cutoff_time"`
	Holidays        []domain.Holiday `json:"holidays"`
	SourceIDs       []string         `json:"source_ids"`
}

// ImportHolidayCalendar handles POST /v1/admin/calendars/{calendar_code}/imports.
//
//	201 → drafted as the next version of the calendar
//	200 → this source version was already imported with the same content (replay)
//	400 → invalid import · 404 → no such calendar
//	409 → this source version was already imported with different content
//
// Each holiday may carry an observed_rule; its observed date is worked out here
// from the rule and the calendar's weekend. The draft is published afterwards by
// a different principal, citing reviewed sources, as any calendar version is.
func (h *Handler) ImportHolidayCalendar(w http.ResponseWriter, r *http.Request) {
	s, ok := h.businessCalendars(w, r)
	if !ok {
		return
	}
	actor, ok := h.admin(w, r, "regulatory_calendar_version", "create")
	if !ok {
		return
	}
	var req ImportCalendarRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.WeekendDays == nil {
		writeMissingField(w, r, "weekend_days")
		return
	}
	v, created, err := s.ImportHolidayCalendar(r.Context(), domain.ImportCalendarParams{
		CalendarCode: chi.URLParam(r, "calendar_code"), SourceAuthority: req.SourceAuthority, SourceReference: req.SourceReference,
		SourceVersion: req.SourceVersion, EffectiveFrom: req.EffectiveFrom, Timezone: req.Timezone, WeekendDays: *req.WeekendDays,
		CutoffTime: req.CutoffTime, Holidays: req.Holidays, SourceIDs: req.SourceIDs, Actor: actor,
	})
	if err != nil {
		h.writeCalendarError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, v)
}

// calendarQuery is what the two reads share: which calendar version answers.
type calendarQuery struct {
	code    string
	date    time.Time
	dateRaw string
	knownAt *time.Time
	pin     *int
}

func (h *Handler) parseCalendarQuery(w http.ResponseWriter, r *http.Request) (calendarQuery, bool) {
	q := r.URL.Query()
	out := calendarQuery{code: chi.URLParam(r, "calendar_code"), dateRaw: q.Get("date")}
	if out.dateRaw == "" {
		writeMissingField(w, r, "date")
		return out, false
	}
	d, err := time.Parse("2006-01-02", out.dateRaw)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_date", "date must be YYYY-MM-DD")
		return out, false
	}
	out.date = d
	if v := q.Get("known_at"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_known_at", "known_at must be an RFC3339 timestamp")
			return out, false
		}
		out.knownAt = &t
	}
	if v := q.Get("calendar_version"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, r, http.StatusBadRequest, "invalid_version", "calendar_version must be a positive integer")
			return out, false
		}
		out.pin = &n
	}
	return out, true
}

func (h *Handler) resolveCalendar(w http.ResponseWriter, r *http.Request, s businessCalendarStore, q calendarQuery) (*store.CalendarVersionRecord, bool, bool) {
	rec, pinned, err := s.ResolveCalendarVersion(r.Context(), q.code, q.dateRaw, q.knownAt, q.pin)
	if err != nil {
		h.writeCalendarError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return nil, false, false
	}
	return rec, pinned, true
}

// IsBusinessDay handles GET /v1/calendars/{calendar_code}/is-business-day.
//
//	date=YYYY-MM-DD        required
//	known_at=RFC3339       optional: answer as the calendar was known then (default now)
//	calendar_version=N     optional: that published version exactly, to reproduce an answer
//
//	200 → is_business_day, why not if it is not, and the calendar version that answered
//	404 → no such calendar, or no published version governs that date
func (h *Handler) IsBusinessDay(w http.ResponseWriter, r *http.Request) {
	s, ok := h.businessCalendars(w, r)
	if !ok {
		return
	}
	q, ok := h.parseCalendarQuery(w, r)
	if !ok {
		return
	}
	rec, pinned, ok := h.resolveCalendar(w, r, s, q)
	if !ok {
		return
	}
	cal := rec.Domain()
	reason, name, non := cal.NonBusinessDay(q.date)
	writeJSON(w, http.StatusOK, domain.BusinessDayCheck{
		Date: q.dateRaw, IsBusinessDay: !non, Reason: reason, HolidayName: name, Calendar: rec.Provenance(pinned),
	})
}

// AddBusinessDays handles GET /v1/calendars/{calendar_code}/add-business-days.
//
//	date=YYYY-MM-DD        required: the start, which is never counted
//	days=N                 required: business days to move, negative to move back (at most 1000)
//	known_at, calendar_version as for is-business-day
//
// One calendar version, the one governing the start date, answers the whole
// calculation: a shift that crosses the start of a later version does not switch
// calendars halfway, so the result is one deterministic function of the inputs.
func (h *Handler) AddBusinessDays(w http.ResponseWriter, r *http.Request) {
	s, ok := h.businessCalendars(w, r)
	if !ok {
		return
	}
	q, ok := h.parseCalendarQuery(w, r)
	if !ok {
		return
	}
	raw := r.URL.Query().Get("days")
	if raw == "" {
		writeMissingField(w, r, "days")
		return
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days > domain.MaxBusinessDayShift || days < -domain.MaxBusinessDayShift {
		writeError(w, r, http.StatusBadRequest, "invalid_days", "days must be a whole number between -1000 and 1000")
		return
	}
	rec, pinned, ok := h.resolveCalendar(w, r, s, q)
	if !ok {
		return
	}
	result, skipped, err := rec.Domain().AddBusinessDays(q.date, days)
	if err != nil {
		h.writeCalendarError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if skipped == nil {
		skipped = []domain.SkippedDay{}
	}
	writeJSON(w, http.StatusOK, domain.AddBusinessDaysResult{
		StartDate: q.dateRaw, Days: days, Result: result.Format("2006-01-02"), Skipped: skipped, Calendar: rec.Provenance(pinned),
	})
}

// maxHolidayEvents bounds the events one publication produces.
const maxHolidayEvents = 400

// emitCalendarPublished announces a published calendar version: the calendar was
// updated, and each holiday occurrence it adds, removes or changes.
func (h *Handler) emitCalendarPublished(r *http.Request, code, actor string, v *store.CalendarVersionRecord) {
	s, ok := h.registry.(businessCalendarStore)
	if !ok {
		return
	}
	changes, err := s.CalendarHolidayChanges(r.Context(), code, v.Version)
	if err != nil {
		h.log.Warn("holiday changes could not be worked out for a published calendar", zap.String("calendar_code", code), zap.Error(err))
		return
	}
	payload := map[string]any{
		"calendar_code": code, "calendar_version": v.Version, "calendar_version_id": v.CalendarVersionID,
		"effective_from": v.EffectiveFrom, "content_digest": v.ContentDigest, "timezone": v.Timezone,
		"holidays_changed": len(changes), "source_authority": v.SourceAuthority, "source_version": v.SourceVersion, "import_hash": v.ImportHash,
	}
	h.emitRegistry(r, events.EventBusinessCalendarUpdated, v.CalendarVersionID, actor, payload)
	for i, ch := range changes {
		if i == maxHolidayEvents {
			break
		}
		h.emitRegistry(r, events.EventHolidayOccurrenceChanged, v.CalendarVersionID, actor, map[string]any{
			"calendar_code": code, "calendar_version": v.Version, "calendar_version_id": v.CalendarVersionID,
			"holiday_date": ch.Date, "change": ch.Change, "name": ch.Name, "previous_name": ch.PreviousName,
			"observed_date": ch.ObservedDate, "previous_observed_date": ch.PreviousObservedDate,
		})
	}
}
