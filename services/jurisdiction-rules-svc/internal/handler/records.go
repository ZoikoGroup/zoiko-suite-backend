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

// ZS-JUR-001 Wave 6: retention rules (s18) and accounting and reporting
// mappings (s19). Both are rules of a closed parameter family served from
// verified released packs. This service decides retention ELIGIBILITY only (it
// deletes nothing; a legal hold always overrides) and classifies codes (it never
// posts to a ledger: the Accounting Kernel stays the posting authority).

func registerRecordsRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/retention-rules:resolve", h.ResolveRetention)
	r.Post("/v1/accounting-mappings:resolve", h.ResolveMapping)
}

type RetentionRequest struct {
	Jurisdiction string `json:"jurisdiction"`
	RecordClass  string `json:"record_class"`
	// TriggerDate is the date the retention period starts from (YYYY-MM-DD). The rule in
	// force on that date is the one that applies.
	TriggerDate string `json:"trigger_date"`
	// Trigger optionally states which event the caller measures from; a mismatch is refused.
	Trigger string `json:"trigger"`
	// TenantMinimum is the tenant's own retention policy. It may extend, never shorten (JUR-NEG-12).
	TenantMinimum *domain.Duration `json:"tenant_minimum"`
	PackRef       string           `json:"pack_ref"`
	PackVersion   string           `json:"pack_version"`
}

type RetentionResponse struct {
	Outcome    string                  `json:"outcome"`
	Resolution *resolver.Decision      `json:"resolution"`
	Retention  *domain.RetentionResult `json:"retention,omitempty"`
}

// ResolveRetention answers how long a record class must be kept, and until when.
//
//	200 RETENTION_RESOLVED or NO_RULE      409 AMBIGUOUS
//	422 UNSUPPORTED_JURISDICTION, TENANT_POLICY_TOO_SHORT, TRIGGER_MISMATCH, NO_PARAMETERS      503 pack_unverified
func (h *Handler) ResolveRetention(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "retention_rule", "resolve")
	if !ok {
		return
	}
	var req RetentionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction", req.Jurisdiction}, requiredField{"record_class", req.RecordClass},
		requiredField{"trigger_date", req.TriggerDate}); field != "" {
		writeMissingField(w, field)
		return
	}
	day, err := time.Parse("2006-01-02", req.TriggerDate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_trigger_date", "trigger_date must be a date, YYYY-MM-DD")
		return
	}
	if req.Trigger != "" && !contains(domain.RetentionTriggers, req.Trigger) {
		writeError(w, http.StatusBadRequest, "invalid_trigger", "trigger must be one of "+strings.Join(domain.RetentionTriggers, ", "))
		return
	}
	if req.TenantMinimum != nil && !req.TenantMinimum.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_tenant_minimum", "tenant_minimum needs years 0-100, months 0-11, days 0-365 and a positive total")
		return
	}
	idemKey, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	rawReq, _ := json.Marshal(req)
	d, err := h.resolver.Resolve(r.Context(), resolver.Request{Jurisdiction: req.Jurisdiction, RuleDomain: domain.RetentionDomain,
		RuleCode: req.RecordClass, At: day, PackRef: req.PackRef, PackVersion: req.PackVersion})
	if err != nil {
		h.writeResolveError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	out := RetentionResponse{Outcome: d.Outcome, Resolution: d}
	if d.Outcome == domain.OutcomeResolved && d.Rule != nil {
		p, perr := domain.ParseRuleParameters(d.Rule.Parameters)
		if len(d.Rule.Parameters) == 0 || perr != nil || p.Family != domain.FamilyRetention {
			out.Outcome = domain.CalcNoParameters
		} else {
			res := domain.DetermineRetention(p, day, req.Trigger, req.TenantMinimum)
			out.Outcome, out.Retention = res.Outcome, &res
		}
	}
	h.finishRecordDecision(w, r, "RETENTION", actor, idemKey, rawReq, day, d, out.Outcome, out, "retention")
}

