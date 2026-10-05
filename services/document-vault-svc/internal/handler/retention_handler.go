package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/document-vault-svc/internal/domain"
)

const (
	actionRetentionRuleManage  = "RETENTION_RULE_MANAGE"
	actionRetentionRuleApprove = "RETENTION_RULE_APPROVE"
	actionRetentionStateManage = "RETENTION_STATE_MANAGE"
	actionRetentionRead        = "RETENTION_READ"
	actionLegalHoldManage      = "LEGAL_HOLD_MANAGE"
	actionLegalHoldRead        = "LEGAL_HOLD_READ"
)

// RetentionStore is DRC-03's own persistence contract — dry-run
// retention evaluation and legal hold, kept separate from the
// existing Store/RecordsStore interfaces.
type RetentionStore interface {
	CreateRetentionRuleVersion(ctx context.Context, p domain.CreateRetentionRuleVersionParams) (*domain.RetentionRuleVersion, error)
	GetRetentionRuleVersion(ctx context.Context, retentionRuleVersionID string) (*domain.RetentionRuleVersion, error)
	ApproveRetentionRuleVersion(ctx context.Context, p domain.ApproveRetentionRuleVersionParams) (*domain.RetentionRuleVersion, error)
	ActivateRetentionRuleVersion(ctx context.Context, retentionRuleVersionID string) (*domain.RetentionRuleVersion, error)

	BindRetentionRule(ctx context.Context, p domain.BindRetentionRuleParams) (*domain.RecordRetentionState, error)
	GetRecordRetentionState(ctx context.Context, retentionStateID string) (*domain.RecordRetentionState, error)
	GetRetentionStateByRecord(ctx context.Context, recordID string) (*domain.RecordRetentionState, error)
	RecordTriggerEvent(ctx context.Context, p domain.RecordTriggerEventParams) (*domain.RecordRetentionState, error)
	EvaluateDue(ctx context.Context, p domain.EvaluateDueParams) (*domain.RecordRetentionState, error)
	FlagForReview(ctx context.Context, p domain.FlagForReviewParams) (*domain.RecordRetentionState, error)
	ApproveForDisposition(ctx context.Context, p domain.ApproveForDispositionParams) (*domain.RecordRetentionState, error)

	CreateLegalHold(ctx context.Context, p domain.CreateLegalHoldParams) (*domain.LegalHold, error)
	GetLegalHold(ctx context.Context, holdID string) (*domain.LegalHold, error)
	ActivateLegalHold(ctx context.Context, p domain.ActivateLegalHoldParams) (*domain.LegalHold, error)
	AddLegalHoldTarget(ctx context.Context, p domain.AddLegalHoldTargetParams) (*domain.LegalHoldTarget, error)
	ReleaseLegalHold(ctx context.Context, p domain.ReleaseLegalHoldParams) (*domain.LegalHold, error)
	ListLegalHoldTargets(ctx context.Context, holdID string) ([]domain.LegalHoldTarget, error)
}

func RegisterRetentionRoutes(r chi.Router, h *Handler, retentionStore RetentionStore) {
	rh := &retentionHandler{Handler: h, store: retentionStore}
	r.Route("/v1/retention-rules", func(r chi.Router) {
		r.Post("/", rh.CreateRetentionRuleVersion)
		r.Get("/{retention_rule_version_id}", rh.GetRetentionRuleVersion)
		r.Post("/{retention_rule_version_id}/approve", rh.ApproveRetentionRuleVersion)
		r.Post("/{retention_rule_version_id}/activate", rh.ActivateRetentionRuleVersion)
	})
	r.Route("/v1/retention-states", func(r chi.Router) {
		r.Post("/", rh.BindRetentionRule)
		r.Get("/{retention_state_id}", rh.GetRecordRetentionState)
		r.Get("/by-record/{record_id}", rh.GetRetentionStateByRecord)
		r.Post("/{retention_state_id}/trigger", rh.RecordTriggerEvent)
		r.Post("/{retention_state_id}/evaluate-due", rh.EvaluateDue)
		r.Post("/{retention_state_id}/flag-for-review", rh.FlagForReview)
		r.Post("/{retention_state_id}/approve-for-disposition", rh.ApproveForDisposition)
	})
	r.Route("/v1/legal-holds", func(r chi.Router) {
		r.Post("/", rh.CreateLegalHold)
		r.Get("/{hold_id}", rh.GetLegalHold)
		r.Post("/{hold_id}/activate", rh.ActivateLegalHold)
		r.Post("/{hold_id}/targets", rh.AddLegalHoldTarget)
		r.Get("/{hold_id}/targets", rh.ListLegalHoldTargets)
		r.Post("/{hold_id}/release", rh.ReleaseLegalHold)
	})
}

type retentionHandler struct {
	*Handler
	store RetentionStore
}

