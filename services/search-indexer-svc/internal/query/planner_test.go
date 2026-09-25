package query

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
)

var testKey = []byte("a-test-key-at-least-thirty-two-bytes-long")

// fakeStore implements store.Store for planner tests.
type fakeStore struct{}

func (f *fakeStore) CreateSource(ctx context.Context, s domain.SearchSource) error                   { return nil }
func (f *fakeStore) GetSource(ctx context.Context, sourceID string) (*domain.SearchSource, error)   { return nil, domain.ErrNotFound }
func (f *fakeStore) GetSourceByType(ctx context.Context, sourceType string) (*domain.SearchSource, error) { return nil, domain.ErrNotFound }
func (f *fakeStore) ListSources(ctx context.Context) ([]domain.SearchSource, error)                { return nil, nil }
func (f *fakeStore) CreateContract(ctx context.Context, c domain.IndexContract) error              { return nil }
func (f *fakeStore) GetContract(ctx context.Context, contractID string) (*domain.IndexContract, error) { return nil, domain.ErrNotFound }
func (f *fakeStore) GetPublishedContract(ctx context.Context, scopeName string) (*domain.IndexContract, error) { return nil, domain.ErrNotFound }
func (f *fakeStore) ListContracts(ctx context.Context, scopeName string) ([]domain.IndexContract, error) { return nil, nil }
func (f *fakeStore) TransitionContract(ctx context.Context, contractID string, from, to domain.ContractState) error { return nil }
func (f *fakeStore) NextContractVersion(ctx context.Context, scopeName string) (int, error)          { return 1, nil }
func (f *fakeStore) CreateGeneration(ctx context.Context, g domain.IndexGeneration) error           { return nil }
func (f *fakeStore) GetGeneration(ctx context.Context, generationID string) (*domain.IndexGeneration, error) { return nil, domain.ErrNotFound }
func (f *fakeStore) ListGenerations(ctx context.Context, scopeName string) ([]domain.IndexGeneration, error) { return nil, nil }
func (f *fakeStore) GetActiveGeneration(ctx context.Context, scopeName string) (*domain.IndexGeneration, error) { return nil, domain.ErrNotFound }
func (f *fakeStore) TransitionGeneration(ctx context.Context, generationID string, from, to domain.GenerationState, digest, note string) error { return nil }
func (f *fakeStore) UpsertCheckpoint(ctx context.Context, cp domain.IndexCheckpoint) error          { return nil }
func (f *fakeStore) ListCheckpoints(ctx context.Context, scopeName string) ([]domain.IndexCheckpoint, error) { return nil, nil }
func (f *fakeStore) GetLatestCheckpoint(ctx context.Context, scopeName string) (*domain.IndexCheckpoint, error) { return nil, nil }
func (f *fakeStore) GetProjectionRecord(ctx context.Context, tenantID, scope, sourceType, sourceID string) (*domain.ProjectionRecord, error) { return nil, nil }
func (f *fakeStore) UpsertProjectionRecord(ctx context.Context, r domain.ProjectionRecord) (bool, error) { return true, nil }
func (f *fakeStore) CountProjections(ctx context.Context, scopeName string) (int64, int64, error)    { return 0, 0, nil }
func (f *fakeStore) UpsertTombstone(ctx context.Context, t domain.RestrictionTombstone, tombstoneID string) (bool, error) { return true, nil }
func (f *fakeStore) MarkTombstoneState(ctx context.Context, tenantID, sourceType, sourceID, sourceEventID string, state domain.PropagationState, reason string) error { return nil }
func (f *fakeStore) ListPendingVerification(ctx context.Context, limit int) ([]domain.RestrictionTombstone, error) { return nil, nil }
func (f *fakeStore) ListTombstones(ctx context.Context, tenantID, scopeName string, limit int) ([]domain.RestrictionTombstone, error) { return nil, nil }
func (f *fakeStore) RecordEvidence(ctx context.Context, e domain.SearchEvidence) error              { return nil }
func (f *fakeStore) ListEvidence(ctx context.Context, tenantID, scopeName string, limit int) ([]domain.SearchEvidence, error) { return nil, nil }
func (f *fakeStore) Ping(ctx context.Context) error                                                 { return nil }
func (f *fakeStore) Close()                                                                        {}

func planner() *Planner { return NewPlanner(DefaultLimits(), &fakeStore{}, testKey) }

