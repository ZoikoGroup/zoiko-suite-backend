package handler

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// The three routes Doc 03 §8.3 lists as inbound APIs of this service and that
// had no HTTP surface at all until now.
//
// ── WHY THEY EXIST SEPARATELY FROM /v1/authorize ────────────────────────────
//
// §8.3's inbound API list is five entries: "evaluate action authorization",
// "validate entity scope", "validate SoD conflicts", "evaluate delegated
// access", "retrieve authorization rationale". Three of them were folded into
// POST /v1/authorize as internal layers and recorded as a deliberate
// simplification, on the reasoning that the capability existed even if the
// surface did not.
//
// The capability does exist. What folding them cost is answerable questions
// that /v1/authorize structurally cannot answer, and each of the three is a
// question somebody has to ask BEFORE a material action rather than about one:
//
//	entity scope     "which of these entities may this principal act in?"
//	                 Through /v1/authorize that is one call per (entity,
//	                 action) pair, and — because every evaluation records its
//	                 artifact — N rows in access_decision_log for a question
//	                 nobody acted on. A console listing twelve entities would
//	                 write twelve decision artifacts to grey out four buttons.
//
//	SoD conflicts    "would granting this role to this person break
//	                 segregation of duties?" /v1/authorize can only answer
//	                 that AFTER the grant exists, because it evaluates what a
//	                 principal holds, not what they are about to hold. So the
//	                 only way to discover the conflict was to create the
//	                 assignment and then watch every use of it be denied.
//	                 That is the control working and the workflow failing.
//
//	delegated access "is this principal acting on their own authority, or
//	                 somebody else's?" /v1/authorize collapses both into one
//	                 GRANTED, and the basis distinguishes them only when
//	                 delegation happened to be the layer that granted — a
//	                 principal who holds an action BOTH directly and by
//	                 delegation reads as pure RBAC. For an approval workflow
//	                 that has to know whether a four-eyes step was satisfied
//	                 by the delegate or the delegator, that is the whole
//	                 question.
//
// ── NONE OF THE THREE RECORDS A DECISION ARTIFACT ───────────────────────────
//
// Deliberate, and the most important property of this file. The critical
// constraint is "no material action executes without an authorization decision
// artifact". These endpoints authorize no action: they answer a question about
// the shape of the grant graph. Recording them would put rows in
// access_decision_log for questions nobody acted on, which makes the log stop
// meaning "one row per authorization of a material act" — and that meaning is
// what an auditor reads it for.
//
// The consequence is that they are an information surface with no artifact
// behind them, so all three REQUIRE a verified principal and tenant, exactly
// as the /v1/admin/* reads do, and all three log what was asked. That is the
// trade taken knowingly: authenticated and logged, rather than unauthenticated
// and recorded as evidence of something that did not happen.
//
// ── AND NONE OF THE THREE IS A MATERIAL WRITE ───────────────────────────────
//
// All three are POST because they take a body — a set of candidate actions
// does not belong in a query string — and all three are classified as
// non-writes by MaterialWrite for the same reason POST /v1/authorize is: they
// change nothing. Adding a route here without adding it there is how one of
// them starts refusing its callers for want of an Idempotency-Key on a
// question.
const (
	EntityScopeValidatePath     = "/v1/entity-scope/validate"
	SoDValidatePath             = "/v1/sod/validate"
	DelegatedAccessEvaluatePath = "/v1/delegated-access/evaluate"
)

// ── POST /v1/entity-scope/validate ──────────────────────────────────────────

type entityScopeRequest struct {
	PrincipalID string `json:"principal_id"`

	// LegalEntityIDs is the set of entities to test. A set rather than one
	// entity because the question is almost always "which of these", and
	// asking it one HTTP call at a time is the cost this endpoint exists to
	// remove.
	//
	// PlatformScopeSentinel is accepted here on the same terms as on
	// /v1/authorize and resolves to the same configured entity.
	LegalEntityIDs []string `json:"legal_entity_ids"`

	// ActionType is OPTIONAL and changes the question:
	//
	//	omitted  is the principal in scope for this entity AT ALL — do they
	//	         hold any grant there? This is the "which entities should be
	//	         selectable" question.
	//	given    is the principal in scope for this entity FOR THIS ACTION?
	//	         Narrower, and the one a caller about to offer a specific
	//	         button wants.
	ActionType string `json:"action_type,omitempty"`

	// TenantID is the same body fallback /v1/authorize carries, for callers
	// that do not forward X-Tenant-Id yet. The header wins; a body that
	// disagrees is refused.
	TenantID string `json:"tenant_id,omitempty"`
}

