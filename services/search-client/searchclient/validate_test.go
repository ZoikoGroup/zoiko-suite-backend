package searchclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run the real client against an in-process HTTP fake, so "no
// network call was made" is a counted fact rather than an assumption. They
// prove request shape and local validation. They do NOT prove behaviour of a
// real OpenSearch cluster — that needs the integration-tagged tests.

type rec struct {
	method, path, body string
}

type fakeEngine struct {
	mu   sync.Mutex
	reqs []rec
	h    func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeEngine) client(t *testing.T) *client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, rec{r.Method, r.URL.Path, string(b)})
		f.mu.Unlock()
		f.h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{Addresses: []string{srv.URL}})
	require.NoError(t, err)
	return c.(*client)
}

func (f *fakeEngine) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

const emptySearchResponse = `{"took":1,"timed_out":false,"_shards":{"total":1,"successful":1,"skipped":0,"failed":0},"hits":{"total":{"value":0,"relation":"eq"},"hits":[]}}`

func okSearch(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, emptySearchResponse)
}

func tenantOnly() []TermFilter { return []TermFilter{{Field: "tenant_id", Values: []string{"t1"}}} }

// ── mandatory filters fail closed ────────────────────────────────────────────

func TestCompile_NeverOmitsAMandatoryFilter(t *testing.T) {
	// Even if compile is reached with an unusable mandatory filter (ExecutePlan
	// refuses it first), the clause becomes match_none — it is never dropped.
	for name, f := range map[string]TermFilter{
		"empty tenant":    {Field: "tenant_id"},
		"empty residency": {Field: "residency_region", Values: nil},
		"empty acl":       {Field: "acl_refs", Values: []string{}},
	} {
		body := ExecutionPlan{MandatoryFilters: []TermFilter{f}}.compile()
		filter := body["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]map[string]any)
		require.Len(t, filter, 1, name)
		_, isNone := filter[0]["match_none"]
		assert.True(t, isNone, "%s: an unusable mandatory filter must compile to match_none, got %v", name, filter[0])
	}
}

func TestCompile_ValidMandatoryFiltersCompileNormally(t *testing.T) {
	body := ExecutionPlan{MandatoryFilters: []TermFilter{
		{Field: "tenant_id", Values: []string{"t1"}},
		{Field: "residency_region", Values: []string{"IN", "GLOBAL"}},
	}}.compile()
	filter := body["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]map[string]any)
	require.Len(t, filter, 2)
	assert.Equal(t, map[string]any{"term": map[string]any{"tenant_id": "t1"}}, filter[0])
	assert.Contains(t, filter[1], "terms")
}

func TestCompile_EmptyUserFilterIsStillOptional(t *testing.T) {
	body := ExecutionPlan{
		MandatoryFilters: tenantOnly(),
		UserFilters:      []TermFilter{{Field: "status"}},
	}.compile()
	filter := body["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]map[string]any)
	assert.Len(t, filter, 1, "an empty user filter only narrows, so it stays optional")
}

func TestExecutePlan_UnusableMandatoryFilters_FailClosedWithoutNetwork(t *testing.T) {
	cases := map[string]ExecutionPlan{
		"empty residency values": {MandatoryFilters: append(tenantOnly(), TermFilter{Field: "residency_region"})},
		"empty acl values":       {MandatoryFilters: append(tenantOnly(), TermFilter{Field: "acl_refs", Values: []string{}})},
		"blank value":            {MandatoryFilters: append(tenantOnly(), TermFilter{Field: "acl_refs", Values: []string{"entity:e1", "  "}})},
		"blank field":            {MandatoryFilters: append(tenantOnly(), TermFilter{Field: " ", Values: []string{"x"}})},
		"empty exclusion":        {MandatoryFilters: tenantOnly(), MandatoryMustNot: []TermFilter{{Field: "tombstoned"}}},
	}
	for name, plan := range cases {
		f := &fakeEngine{h: okSearch}
		_, err := f.client(t).ExecutePlan(context.Background(), "zoiko-documents", plan)
		assert.ErrorIs(t, err, ErrMandatoryFilterEmpty, name)
		assert.Zero(t, f.count(), "%s: an unscoped plan must not reach the engine", name)
	}
}

// ── tenant enforcement ───────────────────────────────────────────────────────

func TestExecutePlan_TenantScope(t *testing.T) {
	cases := map[string]struct {
		plan ExecutionPlan
		ok   bool
	}{
		"no filters at all":            {ExecutionPlan{}, false},
		"no tenant filter":             {ExecutionPlan{MandatoryFilters: []TermFilter{{Field: "residency_region", Values: []string{"IN"}}}}, false},
		"empty tenant values":          {ExecutionPlan{MandatoryFilters: []TermFilter{{Field: "tenant_id"}}}, false},
		"blank tenant":                 {ExecutionPlan{MandatoryFilters: []TermFilter{{Field: "tenant_id", Values: []string{""}}}}, false},
		"several tenants":              {ExecutionPlan{MandatoryFilters: []TermFilter{{Field: "tenant_id", Values: []string{"t1", "t2"}}}}, false},
		"tenant only as a user filter": {ExecutionPlan{UserFilters: tenantOnly()}, false},
		"one tenant":                   {ExecutionPlan{MandatoryFilters: tenantOnly()}, true},
		"tenant plus others": {ExecutionPlan{MandatoryFilters: []TermFilter{
			{Field: "tenant_id", Values: []string{"t1"}},
			{Field: "residency_region", Values: []string{"IN", "GLOBAL"}},
			{Field: "acl_refs", Values: []string{"entity:e1"}},
		}, MandatoryMustNot: []TermFilter{{Field: "tombstoned", Values: []string{"true"}}}}, true},
	}
	for name, tc := range cases {
		f := &fakeEngine{h: okSearch}
		_, err := f.client(t).ExecutePlan(context.Background(), "zoiko-documents", tc.plan)
		if tc.ok {
			require.NoError(t, err, name)
			require.Equal(t, 1, f.count(), name)
			continue
		}
		assert.ErrorIs(t, err, ErrTenantIDRequired, name)
		assert.Zero(t, f.count(), "%s: no network call on an unscoped plan", name)
	}
}

func TestExecutePlan_TenantFilterReachesTheEngineAsAnExactTerm(t *testing.T) {
	f := &fakeEngine{h: okSearch}
	_, err := f.client(t).ExecutePlan(context.Background(), "zoiko-documents", ExecutionPlan{MandatoryFilters: tenantOnly()})
	require.NoError(t, err)
	require.Equal(t, 1, f.count())
	var sent struct {
		Query struct {
			Bool struct {
				Filter []map[string]map[string]any `json:"filter"`
			} `json:"bool"`
		} `json:"query"`
	}
	require.NoError(t, json.Unmarshal([]byte(f.reqs[0].body), &sent))
	require.Len(t, sent.Query.Bool.Filter, 1)
	assert.Equal(t, "t1", sent.Query.Bool.Filter[0]["term"]["tenant_id"])
}

func TestExecutePlan_SemanticPlanIsValidatedToo(t *testing.T) {
	f := &fakeEngine{h: okSearch}
	_, err := f.client(t).ExecutePlan(context.Background(), "zoiko-documents", ExecutionPlan{Vector: []float32{1, 0}, K: 3})
	assert.ErrorIs(t, err, ErrTenantIDRequired)
	assert.Zero(t, f.count())
}

// ── target ───────────────────────────────────────────────────────────────────

func TestExecutePlan_Target(t *testing.T) {
	for _, ok := range []string{"zoiko-documents", "zoiko-obligations", GenerationIndex(IndexDocuments, "ABC123"), "records.v2"} {
		f := &fakeEngine{h: okSearch}
		_, err := f.client(t).ExecutePlan(context.Background(), ok, ExecutionPlan{MandatoryFilters: tenantOnly()})
		require.NoError(t, err, ok)
		assert.Equal(t, 1, f.count(), ok)
	}
	for _, bad := range []string{"", "*", "zoiko-*", "_all", "a,b", "zoiko-documents,zoiko-audit", ".opendistro_security",
		"-zoiko-documents", "+x", "a b", " zoiko-documents", "x/../y", "remote:zoiko-documents", "<logs-{now/d}>", "zoiko?"} {
		f := &fakeEngine{h: okSearch}
		_, err := f.client(t).ExecutePlan(context.Background(), bad, ExecutionPlan{MandatoryFilters: tenantOnly()})
		assert.ErrorIs(t, err, ErrInvalidTarget, "target %q", bad)
		assert.Zero(t, f.count(), "target %q must be refused before any request", bad)
	}
}

// ── EnsureIndex ──────────────────────────────────────────────────────────────

func head404(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }

func TestEnsureIndex_AlreadyExists_NoCreate(t *testing.T) {
	f := &fakeEngine{h: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }}
	require.NoError(t, f.client(t).EnsureIndex(context.Background(), IndexObligations))
	require.Len(t, f.reqs, 1)
	assert.Equal(t, http.MethodHead, f.reqs[0].method)
}

func TestEnsureIndex_Missing_CreatesWithTenantKeywordMapping(t *testing.T) {
	f := &fakeEngine{h: func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			head404(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"acknowledged":true,"shards_acknowledged":true,"index":"zoiko-obligations"}`)
	}}
	require.NoError(t, f.client(t).EnsureIndex(context.Background(), IndexObligations))
	require.Len(t, f.reqs, 2)
	assert.Equal(t, http.MethodPut, f.reqs[1].method)
	assert.Contains(t, f.reqs[1].body, `"tenant_id":{"type":"keyword"}`)
	assert.Contains(t, f.reqs[1].body, `"legal_entity_id":{"type":"keyword"}`)
}

func TestEnsureIndex_LosingTheCreateRace_IsSuccess(t *testing.T) {
	f := &fakeEngine{h: func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			head404(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"root_cause":[{"type":"resource_already_exists_exception","reason":"index [zoiko-obligations] already exists"}],"type":"resource_already_exists_exception","reason":"index [zoiko-obligations] already exists"},"status":400}`)
	}}
	require.NoError(t, f.client(t).EnsureIndex(context.Background(), IndexObligations),
		"another replica created the index between the check and the create")
}

