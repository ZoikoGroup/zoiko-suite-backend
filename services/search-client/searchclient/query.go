package searchclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// ExecutionPlan is a compiled, server-owned query. It is the only shape this
// package will execute.
//
// There is deliberately no field here that carries raw engine DSL, a raw index
// name, or a raw query string. INV-04 ("mandatory authorization filters are
// server-generated and cannot be removed, negated or overridden by query
// syntax") is structural rather than validated: a caller cannot express a
// negation of MandatoryFilters in this type at all, so there is no input to
// sanitise and no bypass to miss. NP-03 fails at compile time, not at runtime.
type ExecutionPlan struct {
	// MandatoryFilters are ANDed exact-match constraints the query planner
	// compiled from trusted context — tenant, residency, retrieval class,
	// resource ACLs. Never caller-supplied.
	MandatoryFilters []TermFilter

	// MandatoryMustNot excludes documents unconditionally, e.g. tombstoned
	// projections and suppressed record states.
	MandatoryMustNot []TermFilter

	// UserFilters are the caller's own narrowing, already validated against
	// the scope's registered filterable fields. They can only ever narrow:
	// they are ANDed alongside MandatoryFilters, never ORed with them.
	UserFilters []TermFilter

	// Text is the free-text query, already normalised. Empty means match-all
	// within the mandatory filters.
	Text string

	// TextFields are the scope's registered searchable fields. An empty list
	// means the caller asked for text search on a scope with no searchable
	// text, which the planner refuses before reaching here — this package
	// falls back to match_none rather than to "*", because "*" would search
	// fields the contract never made searchable (INV-08).
	TextFields []string

	// SourceIncludes is the allowlisted result projection (§6.1
	// requested_fields: "full source object is not the default").
	SourceIncludes []string

	// HighlightFields are the subset of TextFields for which a snippet may be
	// returned. Separate from SourceIncludes because a field can be matchable
	// and still not snippet-able (§7.2).
	HighlightFields []string

	// Facets are registered facetable fields to aggregate over. Computed
	// inside the mandatory filters, never over the whole corpus — §7.3.
	Facets []string
	// FacetMinCount suppresses low-cardinality buckets. §7.3 / NP-08: a facet
	// count of one can reveal the existence of a single privileged record.
	FacetMinCount int
	FacetSize     int

	Size int
	// SearchAfter is the decoded sort cursor from an opaque signed cursor.
	// Offset paging is not offered at all: NP-21 forbids unbounded windows and
	// a from/size API invites exactly that.
	SearchAfter []any
	// Sort is the tie-broken sort order. The final key is always _id so a
	// cursor is stable across equal scores; without it search_after skips or
	// repeats documents at a score boundary.
	Sort []SortKey

	Timeout time.Duration
}

// TermFilter is one exact-match constraint. Values are ORed within a filter
// and filters are ANDed together, which is the only combination a security
// filter ever needs: "tenant is X" and "acl is one of A, B, C".
type TermFilter struct {
	Field  string
	Values []string
}

// SortKey is one registered sortable field and its direction.
type SortKey struct {
	Field string
	Desc  bool
}

// Hit is one candidate result. NOT an authorization: INV-06 — "an index hit is
// not evidence that a user may open the underlying resource." The retrieval
// layer re-authorizes each of these before any content is returned.
type Hit struct {
	DocID      string
	Score      float64
	Source     map[string]any
	Highlights map[string][]string
	// Sort carries this hit's sort values, which become the next page's
	// search_after cursor.
	Sort []any
}

// Result is one executed plan's outcome.
type Result struct {
	Total    int64
	Relation string
	Hits     []Hit
	Facets   map[string][]FacetBucket

	// Partial is true when the engine answered with fewer shards than it has,
	// or timed out. NP-19: "engine returns partial shards but HTTP 200 → mark
	// PARTIAL/DEGRADED; do not present as complete." This is the field that
	// keeps INV-24 honest, and it is why the raw *_shards* block is read
	// rather than trusting the 200.
	Partial       bool
	PartialReason string
	TookMS        int
}

// FacetBucket is one facet value and its count, already min-cell filtered.
type FacetBucket struct {
	Value string
	Count int64
}

