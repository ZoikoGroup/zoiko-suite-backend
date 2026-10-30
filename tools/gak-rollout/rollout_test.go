package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeToday is the day the fake service says it is: a mapping made through its API is in force from then, never earlier.
var fakeToday = time.Now().UTC().Truncate(24 * time.Hour)

// futureDay is a day after today, for a pack that goes live later.
var futureDay = fakeToday.AddDate(0, 0, 3).Format("2006-01-02")

// fakeGL is the part of general-ledger-svc the tool talks to, in memory.
type fakeGL struct {
	mu       sync.Mutex
	books    []bookView
	mappings []mappingView
	packs    map[string][]string // catalogue: name -> mapping keys
	instance map[string]bool     // pack name + version already instantiated
	calls    []string
	deny     map[string]int // "METHOD path" -> status to answer with
	srv      *httptest.Server
}

func newFake(t *testing.T) *fakeGL {
	f := &fakeGL{packs: map[string][]string{"ar-standard": {"AR_CONTROL", "REVENUE"}, "payroll-result": {"PAYROLL_EXPENSE", "NET_PAY_LIABILITY"}}, instance: map[string]bool{}, deny: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGL) writes() []string {
	var w []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET ") {
			w = append(w, c)
		}
	}
	return w
}

func reply(w http.ResponseWriter, st int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGL) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	if r.Header.Get("X-Tenant-Id") == "" || r.Header.Get("X-Principal-Id") == "" {
		reply(w, 401, map[string]string{"error": "identity_missing"})
		return
	}
	if st, ok := f.deny[key]; ok {
		reply(w, st, map[string]string{"error": "authorization_denied"})
		return
	}
	switch {
	case key == "GET /v1/posting-rule-packs":
		var ps []map[string]any
		for n, k := range f.packs {
			ps = append(ps, map[string]any{"name": n, "mapping_keys": k})
		}
		reply(w, 200, map[string]any{"packs": ps})
	case key == "GET /v1/accounting-books":
		reply(w, 200, map[string]any{"books": f.books, "total": len(f.books)})
	case key == "POST /v1/accounting-books":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		nb := bookView{BookID: "book-" + b["book_code"].(string), BookCode: b["book_code"].(string), BookType: b["book_type"].(string),
			AccountingFrameworkID: b["accounting_framework_id"].(string), FunctionalCurrency: b["functional_currency"].(string), Status: "DRAFT"}
		f.books = append(f.books, nb)
		reply(w, 201, nb)
	case strings.HasPrefix(r.URL.Path, "/v1/accounting-books/") && strings.HasSuffix(r.URL.Path, "/activate"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/accounting-books/"), "/activate")
		for i := range f.books {
			if f.books[i].BookID == id {
				f.books[i].Status = "ACTIVE"
			}
		}
		reply(w, 200, map[string]any{"book_id": id})
	case key == "GET /v1/account-mappings":
		if f.mappings == nil {
			reply(w, 200, []mappingView{})
			return
		}
		reply(w, 200, f.mappings)
	case key == "POST /v1/account-mappings":
		var m map[string]string
		_ = json.NewDecoder(r.Body).Decode(&m)
		if m["account_code"] == "9999" {
			reply(w, 400, map[string]string{"error": "invalid_target_account", "detail": "no such account"})
			return
		}
		f.mappings = append(f.mappings, mappingView{MappingKey: m["mapping_key"], AccountCode: m["account_code"], EffectiveFrom: fakeToday, CreatedAt: time.Now()})
		reply(w, 201, m)
	case strings.HasPrefix(r.URL.Path, "/v1/posting-rule-packs/") && strings.HasSuffix(r.URL.Path, "/instantiate"):
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/posting-rule-packs/"), "/instantiate")
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		id := name + " " + b["version"]
		if f.instance[id] {
			reply(w, 409, map[string]any{"error": "already_exists", "detail": id + " already exists"})
			return
		}
		f.instance[id] = true
		reply(w, 201, map[string]any{"pack": name})
	case key == "GET /v1/rollout-readiness":
		pack := r.URL.Query().Get("pack")
		have := map[string]bool{}
		on, _ := time.Parse("2006-01-02", r.URL.Query().Get("on"))
		for _, m := range f.mappings {
			if m.EffectiveFrom.IsZero() || !m.EffectiveFrom.After(on.Add(24*time.Hour-time.Second)) {
				have[m.MappingKey] = true
			}
		}
		rep := map[string]any{"ready": true, "blocking": 0}
		var items []map[string]any
		for _, k := range f.packs[pack] {
			if !have[k] {
				items = append(items, map[string]any{"kind": "MAPPING", "key": k, "state": "UNMAPPED", "blocking": true, "remedy": "map " + k + " to an account"})
				rep["ready"], rep["blocking"] = false, rep["blocking"].(int)+1
			} else {
				items = append(items, map[string]any{"kind": "MAPPING", "key": k, "state": "OK", "blocking": false})
			}
		}
		rep["items"] = items
		reply(w, 200, rep)
	default:
		reply(w, 404, map[string]string{"error": "not_found"})
	}
}