func TestEnsureIndex_OtherCreateFailureIsAnError(t *testing.T) {
	f := &fakeEngine{h: func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			head404(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"mapper_parsing_exception","reason":"bad mapping"},"status":400}`)
	}}
	assert.Error(t, f.client(t).EnsureIndex(context.Background(), IndexObligations))
}

func TestEnsureIndex_UnrelatedErrorContaining404IsNotMissing(t *testing.T) {
	// The exists-check fails with a 500 whose body happens to say "404". The old
	// code read any error text containing "404" as "index missing" and went on
	// to create. The status code is what counts.
	f := &fakeEngine{h: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"type":"exception","reason":"node 10.0.4.04 request 404abc failed"},"status":500}`)
	}}
	err := f.client(t).EnsureIndex(context.Background(), IndexObligations)
	require.Error(t, err)
	for _, r := range f.reqs {
		assert.NotEqual(t, http.MethodPut, r.method, "an unrelated failure must not trigger a create")
	}
}

func TestIsMissing_StructuredStatusBeatsText(t *testing.T) {
	respond := func(status int, body string) *fakeEngine {
		return &fakeEngine{h: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}}
	}
	// A 404 index_not_found really is "missing": zero, no error.
	f := respond(404, `{"error":{"type":"index_not_found_exception","reason":"no such index [idx]"},"status":404}`)
	n, err := f.client(t).CountMissing(context.Background(), "idx", "tenant_id")
	require.NoError(t, err)
	assert.Zero(t, n)

	// A 400 whose prose merely mentions index_not_found is NOT missing: the
	// structured status says 400, so it surfaces as an error.
	f = respond(400, `{"error":{"type":"illegal_argument_exception","reason":"the words index_not_found appear only in prose"},"status":400}`)
	_, err = f.client(t).CountMissing(context.Background(), "idx", "tenant_id")
	assert.Error(t, err)
}

