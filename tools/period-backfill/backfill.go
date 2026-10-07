package main

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

const dateLayout = "2006-01-02"

// config is everything the run needs; nothing about a country or a fiscal-year
// convention is hardcoded -- the start month/day are parameters.
type config struct {
	Tenant          string
	Entities        []string
	StartMonth      int
	StartDay        int
	FromFY, ToFY    int // 0 = derive from the entity's legacy periods
	Maker, Checker  string
	MirrorPrincipal string
	CalendarURL     string
	PeriodURL       string
	CloseURL        string
	CalendarCode    string
	Scope           string // fiscal-calendar scope AND accounting-period book_scope (REF-05 resolves the calendar by it)
	Reason          string
	Apply           bool
	MirrorClosed    bool
}

// ── wire shapes (only the fields this tool reads) ────────────────────────────

type legacyPeriod struct {
	FiscalPeriodID string `json:"fiscal_period_id"`
	PeriodName     string `json:"period_name"`
	PeriodStart    string `json:"period_start"`
	PeriodEnd      string `json:"period_end"`
	CloseStatus    string `json:"close_status"`
}

type calendarVersion struct {
	VersionID  string `json:"version_id"`
	CalendarID string `json:"calendar_id"`
	VersionNo  int    `json:"version_no"`
	Pattern    struct {
		Type string `json:"type"`
	} `json:"pattern"`
	StartMonth    int    `json:"fiscal_year_start_month"`
	StartDay      int    `json:"fiscal_year_start_day"`
	EffectiveFrom string `json:"effective_from"`
	Status        string `json:"status"`
	Version       int64  `json:"version"`
}

type calendarCreated struct {
	Calendar struct {
		CalendarID string `json:"calendar_id"`
	} `json:"calendar"`
	Version calendarVersion `json:"version"`
}

type resolution struct {
	CalendarID string `json:"calendar_id"`
	VersionID  string `json:"version_id"`
}

type refPeriod struct {
	PeriodID          string `json:"period_id"`
	CalendarID        string `json:"calendar_id"`
	CalendarVersionID string `json:"calendar_version_id"`
	BookScope         string `json:"book_scope"`
	PeriodKey         string `json:"period_key"`
	FiscalYear        int    `json:"fiscal_year"`
	StartDate         string `json:"start_date"`
	EndDate           string `json:"end_date"`
	Kind              string `json:"kind"`
	State             string `json:"state"`
}

type materializeResult struct {
	Created       int      `json:"created"`
	Existing      int      `json:"existing"`
	BoundaryDrift []string `json:"boundary_drift"`
}

type mirrorResult struct {
	FiscalPeriodID string `json:"fiscal_period_id"`
	LegacyState    string `json:"legacy_state"`
	Actions        []struct {
		Command string `json:"command"`
		Outcome string `json:"outcome"`
		Ref     string `json:"ref"`
		Reason  string `json:"reason"`
	} `json:"actions"`
}

// ── report ───────────────────────────────────────────────────────────────────

type PeriodRow struct {
	FiscalPeriodID string   `json:"fiscal_period_id"`
	Name           string   `json:"period_name"`
	Start          string   `json:"start"`
	End            string   `json:"end"`
	LegacyState    string   `json:"legacy_state"`
	Mapping        string   `json:"mapping"` // mapped | unmapped
	Reason         string   `json:"reason,omitempty"`
	Match          string   `json:"match,omitempty"` // matched | pending_materialize | unmatched | ambiguous
	Ref05PeriodID  string   `json:"ref05_period_id,omitempty"`
	Ref05Key       string   `json:"ref05_period_key,omitempty"`
	Ref05State     string   `json:"ref05_state,omitempty"`
	Mirror         string   `json:"mirror,omitempty"`
	MirrorActions  []Action `json:"mirror_actions,omitempty"`
}

type Action struct {
	Command string `json:"command"`
	Outcome string `json:"outcome"`
	Ref     string `json:"ref,omitempty"`
}

type Anomaly struct {
	Kind     string `json:"kind"`
	Detail   string `json:"detail"`
	Blocking bool   `json:"blocking"`
}

type CalendarReport struct {
	Scope      string   `json:"scope"`
	Code       string   `json:"code"`
	CalendarID string   `json:"calendar_id,omitempty"`
	VersionID  string   `json:"version_id,omitempty"`
	Steps      []string `json:"steps"`
}