func contract() *domain.IndexContract {
	return &domain.IndexContract{
		ContractID:     "c-1",
		ScopeName:      "obligation",
		Version:        1,
		State:          domain.ContractPublished,
		RetrievalClass: domain.RetrievalR1,
		AuthzAction:    "OBLIGATION_READ",
		Fields: []domain.SearchFieldDefinition{
			{FieldID: "obligation_code", Type: "TEXT", Searchable: true, Returnable: true,
				SnippetAllowed: true, Filterable: true, SensitivityClass: domain.SensitivityInternal},
			{FieldID: "obligation_status", Type: "KEYWORD", Filterable: true, Facetable: true,
				Returnable: true, SensitivityClass: domain.SensitivityInternal},
			{FieldID: "due_date", Type: "DATE", Sortable: true, Returnable: true,
				SensitivityClass: domain.SensitivityInternal},
			// Registered, indexed, and deliberately NOT returnable — the
			// shape NP-09 and NP-53 are about.
			{FieldID: "internal_risk_score", Type: "LONG", Filterable: true, Sortable: false,
				Returnable: false, SensitivityClass: domain.SensitivityRestricted},
			{FieldID: "api_secret", Type: "KEYWORD", SensitivityClass: domain.SensitivitySecretProhibited},
		},
	}
}

func generation() *domain.IndexGeneration {
	return &domain.IndexGeneration{
		GenerationID:  "g-1",
		ContractID:    "c-1",
		ScopeName:     "obligation",
		PhysicalIndex: "obligation-gabc123",
		State:         domain.GenerationActive,
	}
}

func validContext() Context {
	return Context{
		TenantID:        "tenant-a",
		ActorID:         "principal-1",
		LegalEntityID:   "entity-1",
		Purpose:         "COMPLIANCE_REVIEW",
		ResidencyRegion: "EU",
	}
}

func codeOf(t *testing.T, err error) domain.ReasonCode {
	t.Helper()
	require.Error(t, err)
	var qerr *Error
	require.ErrorAs(t, err, &qerr)
	return qerr.Code
}

// NP-01. The Request type has no tenant field at all, so a caller cannot
// assert one — and a body that tries is rejected by the handler's strict
// decoder. This test pins the type-level half: adding a tenant_id field to
// Request would make this fail to compile in spirit, so instead we prove the
// compiled filter comes from context.
func TestCompile_TenantFilterComesFromTrustedContextOnly(t *testing.T) {
	plan, err := planner().Compile(context.Background(), Request{Scope: "obligation", Text: "GST"}, validContext(), contract(), generation())
	require.NoError(t, err)

	var tenantFilter *searchclient.TermFilter
	for i := range plan.Execution.MandatoryFilters {
		if plan.Execution.MandatoryFilters[i].Field == "tenant_id" {
			tenantFilter = &plan.Execution.MandatoryFilters[i]
		}
	}
	require.NotNil(t, tenantFilter, "every plan must carry a tenant filter")
	assert.Equal(t, []string{"tenant-a"}, tenantFilter.Values)

	// And the Request type genuinely has nowhere to put one.
	body, err := json.Marshal(Request{Scope: "obligation"})
	require.NoError(t, err)
	assert.NotContains(t, string(body), "tenant")
}

// ESR-001. No verified tenant, no search — ever.
func TestCompile_RefusesWithoutTenant(t *testing.T) {
	tc := validContext()
	tc.TenantID = ""
	_, err := planner().Compile(context.Background(), Request{Scope: "obligation"}, tc, contract(), generation())
	assert.Equal(t, domain.ReasonTenantContextMissing, codeOf(t, err))
}

// ESR-009. §6.1 makes purpose mandatory for sensitive corpora, and this
// service treats every corpus as governed.
func TestCompile_RefusesWithoutPurpose(t *testing.T) {
	tc := validContext()
	tc.Purpose = ""
	_, err := planner().Compile(context.Background(), Request{Scope: "obligation"}, tc, contract(), generation())
	assert.Equal(t, domain.ReasonPrivacyPurposeBlocked, codeOf(t, err))
}