// ── Retention Rule Versions ──────────────────────────────────────────────────

type createRetentionRuleRequest struct {
	RecordClass          string `json:"record_class"`
	JurisdictionSelector string `json:"jurisdiction_selector"`
	LegalBasisRef        string `json:"legal_basis_ref"`
	PurposeRef           string `json:"purpose_ref"`
	TriggerType          string `json:"trigger_type"`
	DurationDays         int    `json:"duration_days"`
	DispositionAction    string `json:"disposition_action"`
}

func (h *retentionHandler) CreateRetentionRuleVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req createRetentionRuleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionRuleManage) {
		return
	}
	rr, err := h.store.CreateRetentionRuleVersion(r.Context(), domain.CreateRetentionRuleVersionParams{
		RecordClass: req.RecordClass, JurisdictionSelector: req.JurisdictionSelector, LegalBasisRef: req.LegalBasisRef,
		PurposeRef: req.PurposeRef, TriggerType: req.TriggerType, DurationDays: req.DurationDays,
		DispositionAction: req.DispositionAction, CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rr)
}

func (h *retentionHandler) GetRetentionRuleVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionRead) {
		return
	}
	rr, err := h.store.GetRetentionRuleVersion(r.Context(), chi.URLParam(r, "retention_rule_version_id"))
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

// ApproveRetentionRuleVersion requires a different authorization action
// than create — approval is the maker-checker gate, not ordinary
// authoring.
func (h *retentionHandler) ApproveRetentionRuleVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionRuleApprove) {
		return
	}
	rr, err := h.store.ApproveRetentionRuleVersion(r.Context(), domain.ApproveRetentionRuleVersionParams{
		RetentionRuleVersionID: chi.URLParam(r, "retention_rule_version_id"), ApprovedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

func (h *retentionHandler) ActivateRetentionRuleVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionRuleManage) {
		return
	}
	rr, err := h.store.ActivateRetentionRuleVersion(r.Context(), chi.URLParam(r, "retention_rule_version_id"))
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

// ── Record Retention States ──────────────────────────────────────────────────

type bindRetentionRuleRequest struct {
	RecordID               string     `json:"record_id"`
	RetentionRuleVersionID string     `json:"retention_rule_version_id"`
	TriggerDate            *time.Time `json:"trigger_date,omitempty"`
}

func (h *retentionHandler) BindRetentionRule(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req bindRetentionRuleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RecordID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "record_id")
		return
	}
	if req.RetentionRuleVersionID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "retention_rule_version_id")
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionStateManage) {
		return
	}
	st, err := h.store.BindRetentionRule(r.Context(), domain.BindRetentionRuleParams{
		RecordID: req.RecordID, RetentionRuleVersionID: req.RetentionRuleVersionID,
		TriggerDate: req.TriggerDate, CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, st)
}

func (h *retentionHandler) GetRecordRetentionState(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionRead) {
		return
	}
	st, err := h.store.GetRecordRetentionState(r.Context(), chi.URLParam(r, "retention_state_id"))
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *retentionHandler) GetRetentionStateByRecord(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionRead) {
		return
	}
	st, err := h.store.GetRetentionStateByRecord(r.Context(), chi.URLParam(r, "record_id"))
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type recordTriggerEventRequest struct {
	TriggerDate time.Time `json:"trigger_date"`
}

func (h *retentionHandler) RecordTriggerEvent(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req recordTriggerEventRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TriggerDate.IsZero() {
		writeError(w, http.StatusBadRequest, "missing_field", "trigger_date")
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionStateManage) {
		return
	}
	st, err := h.store.RecordTriggerEvent(r.Context(), domain.RecordTriggerEventParams{
		RetentionStateID: chi.URLParam(r, "retention_state_id"), TriggerDate: req.TriggerDate,
	})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type evaluateDueRequest struct {
	AsOf time.Time `json:"as_of"`
}

func (h *retentionHandler) EvaluateDue(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req evaluateDueRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.AsOf.IsZero() {
		writeError(w, http.StatusBadRequest, "missing_field", "as_of")
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionStateManage) {
		return
	}
	st, err := h.store.EvaluateDue(r.Context(), domain.EvaluateDueParams{
		RetentionStateID: chi.URLParam(r, "retention_state_id"), AsOf: req.AsOf,
	})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type flagForReviewRequest struct {
	ReviewNotes string `json:"review_notes"`
}

func (h *retentionHandler) FlagForReview(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req flagForReviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionStateManage) {
		return
	}
	st, err := h.store.FlagForReview(r.Context(), domain.FlagForReviewParams{
		RetentionStateID: chi.URLParam(r, "retention_state_id"), ReviewedByPrincipalID: principalID, ReviewNotes: req.ReviewNotes,
	})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ApproveForDisposition is DRC-03's most consequential command — it
// is refused outright if the record is under an active legal hold.
func (h *retentionHandler) ApproveForDisposition(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionRetentionStateManage) {
		return
	}
	st, err := h.store.ApproveForDisposition(r.Context(), domain.ApproveForDispositionParams{
		RetentionStateID: chi.URLParam(r, "retention_state_id"), ApprovedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ── Legal Holds ──────────────────────────────────────────────────────────────

type createLegalHoldRequest struct {
	MatterRef      string   `json:"matter_ref"`
	AuthorityRef   string   `json:"authority_ref"`
	HoldReasonCode string   `json:"hold_reason_code"`
	RecordIDs      []string `json:"record_ids"`
}

func (h *retentionHandler) CreateLegalHold(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req createLegalHoldRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.MatterRef == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "matter_ref")
		return
	}
	if req.HoldReasonCode == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "hold_reason_code")
		return
	}
	if !h.authorize(w, r, principalID, "", actionLegalHoldManage) {
		return
	}
	hold, err := h.store.CreateLegalHold(r.Context(), domain.CreateLegalHoldParams{
		MatterRef: req.MatterRef, AuthorityRef: req.AuthorityRef, HoldReasonCode: req.HoldReasonCode,
		RecordIDs: req.RecordIDs, IssuedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, hold)
}

func (h *retentionHandler) GetLegalHold(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionLegalHoldRead) {
		return
	}
	hold, err := h.store.GetLegalHold(r.Context(), chi.URLParam(r, "hold_id"))
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hold)
}

