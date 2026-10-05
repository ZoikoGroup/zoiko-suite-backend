package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// wave5Stub is the recording fake behind the four wave-5 store queries.
type wave5Stub struct {
	page       *domain.ControlPopulationPage
	err        error
	fpCalls    map[string][]domain.FiscalPeriodPopulationQuery
	unposted   []domain.UnpostedEventsQuery
	breaks     []domain.EventJournalBreaksQuery
	tenantSeen []string
}

func (s *stubStore) recordFP(name, tenantID string, q domain.FiscalPeriodPopulationQuery) (*domain.ControlPopulationPage, error) {
	if s.wave5.fpCalls == nil {
		s.wave5.fpCalls = map[string][]domain.FiscalPeriodPopulationQuery{}
	}
	s.wave5.fpCalls[name] = append(s.wave5.fpCalls[name], q)
	s.wave5.tenantSeen = append(s.wave5.tenantSeen, tenantID)
	if s.wave5.err != nil {
		return nil, s.wave5.err
	}
	if s.wave5.page != nil {
		return s.wave5.page, nil
	}
	return &domain.ControlPopulationPage{}, nil
}

func (s *stubStore) QueryJournalBalances(_ context.Context, t string, q domain.FiscalPeriodPopulationQuery) (*domain.ControlPopulationPage, error) {
	return s.recordFP("journal-balances", t, q)
}

func (s *stubStore) QueryControlAccountPostings(_ context.Context, t string, q domain.FiscalPeriodPopulationQuery) (*domain.ControlPopulationPage, error) {
	return s.recordFP("control-account-postings", t, q)
}

func (s *stubStore) QueryManualJournals(_ context.Context, t string, q domain.FiscalPeriodPopulationQuery) (*domain.ControlPopulationPage, error) {
	return s.recordFP("manual-journals", t, q)
}

func (s *stubStore) QueryUnpostedEvents(_ context.Context, t string, q domain.UnpostedEventsQuery) (*domain.ControlPopulationPage, error) {
	s.wave5.unposted = append(s.wave5.unposted, q)
	s.wave5.tenantSeen = append(s.wave5.tenantSeen, t)
	if s.wave5.err != nil {
		return nil, s.wave5.err
	}
	if s.wave5.page != nil {
		return s.wave5.page, nil
	}
	return &domain.ControlPopulationPage{}, nil
}

func (s *stubStore) wave5Calls() int {
	n := len(s.wave5.unposted)
	for _, c := range s.wave5.fpCalls {
		n += len(c)
	}
	return n
}

var w5FiscalPops = []string{"journal-balances", "control-account-postings", "manual-journals"}

const (
	w5Base  = "/v1/control-populations/"
	w5UnURL = w5Base + "unposted-events"
)

func w5FP(pop, extra string) string {
	return w5Base + pop + "?legal_entity_id=" + cpEntity + "&fiscal_period=2026-09" + extra
}

func w5Un(extra string) string {
	return w5UnURL + "?legal_entity_id=" + cpEntity + "&created_before=2026-10-01" + extra
}

func w5UnCB(cb string) string {
	return w5UnURL + "?legal_entity_id=" + cpEntity + "&created_before=" + cb
}

func allW5URLs() []string {
	var us []string
	for _, p := range w5FiscalPops {
		us = append(us, w5FP(p, ""))
	}
	return append(us, w5Un(""))
}