type MappingRequest struct {
	Jurisdiction string `json:"jurisdiction"`
	// MappingCode names the mapping (the rule code in the ACCOUNTING_MAPPING domain).
	MappingCode string `json:"mapping_code"`
	EffectiveAt string `json:"effective_at"`
	// From is one code to look up; empty returns the whole mapping.
	From        string `json:"from"`
	PackRef     string `json:"pack_ref"`
	PackVersion string `json:"pack_version"`
}

type MappingResponse struct {
	Outcome    string                `json:"outcome"`
	Resolution *resolver.Decision    `json:"resolution"`
	Mapping    *domain.MappingResult `json:"mapping,omitempty"`
}

// ResolveMapping answers an accounting or reporting mapping from the pack in force.
// It classifies; it never posts.
//
//	200 MAPPING_RESOLVED or NO_RULE      409 AMBIGUOUS
//	422 UNSUPPORTED_JURISDICTION, MAPPING_ENTRY_NOT_FOUND, NO_PARAMETERS      503 pack_unverified
func (h *Handler) ResolveMapping(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "accounting_mapping", "resolve")
	if !ok {
		return
	}
	var req MappingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"jurisdiction", req.Jurisdiction}, requiredField{"mapping_code", req.MappingCode},
		requiredField{"effective_at", req.EffectiveAt}); field != "" {
		writeMissingField(w, field)
		return
	}
	at, err := time.Parse(time.RFC3339Nano, req.EffectiveAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_effective_at", "effective_at must be an RFC3339 instant")
		return
	}
	idemKey, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	canon := req
	canon.EffectiveAt = at.UTC().Format(time.RFC3339Nano)
	rawReq, _ := json.Marshal(canon)
	d, err := h.resolver.Resolve(r.Context(), resolver.Request{Jurisdiction: req.Jurisdiction, RuleDomain: domain.MappingDomain,
		RuleCode: req.MappingCode, At: at, PackRef: req.PackRef, PackVersion: req.PackVersion})
	if err != nil {
		h.writeResolveError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	out := MappingResponse{Outcome: d.Outcome, Resolution: d}
	if d.Outcome == domain.OutcomeResolved && d.Rule != nil {
		p, perr := domain.ParseRuleParameters(d.Rule.Parameters)
		if len(d.Rule.Parameters) == 0 || perr != nil || p.Family != domain.FamilyMapping {
			out.Outcome = domain.CalcNoParameters
		} else {
			res := domain.LookupMapping(p, req.From)
			out.Outcome, out.Mapping = res.Outcome, &res
		}
	}
	h.finishRecordDecision(w, r, "MAPPING", actor, idemKey, rawReq, at.UTC(), d, out.Outcome, out, "mapping")
}

// idempotencyKey reads and bounds the Idempotency-Key header.
func (h *Handler) idempotencyKey(w http.ResponseWriter, r *http.Request) (*string, bool) {
	k := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if k == "" {
		return nil, true
	}
	if len(k) > maxIdempotencyKey {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is too long")
		return nil, false
	}
	return &k, true
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// finishRecordDecision records the evidence and writes the response.
func (h *Handler) finishRecordDecision(w http.ResponseWriter, r *http.Request, kind, actor string, idemKey *string, rawReq []byte, at time.Time,
	d *resolver.Decision, outcome string, body any, key string) {
	corr := r.Header.Get("X-Correlation-ID")
	reqDigest, _ := domain.DigestOf(rawReq)
	respJSON, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_failed", "could not encode the answer")
		return
	}
	rec := store.DecisionRecord{Kind: kind, RequestedBy: actor, CorrelationID: corr, IdempotencyKey: idemKey, RequestDigest: reqDigest,
		RequestJSON: rawReq, EffectiveAt: at, Outcome: outcome, ResponseJSON: respJSON}
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
	case domain.OutcomeUnsupported, domain.CalcNoParameters, domain.RetentionTenantTooShort, domain.RetentionTriggerMismatch, domain.MappingEntryNotFound,
		domain.DependencyUnknown, domain.DependencyExpired, domain.ApprovalMissing:
		status = http.StatusUnprocessableEntity
	case domain.OutcomeAmbiguous:
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"decision_id": stored.DecisionID, "replayed": replayed, key: json.RawMessage(stored.Response)})
}
