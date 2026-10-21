package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/consolidation-svc/internal/clients"
	"zoiko.io/consolidation-svc/internal/domain"
	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
)

type Store interface {
	CreateRun(ctx context.Context, run *domain.ConsolidationRun, actorID, correlationID string) error
	GetRun(ctx context.Context, id string) (*domain.ConsolidationRun, error)
	ListRuns(ctx context.Context, groupLegalEntityID string) ([]domain.ConsolidationRun, error)
	// CompleteRun enqueues the run's completion events (only for status
	// COMPLETED) in the same transaction as the status change.
	CompleteRun(ctx context.Context, id, status string, exceptionCount int, completedAt time.Time, actorID, correlationID string, snapshotCount int, exceptions []string) error
	CreateBalanceSnapshots(ctx context.Context, snapshots []domain.BalanceSnapshot) error
	ListSnapshotsByRun(ctx context.Context, runID string) ([]domain.BalanceSnapshot, error)
	CreateBalanceContributions(ctx context.Context, contributions []domain.BalanceContribution) error
	ListContributionsByRun(ctx context.Context, runID string) ([]domain.BalanceContribution, error)
	// CreateEliminations records a run's per-pair eliminations, all or none
	// (migration 000007, SPEC_DEVIATIONS B9).
	CreateEliminations(ctx context.Context, rows []domain.Elimination) error
	ListEliminationsByRun(ctx context.Context, runID string) ([]domain.Elimination, error)

	// BalanceContributionsPopulation serves the control-population contract (balance-contributions).
	BalanceContributionsPopulation(ctx context.Context, q domain.BalanceContributionsQuery) (*domain.ControlPopulationPage, error)

	// ACC-12 Elimination & Consolidation Adjustments — see migration
	// 000003's doc comment.
	GroupEntityHasRun(ctx context.Context, groupLegalEntityID string) (bool, error)
	HasSnapshotForPeriod(ctx context.Context, groupLegalEntityID, fiscalPeriod string) (bool, error)
	CreateAdjustment(ctx context.Context, a *domain.ConsolidationAdjustment, correlationID string) error
	GetAdjustment(ctx context.Context, id string) (*domain.ConsolidationAdjustment, error)
	GetAdjustmentHistory(ctx context.Context, id string) (*domain.AdjustmentHistory, error)
	ListAdjustments(ctx context.Context, groupLegalEntityID, fiscalPeriod string) ([]domain.ConsolidationAdjustment, error)
	ApproveAdjustment(ctx context.Context, id, principalID, correlationID string) error
	MarkAdjustmentPosted(ctx context.Context, id, principalID, journalID, correlationID string) error
	ReverseAdjustment(ctx context.Context, id, principalID, reason string, supersededBy *string, correlationID string) error
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type DomainClients interface {
	FetchTrialBalance(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (map[string]float64, error)
	FetchEliminableIntercompanyEntries(ctx context.Context, tenantID, principalID string) ([]clients.IntercompanyEntry, error)
	FetchReciprocalBalances(ctx context.Context, tenantID, principalID, entityA, entityB, fiscalPeriod string) (*domain.ReciprocalBalances, error)
	FetchJournal(ctx context.Context, tenantID, journalID string) (*domain.PostedJournal, error)

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
	store   Store
	authz   AuthZClient
	clients DomainClients
	log     *zap.Logger
}

func New(store Store, authz AuthZClient, clients DomainClients, log *zap.Logger) *Handler {
	return &Handler{
		store:   store,
		authz:   authz,
		clients: clients,
		log:     log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/consolidation/runs", func(r chi.Router) {
		r.Post("/", h.StartRun)
		r.Get("/", h.ListRuns)
		r.Get("/{id}", h.GetRun)
		r.Get("/{id}/snapshots", h.ListSnapshots)
		r.Get("/{id}/contributions", h.ListContributions)
		r.Get("/{id}/eliminations", h.ListRunEliminations)
	})
	r.Route("/v1/control-populations", func(r chi.Router) {
		r.Get("/{population}", h.GetControlPopulation)
	})
	r.Route("/v1/consolidation/adjustments", func(r chi.Router) {
		r.Post("/", h.CreateEliminationProposal)
		r.Get("/", h.ListAdjustments)
		r.Get("/{id}", h.GetAdjustment)
		r.Get("/{id}/history", h.GetAdjustmentHistory)
		r.Post("/{id}/approve", h.ApproveConsolidationAdjustment)
		r.Post("/{id}/post", h.PostConsolidationAdjustment)
		r.Post("/{id}/reverse", h.ReverseConsolidationAdjustment)
	})
	r.Route("/v1/consolidation/eliminations", func(r chi.Router) {
		r.Get("/explanation", h.ExplainElimination)
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
	accountCodes, err := domain.NormalizeAccountCodes(req.IntercompanyAccountCodes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_intercompany_account_codes", err.Error())
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

	if err := h.store.CreateRun(r.Context(), run, principalID, correlationID); err != nil {
		h.log.Error("failed to create consolidation run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

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
			_ = h.store.CompleteRun(r.Context(), runID, "FAILED", 1, time.Now().UTC(), principalID, correlationID, 0, nil)
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
		_ = h.store.CompleteRun(r.Context(), runID, "FAILED", 1, time.Now().UTC(), principalID, correlationID, 0, nil)
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "failed to record entity-to-group provenance: "+err.Error())
		return
	}

	// Step 2: Eliminate matched intercompany balances (MATCHED pairs, and
	// CLOSED ones, which were matched first) so a transaction between two
	// group entities isn't double-counted in the group total (A2/B9).
	// Only pairs in the run's scope are considered: both entities among
	// the run's children and the pair in the run's period. Of each such
	// pair, only the lines on the declared intercompany accounts are
	// subtracted, and only from legs whose lines the run actually summed.
	// A pair is eliminated whole or not at all, and every in-scope pair is
	// recorded as its own elimination row, ELIMINATED or BLOCKED. A BLOCKED
	// pair is a visible exception on the run.
	//
	// The population itself must be readable: a run that cannot list the
	// pairs cannot know what it failed to eliminate, so it fails rather
	// than completing with no elimination at all.
	eliminableEntries, err := h.clients.FetchEliminableIntercompanyEntries(r.Context(), tenantID, principalID)
	if err != nil {
		h.log.Error("failed to fetch intercompany entries for elimination", zap.Error(err))
		_ = h.store.CompleteRun(r.Context(), runID, "FAILED", 1, time.Now().UTC(), principalID, correlationID, 0, nil)
		writeError(w, http.StatusServiceUnavailable, "intercompany_unavailable", "failed to read the intercompany pairs to eliminate: "+err.Error())
		return
	}
	eliminations := h.eliminateInScope(r.Context(), tenantID, run, req.ChildLegalEntityIDs, accountCodes, eliminableEntries, consolidatedBalances, now)
	var eliminationExceptions []string
	for _, e := range eliminations {
		if e.Outcome == domain.EliminationOutcomeBlocked {
			h.log.Warn("intercompany pair not eliminated",
				zap.String("intercompany_entry_id", e.IntercompanyEntryID), zap.String("reason", *e.BlockedReason))
			eliminationExceptions = append(eliminationExceptions,
				fmt.Sprintf("intercompany entry %s: %s", e.IntercompanyEntryID, *e.BlockedReason))
		}
	}
	if err := h.store.CreateEliminations(r.Context(), eliminations); err != nil {
		// The elimination record is the run's evidence of what it
		// subtracted; balances without it would be unexplainable.
		h.log.Error("failed to record eliminations", zap.Error(err))
		_ = h.store.CompleteRun(r.Context(), runID, "FAILED", 1, time.Now().UTC(), principalID, correlationID, 0, nil)
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "failed to record eliminations: "+err.Error())
		return
	}

	// Step 3: Produce signed BalanceSnapshots
	balanceAccounts := make([]string, 0, len(consolidatedBalances))
	for code := range consolidatedBalances {
		balanceAccounts = append(balanceAccounts, code)
	}
	sort.Strings(balanceAccounts)

	snapshots := make([]domain.BalanceSnapshot, 0, len(balanceAccounts))
	for _, code := range balanceAccounts {
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
		_ = h.store.CompleteRun(r.Context(), runID, "FAILED", 1, time.Now().UTC(), principalID, correlationID, 0, nil)
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	completedAt := time.Now().UTC()
	exceptionCount := len(eliminationExceptions)
	if err := h.store.CompleteRun(r.Context(), runID, "COMPLETED", exceptionCount, completedAt, principalID, correlationID, len(snapshots), eliminationExceptions); err != nil {
		h.log.Error("failed to mark consolidation run completed", zap.Error(err))
	}

	writeJSON(w, http.StatusCreated, domain.ConsolidationRunResponse{
		ConsolidationRunID: runID,
		GroupLegalEntityID: req.GroupLegalEntityID,
		FiscalPeriod:       req.FiscalPeriod,
		Status:             "COMPLETED",
		ExceptionCount:     exceptionCount,
		StartedAt:          now,
		Snapshots:          snapshots,
		Eliminations:       eliminations,
	})
}

// floatAmount is the exact decimal a float64 line amount was most likely
// sent as: its shortest round-tripping decimal form ("100.1", not
// 100.099999999999994315658...).
func floatAmount(f float64) *big.Rat {
	r, _ := new(big.Rat).SetString(strconv.FormatFloat(f, 'f', -1, 64))
	return r
}

// eliminateInScope scopes the eliminable (MATCHED or CLOSED) pairs to the
// run, decides each in-scope pair's elimination (domain.PlanElimination),
// subtracts the eliminated lines' net (debit - credit, the sign
// FetchTrialBalance added them with) from balances in place, and returns one
// elimination row per in-scope pair. A pair outside the run's entities or
// period is neither eliminated nor recorded. A pair is decided completely
// before any of its lines touch balances, so a leg that cannot be read
// leaves the whole pair uneliminated (BLOCKED), never half eliminated.
func (h *Handler) eliminateInScope(ctx context.Context, tenantID string, run *domain.ConsolidationRun, children, accountCodes []string,
	entries []clients.IntercompanyEntry, balances map[string]float64, now time.Time) []domain.Elimination {
	entities := make(map[string]bool, len(children))
	for _, c := range children {
		entities[c] = true
	}

	journals := map[string]*domain.PostedJournal{}
	fetch := func(journalID string) (*domain.PostedJournal, error) {
		if j, ok := journals[journalID]; ok {
			return j, nil
		}
		j, err := h.clients.FetchJournal(ctx, tenantID, journalID)
		if err != nil {
			return nil, err
		}
		journals[journalID] = j
		return j, nil
	}

	out := []domain.Elimination{}
	for _, entry := range entries {
		basis := domain.PeriodBasisPair
		var blocked string
		switch domain.ScopePair(entry, entities, run.FiscalPeriod) {
		case domain.PairOutOfScope:
			continue
		case domain.PairNeedsSourcePeriod:
			src, err := fetch(entry.SourceJournalID)
			if err != nil {
				basis = domain.PeriodBasisUndetermined
				blocked = fmt.Sprintf("the pair records no fiscal_period and its source journal %s could not be read to find it: %v", entry.SourceJournalID, err)
				break
			}
			if src.FiscalPeriod != run.FiscalPeriod {
				continue
			}
			basis = domain.PeriodBasisSourceJournal
		}

		var lines []domain.EliminatedLine
		if blocked == "" {
			var source, target *domain.PostedJournal
			var err error
			if source, err = fetch(entry.SourceJournalID); err != nil {
				blocked = fmt.Sprintf("source journal %s could not be read: %v", entry.SourceJournalID, err)
			} else if entry.TargetJournalID != nil && *entry.TargetJournalID != "" {
				if target, err = fetch(*entry.TargetJournalID); err != nil {
					blocked = fmt.Sprintf("target journal %s could not be read: %v", *entry.TargetJournalID, err)
				}
			}
			if blocked == "" {
				lines, blocked = domain.PlanElimination(entry, source, target, accountCodes, run.FiscalPeriod)
			}
		}

		e := domain.Elimination{
			EliminationID:            uuid.NewString(),
			TenantID:                 tenantID,
			ConsolidationRunID:       run.ConsolidationRunID,
			IntercompanyEntryID:      entry.IntercompanyEntryID,
			SourceLegalEntityID:      entry.SourceLegalEntityID,
			TargetLegalEntityID:      entry.TargetLegalEntityID,
			FiscalPeriod:             run.FiscalPeriod,
			PeriodBasis:              basis,
			MatchStatus:              entry.MatchStatus,
			SourceJournalID:          entry.SourceJournalID,
			TargetJournalID:          entry.TargetJournalID,
			Amount:                   entry.Amount,
			CurrencyCode:             entry.CurrencyCode,
			CounterpartyAmount:       entry.CounterpartyAmount,
			CounterpartyCurrencyCode: entry.CounterpartyCurrencyCode,
			IntercompanyAccountCodes: accountCodes,
			Lines:                    []domain.EliminatedLine{},
			CreatedAt:                now,
		}
		if blocked != "" {
			e.Outcome = domain.EliminationOutcomeBlocked
			e.BlockedReason = &blocked
		} else {
			e.Outcome = domain.EliminationOutcomeEliminated
			e.Lines = lines
			for _, l := range lines {
				balances[l.AccountCode] -= l.Net()
			}
		}
		out = append(out, e)
	}
	return out
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
		eliminable, err := h.clients.FetchEliminableIntercompanyEntries(r.Context(), tenantID, principalID)
		if err != nil {
			h.log.Error("failed to fetch eliminable intercompany entries for elimination proposal", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "intercompany_unavailable", err.Error())
			return
		}
		// Summed exactly (ZS-DATA-001 D06). A malformed amount already
		// failed the fetch above; this is defence in depth, and refuses
		// rather than treating the entry as zero.
		amounts := make([]string, 0, len(eliminable))
		for _, e := range eliminable {
			amounts = append(amounts, string(e.Amount))
		}
		matchedTotal, err := domain.SumAmounts(amounts)
		if err != nil {
			h.log.Error("intercompany-accounting-svc returned an unreadable amount", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "intercompany_unavailable", err.Error())
			return
		}
		// Adjustment lines are still float64 in this service's own domain
		// (outside this change); each is read as its shortest decimal form
		// so the comparison itself is exact. Per-line larger-of semantics
		// are unchanged (SPEC_DEVIATIONS A5).
		proposedTotal := new(big.Rat)
		for _, l := range req.Lines {
			proposedTotal.Add(proposedTotal, floatAmount(max(l.DebitAmount, l.CreditAmount)))
		}
		if proposedTotal.Cmp(matchedTotal) > 0 {
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
	if err := h.store.CreateAdjustment(r.Context(), adjustment, getCorrelationID(r)); err != nil {
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

	if err := h.store.ApproveAdjustment(r.Context(), id, principalID, getCorrelationID(r)); err != nil {
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

	if err := h.store.MarkAdjustmentPosted(r.Context(), id, principalID, journalID, correlationID); err != nil {
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
	// generic 500 from the store. It must also actually replace this one:
	// a different adjustment, for the same group and period, not itself
	// reversed. Otherwise naming the adjustment itself, or any unrelated
	// one, would satisfy the supersession rule above while replacing nothing.
	if req.SupersededByAdjustmentID != nil && *req.SupersededByAdjustmentID != "" {
		successor, err := h.store.GetAdjustment(r.Context(), *req.SupersededByAdjustmentID)
		if err != nil {
			if errors.Is(err, domain.ErrAdjustmentNotFound) {
				writeError(w, http.StatusBadRequest, "invalid_field", "superseded_by_adjustment_id does not name a real adjustment")
				return
			}
			h.log.Error("failed to verify superseding adjustment", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
			return
		}
		if successor.ConsolidationAdjustmentID == a.ConsolidationAdjustmentID ||
			successor.GroupLegalEntityID != a.GroupLegalEntityID ||
			successor.FiscalPeriod != a.FiscalPeriod ||
			successor.Status == domain.AdjustmentStatusReversed {
			writeError(w, http.StatusUnprocessableEntity, "invalid_supersession", domain.ErrInvalidSupersession.Error())
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

	if err := h.store.ReverseAdjustment(r.Context(), id, principalID, req.Reason, req.SupersededByAdjustmentID, getCorrelationID(r)); err != nil {
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

// ── GET /v1/consolidation/adjustments/{id}/history ────────────────────────────────

// GetAdjustmentHistory is ACC-12's GetAdjustmentHistory query: a read-only
// lifecycle log, not a command. No Idempotency-Key is read. Authorization is
// on the adjustment's own group entity, the same view action GetAdjustment
// uses.
func (h *Handler) GetAdjustmentHistory(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	history, err := h.store.GetAdjustmentHistory(r.Context(), id)
	if errors.Is(err, domain.ErrAdjustmentNotFound) {
		writeError(w, http.StatusNotFound, "adjustment_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch consolidation adjustment history", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, history.GroupLegalEntityID, actionAdjustmentView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, history)
}

// ── GET /v1/consolidation/eliminations/explanation ────────────────────────────────

// ExplainElimination is ACC-12's ExplainElimination query: why a pair is or
// is not eliminable, the rule that decided it, and the reciprocal balance it
// was checked against. The balance comes from intercompany-accounting-svc's
// GetReciprocalBalances (ACC-11 A1) rather than being re-derived from the
// entry list here. Read-only: scoped read plus auth context, no
// Idempotency-Key.
func (h *Handler) ExplainElimination(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	groupLegalEntityID := q.Get("group_legal_entity_id")
	entityA := q.Get("entity_a")
	entityB := q.Get("entity_b")
	fiscalPeriod := q.Get("fiscal_period")

	if runID := q.Get("consolidation_run_id"); runID != "" {
		h.explainRunElimination(w, r, runID, groupLegalEntityID, entityA, entityB, fiscalPeriod)
		return
	}

	if groupLegalEntityID == "" || entityA == "" || entityB == "" || fiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"group_legal_entity_id, entity_a, entity_b and fiscal_period are required")
		return
	}
	if entityA == entityB {
		writeError(w, http.StatusBadRequest, "same_entity_forbidden",
			"entity_a and entity_b must be different legal entities")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, groupLegalEntityID, actionRunView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	balances, err := h.clients.FetchReciprocalBalances(r.Context(), tenantID, principalID, entityA, entityB, fiscalPeriod)
	if err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			h.writeAuthzErr(w, err)
			return
		}
		h.log.Error("failed to fetch reciprocal balances for elimination explanation",
			zap.String("entity_a", entityA), zap.String("entity_b", entityB),
			zap.String("fiscal_period", fiscalPeriod), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "intercompany_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK,
		domain.BuildEliminationExplanation(groupLegalEntityID, entityA, entityB, fiscalPeriod, balances))
}

// explainRunElimination answers ExplainElimination for one run from the
// elimination rows that run recorded (B9), not from the pair's current
// reciprocal balance. The run fixes the group and the period; when the
// caller also names them they must be the run's.
func (h *Handler) explainRunElimination(w http.ResponseWriter, r *http.Request, runID, groupLegalEntityID, entityA, entityB, fiscalPeriod string) {
	if entityA == "" || entityB == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "entity_a and entity_b are required")
		return
	}
	if entityA == entityB {
		writeError(w, http.StatusBadRequest, "same_entity_forbidden",
			"entity_a and entity_b must be different legal entities")
		return
	}
	run, rows, ok := h.runEliminations(w, r, runID)
	if !ok {
		return
	}
	if (groupLegalEntityID != "" && groupLegalEntityID != run.GroupLegalEntityID) ||
		(fiscalPeriod != "" && fiscalPeriod != run.FiscalPeriod) {
		writeError(w, http.StatusBadRequest, "run_scope_mismatch",
			"group_legal_entity_id and fiscal_period, when given, must be the run's")
		return
	}
	writeJSON(w, http.StatusOK, domain.BuildRunEliminationExplanation(run, entityA, entityB, rows))
}

// ── GET /v1/consolidation/runs/{id}/eliminations ─────────────────────────────────

// ListRunEliminations is ListEliminations for one run: every elimination row
// the run recorded, ELIMINATED and BLOCKED.
func (h *Handler) ListRunEliminations(w http.ResponseWriter, r *http.Request) {
	if _, rows, ok := h.runEliminations(w, r, chi.URLParam(r, "id")); ok {
		writeJSON(w, http.StatusOK, rows)
	}
}

// runEliminations loads a run, authorizes CONSOLIDATION_RUN_VIEW on its
// group, and reads its elimination rows.
func (h *Handler) runEliminations(w http.ResponseWriter, r *http.Request, runID string) (*domain.ConsolidationRun, []domain.Elimination, bool) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, nil, false
	}
	run, err := h.store.GetRun(r.Context(), runID)
	if errors.Is(err, domain.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, "run_not_found", "")
		return nil, nil, false
	}
	if err != nil {
		h.log.Error("failed to fetch consolidation run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return nil, nil, false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.GroupLegalEntityID, actionRunView); err != nil {
		h.writeAuthzErr(w, err)
		return nil, nil, false
	}
	rows, err := h.store.ListEliminationsByRun(r.Context(), run.ConsolidationRunID)
	if err != nil {
		h.log.Error("failed to list eliminations", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return nil, nil, false
	}
	if rows == nil {
		rows = []domain.Elimination{}
	}
	return run, rows, true
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
