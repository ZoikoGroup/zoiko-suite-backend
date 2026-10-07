package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	tTenant = "11111111-1111-1111-1111-111111111111"
	tEntity = "22222222-2222-2222-2222-222222222222"
	tMaker  = "33333333-3333-3333-3333-333333333333"
	tCheck  = "66666666-6666-6666-6666-666666666666"
)

type call struct {
	Svc, Method, Path, Principal, IdemKey string
}

// world is one stateful fake of the three services, so a re-run sees what the
// previous run left behind.
type world struct {
	mu sync.Mutex

	fyMonth int
	legacy  []legacyPeriod

	// fiscal-calendar-svc
	calExists   bool
	calID       string
	version     calendarVersion
	proposedBy  string
	createdResp []byte
	createKey   string

	// accounting-period-svc
	ref []refPeriod

	// financial-close-svc
	mirrorOn   bool
	mirrorFail bool // answer 200 but report a failed REF-05 step, as the real replay endpoint does

	calls []call
}

func (w *world) record(svc string, r *http.Request) {
	w.calls = append(w.calls, call{svc, r.Method, r.URL.Path, r.Header.Get("X-Principal-Id"), r.Header.Get("Idempotency-Key")})
}

func (w *world) writes() []call {
	var out []call
	for _, c := range w.calls {
		if c.Method != "GET" {
			out = append(out, c)
		}
	}
	return out
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

func apiErr(rw http.ResponseWriter, status int, code, msg string) {
	writeJSON(rw, status, map[string]string{"code": code, "message": msg})
}

func (w *world) calendarHandler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.record("calendar", r)
		p := r.URL.Path
		switch {
		case r.Method == "GET" && p == "/v1/fiscal-calendars:resolve":
			d := r.URL.Query().Get("date")
			if !w.calExists || w.version.Status != "ACTIVE" || d < w.version.EffectiveFrom {
				apiErr(rw, 404, "NOT_FOUND", "no fiscal calendar version is in force")
				return
			}
			writeJSON(rw, 200, resolution{CalendarID: w.calID, VersionID: w.version.VersionID})
		case r.Method == "GET" && strings.HasPrefix(p, "/v1/fiscal-calendar-versions/"):
			writeJSON(rw, 200, w.version)
		case r.Method == "GET" && strings.HasSuffix(p, "/versions"):
			writeJSON(rw, 200, map[string]any{"items": []calendarVersion{w.version}})
		case r.Method == "POST" && p == "/v1/fiscal-calendars":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if w.calExists {
				if r.Header.Get("Idempotency-Key") == w.createKey {
					rw.WriteHeader(200)
					_, _ = rw.Write(w.createdResp)
					return
				}
				apiErr(rw, 409, "DUPLICATE_CANDIDATE", fmt.Sprintf("calendar %q already exists for this legal entity (calendar_id %s)", body["code"], w.calID))
				return
			}
			w.calExists, w.calID, w.proposedBy, w.createKey = true, "cal-1", r.Header.Get("X-Principal-Id"), r.Header.Get("Idempotency-Key")
			w.version = calendarVersion{VersionID: "ver-1", CalendarID: w.calID, VersionNo: 1, StartMonth: int(body["fiscal_year_start_month"].(float64)),
				StartDay: int(body["fiscal_year_start_day"].(float64)), EffectiveFrom: body["effective_from"].(string), Status: "DRAFT", Version: 1}
			w.version.Pattern.Type = body["pattern"].(map[string]any)["type"].(string)
			var cc calendarCreated
			cc.Calendar.CalendarID, cc.Version = w.calID, w.version
			w.createdResp, _ = json.Marshal(cc)
			writeJSON(rw, 201, cc)
		case r.Method == "POST" && strings.HasSuffix(p, ":approve"):
			var body struct {
				ExpectedVersion int64 `json:"expected_version"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("X-Principal-Id") == w.proposedBy {
				apiErr(rw, 409, "SOD_DENIED", "the proposer may not approve")
				return
			}
			if w.version.Status != "DRAFT" || body.ExpectedVersion != w.version.Version {
				apiErr(rw, 409, "INVALID_TRANSITION", "bad state/version")
				return
			}
			w.version.Status, w.version.Version = "APPROVED", w.version.Version+1
			writeJSON(rw, 200, w.version)
		case r.Method == "POST" && strings.HasSuffix(p, ":activate"):
			var body struct {
				ExpectedVersion int64 `json:"expected_version"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if w.version.Status != "APPROVED" || body.ExpectedVersion != w.version.Version {
				apiErr(rw, 409, "INVALID_TRANSITION", "bad state/version")
				return
			}
			w.version.Status, w.version.Version = "ACTIVE", w.version.Version+1
			writeJSON(rw, 200, w.version)
		default:
			apiErr(rw, 404, "NOT_FOUND", "unexpected "+r.Method+" "+p)
		}
	})
}

