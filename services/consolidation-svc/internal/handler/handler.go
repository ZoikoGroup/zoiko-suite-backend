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

	// ACC-12 Elimination & Consolidation Adjustments — see migration
	// 000003's doc comment.
	GroupEntityHasRun(ctx context.Context, groupLegalEntityID string) (bool, error)
	HasSnapshotForPeriod(ctx context.Context, groupLegalEntityID, fiscalPeriod string) (bool, error)
	CreateAdjustment(ctx context.Context, a *domain.ConsolidationAdjustment) error
	GetAdjustment(ctx context.Context, id string) (*domain.ConsolidationAdjustment, error)
	ListAdjustments(ctx context.Context, groupLegalEntityID, fiscalPeriod string) ([]domain.ConsolidationAdjustment, error)
	ApproveAdjustment(ctx context.Context, id, principalID string) error
	MarkAdjustmentPosted(ctx context.Context, id, principalID, journalID string) error
	ReverseAdjustment(ctx context.Context, id, principalID, reason string, supersededBy *string) error
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

	// ACC-12's own real "ACC-04/05 consolidation book" dependency.
	PostConsolidationAdjustmentJournal(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod, description, sourceEventID, correlationID string, lines []clients.JournalLine) (journalID string, err error)
	ReverseConsolidationAdjustmentJournal(ctx context.Context, tenantID, principalID, journalID, reason string) error
}

