package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"zoiko.io/identity-envelope/govdecision"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/envelope"
	"zoiko.io/policy-svc/internal/evaluator"
	"zoiko.io/policy-svc/internal/resolve"
	"zoiko.io/policy-svc/internal/signatory"
	"zoiko.io/policy-svc/internal/store"
)

// Routes of the ZS-GOV-INT-002 signatory contract (Doc 02 §1, §4). The paths
// are the contract's own: no existing versioned endpoint conflicts with them.
const (
	SignatoryResolvePath = "/internal/v1/policy/signatory-matrix/resolve"
	DecisionPath         = "/internal/v1/policy/decisions/{decision_id}"
	DecisionKeysPath     = "/v1/policy-decision-keys"
)

// OwningService is this service's name on every decision it signs.
const OwningService = "policy-svc"

// HeaderDecisionReplayed marks an answer that is the stored decision of an
// earlier request with the same request_id.
const HeaderDecisionReplayed = "X-Decision-Replayed"

// DecisionObserver counts decisions (telemetry.Metrics in production).
type DecisionObserver interface {
	ObserveDecision(policyType, outcome string)
}

var (
	actionPattern       = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)
	jurisdictionPattern = regexp.MustCompile(`^[A-Z]{2}(-[A-Z0-9]{1,3})?$|^EU$`)
	documentHashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	requestIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
)

// RegisterSignatoryRoutes mounts the signatory contract and the decision-key
// set.
func RegisterSignatoryRoutes(r chi.Router, h *Handler) {
	r.Post(SignatoryResolvePath, h.ResolveSignatoryMatrix)
	r.Get(DecisionPath, h.GetDecision)
	r.Get(DecisionKeysPath, h.GetDecisionKeys)
}

// signatoryResolveRequest is Doc 02 §2.1's common context plus §4.1's input.
type signatoryResolveRequest struct {
	govdecision.RequestContext
	InstrumentType     string                 `json:"instrument_type"`
	Jurisdiction       string                 `json:"jurisdiction"`
	DocumentHash       string                 `json:"document_hash"`
	TermMonths         *int                   `json:"term_months,omitempty"`
	OptionsAmount      *string                `json:"options_amount,omitempty"`
	Converted          *signatoryConverted    `json:"converted,omitempty"`
	RelatedCommitments []signatoryCommitment  `json:"related_commitments,omitempty"`
	RiskFlags          []string               `json:"risk_flags,omitempty"`
	Context            map[string]interface{} `json:"context,omitempty"`
}

type signatoryConverted struct {
	Amount        string `json:"amount"`
	Currency      string `json:"currency"`
	FXSnapshotRef string `json:"fx_snapshot_ref"`
}

type signatoryCommitment struct {
	ResourceID string `json:"resource_id"`
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
}

