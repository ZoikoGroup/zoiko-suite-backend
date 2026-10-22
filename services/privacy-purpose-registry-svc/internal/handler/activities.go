package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
)

// ── create ───────────────────────────────────────────────────────────────────

// validateActivityEnums enforces the structural enumerations that can be
// rejected at write time. Completeness (owners, lists, registries) is the job
// of the activation gates, not of draft creation.
func validateActivityEnums(w http.ResponseWriter, role, owner, notice, dpia string) bool {
	if !domain.PrivacyRole(role).Valid() {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "privacy_role must be one of CONTROLLER, PROCESSOR, JOINT_CONTROLLER")
		return false
	}
	if strings.TrimSpace(owner) == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "owner is required")
		return false
	}
	if notice != "" && !domain.NoticeConsentDependency(notice).Valid() {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "notice_consent_dependency must be REQUIRED, NOT_REQUIRED, or CONDITIONAL")
		return false
	}
	if dpia != "" && !domain.DPIATIAStatus(dpia).Valid() {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "dpia_tia_status must be RESOLVED, REVIEW_REQUIRED, or NOT_REQUIRED")
		return false
	}
	return true
}

func (h *Handler) CreateActivity(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.begin(w, r, PrivacyActivityCreate, true)
	if !ok {
		return
	}
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	key, hash, handled := h.idempotency(w, r, cc, body)
	if handled {
		return
	}

	var req domain.CreateActivityRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "invalid request body")
		return
	}
	if !validateActivityEnums(w, req.PrivacyRole, req.Owner, req.NoticeConsentDependency, req.DPIATIAStatus) {
		return
	}
	if scopeMismatch(w, req.TenantID, cc) {
		return
	}
	req.Owner, req.PrivacyOwner = strings.TrimSpace(req.Owner), strings.TrimSpace(req.PrivacyOwner)
	req.Jurisdictions = domain.NormalizeCodes(req.Jurisdictions)
	req.DataCategories = domain.NormalizeCodes(req.DataCategories)
	req.SubjectClasses = domain.NormalizeCodes(req.SubjectClasses)
	req.PurposeIDs = domain.NormalizeRefs(req.PurposeIDs)
	req.Sources = domain.NormalizeRefs(req.Sources)
	req.Recipients = domain.NormalizeRefs(req.Recipients)
	req.RetentionRuleRefs = domain.NormalizeRefs(req.RetentionRuleRefs)
	req.TransferRefs = domain.NormalizeRefs(req.TransferRefs)
	req.ProcessorRefs = domain.NormalizeRefs(req.ProcessorRefs)

	payload, existing, err := h.execute(r, cc, "CreateActivity", key, hash, http.StatusCreated, func(ctx context.Context) (any, error) {
		_, version, err := h.store.CreateActivity(ctx, cc.RecordTenant(), req, cc.Principal)
		return version, err
	})
	if err != nil {
		h.storeUnavailable(w, "CreateActivity", err)
		return
	}
	h.finish(w, existing, hash, http.StatusCreated, payload)
}

