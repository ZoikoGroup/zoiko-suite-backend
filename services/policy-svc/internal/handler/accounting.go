package handler

// REF-07 Accounting Policy (ZS-SVC-I-001 s4.17).
//
//	POST /v1/accounting-policies                          DraftAccountingPolicy
//	GET  /v1/accounting-policies/versions/{version_id}    GetPolicyVersion
//	GET  /v1/accounting-policies/resolve                  ResolveAccountingPolicy
//	GET  /v1/accounting-policies/explain                  ExplainPolicyResolution
//	GET  /v1/accounting-policies                          ListApplicablePolicies
//
// "Policy resolution uses book + entity/jurisdiction + effective date + policy
// domain." A request that is not answered by exactly one policy version does
// not get a guess: "Ambiguous/missing material policy blocks authoritative
// calculation; no UI fallback." So resolving answers 200 only with the one
// governing version, 404 when none governs and 409 when more than one could;
// both refusals say they block the calculation.

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/resolve"
)

const (
	codeNoApplicablePolicy      = "NO_APPLICABLE_POLICY"
	codeAccountingEndpointUsage = "ACCOUNTING_POLICY_ENDPOINT_REQUIRED"
)

// accountingStore is the store's accounting capability. It is optional so that
// a store without it still satisfies PolicyStore; the accounting routes then
// answer 503.
type accountingStore interface {
	CreateAccountingDraft(ctx context.Context, p domain.DraftAccountingPolicyParams) (*domain.AccountingPolicyVersion, bool, error)
	GetAccountingPolicyVersion(ctx context.Context, versionID string) (*domain.AccountingPolicyVersion, error)
	AccountingCandidates(ctx context.Context, q domain.AccountingCandidateQuery) ([]*domain.AccountingCandidate, error)
	IsAccountingPolicy(ctx context.Context, policyID string) (bool, error)
}

func (h *Handler) accounting(w http.ResponseWriter, r *http.Request) (accountingStore, bool) {
	s, ok := h.store.(accountingStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "store_unavailable", "the store does not support accounting policies", nil)
	}
	return s, ok
}

// refuseAccountingFamily answers 400 and returns true when the policy is an
// accounting policy: it has its own commands, which carry the validation and
// the permissions a generic policy version would bypass.
func (h *Handler) refuseAccountingFamily(w http.ResponseWriter, r *http.Request, policyID string) bool {
	s, ok := h.store.(accountingStore)
	if !ok {
		return false
	}
	is, err := s.IsAccountingPolicy(r.Context(), policyID)
	if err != nil {
		h.storeUnavailable(w, r, "IsAccountingPolicy", err)
		return true
	}
	if is {
		refuseAccountingEndpoint(w, r)
	}
	return is
}

func refuseAccountingEndpoint(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusBadRequest, codeAccountingEndpointUsage, "accounting_policy_endpoint_required",
		domain.ErrAccountingPolicyRequired.Error(), nil)
}

// ── DraftAccountingPolicy ────────────────────────────────────────────────────

type ruleMappingRequest struct {
	RefKind    string `json:"ref_kind"`
	Ref        string `json:"ref"`
	RefVersion string `json:"ref_version"`
	Rationale  string `json:"rationale"`
	MapsTo     string `json:"maps_to,omitempty"`
}

type draftAccountingRequest struct {
	PolicyDomain    string                `json:"policy_domain"`
	Framework       string                `json:"framework"`
	BookID          *string               `json:"book_id,omitempty"`
	JurisdictionID  *string               `json:"jurisdiction_id,omitempty"`
	LegalEntityID   *string               `json:"legal_entity_id,omitempty"`
	SemanticVersion string                `json:"semantic_version"`
	Precedence      int                   `json:"precedence"`
	FrameworkSource string                `json:"framework_source"`
	PolicyTextRef   string                `json:"policy_text_ref"`
	RuleMappings    []ruleMappingRequest  `json:"rule_mappings,omitempty"`
	ImpactAnalysis  domain.ImpactAnalysis `json:"impact_analysis"`
	EffectiveFrom   time.Time             `json:"effective_from"`
	EffectiveTo     *time.Time            `json:"effective_to,omitempty"`
	Rationale       string                `json:"rationale"`
}

