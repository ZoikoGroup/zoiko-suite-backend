package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/paymentstatus"
	"zoiko.io/payroll-run-svc/internal/store"
)

// WFPLiabilityStore is the persistence surface of WFP-06.
type WFPLiabilityStore interface {
	GetPayrollRunRef(ctx context.Context, id string) (*domain.PayrollRunRef, error)
	GetPayrollResultByRef(ctx context.Context, runRefID string) (*domain.PayrollResult, error)
	ListResultComponents(ctx context.Context, runRefID, workerID string) ([]domain.PayrollResultComponent, error)

	RegisterLiabilityClass(ctx context.Context, req domain.RegisterLiabilityClassRequest, actor string) (*domain.LiabilityClass, error)
	ListLiabilityClasses(ctx context.Context, legalEntityID string) ([]domain.LiabilityClass, error)
	ApplyRecognition(ctx context.Context, ref *domain.PayrollRunRef, res *domain.PayrollResult, plan domain.RecognitionPlan,
		review map[string]string, actor, correlationID, asOf string, mk store.LiabilityEventBuilder) (*store.RecognitionOutcome, error)
	DueSweep(ctx context.Context, legalEntityID, asOf string, mk store.LiabilityEventBuilder) ([]domain.PayrollLiability, error)
	ListLiabilities(ctx context.Context, f store.LiabilityFilter) ([]domain.PayrollLiability, error)
	GetLiability(ctx context.Context, id string) (*domain.PayrollLiability, error)
	ListLiabilityExceptions(ctx context.Context, runRefID, legalEntityID, state string) ([]domain.LiabilityException, error)
	ScheduleLiability(ctx context.Context, id string, expectedVersion int, scheduledFor, actor string, mk store.LiabilityEventBuilder) (*domain.PayrollLiability, error)
	SettleLiability(ctx context.Context, id string, req domain.SettleLiabilityRequest, ev store.SettlementEvidence, actor string, mk store.LiabilityEventBuilder) (*store.SettleOutcome, error)
	AdjustLiability(ctx context.Context, id string, req domain.AdjustLiabilityRequest, actor string, mk store.LiabilityEventBuilder) (*domain.PayrollLiability, error)
	DisputeLiability(ctx context.Context, id string, expectedVersion int, reason string, mk store.LiabilityEventBuilder) (*domain.PayrollLiability, error)
	ClearLiabilityDispute(ctx context.Context, id string, expectedVersion int, mk store.LiabilityEventBuilder) (*domain.PayrollLiability, error)
	SupersedeLiabilitiesForRun(ctx context.Context, runRefID string, mk store.LiabilityEventBuilder) (*store.SupersedeOutcome, error)
	ReconcileLiabilities(ctx context.Context, runRefID string, mk store.LiabilityEventBuilder) (*store.LiabilityReconciliation, error)
}

// AuthorityValidator checks a liability authority in counterparty-management-svc.
type AuthorityValidator interface {
	ValidateAuthority(ctx context.Context, tenantID, legalEntityID, counterpartyID string) error
}

// SettlementVerifier asks Banking whether a payment SETTLED.
type SettlementVerifier interface {
	VerifySettled(ctx context.Context, tenantID, legalEntityID, paymentID string) (*paymentstatus.Evidence, error)
}

const (
	actionLiabView      = "WFP_LIABILITY_VIEW"
	actionLiabClass     = "WFP_LIABILITY_CLASS_REGISTER"
	actionLiabRecognize = "WFP_LIABILITY_RECOGNIZE"
	actionLiabSchedule  = "WFP_LIABILITY_SCHEDULE"
	actionLiabSettle    = "WFP_LIABILITY_SETTLE"
	actionLiabAdjust    = "WFP_LIABILITY_ADJUST"
	actionLiabDispute   = "WFP_LIABILITY_DISPUTE"
	actionLiabSupersede = "WFP_LIABILITY_SUPERSEDE"
	actionLiabReconcile = "WFP_LIABILITY_RECONCILE"
	actionLiabSweep     = "WFP_LIABILITY_SWEEP"
)

