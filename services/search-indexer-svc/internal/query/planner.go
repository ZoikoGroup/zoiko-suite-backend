// Package query is ESR-03: it turns a caller's request into a server-owned
// execution plan, or refuses it with a stable reason code.
//
// The governing idea of §6 is that the caller never gets query POWER, only
// query INPUT:
//
//	"Make every search request a policy-bound server-side query plan rather
//	 than allowing arbitrary client query power."
//
// So this package is mostly refusals. Deny by default (§6.2): a scope, a
// field, a filter, a sort key or an operator that is not registered in the
// published contract is rejected, and the rejection names an ESR code rather
// than an English sentence, so a caller can branch on it.
//
// The mandatory filters are assembled here and handed to the engine as part of
// a compiled plan the caller cannot address. There is no path by which a
// caller's input reaches the engine as query syntax — Compile returns a
// searchclient.ExecutionPlan, and that type has no field capable of expressing
// a negation of the mandatory filters. NP-03 is therefore structural.
package query

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/store"
)

// Request is the §6.1 canonical request envelope, minus the parts that come
// from trusted context rather than from the body.
//
// Note what is NOT here: tenant_id and actor. NP-01 is "client supplies
// another tenant_id in body → reject/ignore client tenant; trusted context
// wins", and the cheapest way to honour that is to give the body nowhere to
// put one. A caller that sends tenant_id in the JSON finds it silently
// ignored by the decoder, because no field exists to receive it.
type Request struct {
	Scope           string            `json:"scope"`
	Text            string            `json:"query"`
	Filters         map[string]string `json:"filters,omitempty"`
	RequestedFields []string          `json:"requested_fields,omitempty"`
	Facets          []string          `json:"facets,omitempty"`
	Sort            []SortRequest     `json:"sort,omitempty"`
	Size            int               `json:"size,omitempty"`
	Cursor          string            `json:"cursor,omitempty"`
	// IncludeSnippets asks for highlights. Honoured only for fields the
	// contract marked snippet_allowed (§7.2).
	IncludeSnippets bool `json:"include_snippets,omitempty"`
}

type SortRequest struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc,omitempty"`
}

// Context is the trusted half of a request: resolved, never caller-asserted.
type Context struct {
	TenantID string
	ActorID  string
	// WorkloadID is set when an AI/RAG caller is acting. NP-31: "AI agent
	// calls ESR with privileged platform service identity → reject unless
	// workload is registered to act on behalf of real actor with constrained
	// scope." OnBehalfOf is that real actor.
	WorkloadID    string
	OnBehalfOf    string
	LegalEntityID string
	Purpose       string
	// ResidencyRegion comes from tenant context, never from the request.
	// INV-03 / NP-16.
	ResidencyRegion string
}

// Error is a refusal carrying a §11.3 reason code.
type Error struct {
	Code   domain.ReasonCode
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s %s: %s", e.Code, domain.ReasonMeaning(e.Code), e.Detail)
}

