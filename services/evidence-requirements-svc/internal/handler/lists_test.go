package handler_test

// ER-3: the console read models, paged per API-4.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	"zoiko.io/evidence-requirements-svc/internal/handler"
)

type page struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor string            `json:"next_cursor"`
	PageSize   int               `json:"page_size"`
}

func decodePage(t *testing.T, body []byte) page {
	t.Helper()
	var p page
	require.NoError(t, json.Unmarshal(body, &p), string(body))
	return p
}

// listCase is one console list and a router serving three rows of it.
type listCase struct {
	name, path, unknownFilter string
	router                    func() http.Handler
}

func listCases() []listCase {
	reqs := []domain.EvidenceRequirement{}
	evals := []domain.EvidenceEvaluation{}
	pbcRows := []*domain.EvidenceRequest{}
	for i := 0; i < 3; i++ {
		reqs = append(reqs, domain.EvidenceRequirement{EvidenceRequirementID: fmt.Sprintf("req-%d", i), TenantID: testTenant})
		evals = append(evals, domain.EvidenceEvaluation{EvaluationID: fmt.Sprintf("eval-%d", i), TenantID: testTenant})
		pbcRows = append(pbcRows, &domain.EvidenceRequest{RequestID: fmt.Sprintf("pbc-%d", i), TenantID: testTenant})
	}
	return []listCase{
		{"requirements", "/v1/evidence-requirements", "filter[colour]=red", func() http.Handler {
			return newRouter(&stubStore{requirements: reqs}, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
		}},
		{"evaluations", "/v1/evidence/evaluations", "filter[colour]=red", func() http.Handler {
			return newRouter(&stubStore{evaluations: evals}, &stubPublisher{}, &stubAuthz{}, &stubDocs{})
		}},
		{"pbc requests", "/v1/pbc/requests", "filter[colour]=red", func() http.Handler {
			return pbcRouter(&stubPBC{requests: pbcRows}, &recordingAuthz{})
		}},
	}
}

func TestER03_ListsPageWithSignedCursors(t *testing.T) {
	for _, lc := range listCases() {
		t.Run(lc.name, func(t *testing.T) {
			h := lc.router()

			// First page.
			rec := do(t, h, http.MethodGet, lc.path+"?page_size=2", nil, testTenant, testPrincipal)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			first := decodePage(t, rec.Body.Bytes())
			assert.Len(t, first.Items, 2)
			assert.Equal(t, 2, first.PageSize)
			require.NotEmpty(t, first.NextCursor, "a third row exists, so there is a next page")

			// Next page.
			rec = do(t, h, http.MethodGet, lc.path+"?page_size=2&cursor="+url.QueryEscape(first.NextCursor), nil, testTenant, testPrincipal)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			second := decodePage(t, rec.Body.Bytes())
			assert.Len(t, second.Items, 1)
			assert.Empty(t, second.NextCursor, "the last page has no next cursor")
			assert.NotEqual(t, string(first.Items[0]), string(second.Items[0]))

			// A tampered cursor.
			tampered := first.NextCursor[:len(first.NextCursor)-2] + "xx"
			rec = do(t, h, http.MethodGet, lc.path+"?page_size=2&cursor="+url.QueryEscape(tampered), nil, testTenant, testPrincipal)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "invalid_cursor", problemOf(t, rec)["code"])

			// A cursor replayed by another tenant.
			rec = do(t, h, http.MethodGet, lc.path+"?page_size=2&cursor="+url.QueryEscape(first.NextCursor), nil, otherTenant, testPrincipal)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "invalid_cursor", problemOf(t, rec)["code"])

			// A cursor replayed against another filter set.
			rec = do(t, h, http.MethodGet, lc.path+"?page_size=2&filter[legal_entity_id]="+testEntity+"&cursor="+url.QueryEscape(first.NextCursor), nil, testTenant, testPrincipal)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "invalid_cursor", problemOf(t, rec)["code"])

			// An unknown filter.
			rec = do(t, h, http.MethodGet, lc.path+"?"+lc.unknownFilter, nil, testTenant, testPrincipal)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "unknown_filter", problemOf(t, rec)["code"])

			// A bare (pre-API-4) filter is refused, not ignored.
			rec = do(t, h, http.MethodGet, lc.path+"?legal_entity_id="+testEntity, nil, testTenant, testPrincipal)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "unknown_filter", problemOf(t, rec)["code"])

			// page_size outside 1..200.
			rec = do(t, h, http.MethodGet, lc.path+"?page_size=201", nil, testTenant, testPrincipal)
			assert.Equal(t, http.StatusBadRequest, rec.Code)

			// No identity, no list.
			rec = do(t, h, http.MethodGet, lc.path, nil, testTenant, "")
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