func testConfig() *Config {
	return &Config{
		LegalEntityID: "22222222-2222-4222-8222-222222222222",
		Book: BookSpec{BookCode: "STAT", BookType: "PRIMARY_STATUTORY", AccountingFrameworkID: "IFRS", FunctionalCurrency: "GBP",
			FiscalCalendarID: "CAL", ChartOfAccountsID: "COA", DefaultLedgerID: "LED", EffectiveFrom: "2026-01-01"},
		Mappings: map[string]string{"AR_CONTROL": "1100", "REVENUE": "4000"},
		Packs:    []PackSpec{{Name: "ar-standard", Version: "1.0.0", EffectiveFrom: "2026-01-01"}},
	}
}

func (f *fakeGL) client() *client {
	return newClient(f.srv.URL, "11111111-1111-4111-8111-111111111111", "rollout-operator", "22222222-2222-4222-8222-222222222222", 5*time.Second)
}

func statuses(r *Report, area string) map[string]string {
	m := map[string]string{}
	for _, s := range r.Steps {
		if s.Area == area && s.Status != StWarning {
			m[s.Name] = s.Status
		}
	}
	return m
}

func TestDryRunReadsAndWritesNothing(t *testing.T) {
	f := newFake(t)
	rep, err := Run(context.Background(), f.client(), testConfig(), false, fakeToday)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("a dry run writes nothing, but made %v", w)
	}
	if statuses(rep, "book")["STAT"] != StPlanned || statuses(rep, "mapping")["AR_CONTROL"] != StPlanned || statuses(rep, "mapping")["REVENUE"] != StPlanned ||
		statuses(rep, "pack")["ar-standard 1.0.0"] != StPlanned {
		t.Fatalf("%+v", rep.Steps)
	}
	// the mappings the plan would add are not held against the pack
	if statuses(rep, "readiness")["ar-standard"] != StOK || rep.NeedsAttention() {
		t.Fatalf("%+v", rep.Steps)
	}
}

func TestApplyPreparesTheTenantAndRepeatingIsSafe(t *testing.T) {
	f := newFake(t)
	rep, err := Run(context.Background(), f.client(), testConfig(), true, fakeToday)
	if err != nil {
		t.Fatal(err)
	}
	if statuses(rep, "book")["STAT"] != StDone || statuses(rep, "mapping")["REVENUE"] != StDone || statuses(rep, "pack")["ar-standard 1.0.0"] != StDone ||
		statuses(rep, "readiness")["ar-standard"] != StOK || rep.NeedsAttention() {
		t.Fatalf("%+v", rep.Steps)
	}
	if rep.BookID != "book-STAT" || f.books[0].Status != "ACTIVE" || len(f.mappings) != 2 {
		t.Fatalf("book %q status %q mappings %d", rep.BookID, f.books[0].Status, len(f.mappings))
	}

	before := len(f.writes())
	again, err := Run(context.Background(), f.client(), testConfig(), true, fakeToday)
	if err != nil {
		t.Fatal(err)
	}
	if statuses(again, "book")["STAT"] != StOK || statuses(again, "mapping")["AR_CONTROL"] != StOK || statuses(again, "pack")["ar-standard 1.0.0"] != StOK || again.NeedsAttention() {
		t.Fatalf("%+v", again.Steps)
	}
	// the only write a repeat makes is the pack's instantiate, which the service answers as already there
	for _, w := range f.writes()[before:] {
		if !strings.HasSuffix(w, "/instantiate") {
			t.Fatalf("a repeat must not write %s", w)
		}
	}
	if len(f.mappings) != 2 || len(f.books) != 1 {
		t.Fatal("a repeat creates nothing twice")
	}
}