type WFPLiabilityHandler struct {
	store       WFPLiabilityStore
	authz       AuthZClient
	authorities AuthorityValidator
	banking     SettlementVerifier
	now         func() time.Time
	log         *zap.Logger
}

func NewWFPLiability(st WFPLiabilityStore, authz AuthZClient, authorities AuthorityValidator, banking SettlementVerifier, log *zap.Logger) *WFPLiabilityHandler {
	return &WFPLiabilityHandler{store: st, authz: authz, authorities: authorities, banking: banking, now: time.Now, log: log}
}

func RegisterWFPLiabilityRoutes(r chi.Router, h *WFPLiabilityHandler) {
	r.Route("/v1/payroll/wfp/liabilities", func(r chi.Router) {
		r.Get("/", h.List)
		r.Post("/recognize", h.Recognize)
		r.Post("/due-sweep", h.DueSweep)
		r.Post("/supersede", h.Supersede)
		r.Post("/reconcile", h.Reconcile)
		r.Get("/exceptions", h.Exceptions)
		r.Get("/classes", h.ListClasses)
		r.Post("/classes", h.RegisterClass)
		r.Get("/{id}", h.Get)
		r.Post("/{id}/schedule", h.Schedule)
		r.Post("/{id}/settlements", h.Settle)
		r.Post("/{id}/adjustments", h.Adjust)
		r.Post("/{id}/dispute", h.Dispute)
		r.Post("/{id}/clear-dispute", h.ClearDispute)
	})
}

// ── plumbing ─────────────────────────────────────────────────────────────────

func (h *WFPLiabilityHandler) identity(w http.ResponseWriter, r *http.Request) (principal, tenant string, ok bool) {
	principal = r.Header.Get("X-Principal-Id")
	tenant = svcmiddleware.TenantFromContext(r.Context())
	if principal == "" || tenant == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", "", false
	}
	return principal, tenant, true
}

func (h *WFPLiabilityHandler) authorize(w http.ResponseWriter, r *http.Request, principal, entityID, action string) bool {
	if strings.TrimSpace(entityID) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal entity is required")
		return false
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, entityID, action); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "forbidden", err.Error())
		} else {
			writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
		}
		return false
	}
	return true
}

func (h *WFPLiabilityHandler) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrLiabilityNotFound), errors.Is(err, domain.ErrRunRefNotFound), errors.Is(err, domain.ErrResultNotFound),
		errors.Is(err, domain.ErrLiabilityClassNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrLiabilityVersionConflict):
		writeError(w, http.StatusConflict, "VERSION_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrLiabilityState), errors.Is(err, domain.ErrLiabilityNeedsReview), errors.Is(err, domain.ErrLiabilityResultNotActive),
		errors.Is(err, domain.ErrResultNotSuperseded), errors.Is(err, domain.ErrLiabilityHasSettlements):
		writeError(w, http.StatusConflict, "INVALID_STATE", err.Error())
	case errors.Is(err, domain.ErrSettlementConflict), errors.Is(err, domain.ErrLiabilityClassConflict), errors.Is(err, domain.ErrLiabilityClassOrder):
		writeError(w, http.StatusConflict, "CONFLICT", err.Error())
	case errors.Is(err, domain.ErrPaymentNotSettled), errors.Is(err, domain.ErrPaymentInConflict):
		writeError(w, http.StatusConflict, "PAYMENT_NOT_SETTLED", err.Error())
	case errors.Is(err, domain.ErrOverSettlement), errors.Is(err, domain.ErrAdjustBelowSettled), errors.Is(err, domain.ErrSettlementCurrency),
		errors.Is(err, domain.ErrPaymentEntityMismatch), errors.Is(err, domain.ErrPaymentNotFound), errors.Is(err, domain.ErrNoPostedAccounting),
		errors.Is(err, domain.ErrAuthorityInvalid), errors.Is(err, domain.ErrInstructionMismatch):
		writeError(w, http.StatusUnprocessableEntity, "UNPROCESSABLE", err.Error())
	case errors.Is(err, domain.ErrBankingUnavailable), errors.Is(err, domain.ErrAuthorityUnavailable):
		writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", err.Error())
	case errors.Is(err, domain.ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, "identity_missing", err.Error())
	default:
		h.log.Error("wfp-06 store failure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "payroll liability store unavailable")
	}
}

