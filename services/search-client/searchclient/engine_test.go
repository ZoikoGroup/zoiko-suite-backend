// Unit tests for the parts of the engine that are pure logic: how a contract
// field becomes an OpenSearch mapping property, and how a compiled plan
// becomes a request body.
//
// No cluster required. The integration tests in client_test.go cover the
// round trip; these cover the translation, which is where the security
// properties actually live — a mapping that quietly analyzed tenant_id, or a
// plan body that lost its filter clause, would pass a round-trip test and
// fail an audit.
package searchclient

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerationIndexAndAlias(t *testing.T) {
	assert.Equal(t, "zoiko-obligations-gabc123", GenerationIndex(IndexObligations, "ABC123"))
	assert.Equal(t, "zoiko-obligations", Alias(IndexObligations))
}

// The governance fields are ALL keyword where they are filtered on. A tenant
// filter that went through an analyzer would match on a token, and a token
// match is not a filter.
func TestGovernanceProperties_TenantIsExactMatch(t *testing.T) {
	props := GovernanceProperties()
	for _, field := range []string{"tenant_id", "legal_entity_id", "residency_region", "acl_refs"} {
		p, ok := props[field].(map[string]any)
		require.True(t, ok, "field %s", field)
		assert.Equal(t, "keyword", p["type"], "%s must be exact-match, never analyzed", field)
	}
}

func TestReservedFields_CoversEveryGovernanceProperty(t *testing.T) {
	assert.Len(t, ReservedFields(), len(GovernanceProperties()))
	assert.Contains(t, ReservedFields(), "tenant_id")
	assert.Contains(t, ReservedFields(), "restriction_epoch")
}

// INV-08's mapping half: a registered-but-unsearchable text field is stored
// but not matchable, so it cannot leak through relevance instead of _source.
func TestFieldMapping_UnsearchableTextIsNotIndexed(t *testing.T) {
	p := FieldMapping{Name: "notes", Type: "TEXT", Searchable: false}.property()
	assert.Equal(t, "text", p["type"])
	assert.Equal(t, false, p["index"])
}

func TestFieldMapping_SearchableTextCarriesItsAnalyzer(t *testing.T) {
	p := FieldMapping{Name: "notes", Type: "TEXT", Searchable: true, Analyzer: "english"}.property()
	assert.Equal(t, "english", p["analyzer"])
	assert.NotContains(t, p, "index")
}

// A text field cannot be sorted or aggregated directly; the keyword sub-field
// is what makes that legal, and registering it here keeps the contract the
// only place field capabilities are declared.
func TestFieldMapping_SortableTextGetsAKeywordSubfield(t *testing.T) {
	p := FieldMapping{Name: "code", Type: "TEXT", Searchable: true, Sortable: true}.property()
	fields, ok := p["fields"].(map[string]any)
	require.True(t, ok, "a sortable text field needs a keyword sub-field")
	kw := fields["keyword"].(map[string]any)
	assert.Equal(t, "keyword", kw["type"])
}

func TestFieldMapping_PlainTextNeedsNoSubfield(t *testing.T) {
	p := FieldMapping{Name: "notes", Type: "TEXT", Searchable: true}.property()
	assert.NotContains(t, p, "fields")
}

// An unknown declared type falls back to KEYWORD, not to text: a field whose
// type nobody declared must not become full-text searchable by accident.
func TestFieldMapping_UnknownTypeDefaultsToKeyword(t *testing.T) {
	assert.Equal(t, "keyword", FieldMapping{Name: "x", Type: "WHATEVER"}.osType())
	assert.Equal(t, "keyword", FieldMapping{Name: "x", Type: ""}.osType())
}

func TestFieldMapping_TypeTranslation(t *testing.T) {
	cases := map[string]string{
		"TEXT": "text", "DATE": "date", "LONG": "long", "INTEGER": "long",
		"DOUBLE": "double", "DECIMAL": "double", "BOOLEAN": "boolean", "KEYWORD": "keyword",
	}
	for declared, want := range cases {
		assert.Equal(t, want, FieldMapping{Type: declared}.osType(), declared)
	}
}