// NP-31 / INV-28. A workload may search, but never in its own right — a bare
// service identity with no named principal is the privilege escalation an
// AI/RAG path would otherwise give for free.
func TestCompile_RefusesBareWorkloadWithNoPrincipal(t *testing.T) {
	tc := Context{
		TenantID:   "tenant-a",
		WorkloadID: "workload-rag-1",
		Purpose:    "AI_ASSISTANT",
	}
	_, err := planner().Compile(context.Background(), Request{Scope: "obligation"}, tc, contract(), generation())
	assert.Equal(t, domain.ReasonPartitionNotAuthorized, codeOf(t, err))
}

func TestCompile_AllowsWorkloadActingForANamedPrincipal(t *testing.T) {
	tc := Context{
		TenantID:   "tenant-a",
		WorkloadID: "workload-rag-1",
		OnBehalfOf: "principal-1",
		Purpose:    "AI_ASSISTANT",
	}
	_, err := planner().Compile(context.Background(), Request{Scope: "obligation"}, tc, contract(), generation())
	require.NoError(t, err)
}

// ESR-002 / ESR-011.
func TestCompile_RefusesUnregisteredScopeAndInactiveGeneration(t *testing.T) {
	_, err := planner().Compile(context.Background(), Request{Scope: "nope"}, validContext(), nil, generation())
	assert.Equal(t, domain.ReasonScopeNotRegistered, codeOf(t, err))

	_, err = planner().Compile(context.Background(), Request{Scope: "obligation"}, validContext(), contract(), nil)
	assert.Equal(t, domain.ReasonGenerationNotActive, codeOf(t, err))
}

// ESR-006 / NP-09. Asking for a non-returnable field is REFUSED, not silently
// omitted — a caller that got a result without the field it asked for would
// reasonably conclude the field was empty.
func TestCompile_RefusesNonReturnableField(t *testing.T) {
	_, err := planner().Compile(context.Background(), Request{
		Scope:           "obligation",
		RequestedFields: []string{"obligation_code", "internal_risk_score"},
	}, validContext(), contract(), generation())
	assert.Equal(t, domain.ReasonFieldNotReturnable, codeOf(t, err))
}

// NP-53 and INV-09 together: a prohibited field is indistinguishable from a
// field that does not exist, from every angle a caller can probe.
func TestCompile_ProhibitedFieldIsIndistinguishableFromNonexistent(t *testing.T) {
	p := planner()
	base := validContext()

	prohibited := codeOf(t, mustErr(p.Compile(context.Background(), Request{
		Scope: "obligation", RequestedFields: []string{"api_secret"}}, base, contract(), generation())))
	nonexistent := codeOf(t, mustErr(p.Compile(context.Background(), Request{
		Scope: "obligation", RequestedFields: []string{"no_such_field_at_all"}}, base, contract(), generation())))
	assert.Equal(t, nonexistent, prohibited)

	prohibitedSort := codeOf(t, mustErr(p.Compile(context.Background(), Request{
		Scope: "obligation", Sort: []SortRequest{{Field: "api_secret"}}}, base, contract(), generation())))
	nonexistentSort := codeOf(t, mustErr(p.Compile(context.Background(), Request{
		Scope: "obligation", Sort: []SortRequest{{Field: "no_such_field_at_all"}}}, base, contract(), generation())))
	assert.Equal(t, nonexistentSort, prohibitedSort)

	prohibitedFilter := codeOf(t, mustErr(p.Compile(context.Background(), Request{
		Scope: "obligation", Filters: map[string]string{"api_secret": "x"}}, base, contract(), generation())))
	assert.Equal(t, domain.ReasonFieldNotSearchable, prohibitedFilter)
}

// NP-53. A field that exists and is returnable but is NOT sortable is still
// refused, and with the same code as a field that does not exist.
func TestCompile_RefusesUnregisteredSortKey(t *testing.T) {
	_, err := planner().Compile(context.Background(), Request{
		Scope: "obligation", Sort: []SortRequest{{Field: "obligation_code", Desc: true}},
	}, validContext(), contract(), generation())
	// obligation_code is searchable and returnable but not sortable.
	assert.Equal(t, domain.ReasonFieldNotSearchable, codeOf(t, err))
}

// ESR-003 / §6.2 / NP-22. Wildcards, regex and engine internals are refused
// rather than passed through as literal text.
func TestCompile_RefusesForbiddenOperators(t *testing.T) {
	for _, text := range []string{"GST*", "GS?", "GST~2", "/GST.*/"} {
		t.Run(text, func(t *testing.T) {
			_, err := planner().Compile(context.Background(), Request{Scope: "obligation", Text: text},
				validContext(), contract(), generation())
			assert.Equal(t, domain.ReasonQueryOperatorForbidden, codeOf(t, err))
		})
	}
}