func blankToNil(s *string) *string {
	if s != nil && *s == "" {
		return nil
	}
	return s
}

// DraftAccountingPolicy handles POST /v1/accounting-policies.
//
// It drafts a version of the accounting policy for (policy_domain, book,
// jurisdiction) in the caller's tenant, optionally narrowed to one legal
// entity. The family is found or created by that applicability, so two drafts
// for the same applicability are versions of one policy. A draft is always
// DRAFT: it takes effect only through review, approval and activation by
// others.
//
//	201 drafted · 200 an identical draft of this semantic version already exists
//	400 invalid (the problems are listed) · 404 a rule mapping names a policy
//	version that does not exist · 409 this semantic version exists with different
//	content, or the book has a different framework
func (h *Handler) DraftAccountingPolicy(w http.ResponseWriter, r *http.Request) {
	s, ok := h.accounting(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req draftAccountingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.BookID, req.JurisdictionID = blankToNil(req.BookID), blankToNil(req.JurisdictionID)
	legalEntityID, ok := legalEntityFromRequest(w, r, req.LegalEntityID)
	if !ok {
		return
	}

	mappings := make([]domain.RuleMapping, len(req.RuleMappings))
	for i, m := range req.RuleMappings {
		mappings[i] = domain.RuleMapping{RefKind: m.RefKind, Ref: m.Ref, RefVersion: m.RefVersion, Rationale: m.Rationale, MapsTo: m.MapsTo}
	}
	meta := eventMeta(r, principalID)
	params := domain.DraftAccountingPolicyParams{
		TenantID: tenantID, PolicyDomain: req.PolicyDomain, Framework: req.Framework,
		BookID: req.BookID, JurisdictionID: req.JurisdictionID, LegalEntityID: legalEntityID,
		SemanticVersion: req.SemanticVersion, Precedence: req.Precedence,
		FrameworkSource: req.FrameworkSource, PolicyTextRef: req.PolicyTextRef,
		RuleMappings: mappings, Impact: req.ImpactAnalysis,
		EffectiveFrom: req.EffectiveFrom, EffectiveTo: req.EffectiveTo, Rationale: req.Rationale,
		CreatedByPrincipalID: principalID, RequestKey: meta.IdempotencyKey, Meta: meta,
	}
	if problems := params.Validate(h.opts.Now().UTC()); len(problems) > 0 {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_accounting_policy",
			domain.ErrInvalidAccountingPolicy.Error(), map[string]any{"errors": problems})
		return
	}
	if !h.authorize(w, r, principalID, authz.ActionAccountingPolicyDraft, legalEntityID) {
		return
	}

	v, created, err := s.CreateAccountingDraft(r.Context(), params)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrAccountingDependencyNotFound):
			writeError(w, r, http.StatusNotFound, codeNotFound, "dependency_not_found", err.Error(), nil)
		case errors.Is(err, domain.ErrIdempotencyMismatch):
			writeError(w, r, http.StatusConflict, codeIdempotencyMismatch, "accounting_policy_conflict",
				"this semantic version already exists for this applicability and scope with different content", nil)
		case errors.Is(err, domain.ErrConflict):
			writeError(w, r, http.StatusConflict, codeIdempotencyMismatch, "accounting_policy_conflict",
				"a policy already exists for this applicability with a different framework", nil)
		default:
			h.storeUnavailable(w, r, "DraftAccountingPolicy", err)
		}
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("ETag", `"`+strconv.Itoa(v.RowVersion)+`"`)
	writeJSON(w, status, v)
}

// ── GetPolicyVersion ─────────────────────────────────────────────────────────

// GetAccountingPolicyVersion handles GET /v1/accounting-policies/versions/{version_id}:
// the version with its applicability, what it rests on and its rule mappings.
func (h *Handler) GetAccountingPolicyVersion(w http.ResponseWriter, r *http.Request) {
	s, ok := h.accounting(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "version_id")
	if !requireUUID(w, r, "version_id", id) {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return
	}
	v, err := s.GetAccountingPolicyVersion(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrPolicyVersionNotFound) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "policy_version_not_found", "", map[string]any{"policy_version_id": id})
			return
		}
		h.storeUnavailable(w, r, "GetAccountingPolicyVersion", err)
		return
	}
	w.Header().Set("ETag", `"`+strconv.Itoa(v.RowVersion)+`"`)
	writeJSON(w, http.StatusOK, v)
}

