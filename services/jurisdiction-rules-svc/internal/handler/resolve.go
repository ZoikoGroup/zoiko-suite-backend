package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/problem"
	"zoiko.io/jurisdiction-rules-svc/internal/resolver"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ZS-JUR-001 Wave 2: the runtime rule-resolution API (s25).
//
// Scope note: s25 also lists POST /jurisdiction-decisions:resolve and
// POST /regulatory-calculations:execute. They are NOT built: the first needs
// authoritative entity/counterparty/transaction facts and product rules for
// place of supply that do not exist yet, the second needs a rule formula
// language (s38). Answering either today would be inventing a legal
// conclusion, so there is no endpoint rather than a fake one.

// WithResolver enables the runtime resolution routes.
func (h *Handler) WithResolver(r *resolver.Resolver) *Handler {
	h.resolver = r
	return h
}

func registerResolverRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/rule-resolutions:resolve", h.ResolveRule)
	registerObligationCalcRoute(r, h)
	registerCalculationRoute(r, h)
	registerPayrollRoute(r, h)
	registerRecordsRoutes(r, h)
	registerSubmissionRoutes(r, h)
	r.Get("/v1/rule-decisions/{decision_id}", h.GetRuleDecision)
	r.Get("/v1/pack-coverage", h.PackCoverage)
	r.Post("/v1/admin/resolver-cache:invalidate", h.InvalidateResolverCache)
}

// ResolveRuleRequest is the body of POST /v1/rule-resolutions:resolve.
// The client supplies FACTS (a jurisdiction code is a fact, not a
// conclusion) and an instant; the server selects the pack version and rule.
type ResolveRuleRequest struct {
	Jurisdiction string `json:"jurisdiction"`
	RuleDomain   string `json:"rule_domain"`
	RuleCode     string `json:"rule_code"`
	EffectiveAt  string `json:"effective_at"`
	// PackRef restricts the search to one pack. With PackVersion too, that
	// exact version is replayed (a past decision's pack, for reproduction).
	PackRef     string `json:"pack_ref"`
	PackVersion string `json:"pack_version"`
}

const maxIdempotencyKey = 128

// ResolveRule resolves the applicable rule from verified, released pack
// artifacts and records the decision as evidence.
//
//	200 RESOLVED or NO_RULE      409 AMBIGUOUS (blocked, not guessed)
//	422 UNSUPPORTED_JURISDICTION 503 pack_unverified (fail closed)
func (h *Handler) ResolveRule(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "rule_resolution", "resolve")
	if !ok {
		return
	}
	var req ResolveRuleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction", req.Jurisdiction}, requiredField{"rule_domain", req.RuleDomain},
		requiredField{"rule_code", req.RuleCode}, requiredField{"effective_at", req.EffectiveAt}); field != "" {
		writeMissingField(w, field)
		return
	}
	at, err := time.Parse(time.RFC3339Nano, req.EffectiveAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_effective_at", "effective_at must be an RFC3339 instant")
		return
	}
	var idemKey *string
	if k := strings.TrimSpace(r.Header.Get("Idempotency-Key")); k != "" {
		if len(k) > maxIdempotencyKey {
			writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is too long")
			return
		}
		idemKey = &k
	}

	// The digest binds an idempotency key to one question, in canonical form.
	canonReq := ResolveRuleRequest{Jurisdiction: req.Jurisdiction, RuleDomain: req.RuleDomain, RuleCode: req.RuleCode,
		EffectiveAt: at.UTC().Format(time.RFC3339Nano), PackRef: req.PackRef, PackVersion: req.PackVersion}
	rawReq, _ := json.Marshal(canonReq)
	reqDigest, _ := domain.DigestOf(rawReq)

	corr := r.Header.Get("X-Correlation-ID")
	d, err := h.resolver.Resolve(r.Context(), resolver.Request{Jurisdiction: req.Jurisdiction, RuleDomain: req.RuleDomain,
		RuleCode: req.RuleCode, At: at, PackRef: req.PackRef, PackVersion: req.PackVersion})
	if err != nil {
		h.writeResolveError(w, err, corr)
		return
	}

	respJSON, err := json.Marshal(d)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_failed", "could not encode the decision")
		return
	}
	rec := store.DecisionRecord{RequestedBy: actor, CorrelationID: corr, IdempotencyKey: idemKey, RequestDigest: reqDigest,
		RequestJSON: rawReq, EffectiveAt: at.UTC(), Outcome: d.Outcome, ResponseJSON: respJSON}
	rec.ResolverRing, rec.ResolverRegion = h.resolver.Scope()
	if d.Pack != nil {
		rec.PackRef, rec.PackVersion, rec.PackVersionID = &d.Pack.PackRef, &d.Pack.Version, &d.Pack.PackVersionID
		rec.ArtifactDigest = &d.Pack.ArtifactDigest
		if d.Pack.CertificationID != "" {
			rec.CertificationID = &d.Pack.CertificationID
		}
	}
	if d.Rule != nil {
		rec.RuleID, rec.RuleContentDigest = &d.Rule.RuleID, &d.Rule.ContentDigest
	}
	if d.Basis != "" {
		rec.Basis = &d.Basis
	}
	stored, replayed, err := h.registry.RecordDecision(r.Context(), rec)
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyConflict) {
			writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
			return
		}
		// Evidence is mandatory: an answer that cannot be recorded is not given.
		h.writeRegistryError(w, err, corr)
		return
	}

	status := http.StatusOK
	switch stored.Outcome {
	case domain.OutcomeUnsupported:
		status = http.StatusUnprocessableEntity
	case domain.OutcomeAmbiguous:
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"decision_id": stored.DecisionID, "replayed": replayed, "decision": json.RawMessage(stored.Response)})
}