type entityScopeResult struct {
	LegalEntityID string `json:"legal_entity_id"`
	InScope       bool   `json:"in_scope"`

	// Basis names WHY, in the same vocabulary decision_basis uses, so a
	// caller can show the same explanation the decision log would give:
	// "rbac:role=X", "delegated:from=Y", "no_grant".
	Basis string `json:"basis"`

	// PermittedActions is what the principal holds in this entity — RBAC
	// union delegated. Returned only when ActionType was omitted, i.e. when
	// the caller asked the broad question and is choosing what to offer;
	// suppressed on the narrow question because a caller asking about one
	// action has not asked for the full grant set and should not be handed it.
	PermittedActions []string `json:"permitted_actions,omitempty"`
}

type entityScopeResponse struct {
	PrincipalID string              `json:"principal_id"`
	ActionType  string              `json:"action_type,omitempty"`
	Results     []entityScopeResult `json:"results"`
}

// maxEntityScopeBatch bounds one request. Each entity costs two store reads
// (grants, then delegations only if grants did not answer), and the reads are
// cached per (principal, entity), so a large batch is cheap on the second call
// and not on the first. 100 is well above any real entity list and far below a
// batch somebody could use to walk the estate.
const maxEntityScopeBatch = 100

// ValidateEntityScope handles POST /v1/entity-scope/validate — §8.3's
// "validate entity scope".
//
// Records NO decision artifact; requires a verified principal and tenant. See
// this file's header for both.
//
// Response: 200 one result per requested entity, in the order asked / 400
// missing or oversized input, or PLATFORM on an unconfigured deployment / 401
// missing principal or tenant / 403 body tenant disagrees with the header /
// 503 store unavailable.
func (h *Handler) ValidateEntityScope(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req entityScopeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if strings.TrimSpace(req.PrincipalID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "principal_id"})
		return
	}
	if len(req.LegalEntityIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "legal_entity_ids"})
		return
	}
	if len(req.LegalEntityIDs) > maxEntityScopeBatch {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "batch_too_large",
			"field":   "legal_entity_ids",
			"message": "at most 100 entities per request",
		})
		return
	}

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	results := make([]entityScopeResult, 0, len(req.LegalEntityIDs))
	for _, requested := range req.LegalEntityIDs {
		entityID := strings.TrimSpace(requested)
		if entityID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "missing_field", "field": "legal_entity_ids",
				"message": "legal_entity_ids may not contain an empty value",
			})
			return
		}

		evaluationEntityID, ok := h.resolvePlatformScope(w, entityID, correlationID)
		if !ok {
			return
		}

		rbacActions, basis, err := h.store.FindGrantedActions(r.Context(), req.PrincipalID, evaluationEntityID, tenantScope)
		if err != nil {
			h.log.Error("ValidateEntityScope: store unavailable (rbac lookup)",
				zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}

		held := append([]string{}, rbacActions...)

		// Delegated grants count towards being in scope: a delegate acting in
		// an entity is in scope for it, and an endpoint that said otherwise
		// would grey out exactly the buttons a delegation was created to
		// enable. Looked up unconditionally rather than only when RBAC came
		// back empty, because PermittedActions has to be the whole set —
		// /v1/authorize can stop early since it only needs one action, this
		// endpoint is answering what the principal holds.
		delegatedActions, delegatedBasis, err := h.store.FindDelegatedActions(r.Context(), req.PrincipalID, evaluationEntityID, tenantScope)
		if err != nil {
			h.log.Error("ValidateEntityScope: store unavailable (delegation lookup)",
				zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		held = append(held, delegatedActions...)

		result := entityScopeResult{LegalEntityID: entityID}
		switch {
		case req.ActionType != "":
			result.InScope = contains(held, req.ActionType)
			switch {
			case !result.InScope:
				result.Basis = "no_grant"
			case contains(rbacActions, req.ActionType):
				result.Basis = basis
			default:
				result.Basis = delegatedBasis
			}
		default:
			result.InScope = len(held) > 0
			result.PermittedActions = dedupeSorted(held)
			switch {
			case !result.InScope:
				result.Basis = "no_grant"
			case len(rbacActions) > 0:
				result.Basis = basis
			default:
				result.Basis = delegatedBasis
			}
		}
		results = append(results, result)
	}

	// Logged rather than recorded: no decision artifact, but the question is
	// still a read of who-can-do-what and the caller is named.
	h.log.Info("entity scope validated",
		zap.String("caller_principal_id", callerPrincipal),
		zap.String("subject_principal_id", req.PrincipalID),
		zap.String("action_type", req.ActionType),
		zap.Int("entities", len(req.LegalEntityIDs)),
		zap.String("correlation_id", correlationID))

	writeJSON(w, http.StatusOK, entityScopeResponse{
		PrincipalID: req.PrincipalID,
		ActionType:  req.ActionType,
		Results:     results,
	})
}

// ── POST /v1/sod/validate ───────────────────────────────────────────────────

type sodValidateRequest struct {
	// CandidateActions is what is ABOUT to be granted — the actions in the
	// permission bundle of a role somebody is considering assigning.
	CandidateActions []string `json:"candidate_actions"`

	// PrincipalID and LegalEntityID are optional together. Given, the
	// principal's CURRENT grants (RBAC union delegated) in that entity are
	// resolved and the candidates are checked against them as well as against
	// each other — which is the real question: "may THIS person hold this
	// role". Omitted, only the candidates are checked against each other,
	// which answers "is this bundle internally conflicted" for somebody
	// designing a role before anyone holds it.
	PrincipalID   string `json:"principal_id,omitempty"`
	LegalEntityID string `json:"legal_entity_id,omitempty"`

	TenantID string `json:"tenant_id,omitempty"`
}

type sodConflict struct {
	// CandidateAction is the action that cannot be granted.
	CandidateAction string `json:"candidate_action"`

	// ConflictsWith is the action it conflicts with.
	ConflictsWith string `json:"conflicts_with"`

	// Source says where ConflictsWith came from, because the remedy is
	// different for each: "held" means the principal already has it and
	// something must be taken away first; "candidate" means the bundle
	// conflicts with itself and must be split.
	Source string `json:"source"`
}

type sodValidateResponse struct {
	ConflictFree bool          `json:"conflict_free"`
	Conflicts    []sodConflict `json:"conflicts"`

	// OwnObjectRestricted lists candidate actions that a data-declared
	// own-object rule forbids performing on a resource the principal also
	// owns. NOT a conflict — the grant is legitimate and the restriction
	// applies per-request, at evaluation time, against a specific object. It
	// is reported because somebody assigning the role should know the action
	// will be refused when the holder is also the preparer.
	OwnObjectRestricted []string `json:"own_object_restricted,omitempty"`
}

// maxCandidateActions bounds one request. The conflict check is O(candidates ×
// (held + candidates)) store reads, all cached per (tenant, action, held-set),
// so the bound is about the worst first call rather than the steady state.
const maxCandidateActions = 200

// ValidateSoDConflicts handles POST /v1/sod/validate — §8.3's "validate SoD
// conflicts".
//
// ── WHAT THIS ANSWERS THAT /v1/authorize CANNOT ─────────────────────────────
//
// /v1/authorize evaluates what a principal HOLDS. Separation of duties is
// violated by a COMBINATION, so the conflict only becomes visible once the
// combination exists — which means the only way to discover that a role must
// not be granted to somebody was to grant it and then watch every use of it be
// denied with sod:conflict_with=. The control worked; the operator was left
// with a live assignment that confers nothing and no explanation until they
// read a decision log.
//
// This endpoint asks the question in the other direction: given what this
// principal already holds, would these actions be grantable? It is a
// pre-flight check, it records no decision artifact, and it is what the
// console's grant form calls before offering to write the assignment.
//
// ── CANDIDATES ARE CHECKED AGAINST EACH OTHER TOO ───────────────────────────
//
// A permission bundle can be internally conflicted — PAYMENT_INITIATE and
// PAYMENT_APPROVE in one bundle is the textbook case — and such a bundle
// grants both actions to everyone who holds the role, then denies both to all
// of them. Checking candidates only against currently-held actions would call
// that bundle conflict-free.
//
// Response: 200 the verdict / 400 missing or oversized input / 401 missing
// principal or tenant / 403 body tenant disagrees with the header / 503 store
// unavailable.
func (h *Handler) ValidateSoDConflicts(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req sodValidateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}

	candidates := dedupeSorted(req.CandidateActions)
	if len(candidates) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "candidate_actions"})
		return
	}
	if len(candidates) > maxCandidateActions {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "batch_too_large",
			"field":   "candidate_actions",
			"message": "at most 200 candidate actions per request",
		})
		return
	}

	// PrincipalID without LegalEntityID is refused rather than silently
	// downgraded to the bundle-only check. Grants are entity-scoped, so
	// "what does this principal hold" has no answer without an entity, and
	// answering the narrower question instead would return conflict_free:true
	// for a principal who does conflict — a false all-clear on a control.
	if strings.TrimSpace(req.PrincipalID) != "" && strings.TrimSpace(req.LegalEntityID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "missing_field",
			"field":   "legal_entity_id",
			"message": "legal_entity_id is required when principal_id is given — grants are entity-scoped, so what a principal holds cannot be resolved without one",
		})
		return
	}

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	var held []string
	if strings.TrimSpace(req.PrincipalID) != "" {
		evaluationEntityID, ok := h.resolvePlatformScope(w, strings.TrimSpace(req.LegalEntityID), correlationID)
		if !ok {
			return
		}

		rbacActions, _, err := h.store.FindGrantedActions(r.Context(), req.PrincipalID, evaluationEntityID, tenantScope)
		if err != nil {
			h.log.Error("ValidateSoDConflicts: store unavailable (rbac lookup)",
				zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		delegatedActions, _, err := h.store.FindDelegatedActions(r.Context(), req.PrincipalID, evaluationEntityID, tenantScope)
		if err != nil {
			h.log.Error("ValidateSoDConflicts: store unavailable (delegation lookup)",
				zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		// Delegated grants are included for the same reason /v1/authorize's
		// SoD layer includes them: a conflict reached through a delegation is
		// a conflict. Excluding them here would clear a grant that the
		// evaluation engine then denies, which is the worst of the two errors
		// this endpoint can make.
		held = dedupeSorted(append(rbacActions, delegatedActions...))
	}

	resp := sodValidateResponse{Conflicts: []sodConflict{}}
	for _, candidate := range candidates {
		// The "other actions" for this candidate: everything held, plus every
		// OTHER candidate. The candidate itself is excluded — CheckSoDConflict
		// searches for a rule pairing the candidate with something else, and
		// leaving it in the held set would make a self-referential
		// OWN_OBJECT_FORBIDDEN row look like a static pair conflict.
		others := removeAll(append(append([]string{}, held...), candidates...), candidate)

		conflictingAction, hasConflict, err := h.store.CheckSoDConflict(r.Context(), others, candidate, tenantScope)
		if err != nil {
			h.log.Error("ValidateSoDConflicts: store unavailable (sod check)",
				zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		if hasConflict {
			source := "candidate"
			if contains(held, conflictingAction) {
				// Held wins the label when it is both: the remedy is to
				// revoke something the principal has, which is a different
				// and larger act than splitting a bundle.
				source = "held"
			}
			resp.Conflicts = append(resp.Conflicts, sodConflict{
				CandidateAction: candidate,
				ConflictsWith:   conflictingAction,
				Source:          source,
			})
		}

		ownObject, err := h.store.CheckOwnObjectSoD(r.Context(), candidate, tenantScope)
		if err != nil {
			h.log.Error("ValidateSoDConflicts: store unavailable (own-object sod check)",
				zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		if ownObject {
			resp.OwnObjectRestricted = append(resp.OwnObjectRestricted, candidate)
		}
	}

	// conflict_free reflects CONFLICTS only, not own-object restrictions: an
	// own-object rule does not make the grant wrong, it makes one use of it
	// refused. Folding the two together would have this endpoint refuse to
	// clear a role that is perfectly grantable.
	resp.ConflictFree = len(resp.Conflicts) == 0

	h.log.Info("sod conflicts validated",
		zap.String("caller_principal_id", callerPrincipal),
		zap.String("subject_principal_id", req.PrincipalID),
		zap.Int("candidate_actions", len(candidates)),
		zap.Int("conflicts", len(resp.Conflicts)),
		zap.String("correlation_id", correlationID))

	writeJSON(w, http.StatusOK, resp)
}

// ── POST /v1/delegated-access/evaluate ──────────────────────────────────────

type delegatedAccessRequest struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`

	// ActionType is optional: omitted, the whole delegated action set is
	// returned; given, the answer narrows to that one action.
	ActionType string `json:"action_type,omitempty"`

	TenantID string `json:"tenant_id,omitempty"`
}

type delegatedAccessResponse struct {
	PrincipalID   string `json:"principal_id"`
	LegalEntityID string `json:"legal_entity_id"`

	// HasDelegatedAccess answers the question asked: for the action if one was
	// named, otherwise "does this principal hold anything by delegation here".
	HasDelegatedAccess bool `json:"has_delegated_access"`

	// Basis names the delegator, in decision_basis' own vocabulary
	// ("delegated:from=..."), or "no_delegated_grant".
	Basis string `json:"basis"`

	// DelegatedActions is everything reachable by delegation in this entity —
	// already intersected with the delegators' LIVE grants by the store, so it
	// can never list an action a delegator no longer holds.
	DelegatedActions []string `json:"delegated_actions,omitempty"`

	// HeldDirectly reports whether the principal ALSO holds the named action
	// through their own roles. This is the field that makes the endpoint worth
	// having: /v1/authorize returns one GRANTED for both paths and names RBAC
	// as the basis when both apply, so a four-eyes step could be satisfied by
	// the delegator's own authority while the workflow believed a delegate had
	// acted. Only meaningful when ActionType was given.
	HeldDirectly bool `json:"held_directly,omitempty"`
}

// EvaluateDelegatedAccess handles POST /v1/delegated-access/evaluate — §8.3's
// "evaluate delegated access".
//
// Isolates layer 2 of the evaluation. Records no decision artifact; requires a
// verified principal and tenant. See this file's header.
//
// Response: 200 the answer / 400 missing field, or PLATFORM on an unconfigured
// deployment / 401 missing principal or tenant / 403 body tenant disagrees with
// the header / 503 store unavailable.
func (h *Handler) EvaluateDelegatedAccess(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req delegatedAccessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if strings.TrimSpace(req.PrincipalID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "principal_id"})
		return
	}
	if strings.TrimSpace(req.LegalEntityID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "legal_entity_id"})
		return
	}

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	evaluationEntityID, ok := h.resolvePlatformScope(w, strings.TrimSpace(req.LegalEntityID), correlationID)
	if !ok {
		return
	}

	delegatedActions, delegatedBasis, err := h.store.FindDelegatedActions(r.Context(), req.PrincipalID, evaluationEntityID, tenantScope)
	if err != nil {
		h.log.Error("EvaluateDelegatedAccess: store unavailable (delegation lookup)",
			zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	resp := delegatedAccessResponse{
		PrincipalID:      req.PrincipalID,
		LegalEntityID:    req.LegalEntityID,
		DelegatedActions: dedupeSorted(delegatedActions),
	}

	if req.ActionType != "" {
		resp.HasDelegatedAccess = contains(delegatedActions, req.ActionType)

		rbacActions, _, err := h.store.FindGrantedActions(r.Context(), req.PrincipalID, evaluationEntityID, tenantScope)
		if err != nil {
			h.log.Error("EvaluateDelegatedAccess: store unavailable (rbac lookup)",
				zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		resp.HeldDirectly = contains(rbacActions, req.ActionType)
	} else {
		resp.HasDelegatedAccess = len(delegatedActions) > 0
	}

	if resp.HasDelegatedAccess {
		resp.Basis = delegatedBasis
	} else {
		resp.Basis = "no_delegated_grant"
	}

	h.log.Info("delegated access evaluated",
		zap.String("caller_principal_id", callerPrincipal),
		zap.String("subject_principal_id", req.PrincipalID),
		zap.String("action_type", req.ActionType),
		zap.Bool("has_delegated_access", resp.HasDelegatedAccess),
		zap.String("correlation_id", correlationID))

	writeJSON(w, http.StatusOK, resp)
}

// ── shared ──────────────────────────────────────────────────────────────────

// resolvePlatformScope turns a caller-supplied legal_entity_id into the id to
// evaluate against, resolving PlatformScopeSentinel and refusing it on a
// deployment with no platform-scope entity configured.
//
// Extracted from Authorize when these three routes were added: all four accept
// the sentinel, and four copies of the resolution is how one of them ends up
// evaluating the literal string "PLATFORM" against a uuid column — which is a
// 503 that reads as an outage, not a 400 that names the missing configuration.
func (h *Handler) resolvePlatformScope(w http.ResponseWriter, legalEntityID, correlationID string) (string, bool) {
	if legalEntityID != PlatformScopeSentinel {
		return legalEntityID, true
	}
	if h.platformScopeEntityID == "" {
		h.log.Error("platform scope requested but AUTHZ_PLATFORM_SCOPE_ENTITY_ID is unset",
			zap.String("correlation_id", correlationID))
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "platform_scope_not_configured",
			"message": "legal_entity_id=PLATFORM requires AUTHZ_PLATFORM_SCOPE_ENTITY_ID to be configured on authorization-svc",
		})
		return "", false
	}
	return h.platformScopeEntityID, true
}

// dedupeSorted returns the distinct values of in, sorted.
//
// Sorted so a response is stable across calls — these endpoints answer the same
// question repeatedly and a set that reorders between two identical requests
// looks like a change in the grant graph. Deduped because the union of RBAC and
// delegated actions genuinely overlaps: a principal who holds an action
// directly AND by delegation would otherwise see it listed twice.
func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}
