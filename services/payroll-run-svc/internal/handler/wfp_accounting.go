package handler

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
	"zoiko.io/payroll-run-svc/internal/ledger"
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
)

// WFPAccountingStore is the persistence surface of WFP-05, separate from Store so
// the legacy payroll-run handlers and their stubs are untouched.
type WFPAccountingStore interface {
	GetPayrollRunRef(ctx context.Context, id string) (*domain.PayrollRunRef, error)
	GetPayrollResultByRef(ctx context.Context, runRefID string) (*domain.PayrollResult, error)
	ResultPostingInputFor(ctx context.Context, runRefID string) (domain.ResultPostingInput, *domain.PayrollResult, *domain.PayrollRunRef, error)

	CreateAccountingBridge(ctx context.Context, b *domain.AccountingBridge, mk store.AccountingEventBuilder) (*domain.AccountingBridge, bool, error)
	GetAccountingBridge(ctx context.Context, id string) (*domain.AccountingBridge, error)
	ListAccountingBridges(ctx context.Context, runRefID string) ([]domain.AccountingBridge, error)
	SubmitAccountingBridge(ctx context.Context, id string, expectedVersion int, actor string, mk store.AccountingEventBuilder) (*domain.AccountingBridge, error)
	CreateReversalBridge(ctx context.Context, originalID, reason, creator, correlationID string, mk store.AccountingEventBuilder) (*domain.AccountingBridge, bool, error)
	WithdrawAccountingBridge(ctx context.Context, id string, expectedVersion int, reason string, mk store.AccountingEventBuilder) (*domain.AccountingBridge, error)
	RecordReconciliation(ctx context.Context, id, state string, detail any) (*domain.AccountingBridge, error)
}

// LedgerReader reads a journal from general-ledger-svc (read-only).
type LedgerReader interface {
	GetJournal(ctx context.Context, tenantID, entityID, correlationID, journalID string) (*ledger.Journal, bool, error)
}

const (
	actionAccView      = "WFP_ACCOUNTING_VIEW"
	actionAccCreate    = "WFP_ACCOUNTING_CREATE"
	actionAccSubmit    = "WFP_ACCOUNTING_SUBMIT"
	actionAccRequeue   = "WFP_ACCOUNTING_REQUEUE"
	actionAccReverse   = "WFP_ACCOUNTING_REVERSE"
	actionAccWithdraw  = "WFP_ACCOUNTING_WITHDRAW"
	actionAccReconcile = "WFP_ACCOUNTING_RECONCILE"
)

var bookRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

type WFPAccountingHandler struct {
	store       WFPAccountingStore
	authz       AuthZClient
	ledger      LedgerReader
	defaultBook string
	log         *zap.Logger
}

func NewWFPAccounting(st WFPAccountingStore, authz AuthZClient, ledgerReader LedgerReader, defaultBook string, log *zap.Logger) *WFPAccountingHandler {
	if defaultBook == "" {
		defaultBook = domain.DefaultAccountingBook
	}
	return &WFPAccountingHandler{store: st, authz: authz, ledger: ledgerReader, defaultBook: defaultBook, log: log}
}

func RegisterWFPAccountingRoutes(r chi.Router, h *WFPAccountingHandler) {
	r.Route("/v1/payroll/wfp/accounting/events", func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.List)
		r.Get("/mapping-keys", h.MappingKeys)
		r.Get("/{id}", h.Get)
		r.Get("/{id}/posting-status", h.PostingStatus)
		r.Get("/{id}/journal-references", h.JournalReferences)
		r.Get("/{id}/lineage", h.Lineage)
		r.Post("/{id}/submit", h.Submit)
		r.Post("/{id}/requeue", h.Requeue)
		r.Post("/{id}/reverse", h.Reverse)
		r.Post("/{id}/withdraw", h.Withdraw)
		r.Post("/{id}/reconcile", h.Reconcile)
	})
}

// ── plumbing ─────────────────────────────────────────────────────────────────

func (h *WFPAccountingHandler) identity(w http.ResponseWriter, r *http.Request) (principal, tenant string, ok bool) {
	principal = r.Header.Get("X-Principal-Id")
	tenant = svcmiddleware.TenantFromContext(r.Context())
	if principal == "" || tenant == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", "", false
	}
	return principal, tenant, true
}