// NP-54. A one-character query enumerates rather than searches.
func TestCompile_RefusesQueryShorterThanMinimum(t *testing.T) {
	_, err := planner().Compile(context.Background(), Request{Scope: "obligation", Text: "A"},
		validContext(), contract(), generation())
	assert.Equal(t, domain.ReasonQueryComplexityExceeded, codeOf(t, err))
}

// §6.2's Unicode rule. A decomposed form must be measured the same as a
// composed one, or the length minimum above can be walked around with a
// combining mark.
func TestCompile_NormalisesUnicodeBeforeLengthCheck(t *testing.T) {
	// "e" + combining acute is two runes but one character after NFKC.
	_, err := planner().Compile(context.Background(), Request{Scope: "obligation", Text: "é"},
		validContext(), contract(), generation())
	assert.Equal(t, domain.ReasonQueryComplexityExceeded, codeOf(t, err),
		"a decomposed single character must not pass the minimum-length check")
}

func TestCompile_RefusesControlCharacters(t *testing.T) {
	_, err := planner().Compile(context.Background(), Request{Scope: "obligation", Text: "GST\x00filing"},
		validContext(), contract(), generation())
	assert.Equal(t, domain.ReasonQueryOperatorForbidden, codeOf(t, err))
}

// ESR-015 / NP-21.
func TestCompile_RefusesOversizedPage(t *testing.T) {
	_, err := planner().Compile(context.Background(), Request{Scope: "obligation", Size: 5000},
		validContext(), contract(), generation())
	assert.Equal(t, domain.ReasonResultWindowExceeded, codeOf(t, err))
}

// ESR-004. Facets are the expensive thing, and the score weights them so.
func TestCompile_RefusesPlanOverComplexityBudget(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxComplexityScore = 15
	p := NewPlanner(limits, &fakeStore{}, testKey)

	_, err := p.Compile(context.Background(), Request{
		Scope: "obligation", Text: "GST",
		Facets: []string{"obligation_status", "obligation_status", "obligation_status"},
	}, validContext(), contract(), generation())
	assert.Equal(t, domain.ReasonQueryComplexityExceeded, codeOf(t, err))
}

// INV-04 / NP-03. The mandatory filters are present and the type has no way
// to express their negation — a user filter lands in a separate slice that is
// ANDed alongside, never ORed with, and never subtracted from.
func TestCompile_UserFiltersCanOnlyNarrow(t *testing.T) {
	plan, err := planner().Compile(context.Background(), Request{
		Scope:   "obligation",
		Filters: map[string]string{"obligation_status": "OPEN"},
	}, validContext(), contract(), generation())
	require.NoError(t, err)

	assert.NotEmpty(t, plan.Execution.MandatoryFilters)
	assert.Len(t, plan.Execution.UserFilters, 1)

	compiled := mustJSON(t, plan.Execution)
	assert.Contains(t, compiled, "tenant-a")
	// The only must_not the plan carries is the tombstone exclusion; a user
	// filter never reaches it.
	require.Len(t, plan.Execution.MandatoryMustNot, 1)
	assert.Equal(t, "tombstoned", plan.Execution.MandatoryMustNot[0].Field)
}

// INV-18's query-side half: a tombstoned projection is never a candidate.
func TestCompile_AlwaysExcludesTombstonedDocuments(t *testing.T) {
	plan, err := planner().Compile(context.Background(), Request{Scope: "obligation"}, validContext(), contract(), generation())
	require.NoError(t, err)
	require.Len(t, plan.Execution.MandatoryMustNot, 1)
	assert.Equal(t, "tombstoned", plan.Execution.MandatoryMustNot[0].Field)
	assert.Equal(t, []string{"true"}, plan.Execution.MandatoryMustNot[0].Values)
}

// NP-16. Residency comes from trusted context and narrows the partition set.
func TestCompile_AppliesResidencyFilterFromContext(t *testing.T) {
	plan, err := planner().Compile(context.Background(), Request{Scope: "obligation"}, validContext(), contract(), generation())
	require.NoError(t, err)

	var residency *searchclient.TermFilter
	for i := range plan.Execution.MandatoryFilters {
		if plan.Execution.MandatoryFilters[i].Field == "residency_region" {
			residency = &plan.Execution.MandatoryFilters[i]
		}
	}
	require.NotNil(t, residency)
	assert.Contains(t, residency.Values, "EU")
	assert.Contains(t, residency.Values, "GLOBAL", "shared reference data stays reachable")
}

