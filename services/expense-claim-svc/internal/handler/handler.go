// Package handler exposes expense-claim-svc's REST API — AP-07.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/expense-claim-svc/internal/authz"
	"zoiko.io/expense-claim-svc/internal/documentvault"
	"zoiko.io/expense-claim-svc/internal/domain"
	"zoiko.io/expense-claim-svc/internal/employeemaster"
	"zoiko.io/expense-claim-svc/internal/events"
	svcmiddleware "zoiko.io/expense-claim-svc/internal/middleware"
	"zoiko.io/expense-claim-svc/internal/policy"
	"zoiko.io/expense-claim-svc/internal/store"
	"zoiko.io/expense-claim-svc/internal/tax"
)

// Action constants — AP-07's own contract's "Authorization / permissions"
// line ("expense.read/create; expense.approve; expense.exception.approve"),
// adapted to this platform's SCREAMING_SNAKE_CASE convention (see
// master-register-findings-2026-08-27.md §2.5). Reject/Return reuse the
// Approve action (all three are the decision on a pending claim); Cancel
// and AddExpenseLine reuse Create.
const (
	ExpenseRead             = "EXPENSE_READ"
	ExpenseCreate           = "EXPENSE_CREATE"
	ExpenseApprove          = "EXPENSE_APPROVE"
	ExpenseExceptionApprove = "EXPENSE_EXCEPTION_APPROVE"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
}

// Config is the subset of internal/config the handler needs.
type Config struct {
	ReceiptRequiredThreshold float64
}

type Handler struct {
	store    store.Store
	pub      events.Publisher
	authz    AuthzChecker
	employee employeemaster.Client
	docs     documentvault.Client
	tax      tax.Client
	policy   policy.Client
	cfg      Config
	log      *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, emp employeemaster.Client, docs documentvault.Client, taxClient tax.Client, pol policy.Client, cfg Config, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, employee: emp, docs: docs, tax: taxClient, policy: pol, cfg: cfg, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/ap07/expense-claims", func(r chi.Router) {
		r.Post("/", h.CreateExpenseClaim)
		r.Get("/{claimID}", h.GetExpenseClaim)
		r.Post("/{claimID}/lines", h.AddExpenseLine)
		r.Post("/{claimID}/submit", h.SubmitExpenseClaim)
		r.Post("/{claimID}/approve", h.ApproveExpenseClaim)
		r.Post("/{claimID}/reject", h.RejectExpenseClaim)
		r.Post("/{claimID}/return", h.ReturnForCorrection)
		r.Post("/{claimID}/cancel", h.CancelExpenseClaim)
		r.Post("/{claimID}/policy-exception", h.RecordExpensePolicyException)
		r.Get("/{claimID}/available-actions", h.GetAvailableActions)
		r.Get("/{claimID}/history", h.GetClaimHistory)
		r.Get("/{claimID}/policy-assessment", h.GetPolicyAssessment)
	})
	r.Get("/ap07/receipts/{documentID}/duplicate-assessment", h.GetDuplicateReceiptAssessment)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		return h.handleAuthzErr(w, err)
	}
	return true
}

func (h *Handler) handleAuthzErr(w http.ResponseWriter, err error) bool {
	if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "not authorized to perform this action")
		return false
	}
	h.log.Error("authorization check failed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
	return false
}

