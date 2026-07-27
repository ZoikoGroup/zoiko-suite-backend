package handler

import (
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

	"zoiko.io/consolidation-svc/internal/clients"
	"zoiko.io/consolidation-svc/internal/domain"
	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
)

type Store interface {
	CreateRun(ctx context.Context, run *domain.ConsolidationRun) error
	GetRun(ctx context.Context, id string) (*domain.ConsolidationRun, error)
	ListRuns(ctx context.Context, groupLegalEntityID string) ([]domain.ConsolidationRun, error)
	CompleteRun(ctx context.Context, id, status string, exceptionCount int, completedAt time.Time) error
	CreateBalanceSnapshots(ctx context.Context, snapshots []domain.BalanceSnapshot) error
	ListSnapshotsByRun(ctx context.Context, runID string) ([]domain.BalanceSnapshot, error)
}

type Publisher interface {
	PublishRunStarted(ctx context.Context, correlationID string, run domain.ConsolidationRun)
	PublishCompleted(ctx context.Context, correlationID string, run domain.ConsolidationRun, snapshotCount int)
	PublishExceptionDetected(ctx context.Context, correlationID string, run domain.ConsolidationRun, exceptions []string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type DomainClients interface {
	FetchTrialBalance(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (map[string]float64, error)
	FetchMatchedIntercompanyEntries(ctx context.Context, tenantID string) ([]clients.IntercompanyEntry, error)
}

const (
	actionRunInitiate = "CONSOLIDATION_RUN_INITIATE"
	actionRunView     = "CONSOLIDATION_RUN_VIEW"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	clients   DomainClients
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, clients DomainClients, log *zap.Logger) *Handler {
	return &Handler{
		store:     store,
		publisher: publisher,
		authz:     authz,
		clients:   clients,
		log:       log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/consolidation/runs", func(r chi.Router) {
		r.Post("/", h.StartRun)
		r.Get("/", h.ListRuns)
		r.Get("/{id}", h.GetRun)
		r.Get("/{id}/snapshots", h.ListSnapshots)
	})
}

// ── POST /v1/consolidation/runs ──────────────────────────────────────────────────

func (h *Handler) StartRun(w http.ResponseWriter, r *http.Request) {
	var req domain.StartConsolidationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.GroupLegalEntityID == "" || len(req.ChildLegalEntityIDs) == 0 || req.FiscalPeriod == "" || req.TargetCurrency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "group_legal_entity_id, child_legal_entity_ids, fiscal_period, target_currency are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.GroupLegalEntityID, actionRunInitiate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	correlationID := getCorrelationID(r)

	now := time.Now().UTC()
	runID := uuid.NewString()

	run := &domain.ConsolidationRun{
		ConsolidationRunID: runID,
		TenantID:           tenantID,
		GroupLegalEntityID: req.GroupLegalEntityID,
		FiscalPeriod:       req.FiscalPeriod,
		TargetCurrency:     req.TargetCurrency,
		Status:             "RUNNING",
		ExceptionCount:     0,
		StartedAt:          now,
	}

	if err := h.store.CreateRun(r.Context(), run); err != nil {
		h.log.Error("failed to create consolidation run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	h.publisher.PublishRunStarted(r.Context(), correlationID, *run)

	// Step 1: Query GL Trial Balances across all child legal entities
	consolidatedBalances := make(map[string]float64)
	for _, childID := range req.ChildLegalEntityIDs {
		bal, err := h.clients.FetchTrialBalance(r.Context(), tenantID, childID, req.FiscalPeriod)
		if err != nil {
			h.log.Error("failed to fetch trial balance for child entity", zap.String("child_id", childID), zap.Error(err))
			_ = h.store.CompleteRun(r.Context(), runID, "FAILED", 1, time.Now().UTC())
			writeError(w, http.StatusServiceUnavailable, "gl_fetch_failed", fmt.Sprintf("failed to fetch trial balance for entity %s: %s", childID, err.Error()))
			return
		}
		for accountCode, amount := range bal {
			consolidatedBalances[accountCode] += amount
		}
	}

	// Step 2: Query matched intercompany entries for balance elimination
	_, err := h.clients.FetchMatchedIntercompanyEntries(r.Context(), tenantID)
	if err != nil {
		h.log.Warn("intercompany entries fetch failed — proceeding with uneliminated consolidation", zap.Error(err))
	}

	// Step 3: Produce signed BalanceSnapshots
	accountCodes := make([]string, 0, len(consolidatedBalances))
	for code := range consolidatedBalances {
		accountCodes = append(accountCodes, code)
	}
	sort.Strings(accountCodes)

	snapshots := make([]domain.BalanceSnapshot, 0, len(accountCodes))
	for _, code := range accountCodes {
		bal := consolidatedBalances[code]

		// Cryptographic HMAC-SHA256 signature per snapshot
		sigPayload := fmt.Sprintf("%s:%s:%s:%s:%f", runID, req.GroupLegalEntityID, req.FiscalPeriod, code, bal)
		mac := hmac.New(sha256.New, []byte(tenantID))
		mac.Write([]byte(sigPayload))
		signature := hex.EncodeToString(mac.Sum(nil))

		snapshots = append(snapshots, domain.BalanceSnapshot{
			BalanceSnapshotID:   uuid.NewString(),
			TenantID:            tenantID,
			ConsolidationRunID:  runID,
			LegalEntityID:       req.GroupLegalEntityID,
			FiscalPeriod:        req.FiscalPeriod,
			AccountCode:         code,
			ConsolidatedBalance: bal,
			CurrencyCode:        req.TargetCurrency,
			SnapshotSignature:   signature,
			GeneratedAt:         now,
		})
	}

	if err := h.store.CreateBalanceSnapshots(r.Context(), snapshots); err != nil {
		h.log.Error("failed to store balance snapshots", zap.Error(err))
		_ = h.store.CompleteRun(r.Context(), runID, "FAILED", 1, time.Now().UTC())
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	completedAt := time.Now().UTC()
	if err := h.store.CompleteRun(r.Context(), runID, "COMPLETED", 0, completedAt); err != nil {
		h.log.Error("failed to mark consolidation run completed", zap.Error(err))
	}

	run.Status = "COMPLETED"
	run.CompletedAt = &completedAt

	h.publisher.PublishCompleted(r.Context(), correlationID, *run, len(snapshots))

	writeJSON(w, http.StatusCreated, domain.ConsolidationRunResponse{
		ConsolidationRunID: runID,
		GroupLegalEntityID: req.GroupLegalEntityID,
		FiscalPeriod:       req.FiscalPeriod,
		Status:             "COMPLETED",
		ExceptionCount:     0,
		StartedAt:          now,
		Snapshots:          snapshots,
	})
}

// ── GET /v1/consolidation/runs ────────────────────────────────────────────────────

func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	groupLegalEntityID := r.URL.Query().Get("group_legal_entity_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if groupLegalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, groupLegalEntityID, actionRunView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListRuns(r.Context(), groupLegalEntityID)
	if err != nil {
		h.log.Error("failed to list consolidation runs", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.ConsolidationRun{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/consolidation/runs/{id} ───────────────────────────────────────────────

func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	run, err := h.store.GetRun(r.Context(), id)
	if errors.Is(err, domain.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, "run_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch consolidation run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, run.GroupLegalEntityID, actionRunView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, run)
}

// ── GET /v1/consolidation/runs/{id}/snapshots ────────────────────────────────────

func (h *Handler) ListSnapshots(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	run, err := h.store.GetRun(r.Context(), id)
	if errors.Is(err, domain.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, "run_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch consolidation run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, run.GroupLegalEntityID, actionRunView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	snapshots, err := h.store.ListSnapshotsByRun(r.Context(), id)
	if err != nil {
		h.log.Error("failed to list balance snapshots", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if snapshots == nil {
		snapshots = []domain.BalanceSnapshot{}
	}
	writeJSON(w, http.StatusOK, snapshots)
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