// ── resolution ───────────────────────────────────────────────────────────────

type accountingQuery struct {
	tenantID       string
	legalEntityID  *string
	bookID         *string
	jurisdictionID *string
	policyDomain   string
	asOf, knownAt  time.Time
}

func (h *Handler) parseAccountingQuery(w http.ResponseWriter, r *http.Request, domainRequired bool) (accountingQuery, bool) {
	q := r.URL.Query()
	var out accountingQuery
	out.policyDomain = q.Get("policy_domain")
	if domainRequired && out.policyDomain == "" {
		missingField(w, r, "policy_domain")
		return out, false
	}
	if tooLong(w, r, "policy_domain", out.policyDomain, 64) {
		return out, false
	}
	var ok bool
	if out.tenantID, ok = h.requireTenant(w, r); !ok {
		return out, false
	}
	for name, dst := range map[string]**string{"book_id": &out.bookID, "jurisdiction_id": &out.jurisdictionID} {
		if v := q.Get(name); v != "" {
			vv := v
			*dst = &vv
		}
		if !optionalUUID(w, r, name, *dst) {
			return out, false
		}
	}
	var le *string
	if v := q.Get("legal_entity_id"); v != "" {
		le = &v
	}
	if out.legalEntityID, ok = legalEntityFromRequest(w, r, le); !ok {
		return out, false
	}

	parseTime := func(name string) (*time.Time, bool) {
		v := q.Get(name)
		if v == "" {
			return nil, true
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_"+name, name+" must be a valid RFC3339 timestamp", nil)
			return nil, false
		}
		return &t, true
	}
	asOf, ok := parseTime("as_of")
	if !ok {
		return out, false
	}
	knownAt, ok := parseTime("known_at")
	if !ok {
		return out, false
	}
	a, k, ok := resolve.Times(asOf, knownAt, h.opts.Now().UTC())
	if !ok {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_known_at", "known_at cannot be in the future", nil)
		return out, false
	}
	out.asOf, out.knownAt = a, k
	return out, true
}

// accountingVersionSummary is a version as resolution reports it: what it is,
// where it applies and the pin a consumer persists.
type accountingVersionSummary struct {
	Pin            domain.Pin `json:"pin"`
	PolicyDomain   string     `json:"policy_domain"`
	Framework      string     `json:"framework"`
	BookID         *string    `json:"book_id"`
	JurisdictionID *string    `json:"jurisdiction_id"`
	LegalEntityID  *string    `json:"legal_entity_id"`
	ScopeType      string     `json:"scope_type"`
	Precedence     int        `json:"precedence"`
	VersionStatus  string     `json:"version_status"`
	EffectiveFrom  time.Time  `json:"effective_from"`
	EffectiveTo    *time.Time `json:"effective_to"`
}

func summarise(c *domain.AccountingCandidate) accountingVersionSummary {
	return accountingVersionSummary{
		Pin: domain.Pin{
			PolicyID: c.PolicyID, PolicyVersionID: c.PolicyVersionID, VersionNumber: c.VersionNumber,
			SemanticVersion: c.SemanticVersion, ArtifactDigest: c.ArtifactDigest, RowVersion: c.RowVersion,
		},
		PolicyDomain: c.Applicability.PolicyDomain, Framework: c.Applicability.Framework,
		BookID: c.Applicability.BookID, JurisdictionID: c.Applicability.JurisdictionID, LegalEntityID: c.LegalEntityID,
		ScopeType: c.ScopeType, Precedence: c.Precedence, VersionStatus: c.VersionStatus,
		EffectiveFrom: c.EffectiveFrom, EffectiveTo: c.EffectiveTo,
	}
}

