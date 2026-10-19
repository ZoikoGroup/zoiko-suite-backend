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
	// CreateEntry enqueues the "created" lifecycle event in the same
	// transaction as the insert (services/eventing outbox) — see
	// internal/store's doc comment on why this replaced a post-commit
	// publish from this handler.
	CreateEntry(ctx context.Context, entry *domain.IntercompanyEntry, actorID, correlationID string) (created bool, err error)
	GetEntry(ctx context.Context, id string) (*domain.IntercompanyEntry, error)
	ListEntries(ctx context.Context, sourceEntityID, targetEntityID string) ([]domain.IntercompanyEntry, error)
	// UpdateMatch persists entry's target state and, in the same
	// transaction, enqueues the "posted" or "mismatch.detected" event that
	// state implies.
	UpdateMatch(ctx context.Context, entry *domain.IntercompanyEntry, actorID, correlationID string) error

	// ACC-11 lifecycle — see migrations 000003 and 000006. Each records its
	// state change in the pair's append-only transition history.
	AcknowledgeCounterparty(ctx context.Context, id, principalID, correlationID string) error
	DisputeIntercompany(ctx context.Context, id, principalID, reason, correlationID string) error
	ResolveMismatch(ctx context.Context, id, principalID, resolutionNote, outcome, correlationID string) error
	ClosePair(ctx context.Context, id, principalID, reason, correlationID string) error
	ListTransitions(ctx context.Context, id string) ([]domain.PairTransition, error)

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
// own "Entity loses group relationship mid-period" negative path. Left nil,
// the check is skipped entirely (a deliberate, deployable-without-it
// posture — see MatchEntry's own comment) rather than New's signature
// growing a required parameter every existing caller and test would have
// to update.
func (h *Handler) WithEntityRegistry(c EntityRegistryClient) *Handler {
	h.entityRegistry = c
	return h
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/control-populations/{population}", h.GetControlPopulation)
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
		MatchStatus:              "UNMATCHED",
		CreatedAt:                now,
		UpdatedAt:                now,
	}

	created, err := h.store.CreateEntry(r.Context(), entry, principalID, correlationID)
	if err != nil {
		h.log.Error("failed to create intercompany entry", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if !created {
		// Replay of a prior request for the same source_journal_id — return
		// the original entry; the original request's CreateEntry already
		// enqueued the created event.
		writeJSON(w, http.StatusOK, entry)
		return
	}

	writeJSON(w, http.StatusCreated, entry)
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

	// Fetch target journal detail from general-ledger-svc
	journal, err := h.ledger.GetJournal(r.Context(), tenantID, req.TargetJournalID)
	if errors.Is(err, domain.ErrEntryNotFound) {
		// ACC-11's own negative path, "One side missing": the named
		// counterparty journal does not exist at all. A real accounting
		// fact worth recording as MISMATCH, not a transient 503 — the
		// caller almost certainly named the wrong journal_id, and retrying
		// the identical request will fail identically.
		reason := domain.ErrCounterpartyJournalMissing.Error()
		h.recordMismatch(w, r, entry, req.TargetJournalID, reason, principalID, correlationID)
		return
	}
	if err != nil {
		h.log.Error("failed to query target journal from GL", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "gl_query_failed", err.Error())
		return
	}

	// Validation 1: Target journal entity must match entry target legal entity
	if journal.LegalEntityID != entry.TargetLegalEntityID {
		reason := fmt.Sprintf("target_journal_entity_mismatch: journal entity %s != target entity %s", journal.LegalEntityID, entry.TargetLegalEntityID)
		h.recordMismatch(w, r, entry, req.TargetJournalID, reason, principalID, correlationID)
		return
	}

	// Validation 2: Journal status must be FINALIZED
	if journal.Status != "FINALIZED" {
		reason := fmt.Sprintf("target_journal_not_finalized: status is %s", journal.Status)
		h.recordMismatch(w, r, entry, req.TargetJournalID, reason, principalID, correlationID)
		return
	}

	// Validation 3: the target journal must be in the currency the
	// counterparty side is expected in. Checked before the amount: two
	// amounts in different currencies are not comparable, so a currency
	// difference is refused outright rather than the raw numbers being
	// compared (a 5,000 GBP journal used to match a 5,000 USD pair).
	if !domain.ValidCurrencyCode(journal.CurrencyCode) {
		reason := fmt.Sprintf("currency_unverifiable: target journal currency is %q, expected %s", journal.CurrencyCode, entry.CounterpartyCurrencyCode)
		h.recordMismatch(w, r, entry, req.TargetJournalID, reason, principalID, correlationID)
		return
	}
	if journal.CurrencyCode != entry.CounterpartyCurrencyCode {
		reason := fmt.Sprintf("currency_mismatch: expected counterparty currency %s != journal currency %s", entry.CounterpartyCurrencyCode, journal.CurrencyCode)
		h.recordMismatch(w, r, entry, req.TargetJournalID, reason, principalID, correlationID)
		return
	}

	// Validation 4: the target journal's amount must match the expected
	// counterparty amount, compared exactly (big.Rat) within matchTolerance.
	// The journal amount is still the larger of its total debits and total
	// credits (SPEC_DEVIATIONS A5).
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
	if diff := new(big.Rat).Sub(journalAmount, expected); diff.Abs(diff).Cmp(matchTolerance) > 0 {
		reason := fmt.Sprintf("amount_mismatch: expected counterparty amount %s %s != journal amount %s %s",
			domain.CanonicalAmount(expected), entry.CounterpartyCurrencyCode, domain.CanonicalAmount(journalAmount), journal.CurrencyCode)
		h.recordMismatch(w, r, entry, req.TargetJournalID, reason, principalID, correlationID)
		return
	}

	// Validation 5: source and target legal entities must still share an
	// open group relationship — the spec's own negative path, "Entity
	// loses group relationship mid-period." Skipped (not enforced) when no
	// entity-registry client is configured — see WithEntityRegistry's own
	// comment — rather than this handler failing closed against a
	// dependency a deployment hasn't wired up.
	if h.entityRegistry != nil {
		sourceHierarchies, srcErr := h.entityRegistry.ListHierarchies(r.Context(), tenantID, entry.SourceLegalEntityID)
		targetHierarchies, tgtErr := h.entityRegistry.ListHierarchies(r.Context(), tenantID, entry.TargetLegalEntityID)
		if srcErr != nil || tgtErr != nil {
			h.log.Error("failed to query tenant-entity-registry-svc for group relationship", zap.Error(srcErr), zap.Error(tgtErr))
			writeError(w, http.StatusServiceUnavailable, "entity_registry_unavailable", domain.ErrEntityRegistryUnavailable.Error())
			return
		}
		if !entityregistry.SharesOpenGroupRelationship(sourceHierarchies, targetHierarchies, entry.SourceLegalEntityID, entry.TargetLegalEntityID, time.Now().UTC()) {
			reason := domain.ErrGroupRelationshipLost.Error()
			h.recordMismatch(w, r, entry, req.TargetJournalID, reason, principalID, correlationID)
			return
		}
	}

	// Match Successful
	entry.TargetJournalID = &req.TargetJournalID
	entry.MatchStatus = "MATCHED"
	entry.MismatchReason = nil
	if err := h.store.UpdateMatch(r.Context(), entry, principalID, correlationID); err != nil {
		h.writeUpdateMatchErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, domain.MatchEntryResponse{
		IntercompanyEntryID: id,
		MatchStatus:         "MATCHED",
	})
}

// matchTolerance is the fixed tolerance a journal amount may differ from the
// expected counterparty amount by (unchanged from the float-era 0.001; there
// is no matching-policy version yet, SPEC_DEVIATIONS A5).
var matchTolerance = big.NewRat(1, 1000)

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

// recordMismatch persists a MISMATCH outcome and reports it. The response
// claims MISMATCH only once the store has recorded it (and its event): a
// failed write is a 503 the caller can retry, and a pair whose status moved
// on since it was read is a 422, never overwritten.
func (h *Handler) recordMismatch(w http.ResponseWriter, r *http.Request, entry *domain.IntercompanyEntry, targetJournalID, reason, principalID, correlationID string) {
	entry.TargetJournalID = &targetJournalID
	entry.MatchStatus = domain.MatchStatusMismatch
	entry.MismatchReason = &reason
	if err := h.store.UpdateMatch(r.Context(), entry, principalID, correlationID); err != nil {
		h.writeUpdateMatchErr(w, err)
		return
	}
	writeJSON(w, http.StatusUnprocessableEntity, domain.MatchEntryResponse{
		IntercompanyEntryID: entry.IntercompanyEntryID,
		MatchStatus:         domain.MatchStatusMismatch,
		MismatchReason:      &reason,
	})
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
