// Package handler exposes evidence-requirements-svc's REST API.
//
// Route split follows jurisdiction-rules-svc's proven shape: public read
// paths plus evaluation, and a separate /v1/admin/... namespace for catalog
// mutation which is authorization-gated.
//
// Authorization (decided 21 Oct 2026): evaluate is gated on EVIDENCE_EVALUATE
// at the legal entity it is for — it writes a governance record and emits an
// event for that entity, so it is not an open query. The catalogue and ledger
// reads are gated on EVIDENCE_REQUIREMENT_READ / EVIDENCE_EVALUATION_READ at
// the entity they concern (PLATFORM for tenant-wide requirements). Each gate
// has an enforce | warn mode (EVIDENCE_EVALUATE_AUTHZ, EVIDENCE_READ_AUTHZ);
// warn exists only while the gated callers' principals are being granted.
// Catalogue mutation is always enforced: this service being part of the
// Governance Plane is not a self-authorization exemption.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcenvelope "zoiko.io/evidence-requirements-svc/internal/envelope"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateRequirement(ctx context.Context, r *domain.EvidenceRequirement) (created bool, err error)
	GetRequirement(ctx context.Context, requirementID string) (*domain.EvidenceRequirement, error)
	ListRequirements(ctx context.Context, filter domain.ListRequirementsFilter) ([]domain.EvidenceRequirement, error)
	EffectiveRequirements(ctx context.Context, tenantID, legalEntityID, domainCode, actionType string, asOf time.Time) ([]domain.EvidenceRequirement, error)
	EndDateRequirement(ctx context.Context, tenantID, requirementID string, effectiveTo time.Time, reason, actorPrincipalID string) (*domain.EvidenceRequirement, error)
	// RecordEvaluation appends an evaluation record and optionally writes an
	// outbox event atomically in the same transaction. If outboxEvent is non-nil,
	// it is written to the outbox table only when the evaluation is newly created
	// (not a replay). This ensures the event is never lost even if the broker
	// is unavailable or the client disconnects.
	RecordEvaluation(ctx context.Context, e *domain.EvidenceEvaluation, outboxEvent *domain.OutboxEvent) (created bool, err error)
	GetEvaluation(ctx context.Context, evaluationID string) (*domain.EvidenceEvaluation, error)
}

// Publisher is the event-publishing contract the handler depends on.
// DEPRECATED: Events are now published via transactional outbox.
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
	VerifyDocument(ctx context.Context, tenantID, legalEntityID, documentID, principalID string) error
}

// Action types checked against authorization-svc.
const (
	actionCreateRequirement = "EVIDENCE_REQUIREMENT_CREATE"
	actionRetireRequirement = "EVIDENCE_REQUIREMENT_RETIRE"
	// Decided 21 Oct 2026: evaluate is authorized at the legal entity it is
	// for, and the catalogue and ledger reads are scoped (GOV-07 "strongly
	// scoped"). Registered in access-control-svc 000021.
	actionEvaluate        = "EVIDENCE_EVALUATE"
	actionReadRequirement = "EVIDENCE_REQUIREMENT_READ"
	actionReadEvaluation  = "EVIDENCE_EVALUATION_READ"

	// platformScope is the legal-entity scope of a tenant-wide requirement.
	// authorization-svc resolves it to the platform entity within the
	// caller's tenant. The tenant id used to be sent here, which is not an
	// entity, so tenant-wide catalogue changes could never be authorized.
	platformScope = "PLATFORM"
)

// EntityChecker confirms a legal entity exists in the caller's tenant
// (tenant-entity-registry-svc).
type EntityChecker interface {
	CheckEntity(ctx context.Context, tenantID, legalEntityID string) error
}

type noopChecker struct{}

func (noopChecker) CheckEntity(context.Context, string, string) error { return nil }

// GateMode is how an authorization gate treats a refusal: enforce answers
// 401/403/503; warn logs it and admits (a migration state while callers'
// principals are being granted).
type GateMode string

const (
	GateEnforce GateMode = "enforce"
	GateWarn    GateMode = "warn"
)

