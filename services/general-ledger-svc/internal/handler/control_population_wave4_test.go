package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// wave4Stub is the recording fake behind QueryJournalAccountTotals /
// QueryTrialBalancePopulation.
type wave4Stub struct {
	page       *domain.ControlPopulationPage
	err        error
	jatCalls   []domain.JournalAccountTotalsQuery
	tbCalls    []domain.TrialBalancePopulationQuery
	tenantSeen []string
}

func (s *stubStore) QueryJournalAccountTotals(_ context.Context, tenantID string, q domain.JournalAccountTotalsQuery) (*domain.ControlPopulationPage, error) {
	s.wave4.jatCalls = append(s.wave4.jatCalls, q)
	s.wave4.tenantSeen = append(s.wave4.tenantSeen, tenantID)
	if s.wave4.err != nil {
		return nil, s.wave4.err
	}
	if s.wave4.page != nil {
		return s.wave4.page, nil
	}
	return &domain.ControlPopulationPage{}, nil
}

func (s *stubStore) QueryTrialBalancePopulation(_ context.Context, tenantID string, q domain.TrialBalancePopulationQuery) (*domain.ControlPopulationPage, error) {
	s.wave4.tbCalls = append(s.wave4.tbCalls, q)
	s.wave4.tenantSeen = append(s.wave4.tenantSeen, tenantID)
	if s.wave4.err != nil {
		return nil, s.wave4.err
	}
	if s.wave4.page != nil {
		return s.wave4.page, nil
	}
	return &domain.ControlPopulationPage{}, nil
}

const (
	jatBase = "/v1/control-populations/journal-account-totals"
	tbBase  = "/v1/control-populations/trial-balance"
)

func jatURL(extra string) string {
	return jatBase + "?legal_entity_id=" + cpEntity + "&account_codes=1200,1210&normal_balance=CREDIT" + extra
}

func tbURL(extra string) string {
	return tbBase + "?legal_entity_id=" + cpEntity + "&fiscal_period=2026-09" + extra
}

func TestWave4_BadRequests(t *testing.T) {
	cases := map[string]string{
		// journal-account-totals
		"jat period_id rejected":        jatURL("&period_id=2026-09"),
		"jat unknown param":             jatURL("&bogus=1"),
		"jat repeated param":            jatURL("&limit=1&limit=2"),
		"jat missing entity":            jatBase + "?account_codes=1200&normal_balance=DEBIT",
		"jat bad entity":                jatBase + "?legal_entity_id=x&account_codes=1200&normal_balance=DEBIT",
		"jat missing account_codes":     jatBase + "?legal_entity_id=" + cpEntity + "&normal_balance=DEBIT",
		"jat empty account_codes":       jatBase + "?legal_entity_id=" + cpEntity + "&account_codes=&normal_balance=DEBIT",
		"jat empty code in list":        jatBase + "?legal_entity_id=" + cpEntity + "&account_codes=1200,,1210&normal_balance=DEBIT",
		"jat code too long":             jatBase + "?legal_entity_id=" + cpEntity + "&account_codes=" + repeat("x", 65) + "&normal_balance=DEBIT",
		"jat too many codes":            jatBase + "?legal_entity_id=" + cpEntity + "&account_codes=" + codes(21) + "&normal_balance=DEBIT",
		"jat missing normal_balance":    jatBase + "?legal_entity_id=" + cpEntity + "&account_codes=1200",
		"jat bad normal_balance":        jatBase + "?legal_entity_id=" + cpEntity + "&account_codes=1200&normal_balance=debit",
		"jat fiscal_period not allowed": jatURL("&fiscal_period=2026-09"),
		"jat bad limit":                 jatURL("&limit=5001"),
		"jat bad cursor":                jatURL("&cursor=***"),
		"jat empty-decoding cursor":     jatURL("&cursor=" + base64.RawURLEncoding.EncodeToString([]byte("a\x00b"))),
		// trial-balance
		"tb period_id rejected":        tbURL("&period_id=2026-09"),
		"tb unknown param":             tbURL("&bogus=1"),
		"tb repeated fiscal_period":    tbURL("&fiscal_period=2026-10"),
		"tb missing entity":            tbBase + "?fiscal_period=2026-09",
		"tb bad entity":                tbBase + "?legal_entity_id=x&fiscal_period=2026-09",
		"tb missing fiscal_period":     tbBase + "?legal_entity_id=" + cpEntity,
		"tb empty fiscal_period":       tbBase + "?legal_entity_id=" + cpEntity + "&fiscal_period=",
		"tb fiscal_period too long":    tbBase + "?legal_entity_id=" + cpEntity + "&fiscal_period=" + repeat("9", 21),
		"tb fiscal_period control chr": tbBase + "?legal_entity_id=" + cpEntity + "&fiscal_period=2026%0A09",
		"tb account_codes not allowed": tbURL("&account_codes=1200"),
		"tb bad limit":                 tbURL("&limit=0"),
		"tb bad cursor":                tbURL("&cursor=***"),
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubStore()
			r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
			resp := doRequest(r, http.MethodGet, url, nil, "svc-1")
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
			}
			if len(s.wave4.jatCalls)+len(s.wave4.tbCalls) != 0 {
				t.Fatal("store must not be queried for an invalid request")
			}
		})
	}
}

