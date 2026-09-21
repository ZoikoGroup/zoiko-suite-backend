// Package retrieval is ESR-04: it turns candidates into safe, current,
// source-linked results.
//
// The two-stage shape of §7 is the whole design. The engine answers with
// CANDIDATES; nothing it returns is permission. INV-06 states it plainly —
// "an index hit is not evidence that a user may open the underlying
// resource" — and INV-05 says why it cannot be: "access can change after
// indexing", so the projection's acl_refs describe the world as it was when
// the event was consumed, not as it is now.
//
// So every hit above R0 goes back to authorization-svc before any of its
// content is returned, and a hit whose decision cannot be obtained is
// SUPPRESSED rather than shown (INV-07, NP-04). The cost is a per-hit
// authorization call on a page of results; the alternative is serving
// revoked access from a cache of permissions nobody is refreshing.
package retrieval

import (
	"context"
	"errors"
	"html"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/authz"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/query"
	"zoiko.io/search-indexer-svc/internal/telemetry"
)

// Result is one returned search result.
type Result struct {
	SourceType    string  `json:"source_type"`
	SourceID      string  `json:"source_id"`
	SourceVersion int64   `json:"source_version"`
	TenantID      string  `json:"tenant_id"`
	LegalEntityID string  `json:"legal_entity_id,omitempty"`
	Score         float64 `json:"score"`
	// IndexGeneration answers TC-02 from the result itself.
	IndexGeneration string                `json:"index_generation"`
	IndexedAt       string                `json:"indexed_at,omitempty"`
	RetrievalClass  domain.RetrievalClass `json:"retrieval_class"`
	Fields          map[string]any        `json:"fields"`
	Snippets        map[string][]string   `json:"snippets,omitempty"`
	// Freshness says whether the returned content is the index's view or the
	// source's. §7.1 R2: "index content is not trusted as current display
	// value", so a caller has to be able to tell which it got.
	Freshness string `json:"freshness"`
}

// Suppression is a candidate that was withheld.
//
// Returned to the caller, deliberately — but with a reason code and NOTHING
// else. TC-06: "every suppressed result can emit a non-sensitive reason code
// without revealing forbidden existence details." The count and the code let a
// caller understand its results are not exhaustive; the absent source_ref is
// what stops the suppression list becoming an enumeration oracle.
type Suppression struct {
	ReasonCode domain.ReasonCode `json:"reason_code"`
	Meaning    string            `json:"meaning"`
	Count      int               `json:"count"`
}

// Response is one completed governed search.
type Response struct {
	Scope        string              `json:"scope"`
	Results      []Result            `json:"results"`
	Facets       map[string][]Bucket `json:"facets,omitempty"`
	Suppressions []Suppression       `json:"suppressions,omitempty"`
	Completeness domain.Completeness `json:"completeness_state"`
	// CompletenessDetail names why an answer is not COMPLETE. INV-24: partial
	// results are "explicit and never silently represented as complete".
	CompletenessDetail string `json:"completeness_detail,omitempty"`
	Total              int64  `json:"total"`
	// TotalIsExact is false when the engine capped the count or when
	// suppression means the pre-authorization total no longer describes what
	// the caller may see. NP-40 / §7.3.
	TotalIsExact    bool     `json:"total_is_exact"`
	NextCursor      string   `json:"next_cursor,omitempty"`
	IndexGeneration string   `json:"index_generation"`
	ReasonCodes     []string `json:"reason_codes,omitempty"`
}

type Bucket struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// Hydrator fetches the current source object for an R2 result.
//
// An interface rather than a concrete HTTP client because the source is a
// different service per scope, and the registry knows which. A scope with no
// registered hydrator and an R2 contract is a configuration error the
// retriever reports as ESR-014 rather than silently downgrading to R1 — a
// downgrade would return index content while claiming it was hydrated.
type Hydrator interface {
	Hydrate(ctx context.Context, sourceType, sourceID, tenantID string) (map[string]any, error)
}

// Retriever executes a compiled plan and applies §7's controls to the results.
type Retriever struct {
	engine   searchclient.Engine
	authz    authz.Client
	hydrator Hydrator
	metrics  *telemetry.Metrics
	log      *zap.Logger
	timeout  time.Duration
}

func New(engine searchclient.Engine, az authz.Client, h Hydrator, metrics *telemetry.Metrics, log *zap.Logger, hydrationTimeout time.Duration) *Retriever {
	return &Retriever{
		engine:   engine,
		authz:    az,
		hydrator: h,
		metrics:  metrics,
		log:      log,
		timeout:  hydrationTimeout,
	}
}

