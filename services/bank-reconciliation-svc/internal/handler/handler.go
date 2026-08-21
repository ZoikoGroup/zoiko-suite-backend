// Package handler exposes bank-reconciliation-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	"zoiko.io/bank-reconciliation-svc/internal/ledger"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateStatementLine(ctx context.Context, l *domain.StatementLine) (created bool, err error)
	GetStatementLine(ctx context.Context, statementLineID string) (*domain.StatementLine, error)
	ListStatementLines(ctx context.Context, filter domain.ListStatementLinesFilter) ([]domain.StatementLine, error)
	MatchStatementLine(ctx context.Context, tenantID, statementLineID, journalID, actorPrincipalID string) error
	FlagException(ctx context.Context, tenantID, statementLineID, reason, actorPrincipalID string) error
	CountUnmatched(ctx context.Context, tenantID, bankAccountID, statementDate string) (int, error)
	StatementLegalEntities(ctx context.Context, tenantID, bankAccountID, statementDate string) ([]string, error)
}

// Publisher is the event-publishing contract the handler depends on.
type Publisher interface {
	PublishStatementIngested(ctx context.Context, l domain.StatementLine, actorID string)
	PublishReconciliationMatched(ctx context.Context, l domain.StatementLine)
	PublishReconciliationExceptionRaised(ctx context.Context, l domain.StatementLine)
	PublishReconciliationCompleted(ctx context.Context, correlationID, tenantID, actorID, bankAccountID, statementDate string)
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

// maxBodyBytes bounds a request body. Every route here takes a small,
// fixed-shape document; without a bound the only limit was available memory.
const maxBodyBytes = 64 << 10

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
	if !decodeBody(w, r, &req) {
		return
	}
	if missing := requiredFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	if req.Amount == 0 {
		// A bank statement line of zero has no direction and reconciles
		// against nothing.
		writeError(w, http.StatusBadRequest, "invalid_field", "amount must not be zero")
		return
	}
	if len(req.CurrencyCode) != 3 {
		// currency_code is VARCHAR(3). Anything longer reached Postgres and
		// came back as SQLSTATE 22001, which the store reported as an
		// outage — a 503 for a typo.
		writeError(w, http.StatusBadRequest, "invalid_field", "currency_code must be a 3-letter ISO 4217 code")
		return
	}

	// The verified scope, not the body, decides which register this row lands
	// in. A body naming a different tenant is refused rather than quietly
	// overridden: silently substituting would hide a genuine bug in a caller
	// that believes it is writing somewhere else.
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if req.TenantID != "" && req.TenantID != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
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

	cashAccount := req.GLCashAccountCode
	l := &domain.StatementLine{
		StatementLineID:   uuid.NewString(),
		TenantID:          tenantID,
		LegalEntityID:     req.LegalEntityID,
		BankAccountID:     req.BankAccountID,
		StatementDate:     req.StatementDate,
		Amount:            req.Amount,
		CurrencyCode:      req.CurrencyCode,
		BankReference:     req.BankReference,
		Status:            domain.StatementLineStatusUnmatched,
		GLCashAccountCode: &cashAccount,
		CorrelationID:     req.CorrelationID,
	}
	created, err := h.store.CreateStatementLine(r.Context(), l)
	if err != nil {
		h.writeStoreErr(w, "CreateStatementLine", err)
		return
	}
	if !created {
		// Replay of a prior request with the same correlation_id — return
		// the original line, do not re-publish the ingested event.
		writeJSON(w, http.StatusOK, l)
		return
	}

	h.publisher.PublishStatementIngested(r.Context(), *l, principalID)
	writeJSON(w, http.StatusCreated, l)
}

// ── GET /v1/statement-lines/{statement_line_id} ──────────────────────────────