func TestAMappingToAnotherAccountIsAConflictAndIsNeverChanged(t *testing.T) {
	f := newFake(t)
	f.mappings = []mappingView{{MappingKey: "REVENUE", AccountCode: "4100", CreatedAt: time.Now()}}
	rep, _ := Run(context.Background(), f.client(), testConfig(), true, fakeToday)
	if statuses(rep, "mapping")["REVENUE"] != StConflict || !rep.NeedsAttention() {
		t.Fatalf("%+v", rep.Steps)
	}
	if f.mappings[0].AccountCode != "4100" || len(f.mappings) != 2 { // only AR_CONTROL was added
		t.Fatalf("the existing mapping must stand: %+v", f.mappings)
	}
	for _, w := range f.writes() {
		if w == "POST /v1/account-mappings" && len(f.mappings) > 2 {
			t.Fatal("no second write for the conflicting key")
		}
	}
	// a superseded mapping (ended) is not current and does not conflict
	f2 := newFake(t)
	end := time.Now()
	f2.mappings = []mappingView{{MappingKey: "REVENUE", AccountCode: "4100", EffectiveTo: &end, CreatedAt: time.Now()}}
	rep, _ = Run(context.Background(), f2.client(), testConfig(), true, fakeToday)
	if statuses(rep, "mapping")["REVENUE"] != StDone {
		t.Fatalf("%+v", rep.Steps)
	}
}

func TestABookTakenOutOfUseIsNotReactivated(t *testing.T) {
	f := newFake(t)
	f.books = []bookView{{BookID: "b1", BookCode: "STAT", BookType: "PRIMARY_STATUTORY", AccountingFrameworkID: "IFRS", FunctionalCurrency: "GBP", Status: "SUSPENDED"}}
	rep, _ := Run(context.Background(), f.client(), testConfig(), true, fakeToday)
	if statuses(rep, "book")["STAT"] != StBlocked || !rep.NeedsAttention() {
		t.Fatalf("%+v", rep.Steps)
	}
	for _, w := range f.writes() {
		if strings.HasSuffix(w, "/activate") {
			t.Fatal("a suspended book is a person's decision")
		}
	}
	// a draft book is activated
	f2 := newFake(t)
	f2.books = []bookView{{BookID: "b2", BookCode: "STAT", BookType: "PRIMARY_STATUTORY", AccountingFrameworkID: "IFRS", FunctionalCurrency: "GBP", Status: "DRAFT"}}
	rep, _ = Run(context.Background(), f2.client(), testConfig(), true, fakeToday)
	if statuses(rep, "book")["STAT"] != StDone || f2.books[0].Status != "ACTIVE" {
		t.Fatalf("%+v", rep.Steps)
	}
}

func TestABookWhoseIdentityDiffersIsAConflict(t *testing.T) {
	f := newFake(t)
	f.books = []bookView{{BookID: "b1", BookCode: "STAT", BookType: "PRIMARY_STATUTORY", AccountingFrameworkID: "US-GAAP", FunctionalCurrency: "USD", Status: "ACTIVE"}}
	rep, _ := Run(context.Background(), f.client(), testConfig(), true, fakeToday)
	if statuses(rep, "book")["STAT"] != StConflict || rep.BookID != "b1" {
		t.Fatalf("%+v", rep.Steps)
	}
}

func TestReadinessGapsAreReportedWithTheirRemedy(t *testing.T) {
	f := newFake(t)
	cfg := testConfig()
	delete(cfg.Mappings, "REVENUE") // the pack needs it and nobody mapped it
	rep, _ := Run(context.Background(), f.client(), cfg, true, fakeToday)
	var detail string
	for _, s := range rep.Steps {
		if s.Area == "readiness" {
			detail = s.Detail
		}
	}
	if statuses(rep, "readiness")["ar-standard"] != StBlocked || !strings.Contains(detail, "REVENUE UNMAPPED") || !strings.Contains(detail, "map REVENUE") || !rep.NeedsAttention() {
		t.Fatalf("%+v", rep.Steps)
	}
}

func TestUnknownPackAndUnusedKeysAreCaught(t *testing.T) {
	f := newFake(t)
	cfg := testConfig()
	cfg.Packs = append(cfg.Packs, PackSpec{Name: "no-such-pack", Version: "1.0.0", EffectiveFrom: "2026-01-01"})
	cfg.Mappings["AR_CONTROLL"] = "1100" // a typo no pack uses
	rep, _ := Run(context.Background(), f.client(), cfg, false, fakeToday)
	if statuses(rep, "pack")["no-such-pack"] != StFailed || !rep.NeedsAttention() {
		t.Fatalf("%+v", rep.Steps)
	}
	warned := false
	for _, s := range rep.Steps {
		warned = warned || (s.Status == StWarning && s.Name == "AR_CONTROLL")
	}
	if !warned {
		t.Fatalf("a key no pack uses is warned about: %+v", rep.Steps)
	}
}

