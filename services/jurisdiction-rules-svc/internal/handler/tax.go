package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/resolver"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ZS-JUR-001 Wave 4 (tax half): rule parameters and the regulatory
// calculation API (s11, s12, s25).
//
// DECISION: ZS-JUR-001 governs, so rates, thresholds and rounding live in the
// rule's typed `parameters`, validated here and frozen with the rule, and reach
// runtime only inside signed, tested, certified, released packs. They are
// executed with exact decimal arithmetic. See
// docs/architecture/jurisdiction-pack-decision-rule-parameters.md.

func registerTaxRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/rules/{jurisdiction_rule_id}/parameters", h.GetRuleParameters)
	r.Put("/v1/admin/rules/{jurisdiction_rule_id}/parameters", h.SetRuleParameters)
}

func registerCalculationRoute(r chi.Router, h *Handler) {
	r.Post("/v1/regulatory-calculations:execute", h.ExecuteCalculation)
}

func (h *Handler) writeParametersError(w http.ResponseWriter, err error, corr string) {
	if errors.Is(err, domain.ErrParametersInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_parameters", err.Error())
		return
	}
	h.writeRegistryError(w, err, corr)
}

type SetRuleParametersRequest struct {
	// Parameters is required; an explicit null clears them.
	Parameters json.RawMessage `json:"parameters"`
}

// SetRuleParameters replaces the typed calculation parameters of a DRAFT rule.
func (h *Handler) SetRuleParameters(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_rule", "set_parameters")
	if !ok {
		return
	}
	var req SetRuleParametersRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Parameters) == 0 { // absent; an explicit null arrives as the bytes "null"
		writeMissingField(w, "parameters")
		return
	}
	var params json.RawMessage
	if string(req.Parameters) != "null" {
		params = req.Parameters
	}
	id := chi.URLParam(r, "jurisdiction_rule_id")
	out, err := h.registry.SetRuleParameters(r.Context(), id, params, actor)
	if err != nil {
		h.writeParametersError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jurisdiction_rule_id": id, "parameters": json.RawMessage(nullOrRaw(out))})
}

func nullOrRaw(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}

func (h *Handler) GetRuleParameters(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "jurisdiction_rule_id")
	out, err := h.registry.GetRuleParameters(r.Context(), id)
	if err != nil {
		h.writeParametersError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jurisdiction_rule_id": id, "parameters": json.RawMessage(nullOrRaw(out))})
}

// ── calculation ─────────────────────────────────────────────────────────────

// ExecuteCalculationRequest carries FACTS only: the rule, the pack release and
// the parameters are chosen by the server.
type ExecuteCalculationRequest struct {
	Jurisdiction  string `json:"jurisdiction"`
	RuleDomain    string `json:"rule_domain"`
	RuleCode      string `json:"rule_code"`
	EffectiveAt   string `json:"effective_at"`
	TaxableAmount string `json:"taxable_amount"`
	Currency      string `json:"currency"`
	PackRef       string `json:"pack_ref"`
	PackVersion   string `json:"pack_version"`
}

var currencyShapeRe = regexp.MustCompile(`^[A-Z]{3}$`)

// CalculationResponse is the stored and returned body of a calculation.
type CalculationResponse struct {
	Outcome       string             `json:"outcome"`
	Currency      string             `json:"currency"`
	TaxableAmount string             `json:"taxable_amount"`
	Resolution    *resolver.Decision `json:"resolution"`
	Tax           *domain.TaxResult  `json:"tax,omitempty"`
}

// ExecuteCalculation resolves the applicable rule from verified released packs
// and applies its parameters to a taxable amount with exact decimal arithmetic.
// The inputs, every step, the result and the rule and pack versions are
// recorded as evidence (s12).
//
//	200 CALCULATED or NO_RULE      409 AMBIGUOUS
//	422 UNSUPPORTED_JURISDICTION, NO_PARAMETERS, INVALID_FACTS      503 pack_unverified
func (h *Handler) ExecuteCalculation(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "regulatory_calculation", "execute")
	if !ok {
		return
	}
	var req ExecuteCalculationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction", req.Jurisdiction}, requiredField{"rule_domain", req.RuleDomain},
		requiredField{"rule_code", req.RuleCode}, requiredField{"effective_at", req.EffectiveAt},
		requiredField{"taxable_amount", req.TaxableAmount}, requiredField{"currency", req.Currency}); field != "" {
		writeMissingField(w, field)
		return
	}
	if !currencyShapeRe.MatchString(req.Currency) {
		writeError(w, http.StatusBadRequest, "invalid_currency", "currency must be a three-letter upper-case code (its precision is the rule's rounding scale; no currency registry exists to validate it)")
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
	canon := req
	canon.EffectiveAt = at.UTC().Format(time.RFC3339Nano)
	rawReq, _ := json.Marshal(canon)
	reqDigest, _ := domain.DigestOf(rawReq)

	corr := r.Header.Get("X-Correlation-ID")
	d, err := h.resolver.Resolve(r.Context(), resolver.Request{Jurisdiction: req.Jurisdiction, RuleDomain: req.RuleDomain,
		RuleCode: req.RuleCode, At: at, PackRef: req.PackRef, PackVersion: req.PackVersion})
	if err != nil {
		h.writeResolveError(w, err, corr)
		return
	}

	out := CalculationResponse{Outcome: d.Outcome, Currency: req.Currency, TaxableAmount: req.TaxableAmount, Resolution: d}
	if d.Outcome == domain.OutcomeResolved && d.Rule != nil {
		if len(d.Rule.Parameters) == 0 {
			out.Outcome = domain.CalcNoParameters
		} else {
			p, perr := domain.ParseRuleParameters(d.Rule.Parameters)
			if perr != nil {
				// A released artifact should never carry invalid parameters; if one does, say so and compute nothing.
				t := domain.TaxResult{Outcome: domain.CalcInvalidFacts, Message: perr.Error(), Steps: []string{perr.Error()}}
				out.Outcome, out.Tax = domain.CalcInvalidFacts, &t
			} else {
				t := domain.CalculateTax(p, req.TaxableAmount)
				out.Outcome, out.Tax = t.Outcome, &t
			}
		}
	}

	respJSON, err := json.Marshal(out)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_failed", "could not encode the calculation")
		return
	}
	rec := store.DecisionRecord{Kind: "CALCULATION", RequestedBy: actor, CorrelationID: corr, IdempotencyKey: idemKey, RequestDigest: reqDigest,
		RequestJSON: rawReq, EffectiveAt: at.UTC(), Outcome: out.Outcome, ResponseJSON: respJSON}
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
		h.writeRegistryError(w, err, corr)
		return
	}
	status := http.StatusOK
	switch stored.Outcome {
	case domain.OutcomeUnsupported, domain.CalcNoParameters, domain.CalcInvalidFacts, domain.CalcParameterOnly:
		status = http.StatusUnprocessableEntity
	case domain.OutcomeAmbiguous:
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"decision_id": stored.DecisionID, "replayed": replayed, "calculation": json.RawMessage(stored.Response)})
}
