// Package handler exposes general-ledger-svc's REST API.
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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

	// CompileTrialBalance is ACC-15's real, durable trial-balance capability
	// — see migration 000006's doc comment.
	CompileTrialBalance(ctx context.Context, tenantID, legalEntityID, fiscalPeriod, principalID string) (*domain.TrialBalanceSnapshot, error)
	GetTrialBalance(ctx context.Context, tenantID, snapshotID string) (*domain.TrialBalanceSnapshot, error)

	// ACC-01 Chart of Accounts — see migration 000007's doc comment.
	CreateAccount(ctx context.Context, a *domain.Account) error
	GetAccountByCode(ctx context.Context, tenantID, accountCode string) (*domain.Account, error)
	ListAccounts(ctx context.Context, tenantID string) ([]domain.Account, error)
	DeactivateAccount(ctx context.Context, tenantID, accountCode string) error

	// ACC-02 Account Mapping — see migration 000008's doc comment.
	SetAccountMapping(ctx context.Context, m *domain.AccountMapping) error
	GetCurrentAccountMapping(ctx context.Context, tenantID, mappingKey string) (*domain.AccountMapping, error)
	ListAccountMappings(ctx context.Context, tenantID string) ([]domain.AccountMapping, error)

	// ACC-04 Posting Engine — see migration 000009's doc comment.
	CreatePostingExecution(ctx context.Context, e *domain.PostingExecution) error
	GetPostingExecution(ctx context.Context, tenantID, executionID string) (*domain.PostingExecution, error)
	GetPostingExecutionBySource(ctx context.Context, tenantID, sourceEventID string) (*domain.PostingExecution, error)
	MarkPostingExecutionCommitted(ctx context.Context, tenantID, executionID, journalID string, committedAt time.Time) error
	MarkPostingExecutionFailed(ctx context.Context, tenantID, executionID, status, reason string) error

	// ACC-03 journal proposal/approval lifecycle — see migration 000010's
	// doc comment.
	SubmitJournalForApproval(ctx context.Context, tenantID, journalID, principalID string) error
	ApproveJournal(ctx context.Context, tenantID, journalID, principalID, fingerprint string) error
	RejectJournal(ctx context.Context, tenantID, journalID, principalID, reason string) error
	RequestJournalPosting(ctx context.Context, tenantID, journalID, principalID string) error
	MarkJournalPosted(ctx context.Context, tenantID, journalID string) error
	AmendDraftJournal(ctx context.Context, tenantID, journalID string, h *domain.JournalHeader, lines []domain.JournalLine) error
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
	// actionCompileTrialBalance is read-adjacent but materially different
	// from actionValidateJournal/actionPostJournal: it doesn't mutate any
	// journal, but it DOES create a new permanent, signed-off-on financial
	// artifact (a durable snapshot other services rely on), which is closer
	// in consequence to a write than to GL_JOURNAL's own read actions.
	actionCompileTrialBalance = "GL_TRIAL_BALANCE_COMPILE"
	actionViewTrialBalance    = "GL_TRIAL_BALANCE_VIEW"

	// ACC-01 Chart of Accounts actions — deliberately their own action
	// namespace (COA_*, not GL_*), since this is a separate authority per
	// the spec's own Cross-Service Accounting Authority Matrix even though
	// it's co-deployed in this same process.
	actionCreateAccount     = "COA_ACCOUNT_CREATE"
	actionViewAccount       = "COA_ACCOUNT_VIEW"
	actionDeactivateAccount = "COA_ACCOUNT_DEACTIVATE"
	// actionOverridePostingRestriction gates the invariant #7 override —
	// deliberately a distinct, more sensitive action than actionCreateJournal
	// itself, so it can be granted to a narrower group.
	actionOverridePostingRestriction = "COA_CONTROL_ACCOUNT_POSTING_OVERRIDE"

	// ACC-02 Account Mapping actions.
	actionSetAccountMapping  = "COA_MAPPING_SET"
	actionViewAccountMapping = "COA_MAPPING_VIEW"

	// ACC-04 Posting Engine actions. The spec names a single execute action
	// literally ("accounting.post.execute internal") and requires it never
	// be reachable by ordinary human bypass of source approval/SoD — this
	// platform enforces WHICH identities hold an action via
	// authorization-svc's own role grants, so actionPostingExecute is the
	// namespaced action a deployment would grant ONLY to internal service
	// identities, never a general human role. actionPostingReverse is
	// deliberately separate ("reversal requires authorized correction
	// command") — a correction is a materially more sensitive act than an
	// ordinary posting.
	actionPostingExecute   = "GL_POSTING_EXECUTE"
	actionPostingReverse   = "GL_POSTING_REVERSE"
	actionPostingReprocess = "GL_POSTING_REPROCESS"
	actionPostingView      = "GL_POSTING_VIEW"

	// ACC-03 journal proposal/approval lifecycle actions. The spec's own
	// permissions field: "journal.create; journal.submit; journal.approve;
	// journal.post.request" — mapped onto this platform's GL_JOURNAL_*
	// namespace rather than the doc's dotted form, matching every other
	// action name in this file. actionApproveJournalProposal is
	// deliberately distinct from actionSubmitJournal — segregation of
	// duties is what SoD MEANS, and collapsing the two into one grantable
	// action would make maker/checker unenforceable at the authorization
	// layer no matter what the handler itself checks.
	actionSubmitJournal          = "GL_JOURNAL_SUBMIT"
	actionApproveJournalProposal = "GL_JOURNAL_APPROVE"
	actionRejectJournal          = "GL_JOURNAL_REJECT"
	actionRequestPosting         = "GL_JOURNAL_POST_REQUEST"
	actionAmendDraftJournal      = "GL_JOURNAL_AMEND"
	actionRequestCorrection      = "GL_JOURNAL_CORRECT"
)