func (h *WFPLiabilityHandler) events(r *http.Request, actor string) store.LiabilityEventBuilder {
	return func(ch store.LiabilityChange) ([]domain.OutboxEvent, error) {
		return events.BuildLiabilityEvents(ch.Kind, ch.Liability, ch.Exception, ch.Detail, actor, getCorrelationID(r), r.Header.Get("X-Causation-Id"))
	}
}

func (h *WFPLiabilityHandler) today() string { return h.now().UTC().Format("2006-01-02") }

// loadAuthorized loads a liability and authorizes against ITS legal entity.
func (h *WFPLiabilityHandler) loadAuthorized(w http.ResponseWriter, r *http.Request, action string) (*domain.PayrollLiability, string, string, bool) {
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return nil, "", "", false
	}
	l, err := h.store.GetLiability(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeErr(w, err)
		return nil, "", "", false
	}
	if !h.authorize(w, r, principal, l.LegalEntityID, action) {
		return nil, "", "", false
	}
	return l, principal, tenant, true
}

// runRefBody reads {run_ref_id}, loads the run reference and authorizes against its entity.
func (h *WFPLiabilityHandler) runRefBody(w http.ResponseWriter, r *http.Request, action string, into *string) (*domain.PayrollRunRef, string, string, bool) {
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return nil, "", "", false
	}
	if _, err := uuid.Parse(*into); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "run_ref_id must be a UUID")
		return nil, "", "", false
	}
	ref, err := h.store.GetPayrollRunRef(r.Context(), *into)
	if err != nil {
		h.writeErr(w, err)
		return nil, "", "", false
	}
	if !h.authorize(w, r, principal, ref.LegalEntityID, action) {
		return nil, "", "", false
	}
	return ref, principal, tenant, true
}

// ── registry ─────────────────────────────────────────────────────────────────