type MaterializeRow struct {
	FiscalYear int      `json:"fiscal_year"`
	Action     string   `json:"action"` // already_materialized | would_materialize | materialized | skipped | error
	Created    int      `json:"created,omitempty"`
	Existing   int      `json:"existing,omitempty"`
	Drift      []string `json:"boundary_drift,omitempty"`
	Note       string   `json:"note,omitempty"`
}

type EntityReport struct {
	LegalEntityID string           `json:"legal_entity_id"`
	FiscalYears   []int            `json:"fiscal_years"`
	Calendar      CalendarReport   `json:"calendar"`
	Materialize   []MaterializeRow `json:"materialize"`
	Periods       []PeriodRow      `json:"periods"`
	Anomalies     []Anomaly        `json:"anomalies"`
	Errors        []string         `json:"errors"`
}

type Summary struct {
	Entities          int `json:"entities"`
	LegacyPeriods     int `json:"legacy_periods"`
	Mapped            int `json:"mapped"`
	Unmapped          int `json:"unmapped"`
	Matched           int `json:"matched"`
	Unmatched         int `json:"unmatched"`
	Anomalies         int `json:"anomalies"`
	BlockingAnomalies int `json:"blocking_anomalies"`
	Errors            int `json:"errors"`
	WouldMirror       int `json:"would_mirror"`
	Mirrored          int `json:"mirrored"`
}

type Report struct {
	RunID       string         `json:"run_id"`
	GeneratedAt string         `json:"generated_at"`
	Mode        string         `json:"mode"` // dry-run | apply
	Tenant      string         `json:"tenant_id"`
	Maker       string         `json:"maker_principal_id"`
	Checker     string         `json:"checker_principal_id"`
	FYStart     string         `json:"fiscal_year_start"` // MM-DD
	Scope       string         `json:"scope"`
	MirrorStage string         `json:"mirror_stage"` // not_selected | planned | applied
	Entities    []EntityReport `json:"entities"`
	Summary     Summary        `json:"summary"`
}

// ── runner ───────────────────────────────────────────────────────────────────

type runner struct {
	cfg            config
	c              *apiClient
	mirrorDisabled bool
}

func (r *runner) maker(entity string) actor   { return actor{r.cfg.Maker, entity} }
func (r *runner) checker(entity string) actor { return actor{r.cfg.Checker, entity} }

func (r *runner) run(ctx context.Context) *Report {
	rep := &Report{
		RunID: r.c.runID, GeneratedAt: time.Now().UTC().Format(time.RFC3339), Tenant: r.cfg.Tenant,
		Maker: r.cfg.Maker, Checker: r.cfg.Checker, Scope: r.cfg.Scope, Mode: "dry-run",
		FYStart: fmt.Sprintf("%02d-%02d", r.cfg.StartMonth, r.cfg.StartDay), MirrorStage: "not_selected",
	}
	if r.cfg.Apply {
		rep.Mode = "apply"
	}
	if r.cfg.MirrorClosed {
		rep.MirrorStage = "planned"
		if r.cfg.Apply {
			rep.MirrorStage = "applied"
		}
	}
	for _, e := range r.cfg.Entities {
		er := r.processEntity(ctx, e)
		rep.Entities = append(rep.Entities, *er)
	}
	rep.Summary = summarize(rep)
	return rep
}

func summarize(rep *Report) Summary {
	var s Summary
	s.Entities = len(rep.Entities)
	for _, e := range rep.Entities {
		s.Errors += len(e.Errors)
		for _, a := range e.Anomalies {
			s.Anomalies++
			if a.Blocking {
				s.BlockingAnomalies++
			}
		}
		for _, p := range e.Periods {
			s.LegacyPeriods++
			if p.Mapping == "mapped" {
				s.Mapped++
			} else {
				s.Unmapped++
			}
			switch p.Match {
			case "matched":
				s.Matched++
			case "unmatched", "ambiguous":
				s.Unmatched++
			}
			switch p.Mirror {
			case "would_mirror":
				s.WouldMirror++
			case "mirrored":
				s.Mirrored++
			}
		}
	}
	return s
}

// Failed reports whether the run should exit non-zero.
func (rep *Report) Failed(strict bool) bool {
	if rep.Summary.Errors > 0 {
		return true
	}
	return strict && (rep.Summary.Unmapped > 0 || rep.Summary.Unmatched > 0 || rep.Summary.BlockingAnomalies > 0)
}

