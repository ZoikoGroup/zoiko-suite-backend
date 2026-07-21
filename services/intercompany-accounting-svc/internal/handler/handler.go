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
}

type Publisher interface {
	PublishEntryCreated(ctx context.Context, correlationID string, entry domain.IntercompanyEntry)
	PublishEntryPosted(ctx context.Context, correlationID string, entry domain.IntercompanyEntry)
	PublishMismatchDetected(ctx context.Context, correlationID string, entry domain.IntercompanyEntry, reason string)
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

	h.publisher.PublishEntryCreated(r.Context(), correlationID, *entry)
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

	if entry.MatchStatus == "MATCHED" {
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
		h.publisher.PublishMismatchDetected(r.Context(), correlationID, *entry, reason)
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
		h.publisher.PublishMismatchDetected(r.Context(), correlationID, *entry, reason)
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
		h.publisher.PublishMismatchDetected(r.Context(), correlationID, *entry, reason)
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

	h.publisher.PublishEntryPosted(r.Context(), correlationID, *entry)
	writeJSON(w, http.StatusOK, domain.MatchEntryResponse{
		IntercompanyEntryID: id,
		MatchStatus:         "MATCHED",
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
