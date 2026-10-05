package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/domain"
)

func (s *stubStore) ControlPopulation(_ context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	s.popQuery = q
	if s.popErr != nil {
		return nil, s.popErr
	}
	return s.popPage, nil
}

const (
	popEntity    = "11111111-1111-4111-8111-111111111111"
	popLastID    = "22222222-2222-4222-8222-222222222222"
	popTenantHdr = "33333333-3333-4333-8333-333333333333"
)

func popRequest(t *testing.T, path string, tenant, principal string, s *stubStore, a *stubAuthZ) *httptest.ResponseRecorder {
	t.Helper()
	r := newRouter(s, &stubPublisher{}, a, "http://unused.invalid")
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestControlPopulation_ErrorMatrix(t *testing.T) {
	base := "/v1/control-populations/open-invoices?legal_entity_id=" + popEntity
	cases := []struct {
		name      string
		path      string
		tenant    string
		principal string
		authz     error
		want      int
	}{
		{"missing tenant", base, "", "p1", nil, http.StatusUnauthorized},
		{"missing principal", base, popTenantHdr, "", nil, http.StatusUnauthorized},
		{"missing legal_entity_id", "/v1/control-populations/open-invoices", popTenantHdr, "p1", nil, http.StatusBadRequest},
		{"invalid legal_entity_id", "/v1/control-populations/open-invoices?legal_entity_id=nope", popTenantHdr, "p1", nil, http.StatusBadRequest},
		{"unknown query param", base + "&status=PAID", popTenantHdr, "p1", nil, http.StatusBadRequest},
		{"bad period_id", base + "&period_id=2026-13", popTenantHdr, "p1", nil, http.StatusBadRequest},
		{"bad period_id shape", base + "&period_id=2026-9-1", popTenantHdr, "p1", nil, http.StatusBadRequest},
		{"bad limit", base + "&limit=0", popTenantHdr, "p1", nil, http.StatusBadRequest},
		{"bad cursor", base + "&cursor=!!!", popTenantHdr, "p1", nil, http.StatusBadRequest},
		{"unknown population", "/v1/control-populations/closed-invoices?legal_entity_id=" + popEntity, popTenantHdr, "p1", nil, http.StatusNotFound},
		{"authz denied", base, popTenantHdr, "p1", domain.ErrAuthorizationDenied, http.StatusForbidden},
		{"authz unavailable fails closed", base, popTenantHdr, "p1", domain.ErrAuthzServiceUnavailable, http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStubStore()
			s.popPage = &domain.ControlPopulationPage{}
			rec := popRequest(t, c.path, c.tenant, c.principal, s, &stubAuthZ{err: c.authz})
			assert.Equal(t, c.want, rec.Code, rec.Body.String())
			if c.want != http.StatusOK {
				assert.Equal(t, domain.ControlPopulationQuery{}, s.popQuery, "the store must not be reached")
			}
		})
	}
}

func TestControlPopulation_StoreErrors(t *testing.T) {
	base := "/v1/control-populations/open-invoices?legal_entity_id=" + popEntity
	s := newStubStore()
	s.popErr = domain.ErrPopulationTooLarge
	assert.Equal(t, http.StatusUnprocessableEntity, popRequest(t, base, popTenantHdr, "p1", s, &stubAuthZ{}).Code)

	s.popErr = domain.ErrStoreUnavailable
	assert.Equal(t, http.StatusServiceUnavailable, popRequest(t, base, popTenantHdr, "p1", s, &stubAuthZ{}).Code)
}

func TestControlPopulation_HappyPathShapeAndQuery(t *testing.T) {
	s := newStubStore()
	s.popPage = &domain.ControlPopulationPage{
		Records: []domain.ControlRecord{{
			RecordID: popLastID, Reference: "INV-1", Amount: "1250.50", Currency: "USD", Date: "2026-09-01",
			Attributes: map[string]string{"status": "SENT", "customer_id": "c1", "due_date": "2026-10-01", "invoice_amount": "1250.50"},
		}},
		NextCursor:     popLastID,
		Watermark:      "abc:1",
		DeclaredTotals: domain.DeclaredTotals{RowCount: 3, Totals: map[string]string{"USD": "1250.50"}},
	}
	path := "/v1/control-populations/open-invoices?" + url.Values{
		"legal_entity_id": {popEntity}, "period_id": {"2026-02"}, "limit": {"2"},
		"cursor": {base64.RawURLEncoding.EncodeToString([]byte(popLastID))},
	}.Encode()
	rec := popRequest(t, path, popTenantHdr, "p1", s, &stubAuthZ{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	for _, k := range []string{"records", "next_cursor", "watermark", "declared_totals"} {
		assert.Contains(t, raw, k)
	}
	var out struct {
		Records []struct {
			RecordID   string            `json:"record_id"`
			Amount     any               `json:"amount"`
			Attributes map[string]string `json:"attributes"`
		} `json:"records"`
		NextCursor     string `json:"next_cursor"`
		Watermark      string `json:"watermark"`
		DeclaredTotals struct {
			RowCount int               `json:"row_count"`
			Totals   map[string]string `json:"totals"`
		} `json:"declared_totals"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "1250.50", out.Records[0].Amount, "amount must be a JSON string")
	assert.Equal(t, "abc:1", out.Watermark)
	assert.Equal(t, 3, out.DeclaredTotals.RowCount)
	assert.Equal(t, "1250.50", out.DeclaredTotals.Totals["USD"])
	// The cursor is opaque: not the raw record id, and decodes back to it.
	assert.NotEqual(t, popLastID, out.NextCursor)
	dec, err := base64.RawURLEncoding.DecodeString(out.NextCursor)
	require.NoError(t, err)
	assert.Equal(t, popLastID, string(dec))

	// The handler passed the verified tenant, the entity, the month-end cut-off
	// and the decoded keyset position down.
	assert.Equal(t, popTenantHdr, s.popQuery.TenantID)
	assert.Equal(t, popEntity, s.popQuery.LegalEntityID)
	assert.Equal(t, 2, s.popQuery.Limit)
	assert.Equal(t, popLastID, s.popQuery.AfterRecordID)
	require.NotNil(t, s.popQuery.CutOff)
	assert.Equal(t, "2026-02-28", s.popQuery.CutOff.Format("2006-01-02"))
}

func TestControlPopulation_EmptyPopulationIsEmptyListNotNull(t *testing.T) {
	s := newStubStore()
	s.popPage = &domain.ControlPopulationPage{Watermark: "d41d8cd98f00b204e9800998ecf8427e:0"}
	rec := popRequest(t, "/v1/control-populations/open-invoices?legal_entity_id="+popEntity, popTenantHdr, "p1", s, &stubAuthZ{})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"records":[]`)
	assert.Contains(t, rec.Body.String(), `"totals":{}`)
	assert.Contains(t, rec.Body.String(), `"next_cursor":""`)
}
