package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/vendor-due-diligence-svc/internal/domain"
	svcmiddleware "zoiko.io/vendor-due-diligence-svc/internal/middleware"
)

type Store interface {
	CreateCheck(ctx context.Context, c *domain.VendorDDCheck) (created bool, err error)
	GetCheck(ctx context.Context, id string) (*domain.VendorDDCheck, error)
	ListChecks(ctx context.Context, legalEntityID, counterpartyID string) ([]domain.VendorDDCheck, error)
	CompleteCheck(ctx context.Context, id, newStatus, riskOutcome, screeningBasis string) error
	AddEvidence(ctx context.Context, e *domain.VendorDDEvidence) error
	ListEvidence(ctx context.Context, checkID string) ([]domain.VendorDDEvidence, error)
}

type Publisher interface {
	PublishStarted(ctx context.Context, correlationID string, check domain.VendorDDCheck)
	PublishCompleted(ctx context.Context, correlationID string, check domain.VendorDDCheck)
	PublishFailed(ctx context.Context, correlationID string, check domain.VendorDDCheck, reason string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// CounterpartyClient pushes a completed check's outcome onto the
// counterparty's record in counterparty-management-svc. Best-effort by
// design — see updateCounterparty below.
type CounterpartyClient interface {
	UpdateComplianceStatus(ctx context.Context, tenantID, counterpartyID, complianceStatus string) error
	UpdateRiskCategory(ctx context.Context, tenantID, counterpartyID, riskCategory string) error
}

const (
	actionInitiate = "VENDOR_DD_INITIATE"
	actionView     = "VENDOR_DD_VIEW"
)

// stubSanctionsDenylist is a documented stub, not a real sanctions-list
// integration — external-data-feed-svc was checked and does not carry
// sanctions/watchlist data (its FeedType is MARKET_DATA/CREDIT_SCORE/
// COMPANY_INFO/FX_RATE/ESG_DATA only), so a real feed for this doesn't exist
// yet on the platform. This is a case-insensitive exact-name match against a
// hardcoded list, matching the "documented stub-first posture" convention
// used by accounts-payable-svc's authz client. Replace with a real
// sanctions-list integration when one exists.
var stubSanctionsDenylist = []string{
	"acme sanctioned holdings",
	"restricted trading corp",
}

// screenVendorName runs the stub check. flagged=true means the name matched
// the denylist; basis is always populated so there is a human-readable
// reason on the evidence record either way.
func screenVendorName(vendorName string) (flagged bool, basis string) {
	needle := strings.ToLower(strings.TrimSpace(vendorName))
	for _, entry := range stubSanctionsDenylist {
		if needle == entry {
			return true, "matched stub sanctions denylist entry: " + entry
		}
	}
	return false, "no match against stub sanctions denylist"
}

type Handler struct {
	store        Store
	publisher    Publisher
	authz        AuthZClient
	counterparty CounterpartyClient
	log          *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, counterparty CounterpartyClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, counterparty: counterparty, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/vendor-checks", func(r chi.Router) {
		r.Post("/", h.CreateCheck)
		r.Get("/", h.ListChecks)
		r.Get("/{id}", h.GetCheck)
	})
}

// ── POST /v1/vendor-checks ────────────────────────────────────────────────────