func (h *WFPAccountingHandler) authorize(w http.ResponseWriter, r *http.Request, principal, entityID, action string) bool {
	if strings.TrimSpace(entityID) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal entity is required")
		return false
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, entityID, action); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "forbidden", err.Error())
		} else {
			writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
		}
		return false
	}
	return true
}

// loadAuthorized loads a subject and authorizes against ITS legal entity.
func (h *WFPAccountingHandler) loadAuthorized(w http.ResponseWriter, r *http.Request, action string) (*domain.AccountingBridge, string, string, bool) {
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return nil, "", "", false
	}
	b, err := h.store.GetAccountingBridge(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeErr(w, err)
		return nil, "", "", false
	}
	if !h.authorize(w, r, principal, b.LegalEntityID, action) {
		return nil, "", "", false
	}
	return b, principal, tenant, true
}

func (h *WFPAccountingHandler) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrBridgeNotFound), errors.Is(err, domain.ErrRunRefNotFound), errors.Is(err, domain.ErrResultNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrBridgeVersionConflict):
		writeError(w, http.StatusConflict, "VERSION_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrBridgeNotSubmittable), errors.Is(err, domain.ErrBridgeNotReversible), errors.Is(err, domain.ErrBridgeNotWithdrawable),
		errors.Is(err, domain.ErrBridgeNotPosted):
		writeError(w, http.StatusConflict, "INVALID_STATE", err.Error())
	case errors.Is(err, domain.ErrResultNotPostable):
		writeError(w, http.StatusConflict, "RESULT_NOT_POSTABLE", err.Error())
	case errors.Is(err, domain.ErrSoDSamePrincipal):
		writeError(w, http.StatusForbidden, "SOD_SAME_PRINCIPAL", err.Error())
	case errors.Is(err, domain.ErrNothingToPost):
		writeError(w, http.StatusUnprocessableEntity, "NOTHING_TO_POST", err.Error())
	case errors.Is(err, domain.ErrUnbalancedPosting):
		writeError(w, http.StatusUnprocessableEntity, "UNBALANCED_POSTING", err.Error())
	case errors.Is(err, domain.ErrLedgerUnavailable):
		writeError(w, http.StatusServiceUnavailable, "ledger_unavailable", err.Error())
	case errors.Is(err, domain.ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, "identity_missing", err.Error())
	default:
		h.log.Error("wfp-05 store failure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "payroll accounting store unavailable")
	}
}

func (h *WFPAccountingHandler) events(r *http.Request, actor string) store.AccountingEventBuilder {
	return func(ch store.AccountingChange) ([]domain.OutboxEvent, error) {
		return events.BuildAccountingEvents(string(ch.Kind), ch.Bridge, ch.Original, ch.Reason, actor, getCorrelationID(r), r.Header.Get("X-Causation-Id"))
	}
}

func (h *WFPAccountingHandler) transitionBody(w http.ResponseWriter, r *http.Request, needReason bool) (domain.AccountingTransitionRequest, int, bool) {
	var req domain.AccountingTransitionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return req, 0, false
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) is required")
		return req, 0, false
	}
	if needReason && strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required")
		return req, 0, false
	}
	return req, version, true
}

// ── commands ─────────────────────────────────────────────────────────────────