// Execute runs the plan and builds the response.
func (r *Retriever) Execute(ctx context.Context, plan *query.Plan, tc query.Context, mintCursor func([]any) (string, error)) (*Response, error) {
	raw, err := r.engine.ExecutePlan(ctx, plan.Target, plan.Execution)
	if err != nil {
		r.metrics.EngineError("search")
		if errors.Is(err, searchclient.ErrNoActiveGeneration) {
			return nil, &query.Error{
				Code:   domain.ReasonGenerationNotActive,
				Detail: "the scope's alias resolves to no index",
			}
		}
		return nil, err
	}

	resp := &Response{
		Scope:           plan.Scope,
		Results:         make([]Result, 0, len(raw.Hits)),
		Completeness:    domain.CompletenessComplete,
		Total:           raw.Total,
		TotalIsExact:    raw.Relation == "eq" || raw.Relation == "",
		IndexGeneration: strings.Join(plan.PartitionSet, ","),
	}

	// NP-19. A partial engine answer is reported as partial before anything
	// else happens to it, so a suppression later cannot overwrite the state
	// with something that reads better.
	if raw.Partial {
		resp.Completeness = domain.CompletenessPartial
		resp.CompletenessDetail = raw.PartialReason
		resp.TotalIsExact = false
		resp.ReasonCodes = append(resp.ReasonCodes, string(domain.ReasonSearchDegradedPartial))
	}

	suppressed := map[domain.ReasonCode]int{}
	var lastSort []any

	for _, hit := range raw.Hits {
		lastSort = hit.Sort

		candidate := readCandidate(hit)
		if candidate.sourceID == "" {
			// A hit with no source_ref cannot be traced (TC-01) and cannot be
			// re-authorized. Suppressed rather than returned: an untraceable
			// result is a result nobody can check.
			suppressed[domain.ReasonSourceSuppressed]++
			continue
		}

		// NP-05 / INV-05. The projection's restriction_epoch says what the
		// index believed; a tombstoned document should already have been
		// excluded by the mandatory must_not, so reaching here means the
		// filter and the document disagree — which is a correctness failure
		// in the query layer, not a routine skip. Suppressed and logged.
		if candidate.tombstoned {
			r.log.Error("tombstoned document survived the mandatory filter",
				zap.String("scope", plan.Scope), zap.String("source_id", candidate.sourceID))
			suppressed[domain.ReasonSourceSuppressed]++
			continue
		}

		class := plan.RetrievalClass
		if candidate.retrievalClass != "" {
			// The document's own class wins when it is STRICTER than the
			// contract's. A contract-level R1 must not downgrade a document
			// stamped R2 — per-document classification exists precisely so a
			// sensitive record inside an ordinary corpus keeps its own
			// handling (OD-05).
			if docClass := domain.RetrievalClass(candidate.retrievalClass); stricter(docClass, class) {
				class = docClass
			}
		}

		if class.RequiresReauthorization() {
			entity := candidate.legalEntityID
			if entity == "" {
				entity = tc.LegalEntityID
			}
			// The actor is the REAL principal, never the workload. INV-28 /
			// NP-31: an AI caller may not "use a more privileged service
			// identity to expand evidence", so the authorization question is
			// asked about the human on whose behalf it is acting.
			actor := tc.ActorID
			if actor == "" {
				actor = tc.OnBehalfOf
			}

			err := r.authz.CheckAllowed(ctx, actor, entity, plan.AuthzAction)
			switch {
			case err == nil:
				r.metrics.RetrievalDecisionsTotal.WithLabelValues(plan.Scope, "permit").Inc()
				r.metrics.AuthzDecision(plan.AuthzAction, "allowed")
			case errors.Is(err, authz.ErrDenied):
				r.metrics.RetrievalDecisionsTotal.WithLabelValues(plan.Scope, "suppress").Inc()
				r.metrics.AuthzDecision(plan.AuthzAction, "denied")
				suppressed[domain.ReasonSourceSuppressed]++
				continue
			default:
				// INV-07 / NP-04. No decision means no result. Recorded as
				// INDETERMINATE rather than as a denial, because the operator
				// reading the evidence needs to know the difference between
				// "policy said no" and "policy could not be reached" — the
				// second is an outage and the first is not.
				r.metrics.RetrievalDecisionsTotal.WithLabelValues(plan.Scope, "indeterminate").Inc()
				r.metrics.AuthzDecision(plan.AuthzAction, "unavailable")
				suppressed[domain.ReasonAuthorizationIndet]++
				// A single indeterminate makes the whole answer non-exhaustive.
				resp.Completeness = degrade(resp.Completeness, domain.CompletenessPartial)
				if resp.CompletenessDetail == "" {
					resp.CompletenessDetail = "one or more candidates could not be authorized"
				}
				continue
			}
		}

		result := Result{
			SourceType:      candidate.sourceType,
			SourceID:        candidate.sourceID,
			SourceVersion:   candidate.sourceVersion,
			TenantID:        candidate.tenantID,
			LegalEntityID:   candidate.legalEntityID,
			Score:           hit.Score,
			IndexGeneration: candidate.generation,
			IndexedAt:       candidate.indexedAt,
			RetrievalClass:  class,
			Fields:          projectFields(hit.Source, plan.ReturnableFields),
			Freshness:       "INDEX",
		}

		if class.RequiresHydration() {
			hydrated, herr := r.hydrate(ctx, candidate, plan)
			switch {
			case herr == nil && hydrated != nil:
				// The source's current state replaces the index's view
				// entirely, filtered through the same returnable allowlist —
				// a hydrated document must not expose fields the contract
				// never made returnable just because it came from the source.
				result.Fields = projectFields(hydrated, plan.ReturnableFields)
				result.Freshness = "SOURCE"
			case errors.Is(herr, errSourceGone):
				// NP-55: "current source is deleted between query and
				// hydration → hydration returns NOT_FOUND/SUPPRESS; no stale
				// result body." Suppressed, never served from the index.
				suppressed[domain.ReasonSourceSuppressed]++
				continue
			default:
				// NP-20: "source hydration fails after candidate hit →
				// suppress material content / return explicit unavailable;
				// never trust stale index as authoritative."
				r.log.Warn("source hydration failed — suppressing the result",
					zap.String("scope", plan.Scope),
					zap.String("source_id", candidate.sourceID),
					zap.Error(herr))
				suppressed[domain.ReasonSourceHydrationFailed]++
				resp.Completeness = degrade(resp.Completeness, domain.CompletenessPartial)
				if resp.CompletenessDetail == "" {
					resp.CompletenessDetail = "one or more results could not be hydrated from their source"
				}
				continue
			}
		}

		if len(hit.Highlights) > 0 && len(plan.SnippetFields) > 0 {
			result.Snippets = safeSnippets(hit.Highlights, plan.SnippetFields)
		}

		resp.Results = append(resp.Results, result)
	}

	// The total. Once anything was suppressed, the engine's count describes a
	// population the caller may not see — NP-40 ("global search result count
	// includes inaccessible domains → counts computed only after domain
	// eligibility filters or omitted") and §7.3's rule that counts must not
	// reveal the existence of unreachable records.
	//
	// Reported as inexact rather than recomputed downward: a corrected count
	// would still be a count of a filtered population, and subtracting the
	// suppressions would tell the caller exactly how many results it was not
	// allowed to see — which is the existence disclosure the rule is about.
	if len(suppressed) > 0 {
		resp.TotalIsExact = false
	}

	for code, count := range suppressed {
		resp.Suppressions = append(resp.Suppressions, Suppression{
			ReasonCode: code, Meaning: domain.ReasonMeaning(code), Count: count,
		})
		resp.ReasonCodes = appendUnique(resp.ReasonCodes, string(code))
	}

	if len(raw.Facets) > 0 {
		resp.Facets = make(map[string][]Bucket, len(raw.Facets))
		for field, buckets := range raw.Facets {
			out := make([]Bucket, 0, len(buckets))
			for _, b := range buckets {
				out = append(out, Bucket{Value: b.Value, Count: b.Count})
			}
			// The ".keyword" suffix is an engine implementation detail the
			// planner added; a caller asked for the field by its contract
			// name and should get it back by that name.
			resp.Facets[strings.TrimSuffix(field, ".keyword")] = out
		}
	}

	// A cursor is only minted when a full page came back. Minting one for a
	// short page would hand the caller a token that walks into an empty
	// result and looks like data loss.
	if mintCursor != nil && len(raw.Hits) >= plan.Execution.Size && len(lastSort) > 0 {
		if cursor, err := mintCursor(lastSort); err == nil {
			resp.NextCursor = cursor
		}
	}

	r.metrics.SearchesTotal.WithLabelValues(plan.Scope, string(resp.Completeness)).Inc()
	if len(resp.Results) == 0 {
		r.metrics.SearchZeroResults.WithLabelValues(plan.Scope).Inc()
	}
	return resp, nil
}