func (h *Handler) CreateActivityVersion(w http.ResponseWriter, r *http.Request) {
	activityID := chi.URLParam(r, "activityID")
	cc, ok := h.begin(w, r, PrivacyActivityCreate, true)
	if !ok {
		return
	}
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	key, hash, handled := h.idempotency(w, r, cc, body)
	if handled {
		return
	}

	var req domain.CreateActivityVersionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "invalid request body")
		return
	}
	if req.ParentVersionID == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "parent_version_id is required")
		return
	}
	if !validateActivityEnums(w, req.PrivacyRole, req.Owner, req.NoticeConsentDependency, req.DPIATIAStatus) {
		return
	}

	parent, err := h.store.FindActivityVersion(r.Context(), activityID, req.ParentVersionID)
	if err != nil {
		if errors.Is(err, domain.ErrActivityVersionNotFound) {
			writeCoded(w, http.StatusNotFound, domain.ErrCodeNotFound, "parent processing activity version not found")
			return
		}
		h.storeUnavailable(w, "CreateActivityVersion", err)
		return
	}
	// Platform tenant isolation (§23.1): only the owning plane may extend a record.
	if wrongPlane(w, cc, parent.TenantID) {
		return
	}
	req.Owner, req.PrivacyOwner = strings.TrimSpace(req.Owner), strings.TrimSpace(req.PrivacyOwner)
	req.Jurisdictions = domain.NormalizeCodes(req.Jurisdictions)
	req.DataCategories = domain.NormalizeCodes(req.DataCategories)
	req.SubjectClasses = domain.NormalizeCodes(req.SubjectClasses)
	req.PurposeIDs = domain.NormalizeRefs(req.PurposeIDs)
	req.Sources = domain.NormalizeRefs(req.Sources)
	req.Recipients = domain.NormalizeRefs(req.Recipients)
	req.RetentionRuleRefs = domain.NormalizeRefs(req.RetentionRuleRefs)
	req.TransferRefs = domain.NormalizeRefs(req.TransferRefs)
	req.ProcessorRefs = domain.NormalizeRefs(req.ProcessorRefs)

	payload, existing, err := h.execute(r, cc, "CreateActivityVersion", key, hash, http.StatusCreated, func(ctx context.Context) (any, error) {
		return h.store.CreateActivityVersion(ctx, activityID, req, cc.Principal)
	})
	if err != nil {
		if errors.Is(err, domain.ErrActivityVersionNotFound) {
			writeCoded(w, http.StatusNotFound, domain.ErrCodeNotFound, "parent processing activity version not found")
			return
		}
		h.storeUnavailable(w, "CreateActivityVersion", err)
		return
	}
	h.finish(w, existing, hash, http.StatusCreated, payload)
}

// ── reads ────────────────────────────────────────────────────────────────────