// ResolveSignatoryMatrix handles POST /internal/v1/policy/signatory-matrix/resolve
// (Doc 02 §4): it resolves the active signer requirement for one instrument
// and answers the signed canonical decision. It decides no signer's
// authority and records no signature.
//
//	200 a decision: SIGNATURE_REQUIRED, PERMIT (explicit exemption) or
//	    NOT_CONFIGURED — the outcome, not the status, is the answer
//	400 schema violation · 401 no identity · 403 denied, foreign tenant or
//	    entity, or an actor other than the verified caller
//	409 request_id reused with different content
//	500 a stored matrix no longer matches its digest · 503 store unavailable
func (h *Handler) ResolveSignatoryMatrix(w http.ResponseWriter, r *http.Request) {
	if h.opts.DecisionSigner == nil {
		writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "decision_signer_unavailable", "this instance cannot sign decisions", nil)
		return
	}
	var req signatoryResolveRequest
	if !decodeStrictJSON(w, r, &req) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.validateSignatoryRequest(w, r, &req, tenantID) {
		return
	}
	legalEntityID := req.LegalEntityID
	principalID, ok := h.gate(w, r, h.opts.EvaluateAuthz, authz.ActionPolicyEvaluate, &legalEntityID)
	if !ok {
		return
	}
	if req.ActorPrincipalID == "" {
		req.ActorPrincipalID = principalID
	}

	input := signatory.Input{
		InstrumentType:      req.InstrumentType,
		Amount:              signatory.Amount(req.Money.Amount),
		Currency:            req.Money.Currency,
		Basis:               req.Money.Basis,
		RiskFlags:           sortedUnique(req.RiskFlags),
		DocumentHash:        req.DocumentHash,
		PreparerPrincipalID: req.ActorPrincipalID,
	}
	if req.OptionsAmount != nil {
		a := signatory.Amount(*req.OptionsAmount)
		input.OptionsAmount = &a
	}
	if c := req.Converted; c != nil {
		input.Converted = &signatory.Converted{Amount: signatory.Amount(c.Amount), Currency: c.Currency, FXSnapshotRef: c.FXSnapshotRef}
	}
	for _, rc := range req.RelatedCommitments {
		input.RelatedCommitments = append(input.RelatedCommitments, signatory.Commitment{ResourceID: rc.ResourceID, Amount: signatory.Amount(rc.Amount), Currency: rc.Currency})
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_request", err.Error(), nil)
		return
	}
	parsed, err := signatory.ParseInput(inputJSON)
	if err != nil {
		h.writeEvaluationError(w, r, signatory.PolicyType, "", toEvaluatorInputError(err))
		return
	}

	fp, err := govdecision.Fingerprint(map[string]any{
		"contract_major":      "1",
		"tenant_id":           tenantID,
		"legal_entity_id":     legalEntityID,
		"actor_principal_id":  req.ActorPrincipalID,
		"action":              req.Action,
		"resource":            req.Resource,
		"money":               req.Money,
		"instrument_type":     req.InstrumentType,
		"jurisdiction":        req.Jurisdiction,
		"document_hash":       req.DocumentHash,
		"term_months":         req.TermMonths,
		"options_amount":      req.OptionsAmount,
		"converted":           req.Converted,
		"related_commitments": req.RelatedCommitments,
		"risk_flags":          input.RiskFlags,
		"context":             req.Context,
		"evaluated_for_at":    req.EvaluatedForAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_request", err.Error(), nil)
		return
	}

	// A retry with the same request_id gets the original decision (Doc 02
	// §9.1); the same id with other content is a conflict, never a merge.
	if existing, err := h.store.FindEvaluationByDecision(r.Context(), tenantID, req.RequestID); err == nil {
		h.answerDecisionReplay(w, r, existing, fp)
		return
	} else if !errors.Is(err, domain.ErrEvaluationNotFound) {
		h.storeUnavailable(w, r, "ResolveSignatoryMatrix", err)
		return
	}

	now := h.opts.Now().UTC().Truncate(time.Microsecond)
	asOf := req.EvaluatedForAt.UTC()
	candidates, err := h.candidates.Candidates(r.Context(), signatory.PolicyType, &tenantID, &legalEntityID)
	if err != nil {
		h.storeUnavailable(w, r, "ResolveSignatoryMatrix", err)
		return
	}
	highRisk := parsed.HighRisk()
	covers := func(v *domain.ApplicablePolicyVersion) bool {
		return signatory.CoversPayload(v.RulePayload, parsed.InstrumentType, highRisk)
	}
	res := resolve.ResolveRanked(candidates, asOf, now, func(v *domain.ApplicablePolicyVersion) []int {
		return []int{boolInt(covers(v)), scopeSpecificity(v)}
	})

	d := &govdecision.Decision{
		ContractVersion:  govdecision.ContractVersion,
		DecisionID:       uuid.NewString(),
		RequestID:        req.RequestID,
		CorrelationID:    req.CorrelationID,
		OwningService:    OwningService,
		PolicyFamily:     signatory.PolicyType,
		TenantID:         tenantID,
		LegalEntityID:    legalEntityID,
		ActorPrincipalID: req.ActorPrincipalID,
		Action:           req.Action,
		Resource:         req.Resource,
		EvaluatedAt:      now,
		InputFingerprint: fp,
		RequiredActions:  []govdecision.RequiredAction{},
		UpstreamRefs:     []string{},
	}
	d.EvidenceRef = "policy-svc:policy-evaluations/" + d.DecisionID
	ttl := signatory.DefaultDecisionTTL
	detail := map[string]any{"resolution_status": string(res.Status), "as_of": asOf, "known_at": now, "jurisdiction": req.Jurisdiction}
	var selected *domain.ApplicablePolicyVersion
	var ruleIDs []string

	switch {
	case res.Status == resolve.Conflicted && covers(res.Conflicting[0]):
		// Overlapping matrices are invalid configuration, never first-match
		// success (Doc 01 §3.2).
		ids := make([]string, 0, len(res.Conflicting))
		for _, c := range res.Conflicting {
			ids = append(ids, c.PolicyVersionID)
		}
		detail["candidate_version_ids"] = ids
		h.recordSignatoryConflict(r, tenantID, legalEntityID, principalID, req.RequestID, res, ids)
		d.Outcome, d.ReasonCodes = govdecision.NotConfigured, []string{govdecision.ReasonConfigRuleOverlap}
		d.RuleID = signatory.PolicyType + "#overlap"
	case res.Status == resolve.Resolved && covers(res.Selected):
		selected = res.Selected
		if !digestMatches(selected) {
			h.log.Error("ResolveSignatoryMatrix: stored payload does not match its artifact digest",
				zap.String("policy_version_id", selected.PolicyVersionID))
			writeError(w, r, http.StatusInternalServerError, codeEvidenceIntegrity, "artifact_digest_mismatch", "",
				map[string]any{"policy_version_id": selected.PolicyVersionID, "outcome": govdecision.Error})
			return
		}
		d.PolicyID, d.PolicyCode, d.PolicyVersionID = selected.PolicyID, selected.PolicyCode, selected.PolicyVersionID
		d.PolicyVersion = strconv.Itoa(selected.VersionNumber)
		outcome, evalErr := evaluator.Evaluate(signatory.PolicyType, selected.PolicyVersionID, selected.RulePayload, inputJSON)
		var inErr *evaluator.InputError
		switch {
		case errors.As(evalErr, &inErr):
			h.writeEvaluationError(w, r, signatory.PolicyType, selected.PolicyVersionID, evalErr)
			return
		case evalErr != nil:
			// A stored matrix that no longer parses (authoring refuses
			// these) is a configuration fault: hold, visibly.
			h.log.Error("ResolveSignatoryMatrix: governing matrix cannot be evaluated",
				zap.String("policy_version_id", selected.PolicyVersionID), zap.Error(evalErr))
			d.Outcome, d.ReasonCodes = govdecision.NotConfigured, []string{govdecision.ReasonConfigPolicyInvalid}
			d.RuleID = selected.PolicyVersionID + "#invalid"
			ruleIDs = []string{d.RuleID}
		default:
			sr := outcome.Signatory
			d.Outcome, d.ReasonCodes, ruleIDs = sr.Outcome, sr.ReasonCodes, sr.RuleIDs
			d.RuleID = ruleIDs[len(ruleIDs)-1]
			if sr.RequiredActions != nil {
				d.RequiredActions = sr.RequiredActions
			}
			if sr.UpstreamRefs != nil {
				d.UpstreamRefs = sr.UpstreamRefs
			}
			detail["band_id"], detail["exemption_id"] = sr.Detail.BandID, sr.Detail.ExemptionID
			detail["compared_amount"], detail["currency"] = sr.Detail.ComparedAmount, sr.Detail.Currency
			detail["valuation_basis"], detail["high_risk"] = sr.Detail.ValuationBasis, sr.Detail.HighRisk
			detail["aggregated_series"] = sr.Detail.AggregatedSeries
			if m, err := signatory.ParseMatrix(selected.RulePayload); err == nil {
				ttl = m.TTL()
			}
		}
	default:
		// Nothing governs this instrument. A high-risk request needs an
		// instrument-specific matrix, whatever ordinary matrices exist.
		reason := govdecision.ReasonConfigPolicyMissing
		if highRisk {
			reason = govdecision.ReasonConfigInstrumentMatrixMissing
		}
		d.Outcome, d.ReasonCodes = govdecision.NotConfigured, []string{reason}
		d.RuleID = signatory.PolicyType + "#not_configured"
	}
	if len(ruleIDs) == 0 {
		ruleIDs = []string{d.RuleID}
	}
	detail["instrument_type"] = parsed.InstrumentType
	if raw, err := json.Marshal(detail); err == nil {
		d.Detail = raw
	}
	d.ValidUntil = now.Add(ttl)
	if selected != nil && selected.EffectiveTo != nil && selected.EffectiveTo.Before(d.ValidUntil) {
		d.ValidUntil = selected.EffectiveTo.UTC()
	}

	raw, err := h.opts.DecisionSigner.Sign(d)
	if err != nil {
		h.log.Error("ResolveSignatoryMatrix: could not sign the decision", zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, codeEvidenceIntegrity, "decision_signing_failed", "", map[string]any{"outcome": govdecision.Error})
		return
	}

	var causation, envRequestID string
	if env, ok := envelope.FromContext(r.Context()); ok {
		causation, envRequestID = env.CausationID, env.RequestID
	}
	validUntil := d.ValidUntil
	ev := &domain.Evaluation{
		EvaluationID:           d.DecisionID,
		TenantID:               tenantID,
		DecisionID:             req.RequestID,
		LegalEntityID:          &legalEntityID,
		PolicyType:             signatory.PolicyType,
		RequestedByPrincipalID: principalID,
		InputFingerprint:       fp,
		ActionContext:          inputJSON,
		AsOf:                   asOf,
		KnownAt:                now,
		Result:                 d.Outcome,
		Outcome:                d.Outcome,
		RuleBasis:              signatory.PolicyType + ":unresolved",
		SelectedRuleIDs:        ruleIDs,
		EvaluatorVersion:       signatory.EvaluatorVersion,
		CorrelationID:          req.CorrelationID,
		CausationID:            causation,
		RequestID:              envRequestID,
		EvaluatedAt:            now,
		ContractVersion:        govdecision.ContractVersion,
		DecisionOutcome:        d.Outcome,
		ReasonCodes:            d.ReasonCodes,
		ValidUntil:             &validUntil,
		DecisionEnvelope:       raw,
	}
	if selected != nil {
		ev.PolicyID, ev.PolicyVersionID, ev.PolicyCode, ev.ArtifactDigest = selected.PolicyID, selected.PolicyVersionID, selected.PolicyCode, selected.ArtifactDigest
		ev.RuleBasis = fmt.Sprintf("%s:%s", selected.PolicyCode, selected.PolicyVersionID)
	}
	saved, created, err := h.store.SaveEvaluation(r.Context(), ev)
	if err != nil {
		// No durable evidence, no decision (Doc 02 C02-P0-15).
		h.storeUnavailable(w, r, "ResolveSignatoryMatrix", err)
		return
	}
	if !created {
		h.answerDecisionReplay(w, r, saved, fp)
		return
	}
	h.observeDecision(signatory.PolicyType, d.Outcome)
	writeDecision(w, raw, false)
}

