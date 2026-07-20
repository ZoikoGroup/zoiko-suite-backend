// Package handler exposes intercompany-accounting-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	"zoiko.io/intercompany-accounting-svc/internal/ledger"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateEntry(ctx context.Context, e *domain.IntercompanyEntry) error
	GetEntry(ctx context.Context, intercompanyEntryID string) (*domain.IntercompanyEntry, error)
	ListEntries(ctx context.Context, filter domain.ListEntriesFilter) ([]domain.IntercompanyEntry, error)
	MatchEntry(ctx context.Context, tenantID, intercompanyEntryID, targetJournalEntryID, actorPrincipalID string) error
	MismatchEntry(ctx context.Context, tenantID, intercompanyEntryID, targetJournalEntryID, reason string) error
}

// Publisher is the event-publishing contract the handler depends on.
type Publisher interface {
	PublishEntryCreated(ctx context.Context, e domain.IntercompanyEntry)
	PublishEntryPosted(ctx context.Context, e domain.IntercompanyEntry)
	PublishMismatchDetected(ctx context.Context, e domain.IntercompanyEntry)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Action types checked against authorization-svc.
const (
	actionCreateEntry = "INTERCO_ENTRY_CREATE"
	actionLinkTarget  = "INTERCO_LINK_TARGET"
)

// amountEpsilon is the tolerance for comparing two NUMERIC(18,2) values that
// have made a round trip through float64 — well below one cent, so it never
// masks a genuine mismatch.
const amountEpsilon = 0.005

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	ledger    ledger.Client
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, ledgerClient ledger.Client, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, ledger: ledgerClient, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/intercompany-entries", func(r chi.Router) {
		r.Post("/", h.CreateEntry)
		r.Get("/", h.ListEntries)
		r.Get("/{intercompany_entry_id}", h.GetEntry)
		r.Post("/{intercompany_entry_id}/link-target", h.LinkTargetJournal)
	})
}

// ── POST /v1/intercompany-entries ────────────────────────────────────────────

