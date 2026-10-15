package query

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/search-indexer-svc/internal/domain"
)

func semanticContract() *domain.IndexContract {
	c := contract()
	c.Embedding = &domain.EmbeddingSpec{Model: "m", ModelVersion: "1", Dimensions: 4,
		SourceFields: []string{"obligation_code"}, Preprocessing: "nfkc-ws-v1", Similarity: "cosinesimil"}
	return c
}

func semanticReq() SemanticRequest {
	return SemanticRequest{Scope: "obligation", Text: "which filings are overdue?", Size: 10}
}

// A natural-language question is not lexical syntax: '?' is fine in pure
// semantic mode, and the pinned model is a mandatory filter (NP-35).
func TestPlanSemantic_PinsTheModelAndAcceptsQuestions(t *testing.T) {
	plan, err := planner().PlanSemantic(semanticReq(), validContext(), semanticContract(), generation())
	require.NoError(t, err)

	assert.True(t, plan.Semantic)
	assert.Equal(t, "m@1", plan.PinnedModel)
	assert.Equal(t, 10, plan.Execution.K)
	assert.False(t, plan.Execution.Hybrid)
	assert.Nil(t, plan.Execution.Vector, "the planner never produces the vector; it is embedded after the plan is allowed")

	var model, tenant bool
	for _, f := range plan.Execution.MandatoryFilters {
		if f.Field == "embedding_model" && f.Values[0] == "m@1" {
			model = true
		}
		if f.Field == "tenant_id" && f.Values[0] == "tenant-a" {
			tenant = true
		}
	}
	assert.True(t, model, "only vectors from the pinned model are candidates")
	assert.True(t, tenant, "the same trusted tenant filter as lexical search")
}

func TestPlanSemantic_LexicalScopeIsNotRegistered(t *testing.T) {
	_, err := planner().PlanSemantic(semanticReq(), validContext(), contract(), generation())
	assert.Equal(t, domain.ReasonScopeNotRegistered, codeOf(t, err))
}

func TestPlanSemantic_ExpectedModelMismatchIsESR019(t *testing.T) {
	req := semanticReq()
	req.Model = "m@2"
	_, err := planner().PlanSemantic(req, validContext(), semanticContract(), generation())
	assert.Equal(t, domain.ReasonSemanticModelMismatch, codeOf(t, err))
}

func TestPlanSemantic_NoCursorPastK(t *testing.T) {
	req := semanticReq()
	req.Cursor = "anything"
	_, err := planner().PlanSemantic(req, validContext(), semanticContract(), generation())
	assert.Equal(t, domain.ReasonResultWindowExceeded, codeOf(t, err))

	req = semanticReq()
	req.K = 10_000
	_, err = planner().PlanSemantic(req, validContext(), semanticContract(), generation())
	assert.Equal(t, domain.ReasonResultWindowExceeded, codeOf(t, err))
}

// The shared §1.3 ordering applies: no purpose, no semantic search either.
func TestPlanSemantic_SamePolicyEnvelopeAsLexical(t *testing.T) {
	tc := validContext()
	tc.Purpose = ""
	_, err := planner().PlanSemantic(semanticReq(), tc, semanticContract(), generation())
	assert.Equal(t, domain.ReasonPrivacyPurposeBlocked, codeOf(t, err))

	req := semanticReq()
	req.RequestedFields = []string{"internal_risk_score"}
	_, err = planner().PlanSemantic(req, validContext(), semanticContract(), generation())
	assert.Equal(t, domain.ReasonFieldNotReturnable, codeOf(t, err))
}

func TestPlanSemantic_BoundsTheTextSentToTheProvider(t *testing.T) {
	req := semanticReq()
	req.Text = strings.Repeat("a", maxSemanticQueryRunes+1)
	_, err := planner().PlanSemantic(req, validContext(), semanticContract(), generation())
	assert.Equal(t, domain.ReasonQueryComplexityExceeded, codeOf(t, err))

	req.Text = "   "
	_, err = planner().PlanSemantic(req, validContext(), semanticContract(), generation())
	assert.Error(t, err)
}

// Hybrid keeps the lexical half under lexical rules.
func TestPlanSemantic_HybridAppliesLexicalOperatorRules(t *testing.T) {
	req := semanticReq()
	req.Mode = ModeHybrid
	req.Text = "late fil*"
	_, err := planner().PlanSemantic(req, validContext(), semanticContract(), generation())
	assert.Equal(t, domain.ReasonQueryOperatorForbidden, codeOf(t, err))

	req.Text = "late filing"
	plan, err := planner().PlanSemantic(req, validContext(), semanticContract(), generation())
	require.NoError(t, err)
	assert.True(t, plan.Execution.Hybrid)
}

// Every returnable field carries its source path for hydration.
func TestCompile_CarriesSourcePathsForReturnableFields(t *testing.T) {
	c := contract()
	c.Fields[0].SourcePath = "code.value"
	plan, err := planner().Compile(Request{Scope: "obligation"}, validContext(), c, generation())
	require.NoError(t, err)
	assert.Equal(t, "code.value", plan.SourcePaths["obligation_code"])
	assert.NotContains(t, plan.SourcePaths, "internal_risk_score", "non-returnable fields have no path to hydrate")
}