// ── per entity ───────────────────────────────────────────────────────────────

func (r *runner) processEntity(ctx context.Context, entity string) *EntityReport {
	er := &EntityReport{LegalEntityID: entity, Calendar: CalendarReport{Scope: r.cfg.Scope, Code: r.cfg.CalendarCode, Steps: []string{}},
		Materialize: []MaterializeRow{}, Periods: []PeriodRow{}, Anomalies: []Anomaly{}, Errors: []string{}, FiscalYears: []int{}}

	// 1. legacy periods (read-only).
	var legacy []legacyPeriod
	if _, _, err := r.c.do(ctx, r.maker(entity), "GET", r.cfg.CloseURL+"/v1/close/periods/?legal_entity_id="+url.QueryEscape(entity), nil, "", &legacy); err != nil {
		er.Errors = append(er.Errors, "list legacy periods: "+err.Error())
		return er
	}
	er.Periods = classify(legacy, er)

	// 2. span.
	er.FiscalYears = r.span(er)
	if len(er.FiscalYears) == 0 {
		if len(legacy) > 0 {
			er.Anomalies = append(er.Anomalies, Anomaly{"no_span", "no mappable legacy period and no --from-fy/--to-fy given; nothing to materialize", true})
		} else {
			er.Errors = append(er.Errors, "entity has no legacy periods and no --from-fy/--to-fy was given; cannot derive a fiscal-year span")
		}
	}

	// 3. calendar.
	cal := r.ensureCalendar(ctx, entity, er)

	// 4. REF-05 periods, then materialize missing fiscal years.
	ref, err := r.listRef(ctx, entity)
	if err != nil {
		er.Errors = append(er.Errors, "list accounting periods: "+err.Error())
		return er
	}
	if r.materialize(ctx, entity, cal, ref, er) {
		if ref, err = r.listRef(ctx, entity); err != nil {
			er.Errors = append(er.Errors, "re-list accounting periods: "+err.Error())
			return er
		}
	}

	// 5. match legacy -> REF-05 strictly by date range.
	r.match(er, ref)

	// 6. optional mirror stage.
	if r.cfg.MirrorClosed {
		r.mirror(ctx, entity, er)
	}
	return er
}

var nameRe = regexp.MustCompile(`^(\d{4})-(\d{2})$`)

func parseDate(s string) (time.Time, error) {
	if len(s) >= 10 {
		s = s[:10]
	}
	return time.Parse(dateLayout, s)
}

func fmtDate(t time.Time) string { return t.Format(dateLayout) }

// classify turns legacy rows into report rows and records overlap/gap
// anomalies. It never repairs or reinterprets: a row that is not exactly one
// calendar month is unmapped, with the reason.
func classify(legacy []legacyPeriod, er *EntityReport) []PeriodRow {
	rows := make([]PeriodRow, 0, len(legacy))
	type span struct {
		s, e time.Time
		name string
	}
	var spans []span
	for _, l := range legacy {
		row := PeriodRow{FiscalPeriodID: l.FiscalPeriodID, Name: l.PeriodName, LegacyState: l.CloseStatus, Mapping: "unmapped"}
		s, e1 := parseDate(l.PeriodStart)
		e, e2 := parseDate(l.PeriodEnd)
		row.Start, row.End = l.PeriodStart, l.PeriodEnd
		if e1 != nil || e2 != nil {
			row.Reason = "start/end dates are not parseable"
		} else {
			row.Start, row.End = fmtDate(s), fmtDate(e)
			spans = append(spans, span{s, e, l.PeriodName})
			m := nameRe.FindStringSubmatch(l.PeriodName)
			lastDay := time.Date(s.Year(), s.Month()+1, 0, 0, 0, 0, 0, time.UTC)
			switch {
			case m == nil:
				row.Reason = fmt.Sprintf("name %q does not parse as a calendar month (YYYY-MM)", l.PeriodName)
			case s.Day() != 1 || !e.Equal(lastDay):
				row.Reason = fmt.Sprintf("dates %s..%s are not exactly one calendar month", row.Start, row.End)
			case fmt.Sprintf("%04d-%02d", s.Year(), int(s.Month())) != l.PeriodName:
				row.Reason = fmt.Sprintf("name %q disagrees with its dates (%s)", l.PeriodName, row.Start)
			default:
				row.Mapping = "mapped"
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].s.Before(spans[j].s) })
	for i := 1; i < len(spans); i++ {
		prev, cur := spans[i-1], spans[i]
		switch {
		case !cur.s.After(prev.e):
			er.Anomalies = append(er.Anomalies, Anomaly{"overlap", fmt.Sprintf("legacy periods %q (%s..%s) and %q (%s..%s) overlap", prev.name, fmtDate(prev.s), fmtDate(prev.e), cur.name, fmtDate(cur.s), fmtDate(cur.e)), true})
		case cur.s.After(prev.e.AddDate(0, 0, 1)):
			er.Anomalies = append(er.Anomalies, Anomaly{"gap", fmt.Sprintf("gap between legacy periods %q (ends %s) and %q (starts %s)", prev.name, fmtDate(prev.e), cur.name, fmtDate(cur.s)), false})
		}
	}
	return rows
}