func (h *Handler) fetchClaimForAuth(w http.ResponseWriter, r *http.Request, claimID string) (*domain.ExpenseClaim, bool) {
	c, err := h.store.FindClaim(r.Context(), claimID)
	if err != nil {
		if errors.Is(err, domain.ErrClaimNotFound) {
			writeError(w, http.StatusNotFound, "expense claim not found")
			return nil, false
		}
		h.log.Error("fetchClaimForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return c, true
}

// decide performs the SoD-checked authorization common to every claim
// decision (Approve/Reject/Return/RecordExpensePolicyException): the
// claimant can never decide their own claim — enforced by
// authorization-svc's dynamic own-object layer, the same feature and
// calling pattern first exercised by supplier-financial-profile-svc (AP-01)
// and reused by goods-service-receipt-svc (AP-04). This is the direct
// enforcement of negative-path scenario #1.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request, principalID string, claim *domain.ExpenseClaim, actionType string) bool {
	if err := h.authz.CheckAllowedOwnObject(r.Context(), principalID, claim.LegalEntityID, actionType, claim.ClaimantPrincipalID); err != nil {
		h.handleAuthzErr(w, err)
		return false
	}
	return true
}

// ── claims ───────────────────────────────────────────────────────────────────

func (h *Handler) CreateExpenseClaim(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateExpenseClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.ClaimantPrincipalID == "" || req.Currency == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, claimant_principal_id and currency are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, ExpenseCreate) {
		return
	}

	if err := h.employee.VerifyActiveClaimant(r.Context(), principalID, req.LegalEntityID, req.ClaimantPrincipalID); err != nil {
		h.writeClaimantErr(w, err)
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	claim, err := h.store.CreateClaim(r.Context(), verifiedTenant, req, principalID)
	if err != nil {
		h.log.Error("CreateExpenseClaim: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventClaimCreated, EntityID: claim.ClaimID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: claim,
	})
	writeJSON(w, http.StatusCreated, claim)
}

func (h *Handler) writeClaimantErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrClaimantNotEligible):
		writeError(w, http.StatusBadRequest, "claimant does not exist, does not belong to this legal entity, or is not an active employee")
	default:
		h.log.Error("employee-master-svc lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "employee-master-svc unavailable")
	}
}