func TestIsMissing_TextWithA404ThatIsNotAStatusIsNotMissing(t *testing.T) {
	for _, s := range []string{
		"dial tcp 10.0.4.04:9200: connection refused",
		"request id 404abc failed",
		"searchclient: something took 404ms",
	} {
		assert.False(t, isMissing(errString(s)), "%q", s)
	}
}

// ── projection validation ────────────────────────────────────────────────────

func TestProjectionWriters_ShareTheSameValidation(t *testing.T) {
	good := Projection{DocID: "t1:invoice:i1", TenantID: "t1", SourceType: "invoice", SourceID: "i1"}
	bad := map[string]Projection{
		"no doc id":    {TenantID: "t1", SourceType: "invoice", SourceID: "i1"},
		"no tenant":    {DocID: "d", SourceType: "invoice", SourceID: "i1"},
		"blank tenant": {DocID: "d", TenantID: "  ", SourceType: "invoice", SourceID: "i1"},
		"no type":      {DocID: "d", TenantID: "t1", SourceID: "i1"},
		"no source id": {DocID: "d", TenantID: "t1", SourceType: "invoice"},
	}
	for name, p := range bad {
		f := &fakeEngine{h: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }}
		c := f.client(t)
		assert.Error(t, c.IndexProjection(context.Background(), "idx", p), "IndexProjection: "+name)
		assert.Error(t, c.CreateProjection(context.Background(), "idx", p), "CreateProjection: "+name)
		assert.Zero(t, f.count(), "%s: an invalid projection must not reach the engine", name)
	}

	f := &fakeEngine{h: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"_index":"idx","_id":"d","_version":1,"result":"created","_shards":{"total":1,"successful":1,"failed":0},"_seq_no":0,"_primary_term":1}`)
	}}
	c := f.client(t)
	require.NoError(t, c.IndexProjection(context.Background(), "idx", good))
	require.NoError(t, c.CreateProjection(context.Background(), "idx", good))
	assert.Equal(t, 2, f.count())
}

func TestProjectionValidation_DoesNotRequireLegalEntity(t *testing.T) {
	// ESR §2.1 does not list legal_entity_id on a ProjectionDocument, so the
	// shared validation does not invent the requirement.
	assert.NoError(t, Projection{DocID: "d", TenantID: "t", SourceType: "x", SourceID: "y"}.validate("x"))
}
