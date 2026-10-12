// Package handler exposes expense-claim-svc's REST API — AP-07.
//
// Command contract (spec section 16):
//   - Idempotency-Key is REQUIRED on ApproveExpenseClaim (400
//     IDEMPOTENCY_KEY_REQUIRED) and honoured on every other command. A
//     repeat with the same key and body replays the stored status and body
//     with Idempotent-Replay: true; the same key with a different request is
//     422 IDEMPOTENCY_KEY_REUSED.
//   - expected_version is REQUIRED on approve, reject, return and
//     policy-exception (400 VALIDATION_FAILED when absent) and honoured on
//     submit, cancel and close; a mismatch is 409 STALE_VERSION.
//   - Errors carry a stable machine-readable "code" next to the "error" text.
//   - Every route needs X-Tenant-Id and X-Principal-Id; reads are authorized
//     (EXPENSE_READ) and tenant-scoped.
package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/expense-claim-svc/internal/authz"
	"zoiko.io/expense-claim-svc/internal/configflag"
	"zoiko.io/expense-claim-svc/internal/documentvault"
	"zoiko.io/expense-claim-svc/internal/domain"
	"zoiko.io/expense-claim-svc/internal/employeemaster"
	svcmiddleware "zoiko.io/expense-claim-svc/internal/middleware"
	"zoiko.io/expense-claim-svc/internal/payableopenitem"
	"zoiko.io/expense-claim-svc/internal/policy"
	"zoiko.io/expense-claim-svc/internal/store"
	"zoiko.io/expense-claim-svc/internal/tax"
)

// Action constants — AP-07's own contract's "Authorization / permissions"
// line ("expense.read/create; expense.approve; expense.exception.approve"),
// adapted to this platform's SCREAMING_SNAKE_CASE convention (see
// master-register-findings-2026-08-27.md §2.5). Reject/Return reuse the
// Approve action (all three are the decision on a pending claim); Cancel,
// AddExpenseLine and VoidExpenseLine reuse Create; Close reuses Approve.
const (
	ExpenseRead             = "EXPENSE_READ"
	ExpenseCreate           = "EXPENSE_CREATE"
	ExpenseApprove          = "EXPENSE_APPROVE"
	ExpenseExceptionApprove = "EXPENSE_EXCEPTION_APPROVE"
)

// Stable error codes (spec section 16) plus a few AP-07-specific ones.
const (
	CodeValidation         = "VALIDATION_FAILED"
	CodeForbidden          = "FORBIDDEN"
	CodeSoDConflict        = "SOD_CONFLICT"
	CodeDuplicateRisk      = "DUPLICATE_RISK"
	CodeStaleVersion       = "STALE_VERSION"
	CodeTaxUnavailable     = "TAX_UNAVAILABLE"
	CodeKeyRequired        = "IDEMPOTENCY_KEY_REQUIRED"
	CodeKeyReused          = "IDEMPOTENCY_KEY_REUSED"
	CodeNotFound           = "NOT_FOUND"
	CodeInvalidTransition  = "INVALID_TRANSITION"
	CodeReceiptRequired    = "RECEIPT_REQUIRED"
	CodePolicyUnavailable  = "POLICY_UNAVAILABLE"
	CodePayableNotSettled  = "PAYABLE_NOT_SETTLED"
	CodeDependencyDown     = "DEPENDENCY_UNAVAILABLE"
	CodeStoreUnavailable   = "STORE_UNAVAILABLE"
	CodeDocumentNotUsable  = "DOCUMENT_NOT_USABLE"
	CodeClaimantNotEligble = "CLAIMANT_NOT_ELIGIBLE"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	CheckAllowedOwnObject(ctx context.Context, principalID, legalEntityID, actionType, resourceOwnerPrincipalID string) error
}

// PayableProcessor lets the approval path attempt the AP-08 hand-off
// immediately. It is best-effort: the durable payable_requests row and the
// scheduled relay are the guarantee.
type PayableProcessor interface {
	ProcessClaim(ctx context.Context, claimID string)
}

// Config is the subset of internal/config the handler needs.
type Config struct {
	ReceiptRequiredThreshold float64
	Environment              string
	// ReimbursementTermsDays is the default payment term after approval.
	ReimbursementTermsDays float64
	// PolicyControlledCategories: categories for which a missing or
	// unavailable approval-threshold policy blocks submission. "*" = all.
	PolicyControlledCategories []string
	// Posting carries the ACC-02 mapping keys and fiscal-period layout of the
	// GL posting request an approval writes.
	Posting domain.PostingConfig
}

type Deps struct {
	Store       store.Store
	Authz       AuthzChecker
	Employee    employeemaster.Client
	Docs        documentvault.Client
	Tax         tax.Client
	Policy      policy.Client
	Payable     payableopenitem.Client
	ConfigFlags configflag.Client
	Relay       PayableProcessor // optional
	Config      Config
	Log         *zap.Logger
}

type Handler struct {
	Deps
	now func() time.Time
}

