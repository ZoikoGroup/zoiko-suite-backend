// Package handler exposes bank-reconciliation-svc's REST API.
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

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	"zoiko.io/bank-reconciliation-svc/internal/ledger"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateStatementLine(ctx context.Context, l *domain.StatementLine) error
	GetStatementLine(ctx context.Context, statementLineID string) (*domain.StatementLine, error)
	ListStatementLines(ctx context.Context, filter domain.ListStatementLinesFilter) ([]domain.StatementLine, error)
	MatchStatementLine(ctx context.Context, tenantID, statementLineID, journalID, actorPrincipalID string) error
	FlagException(ctx context.Context, tenantID, statementLineID, reason, actorPrincipalID string) error
	CountUnmatched(ctx context.Context, tenantID, bankAccountID, statementDate string) (int, error)
}

// Publisher is the event-publishing contract the handler depends on.
type Publisher interface {
	PublishStatementIngested(ctx context.Context, l domain.StatementLine)
	PublishReconciliationMatched(ctx context.Context, l domain.StatementLine)
	PublishReconciliationExceptionRaised(ctx context.Context, l domain.StatementLine)
	PublishReconciliationCompleted(ctx context.Context, tenantID, bankAccountID, statementDate string)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Action types checked against authorization-svc.
const (
	actionIngestStatementLine = "BANKREC_STATEMENT_INGEST"
	actionMatch               = "BANKREC_MATCH"
	actionFlagException       = "BANKREC_FLAG_EXCEPTION"
	actionCompleteStatement   = "BANKREC_COMPLETE_STATEMENT"
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
	r.Route("/v1/statement-lines", func(r chi.Router) {
		r.Post("/", h.CreateStatementLine)
		r.Get("/", h.ListStatementLines)
		r.Get("/{statement_line_id}", h.GetStatementLine)
		r.Post("/{statement_line_id}/match", h.MatchStatementLine)
		r.Post("/{statement_line_id}/exception", h.FlagException)
	})
	r.Post("/v1/bank-accounts/{bank_account_id}/statements/{statement_date}/complete", h.CompleteStatement)
}

// ── POST /v1/statement-lines ─────────────────────────────────────────────────

func (h *Handler) CreateStatementLine(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateStatementLineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if missing := requiredFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	if req.Amount == 0 {
		writeError(w, http.StatusBadRequest, "invalid_field", "amount must not be zero")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionIngestStatementLine); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	l := &domain.StatementLine{
		StatementLineID: uuid.NewString(),
		TenantID:        req.TenantID,
		LegalEntityID:   req.LegalEntityID,
		BankAccountID:   req.BankAccountID,
		StatementDate:   req.StatementDate,
		Amount:          req.Amount,
		CurrencyCode:    req.CurrencyCode,
		BankReference:   req.BankReference,
		Status:          domain.StatementLineStatusUnmatched,
		CorrelationID:   req.CorrelationID,
	}
	if err := h.store.CreateStatementLine(r.Context(), l); err != nil {
		h.log.Error("CreateStatementLine: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	h.publisher.PublishStatementIngested(r.Context(), *l)
	writeJSON(w, http.StatusCreated, l)
}

// ── GET /v1/statement-lines/{statement_line_id} ──────────────────────────────

func (h *Handler) GetStatementLine(w http.ResponseWriter, r *http.Request) {
	statementLineID := chi.URLParam(r, "statement_line_id")
	l, err := h.store.GetStatementLine(r.Context(), statementLineID)
	if err != nil {
		h.log.Error("GetStatementLine: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if l == nil {
		writeError(w, http.StatusNotFound, "statement_line_not_found", "")
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// ── GET /v1/statement-lines ───────────────────────────────────────────────────

func (h *Handler) ListStatementLines(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := domain.ListStatementLinesFilter{
		TenantID:      q.Get("tenant_id"),
		BankAccountID: q.Get("bank_account_id"),
		StatementDate: q.Get("statement_date"),
		Status:        q.Get("status"),
	}
	if filter.TenantID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "tenant_id")
		return
	}
	list, err := h.store.ListStatementLines(r.Context(), filter)
	if err != nil {
		h.log.Error("ListStatementLines: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/statement-lines/{statement_line_id}/match ───────────────────────
//
// UNMATCHED|EXCEPTION -> MATCHED. The caller names a general-ledger-svc
// journal_id; this handler verifies it independently — real FINALIZED
// status, matching legal entity, and matching net amount — before ever
// persisting the match. Never trusts the claim at face value.
func (h *Handler) MatchStatementLine(w http.ResponseWriter, r *http.Request) {
	var req domain.MatchStatementLineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.JournalID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "journal_id")
		return
	}

	statementLineID := chi.URLParam(r, "statement_line_id")
	l, err := h.store.GetStatementLine(r.Context(), statementLineID)
	if err != nil {
		h.log.Error("MatchStatementLine: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if l == nil {
		writeError(w, http.StatusNotFound, "statement_line_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionMatch); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.verifyJournalMatches(r.Context(), *l, req.JournalID); err != nil {
		if errors.Is(err, domain.ErrLedgerVerificationFailed) {
			writeError(w, http.StatusBadRequest, "ledger_verification_failed", err.Error())
		} else {
			h.log.Error("ledger verification unavailable — failing closed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "ledger_service_unavailable", "")
		}
		return
	}

	if err := h.store.MatchStatementLine(r.Context(), l.TenantID, statementLineID, req.JournalID, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	l.Status = domain.StatementLineStatusMatched
	l.MatchedJournalID = &req.JournalID
	l.MatchedByPrincipalID = &principalID
	h.publisher.PublishReconciliationMatched(r.Context(), *l)
	writeJSON(w, http.StatusOK, l)
}

// verifyJournalMatches checks journalID against general-ledger-svc: it must
// exist, be FINALIZED, belong to the statement line's legal entity, and its
// net amount must match the statement line's amount (compared by magnitude —
// debit/credit sign-convention reconciliation is a documented v1 gap, see
// package doc).
func (h *Handler) verifyJournalMatches(ctx context.Context, l domain.StatementLine, journalID string) error {
	j, err := h.ledger.GetJournal(ctx, l.TenantID, journalID)
	if err != nil {
		if errors.Is(err, ledger.ErrJournalNotFound) {
			return domain.ErrLedgerVerificationFailed
		}
		return domain.ErrLedgerServiceUnavailable
	}
	if j.Status != "FINALIZED" {
		return domain.ErrLedgerVerificationFailed
	}
	if j.LegalEntityID != l.LegalEntityID {
		return domain.ErrLedgerVerificationFailed
	}
	if math.Abs(math.Abs(j.NetAmount())-math.Abs(l.Amount)) > amountEpsilon {
		return domain.ErrLedgerVerificationFailed
	}
	return nil
}

// ── POST /v1/statement-lines/{statement_line_id}/exception ───────────────────
//
// UNMATCHED -> EXCEPTION. Requires a reason — an exception with no stated
// reason isn't a useful queue item for whoever investigates it later.
func (h *Handler) FlagException(w http.ResponseWriter, r *http.Request) {
	var req domain.FlagExceptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}

	statementLineID := chi.URLParam(r, "statement_line_id")
	l, err := h.store.GetStatementLine(r.Context(), statementLineID)
	if err != nil {
		h.log.Error("FlagException: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if l == nil {
		writeError(w, http.StatusNotFound, "statement_line_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionFlagException); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.FlagException(r.Context(), l.TenantID, statementLineID, req.Reason, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	l.Status = domain.StatementLineStatusException
	l.ExceptionReason = &req.Reason
	l.FlaggedByPrincipalID = &principalID
	h.publisher.PublishReconciliationExceptionRaised(r.Context(), *l)
	writeJSON(w, http.StatusOK, l)
}

// ── POST /v1/bank-accounts/{bank_account_id}/statements/{statement_date}/complete ──
//
// Publishes reconciliation.completed once no line for this bank account +
// statement date is still UNMATCHED. This is a derived signal, not stored
// state — there's no separate "reconciliation batch" record in v1.
func (h *Handler) CompleteStatement(w http.ResponseWriter, r *http.Request) {
	bankAccountID := chi.URLParam(r, "bank_account_id")
	statementDate := chi.URLParam(r, "statement_date")
	tenantID := r.URL.Query().Get("tenant_id")
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "tenant_id")
		return
	}
	// A bank account belongs to exactly one legal entity (data model,
	// 04-data-model.md §8.1) — required explicitly here rather than
	// inferred, since authorization-svc's /v1/authorize rejects an empty
	// legal_entity_id outright (400), and this endpoint has no single
	// statement line to read one from.
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionCompleteStatement); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	count, err := h.store.CountUnmatched(r.Context(), tenantID, bankAccountID, statementDate)
	if err != nil {
		h.log.Error("CompleteStatement: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if count > 0 {
		writeError(w, http.StatusUnprocessableEntity, "statement_incomplete", domain.ErrStatementIncomplete.Error())
		return
	}

	h.publisher.PublishReconciliationCompleted(r.Context(), tenantID, bankAccountID, statementDate)
	writeJSON(w, http.StatusOK, map[string]string{
		"tenant_id":       tenantID,
		"bank_account_id": bankAccountID,
		"statement_date":  statementDate,
		"status":          "COMPLETED",
	})
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

func requiredFieldMissing(req domain.CreateStatementLineRequest) string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.BankAccountID == "":
		return "bank_account_id"
	case req.StatementDate.IsZero():
		return "statement_date"
	case req.CurrencyCode == "":
		return "currency_code"
	case req.BankReference == "":
		return "bank_reference"
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