// ExecutePlan runs a compiled plan against a target, which is always an ALIAS
// and never a concrete index chosen by a caller.
func (c *client) ExecutePlan(ctx context.Context, target string, plan ExecutionPlan) (Result, error) {
	body, err := json.Marshal(plan.compile())
	if err != nil {
		return Result{}, fmt.Errorf("searchclient: ExecutePlan marshal: %w", err)
	}

	resp, err := c.os.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{target},
		Body:    bytes.NewReader(body),
	})
	if err != nil {
		if isMissing(err) {
			// An alias with no active generation is not an empty result: a
			// caller told "no matches" would conclude the data does not
			// exist. §8.3 wants this surfaced, so it is an error the handler
			// maps to ESR-011 INDEX_GENERATION_NOT_ACTIVE.
			return Result{}, fmt.Errorf("%w: %s", ErrNoActiveGeneration, target)
		}
		return Result{}, fmt.Errorf("searchclient: ExecutePlan %s: %w", target, err)
	}

	raw, isErr := drain(resp.Inspect().Response)
	if isErr {
		return Result{}, fmt.Errorf("searchclient: ExecutePlan %s failed: %s", target, raw)
	}

	out := Result{
		Total:    int64(resp.Hits.Total.Value),
		Relation: resp.Hits.Total.Relation,
		TookMS:   resp.Took,
		Hits:     make([]Hit, 0, len(resp.Hits.Hits)),
	}

	// NP-19. A 200 with failed shards is a partial answer wearing a success
	// status; reporting it as complete is the silent over-claim INV-24 exists
	// to prevent.
	if resp.Timeout {
		out.Partial = true
		out.PartialReason = "engine timeout"
	}
	if resp.Shards.Failed > 0 || (resp.Shards.Total > 0 && resp.Shards.Successful < resp.Shards.Total-resp.Shards.Skipped) {
		out.Partial = true
		reason := fmt.Sprintf("%d of %d shards failed", resp.Shards.Failed, resp.Shards.Total)
		if out.PartialReason != "" {
			out.PartialReason += "; " + reason
		} else {
			out.PartialReason = reason
		}
	}

	// Highlights are not on the typed SearchHit, so they come from the raw
	// body. Decoding the whole response a second time is cheap next to the
	// query and avoids a fork of the vendor's type.
	var rawResp struct {
		Hits struct {
			Hits []struct {
				ID        string              `json:"_id"`
				Highlight map[string][]string `json:"highlight"`
			} `json:"hits"`
		} `json:"hits"`
	}
	highlights := map[string]map[string][]string{}
	if err := json.Unmarshal([]byte(raw), &rawResp); err == nil {
		for _, h := range rawResp.Hits.Hits {
			if len(h.Highlight) > 0 {
				highlights[h.ID] = h.Highlight
			}
		}
	}

	for _, h := range resp.Hits.Hits {
		var source map[string]any
		if len(h.Source) > 0 {
			if err := json.Unmarshal(h.Source, &source); err != nil {
				return Result{}, fmt.Errorf("searchclient: ExecutePlan decode hit %s: %w", h.ID, err)
			}
		}
		out.Hits = append(out.Hits, Hit{
			DocID:      h.ID,
			Score:      float64(h.Score),
			Source:     source,
			Highlights: highlights[h.ID],
			Sort:       h.Sort,
		})
	}

	if len(plan.Facets) > 0 && len(resp.Aggregations) > 0 {
		out.Facets = decodeFacets(resp.Aggregations, plan.Facets, plan.FacetMinCount)
	}
	return out, nil
}

// ErrNoActiveGeneration means the scope's alias resolves to nothing — the
// scope has never been built, or its only generation was dropped.
var ErrNoActiveGeneration = fmt.Errorf("searchclient: scope has no active index generation")