func New(d Deps) *Handler {
	if d.Config.PolicyControlledCategories == nil {
		d.Config.PolicyControlledCategories = []string{"*"}
	}
	if d.Config.ReimbursementTermsDays <= 0 {
		d.Config.ReimbursementTermsDays = 14
	}
	return &Handler{Deps: d, now: time.Now}
}

// resolveReceiptThreshold asks configuration-feature-flag-svc for a
// tenant-specific (falling back to global) RECEIPT_REQUIRED_THRESHOLD
// override, falling back further to this instance's own static
// cfg.ReceiptRequiredThreshold when the registry has no override
// configured, or cannot be reached at all. This is a policy value, not a
// security gate — an unreachable registry keeps this claim approvable
// under the value this service already shipped with, rather than
// blocking approvals platform-wide over a config-lookup outage.
func (h *Handler) resolveReceiptThreshold(ctx context.Context, tenantID string) float64 {
	if h.ConfigFlags == nil || tenantID == "" {
		return h.Config.ReceiptRequiredThreshold
	}
	threshold, found, err := h.ConfigFlags.ResolveReceiptThreshold(ctx, h.Config.Environment, tenantID)
	if err != nil {
		h.Log.Warn("configuration-feature-flag-svc unavailable — using static default threshold", zap.Error(err))
		return h.Config.ReceiptRequiredThreshold
	}
	if !found {
		return h.Config.ReceiptRequiredThreshold
	}
	return threshold
}

// reimbursementDueDate derives the payable's due date from payment terms:
// the tenant's EXPENSE_REIMBURSEMENT_TERMS_DAYS entry, else the static
// default — never "now".
func (h *Handler) reimbursementDueDate(ctx context.Context, tenantID string) time.Time {
	days := h.Config.ReimbursementTermsDays
	if h.ConfigFlags != nil && tenantID != "" {
		if d, found, err := h.ConfigFlags.ResolveReimbursementTermsDays(ctx, h.Config.Environment, tenantID); err == nil && found && d > 0 {
			days = d
		}
	}
	return h.now().UTC().Add(time.Duration(days * float64(24*time.Hour))).Truncate(24 * time.Hour)
}

func (h *Handler) isControlled(category string) bool {
	for _, c := range h.Config.PolicyControlledCategories {
		if c == "*" || strings.EqualFold(c, category) {
			return true
		}
	}
	return false
}

func (h *Handler) anyControlled(lines []domain.ExpenseLine) bool {
	for _, l := range lines {
		if h.isControlled(l.Category) {
			return true
		}
	}
	return false
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Group(func(r chi.Router) {
		r.Use(svcmiddleware.RequireTenant)
		r.Route("/ap07/expense-claims", func(r chi.Router) {
			r.Post("/", h.CreateExpenseClaim)
			r.Get("/{claimID}", h.GetExpenseClaim)
			r.Post("/{claimID}/lines", h.AddExpenseLine)
			r.Post("/{claimID}/lines/{lineID}/void", h.VoidExpenseLine)
			r.Post("/{claimID}/submit", h.SubmitExpenseClaim)
			r.Post("/{claimID}/approve", h.ApproveExpenseClaim)
			r.Post("/{claimID}/reject", h.RejectExpenseClaim)
			r.Post("/{claimID}/return", h.ReturnForCorrection)
			r.Post("/{claimID}/cancel", h.CancelExpenseClaim)
			r.Post("/{claimID}/close", h.CloseExpenseClaim)
			r.Post("/{claimID}/policy-exception", h.RecordExpensePolicyException)
			r.Get("/{claimID}/available-actions", h.GetAvailableActions)
			r.Get("/{claimID}/history", h.GetClaimHistory)
			r.Get("/{claimID}/submissions", h.GetClaimSubmissions)
			r.Get("/{claimID}/policy-assessment", h.GetPolicyAssessment)
			r.Get("/{claimID}/accounting-status", h.GetAccountingStatus)
			r.Post("/{claimID}/accounting/requeue", h.RequeueAccounting)
		})
		r.Get("/ap07/receipts/{documentID}/duplicate-assessment", h.GetDuplicateReceiptAssessment)
	})
}

// ── plumbing ─────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeErr(w, http.StatusUnauthorized, CodeForbidden, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.Authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		return h.handleAuthzErr(w, err, CodeForbidden)
	}
	return true
}

func (h *Handler) handleAuthzErr(w http.ResponseWriter, err error, deniedCode string) bool {
	if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
		writeErr(w, http.StatusForbidden, deniedCode, "not authorized to perform this action")
		return false
	}
	h.Log.Error("authorization check failed", zap.Error(err))
	writeErr(w, http.StatusServiceUnavailable, CodeDependencyDown, "authorization service unavailable")
	return false
}