var errSourceGone = errors.New("source record no longer exists")

func (r *Retriever) hydrate(ctx context.Context, c candidate, plan *query.Plan) (map[string]any, error) {
	if r.hydrator == nil {
		// ESR-014 rather than a silent downgrade to index content. A scope
		// configured R2 with no hydrator is a deployment error, and serving
		// index content while claiming R2 would be a lie about freshness that
		// nothing downstream could detect.
		return nil, errors.New("scope requires source hydration but no hydrator is configured")
	}
	hctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.hydrator.Hydrate(hctx, c.sourceType, c.sourceID, c.tenantID)
}

// candidate is the governance lineage read off a hit.
type candidate struct {
	tenantID       string
	legalEntityID  string
	sourceType     string
	sourceID       string
	sourceVersion  int64
	generation     string
	indexedAt      string
	retrievalClass string
	tombstoned     bool
}

func readCandidate(hit searchclient.Hit) candidate {
	s := hit.Source
	c := candidate{
		tenantID:       str(s, "tenant_id"),
		legalEntityID:  str(s, "legal_entity_id"),
		sourceType:     str(s, "source_type"),
		sourceID:       str(s, "source_id"),
		generation:     str(s, "index_generation"),
		indexedAt:      str(s, "indexed_at"),
		retrievalClass: str(s, "retrieval_class"),
	}
	if v, ok := s["source_version"].(float64); ok {
		c.sourceVersion = int64(v)
	}
	if v, ok := s["tombstoned"].(bool); ok {
		c.tombstoned = v
	}
	return c
}

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// projectFields filters a source document through the returnable allowlist.
//
// Applied even though the engine was already told which fields to include.
// Two reasons, and the second is the one that matters: a hydrated document did
// NOT come from the engine and has never been filtered at all, and an
// allowlist applied in one place but not the other is an allowlist with a hole
// in exactly the path that carries the freshest, most complete data.
func projectFields(source map[string]any, returnable map[string]bool) map[string]any {
	out := make(map[string]any, len(returnable))
	for name := range returnable {
		if v, ok := source[name]; ok && v != nil {
			out[name] = v
		}
	}
	return out
}

