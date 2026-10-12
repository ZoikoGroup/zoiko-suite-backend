package searchclient

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// §10.1 / NP-33. The vector field maps to a Lucene HNSW knn_vector — the
// engine whose k-NN honours a filter during the graph walk — at the width the
// contract pinned, and turns on index.knn.
func TestGenerationBody_VectorFieldIsLuceneKnnAtThePinnedWidth(t *testing.T) {
	body, err := generationBody([]FieldMapping{
		{Name: "title", Type: "TEXT", Searchable: true},
		VectorMapping(384, "cosinesimil"),
	})
	require.NoError(t, err)

	settings := body["settings"].(map[string]any)["index"].(map[string]any)
	assert.Equal(t, true, settings["knn"])

	props := body["mappings"].(map[string]any)["properties"].(map[string]any)
	vec := props[EmbeddingVectorField].(map[string]any)
	assert.Equal(t, "knn_vector", vec["type"])
	assert.Equal(t, 384, vec["dimension"])
	method := vec["method"].(map[string]any)
	assert.Equal(t, "lucene", method["engine"])
	assert.Equal(t, "cosinesimil", method["space_type"])
}

// A lexical-only generation carries no k-NN setting and no vector property.
func TestGenerationBody_LexicalGenerationHasNoVector(t *testing.T) {
	body, err := generationBody([]FieldMapping{{Name: "title", Type: "TEXT", Searchable: true}})
	require.NoError(t, err)
	settings := body["settings"].(map[string]any)["index"].(map[string]any)
	assert.NotContains(t, settings, "knn")
	props := body["mappings"].(map[string]any)["properties"].(map[string]any)
	assert.NotContains(t, props, EmbeddingVectorField)
	assert.Equal(t, "strict", body["mappings"].(map[string]any)["dynamic"])
}

// A contract cannot register the vector field as an ordinary field, and a
// vector cannot be declared under any other name or with no width.
func TestGenerationBody_RefusesContractSuppliedVectors(t *testing.T) {
	_, err := generationBody([]FieldMapping{{Name: EmbeddingVectorField, Type: "KEYWORD"}})
	assert.Error(t, err)
	_, err = generationBody([]FieldMapping{{Name: "my_vector", Type: "VECTOR", Dimensions: 8}})
	assert.Error(t, err)
	_, err = generationBody([]FieldMapping{VectorMapping(0, "")})
	assert.Error(t, err)
	_, err = generationBody([]FieldMapping{{Name: "embedding_model", Type: "KEYWORD"}})
	assert.Error(t, err)
}

// INV-26: the vector rides in the governed document, with its model pin.
func TestProjection_CarriesItsVectorAndModelPin(t *testing.T) {
	doc := Projection{
		TenantID: "t", SourceType: "doc", SourceID: "1",
		Embedding: []float32{0.1, 0.2}, EmbeddingModel: "m@1",
	}.document()
	assert.Equal(t, []float32{0.1, 0.2}, doc[EmbeddingVectorField])
	assert.Equal(t, "m@1", doc["embedding_model"])
	assert.Equal(t, "t", doc["tenant_id"])
}

// NP-34's worse twin: a tombstone never keeps a vector, even if one was set.
func TestProjection_TombstoneNeverCarriesAVector(t *testing.T) {
	doc := Projection{
		TenantID: "t", SourceType: "doc", SourceID: "1", Tombstoned: true,
		Embedding: []float32{0.1, 0.2}, EmbeddingModel: "m@1",
	}.document()
	assert.NotContains(t, doc, EmbeddingVectorField)
	assert.NotContains(t, doc, "embedding_model")
}

// NP-33. The eligibility filters are inside the knn clause (applied during the
// graph walk) AND in the outer bool — so the K neighbours are chosen only from
// the caller's tenant, not chosen globally and trimmed afterwards.
func TestCompile_SemanticFiltersInsideTheKnnWalk(t *testing.T) {
	body := compiled(t, ExecutionPlan{
		MandatoryFilters: []TermFilter{
			{Field: "tenant_id", Values: []string{"tenant-a"}},
			{Field: "embedding_model", Values: []string{"m@1"}},
		},
		MandatoryMustNot: []TermFilter{{Field: "tombstoned", Values: []string{"true"}}},
		Vector:           []float32{0.5, 0.5},
		K:                10,
		Size:             5,
	})

	boolQ := body["query"].(map[string]any)["bool"].(map[string]any)
	outer, _ := json.Marshal(boolQ["filter"])
	assert.Contains(t, string(outer), "tenant-a")

	must := boolQ["must"].([]any)
	require.Len(t, must, 1)
	knn := must[0].(map[string]any)["knn"].(map[string]any)[EmbeddingVectorField].(map[string]any)
	assert.Equal(t, float64(10), knn["k"])
	inner, _ := json.Marshal(knn["filter"])
	assert.Contains(t, string(inner), "tenant-a", "tenant filter must apply during the ANN walk")
	assert.Contains(t, string(inner), "m@1", "model pin must apply during the ANN walk")
	assert.Contains(t, string(inner), "tombstoned", "tombstones must be excluded during the ANN walk")

	assert.Equal(t, float64(5), body["size"])
	assert.NotContains(t, body, "aggs")
	assert.NotContains(t, body, "highlight")
	assert.NotContains(t, body, "search_after")
}

// Hybrid adds the lexical clause as an alternative, bounded by the same outer
// filter; pure semantic ignores the text for matching.
func TestCompile_HybridAddsTheLexicalClause(t *testing.T) {
	plan := ExecutionPlan{
		MandatoryFilters: []TermFilter{{Field: "tenant_id", Values: []string{"t"}}},
		Vector:           []float32{1},
		K:                5,
		Text:             "late filing",
		TextFields:       []string{"title"},
	}
	semantic := compiled(t, plan)
	assert.NotContains(t, semantic["query"].(map[string]any)["bool"], "should")

	plan.Hybrid = true
	hybrid := compiled(t, plan)
	boolQ := hybrid["query"].(map[string]any)["bool"].(map[string]any)
	should := boolQ["should"].([]any)
	require.Len(t, should, 2)
	assert.Equal(t, float64(1), boolQ["minimum_should_match"])
	assert.Contains(t, boolQ, "filter")
}
