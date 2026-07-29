// Package handler exposes evidence-requirements-svc's REST API.
//
// Route split follows jurisdiction-rules-svc's proven shape: public read
// paths plus evaluation, and a separate /v1/admin/... namespace for catalog
// mutation which is authorization-gated.
//
// Why /v1/evidence/evaluate is NOT authz-gated: it is a precondition query,
// not a material action. It mutates nothing a caller owns — its only write is
// an append-only record of its own determination. Gating it would require
// every calling service's principal to hold an extra permission, and an
// authorization-svc blip would then fail-closed a finalization for a reason
// unrelated to evidence. jurisdiction-rules-svc draws the same line: reads
// and rule evaluation open, /v1/admin/* gated. Catalog mutation IS a material
// governance act and is gated accordingly — this service being part of the
// Governance Plane is not a self-authorization exemption.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateRequirement(ctx context.Context, r *domain.EvidenceRequirement) (created bool, err error)
	GetRequirement(ctx context.Context, requirementID string) (*domain.EvidenceRequirement, error)
	ListRequirements(ctx context.Context, filter domain.ListRequirementsFilter) ([]domain.EvidenceRequirement, error)
	EffectiveRequirements(ctx context.Context, tenantID, legalEntityID, domainCode, actionType string, asOf time.Time) ([]domain.EvidenceRequirement, error)
	EndDateRequirement(ctx context.Context, tenantID, requirementID string, effectiveTo time.Time, reason, actorPrincipalID string) (*domain.EvidenceRequirement, error)
	RecordEvaluation(ctx context.Context, e *domain.EvidenceEvaluation) (created bool, err error)
	GetEvaluation(ctx context.Context, evaluationID string) (*domain.EvidenceEvaluation, error)
}

// Publisher is the event-publishing contract the handler depends on.
type Publisher interface {
	PublishEvaluation(ctx context.Context, e domain.EvidenceEvaluation)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// DocumentVaultClient is the artifact-verification contract the evaluator
// depends on — see internal/documentvault's package doc for why asserted
// artifacts are verified rather than trusted.
type DocumentVaultClient interface {
	VerifyDocument(ctx context.Context, tenantID, legalEntityID, documentID string) error
}

// Action types checked against authorization-svc for catalog mutation.
const (
	actionCreateRequirement = "EVIDENCE_REQUIREMENT_CREATE"
	actionRetireRequirement = "EVIDENCE_REQUIREMENT_RETIRE"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	docs      DocumentVaultClient
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, docs DocumentVaultClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, docs: docs, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/evidence", func(r chi.Router) {
		r.Post("/evaluate", h.Evaluate)
		r.Get("/evaluations/{evaluation_id}", h.GetEvaluation)
	})
	r.Route("/v1/evidence-requirements", func(r chi.Router) {
		r.Get("/", h.ListRequirements)
		r.Get("/{evidence_requirement_id}", h.GetRequirement)
	})
	// Catalog mutation. Authorization-gated — see package doc.
	r.Route("/v1/admin/evidence-requirements", func(r chi.Router) {
		r.Post("/", h.CreateRequirement)
		r.Post("/{evidence_requirement_id}/end-date", h.EndDateRequirement)
	})
}