func (h *retentionHandler) ActivateLegalHold(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionLegalHoldManage) {
		return
	}
	hold, err := h.store.ActivateLegalHold(r.Context(), domain.ActivateLegalHoldParams{
		HoldID: chi.URLParam(r, "hold_id"), ActivatedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hold)
}

type addLegalHoldTargetRequest struct {
	RecordID string `json:"record_id"`
}

func (h *retentionHandler) AddLegalHoldTarget(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req addLegalHoldTargetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RecordID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "record_id")
		return
	}
	if !h.authorize(w, r, principalID, "", actionLegalHoldManage) {
		return
	}
	target, err := h.store.AddLegalHoldTarget(r.Context(), domain.AddLegalHoldTargetParams{
		HoldID: chi.URLParam(r, "hold_id"), RecordID: req.RecordID, CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, target)
}

func (h *retentionHandler) ListLegalHoldTargets(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionLegalHoldRead) {
		return
	}
	targets, err := h.store.ListLegalHoldTargets(r.Context(), chi.URLParam(r, "hold_id"))
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	if targets == nil {
		targets = []domain.LegalHoldTarget{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": targets, "count": len(targets)})
}

type releaseLegalHoldRequest struct {
	ReleaseReason string `json:"release_reason"`
}

func (h *retentionHandler) ReleaseLegalHold(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req releaseLegalHoldRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ReleaseReason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "release_reason")
		return
	}
	if !h.authorize(w, r, principalID, "", actionLegalHoldManage) {
		return
	}
	hold, err := h.store.ReleaseLegalHold(r.Context(), domain.ReleaseLegalHoldParams{
		HoldID: chi.URLParam(r, "hold_id"), ReleaseReason: req.ReleaseReason, ReleasedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleRetentionStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hold)
}

func (h *retentionHandler) handleRetentionStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrRetentionRuleNotFound), errors.Is(err, domain.ErrRetentionStateNotFound),
		errors.Is(err, domain.ErrLegalHoldNotFound), errors.Is(err, domain.ErrRecordNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidRecordClass), errors.Is(err, domain.ErrJurisdictionScopeRequired),
		errors.Is(err, domain.ErrInvalidTriggerType), errors.Is(err, domain.ErrInvalidDispositionAction),
		errors.Is(err, domain.ErrNoRecordIDsForHold):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, domain.ErrRetentionRuleNotDraft), errors.Is(err, domain.ErrRetentionRuleNotApproved),
		errors.Is(err, domain.ErrRetentionRuleSelfApproval), errors.Is(err, domain.ErrRetentionRuleNotActive),
		errors.Is(err, domain.ErrRetentionRuleMismatch), errors.Is(err, domain.ErrRecordAlreadyBound),
		errors.Is(err, domain.ErrRetentionStateNotWaiting), errors.Is(err, domain.ErrRetentionStateNotActive),
		errors.Is(err, domain.ErrRetentionStateNotDue), errors.Is(err, domain.ErrRetentionStateNotReviewRequired),
		errors.Is(err, domain.ErrNotYetDue), errors.Is(err, domain.ErrRecordUnderLegalHold),
		errors.Is(err, domain.ErrLegalHoldNotDraft), errors.Is(err, domain.ErrLegalHoldNotActive),
		errors.Is(err, domain.ErrLegalHoldTargetExists):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		h.handleStoreError(w, err)
	}
}
