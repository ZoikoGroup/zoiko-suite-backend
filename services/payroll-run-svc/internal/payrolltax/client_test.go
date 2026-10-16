package payrolltax

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
)

func serve(t *testing.T, status int, body string, seen *http.Request, seenBody *Request) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = *r.Clone(context.Background())
		}
		if seenBody != nil {
			_ = json.NewDecoder(r.Body).Decode(seenBody)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL+"/", nil)
}

const good = `{"calculation_id":"calc-1","jurisdiction_code":"GB","total_tax_amount":679.5,"engine_type":"STANDARD_ENGINE","rule_version_used":"2026.1",
 "tax_breakdown":[{"tax_name":"Income Tax","tax_type":"STATE_FEDERAL","rate_pct":15,"tax_amount":450},
                  {"tax_name":"Social Insurance","tax_type":"SOCIAL_SECURITY","rate_pct":6.2,"tax_amount":186},
                  {"tax_name":"Medical Insurance","tax_type":"HEALTH_STATUTORY","rate_pct":1.45,"tax_amount":43.5}]}`

func request() Request {
	return Request{PayrollRunID: "run-1", EmployeeID: "emp-1", JurisdictionCode: "GB", GrossTaxableAmount: 3000, Currency: "GBP", CorrelationID: "corr-1"}
}

func TestCalculateSendsTheIdentityAndTheRequestAndReturnsTheEnginesAnswer(t *testing.T) {
	var seen http.Request
	var body Request
	c := serve(t, 201, good, &seen, &body)
	res, err := c.Calculate(context.Background(), Caller{TenantID: "t-1", PrincipalID: "p-1"}, request())
	require.NoError(t, err)
	assert.Equal(t, "/v1/payroll-tax/calculate", seen.URL.Path)
	assert.Equal(t, http.MethodPost, seen.Method)
	assert.Equal(t, "t-1", seen.Header.Get("X-Tenant-Id"))
	assert.Equal(t, "p-1", seen.Header.Get("X-Principal-Id"))
	assert.Equal(t, "corr-1", seen.Header.Get("X-Correlation-ID"))
	assert.Equal(t, request(), body)
	assert.Equal(t, "calc-1", res.CalculationID)
	assert.Equal(t, "2026.1", res.RuleVersionUsed)
	assert.Equal(t, "STANDARD_ENGINE", res.EngineType)
	assert.Len(t, res.TaxBreakdown, 3)
	assert.InDelta(t, 679.5, res.TotalTaxAmount, 0.0001)

	// 200 is a replay of a stored calculation; it is just as valid.
	_, err = serve(t, 200, good, nil, nil).Calculate(context.Background(), Caller{TenantID: "t-1", PrincipalID: "p-1"}, request())
	assert.NoError(t, err)
}

func TestAnythingButACleanAnswerIsTheTaxEngineUnavailable(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"server error":   {500, ``, domain.ErrTaxEngineUnavailable},
		"refused":        {403, `{"error":"forbidden"}`, domain.ErrTaxEngineUnavailable},
		"bad request":    {400, `{}`, domain.ErrTaxEngineUnavailable},
		"garbage":        {200, `nope`, domain.ErrTaxEngineUnavailable},
		"no calc id":     {200, `{"engine_type":"E","rule_version_used":"1","total_tax_amount":0,"tax_breakdown":[]}`, domain.ErrTaxEngineUnavailable},
		"no rule":        {200, `{"calculation_id":"c","engine_type":"E","total_tax_amount":0,"tax_breakdown":[]}`, domain.ErrTaxEngineUnavailable},
		"no engine":      {200, `{"calculation_id":"c","rule_version_used":"1","total_tax_amount":0,"tax_breakdown":[]}`, domain.ErrTaxEngineUnavailable},
		"negative part":  {200, `{"calculation_id":"c","engine_type":"E","rule_version_used":"1","total_tax_amount":-5,"tax_breakdown":[{"tax_name":"x","tax_type":"FEDERAL","tax_amount":-5}]}`, domain.ErrTaxEngineUnavailable},
		"total mismatch": {200, `{"calculation_id":"c","engine_type":"E","rule_version_used":"1","total_tax_amount":100,"tax_breakdown":[{"tax_name":"x","tax_type":"FEDERAL","tax_amount":99}]}`, domain.ErrTaxTotalMismatch},
	} {
		_, err := serve(t, tc.status, tc.body, nil, nil).Calculate(context.Background(), Caller{TenantID: "t-1", PrincipalID: "p-1"}, request())
		assert.ErrorIs(t, err, tc.want, name)
	}
	_, err := NewClient("http://127.0.0.1:1", nil).Calculate(context.Background(), Caller{TenantID: "t-1", PrincipalID: "p-1"}, request())
	assert.ErrorIs(t, err, domain.ErrTaxEngineUnavailable, "unreachable blocks the run, it does not estimate")
}