// GetActivity handles GET /privacy/processing-activities/{id}?as_of=&known_as_of=
// — the version that GOVERNED the activity at as_of and that the platform had
// approved by known_as_of (§9.1, §20.3). With neither parameter it answers
// "what governs now"; an activity with no governing version is PRV-002.
func (h *Handler) GetActivity(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, PrivacyActivityRead, false); !ok {
		return
	}
	asOf, knownAsOf, ok := h.parseInstants(w, r)
	if !ok {
		return
	}
	version, err := h.store.ResolveActivityAsOf(r.Context(), chi.URLParam(r, "activityID"), asOf, knownAsOf)
	if err != nil {
		if errors.Is(err, domain.ErrActivityNotFound) {
			writeCoded(w, http.StatusNotFound, domain.ErrCodeActivityInactive, "no processing-activity version governed this activity at the requested time")
			return
		}
		h.storeUnavailable(w, "GetActivity", err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}

func (h *Handler) GetActivityVersion(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, PrivacyActivityRead, false); !ok {
		return
	}
	version, err := h.store.FindActivityVersion(r.Context(), chi.URLParam(r, "activityID"), chi.URLParam(r, "versionID"))
	if err != nil {
		if errors.Is(err, domain.ErrActivityVersionNotFound) {
			writeCoded(w, http.StatusNotFound, domain.ErrCodeNotFound, "processing activity version not found")
			return
		}
		h.storeUnavailable(w, "GetActivityVersion", err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}

func (h *Handler) ListROPA(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, PrivacyRopaRead, false); !ok {
		return
	}
	role := strings.TrimSpace(r.URL.Query().Get("role"))
	if role != "" && !domain.PrivacyRole(role).Valid() {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "role must be one of CONTROLLER, PROCESSOR, JOINT_CONTROLLER")
		return
	}
	jurisdiction := domain.NormalizeCode(r.URL.Query().Get("jurisdiction"))

	versions, err := h.store.ListActiveActivities(r.Context(), role, jurisdiction)
	if err != nil {
		h.storeUnavailable(w, "ListROPA", err)
		return
	}
	if versions == nil {
		versions = []domain.ProcessingActivityVersion{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": versions, "count": len(versions)})
}

// ── lifecycle ────────────────────────────────────────────────────────────────

// lifecycleSpec describes one lifecycle operation. Every transition runs the
// same pipeline (lifecycleAction): pre-flight, body, idempotency, load the
// version (the latest, for the canonical routes — resolved only AFTER
// authentication and authorization, so an anonymous caller cannot probe
// activity ids), refuse a record the caller's plane does not own, then the
// operation's own check and its store call.
type lifecycleSpec struct {
	authz string
	op    string
	// check validates the loaded version against the operation's rules. It
	// returns an operation argument (the rejection reason) and false after
	// writing the refusal itself.
	check func(ctx context.Context, w http.ResponseWriter, cc *callContext, v *domain.ProcessingActivityVersion, body []byte) (arg string, ok bool)
	// run performs the store mutation; the store writes the audit row and the
	// outbox event in the same transaction.
	run func(ctx context.Context, cc *callContext, v *domain.ProcessingActivityVersion, arg string) (*domain.ProcessingActivityVersion, error)
}

func conflictState(w http.ResponseWriter, need domain.ActivityVersionStatus, have domain.ActivityVersionStatus) {
	writeCoded(w, http.StatusConflict, domain.ErrCodeInvalidTransition,
		fmt.Sprintf("version is %s; this operation requires %s", have, need))
}

func (h *Handler) lifecycleAction(w http.ResponseWriter, r *http.Request, activityID, versionID string, s lifecycleSpec) {
	cc, ok := h.begin(w, r, s.authz, true)
	if !ok {
		return
	}
	body, ok := h.readBody(w, r)
	if !ok {
		return
	}
	key, hash, handled := h.idempotency(w, r, cc, body)
	if handled {
		return
	}

	var existing *domain.ProcessingActivityVersion
	var err error
	if versionID == "" {
		existing, err = h.store.FindLatestActivityVersion(r.Context(), activityID)
	} else {
		existing, err = h.store.FindActivityVersion(r.Context(), activityID, versionID)
	}
	if err != nil {
		if errors.Is(err, domain.ErrActivityNotFound) || errors.Is(err, domain.ErrActivityVersionNotFound) {
			writeCoded(w, http.StatusNotFound, domain.ErrCodeNotFound, "processing activity version not found")
			return
		}
		h.storeUnavailable(w, s.op, err)
		return
	}
	if wrongPlane(w, cc, existing.TenantID) {
		return
	}

	arg := ""
	if s.check != nil {
		if arg, ok = s.check(r.Context(), w, cc, existing, body); !ok {
			return
		}
	}

	payload, replay, err := h.execute(r, cc, s.op, key, hash, http.StatusOK, func(ctx context.Context) (any, error) {
		return s.run(ctx, cc, existing, arg)
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrActivityVersionNotFound):
			writeCoded(w, http.StatusNotFound, domain.ErrCodeNotFound, "processing activity version not found")
		case errors.Is(err, domain.ErrInvalidTransition):
			writeCoded(w, http.StatusConflict, domain.ErrCodeInvalidTransition, "version is not in a state that permits this operation")
		default:
			h.storeUnavailable(w, s.op, err)
		}
		return
	}
	h.finish(w, replay, hash, http.StatusOK, payload)
}

// requireState returns a check that insists on one source state.
func requireState(need domain.ActivityVersionStatus) func(context.Context, http.ResponseWriter, *callContext, *domain.ProcessingActivityVersion, []byte) (string, bool) {
	return func(_ context.Context, w http.ResponseWriter, _ *callContext, v *domain.ProcessingActivityVersion, _ []byte) (string, bool) {
		if v.VersionStatus != need {
			conflictState(w, need, v.VersionStatus)
			return "", false
		}
		return "", true
	}
}

// plainTransition builds the spec for a transition with no extra rules.
func (h *Handler) plainTransition(authz, op string, from, to domain.ActivityVersionStatus) lifecycleSpec {
	return lifecycleSpec{
		authz: authz, op: op, check: requireState(from),
		run: func(ctx context.Context, cc *callContext, v *domain.ProcessingActivityVersion, _ string) (*domain.ProcessingActivityVersion, error) {
			return h.store.TransitionActivity(ctx, v.ActivityID, v.ActivityVersionID, from, to, cc.Principal, cc.Correlation)
		},
	}
}

// makerChecker refuses the version's own creator (§18 segregation of duties).
func makerChecker(w http.ResponseWriter, cc *callContext, v *domain.ProcessingActivityVersion, verb string) bool {
	if v.CreatedByPrincipalID != "" && v.CreatedByPrincipalID == cc.Principal {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeSoDDenied,
			"maker cannot "+verb+" their own processing activity version (segregation of duties)")
		return false
	}
	return true
}

// ── validate ─────────────────────────────────────────────────────────────────

