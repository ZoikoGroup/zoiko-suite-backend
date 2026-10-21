package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	"zoiko.io/intercompany-accounting-svc/internal/entityregistry"
	"zoiko.io/intercompany-accounting-svc/internal/ledger"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
)

type Store interface {
	// CreateEntryWithAttempt inserts a new pair and, in the same
	// transaction, the create-time source-verification attempt when the
	// pair is created directly in MISMATCH (SPEC_DEVIATIONS A6), its
	// creation transition and the "created" lifecycle event. attempt is nil
	// for a pair created UNMATCHED. A MISMATCH pair without an attempt is
	// refused: the reason would have no evidence.
	CreateEntryWithAttempt(ctx context.Context, entry *domain.IntercompanyEntry, attempt *domain.MatchAttempt, actorID, correlationID string) (created bool, err error)
	// GetEntryBySourceJournal resolves the idempotent-create key before the
	// source journal is verified, so a retried create returns the original
	// pair without re-querying general-ledger-svc or adding a second
	// attempt.
	GetEntryBySourceJournal(ctx context.Context, sourceJournalID string) (*domain.IntercompanyEntry, error)
	GetEntry(ctx context.Context, id string) (*domain.IntercompanyEntry, error)
	ListEntries(ctx context.Context, sourceEntityID, targetEntityID string) ([]domain.IntercompanyEntry, error)
	// ReciprocalBalances is GetReciprocalBalances' read (SPEC_DEVIATIONS A1):
	// the period's exact per-currency totals between one pair of legal
	// entities, grouped by direction, currency and match status.
	ReciprocalBalances(ctx context.Context, entityA, entityB, period string) ([]domain.ReciprocalBalanceGroup, error)
	// UpdateMatch persists entry's target state and, in the same
	// transaction, the attempt that produced it and the "matched" (legacy
	// "posted") or "mismatch-detected" event that state implies.
	UpdateMatch(ctx context.Context, entry *domain.IntercompanyEntry, attempt *domain.MatchAttempt, actorID, correlationID string) error

	// ACC-11 lifecycle — see migrations 000003 and 000006. Each records its
	// state change in the pair's append-only transition history.
	AcknowledgeCounterparty(ctx context.Context, id, principalID, correlationID string) error
	DisputeIntercompany(ctx context.Context, id, principalID, reason, correlationID string) error
	ResolveMismatch(ctx context.Context, id, principalID, resolutionNote, outcome, correlationID string) error
	ClosePair(ctx context.Context, id, principalID, reason, correlationID string) error
	ListTransitions(ctx context.Context, id string) ([]domain.PairTransition, error)

	// ListMatchAttempts is ExplainMismatch's evidence (migration 000007).
	ListMatchAttempts(ctx context.Context, id string) ([]domain.MatchAttempt, error)

	// Control-population read (docs/architecture/control-population-contract.md).
	ControlPopulation(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type LedgerClient interface {
	GetJournal(ctx context.Context, tenantID, journalID string) (*ledger.JournalDetail, error)
}

// EntityRegistryClient is ACC-11's own closure of "Entity loses group
// relationship mid-period" — see internal/entityregistry's doc comment.
type EntityRegistryClient interface {
	ListHierarchies(ctx context.Context, tenantID, legalEntityID string) ([]entityregistry.Hierarchy, error)
}

const (
	actionCreateEntry = "INTERCOMPANY_ENTRY_CREATE"
	actionViewEntry   = "INTERCOMPANY_ENTRY_VIEW"
	actionMatchEntry  = "INTERCOMPANY_ENTRY_MATCH"

	// ACC-11 lifecycle actions. actionResolveMismatch is deliberately
	// distinct from actionDisputeIntercompany — the spec's own state model
	// treats disputing and resolving a dispute as different acts, and
	// collapsing them into one grantable action would let whoever raises a
	// dispute also be the one who closes it out, with no independent check.
	actionAcknowledgeCounterparty = "INTERCOMPANY_ENTRY_ACKNOWLEDGE"
	actionDisputeIntercompany     = "INTERCOMPANY_ENTRY_DISPUTE"
	actionResolveMismatch         = "INTERCOMPANY_ENTRY_RESOLVE"

	// actionClosePair is checked on BOTH entities: a terminal close takes
	// the pair out of each side's open set, so neither side alone may do it.
	actionClosePair = "INTERCOMPANY_ENTRY_CLOSE"
)

type Handler struct {
	store          Store
	authz          AuthZClient
	ledger         LedgerClient
	entityRegistry EntityRegistryClient
	log            *zap.Logger
}

func New(store Store, authz AuthZClient, ledger LedgerClient, log *zap.Logger) *Handler {
	return &Handler{
		store:  store,
		authz:  authz,
		ledger: ledger,
		log:    log,
	}
}

// WithEntityRegistry sets the client MatchIntercompany uses to check ACC-11's
// own "Entity loses group relationship mid-period" negative path. It is not a
// New parameter because handler tests exercise the other match rules without
// an entity registry; cmd/server, the only production entrypoint, always
// calls it. Left nil the check is skipped and logged — see MatchEntry's own
// comment and SPEC_DEVIATIONS B13.
func (h *Handler) WithEntityRegistry(c EntityRegistryClient) *Handler {
	h.entityRegistry = c
	return h
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/control-populations/{population}", h.GetControlPopulation)
	r.Get("/v1/intercompany/reciprocal-balances", h.GetReciprocalBalances)
	r.Route("/v1/intercompany/entries", func(r chi.Router) {
		r.Post("/", h.CreateEntry)
		r.Get("/", h.ListEntries)
		r.Get("/{id}", h.GetEntry)
		r.Post("/{id}/match", h.MatchEntry)
		r.Post("/{id}/acknowledge", h.AcknowledgeCounterparty)
		r.Post("/{id}/dispute", h.DisputeIntercompany)
		r.Post("/{id}/resolve", h.ResolveMismatch)
		r.Post("/{id}/close", h.ClosePair)
		r.Get("/{id}/transitions", h.ListTransitions)
		r.Get("/{id}/explanation", h.ExplainMismatch)
	})
}

// ── POST /v1/intercompany/entries ────────────────────────────────────────────────

func (h *Handler) CreateEntry(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.SourceLegalEntityID == "" || req.TargetLegalEntityID == "" || req.SourceJournalID == "" || req.CurrencyCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "source_legal_entity_id, target_legal_entity_id, source_journal_id, currency_code are required")
		return
	}

	if req.SourceLegalEntityID == req.TargetLegalEntityID {
		writeError(w, http.StatusBadRequest, "same_entity_forbidden", string(domain.ErrSameEntityForbidden))
		return
	}

	pair, code, err := resolvePairSides(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, code, err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.SourceLegalEntityID, actionCreateEntry); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	correlationID := getCorrelationID(r)

	// Idempotent replay, resolved before general-ledger-svc is consulted: a
	// retried POST for a source_journal_id that already has a pair returns
	// that pair unchanged. Verifying first would let a transient GL outage
	// turn a successful create's retry into a 503, and could record a second
	// attempt against a pair that was already created.
	existing, err := h.store.GetEntryBySourceJournal(r.Context(), req.SourceJournalID)
	if err == nil {
		writeJSON(w, http.StatusOK, existing)
		return
	}
	if !errors.Is(err, domain.ErrEntryNotFound) {
		h.log.Error("failed to resolve existing intercompany entry", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	now := time.Now().UTC()
	entry := &domain.IntercompanyEntry{
		IntercompanyEntryID:      uuid.NewString(),
		TenantID:                 tenantID,
		SourceLegalEntityID:      req.SourceLegalEntityID,
		TargetLegalEntityID:      req.TargetLegalEntityID,
		SourceJournalID:          req.SourceJournalID,
		Amount:                   pair.amount,
		CurrencyCode:             req.CurrencyCode,
		CounterpartyAmount:       pair.counterpartyAmount,
		CounterpartyCurrencyCode: pair.counterpartyCurrency,
		FiscalPeriod:             pair.fiscalPeriod,
		Dimensions:               pair.dimensions,
		MatchStatus:              domain.MatchStatusUnmatched,
		CreatedAt:                now,
		UpdatedAt:                now,
	}

	// ACC-11 A6: the source journal gets the same four checks MatchEntry
	// runs on the target, at create time. A journal that fails one is a real
	// accounting fact: the pair is created in MISMATCH with an attempt
	// naming the rule, not refused with a 400 the caller cannot fix by
	// retrying the same request.
	attempt, err := h.verifySourceJournal(r.Context(), tenantID, entry, principalID, correlationID)
	if err != nil {
		h.log.Error("failed to verify source journal with GL", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "gl_query_failed", err.Error())
		return
	}

	created, err := h.store.CreateEntryWithAttempt(r.Context(), entry, attempt, principalID, correlationID)
	if err != nil {
		h.log.Error("failed to create intercompany entry", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if !created {
		// A concurrent request created the same source_journal_id between
		// the replay lookup above and this insert. CreateEntryWithAttempt
		// has replaced entry with the original row and recorded no attempt
		// for this request; the original request already enqueued its
		// events.
		writeJSON(w, http.StatusOK, entry)
		return
	}

	writeJSON(w, http.StatusCreated, entry)
}

// verifySourceJournal runs MatchEntry's target-side checks on the source
// journal at create time (SPEC_DEVIATIONS A6): it exists, its legal entity is
// the pair's source entity, it is FINALIZED, and its currency is both valid
// and the currency the source side is denominated in. It reuses
// checkJournal/GetJournal, so there is one journal-verification path, not a
// second one.
//
// A failure sets entry to MISMATCH and returns the attempt that records it.
// A nil attempt with a nil error means the journal passed and the pair stays
// UNMATCHED. An error is a dependency failure (general-ledger-svc or an
// unreadable journal amount): the caller answers 503 and creates nothing.
func (h *Handler) verifySourceJournal(ctx context.Context, tenantID string, entry *domain.IntercompanyEntry, principalID, correlationID string) (*domain.MatchAttempt, error) {
	journal, failure, err := h.checkJournal(ctx, tenantID, entry.SourceJournalID, sourceJournalCheck(entry))
	if err != nil {
		return nil, err
	}
	if failure == nil {
		return nil, nil
	}

	sourceJournalID := entry.SourceJournalID
	attempt := &domain.MatchAttempt{
		IntercompanyEntryID: entry.IntercompanyEntryID,
		Outcome:             domain.MatchStatusMismatch,
		FailedRule:          &failure.rule,
		Message:             &failure.message,
		Source: domain.MatchAttemptSide{
			JournalID: &sourceJournalID, ExpectedAmount: entry.Amount, ExpectedCurrency: entry.CurrencyCode,
		},
		// The target journal has not been named yet; its expected side is
		// still recorded, as every attempt records both sides' expectations.
		Target: domain.MatchAttemptSide{
			ExpectedAmount: entry.CounterpartyAmount, ExpectedCurrency: entry.CounterpartyCurrencyCode,
		},
		ExaminedJournalIDs: []string{entry.SourceJournalID},
		Tolerance:          toleranceDecimal(),
		FiscalPeriod:       entry.FiscalPeriod,
		AttemptedBy:        principalID,
		CorrelationID:      &correlationID,
	}
	if journal != nil {
		total, err := journalTotal(journal)
		if err != nil {
			return nil, fmt.Errorf("read source journal amount: %w", err)
		}
		observeJournal(&attempt.Source, journal, total)
	}

	entry.MatchStatus = domain.MatchStatusMismatch
	entry.MismatchReason = &failure.message
	return attempt, nil
}

// pairSides is a validated CreateIntercompanyPair request's two sides,
// period and dimensions, ready to store.
type pairSides struct {
	amount               domain.Decimal
	counterpartyAmount   domain.Decimal
	counterpartyCurrency string
	fiscalPeriod         *string
	dimensions           domain.Dimensions
}

// resolvePairSides validates both sides of a new pair and fills in the
// counterparty defaults (see domain.CreateEntryRequest). It returns the
// error code to answer 400 with when the request is refused.
func resolvePairSides(req domain.CreateEntryRequest) (pairSides, string, error) {
	var p pairSides
	amount, err := domain.ValidatePositiveAmount(req.Amount)
	if err != nil {
		return p, "invalid_amount", err
	}
	if !domain.ValidCurrencyCode(req.CurrencyCode) {
		return p, "invalid_currency", domain.ErrInvalidCurrency
	}
	p.amount = domain.Decimal(domain.CanonicalAmount(amount))

	p.counterpartyCurrency = req.CounterpartyCurrencyCode
	if p.counterpartyCurrency == "" {
		p.counterpartyCurrency = req.CurrencyCode
	}
	if !domain.ValidCurrencyCode(p.counterpartyCurrency) {
		return p, "invalid_currency", domain.ErrInvalidCurrency
	}

	switch {
	case req.CounterpartyAmount == nil && p.counterpartyCurrency != req.CurrencyCode:
		return p, "counterparty_amount_required", domain.ErrCounterpartyAmountRequired
	case req.CounterpartyAmount == nil:
		p.counterpartyAmount = p.amount
	default:
		cp, err := domain.ValidatePositiveAmount(*req.CounterpartyAmount)
		if err != nil {
			return p, "invalid_amount", err
		}
		if p.counterpartyCurrency == req.CurrencyCode && cp.Cmp(amount) != 0 {
			return p, "counterparty_amount_differs", domain.ErrCounterpartyAmountDiffers
		}
		p.counterpartyAmount = domain.Decimal(domain.CanonicalAmount(cp))
	}

	if req.FiscalPeriod != "" {
		if !domain.ValidFiscalPeriod(req.FiscalPeriod) {
			return p, "invalid_fiscal_period", domain.ErrInvalidFiscalPeriod
		}
		fp := req.FiscalPeriod
		p.fiscalPeriod = &fp
	}

	if err := req.Dimensions.Validate(); err != nil {
		return p, "invalid_dimensions", err
	}
	p.dimensions = req.Dimensions
	if p.dimensions == nil {
		p.dimensions = domain.Dimensions{}
	}
	return p, "", nil
}

// ── GET /v1/intercompany/entries ─────────────────────────────────────────────────

func (h *Handler) ListEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sourceEntityID := q.Get("source_legal_entity_id")
	targetEntityID := q.Get("target_legal_entity_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	entityForAuthz := sourceEntityID
	if entityForAuthz == "" {
		entityForAuthz = targetEntityID
	}
	if entityForAuthz != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, entityForAuthz, actionViewEntry); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListEntries(r.Context(), sourceEntityID, targetEntityID)
	if err != nil {
		h.log.Error("failed to list intercompany entries", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.IntercompanyEntry{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/intercompany/reciprocal-balances ─────────────────────────────────────

// GetReciprocalBalances is ACC-11's own GetReciprocalBalances query
// (SPEC_DEVIATIONS A1). It answers with the period's intercompany positions
// between two legal entities, split by match_status: each direction's totals
// by currency and their net per currency, all exact decimals. CLOSED counts
// — a closed pair was a real settled position, the same reasoning that keeps
// it eliminable in consolidation-svc.
//
// Authorized on BOTH entities: a reciprocal balance is both sides' position,
// so a principal entitled on one side only may not read the other's. Same
// two-entity check as ClosePair.
func (h *Handler) GetReciprocalBalances(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entityA, entityB, period := q.Get("entity_a"), q.Get("entity_b"), q.Get("period")

	if entityA == "" || entityB == "" || period == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "entity_a, entity_b and period are required")
		return
	}
	if entityA == entityB {
		writeError(w, http.StatusBadRequest, "same_entity_forbidden", string(domain.ErrSameEntityForbidden))
		return
	}
	if !domain.ValidFiscalPeriod(period) {
		writeError(w, http.StatusBadRequest, "invalid_fiscal_period", domain.ErrInvalidFiscalPeriod.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	for _, entityID := range []string{entityA, entityB} {
		if err := h.authz.CheckAllowed(r.Context(), principalID, entityID, actionViewEntry); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	groups, err := h.store.ReciprocalBalances(r.Context(), entityA, entityB, period)
	if err != nil {
		h.log.Error("failed to aggregate reciprocal balances", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	balances, err := domain.BuildReciprocalBalances(entityA, entityB, period, groups)
	if err != nil {
		h.log.Error("stored intercompany amount is unreadable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, balances)
}

// ── GET /v1/intercompany/entries/{id} ────────────────────────────────────────────

func (h *Handler) GetEntry(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	entry, err := h.store.GetEntry(r.Context(), id)
	if errors.Is(err, domain.ErrEntryNotFound) {
		writeError(w, http.StatusNotFound, "entry_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch intercompany entry", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entry.SourceLegalEntityID, actionViewEntry); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, entry)
}

// ── POST /v1/intercompany/entries/{id}/match ─────────────────────────────────────

func (h *Handler) MatchEntry(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	correlationID := getCorrelationID(r)

	var req domain.MatchEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.TargetJournalID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "target_journal_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	entry, err := h.store.GetEntry(r.Context(), id)
	if errors.Is(err, domain.ErrEntryNotFound) {
		writeError(w, http.StatusNotFound, "entry_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch intercompany entry", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// MatchIntercompany is only meaningful from the statuses in
	// domain.MatchableStatuses. This early check gives a clean 422 without
	// calling general-ledger-svc; the store re-checks it in the UPDATE.
	if !slices.Contains(domain.MatchableStatuses, entry.MatchStatus) {
		writeError(w, http.StatusUnprocessableEntity, "entry_already_matched", string(domain.ErrEntryAlreadyMatched))
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entry.TargetLegalEntityID, actionMatchEntry); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())

	// attempt is this call's evidence (ExplainMismatch): filled in as each
	// check runs, and recorded with whichever outcome the match reaches.
	attempt := newMatchAttempt(entry, req.TargetJournalID, principalID, correlationID)

	// Fetch the target journal and run the four checks every pairing side
	// gets: it exists, its legal entity is the expected one, it is FINALIZED,
	// and its currency is valid and the expected one. The amount check below
	// is target-specific; a create-time source check stops at these four
	// (SPEC_DEVIATIONS A6).
	journal, failure, err := h.checkJournal(r.Context(), tenantID, req.TargetJournalID, targetJournalCheck(entry))
	if err != nil {
		h.log.Error("failed to query target journal from GL", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "gl_query_failed", err.Error())
		return
	}
	if journal == nil {
		// ACC-11's own negative path, "One side missing": the named
		// counterparty journal does not exist at all. A real accounting
		// fact worth recording as MISMATCH, not a transient 503 — the
		// caller almost certainly named the wrong journal_id, and retrying
		// the identical request will fail identically.
		h.recordMismatch(w, r, entry, attempt, failure.rule, failure.message)
		return
	}

	// The journal amount is the larger of its total debits and total
	// credits (SPEC_DEVIATIONS A5), summed exactly. Read before the checks'
	// outcome is recorded so every attempt records what the journal held,
	// whichever rule fails.
	journalAmount, err := journalTotal(journal)
	if err != nil {
		h.log.Error("general-ledger-svc returned an unreadable journal amount", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "gl_query_failed", err.Error())
		return
	}
	expected, err := entry.CounterpartyAmount.Rat()
	if err != nil {
		h.log.Error("stored counterparty amount is unreadable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	observeJournal(&attempt.Target, journal, journalAmount)

	if failure != nil {
		h.recordMismatch(w, r, entry, attempt, failure.rule, failure.message)
		return
	}

	// The target journal's amount must match the expected counterparty
	// amount, compared exactly (big.Rat) within matchTolerance.
	if diff := new(big.Rat).Sub(journalAmount, expected); diff.Abs(diff).Cmp(matchTolerance) > 0 {
		reason := fmt.Sprintf("amount_mismatch: expected counterparty amount %s %s != journal amount %s %s",
			domain.CanonicalAmount(expected), entry.CounterpartyCurrencyCode, domain.CanonicalAmount(journalAmount), journal.CurrencyCode)
		h.recordMismatch(w, r, entry, attempt, domain.MatchRuleAmountMismatch, reason)
		return
	}

	// Validation 5: source and target legal entities must still share an
	// open group relationship — the spec's own negative path, "Entity
	// loses group relationship mid-period." The relationship is evaluated
	// as of the end of the pair's fiscal_period, not at the moment of the
	// match call (SPEC_DEVIATIONS A11): a relationship that held through
	// the period still passes even if it has ended since, and one that only
	// started after the period fails even if it is open today.
	//
	// An unconfigured client skips the check. That branch is unreachable
	// from cmd/server, which always wires the client (TENANT_REGISTRY_URL
	// has a non-empty default); it exists for tests, and it is logged so a
	// future entrypoint that forgets the client is not silent.
	if h.entityRegistry == nil {
		h.log.Warn("entity-registry client not configured; skipping group relationship check",
			zap.String("intercompany_entry_id", entry.IntercompanyEntryID))
	} else {
		sourceHierarchies, srcErr := h.entityRegistry.ListHierarchies(r.Context(), tenantID, entry.SourceLegalEntityID)
		targetHierarchies, tgtErr := h.entityRegistry.ListHierarchies(r.Context(), tenantID, entry.TargetLegalEntityID)
		if srcErr != nil || tgtErr != nil {
			h.log.Error("failed to query tenant-entity-registry-svc for group relationship", zap.Error(srcErr), zap.Error(tgtErr))
			writeError(w, http.StatusServiceUnavailable, "entity_registry_unavailable", domain.ErrEntityRegistryUnavailable.Error())
			return
		}
		if !entityregistry.SharesOpenGroupRelationship(sourceHierarchies, targetHierarchies, entry.SourceLegalEntityID, entry.TargetLegalEntityID, groupRelationshipAsOf(entry)) {
			reason := domain.ErrGroupRelationshipLost.Error()
			h.recordMismatch(w, r, entry, attempt, domain.MatchRuleGroupRelationshipLost, reason)
			return
		}
	}

	// Match Successful
	entry.TargetJournalID = &req.TargetJournalID
	entry.MatchStatus = domain.MatchStatusMatched
	attempt.Outcome = domain.MatchStatusMatched
	if err := h.store.UpdateMatch(r.Context(), entry, attempt, principalID, correlationID); err != nil {
		h.writeUpdateMatchErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, domain.MatchEntryResponse{
		IntercompanyEntryID: id,
		MatchStatus:         domain.MatchStatusMatched,
	})
}

// groupRelationshipAsOf is the instant Validation 5 evaluates the
// source/target relationship at (SPEC_DEVIATIONS A11): the end of the pair's
// fiscal_period when it names a calendar month, so the check answers "did the
// relationship hold through the period this pair belongs to".
//
// A pair with no period, or with a period that is not YYYY-MM, is checked as
// of now. fiscal_period is a free 1-20 character reference (A4/B7) and
// general-ledger-svc stores it verbatim, so anything else has no calendar end
// to evaluate at; "now" is the only instant left, and it is the pre-A11
// behaviour.
func groupRelationshipAsOf(entry *domain.IntercompanyEntry) time.Time {
	now := time.Now().UTC()
	if entry.FiscalPeriod == nil {
		return now
	}
	end, ok := domain.FiscalPeriodEnd(*entry.FiscalPeriod)
	if !ok {
		return now
	}
	return end
}

// matchTolerance is the fixed tolerance a journal amount may differ from the
// expected counterparty amount by (unchanged from the float-era 0.001; there
// is no matching-policy version yet, SPEC_DEVIATIONS A5).
var matchTolerance = big.NewRat(1, 1000)

func toleranceDecimal() domain.Decimal {
	return domain.Decimal(domain.CanonicalAmount(matchTolerance))
}

// journalCheck describes the four checks one side of a pair gets from
// general-ledger-svc and the rule names a failure records. The checks and
// their order are the same on both sides; only the rules and the sentences
// differ, so there is one verification path (SPEC_DEVIATIONS A6).
type journalCheck struct {
	side                  string // "source" or "target"
	expectedEntityID      string
	expectedCurrency      string
	expectedCurrencyLabel string // "source" or "counterparty" in a currency_mismatch sentence
	missingRule           string
	missingMessage        string
	entityRule            string
	notFinalizedRule      string
	unverifiableRule      string
	currencyMismatchRule  string
}

func sourceJournalCheck(entry *domain.IntercompanyEntry) journalCheck {
	return journalCheck{
		side:                  "source",
		expectedEntityID:      entry.SourceLegalEntityID,
		expectedCurrency:      entry.CurrencyCode,
		expectedCurrencyLabel: "source",
		missingRule:           domain.MatchRuleSourceJournalMissing,
		missingMessage:        domain.ErrSourceJournalMissing.Error(),
		entityRule:            domain.MatchRuleSourceEntityMismatch,
		notFinalizedRule:      domain.MatchRuleSourceJournalNotFinalized,
		unverifiableRule:      domain.MatchRuleSourceCurrencyUnverifiable,
		currencyMismatchRule:  domain.MatchRuleSourceCurrencyMismatch,
	}
}

func targetJournalCheck(entry *domain.IntercompanyEntry) journalCheck {
	return journalCheck{
		side:                  "target",
		expectedEntityID:      entry.TargetLegalEntityID,
		expectedCurrency:      entry.CounterpartyCurrencyCode,
		expectedCurrencyLabel: "counterparty",
		missingRule:           domain.MatchRuleCounterpartyJournalMissing,
		missingMessage:        domain.ErrCounterpartyJournalMissing.Error(),
		entityRule:            domain.MatchRuleTargetEntityMismatch,
		notFinalizedRule:      domain.MatchRuleTargetJournalNotFinalized,
		unverifiableRule:      domain.MatchRuleCurrencyUnverifiable,
		currencyMismatchRule:  domain.MatchRuleCurrencyMismatch,
	}
}

// journalFailure is a real accounting fact about a journal: which rule it
// failed and the sentence that explains it.
type journalFailure struct {
	rule    string
	message string
}

// checkJournal fetches journalID with the same general-ledger-svc call
// MatchEntry has always used and applies the four checks common to both
// sides of a pair, in this order: exists, belongs to expectedEntityID, is
// FINALIZED, and carries a valid ISO 4217 currency equal to expectedCurrency.
// Currency is checked before any amount comparison, so two amounts in
// different currencies are never compared as if comparable.
//
// A missing journal returns a nil detail and a failure; any other failure
// returns the detail as fetched, so the caller can still record what was
// observed. Only a general-ledger-svc error returns err.
func (h *Handler) checkJournal(ctx context.Context, tenantID, journalID string, c journalCheck) (*ledger.JournalDetail, *journalFailure, error) {
	journal, err := h.ledger.GetJournal(ctx, tenantID, journalID)
	if errors.Is(err, domain.ErrEntryNotFound) {
		return nil, &journalFailure{rule: c.missingRule, message: c.missingMessage}, nil
	}
	if err != nil {
		return nil, nil, err
	}

	if journal.LegalEntityID != c.expectedEntityID {
		return journal, &journalFailure{
			rule:    c.entityRule,
			message: fmt.Sprintf("%s_journal_entity_mismatch: journal entity %s != %s entity %s", c.side, journal.LegalEntityID, c.side, c.expectedEntityID),
		}, nil
	}
	if journal.Status != "FINALIZED" {
		return journal, &journalFailure{
			rule:    c.notFinalizedRule,
			message: fmt.Sprintf("%s_journal_not_finalized: status is %s", c.side, journal.Status),
		}, nil
	}
	if !domain.ValidCurrencyCode(journal.CurrencyCode) {
		return journal, &journalFailure{
			rule:    c.unverifiableRule,
			message: fmt.Sprintf("currency_unverifiable: %s journal currency is %q, expected %s", c.side, journal.CurrencyCode, c.expectedCurrency),
		}, nil
	}
	if journal.CurrencyCode != c.expectedCurrency {
		return journal, &journalFailure{
			rule:    c.currencyMismatchRule,
			message: fmt.Sprintf("currency_mismatch: expected %s currency %s != journal currency %s", c.expectedCurrencyLabel, c.expectedCurrency, journal.CurrencyCode),
		}, nil
	}
	return journal, nil, nil
}

// journalTotal is the larger of the journal's total debits and total
// credits, summed exactly. An absent line amount counts as zero, as it did
// when lines were decoded into float64.
func journalTotal(j *ledger.JournalDetail) (*big.Rat, error) {
	var debits, credits []string
	for _, l := range j.Lines {
		if l.DebitAmount != "" {
			debits = append(debits, string(l.DebitAmount))
		}
		if l.CreditAmount != "" {
			credits = append(credits, string(l.CreditAmount))
		}
	}
	totalDebit, err := domain.SumAmounts(debits)
	if err != nil {
		return nil, err
	}
	totalCredit, err := domain.SumAmounts(credits)
	if err != nil {
		return nil, err
	}
	if totalCredit.Cmp(totalDebit) > 0 {
		return totalCredit, nil
	}
	return totalDebit, nil
}

// newMatchAttempt starts the evidence for one match attempt: what each side
// of the pair expects, which journal is examined, and the tolerance. The
// source side is expected but not re-examined at match time: it was verified
// when the pair was created (SPEC_DEVIATIONS A6), and that create-time
// attempt carries its observed fields.
func newMatchAttempt(entry *domain.IntercompanyEntry, targetJournalID, principalID, correlationID string) *domain.MatchAttempt {
	sourceJournalID := entry.SourceJournalID
	return &domain.MatchAttempt{
		IntercompanyEntryID: entry.IntercompanyEntryID,
		Source: domain.MatchAttemptSide{
			JournalID: &sourceJournalID, ExpectedAmount: entry.Amount, ExpectedCurrency: entry.CurrencyCode,
		},
		Target: domain.MatchAttemptSide{
			JournalID: &targetJournalID, ExpectedAmount: entry.CounterpartyAmount, ExpectedCurrency: entry.CounterpartyCurrencyCode,
		},
		ExaminedJournalIDs: []string{targetJournalID},
		Tolerance:          toleranceDecimal(),
		AttemptedBy:        principalID,
		CorrelationID:      &correlationID,
	}
}

// observeJournal records what general-ledger-svc reported for a journal,
// exactly as reported.
func observeJournal(side *domain.MatchAttemptSide, j *ledger.JournalDetail, total *big.Rat) {
	amount := domain.Decimal(domain.CanonicalAmount(total))
	currency, entity, status, period := j.CurrencyCode, j.LegalEntityID, j.Status, j.FiscalPeriod
	side.ObservedAmount = &amount
	side.ObservedCurrency = &currency
	side.ObservedLegalEntityID = &entity
	side.ObservedStatus = &status
	side.ObservedFiscalPeriod = &period
}

// recordMismatch persists a MISMATCH outcome, with the rule that failed,
// and reports it. The response claims MISMATCH only once the store has
// recorded it (and its attempt and event): a failed write is a 503 the
// caller can retry, and a pair whose status moved on since it was read is a
// 422, never overwritten.
func (h *Handler) recordMismatch(w http.ResponseWriter, r *http.Request, entry *domain.IntercompanyEntry, attempt *domain.MatchAttempt, rule, reason string) {
	entry.TargetJournalID = attempt.Target.JournalID
	entry.MatchStatus = domain.MatchStatusMismatch
	entry.MismatchReason = &reason
	attempt.Outcome = domain.MatchStatusMismatch
	attempt.FailedRule = &rule
	attempt.Message = &reason
	if err := h.store.UpdateMatch(r.Context(), entry, attempt, attempt.AttemptedBy, derefOr(attempt.CorrelationID)); err != nil {
		h.writeUpdateMatchErr(w, err)
		return
	}
	writeJSON(w, http.StatusUnprocessableEntity, domain.MatchEntryResponse{
		IntercompanyEntryID: entry.IntercompanyEntryID,
		MatchStatus:         domain.MatchStatusMismatch,
		MismatchReason:      &reason,
		FailedRule:          &rule,
	})
}

func derefOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h *Handler) writeUpdateMatchErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidPairTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrEntryNotFound):
		writeError(w, http.StatusNotFound, "entry_not_found", "")
	default:
		h.log.Error("failed to update match status in store", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

// ── POST /v1/intercompany/entries/{id}/acknowledge ───────────────────────────────

// AcknowledgeCounterparty is ACC-11's own AcknowledgeCounterparty command —
// moves a pair to AWAITING_COUNTERPARTY, the spec's own named milestone
// between a pair being opened and it being matched.
func (h *Handler) AcknowledgeCounterparty(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	entry, err := h.store.GetEntry(r.Context(), id)
	if errors.Is(err, domain.ErrEntryNotFound) {
		writeError(w, http.StatusNotFound, "entry_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch intercompany entry", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entry.TargetLegalEntityID, actionAcknowledgeCounterparty); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.AcknowledgeCounterparty(r.Context(), id, principalID, getCorrelationID(r)); err != nil {
		h.writeTransitionErr(w, "failed to acknowledge counterparty", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"match_status": domain.MatchStatusAwaitingCounterparty})
}

// ── POST /v1/intercompany/entries/{id}/dispute ────────────────────────────────────

// DisputeIntercompany is ACC-11's own DisputeIntercompany command — flags a
// MISMATCH pair for investigation, only reachable from MISMATCH.
func (h *Handler) DisputeIntercompany(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.DisputeIntercompanyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrDisputeReasonRequired.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	entry, err := h.store.GetEntry(r.Context(), id)
	if errors.Is(err, domain.ErrEntryNotFound) {
		writeError(w, http.StatusNotFound, "entry_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch intercompany entry", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entry.TargetLegalEntityID, actionDisputeIntercompany); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.DisputeIntercompany(r.Context(), id, principalID, req.Reason, getCorrelationID(r)); err != nil {
		h.writeTransitionErr(w, "failed to dispute intercompany pair", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"match_status": domain.MatchStatusDisputed})
}

// ── POST /v1/intercompany/entries/{id}/resolve ────────────────────────────────────

// ResolveMismatch is ACC-11's own ResolveMismatch command — records a
// human resolution decision for a DISPUTED pair. Deliberately distinct
// authorization action from DisputeIntercompany (see actionResolveMismatch's
// own comment): whoever raises a dispute is not automatically who can close it.
func (h *Handler) ResolveMismatch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.ResolveMismatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.ResolutionNote == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrResolutionNoteRequired.Error())
		return
	}
	if !domain.ValidResolutionOutcome(req.Outcome) {
		writeError(w, http.StatusBadRequest, "invalid_outcome", domain.ErrResolutionOutcomeInvalid.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	entry, err := h.store.GetEntry(r.Context(), id)
	if errors.Is(err, domain.ErrEntryNotFound) {
		writeError(w, http.StatusNotFound, "entry_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch intercompany entry", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entry.TargetLegalEntityID, actionResolveMismatch); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.ResolveMismatch(r.Context(), id, principalID, req.ResolutionNote, req.Outcome, getCorrelationID(r)); err != nil {
		h.writeTransitionErr(w, "failed to resolve intercompany mismatch", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"match_status": domain.MatchStatusResolved, "resolution_outcome": req.Outcome})
}

// ── POST /v1/intercompany/entries/{id}/close ──────────────────────────────────────

// ClosePair moves a MATCHED pair to CLOSED, its terminal state (e.g. once
// its period has been consolidated). Authorized on both entities: see
// actionClosePair.
func (h *Handler) ClosePair(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.ClosePairRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrCloseReasonRequired.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	entry, ok := h.fetchEntry(w, r, id)
	if !ok {
		return
	}

	for _, entityID := range []string{entry.SourceLegalEntityID, entry.TargetLegalEntityID} {
		if err := h.authz.CheckAllowed(r.Context(), principalID, entityID, actionClosePair); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	if err := h.store.ClosePair(r.Context(), id, principalID, req.Reason, getCorrelationID(r)); err != nil {
		h.writeTransitionErr(w, "failed to close intercompany pair", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"match_status": domain.MatchStatusClosed})
}

// ── GET /v1/intercompany/entries/{id}/transitions ─────────────────────────────────

// ListTransitions returns a pair's append-only state history, oldest first.
// Authorized like GetEntry.
func (h *Handler) ListTransitions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	entry, ok := h.fetchEntry(w, r, id)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entry.SourceLegalEntityID, actionViewEntry); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	transitions, err := h.store.ListTransitions(r.Context(), id)
	if err != nil {
		h.log.Error("failed to list intercompany pair transitions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, transitions)
}

// ── GET /v1/intercompany/entries/{id}/explanation ─────────────────────────────────

// ExplainMismatch returns the pair's current status and mismatch_reason
// with every match attempt behind them, oldest first: expected against
// observed amount and currency per side, the rule that failed, the journals
// examined and the tolerance applied. Authorized like GetEntry.
func (h *Handler) ExplainMismatch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	entry, ok := h.fetchEntry(w, r, id)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entry.SourceLegalEntityID, actionViewEntry); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	attempts, err := h.store.ListMatchAttempts(r.Context(), id)
	if err != nil {
		h.log.Error("failed to list intercompany match attempts", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, domain.MismatchExplanation{
		IntercompanyEntryID: entry.IntercompanyEntryID,
		MatchStatus:         entry.MatchStatus,
		MismatchReason:      entry.MismatchReason,
		Attempts:            attempts,
	})
}

// fetchEntry reads a pair, answering 404 or 503 itself when it cannot.
func (h *Handler) fetchEntry(w http.ResponseWriter, r *http.Request, id string) (*domain.IntercompanyEntry, bool) {
	entry, err := h.store.GetEntry(r.Context(), id)
	if errors.Is(err, domain.ErrEntryNotFound) {
		writeError(w, http.StatusNotFound, "entry_not_found", "")
		return nil, false
	}
	if err != nil {
		h.log.Error("failed to fetch intercompany entry", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return nil, false
	}
	return entry, true
}

// writeTransitionErr maps a lifecycle store error: a pair not in the status
// the command needs is 422, a pair that does not exist 404, anything else a
// 503 the caller can retry.
func (h *Handler) writeTransitionErr(w http.ResponseWriter, logMsg string, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidPairTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrEntryNotFound):
		writeError(w, http.StatusNotFound, "entry_not_found", "")
	default:
		h.log.Error(logMsg, zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
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