// §6.1's deny-by-default projection: with nothing requested, only fields the
// contract marked returnable come back, plus governance lineage.
func TestCompile_DefaultProjectionIsReturnableFieldsOnly(t *testing.T) {
	plan, err := planner().Compile(context.Background(), Request{Scope: "obligation"}, validContext(), contract(), generation())
	require.NoError(t, err)

	includes := strings.Join(plan.Execution.SourceIncludes, ",")
	assert.Contains(t, includes, "obligation_code")
	assert.Contains(t, includes, "tenant_id")
	assert.NotContains(t, includes, "internal_risk_score")
	assert.NotContains(t, includes, "api_secret")
}

// INV-13 / §7.2. A snippet needs the field to be snippet-allowed AND
// returnable AND searchable; a field that is only matchable contributes to
// ranking without contributing a fragment.
func TestCompile_SnippetsOnlyFromSnippetAllowedFields(t *testing.T) {
	plan, err := planner().Compile(context.Background(), Request{
		Scope: "obligation", Text: "GST", IncludeSnippets: true,
	}, validContext(), contract(), generation())
	require.NoError(t, err)

	assert.Equal(t, []string{"obligation_code"}, plan.Execution.HighlightFields)
	assert.True(t, plan.SnippetFields["obligation_code"])
	assert.False(t, plan.SnippetFields["internal_risk_score"])
}

// A TEXT field filtered on must use its keyword sub-field, or the "filter"
// matches a token rather than the value.
func TestCompile_TextFilterUsesKeywordSubfield(t *testing.T) {
	plan, err := planner().Compile(context.Background(), Request{
		Scope: "obligation", Filters: map[string]string{"obligation_code": "GST-Q4-2026"},
	}, validContext(), contract(), generation())
	require.NoError(t, err)
	require.Len(t, plan.Execution.UserFilters, 1)
	assert.Equal(t, "obligation_code.keyword", plan.Execution.UserFilters[0].Field)
}

// A scope with no searchable field compiles to match_none, never to "*".
// Falling back to a wildcard would search fields the contract deliberately
// left unsearchable.
func TestCompile_TextOnScopeWithNoSearchableFieldIsRefused(t *testing.T) {
	c := contract()
	for i := range c.Fields {
		c.Fields[i].Searchable = false
	}
	_, err := planner().Compile(context.Background(), Request{Scope: "obligation", Text: "anything"},
		validContext(), c, generation())
	assert.Equal(t, domain.ReasonFieldNotSearchable, codeOf(t, err))
}

// The plan digest changes when the filters change, which is what binds a
// cursor to the query that produced it (NP-56).
func TestCompile_PlanDigestChangesWithFilters(t *testing.T) {
	p := planner()
	a, err := p.Compile(context.Background(), Request{Scope: "obligation", Text: "GST"}, validContext(), contract(), generation())
	require.NoError(t, err)
	b, err := p.Compile(context.Background(), Request{Scope: "obligation", Text: "GST",
		Filters: map[string]string{"obligation_status": "OPEN"}}, validContext(), contract(), generation())
	require.NoError(t, err)
	assert.NotEqual(t, a.PlanDigest, b.PlanDigest)
}

// INV-17. The evidence carries a digest of the query, never the query.
func TestCompile_QueryDigestIsNotTheQueryText(t *testing.T) {
	plan, err := planner().Compile(context.Background(), Request{
		Scope: "obligation", Text: "termination of Jane Doe misconduct",
	}, validContext(), contract(), generation())
	require.NoError(t, err)
	assert.NotContains(t, plan.QueryDigest, "Jane")
	assert.NotContains(t, plan.QueryDigest, "misconduct")
	assert.Len(t, plan.QueryDigest, 64)
}