func (h *Handler) fetchClaimForAuth(w http.ResponseWriter, r *http.Request, claimID string) (*domain.ExpenseClaim, bool) {
	c, err := h.Store.FindClaim(r.Context(), claimID)
	if err != nil {
		if errors.Is(err, domain.ErrClaimNotFound) {
			writeErr(w, http.StatusNotFound, CodeNotFound, "expense claim not found")
			return nil, false
		}
		h.Log.Error("fetchClaimForAuth: store unavailable", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
		return nil, false
	}
	return c, true
}

// authorizeRead is the common prologue of every query: principal present,
// claim visible in the caller's tenant, EXPENSE_READ on its legal entity.
func (h *Handler) authorizeRead(w http.ResponseWriter, r *http.Request, claimID string) (*domain.ExpenseClaim, bool) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	claim, ok := h.fetchClaimForAuth(w, r, claimID)
	if !ok {
		return nil, false
	}
	if !h.authorize(w, r, principalID, claim.LegalEntityID, ExpenseRead) {
		return nil, false
	}
	return claim, true
}

// decide performs the SoD-checked authorization common to every claim
// decision: the claimant can never decide their own claim. The check is made
// locally (the claim's claimant is known) AND by authorization-svc's dynamic
// own-object layer, so self-approval is refused even if that layer were
// misconfigured — the direct enforcement of negative-path scenario #1.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request, principalID string, claim *domain.ExpenseClaim, actionType string) bool {
	if principalID == claim.ClaimantPrincipalID {
		writeErr(w, http.StatusForbidden, CodeSoDConflict, "a claimant cannot decide their own expense claim")
		return false
	}
	if err := h.Authz.CheckAllowedOwnObject(r.Context(), principalID, claim.LegalEntityID, actionType, claim.ClaimantPrincipalID); err != nil {
		h.handleAuthzErr(w, err, CodeSoDConflict)
		return false
	}
	return true
}

// readBody reads the (bounded) request body once, so it can be both hashed
// for idempotency and decoded.
func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}

func decodeBody(body []byte, v interface{}) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// idemCheck implements the Idempotency-Key contract. It returns the key to
// pass to the store (nil when none was supplied) and stop=true when it has
// already written the response (missing required key, reuse conflict or
// replay).
func (h *Handler) idemCheck(w http.ResponseWriter, r *http.Request, op string, body []byte, required bool) (idem *domain.IdemKey, stop bool) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		if required {
			writeErr(w, http.StatusBadRequest, CodeKeyRequired, "Idempotency-Key header is required for "+op)
			return nil, true
		}
		return nil, false
	}
	sum := sha256.Sum256([]byte(op + "|" + r.URL.Path + "|" + string(body)))
	idem = &domain.IdemKey{Key: key, Operation: op, RequestHash: hex.EncodeToString(sum[:])}
	rec, err := h.Store.GetIdempotency(r.Context(), key)
	if err != nil {
		h.Log.Error("idempotency lookup failed", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
		return nil, true
	}
	if rec == nil {
		return idem, false
	}
	if rec.Operation != op || rec.RequestHash != idem.RequestHash {
		writeErr(w, http.StatusUnprocessableEntity, CodeKeyReused, "Idempotency-Key was already used with a different request")
		return nil, true
	}
	replay(w, rec)
	return nil, true
}

func replay(w http.ResponseWriter, rec *domain.IdemRecord) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotent-Replay", "true")
	w.WriteHeader(rec.StatusCode)
	_, _ = w.Write(rec.Body)
}

// storeErr maps a store error to a response and reports whether it wrote one.
func (h *Handler) storeErr(w http.ResponseWriter, r *http.Request, op string, err error, idem *domain.IdemKey) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, domain.ErrClaimNotFound):
		writeErr(w, http.StatusNotFound, CodeNotFound, "expense claim not found")
	case errors.Is(err, domain.ErrLineNotFound):
		writeErr(w, http.StatusNotFound, CodeNotFound, "expense line not found")
	case errors.Is(err, domain.ErrStaleVersion):
		writeErr(w, http.StatusConflict, CodeStaleVersion, domain.ErrStaleVersion.Error())
	case errors.Is(err, domain.ErrInvalidTransition):
		writeErr(w, http.StatusConflict, CodeInvalidTransition, "claim is not in a state that accepts this command")
	case errors.Is(err, domain.ErrNoLines):
		writeErr(w, http.StatusBadRequest, CodeValidation, domain.ErrNoLines.Error())
	case errors.Is(err, domain.ErrCurrencyMismatch):
		writeErr(w, http.StatusBadRequest, CodeValidation, domain.ErrCurrencyMismatch.Error())
	case errors.Is(err, domain.ErrDocumentNotFound):
		writeErr(w, http.StatusBadRequest, CodeValidation, "receipt document not found")
	case errors.Is(err, domain.ErrDuplicateReceipt):
		writeErr(w, http.StatusConflict, CodeDuplicateRisk, "receipt document is already attached to another expense line")
	case errors.Is(err, domain.ErrIdempotencyConflict) && idem != nil:
		// A concurrent request with the same key committed first: replay it.
		if rec, gerr := h.Store.GetIdempotency(r.Context(), idem.Key); gerr == nil && rec != nil && rec.RequestHash == idem.RequestHash {
			replay(w, rec)
		} else {
			writeErr(w, http.StatusUnprocessableEntity, CodeKeyReused, "Idempotency-Key was already used with a different request")
		}
	default:
		h.Log.Error(op+": store unavailable", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
	}
	return true
}