func (h *Handler) validateSpec() lifecycleSpec {
	return lifecycleSpec{
		authz: PrivacyActivityValidate, op: "ValidateActivity", check: requireState(domain.ActivityStatusDraft),
		run: func(ctx context.Context, cc *callContext, v *domain.ProcessingActivityVersion, _ string) (*domain.ProcessingActivityVersion, error) {
			findings, err := h.runStructuralValidation(ctx, v)
			if err != nil {
				return nil, err
			}
			return h.store.SetValidationOutcome(ctx, v.ActivityID, v.ActivityVersionID, findings, cc.Principal, cc.Correlation)
		},
	}
}

func (h *Handler) ValidateActivityVersion(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), chi.URLParam(r, "versionID"), h.validateSpec())
}
func (h *Handler) ValidateLatestActivity(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), "", h.validateSpec())
}

// ── submit / suspend / resume ────────────────────────────────────────────────
//
// SUBMIT is a real, authorized, audited transition to SUBMITTED. §9.1 says
// submit sends the exact version to the governed WFC approval workflow; WFC
// orchestration is not integrated (dependency-blocked — see README), so
// SUBMITTED is never treated as APPROVED: approval is only ever reached
// through a separate, separately-authorized, maker-checker-gated Approve call.

func (h *Handler) SubmitActivityVersion(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), chi.URLParam(r, "versionID"),
		h.plainTransition(PrivacyActivitySubmit, "SubmitActivity", domain.ActivityStatusValidated, domain.ActivityStatusSubmitted))
}
func (h *Handler) SubmitLatestActivity(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), "",
		h.plainTransition(PrivacyActivitySubmit, "SubmitActivity", domain.ActivityStatusValidated, domain.ActivityStatusSubmitted))
}
func (h *Handler) SuspendActivityVersion(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), chi.URLParam(r, "versionID"),
		h.plainTransition(PrivacyActivitySuspend, "SuspendActivity", domain.ActivityStatusActive, domain.ActivityStatusSuspended))
}
func (h *Handler) SuspendLatestActivity(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), "",
		h.plainTransition(PrivacyActivitySuspend, "SuspendActivity", domain.ActivityStatusActive, domain.ActivityStatusSuspended))
}
func (h *Handler) ResumeActivityVersion(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), chi.URLParam(r, "versionID"),
		h.plainTransition(PrivacyActivityResume, "ResumeActivity", domain.ActivityStatusSuspended, domain.ActivityStatusActive))
}
func (h *Handler) ResumeLatestActivity(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), "",
		h.plainTransition(PrivacyActivityResume, "ResumeActivity", domain.ActivityStatusSuspended, domain.ActivityStatusActive))
}

// ── approve / reject (maker-checker) ─────────────────────────────────────────

func (h *Handler) approveSpec() lifecycleSpec {
	s := h.plainTransition(PrivacyActivityApprove, "ApproveActivity", domain.ActivityStatusSubmitted, domain.ActivityStatusApproved)
	state := s.check
	s.check = func(ctx context.Context, w http.ResponseWriter, cc *callContext, v *domain.ProcessingActivityVersion, body []byte) (string, bool) {
		if !makerChecker(w, cc, v, "approve") {
			return "", false
		}
		return state(ctx, w, cc, v, body)
	}
	return s
}

func (h *Handler) ApproveActivityVersion(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), chi.URLParam(r, "versionID"), h.approveSpec())
}
func (h *Handler) ApproveLatestActivity(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), "", h.approveSpec())
}

func (h *Handler) rejectSpec() lifecycleSpec {
	return lifecycleSpec{
		authz: PrivacyActivityReject, op: "RejectActivity",
		check: func(ctx context.Context, w http.ResponseWriter, cc *callContext, v *domain.ProcessingActivityVersion, body []byte) (string, bool) {
			var req domain.RejectActivityRequest
			if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Reason) == "" {
				writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "a JSON body with a non-empty reason is required")
				return "", false
			}
			if !makerChecker(w, cc, v, "reject") {
				return "", false
			}
			if v.VersionStatus != domain.ActivityStatusSubmitted {
				conflictState(w, domain.ActivityStatusSubmitted, v.VersionStatus)
				return "", false
			}
			return strings.TrimSpace(req.Reason), true
		},
		run: func(ctx context.Context, cc *callContext, v *domain.ProcessingActivityVersion, reason string) (*domain.ProcessingActivityVersion, error) {
			return h.store.RejectActivity(ctx, v.ActivityID, v.ActivityVersionID, reason, cc.Principal, cc.Correlation)
		},
	}
}

func (h *Handler) RejectActivityVersion(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), chi.URLParam(r, "versionID"), h.rejectSpec())
}
func (h *Handler) RejectLatestActivity(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), "", h.rejectSpec())
}

