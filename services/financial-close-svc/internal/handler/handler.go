package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
)

type Store interface {
	CreateFiscalPeriod(ctx context.Context, fp *domain.FiscalPeriod) (created bool, err error)
	GetFiscalPeriod(ctx context.Context, id string) (*domain.FiscalPeriod, error)
	GetFiscalPeriodByName(ctx context.Context, legalEntityID, name string) (*domain.FiscalPeriod, error)
	ListFiscalPeriods(ctx context.Context, legalEntityID string) ([]domain.FiscalPeriod, error)
	LockFiscalPeriod(ctx context.Context, id string, lockedAt time.Time, evidenceDocID string) error
	CreateCloseEvidence(ctx context.Context, evidence *domain.CloseEvidence) error
}

type Publisher interface {
	PublishCloseStarted(ctx context.Context, correlationID string, fp domain.FiscalPeriod)
	PublishCloseBlocked(ctx context.Context, correlationID string, fp domain.FiscalPeriod, reasons []string)
	PublishClosed(ctx context.Context, correlationID string, fp domain.FiscalPeriod, evidenceID string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Clients interface {
	GetUnpostedJournalsCount(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (int, error)
	CompileTrialBalance(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (map[string]float64, error)
	GetUnsettledAPInvoicesCount(ctx context.Context, tenantID, legalEntityID string) (int, error)
	GetUnsettledARInvoicesCount(ctx context.Context, tenantID, legalEntityID string) (int, error)
	UploadCloseEvidence(ctx context.Context, tenantID, legalEntityID, periodName string, trialBalance map[string]float64, principalID string) (string, error)
}

const (
	actionCloseConfig   = "PERIOD_CLOSE_CONFIG"
	actionCloseView     = "PERIOD_CLOSE_VIEW"
	actionCloseInitiate = "PERIOD_CLOSE_INITIATE"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	clients   Clients
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, clients Clients, log *zap.Logger) *Handler {
	return &Handler{
		store:     store,
		publisher: publisher,
		authz:     authz,
		clients:   clients,
		log:       log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/close/periods", func(r chi.Router) {
		r.Post("/", h.CreateFiscalPeriod)
		r.Get("/", h.ListFiscalPeriods)
		r.Get("/status", h.GetPeriodStatus)
		r.Post("/{id}/lock", h.LockPeriod)
	})
}

// ── POST /v1/close/periods ────────────────────────────────────────────────────────

func (h *Handler) CreateFiscalPeriod(w http.ResponseWriter, r *http.Request) {
	var req domain.PeriodCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.PeriodName == "" || req.PeriodStart.IsZero() || req.PeriodEnd.IsZero() {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, period_name, period_start, period_end are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCloseConfig); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())

	fp := &domain.FiscalPeriod{
		FiscalPeriodID: uuid.NewString(),
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		PeriodName:     req.PeriodName,
		PeriodStart:    req.PeriodStart.UTC(),
		PeriodEnd:      req.PeriodEnd.UTC(),
		CloseStatus:    "OPEN",
	}

	created, err := h.store.CreateFiscalPeriod(r.Context(), fp)
	if err != nil {
		h.log.Error("failed to create fiscal period", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if !created {
		// Replay of a prior request for the same (legal_entity_id, period_name)
		// — return the original period rather than erroring.
		writeJSON(w, http.StatusOK, fp)
		return
	}

	writeJSON(w, http.StatusCreated, fp)
}

// ── GET /v1/close/periods ─────────────────────────────────────────────────────────

func (h *Handler) ListFiscalPeriods(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")

	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionCloseView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListFiscalPeriods(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("failed to list fiscal periods", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/close/periods/status ──────────────────────────────────────────────────

func (h *Handler) GetPeriodStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")
	periodName := q.Get("period_name")

	if legalEntityID == "" || periodName == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and period_name are required")
		return
	}

	// Internal validation queries (e.g. from general-ledger-svc) bypass auth checking
	// but are still tenant isolated.
	fp, err := h.store.GetFiscalPeriodByName(r.Context(), legalEntityID, periodName)
	if errors.Is(err, domain.ErrFiscalPeriodNotFound) {
		writeJSON(w, http.StatusOK, map[string]string{
			"period_name":  periodName,
			"close_status": "OPEN", // Default to open if not registered
		})
		return
	}
	if err != nil {
		h.log.Error("failed to fetch fiscal period status", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"fiscal_period_id": fp.FiscalPeriodID,
		"period_name":      fp.PeriodName,
		"close_status":     fp.CloseStatus,
	})
}

// ── POST /v1/close/periods/{id}/lock ──────────────────────────────────────────────

func (h *Handler) LockPeriod(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = uuid.NewString()
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())

	fp, err := h.store.GetFiscalPeriod(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrFiscalPeriodNotFound) {
			writeError(w, http.StatusNotFound, "period_not_found", "")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionCloseInitiate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if fp.CloseStatus != "OPEN" {
		writeError(w, http.StatusUnprocessableEntity, "period_already_locked", string(domain.ErrPeriodAlreadyLocked))
		return
	}

	h.publisher.PublishCloseStarted(r.Context(), correlationID, *fp)

	// Step 1: Run Readiness Checks (FAIL CLOSED on any dependency query error)
	var blockingIssues []string

	// Check GL unposted journals
	unpostedCount, err := h.clients.GetUnpostedJournalsCount(r.Context(), tenantID, fp.LegalEntityID, fp.PeriodName)
	if err != nil {
		h.log.Error("failed to verify outstanding journals", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "readiness_check_failed", "general-ledger-svc query failed: close blocked")
		return
	}
	if unpostedCount > 0 {
		blockingIssues = append(blockingIssues, fmt.Sprintf("unposted_journals_exist: %d journals are in PENDING or VALIDATED status", unpostedCount))
	}

	// Check AP un-paid invoices
	unsettledAP, err := h.clients.GetUnsettledAPInvoicesCount(r.Context(), tenantID, fp.LegalEntityID)
	if err != nil {
		h.log.Error("failed to verify AP invoices", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "readiness_check_failed", "accounts-payable-svc query failed: close blocked")
		return
	}
	if unsettledAP > 0 {
		blockingIssues = append(blockingIssues, fmt.Sprintf("unsettled_ap_invoices_exist: %d invoices are not fully payment requested", unsettledAP))
	}

	// Check AR un-paid invoices
	unsettledAR, err := h.clients.GetUnsettledARInvoicesCount(r.Context(), tenantID, fp.LegalEntityID)
	if err != nil {
		h.log.Error("failed to verify AR invoices", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "readiness_check_failed", "accounts-receivable-svc query failed: close blocked")
		return
	}
	if unsettledAR > 0 {
		blockingIssues = append(blockingIssues, fmt.Sprintf("unsettled_ar_invoices_exist: %d invoices are not PAID", unsettledAR))
	}

	// Check for blocking errors
	if len(blockingIssues) > 0 {
		h.publisher.PublishCloseBlocked(r.Context(), correlationID, *fp, blockingIssues)
		h.log.Warn("period close blocked by outstanding items", zap.String("period_id", id), zap.Strings("reasons", blockingIssues))
		writeJSON(w, http.StatusUnprocessableEntity, domain.ReadinessCheckResponse{
			IsReady:        false,
			BlockingIssues: blockingIssues,
		})
		return
	}

	// Step 2: Compile Trial Balance & Generate Evidence (FAIL CLOSED on error)
	balances, err := h.clients.CompileTrialBalance(r.Context(), tenantID, fp.LegalEntityID, fp.PeriodName)
	if err != nil {
		h.log.Error("failed to compile trial balance", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "close_failed", "cannot compile trial balance: "+err.Error())
		return
	}

	// Upload evidence to Document Vault (exposes file UUID)
	docID, err := h.clients.UploadCloseEvidence(r.Context(), tenantID, fp.LegalEntityID, fp.PeriodName, balances, principalID)
	if err != nil {
		h.log.Error("failed to upload close evidence document", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "close_failed", "failed to record close evidence in vault")
		return
	}

	// Calculate verification hash & cryptographic signature
	keys := make([]string, 0, len(balances))
	for k := range balances {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	for _, k := range keys {
		buf.WriteString(fmt.Sprintf("%s:%f;", k, balances[k]))
	}

	hashBytes := sha256.Sum256(buf.Bytes())
	trialBalanceHash := hex.EncodeToString(hashBytes[:])

	// Sign the trial balance hash utilizing the TenantID as secret seed for RLS compliance validation
	mac := hmac.New(sha256.New, []byte(tenantID))
	mac.Write(hashBytes[:])
	signature := hex.EncodeToString(mac.Sum(nil))

	now := time.Now().UTC()

	// Update DB record lock state
	if err := h.store.LockFiscalPeriod(r.Context(), id, now, docID); err != nil {
		if errors.Is(err, domain.ErrPeriodAlreadyLocked) {
			// Replay of a prior request that already succeeded (e.g. a client
			// timeout on a lock call that actually completed server-side) —
			// return the current locked state rather than misreporting this
			// as a store outage.
			current, getErr := h.store.GetFiscalPeriod(r.Context(), id)
			if getErr != nil {
				h.log.Error("failed to fetch already-locked period", zap.Error(getErr))
				writeError(w, http.StatusServiceUnavailable, "store_unavailable", getErr.Error())
				return
			}
			writeJSON(w, http.StatusOK, domain.PeriodLockResponse{
				FiscalPeriodID:     current.FiscalPeriodID,
				PeriodName:         current.PeriodName,
				CloseStatus:        current.CloseStatus,
				CloseLockedAt:      derefTime(current.CloseLockedAt),
				EvidenceDocumentID: derefString(current.EvidenceDocumentID),
			})
			return
		}
		h.log.Error("failed to lock fiscal period", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// Insert close evidence
	evidence := &domain.CloseEvidence{
		EvidenceID:       uuid.NewString(),
		TenantID:         tenantID,
		FiscalPeriodID:   id,
		TrialBalanceHash: trialBalanceHash,
		Signature:        signature,
		GeneratedAt:      now,
	}
	if err := h.store.CreateCloseEvidence(r.Context(), evidence); err != nil {
		h.log.Error("failed to write close evidence record", zap.Error(err))
		// Log warning only as the period itself has been successfully locked
	}

	h.publisher.PublishClosed(r.Context(), correlationID, *fp, docID)

	writeJSON(w, http.StatusOK, domain.PeriodLockResponse{
		FiscalPeriodID:     id,
		PeriodName:         fp.PeriodName,
		CloseStatus:        "LOCKED",
		CloseLockedAt:      now,
		EvidenceDocumentID: docID,
		VerificationHash:   trialBalanceHash,
	})
}

// ── Helpers ──────────────────────────────────────────────────────────────────────

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

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
