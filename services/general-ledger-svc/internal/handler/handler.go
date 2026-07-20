// Package handler exposes general-ledger-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/close"
	"zoiko.io/general-ledger-svc/internal/domain"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateJournal(ctx context.Context, h *domain.JournalHeader, lines []domain.JournalLine) (resultLines []domain.JournalLine, created bool, err error)
	GetJournal(ctx context.Context, journalID string) (*domain.JournalHeader, []domain.JournalLine, error)
	ListJournals(ctx context.Context, filter domain.ListJournalsFilter) ([]domain.JournalHeader, error)
	TransitionJournal(ctx context.Context, tenantID, journalID string, fromStatus, toStatus domain.JournalStatus, actorPrincipalID string) error
	SumLines(ctx context.Context, tenantID, journalID string) (debitTotal, creditTotal float64, err error)
}

// Publisher is the event-publishing contract the handler depends on.
type Publisher interface {
	PublishJournalCreated(ctx context.Context, h domain.JournalHeader)
	PublishJournalValidated(ctx context.Context, h domain.JournalHeader)
	PublishJournalPosted(ctx context.Context, h domain.JournalHeader)
	PublishJournalReversed(ctx context.Context, h domain.JournalHeader, reversingJournalID string)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Action types checked against authorization-svc. A single, platform-wide
// action type per journal lifecycle stage — nothing in the docs specifies
// finer-grained codes for v1.
const (
	actionCreateJournal   = "GL_JOURNAL_CREATE"
	actionValidateJournal = "GL_JOURNAL_VALIDATE"
	actionPostJournal     = "GL_JOURNAL_POST"
	actionReverseJournal  = "GL_JOURNAL_REVERSE"
)

type Handler struct {
	store       Store
	publisher   Publisher
	authz       AuthZClient
	closeClient close.Client
	log         *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, closeClient close.Client, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, closeClient: closeClient, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/journals", func(r chi.Router) {
		r.Post("/", h.CreateJournal)
		r.Get("/", h.ListJournals)
		r.Get("/{journal_id}", h.GetJournal)
		r.Post("/{journal_id}/validate", h.ValidateJournal)
		r.Post("/{journal_id}/post", h.PostJournal)
		r.Post("/{journal_id}/reverse", h.ReverseJournal)
	})
}

// ── POST /v1/journals ────────────────────────────────────────────────────────