func TestWave5_BadRequests(t *testing.T) {
	cases := map[string]string{
		"un period_id rejected":        w5Un("&period_id=2026-09"),
		"un unknown param":             w5Un("&bogus=1"),
		"un repeated created_before":   w5Un("&created_before=2026-10-02"),
		"un missing entity":            w5UnURL + "?created_before=2026-10-01",
		"un bad entity":                w5UnURL + "?legal_entity_id=x&created_before=2026-10-01",
		"un missing created_before":    w5UnURL + "?legal_entity_id=" + cpEntity,
		"un empty created_before":      w5UnCB(""),
		"un garbage created_before":    w5UnCB("yesterday"),
		"un bad month":                 w5UnCB("2026-13-01"),
		"un bad day":                   w5UnCB("2026-02-30"),
		"un slash date":                w5UnCB("2026/10/01"),
		"un timestamp without zone":    w5UnCB("2026-10-01T00:00:00"),
		"un fiscal_period not allowed": w5Un("&fiscal_period=2026-09"),
		"un bad limit":                 w5Un("&limit=5001"),
		"un bad cursor":                w5Un("&cursor=***"),
	}
	for _, p := range w5FiscalPops {
		base := w5Base + p
		cases[p+" period_id rejected"] = w5FP(p, "&period_id=2026-09")
		cases[p+" unknown param"] = w5FP(p, "&bogus=1")
		cases[p+" repeated fiscal_period"] = w5FP(p, "&fiscal_period=2026-10")
		cases[p+" missing entity"] = base + "?fiscal_period=2026-09"
		cases[p+" bad entity"] = base + "?legal_entity_id=x&fiscal_period=2026-09"
		cases[p+" missing fiscal_period"] = base + "?legal_entity_id=" + cpEntity
		cases[p+" empty fiscal_period"] = base + "?legal_entity_id=" + cpEntity + "&fiscal_period="
		cases[p+" fiscal_period too long"] = base + "?legal_entity_id=" + cpEntity + "&fiscal_period=" + repeat("9", 21)
		cases[p+" fiscal_period control chr"] = base + "?legal_entity_id=" + cpEntity + "&fiscal_period=2026%0A09"
		cases[p+" created_before not allowed"] = w5FP(p, "&created_before=2026-10-01")
		cases[p+" account_codes not allowed"] = w5FP(p, "&account_codes=1200")
		cases[p+" bad limit"] = w5FP(p, "&limit=0")
		cases[p+" bad cursor"] = w5FP(p, "&cursor=***")
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubStore()
			r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
			resp := doRequest(r, http.MethodGet, url, nil, "svc-1")
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
			}
			if s.wave5Calls() != 0 {
				t.Fatal("store must not be queried for an invalid request")
			}
		})
	}
}

func TestWave5_CreatedBeforeAcceptedForms(t *testing.T) {
	cases := map[string]time.Time{
		"2026-10-01":                  time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		"2026-10-01T12:30:00Z":        time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC),
		"2026-10-01T12:30:00.5Z":      time.Date(2026, 10, 1, 12, 30, 0, 500000000, time.UTC),
		"2026-10-01T12:30:00-05:00":   time.Date(2026, 10, 1, 17, 30, 0, 0, time.UTC),
		"2026-10-01T12:30:00%2B02:00": time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC),
	}
	for raw, want := range cases {
		s := newStubStore()
		r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
		resp := doRequest(r, http.MethodGet, w5UnCB(raw), nil, "svc-1")
		if resp.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", raw, resp.Code, resp.Body.String())
		}
		if got := s.wave5.unposted[0].CreatedBefore; !got.Equal(want) || got.Location() != time.UTC {
			t.Fatalf("%s: store got %v, want %v (UTC)", raw, got, want)
		}
	}
}

func TestWave5_FiscalPeriodIsVerbatim(t *testing.T) {
	for _, p := range w5FiscalPops {
		s := newStubStore()
		r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
		resp := doRequest(r, http.MethodGet, w5Base+p+"?legal_entity_id="+cpEntity+"&fiscal_period=FY26-P09", nil, "svc-1")
		if resp.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", p, resp.Code, resp.Body.String())
		}
		if c := s.wave5.fpCalls[p]; len(c) != 1 || c[0].FiscalPeriod != "FY26-P09" || c[0].LegalEntityID != cpEntity || c[0].Limit != 1000 {
			t.Fatalf("%s: unexpected store query %+v", p, s.wave5.fpCalls)
		}
	}
}

func TestWave5_Unauthenticated_Returns401(t *testing.T) {
	for _, u := range allW5URLs() {
		r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
		if resp := doRequestAs(r, http.MethodGet, u, nil, "svc-1", ""); resp.Code != http.StatusUnauthorized {
			t.Fatalf("%s: missing tenant expected 401, got %d", u, resp.Code)
		}
		if resp := doRequest(r, http.MethodGet, u, nil, ""); resp.Code != http.StatusUnauthorized {
			t.Fatalf("%s: missing principal expected 401, got %d", u, resp.Code)
		}
	}
}

func TestWave5_UnknownPopulation_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	for _, p := range []string{"journal-balance", "manual-journal", "unposted-event", "control-account-posting"} {
		resp := doRequest(r, http.MethodGet, w5Base+p+"?legal_entity_id="+cpEntity, nil, "svc-1")
		if resp.Code != http.StatusNotFound {
			t.Fatalf("%s: expected 404, got %d", p, resp.Code)
		}
	}
}

