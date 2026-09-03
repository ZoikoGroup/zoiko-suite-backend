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
	CreateBalanceContributions(ctx context.Context, contributions []domain.BalanceContribution) error
	ListContributionsByRun(ctx context.Context, runID string) ([]domain.BalanceContribution, error)
}

type Publisher interface {
	PublishRunStarted(ctx context.Context, correlationID, actorID string, run domain.ConsolidationRun)
	PublishCompleted(ctx context.Context, correlationID, actorID string, run domain.ConsolidationRun, snapshotCount int)
	PublishExceptionDetected(ctx context.Context, correlationID, actorID string, run domain.ConsolidationRun, exceptions []string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type DomainClients interface {
	FetchTrialBalance(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (map[string]float64, error)
	FetchMatchedIntercompanyEntries(ctx context.Context, tenantID, principalID string) ([]clients.IntercompanyEntry, error)
	FetchJournalLines(ctx context.Context, tenantID, journalID string) ([]clients.JournalLine, error)
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
		r.Get("/{id}/contributions", h.ListContributions)
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

	h.publisher.PublishRunStarted(r.Context(), correlationID, principalID, *run)

	// Step 1: Query GL Trial Balances across all child legal entities.
	// Each child's own contribution to each account is recorded (ACC-13
	// entity-to-group provenance) BEFORE elimination touches
	// consolidatedBalances — this is exactly what that entity's trial
	// balance reported, not the post-elimination group figure.
	consolidatedBalances := make(map[string]float64)
	var contributions []domain.BalanceContribution
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
			contributions = append(contributions, domain.BalanceContribution{
				BalanceContributionID: uuid.NewString(),
				ConsolidationRunID:    runID,
				AccountCode:           accountCode,
				SourceLegalEntityID:   childID,
				GrossAmount:           amount,
				GeneratedAt:           now,
			})
		}
	}
	if err := h.store.CreateBalanceContributions(r.Context(), contributions); err != nil {
		// Provenance is the whole point of this fix (see
		// master-register-findings-2026-08-27.md §3.31) — a run that
		// completes without it recorded would silently repeat the same
		// "claimed but not actually done" shape §3.29 already fixed once.
		h.log.Error("failed to record balance contributions", zap.Error(err))
		_ = h.store.CompleteRun(r.Context(), runID, "FAILED", 1, time.Now().UTC())
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "failed to record entity-to-group provenance: "+err.Error())
		return
	}

	// Step 2: Eliminate matched intercompany balances so a transaction
	// between two group entities isn't double-counted in the group total.
	// Elimination reverses the REAL posted net contribution (debit-credit)
	// of both legs' actual journal lines — never a guessed elimination
	// account, since no intercompany-to-account mapping exists anywhere on
	// this platform (IntercompanyEntry itself carries no account_code).
	// A leg that can't be fetched is a real, visible exception on the run
	// — never a silent warning a caller of this API would never see (this
	// replaces a prior version of this step that fetched matched entries
	// and then discarded the result entirely; see
	// master-register-findings-2026-08-27.md §3.29).
	var eliminationExceptions []string
	matchedEntries, err := h.clients.FetchMatchedIntercompanyEntries(r.Context(), tenantID, principalID)
	if err != nil {
		h.log.Error("failed to fetch intercompany entries for elimination", zap.Error(err))
		eliminationExceptions = append(eliminationExceptions, "intercompany entries unavailable: "+err.Error())
	} else {
		for _, entry := range matchedEntries {
			if elimErr := h.eliminateMatchedEntry(r.Context(), tenantID, entry, consolidatedBalances); elimErr != nil {
				h.log.Error("failed to eliminate matched intercompany entry",
					zap.String("intercompany_entry_id", entry.IntercompanyEntryID), zap.Error(elimErr))
				eliminationExceptions = append(eliminationExceptions,
					fmt.Sprintf("intercompany entry %s: %s", entry.IntercompanyEntryID, elimErr.Error()))
			}
		}
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
	exceptionCount := len(eliminationExceptions)
	if err := h.store.CompleteRun(r.Context(), runID, "COMPLETED", exceptionCount, completedAt); err != nil {
		h.log.Error("failed to mark consolidation run completed", zap.Error(err))
	}

	run.Status = "COMPLETED"
	run.CompletedAt = &completedAt
	run.ExceptionCount = exceptionCount

	if exceptionCount > 0 {
		// Visible, not silent: a run whose elimination step partially or
		// fully failed still completes (the balances it DID compute are
		// real), but callers must be able to see that some intercompany
		// legs were not eliminated, not infer it from a suspiciously round
		// group total.
		h.publisher.PublishExceptionDetected(r.Context(), correlationID, principalID, *run, eliminationExceptions)
	}
	h.publisher.PublishCompleted(r.Context(), correlationID, principalID, *run, len(snapshots))

	writeJSON(w, http.StatusCreated, domain.ConsolidationRunResponse{
		ConsolidationRunID: runID,
		GroupLegalEntityID: req.GroupLegalEntityID,
		FiscalPeriod:       req.FiscalPeriod,
		Status:             "COMPLETED",
		ExceptionCount:     exceptionCount,
		StartedAt:          now,
		Snapshots:          snapshots,
	})
}

// eliminateMatchedEntry subtracts entry's real, posted net contribution
// (debit - credit, both legs) from balances in place — the same sign
// convention FetchTrialBalance itself uses when building consolidatedBalances,
// so eliminating a leg here exactly reverses what it originally added.
func (h *Handler) eliminateMatchedEntry(ctx context.Context, tenantID string, entry clients.IntercompanyEntry, balances map[string]float64) error {
	sourceLines, err := h.clients.FetchJournalLines(ctx, tenantID, entry.SourceJournalID)
	if err != nil {
		return fmt.Errorf("source journal %s: %w", entry.SourceJournalID, err)
	}
	for _, l := range sourceLines {
		balances[l.AccountCode] -= l.DebitAmount - l.CreditAmount
	}

	if entry.TargetJournalID == nil || *entry.TargetJournalID == "" {
		// MatchEntry requires a target_journal_id to reach MATCHED status
		// (see intercompany-accounting-svc's MatchEntryRequest) — a MATCHED
		// entry with no target journal would itself be a defect in that
		// service, not something to silently tolerate here.
		return fmt.Errorf("matched entry has no target_journal_id")
	}
	targetLines, err := h.clients.FetchJournalLines(ctx, tenantID, *entry.TargetJournalID)
	if err != nil {
		return fmt.Errorf("target journal %s: %w", *entry.TargetJournalID, err)
	}
	for _, l := range targetLines {
		balances[l.AccountCode] -= l.DebitAmount - l.CreditAmount
	}
	return nil
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

// ListContributions answers ACC-13's entity-to-group provenance question
// directly: which child entities' balances actually summed into this run's
// group-level numbers, before elimination. Same authz posture as
// ListSnapshots — a run's provenance is exactly as sensitive as its result.
func (h *Handler) ListContributions(w http.ResponseWriter, r *http.Request) {
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

	contributions, err := h.store.ListContributionsByRun(r.Context(), id)
	if err != nil {
		h.log.Error("failed to list balance contributions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if contributions == nil {
		contributions = []domain.BalanceContribution{}
	}
	writeJSON(w, http.StatusOK, contributions)
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
