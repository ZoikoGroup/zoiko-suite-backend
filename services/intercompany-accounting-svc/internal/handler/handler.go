package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	"zoiko.io/intercompany-accounting-svc/internal/ledger"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
)

type Store interface {
	CreateEntry(ctx context.Context, entry *domain.IntercompanyEntry) (created bool, err error)
	GetEntry(ctx context.Context, id string) (*domain.IntercompanyEntry, error)
	ListEntries(ctx context.Context, sourceEntityID, targetEntityID string) ([]domain.IntercompanyEntry, error)
	UpdateMatch(ctx context.Context, id, targetJournalID, matchStatus string, mismatchReason *string) error

	// ACC-11 lifecycle — see migration 000003's doc comment.
	AcknowledgeCounterparty(ctx context.Context, id, principalID string) error
	DisputeIntercompany(ctx context.Context, id, principalID, reason string) error
	ResolveMismatch(ctx context.Context, id, principalID, resolutionNote string) error
}

type Publisher interface {
	PublishEntryCreated(ctx context.Context, correlationID, actorID string, entry domain.IntercompanyEntry)
	PublishEntryPosted(ctx context.Context, correlationID, actorID string, entry domain.IntercompanyEntry)
	PublishMismatchDetected(ctx context.Context, correlationID, actorID string, entry domain.IntercompanyEntry, reason string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type LedgerClient interface {
	GetJournal(ctx context.Context, tenantID, journalID string) (*ledger.JournalDetail, error)
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
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	ledger    LedgerClient
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, ledger LedgerClient, log *zap.Logger) *Handler {
	return &Handler{
		store:     store,
		publisher: publisher,
		authz:     authz,
		ledger:    ledger,
		log:       log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/intercompany/entries", func(r chi.Router) {
		r.Post("/", h.CreateEntry)
		r.Get("/", h.ListEntries)
		r.Get("/{id}", h.GetEntry)
		r.Post("/{id}/match", h.MatchEntry)
		r.Post("/{id}/acknowledge", h.AcknowledgeCounterparty)
		r.Post("/{id}/dispute", h.DisputeIntercompany)
		r.Post("/{id}/resolve", h.ResolveMismatch)
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

	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_amount", string(domain.ErrInvalidAmount))
		return
	}

	if req.SourceLegalEntityID == req.TargetLegalEntityID {
		writeError(w, http.StatusBadRequest, "same_entity_forbidden", string(domain.ErrSameEntityForbidden))
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
		IntercompanyEntryID: uuid.NewString(),
		TenantID:            tenantID,
		SourceLegalEntityID: req.SourceLegalEntityID,
		TargetLegalEntityID: req.TargetLegalEntityID,
		SourceJournalID:     req.SourceJournalID,
		Amount:              req.Amount,
		CurrencyCode:        req.CurrencyCode,
		MatchStatus:         "UNMATCHED",
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	created, err := h.store.CreateEntry(r.Context(), entry)
	if err != nil {
		h.log.Error("failed to create intercompany entry", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if !created {
		// Replay of a prior request for the same source_journal_id — return
		// the original entry, do not re-publish the created event.
		writeJSON(w, http.StatusOK, entry)
		return
	}

	h.publisher.PublishEntryCreated(r.Context(), correlationID, principalID, *entry)
	writeJSON(w, http.StatusCreated, entry)
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

	// MatchIntercompany is only meaningful from OPEN ("UNMATCHED") or
	// AWAITING_COUNTERPARTY — a pair already MATCHED, under DISPUTE, or
	// RESOLVED has left the part of the state model matching belongs to.
	switch entry.MatchStatus {
	case domain.MatchStatusUnmatched, domain.MatchStatusAwaitingCounterparty, domain.MatchStatusMismatch:
		// MISMATCH is also re-matchable: a caller correcting a prior
		// mismatch (e.g. supplying the right target_journal_id this time)
		// re-runs MatchIntercompany rather than needing a separate command.
	default:
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
		_ = h.store.UpdateMatch(r.Context(), id, req.TargetJournalID, domain.MatchStatusMismatch, &reason)
		entry.TargetJournalID = &req.TargetJournalID
		entry.MatchStatus = domain.MatchStatusMismatch
		entry.MismatchReason = &reason
		h.publisher.PublishMismatchDetected(r.Context(), correlationID, principalID, *entry, reason)
		writeJSON(w, http.StatusUnprocessableEntity, domain.MatchEntryResponse{
			IntercompanyEntryID: id,
			MatchStatus:         domain.MatchStatusMismatch,
			MismatchReason:      &reason,
		})
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
		_ = h.store.UpdateMatch(r.Context(), id, req.TargetJournalID, "MISMATCH", &reason)
		entry.TargetJournalID = &req.TargetJournalID
		entry.MatchStatus = "MISMATCH"
		entry.MismatchReason = &reason
		h.publisher.PublishMismatchDetected(r.Context(), correlationID, principalID, *entry, reason)
		writeJSON(w, http.StatusUnprocessableEntity, domain.MatchEntryResponse{
			IntercompanyEntryID: id,
			MatchStatus:         "MISMATCH",
			MismatchReason:      &reason,
		})
		return
	}

	// Validation 2: Journal status must be FINALIZED
	if journal.Status != "FINALIZED" {
		reason := fmt.Sprintf("target_journal_not_finalized: status is %s", journal.Status)
		_ = h.store.UpdateMatch(r.Context(), id, req.TargetJournalID, "MISMATCH", &reason)
		entry.TargetJournalID = &req.TargetJournalID
		entry.MatchStatus = "MISMATCH"
		entry.MismatchReason = &reason
		h.publisher.PublishMismatchDetected(r.Context(), correlationID, principalID, *entry, reason)
		writeJSON(w, http.StatusUnprocessableEntity, domain.MatchEntryResponse{
			IntercompanyEntryID: id,
			MatchStatus:         "MISMATCH",
			MismatchReason:      &reason,
		})
		return
	}

	// Validation 3: Sum of line amounts in target journal must match intercompany entry amount
	var totalDebit, totalCredit float64
	for _, l := range journal.Lines {
		totalDebit += l.DebitAmount
		totalCredit += l.CreditAmount
	}
	journalAmount := totalDebit
	if totalCredit > totalDebit {
		journalAmount = totalCredit
	}

	if math.Abs(journalAmount-entry.Amount) > 0.001 {
		reason := fmt.Sprintf("amount_mismatch: intercompany amount %f != journal amount %f", entry.Amount, journalAmount)
		_ = h.store.UpdateMatch(r.Context(), id, req.TargetJournalID, "MISMATCH", &reason)
		entry.TargetJournalID = &req.TargetJournalID
		entry.MatchStatus = "MISMATCH"
		entry.MismatchReason = &reason
		h.publisher.PublishMismatchDetected(r.Context(), correlationID, principalID, *entry, reason)
		writeJSON(w, http.StatusUnprocessableEntity, domain.MatchEntryResponse{
			IntercompanyEntryID: id,
			MatchStatus:         "MISMATCH",
			MismatchReason:      &reason,
		})
		return
	}

	// Match Successful
	if err := h.store.UpdateMatch(r.Context(), id, req.TargetJournalID, "MATCHED", nil); err != nil {
		h.log.Error("failed to update match status in store", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	entry.TargetJournalID = &req.TargetJournalID
	entry.MatchStatus = "MATCHED"
	entry.MismatchReason = nil

	h.publisher.PublishEntryPosted(r.Context(), correlationID, principalID, *entry)
	writeJSON(w, http.StatusOK, domain.MatchEntryResponse{
		IntercompanyEntryID: id,
		MatchStatus:         "MATCHED",
	})
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

	if err := h.store.AcknowledgeCounterparty(r.Context(), id, principalID); err != nil {
		if errors.Is(err, domain.ErrInvalidPairTransition) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
			return
		}
		h.log.Error("failed to acknowledge counterparty", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
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

	if err := h.store.DisputeIntercompany(r.Context(), id, principalID, req.Reason); err != nil {
		if errors.Is(err, domain.ErrInvalidPairTransition) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
			return
		}
		h.log.Error("failed to dispute intercompany pair", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
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

	if err := h.store.ResolveMismatch(r.Context(), id, principalID, req.ResolutionNote); err != nil {
		if errors.Is(err, domain.ErrInvalidPairTransition) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
			return
		}
		h.log.Error("failed to resolve intercompany mismatch", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"match_status": domain.MatchStatusResolved})
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
