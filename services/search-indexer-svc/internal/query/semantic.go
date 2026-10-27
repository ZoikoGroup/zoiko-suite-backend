package query

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
)

// SemanticRequest is POST /v1/search/semantic's body (§11.1: "semantic/hybrid
// retrieval for registered scopes; same policy envelope as lexical search;
// embedding model fixed by scope").
//
// Like Request, it has nowhere to put a tenant, an actor, an index name or a
// vector. A caller-supplied vector would let the caller choose what its query
// is "similar" to without the pinned model ever being consulted — and would
// make NP-35's pin check meaningless on the query side.
type SemanticRequest struct {
	Scope           string            `json:"scope"`
	Text            string            `json:"query"`
	Mode            string            `json:"mode,omitempty"`
	Filters         map[string]string `json:"filters,omitempty"`
	RequestedFields []string          `json:"requested_fields,omitempty"`
	Size            int               `json:"size,omitempty"`
	K               int               `json:"k,omitempty"`
	// Model is optional. A caller that states the model it expects (an AI
	// orchestrator pinned to one embedding space) gets ESR-019 when the
	// scope's pin differs, instead of results from a space it did not ask for.
	Model string `json:"model,omitempty"`
	// Cursor exists only to be refused with a reason code: a nearest-neighbour
	// page is one bounded page, and paging past K is not a thing ANN can do
	// honestly.
	Cursor string `json:"cursor,omitempty"`
}

const (
	ModeSemantic = "semantic"
	ModeHybrid   = "hybrid"
)

// maxSemanticQueryRunes bounds what is sent to the embedding provider. A query
// is embedded on every request, so an unbounded one is an unbounded cost the
// caller chooses (§6.2 execution-time budgets).
const maxSemanticQueryRunes = 2000

// PlanSemantic compiles a semantic or hybrid plan WITHOUT its query vector.
//
// Everything that can refuse the request happens here, before the text is sent
// anywhere: identity, purpose, scope, the model pin, filters, fields, window.
// Only a request that would be allowed to run is embedded — so a refused
// request never reaches the provider, and the provider never learns what an
// unauthorised caller wanted to look for. The handler embeds Execution.Text
// with the pinned model and sets Execution.Vector.
func (p *Planner) PlanSemantic(req SemanticRequest, tc Context, contract *domain.IndexContract, generation *domain.IndexGeneration) (*Plan, error) {
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = ModeSemantic
	}
	if mode != ModeSemantic && mode != ModeHybrid {
		return nil, refuse(domain.ReasonQueryOperatorForbidden,
			"mode must be %q or %q", ModeSemantic, ModeHybrid)
	}

	// The shared §1.3 ordering first — identity, purpose, scope, filters,
	// fields, window — through the same Compile the lexical path uses, so
	// the two cannot drift. Text is withheld from it in pure semantic mode:
	// the lexical operator rules (no '?', no '/') are about query syntax a
	// natural-language question does not have.
	base := Request{
		Scope:           req.Scope,
		Filters:         req.Filters,
		RequestedFields: req.RequestedFields,
		Size:            req.Size,
	}
	if mode == ModeHybrid {
		base.Text = req.Text
	}
	plan, err := p.Compile(base, tc, contract, generation)
	if err != nil {
		return nil, err
	}

	spec := contract.Embedding
	if spec == nil {
		// §11.1: semantic retrieval is for REGISTERED scopes. A lexical-only
		// scope has no embedding space to search, and answering with lexical
		// results would be a semantic claim nothing backs.
		return nil, refuse(domain.ReasonScopeNotRegistered,
			"scope %q is not registered for semantic retrieval (its contract pins no embedding model)",
			contract.ScopeName)
	}
	if req.Model != "" && req.Model != spec.PinnedModel() {
		return nil, refuse(domain.ReasonSemanticModelMismatch,
			"scope %q is pinned to %s; the request expects %s", contract.ScopeName, spec.PinnedModel(), req.Model)
	}
	if req.Cursor != "" {
		return nil, refuse(domain.ReasonResultWindowExceeded,
			"semantic retrieval returns one bounded page; there is no cursor past k")
	}

	text := strings.TrimSpace(norm.NFKC.String(req.Text))
	if text == "" {
		return nil, refuse(domain.ReasonFieldNotSearchable, "semantic retrieval needs query text to embed")
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			return nil, refuse(domain.ReasonQueryOperatorForbidden, "query contains a control character")
		}
	}
	runes := utf8.RuneCountInString(text)
	if p.limits.MinQueryLength > 0 && runes < p.limits.MinQueryLength {
		return nil, refuse(domain.ReasonQueryComplexityExceeded,
			"a query must be at least %d characters", p.limits.MinQueryLength)
	}
	if runes > maxSemanticQueryRunes {
		return nil, refuse(domain.ReasonQueryComplexityExceeded,
			"a semantic query may be at most %d characters", maxSemanticQueryRunes)
	}

	size := plan.Execution.Size
	k := req.K
	if k <= 0 {
		k = size
	}
	if k > p.limits.MaxResultWindow {
		return nil, refuse(domain.ReasonResultWindowExceeded,
			"k %d exceeds the maximum of %d", k, p.limits.MaxResultWindow)
	}
	if size > k {
		size = k
	}

	// NP-35 on the read side: only vectors from the pinned model are
	// candidates. Mandatory, so it is inside the ANN walk's filter too.
	plan.Execution.MandatoryFilters = append(plan.Execution.MandatoryFilters,
		searchclient.TermFilter{Field: "embedding_model", Values: []string{spec.PinnedModel()}})

	plan.Execution.Text = text
	plan.Execution.K = k
	plan.Execution.Size = size
	plan.Execution.Hybrid = mode == ModeHybrid
	// No snippets, facets, sort or cursor on a nearest-neighbour page.
	plan.Execution.HighlightFields = nil
	plan.Execution.Facets = nil
	plan.Execution.Sort = nil
	plan.SnippetFields = map[string]bool{}

	plan.Semantic = true
	plan.PinnedModel = spec.PinnedModel()
	plan.QueryDigest = digest(text)
	plan.FiltersDigest = digestFilters(plan.Execution.MandatoryFilters)
	plan.PlanDigest = digest(strings.Join([]string{
		digestPlan(plan), "semantic", mode, spec.PinnedModel(), fmt.Sprint(k),
	}, "|"))
	// An ANN walk costs roughly in proportion to k, and each hit above R0 is
	// re-authorized — so k is weighted like page size, plus a fixed cost for
	// the embedding round trip.
	plan.ComplexityScore = complexity(plan.Execution) + 10 + k/5
	if plan.ComplexityScore > p.limits.MaxComplexityScore {
		return nil, refuse(domain.ReasonQueryComplexityExceeded,
			"plan complexity %d exceeds the budget of %d", plan.ComplexityScore, p.limits.MaxComplexityScore)
	}
	return plan, nil
}