func TestER03_ListsAreAuthorizedAtTheFilteredEntity(t *testing.T) {
	for _, c := range []struct{ path, action string }{
		{"/v1/evidence-requirements", "EVIDENCE_REQUIREMENT_READ"},
		{"/v1/evidence/evaluations", "EVIDENCE_EVALUATION_READ"},
		{"/v1/evidence/summary", "EVIDENCE_REQUIREMENT_READ"},
	} {
		az := &recordingAuthz{deny: map[string]error{c.action: domain.ErrAuthorizationDenied}}
		h := completionRouter(&stubStore{summaryResult: &domain.EvidenceSummary{}}, az, &stubDocs{}, &stubManifest{}, &stubEsignature{}, stubRegistry{}, handler.GateEnforce, handler.GateEnforce)
		rec := do(t, h, http.MethodGet, c.path, nil, testTenant, testPrincipal)
		assert.Equal(t, http.StatusForbidden, rec.Code, c.path)
		assert.Equal(t, []string{c.action + "@PLATFORM"}, az.asked, c.path)
	}

	// The PBC register is the managers' register.
	az := &recordingAuthz{deny: map[string]error{"EVIDENCE_REQUEST_MANAGE": domain.ErrAuthorizationDenied}}
	rec := do(t, pbcRouter(&stubPBC{}, az), http.MethodGet, "/v1/pbc/requests?filter[legal_entity_id]="+testEntity, nil, testTenant, testPrincipal)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, []string{"EVIDENCE_REQUEST_MANAGE@" + testEntity}, az.asked)
}

func TestER03_EvaluationFiltersAreValidated(t *testing.T) {
	st := &stubStore{}
	h := newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{})

	rec := do(t, h, http.MethodGet, "/v1/evidence/evaluations?filter[outcome]=MAYBE", nil, testTenant, testPrincipal)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = do(t, h, http.MethodGet, "/v1/evidence/evaluations?filter[since]=last-week", nil, testTenant, testPrincipal)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	since := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	rec = do(t, h, http.MethodGet, "/v1/evidence/evaluations?filter[outcome]=MISSING&filter[since]="+url.QueryEscape(since)+
		"&filter[domain_code]=FINANCE&filter[action_type]=JOURNAL_POST", nil, testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "MISSING", st.evaluationFilter.Outcome)
	assert.Equal(t, "FINANCE", st.evaluationFilter.DomainCode)
	assert.Equal(t, since, st.evaluationFilter.Since.UTC().Format(time.RFC3339))
}

func TestER03_PBCListKeepsItsOpenOnlyDefault(t *testing.T) {
	pbc := &stubPBC{}
	h := pbcRouter(pbc, &recordingAuthz{})
	rec := do(t, h, http.MethodGet, "/v1/pbc/requests", nil, testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, pbc.listed.OpenOnly)

	rec = do(t, h, http.MethodGet, "/v1/pbc/requests?filter[open]=false&filter[status]=closed", nil, testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, pbc.listed.OpenOnly)
	assert.Equal(t, "CLOSED", pbc.listed.Status)

	rec = do(t, h, http.MethodGet, "/v1/pbc/requests?filter[status]=LOST", nil, testTenant, testPrincipal)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// An unreadable summary is 503, never a page of zeros.
func TestER03_SummaryUnavailableIsNeverZero(t *testing.T) {
	st := &stubStore{summaryErr: domain.ErrStoreUnavailable}
	rec := do(t, newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{}), http.MethodGet, "/v1/evidence/summary", nil, testTenant, testPrincipal)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.False(t, strings.Contains(rec.Body.String(), "active_requirements"))

	st = &stubStore{summaryResult: &domain.EvidenceSummary{ActiveRequirements: 4, EvaluationsMissing30d: 2}}
	rec = do(t, newRouter(st, &stubPublisher{}, &stubAuthz{}, &stubDocs{}), http.MethodGet, "/v1/evidence/summary", nil, testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got domain.EvidenceSummary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, 4, got.ActiveRequirements)
	assert.Equal(t, 2, got.EvaluationsMissing30d)
}