// ── claims ───────────────────────────────────────────────────────────────────

func (h *Handler) CreateExpenseClaim(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return
	}
	var req domain.CreateExpenseClaimRequest
	if err := decodeBody(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.ClaimantPrincipalID == "" || req.Currency == "" {
		writeErr(w, http.StatusBadRequest, CodeValidation, "legal_entity_id, claimant_principal_id and currency are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, ExpenseCreate) {
		return
	}
	idem, stop := h.idemCheck(w, r, "CreateExpenseClaim", body, false)
	if stop {
		return
	}

	if err := h.Employee.VerifyActiveClaimant(r.Context(), principalID, req.LegalEntityID, req.ClaimantPrincipalID); err != nil {
		h.writeClaimantErr(w, err)
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	claim, err := h.Store.CreateClaim(r.Context(), verifiedTenant, req, principalID, r.Header.Get("X-Correlation-ID"), idem)
	if h.storeErr(w, r, "CreateExpenseClaim", err, idem) {
		return
	}
	writeJSON(w, http.StatusCreated, claim)
}

func (h *Handler) writeClaimantErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrClaimantNotEligible):
		writeErr(w, http.StatusBadRequest, CodeClaimantNotEligble, "claimant does not exist, does not belong to this legal entity, or is not an active employee")
	default:
		h.Log.Error("employee-master-svc lookup failed", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeDependencyDown, "employee-master-svc unavailable")
	}
}

func (h *Handler) GetExpenseClaim(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	claim, ok := h.authorizeRead(w, r, claimID)
	if !ok {
		return
	}
	lines, err := h.Store.ListLines(r.Context(), claimID)
	if err != nil {
		h.Log.Error("GetExpenseClaim: failed to list lines", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
		return
	}
	if lines == nil {
		lines = []domain.ExpenseLine{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"claim": claim, "lines": lines})
}

// AddExpenseLine verifies a supplied receipt document against the real
// document-vault-svc before it can be attached. Cross-claim reuse of the same
// receipt (negative-path scenario #2) is caught by the database's partial
// unique index.
func (h *Handler) AddExpenseLine(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return
	}
	var req domain.AddExpenseLineRequest
	if err := decodeBody(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return
	}
	if req.Merchant == "" || req.Amount <= 0 || req.Currency == "" || req.ExpenseDate.IsZero() {
		writeErr(w, http.StatusBadRequest, CodeValidation, "merchant, positive amount, currency and expense_date are required")
		return
	}
	if req.ClaimTaxRecovery && (req.Jurisdiction == "" || req.TaxCategory == "") {
		writeErr(w, http.StatusBadRequest, CodeValidation, "jurisdiction and tax_category are required when claim_tax_recovery is true")
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
	idem, stop := h.idemCheck(w, r, "AddExpenseLine", body, false)
	if stop {
		return
	}
	if !domain.CanAddLine(claim.Status) {
		writeErr(w, http.StatusConflict, CodeInvalidTransition, "claim is not in a state that accepts new expense lines")
		return
	}
	if req.Currency != claim.Currency {
		writeErr(w, http.StatusBadRequest, CodeValidation, domain.ErrCurrencyMismatch.Error())
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if req.ReceiptDocumentID != "" {
		if err := h.Docs.VerifyReceipt(r.Context(), principalID, verifiedTenant, claim.LegalEntityID, req.ReceiptDocumentID); err != nil {
			h.writeDocumentErr(w, err)
			return
		}
	}

	line, err := h.Store.AddExpenseLine(r.Context(), claimID, req, principalID, idem)
	if h.storeErr(w, r, "AddExpenseLine", err, idem) {
		return
	}
	writeJSON(w, http.StatusCreated, line)
}

// VoidExpenseLine voids (never deletes) a line on a DRAFT/RETURNED claim so a
// correction can be made while the evidence stays on record.
func (h *Handler) VoidExpenseLine(w http.ResponseWriter, r *http.Request) {
	claimID, lineID := chi.URLParam(r, "claimID"), chi.URLParam(r, "lineID")
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return
	}
	var req domain.VoidLineRequest
	if err := decodeBody(body, &req); err != nil || req.Reason == "" {
		writeErr(w, http.StatusBadRequest, CodeValidation, "reason is required")
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
	idem, stop := h.idemCheck(w, r, "VoidExpenseLine", body, false)
	if stop {
		return
	}
	line, err := h.Store.VoidExpenseLine(r.Context(), claimID, lineID, req.Reason, principalID, r.Header.Get("X-Correlation-ID"), idem)
	if h.storeErr(w, r, "VoidExpenseLine", err, idem) {
		return
	}
	writeJSON(w, http.StatusOK, line)
}

func (h *Handler) writeDocumentErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrDocumentNotFound):
		writeErr(w, http.StatusBadRequest, CodeValidation, "receipt document not found")
	case errors.Is(err, domain.ErrDocumentMismatch):
		writeErr(w, http.StatusForbidden, CodeForbidden, "receipt document does not belong to the caller's tenant/legal entity")
	case errors.Is(err, domain.ErrDocumentNotUsable):
		writeErr(w, http.StatusConflict, CodeDocumentNotUsable, "receipt document is not in a usable state")
	default:
		h.Log.Error("document-vault-svc lookup failed", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeDependencyDown, "document-vault-svc unavailable")
	}
}