// compile lowers the plan into the engine's request body.
func (p ExecutionPlan) compile() map[string]any {
	filter := make([]map[string]any, 0, len(p.MandatoryFilters)+len(p.UserFilters))
	for _, f := range p.MandatoryFilters {
		if c := f.clause(); c != nil {
			filter = append(filter, c)
		}
	}
	for _, f := range p.UserFilters {
		if c := f.clause(); c != nil {
			filter = append(filter, c)
		}
	}

	mustNot := make([]map[string]any, 0, len(p.MandatoryMustNot))
	for _, f := range p.MandatoryMustNot {
		if c := f.clause(); c != nil {
			mustNot = append(mustNot, c)
		}
	}

	var must []map[string]any
	switch {
	case p.Text == "":
		// No text: the filters alone define the result set.
	case len(p.TextFields) == 0:
		// Text asked for on a scope with no searchable field. match_none, not
		// a wildcard: falling back to "*" would search fields the contract
		// deliberately left unsearchable.
		must = append(must, map[string]any{"match_none": map[string]any{}})
	default:
		must = append(must, map[string]any{
			"multi_match": map[string]any{
				"query":  p.Text,
				"type":   "best_fields",
				"fields": p.TextFields,
				// No leading wildcards, no regex, no query_string — §6.2
				// forbids constructs that bypass budgets or expose engine
				// internals, and multi_match over a fixed field list has
				// none of them.
				"operator": "and",
			},
		})
	}

	size := p.Size
	if size <= 0 {
		size = 20
	}

	query := map[string]any{
		"bool": map[string]any{
			"filter":   filter,
			"must":     must,
			"must_not": mustNot,
		},
	}

	body := map[string]any{
		"size":  size,
		"query": query,
		// An exact total is what makes "PARTIAL vs COMPLETE" meaningful, and
		// the counts are already inside the mandatory filters, so this cannot
		// leak global corpus cardinality (§7.3).
		"track_total_hits": true,
	}

	if len(p.SourceIncludes) > 0 {
		body["_source"] = map[string]any{"includes": p.SourceIncludes}
	} else {
		// Deny by default: a plan that named no returnable fields returns
		// governance lineage only, never the whole document (ESR-006).
		body["_source"] = map[string]any{"includes": []string{
			"tenant_id", "legal_entity_id", "source_type", "source_id",
			"source_version", "restriction_epoch", "retrieval_class",
			"sensitivity_class", "acl_refs", "index_generation", "indexed_at",
		}}
	}

	sorts := make([]any, 0, len(p.Sort)+2)
	for _, s := range p.Sort {
		dir := "asc"
		if s.Desc {
			dir = "desc"
		}
		sorts = append(sorts, map[string]any{s.Field: map[string]any{"order": dir}})
	}
	sorts = append(sorts, map[string]any{"_score": map[string]any{"order": "desc"}})
	// Always last. Without a unique tie-break, search_after can skip or repeat
	// documents whose sort values are equal — a cursor that silently loses
	// rows is worse than no cursor.
	sorts = append(sorts, map[string]any{"source_id": map[string]any{"order": "asc"}})
	body["sort"] = sorts

	if len(p.SearchAfter) > 0 {
		body["search_after"] = p.SearchAfter
	}

	if len(p.HighlightFields) > 0 && p.Text != "" {
		fields := make(map[string]any, len(p.HighlightFields))
		for _, f := range p.HighlightFields {
			fields[f] = map[string]any{
				"fragment_size":       160,
				"number_of_fragments": 1,
			}
		}
		body["highlight"] = map[string]any{
			// The tags are stripped and re-applied by the retrieval layer
			// after escaping. Leaving the engine's default <em> in place would
			// mean returning unescaped stored markup wrapped in real HTML —
			// NP-24 in one step.
			"pre_tags":  []string{""},
			"post_tags": []string{""},
			"fields":    fields,
			// Match the query rather than re-analysing: a highlighter that
			// runs its own query can surface a span the query never matched.
			"require_field_match": true,
		}
	}

	if len(p.Facets) > 0 {
		size := p.FacetSize
		if size <= 0 {
			size = 10
		}
		aggs := make(map[string]any, len(p.Facets))
		for _, f := range p.Facets {
			aggs[f] = map[string]any{
				"terms": map[string]any{
					"field": f,
					"size":  size,
					// min_doc_count at the engine keeps suppressed buckets off
					// the wire entirely, rather than relying on this process
					// to drop them after they have already been computed and
					// transmitted (§7.3).
					"min_doc_count": maxInt(p.FacetMinCount, 1),
				},
			}
		}
		body["aggs"] = aggs
	}

	if p.Timeout > 0 {
		body["timeout"] = fmt.Sprintf("%dms", p.Timeout.Milliseconds())
	}
	return body
}

func (f TermFilter) clause() map[string]any {
	if f.Field == "" || len(f.Values) == 0 {
		return nil
	}
	if len(f.Values) == 1 {
		return map[string]any{"term": map[string]any{f.Field: f.Values[0]}}
	}
	values := make([]any, 0, len(f.Values))
	for _, v := range f.Values {
		values = append(values, v)
	}
	return map[string]any{"terms": map[string]any{f.Field: values}}
}

// decodeFacets pulls terms buckets out of the aggregation block and applies the
// minimum-cell rule a second time.
//
// Belt and braces on purpose. min_doc_count is set on the request, but a facet
// that leaked a count of one would be an existence disclosure (NP-08), and a
// control that matters that much is worth enforcing on both sides of the wire —
// an engine upgrade that changed min_doc_count semantics would otherwise turn
// a security rule off silently.
func decodeFacets(raw json.RawMessage, fields []string, minCount int) map[string][]FacetBucket {
	var parsed map[string]struct {
		Buckets []struct {
			Key      any   `json:"key"`
			DocCount int64 `json:"doc_count"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}

	out := make(map[string][]FacetBucket, len(fields))
	for _, field := range fields {
		agg, ok := parsed[field]
		if !ok {
			continue
		}
		buckets := make([]FacetBucket, 0, len(agg.Buckets))
		for _, b := range agg.Buckets {
			if b.DocCount < int64(maxInt(minCount, 1)) {
				continue
			}
			buckets = append(buckets, FacetBucket{Value: fmt.Sprint(b.Key), Count: b.DocCount})
		}
		if len(buckets) == 0 {
			continue
		}
		sort.SliceStable(buckets, func(i, j int) bool { return buckets[i].Count > buckets[j].Count })
		out[field] = buckets
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// StripHighlightMarkers removes the private-use sentinels compile() asked the
// engine to wrap matches in, returning the plain text and the matched spans.
//
// The retrieval layer escapes the plain text first and re-inserts real markup
// around the spans afterwards, so stored HTML in an indexed body cannot reach a
// browser as markup (NP-24 / §7.2's "HTML is escaped/sanitized; stored source
// markup cannot execute in the client").
func StripHighlightMarkers(fragment string) string {
	return strings.NewReplacer("", "", "", "").Replace(fragment)
}

// HighlightOpen and HighlightClose are the sentinels. Exported so the retrieval
// layer can find the spans without re-deriving them.
const (
	HighlightOpen  = ""
	HighlightClose = ""
)
