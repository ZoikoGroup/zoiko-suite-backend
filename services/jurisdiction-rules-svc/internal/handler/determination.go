package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ZS-JUR-001 s9 and s25: POST /v1/jurisdiction-decisions:resolve.
//
// Mounted when both the entity applicability projection and the registry are wired. The
// decision rests on the entity's authorised assignments (tenant data projected from
// tenant-entity-registry-svc); a claimed jurisdiction is an input fact that can select among
// them and never add one (JUR-NEG-05). Place of supply and other transaction-level rules do
// not exist yet, so this never decides them.

const determinationPath = "/v1/jurisdiction-decisions:resolve"

type JurisdictionDecisionRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	// Purpose is TAX, FILING or ANY.
	Purpose string `json:"purpose"`
	// EffectiveAt is the instant the assignments must be in force (RFC 3339).
	EffectiveAt string `json:"effective_at"`
	// ClaimedJurisdiction is the caller's own view (a jurisdiction code). It is checked, never trusted.
	ClaimedJurisdiction string `json:"claimed_jurisdiction"`
}

// recordedDecisionRequest is what the platform-wide evidence ledger keeps of the request: a digest of
// tenant and entity, never their ids, because the ledger is not tenant-scoped.
type recordedDecisionRequest struct {
	EntityRefDigest     string `json:"entity_ref_digest"`
	Purpose             string `json:"purpose"`
	EffectiveAt         string `json:"effective_at"`
	ClaimedJurisdiction string `json:"claimed_jurisdiction,omitempty"`
}

// ResolveJurisdictionDecision determines the jurisdiction that applies to a legal entity.
//
//	200 JURISDICTION_DETERMINED      409 AMBIGUOUS_JURISDICTION or idempotency_conflict
//	422 NO_APPLICABLE_JURISDICTION or CLAIM_NOT_AUTHORISED      401 no X-Tenant-Id      503 store unavailable
func (h *Handler) ResolveJurisdictionDecision(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_decision", "resolve")
	if !ok {
		return
	}
	tenantID := strings.TrimSpace(r.Header.Get("X-Tenant-Id"))
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "missing_tenant", "X-Tenant-Id is required: entity jurisdiction assignments are tenant data")
		return
	}
	var req JurisdictionDecisionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"legal_entity_id", req.LegalEntityID}, requiredField{"purpose", req.Purpose},
		requiredField{"effective_at", req.EffectiveAt}); field != "" {
		writeMissingField(w, field)
		return
	}
	if len(req.LegalEntityID) > 128 || len(req.ClaimedJurisdiction) > 64 {
		writeError(w, http.StatusBadRequest, "invalid_request", "legal_entity_id is at most 128 characters and claimed_jurisdiction at most 64")
		return
	}
	if !domain.ValidPurpose(req.Purpose) {
		writeError(w, http.StatusBadRequest, "invalid_purpose", "purpose must be one of TAX, FILING, ANY")
		return
	}
	at, err := time.Parse(time.RFC3339Nano, req.EffectiveAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_effective_at", "effective_at must be an RFC3339 instant")
		return
	}
	at = at.UTC()
	idemKey, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	corr := r.Header.Get("X-Correlation-ID")

	rows, err := h.entities.ListEntityJurisdictions(r.Context(), tenantID, req.LegalEntityID, at)
	if err != nil {
		h.writeStoreError(w, err, corr)
		return
	}
	var assignments []domain.Assignment
	for _, a := range rows {
		j, err := h.store.FindByID(r.Context(), a.JurisdictionID)
		if errors.Is(err, domain.ErrJurisdictionNotFound) || (err == nil && !j.ActiveFlag) {
			continue // a deactivated or unknown jurisdiction is not an authorised one
		}
		if err != nil {
			h.writeStoreError(w, err, corr)
			return
		}
		assignments = append(assignments, domain.Assignment{JurisdictionID: a.JurisdictionID, JurisdictionCode: j.JurisdictionCode,
			ApplicabilityID: a.ApplicabilityID, ApplicabilityType: a.ApplicabilityType, EffectiveFrom: a.EffectiveFrom, EffectiveTo: a.EffectiveTo,
			SourceEventID: a.SourceEventID, SourceObjectVersion: a.SourceObjectVersion})
	}

	det := domain.DetermineJurisdiction(assignments, req.Purpose, req.ClaimedJurisdiction, at)
	for i := range det.Candidates {
		v, err := h.registry.JurisdictionSupport(r.Context(), det.Candidates[i].JurisdictionCode)
		if err != nil {
			h.writeRegistryError(w, err, corr)
			return
		}
		det.Candidates[i].Support = v.Outcome
		if det.Jurisdiction != nil && det.Jurisdiction.JurisdictionID == det.Candidates[i].JurisdictionID {
			c := det.Candidates[i]
			det.Jurisdiction = &c
		}
	}

	entityDigest := domain.IdentifierDigest(tenantID + "\x00" + req.LegalEntityID)
	rawReq, _ := json.Marshal(recordedDecisionRequest{EntityRefDigest: entityDigest, Purpose: req.Purpose,
		EffectiveAt: at.Format(time.RFC3339Nano), ClaimedJurisdiction: strings.TrimSpace(req.ClaimedJurisdiction)})
	reqDigest, _ := domain.DigestOf(rawReq)
	respJSON, err := json.Marshal(det)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_failed", "could not encode the answer")
		return
	}
	stored, replayed, err := h.registry.RecordDecision(r.Context(), store.DecisionRecord{Kind: "JURISDICTION", RequestedBy: actor, CorrelationID: corr,
		IdempotencyKey: idemKey, RequestDigest: reqDigest, RequestJSON: rawReq, EffectiveAt: at, Outcome: det.Outcome, ResponseJSON: respJSON})
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyConflict) {
			writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
			return
		}
		h.writeRegistryError(w, err, corr)
		return
	}
	if !replayed {
		payload := map[string]any{"decision_id": stored.DecisionID, "outcome": det.Outcome, "purpose": det.Purpose, "entity_ref_digest": entityDigest}
		if det.Jurisdiction != nil {
			payload["jurisdiction_code"] = det.Jurisdiction.JurisdictionCode
		}
		h.emitRegistry(r, events.EventJurisdictionDecisionResolved, stored.DecisionID, actor, payload)
	}

	status := http.StatusOK
	switch stored.Outcome {
	case domain.OutcomeJurisdictionAmbiguous:
		status = http.StatusConflict
	case domain.OutcomeNoApplicable, domain.OutcomeClaimNotAuthorised:
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]any{"decision_id": stored.DecisionID, "replayed": replayed, "determination": json.RawMessage(stored.Response)})
}