func TestARefusalFromTheServiceIsReportedNotHidden(t *testing.T) {
	f := newFake(t)
	f.deny["GET /v1/accounting-books"] = 403
	f.deny["POST /v1/account-mappings"] = 403
	rep, err := Run(context.Background(), f.client(), testConfig(), true, fakeToday)
	if err != nil {
		t.Fatal(err)
	}
	var books, maps string
	for _, s := range rep.Steps {
		if s.Area == "book" {
			books = s.Status + " " + s.Detail
		}
		if s.Area == "mapping" && s.Status == StFailed {
			maps = s.Detail
		}
	}
	if !strings.HasPrefix(books, StFailed) || !strings.Contains(books, "403") || !strings.Contains(maps, "authorization_denied") || !rep.NeedsAttention() {
		t.Fatalf("book %q mapping %q", books, maps)
	}
	// a target account the chart does not have is the service's refusal, passed on
	f2 := newFake(t)
	cfg := testConfig()
	cfg.Mappings["REVENUE"] = "9999"
	rep, _ = Run(context.Background(), f2.client(), cfg, true, fakeToday)
	if statuses(rep, "mapping")["REVENUE"] != StFailed {
		t.Fatalf("%+v", rep.Steps)
	}
}

func TestTheClientCarriesIdentitiesAndNothingElse(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		reply(w, 200, map[string]any{"packs": []any{}})
	}))
	defer srv.Close()
	c := newClient(srv.URL, "T1", "P1", "E1", time.Second)
	var out map[string]any
	if err := c.do(context.Background(), "GET", "/v1/posting-rule-packs", nil, &out); err != nil {
		t.Fatal(err)
	}
	if got.Get("X-Tenant-Id") != "T1" || got.Get("X-Principal-Id") != "P1" || got.Get("X-Legal-Entity-Id") != "E1" || got.Get("Authorization") != "" || got.Get("Cookie") != "" {
		t.Fatalf("%v", got)
	}
}