func TestWave4_FiscalPeriodIsNotAssumedToBeYYYYMM(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, tbBase+"?legal_entity_id="+cpEntity+"&fiscal_period=FY26-P09", nil, "svc-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(s.wave4.tbCalls) != 1 || s.wave4.tbCalls[0].FiscalPeriod != "FY26-P09" {
		t.Fatalf("fiscal_period must reach the store verbatim: %+v", s.wave4.tbCalls)
	}
}

func TestWave4_Unauthenticated_Returns401(t *testing.T) {
	for _, u := range []string{jatURL(""), tbURL("")} {
		r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
		if resp := doRequestAs(r, http.MethodGet, u, nil, "svc-1", ""); resp.Code != http.StatusUnauthorized {
			t.Fatalf("%s: missing tenant expected 401, got %d", u, resp.Code)
		}
		if resp := doRequest(r, http.MethodGet, u, nil, ""); resp.Code != http.StatusUnauthorized {
			t.Fatalf("%s: missing principal expected 401, got %d", u, resp.Code)
		}
	}
}

func TestWave4_UnknownPopulation_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	resp := doRequest(r, http.MethodGet, "/v1/control-populations/trial-balances?legal_entity_id="+cpEntity, nil, "svc-1")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Code)
	}
}

func TestWave4_FailClosedAuthz(t *testing.T) {
	for _, u := range []string{jatURL(""), tbURL("")} {
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
		if len(s.wave4.jatCalls)+len(s.wave4.tbCalls)+len(s2.wave4.jatCalls)+len(s2.wave4.tbCalls) != 0 {
			t.Fatal("store must not be called when authorization fails")
		}
	}
}

func TestWave4_TooLarge_Returns422_StoreDown_Returns503(t *testing.T) {
	for _, u := range []string{jatURL(""), tbURL("")} {
		s := newStubStore()
		s.wave4.err = domain.ErrControlPopulationTooLarge
		if resp := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, u, nil, "svc-1"); resp.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: expected 422, got %d", u, resp.Code)
		}
		s.wave4.err = errors.New("db down")
		if resp := doRequest(newRouter(s, &stubPublisher{}, &stubAuthZ{}), http.MethodGet, u, nil, "svc-1"); resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: expected 503, got %d", u, resp.Code)
		}
	}
}

type wave4Body struct {
	Records []struct {
		RecordID   string            `json:"record_id"`
		Reference  string            `json:"reference"`
		Amount     string            `json:"amount"`
		Currency   string            `json:"currency"`
		Date       string            `json:"date"`
		Attributes map[string]string `json:"attributes"`
	} `json:"records"`
	NextCursor     string `json:"next_cursor"`
	Watermark      string `json:"watermark"`
	DeclaredTotals struct {
		RowCount int64             `json:"row_count"`
		Totals   map[string]string `json:"totals"`
	} `json:"declared_totals"`
}