func (h *Handler) validateSignatoryRequest(w http.ResponseWriter, r *http.Request, req *signatoryResolveRequest, tenantID string) bool {
	bad := func(reason, msg, field string) bool {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, reason, msg, map[string]any{"field": field})
		return false
	}
	switch {
	case req.ContractVersion == "":
		return bad("missing_field", "contract_version is required", "contract_version")
	case !strings.HasPrefix(req.ContractVersion+".", "1."):
		return bad("unsupported_contract_version", "contract_version must be 1.x", "contract_version")
	case req.RequestID == "":
		return bad("missing_field", "request_id is required", "request_id")
	case !requestIDPattern.MatchString(req.RequestID):
		return bad("invalid_request_id", "request_id must be 1-64 characters of [A-Za-z0-9._:-]", "request_id")
	case strings.TrimSpace(req.CorrelationID) == "":
		return bad("missing_field", "correlation_id is required", "correlation_id")
	case len(req.CorrelationID) > 128:
		return bad("field_too_long", "correlation_id exceeds 128 characters", "correlation_id")
	case req.TenantID == "":
		return bad("missing_field", "tenant_id is required", "tenant_id")
	case req.LegalEntityID == "":
		return bad("missing_field", "legal_entity_id is required", "legal_entity_id")
	case !validUUID(req.TenantID):
		return bad("invalid_tenant_id", "tenant_id must be a UUID", "tenant_id")
	case !validUUID(req.LegalEntityID):
		return bad("invalid_legal_entity_id", "legal_entity_id must be a UUID", "legal_entity_id")
	}
	// The body names the scope; the verified identity decides it (Doc 02 §11,
	// C02-P0-03). A different tenant, entity or actor is refused, not
	// silently replaced.
	if !strings.EqualFold(req.TenantID, tenantID) {
		writeError(w, r, http.StatusForbidden, codeContextUnresolved, "tenant_scope_mismatch",
			"tenant_id does not match the caller's verified tenant scope", nil)
		return false
	}
	if env, ok := envelope.FromContext(r.Context()); ok && env.LegalEntityID != "" && env.LegalEntityID != "PLATFORM" &&
		!strings.EqualFold(env.LegalEntityID, req.LegalEntityID) {
		writeError(w, r, http.StatusForbidden, codeContextUnresolved, "legal_entity_scope_mismatch",
			"legal_entity_id does not match the caller's X-Legal-Entity-Id", nil)
		return false
	}
	if p := strings.TrimSpace(r.Header.Get("X-Principal-Id")); req.ActorPrincipalID != "" && p != "" && req.ActorPrincipalID != p {
		writeError(w, r, http.StatusForbidden, codeContextUnresolved, "actor_mismatch",
			"actor_principal_id must be the verified caller; a decision is never issued for someone else", nil)
		return false
	}
	switch {
	case req.Action == "":
		return bad("missing_field", "action is required", "action")
	case !actionPattern.MatchString(req.Action):
		return bad("invalid_action", "action must be an upper-case operation name such as SIGN_CONTRACT", "action")
	case strings.TrimSpace(req.Resource.Type) == "" || strings.TrimSpace(req.Resource.ID) == "" || strings.TrimSpace(req.Resource.Version) == "":
		return bad("missing_field", "resource needs type, id and version", "resource")
	case len(req.Resource.Type) > 64 || len(req.Resource.ID) > 128 || len(req.Resource.Version) > 64:
		return bad("field_too_long", "resource type, id or version is too long", "resource")
	case req.Money == nil:
		return bad("missing_field", "money is required: the committed value decides the band", "money")
	case req.InstrumentType == "":
		return bad("missing_field", "instrument_type is required", "instrument_type")
	case req.Jurisdiction == "":
		return bad("missing_field", "jurisdiction is required", "jurisdiction")
	case !jurisdictionPattern.MatchString(req.Jurisdiction):
		return bad("invalid_jurisdiction", "jurisdiction must be an ISO 3166 code (GB, US-CA) or EU", "jurisdiction")
	case req.DocumentHash == "":
		return bad("missing_field", "document_hash is required: signatures bind to the exact document", "document_hash")
	case !documentHashPattern.MatchString(req.DocumentHash):
		return bad("invalid_document_hash", "document_hash must be sha256:<64 lower-case hex>", "document_hash")
	case req.EvaluatedForAt.IsZero():
		return bad("missing_field", "evaluated_for_at is required", "evaluated_for_at")
	case req.TermMonths != nil && (*req.TermMonths < 0 || *req.TermMonths > 1200):
		return bad("invalid_term_months", "term_months must be between 0 and 1200", "term_months")
	case len(req.RelatedCommitments) > 100:
		return bad("too_many_related_commitments", "related_commitments holds at most 100 entries", "related_commitments")
	}
	return true
}

