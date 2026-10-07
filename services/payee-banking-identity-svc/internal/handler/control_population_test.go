package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payee-banking-identity-svc/internal/authz"
	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/handler"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
)

const (
	cpTenant = "11111111-1111-4111-8111-111111111111"
	cpEntity = "le-org10-1"
	cpURL    = "/v1/control-populations/destination-changes"
	cpQuery  = "?legal_entity_id=" + cpEntity + "&changed_from=2026-09-01&changed_to=2026-09-30"
	cpCursor = "22222222-2222-4222-8222-222222222222"
)

type cpStore struct {
	*stubStore
	page  *domain.ControlPopulationPage
	err   error
	calls []domain.DestinationChangesQuery
	when  []string
}

func (s *cpStore) QueryDestinationChanges(_ context.Context, tenantID string, q domain.DestinationChangesQuery) (*domain.ControlPopulationPage, error) {
	s.calls = append(s.calls, q)
	s.when = append(s.when, tenantID)
	if s.err != nil {
		return nil, s.err
	}
	if s.page != nil {
		return s.page, nil
	}
	return &domain.ControlPopulationPage{}, nil
}

// QueryDestinationChanges lets the shared in-memory stub satisfy store.Store.
func (s *stubStore) QueryDestinationChanges(_ context.Context, _ string, _ domain.DestinationChangesQuery) (*domain.ControlPopulationPage, error) {
	return &domain.ControlPopulationPage{}, nil
}

type cpAuthz struct {
	err     error
	actions []string
	entity  string
}

func (a *cpAuthz) CheckAllowed(_ context.Context, _, entity, action string) error {
	a.actions = append(a.actions, action)
	a.entity = entity
	return a.err
}

func (a *cpAuthz) CheckAllowedOwnObject(_ context.Context, _, _, _, _ string) error { return a.err }

func cpRouter(st *cpStore, az *cpAuthz) chi.Router {
	h := handler.New(st, &stubPublisher{}, az, newStubParty(), zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func cpGet(r http.Handler, url, tenant, principal string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestDestinationChanges_BadRequests(t *testing.T) {
	base := cpURL + "?legal_entity_id=" + cpEntity
	cases := map[string]string{
		"period_id rejected":       cpURL + cpQuery + "&period_id=2026-09",
		"unknown param":            cpURL + cpQuery + "&bogus=1",
		"repeated changed_from":    cpURL + cpQuery + "&changed_from=2026-09-02",
		"repeated legal_entity_id": cpURL + cpQuery + "&legal_entity_id=other",
		"missing entity":           cpURL + "?changed_from=2026-09-01&changed_to=2026-09-30",
		"bad entity":               cpURL + "?legal_entity_id=%20x&changed_from=2026-09-01&changed_to=2026-09-30",
		"missing changed_from":     base + "&changed_to=2026-09-30",
		"missing changed_to":       base + "&changed_from=2026-09-01",
		"empty changed_from":       base + "&changed_from=&changed_to=2026-09-30",
		"garbage date":             base + "&changed_from=yesterday&changed_to=2026-09-30",
		"slash date":               base + "&changed_from=2026/09/01&changed_to=2026-09-30",
		"impossible date":          base + "&changed_from=2026-02-30&changed_to=2026-09-30",
		"timestamp not date":       base + "&changed_from=2026-09-01T00:00:00Z&changed_to=2026-09-30",
		"from after to":            base + "&changed_from=2026-09-30&changed_to=2026-09-29",
		"span 401 days":            base + "&changed_from=2025-01-01&changed_to=2026-02-06",
		"limit zero":               cpURL + cpQuery + "&limit=0",
		"limit too big":            cpURL + cpQuery + "&limit=5001",
		"limit not int":            cpURL + cpQuery + "&limit=abc",
		"cursor garbage":           cpURL + cpQuery + "&cursor=***",
		"cursor not uuid":          cpURL + cpQuery + "&cursor=" + base64.RawURLEncoding.EncodeToString([]byte("nope")),
	}
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			st := &cpStore{stubStore: newStubStore()}
			az := &cpAuthz{}
			w := cpGet(cpRouter(st, az), url, cpTenant, "auditor-1")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			if len(st.calls) != 0 {
				t.Fatal("store must not be queried for an invalid request")
			}
		})
	}
}

func TestDestinationChanges_SpanBoundaries(t *testing.T) {
	// 2025-01-01 .. 2026-02-05 is exactly 400 days apart: allowed.
	base := cpURL + "?legal_entity_id=" + cpEntity
	for name, tc := range map[string]struct {
		q    string
		want int
	}{
		"400 days ok":        {"&changed_from=2025-01-01&changed_to=2026-02-05", http.StatusOK},
		"same day ok":        {"&changed_from=2026-09-01&changed_to=2026-09-01", http.StatusOK},
		"401 days rejected":  {"&changed_from=2025-01-01&changed_to=2026-02-06", http.StatusBadRequest},
		"leap span rejected": {"&changed_from=2024-01-01&changed_to=2025-12-31", http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			w := cpGet(cpRouter(&cpStore{stubStore: newStubStore()}, &cpAuthz{}), base+tc.q, cpTenant, "auditor-1")
			if w.Code != tc.want {
				t.Fatalf("expected %d, got %d: %s", tc.want, w.Code, w.Body.String())
			}
		})
	}
}