type accountingTraceEntry struct {
	resolve.TraceEntry
	PolicyDomain    string  `json:"policy_domain"`
	BookID          *string `json:"book_id"`
	JurisdictionID  *string `json:"jurisdiction_id"`
	SemanticVersion string  `json:"semantic_version"`
	Precedence      int     `json:"precedence"`
}

// AccountingResolution is the answer for one policy domain.
type AccountingResolution struct {
	Status string `json:"status"`
	// BlocksCalculation is true unless exactly one version governs: a
	// consumer must not calculate or post on any other answer.
	BlocksCalculation bool                       `json:"blocks_calculation"`
	PolicyDomain      string                     `json:"policy_domain"`
	AsOf              time.Time                  `json:"as_of"`
	KnownAt           time.Time                  `json:"known_at"`
	Pin               *domain.Pin                `json:"pin,omitempty"`
	Selected          *accountingVersionSummary  `json:"selected,omitempty"`
	Conflicting       []accountingVersionSummary `json:"conflicting,omitempty"`
	Trace             []accountingTraceEntry     `json:"trace,omitempty"`

	selectedID string
}

// resolveAccounting resolves one domain among candidates.
func resolveAccounting(policyDomain string, candidates []*domain.AccountingCandidate, asOf, knownAt time.Time) AccountingResolution {
	var inDomain []*domain.AccountingCandidate
	byVersion := map[*domain.ApplicablePolicyVersion]*domain.AccountingCandidate{}
	list := make([]*domain.ApplicablePolicyVersion, 0, len(candidates))
	for _, c := range candidates {
		if c.Applicability.PolicyDomain != policyDomain {
			continue
		}
		inDomain = append(inDomain, c)
		byVersion[&c.ApplicablePolicyVersion] = c
		list = append(list, &c.ApplicablePolicyVersion)
	}
	res := resolve.ResolveRanked(list, asOf, knownAt, func(v *domain.ApplicablePolicyVersion) []int {
		return byVersion[v].Rank()
	})

	out := AccountingResolution{
		Status: string(res.Status), BlocksCalculation: res.Status != resolve.Resolved,
		PolicyDomain: policyDomain, AsOf: res.AsOf, KnownAt: res.KnownAt,
	}
	switch res.Status {
	case resolve.Resolved:
		c := byVersion[res.Selected]
		pin := summarise(c).Pin
		sel := summarise(c)
		out.Pin, out.Selected, out.selectedID = &pin, &sel, c.PolicyVersionID
	case resolve.Conflicted:
		for _, v := range res.Conflicting {
			out.Conflicting = append(out.Conflicting, summarise(byVersion[v]))
		}
	}
	for i, t := range res.Trace {
		c := byVersion[list[i]]
		out.Trace = append(out.Trace, accountingTraceEntry{
			TraceEntry: t, PolicyDomain: c.Applicability.PolicyDomain, BookID: c.Applicability.BookID,
			JurisdictionID: c.Applicability.JurisdictionID, SemanticVersion: c.SemanticVersion, Precedence: c.Precedence,
		})
	}
	return out
}

func (h *Handler) accountingCandidates(w http.ResponseWriter, r *http.Request, s accountingStore, q accountingQuery, op string) ([]*domain.AccountingCandidate, bool) {
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, q.legalEntityID); !ok {
		return nil, false
	}
	cands, err := s.AccountingCandidates(r.Context(), domain.AccountingCandidateQuery{
		TenantID: q.tenantID, LegalEntityID: q.legalEntityID, BookID: q.bookID, JurisdictionID: q.jurisdictionID, PolicyDomain: q.policyDomain,
	})
	if err != nil {
		h.storeUnavailable(w, r, op, err)
		return nil, false
	}
	return cands, true
}