// SubmitExpenseClaim is where two of AP-07's negative-path scenarios are
// enforced. #4: every active line declaring ClaimTaxRecovery gets a live
// tax-determination-svc call — TaxableAmount/CalculatedTaxAmount only ever
// come from that call's own response, and a failed call blocks the whole
// submission (TAX_UNAVAILABLE). The claim is then frozen into an immutable
// submission snapshot (SUBMITTED) and policy-svc's APPROVAL_THRESHOLD
// evaluation routes it to PENDING_APPROVAL. A missing or unavailable policy
// FAILS CLOSED for controlled categories, leaving the claim SUBMITTED so the
// same command can be re-run to resume routing.
func (h *Handler) SubmitExpenseClaim(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return
	}
	var req domain.VersionedRequest
	if err := decodeBody(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
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
	idem, stop := h.idemCheck(w, r, "SubmitExpenseClaim", body, false)
	if stop {
		return
	}
	if !domain.CanSubmit(claim.Status) {
		writeErr(w, http.StatusConflict, CodeInvalidTransition, "claim is not in a submittable state")
		return
	}
	if req.ExpectedVersion != nil && *req.ExpectedVersion != claim.Version {
		writeErr(w, http.StatusConflict, CodeStaleVersion, domain.ErrStaleVersion.Error())
		return
	}

	ctx := r.Context()
	corr := r.Header.Get("X-Correlation-ID")
	verifiedTenant := svcmiddleware.TenantFromContext(ctx)

	allLines, err := h.Store.ListLines(ctx, claimID)
	if err != nil {
		h.Log.Error("SubmitExpenseClaim: failed to list lines", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
		return
	}
	lines := domain.ActiveLines(allLines)

	if claim.Status != domain.StatusSubmitted {
		if len(lines) == 0 {
			writeErr(w, http.StatusBadRequest, CodeValidation, "at least one expense line is required to submit a claim")
			return
		}
		if strings.TrimSpace(claim.BusinessPurpose) == "" {
			writeErr(w, http.StatusUnprocessableEntity, CodeValidation, "business_purpose is required to submit a claim")
			return
		}
		for _, l := range lines {
			if !l.ClaimTaxRecovery || l.TaxDeterminationID != "" {
				continue
			}
			result, err := h.Tax.Determine(ctx, principalID, tax.DetermineRequest{
				TransactionID: l.LineID, LegalEntityID: claim.LegalEntityID, JurisdictionID: l.Jurisdiction,
				TaxCategory: l.TaxCategory, GrossAmount: l.Amount, Currency: l.Currency,
				EffectiveFrom: l.ExpenseDate.Format("2006-01-02"),
			})
			if err != nil {
				h.Log.Warn("SubmitExpenseClaim: tax determination failed — blocking submission", zap.Error(err))
				writeErr(w, http.StatusUnprocessableEntity, CodeTaxUnavailable, "tax determination failed for a line claiming tax recovery; submission blocked")
				return
			}
			if err := h.Store.SetLineTaxDetermination(ctx, l.LineID, result.DeterminationID, result.TaxableAmount, result.CalculatedTaxAmount); err != nil {
				h.Log.Error("SubmitExpenseClaim: failed to record tax determination", zap.Error(err))
				writeErr(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
				return
			}
		}
		if _, err := h.Store.SubmitClaim(ctx, domain.CommandParams{
			ClaimID: claimID, PrincipalID: principalID, CorrelationID: corr, ExpectedVersion: req.ExpectedVersion,
		}); h.storeErr(w, r, "SubmitExpenseClaim", err, nil) {
			return
		}
	}

	var total float64
	for _, l := range lines {
		total += l.Amount
	}
	policyResult, policyVersionID, perr := h.Policy.EvaluateApprovalThreshold(ctx, principalID, verifiedTenant, claim.LegalEntityID, total)
	if perr != nil {
		controlled := h.anyControlled(lines)
		switch {
		case errors.Is(perr, domain.ErrNoApplicablePolicy) && controlled:
			writeErr(w, http.StatusUnprocessableEntity, CodePolicyUnavailable, "no applicable approval policy for a controlled expense category; the claim stays SUBMITTED")
			return
		case !errors.Is(perr, domain.ErrNoApplicablePolicy) && controlled:
			h.Log.Error("SubmitExpenseClaim: policy evaluation failed", zap.Error(perr))
			writeErr(w, http.StatusServiceUnavailable, CodePolicyUnavailable, "policy-svc unavailable; the claim stays SUBMITTED")
			return
		}
		// Only non-controlled categories: proceed explicitly unassessed.
		policyResult, policyVersionID = string(domain.PolicyNotAssessed), ""
	}

	routed, err := h.Store.RouteForApproval(ctx, domain.RoutingParams{
		CommandParams:   domain.CommandParams{ClaimID: claimID, PrincipalID: principalID, CorrelationID: corr, Idem: idem},
		PolicyResult:    domain.PolicyAssessmentResult(policyResult),
		PolicyVersionID: policyVersionID,
	})
	if h.storeErr(w, r, "SubmitExpenseClaim", err, idem) {
		return
	}
	writeJSON(w, http.StatusOK, routed)
}

// ApproveExpenseClaim is AP-07's SoD-and-policy checkpoint. Idempotency-Key
// and expected_version are both REQUIRED. The approving principal is checked
// against authorization-svc's own-object SoD layer (never the claimant
// themselves — negative-path #1); a claim policy-svc flagged
// APPROVAL_REQUIRED needs the stronger ExpenseExceptionApprove authority;
// #3 (a line over the receipt-required threshold with no attached receipt)
// blocks approval unless a delegated policy exception was recorded; #4 (tax
// reclaim without a TAX determination) blocks approval; and a missing
// business purpose or policy assessment blocks approval (spec section 10).
// The AP-08 payable is NOT created here: the approval transaction writes a
// durable payable request which the payable relay fulfils.
func (h *Handler) ApproveExpenseClaim(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return
	}
	var req domain.VersionedRequest
	if err := decodeBody(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
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
	requiredAction := ExpenseApprove
	if claim.PolicyAssessmentResult == domain.PolicyApprovalRequired {
		requiredAction = ExpenseExceptionApprove
	}
	if !h.decide(w, r, principalID, claim, requiredAction) {
		return
	}
	idem, stop := h.idemCheck(w, r, "ApproveExpenseClaim", body, true)
	if stop {
		return
	}
	if req.ExpectedVersion == nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "expected_version is required to approve a claim")
		return
	}
	if !domain.CanDecide(claim.Status) {
		writeErr(w, http.StatusConflict, CodeInvalidTransition, "claim is not pending approval")
		return
	}
	if *req.ExpectedVersion != claim.Version {
		writeErr(w, http.StatusConflict, CodeStaleVersion, domain.ErrStaleVersion.Error())
		return
	}

	ctx := r.Context()
	allLines, err := h.Store.ListLines(ctx, claimID)
	if err != nil {
		h.Log.Error("ApproveExpenseClaim: failed to list lines", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
		return
	}
	lines := domain.ActiveLines(allLines)
	if len(lines) == 0 {
		writeErr(w, http.StatusBadRequest, CodeValidation, domain.ErrNoLines.Error())
		return
	}
	if strings.TrimSpace(claim.BusinessPurpose) == "" {
		writeErr(w, http.StatusUnprocessableEntity, CodeValidation, "business_purpose is missing; approval blocked")
		return
	}
	if claim.PolicyAssessmentResult == domain.PolicyNotAssessed && h.anyControlled(lines) {
		writeErr(w, http.StatusUnprocessableEntity, CodePolicyUnavailable, "no policy assessment on record for a controlled expense category; approval blocked")
		return
	}
	if !claim.HasPolicyException {
		threshold := h.Config.ReceiptRequiredThreshold
		if claim.TenantID != nil {
			threshold = h.resolveReceiptThreshold(ctx, *claim.TenantID)
		}
		for _, l := range lines {
			if l.Amount > threshold && l.ReceiptDocumentID == "" {
				writeErr(w, http.StatusConflict, CodeReceiptRequired, "one or more expense lines exceed the receipt-required threshold without an attached receipt")
				return
			}
		}
	}
	// Negative-path #4, defence in depth: a reclaim never proceeds without the
	// TAX determination that Submit obtained.
	for _, l := range lines {
		if l.ClaimTaxRecovery && l.TaxDeterminationID == "" {
			writeErr(w, http.StatusUnprocessableEntity, CodeTaxUnavailable, "a line claims tax recovery without a TAX determination; approval blocked")
			return
		}
	}

	verifiedTenant := svcmiddleware.TenantFromContext(ctx)
	updated, err := h.Store.ApproveClaim(ctx, domain.ApproveParams{
		CommandParams: domain.CommandParams{
			ClaimID: claimID, PrincipalID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"),
			ExpectedVersion: req.ExpectedVersion, Idem: idem,
		},
		DueDate: h.reimbursementDueDate(ctx, verifiedTenant),
		Posting: h.Config.Posting,
	})
	if h.storeErr(w, r, "ApproveExpenseClaim", err, idem) {
		return
	}

	// Best-effort immediate hand-off; the relay guarantees it either way.
	if h.Relay != nil {
		h.Relay.ProcessClaim(ctx, claimID)
		if fresh, ferr := h.Store.FindClaim(ctx, claimID); ferr == nil {
			updated = fresh
		}
	}
	writeJSON(w, http.StatusOK, updated)
}

// decisionCommand is the shared body of Reject/Return/RecordPolicyException:
// reason and expected_version are required, the decider must be independent
// of the claimant, and the command is idempotent when a key is supplied.
func (h *Handler) decisionCommand(w http.ResponseWriter, r *http.Request, op, action string,
	run func(ctx context.Context, p domain.CommandParams) (*domain.ExpenseClaim, error)) {
	claimID := chi.URLParam(r, "claimID")
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return
	}
	var req domain.DecisionRequest
	if err := decodeBody(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeErr(w, http.StatusBadRequest, CodeValidation, "reason is required")
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
	if !h.decide(w, r, principalID, claim, action) {
		return
	}
	idem, stop := h.idemCheck(w, r, op, body, false)
	if stop {
		return
	}
	if req.ExpectedVersion == nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "expected_version is required")
		return
	}
	if !domain.CanDecide(claim.Status) {
		writeErr(w, http.StatusConflict, CodeInvalidTransition, "claim is not pending approval")
		return
	}
	updated, err := run(r.Context(), domain.CommandParams{
		ClaimID: claimID, PrincipalID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"),
		Reason: req.Reason, ExpectedVersion: req.ExpectedVersion, Idem: idem,
	})
	if h.storeErr(w, r, op, err, idem) {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) RejectExpenseClaim(w http.ResponseWriter, r *http.Request) {
	h.decisionCommand(w, r, "RejectExpenseClaim", ExpenseApprove, h.Store.RejectClaim)
}