func (h *Handler) GetStatementLine(w http.ResponseWriter, r *http.Request) {
	// Checked here as well as in the store. A caller with no verified scope
	// used to be answered 404 statement_line_not_found — which is not what
	// happened, and reads as reassurance that the row is absent rather than
	// that the request was never scoped to look for it.
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	statementLineID := chi.URLParam(r, "statement_line_id")
	l, err := h.store.GetStatementLine(r.Context(), statementLineID)
	if err != nil {
		h.writeStoreErr(w, "GetStatementLine", err)
		return
	}
	if l == nil {
		writeError(w, http.StatusNotFound, "statement_line_not_found", "")
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// ── GET /v1/statement-lines ───────────────────────────────────────────────────
//
// Scoped to the caller's VERIFIED tenant. It used to be scoped to
// ?tenant_id — so naming somebody else's tenant returned their whole bank
// register. Reads are not separately authorized (the tenant scope is the
// boundary), matching general-ledger-svc, whose register this one reconciles
// against.
func (h *Handler) ListStatementLines(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, err := parseLimit(q.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_field", err.Error())
		return
	}
	filter := domain.ListStatementLinesFilter{
		TenantID:      tenantID,
		BankAccountID: q.Get("bank_account_id"),
		StatementDate: q.Get("statement_date"),
		Status:        q.Get("status"),
		Limit:         limit,
	}
	list, err := h.store.ListStatementLines(r.Context(), filter)
	if err != nil {
		h.writeStoreErr(w, "ListStatementLines", err)
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
	if !decodeBody(w, r, &req) {
		return
	}
	if req.JournalID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "journal_id")
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	statementLineID := chi.URLParam(r, "statement_line_id")
	l, err := h.store.GetStatementLine(r.Context(), statementLineID)
	if err != nil {
		h.writeStoreErr(w, "MatchStatementLine", err)
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
		switch {
		case errors.Is(err, domain.ErrCashAccountUnknown):
			writeError(w, http.StatusUnprocessableEntity, "cash_account_unknown", err.Error())
		case errors.Is(err, domain.ErrLedgerVerificationFailed):
			writeError(w, http.StatusBadRequest, "ledger_verification_failed", err.Error())
		default:
			h.log.Error("ledger verification unavailable — failing closed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "ledger_service_unavailable", "")
		}
		return
	}

	// The verified tenant, not the stored row's — the row was already read
	// under that scope, so they agree, and scoping a write by data read from
	// the database is a habit that stops being safe the moment the read
	// stops being scoped.
	if err := h.store.MatchStatementLine(r.Context(), tenantID, statementLineID, req.JournalID, principalID); err != nil {
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
// exist, be FINALIZED, belong to the statement line's legal entity, and it
// must have moved exactly the statement line's amount — in the same
// direction — through the ledger account that represents this bank account.
//
// The direction half is the part that was missing. The old check compared
// abs(journal net amount) with abs(statement amount), where "net amount" was
// the sum of one side of a balanced journal and so was unsigned by
// construction. A 500.00 withdrawal therefore reconciled cleanly against a
// journal recording a 500.00 deposit, and vice versa — the two errors a
// reconciliation exists to catch. Direction is now read from which side of
// the journal the cash line falls on: a net debit to the bank's own account
// is money in, a net credit is money out.
//
// Compared in exact cents. Both ends store NUMERIC(18,2); they are only
// float64 in transit because that is what JSON gives us, and the previous
// code papered over that with a 0.005 tolerance rather than removing it.
func (h *Handler) verifyJournalMatches(ctx context.Context, l domain.StatementLine, journalID string) error {
	if l.GLCashAccountCode == nil || *l.GLCashAccountCode == "" {
		// Refused, not weakened. Falling back to the magnitude comparison
		// here would quietly reinstate exactly the defect this check exists
		// to remove, on precisely the rows least able to afford it.
		return domain.ErrCashAccountUnknown
	}

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

	movedCents, touched := j.CashMovementCents(*l.GLCashAccountCode)
	if !touched {
		// The journal is real and FINALIZED but posts nothing at all to this
		// bank account, so it cannot be what this line represents.
		return domain.ErrLedgerVerificationFailed
	}
	if movedCents != ledger.ToCents(l.Amount) {
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
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}
	if len(req.Reason) > 500 {
		// exception_reason is VARCHAR(500); a longer one used to reach
		// Postgres and return 22001, reported to the caller as an outage.
		writeError(w, http.StatusBadRequest, "invalid_field", "reason must be 500 characters or fewer")
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	statementLineID := chi.URLParam(r, "statement_line_id")
	l, err := h.store.GetStatementLine(r.Context(), statementLineID)
	if err != nil {
		h.writeStoreErr(w, "FlagException", err)
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

	if err := h.store.FlagException(r.Context(), tenantID, statementLineID, req.Reason, principalID); err != nil {
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

	// The verified scope, not ?tenant_id. Reading it from the query string
	// meant a caller could count, complete, and publish
	// reconciliation.completed against another tenant's bank account.
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
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

	// Bind the authorization to the resource it just authorized. The check
	// above passed for the legal entity the CALLER named; nothing until now
	// connected that entity to the bank account being completed, so a
	// principal holding BANKREC_COMPLETE_STATEMENT over any one entity could
	// complete a statement belonging to another.
	entities, err := h.store.StatementLegalEntities(r.Context(), tenantID, bankAccountID, statementDate)
	if err != nil {
		h.writeStoreErr(w, "CompleteStatement", err)
		return
	}
	for _, e := range entities {
		if e != legalEntityID {
			writeError(w, http.StatusForbidden, "legal_entity_mismatch", domain.ErrLegalEntityMismatch.Error())
			return
		}
	}

	count, err := h.store.CountUnmatched(r.Context(), tenantID, bankAccountID, statementDate)
	if err != nil {
		h.writeStoreErr(w, "CompleteStatement", err)
		return
	}
	if count > 0 {
		writeError(w, http.StatusUnprocessableEntity, "statement_incomplete", plural(count)+" still unmatched: "+domain.ErrStatementIncomplete.Error())
		return
	}
	if len(entities) == 0 {
		// No lines at all for this account and date. Publishing
		// reconciliation.completed here would announce that a statement
		// nobody ingested has been reconciled — an unmatched count of zero
		// and an empty statement are not the same event.
		writeError(w, http.StatusNotFound, "statement_not_found", "no statement lines exist for this bank account and date")
		return
	}

	h.publisher.PublishReconciliationCompleted(r.Context(), r.Header.Get("X-Correlation-ID"), tenantID, principalID, bankAccountID, statementDate)
	writeJSON(w, http.StatusOK, map[string]string{
		"tenant_id":       tenantID,
		"bank_account_id": bankAccountID,
		"statement_date":  statementDate,
		"status":          "COMPLETED",
	})
}

// ── helpers ──────────────────────────────────────────────────────────────────

// requireTenant reads the caller's verified tenant scope, set from
// X-Tenant-Id by middleware.TenantContext. Absent means the request never
// passed gateway-auth-svc's ForwardAuth verification — fail closed with 401,
// which is what the caller needs to be told. Several routes previously
// answered 404 or read the tenant from a query parameter instead.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", domain.ErrTenantScopeMissing.Error())
		return "", false
	}
	return tenantID, true
}

// decodeBody bounds the request body and rejects unknown fields. Silently
// ignoring an unrecognised key means a caller that misspells one — or sends a
// field this version does not honour, such as a tenant_id it expects to be
// respected — is told its request succeeded exactly as written.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

// writeStoreErr separates a caller's malformed input from an actual outage.
// Everything used to be reported as 503 store_unavailable, so a mistyped uuid
// read as "the database is down" — both to whoever sent it and to anything
// watching this service's error rate.
func (h *Handler) writeStoreErr(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrTenantScopeMissing):
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", domain.ErrTenantScopeMissing.Error())
	case errors.Is(err, domain.ErrInvalidIdentifier):
		writeError(w, http.StatusBadRequest, "invalid_identifier", domain.ErrInvalidIdentifier.Error())
	default:
		h.log.Error(op+": store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

// parseLimit reads ?limit. A non-numeric value is refused rather than
// silently treated as "use the default" — a caller asking for 500 rows and
// receiving 200 with no indication of why would read the short page as the
// whole register.
func parseLimit(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("limit must be a positive integer")
	}
	if n <= 0 {
		return 0, errors.New("limit must be a positive integer")
	}
	if n > domain.MaxListLimit {
		return 0, fmt.Errorf("limit must be %d or fewer", domain.MaxListLimit)
	}
	return n, nil
}

func plural(n int) string {
	if n == 1 {
		return "1 line"
	}
	return strconv.Itoa(n) + " lines"
}

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

// tenant_id is deliberately absent from this list: the row's tenant comes
// from the verified X-Tenant-Id header, so requiring it in the body would
// demand a field the service does not act on.
func requiredFieldMissing(req domain.CreateStatementLineRequest) string {
	switch {
	case req.GLCashAccountCode == "":
		// Required since migration 000003. Without it no future match
		// against this line can verify direction, and the service refuses to
		// match rather than verifying something weaker — so accepting the
		// line without one would only defer the failure to a point where the
		// caller can no longer fix it.
		return "gl_cash_account_code"
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
	case req.CorrelationID == "":
		return "correlation_id"
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