func toEvaluatorInputError(err error) error {
	var ie *signatory.InputError
	if errors.As(err, &ie) {
		field := strings.Replace(ie.Field, "action_context.", "", 1)
		switch field {
		case "amount", "currency", "basis":
			field = "money." + field
		}
		return &evaluator.InputError{Field: field, Reason: ie.Reason, Detail: ie.Detail}
	}
	return &evaluator.InputError{Field: "request", Reason: "invalid", Detail: err.Error()}
}

// answerDecisionReplay answers a request_id seen before: the stored decision
// when the content matches, 409 otherwise.
func (h *Handler) answerDecisionReplay(w http.ResponseWriter, r *http.Request, ev *domain.Evaluation, fingerprint string) {
	if ev.InputFingerprint != fingerprint || len(ev.DecisionEnvelope) == 0 {
		writeError(w, r, http.StatusConflict, codeIdempotencyMismatch, "request_id_conflict",
			"request_id was already used with different content", map[string]any{"request_id": ev.DecisionID})
		return
	}
	writeDecision(w, ev.DecisionEnvelope, true)
}

func writeDecision(w http.ResponseWriter, raw []byte, replayed bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if replayed {
		w.Header().Set(HeaderDecisionReplayed, "true")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (h *Handler) recordSignatoryConflict(r *http.Request, tenantID, legalEntityID, principalID, requestID string, res resolve.Result, ids []string) {
	meta := eventMeta(r, principalID)
	payload, _ := json.Marshal(map[string]any{
		"policy_type":               signatory.PolicyType,
		"tenant_id":                 tenantID,
		"legal_entity_id":           legalEntityID,
		"decision_id":               requestID,
		"as_of":                     res.AsOf,
		"known_at":                  res.KnownAt,
		"candidate_version_ids":     ids,
		"requested_by_principal_id": principalID,
	})
	if err := h.store.EnqueueEvent(r.Context(), store.OutboxEvent{
		EventType:      "policy.applicability.conflicted",
		TenantID:       &tenantID,
		LegalEntityID:  &legalEntityID,
		ActorID:        principalID,
		CorrelationID:  meta.CorrelationID,
		CausationID:    meta.CausationID,
		IdempotencyKey: "policy-conflict:" + tenantID + ":" + requestID,
		AggregateID:    signatory.PolicyType,
		Payload:        payload,
	}); err != nil {
		h.log.Error("ResolveSignatoryMatrix: could not record policy.applicability.conflicted", zap.Error(err))
	}
}

// GetDecision handles GET /internal/v1/policy/decisions/{decision_id}: the
// signed decision exactly as it was answered, for an enforcement point that
// re-checks it at its execution boundary (Doc 02 §8.4). Tenant-scoped.
func (h *Handler) GetDecision(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "decision_id")
	if !requireUUID(w, r, "decision_id", id) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return
	}
	ev, err := h.store.FindEvaluationByID(r.Context(), tenantID, id)
	switch {
	case errors.Is(err, domain.ErrEvaluationNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "decision_not_found", "", map[string]any{"decision_id": id})
		return
	case err != nil:
		h.storeUnavailable(w, r, "GetDecision", err)
		return
	}
	if len(ev.DecisionEnvelope) == 0 {
		// An evaluation of the legacy route has no canonical envelope.
		writeError(w, r, http.StatusNotFound, codeNotFound, "decision_not_found", "this evaluation has no canonical decision envelope", map[string]any{"decision_id": id})
		return
	}
	writeDecision(w, ev.DecisionEnvelope, false)
}

// GetDecisionKeys handles GET /v1/policy-decision-keys: the public keys that
// verify this service's decisions (a JWKS of Ed25519 keys).
func (h *Handler) GetDecisionKeys(w http.ResponseWriter, r *http.Request) {
	if h.opts.DecisionSigner == nil {
		writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "decision_signer_unavailable", "", nil)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, govdecision.JWKS{Keys: []govdecision.JWK{h.opts.DecisionSigner.JWK()}})
}

func (h *Handler) observeDecision(policyType, outcome string) {
	if h.opts.Decisions != nil {
		h.opts.Decisions.ObserveDecision(policyType, outcome)
	}
}

// decodeStrictJSON is decodeJSON for a contract route: an unknown member is a
// schema violation (Doc 02 §9: 400), so a misspelt field is never ignored.
func decodeStrictJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, r, http.StatusRequestEntityTooLarge, codeInvalidRequest, "request_too_large", "", nil)
			return false
		}
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "schema_violation", err.Error(), nil)
		return false
	}
	return true
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scopeSpecificity(v *domain.ApplicablePolicyVersion) int {
	n := 0
	if v.TenantID != nil {
		n++
	}
	if v.LegalEntityID != nil {
		n++
	}
	return n
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