// INV-02. The governance fields are merged over the domain body, so a source
// field named tenant_id loses to the trusted value rather than replacing it.
func TestProjection_GovernanceFieldsOverrideTheBody(t *testing.T) {
	p := Projection{
		DocID: "d", TenantID: "trusted", SourceType: "obligation", SourceID: "ob-1",
		Fields: map[string]any{"tenant_id": "attacker", "obligation_code": "GST"},
	}
	doc := p.document()
	assert.Equal(t, "trusted", doc["tenant_id"])
	assert.Equal(t, "GST", doc["obligation_code"])
}

// The document id is tenant-first: an id beginning with the source id would
// let a mis-set tenant overwrite another tenant's document at the same id.
func TestProjectionDocID_IsTenantFirst(t *testing.T) {
	id := ProjectionDocID("tenant-a", "obligation", "ob-1")
	assert.Equal(t, "tenant-a:obligation:ob-1", id)
	assert.NotEqual(t, id, ProjectionDocID("tenant-b", "obligation", "ob-1"))
}

func TestProjection_TombstoneCarriesItsMarkers(t *testing.T) {
	doc := Projection{
		DocID: "d", TenantID: "t", SourceType: "obligation", SourceID: "ob-1",
		Tombstoned: true, TombstoneReason: "obligation.deleted", TombstoneSource: "evt-1",
		RestrictionEpoch: 42,
	}.document()

	assert.Equal(t, true, doc["tombstoned"])
	assert.Equal(t, "obligation.deleted", doc["tombstone_reason"])
	assert.Equal(t, "evt-1", doc["tombstone_source_event"])
	assert.Equal(t, int64(42), doc["restriction_epoch"])
}

// ── plan compilation ─────────────────────────────────────────────────────────