// Create is CreatePayrollAccountingEvent: it builds the balanced posting
// proposal for an ACCEPTED result version. Nothing reaches the ledger until it is
// submitted by a different principal.
func (h *WFPAccountingHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAccountingEventRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if _, err := uuid.Parse(req.RunRefID); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "run_ref_id must be a UUID")
		return
	}
	book := h.defaultBook
	if req.AccountingBookID != "" {
		if !bookRE.MatchString(req.AccountingBookID) {
			writeError(w, http.StatusBadRequest, "validation_failed", "accounting_book_id must be 1 to 32 letters, digits, '_' or '-'")
			return
		}
		book = req.AccountingBookID
	}
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return
	}
	input, res, ref, err := h.store.ResultPostingInputFor(r.Context(), req.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if !h.authorize(w, r, principal, ref.LegalEntityID, actionAccCreate) {
		return
	}
	if ref.AcceptanceState != domain.AcceptanceAccepted || res.Outcome != domain.ResultAccepted {
		h.writeErr(w, domain.ErrResultNotPostable)
		return
	}
	lines, err := domain.BuildPostingLines(input)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	debit, credit, err := domain.SumLines(lines)
	if err != nil {
		h.writeErr(w, err)
		return
	}

	desc := fmt.Sprintf("Payroll %s run %s v%d", ref.PayrollPeriodID, ref.SourceRunID, ref.SourceRunVersion)
	if len(res.EvidenceFlags) > 0 {
		desc += " [" + strings.Join(res.EvidenceFlags, ",") + "]"
	}
	b := &domain.AccountingBridge{
		BridgeID: uuid.NewString(), TenantID: tenant, RunRefID: ref.RunRefID, ResultID: res.ResultID, LegalEntityID: ref.LegalEntityID,
		AccountingBookID: book, PostingPurpose: domain.PurposeResult, SourceEventID: domain.SourceEventID(res.ResultID, book, domain.PurposeResult),
		FiscalPeriod: domain.FiscalPeriodOf(ref.PayDate), DocumentDate: ref.PayDate, PostingDate: ref.PayDate,
		TransactionCurrency: res.Currency, Description: desc, Lines: lines, LineCount: len(lines),
		TotalDebit: domain.FormatDecimal(debit), TotalCredit: domain.FormatDecimal(credit), EvidenceFlags: res.EvidenceFlags,
		CreatedBy: principal, CorrelationID: getCorrelationID(r),
	}
	out, created, err := h.store.CreateAccountingBridge(r.Context(), b, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	status := http.StatusOK // the canonical subject for this result + book + purpose already existed
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
}

func (h *WFPAccountingHandler) submit(w http.ResponseWriter, r *http.Request, action string, wantState string) {
	_, version, ok := h.transitionBody(w, r, false)
	if !ok {
		return
	}
	b, principal, _, ok := h.loadAuthorized(w, r, action)
	if !ok {
		return
	}
	if wantState != "" && b.State != wantState {
		h.writeErr(w, domain.ErrBridgeNotSubmittable)
		return
	}
	// Separation of duties, checked here for a precise answer and again by the
	// database so no other path can bypass it.
	res, err := h.store.GetPayrollResultByRef(r.Context(), b.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if res.ReceivedBy == principal {
		h.writeErr(w, domain.ErrSoDSamePrincipal)
		return
	}
	out, err := h.store.SubmitAccountingBridge(r.Context(), b.BridgeID, version, principal, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Submit is SubmitPayrollAccountingEvent: queue the subject for the ledger. The
// submitter must not be the principal who received the payroll result.
func (h *WFPAccountingHandler) Submit(w http.ResponseWriter, r *http.Request) {
	h.submit(w, r, actionAccSubmit, domain.BridgeEventCreated)
}

// Requeue resubmits a subject the ledger REJECTED, after its cause was fixed
// (an unmapped key, a locked period).
func (h *WFPAccountingHandler) Requeue(w http.ResponseWriter, r *http.Request) {
	h.submit(w, r, actionAccRequeue, domain.BridgeRejected)
}

// Reverse is ReversePayrollAccountingEvent: it creates the reversal subject for a
// POSTED subject whose result was superseded. A posted journal is never deleted or
// replaced; the ledger reverses it.
func (h *WFPAccountingHandler) Reverse(w http.ResponseWriter, r *http.Request) {
	req, _, ok := h.transitionBodyNoVersion(w, r)
	if !ok {
		return
	}
	b, principal, _, ok := h.loadAuthorized(w, r, actionAccReverse)
	if !ok {
		return
	}
	if b.PostingPurpose != domain.PurposeResult || b.State != domain.BridgePosted {
		h.writeErr(w, domain.ErrBridgeNotReversible)
		return
	}
	ref, err := h.store.GetPayrollRunRef(r.Context(), b.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if ref.AcceptanceState != domain.AcceptanceSuperseded {
		h.writeErr(w, domain.ErrBridgeNotReversible)
		return
	}
	out, created, err := h.store.CreateReversalBridge(r.Context(), b.BridgeID, req.Reason, principal, getCorrelationID(r), h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
}

// transitionBodyNoVersion reads a body with a mandatory reason and no version
// (Reverse creates a new subject, so there is no version to guard).
func (h *WFPAccountingHandler) transitionBodyNoVersion(w http.ResponseWriter, r *http.Request) (domain.AccountingTransitionRequest, int, bool) {
	var req domain.AccountingTransitionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return req, 0, false
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required")
		return req, 0, false
	}
	return req, 0, true
}

// Withdraw ends an unposted subject whose result was superseded.
func (h *WFPAccountingHandler) Withdraw(w http.ResponseWriter, r *http.Request) {
	req, version, ok := h.transitionBody(w, r, true)
	if !ok {
		return
	}
	b, principal, _, ok := h.loadAuthorized(w, r, actionAccWithdraw)
	if !ok {
		return
	}
	ref, err := h.store.GetPayrollRunRef(r.Context(), b.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if ref.AcceptanceState != domain.AcceptanceSuperseded {
		h.writeErr(w, domain.ErrBridgeNotWithdrawable)
		return
	}
	out, err := h.store.WithdrawAccountingBridge(r.Context(), b.BridgeID, version, req.Reason, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Reconcile is ReconcilePayrollPosting: compare a posted subject with the ledger's
// journal, exactly, and record the outcome. An unreachable ledger is "unknown"
// (503 and nothing recorded), never a match.
func (h *WFPAccountingHandler) Reconcile(w http.ResponseWriter, r *http.Request) {
	b, _, tenant, ok := h.loadAuthorized(w, r, actionAccReconcile)
	if !ok {
		return
	}
	if b.JournalID == nil {
		h.writeErr(w, domain.ErrBridgeNotPosted)
		return
	}
	j, found, err := h.ledger.GetJournal(r.Context(), tenant, b.LegalEntityID, getCorrelationID(r), *b.JournalID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	state, detail := compareJournal(b, j, found)
	out, err := h.store.RecordReconciliation(r.Context(), b.BridgeID, state, detail)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bridge": out, "reconciliation_state": state, "differences": detail["differences"]})
}

func ratOf(f float64) *big.Rat {
	r, _ := new(big.Rat).SetString(strconv.FormatFloat(f, 'f', 4, 64))
	return r
}

// compareJournal compares a subject with the journal the ledger holds, exactly.
func compareJournal(b *domain.AccountingBridge, j *ledger.Journal, found bool) (string, map[string]any) {
	diffs := []string{}
	if !found {
		diffs = append(diffs, "the ledger has no journal "+*b.JournalID)
		return domain.ReconMismatch, map[string]any{"differences": diffs, "journal_id": b.JournalID}
	}
	if j.Status != "FINALIZED" {
		diffs = append(diffs, "journal status is "+j.Status+", not FINALIZED")
	}
	if j.LegalEntityID != b.LegalEntityID {
		diffs = append(diffs, "journal legal entity "+j.LegalEntityID+" differs from "+b.LegalEntityID)
	}
	if j.FiscalPeriod != b.FiscalPeriod {
		diffs = append(diffs, "journal period "+j.FiscalPeriod+" differs from "+b.FiscalPeriod)
	}
	if j.CurrencyCode != b.TransactionCurrency {
		diffs = append(diffs, "journal currency "+j.CurrencyCode+" differs from "+b.TransactionCurrency)
	}
	debit, credit := new(big.Rat), new(big.Rat)
	for _, l := range j.Lines {
		debit.Add(debit, ratOf(l.DebitAmount))
		credit.Add(credit, ratOf(l.CreditAmount))
	}
	if domain.FormatDecimal(debit) != b.TotalDebit {
		diffs = append(diffs, fmt.Sprintf("journal debits %s differ from subject %s", domain.FormatDecimal(debit), b.TotalDebit))
	}
	if domain.FormatDecimal(credit) != b.TotalCredit {
		diffs = append(diffs, fmt.Sprintf("journal credits %s differ from subject %s", domain.FormatDecimal(credit), b.TotalCredit))
	}
	if b.PostingPurpose == domain.PurposeResult && len(j.Lines) != b.LineCount {
		diffs = append(diffs, fmt.Sprintf("journal has %d lines, subject proposed %d", len(j.Lines), b.LineCount))
	}
	state := domain.ReconMatched
	if len(diffs) > 0 {
		state = domain.ReconMismatch
	}
	return state, map[string]any{"differences": diffs, "journal_id": b.JournalID, "journal_debits": domain.FormatDecimal(debit), "journal_credits": domain.FormatDecimal(credit)}
}

// ── queries ──────────────────────────────────────────────────────────────────

// Get is GetPayrollAccountingBridge.
func (h *WFPAccountingHandler) Get(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionAccView)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (h *WFPAccountingHandler) List(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	runRef := r.URL.Query().Get("run_ref_id")
	if _, err := uuid.Parse(runRef); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "run_ref_id (UUID) is required")
		return
	}
	ref, err := h.store.GetPayrollRunRef(r.Context(), runRef)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if !h.authorize(w, r, principal, ref.LegalEntityID, actionAccView) {
		return
	}
	bs, err := h.store.ListAccountingBridges(r.Context(), runRef)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": bs})
}

// MappingKeys lists every account-mapping key payroll posting can emit, so finance
// can configure them in the ledger before the first posting.
func (h *WFPAccountingHandler) MappingKeys(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := h.identity(w, r)
	if !ok || !h.authorize(w, r, principal, strings.TrimSpace(r.Header.Get("X-Legal-Entity-Id")), actionAccView) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mapping_keys": domain.AllPayrollMappingKeys()})
}

// PostingStatus is GetPostingStatus.
func (h *WFPAccountingHandler) PostingStatus(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionAccView)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bridge_id": b.BridgeID, "state": b.State, "correction_kind": b.CorrectionKind, "attempts": b.Attempts,
		"max_attempts": domain.MaxPostingAttempts, "waiting_for_requeue": b.State == domain.BridgeSubmitted && b.Attempts >= domain.MaxPostingAttempts,
		"last_error": b.LastError, "rejection_reason": b.RejectionReason, "submitted_by": b.SubmittedBy, "submitted_at": b.SubmittedAt,
		"posted_at": b.PostedAt, "reconciliation_state": b.ReconciliationState, "reconciled_at": b.ReconciledAt,
	})
}

// JournalReferences is GetJournalReferences.
func (h *WFPAccountingHandler) JournalReferences(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionAccView)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bridge_id": b.BridgeID, "posting_purpose": b.PostingPurpose, "source_event_id": b.SourceEventID,
		"execution_id": b.ExecutionID, "journal_id": b.JournalID, "fiscal_period": b.FiscalPeriod,
	})
}

// Lineage is GetPayrollToGLLineage: payroll result version -> accounting subject(s)
// -> posting execution -> journal, including a reversal when there is one.
func (h *WFPAccountingHandler) Lineage(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionAccView)
	if !ok {
		return
	}
	ref, err := h.store.GetPayrollRunRef(r.Context(), b.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	res, err := h.store.GetPayrollResultByRef(r.Context(), b.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	all, err := h.store.ListAccountingBridges(r.Context(), b.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	type link struct {
		BridgeID       string  `json:"bridge_id"`
		Purpose        string  `json:"posting_purpose"`
		State          string  `json:"state"`
		SourceEventID  string  `json:"source_event_id"`
		ExecutionID    *string `json:"execution_id,omitempty"`
		JournalID      *string `json:"journal_id,omitempty"`
		Original       *string `json:"original_bridge_id,omitempty"`
		Reconciliation string  `json:"reconciliation_state"`
	}
	links := make([]link, 0, len(all))
	for _, x := range all {
		links = append(links, link{x.BridgeID, x.PostingPurpose, x.State, x.SourceEventID, x.ExecutionID, x.JournalID, x.OriginalBridgeID, x.ReconciliationState})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_ref": map[string]any{
			"run_ref_id": ref.RunRefID, "source_system_id": ref.SourceSystemID, "source_run_id": ref.SourceRunID,
			"source_run_version": ref.SourceRunVersion, "acceptance_state": ref.AcceptanceState, "rule_manifest_id": ref.RuleManifestID,
		},
		"result":   map[string]any{"result_id": res.ResultID, "outcome": res.Outcome, "result_fingerprint": res.ResultFingerprint, "evidence_flags": res.EvidenceFlags},
		"subjects": links,
	})
}