// fiscalYearOf is the START_YEAR label: the year of the anchor on or before d.
func fiscalYearOf(d time.Time, month, day int) int {
	a := time.Date(d.Year(), time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if d.Before(a) {
		return d.Year() - 1
	}
	return d.Year()
}

func anchor(fy, month, day int) time.Time {
	return time.Date(fy, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}

func (r *runner) span(er *EntityReport) []int {
	lo, hi := r.cfg.FromFY, r.cfg.ToFY
	dlo, dhi := 0, 0
	for _, p := range er.Periods {
		if p.Mapping != "mapped" {
			continue
		}
		s, err := parseDate(p.Start)
		if err != nil {
			continue
		}
		fy := fiscalYearOf(s, r.cfg.StartMonth, r.cfg.StartDay)
		if dlo == 0 || fy < dlo {
			dlo = fy
		}
		if fy > dhi {
			dhi = fy
		}
	}
	if lo == 0 {
		lo = dlo
	}
	if hi == 0 {
		hi = dhi
	}
	if lo == 0 || hi == 0 || hi < lo {
		return nil
	}
	var out []int
	for y := lo; y <= hi; y++ {
		out = append(out, y)
	}
	return out
}

// ── calendar ─────────────────────────────────────────────────────────────────

type calState struct {
	exists  bool
	blocked bool
	calID   string
}

var dupCalRe = regexp.MustCompile(`calendar_id ([0-9A-Za-z-]+)`)

func (r *runner) resolveAt(ctx context.Context, entity string, d time.Time) (*resolution, error) {
	q := url.Values{"legal_entity_id": {entity}, "scope": {r.cfg.Scope}, "date": {fmtDate(d)}}
	var res resolution
	if _, _, err := r.c.do(ctx, r.maker(entity), "GET", r.cfg.CalendarURL+"/v1/fiscal-calendars:resolve?"+q.Encode(), nil, "", &res); err != nil {
		if isStatus(err, 404) {
			return nil, nil
		}
		return nil, err
	}
	return &res, nil
}

func (r *runner) getVersion(ctx context.Context, entity, vid string) (*calendarVersion, error) {
	var v calendarVersion
	if _, _, err := r.c.do(ctx, r.maker(entity), "GET", r.cfg.CalendarURL+"/v1/fiscal-calendar-versions/"+url.PathEscape(vid), nil, "", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// compatible says whether an existing version is the shape this tool would
// have created. A different calendar is never overwritten or "fixed".
func (r *runner) compatible(v *calendarVersion) string {
	if v.Pattern.Type != "CALENDAR_MONTHS" {
		return fmt.Sprintf("existing version %s uses pattern %s, not CALENDAR_MONTHS", v.VersionID, v.Pattern.Type)
	}
	if v.StartMonth != r.cfg.StartMonth || v.StartDay != r.cfg.StartDay {
		return fmt.Sprintf("existing version %s starts its fiscal year %02d-%02d, not the requested %02d-%02d", v.VersionID, v.StartMonth, v.StartDay, r.cfg.StartMonth, r.cfg.StartDay)
	}
	return ""
}

func (r *runner) ensureCalendar(ctx context.Context, entity string, er *EntityReport) *calState {
	st := &calState{}
	if len(er.FiscalYears) == 0 {
		st.blocked = true
		return st
	}
	first := anchor(er.FiscalYears[0], r.cfg.StartMonth, r.cfg.StartDay)
	res, err := r.resolveAt(ctx, entity, first)
	if err != nil {
		er.Errors = append(er.Errors, "resolve calendar: "+err.Error())
		st.blocked = true
		return st
	}
	if res != nil {
		v, err := r.getVersion(ctx, entity, res.VersionID)
		if err != nil {
			er.Errors = append(er.Errors, "read calendar version: "+err.Error())
			st.blocked = true
			return st
		}
		if why := r.compatible(v); why != "" {
			er.Anomalies = append(er.Anomalies, Anomaly{"calendar_incompatible", why + "; not changed", true})
			st.blocked = true
		}
		st.exists, st.calID = true, res.CalendarID
		er.Calendar.CalendarID, er.Calendar.VersionID = res.CalendarID, res.VersionID
		er.Calendar.Steps = append(er.Calendar.Steps, "exists (in force at "+fmtDate(first)+"): nothing to do")
		return st
	}

	// No version in force at the first fiscal-year start.
	if !r.cfg.Apply {
		er.Calendar.Steps = append(er.Calendar.Steps,
			"would create CALENDAR_MONTHS calendar "+r.cfg.CalendarCode+" (maker), effective from "+fmtDate(first),
			"would approve (checker)", "would activate (checker)")
		return st
	}
	v, calID, err := r.createOrFind(ctx, entity, first, er)
	if err != nil {
		er.Errors = append(er.Errors, "create calendar: "+err.Error())
		st.blocked = true
		return st
	}
	st.calID = calID
	er.Calendar.CalendarID = calID
	if v.Status == "DRAFT" {
		var out calendarVersion
		_, _, err := r.c.do(ctx, r.checker(entity), "POST", r.cfg.CalendarURL+"/v1/fiscal-calendar-versions/"+v.VersionID+":approve",
			map[string]any{"expected_version": v.Version, "reason": r.cfg.Reason}, idemKey("approve", r.cfg.Tenant, entity, v.VersionID, fmt.Sprint(v.Version)), &out)
		if err != nil {
			er.Errors = append(er.Errors, "approve calendar version: "+err.Error())
			st.blocked = true
			return st
		}
		v = &out
		er.Calendar.Steps = append(er.Calendar.Steps, "approved (checker)")
	}
	if v.Status == "APPROVED" {
		var out calendarVersion
		_, _, err := r.c.do(ctx, r.checker(entity), "POST", r.cfg.CalendarURL+"/v1/fiscal-calendar-versions/"+v.VersionID+":activate",
			map[string]any{"expected_version": v.Version, "reason": r.cfg.Reason}, idemKey("activate", r.cfg.Tenant, entity, v.VersionID, fmt.Sprint(v.Version)), &out)
		if err != nil {
			er.Errors = append(er.Errors, "activate calendar version: "+err.Error())
			st.blocked = true
			return st
		}
		v = &out
		er.Calendar.Steps = append(er.Calendar.Steps, "activated (checker)")
	}
	if v.Status != "ACTIVE" {
		er.Errors = append(er.Errors, fmt.Sprintf("calendar version %s is %s; expected ACTIVE", v.VersionID, v.Status))
		st.blocked = true
		return st
	}
	st.exists = true
	er.Calendar.VersionID = v.VersionID
	return st
}

// createOrFind proposes the calendar as the maker. A duplicate (the calendar
// was created by an earlier, interrupted run under a different key) is picked
// up and continued rather than treated as a failure.
func (r *runner) createOrFind(ctx context.Context, entity string, from time.Time, er *EntityReport) (*calendarVersion, string, error) {
	body := map[string]any{
		"legal_entity_id": entity, "code": r.cfg.CalendarCode, "scope": r.cfg.Scope, "reason": r.cfg.Reason,
		"pattern":                 map[string]any{"type": "CALENDAR_MONTHS"},
		"fiscal_year_start_month": r.cfg.StartMonth, "fiscal_year_start_day": r.cfg.StartDay,
		"effective_from": fmtDate(from),
	}
	var out calendarCreated
	_, _, err := r.c.do(ctx, r.maker(entity), "POST", r.cfg.CalendarURL+"/v1/fiscal-calendars", body,
		idemKey("create-calendar", r.cfg.Tenant, entity, r.cfg.CalendarCode, r.cfg.Scope, fmtDate(from), fmt.Sprint(r.cfg.StartMonth, r.cfg.StartDay)), &out)
	if err == nil {
		er.Calendar.Steps = append(er.Calendar.Steps, "created draft (maker)")
		return &out.Version, out.Calendar.CalendarID, nil
	}
	ae, ok := err.(*apiError)
	if !ok || ae.Status != 409 {
		return nil, "", err
	}
	m := dupCalRe.FindStringSubmatch(ae.Message)
	if m == nil {
		return nil, "", fmt.Errorf("calendar code %q already exists but its id could not be determined: %w", r.cfg.CalendarCode, err)
	}
	var list struct {
		Items []calendarVersion `json:"items"`
	}
	if _, _, lerr := r.c.do(ctx, r.maker(entity), "GET", r.cfg.CalendarURL+"/v1/fiscal-calendars/"+m[1]+"/versions", nil, "", &list); lerr != nil {
		return nil, "", lerr
	}
	var pick *calendarVersion
	for i := range list.Items {
		v := &list.Items[i]
		if v.EffectiveFrom == fmtDate(from) && (pick == nil || v.VersionNo > pick.VersionNo) {
			pick = v
		}
	}
	if pick == nil {
		return nil, "", fmt.Errorf("calendar %s exists but has no version effective from %s; resolve it by hand", m[1], fmtDate(from))
	}
	if why := r.compatible(pick); why != "" {
		return nil, "", fmt.Errorf("%s", why)
	}
	er.Calendar.Steps = append(er.Calendar.Steps, "found existing calendar "+m[1]+" (status "+pick.Status+"); continuing")
	return pick, m[1], nil
}

// ── REF-05 ───────────────────────────────────────────────────────────────────

func (r *runner) listRef(ctx context.Context, entity string) ([]refPeriod, error) {
	const limit = 500
	var all []refPeriod
	for offset := 0; ; offset += limit {
		var page struct {
			Items []refPeriod `json:"items"`
		}
		q := url.Values{"legal_entity_id": {entity}, "limit": {fmt.Sprint(limit)}, "offset": {fmt.Sprint(offset)}}
		if _, _, err := r.c.do(ctx, r.maker(entity), "GET", r.cfg.PeriodURL+"/v1/accounting-periods?"+q.Encode(), nil, "", &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if len(page.Items) < limit {
			return all, nil
		}
	}
}

// materialize returns true when it created anything (so the caller re-reads).
func (r *runner) materialize(ctx context.Context, entity string, cal *calState, ref []refPeriod, er *EntityReport) bool {
	created := false
	for _, fy := range er.FiscalYears {
		row := MaterializeRow{FiscalYear: fy}
		finish := func() { er.Materialize = append(er.Materialize, row) }
		if cal.blocked {
			row.Action, row.Note = "skipped", "calendar blocked (see errors/anomalies)"
			finish()
			continue
		}
		if !cal.exists {
			row.Action, row.Note = "would_materialize", "after the calendar is created and activated"
			finish()
			continue
		}
		res, err := r.resolveAt(ctx, entity, anchor(fy, r.cfg.StartMonth, r.cfg.StartDay))
		if err != nil {
			row.Action, row.Note = "error", err.Error()
			er.Errors = append(er.Errors, fmt.Sprintf("resolve calendar for FY%d: %v", fy, err))
			finish()
			continue
		}
		if res == nil {
			row.Action, row.Note = "skipped", "no calendar version in force at the fiscal-year start"
			er.Anomalies = append(er.Anomalies, Anomaly{"no_calendar_for_year", fmt.Sprintf("no calendar version in force at the start of FY%d", fy), true})
			finish()
			continue
		}
		v, err := r.getVersion(ctx, entity, res.VersionID)
		if err != nil {
			row.Action, row.Note = "error", err.Error()
			er.Errors = append(er.Errors, fmt.Sprintf("read calendar version for FY%d: %v", fy, err))
			finish()
			continue
		}
		if why := r.compatible(v); why != "" {
			row.Action, row.Note = "skipped", why
			er.Anomalies = append(er.Anomalies, Anomaly{"calendar_incompatible", fmt.Sprintf("FY%d: %s; not materialized", fy, why), true})
			finish()
			continue
		}
		present := 0
		for _, p := range ref {
			if p.FiscalYear == fy && p.BookScope == r.cfg.Scope && p.CalendarVersionID == res.VersionID {
				present++
			}
		}
		if present > 0 {
			row.Action, row.Existing = "already_materialized", present
			finish()
			continue
		}
		if !r.cfg.Apply {
			row.Action = "would_materialize"
			finish()
			continue
		}
		var out materializeResult
		_, _, err = r.c.do(ctx, r.maker(entity), "POST", r.cfg.PeriodURL+"/v1/accounting-periods:materialize", map[string]any{
			"legal_entity_id": entity, "calendar_id": res.CalendarID, "fiscal_year": fy, "book_scope": r.cfg.Scope,
			"calendar_version_id": res.VersionID, "reason": r.cfg.Reason,
		}, idemKey("materialize", r.cfg.Tenant, entity, res.CalendarID, res.VersionID, fmt.Sprint(fy), r.cfg.Scope), &out)
		if err != nil {
			row.Action, row.Note = "error", err.Error()
			er.Errors = append(er.Errors, fmt.Sprintf("materialize FY%d: %v", fy, err))
			finish()
			continue
		}
		row.Action, row.Created, row.Existing, row.Drift = "materialized", out.Created, out.Existing, out.BoundaryDrift
		if out.Created > 0 {
			created = true
		}
		for _, k := range out.BoundaryDrift {
			er.Anomalies = append(er.Anomalies, Anomaly{"boundary_drift", fmt.Sprintf("FY%d period %s already existed with boundaries that differ from the calendar; not changed", fy, k), true})
		}
		finish()
	}
	return created
}

// match pairs each legacy period with a REF-05 NORMAL period of this book
// scope by exact start/end date -- never by name or key.
func (r *runner) match(er *EntityReport, ref []refPeriod) {
	for i := range er.Periods {
		p := &er.Periods[i]
		if p.Mapping != "mapped" {
			continue
		}
		var hits []refPeriod
		for _, q := range ref {
			if q.Kind == "NORMAL" && q.BookScope == r.cfg.Scope && q.StartDate == p.Start && q.EndDate == p.End {
				hits = append(hits, q)
			}
		}
		switch len(hits) {
		case 1:
			p.Match, p.Ref05PeriodID, p.Ref05Key, p.Ref05State = "matched", hits[0].PeriodID, hits[0].PeriodKey, hits[0].State
		case 0:
			if r.pendingMaterialize(er, p) {
				p.Match = "pending_materialize"
			} else {
				p.Match = "unmatched"
				er.Anomalies = append(er.Anomalies, Anomaly{"unmatched", fmt.Sprintf("legacy period %s (%s..%s) has no REF-05 period with the same dates", p.Name, p.Start, p.End), true})
			}
		default:
			p.Match = "ambiguous"
			er.Anomalies = append(er.Anomalies, Anomaly{"ambiguous_match", fmt.Sprintf("legacy period %s (%s..%s) matches %d REF-05 periods by date; not guessing", p.Name, p.Start, p.End, len(hits)), true})
		}
	}
}

// pendingMaterialize: in a dry-run, a period whose fiscal year is still to be
// materialized is expected to be missing, so it is not yet an anomaly.
func (r *runner) pendingMaterialize(er *EntityReport, p *PeriodRow) bool {
	if r.cfg.Apply {
		return false
	}
	s, err := parseDate(p.Start)
	if err != nil {
		return false
	}
	fy := fiscalYearOf(s, r.cfg.StartMonth, r.cfg.StartDay)
	for _, m := range er.Materialize {
		if m.FiscalYear == fy && m.Action == "would_materialize" {
			return true
		}
	}
	return false
}

// ── mirror (optional last stage) ─────────────────────────────────────────────

func (r *runner) mirror(ctx context.Context, entity string, er *EntityReport) {
	for i := range er.Periods {
		p := &er.Periods[i]
		if p.Mapping != "mapped" || p.LegacyState == "OPEN" {
			continue
		}
		switch {
		case p.LegacyState != "CLOSED" && p.LegacyState != "LOCKED":
			p.Mirror = "skipped: unknown legacy state " + p.LegacyState
			continue
		case p.Match != "matched" && p.Match != "pending_materialize":
			p.Mirror = "skipped: no single REF-05 period matched by date"
			continue
		case p.LegacyState == "CLOSED" && p.Ref05State != "" && p.Ref05State != "OPEN":
			p.Mirror = "already_mirrored"
			continue
		case p.LegacyState == "LOCKED" && p.Ref05State == "HARD_CLOSED":
			p.Mirror = "already_mirrored"
			continue
		}
		if !r.cfg.Apply {
			p.Mirror = "would_mirror"
			continue
		}
		if r.mirrorDisabled {
			p.Mirror = "skipped: mirroring disabled (earlier 409)"
			continue
		}
		var out mirrorResult
		_, _, err := r.c.do(ctx, actor{r.cfg.MirrorPrincipal, entity}, "POST", r.cfg.CloseURL+"/v1/close/periods/"+url.PathEscape(p.FiscalPeriodID)+":mirror-to-period-service", nil,
			idemKey("mirror", r.cfg.Tenant, entity, p.FiscalPeriodID, p.LegacyState), &out)
		if err != nil {
			p.Mirror = "error"
			if isStatus(err, 409) {
				r.mirrorDisabled = true
				er.Errors = append(er.Errors, "mirror: financial-close-svc answered 409 (is PERIOD_SERVICE_MIRROR=on?); remaining mirror calls skipped: "+err.Error())
			} else {
				er.Errors = append(er.Errors, fmt.Sprintf("mirror %s: %v", p.Name, err))
			}
			continue
		}
		// financial-close-svc answers 200 even when a REF-05 step failed; the
		// truth is in actions[].outcome, so a failed action is an error here, not
		// a successful mirror.
		failed := ""
		for _, a := range out.Actions {
			p.MirrorActions = append(p.MirrorActions, Action{a.Command, a.Outcome, a.Ref})
			if strings.EqualFold(a.Outcome, "failed") && failed == "" {
				failed = a.Command + ": " + a.Reason
			}
		}
		if failed != "" {
			p.Mirror = "error"
			er.Errors = append(er.Errors, fmt.Sprintf("mirror %s: REF-05 step failed (%s)", p.Name, failed))
			continue
		}
		p.Mirror = "mirrored"
	}
}

// ── human summary ────────────────────────────────────────────────────────────

func (rep *Report) Human() string {
	var b strings.Builder
	fmt.Fprintf(&b, "period-backfill %s  tenant=%s  fiscal-year-start=%s  scope=%s\n", strings.ToUpper(rep.Mode), rep.Tenant, rep.FYStart, rep.Scope)
	if rep.Mode == "dry-run" {
		b.WriteString("DRY RUN: nothing was written. Re-run with --apply to execute this plan.\n")
	}
	for _, e := range rep.Entities {
		fmt.Fprintf(&b, "\nentity %s  fiscal years %v\n", e.LegalEntityID, e.FiscalYears)
		for _, s := range e.Calendar.Steps {
			fmt.Fprintf(&b, "  calendar: %s\n", s)
		}
		for _, m := range e.Materialize {
			fmt.Fprintf(&b, "  materialize FY%d: %s", m.FiscalYear, m.Action)
			if m.Created+m.Existing > 0 {
				fmt.Fprintf(&b, " (created %d, existing %d)", m.Created, m.Existing)
			}
			if m.Note != "" {
				fmt.Fprintf(&b, " - %s", m.Note)
			}
			b.WriteString("\n")
		}
		for _, p := range e.Periods {
			if p.Mapping == "unmapped" {
				fmt.Fprintf(&b, "  UNMAPPED %s (%s..%s, %s): %s\n", p.Name, p.Start, p.End, p.LegacyState, p.Reason)
			} else if p.Mirror != "" || p.Match != "matched" {
				fmt.Fprintf(&b, "  %s %s match=%s mirror=%s\n", p.Name, p.LegacyState, p.Match, p.Mirror)
			}
		}
		for _, a := range e.Anomalies {
			info := ""
			if !a.Blocking {
				info = " (info)"
			}
			fmt.Fprintf(&b, "  ANOMALY [%s]%s %s\n", a.Kind, info, a.Detail)
		}
		for _, x := range e.Errors {
			fmt.Fprintf(&b, "  ERROR %s\n", x)
		}
	}
	s := rep.Summary
	fmt.Fprintf(&b, "\nsummary: entities=%d legacy=%d mapped=%d unmapped=%d matched=%d unmatched=%d anomalies=%d (blocking %d) errors=%d would_mirror=%d mirrored=%d\n",
		s.Entities, s.LegacyPeriods, s.Mapped, s.Unmapped, s.Matched, s.Unmatched, s.Anomalies, s.BlockingAnomalies, s.Errors, s.WouldMirror, s.Mirrored)
	fmt.Fprintf(&b, "mirror stage: %s\n", rep.MirrorStage)
	return b.String()
}