func compiled(t *testing.T, p ExecutionPlan) map[string]any {
	t.Helper()
	b, err := json.Marshal(p.compile())
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

// INV-04. The mandatory filters land in the bool query's `filter` clause, and
// there is no input on this type that could negate them.
func TestCompile_MandatoryFiltersLandInTheFilterClause(t *testing.T) {
	body := compiled(t, ExecutionPlan{
		MandatoryFilters: []TermFilter{{Field: "tenant_id", Values: []string{"tenant-a"}}},
		MandatoryMustNot: []TermFilter{{Field: "tombstoned", Values: []string{"true"}}},
		Size:             10,
	})

	boolQuery := body["query"].(map[string]any)["bool"].(map[string]any)
	filters := boolQuery["filter"].([]any)
	require.Len(t, filters, 1)
	assert.Contains(t, mustJSON(t, filters[0]), "tenant-a")

	mustNot := boolQuery["must_not"].([]any)
	require.Len(t, mustNot, 1)
	assert.Contains(t, mustJSON(t, mustNot[0]), "tombstoned")
}

// A user filter is ANDed alongside the mandatory ones, never ORed with them:
// it can only ever narrow.
func TestCompile_UserFiltersAreAndedAlongsideMandatory(t *testing.T) {
	body := compiled(t, ExecutionPlan{
		MandatoryFilters: []TermFilter{{Field: "tenant_id", Values: []string{"tenant-a"}}},
		UserFilters:      []TermFilter{{Field: "status", Values: []string{"OPEN"}}},
		Size:             10,
	})

	filters := body["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]any)
	assert.Len(t, filters, 2, "both land in the same ANDed filter clause")
}

// INV-08. Text search only ever touches the registered searchable fields —
// never "*", which would search fields the contract left unsearchable.
func TestCompile_TextSearchesOnlyRegisteredFields(t *testing.T) {
	body := compiled(t, ExecutionPlan{
		Text: "GST", TextFields: []string{"obligation_code"}, Size: 10,
	})
	must := body["query"].(map[string]any)["bool"].(map[string]any)["must"].([]any)
	require.Len(t, must, 1)

	mm := must[0].(map[string]any)["multi_match"].(map[string]any)
	assert.Equal(t, []any{"obligation_code"}, mm["fields"])
	assert.NotContains(t, mustJSON(t, mm), `"*"`)
}

// A scope with no searchable field compiles to match_none, not a wildcard.
func TestCompile_TextWithNoSearchableFieldsIsMatchNone(t *testing.T) {
	body := compiled(t, ExecutionPlan{Text: "GST", TextFields: nil, Size: 10})
	must := body["query"].(map[string]any)["bool"].(map[string]any)["must"].([]any)
	require.Len(t, must, 1)
	assert.Contains(t, must[0].(map[string]any), "match_none")
}

// §6.1's deny-by-default projection: a plan that named no returnable fields
// returns governance lineage only, never the whole document.
func TestCompile_EmptySourceIncludesFallsBackToLineageOnly(t *testing.T) {
	body := compiled(t, ExecutionPlan{Size: 10})
	includes := body["_source"].(map[string]any)["includes"].([]any)
	assert.Contains(t, includes, "tenant_id")
	assert.Contains(t, includes, "source_id")
	assert.NotContains(t, includes, "*")
}

// Without a unique tie-break, search_after can skip or repeat documents whose
// sort values are equal — a cursor that silently loses rows.
func TestCompile_SortAlwaysEndsWithAUniqueTieBreak(t *testing.T) {
	body := compiled(t, ExecutionPlan{
		Sort: []SortKey{{Field: "due_date", Desc: true}}, Size: 10,
	})
	sorts := body["sort"].([]any)
	require.GreaterOrEqual(t, len(sorts), 3)
	last := sorts[len(sorts)-1].(map[string]any)
	assert.Contains(t, last, "source_id", "the final sort key must be unique per document")
}

// The highlighter is asked for private-use sentinels, not <em>. Returning the
// engine's default tags would mean shipping unescaped stored markup wrapped
// in real HTML — NP-24 in one step.
func TestCompile_HighlightUsesPrivateUseSentinelsNotHTML(t *testing.T) {
	body := compiled(t, ExecutionPlan{
		Text: "GST", TextFields: []string{"code"}, HighlightFields: []string{"code"}, Size: 10,
	})
	hl := body["highlight"].(map[string]any)
	assert.Equal(t, []any{HighlightOpen}, hl["pre_tags"])
	assert.Equal(t, []any{HighlightClose}, hl["post_tags"])
	assert.NotContains(t, mustJSON(t, hl), "<em>")
	assert.Equal(t, true, hl["require_field_match"],
		"a highlighter running its own query can surface a span the query never matched")
}

// No highlight block at all without query text — there is nothing to mark.
func TestCompile_NoHighlightWithoutText(t *testing.T) {
	body := compiled(t, ExecutionPlan{HighlightFields: []string{"code"}, Size: 10})
	assert.NotContains(t, body, "highlight")
}

// §7.3 / NP-08. min_doc_count keeps suppressed buckets off the wire entirely
// rather than relying on this process to drop them after transmission.
func TestCompile_FacetsCarryTheMinimumCellRule(t *testing.T) {
	body := compiled(t, ExecutionPlan{Facets: []string{"status"}, FacetMinCount: 5, Size: 10})
	agg := body["aggs"].(map[string]any)["status"].(map[string]any)["terms"].(map[string]any)
	assert.Equal(t, float64(5), agg["min_doc_count"])
}

// A min count of 0 or 1 is no suppression at all; the floor is enforced here
// as well as in config, because this is the last place before the wire.
func TestCompile_FacetMinCountNeverDropsBelowOne(t *testing.T) {
	body := compiled(t, ExecutionPlan{Facets: []string{"status"}, FacetMinCount: 0, Size: 10})
	agg := body["aggs"].(map[string]any)["status"].(map[string]any)["terms"].(map[string]any)
	assert.Equal(t, float64(1), agg["min_doc_count"])
}

// Without track_total_hits OpenSearch stops counting at 10 000 and reports
// {"value":10000,"relation":"gte"} — a completeness check that silently
// plateaus reads as a pass.
func TestCompile_AlwaysTracksTotalHits(t *testing.T) {
	body := compiled(t, ExecutionPlan{Size: 10})
	assert.Equal(t, true, body["track_total_hits"])
}

func TestCompile_DefaultsPageSize(t *testing.T) {
	assert.Equal(t, float64(20), compiled(t, ExecutionPlan{})["size"])
}

func TestCompile_TimeoutIsRenderedInMilliseconds(t *testing.T) {
	body := compiled(t, ExecutionPlan{Size: 10, Timeout: 250 * time.Millisecond})
	assert.Equal(t, "250ms", body["timeout"])
}

func TestTermFilter_SingleValueUsesTermAndMultiUsesTerms(t *testing.T) {
	single := TermFilter{Field: "tenant_id", Values: []string{"a"}}.clause()
	assert.Contains(t, single, "term")

	multi := TermFilter{Field: "acl_refs", Values: []string{"a", "b"}}.clause()
	assert.Contains(t, multi, "terms")

	assert.Nil(t, TermFilter{Field: "x"}.clause(), "an empty filter compiles to nothing")
	assert.Nil(t, TermFilter{Values: []string{"a"}}.clause())
}

// The second min-cell pass. Belt and braces on purpose: an engine upgrade
// that changed min_doc_count semantics would otherwise turn a security rule
// off silently.
func TestDecodeFacets_AppliesTheMinimumCellRuleAgain(t *testing.T) {
	raw := json.RawMessage(`{"status":{"buckets":[
		{"key":"OPEN","doc_count":9},
		{"key":"PRIVILEGED","doc_count":1}
	]}}`)
	out := decodeFacets(raw, []string{"status"}, 2)

	require.Len(t, out["status"], 1)
	assert.Equal(t, "OPEN", out["status"][0].Value)
	for _, b := range out["status"] {
		assert.NotEqual(t, "PRIVILEGED", b.Value,
			"a bucket of one can reveal a single privileged record")
	}
}

func TestDecodeFacets_SortsByCountDescending(t *testing.T) {
	raw := json.RawMessage(`{"status":{"buckets":[
		{"key":"A","doc_count":3},{"key":"B","doc_count":9},{"key":"C","doc_count":5}
	]}}`)
	out := decodeFacets(raw, []string{"status"}, 2)
	require.Len(t, out["status"], 3)
	assert.Equal(t, "B", out["status"][0].Value)
	assert.Equal(t, "C", out["status"][1].Value)
}

func TestStripHighlightMarkers(t *testing.T) {
	assert.Equal(t, "GST filing",
		StripHighlightMarkers(HighlightOpen+"GST"+HighlightClose+" filing"))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// The exact strings opensearch-go produces for a missing index.
//
// `status: [404 Not Found]` is the real one, and matching only on
// `status: 404` meant EnsureGeneration treated "this index does not exist
// yet" — the ordinary case for every generation build — as a hard failure.
// indexExists now reads the response's status code, which is exact; this
// pins the text fallback for the paths where no response is available.
func TestIsMissing_RecognisesTheEnginesActualNotFoundText(t *testing.T) {
	missing := []string{
		"searchclient: indexExists foo: status: [404 Not Found]",
		"status: 404",
		"index_not_found_exception",
		"alias_not_found_exception",
		"Status: [404 Not Found]",
	}
	for _, s := range missing {
		assert.True(t, isMissing(errString(s)), "should be missing: %q", s)
	}

	present := []string{
		"status: [500 Internal Server Error]",
		"connection refused",
		"searchclient: cluster health failed",
	}
	for _, s := range present {
		assert.False(t, isMissing(errString(s)), "should NOT be missing: %q", s)
	}
	assert.False(t, isMissing(nil))
}

type errString string

func (e errString) Error() string { return string(e) }