func TestDestinationChanges_Unauthenticated(t *testing.T) {
	for name, hdr := range map[string][2]string{
		"no principal":   {cpTenant, ""},
		"no tenant":      {"", "auditor-1"},
		"tenant not uid": {"tenant-org10-1", "auditor-1"},
	} {
		t.Run(name, func(t *testing.T) {
			st := &cpStore{stubStore: newStubStore()}
			az := &cpAuthz{}
			w := cpGet(cpRouter(st, az), cpURL+cpQuery, hdr[0], hdr[1])
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", w.Code)
			}
			if len(st.calls) != 0 || len(az.actions) != 0 {
				t.Fatal("neither authz nor store may be reached")
			}
		})
	}
}

func TestDestinationChanges_AuthzFailClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"denied": {authzpkg.ErrAuthorizationDenied, http.StatusForbidden},
		"outage": {authzpkg.ErrAuthzServiceUnavailable, http.StatusServiceUnavailable},
		"other":  {errors.New("boom"), http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			st := &cpStore{stubStore: newStubStore()}
			w := cpGet(cpRouter(st, &cpAuthz{err: tc.err}), cpURL+cpQuery, cpTenant, "auditor-1")
			if w.Code != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, w.Code)
			}
			if len(st.calls) != 0 {
				t.Fatal("store must not be queried without authorization")
			}
		})
	}
}

func TestDestinationChanges_StoreFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"unavailable": {errors.New("db down"), http.StatusServiceUnavailable},
		"too large":   {domain.ErrControlPopulationTooLarge, http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			st := &cpStore{stubStore: newStubStore(), err: tc.err}
			w := cpGet(cpRouter(st, &cpAuthz{}), cpURL+cpQuery, cpTenant, "auditor-1")
			if w.Code != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, w.Code)
			}
		})
	}
}

func TestDestinationChanges_HappyPath(t *testing.T) {
	const id = "33333333-3333-4333-8333-333333333333"
	st := &cpStore{stubStore: newStubStore(), page: &domain.ControlPopulationPage{
		Records: []domain.ControlPopulationRecord{{
			RecordID: id, Reference: id, Amount: "0", Currency: "XXX", Date: "2026-09-05",
			Attributes: map[string]string{"party_ref": "party-1", "status": "ACTIVE", "proposed_by": "p", "verified_by": "v", "approved_by": "a", "sod_gap": ""},
		}},
		NextRecordID: id,
		Watermark:    "pb1:n=5;md5=abc",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 5,
			Totals: map[string]string{"XXX": "0"}},
	}}
	az := &cpAuthz{}
	w := cpGet(cpRouter(st, az), cpURL+cpQuery+"&limit=1&cursor="+base64.RawURLEncoding.EncodeToString([]byte(strings.ToUpper(cpCursor))), cpTenant, "auditor-1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(az.actions) != 1 || az.actions[0] != "PAYEE_BANKING_CONTROL_POPULATION_READ" || az.entity != cpEntity {
		t.Fatalf("unexpected authz check: %v %q", az.actions, az.entity)
	}
	if len(st.calls) != 1 || st.when[0] != cpTenant {
		t.Fatalf("store must receive the verified tenant: %v", st.when)
	}
	q := st.calls[0]
	if q.LegalEntityID != cpEntity || q.ChangedFrom != "2026-09-01" || q.ChangedTo != "2026-09-30" || q.Limit != 1 || q.AfterRecordID != cpCursor {
		t.Fatalf("unexpected query: %+v", q)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"records", "next_cursor", "watermark", "declared_totals"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("missing %s in response", k)
		}
	}
	var resp struct {
		NextCursor     string `json:"next_cursor"`
		Watermark      string `json:"watermark"`
		DeclaredTotals struct {
			RowCount int64             `json:"row_count"`
			Totals   map[string]string `json:"totals"`
		} `json:"declared_totals"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	var raw struct {
		Records []map[string]any `json:"records"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	rec := raw.Records[0]
	if rec["amount"] != "0" || rec["currency"] != "XXX" || rec["record_id"] != id || rec["reference"] != id || rec["date"] != "2026-09-05" {
		t.Fatalf("unexpected record %v", rec)
	}
	if _, isString := rec["amount"].(string); !isString {
		t.Fatal("amount must be a JSON string")
	}
	if resp.Watermark != "pb1:n=5;md5=abc" || resp.DeclaredTotals.RowCount != 5 || resp.DeclaredTotals.Totals["XXX"] != "0" {
		t.Fatalf("unexpected envelope: %+v", resp)
	}
	dec, err := base64.RawURLEncoding.DecodeString(resp.NextCursor)
	if err != nil || string(dec) != id {
		t.Fatalf("next_cursor must encode the last record id, got %q", resp.NextCursor)
	}
}

func TestDestinationChanges_EmptyPageShape(t *testing.T) {
	st := &cpStore{stubStore: newStubStore(), page: &domain.ControlPopulationPage{Watermark: "pb1:n=0;md5=x"}}
	w := cpGet(cpRouter(st, &cpAuthz{}), cpURL+cpQuery, cpTenant, "auditor-1")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"records":[]`) || !strings.Contains(w.Body.String(), `"next_cursor":""`) {
		t.Fatalf("empty page must render [] and empty cursor: %s", w.Body.String())
	}
	if st.calls[0].Limit != 1000 {
		t.Fatalf("default limit must be 1000, got %d", st.calls[0].Limit)
	}
}