func (w *world) periodHandler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.record("period", r)
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/accounting-periods":
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			items := []refPeriod{}
			for i := offset; i < len(w.ref) && len(items) < limit; i++ {
				items = append(items, w.ref[i])
			}
			writeJSON(rw, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
		case r.Method == "POST" && r.URL.Path == "/v1/accounting-periods:materialize":
			var body struct {
				FiscalYear int    `json:"fiscal_year"`
				BookScope  string `json:"book_scope"`
				VersionID  string `json:"calendar_version_id"`
				CalendarID string `json:"calendar_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			a := time.Date(body.FiscalYear, time.Month(w.fyMonth), 1, 0, 0, 0, 0, time.UTC)
			created := 0
			for i := 0; i < 12; i++ {
				s, e := a.AddDate(0, i, 0), a.AddDate(0, i+1, -1)
				key := fmt.Sprintf("FY%d-P%02d", body.FiscalYear, i+1)
				dup := false
				for _, x := range w.ref {
					dup = dup || (x.PeriodKey == key && x.CalendarVersionID == body.VersionID)
				}
				if dup {
					continue
				}
				w.ref = append(w.ref, refPeriod{PeriodID: "ref-" + key, CalendarID: body.CalendarID, CalendarVersionID: body.VersionID, BookScope: body.BookScope,
					PeriodKey: key, FiscalYear: body.FiscalYear, StartDate: fmtDate(s), EndDate: fmtDate(e), Kind: "NORMAL", State: "OPEN"})
				created++
			}
			writeJSON(rw, 201, materializeResult{Created: created, Existing: 12 - created, BoundaryDrift: []string{}})
		default:
			apiErr(rw, 404, "NOT_FOUND", "unexpected "+r.Method+" "+r.URL.Path)
		}
	})
}

func (w *world) closeHandler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.record("close", r)
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/close/periods/":
			if r.URL.Query().Get("legal_entity_id") != tEntity {
				writeJSON(rw, 200, []legacyPeriod{})
				return
			}
			writeJSON(rw, 200, w.legacy)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, ":mirror-to-period-service"):
			if !w.mirrorOn {
				apiErr(rw, 409, "MIRROR_DISABLED", "mirroring is disabled")
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/close/periods/"), ":mirror-to-period-service")
			if w.mirrorFail {
				writeJSON(rw, 200, map[string]any{"fiscal_period_id": id, "legacy_state": "LOCKED",
					"actions": []map[string]string{{"command": "HARD_CLOSE", "outcome": "failed", "reason": "source_unverified"}}})
				return
			}
			for _, l := range w.legacy {
				if l.FiscalPeriodID != id {
					continue
				}
				for i := range w.ref {
					if w.ref[i].StartDate == l.PeriodStart[:10] {
						if l.CloseStatus == "LOCKED" {
							w.ref[i].State = "HARD_CLOSED"
						} else {
							w.ref[i].State = "SOFT_CLOSED"
						}
					}
				}
				writeJSON(rw, 200, map[string]any{"fiscal_period_id": id, "legacy_state": l.CloseStatus,
					"actions": []map[string]string{{"command": "SOFT_CLOSE", "outcome": "APPLIED", "ref": "x"}}})
				return
			}
			apiErr(rw, 404, "NOT_FOUND", "no such period")
		default:
			apiErr(rw, 404, "NOT_FOUND", "unexpected "+r.Method+" "+r.URL.Path)
		}
	})
}

func lp(id, name, start, end, st string) legacyPeriod {
	return legacyPeriod{FiscalPeriodID: id, PeriodName: name, PeriodStart: start + "T00:00:00Z", PeriodEnd: end + "T00:00:00Z", CloseStatus: st}
}

func newWorld(fyMonth int, legacy ...legacyPeriod) (*world, config, func()) {
	w := &world{fyMonth: fyMonth, legacy: legacy}
	cal, per, cls := httptest.NewServer(w.calendarHandler()), httptest.NewServer(w.periodHandler()), httptest.NewServer(w.closeHandler())
	cfg := config{Tenant: tTenant, Entities: []string{tEntity}, StartMonth: fyMonth, StartDay: 1, Maker: tMaker, Checker: tCheck, MirrorPrincipal: tMaker,
		CalendarURL: cal.URL, PeriodURL: per.URL, CloseURL: cls.URL, CalendarCode: "CALENDAR-MONTHS", Scope: "PRIMARY", Reason: "test"}
	return w, cfg, func() { cal.Close(); per.Close(); cls.Close() }
}

func run(cfg config) *Report {
	rn := &runner{cfg: cfg, c: newClient(5*time.Second, cfg.Tenant, "test-run")}
	return rn.run(context.Background())
}

// calendar-year legacy data: 2026-01..2026-03 plus one Q1-style row and one
// row whose dates are not a calendar month.
func sampleLegacy() []legacyPeriod {
	return []legacyPeriod{
		lp("p-01", "2026-01", "2026-01-01", "2026-01-31", "LOCKED"),
		lp("p-02", "2026-02", "2026-02-01", "2026-02-28", "CLOSED"),
		lp("p-03", "2026-03", "2026-03-01", "2026-03-31", "OPEN"),
	}
}

func TestDryRunMakesNoWrites(t *testing.T) {
	legacy := append(sampleLegacy(),
		lp("p-q", "2024-Q1", "2024-01-01", "2024-03-31", "LOCKED"),
		lp("p-x", "2026-04", "2026-04-01", "2026-04-29", "OPEN"))
	w, cfg, stop := newWorld(1, legacy...)
	defer stop()
	cfg.MirrorClosed = true
	rep := run(cfg)

	if n := len(w.writes()); n != 0 {
		t.Fatalf("dry-run issued %d writes: %+v", n, w.writes())
	}
	if rep.Mode != "dry-run" {
		t.Fatalf("mode = %q", rep.Mode)
	}
	e := rep.Entities[0]
	if len(e.Errors) != 0 {
		t.Fatalf("errors: %v", e.Errors)
	}
	if got := strings.Join(e.Calendar.Steps, "|"); !strings.Contains(got, "would create") || !strings.Contains(got, "would activate") {
		t.Fatalf("calendar steps = %v", e.Calendar.Steps)
	}
	if rep.Summary.Unmapped != 2 || rep.Summary.Mapped != 3 {
		t.Fatalf("mapped/unmapped = %d/%d", rep.Summary.Mapped, rep.Summary.Unmapped)
	}
	reasons := map[string]string{}
	for _, p := range e.Periods {
		reasons[p.Name] = p.Reason
	}
	if !strings.Contains(reasons["2024-Q1"], "does not parse") || !strings.Contains(reasons["2026-04"], "not exactly one calendar month") {
		t.Fatalf("unmapped reasons = %v", reasons)
	}
	// 2024-Q1 is unmapped so it does not extend the span: FY2026 only.
	if len(e.FiscalYears) != 1 || e.FiscalYears[0] != 2026 {
		t.Fatalf("fiscal years = %v", e.FiscalYears)
	}
	if e.Materialize[0].Action != "would_materialize" {
		t.Fatalf("materialize = %+v", e.Materialize)
	}
	if rep.Summary.WouldMirror != 2 {
		t.Fatalf("would_mirror = %d (want the LOCKED and CLOSED periods, not the OPEN one)", rep.Summary.WouldMirror)
	}
	if rep.Failed(false) {
		t.Fatalf("a dry run with unmapped periods must not fail without --strict")
	}
	if !rep.Failed(true) {
		t.Fatalf("--strict must fail on unmapped periods")
	}
}

func TestApplySequenceAndPrincipals(t *testing.T) {
	w, cfg, stop := newWorld(1, sampleLegacy()...)
	defer stop()
	cfg.Apply = true
	rep := run(cfg)
	if rep.Summary.Errors != 0 {
		t.Fatalf("errors: %v", rep.Entities[0].Errors)
	}
	var seq []string
	for _, c := range w.writes() {
		seq = append(seq, c.Svc+" "+c.Path+" "+c.Principal)
	}
	want := []string{
		"calendar /v1/fiscal-calendars " + tMaker,
		"calendar /v1/fiscal-calendar-versions/ver-1:approve " + tCheck,
		"calendar /v1/fiscal-calendar-versions/ver-1:activate " + tCheck,
		"period /v1/accounting-periods:materialize " + tMaker,
	}
	if strings.Join(seq, "\n") != strings.Join(want, "\n") {
		t.Fatalf("write sequence:\n%s\nwant:\n%s", strings.Join(seq, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range w.writes() {
		if c.IdemKey == "" {
			t.Fatalf("write without Idempotency-Key: %+v", c)
		}
	}
	if rep.Summary.Matched != 3 || rep.Summary.Unmatched != 0 {
		t.Fatalf("matched/unmatched = %d/%d", rep.Summary.Matched, rep.Summary.Unmatched)
	}
	if rep.Failed(true) {
		t.Fatalf("clean apply should not fail even with --strict: %+v", rep.Summary)
	}
}

func TestRerunIsIdempotent(t *testing.T) {
	w, cfg, stop := newWorld(1, sampleLegacy()...)
	defer stop()
	cfg.Apply = true
	run(cfg)
	w.mu.Lock()
	w.calls = nil
	w.mu.Unlock()
	rep := run(cfg)
	if n := len(w.writes()); n != 0 {
		t.Fatalf("re-run issued %d writes: %+v", n, w.writes())
	}
	if rep.Entities[0].Materialize[0].Action != "already_materialized" {
		t.Fatalf("materialize = %+v", rep.Entities[0].Materialize)
	}
}

func TestResumesInterruptedCalendar(t *testing.T) {
	// A previous run created the draft (under a different key) and died.
	w, cfg, stop := newWorld(1, sampleLegacy()...)
	defer stop()
	w.calExists, w.calID, w.proposedBy, w.createKey = true, "cal-1", tMaker, "some-other-run"
	w.version = calendarVersion{VersionID: "ver-1", CalendarID: "cal-1", VersionNo: 1, StartMonth: 1, StartDay: 1, EffectiveFrom: "2026-01-01", Status: "DRAFT", Version: 1}
	w.version.Pattern.Type = "CALENDAR_MONTHS"
	cfg.Apply = true
	rep := run(cfg)
	if rep.Summary.Errors != 0 {
		t.Fatalf("errors: %v", rep.Entities[0].Errors)
	}
	if rep.Summary.Matched != 3 {
		t.Fatalf("matched = %d", rep.Summary.Matched)
	}
}

func TestIncompatibleExistingCalendarIsNeverChanged(t *testing.T) {
	w, cfg, stop := newWorld(1, sampleLegacy()...)
	defer stop()
	w.calExists, w.calID = true, "cal-9"
	w.version = calendarVersion{VersionID: "ver-9", CalendarID: "cal-9", VersionNo: 1, StartMonth: 1, StartDay: 1, EffectiveFrom: "2020-01-01", Status: "ACTIVE", Version: 3}
	w.version.Pattern.Type = "WEEK_PATTERN"
	cfg.Apply = true
	rep := run(cfg)
	if n := len(w.writes()); n != 0 {
		t.Fatalf("incompatible calendar was written to: %+v", w.writes())
	}
	found := false
	for _, a := range rep.Entities[0].Anomalies {
		found = found || a.Kind == "calendar_incompatible"
	}
	if !found || !rep.Failed(true) {
		t.Fatalf("anomalies = %+v", rep.Entities[0].Anomalies)
	}
}

func TestMatchingIsByDateNotName(t *testing.T) {
	// FY starts 1 April: legacy "2026-07" is REF-05 period FY2026-P04.
	legacy := []legacyPeriod{lp("p-07", "2026-07", "2026-07-01", "2026-07-31", "OPEN")}
	_, cfg, stop := newWorld(4, legacy...)
	defer stop()
	cfg.Apply = true
	rep := run(cfg)
	if rep.Summary.Errors != 0 {
		t.Fatalf("errors: %v", rep.Entities[0].Errors)
	}
	p := rep.Entities[0].Periods[0]
	if p.Match != "matched" || p.Ref05Key != "FY2026-P04" {
		t.Fatalf("period = %+v", p)
	}
	if fy := rep.Entities[0].FiscalYears; len(fy) != 1 || fy[0] != 2026 {
		t.Fatalf("fiscal years = %v", fy)
	}
	// 2026-02 belongs to FY2025 when the year starts in April.
	_, cfg2, stop2 := newWorld(4, lp("p-02", "2026-02", "2026-02-01", "2026-02-28", "OPEN"))
	defer stop2()
	cfg2.Apply = true
	rep2 := run(cfg2)
	if fy := rep2.Entities[0].FiscalYears; len(fy) != 1 || fy[0] != 2025 {
		t.Fatalf("fiscal years = %v", fy)
	}
}

func TestUnmatchedAfterApplyIsAnAnomalyNotForced(t *testing.T) {
	// A legacy period that is a real calendar month but outside the explicit span.
	w, cfg, stop := newWorld(1, sampleLegacy()...)
	defer stop()
	cfg.Apply, cfg.FromFY, cfg.ToFY = true, 2027, 2027
	rep := run(cfg)
	if rep.Summary.Unmatched != 3 {
		t.Fatalf("unmatched = %d", rep.Summary.Unmatched)
	}
	if !rep.Failed(true) || rep.Failed(false) {
		t.Fatalf("strict/non-strict mismatch")
	}
	_ = w
}

func TestGapAndOverlapAreReported(t *testing.T) {
	legacy := []legacyPeriod{
		lp("a", "2026-01", "2026-01-01", "2026-01-31", "OPEN"),
		lp("b", "2026-03", "2026-03-01", "2026-03-31", "OPEN"), // gap over February
		lp("c", "2026-Q1", "2026-03-15", "2026-04-15", "OPEN"), // overlaps March
	}
	_, cfg, stop := newWorld(1, legacy...)
	defer stop()
	rep := run(cfg)
	kinds := map[string]bool{}
	for _, a := range rep.Entities[0].Anomalies {
		kinds[a.Kind] = true
	}
	if !kinds["gap"] || !kinds["overlap"] {
		t.Fatalf("anomalies = %+v", rep.Entities[0].Anomalies)
	}
}

func TestMirrorOnlyNonOpenAndOnlyWhenSelected(t *testing.T) {
	w, cfg, stop := newWorld(1, sampleLegacy()...)
	defer stop()
	w.mirrorOn = true
	cfg.Apply = true
	run(cfg) // no --mirror-closed
	for _, c := range w.calls {
		if strings.Contains(c.Path, "mirror") {
			t.Fatalf("mirror called without --mirror-closed: %+v", c)
		}
	}

	cfg.MirrorClosed = true
	rep := run(cfg)
	var mirrored []string
	for _, c := range w.calls {
		if strings.Contains(c.Path, "mirror") {
			mirrored = append(mirrored, c.Path)
			if c.IdemKey == "" {
				t.Fatalf("mirror without Idempotency-Key")
			}
		}
	}
	if len(mirrored) != 2 || strings.Contains(strings.Join(mirrored, ","), "p-03") {
		t.Fatalf("mirror calls = %v (want p-01 and p-02 only)", mirrored)
	}
	if rep.Summary.Mirrored != 2 || rep.Summary.Errors != 0 {
		t.Fatalf("summary = %+v", rep.Summary)
	}

	// Re-run: REF-05 already reflects the closes, so nothing more is replayed.
	w.mu.Lock()
	w.calls = nil
	w.mu.Unlock()
	rep = run(cfg)
	if n := len(w.writes()); n != 0 {
		t.Fatalf("mirror re-run wrote: %+v", w.writes())
	}
	if rep.Summary.Mirrored != 0 {
		t.Fatalf("mirrored = %d", rep.Summary.Mirrored)
	}
}

func TestMirrorDisabledStopsAfterFirst409(t *testing.T) {
	w, cfg, stop := newWorld(1, sampleLegacy()...)
	defer stop()
	w.mirrorOn = false
	cfg.Apply, cfg.MirrorClosed = true, true
	rep := run(cfg)
	n := 0
	for _, c := range w.calls {
		if strings.Contains(c.Path, "mirror") {
			n++
		}
	}
	if n != 1 || rep.Summary.Errors != 1 || !rep.Failed(false) {
		t.Fatalf("mirror calls=%d errors=%d", n, rep.Summary.Errors)
	}
}

func TestSoDRejectedUpFront(t *testing.T) {
	w, cfg, stop := newWorld(1, sampleLegacy()...)
	defer stop()
	var out, errb bytes.Buffer
	code := realMain([]string{"--tenant", cfg.Tenant, "--entity", tEntity, "--maker", tMaker, "--checker", strings.ToUpper(tMaker),
		"--calendar-url", cfg.CalendarURL, "--period-url", cfg.PeriodURL, "--close-url", cfg.CloseURL, "--apply"}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "segregation of duties") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if len(w.calls) != 0 {
		t.Fatalf("calls made before SoD check: %+v", w.calls)
	}
	// Missing checker is also a usage error.
	if code := realMain([]string{"--tenant", cfg.Tenant, "--entity", tEntity, "--maker", tMaker}, &out, &errb); code != 2 {
		t.Fatalf("missing checker code=%d", code)
	}
}

func TestCLIReportAndExitCodes(t *testing.T) {
	legacy := append(sampleLegacy(), lp("p-q", "2025-Q4", "2025-10-01", "2025-12-31", "LOCKED"))
	w, cfg, stop := newWorld(1, legacy...)
	defer stop()
	dir := t.TempDir()
	ef := filepath.Join(dir, "entities.txt")
	_ = os.WriteFile(ef, []byte("# demo\n"+tEntity+"  # main\n\n"+tEntity+"\n"), 0o600)
	rp := filepath.Join(dir, "report.json")
	base := []string{"--tenant", cfg.Tenant, "--entities-file", ef, "--maker", tMaker, "--checker", tCheck,
		"--calendar-url", cfg.CalendarURL, "--period-url", cfg.PeriodURL, "--close-url", cfg.CloseURL, "--report", rp}

	var out, errb bytes.Buffer
	if code := realMain(base, &out, &errb); code != 0 {
		t.Fatalf("dry-run code=%d stderr=%s", code, errb.String())
	}
	if n := len(w.writes()); n != 0 {
		t.Fatalf("dry run wrote: %+v", w.writes())
	}
	if !strings.Contains(out.String(), "DRY RUN") || !strings.Contains(out.String(), "UNMAPPED 2025-Q4") {
		t.Fatalf("human summary:\n%s", out.String())
	}
	raw, err := os.ReadFile(rp)
	if err != nil {
		t.Fatal(err)
	}
	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("report is not JSON: %v", err)
	}
	if rep.Mode != "dry-run" || len(rep.Entities) != 1 || rep.Summary.Unmapped != 1 {
		t.Fatalf("report = %+v", rep.Summary)
	}
	if strings.Contains(string(raw), "token") || strings.Contains(string(raw), "secret") {
		t.Fatalf("report mentions secrets")
	}

	if code := realMain(append(append([]string{}, base...), "--strict"), &out, &errb); code != 1 {
		t.Fatalf("strict dry-run with unmapped code=%d", code)
	}
	if code := realMain(append(append([]string{}, base...), "--apply"), &out, &errb); code != 0 {
		t.Fatalf("apply code=%d", code)
	}
	if code := realMain(append(append([]string{}, base...), "--apply", "--strict"), &out, &errb); code != 1 {
		t.Fatalf("strict apply with unmapped code=%d", code)
	}
}

// financial-close-svc's replay answers 200 even when a REF-05 step failed. That
// must not be reported as a successful mirror.
func TestMirrorFailedActionIsAnErrorNotAMirror(t *testing.T) {
	w, cfg, stop := newWorld(1, sampleLegacy()...)
	defer stop()
	w.mirrorOn = true
	w.mirrorFail = true
	cfg.Apply = true
	cfg.MirrorClosed = true
	rep := run(cfg)
	if rep.Summary.Mirrored != 0 {
		t.Fatalf("mirrored = %d, want 0: a failed REF-05 step is not a mirror", rep.Summary.Mirrored)
	}
	if rep.Summary.Errors == 0 {
		t.Fatalf("a failed REF-05 step must be reported as an error, summary = %+v", rep.Summary)
	}
	found := false
	for _, e := range rep.Entities[0].Errors {
		if strings.Contains(e, "source_unverified") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the failure reason must reach the report: %+v", rep.Entities[0].Errors)
	}
}
