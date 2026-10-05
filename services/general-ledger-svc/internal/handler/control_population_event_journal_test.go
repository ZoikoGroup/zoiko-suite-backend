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

const ejURL = w5Base + "event-journal-breaks"

func ejQ(extra string) string {
	return ejURL + "?legal_entity_id=" + cpEntity + "&created_before=2026-10-01" + extra
}

func ejCB(cb string) string {
	return ejURL + "?legal_entity_id=" + cpEntity + "&created_before=" + cb
}

func (s *stubStore) QueryEventJournalBreaks(_ context.Context, t string, q domain.EventJournalBreaksQuery) (*domain.ControlPopulationPage, error) {
	s.wave5.breaks = append(s.wave5.breaks, q)
	s.wave5.tenantSeen = append(s.wave5.tenantSeen, t)
	if s.wave5.err != nil {
		return nil, s.wave5.err
	}
	if s.wave5.page != nil {
		return s.wave5.page, nil
	}
	return &domain.ControlPopulationPage{}, nil
}

func TestEventJournalBreaks_BadRequests(t *testing.T) {
	cases := map[string]string{
		"period_id rejected":      ejQ("&period_id=2026-09"),
		"unknown param":           ejQ("&bogus=1"),
		"fiscal_period rejected":  ejQ("&fiscal_period=2026-09"),
		"repeated created_before": ejQ("&created_before=2026-10-02"),
		"missing entity":          ejURL + "?created_before=2026-10-01",
		"bad entity":              ejURL + "?legal_entity_id=x&created_before=2026-10-01",
		"missing created_before":  ejURL + "?legal_entity_id=" + cpEntity,
		"empty created_before":    ejCB(""),
		"garbage created_before":  ejCB("yesterday"),
		"bad month":               ejCB("2026-13-01"),
		"bad day":                 ejCB("2026-02-30"),
		"slash date":              ejCB("2026/10/01"),
		"timestamp without zone":  ejCB("2026-10-01T00:00:00"),
		"bad limit":               ejQ("&limit=5001"),
		"bad cursor":              ejQ("&cursor=***"),
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubStore()
			r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
			resp := doRequest(r, http.MethodGet, url, nil, "svc-1")
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
			}
			if len(s.wave5.breaks) != 0 {
				t.Fatal("store must not be queried for an invalid request")
			}
		})
	}
}

func TestEventJournalBreaks_CreatedBeforeAcceptedForms(t *testing.T) {
	cases := map[string]time.Time{
		"2026-10-01":                  time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		"2026-10-01T12:30:00Z":        time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC),
		"2026-10-01T12:30:00-05:00":   time.Date(2026, 10, 1, 17, 30, 0, 0, time.UTC),
		"2026-10-01T12:30:00%2B02:00": time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC),
	}
	for raw, want := range cases {
		s := newStubStore()
		r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
		resp := doRequest(r, http.MethodGet, ejCB(raw), nil, "svc-1")
		if resp.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", raw, resp.Code, resp.Body.String())
		}
		if got := s.wave5.breaks[0].CreatedBefore; !got.Equal(want) || got.Location() != time.UTC {
			t.Fatalf("%s: store got %v, want %v (UTC)", raw, got, want)
		}
		if q := s.wave5.breaks[0]; q.LegalEntityID != cpEntity || q.Limit != 1000 {
			t.Fatalf("unexpected query %+v", q)
		}
	}
}

func TestEventJournalBreaks_Unauthenticated_Returns401(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	if resp := doRequestAs(r, http.MethodGet, ejQ(""), nil, "svc-1", ""); resp.Code != http.StatusUnauthorized {
		t.Fatalf("missing tenant expected 401, got %d", resp.Code)
	}
	if resp := doRequest(r, http.MethodGet, ejQ(""), nil, ""); resp.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal expected 401, got %d", resp.Code)
	}
}

func TestEventJournalBreaks_UnknownPopulation_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, w5Base+"event-journal-break?legal_entity_id="+cpEntity, nil, "svc-1")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Code)
	}
}

func TestEventJournalBreaks_FailClosedAuthz(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	if resp := doRequest(r, http.MethodGet, ejQ(""), nil, "svc-1"); resp.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.Code)
	}
	s2 := newStubStore()
	r = newRouter(s2, &stubPublisher{}, &stubAuthZ{err: errors.New("authz down")})
	if resp := doRequest(r, http.MethodGet, ejQ(""), nil, "svc-1"); resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.Code)
	}
	if len(s.wave5.breaks)+len(s2.wave5.breaks) != 0 {
		t.Fatal("store must not be called when authorization fails")
	}
}

func TestEventJournalBreaks_TooLarge_Returns422_StoreDown_Returns503(t *testing.T) {
	s := newStubStore()
	s.wave5.err = domain.ErrControlPopulationTooLarge
	if resp := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, ejQ(""), nil, "svc-1"); resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", resp.Code)
	}
	s.wave5.err = errors.New("db down")
	if resp := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, ejQ(""), nil, "svc-1"); resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.Code)
	}
}

func TestEventJournalBreaks_HappyPath(t *testing.T) {
	s := newStubStore()
	last := "EVENT_WITHOUT_JOURNAL:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	s.wave5.page = &domain.ControlPopulationPage{
		Records: []domain.ControlPopulationRecord{{
			RecordID: last, Reference: "EVT-1", Amount: "0", Currency: "XXX", Date: "2026-09-01",
			Attributes: map[string]string{"break_type": "EVENT_WITHOUT_JOURNAL"},
		}},
		NextRecordID:   last,
		Watermark:      "gl8:n=1;entry_seq=0;md5=x",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 1, Totals: map[string]string{"XXX": "0"}},
	}
	az := &cpRecordingAuthZ{}
	r := newRouterWithAuthz(s, az)
	resp := doRequest(r, http.MethodGet, ejQ("&limit=1"), nil, "svc-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var body wave4Body
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Records) != 1 || body.Records[0].Attributes["break_type"] != "EVENT_WITHOUT_JOURNAL" ||
		body.Watermark != "gl8:n=1;entry_seq=0;md5=x" || body.DeclaredTotals.RowCount != 1 {
		t.Fatalf("unexpected body %+v", body)
	}
	if dec, err := base64.RawURLEncoding.DecodeString(body.NextCursor); err != nil || string(dec) != last {
		t.Fatalf("next_cursor should encode the last record_id, got %q", body.NextCursor)
	}
	if az.action != "GL_CONTROL_POPULATION_READ" || az.entity != cpEntity || az.principal != "svc-1" {
		t.Fatalf("unexpected authz call %+v", az)
	}
	if len(s.wave5.tenantSeen) != 1 || s.wave5.tenantSeen[0] != testTenantID {
		t.Fatalf("expected one tenant-scoped store call, got %v", s.wave5.tenantSeen)
	}
	if resp = doRequest(r, http.MethodGet, ejQ("&cursor="+body.NextCursor), nil, "svc-1"); resp.Code != http.StatusOK {
		t.Fatalf("cursor request %d", resp.Code)
	}
	if got := s.wave5.breaks[1].AfterRecordID; got != last {
		t.Fatalf("cursor not decoded to record id: %q", got)
	}

	resp = doRequest(newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}), http.MethodGet, ejQ(""), nil, "svc-1")
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(resp.Body.Bytes(), &raw)
	if string(raw["records"]) != "[]" || string(raw["next_cursor"]) != `""` {
		t.Fatalf("empty population encoding wrong: %s", resp.Body.String())
	}
}