// ParseGateMode reads a configured mode; anything but "warn" enforces.
func ParseGateMode(s string) GateMode {
	if s == string(GateWarn) {
		return GateWarn
	}
	return GateEnforce
}

type Handler struct {
	store        Store
	authz        AuthZClient
	docs         DocumentVaultClient
	registry     EntityChecker
	evaluateMode GateMode
	readMode     GateMode
	log          *zap.Logger
}

// New builds a Handler with both gates enforcing and no registry check;
// main.go adds the registry with WithRegistry.
func New(store Store, authz AuthZClient, docs DocumentVaultClient, log *zap.Logger) *Handler {
	return &Handler{store: store, authz: authz, docs: docs, registry: noopChecker{},
		evaluateMode: GateEnforce, readMode: GateEnforce, log: log}
}

// WithRegistry confirms every legal entity a request names against
// tenant-entity-registry-svc.
func (h *Handler) WithRegistry(c EntityChecker) *Handler {
	h.registry = c
	return h
}

// WithGateModes sets the evaluate and read authorization modes.
func (h *Handler) WithGateModes(evaluate, read GateMode) *Handler {
	h.evaluateMode, h.readMode = evaluate, read
	return h
}

// gate authorizes principalID for action at scope under mode. In warn mode a
// refusal or an outage is logged and admitted; in enforce mode it is answered
// (403 / 503) and false is returned.
func (h *Handler) gate(w http.ResponseWriter, r *http.Request, mode GateMode, principalID, scope, action string) bool {
	err := h.authz.CheckAllowed(r.Context(), principalID, scope, action)
	if err == nil {
		return true
	}
	if mode == GateWarn {
		h.log.Warn("authorization gate (warn mode): would be refused; admitted",
			zap.String("action_type", action), zap.String("principal_id", principalID),
			zap.String("scope", scope), zap.Error(err))
		return true
	}
	h.writeAuthzErr(w, r, err)
	return false
}