func refuse(code domain.ReasonCode, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Plan is a compiled plan plus the evidence fields §2.1's QueryPlan requires.
type Plan struct {
	Scope           string
	Execution       searchclient.ExecutionPlan
	Target          string
	ContractID      string
	ContractVersion int
	RetrievalClass  domain.RetrievalClass
	AuthzAction     string

	// Digests, for evidence and for cursor binding. TC-10: "every query plan
	// records mandatory-filter digest and eligible partition set."
	QueryDigest     string
	FiltersDigest   string
	PlanDigest      string
	PartitionSet    []string
	ComplexityScore int

	// SnippetFields are the fields a snippet may be produced from. Carried
	// separately from the execution plan's HighlightFields because the
	// retrieval layer re-checks it after re-authorization — a field may be
	// snippet-allowed by contract and still not returnable for a particular
	// hit.
	SnippetFields map[string]bool
	// ReturnableFields is the allowlist the retrieval layer filters _source
	// through a second time.
	ReturnableFields map[string]bool

	// Page is the cursor page number this plan serves, for the window check.
	Page int
}

// Limits bounds what a plan may ask for.
type Limits struct {
	MaxResultWindow    int
	MaxComplexityScore int
	FacetMinCount      int
	// MaxPages bounds a cursor chain. NP-21 forbids unbounded result windows;
	// a cursor has no offset to bound, so the walk itself is what gets
	// bounded. Generous, because a legitimate export walks a long way — and
	// an export is supposed to go down the R3 path anyway (INV-29).
	MaxPages int
	// MinQueryLength guards NP-54: "one-character queries enumerate person
	// directory." Zero disables it, which is correct for scopes with no
	// person-shaped data; the planner applies it per sensitivity below.
	MinQueryLength int
}

// DefaultLimits are the platform defaults until OD-06/OD-13 are ratified.
func DefaultLimits() Limits {
	return Limits{
		MaxResultWindow:    100,
		MaxComplexityScore: 100,
		FacetMinCount:      2,
		MaxPages:           100,
		MinQueryLength:     2,
	}
}

// Planner compiles requests against published contracts.
type Planner struct {
	limits  Limits
	store   store.Store
	// cursorKey signs and verifies pagination tokens.
	cursorKey []byte
}

func NewPlanner(limits Limits, store store.Store, cursorKey []byte) *Planner {
	return &Planner{limits: limits, store: store, cursorKey: cursorKey}
}

// Compile turns a request plus trusted context plus a published contract into
// an execution plan, or refuses it.
//
// The order of checks is deliberate and is the §1.3 decision ordering:
// identity, then purpose, then scope, then partitions, then filters, then
// budget. A request that fails an earlier check never reaches a later one, so
// a caller cannot learn from a complexity refusal that a scope exists.
func (p *Planner) Compile(ctx context.Context, req Request, tc Context, contract *domain.IndexContract, generation *domain.IndexGeneration) (*Plan, error) {
	// 1. Trusted actor/tenant.
	if tc.TenantID == "" {
		return nil, refuse(domain.ReasonTenantContextMissing,
			"no verified tenant on the request; search is never executed without one")
	}
	if tc.ActorID == "" && tc.WorkloadID == "" {
		return nil, refuse(domain.ReasonTenantContextMissing,
			"no verified actor or workload on the request")
	}
	// NP-31. A workload may search, but only as a named actor. A workload
	// with no on-behalf-of is a service identity searching in its own right,
	// which is exactly the privilege escalation INV-28 forbids: "AI/RAG search
	// cannot bypass current actor/workload authorization or use a more
	// privileged service identity to expand evidence."
	if tc.ActorID == "" && tc.WorkloadID != "" && tc.OnBehalfOf == "" {
		return nil, refuse(domain.ReasonPartitionNotAuthorized,
			"a workload must search on behalf of a named principal; a bare service identity may not retrieve")
	}

	// 2. Purpose, for sensitive corpora.
	if tc.Purpose == "" {
		return nil, refuse(domain.ReasonPrivacyPurposeBlocked,
			"purpose_context is required for governed search")
	}

	// 3. Scope.
	if contract == nil {
		return nil, refuse(domain.ReasonScopeNotRegistered,
			"scope %q has no published index contract", req.Scope)
	}
	if generation == nil {
		return nil, refuse(domain.ReasonGenerationNotActive,
			"scope %q has no active index generation", req.Scope)
	}

	// 3b. Freshness check (ESR-012). A scope whose latest checkpoint reports
	// STALE must not be queried — the index cannot be trusted as a current
	// view of the source. §5.3: "STALE is never represented as CURRENT".
	// UNKNOWN is also a refusal: an unmeasured index is not a safe index.
	if p.store != nil {
		cp, err := p.store.GetLatestCheckpoint(ctx, contract.ScopeName)
		if err == nil && cp != nil {
			switch cp.Freshness {
			case domain.FreshnessStale:
				return nil, refuse(domain.ReasonIndexStaleForScope,
					"scope %q has stale index (checkpoint freshness=STALE, lag=%dms, watermark=%d)",
					contract.ScopeName, cp.LagMS, cp.Watermark)
			case domain.FreshnessUnknown:
				return nil, refuse(domain.ReasonIndexStaleForScope,
					"scope %q has unmeasured index (checkpoint freshness=UNKNOWN)",
					contract.ScopeName)
			}
		}
	}

	fields := indexFields(contract)

	// 4/5. Mandatory filters, compiled from trusted context only.
	mandatory := p.mandatoryFilters(tc, contract)
	mustNot := []searchclient.TermFilter{
		// A tombstoned projection is never a candidate. First and
		// unconditional: INV-18's restriction propagation is only as good as
		// the query that honours it, and a tombstone the query ignored is a
		// tombstone that did nothing.
		{Field: "tombstoned", Values: []string{"true"}},
	}

	// 6. User filters, validated against registered filterable fields.
	userFilters, err := p.userFilters(req, fields)
	if err != nil {
		return nil, err
	}

	// Text normalisation. §6.2: "normalize Unicode and locale behavior
	// consistently; do not perform security decisions on unnormalized user
	// strings." NFKC before every length and operator check below, so a
	// composed and a decomposed form of the same string are treated
	// identically — otherwise a two-character minimum could be defeated with
	// a combining mark.
	text := strings.TrimSpace(norm.NFKC.String(req.Text))
	if err := p.checkOperators(text); err != nil {
		return nil, err
	}
	if text != "" && p.limits.MinQueryLength > 0 && utf8.RuneCountInString(text) < p.limits.MinQueryLength {
		// NP-54. Applied to every scope rather than only person-shaped ones:
		// a single character against invoices enumerates just as effectively,
		// and the cost to a legitimate caller is one more keystroke.
		return nil, refuse(domain.ReasonQueryComplexityExceeded,
			"a query must be at least %d characters; shorter queries enumerate rather than search",
			p.limits.MinQueryLength)
	}

	searchable := namesWhere(fields, func(f domain.SearchFieldDefinition) bool { return f.Searchable })
	if text != "" && len(searchable) == 0 {
		return nil, refuse(domain.ReasonFieldNotSearchable,
			"scope %q registers no searchable text field", contract.ScopeName)
	}

	// requested_fields, validated against registered returnable fields.
	includes, returnable, err := p.requestedFields(req, fields)
	if err != nil {
		return nil, err
	}

	// Sort keys. NP-53: "user sorts by hidden salary field → sort key not
	// registered/authorized; reject without leaking field existence." The
	// refusal says "not sortable", never "does not exist", so the two are
	// indistinguishable to a caller probing for fields.
	sortKeys, err := p.sortKeys(req, fields)
	if err != nil {
		return nil, err
	}

	// Facets, validated against registered facetable fields.
	facets, err := p.facets(req, fields)
	if err != nil {
		return nil, err
	}

	// Snippets, validated against snippet_allowed AND returnable.
	snippetFields := map[string]bool{}
	var highlight []string
	if req.IncludeSnippets && text != "" {
		for _, f := range fields {
			if f.SnippetAllowed && f.Returnable && f.Searchable && returnable[f.FieldID] {
				snippetFields[f.FieldID] = true
				highlight = append(highlight, f.FieldID)
			}
		}
		sort.Strings(highlight)
	}

	size := req.Size
	if size <= 0 {
		size = 20
	}
	if size > p.limits.MaxResultWindow {
		return nil, refuse(domain.ReasonResultWindowExceeded,
			"requested page size %d exceeds the maximum of %d", size, p.limits.MaxResultWindow)
	}

	exec := searchclient.ExecutionPlan{
		MandatoryFilters: mandatory,
		MandatoryMustNot: mustNot,
		UserFilters:      userFilters,
		Text:             text,
		TextFields:       searchable,
		SourceIncludes:   includes,
		HighlightFields:  highlight,
		Facets:           facets,
		FacetMinCount:    p.limits.FacetMinCount,
		FacetSize:        10,
		Size:             size,
		Sort:             sortKeys,
	}

	plan := &Plan{
		Scope:            contract.ScopeName,
		Execution:        exec,
		Target:           searchclient.Alias(searchclient.IndexName(contract.ScopeName)),
		ContractID:       contract.ContractID,
		ContractVersion:  contract.Version,
		RetrievalClass:   contract.RetrievalClass,
		AuthzAction:      contract.AuthzAction,
		PartitionSet:     []string{generation.PhysicalIndex},
		SnippetFields:    snippetFields,
		ReturnableFields: returnable,
	}

	plan.QueryDigest = digest(text)
	plan.FiltersDigest = digestFilters(mandatory)
	plan.PlanDigest = digestPlan(plan)
	plan.ComplexityScore = complexity(exec)

	// 6.2's budget, checked last so the score reflects the finished plan.
	if plan.ComplexityScore > p.limits.MaxComplexityScore {
		return nil, refuse(domain.ReasonQueryComplexityExceeded,
			"plan complexity %d exceeds the budget of %d", plan.ComplexityScore, p.limits.MaxComplexityScore)
	}

	// The cursor is decoded LAST, against the finished plan digest, so a
	// cursor issued for a different filter set cannot be replayed onto this
	// one (NP-56).
	if req.Cursor != "" {
		c, err := DecodeCursor(p.cursorKey, req.Cursor, tc.TenantID, contract.ScopeName, plan.PlanDigest)
		if err != nil {
			return nil, refuse(domain.ReasonResultWindowExceeded,
				"pagination cursor is not valid for this request: %v", err)
		}
		if c.Page >= p.limits.MaxPages {
			return nil, refuse(domain.ReasonResultWindowExceeded,
				"cursor has walked %d pages, the maximum; narrow the query or use an authorized export",
				c.Page)
		}
		plan.Execution.SearchAfter = c.After
		plan.Page = c.Page
	}
	return plan, nil
}

// NextCursor mints the cursor for the page after this one.
func (p *Planner) NextCursor(plan *Plan, tenantID string, lastSort []any) (string, error) {
	if len(lastSort) == 0 {
		return "", nil
	}
	return EncodeCursor(p.cursorKey, Cursor{
		TenantID:   tenantID,
		Scope:      plan.Scope,
		PlanDigest: plan.PlanDigest,
		After:      lastSort,
		Page:       plan.Page + 1,
	})
}

// mandatoryFilters compiles §6.3's filter layers from trusted context.
//
// Every value here comes from resolved context or from the contract. None of
// it is reachable from the request body, which is what INV-04 means by
// "server-generated and cannot be removed, negated or overridden by query
// syntax".
func (p *Planner) mandatoryFilters(tc Context, contract *domain.IndexContract) []searchclient.TermFilter {
	filters := []searchclient.TermFilter{
		// Tenant/partition layer. INV-02, NP-01.
		{Field: "tenant_id", Values: []string{tc.TenantID}},
	}

	if tc.ResidencyRegion != "" {
		// Residency layer. NP-16: "cross-region replica would violate
		// residency → partition not eligible." GLOBAL is always eligible
		// alongside the caller's own region; a shared reference corpus is
		// stored once and read from everywhere, and excluding it would make
		// reference data invisible outside its home region.
		filters = append(filters, searchclient.TermFilter{
			Field:  "residency_region",
			Values: []string{tc.ResidencyRegion, "GLOBAL"},
		})
	}

	if tc.LegalEntityID != "" {
		// Resource-authorization layer, as a CANDIDATE narrowing only. §5.2:
		// "ACL/resource attributes in the projection are hints for candidate
		// filtering, not the final authority for material retrieval." The
		// authority is the per-hit re-authorization in the retrieval layer;
		// this filter exists to keep the candidate set small, not to decide.
		filters = append(filters, searchclient.TermFilter{
			Field:  "acl_refs",
			Values: []string{"entity:" + tc.LegalEntityID},
		})
	}
	return filters
}

func (p *Planner) userFilters(req Request, fields map[string]domain.SearchFieldDefinition) ([]searchclient.TermFilter, error) {
	if len(req.Filters) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(req.Filters))
	for k := range req.Filters {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make([]searchclient.TermFilter, 0, len(names))
	for _, name := range names {
		f, ok := fields[name]
		if !ok || !f.Filterable {
			// One code for both "unknown" and "not filterable". Telling them
			// apart would let a caller enumerate the contract's hidden fields
			// by watching which name produces which refusal.
			return nil, refuse(domain.ReasonFieldNotSearchable,
				"field %q is not registered as filterable on this scope", name)
		}
		value := strings.TrimSpace(norm.NFKC.String(req.Filters[name]))
		if value == "" {
			continue
		}
		field := name
		if strings.EqualFold(f.Type, "TEXT") {
			// A TEXT field is analyzed; filtering on it would match a token
			// rather than the value. The contract's keyword sub-field is what
			// makes an exact filter possible, and EnsureGeneration only
			// creates it when the field is filterable — which was checked
			// above.
			field = name + ".keyword"
		}
		out = append(out, searchclient.TermFilter{Field: field, Values: []string{value}})
	}
	return out, nil
}

func (p *Planner) requestedFields(req Request, fields map[string]domain.SearchFieldDefinition) ([]string, map[string]bool, error) {
	returnable := map[string]bool{}

	// The governance lineage is always returned. TC-01 requires every result
	// to trace to tenant + source_type + source_id + source_version, so these
	// are not the caller's to request or to omit.
	includes := []string{
		"tenant_id", "legal_entity_id", "source_type", "source_id",
		"source_version", "restriction_epoch", "retrieval_class",
		"sensitivity_class", "acl_refs", "index_generation", "indexed_at", "tombstoned",
	}

	if len(req.RequestedFields) == 0 {
		// Deny by default (§6.1: "full source object is not the default").
		// With nothing requested, the caller gets lineage plus the fields the
		// contract marked returnable — which is the minimum useful result,
		// not the whole document.
		for name, f := range fields {
			if f.Returnable {
				includes = append(includes, name)
				returnable[name] = true
			}
		}
		sort.Strings(includes)
		return includes, returnable, nil
	}

	for _, name := range req.RequestedFields {
		f, ok := fields[name]
		if !ok || !f.Returnable {
			// ESR-006 FIELD_NOT_RETURNABLE. NP-09: "query asks for
			// non-returnable sensitive field → field omitted/blocked." A
			// refusal rather than a silent omission, because a caller that
			// asked for a field and got a result without it would reasonably
			// conclude the field was empty.
			return nil, nil, refuse(domain.ReasonFieldNotReturnable,
				"field %q is not registered as returnable on this scope", name)
		}
		includes = append(includes, name)
		returnable[name] = true
	}
	sort.Strings(includes)
	return includes, returnable, nil
}

func (p *Planner) sortKeys(req Request, fields map[string]domain.SearchFieldDefinition) ([]searchclient.SortKey, error) {
	out := make([]searchclient.SortKey, 0, len(req.Sort))
	for _, s := range req.Sort {
		f, ok := fields[s.Field]
		if !ok || !f.Sortable {
			return nil, refuse(domain.ReasonFieldNotSearchable,
				"field %q is not registered as sortable on this scope", s.Field)
		}
		field := s.Field
		if strings.EqualFold(f.Type, "TEXT") {
			field = s.Field + ".keyword"
		}
		out = append(out, searchclient.SortKey{Field: field, Desc: s.Desc})
	}
	return out, nil
}

func (p *Planner) facets(req Request, fields map[string]domain.SearchFieldDefinition) ([]string, error) {
	out := make([]string, 0, len(req.Facets))
	for _, name := range req.Facets {
		f, ok := fields[name]
		if !ok || !f.Facetable {
			return nil, refuse(domain.ReasonFieldNotSearchable,
				"field %q is not registered as facetable on this scope", name)
		}
		if strings.EqualFold(f.Type, "TEXT") {
			out = append(out, name+".keyword")
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// forbiddenOperators are the query constructs §6.2 rejects outright.
//
// Checked against the raw text rather than parsed out of it, because this
// service does not accept an engine query language at all — the text goes into
// a multi_match over a fixed field list, where none of these has any meaning.
// Rejecting them is therefore not sanitisation but honesty: a caller that sent
// `status:OPEN AND salary:>100000` expecting it to be interpreted would
// otherwise get a full-text match on the literal string and no indication that
// its query language was never supported.
//
// These are PUNCTUATION, matched as substrings. Every one of them is a
// character a lexical query has no legitimate use for.
var forbiddenOperators = []struct {
	token  string
	reason string
}{
	{"*", "wildcards are not supported; they bypass the complexity budget"},
	{"?", "single-character wildcards are not supported"},
	{"~", "fuzzy operators are not supported; they expand unboundedly"},
	{"/", "regular expressions are not supported"},
}

// forbiddenWords are engine identifiers, matched on WORD BOUNDARIES.
//
// Not substrings, and the distinction is not pedantry: "script" appears
// inside "description", "transcript", "prescription" and "subscription", all
// of which are ordinary things to search a business corpus for. A substring
// check here would refuse "invoice description" as an attempted engine script
// — a refusal the caller could neither understand nor work around, on one of
// the most common words in the estate.
var forbiddenWords = regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}_])(script|painless|_index|_source|_id|_score)($|[^\p{L}\p{N}_])`)

func (p *Planner) checkOperators(text string) error {
	lowered := strings.ToLower(text)
	for _, op := range forbiddenOperators {
		if strings.Contains(lowered, op.token) {
			return refuse(domain.ReasonQueryOperatorForbidden, "%s", op.reason)
		}
	}
	if m := forbiddenWords.FindStringSubmatch(text); m != nil {
		return refuse(domain.ReasonQueryOperatorForbidden,
			"%q names an engine internal; index selection, scoring and scripting are server-owned", m[2])
	}
	// A control character in a query is not a query. Rejected rather than
	// stripped: stripping changes what the caller asked for without telling
	// them, and a normalised-away character is exactly how a length or
	// operator check gets bypassed.
	for _, r := range text {
		if unicode.IsControl(r) {
			return refuse(domain.ReasonQueryOperatorForbidden,
				"query contains a control character")
		}
	}
	return nil
}

// complexity scores a compiled plan against §6.2's budget.
//
// Weighted by what actually costs the engine work, not by clause count. A
// facet is an aggregation over the whole matched set and is worth far more
// than a term filter, which is a bitset intersection; a page of 100 costs
// materially more to hydrate and re-authorize than a page of 10, and the
// re-authorization is the expensive half.
func complexity(p searchclient.ExecutionPlan) int {
	score := 1
	score += len(p.MandatoryFilters)
	score += len(p.UserFilters) * 2
	if p.Text != "" {
		score += 2 + len(p.TextFields)
	}
	score += len(p.Facets) * 10
	score += len(p.HighlightFields) * 3
	score += len(p.Sort) * 2
	score += p.Size / 10
	return score
}

// indexFields maps a contract's fields by name, excluding prohibited ones.
//
// SECRET_PROHIBITED fields are absent from the map entirely, so every lookup
// in this package — filterable, returnable, sortable, facetable, snippet —
// answers "not registered" for them without any of those checks having to
// know the class exists. INV-09 enforced by absence rather than by five
// separate conditions, one of which would eventually be forgotten.
func indexFields(c *domain.IndexContract) map[string]domain.SearchFieldDefinition {
	out := make(map[string]domain.SearchFieldDefinition, len(c.Fields))
	for _, f := range c.Fields {
		if f.SensitivityClass == domain.SensitivitySecretProhibited {
			continue
		}
		out[f.FieldID] = f
	}
	return out
}

func namesWhere(fields map[string]domain.SearchFieldDefinition, pred func(domain.SearchFieldDefinition) bool) []string {
	out := []string{}
	for name, f := range fields {
		if pred(f) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// digest hashes a value for evidence. INV-17: query text is never stored, and
// a digest supports the replay correlation the evidence is actually for.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func digestFilters(filters []searchclient.TermFilter) string {
	parts := make([]string, 0, len(filters))
	for _, f := range filters {
		values := append([]string{}, f.Values...)
		sort.Strings(values)
		parts = append(parts, f.Field+"="+strings.Join(values, ","))
	}
	sort.Strings(parts)
	return digest(strings.Join(parts, "&"))
}

func digestPlan(p *Plan) string {
	e := p.Execution
	parts := []string{
		p.Scope,
		p.ContractID,
		p.FiltersDigest,
		p.QueryDigest,
		strings.Join(e.TextFields, ","),
		strings.Join(e.SourceIncludes, ","),
		strings.Join(e.Facets, ","),
		strings.Join(e.HighlightFields, ","),
		fmt.Sprint(e.Size),
	}
	for _, f := range e.UserFilters {
		parts = append(parts, f.Field+"="+strings.Join(f.Values, ","))
	}
	for _, s := range e.Sort {
		parts = append(parts, fmt.Sprintf("sort:%s:%v", s.Field, s.Desc))
	}
	return digest(strings.Join(parts, "|"))
}
