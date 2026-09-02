// Package handler exposes general-ledger-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/close"
	"zoiko.io/general-ledger-svc/internal/domain"
	svcenvelope "zoiko.io/general-ledger-svc/internal/envelope"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateJournal(ctx context.Context, h *domain.JournalHeader, lines []domain.JournalLine) (resultLines []domain.JournalLine, created bool, err error)
	GetJournal(ctx context.Context, journalID string) (*domain.JournalHeader, []domain.JournalLine, error)

	// GetJournalByCorrelationID resolves the idempotency key to the journal it
	// created, so a retried reversal can be recognised as one.
	GetJournalByCorrelationID(ctx context.Context, tenantID, correlationID string) (*domain.JournalHeader, []domain.JournalLine, error)
	ListJournals(ctx context.Context, filter domain.ListJournalsFilter) ([]domain.JournalHeader, error)
	TransitionJournal(ctx context.Context, tenantID, journalID string, fromStatus, toStatus domain.JournalStatus, actorPrincipalID string) error

	// ReverseJournal posts the reversing journal and marks the original
	// REVERSED in one transaction — see the store method's comment for why
	// these cannot be two calls.
	ReverseJournal(ctx context.Context, tenantID, originalJournalID string, reversing *domain.JournalHeader, reversingLines []domain.JournalLine, actorPrincipalID string) (resultLines []domain.JournalLine, created bool, err error)

	// SumLines returns exact minor units (cents), not float64 — the balance
	// invariant below is decided by exact equality.
	SumLines(ctx context.Context, tenantID, journalID string) (debitTotal, creditTotal int64, err error)
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

	// clock supplies the reversal posting date. Injectable because a test that
	// asserts which day a reversal posts to cannot do so against time.Now, and
	// a reversal landing in the wrong period is exactly the bug worth a test.
	// Nil means time.Now — see Handler.now.
	clock func() time.Time
}

func New(store Store, publisher Publisher, authz AuthZClient, closeClient close.Client, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, closeClient: closeClient, log: log}
}

// WithClock returns h with its clock replaced, for tests.
func (h *Handler) WithClock(clock func() time.Time) *Handler {
	h.clock = clock
	return h
}