func (h *Handler) ReturnForCorrection(w http.ResponseWriter, r *http.Request) {
	h.decisionCommand(w, r, "ReturnForCorrection", ExpenseApprove, h.Store.ReturnClaim)
}

// RecordExpensePolicyException waives the receipt-evidence requirement
// (negative-path #3) for this claim — the delegated exception authority the
// spec's SoD line refers to ("approver cannot override policy/receipt
// requirement outside delegated exception authority"). Requires the
// stronger ExpenseExceptionApprove action and the same SoD check as any
// other decision.
func (h *Handler) RecordExpensePolicyException(w http.ResponseWriter, r *http.Request) {
	h.decisionCommand(w, r, "RecordExpensePolicyException", ExpenseExceptionApprove, h.Store.RecordPolicyException)
}

func (h *Handler) CancelExpenseClaim(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return
	}
	var req domain.DecisionRequest
	if err := decodeBody(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
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
	idem, stop := h.idemCheck(w, r, "CancelExpenseClaim", body, false)
	if stop {
		return
	}
	if !domain.CanCancel(claim.Status) {
		writeErr(w, http.StatusConflict, CodeInvalidTransition, "claim is not in a cancellable state")
		return
	}
	updated, err := h.Store.CancelClaim(r.Context(), domain.CommandParams{
		ClaimID: claimID, PrincipalID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"),
		Reason: req.Reason, ExpectedVersion: req.ExpectedVersion, Idem: idem,
	})
	if h.storeErr(w, r, "CancelExpenseClaim", err, idem) {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// CloseExpenseClaim closes a REIMBURSABLE claim. It is only ever allowed once
// AP-08 reports the claim's payable SETTLED — the state is verified live
// against AP-08, so a reason alone cannot close an unpaid claim. (The payable
// relay closes settled claims on its own; this is the explicit command.)
func (h *Handler) CloseExpenseClaim(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeValidation, "invalid request body")
		return
	}
	var req domain.DecisionRequest
	if err := decodeBody(body, &req); err != nil || req.Reason == "" {
		writeErr(w, http.StatusBadRequest, CodeValidation, "reason is required")
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
	if !h.authorize(w, r, principalID, claim.LegalEntityID, ExpenseApprove) {
		return
	}
	idem, stop := h.idemCheck(w, r, "CloseExpenseClaim", body, false)
	if stop {
		return
	}
	if !domain.CanClose(claim.Status) {
		writeErr(w, http.StatusConflict, CodeInvalidTransition, "only a reimbursable claim can be closed")
		return
	}
	if claim.PayableID == "" {
		writeErr(w, http.StatusConflict, CodePayableNotSettled, "the claim has no AP-08 payable to be settled")
		return
	}
	p, err := h.Payable.GetPayable(r.Context(), svcmiddleware.TenantFromContext(r.Context()), principalID, claim.PayableID)
	if err != nil {
		h.Log.Error("CloseExpenseClaim: AP-08 lookup failed", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeDependencyDown, "payable-open-item-svc unavailable")
		return
	}
	if p.Status != payableopenitem.StatusSettled {
		writeErr(w, http.StatusConflict, CodePayableNotSettled, "the claim's AP-08 payable is not SETTLED")
		return
	}
	updated, err := h.Store.CloseClaim(r.Context(), domain.CommandParams{
		ClaimID: claimID, PrincipalID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"),
		Reason: req.Reason, ExpectedVersion: req.ExpectedVersion, Idem: idem,
	})
	if h.storeErr(w, r, "CloseExpenseClaim", err, idem) {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── queries ──────────────────────────────────────────────────────────────────

func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	claim, ok := h.authorizeRead(w, r, claimID)
	if !ok {
		return
	}
	var actions []string
	if domain.CanAddLine(claim.Status) {
		actions = append(actions, "AddExpenseLine", "VoidExpenseLine")
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
	if domain.CanClose(claim.Status) {
		actions = append(actions, "CloseExpenseClaim")
	}
	if actions == nil {
		actions = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"claim_id": claimID, "status": claim.Status, "version": claim.Version, "available_actions": actions})
}

func (h *Handler) GetClaimHistory(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	if _, ok := h.authorizeRead(w, r, claimID); !ok {
		return
	}
	evts, err := h.Store.ListClaimEvents(r.Context(), claimID)
	if err != nil {
		h.Log.Error("GetClaimHistory: store unavailable", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
		return
	}
	if evts == nil {
		evts = []domain.ExpenseClaimEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": evts, "count": len(evts)})
}

// GetClaimSubmissions lists the immutable submission snapshots (one per
// submission version) so a reviewer can see exactly what each approver was
// shown, including versions superseded by a correction.
func (h *Handler) GetClaimSubmissions(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	if _, ok := h.authorizeRead(w, r, claimID); !ok {
		return
	}
	subs, err := h.Store.ListSubmissions(r.Context(), claimID)
	if err != nil {
		h.Log.Error("GetClaimSubmissions: store unavailable", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
		return
	}
	if subs == nil {
		subs = []domain.ExpenseClaimSubmission{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": subs, "count": len(subs)})
}

func (h *Handler) GetPolicyAssessment(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	claim, ok := h.authorizeRead(w, r, claimID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"claim_id": claimID, "result": claim.PolicyAssessmentResult, "policy_version_id": claim.PolicyVersionID,
	})
}

// GetAccountingStatus reports the true state of the claim's ACC-04 posting
// request(s): PENDING, POSTED, FAILED or QUARANTINED with the attempt count, the
// last error and the ledger's execution/journal ids. An accounting failure is
// visible here, never silent; this service never writes the ledger itself.
func (h *Handler) GetAccountingStatus(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	if _, ok := h.authorizeRead(w, r, claimID); !ok {
		return
	}
	reqs, err := h.Store.ListPostingRequests(r.Context(), claimID)
	if err != nil {
		h.Log.Error("GetAccountingStatus: store unavailable", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
		return
	}
	out := make([]map[string]interface{}, 0, len(reqs))
	for _, p := range reqs {
		out = append(out, map[string]interface{}{
			"source_event_id": p.SourceEventID, "status": p.Status, "attempts": p.Attempts, "last_error": p.LastError,
			"posting_execution_id": p.PostingExecutionID, "journal_id": p.JournalID, "posted_at": p.PostedAt,
		})
	}
	status := "NOT_APPLICABLE"
	if len(reqs) > 0 {
		status = reqs[len(reqs)-1].Status
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"claim_id": claimID, "status": status, "postings": out})
}

// RequeueAccounting puts the claim's FAILED/QUARANTINED posting requests back to
// PENDING once an operator fixed the cause (for example a missing ACC-02
// mapping). It needs the exception authority, since it re-opens a financial
// consequence; a POSTED request is never touched and a requeue re-submits the
// same source_event_id, which the ledger treats idempotently.
func (h *Handler) RequeueAccounting(w http.ResponseWriter, r *http.Request) {
	claimID := chi.URLParam(r, "claimID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	claim, ok := h.fetchClaimForAuth(w, r, claimID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, claim.LegalEntityID, ExpenseExceptionApprove) {
		return
	}
	n, err := h.Store.RequeuePostings(r.Context(), claimID)
	if err != nil {
		h.Log.Error("RequeueAccounting: store unavailable", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"claim_id": claimID, "requeued": n})
}

// GetDuplicateReceiptAssessment reports whether documentID is already
// attached to any live expense line — the queryable half of negative-path
// scenario #2, backed by the same store the database constraint reads from.
// legal_entity_id is required so the read can be authorized.
func (h *Handler) GetDuplicateReceiptAssessment(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "documentID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeErr(w, http.StatusBadRequest, CodeValidation, "legal_entity_id query parameter is required")
		return
	}
	if !h.authorize(w, r, principalID, legalEntityID, ExpenseRead) {
		return
	}
	inUse, claimID, lineID, err := h.Store.IsReceiptInUse(r.Context(), documentID)
	if err != nil {
		h.Log.Error("GetDuplicateReceiptAssessment: store unavailable", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, CodeStoreUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"document_id": documentID, "in_use": inUse, "claim_id": claimID, "line_id": lineID,
	})
}