func TestWave4_JournalAccountTotals_HappyPath(t *testing.T) {
	s := newStubStore()
	jid := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	last := jid + ":USD"
	s.wave4.page = &domain.ControlPopulationPage{
		Records: []domain.ControlPopulationRecord{{
			RecordID: last, Reference: jid, Amount: "70.25", Currency: "USD", Date: "2026-09-01",
			Attributes: map[string]string{"journal_id": jid, "fiscal_period": "2026-09", "account_codes": "1200,1210", "entry_count": "2"},
		}},
		NextRecordID:   last,
		Watermark:      "gl3:n=2;entry_seq=9;md5=x",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 2, Totals: map[string]string{"USD": "80.25"}},
	}
	az := &cpRecordingAuthZ{}
	r := newRouterWithAuthz(s, az)
	resp := doRequest(r, http.MethodGet, jatURL("&limit=1"), nil, "svc-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var body wave4Body
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Records) != 1 || body.Records[0].RecordID != last || body.Records[0].Reference != jid ||
		body.Records[0].Amount != "70.25" || body.Records[0].Attributes["entry_count"] != "2" {
		t.Fatalf("unexpected records: %+v", body.Records)
	}
	if body.Watermark != "gl3:n=2;entry_seq=9;md5=x" || body.DeclaredTotals.RowCount != 2 || body.DeclaredTotals.Totals["USD"] != "80.25" {
		t.Fatalf("unexpected watermark/totals: %+v", body)
	}
	if dec, err := base64.RawURLEncoding.DecodeString(body.NextCursor); err != nil || string(dec) != last {
		t.Fatalf("next_cursor should encode the last record_id, got %q", body.NextCursor)
	}
	if az.action != "GL_CONTROL_POPULATION_READ" || az.entity != cpEntity || az.principal != "svc-1" {
		t.Fatalf("unexpected authz call: %+v", az)
	}
	if len(s.wave4.jatCalls) != 1 || s.wave4.tenantSeen[0] != testTenantID {
		t.Fatalf("expected one tenant-scoped store call, got %+v", s.wave4)
	}
	c := s.wave4.jatCalls[0]
	if c.LegalEntityID != cpEntity || c.NormalBalance != "CREDIT" || c.Limit != 1 ||
		len(c.AccountCodes) != 2 || c.AccountCodes[0] != "1200" {
		t.Fatalf("unexpected store query: %+v", c)
	}

	resp = doRequest(r, http.MethodGet, jatURL("&cursor="+body.NextCursor), nil, "svc-1")
	if resp.Code != http.StatusOK || s.wave4.jatCalls[1].AfterRecordID != last {
		t.Fatalf("cursor not decoded to record id: %d %+v", resp.Code, s.wave4.jatCalls)
	}
}

func TestWave4_TrialBalance_HappyPath_AndEmptyArrays(t *testing.T) {
	s := newStubStore()
	last := "1200:USD"
	s.wave4.page = &domain.ControlPopulationPage{
		Records: []domain.ControlPopulationRecord{{
			RecordID: last, Reference: "1200", Amount: "-12.5000", Currency: "USD", Date: "2026-09-30",
			Attributes: map[string]string{"account_code": "1200", "fiscal_period": "2026-09", "entry_count": "3", "max_entry_seq": "7"},
		}},
		NextRecordID:   last,
		Watermark:      "gl2:n=1;entry_seq=7;md5=y",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 1, Totals: map[string]string{"USD": "-12.5000"}},
	}
	az := &cpRecordingAuthZ{}
	r := newRouterWithAuthz(s, az)
	resp := doRequest(r, http.MethodGet, tbURL(""), nil, "svc-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var body wave4Body
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Records) != 1 || body.Records[0].Reference != "1200" || body.Records[0].Amount != "-12.5000" ||
		body.Records[0].Attributes["max_entry_seq"] != "7" || body.Watermark != "gl2:n=1;entry_seq=7;md5=y" {
		t.Fatalf("unexpected body: %+v", body)
	}
	if az.action != "GL_CONTROL_POPULATION_READ" {
		t.Fatalf("unexpected authz action %q", az.action)
	}
	if c := s.wave4.tbCalls[0]; c.FiscalPeriod != "2026-09" || c.LegalEntityID != cpEntity || c.Limit != 1000 {
		t.Fatalf("unexpected store query: %+v", c)
	}

	// Empty population encodes [] and "" not null.
	r2 := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	resp = doRequest(r2, http.MethodGet, tbURL(""), nil, "svc-1")
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(resp.Body.Bytes(), &raw)
	if string(raw["records"]) != "[]" || string(raw["next_cursor"]) != `""` {
		t.Fatalf("empty population encoding wrong: %s", resp.Body.String())
	}
}