func (h *Handler) CreateEntry(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateIntercompanyEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if missing := requiredFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_field", "amount must be greater than zero")
		return
	}
	if req.SourceLegalEntityID == req.TargetLegalEntityID {
		writeError(w, http.StatusBadRequest, "invalid_field", "source and target legal entities must differ")
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

	e := &domain.IntercompanyEntry{
		IntercompanyEntryID:  uuid.NewString(),
		TenantID:             req.TenantID,
		SourceLegalEntityID:  req.SourceLegalEntityID,
		TargetLegalEntityID:  req.TargetLegalEntityID,
		SourceJournalEntryID: req.SourceJournalEntryID,
		Amount:               req.Amount,
		CurrencyCode:         req.CurrencyCode,
		Description:          req.Description,
		MatchStatus:          domain.MatchStatusPending,
		CreatedByPrincipalID: principalID,
		CorrelationID:        req.CorrelationID,
	}
	if err := h.store.CreateEntry(r.Context(), e); err != nil {
		h.log.Error("CreateEntry: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	h.publisher.PublishEntryCreated(r.Context(), *e)
	writeJSON(w, http.StatusCreated, e)
}

// ── GET /v1/intercompany-entries/{intercompany_entry_id} ────────────────────

func (h *Handler) GetEntry(w http.ResponseWriter, r *http.Request) {
	entryID := chi.URLParam(r, "intercompany_entry_id")
	e, err := h.store.GetEntry(r.Context(), entryID)
	if err != nil {
		h.log.Error("GetEntry: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if e == nil {
		writeError(w, http.StatusNotFound, "entry_not_found", "")
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// ── GET /v1/intercompany-entries ─────────────────────────────────────────────

func (h *Handler) ListEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := domain.ListEntriesFilter{
		TenantID:    q.Get("tenant_id"),
		MatchStatus: q.Get("match_status"),
	}
	if filter.TenantID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "tenant_id")
		return
	}
	list, err := h.store.ListEntries(r.Context(), filter)
	if err != nil {
		h.log.Error("ListEntries: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/intercompany-entries/{intercompany_entry_id}/link-target ───────
//
// PENDING|MISMATCHED -> MATCHED or MISMATCHED. The caller names a
// general-ledger-svc journal_id for the target entity's side; this handler
// verifies BOTH the source and target journals independently — real
// FINALIZED status, correct legal entity on each side, and agreeing net
// amount — before deciding the outcome. A failed verification is not a 4xx
// error, it's a valid MISMATCHED result, returned 200.
func (h *Handler) LinkTargetJournal(w http.ResponseWriter, r *http.Request) {
	var req domain.LinkTargetJournalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.TargetJournalEntryID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "target_journal_entry_id")
		return
	}

	entryID := chi.URLParam(r, "intercompany_entry_id")
	e, err := h.store.GetEntry(r.Context(), entryID)
	if err != nil {
		h.log.Error("LinkTargetJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if e == nil {
		writeError(w, http.StatusNotFound, "entry_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, e.TargetLegalEntityID, actionLinkTarget); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	mismatchReason, err := h.verifyBothJournals(r.Context(), *e, req.TargetJournalEntryID)
	if err != nil {
		h.log.Error("ledger verification unavailable — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "ledger_service_unavailable", "")
		return
	}

	if mismatchReason != "" {
		if err := h.store.MismatchEntry(r.Context(), e.TenantID, entryID, req.TargetJournalEntryID, mismatchReason); err != nil {
			h.handleTransitionErr(w, err)
			return
		}
		e.MatchStatus = domain.MatchStatusMismatched
		e.TargetJournalEntryID = &req.TargetJournalEntryID
		e.MismatchReason = &mismatchReason
		h.publisher.PublishMismatchDetected(r.Context(), *e)
		writeJSON(w, http.StatusOK, e)
		return
	}

	if err := h.store.MatchEntry(r.Context(), e.TenantID, entryID, req.TargetJournalEntryID, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}
	e.MatchStatus = domain.MatchStatusMatched
	e.TargetJournalEntryID = &req.TargetJournalEntryID
	e.MatchedByPrincipalID = &principalID
	h.publisher.PublishEntryPosted(r.Context(), *e)
	writeJSON(w, http.StatusOK, e)
}

// verifyBothJournals checks the source and target journals against
// general-ledger-svc independently. Returns a non-empty mismatchReason
// (never an error) if either check fails verification — that's a valid
// MISMATCHED outcome, not a fault. Returns an error only when
// general-ledger-svc itself couldn't be reached or answered unexpectedly,
// which must fail closed rather than be treated as a mismatch.
func (h *Handler) verifyBothJournals(ctx context.Context, e domain.IntercompanyEntry, targetJournalID string) (string, error) {
	srcJournal, err := h.ledger.GetJournal(ctx, e.TenantID, e.SourceJournalEntryID)
	if err != nil {
		if errors.Is(err, ledger.ErrJournalNotFound) {
			return "source journal no longer exists", nil
		}
		return "", err
	}
	tgtJournal, err := h.ledger.GetJournal(ctx, e.TenantID, targetJournalID)
	if err != nil {
		if errors.Is(err, ledger.ErrJournalNotFound) {
			return "target journal does not exist", nil
		}
		return "", err
	}

	if srcJournal.Status != "FINALIZED" {
		return "source journal is not FINALIZED", nil
	}
	if tgtJournal.Status != "FINALIZED" {
		return "target journal is not FINALIZED", nil
	}
	if srcJournal.LegalEntityID != e.SourceLegalEntityID {
		return "source journal belongs to a different legal entity", nil
	}
	if tgtJournal.LegalEntityID != e.TargetLegalEntityID {
		return "target journal belongs to a different legal entity", nil
	}
	if math.Abs(math.Abs(srcJournal.NetAmount())-e.Amount) > amountEpsilon {
		return "source journal amount does not match the entry amount", nil
	}
	if math.Abs(math.Abs(tgtJournal.NetAmount())-e.Amount) > amountEpsilon {
		return "target journal amount does not match the entry amount", nil
	}

	return "", nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAuthorizationDenied):
		writeError(w, http.StatusForbidden, "authorization_denied", "")
	default:
		h.log.Error("authorization check failed — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization_service_unavailable", "")
	}
}

func (h *Handler) handleTransitionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidTransition.Error())
	default:
		h.log.Error("transition: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

func requiredFieldMissing(req domain.CreateIntercompanyEntryRequest) string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.SourceLegalEntityID == "":
		return "source_legal_entity_id"
	case req.TargetLegalEntityID == "":
		return "target_legal_entity_id"
	case req.SourceJournalEntryID == "":
		return "source_journal_entry_id"
	case req.CurrencyCode == "":
		return "currency_code"
	case req.Description == "":
		return "description"
	default:
		return ""
	}
}

// requirePrincipal reads the caller's identity from X-Principal-Id — set by
// gateway-auth-svc's ForwardAuth verification after checking the signed
// IdentityContextEnvelope JWT. This service never decodes a JWT itself,
// matching every other Phase 3 service's pattern. A request with no
// resolved principal never passed identity verification — fail closed with
// 401.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", domain.ErrIdentityMissing.Error())
		return "", false
	}
	return principalID, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type errorResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorResponse{Error: code, Detail: detail})
}