// NP-57. A cursor edited to name a different tenant does not verify.
func TestCompile_RejectsCursorForAnotherTenant(t *testing.T) {
	p := planner()
	plan, err := p.Compile(context.Background(), Request{Scope: "obligation", Text: "GST"}, validContext(), contract(), generation())
	require.NoError(t, err)

	// A cursor legitimately minted for another tenant, with a valid signature.
	foreign, err := EncodeCursor(testKey, Cursor{
		TenantID: "tenant-b", Scope: "obligation", PlanDigest: plan.PlanDigest,
		After: []any{1.0}, Page: 1,
	})
	require.NoError(t, err)

	_, err = p.Compile(context.Background(), Request{Scope: "obligation", Text: "GST", Cursor: foreign},
		validContext(), contract(), generation())
	assert.Equal(t, domain.ReasonResultWindowExceeded, codeOf(t, err))
}

// NP-56. A cursor from a different filter set does not carry over.
func TestCompile_RejectsCursorFromADifferentPlan(t *testing.T) {
	p := planner()
	narrow, err := p.Compile(context.Background(), Request{Scope: "obligation", Text: "GST",
		Filters: map[string]string{"obligation_status": "OPEN"}}, validContext(), contract(), generation())
	require.NoError(t, err)
	cursor, err := p.NextCursor(narrow, "tenant-a", []any{1.0})
	require.NoError(t, err)

	_, err = p.Compile(context.Background(), Request{Scope: "obligation", Text: "GST", Cursor: cursor},
		validContext(), contract(), generation())
	assert.Equal(t, domain.ReasonResultWindowExceeded, codeOf(t, err),
		"a cursor must not survive a change to the query it paged")
}

// A cursor for the same plan and tenant DOES carry over, and lands as
// search_after rather than as an offset.
func TestCompile_AcceptsItsOwnCursor(t *testing.T) {
	p := planner()
	plan, err := p.Compile(context.Background(), Request{Scope: "obligation", Text: "GST"}, validContext(), contract(), generation())
	require.NoError(t, err)
	cursor, err := p.NextCursor(plan, "tenant-a", []any{2.5, "ob-9"})
	require.NoError(t, err)
	require.NotEmpty(t, cursor)

	next, err := p.Compile(context.Background(), Request{Scope: "obligation", Text: "GST", Cursor: cursor},
		validContext(), contract(), generation())
	require.NoError(t, err)
	assert.Equal(t, []any{2.5, "ob-9"}, next.Execution.SearchAfter)
	assert.Equal(t, 1, next.Page)
}

// NP-21's cursor half: the walk itself is bounded.
func TestCompile_RefusesCursorPastThePageLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPages = 3
	p := NewPlanner(limits, &fakeStore{}, testKey)

	plan, err := p.Compile(context.Background(), Request{Scope: "obligation", Text: "GST"}, validContext(), contract(), generation())
	require.NoError(t, err)
	exhausted, err := EncodeCursor(testKey, Cursor{
		TenantID: "tenant-a", Scope: "obligation", PlanDigest: plan.PlanDigest,
		After: []any{1.0}, Page: 3,
	})
	require.NoError(t, err)

	_, err = p.Compile(context.Background(), Request{Scope: "obligation", Text: "GST", Cursor: exhausted},
		validContext(), contract(), generation())
	assert.Equal(t, domain.ReasonResultWindowExceeded, codeOf(t, err))
}

func mustErr(_ *Plan, err error) error { return err }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// The word-boundary rule on engine identifiers, and the reason it exists.
//
// "script" as a substring appears inside description, transcript,
// prescription and subscription — four ordinary things to search a business
// corpus for. An earlier version of checkOperators used strings.Contains and
// would have refused every one of them as an attempted engine script, with a
// message the caller could neither understand nor work around.
func TestCompile_EngineIdentifiersMatchOnWordBoundariesOnly(t *testing.T) {
	allowed := []string{
		"invoice description",
		"transcript of the hearing",
		"prescription reimbursement",
		"subscription renewal",
		"description",
	}
	for _, text := range allowed {
		t.Run("allowed/"+text, func(t *testing.T) {
			_, err := planner().Compile(context.Background(), Request{Scope: "obligation", Text: text},
				validContext(), contract(), generation())
			require.NoError(t, err, "a legitimate business term must not be refused")
		})
	}

	refused := []string{"script", "SCRIPT", "a script here", "painless", "_index", "_source", "_score"}
	for _, text := range refused {
		t.Run("refused/"+text, func(t *testing.T) {
			_, err := planner().Compile(context.Background(), Request{Scope: "obligation", Text: text},
				validContext(), contract(), generation())
			assert.Equal(t, domain.ReasonQueryOperatorForbidden, codeOf(t, err))
		})
	}
}
