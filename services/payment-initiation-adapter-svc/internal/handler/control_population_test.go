package handler_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/provideradapter"
)

const (
	cpTenant  = "6f1c1c0e-0000-4000-8000-000000000001"
	cpEntity  = "6f1c1c0e-0000-4000-8000-000000000002"
	cpPath    = "/v1/control-populations/payment-attempts"
	cpAttempt = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

func cpGet(st *stubStore, az *stubAuthz, path, tenant, principal string) *httptest.ResponseRecorder {
	r := newTestRouter(st, az, provideradapter.NewStubProviderAdapter())
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

func TestControlPopulation_PaymentAttempts_HappyPath(t *testing.T) {
	st := newStubStore()
	st.popPage = &domain.ControlPopulationPage{
		Records: []domain.ControlPopulationRecord{{RecordID: cpAttempt, Reference: "PAY-1", Amount: "-10.50", Currency: "EUR",
			Date: "2026-09-01", Attributes: map[string]string{"status": "SUBMITTED"}}},
		NextRecordID: cpAttempt, Watermark: "pa1:n=1;md5=x",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 1, Totals: map[string]string{"EUR": "-10.50"}},
	}
	rec := cpGet(st, &stubAuthz{}, cpPath+"?legal_entity_id="+cpEntity+"&period_id=2026-09&limit=7", cpTenant, "p1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Records    []domain.ControlPopulationRecord `json:"records"`
		NextCursor string                           `json:"next_cursor"`
		Watermark  string                           `json:"watermark"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "pa1:n=1;md5=x", body.Watermark)
	require.Len(t, body.Records, 1)
	assert.Equal(t, "-10.50", body.Records[0].Amount)
	assert.Equal(t, base64.RawURLEncoding.EncodeToString([]byte(cpAttempt)), body.NextCursor)

	assert.Equal(t, cpTenant, st.popTenant)
	assert.Equal(t, cpEntity, st.popQuery.LegalEntityID)
	assert.Equal(t, 7, st.popQuery.Limit)
	require.NotNil(t, st.popQuery.CutOff)
	assert.Equal(t, "2026-09-30", st.popQuery.CutOff.Format("2006-01-02"), "period_id is the end-of-month cut-off")

	// no period_id means no cut-off; the cursor round-trips as the decoded id
	st.popPage = nil // now the store answers an empty population
	rec = cpGet(st, &stubAuthz{}, cpPath+"?legal_entity_id="+cpEntity+"&cursor="+body.NextCursor, cpTenant, "p1")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, st.popQuery.CutOff)
	assert.Equal(t, cpAttempt, st.popQuery.AfterRecordID)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Equal(t, "[]", string(raw["records"]), "an empty population encodes as [], never null")
	assert.Equal(t, `""`, string(raw["next_cursor"]))
}

func TestControlPopulation_PaymentAttempts_RejectsMalformedRequests(t *testing.T) {
	ok := "?legal_entity_id=" + cpEntity
	cases := map[string]struct {
		path, tenant, principal string
		want                    int
	}{
		"no tenant":          {cpPath + ok, "", "p1", http.StatusUnauthorized},
		"tenant not a uuid":  {cpPath + ok, "tenant-1", "p1", http.StatusUnauthorized},
		"no principal":       {cpPath + ok, cpTenant, "", http.StatusUnauthorized},
		"missing entity":     {cpPath, cpTenant, "p1", http.StatusBadRequest},
		"entity not a uuid":  {cpPath + "?legal_entity_id=le-1", cpTenant, "p1", http.StatusBadRequest},
		"bad period":         {cpPath + ok + "&period_id=2026-13", cpTenant, "p1", http.StatusBadRequest},
		"date as period":     {cpPath + ok + "&period_id=2026-09-01", cpTenant, "p1", http.StatusBadRequest},
		"unknown param":      {cpPath + ok + "&bogus=1", cpTenant, "p1", http.StatusBadRequest},
		"repeated param":     {cpPath + ok + "&limit=1&limit=2", cpTenant, "p1", http.StatusBadRequest},
		"limit zero":         {cpPath + ok + "&limit=0", cpTenant, "p1", http.StatusBadRequest},
		"limit over max":     {cpPath + ok + "&limit=5001", cpTenant, "p1", http.StatusBadRequest},
		"cursor not a uuid":  {cpPath + ok + "&cursor=" + base64.RawURLEncoding.EncodeToString([]byte("nope")), cpTenant, "p1", http.StatusBadRequest},
		"cursor not base64":  {cpPath + ok + "&cursor=***", cpTenant, "p1", http.StatusBadRequest},
		"unknown population": {"/v1/control-populations/nope" + ok, cpTenant, "p1", http.StatusNotFound},
	}
	for name, c := range cases {
		st := newStubStore()
		assert.Equal(t, c.want, cpGet(st, &stubAuthz{}, c.path, c.tenant, c.principal).Code, name)
		assert.Zero(t, st.popCalls, "%s: the store must not be consulted", name)
	}
}

func TestControlPopulation_PaymentAttempts_AuthorizationAndStoreFailuresFailClosed(t *testing.T) {
	path := cpPath + "?legal_entity_id=" + cpEntity

	st := newStubStore()
	assert.Equal(t, http.StatusForbidden, cpGet(st, &stubAuthz{deny: true}, path, cpTenant, "p1").Code)
	assert.Zero(t, st.popCalls, "the store is never consulted for a denied caller")

	st = newStubStore()
	st.popErr = domain.ErrControlPopulationTooLarge
	assert.Equal(t, http.StatusUnprocessableEntity, cpGet(st, &stubAuthz{}, path, cpTenant, "p1").Code)

	st = newStubStore()
	st.popErr = errors.New("db down")
	assert.Equal(t, http.StatusServiceUnavailable, cpGet(st, &stubAuthz{}, path, cpTenant, "p1").Code)
}