func (h *Handler) now() time.Time {
	if h.clock != nil {
		return h.clock()
	}
	return time.Now()
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
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := requiredJournalFieldMissing(req); missing != "" {
		writeError(w, http.StatusBadRequest, "missing_field", missing)
		return
	}
	if code, detail := invalidJournalInput(req); code != "" {
		writeError(w, http.StatusBadRequest, code, detail)
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// The body names a tenant and so does the gateway. Only one of them was
	// verified. They used to be allowed to disagree, and the store then wrote
	// the row under the body's tenant while scoping the transaction to the
	// header's — filing a journal into a ledger the caller has no relationship
	// with, invisible to the tenant who created it.
	if req.TenantID != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
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

	// ACC-03's book input. §4 makes book_id server-resolvable, so an envelope
	// value is preferred over the body's — the header is set by the gateway
	// from verified context, the body by whoever wrote the JSON.
	env := svcenvelope.MustFromContext(r.Context())
	bookID, reportingBasis := req.BookID, req.ReportingBasis
	if env.BookID != "" {
		bookID = &env.BookID
	}
	if env.ReportingBasis != "" {
		reportingBasis = &env.ReportingBasis
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
		SourceEventID:        req.SourceEventID,
		GovernanceDecisionID: req.GovernanceDecisionID,

		JournalType:     req.JournalType,
		TransactionDate: req.TransactionDate,
		PostingDate:     req.PostingDate,
		CurrencyCode:    req.CurrencyCode,
		BookID:          bookID,
		ReportingBasis:  reportingBasis,
		EvidenceRefs:    mergeEvidenceRefs(req.EvidenceRefs, env.EvidenceRefs),
	}
	lines := make([]domain.JournalLine, len(req.Lines))
	for i, l := range req.Lines {
		lines[i] = domain.JournalLine{
			AccountCode:        l.AccountCode,
			DebitAmount:        l.DebitAmount,
			CreditAmount:       l.CreditAmount,
			Description:        l.Description,
			TaxCode:            l.TaxCode,
			TaxLogicSnapshotID: l.TaxLogicSnapshotID,
			Dimensions:         l.Dimensions,
		}
	}

	resultLines, created, err := h.store.CreateJournal(r.Context(), header, lines)
	if err != nil {
		// tenant_id and legal_entity_id are compared against uuid columns, so a
		// non-UUID is a bad field — a 400 naming them, not a 503 implying the
		// ledger is down.
		if errors.Is(err, domain.ErrInvalidIdentifier) {
			writeError(w, http.StatusBadRequest, "invalid_field",
				"tenant_id and legal_entity_id must both be UUIDs")
			return
		}
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
//
// Scoped to the caller's VERIFIED tenant, not to the tenant_id in the query
// string. It used to be the other way around: GetJournal filtered by the
// gateway-verified X-Tenant-Id and answered 404 for another tenant's journal,
// while ListJournals passed the query parameter straight through to the WHERE
// clause. So the boundary this service enforced one journal at a time could be
// stepped over wholesale — `?tenant_id=<anyone>` returned that tenant's entire
// general ledger, every entity, every period, amounts and all. A read that
// needs no id to guess is the worse of the two leaks, and it was the unguarded
// one.
//
// tenant_id is still accepted, because callers inside the estate send it
// (accounts-receivable-svc, financial-close-svc, consolidation-svc), but it is
// now only permitted to agree with the verified scope. Disagreement is refused
// outright rather than served under either tenant, and rather than answered
// with an empty list — an empty register reads as "this tenant has no
// journals", which is a different and more misleading claim than "no".
func (h *Handler) ListJournals(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	if claimed := q.Get("tenant_id"); claimed != "" && claimed != tenantID {
		writeError(w, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return
	}

	limit, ok := parseLimit(w, q.Get("limit"))
	if !ok {
		return
	}

	filter := domain.ListJournalsFilter{
		TenantID:      tenantID,
		LegalEntityID: q.Get("legal_entity_id"),
		FiscalPeriod:  q.Get("fiscal_period"),
		Status:        q.Get("status"),
		Limit:         limit,
	}

	journals, err := h.store.ListJournals(r.Context(), filter)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidIdentifier) {
			// legal_entity_id is compared as text, so it cannot land here; the
			// verified tenant is the only uuid comparison left, and a gateway
			// that forwarded a non-UUID tenant scope is a fault worth naming
			// rather than reporting as a dead store.
			writeError(w, http.StatusBadRequest, "invalid_field", "tenant scope must be a UUID")
			return
		}
		h.log.Error("ListJournals: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	// A nil slice marshals to JSON null, which every caller then has to
	// special-case. An empty ledger is an empty list.
	if journals == nil {
		journals = []domain.JournalHeader{}
	}
	writeJSON(w, http.StatusOK, journals)
}

// parseLimit reads an optional ?limit. A limit that isn't a positive integer is
// refused rather than silently replaced with the default — a caller who asked
// for a specific page size and got another one has no way to notice.
func parseLimit(w http.ResponseWriter, raw string) (int, bool) {
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_field", "limit must be a positive integer")
		return 0, false
	}
	return n, true
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

	// Exact equality on exact minor units. Postgres sums the NUMERIC(18,2)
	// columns and returns cents as bigint, so this compares integers — the
	// same test written over two float64 sums would reject journals that
	// balance perfectly well in decimal.
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
	if !decodeJSON(w, r, &req) {
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

	// Authorization is checked before the state of the journal is reported on,
	// so an unauthorized caller cannot use the difference between 422 and 412
	// to read a journal's posting status.
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, header.LegalEntityID, actionReverseJournal); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if header.Status != domain.JournalStatusFinalized {
		// A journal that is already REVERSED may be the caller's own reversal
		// coming back after a network timeout. Reversal advertises an
		// idempotency key, and this check used to reject the retry before the
		// store could recognise it — answering "only a FINALIZED journal may be
		// reversed", which reads as a refusal for an operation that in fact
		// succeeded. The idempotent branch below it was unreachable code.
		if replay, replayLines, ok := h.reversalReplay(r.Context(), header, journalID, req.CorrelationID); ok {
			writeJSON(w, http.StatusOK, domain.JournalWithLines{JournalHeader: *replay, Lines: replayLines})
			return
		}
		writeError(w, http.StatusUnprocessableEntity, "only_finalized_reversible", domain.ErrOnlyFinalizedReversible.Error())
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

	// The reversing journal inherits the original's ACC-03 inputs rather than
	// taking new ones. A reversal is not an independent business event: it must
	// land in the same book, the same currency and against the same source
	// document, or it does not net the original out. The two fields that do
	// differ are journal_type, which is REVERSAL by definition, and
	// posting_date — the reversal reaches the ledger today, not on the day the
	// entry it reverses did. transaction_date stays the original's, because the
	// underlying document has not changed.
	reversalPostingDate := domain.Date{Time: h.now().UTC().Truncate(24 * time.Hour)}
	if reversalPostingDate.Before(header.TransactionDate.Time) {
		// Only reachable for a journal whose document is dated in the future.
		// Posting before the document exists would trip the same invariant
		// CreateJournal refuses, so the reversal follows the document instead.
		reversalPostingDate = header.TransactionDate
	}

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

		JournalType:     domain.JournalTypeReversal,
		TransactionDate: header.TransactionDate,
		PostingDate:     reversalPostingDate,
		CurrencyCode:    header.CurrencyCode,
		BookID:          header.BookID,
		ReportingBasis:  header.ReportingBasis,
		EvidenceRefs:    header.EvidenceRefs,
	}
	reversingLines := make([]domain.JournalLine, len(lines))
	for i, l := range lines {
		reversingLines[i] = domain.JournalLine{
			AccountCode:        l.AccountCode,
			DebitAmount:        l.CreditAmount, // exact debit/credit inverse
			CreditAmount:       l.DebitAmount,
			Description:        l.Description,
			TaxCode:            l.TaxCode, // same tax basis as the line being reversed
			TaxLogicSnapshotID: l.TaxLogicSnapshotID,
			Dimensions:         l.Dimensions, // reverse against the same analysis axes
		}
	}

	// One transaction: the reversing journal is posted and the original marked
	// REVERSED together, or neither happens. As two calls, a failure between
	// them left the books holding both the original posting and its inverse as
	// live FINALIZED entries — a double-counted ledger no later request would
	// ever reconcile.
	resultLines, created, err := h.store.ReverseJournal(
		r.Context(), header.TenantID, journalID, reversingHeader, reversingLines, principalID)
	if err != nil {
		// The original stopped being FINALIZED between the read above and the
		// write — a concurrent reversal won the race. Its reversing journal
		// rolled back with it, so there is nothing to clean up.
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusUnprocessableEntity, "only_finalized_reversible",
				domain.ErrOnlyFinalizedReversible.Error())
			return
		}
		h.log.Error("ReverseJournal: failed to post reversing journal", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	// created=false means this correlation_id already reversed this journal on
	// an earlier call — a retry, not a new reversal. reversingHeader has been
	// resolved to the journal that actually exists, so the reply is the stored
	// reversal rather than a fresh id for a row that was never written.
	if !created {
		writeJSON(w, http.StatusOK, domain.JournalWithLines{JournalHeader: *reversingHeader, Lines: resultLines})
		return
	}

	header.Status = domain.JournalStatusReversed
	h.publisher.PublishJournalReversed(r.Context(), *header, reversingHeader.JournalID)
	writeJSON(w, http.StatusCreated, domain.JournalWithLines{JournalHeader: *reversingHeader, Lines: resultLines})
}

// reversalReplay reports whether this exact reversal has already been applied:
// the journal is REVERSED, and the caller's correlation_id belongs to a journal
// that reverses this one. Only then is the earlier reversal returned — a
// correlation_id that names some unrelated journal is not this caller's
// reversal coming back, and must not be answered as though it were.
func (h *Handler) reversalReplay(ctx context.Context, header *domain.JournalHeader, journalID, correlationID string) (*domain.JournalHeader, []domain.JournalLine, bool) {
	if header.Status != domain.JournalStatusReversed || correlationID == "" {
		return nil, nil, false
	}
	existing, lines, err := h.store.GetJournalByCorrelationID(ctx, header.TenantID, correlationID)
	if err != nil {
		// Treat a failed lookup as "not a replay". The caller then gets the
		// ordinary 422, which is the truthful answer for a REVERSED journal;
		// inventing a success here would be worse than an unhelpful refusal.
		h.log.Error("reversalReplay: lookup failed", zap.Error(err))
		return nil, nil, false
	}
	if existing == nil || existing.ReversalOfJournalID == nil || *existing.ReversalOfJournalID != journalID {
		return nil, nil, false
	}
	return existing, lines, true
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

	// ACC-03 required business/source inputs. Reported the same way as the
	// fields above so a caller adopting the contract gets one consistent
	// missing_field answer rather than two different refusal shapes.
	case req.JournalType == "":
		return "journal_type"
	case req.TransactionDate.IsZero():
		return "transaction_date"
	case req.PostingDate.IsZero():
		return "posting_date"
	case req.CurrencyCode == "":
		return "currency_code"
	default:
		return ""
	}
}

// invalidJournalInput checks the ACC-03 inputs that are present but wrong,
// as opposed to absent. Returns an empty code when the request is acceptable.
//
// Separate from requiredJournalFieldMissing because "you did not send
// journal_type" and "ACRUAL is not a journal type" are different mistakes and
// a caller can only fix the second if it is told which value was rejected.
func invalidJournalInput(req domain.CreateJournalRequest) (code, detail string) {
	if !domain.ValidJournalType(req.JournalType) {
		return "invalid_journal_type", domain.ErrInvalidJournalType.Error()
	}
	if !domain.ValidCurrencyCode(req.CurrencyCode) {
		return "invalid_currency_code", domain.ErrInvalidCurrency.Error()
	}
	if req.PostingDate.Before(req.TransactionDate.Time) {
		return "invalid_posting_date", domain.ErrPostingBeforeTransaction.Error()
	}
	return "", ""
}

func exactlyOneNonZero(debit, credit float64) bool {
	return (debit > 0 && credit == 0) || (credit > 0 && debit == 0)
}

// mergeEvidenceRefs unions the body's evidence_refs with the §4 envelope's
// X-Evidence-Refs, preserving first-seen order and dropping duplicates.
//
// Union rather than "header wins": the two carry different things in practice.
// A caller posting an invoice-derived journal puts the invoice document on the
// envelope for the whole request, and names the specific supporting schedules
// in the body. Taking only one of them would drop real evidence, and INV-10
// makes evidence a completion condition rather than a nice-to-have.
func mergeEvidenceRefs(body, envelope []string) []string {
	if len(body) == 0 && len(envelope) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(body)+len(envelope))
	out := make([]string, 0, len(body)+len(envelope))
	for _, group := range [][]string{body, envelope} {
		for _, ref := range group {
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			out = append(out, ref)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

// requireTenant reads the caller's verified tenant scope from X-Tenant-Id, set
// by the same gateway ForwardAuth step that sets X-Principal-Id (see
// internal/middleware.TenantContext). A request with no verified tenant scope
// never passed that verification, and is refused rather than served under a
// tenant it names itself — the whole point of the header is that it is the one
// tenant claim in the request the caller did not write.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", domain.ErrTenantScopeMissing.Error())
		return "", false
	}
	return tenantID, true
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

// maxRequestBytes caps a JSON request body. A bare json.Decoder reads until EOF,
// so without this a single request can make the service allocate whatever the
// client is willing to send -- no auth needed, and nothing in the metrics to
// distinguish it from load.
const maxRequestBytes = 256 << 10 // 256 KiB

// decodeJSON reads a size-capped JSON body, answering 413 rather than 400 when
// the cap is what stopped it: "too large" and "malformed" are different faults
// and a caller can only act on the difference.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}