const (
	actionRunInitiate = "CONSOLIDATION_RUN_INITIATE"
	actionRunView     = "CONSOLIDATION_RUN_VIEW"

	// ACC-12 lifecycle actions. actionAdjustmentApprove is deliberately
	// distinct from actionAdjustmentCreate — the spec's own negative path
	// "Top-side journal self-approved" presumes maker/checker is even
	// possible to enforce at the authorization layer, which collapsing
	// the two into one grantable action would prevent, the same reasoning
	// ACC-03's GL_JOURNAL_SUBMIT/GL_JOURNAL_APPROVE split already
	// established in this platform.
	actionAdjustmentCreate  = "CONSOLIDATION_ADJUSTMENT_CREATE"
	actionAdjustmentView    = "CONSOLIDATION_ADJUSTMENT_VIEW"
	actionAdjustmentApprove = "CONSOLIDATION_ADJUSTMENT_APPROVE"
	actionAdjustmentPost    = "CONSOLIDATION_ADJUSTMENT_POST"
	actionAdjustmentReverse = "CONSOLIDATION_ADJUSTMENT_REVERSE"
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
	r.Route("/v1/consolidation/adjustments", func(r chi.Router) {
		r.Post("/", h.CreateEliminationProposal)
		r.Get("/", h.ListAdjustments)
		r.Get("/{id}", h.GetAdjustment)
		r.Post("/{id}/approve", h.ApproveConsolidationAdjustment)
		r.Post("/{id}/post", h.PostConsolidationAdjustment)
		r.Post("/{id}/reverse", h.ReverseConsolidationAdjustment)
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

// ── POST /v1/consolidation/adjustments ────────────────────────────────────────────
//
// ACC-12 Elimination & Consolidation Adjustments. See migration 000003's
// doc comment for how this differs from StartRun's own automatic
// elimination step.

func (h *Handler) CreateEliminationProposal(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateEliminationProposalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.GroupLegalEntityID == "" || req.FiscalPeriod == "" || len(req.Lines) == 0 {
		writeError(w, http.StatusBadRequest, "missing_fields", "group_legal_entity_id, fiscal_period and at least one line are required")
		return
	}
	if req.AdjustmentType != domain.AdjustmentTypeElimination && req.AdjustmentType != domain.AdjustmentTypeManual {
		writeError(w, http.StatusBadRequest, "invalid_adjustment_type", "adjustment_type must be ELIMINATION or MANUAL")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.GroupLegalEntityID, actionAdjustmentCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Negative path: "Adjustment targets statutory book." A consolidation
	// adjustment may only target an entity this tenant has actually run a
	// real consolidation FOR as the group entity — never an arbitrary
	// legal entity a caller could name, which would let an "adjustment"
	// quietly rewrite a child's own statutory ledger instead of the
	// consolidation-only book.
	hasRun, err := h.store.GroupEntityHasRun(r.Context(), req.GroupLegalEntityID)
	if err != nil {
		h.log.Error("failed to verify group entity", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if !hasRun {
		writeError(w, http.StatusUnprocessableEntity, "targets_statutory_book", domain.ErrAdjustmentTargetsStatutoryBook.Error())
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())

	// Negative path: "Elimination exceeds matched reciprocal balance." Only
	// checked for ELIMINATION-type adjustments — a MANUAL top-side
	// adjustment (e.g. a consolidation-only reclass) has no matched
	// intercompany balance to be bounded by.
	if req.AdjustmentType == domain.AdjustmentTypeElimination {
		matched, err := h.clients.FetchMatchedIntercompanyEntries(r.Context(), tenantID, principalID)
		if err != nil {
			h.log.Error("failed to fetch matched intercompany entries for elimination proposal", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "intercompany_unavailable", err.Error())
			return
		}
		var matchedTotal float64
		for _, e := range matched {
			matchedTotal += e.Amount
		}
		var proposedTotal float64
		for _, l := range req.Lines {
			if l.DebitAmount > l.CreditAmount {
				proposedTotal += l.DebitAmount
			} else {
				proposedTotal += l.CreditAmount
			}
		}
		if proposedTotal > matchedTotal {
			writeError(w, http.StatusUnprocessableEntity, "elimination_exceeds_matched_balance", domain.ErrEliminationExceedsMatchedBalance.Error())
			return
		}
	}

	now := time.Now().UTC()
	adjustment := &domain.ConsolidationAdjustment{
		ConsolidationAdjustmentID: uuid.NewString(),
		TenantID:                  tenantID,
		GroupLegalEntityID:        req.GroupLegalEntityID,
		FiscalPeriod:              req.FiscalPeriod,
		AdjustmentType:            req.AdjustmentType,
		Description:               req.Description,
		// No separate Submit command is named anywhere in the wireframe —
		// see migration 000003's doc comment — so this lands directly in
		// PENDING_APPROVAL rather than an unreachable DRAFT.
		Status:               domain.AdjustmentStatusPendingApproval,
		Lines:                req.Lines,
		CreatedAt:            now,
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateAdjustment(r.Context(), adjustment); err != nil {
		h.log.Error("failed to create consolidation adjustment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, adjustment)
}

// ── GET /v1/consolidation/adjustments ─────────────────────────────────────────────

func (h *Handler) ListAdjustments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	groupLegalEntityID := q.Get("group_legal_entity_id")
	fiscalPeriod := q.Get("fiscal_period")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if groupLegalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, groupLegalEntityID, actionAdjustmentView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListAdjustments(r.Context(), groupLegalEntityID, fiscalPeriod)
	if err != nil {
		h.log.Error("failed to list consolidation adjustments", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.ConsolidationAdjustment{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/consolidation/adjustments/{id} ────────────────────────────────────────

func (h *Handler) GetAdjustment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, err := h.store.GetAdjustment(r.Context(), id)
	if errors.Is(err, domain.ErrAdjustmentNotFound) {
		writeError(w, http.StatusNotFound, "adjustment_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch consolidation adjustment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, a.GroupLegalEntityID, actionAdjustmentView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// ── POST /v1/consolidation/adjustments/{id}/approve ───────────────────────────────

func (h *Handler) ApproveConsolidationAdjustment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, err := h.store.GetAdjustment(r.Context(), id)
	if errors.Is(err, domain.ErrAdjustmentNotFound) {
		writeError(w, http.StatusNotFound, "adjustment_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch consolidation adjustment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// Negative path: "Top-side journal self-approved." The same
	// maker/checker posture ACC-03 applies to journal approval.
	if a.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_permitted", domain.ErrSelfApprovalNotPermitted.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, a.GroupLegalEntityID, actionAdjustmentApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.ApproveAdjustment(r.Context(), id, principalID); err != nil {
		if errors.Is(err, domain.ErrInvalidAdjustmentTransition) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
			return
		}
		h.log.Error("failed to approve consolidation adjustment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": domain.AdjustmentStatusApproved})
}

// ── POST /v1/consolidation/adjustments/{id}/post ──────────────────────────────────

func (h *Handler) PostConsolidationAdjustment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	correlationID := getCorrelationID(r)
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, err := h.store.GetAdjustment(r.Context(), id)
	if errors.Is(err, domain.ErrAdjustmentNotFound) {
		writeError(w, http.StatusNotFound, "adjustment_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch consolidation adjustment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if a.Status != domain.AdjustmentStatusApproved {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidAdjustmentTransition.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, a.GroupLegalEntityID, actionAdjustmentPost); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	lines := make([]clients.JournalLine, len(a.Lines))
	for i, l := range a.Lines {
		lines[i] = clients.JournalLine{AccountCode: l.AccountCode, DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount}
	}
	journalID, err := h.clients.PostConsolidationAdjustmentJournal(
		r.Context(), tenantID, principalID, a.GroupLegalEntityID, a.FiscalPeriod, a.Description,
		a.ConsolidationAdjustmentID, correlationID, lines,
	)
	if err != nil {
		h.log.Error("failed to post consolidation adjustment journal", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "gl_post_failed", err.Error())
		return
	}

	if err := h.store.MarkAdjustmentPosted(r.Context(), id, principalID, journalID); err != nil {
		h.log.Error("consolidation adjustment journal posted but the adjustment could not be marked POSTED",
			zap.String("consolidation_adjustment_id", id), zap.String("journal_id", journalID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "adjustment_not_recorded",
			"the journal IS posted ("+journalID+"), but the adjustment could not be marked POSTED.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": domain.AdjustmentStatusPosted, "consolidation_book_journal_id": journalID})
}

// ── POST /v1/consolidation/adjustments/{id}/reverse ───────────────────────────────

func (h *Handler) ReverseConsolidationAdjustment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.ReverseConsolidationAdjustmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrReasonRequired.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, err := h.store.GetAdjustment(r.Context(), id)
	if errors.Is(err, domain.ErrAdjustmentNotFound) {
		writeError(w, http.StatusNotFound, "adjustment_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch consolidation adjustment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if a.Status != domain.AdjustmentStatusPosted {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidAdjustmentTransition.Error())
		return
	}

	// Negative path: "Reverse after snapshot without supersession." Once a
	// real BalanceSnapshot exists for this group/period, a bare reversal
	// would silently change a number a report may already have been
	// generated from — a superseding replacement must be named instead.
	hasSnapshot, err := h.store.HasSnapshotForPeriod(r.Context(), a.GroupLegalEntityID, a.FiscalPeriod)
	if err != nil {
		h.log.Error("failed to check for an existing snapshot", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if hasSnapshot && (req.SupersededByAdjustmentID == nil || *req.SupersededByAdjustmentID == "") {
		writeError(w, http.StatusUnprocessableEntity, "reversal_requires_supersession", domain.ErrReversalRequiresSupersession.Error())
		return
	}
	// A named supersession must be a real adjustment — checked here so a
	// bad id is a clean 400, not a foreign-key violation surfacing as a
	// generic 500 from the store.
	if req.SupersededByAdjustmentID != nil && *req.SupersededByAdjustmentID != "" {
		if _, err := h.store.GetAdjustment(r.Context(), *req.SupersededByAdjustmentID); err != nil {
			if errors.Is(err, domain.ErrAdjustmentNotFound) {
				writeError(w, http.StatusBadRequest, "invalid_field", "superseded_by_adjustment_id does not name a real adjustment")
				return
			}
			h.log.Error("failed to verify superseding adjustment", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
			return
		}
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, a.GroupLegalEntityID, actionAdjustmentReverse); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if a.ConsolidationBookJournalID != nil {
		if err := h.clients.ReverseConsolidationAdjustmentJournal(r.Context(), tenantID, principalID, *a.ConsolidationBookJournalID, req.Reason); err != nil {
			h.log.Error("failed to reverse consolidation adjustment journal", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "gl_reverse_failed", err.Error())
			return
		}
	}

	if err := h.store.ReverseAdjustment(r.Context(), id, principalID, req.Reason, req.SupersededByAdjustmentID); err != nil {
		if errors.Is(err, domain.ErrInvalidAdjustmentTransition) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
			return
		}
		h.log.Error("consolidation adjustment journal reversed but the adjustment could not be marked REVERSED", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "adjustment_not_recorded", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": domain.AdjustmentStatusReversed})
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