func TestConfigurationIsValidatedBeforeAnythingIsCalled(t *testing.T) {
	good := testConfig()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"no entity":         func(c *Config) { c.LegalEntityID = "" },
		"bad book type":     func(c *Config) { c.Book.BookType = "SIDEWAYS" },
		"bad currency":      func(c *Config) { c.Book.FunctionalCurrency = "pounds" },
		"bad date":          func(c *Config) { c.Book.EffectiveFrom = "1 Jan 2026" },
		"no calendar":       func(c *Config) { c.Book.FiscalCalendarID = "" },
		"no mappings":       func(c *Config) { c.Mappings = nil },
		"empty account":     func(c *Config) { c.Mappings["AR_CONTROL"] = " " },
		"no packs":          func(c *Config) { c.Packs = nil },
		"pack no version":   func(c *Config) { c.Packs[0].Version = "" },
		"pack bad date":     func(c *Config) { c.Packs[0].EffectiveFrom = "soon" },
		"pack listed twice": func(c *Config) { c.Packs = append(c.Packs, c.Packs[0]) },
	} {
		c := testConfig()
		mut(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func writeCfg(t *testing.T, cfg any) string {
	t.Helper()
	b, _ := json.Marshal(cfg)
	p := filepath.Join(t.TempDir(), "rollout.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommandLine(t *testing.T) {
	f := newFake(t)
	cfg := writeCfg(t, testConfig())
	args := func(extra ...string) []string {
		return append([]string{"--base", f.srv.URL, "--tenant", "11111111-1111-4111-8111-111111111111", "--principal", "ops", "--config", cfg}, extra...)
	}

	var out, errw bytes.Buffer
	if code := run(args(), &out, &errw); code != exitOK || !strings.Contains(out.String(), "DRY RUN") || strings.Contains(out.String(), "LEDGER_KERNEL_BOOK_ID") {
		t.Fatalf("%d\n%s%s", code, out.String(), errw.String())
	}
	if len(f.writes()) != 0 {
		t.Fatal("a dry run wrote")
	}

	out.Reset()
	if code := run(args("--apply"), &out, &errw); code != exitOK {
		t.Fatalf("%d\n%s%s", code, out.String(), errw.String())
	}
	for _, want := range []string{"APPLIED", "LEDGER_KERNEL_BOOK_ID=book-STAT", "LEDGER_KERNEL_CURRENCY=GBP", "Approve it - by someone other than its author"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := run(args("--json"), &out, &errw); code != exitOK {
		t.Fatalf("%d %s", code, errw.String())
	}
	var rep Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || rep.BookID != "book-STAT" {
		t.Fatalf("%v %s", err, out.String())
	}

	// something that needs a person: exit 1, and no instruction to switch anything over
	f.mappings[0].AccountCode = "4999"
	out.Reset()
	if code := run(args(), &out, &errw); code != exitAttention || strings.Contains(out.String(), "LEDGER_KERNEL_BOOK_ID") || !strings.Contains(out.String(), "needs a person") {
		t.Fatalf("%d\n%s", code, out.String())
	}

	// usage
	for name, a := range map[string][]string{
		"no flags":        {},
		"no config file":  {"--tenant", "t", "--principal", "p", "--config", filepath.Join(t.TempDir(), "missing.json")},
		"unknown option":  {"--bogus"},
		"unknown field":   {"--tenant", "t", "--principal", "p", "--config", writeCfg(t, map[string]any{"legal_entity_id": "x", "typo_field": 1})},
		"invalid content": {"--tenant", "t", "--principal", "p", "--config", writeCfg(t, map[string]any{"legal_entity_id": "x"})},
	} {
		out.Reset()
		errw.Reset()
		if code := run(a, &out, &errw); code != exitUsage {
			t.Errorf("%s: exit %d", name, code)
		}
	}
}

// A mapping made through the API takes effect the day it is made. A pack dated earlier than that is not ready for the days in between, and
// the tool says so rather than reporting a clean readiness for a date nothing could have resolved on.
func TestAPackDatedBeforeTodayWarnsThatNewMappingsTakeEffectToday(t *testing.T) {
	f := newFake(t)
	rep, err := Run(context.Background(), f.client(), testConfig(), true, fakeToday) // the pack is effective from 2026-01-01
	if err != nil {
		t.Fatal(err)
	}
	var warned bool
	for _, s := range rep.Steps {
		warned = warned || (s.Area == "readiness" && s.Status == StWarning && strings.Contains(s.Detail, fakeToday.Format("2006-01-02")) && strings.Contains(s.Detail, "MAPPING_MISSING"))
	}
	if !warned {
		t.Fatalf("%+v", rep.Steps)
	}
	// readiness is judged as of the first day the mappings can resolve, so the report is honest and clean for that day
	if statuses(rep, "readiness")["ar-standard"] != StOK {
		t.Fatalf("%+v", rep.Steps)
	}

	// a pack dated on or after today needs no warning
	f2 := newFake(t)
	cfg := testConfig()
	cfg.Packs[0].EffectiveFrom = futureDay
	rep, _ = Run(context.Background(), f2.client(), cfg, true, fakeToday)
	for _, s := range rep.Steps {
		if s.Status == StWarning && s.Area == "readiness" {
			t.Fatalf("no warning for a pack dated after today: %+v", s)
		}
	}
	if statuses(rep, "readiness")["ar-standard"] != StOK {
		t.Fatalf("%+v", rep.Steps)
	}

	// and a repeat, which adds no mapping, has nothing to warn about
	rep, _ = Run(context.Background(), f.client(), testConfig(), true, fakeToday)
	for _, s := range rep.Steps {
		if s.Status == StWarning && s.Area == "readiness" {
			t.Fatalf("%+v", s)
		}
	}
}

// Many gaps are summarised: the first few, how many more, and the first remedy once.
func TestManyReadinessGapsAreSummarised(t *testing.T) {
	f := newFake(t)
	f.packs["big"] = []string{"K1", "K2", "K3", "K4", "K5", "K6", "K7"}
	cfg := testConfig()
	cfg.Packs = []PackSpec{{Name: "big", Version: "1.0.0", EffectiveFrom: futureDay}}
	cfg.Mappings = map[string]string{"K1": "1100"}
	rep, _ := Run(context.Background(), f.client(), cfg, true, fakeToday)
	var d string
	for _, s := range rep.Steps {
		if s.Area == "readiness" && s.Status == StBlocked {
			d = s.Detail
		}
	}
	if !strings.Contains(d, "and 1 more") || !strings.Contains(d, "For the first: map K2") || strings.Count(d, "UNMAPPED") != 5 {
		t.Fatalf("%q", d)
	}
}