// safeSnippets escapes a highlight fragment and re-applies markup only around
// the spans the engine actually matched.
//
// NP-24 and §7.2: "HTML is escaped/sanitized; stored source markup cannot
// execute in the client." The engine was asked to wrap matches in private-use
// sentinels rather than in <em>, so the fragment that arrives here is entirely
// untrusted text with two known marker runes in it. Escaping the whole thing
// first and then swapping the markers for real tags means stored markup ends
// up as visible text and only OUR tags are markup — the ordering is the
// control, and doing it the other way round would escape the tags we just
// added and leave the stored ones alone.
func safeSnippets(highlights map[string][]string, allowed map[string]bool) map[string][]string {
	if len(highlights) == 0 {
		return nil
	}
	out := map[string][]string{}
	for field, fragments := range highlights {
		name := strings.TrimSuffix(field, ".keyword")
		if !allowed[name] {
			// A field can be matchable without being snippet-able. §7.2:
			// "a matching sensitive term does not authorize surrounding text
			// disclosure", and NP-25 adds that the client must not even learn
			// which hidden field matched.
			continue
		}
		safe := make([]string, 0, len(fragments))
		for _, fragment := range fragments {
			escaped := html.EscapeString(fragment)
			escaped = strings.ReplaceAll(escaped, searchclient.HighlightOpen, "<mark>")
			escaped = strings.ReplaceAll(escaped, searchclient.HighlightClose, "</mark>")
			safe = append(safe, escaped)
		}
		if len(safe) > 0 {
			out[name] = safe
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// stricter reports whether a is a stricter retrieval class than b.
func stricter(a, b domain.RetrievalClass) bool { return rank(a) > rank(b) }

func rank(c domain.RetrievalClass) int {
	switch c {
	case domain.RetrievalR0:
		return 0
	case domain.RetrievalR1:
		return 1
	case domain.RetrievalR2:
		return 2
	case domain.RetrievalR3:
		return 3
	default:
		// An unrecognised class ranks HIGHEST, not lowest. A value this
		// version does not know about is more likely a class added later
		// because something needed stricter handling than one added because
		// something needed less.
		return 4
	}
}

// degrade moves a completeness state in the worse direction only. A later
// COMPLETE can never overwrite an earlier PARTIAL — INV-24 again: the answer
// is as incomplete as its worst part.
func degrade(current, next domain.Completeness) domain.Completeness {
	order := map[domain.Completeness]int{
		domain.CompletenessComplete: 0,
		domain.CompletenessPartial:  1,
		domain.CompletenessDegraded: 2,
		domain.CompletenessUnknown:  3,
	}
	if order[next] > order[current] {
		return next
	}
	return current
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}
