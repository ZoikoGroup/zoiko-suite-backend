package benefits_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/benefits"
)

func server(t *testing.T, status int, body string, capture *http.Request) *benefits.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			*capture = *r.Clone(r.Context())
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return benefits.NewClient(srv.URL, nil)
}

func TestListPlans(t *testing.T) {
	var got http.Request
	c := server(t, 200, `[{"plan_id":"pl-1","plan_type":"HEALTH_INSURANCE","deduction_tax_treatment":"PRE_TAX","currency":"GBP","status":"ACTIVE"}]`, &got)
	plans, err := c.ListPlans(context.Background(), "t-1", "p-1", "le 1")
	require.NoError(t, err)
	require.Len(t, plans, 1)
	assert.Equal(t, "PRE_TAX", plans[0].DeductionTaxTreatment)
	assert.Equal(t, "/v1/benefits/plans", got.URL.Path)
	assert.Equal(t, "le 1", got.URL.Query().Get("legal_entity_id"), "the entity is query-escaped")
	assert.Equal(t, "t-1", got.Header.Get("X-Tenant-Id"))
	assert.Equal(t, "p-1", got.Header.Get("X-Principal-Id"))
}

func TestListActiveElections(t *testing.T) {
	var got http.Request
	c := server(t, 200, `[{"election_id":"e-1","plan_id":"pl-1","employee_contribution_amount":50.5,"employer_contribution_amount":120,"effective_from":"2026-01-01","status":"ACTIVE"}]`, &got)
	es, err := c.ListActiveElections(context.Background(), "t-1", "p-1", "w/1")
	require.NoError(t, err)
	require.Len(t, es, 1)
	assert.Equal(t, 50.5, es[0].EmployeeContributionAmount)
	assert.Equal(t, "/v1/benefits/elections/employee/w%2F1", got.URL.EscapedPath(), "the employee id is path-escaped")
	assert.Equal(t, "ACTIVE", got.URL.Query().Get("status"))
}

func TestFailuresAreErrorsNeverAnEmptyList(t *testing.T) {
	for _, status := range []int{401, 403, 404, 500, 503} {
		c := server(t, status, `[]`, nil)
		_, err := c.ListActiveElections(context.Background(), "t", "p", "w")
		assert.Error(t, err, "status %d must not read as 'no elections'", status)
		_, err = c.ListPlans(context.Background(), "t", "p", "le")
		assert.Error(t, err, "status %d must not read as 'no plans'", status)
	}
	c := server(t, 200, `not json`, nil)
	_, err := c.ListPlans(context.Background(), "t", "p", "le")
	assert.Error(t, err)
	_, err = benefits.NewClient("http://127.0.0.1:1", nil).ListActiveElections(context.Background(), "t", "p", "w")
	assert.Error(t, err)
}