// checkEntity refuses a legal entity the registry does not hold in the
// caller's tenant (422) and fails closed when the registry cannot answer.
func (h *Handler) checkEntity(w http.ResponseWriter, r *http.Request, tenantID, legalEntityID string) bool {
	err := h.registry.CheckEntity(r.Context(), tenantID, legalEntityID)
	switch {
	case err == nil:
		return true
	case errors.Is(err, domain.ErrLegalEntityNotFound):
		writeError(w, r, http.StatusUnprocessableEntity, "legal_entity_not_found",
			"the legal entity does not exist in this tenant (tenant-entity-registry-svc)")
	default:
		h.log.Error("legal entity check failed — failing closed", zap.String("legal_entity_id", legalEntityID), zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "registry_unavailable", "")
	}
	return false
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
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case req.LegalEntityID == "":
		writeError(w, r, http.StatusBadRequest, "missing_field", "legal_entity_id")
		return
	case req.DomainCode == "":
		writeError(w, r, http.StatusBadRequest, "missing_field", "domain_code")
		return
	case req.ActionType == "":
		writeError(w, r, http.StatusBadRequest, "missing_field", "action_type")
		return
	case req.CorrelationID == "":
		writeError(w, r, http.StatusBadRequest, "missing_field", "correlation_id")
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

	// Get the Idempotency-Key and LegalEntityID from the envelope for replay
	// protection and cross-validation with the body.
	env, _ := svcenvelope.FromContext(r.Context())
	idempotencyKey := env.IdempotencyKey
	if idempotencyKey == "" {
		// Should never happen in write-strict mode, but defensive
		writeError(w, r, http.StatusBadRequest, "missing_field", "idempotency_key")
		return
	}
	envelopeLegalEntityID := env.LegalEntityID

	// Validate body's legal_entity_id is a valid UUID before hitting the DB
	// (otherwise 22P02 maps to 503 store_unavailable per X10).
	if !isUUID(req.LegalEntityID) {
		writeError(w, r, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	}

	// The body's legal_entity_id MUST match the envelope's X-Legal-Entity-Id.
	// The envelope requires this header on material writes (LegalEntityID:
	// RequiredOnWrite), so a mismatch means the caller is trying to evaluate
	// for a different entity than the one their identity context establishes.
	if envelopeLegalEntityID != "" && req.LegalEntityID != envelopeLegalEntityID {
		writeError(w, r, http.StatusBadRequest, "legal_entity_mismatch",
			"body legal_entity_id does not match envelope X-Legal-Entity-Id")
		return
	}
	// If envelope doesn't have it (should not happen in write-strict), fall back
	// to body value but log a warning.
	effectiveLegalEntityID := req.LegalEntityID
	if envelopeLegalEntityID != "" {
		effectiveLegalEntityID = envelopeLegalEntityID
	}

	// Authorized at the entity the determination is for (decided 21 Oct
	// 2026): an evaluation writes a governance record for that entity and
	// emits its event, so "any principal in the tenant, for any entity" was
	// a write the caller held no grant for.
	if !h.gate(w, r, h.evaluateMode, principalID, effectiveLegalEntityID, actionEvaluate) {
		return
	}
	// Server-validated legal entity (GCP §2 invariant #2): it must exist in
	// the caller's tenant.
	if !h.checkEntity(w, r, tenantID, effectiveLegalEntityID) {
		return
	}

	asOf := time.Now().UTC()
	effective, err := h.store.EffectiveRequirements(r.Context(), tenantID, effectiveLegalEntityID, req.DomainCode, req.ActionType, asOf)
	if err != nil {
		h.log.Error("Evaluate: store unavailable", zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	// Verify asserted artifacts BEFORE matching them against requirements.
	// A document-vault outage means the determination cannot be made at all —
	// recording MISSING off the back of an infrastructure failure would write
	// a false fact into an append-only evidence ledger, which is worse than
	// returning an honest 503.
	checked, err := h.checkArtifacts(r.Context(), tenantID, effectiveLegalEntityID, principalID, req.PresentArtifacts)
	if err != nil {
		h.log.Error("Evaluate: artifact verification unavailable — failing closed", zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "document_service_unavailable", "")
		return
	}

	outcome, unmet := h.determine(effective, checked)

	unmetJSON, err := json.Marshal(unmet)
	if err != nil {
		h.log.Error("Evaluate: failed to marshal unmet payload", zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	presentJSON, err := json.Marshal(req.PresentArtifacts)
	if err != nil {
		h.log.Error("Evaluate: failed to marshal present artifacts payload", zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}

	// Collect all requirement IDs that were evaluated (both satisfied and unmet)
	requirementIDs := make([]string, len(effective))
	for i, r := range effective {
		requirementIDs[i] = r.EvidenceRequirementID
	}

	eval := &domain.EvidenceEvaluation{
		EvaluationID:            uuid.NewString(),
		TenantID:                tenantID,
		LegalEntityID:           effectiveLegalEntityID,
		DomainCode:              req.DomainCode,
		ActionType:              req.ActionType,
		Outcome:                 outcome,
		UnmetPayload:            unmetJSON,
		PresentArtifactsPayload: presentJSON,
		EvaluatedForPrincipalID: principalID,
		CorrelationID:           req.CorrelationID,
		IdempotencyKey:          idempotencyKey,
		RequirementIDs:          requirementIDs,
	}

	// Prepare outbox event for atomic write with evaluation (only for SATISFIED/MISSING)
	var outboxEvent *domain.OutboxEvent
	if outcome == domain.OutcomeSatisfied || outcome == domain.OutcomeMissing {
		eventType := "evidence.requirement.satisfied"
		if outcome == domain.OutcomeMissing {
			eventType = "evidence.requirement.missing"
		}
		// GCP §16 event minimum: causation, jurisdiction context and the
		// governing references (the requirement set the determination was
		// tested against), so a consumer can reproduce the decision.
		payload := map[string]any{
			"evaluation_id":              eval.EvaluationID,
			"tenant_id":                  eval.TenantID,
			"legal_entity_id":            eval.LegalEntityID,
			"evaluated_for_principal_id": eval.EvaluatedForPrincipalID,
			"correlation_id":             eval.CorrelationID,
			"causation_id":               env.CausationID,
			"jurisdiction_context":       env.JurisdictionContext,
			"idempotency_key":            eval.IdempotencyKey,
			"domain_code":                eval.DomainCode,
			"action_type":                eval.ActionType,
			"outcome":                    string(eval.Outcome),
			"requirement_ids":            eval.RequirementIDs,
		}
		if outcome == domain.OutcomeMissing {
			payload["unmet"] = json.RawMessage(eval.UnmetPayload)
		}
		payloadBytes, _ := json.Marshal(payload)
		outboxEvent = &domain.OutboxEvent{
			AggregateType: "evidence_evaluation",
			AggregateID:   eval.EvaluationID,
			EventType:     eventType,
			Payload:       payloadBytes,
		}
	}

	created, err := h.store.RecordEvaluation(r.Context(), eval, outboxEvent)
	if err != nil {
		h.log.Error("Evaluate: failed to record evaluation", zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	if created {
		// Event was written to outbox atomically; background worker will publish it.
		// No direct Kafka publish here anymore.
	} else {
		// Replay: return the ORIGINAL determination and do not republish.
		// The stored unmet payload is authoritative �?" the catalog may have
		// changed since, and the recorded decision must not be rewritten.
		//
		// A key replayed with a DIFFERENT request is not a retry: answering
		// the original would report success for a determination that was
		// never made (API standard: IDEMPOTENCY_MISMATCH).
		if eval.DomainCode != req.DomainCode ||
			eval.ActionType != req.ActionType ||
			eval.LegalEntityID != effectiveLegalEntityID ||
			eval.EvaluatedForPrincipalID != principalID ||
			!jsonSemanticallyEqual(eval.PresentArtifactsPayload, presentJSON) {
			writeError(w, r, http.StatusConflict, "idempotency_mismatch",
				"Idempotency-Key was already used for a different evaluation request")
			return
		}
		if err := json.Unmarshal(eval.UnmetPayload, &unmet); err != nil {
			h.log.Warn("Evaluate: stored unmet payload unreadable on replay", zap.Error(err))
			unmet = nil
		}
	}

	writeJSON(w, r, http.StatusOK, domain.EvaluateResponse{
		EvaluationID:  eval.EvaluationID,
		Outcome:       eval.Outcome,
		Unmet:         unmet,
		EvaluatedAt:   eval.EvaluatedAt,
		CorrelationID: eval.CorrelationID,
		DomainCode:    eval.DomainCode,
		ActionType:    eval.ActionType,
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
// Results are memoised per artifact so a requirement set that names the same
// document twice does not double-call document-vault. A duplicate is NOT
// counted twice toward minimum_count (Gap 8). Duplicates are recognised by
// canonical reference (artifactKey), not by the exact string: "ABC…",
// "abc…" and " abc… " name the same document-vault UUID, and comparing the
// raw strings let one document satisfy minimum_count: 2.
func (h *Handler) checkArtifacts(ctx context.Context, tenantID, legalEntityID, principalID string, artifacts []domain.PresentArtifact) ([]checkedArtifact, error) {
	out := make([]checkedArtifact, 0, len(artifacts))
	seen := make(map[string]checkedArtifact, len(artifacts))

	for _, a := range artifacts {
		key := artifactKey(a.ReferenceID)
		if a.EvidenceType != domain.EvidenceTypeSupportingDocument {
			// Not a document-vault reference. Taken on the caller's word in
			// v1 — there is no service that owns these references yet
			// (context.md §11.2). Require a non-empty reference_id to count
			// (prevents empty reference_id from satisfying minimum_count).
			if key == "" {
				out = append(out, checkedArtifact{artifact: a, counts: false, rejectReason: "empty reference_id for non-document artifact"})
				continue
			}
			// Deduplicate by reference for non-documents too
			if _, ok := seen[key]; ok {
				continue // the same artifact does not count again
			}
			out = append(out, checkedArtifact{artifact: a, counts: true})
			seen[key] = checkedArtifact{artifact: a, counts: true}
			continue
		}
		// Supporting document: require reference_id
		if key == "" {
			out = append(out, checkedArtifact{artifact: a, counts: false, rejectReason: "empty reference_id for supporting document"})
			continue
		}
		if _, ok := seen[key]; ok {
			continue // the same document does not count again
		}

		c := checkedArtifact{artifact: a, counts: true}
		switch err := h.docs.VerifyDocument(ctx, tenantID, legalEntityID, a.ReferenceID, principalID); {
		case err == nil:
		case errors.Is(err, domain.ErrDocumentNotFound):
			c.counts, c.rejectReason = false, "referenced document does not exist in document-vault-svc"
		case errors.Is(err, domain.ErrDocumentMismatch):
			c.counts, c.rejectReason = false, "referenced document belongs to a different tenant or legal entity"
		default:
			// Unreachable / unexpected response: cannot determine.
			return nil, err
		}
		seen[key] = c
		out = append(out, c)
	}
	return out, nil
}

// artifactKey is the canonical identity of an asserted artifact reference: a
// UUID in its canonical form, anything else trimmed and lower-cased. "" means
// no reference at all.
func artifactKey(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if id, err := uuid.Parse(ref); err == nil {
		return id.String()
	}
	return strings.ToLower(ref)
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
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	eval, err := h.store.GetEvaluation(r.Context(), chi.URLParam(r, "evaluation_id"))
	if err != nil {
		h.log.Error("GetEvaluation: store unavailable", zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if eval == nil {
		writeError(w, r, http.StatusNotFound, "evaluation_not_found", "")
		return
	}
	// The ledger holds the asserted artifact references: read it at the
	// entity it is about (GOV-07 "strongly scoped").
	if !h.gate(w, r, h.readMode, principalID, eval.LegalEntityID, actionReadEvaluation) {
		return
	}
	writeJSON(w, r, http.StatusOK, eval)
}

// ── GET /v1/evidence-requirements ──────────────────────────────────────────────

// ListRequirements returns the caller's own tenant's requirement catalog.
//
// The scope comes from the verified X-Tenant-Id header. It used to come from
// ?tenant_id=, which the store both filtered on AND set app.tenant_id from — so
// `?tenant_id=<any-uuid>` returned that tenant's entire requirement catalog
// while CreateRequirement, on the same service, had always insisted the body
// agree with the header. The read side was the half nobody checked.
func (h *Handler) ListRequirements(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if claimed := q.Get("tenant_id"); claimed != "" && claimed != tenantID {
		writeError(w, r, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return
	}
	// legal_entity_id is compared as `legal_entity_id::text = $n`, casting the
	// COLUMN to text rather than the parameter to uuid — so a malformed value does
	// not error, it silently matches nothing, and an empty catalog reads as "this
	// entity has no requirements", which for an evidence gate reads as permission.
	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID != "" && !isUUID(legalEntityID) {
		writeError(w, r, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	readScope := platformScope
	if legalEntityID != "" {
		readScope = legalEntityID
	}
	if !h.gate(w, r, h.readMode, principalID, readScope, actionReadRequirement) {
		return
	}
	filter := domain.ListRequirementsFilter{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		DomainCode:    q.Get("domain_code"),
		ActionType:    q.Get("action_type"),
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
			writeError(w, r, http.StatusBadRequest, "invalid_field", "as_of must be RFC3339 or 'now'")
			return
		}
		filter.AsOf = t
	}

	list, err := h.store.ListRequirements(r.Context(), filter)
	if err != nil {
		// legal_entity_id is compared as text, so the verified tenant is the only
		// uuid comparison left here. A gateway that forwarded a non-UUID tenant
		// scope is a fault worth naming rather than reporting as a dead store.
		if errors.Is(err, domain.ErrInvalidIdentifier) {
			writeError(w, r, http.StatusBadRequest, "invalid_field", "tenant scope must be a UUID")
			return
		}
		h.log.Error("ListRequirements: store unavailable", zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if list == nil {
		list = []domain.EvidenceRequirement{}
	}
	writeJSON(w, r, http.StatusOK, list)
}

// ── GET /v1/evidence-requirements/{evidence_requirement_id} ────────────────────

func (h *Handler) GetRequirement(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	req, err := h.store.GetRequirement(r.Context(), chi.URLParam(r, "evidence_requirement_id"))
	if err != nil {
		h.log.Error("GetRequirement: store unavailable", zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if req == nil {
		writeError(w, r, http.StatusNotFound, "requirement_not_found", "")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	readScope := platformScope
	if req.LegalEntityID != nil && *req.LegalEntityID != "" {
		readScope = *req.LegalEntityID
	}
	if !h.gate(w, r, h.readMode, principalID, readScope, actionReadRequirement) {
		return
	}
	writeJSON(w, r, http.StatusOK, req)
}

// ── POST /v1/admin/evidence-requirements ───────────────────────────────────────

func (h *Handler) CreateRequirement(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRequirementRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case req.TenantID == "":
		writeError(w, r, http.StatusBadRequest, "missing_field", "tenant_id")
		return
	case req.DomainCode == "":
		writeError(w, r, http.StatusBadRequest, "missing_field", "domain_code")
		return
	case req.ActionType == "":
		writeError(w, r, http.StatusBadRequest, "missing_field", "action_type")
		return
	case req.EvidenceType == "":
		writeError(w, r, http.StatusBadRequest, "missing_field", "evidence_type")
		return
	case req.CorrelationID == "":
		writeError(w, r, http.StatusBadRequest, "missing_field", "correlation_id")
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
		writeError(w, r, http.StatusForbidden, "tenant_scope_mismatch",
			"body tenant_id does not match the caller's verified tenant scope")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if req.LegalEntityID != nil && *req.LegalEntityID != "" && !isUUID(*req.LegalEntityID) {
		writeError(w, r, http.StatusBadRequest, "invalid_field", "legal_entity_id must be a UUID")
		return
	}

	// Get the Idempotency-Key from the envelope for replay protection
	env, _ := svcenvelope.FromContext(r.Context())
	idempotencyKey := env.IdempotencyKey
	if idempotencyKey == "" {
		// Should never happen in write-strict mode for admin writes
		writeError(w, r, http.StatusBadRequest, "missing_field", "idempotency_key")
		return
	}

	// Authorization is scoped to the legal entity when the requirement is
	// entity-specific; a tenant-wide requirement authorizes at the PLATFORM
	// scope within the caller's tenant. It used to send the tenant id in the
	// legal-entity slot, which authorization-svc cannot resolve as an entity,
	// so no tenant-wide requirement could ever be authorized.
	authzScope := platformScope
	if req.LegalEntityID != nil && *req.LegalEntityID != "" {
		authzScope = *req.LegalEntityID
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, authzScope, actionCreateRequirement); err != nil {
		h.writeAuthzErr(w, r, err)
		return
	}
	if req.LegalEntityID != nil && *req.LegalEntityID != "" && !h.checkEntity(w, r, tenantID, *req.LegalEntityID) {
		return
	}

	effectiveFrom := time.Now().UTC()
	if req.EffectiveFrom != nil {
		effectiveFrom = req.EffectiveFrom.UTC()
		// Prevent retroactive effective_from (before now) — a requirement
		// cannot govern actions that already happened.
		if effectiveFrom.Before(time.Now().UTC().Add(-time.Minute)) {
			writeError(w, r, http.StatusBadRequest, "invalid_field",
				"effective_from cannot be retroactive (before now)")
			return
		}
	}

	// Validate requirement_payload against the rule shape the evaluator
	// reads. An object check alone admitted {"minimum_count": "two"}, which
	// then failed to decode at every evaluation and blocked every gated
	// action (fail-closed, but caused by a rule nobody could have meant).
	if len(req.RequirementPayload) > 0 {
		if msg := validateRequirementPayload(req.RequirementPayload); msg != "" {
			writeError(w, r, http.StatusBadRequest, "invalid_field", msg)
			return
		}
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
		IdempotencyKey:        idempotencyKey,
	}
	requestedPayload := req.RequirementPayload
	created, err := h.store.CreateRequirement(r.Context(), er)
	if err != nil {
		h.log.Error("CreateRequirement: store unavailable", zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	if !created {
		// Replay of the same key must describe the same requirement. The
		// effective_from comparison only applies when the caller supplied
		// one; an omitted value is server-assigned and legitimately differs
		// between the original call and the retry.
		effectiveFromMatches := req.EffectiveFrom == nil ||
			er.EffectiveFrom.UTC().Truncate(time.Microsecond).Equal(effectiveFrom.UTC().Truncate(time.Microsecond))
		if er.DomainCode != req.DomainCode ||
			er.ActionType != req.ActionType ||
			er.EvidenceType != req.EvidenceType ||
			!optionalStringEqual(er.LegalEntityID, req.LegalEntityID) ||
			er.CreatedByPrincipalID != principalID ||
			!effectiveFromMatches ||
			!jsonSemanticallyEqual(er.RequirementPayload, requestedPayload) {
			writeError(w, r, http.StatusConflict, "idempotency_mismatch",
				"Idempotency-Key was already used for a different requirement")
			return
		}
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set("Location", "/v1/evidence-requirements/"+er.EvidenceRequirementID)
	}
	writeJSON(w, r, status, er)
}

// validateRequirementPayload checks a requirement_payload against
// domain.RequirementSpec: an object, only known members, the right types,
// and a non-negative minimum_count. Returns "" when it is acceptable.
func validateRequirementPayload(raw json.RawMessage) string {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
		return "requirement_payload must be a JSON object"
	}
	var spec domain.RequirementSpec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return "requirement_payload does not match the rule shape {minimum_count, artifact_subtype, description}: " + err.Error()
	}
	if spec.MinimumCount < 0 {
		return "requirement_payload.minimum_count must not be negative"
	}
	return ""
}

// ── POST /v1/admin/evidence-requirements/{id}/end-date ─────────────────────────
//
// Retirement is effective end-dating. There is no DELETE route and no
// is_deleted flag anywhere in this service (doctrine: no soft-delete on
// material objects).
func (h *Handler) EndDateRequirement(w http.ResponseWriter, r *http.Request) {
	var req domain.EndDateRequirementRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		// A retirement with no stated reason is not useful evidence — same
		// rationale as purchase-order-svc's required amendment reason.
		writeError(w, r, http.StatusBadRequest, "missing_field", "reason")
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
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if existing == nil {
		writeError(w, r, http.StatusNotFound, "requirement_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	authzScope := platformScope
	if existing.LegalEntityID != nil && *existing.LegalEntityID != "" {
		authzScope = *existing.LegalEntityID
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, authzScope, actionRetireRequirement); err != nil {
		h.writeAuthzErr(w, r, err)
		return
	}

	effectiveTo := time.Now().UTC()
	if req.EffectiveTo != nil {
		effectiveTo = req.EffectiveTo.UTC()
	}
	// Validate effective_to is after effective_from (DB constraint also enforces this)
	if !effectiveTo.After(existing.EffectiveFrom) {
		writeError(w, r, http.StatusBadRequest, "invalid_field",
			"effective_to must be after effective_from")
		return
	}
	// Prevent retroactive retirement that changes historical governance
	if effectiveTo.Before(time.Now().UTC().Add(-time.Minute)) {
		writeError(w, r, http.StatusBadRequest, "invalid_field",
			"effective_to cannot be retroactive (before now)")
		return
	}

	updated, err := h.store.EndDateRequirement(r.Context(), tenantID, requirementID, effectiveTo, req.Reason, principalID)
	switch {
	case err == nil:
		writeJSON(w, r, http.StatusOK, updated)
	case errors.Is(err, domain.ErrAlreadyRetired):
		writeError(w, r, http.StatusUnprocessableEntity, "already_retired", domain.ErrAlreadyRetired.Error())
	case errors.Is(err, domain.ErrRequirementNotFound):
		writeError(w, r, http.StatusNotFound, "requirement_not_found", "")
	default:
		h.log.Error("EndDateRequirement: store failure", zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) writeAuthzErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrAuthorizationDenied):
		writeError(w, r, http.StatusForbidden, "authorization_denied", "")
	default:
		h.log.Error("authorization check failed — failing closed", zap.Error(err))
		writeError(w, r, http.StatusServiceUnavailable, "authorization_service_unavailable", "")
	}
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

func optionalStringEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// jsonSemanticallyEqual compares two JSON values after decoding, so key
// ordering and whitespace do not turn an honest retry into a mismatch. An
// empty payload is normalised to {} because that is what the store persists.
func jsonSemanticallyEqual(a, b []byte) bool {
	if len(a) == 0 {
		a = []byte(`{}`)
	}
	if len(b) == 0 {
		b = []byte(`{}`)
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return string(a) == string(b)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return string(a) == string(b)
	}
	return reflect.DeepEqual(av, bv)
}

// requireTenant reads the caller's verified tenant scope from context (set by
// middleware.TenantContext from X-Tenant-Id). Absent scope is a 400 — never
// substituted with a placeholder tenant, which is a live cross-tenant defect
// in offboarding-severance-svc and workforce-compliance-svc.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		// 401 like the envelope's own refusal: an unscoped caller is an
		// unauthenticated one. Reads answered 400 and writes 401 for the
		// same fault.
		writeError(w, r, http.StatusUnauthorized, "missing_tenant", domain.ErrTenantMissing.Error())
		return "", false
	}
	return tenantID, true
}

// requirePrincipal reads the caller's identity. The identity gate
// (identity-envelope) sets X-Principal-Id from the verified envelope; a
// service calling over verified mTLS as a registered peer arrives with
// X-Identity-Basis: peer and X-Workload-Id set by the gate, and acts as that
// workload (GCP §16 "subject_id / workload_id"). Neither header can be
// supplied by the client: the gate removes or overwrites both. A request with
// no resolved identity never passed verification — fail closed with 401.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" && r.Header.Get(idenv.HeaderIdentityBasis) == string(idenv.BasisPeer) {
		principalID = r.Header.Get(idenv.HeaderWorkloadID)
	}
	if principalID == "" {
		writeError(w, r, http.StatusUnauthorized, "identity_missing", domain.ErrIdentityMissing.Error())
		return "", false
	}
	return principalID, true
}

// writeError writes a RFC 9457 problem details response using the shared apicontract package.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	apicontract.WriteProblem(w, r, apicontract.Problem{
		Status:    status,
		Code:      code,
		Detail:    detail,
		Retryable: status >= 500,
	})
}

// writeJSON writes body as JSON. An error body is rendered as RFC 9457
// application/problem+json with the §16 error_class via apicontract.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	if errResp, ok := body.(map[string]any); ok && status >= 400 {
		if errCode, ok := errResp["error"].(string); ok {
			message := ""
			if msg, ok := errResp["detail"].(string); ok {
				message = msg
			}
			// Every other member of the refusal stays on the problem as an
			// extension member.
			extra := map[string]any{}
			for k, v := range errResp {
				if k != "error" && k != "detail" {
					extra[k] = v
				}
			}
			apicontract.WriteProblem(w, r, apicontract.Problem{
				Status:    status,
				Code:      errCode,
				Detail:    message,
				Retryable: status >= 500,
				Extra:     extra,
			})
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// maxRequestBytes caps a JSON request body. A bare json.Decoder reads until EOF,
// so without this a single request can make the service allocate whatever the
// client is willing to send -- no auth needed, and nothing in the metrics to
// distinguish it from load.
const maxRequestBytes = 256 << 10 // 256 KiB

// decodeJSON reads a size-capped JSON body, answering 413 rather than 400 when
// the cap is what stopped it: "too large" and "malformed" are different faults
// and a caller can only act on the difference.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "")
			return false
		}
		writeError(w, r, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}