func TestWave5_FailClosedAuthz(t *testing.T) {
	for _, u := range allW5URLs() {
		s := newStubStore()
		r := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
		if resp := doRequest(r, http.MethodGet, u, nil, "svc-1"); resp.Code != http.StatusForbidden {
			t.Fatalf("%s: expected 403, got %d", u, resp.Code)
		}
		s2 := newStubStore()
		r = newRouter(s2, &stubPublisher{}, &stubAuthZ{err: errors.New("authz down")})
		if resp := doRequest(r, http.MethodGet, u, nil, "svc-1"); resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: expected 503, got %d", u, resp.Code)
		}
		if s.wave5Calls()+s2.wave5Calls() != 0 {
			t.Fatal("store must not be called when authorization fails")
		}
	}
}

func TestWave5_TooLarge_Returns422_StoreDown_Returns503(t *testing.T) {
	for _, u := range allW5URLs() {
		s := newStubStore()
		s.wave5.err = domain.ErrControlPopulationTooLarge
		if resp := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, u, nil, "svc-1"); resp.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: expected 422, got %d", u, resp.Code)
		}
		s.wave5.err = errors.New("db down")
		if resp := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, u, nil, "svc-1"); resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: expected 503, got %d", u, resp.Code)
		}
	}
}

func TestWave5_HappyPath_ShapeAuthzCursorAndEmptyArrays(t *testing.T) {
	for _, u := range allW5URLs() {
		s := newStubStore()
		last := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa:USD"
		s.wave5.page = &domain.ControlPopulationPage{
			Records: []domain.ControlPopulationRecord{{
				RecordID: last, Reference: "ref", Amount: "70.25", Currency: "USD", Date: "2026-09-01",
				Attributes: map[string]string{"k": "v"},
			}},
			NextRecordID:   last,
			Watermark:      "gl4:n=1;entry_seq=9;md5=x",
			DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 1, Totals: map[string]string{"USD": "70.25"}},
		}
		az := &cpRecordingAuthZ{}
		r := newRouterWithAuthz(s, az)
		resp := doRequest(r, http.MethodGet, u+"&limit=1", nil, "svc-1")
		if resp.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", u, resp.Code, resp.Body.String())
		}
		var body wave4Body
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Records) != 1 || body.Records[0].Amount != "70.25" || body.Watermark != "gl4:n=1;entry_seq=9;md5=x" ||
			body.DeclaredTotals.RowCount != 1 || body.DeclaredTotals.Totals["USD"] != "70.25" {
			t.Fatalf("%s: unexpected body %+v", u, body)
		}
		if dec, err := base64.RawURLEncoding.DecodeString(body.NextCursor); err != nil || string(dec) != last {
			t.Fatalf("%s: next_cursor should encode the last record_id, got %q", u, body.NextCursor)
		}
		if az.action != "GL_CONTROL_POPULATION_READ" || az.entity != cpEntity || az.principal != "svc-1" {
			t.Fatalf("%s: unexpected authz call %+v", u, az)
		}
		if len(s.wave5.tenantSeen) != 1 || s.wave5.tenantSeen[0] != testTenantID {
			t.Fatalf("%s: expected one tenant-scoped store call, got %v", u, s.wave5.tenantSeen)
		}

		resp = doRequest(r, http.MethodGet, u+"&cursor="+body.NextCursor, nil, "svc-1")
		if resp.Code != http.StatusOK {
			t.Fatalf("%s: cursor request %d", u, resp.Code)
		}
		gotAfter := ""
		for _, c := range s.wave5.fpCalls {
			for _, q := range c {
				if q.AfterRecordID != "" {
					gotAfter = q.AfterRecordID
				}
			}
		}
		for _, q := range s.wave5.unposted {
			if q.AfterRecordID != "" {
				gotAfter = q.AfterRecordID
			}
		}
		if gotAfter != last {
			t.Fatalf("%s: cursor not decoded to record id: %q", u, gotAfter)
		}

		r2 := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
		resp = doRequest(r2, http.MethodGet, u, nil, "svc-1")
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(resp.Body.Bytes(), &raw)
		if string(raw["records"]) != "[]" || string(raw["next_cursor"]) != `""` {
			t.Fatalf("%s: empty population encoding wrong: %s", u, resp.Body.String())
		}
	}
}