func (h *Handler) writeResolveError(w http.ResponseWriter, err error, corr string) {
	var ue *resolver.UnverifiedError
	switch {
	case errors.As(err, &ue):
		problem.Write(w, problem.New(http.StatusServiceUnavailable, "pack_unverified",
			"a pack covering this jurisdiction failed verification; no answer is given rather than a different one").
			With("failures", ue.Failures))
	case errors.Is(err, resolver.ErrBadRequest):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, resolver.ErrPinnedNotEligible):
		writeError(w, http.StatusConflict, "pinned_pack_not_eligible", err.Error())
	case errors.Is(err, resolver.ErrPackNotFound), errors.Is(err, domain.ErrPackVersionNotFound):
		writeError(w, http.StatusNotFound, "pack_version_not_found", "no such pack version")
	default:
		h.writeRegistryError(w, err, corr)
	}
}

// GetRuleDecision returns a recorded decision (the evidence of a past resolution).
func (h *Handler) GetRuleDecision(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "rule_decision", "view"); !ok {
		return
	}
	d, err := h.registry.GetDecision(r.Context(), chi.URLParam(r, "decision_id"))
	if err != nil {
		if errors.Is(err, domain.ErrDecisionNotFound) {
			writeError(w, http.StatusNotFound, "decision_not_found", err.Error())
			return
		}
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// PackCoverage lists the eligible pack versions that name a jurisdiction.
// A read model of what is supported; it states no legal conclusion.
func (h *Handler) PackCoverage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	j := strings.TrimSpace(q.Get("jurisdiction"))
	if j == "" {
		writeMissingField(w, "jurisdiction")
		return
	}
	at := time.Now().UTC()
	if v := q.Get("effective_at"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_effective_at", "effective_at must be an RFC3339 instant")
			return
		}
		at = t
	}
	entries, err := h.resolver.Coverage(r.Context(), j, at)
	if err != nil {
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jurisdiction": j, "effective_at": at, "packs": entries})
}

// InvalidateResolverCache drops every cached pack so the next resolution
// re-reads and re-verifies. Use it after revoking a key or withdrawing a pack.
func (h *Handler) InvalidateResolverCache(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "resolver_cache", "invalidate"); !ok {
		return
	}
	h.resolver.Invalidate()
	hits, misses, refreshes := h.resolver.Stats()
	writeJSON(w, http.StatusOK, map[string]any{"invalidated": true, "cache_hits": hits, "cache_misses": misses, "eligible_list_refreshes": refreshes})
}