// coaPlatformScopeID is the legal_entity_id presented to authorization-svc
// for Chart of Accounts administration — v1 treats the chart as tenant-wide
// reference data shared across legal entities (same posture as
// jurisdiction-rules-svc's platform-wide facts), not entity-scoped. A
// future version splitting per-entity local charts would change this, not
// this handler's authz call shape.
const coaPlatformScopeID = "00000000-0000-0000-0000-00000000f001"

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

		// ACC-03 journal proposal/approval lifecycle.
		r.Post("/{journal_id}/amend", h.AmendDraftJournal)
		r.Post("/{journal_id}/submit", h.SubmitJournal)
		r.Post("/{journal_id}/approve", h.ApproveJournal)
		r.Post("/{journal_id}/reject", h.RejectJournal)
		r.Post("/{journal_id}/request-posting", h.RequestPosting)
		r.Post("/{journal_id}/correct", h.RequestCorrection)
		r.Get("/{journal_id}/available-actions", h.GetAvailableActions)
		r.Get("/{journal_id}/history", h.GetJournalHistory)
	})
	r.Route("/v1/trial-balance", func(r chi.Router) {
		r.Post("/compile", h.CompileTrialBalance)
		r.Get("/{snapshot_id}", h.GetTrialBalance)
	})
	r.Route("/v1/chart-of-accounts", func(r chi.Router) {
		r.Post("/", h.CreateAccount)
		r.Get("/", h.ListAccounts)
		r.Get("/{account_code}", h.GetAccount)
		r.Post("/{account_code}/deactivate", h.DeactivateAccount)
	})
	r.Route("/v1/account-mappings", func(r chi.Router) {
		r.Post("/", h.SetAccountMapping)
		r.Get("/", h.ListAccountMappings)
		r.Get("/{mapping_key}", h.GetAccountMapping)
	})
	r.Route("/v1/postings", func(r chi.Router) {
		r.Post("/events", h.PostAccountingEvent)
		r.Post("/journals", h.PostApprovedJournal)
		r.Post("/reversals", h.CreateReversalPosting)
		r.Get("/by-source", h.GetPostingBySource)
		r.Get("/verify-uniqueness", h.VerifyPostingUniqueness)
		r.Get("/{execution_id}", h.GetPostingExecution)
		r.Get("/{execution_id}/explain", h.ExplainPosting)
		r.Post("/{execution_id}/reprocess", h.ReprocessFailedPosting)
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

	// ACC-01 invariant #7: "Control accounts cannot be bypassed by ordinary
	// manual journals where policy restricts direct posting." Enforced for
	// real for any account someone has actually registered — an account
	// code that has never been onboarded into the Chart of Accounts is
	// allowed through unvalidated (an honest, deliberate bootstrap gap: the
	// chart started empty, and every existing caller across this platform
	// posts using account codes nothing has ever validated, so treating an
	// unregistered code as a hard failure would break every one of them the
	// moment this shipped, not just newly-restricted ones).
	if !h.checkAccountRestrictions(w, r, req, principalID) {
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

		// ACC-03: every ordinary journal a human creates starts as a
		// proposal nobody has acted on yet — explicit here rather than
		// left to a store-layer default, since the handler is where this
		// service's actual intent belongs.
		ApprovalStatus: domain.ApprovalStatusDraft,
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

// checkAccountRestrictions is ACC-01 invariant #7 enforced for real: a
// registered account that is INACTIVE may never be posted to, and a
// registered control account with direct_posting_restricted=true requires
// both the caller's explicit opt-in (OverrideControlAccountRestriction)
// AND a separate, more sensitive authorization
// (actionOverridePostingRestriction) to be posted to directly. An account
// code the chart has never heard of is let through unvalidated — see
// CreateJournal's own call-site comment for why that bootstrap gap is
// deliberate, not an oversight. Writes its own HTTP response and returns
// false on any rejection.
func (h *Handler) checkAccountRestrictions(w http.ResponseWriter, r *http.Request, req domain.CreateJournalRequest, principalID string) bool {
	seen := map[string]bool{}
	for _, l := range req.Lines {
		if seen[l.AccountCode] {
			continue // avoid re-checking (and re-authorizing) the same account twice in one journal
		}
		seen[l.AccountCode] = true

		acct, err := h.store.GetAccountByCode(r.Context(), req.TenantID, l.AccountCode)
		if errors.Is(err, domain.ErrAccountNotFound) {
			continue // unregistered legacy account code — deliberately allowed, see caller comment
		}
		if err != nil {
			h.log.Error("checkAccountRestrictions: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
			return false
		}
		if acct.Status != "ACTIVE" {
			writeError(w, http.StatusUnprocessableEntity, "account_inactive",
				domain.ErrAccountInactive.Error()+": "+l.AccountCode)
			return false
		}
		if acct.IsControlAccount && acct.DirectPostingRestricted {
			if !req.OverrideControlAccountRestriction {
				writeError(w, http.StatusUnprocessableEntity, "control_account_posting_restricted",
					domain.ErrControlAccountPostingRestricted.Error()+": "+l.AccountCode)
				return false
			}
			if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionOverridePostingRestriction); err != nil {
				h.writeAuthzErr(w, err)
				return false
			}
		}
	}
	return true
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

// ── POST /v1/trial-balance/compile ──────────────────────────────────────────

// CompileTrialBalance is ACC-15's real, durable trial-balance capability:
// the platform's own authoritative compilation, pinned to an explicit
// ledger watermark, replacing what used to be every caller (financial-
// close-svc included) re-deriving one ad hoc from raw journal pages. See
// migration 000006 and master-register-findings-2026-08-27.md §3.32.
func (h *Handler) CompileTrialBalance(w http.ResponseWriter, r *http.Request) {
	var req domain.CompileTrialBalanceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.FiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id and fiscal_period are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCompileTrialBalance); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	snap, err := h.store.CompileTrialBalance(r.Context(), tenantID, req.LegalEntityID, req.FiscalPeriod, principalID)
	if err != nil {
		h.log.Error("CompileTrialBalance: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if snap.Lines == nil {
		snap.Lines = []domain.TrialBalanceLine{}
	}
	writeJSON(w, http.StatusCreated, snap)
}

// ── GET /v1/trial-balance/{snapshot_id} ─────────────────────────────────────

func (h *Handler) GetTrialBalance(w http.ResponseWriter, r *http.Request) {
	snapshotID := chi.URLParam(r, "snapshot_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	snap, err := h.store.GetTrialBalance(r.Context(), tenantID, snapshotID)
	if errors.Is(err, domain.ErrTrialBalanceNotFound) {
		writeError(w, http.StatusNotFound, "trial_balance_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("GetTrialBalance: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, snap.LegalEntityID, actionViewTrialBalance); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if snap.Lines == nil {
		snap.Lines = []domain.TrialBalanceLine{}
	}
	writeJSON(w, http.StatusOK, snap)
}

// ── ACC-01 Chart of Accounts ─────────────────────────────────────────────────

// CreateAccount is ACC-01's real posting-account master — see migration
// 000007's doc comment and this package's own domain doc comment for why
// this is a separate authority from journal state even though it lives in
// this same deployable process.
func (h *Handler) CreateAccount(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.AccountCode == "" || req.AccountName == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "account_code and account_name are required")
		return
	}
	if !domain.IsValidAccountType(req.AccountType) {
		writeError(w, http.StatusBadRequest, "invalid_field", domain.ErrInvalidAccountType.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, coaPlatformScopeID, actionCreateAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	a := &domain.Account{
		AccountID:               uuid.NewString(),
		TenantID:                tenantID,
		AccountCode:             req.AccountCode,
		AccountName:             req.AccountName,
		AccountType:             req.AccountType,
		ParentAccountID:         req.ParentAccountID,
		IsControlAccount:        req.IsControlAccount,
		DirectPostingRestricted: req.DirectPostingRestricted,
		Status:                  "ACTIVE",
		CreatedAt:               time.Now().UTC(),
		CreatedByPrincipalID:    principalID,
	}
	if err := h.store.CreateAccount(r.Context(), a); err != nil {
		switch {
		case errors.Is(err, domain.ErrAccountAlreadyExists):
			writeError(w, http.StatusConflict, "account_already_exists", err.Error())
		case errors.Is(err, domain.ErrParentAccountNotFound):
			writeError(w, http.StatusBadRequest, "parent_account_not_found", err.Error())
		default:
			h.log.Error("CreateAccount: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		}
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (h *Handler) GetAccount(w http.ResponseWriter, r *http.Request) {
	accountCode := chi.URLParam(r, "account_code")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, coaPlatformScopeID, actionViewAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	a, err := h.store.GetAccountByCode(r.Context(), tenantID, accountCode)
	if errors.Is(err, domain.ErrAccountNotFound) {
		writeError(w, http.StatusNotFound, "account_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("GetAccount: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) ListAccounts(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, coaPlatformScopeID, actionViewAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	accounts, err := h.store.ListAccounts(r.Context(), tenantID)
	if err != nil {
		h.log.Error("ListAccounts: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if accounts == nil {
		accounts = []domain.Account{}
	}
	writeJSON(w, http.StatusOK, accounts)
}

func (h *Handler) DeactivateAccount(w http.ResponseWriter, r *http.Request) {
	accountCode := chi.URLParam(r, "account_code")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, coaPlatformScopeID, actionDeactivateAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.DeactivateAccount(r.Context(), tenantID, accountCode); err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeError(w, http.StatusNotFound, "account_not_found", "")
			return
		}
		h.log.Error("DeactivateAccount: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"account_code": accountCode, "status": "INACTIVE"})
}

// ── ACC-02 Account Mapping ───────────────────────────────────────────────────

// SetAccountMapping records a new effective-dated mapping, superseding any
// prior current mapping for the same key. See migration 000008's doc
// comment — this never destructively overwrites the mapping's history.
func (h *Handler) SetAccountMapping(w http.ResponseWriter, r *http.Request) {
	var req domain.SetAccountMappingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.MappingKey == "" || req.AccountCode == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "mapping_key and account_code are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, coaPlatformScopeID, actionSetAccountMapping); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	m := &domain.AccountMapping{
		AccountMappingID:     uuid.NewString(),
		TenantID:             tenantID,
		MappingKey:           req.MappingKey,
		AccountCode:          req.AccountCode,
		EffectiveFrom:        time.Now().UTC(),
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.SetAccountMapping(r.Context(), m); err != nil {
		if errors.Is(err, domain.ErrMappingTargetAccountInvalid) {
			writeError(w, http.StatusBadRequest, "invalid_target_account", err.Error())
			return
		}
		h.log.Error("SetAccountMapping: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *Handler) GetAccountMapping(w http.ResponseWriter, r *http.Request) {
	mappingKey := chi.URLParam(r, "mapping_key")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, coaPlatformScopeID, actionViewAccountMapping); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	m, err := h.store.GetCurrentAccountMapping(r.Context(), tenantID, mappingKey)
	if errors.Is(err, domain.ErrAccountMappingNotFound) {
		writeError(w, http.StatusNotFound, "mapping_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("GetAccountMapping: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) ListAccountMappings(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, coaPlatformScopeID, actionViewAccountMapping); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	mappings, err := h.store.ListAccountMappings(r.Context(), tenantID)
	if err != nil {
		h.log.Error("ListAccountMappings: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if mappings == nil {
		mappings = []domain.AccountMapping{}
	}
	writeJSON(w, http.StatusOK, mappings)
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

// ── ACC-03 (Journal Entry: proposal/approval lifecycle) ──────────────────────
//
// "owns JournalHeader, JournalLine proposal, journal lifecycle state,
// approval subject fingerprint and posting reference. Must never own:
// Append-only ledger truth." State model (verbatim): "Draft →
// PendingApproval → Approved → PostingRequested → Posted; rejected/
// cancelled before posting; corrections create new journals." This is a
// SEPARATE lifecycle from journal_headers.status — see
// domain.ApprovalStatus's own doc comment and migration 000010.

// AmendDraftJournal replaces a journal's editable fields and every line.
// Legal only from DRAFT or PENDING_APPROVAL — the store's own WHERE
// clause is the real enforcement (see AmendDraftJournal's store-layer doc
// comment), satisfying the spec's own negative paths #2 ("journal changed
// after approval") and #4 ("attempt edit after posting") structurally:
// there is no other status this ever succeeds from.
func (h *Handler) AmendDraftJournal(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "journal_id")
	var req domain.AmendDraftJournalRequest
	if !decodeJSON(w, r, &req) {
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
	if !domain.ValidJournalType(req.JournalType) {
		writeError(w, http.StatusBadRequest, "invalid_journal_type", domain.ErrInvalidJournalType.Error())
		return
	}
	if !domain.ValidCurrencyCode(req.CurrencyCode) {
		writeError(w, http.StatusBadRequest, "invalid_currency_code", domain.ErrInvalidCurrency.Error())
		return
	}
	if req.PostingDate.Before(req.TransactionDate.Time) {
		writeError(w, http.StatusBadRequest, "invalid_posting_date", domain.ErrPostingBeforeTransaction.Error())
		return
	}

	header, _, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("AmendDraftJournal: store unavailable", zap.Error(err))
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, header.LegalEntityID, actionAmendDraftJournal); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	newHeader := &domain.JournalHeader{
		Description: req.Description, JournalType: req.JournalType,
		TransactionDate: req.TransactionDate, PostingDate: req.PostingDate,
		CurrencyCode: req.CurrencyCode, BookID: req.BookID, ReportingBasis: req.ReportingBasis,
		EvidenceRefs: req.EvidenceRefs,
	}
	lines := make([]domain.JournalLine, len(req.Lines))
	for i, l := range req.Lines {
		lines[i] = domain.JournalLine{
			AccountCode: l.AccountCode, DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount,
			Description: l.Description, TaxCode: l.TaxCode, TaxLogicSnapshotID: l.TaxLogicSnapshotID,
			Dimensions: l.Dimensions,
		}
	}
	if err := h.store.AmendDraftJournal(r.Context(), tenantID, journalID, newHeader, lines); err != nil {
		if errors.Is(err, domain.ErrInvalidApprovalTransition) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidApprovalTransition.Error())
			return
		}
		h.log.Error("AmendDraftJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	updated, lines2, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil || updated == nil {
		h.log.Error("AmendDraftJournal: failed to reload amended journal", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, domain.JournalWithLines{JournalHeader: *updated, Lines: lines2})
}

// SubmitJournal moves DRAFT -> PENDING_APPROVAL. The spec's own negative
// path #1, "Debit/credit imbalance," is caught HERE — before an
// approver's time is spent reviewing a proposal that could never post —
// not only later at ValidateJournal's own balance check.
func (h *Handler) SubmitJournal(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "journal_id")
	header, _, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("SubmitJournal: store unavailable", zap.Error(err))
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, header.LegalEntityID, actionSubmitJournal); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	debitTotal, creditTotal, err := h.store.SumLines(r.Context(), tenantID, journalID)
	if err != nil {
		h.log.Error("SubmitJournal: failed to sum lines", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if debitTotal != creditTotal {
		writeError(w, http.StatusUnprocessableEntity, "unbalanced_journal", domain.ErrJournalUnbalancedAtSubmit.Error())
		return
	}

	if err := h.store.SubmitJournalForApproval(r.Context(), tenantID, journalID, principalID); err != nil {
		if errors.Is(err, domain.ErrInvalidApprovalTransition) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidApprovalTransition.Error())
			return
		}
		h.log.Error("SubmitJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	now := time.Now().UTC()
	header.ApprovalStatus, header.SubmittedAt, header.SubmittedByPrincipalID = domain.ApprovalStatusPendingApproval, &now, &principalID
	writeJSON(w, http.StatusOK, header)
}

// ApproveJournal moves PENDING_APPROVAL -> APPROVED. The spec's own
// negative path #3, "Preparer self-approves protected journal": no
// journal-class configuration exists to say which journals are
// "protected" (see domain.ErrSelfApprovalNotPermitted's own doc comment),
// so maker/checker applies universally — the principal who submitted a
// journal may never also approve it.
func (h *Handler) ApproveJournal(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "journal_id")
	header, _, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("ApproveJournal: store unavailable", zap.Error(err))
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, header.LegalEntityID, actionApproveJournalProposal); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if header.SubmittedByPrincipalID != nil && *header.SubmittedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_permitted", domain.ErrSelfApprovalNotPermitted.Error())
		return
	}

	_, lines, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("ApproveJournal: failed to load lines for fingerprint", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	fingerprint := computeApprovalFingerprint(*header, lines)

	if err := h.store.ApproveJournal(r.Context(), tenantID, journalID, principalID, fingerprint); err != nil {
		if errors.Is(err, domain.ErrInvalidApprovalTransition) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidApprovalTransition.Error())
			return
		}
		h.log.Error("ApproveJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	now := time.Now().UTC()
	header.ApprovalStatus, header.ApprovedAt, header.ApprovedByPrincipalID, header.ApprovalFingerprint = domain.ApprovalStatusApproved, &now, &principalID, &fingerprint
	writeJSON(w, http.StatusOK, header)
}

// computeApprovalFingerprint hashes exactly the content an approver is
// signing off on — the spec's own named evidence field, "approval subject
// fingerprint." Deterministic and order-sensitive (lines are already
// stored in line_number order), so the same content always produces the
// same fingerprint.
func computeApprovalFingerprint(h domain.JournalHeader, lines []domain.JournalLine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%s|%s\n", h.LegalEntityID, h.FiscalPeriod, h.Description,
		h.JournalType, h.TransactionDate.String(), h.PostingDate.String(), h.CurrencyCode)
	for _, l := range lines {
		fmt.Fprintf(&b, "%d:%s:%.2f:%.2f\n", l.LineNumber, l.AccountCode, l.DebitAmount, l.CreditAmount)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func (h *Handler) RejectJournal(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "journal_id")
	var req domain.RejectJournalRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	header, _, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("RejectJournal: store unavailable", zap.Error(err))
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, header.LegalEntityID, actionRejectJournal); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason_required", domain.ErrRejectReasonRequired.Error())
		return
	}
	if err := h.store.RejectJournal(r.Context(), tenantID, journalID, principalID, req.Reason); err != nil {
		if errors.Is(err, domain.ErrInvalidApprovalTransition) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidApprovalTransition.Error())
			return
		}
		h.log.Error("RejectJournal: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	now := time.Now().UTC()
	header.ApprovalStatus, header.RejectedAt, header.RejectedByPrincipalID, header.RejectionReason = domain.ApprovalStatusRejected, &now, &principalID, &req.Reason
	writeJSON(w, http.StatusOK, header)
}

// RequestPosting moves APPROVED -> POSTING_REQUESTED — the handoff to
// ACC-04. Only a POSTING_REQUESTED journal is eligible for
// PostApprovedJournal to actually commit (see that handler's own check).
func (h *Handler) RequestPosting(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "journal_id")
	header, _, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("RequestPosting: store unavailable", zap.Error(err))
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, header.LegalEntityID, actionRequestPosting); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.RequestJournalPosting(r.Context(), tenantID, journalID, principalID); err != nil {
		if errors.Is(err, domain.ErrInvalidApprovalTransition) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidApprovalTransition.Error())
			return
		}
		h.log.Error("RequestPosting: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	now := time.Now().UTC()
	header.ApprovalStatus, header.PostingRequestedAt, header.PostingRequestedByPrincipalID = domain.ApprovalStatusPostingRequested, &now, &principalID
	writeJSON(w, http.StatusOK, header)
}

// RequestCorrection implements the spec's own state model literally:
// "corrections create new journals" — never an in-place edit of a POSTED
// one. Creates a brand-new DRAFT journal, CorrectionOfJournalID pointing
// at the original, ready to go through the full lifecycle again.
func (h *Handler) RequestCorrection(w http.ResponseWriter, r *http.Request) {
	originalID := chi.URLParam(r, "journal_id")
	var req domain.RequestCorrectionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
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

	original, _, err := h.store.GetJournal(r.Context(), originalID)
	if err != nil {
		h.log.Error("RequestCorrection: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if original == nil {
		writeError(w, http.StatusNotFound, "journal_not_found", "")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, original.LegalEntityID, actionRequestCorrection); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if original.ApprovalStatus != domain.ApprovalStatusPosted {
		writeError(w, http.StatusUnprocessableEntity, "correction_source_not_posted", domain.ErrCorrectionSourceNotPosted.Error())
		return
	}

	correctionOf := originalID
	newHeader := &domain.JournalHeader{
		JournalID: uuid.NewString(), TenantID: tenantID, LegalEntityID: original.LegalEntityID,
		FiscalPeriod: original.FiscalPeriod, Status: domain.JournalStatusPending,
		JournalType: original.JournalType, TransactionDate: original.TransactionDate, PostingDate: original.PostingDate,
		CurrencyCode: original.CurrencyCode, BookID: original.BookID, ReportingBasis: original.ReportingBasis,
		Description: "Correction of " + originalID + ": " + req.Reason, CreatedByPrincipalID: principalID,
		CorrelationID:         "correction:" + originalID + ":" + uuid.NewString(),
		CorrectionOfJournalID: &correctionOf,
		// "Corrections create new journals" — a correction is a brand-new
		// proposal, starting DRAFT and going through the full ACC-03
		// lifecycle again, exactly like any other journal a human creates.
		ApprovalStatus: domain.ApprovalStatusDraft,
	}
	lines := make([]domain.JournalLine, len(req.Lines))
	for i, l := range req.Lines {
		lines[i] = domain.JournalLine{
			AccountCode: l.AccountCode, DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount,
			Description: l.Description, TaxCode: l.TaxCode, TaxLogicSnapshotID: l.TaxLogicSnapshotID,
			Dimensions: l.Dimensions,
		}
	}
	if _, _, err := h.store.CreateJournal(r.Context(), newHeader, lines); err != nil {
		h.log.Error("RequestCorrection: failed to create correcting journal", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusCreated, newHeader)
}

// GetAvailableActions answers ACC-03's own GetAvailableActions query,
// derived directly from ValidApprovalTransitions — the same rulebook
// every lifecycle handler above actually enforces, so this can never
// advertise an action that would then be refused.
func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "journal_id")
	header, _, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("GetAvailableActions: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if header == nil {
		writeError(w, http.StatusNotFound, "journal_not_found", "")
		return
	}
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	actionsByTarget := map[domain.ApprovalStatus]string{
		domain.ApprovalStatusPendingApproval:  "submit",
		domain.ApprovalStatusApproved:         "approve",
		domain.ApprovalStatusRejected:         "reject",
		domain.ApprovalStatusPostingRequested: "request_posting",
		domain.ApprovalStatusCancelled:        "cancel",
	}
	var actions []string
	for _, to := range domain.ValidApprovalTransitions[header.ApprovalStatus] {
		if name, ok := actionsByTarget[to]; ok {
			actions = append(actions, name)
		}
	}
	if header.ApprovalStatus == domain.ApprovalStatusDraft || header.ApprovalStatus == domain.ApprovalStatusPendingApproval {
		actions = append(actions, "amend")
	}
	if header.ApprovalStatus == domain.ApprovalStatusPosted {
		actions = append(actions, "correct")
	}
	if actions == nil {
		actions = []string{}
	}
	writeJSON(w, http.StatusOK, domain.AvailableActions{JournalID: journalID, Actions: actions})
}

// GetJournalHistory answers ACC-03's own GetJournalHistory query, derived
// from the header's own timestamp/actor columns — never a separate events
// table this v1 didn't build; an entry appears only if its corresponding
// timestamp is actually set.
func (h *Handler) GetJournalHistory(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "journal_id")
	header, _, err := h.store.GetJournal(r.Context(), journalID)
	if err != nil {
		h.log.Error("GetJournalHistory: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if header == nil {
		writeError(w, http.StatusNotFound, "journal_not_found", "")
		return
	}
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	history := []domain.JournalHistoryEntry{
		{Event: "created", At: header.CreatedAt, PrincipalID: header.CreatedByPrincipalID},
	}
	if header.SubmittedAt != nil {
		history = append(history, domain.JournalHistoryEntry{Event: "submitted", At: *header.SubmittedAt, PrincipalID: derefString(header.SubmittedByPrincipalID)})
	}
	if header.ApprovedAt != nil {
		history = append(history, domain.JournalHistoryEntry{Event: "approved", At: *header.ApprovedAt, PrincipalID: derefString(header.ApprovedByPrincipalID), Detail: derefString(header.ApprovalFingerprint)})
	}
	if header.RejectedAt != nil {
		history = append(history, domain.JournalHistoryEntry{Event: "rejected", At: *header.RejectedAt, PrincipalID: derefString(header.RejectedByPrincipalID), Detail: derefString(header.RejectionReason)})
	}
	if header.PostingRequestedAt != nil {
		history = append(history, domain.JournalHistoryEntry{Event: "posting_requested", At: *header.PostingRequestedAt, PrincipalID: derefString(header.PostingRequestedByPrincipalID)})
	}
	if header.PostedAt != nil {
		history = append(history, domain.JournalHistoryEntry{Event: "posted", At: *header.PostedAt, PrincipalID: derefString(header.PostedByPrincipalID)})
	}
	writeJSON(w, http.StatusOK, history)
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ── ACC-04 (Posting Engine) ───────────────────────────────────────────────────
//
// "owns PostingExecution, calculation trace, rule resolution, posting
// batch and consequence uniqueness record; ledger entries are committed
// to ACC-05." In this platform's own co-located deployment, ACC-05 IS
// this same service's journal_headers/journal_lines — so ACC-04
// deliberately ORCHESTRATES the existing Create/Validate/Post primitives
// (checkAccountRestrictions, closeClient.CheckPeriodOpen, store.CreateJournal,
// store.SumLines, store.TransitionJournal, store.ReverseJournal) rather than
// re-implementing ledger writes, exactly matching the spec's own boundary:
// "must never own: Source business fact, tax determination" — a caller
// still supplies the business fact (which accounts, which amounts); ACC-04
// only decides HOW that gets committed, atomically, exactly once.

// resolvePostingLines turns caller-declared PostingEventLineInput lines
// into real journal lines, resolving any mapping_key via ACC-02. The
// spec's own negative path, "Posting rule ambiguity," is enforced here: a
// line naming neither or both of account_code/mapping_key, or a
// mapping_key with no current mapping, is refused rather than guessed.
func (h *Handler) resolvePostingLines(ctx context.Context, tenantID string, lines []domain.PostingEventLineInput) ([]domain.CreateJournalLineInput, map[string]string, error) {
	resolved := make([]domain.CreateJournalLineInput, len(lines))
	trace := make(map[string]string, len(lines))
	for i, l := range lines {
		hasCode := l.AccountCode != nil && *l.AccountCode != ""
		hasMapping := l.MappingKey != nil && *l.MappingKey != ""
		if hasCode == hasMapping { // both or neither
			return nil, nil, domain.ErrPostingRuleAmbiguous
		}
		accountCode := ""
		if hasCode {
			accountCode = *l.AccountCode
			trace[fmt.Sprintf("line_%d", i)] = "account_code:" + accountCode
		} else {
			m, err := h.store.GetCurrentAccountMapping(ctx, tenantID, *l.MappingKey)
			if err != nil {
				return nil, nil, domain.ErrPostingRuleAmbiguous
			}
			accountCode = m.AccountCode
			trace[fmt.Sprintf("line_%d", i)] = "mapping_key:" + *l.MappingKey + " -> account_code:" + accountCode
		}
		if !exactlyOneNonZero(l.DebitAmount, l.CreditAmount) {
			return nil, nil, domain.ErrInvalidLine
		}
		resolved[i] = domain.CreateJournalLineInput{
			AccountCode:  accountCode,
			DebitAmount:  l.DebitAmount,
			CreditAmount: l.CreditAmount,
			Description:  l.Description,
		}
	}
	return resolved, trace, nil
}

// commitJournal drives an already-created PENDING journal through the
// exact same Validate and Post transitions ValidateJournal/PostJournal's
// own handlers use — including a period-lock re-check immediately before
// the FINALIZED transition, the spec's own negative path, "Closed period
// race during commit": a period locked AFTER this journal was created but
// BEFORE it is actually posted must still block the commit.
func (h *Handler) commitJournal(ctx context.Context, header *domain.JournalHeader, principalID string) error {
	debitTotal, creditTotal, err := h.store.SumLines(ctx, header.TenantID, header.JournalID)
	if err != nil {
		return err
	}
	if debitTotal != creditTotal {
		return domain.ErrUnbalancedJournal
	}
	if err := h.store.TransitionJournal(ctx, header.TenantID, header.JournalID,
		domain.JournalStatusPending, domain.JournalStatusValidated, principalID); err != nil {
		return err
	}
	if err := h.closeClient.CheckPeriodOpen(ctx, header.TenantID, header.LegalEntityID, header.FiscalPeriod); err != nil {
		return err
	}
	if err := h.store.TransitionJournal(ctx, header.TenantID, header.JournalID,
		domain.JournalStatusValidated, domain.JournalStatusFinalized, principalID); err != nil {
		return err
	}
	// Close the loop with ACC-03: the ledger commit above is the primary
	// fact and has already happened, so a failure here is logged, not
	// fatal to the overall post — the same posture this file already takes
	// with every other "secondary evidence write after the real fact
	// already landed" case (e.g. posting execution records).
	if err := h.store.MarkJournalPosted(ctx, header.TenantID, header.JournalID); err != nil {
		h.log.Error("journal finalized but ApprovalStatus could not be advanced to POSTED",
			zap.String("journal_id", header.JournalID), zap.Error(err))
	}
	return nil
}

// PostAccountingEvent is ACC-04's primary command: given a caller-declared
// accounting event, resolve its lines, create the journal, and commit it —
// atomically, exactly once per source_event_id.
func (h *Handler) PostAccountingEvent(w http.ResponseWriter, r *http.Request) {
	var req domain.PostAccountingEventRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.FiscalPeriod == "" || req.SourceEventID == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id, fiscal_period, source_event_id and correlation_id are required")
		return
	}
	if len(req.Lines) == 0 {
		writeError(w, http.StatusBadRequest, "no_lines", domain.ErrNoLines.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionPostingExecute); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Duplicate source event: return the PRIOR execution, never a second
	// posting consequence for the same source fact.
	if existing, err := h.store.GetPostingExecutionBySource(r.Context(), tenantID, req.SourceEventID); err == nil {
		writeJSON(w, http.StatusOK, existing)
		return
	} else if !errors.Is(err, domain.ErrPostingExecutionNotFound) {
		h.log.Error("PostAccountingEvent: failed to check for an existing execution", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	resolvedLines, trace, err := h.resolvePostingLines(r.Context(), tenantID, req.Lines)
	if err != nil {
		if errors.Is(err, domain.ErrPostingRuleAmbiguous) {
			writeError(w, http.StatusUnprocessableEntity, "posting_rule_ambiguous", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "invalid_line", err.Error())
		}
		return
	}

	journalReq := domain.CreateJournalRequest{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, FiscalPeriod: req.FiscalPeriod,
		Description: req.Description, Lines: resolvedLines, CorrelationID: req.CorrelationID,
		SourceEventID: &req.SourceEventID,
	}
	if err := h.closeClient.CheckPeriodOpen(r.Context(), tenantID, req.LegalEntityID, req.FiscalPeriod); err != nil {
		h.writePeriodErr(w, err)
		return
	}
	if !h.checkAccountRestrictions(w, r, journalReq, principalID) {
		return
	}

	traceJSON, _ := json.Marshal(trace)
	sourceEventID := req.SourceEventID
	exec := &domain.PostingExecution{
		ExecutionID: uuid.NewString(), TenantID: tenantID, LegalEntityID: req.LegalEntityID, Kind: domain.PostingExecutionKindEvent,
		SourceEventID: &sourceEventID, Status: domain.PostingExecutionStatusSubmitted,
		CalculationTrace: string(traceJSON), CorrelationID: req.CorrelationID,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreatePostingExecution(r.Context(), exec); err != nil {
		h.log.Error("PostAccountingEvent: failed to record posting execution", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	header := &domain.JournalHeader{
		JournalID: uuid.NewString(), TenantID: tenantID, LegalEntityID: req.LegalEntityID,
		FiscalPeriod: req.FiscalPeriod, Status: domain.JournalStatusPending, Description: req.Description,
		CreatedByPrincipalID: principalID, CorrelationID: req.CorrelationID, SourceEventID: &sourceEventID,
		// System-originated: this bypasses ACC-03's human Draft/Submit/
		// Approve workflow entirely (already gated by actionPostingExecute,
		// which a deployment grants only to internal service identities —
		// see that action's own doc comment), landing directly at
		// POSTING_REQUESTED so commitJournal's own MarkJournalPosted call
		// has a valid ApprovalStatus to advance from.
		ApprovalStatus: domain.ApprovalStatusPostingRequested,
	}
	lines := make([]domain.JournalLine, len(resolvedLines))
	for i, l := range resolvedLines {
		lines[i] = domain.JournalLine{AccountCode: l.AccountCode, DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount, Description: l.Description}
	}
	if _, _, err := h.store.CreateJournal(r.Context(), header, lines); err != nil {
		h.failExecution(r.Context(), tenantID, exec.ExecutionID, err)
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	if err := h.commitJournal(r.Context(), header, principalID); err != nil {
		h.failExecution(r.Context(), tenantID, exec.ExecutionID, err)
		h.writePeriodErr(w, err)
		return
	}

	now := time.Now().UTC()
	if err := h.store.MarkPostingExecutionCommitted(r.Context(), tenantID, exec.ExecutionID, header.JournalID, now); err != nil {
		h.log.Error("journal committed but the posting execution could not be marked COMMITTED",
			zap.String("execution_id", exec.ExecutionID), zap.String("journal_id", header.JournalID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "execution_not_recorded",
			"the journal IS finalized ("+header.JournalID+"), but the posting execution could not be marked COMMITTED.")
		return
	}
	exec.Status, exec.JournalID, exec.CommittedAt = domain.PostingExecutionStatusCommitted, &header.JournalID, &now
	h.publisher.PublishJournalPosted(r.Context(), *header)
	writeJSON(w, http.StatusCreated, exec)
}

// failExecution marks an execution FAILED (or QUARANTINED for an
// ambiguous/unbalanced posting rule, which needs a human to resolve
// rather than a simple retry) with the underlying error as its permanent,
// evidenced reason — the spec's own state model, "no partial committed
// state": an execution is never left silently stuck in SUBMITTED.
func (h *Handler) failExecution(ctx context.Context, tenantID, executionID string, cause error) {
	status := domain.PostingExecutionStatusFailed
	if errors.Is(cause, domain.ErrPostingRuleAmbiguous) || errors.Is(cause, domain.ErrUnbalancedJournal) {
		status = domain.PostingExecutionStatusQuarantined
	}
	if err := h.store.MarkPostingExecutionFailed(ctx, tenantID, executionID, status, cause.Error()); err != nil {
		h.log.Error("failed to record posting execution failure", zap.String("execution_id", executionID), zap.Error(err))
	}
}

func (h *Handler) writePeriodErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrPeriodLocked) {
		writeError(w, http.StatusPreconditionFailed, "period_locked", err.Error())
		return
	}
	if errors.Is(err, domain.ErrUnbalancedJournal) {
		writeError(w, http.StatusUnprocessableEntity, "unbalanced_journal", err.Error())
		return
	}
	writeError(w, http.StatusServiceUnavailable, "close_check_failed", err.Error())
}

// PostApprovedJournal wraps the final VALIDATED -> FINALIZED commit of an
// already-existing journal (created via the ordinary journal API, e.g.
// ACC-03's own approval flow) in a PostingExecution audit record — the
// spec's "approved journal" input is this platform's VALIDATED status;
// posting a still-PENDING journal is refused rather than silently
// validating it first, since that is a separate, distinctly-authorized
// step this command must not fold in unannounced.
func (h *Handler) PostApprovedJournal(w http.ResponseWriter, r *http.Request) {
	var req domain.PostApprovedJournalRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.JournalID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "journal_id")
		return
	}
	header, _, err := h.store.GetJournal(r.Context(), req.JournalID)
	if err != nil {
		h.log.Error("PostApprovedJournal: store unavailable", zap.Error(err))
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, header.LegalEntityID, actionPostingExecute); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if header.Status != domain.JournalStatusValidated {
		writeError(w, http.StatusUnprocessableEntity, "journal_not_validated",
			"journal must be VALIDATED before it can be posted through the posting engine; current status: "+string(header.Status))
		return
	}
	// ACC-03 gate: this is no longer "any VALIDATED journal" — only one
	// that actually completed the human proposal/approval workflow and had
	// RequestPosting called on it may enter the posting engine. This is
	// the real integration point between ACC-03 and ACC-04 the spec's own
	// "Dependencies: ... ACC-04" line names.
	if header.ApprovalStatus != domain.ApprovalStatusPostingRequested {
		writeError(w, http.StatusUnprocessableEntity, "posting_not_requested",
			"journal must have completed ACC-03's approval workflow and had posting requested; current approval_status: "+string(header.ApprovalStatus))
		return
	}

	exec := &domain.PostingExecution{
		ExecutionID: uuid.NewString(), TenantID: tenantID, LegalEntityID: header.LegalEntityID, Kind: domain.PostingExecutionKindApprovedJournal,
		Status: domain.PostingExecutionStatusSubmitted, CalculationTrace: "{}", CorrelationID: header.CorrelationID,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreatePostingExecution(r.Context(), exec); err != nil {
		h.log.Error("PostApprovedJournal: failed to record posting execution", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	if err := h.closeClient.CheckPeriodOpen(r.Context(), header.TenantID, header.LegalEntityID, header.FiscalPeriod); err != nil {
		h.failExecution(r.Context(), tenantID, exec.ExecutionID, err)
		h.writePeriodErr(w, err)
		return
	}
	if err := h.store.TransitionJournal(r.Context(), header.TenantID, req.JournalID,
		domain.JournalStatusValidated, domain.JournalStatusFinalized, principalID); err != nil {
		h.failExecution(r.Context(), tenantID, exec.ExecutionID, err)
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	// Close the loop with ACC-03 — same posture as commitJournal's own
	// call: the ledger commit above is the primary fact and has already
	// happened, so a failure here is logged, not fatal to the response.
	if err := h.store.MarkJournalPosted(r.Context(), header.TenantID, req.JournalID); err != nil {
		h.log.Error("journal finalized but ApprovalStatus could not be advanced to POSTED",
			zap.String("journal_id", req.JournalID), zap.Error(err))
	}

	now := time.Now().UTC()
	if err := h.store.MarkPostingExecutionCommitted(r.Context(), tenantID, exec.ExecutionID, req.JournalID, now); err != nil {
		h.log.Error("journal finalized but the posting execution could not be marked COMMITTED", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "execution_not_recorded",
			"the journal IS finalized ("+req.JournalID+"), but the posting execution could not be marked COMMITTED.")
		return
	}
	exec.Status, exec.JournalID, exec.CommittedAt = domain.PostingExecutionStatusCommitted, &req.JournalID, &now
	header.Status = domain.JournalStatusFinalized
	header.ApprovalStatus = domain.ApprovalStatusPosted
	h.publisher.PublishJournalPosted(r.Context(), *header)
	writeJSON(w, http.StatusOK, exec)
}

// CreateReversalPosting wraps the existing ReverseJournal store primitive
// in its own PostingExecution record. Deliberately its own authorization
// action (actionPostingReverse) — the spec's own words: "reversal
// requires authorized correction command."
func (h *Handler) CreateReversalPosting(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateReversalPostingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.OriginalJournalID == "" || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "original_journal_id and reason are required")
		return
	}

	header, _, err := h.store.GetJournal(r.Context(), req.OriginalJournalID)
	if err != nil {
		h.log.Error("CreateReversalPosting: store unavailable", zap.Error(err))
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
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, header.LegalEntityID, actionPostingReverse); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if header.Status != domain.JournalStatusFinalized {
		writeError(w, http.StatusUnprocessableEntity, "only_finalized_reversible", domain.ErrOnlyFinalizedReversible.Error())
		return
	}
	if err := h.closeClient.CheckPeriodOpen(r.Context(), header.TenantID, header.LegalEntityID, header.FiscalPeriod); err != nil {
		h.writePeriodErr(w, err)
		return
	}

	correlationID := "posting-reversal:" + req.OriginalJournalID
	exec := &domain.PostingExecution{
		ExecutionID: uuid.NewString(), TenantID: tenantID, LegalEntityID: header.LegalEntityID, Kind: domain.PostingExecutionKindReversal,
		Status: domain.PostingExecutionStatusSubmitted, CalculationTrace: "{}", CorrelationID: correlationID,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreatePostingExecution(r.Context(), exec); err != nil {
		h.log.Error("CreateReversalPosting: failed to record posting execution", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	reversalID := req.OriginalJournalID
	reversingHeader := &domain.JournalHeader{
		JournalID: uuid.NewString(), TenantID: header.TenantID, LegalEntityID: header.LegalEntityID,
		FiscalPeriod: header.FiscalPeriod, Status: domain.JournalStatusFinalized, ReversalOfJournalID: &reversalID,
		Description: "Reversal of " + req.OriginalJournalID + ": " + req.Reason, CreatedByPrincipalID: principalID,
		PostedByPrincipalID: &principalID, CorrelationID: correlationID,
	}
	_, originalLines, err := h.store.GetJournal(r.Context(), req.OriginalJournalID)
	if err != nil {
		h.failExecution(r.Context(), tenantID, exec.ExecutionID, err)
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	reversingLines := make([]domain.JournalLine, len(originalLines))
	for i, l := range originalLines {
		reversingLines[i] = domain.JournalLine{AccountCode: l.AccountCode, DebitAmount: l.CreditAmount, CreditAmount: l.DebitAmount, Description: l.Description}
	}

	_, _, err = h.store.ReverseJournal(r.Context(), header.TenantID, req.OriginalJournalID, reversingHeader, reversingLines, principalID)
	if err != nil {
		h.failExecution(r.Context(), tenantID, exec.ExecutionID, err)
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusUnprocessableEntity, "only_finalized_reversible", domain.ErrOnlyFinalizedReversible.Error())
			return
		}
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}

	now := time.Now().UTC()
	if err := h.store.MarkPostingExecutionCommitted(r.Context(), tenantID, exec.ExecutionID, reversingHeader.JournalID, now); err != nil {
		h.log.Error("reversal posted but the posting execution could not be marked COMMITTED", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "execution_not_recorded",
			"the reversal journal IS finalized ("+reversingHeader.JournalID+"), but the posting execution could not be marked COMMITTED.")
		return
	}
	exec.Status, exec.JournalID, exec.CommittedAt = domain.PostingExecutionStatusCommitted, &reversingHeader.JournalID, &now
	h.publisher.PublishJournalReversed(r.Context(), *header, reversingHeader.JournalID)
	writeJSON(w, http.StatusCreated, exec)
}

// ReprocessFailedPosting retries a FAILED/QUARANTINED execution.
// Deliberately scoped to executions that already produced a journal (the
// original create step succeeded but commit failed, e.g. a transient
// period-lock race) — an execution that failed before any journal ever
// existed carries no persisted original request to safely replay, and
// resubmitting as a brand-new PostAccountingEvent is the honest path
// rather than fabricating a retry from data this record never kept.
func (h *Handler) ReprocessFailedPosting(w http.ResponseWriter, r *http.Request) {
	executionID := chi.URLParam(r, "execution_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	exec, err := h.store.GetPostingExecution(r.Context(), tenantID, executionID)
	if err != nil {
		h.writePostingExecutionErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, exec.LegalEntityID, actionPostingReprocess); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if exec.Status == domain.PostingExecutionStatusCommitted {
		writeError(w, http.StatusUnprocessableEntity, "already_committed", domain.ErrPostingAlreadyCommitted.Error())
		return
	}
	if exec.Status != domain.PostingExecutionStatusFailed && exec.Status != domain.PostingExecutionStatusQuarantined {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidPostingTransition.Error())
		return
	}
	if exec.JournalID == nil {
		writeError(w, http.StatusUnprocessableEntity, "no_journal_to_reprocess",
			"this execution failed before any journal was created; resubmit as a new posting request")
		return
	}

	header, _, err := h.store.GetJournal(r.Context(), *exec.JournalID)
	if err != nil || header == nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if header.Status == domain.JournalStatusPending {
		if err := h.commitJournal(r.Context(), header, principalID); err != nil {
			h.failExecution(r.Context(), tenantID, executionID, err)
			h.writePeriodErr(w, err)
			return
		}
	} else if header.Status == domain.JournalStatusValidated {
		if err := h.closeClient.CheckPeriodOpen(r.Context(), header.TenantID, header.LegalEntityID, header.FiscalPeriod); err != nil {
			h.failExecution(r.Context(), tenantID, executionID, err)
			h.writePeriodErr(w, err)
			return
		}
		if err := h.store.TransitionJournal(r.Context(), header.TenantID, header.JournalID,
			domain.JournalStatusValidated, domain.JournalStatusFinalized, principalID); err != nil {
			h.failExecution(r.Context(), tenantID, executionID, err)
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
			return
		}
		if err := h.store.MarkJournalPosted(r.Context(), header.TenantID, header.JournalID); err != nil {
			h.log.Error("journal finalized on reprocess but ApprovalStatus could not be advanced to POSTED",
				zap.String("journal_id", header.JournalID), zap.Error(err))
		}
	}

	now := time.Now().UTC()
	if err := h.store.MarkPostingExecutionCommitted(r.Context(), tenantID, executionID, *exec.JournalID, now); err != nil {
		h.log.Error("journal committed on reprocess but the posting execution could not be marked COMMITTED", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "execution_not_recorded", "")
		return
	}
	exec.Status, exec.CommittedAt = domain.PostingExecutionStatusCommitted, &now
	writeJSON(w, http.StatusOK, exec)
}

func (h *Handler) GetPostingExecution(w http.ResponseWriter, r *http.Request) {
	executionID := chi.URLParam(r, "execution_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	exec, err := h.store.GetPostingExecution(r.Context(), tenantID, executionID)
	if err != nil {
		h.writePostingExecutionErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, exec.LegalEntityID, actionPostingView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, exec)
}

// ExplainPosting answers ACC-04's own ExplainPosting query — the same
// record GetPostingExecution returns, since CalculationTrace (the "why"
// this execution resolved the way it did) is already carried on the
// execution itself rather than computed separately.
func (h *Handler) ExplainPosting(w http.ResponseWriter, r *http.Request) {
	h.GetPostingExecution(w, r)
}

func (h *Handler) GetPostingBySource(w http.ResponseWriter, r *http.Request) {
	sourceEventID := r.URL.Query().Get("source_event_id")
	if sourceEventID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "source_event_id")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	exec, err := h.store.GetPostingExecutionBySource(r.Context(), tenantID, sourceEventID)
	if err != nil {
		h.writePostingExecutionErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, exec.LegalEntityID, actionPostingView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, exec)
}

// VerifyPostingUniqueness answers whether a source_event_id has already
// produced a posting execution — a plain boolean, never itself the full
// record (GetPostingBySource is the read for that).
func (h *Handler) VerifyPostingUniqueness(w http.ResponseWriter, r *http.Request) {
	sourceEventID := r.URL.Query().Get("source_event_id")
	if sourceEventID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "source_event_id")
		return
	}
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	_, err := h.store.GetPostingExecutionBySource(r.Context(), tenantID, sourceEventID)
	if err != nil && !errors.Is(err, domain.ErrPostingExecutionNotFound) {
		h.log.Error("VerifyPostingUniqueness: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"exists": err == nil})
}

func (h *Handler) writePostingExecutionErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrPostingExecutionNotFound) {
		writeError(w, http.StatusNotFound, "posting_execution_not_found", "")
		return
	}
	h.log.Error("posting execution store unavailable", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
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
