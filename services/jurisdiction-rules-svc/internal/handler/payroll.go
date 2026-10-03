package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/resolver"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ZS-JUR-001 Wave 6 (payroll): the statutory parameter interface (s17).
//
// Payroll CALCULATION stays with the payroll product. This endpoint hands it
// the versioned, sourced, verified statutory parameters in force at a pay date,
// as one consistent set, and records exactly what it handed over.

func registerPayrollRoute(r chi.Router, h *Handler) {
	r.Post("/v1/payroll-statutory-parameters:resolve", h.ResolvePayrollParameters)
}

type PayrollParametersRequest struct {
	Jurisdiction string `json:"jurisdiction"`
	// EffectiveAt is the pay date (or the instant the parameters must be in force).
	EffectiveAt string `json:"effective_at"`
	// Classes limits the answer to s17 classes; empty means all.
	Classes     []string `json:"classes"`
	PackRef     string   `json:"pack_ref"`
	PackVersion string   `json:"pack_version"`
}

// ResolvePayrollParameters resolves every PAYROLL statutory parameter in force
// at the instant from verified released packs.
//
//	200 PARAMETERS_RESOLVED or NO_RULE      409 AMBIGUOUS (no partial set is ever returned)
//	422 UNSUPPORTED_JURISDICTION            503 pack_unverified
func (h *Handler) ResolvePayrollParameters(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "payroll_parameters", "resolve")
	if !ok {
		return
	}
	var req PayrollParametersRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction", req.Jurisdiction}, requiredField{"effective_at", req.EffectiveAt}); field != "" {
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
	canon := req
	canon.EffectiveAt = at.UTC().Format(time.RFC3339Nano)
	rawReq, _ := json.Marshal(canon)
	reqDigest, _ := domain.DigestOf(rawReq)

	corr := r.Header.Get("X-Correlation-ID")
	set, err := h.resolver.ResolveParameterSet(r.Context(), resolver.ParameterSetRequest{Jurisdiction: req.Jurisdiction, At: at,
		Classes: req.Classes, PackRef: req.PackRef, PackVersion: req.PackVersion})
	if err != nil {
		h.writeResolveError(w, err, corr)
		return
	}
	respJSON, err := json.Marshal(set)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_failed", "could not encode the parameter set")
		return
	}
	rec := store.DecisionRecord{Kind: "PARAMETER_SET", RequestedBy: actor, CorrelationID: corr, IdempotencyKey: idemKey, RequestDigest: reqDigest,
		RequestJSON: rawReq, EffectiveAt: at.UTC(), Outcome: set.Outcome, ResponseJSON: respJSON}
	rec.ResolverRing, rec.ResolverRegion = h.resolver.Scope()
	if set.BundleDigest != "" {
		rec.RuleContentDigest = &set.BundleDigest
		// When the whole set comes from one pack release, anchor the row on it too.
		first := set.Items[0].Pack
		same := true
		for _, it := range set.Items {
			same = same && it.Pack.PackVersionID == first.PackVersionID
		}
		if same {
			rec.PackRef, rec.PackVersion, rec.PackVersionID, rec.ArtifactDigest = &first.PackRef, &first.Version, &first.PackVersionID, &first.ArtifactDigest
			if first.CertificationID != "" {
				rec.CertificationID = &first.CertificationID
			}
		}
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
	case domain.OutcomeUnsupported:
		status = http.StatusUnprocessableEntity
	case domain.OutcomeAmbiguous:
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"decision_id": stored.DecisionID, "replayed": replayed, "parameter_set": json.RawMessage(stored.Response)})
}