// ── retire ───────────────────────────────────────────────────────────────────

func (h *Handler) retireSpec() lifecycleSpec {
	return lifecycleSpec{
		authz: PrivacyActivityRetire, op: "RetireActivity",
		check: func(_ context.Context, w http.ResponseWriter, _ *callContext, v *domain.ProcessingActivityVersion, _ []byte) (string, bool) {
			if !domain.ValidActivityTransition(v.VersionStatus, domain.ActivityStatusRetired) {
				writeCoded(w, http.StatusConflict, domain.ErrCodeInvalidTransition,
					fmt.Sprintf("version is %s; only an ACTIVE or SUSPENDED version can be retired", v.VersionStatus))
				return "", false
			}
			return "", true
		},
		run: func(ctx context.Context, cc *callContext, v *domain.ProcessingActivityVersion, _ string) (*domain.ProcessingActivityVersion, error) {
			return h.store.TransitionActivity(ctx, v.ActivityID, v.ActivityVersionID, v.VersionStatus, domain.ActivityStatusRetired, cc.Principal, cc.Correlation)
		},
	}
}

func (h *Handler) RetireActivityVersion(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), chi.URLParam(r, "versionID"), h.retireSpec())
}
func (h *Handler) RetireLatestActivity(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), "", h.retireSpec())
}

// ── activate ─────────────────────────────────────────────────────────────────

// activateSpec activates an APPROVED version.
//
// Effective time. §9.1: "Activate only approved version at governed effective
// time", and §2.1 leaves workflow timers to WFC. This service therefore
// activates at the SERVER'S instant and records that as effective_from — the
// caller cannot backdate (which would rewrite what governed the past) or
// post-date (which needs a scheduler this service does not have, and would
// leave the prior version retired while the new one is not yet in force). A
// caller-supplied effective_from is accepted only as an assertion of "now",
// within effectiveTimeTolerance of the server clock; anything else is 422.
//
// Gates. The full §8.2 gate set is re-run here against the live registries; a
// version that passed validation can still fail activation (a purpose's
// jurisdiction registry entry, say, no longer resolves). The refusal is 422
// carrying every finding and its §32 code.
func (h *Handler) activateSpec() lifecycleSpec {
	return lifecycleSpec{
		authz: PrivacyActivityActivate, op: "ActivateActivity",
		check: func(ctx context.Context, w http.ResponseWriter, cc *callContext, v *domain.ProcessingActivityVersion, body []byte) (string, bool) {
			if v.VersionStatus != domain.ActivityStatusApproved {
				conflictState(w, domain.ActivityStatusApproved, v.VersionStatus)
				return "", false
			}
			if len(bytes.TrimSpace(body)) > 0 {
				var req domain.ActivateActivityRequest
				if err := json.Unmarshal(body, &req); err != nil {
					writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "invalid request body")
					return "", false
				}
				if req.EffectiveFrom != nil {
					skew := h.now().Sub(*req.EffectiveFrom)
					if skew < 0 {
						skew = -skew
					}
					if skew > effectiveTimeTolerance {
						writeCoded(w, http.StatusUnprocessableEntity, domain.ErrCodeEffectiveTime, domain.ErrEffectiveTimeInvalid.Error())
						return "", false
					}
				}
			}
			findings, err := h.runStructuralValidation(ctx, v)
			if err != nil {
				h.storeUnavailable(w, "ActivateActivity", err)
				return "", false
			}
			if len(findings) > 0 {
				writeJSON(w, http.StatusUnprocessableEntity, errorResponse{
					Error:     "activation blocked: the version does not satisfy the §8.2 minimum activation gates",
					ErrorCode: findings[0].Code,
					Findings:  findings,
				})
				return "", false
			}
			return "", true
		},
		run: func(ctx context.Context, cc *callContext, v *domain.ProcessingActivityVersion, _ string) (*domain.ProcessingActivityVersion, error) {
			return h.store.ActivateActivity(ctx, v.ActivityID, v.ActivityVersionID, cc.Principal, cc.Correlation)
		},
	}
}

func (h *Handler) ActivateActivityVersion(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), chi.URLParam(r, "versionID"), h.activateSpec())
}
func (h *Handler) ActivateLatestActivity(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, chi.URLParam(r, "activityID"), "", h.activateSpec())
}