// RegisterClass appends a liability-class row. The authority must be a real, ACTIVE
// counterparty of the legal entity: an unreachable counterparty service is a refusal,
// not a pass.
func (h *WFPLiabilityHandler) RegisterClass(w http.ResponseWriter, r *http.Request) {
	var req domain.RegisterLiabilityClassRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principal, req.LegalEntityID, actionLiabClass) {
		return
	}
	if err := h.authorities.ValidateAuthority(r.Context(), tenant, req.LegalEntityID, req.AuthorityCounterpartyID); err != nil {
		h.writeErr(w, err)
		return
	}
	c, err := h.store.RegisterLiabilityClass(r.Context(), req, principal)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *WFPLiabilityHandler) ListClasses(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	entity := strings.TrimSpace(r.URL.Query().Get("legal_entity_id"))
	if !h.authorize(w, r, principal, entity, actionLiabView) {
		return
	}
	out, err := h.store.ListLiabilityClasses(r.Context(), entity)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

// ── recognition ──────────────────────────────────────────────────────────────

// Recognize is RecognizePayrollLiabilities: it turns an ACCEPTED result's components
// into liabilities through the registry. It is idempotent: recognizing again changes
// nothing that already exists, opens exceptions for what the registry does not cover,
// and resolves exceptions whose cause was fixed. An unreachable counterparty service
// stops the whole command; an authority that is not usable recognizes the liability
// under review.
func (h *WFPLiabilityHandler) Recognize(w http.ResponseWriter, r *http.Request) {
	var req domain.RecognizeLiabilitiesRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	ref, principal, tenant, ok := h.runRefBody(w, r, actionLiabRecognize, &req.RunRefID)
	if !ok {
		return
	}
	res, err := h.store.GetPayrollResultByRef(r.Context(), ref.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if ref.AcceptanceState != domain.AcceptanceAccepted || res.Outcome != domain.ResultAccepted {
		h.writeErr(w, domain.ErrLiabilityResultNotActive)
		return
	}
	comps, err := h.store.ListResultComponents(r.Context(), ref.RunRefID, "")
	if err != nil {
		h.writeErr(w, err)
		return
	}
	classes, err := h.store.ListLiabilityClasses(r.Context(), ref.LegalEntityID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	plan, err := domain.PlanRecognition(domain.PlanInput{PayDate: ref.PayDate, Flags: res.EvidenceFlags, Components: comps, Classes: classes})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	review := map[string]string{}
	for _, d := range plan.Liabilities {
		id := d.Class.AuthorityCounterpartyID
		if _, done := review[id]; done {
			continue
		}
		switch verr := h.authorities.ValidateAuthority(r.Context(), tenant, ref.LegalEntityID, id); {
		case verr == nil:
			review[id] = ""
		case errors.Is(verr, domain.ErrAuthorityInvalid):
			review[id] = verr.Error()
		default:
			h.writeErr(w, verr)
			return
		}
	}
	out, err := h.store.ApplyRecognition(r.Context(), ref, res, plan, review, principal, getCorrelationID(r), h.today(), h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	status := http.StatusOK
	if out.NewCount > 0 {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
}

// DueSweep moves liabilities whose due date has arrived to DUE.
func (h *WFPLiabilityHandler) DueSweep(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LegalEntityID string `json:"legal_entity_id"`
	}
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	principal, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principal, req.LegalEntityID, actionLiabSweep) {
		return
	}
	out, err := h.store.DueSweep(r.Context(), req.LegalEntityID, h.today(), h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

// ── reads ────────────────────────────────────────────────────────────────────

func (h *WFPLiabilityHandler) List(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	entity := strings.TrimSpace(q.Get("legal_entity_id"))
	runRef := strings.TrimSpace(q.Get("run_ref_id"))
	if runRef != "" {
		if _, err := uuid.Parse(runRef); err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", "run_ref_id must be a UUID")
			return
		}
		ref, err := h.store.GetPayrollRunRef(r.Context(), runRef)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		entity = ref.LegalEntityID
	}
	if !h.authorize(w, r, principal, entity, actionLiabView) {
		return
	}
	out, err := h.store.ListLiabilities(r.Context(), store.LiabilityFilter{
		RunRefID: runRef, LegalEntityID: entity, State: q.Get("state"), AuthorityID: q.Get("authority_counterparty_id"),
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

func (h *WFPLiabilityHandler) Get(w http.ResponseWriter, r *http.Request) {
	l, _, _, ok := h.loadAuthorized(w, r, actionLiabView)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// Exceptions lists open (or, with ?state=, any) liability exceptions of a result or entity.
func (h *WFPLiabilityHandler) Exceptions(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	entity := strings.TrimSpace(q.Get("legal_entity_id"))
	runRef := strings.TrimSpace(q.Get("run_ref_id"))
	if runRef != "" {
		if _, err := uuid.Parse(runRef); err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", "run_ref_id must be a UUID")
			return
		}
		ref, err := h.store.GetPayrollRunRef(r.Context(), runRef)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		entity = ref.LegalEntityID
	}
	if !h.authorize(w, r, principal, entity, actionLiabView) {
		return
	}
	state := q.Get("state")
	if state == "" {
		state = domain.ExcOpen
	}
	out, err := h.store.ListLiabilityExceptions(r.Context(), runRef, entity, state)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

// ── commands on one liability ────────────────────────────────────────────────

func (h *WFPLiabilityHandler) Schedule(w http.ResponseWriter, r *http.Request) {
	var req domain.ScheduleLiabilityRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) is required")
		return
	}
	if _, err := time.Parse("2006-01-02", req.ScheduledFor); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "scheduled_for must be a date (YYYY-MM-DD)")
		return
	}
	l, principal, _, ok := h.loadAuthorized(w, r, actionLiabSchedule)
	if !ok {
		return
	}
	out, err := h.store.ScheduleLiability(r.Context(), l.LiabilityID, version, req.ScheduledFor, principal, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Settle applies a Banking settlement to a liability. The payment must be SETTLED in
// payment-status-svc for the liability's legal entity; Banking being unreachable is a
// refusal. Payment-status carries no amount or payee, so the amount is the caller's
// declaration and is stored as such (amount_evidence).
func (h *WFPLiabilityHandler) Settle(w http.ResponseWriter, r *http.Request) {
	var req domain.SettleLiabilityRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	l, principal, tenant, ok := h.loadAuthorized(w, r, actionLiabSettle)
	if !ok {
		return
	}
	ev, err := h.banking.VerifySettled(r.Context(), tenant, l.LegalEntityID, req.PaymentID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if req.InstructionID != "" && ev.SourceReference != req.InstructionID {
		// Banking holds this payment under a different reference: it is not the payment of that instruction.
		h.writeErr(w, domain.ErrInstructionMismatch)
		return
	}
	fs := ev.FinalitySource
	var fsp *string
	if fs != "" {
		fsp = &fs
	}
	out, err := h.store.SettleLiability(r.Context(), l.LiabilityID, req, store.SettlementEvidence{BankingStatus: ev.Status, FinalitySource: fsp}, principal, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	status := http.StatusCreated
	if out.Replay {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"liability": out.Liability, "settlement": out.Settlement, "replay": out.Replay})
}

func (h *WFPLiabilityHandler) Adjust(w http.ResponseWriter, r *http.Request) {
	var req domain.AdjustLiabilityRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) is required")
		return
	}
	req.ExpectedVersion = version
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	l, principal, _, ok := h.loadAuthorized(w, r, actionLiabAdjust)
	if !ok {
		return
	}
	out, err := h.store.AdjustLiability(r.Context(), l.LiabilityID, req, principal, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *WFPLiabilityHandler) Dispute(w http.ResponseWriter, r *http.Request) {
	var req domain.DisputeLiabilityRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok || strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) and reason are required")
		return
	}
	l, principal, _, ok := h.loadAuthorized(w, r, actionLiabDispute)
	if !ok {
		return
	}
	out, err := h.store.DisputeLiability(r.Context(), l.LiabilityID, version, req.Reason, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *WFPLiabilityHandler) ClearDispute(w http.ResponseWriter, r *http.Request) {
	var req domain.LiabilityTransitionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) is required")
		return
	}
	l, principal, _, ok := h.loadAuthorized(w, r, actionLiabDispute)
	if !ok {
		return
	}
	out, err := h.store.ClearLiabilityDispute(r.Context(), l.LiabilityID, version, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ── commands on a result ─────────────────────────────────────────────────────

// Supersede follows a result's supersession (Wave 4): unsettled liabilities of the
// superseded result become SUPERSEDED, and any with settlements come back in
// needs_adjustment for an explicit evidenced adjustment.
func (h *WFPLiabilityHandler) Supersede(w http.ResponseWriter, r *http.Request) {
	var req domain.SupersedeLiabilitiesRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	_, principal, _, ok := h.runRefBody(w, r, actionLiabSupersede, &req.RunRefID)
	if !ok {
		return
	}
	out, err := h.store.SupersedeLiabilitiesForRun(r.Context(), req.RunRefID, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Reconcile compares a result's liabilities with its POSTED payroll journal (per
// payable key). A mismatch is recorded as an exception and an event, not hidden.
func (h *WFPLiabilityHandler) Reconcile(w http.ResponseWriter, r *http.Request) {
	var req domain.RecognizeLiabilitiesRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	_, principal, _, ok := h.runRefBody(w, r, actionLiabReconcile, &req.RunRefID)
	if !ok {
		return
	}
	out, err := h.store.ReconcileLiabilities(r.Context(), req.RunRefID, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