// ResolveAccountingPolicy handles GET /v1/accounting-policies/resolve.
//
//	policy_domain=X       required
//	book_id=Y             the accounting book; omit for a request that names none
//	jurisdiction_id=Z     omit for a request that names none
//	legal_entity_id=E     optional (or the X-Legal-Entity-Id header)
//	as_of=RFC3339         business time (default now)
//	known_at=RFC3339      knowledge cut (default: as_of if past, else now)
//
//	200 exactly one version governs: pin it on whatever you calculate or post
//	404 NO_APPLICABLE_POLICY · 409 POLICY_CONFLICTED: both with blocks_calculation
//	and resolution_status (status is the HTTP status of the problem document)
func (h *Handler) ResolveAccountingPolicy(w http.ResponseWriter, r *http.Request) {
	s, ok := h.accounting(w, r)
	if !ok {
		return
	}
	q, ok := h.parseAccountingQuery(w, r, true)
	if !ok {
		return
	}
	cands, ok := h.accountingCandidates(w, r, s, q, "ResolveAccountingPolicy")
	if !ok {
		return
	}
	res := resolveAccounting(q.policyDomain, cands, q.asOf, q.knownAt)
	res.Trace = nil

	switch res.Status {
	case string(resolve.Resolved):
		full, err := s.GetAccountingPolicyVersion(r.Context(), res.selectedID)
		if err != nil {
			h.storeUnavailable(w, r, "ResolveAccountingPolicy", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": res.Status, "blocks_calculation": false, "policy_domain": res.PolicyDomain,
			"as_of": res.AsOf, "known_at": res.KnownAt, "pin": res.Pin, "policy": full,
		})
	case string(resolve.Conflicted):
		writeError(w, r, http.StatusConflict, codePolicyConflicted, "accounting_policy_ambiguous",
			"more than one accounting policy applies equally: calculation is blocked until one is given a higher precedence",
			map[string]any{"resolution_status": res.Status, "blocks_calculation": true, "policy_domain": res.PolicyDomain, "as_of": res.AsOf, "conflicting": res.Conflicting})
	default:
		writeError(w, r, http.StatusNotFound, codeNoApplicablePolicy, "no_applicable_accounting_policy",
			"no accounting policy governs this request at that time: calculation is blocked",
			map[string]any{"resolution_status": res.Status, "blocks_calculation": true, "policy_domain": res.PolicyDomain, "as_of": res.AsOf})
	}
}

// ExplainAccountingResolution handles GET /v1/accounting-policies/explain: the
// resolution for a domain and, for every candidate version, whether it governs
// and why not when it does not. The explanation is the executed trace itself.
// Always 200: the status is the answer.
func (h *Handler) ExplainAccountingResolution(w http.ResponseWriter, r *http.Request) {
	s, ok := h.accounting(w, r)
	if !ok {
		return
	}
	q, ok := h.parseAccountingQuery(w, r, true)
	if !ok {
		return
	}
	cands, ok := h.accountingCandidates(w, r, s, q, "ExplainAccountingResolution")
	if !ok {
		return
	}
	res := resolveAccounting(q.policyDomain, cands, q.asOf, q.knownAt)
	if res.Trace == nil {
		res.Trace = []accountingTraceEntry{}
	}
	writeJSON(w, http.StatusOK, res)
}

// ListApplicableAccountingPolicies handles GET /v1/accounting-policies: for the
// entity, book, jurisdiction and date, the resolution of every policy domain
// that has a policy bound to the request, or of one domain with policy_domain.
// A domain whose policies are ambiguous is listed as CONFLICTED, not dropped.
func (h *Handler) ListApplicableAccountingPolicies(w http.ResponseWriter, r *http.Request) {
	s, ok := h.accounting(w, r)
	if !ok {
		return
	}
	q, ok := h.parseAccountingQuery(w, r, false)
	if !ok {
		return
	}
	cands, ok := h.accountingCandidates(w, r, s, q, "ListApplicableAccountingPolicies")
	if !ok {
		return
	}
	seen := map[string]bool{}
	var domains []string
	for _, c := range cands {
		if !seen[c.Applicability.PolicyDomain] {
			seen[c.Applicability.PolicyDomain] = true
			domains = append(domains, c.Applicability.PolicyDomain)
		}
	}
	sort.Strings(domains)
	out := make([]AccountingResolution, 0, len(domains))
	for _, d := range domains {
		res := resolveAccounting(d, cands, q.asOf, q.knownAt)
		res.Trace = nil
		out = append(out, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"as_of": q.asOf, "known_at": q.knownAt, "policies": out})
}