func (h *Handler) GetExpenseClaim(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	claim, ok := h.fetchClaimForAuth(w, r, claimID)
	if !ok {
		return
	}
	lines, err := h.store.ListLines(r.Context(), claimID)
	if err != nil {
		h.log.Error("GetExpenseClaim: failed to list lines", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if lines == nil {
		lines = []domain.ExpenseLine{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"claim": claim, "lines": lines})
}

// AddExpenseLine verifies a supplied receipt document against the real
// document-vault-svc before it can be attached — see internal/domain's
// package doc. Cross-claim reuse of the same receipt (negative-path
// scenario #2) is caught by the store's own database constraint.
func (h *Handler) AddExpenseLine(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	var req domain.AddExpenseLineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Merchant == "" || req.Amount <= 0 || req.Currency == "" || req.ExpenseDate.IsZero() {
		writeError(w, http.StatusBadRequest, "merchant, positive amount, currency and expense_date are required")
		return
	}
	if req.ClaimTaxRecovery && (req.Jurisdiction == "" || req.TaxCategory == "") {
		writeError(w, http.StatusBadRequest, "jurisdiction and tax_category are required when claim_tax_recovery is true")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	claim, ok := h.fetchClaimForAuth(w, r, claimID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, claim.LegalEntityID, ExpenseCreate) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if req.ReceiptDocumentID != "" {
		if err := h.docs.VerifyReceipt(r.Context(), principalID, verifiedTenant, claim.LegalEntityID, req.ReceiptDocumentID); err != nil {
			h.writeDocumentErr(w, err)
			return
		}
	}

	line, err := h.store.AddExpenseLine(r.Context(), claimID, req, principalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrDuplicateReceipt):
			writeError(w, http.StatusConflict, "receipt document is already attached to another expense line")
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "claim is not in a state that accepts new expense lines")
		case errors.Is(err, domain.ErrClaimNotFound):
			writeError(w, http.StatusNotFound, "expense claim not found")
		default:
			h.log.Error("AddExpenseLine: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}
	writeJSON(w, http.StatusCreated, line)
}

func (h *Handler) writeDocumentErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrDocumentNotFound):
		writeError(w, http.StatusBadRequest, "receipt document not found")
	case errors.Is(err, domain.ErrDocumentMismatch):
		writeError(w, http.StatusForbidden, "receipt document does not belong to the caller's tenant/legal entity")
	case errors.Is(err, domain.ErrDocumentNotUsable):
		writeError(w, http.StatusConflict, "receipt document is not in a usable state")
	default:
		h.log.Error("document-vault-svc lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "document-vault-svc unavailable")
	}
}

// SubmitExpenseClaim is where two of AP-07's negative-path scenarios are
// enforced for real. #4: every line declaring ClaimTaxRecovery gets a live
// tax-determination-svc call — TaxableAmount/CalculatedTaxAmount only ever
// come from that call's own response, and a failed call blocks the whole
// submission rather than leaving an invented figure. It also calls
// policy-svc's real APPROVAL_THRESHOLD evaluation against the claim total,
// storing the result for ApproveExpenseClaim to gate on.
func (h *Handler) SubmitExpenseClaim(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	claim, ok := h.fetchClaimForAuth(w, r, claimID)
	if !ok {
		return
	}
	if !domain.CanSubmit(claim.Status) {
		writeError(w, http.StatusConflict, "claim is not in a submittable state")
		return
	}
	if !h.authorize(w, r, principalID, claim.LegalEntityID, ExpenseCreate) {
		return
	}

	lines, err := h.store.ListLines(r.Context(), claimID)
	if err != nil {
		h.log.Error("SubmitExpenseClaim: failed to list lines", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if len(lines) == 0 {
		writeError(w, http.StatusBadRequest, "at least one expense line is required to submit a claim")
		return
	}

	var total float64
	for _, l := range lines {
		total += l.Amount
		if !l.ClaimTaxRecovery || l.TaxDeterminationID != "" {
			continue
		}
		result, err := h.tax.Determine(r.Context(), principalID, tax.DetermineRequest{
			TransactionID: l.LineID, LegalEntityID: claim.LegalEntityID, JurisdictionID: l.Jurisdiction,
			TaxCategory: l.TaxCategory, GrossAmount: l.Amount, Currency: l.Currency,
			EffectiveFrom: l.ExpenseDate.Format("2006-01-02"),
		})
		if err != nil {
			h.log.Warn("SubmitExpenseClaim: tax determination failed — blocking submission", zap.Error(err))
			writeError(w, http.StatusUnprocessableEntity, "tax determination failed for a line claiming tax recovery; submission blocked")
			return
		}
		if err := h.store.SetLineTaxDetermination(r.Context(), l.LineID, result.DeterminationID, result.TaxableAmount, result.CalculatedTaxAmount); err != nil {
			h.log.Error("SubmitExpenseClaim: failed to record tax determination", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
			return
		}
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	policyResult, policyVersionID, err := h.policy.EvaluateApprovalThreshold(r.Context(), principalID, verifiedTenant, claim.LegalEntityID, total)
	if err != nil {
		h.log.Error("SubmitExpenseClaim: policy evaluation failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "policy-svc unavailable")
		return
	}

	updated, err := h.store.SubmitClaim(r.Context(), claimID, domain.PolicyAssessmentResult(policyResult), policyVersionID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "claim is not in a submittable state")
			return
		}
		h.log.Error("SubmitExpenseClaim: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventClaimSubmitted, EntityID: updated.ClaimID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

// ApproveExpenseClaim is AP-07's SoD-and-policy checkpoint: the approving
// principal is checked against authorization-svc's own-object SoD layer
// (never the claimant themselves — negative-path #1); a claim policy-svc
// flagged APPROVAL_REQUIRED needs the stronger ExpenseExceptionApprove
// authority to approve, not the ordinary ExpenseApprove; and negative-path
// #3 (a line over the receipt-required threshold with no attached receipt)
// blocks approval outright unless RecordExpensePolicyException already
// granted a delegated waiver for this claim.
func (h *Handler) ApproveExpenseClaim(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	claim, ok := h.fetchClaimForAuth(w, r, claimID)
	if !ok {
		return
	}
	if !domain.CanDecide(claim.Status) {
		writeError(w, http.StatusConflict, "claim is not pending approval")
		return
	}

	requiredAction := ExpenseApprove
	if claim.PolicyAssessmentResult == domain.PolicyApprovalRequired {
		requiredAction = ExpenseExceptionApprove
	}
	if !h.decide(w, r, principalID, claim, requiredAction) {
		return
	}

	if !claim.HasPolicyException {
		lines, err := h.store.ListLines(r.Context(), claimID)
		if err != nil {
			h.log.Error("ApproveExpenseClaim: failed to list lines", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
			return
		}
		for _, l := range lines {
			if l.Amount > h.cfg.ReceiptRequiredThreshold && l.ReceiptDocumentID == "" {
				writeError(w, http.StatusConflict, "one or more expense lines exceed the receipt-required threshold without an attached receipt")
				return
			}
		}
	}

	updated, err := h.store.ApproveClaim(r.Context(), claimID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "claim is not pending approval")
			return
		}
		h.log.Error("ApproveExpenseClaim: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventClaimApproved, EntityID: updated.ClaimID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	// AP-08 has no real reimbursement-consuming endpoint for a claimant
	// expense (see internal/domain's package doc) — this event is emitted
	// honestly with no consumer, not routed through accounts-payable-svc's
	// unrelated vendor-invoice model.
	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventClaimPayableRequested, EntityID: updated.ClaimID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) RejectExpenseClaim(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	var req domain.RejectClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	claim, ok := h.fetchClaimForAuth(w, r, claimID)
	if !ok {
		return
	}
	if !domain.CanDecide(claim.Status) {
		writeError(w, http.StatusConflict, "claim is not pending approval")
		return
	}
	if !h.decide(w, r, principalID, claim, ExpenseApprove) {
		return
	}

	updated, err := h.store.RejectClaim(r.Context(), claimID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "claim is not pending approval")
			return
		}
		h.log.Error("RejectExpenseClaim: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) ReturnForCorrection(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	var req domain.ReturnClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	claim, ok := h.fetchClaimForAuth(w, r, claimID)
	if !ok {
		return
	}
	if !domain.CanDecide(claim.Status) {
		writeError(w, http.StatusConflict, "claim is not pending approval")
		return
	}
	if !h.decide(w, r, principalID, claim, ExpenseApprove) {
		return
	}

	updated, err := h.store.ReturnClaim(r.Context(), claimID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "claim is not pending approval")
			return
		}
		h.log.Error("ReturnForCorrection: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) CancelExpenseClaim(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	var req domain.CancelClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	claim, ok := h.fetchClaimForAuth(w, r, claimID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, claim.LegalEntityID, ExpenseCreate) {
		return
	}

	updated, err := h.store.CancelClaim(r.Context(), claimID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "claim is not in a cancellable state")
			return
		}
		h.log.Error("CancelExpenseClaim: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// RecordExpensePolicyException waives the receipt-evidence requirement
// (negative-path #3) for this claim — the delegated exception authority the
// spec's SoD line refers to ("approver cannot override policy/receipt
// requirement outside delegated exception authority"). Requires the
// stronger ExpenseExceptionApprove action and the same SoD check as any
// other decision.
func (h *Handler) RecordExpensePolicyException(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	var req domain.RecordPolicyExceptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	claim, ok := h.fetchClaimForAuth(w, r, claimID)
	if !ok {
		return
	}
	if !domain.CanDecide(claim.Status) {
		writeError(w, http.StatusConflict, "claim is not pending approval")
		return
	}
	if !h.decide(w, r, principalID, claim, ExpenseExceptionApprove) {
		return
	}

	updated, err := h.store.RecordPolicyException(r.Context(), claimID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "claim is not pending approval")
			return
		}
		h.log.Error("RecordExpensePolicyException: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── queries ──────────────────────────────────────────────────────────────────

func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	claim, ok := h.fetchClaimForAuth(w, r, claimID)
	if !ok {
		return
	}
	var actions []string
	if domain.CanAddLine(claim.Status) {
		actions = append(actions, "AddExpenseLine")
	}
	if domain.CanSubmit(claim.Status) {
		actions = append(actions, "SubmitExpenseClaim")
	}
	if domain.CanDecide(claim.Status) {
		actions = append(actions, "ApproveExpenseClaim", "RejectExpenseClaim", "ReturnForCorrection", "RecordExpensePolicyException")
	}
	if domain.CanCancel(claim.Status) {
		actions = append(actions, "CancelExpenseClaim")
	}
	if actions == nil {
		actions = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"claim_id": claimID, "status": claim.Status, "available_actions": actions})
}

func (h *Handler) GetClaimHistory(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	if _, ok := h.fetchClaimForAuth(w, r, claimID); !ok {
		return
	}
	events, err := h.store.ListClaimEvents(r.Context(), claimID)
	if err != nil {
		h.log.Error("GetClaimHistory: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if events == nil {
		events = []domain.ExpenseClaimEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": events, "count": len(events)})
}

func (h *Handler) GetPolicyAssessment(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	claim, ok := h.fetchClaimForAuth(w, r, claimID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"claim_id": claimID, "result": claim.PolicyAssessmentResult, "policy_version_id": claim.PolicyVersionID,
	})
}

// GetDuplicateReceiptAssessment reports whether documentID is already
// attached to any expense line — the queryable half of negative-path
// scenario #2, backed by the same store the database constraint itself
// reads from.
func (h *Handler) GetDuplicateReceiptAssessment(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "documentID")
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	inUse, claimID, lineID, err := h.store.IsReceiptInUse(r.Context(), documentID)
	if err != nil {
		h.log.Error("GetDuplicateReceiptAssessment: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"document_id": documentID, "in_use": inUse, "claim_id": claimID, "line_id": lineID,
	})
}