func (h *Handler) CreateJournal(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateJournalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if missing := requiredJournalFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	if len(req.Lines) == 0 {
		writeError(w, http.StatusBadRequest, "no_lines", domain.ErrNoLines.Error())
		return
	}
	for _, l := range req.Lines {
		if !exactlyOneNonZero(l.DebitAmount, l.CreditAmount) {
			writeError(w, http.StatusBadRequest, "invalid_line", domain.ErrInvalidLine.Error())
			return
		}
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCreateJournal); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Enforce Period Lock Check
	if err := h.closeClient.CheckPeriodOpen(r.Context(), req.TenantID, req.LegalEntityID, req.FiscalPeriod); err != nil {
		if errors.Is(err, domain.ErrPeriodLocked) {
			writeError(w, http.StatusPreconditionFailed, "period_locked", err.Error())
		} else {
			writeError(w, http.StatusServiceUnavailable, "close_check_failed", err.Error())
		}
		return
	}

	header := &domain.JournalHeader{
		JournalID:            uuid.NewString(),
		TenantID:             req.TenantID,
		LegalEntityID:        req.LegalEntityID,
		FiscalPeriod:         req.FiscalPeriod,
		Status:               domain.JournalStatusPending,
		Description:          req.Description,
		CreatedByPrincipalID: principalID,
		CorrelationID:        req.CorrelationID,
	}
	lines := make([]domain.JournalLine, len(req.Lines))
	for i, l := range req.Lines {
		lines[i] = domain.JournalLine{
			AccountCode:  l.AccountCode,
			DebitAmount:  l.DebitAmount,
			CreditAmount: l.CreditAmount,
			Description:  l.Description,
		}
	}

	resultLines, created, err := h.store.CreateJournal(r.Context(), header, lines)
	if err != nil {
		h.log.Error("CreateJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	// created=false means this correlation_id was already used — a client
	// retry after a network timeout, not a genuinely new journal. Return
	// the original journal with 200, not a duplicate with 201.
	if !created {
		writeJSON(w, http.StatusOK, domain.JournalWithLines{JournalHeader: *header, Lines: resultLines})
		return
	}

	h.publisher.PublishJournalCreated(r.Context(), *header)
	writeJSON(w, http.StatusCreated, domain.JournalWithLines{JournalHeader: *header, Lines: resultLines})
}

// ── GET /v1/journals/{journal_id} ────────────────────────────────────────────

func (h *Handler) GetJournal(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "journal_id")
	header, lines, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("GetJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if header == nil {
		writeError(w, http.StatusNotFound, "journal_not_found", "")
		return
	}
	writeJSON(w, http.StatusOK, domain.JournalWithLines{JournalHeader: *header, Lines: lines})
}

// ── GET /v1/journals ──────────────────────────────────────────────────────────

func (h *Handler) ListJournals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := domain.ListJournalsFilter{
		TenantID:      q.Get("tenant_id"),
		LegalEntityID: q.Get("legal_entity_id"),
		FiscalPeriod:  q.Get("fiscal_period"),
		Status:        q.Get("status"),
	}
	if filter.TenantID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "tenant_id")
		return
	}
	journals, err := h.store.ListJournals(r.Context(), filter)
	if err != nil {
		h.log.Error("ListJournals: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, journals)
}

// ── POST /v1/journals/{journal_id}/validate ──────────────────────────────────
//
// PENDING -> VALIDATED. Enforces the double-entry invariant: sum(debits)
// must equal sum(credits) across every line, otherwise the journal is
// rejected outright — it never silently becomes VALIDATED unbalanced.
func (h *Handler) ValidateJournal(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "journal_id")
	header, _, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("ValidateJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if header == nil {
		writeError(w, http.StatusNotFound, "journal_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, header.LegalEntityID, actionValidateJournal); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	debitTotal, creditTotal, err := h.store.SumLines(r.Context(), header.TenantID, journalID)
	if err != nil {
		h.log.Error("ValidateJournal: failed to sum lines", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if debitTotal != creditTotal {
		writeError(w, http.StatusUnprocessableEntity, "unbalanced_journal", domain.ErrUnbalancedJournal.Error())
		return
	}

	if err := h.store.TransitionJournal(r.Context(), header.TenantID, journalID,
		domain.JournalStatusPending, domain.JournalStatusValidated, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	header.Status = domain.JournalStatusValidated
	h.publisher.PublishJournalValidated(r.Context(), *header)
	writeJSON(w, http.StatusOK, header)
}

// ── POST /v1/journals/{journal_id}/post ──────────────────────────────────────
//
// VALIDATED -> FINALIZED. This is the immutable-posting step: once FINALIZED,
// the journal's lines may never be edited — corrections only via reversal.
func (h *Handler) PostJournal(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "journal_id")
	header, _, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("PostJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if header == nil {
		writeError(w, http.StatusNotFound, "journal_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, header.LegalEntityID, actionPostJournal); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Enforce Period Lock Check
	if err := h.closeClient.CheckPeriodOpen(r.Context(), header.TenantID, header.LegalEntityID, header.FiscalPeriod); err != nil {
		if errors.Is(err, domain.ErrPeriodLocked) {
			writeError(w, http.StatusPreconditionFailed, "period_locked", err.Error())
		} else {
			writeError(w, http.StatusServiceUnavailable, "close_check_failed", err.Error())
		}
		return
	}

	if err := h.store.TransitionJournal(r.Context(), header.TenantID, journalID,
		domain.JournalStatusValidated, domain.JournalStatusFinalized, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	header.Status = domain.JournalStatusFinalized
	h.publisher.PublishJournalPosted(r.Context(), *header)
	writeJSON(w, http.StatusOK, header)
}

// ── POST /v1/journals/{journal_id}/reverse ───────────────────────────────────
//
// Only a FINALIZED journal may be reversed. Reversal never edits the
// original journal's lines — it creates a brand-new journal whose lines are
// the exact debit/credit inverse of the original, already FINALIZED (a
// reversal is itself an authoritative posting, not a draft), and marks the
// original REVERSED. This is the platform's only sanctioned "correction"
// mechanism for posted financial data.
func (h *Handler) ReverseJournal(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "journal_id")

	var req domain.ReverseJournalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}
	if req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "correlation_id")
		return
	}

	header, lines, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("ReverseJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if header == nil {
		writeError(w, http.StatusNotFound, "journal_not_found", "")
		return
	}
	if header.Status != domain.JournalStatusFinalized {
		writeError(w, http.StatusUnprocessableEntity, "only_finalized_reversible", domain.ErrOnlyFinalizedReversible.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, header.LegalEntityID, actionReverseJournal); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Enforce Period Lock Check
	if err := h.closeClient.CheckPeriodOpen(r.Context(), header.TenantID, header.LegalEntityID, header.FiscalPeriod); err != nil {
		if errors.Is(err, domain.ErrPeriodLocked) {
			writeError(w, http.StatusPreconditionFailed, "period_locked", err.Error())
		} else {
			writeError(w, http.StatusServiceUnavailable, "close_check_failed", err.Error())
		}
		return
	}

	reversalID := journalID
	reversingHeader := &domain.JournalHeader{
		JournalID:            uuid.NewString(),
		TenantID:             header.TenantID,
		LegalEntityID:        header.LegalEntityID,
		FiscalPeriod:         header.FiscalPeriod,
		Status:               domain.JournalStatusFinalized,
		ReversalOfJournalID:  &reversalID,
		Description:          "Reversal of " + journalID + ": " + req.Reason,
		CreatedByPrincipalID: principalID,
		PostedByPrincipalID:  &principalID,
		CorrelationID:        req.CorrelationID,
	}
	reversingLines := make([]domain.JournalLine, len(lines))
	for i, l := range lines {
		reversingLines[i] = domain.JournalLine{
			AccountCode:  l.AccountCode,
			DebitAmount:  l.CreditAmount, // exact debit/credit inverse
			CreditAmount: l.DebitAmount,
			Description:  l.Description,
		}
	}

	resultLines, created, err := h.store.CreateJournal(r.Context(), reversingHeader, reversingLines)
	if err != nil {
		h.log.Error("ReverseJournal: failed to create reversing journal", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	// created=false means this correlation_id already reversed this journal
	// on an earlier call — a retry, not a new reversal. The original
	// journal is already REVERSED; re-running TransitionJournal would
	// correctly fail with ErrInvalidTransition (it's not FINALIZED anymore)
	// and wrongly report a retry as an error. Return the original result.
	if !created {
		writeJSON(w, http.StatusOK, domain.JournalWithLines{JournalHeader: *reversingHeader, Lines: resultLines})
		return
	}

	if err := h.store.TransitionJournal(r.Context(), header.TenantID, journalID,
		domain.JournalStatusFinalized, domain.JournalStatusReversed, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	h.publisher.PublishJournalReversed(r.Context(), *header, reversingHeader.JournalID)
	writeJSON(w, http.StatusCreated, domain.JournalWithLines{JournalHeader: *reversingHeader, Lines: resultLines})
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
		h.log.Error("TransitionJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

func requiredJournalFieldMissing(req domain.CreateJournalRequest) string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.FiscalPeriod == "":
		return "fiscal_period"
	case req.CorrelationID == "":
		// Required, not optional: correlation_id is the idempotency key
		// that lets a client retry safely after a network timeout without
		// double-posting a journal. An idempotency key nobody's required
		// to send protects nobody.
		return "correlation_id"
	default:
		return ""
	}
}

func exactlyOneNonZero(debit, credit float64) bool {
	return (debit > 0 && credit == 0) || (credit > 0 && debit == 0)
}

// requirePrincipal reads the caller's identity from X-Principal-Id — set by
// gateway-auth-svc's ForwardAuth verification (or Traefik, in a real
// deployment) after checking the signed IdentityContextEnvelope JWT. This
// service never decodes a JWT itself, matching schema-registry-svc's
// pattern exactly: identity is resolved once, upstream of every backend,
// not re-derived independently by each service (03-microservices.md §9.1
// critical constraint). A request with no resolved principal never passed
// identity verification — fail closed with 401, it is never treated as an
// anonymous/system actor.
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