// CreateCheck starts a due-diligence check and runs it to completion
// synchronously (the stub screening has no I/O, so there is nothing to wait
// on asynchronously here — a real screening integration might change that).
//
// Idempotent on (tenant_id, correlation_id): a retry of an already-processed
// request returns the existing check and its evidence as-is, without
// re-running screening or re-notifying counterparty-management-svc a second
// time for the same request.
func (h *Handler) CreateCheck(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.CounterpartyID == "" || req.LegalEntityID == "" || req.VendorName == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"counterparty_id, legal_entity_id, vendor_name, correlation_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionInitiate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	correlationID := getCorrelationID(r)
	now := time.Now().UTC()

	check := &domain.VendorDDCheck{
		CheckID:                uuid.NewString(),
		TenantID:               tenantID,
		LegalEntityID:          req.LegalEntityID,
		CounterpartyID:         req.CounterpartyID,
		VendorName:             req.VendorName,
		Status:                 "STARTED",
		CorrelationID:          req.CorrelationID,
		InitiatedByPrincipalID: principalID,
		StartedAt:              now,
	}

	created, err := h.store.CreateCheck(r.Context(), check)
	if err != nil {
		h.log.Error("failed to create vendor dd check", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if !created {
		// Replay: this correlation_id was already processed. Return the
		// existing check's current state rather than running screening again.
		evidence, err := h.store.ListEvidence(r.Context(), check.CheckID)
		if err != nil {
			h.log.Error("failed to list evidence for replayed check", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
			return
		}
		if evidence == nil {
			evidence = []domain.VendorDDEvidence{}
		}
		writeJSON(w, http.StatusOK, domain.CheckDetailResponse{Check: *check, Evidence: evidence})
		return
	}

	h.publisher.PublishStarted(r.Context(), correlationID, *check)

	flagged, basis := screenVendorName(req.VendorName)

	evidence := &domain.VendorDDEvidence{
		EvidenceID:   uuid.NewString(),
		CheckID:      check.CheckID,
		TenantID:     tenantID,
		EvidenceType: "SANCTIONS_SCREENING",
		Description:  basis,
		RecordedAt:   time.Now().UTC(),
	}
	if err := h.store.AddEvidence(r.Context(), evidence); err != nil {
		// The check row itself was already created and is visible via GET —
		// losing the evidence write shouldn't also lose that. Log loudly and
		// continue; an evidence-list gap is recoverable by re-running a check,
		// a lost check row would not be.
		h.log.Error("failed to record vendor dd evidence", zap.Error(err))
	}

	riskOutcome := "CLEAR"
	if flagged {
		riskOutcome = "FLAGGED"
	}

	if err := h.store.CompleteCheck(r.Context(), check.CheckID, "COMPLETED", riskOutcome, basis); err != nil {
		h.log.Error("failed to complete vendor dd check", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	check.Status = "COMPLETED"
	check.RiskOutcome = riskOutcome
	check.ScreeningBasis = basis
	completedAt := time.Now().UTC()
	check.CompletedAt = &completedAt

	h.publisher.PublishCompleted(r.Context(), correlationID, *check)

	h.updateCounterparty(r.Context(), tenantID, check.CounterpartyID, riskOutcome)

	writeJSON(w, http.StatusCreated, domain.CheckDetailResponse{
		Check:    *check,
		Evidence: []domain.VendorDDEvidence{*evidence},
	})
}

// updateCounterparty pushes this check's outcome onto counterparty-
// management-svc, best-effort. counterparty-management-svc being unreachable
// (or slow, or itself broken) does not make this service's own due-diligence
// result any less true — the check ran, the evidence is recorded, and that
// is the durable source of truth. A failed push here just means the
// counterparty record wasn't enriched with it yet; it is logged loudly so
// the gap is visible, but it never fails the request back to the caller.
func (h *Handler) updateCounterparty(ctx context.Context, tenantID, counterpartyID, riskOutcome string) {
	complianceStatus := "VERIFIED"
	if riskOutcome == "FLAGGED" {
		complianceStatus = "REJECTED"
	}

	if err := h.counterparty.UpdateComplianceStatus(ctx, tenantID, counterpartyID, complianceStatus); err != nil {
		h.log.Warn("failed to push compliance status to counterparty-management-svc",
			zap.String("counterparty_id", counterpartyID), zap.Error(err))
	}

	if riskOutcome == "FLAGGED" {
		if err := h.counterparty.UpdateRiskCategory(ctx, tenantID, counterpartyID, "HIGH"); err != nil {
			h.log.Warn("failed to push risk category to counterparty-management-svc",
				zap.String("counterparty_id", counterpartyID), zap.Error(err))
		}
	}
}

// ── GET /v1/vendor-checks ─────────────────────────────────────────────────────

func (h *Handler) ListChecks(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	counterpartyID := r.URL.Query().Get("counterparty_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListChecks(r.Context(), legalEntityID, counterpartyID)
	if err != nil {
		h.log.Error("failed to list vendor dd checks", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.VendorDDCheck{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/vendor-checks/{id} ────────────────────────────────────────────────

func (h *Handler) GetCheck(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	check, err := h.store.GetCheck(r.Context(), id)
	if errors.Is(err, domain.ErrCheckNotFound) {
		writeError(w, http.StatusNotFound, "check_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch vendor dd check", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, check.LegalEntityID, actionView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	evidence, err := h.store.ListEvidence(r.Context(), id)
	if err != nil {
		h.log.Error("failed to list vendor dd evidence", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if evidence == nil {
		evidence = []domain.VendorDDEvidence{}
	}

	writeJSON(w, http.StatusOK, domain.CheckDetailResponse{Check: *check, Evidence: evidence})
}

// ── Helpers ────────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func getCorrelationID(r *http.Request) string {
	cid := r.Header.Get("X-Correlation-ID")
	if cid == "" {
		return uuid.NewString()
	}
	return cid
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code":    code,
		"error_message": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