// ── POST /v1/evidence/evaluate ────────────────────────────────────────────────
//
// The point of the service: determine whether the evidence required before
// this action may complete actually exists (03-microservices.md §8.6, "No
// finalization path may skip required evidence states").
func (h *Handler) Evaluate(w http.ResponseWriter, r *http.Request) {
	var req domain.EvaluateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	switch {
	case req.LegalEntityID == "":
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id")
		return
	case req.DomainCode == "":
		writeError(w, http.StatusBadRequest, "missing_field", "domain_code")
		return
	case req.ActionType == "":
		writeError(w, http.StatusBadRequest, "missing_field", "action_type")
		return
	case req.CorrelationID == "":
		writeError(w, http.StatusBadRequest, "missing_field", "correlation_id")
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// Identity, not authorization: the determination is recorded against the
	// principal it was made for, so an unidentified caller cannot produce a
	// meaningful evidence record.
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	asOf := time.Now().UTC()
	effective, err := h.store.EffectiveRequirements(r.Context(), tenantID, req.LegalEntityID, req.DomainCode, req.ActionType, asOf)
	if err != nil {
		h.log.Error("Evaluate: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	// Verify asserted artifacts BEFORE matching them against requirements.
	// A document-vault outage means the determination cannot be made at all —
	// recording MISSING off the back of an infrastructure failure would write
	// a false fact into an append-only evidence ledger, which is worse than
	// returning an honest 503.
	checked, err := h.checkArtifacts(r.Context(), tenantID, req.LegalEntityID, req.PresentArtifacts)
	if err != nil {
		h.log.Error("Evaluate: artifact verification unavailable — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "document_service_unavailable", "")
		return
	}

	outcome, unmet := h.determine(effective, checked)

	unmetJSON, err := json.Marshal(unmet)
	if err != nil {
		h.log.Error("Evaluate: failed to marshal unmet payload", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal_error", "")
		return
	}
	presentJSON, err := json.Marshal(req.PresentArtifacts)
	if err != nil {
		h.log.Error("Evaluate: failed to marshal present artifacts payload", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal_error", "")
		return
	}

	eval := &domain.EvidenceEvaluation{
		EvaluationID:            uuid.NewString(),
		TenantID:                tenantID,
		LegalEntityID:           req.LegalEntityID,
		DomainCode:              req.DomainCode,
		ActionType:              req.ActionType,
		Outcome:                 outcome,
		UnmetPayload:            unmetJSON,
		PresentArtifactsPayload: presentJSON,
		EvaluatedForPrincipalID: principalID,
		CorrelationID:           req.CorrelationID,
	}
	created, err := h.store.RecordEvaluation(r.Context(), eval)
	if err != nil {
		h.log.Error("Evaluate: failed to record evaluation", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	if created {
		h.publisher.PublishEvaluation(r.Context(), *eval)
	} else {
		// Replay: return the ORIGINAL determination and do not republish.
		// The stored unmet payload is authoritative — the catalog may have
		// changed since, and the recorded decision must not be rewritten.
		if err := json.Unmarshal(eval.UnmetPayload, &unmet); err != nil {
			h.log.Warn("Evaluate: stored unmet payload unreadable on replay", zap.Error(err))
			unmet = nil
		}
	}

	writeJSON(w, http.StatusOK, domain.EvaluateResponse{
		EvaluationID:  eval.EvaluationID,
		Outcome:       eval.Outcome,
		Unmet:         unmet,
		EvaluatedAt:   eval.EvaluatedAt,
		CorrelationID: eval.CorrelationID,
	})
}

// checkedArtifact is one asserted artifact plus whether it survived
// verification. Rejected artifacts stay in the slice rather than being
// dropped, so the unmet reason can say why something the caller offered did
// not count.
type checkedArtifact struct {
	artifact     domain.PresentArtifact
	counts       bool
	rejectReason string
}

// checkArtifacts verifies every asserted artifact that points at a
// verifiable store. Returns a non-nil error only when verification could not
// be performed at all (document-vault unreachable) — a document that is
// genuinely absent or out of scope is reported as a non-counting artifact,
// not an error.
//
// Results are memoised per reference_id so a requirement set that names the
// same document twice does not double-call document-vault.
func (h *Handler) checkArtifacts(ctx context.Context, tenantID, legalEntityID string, artifacts []domain.PresentArtifact) ([]checkedArtifact, error) {
	out := make([]checkedArtifact, 0, len(artifacts))
	seen := make(map[string]checkedArtifact, len(artifacts))

	for _, a := range artifacts {
		if a.EvidenceType != domain.EvidenceTypeSupportingDocument || a.ReferenceID == "" {
			// Not a document-vault reference. Taken on the caller's word in
			// v1 — there is no service that owns these references yet
			// (context.md §11.2).
			out = append(out, checkedArtifact{artifact: a, counts: true})
			continue
		}
		if prev, ok := seen[a.ReferenceID]; ok {
			prev.artifact = a
			out = append(out, prev)
			continue
		}

		c := checkedArtifact{artifact: a, counts: true}
		switch err := h.docs.VerifyDocument(ctx, tenantID, legalEntityID, a.ReferenceID); {
		case err == nil:
		case errors.Is(err, domain.ErrDocumentNotFound):
			c.counts, c.rejectReason = false, "referenced document does not exist in document-vault-svc"
		case errors.Is(err, domain.ErrDocumentMismatch):
			c.counts, c.rejectReason = false, "referenced document belongs to a different tenant or legal entity"
		default:
			// Unreachable / unexpected response: cannot determine.
			return nil, err
		}
		seen[a.ReferenceID] = c
		out = append(out, c)
	}
	return out, nil
}

// determine is the sufficiency evaluator (§8.6's "evidence sufficiency
// logic").
//
// It is driven entirely by RequirementSpec data decoded from
// requirement_payload. There is deliberately no branch on jurisdiction,
// country, currency, or domain here, and there must never be one: doctrine
// requires that adding a jurisdiction be an INSERT, not a code change.
func (h *Handler) determine(effective []domain.EvidenceRequirement, checked []checkedArtifact) (domain.Outcome, []domain.UnmetRequirement) {
	if len(effective) == 0 {
		// An empty catalog is a legitimate data state, and it is NOT
		// SATISFIED. Collapsing the two would make "nobody has configured
		// this yet" indistinguishable from "verified complete" — the defect
		// shape tax-determination-svc ships today with its synthetic
		// ZERO-TAX fallback.
		//
		// Empty slice, not nil: the recorded unmet_payload should be [] in
		// the ledger, not the JSON literal null.
		return domain.OutcomeNoRequirementsDefined, []domain.UnmetRequirement{}
	}

	unmet := make([]domain.UnmetRequirement, 0)
	for _, req := range effective {
		var spec domain.RequirementSpec
		if len(req.RequirementPayload) > 0 {
			if err := json.Unmarshal(req.RequirementPayload, &spec); err != nil {
				// A requirement whose payload cannot be read cannot be
				// evaluated. Fail closed: report it unmet rather than
				// skipping it, so a malformed rule blocks the action instead
				// of silently disappearing from the gate.
				h.log.Error("determine: unreadable requirement_payload — treating requirement as unmet",
					zap.String("evidence_requirement_id", req.EvidenceRequirementID), zap.Error(err))
				unmet = append(unmet, domain.UnmetRequirement{
					EvidenceRequirementID: req.EvidenceRequirementID,
					EvidenceType:          req.EvidenceType,
					Reason:                "requirement payload is unreadable and cannot be evaluated",
				})
				continue
			}
		}

		minimum := spec.MinimumCount
		if minimum <= 0 {
			minimum = 1
		}

		matched, rejected := 0, ""
		for _, c := range checked {
			if c.artifact.EvidenceType != req.EvidenceType {
				continue
			}
			if spec.ArtifactSubtype != "" && c.artifact.ArtifactSubtype != spec.ArtifactSubtype {
				continue
			}
			if !c.counts {
				if rejected == "" {
					rejected = c.rejectReason
				}
				continue
			}
			matched++
		}

		if matched >= minimum {
			continue
		}
		unmet = append(unmet, domain.UnmetRequirement{
			EvidenceRequirementID: req.EvidenceRequirementID,
			EvidenceType:          req.EvidenceType,
			Reason:                unmetReason(spec, minimum, matched, rejected),
		})
	}

	if len(unmet) > 0 {
		return domain.OutcomeMissing, unmet
	}
	return domain.OutcomeSatisfied, unmet
}

// unmetReason builds the explainable reason returned to a blocked caller.
// Named individually per requirement rather than as one aggregate boolean —
// a caller that is blocked needs to learn what to go and produce.
func unmetReason(spec domain.RequirementSpec, minimum, matched int, rejected string) string {
	reason := fmt.Sprintf("requires %d matching artifact(s), %d present", minimum, matched)
	if spec.ArtifactSubtype != "" {
		reason += fmt.Sprintf(" (artifact_subtype %q)", spec.ArtifactSubtype)
	}
	if rejected != "" {
		reason += "; an offered artifact did not count: " + rejected
	}
	if spec.Description != "" {
		reason += "; " + spec.Description
	}
	return reason
}

// ── GET /v1/evidence/evaluations/{evaluation_id} ───────────────────────────────

func (h *Handler) GetEvaluation(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	eval, err := h.store.GetEvaluation(r.Context(), chi.URLParam(r, "evaluation_id"))
	if err != nil {
		h.log.Error("GetEvaluation: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if eval == nil {
		writeError(w, http.StatusNotFound, "evaluation_not_found", "")
		return
	}
	writeJSON(w, http.StatusOK, eval)
}

// ── GET /v1/evidence-requirements ──────────────────────────────────────────────

func (h *Handler) ListRequirements(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := domain.ListRequirementsFilter{
		TenantID:      q.Get("tenant_id"),
		LegalEntityID: q.Get("legal_entity_id"),
		DomainCode:    q.Get("domain_code"),
		ActionType:    q.Get("action_type"),
	}
	if filter.TenantID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "tenant_id")
		return
	}

	// as_of=now is the common case (what is in force right now). Omitting it
	// returns the full history including retired requirements, which is what
	// an auditor reviewing a past decision needs.
	switch v := q.Get("as_of"); v {
	case "":
	case "now":
		filter.AsOf = time.Now().UTC()
	default:
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_field", "as_of must be RFC3339 or 'now'")
			return
		}
		filter.AsOf = t
	}

	list, err := h.store.ListRequirements(r.Context(), filter)
	if err != nil {
		h.log.Error("ListRequirements: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/evidence-requirements/{evidence_requirement_id} ────────────────────

func (h *Handler) GetRequirement(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	req, err := h.store.GetRequirement(r.Context(), chi.URLParam(r, "evidence_requirement_id"))
	if err != nil {
		h.log.Error("GetRequirement: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if req == nil {
		writeError(w, http.StatusNotFound, "requirement_not_found", "")
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// ── POST /v1/admin/evidence-requirements ───────────────────────────────────────

func (h *Handler) CreateRequirement(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRequirementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	switch {
	case req.TenantID == "":
		writeError(w, http.StatusBadRequest, "missing_field", "tenant_id")
		return
	case req.DomainCode == "":
		writeError(w, http.StatusBadRequest, "missing_field", "domain_code")
		return
	case req.ActionType == "":
		writeError(w, http.StatusBadRequest, "missing_field", "action_type")
		return
	case req.EvidenceType == "":
		writeError(w, http.StatusBadRequest, "missing_field", "evidence_type")
		return
	case req.CorrelationID == "":
		writeError(w, http.StatusBadRequest, "missing_field", "correlation_id")
		return
	}

	// The caller's real tenant scope comes from the verified header, never
	// from the body alone. If both are present they must agree — otherwise a
	// caller could write into another tenant's catalog by putting a
	// different tenant_id in the body.
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if req.TenantID != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch",
			"body tenant_id does not match the caller's verified tenant scope")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// Authorization is scoped to the legal entity when the requirement is
	// entity-specific; tenant-wide requirements authorize against the tenant
	// itself, which is the broadest scope and therefore the correct one to
	// check for a rule that will apply to every entity.
	authzScope := tenantID
	if req.LegalEntityID != nil && *req.LegalEntityID != "" {
		authzScope = *req.LegalEntityID
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, authzScope, actionCreateRequirement); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	effectiveFrom := time.Now().UTC()
	if req.EffectiveFrom != nil {
		effectiveFrom = req.EffectiveFrom.UTC()
	}

	er := &domain.EvidenceRequirement{
		EvidenceRequirementID: uuid.NewString(),
		TenantID:              req.TenantID,
		LegalEntityID:         req.LegalEntityID,
		DomainCode:            req.DomainCode,
		ActionType:            req.ActionType,
		EvidenceType:          req.EvidenceType,
		RequirementPayload:    req.RequirementPayload,
		EffectiveFrom:         effectiveFrom,
		CreatedByPrincipalID:  principalID,
		CorrelationID:         req.CorrelationID,
	}
	created, err := h.store.CreateRequirement(r.Context(), er)
	if err != nil {
		h.log.Error("CreateRequirement: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, er)
}

// ── POST /v1/admin/evidence-requirements/{id}/end-date ─────────────────────────
//
// Retirement is effective end-dating. There is no DELETE route and no
// is_deleted flag anywhere in this service (doctrine: no soft-delete on
// material objects).
func (h *Handler) EndDateRequirement(w http.ResponseWriter, r *http.Request) {
	var req domain.EndDateRequirementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Reason == "" {
		// A retirement with no stated reason is not useful evidence — same
		// rationale as purchase-order-svc's required amendment reason.
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	requirementID := chi.URLParam(r, "evidence_requirement_id")
	existing, err := h.store.GetRequirement(r.Context(), requirementID)
	if err != nil {
		h.log.Error("EndDateRequirement: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "requirement_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	authzScope := tenantID
	if existing.LegalEntityID != nil && *existing.LegalEntityID != "" {
		authzScope = *existing.LegalEntityID
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, authzScope, actionRetireRequirement); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	effectiveTo := time.Now().UTC()
	if req.EffectiveTo != nil {
		effectiveTo = req.EffectiveTo.UTC()
	}

	updated, err := h.store.EndDateRequirement(r.Context(), tenantID, requirementID, effectiveTo, req.Reason, principalID)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, updated)
	case errors.Is(err, domain.ErrAlreadyRetired):
		writeError(w, http.StatusUnprocessableEntity, "already_retired", domain.ErrAlreadyRetired.Error())
	case errors.Is(err, domain.ErrRequirementNotFound):
		writeError(w, http.StatusNotFound, "requirement_not_found", "")
	default:
		h.log.Error("EndDateRequirement: store failure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAuthorizationDenied):
		writeError(w, http.StatusForbidden, "authorization_denied", "")
	default:
		h.log.Error("authorization check failed — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization_service_unavailable", "")
	}
}

// requireTenant reads the caller's verified tenant scope from context (set by
// middleware.TenantContext from X-Tenant-Id). Absent scope is a 400 — never
// substituted with a placeholder tenant, which is a live cross-tenant defect
// in offboarding-severance-svc and workforce-compliance-svc.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "missing_tenant", domain.ErrTenantMissing.Error())
		return "", false
	}
	return tenantID, true
}

// requirePrincipal reads the caller's identity from X-Principal-Id — set by
// gateway-auth-svc's ForwardAuth verification after checking the signed
// IdentityContextEnvelope JWT. This service never decodes a JWT itself. A
// request with no resolved principal never passed identity verification —
// fail closed with 401.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", domain.ErrIdentityMissing.Error())
		return "", false
	}
	return principalID, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type errorResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorResponse{Error: code, Detail: detail})
}
