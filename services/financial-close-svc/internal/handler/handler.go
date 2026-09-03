package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
)

type Store interface {
	CreateFiscalPeriod(ctx context.Context, fp *domain.FiscalPeriod) (created bool, err error)
	GetFiscalPeriod(ctx context.Context, id string) (*domain.FiscalPeriod, error)
	GetFiscalPeriodByName(ctx context.Context, legalEntityID, name string) (*domain.FiscalPeriod, error)
	ListFiscalPeriods(ctx context.Context, legalEntityID string) ([]domain.FiscalPeriod, error)
	LockFiscalPeriod(ctx context.Context, id string, lockedAt time.Time, evidenceDocID string) error
	CreateCloseEvidence(ctx context.Context, evidence *domain.CloseEvidence) error
	// ReopenFiscalPeriod transitions id from LOCKED back to OPEN, atomically
	// and only from LOCKED (mirrors LockFiscalPeriod's OPEN-only guard).
	// clearedEvidenceDocID is what evidence_document_id is reset to (empty
	// string — the prior close's own CloseEvidence row is untouched and
	// stays queryable by fiscal_period_id; only the pointer on the period
	// itself is cleared, since the next close will produce a new one).
	ReopenFiscalPeriod(ctx context.Context, id string, reopenedAt time.Time) error
	CreateReopenEvent(ctx context.Context, event *domain.PeriodReopenEvent) error
	// CreateControlRun persists one ACC-06 subledger-to-GL reconciliation
	// result — append-only, see migration 000004's doc comment.
	CreateControlRun(ctx context.Context, run *domain.SubledgerControlRun) error
	ListControlRuns(ctx context.Context, legalEntityID, fiscalPeriod string) ([]domain.SubledgerControlRun, error)

	// ACC-07 (Accruals) — see internal/domain's AccrualSchedule doc comment
	// for the authority boundary these methods implement.
	CreateAccrualSchedule(ctx context.Context, sch *domain.AccrualSchedule) error
	GetAccrualSchedule(ctx context.Context, scheduleID string) (*domain.AccrualSchedule, error)
	ListAccrualSchedules(ctx context.Context, legalEntityID string) ([]domain.AccrualSchedule, error)
	SubmitAccrualSchedule(ctx context.Context, scheduleID, principalID string, at time.Time) error
	ApproveAccrualSchedule(ctx context.Context, scheduleID, principalID string, at time.Time) error
	ActivateAccrualSchedule(ctx context.Context, scheduleID string) error
	CompleteAccrualSchedule(ctx context.Context, scheduleID string) error
	CancelAccrualSchedule(ctx context.Context, scheduleID, fromStatus, principalID string, at time.Time) error
	AmendAccrualSchedule(ctx context.Context, scheduleID string, totalAmount float64, periodCount int) error
	CreateRecognitionInstance(ctx context.Context, inst *domain.RecognitionInstance) (created bool, err error)
	ListRecognitionInstances(ctx context.Context, scheduleID string) ([]domain.RecognitionInstance, error)

	// ACC-08 (Prepayments & Deferrals) — economically the mirror of ACC-07,
	// see domain.PrepaymentSchedule's doc comment.
	CreatePrepaymentSchedule(ctx context.Context, sch *domain.PrepaymentSchedule) error
	GetPrepaymentSchedule(ctx context.Context, scheduleID string) (*domain.PrepaymentSchedule, error)
	ListPrepaymentSchedules(ctx context.Context, legalEntityID string) ([]domain.PrepaymentSchedule, error)
	ApprovePrepaymentSchedule(ctx context.Context, scheduleID, principalID string, at time.Time) error
	ActivatePrepaymentSchedule(ctx context.Context, scheduleID string) error
	CompletePrepaymentSchedule(ctx context.Context, scheduleID string) error
	TerminatePrepaymentSchedule(ctx context.Context, scheduleID, fromStatus, principalID, reason, treatment string, at time.Time) error
	ModifyFuturePrepaymentSchedule(ctx context.Context, scheduleID string, totalAmount float64, periodCount int) error
	CreatePrepaymentRecognition(ctx context.Context, inst *domain.PrepaymentRecognitionInstance) (created bool, err error)
	ListPrepaymentRecognitions(ctx context.Context, scheduleID string) ([]domain.PrepaymentRecognitionInstance, error)

	// ACC-09 (Allocation Engine) — see domain.AllocationRule/AllocationRun's
	// doc comments for the authority boundary these implement.
	CreateAllocationRule(ctx context.Context, rule *domain.AllocationRule) error
	GetCurrentAllocationRule(ctx context.Context, ruleID string) (*domain.AllocationRule, error)
	GetAllocationRuleVersion(ctx context.Context, ruleVersionID string) (*domain.AllocationRule, error)
	ListAllocationRules(ctx context.Context, legalEntityID string) ([]domain.AllocationRule, error)
	ApproveAllocationRule(ctx context.Context, ruleVersionID, principalID string, at time.Time) error
	ActivateAllocationRule(ctx context.Context, ruleVersionID string) error
	CreateAllocationRun(ctx context.Context, run *domain.AllocationRun) error
	GetAllocationRunByRuleAndPeriod(ctx context.Context, ruleID, fiscalPeriod string) (*domain.AllocationRun, error)
	GetAllocationRun(ctx context.Context, runID string) (*domain.AllocationRun, error)
	MarkAllocationRunCalculated(ctx context.Context, runID string, sourceAmount float64, at time.Time) error
	MarkAllocationRunPosted(ctx context.Context, runID, journalID string, at time.Time) error
	MarkAllocationRunFailed(ctx context.Context, runID, reason string) error
	CreateAllocationResultLines(ctx context.Context, runID string, lines []domain.AllocationResultLine) error
	ListAllocationExceptions(ctx context.Context, legalEntityID string) ([]domain.AllocationRun, error)

	// ACC-10 (Foreign Currency Revaluation) — see domain.FXRevaluationRun's
	// doc comment for the authority boundary these implement.
	CreateFXRevaluationRun(ctx context.Context, run *domain.FXRevaluationRun) error
	GetFXRevaluationRun(ctx context.Context, runID string) (*domain.FXRevaluationRun, error)
	ListFXRevaluationRuns(ctx context.Context, legalEntityID, fiscalPeriod string) ([]domain.FXRevaluationRun, error)
	ApproveFXRevaluationRun(ctx context.Context, runID, principalID string, at time.Time) error
	MarkFXRevaluationPosted(ctx context.Context, runID, journalID, principalID string, at time.Time) error

	// ACC-17 (Opening Balance & Migration) — see domain.MigrationBatch's
	// doc comment for the authority boundary these implement.
	CreateMigrationBatch(ctx context.Context, b *domain.MigrationBatch) error
	GetMigrationBatchBySourceSystem(ctx context.Context, legalEntityID, fiscalPeriod, sourceSystemName string) (*domain.MigrationBatch, error)
	GetMigrationBatch(ctx context.Context, batchID string) (*domain.MigrationBatch, error)
	MarkMigrationBatchValidated(ctx context.Context, batchID string, at time.Time) error
	QuarantineMigrationBatch(ctx context.Context, batchID, fromStatus, reason string) error
	ApproveMigrationBatch(ctx context.Context, batchID, principalID string, at time.Time) error
	MarkMigrationBatchPosted(ctx context.Context, batchID, journalID string, at time.Time) error
	MarkMigrationBatchReconciled(ctx context.Context, batchID string, at time.Time) error
	CertifyMigrationBatch(ctx context.Context, batchID, principalID, reason string, at time.Time) error
	ListQuarantinedMigrationBatches(ctx context.Context, legalEntityID string) ([]domain.MigrationBatch, error)

	// ACC-16 (Signed Financial Snapshot) — see domain.FinancialSnapshot's
	// doc comment for the authority boundary these implement.
	CreateFinancialSnapshot(ctx context.Context, snap *domain.FinancialSnapshot) error
	GetFinancialSnapshot(ctx context.Context, snapshotID string) (*domain.FinancialSnapshot, error)
	SealFinancialSnapshot(ctx context.Context, snapshotID, contentHash, signature string, at time.Time) error
	CertifyFinancialSnapshot(ctx context.Context, snapshotID, principalID, reason string, at time.Time) error
	SupersedeFinancialSnapshot(ctx context.Context, snapshotID, fromStatus, newSnapshotID string, at time.Time) error
	ListSnapshotSupersession(ctx context.Context, legalEntityID, purpose string) ([]domain.FinancialSnapshot, error)

	// ACC-18 (Source-to-Report Traceability) — see domain.LineageEdge's
	// doc comment for the authority boundary these implement.
	RecordLineageEdge(ctx context.Context, edge *domain.LineageEdge) error
	ListLineageEdgesTo(ctx context.Context, toType, toID string) ([]domain.LineageEdge, error)
	ListPostedJournalRefs(ctx context.Context, legalEntityID string) ([]domain.PostedJournalRef, error)
	GetLineageProjectionStatus(ctx context.Context, legalEntityID string) (*domain.LineageProjectionStatus, error)
	UpsertLineageProjectionStatus(ctx context.Context, legalEntityID, status string, degradedReason *string, at *time.Time) error
}

type Publisher interface {
	PublishCloseStarted(ctx context.Context, correlationID, actorID string, fp domain.FiscalPeriod)
	PublishCloseBlocked(ctx context.Context, correlationID, actorID string, fp domain.FiscalPeriod, reasons []string)
	PublishClosed(ctx context.Context, correlationID, actorID string, fp domain.FiscalPeriod, evidenceID string)
	PublishReopened(ctx context.Context, correlationID, actorID string, fp domain.FiscalPeriod, reason string)
	PublishSubledgerControlException(ctx context.Context, correlationID, actorID string, run domain.SubledgerControlRun)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Clients interface {
	GetUnpostedJournalsCount(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (int, error)
	CompileTrialBalance(ctx context.Context, tenantID, legalEntityID, fiscalPeriod, principalID string) (map[string]float64, error)
	// The AP/AR counts take the period bounds: an unsettled invoice only blocks
	// the period it belongs to. Without them a single outstanding invoice
	// anywhere blocked every period forever.
	GetUnsettledAPInvoicesCount(ctx context.Context, tenantID, legalEntityID string, periodStart, periodEnd time.Time) (int, error)
	GetUnsettledARInvoicesCount(ctx context.Context, tenantID, legalEntityID string, periodStart, periodEnd time.Time) (int, error)
	UploadCloseEvidence(ctx context.Context, tenantID, legalEntityID, periodName string, trialBalance map[string]float64, principalID string) (string, error)
	// GetControlAccountCode resolves an ACC-06 caller-declared mapping key to
	// the real chart-registered account code it currently names, via GL's
	// ACC-02 mapping endpoint.
	GetControlAccountCode(ctx context.Context, tenantID, mappingKey string) (string, error)
	GetAPSubledgerTotal(ctx context.Context, tenantID, legalEntityID string) (float64, error)
	GetARSubledgerTotal(ctx context.Context, tenantID, legalEntityID string) (float64, error)
	// PostAccrualRecognitionJournal is ACC-07's only path to the ledger —
	// see its doc comment in internal/clients for why.
	PostAccrualRecognitionJournal(ctx context.Context, tenantID, legalEntityID, fiscalPeriod, correlationID, principalID, description, debitAccountCode, creditAccountCode string, amount float64) (journalID string, err error)
	// GetAccountStatus/PostAllocationJournal back ACC-09 — see their doc
	// comments in internal/clients.
	GetAccountStatus(ctx context.Context, tenantID, principalID, accountCode string) (status string, err error)
	PostAllocationJournal(ctx context.Context, tenantID, legalEntityID, fiscalPeriod, correlationID, principalID, description, sourceAccountCode string, sourceAmount float64, debitLines []domain.AllocationJournalLine) (journalID string, err error)
	// GetAccountType/PostMultiLineJournal back ACC-10 — see their doc
	// comments in internal/clients.
	GetAccountType(ctx context.Context, tenantID, principalID, accountCode string) (accountType string, err error)
	PostMultiLineJournal(ctx context.Context, tenantID, legalEntityID, fiscalPeriod, correlationID, principalID, description string, lines []domain.JournalLineInput) (journalID string, err error)
}

const (
	actionCloseConfig   = "PERIOD_CLOSE_CONFIG"
	actionCloseView     = "PERIOD_CLOSE_VIEW"
	actionCloseInitiate = "PERIOD_CLOSE_INITIATE"
	// actionPeriodReopen is deliberately its own action, not reused from
	// actionCloseInitiate — ACC-14 invariant #6 requires reopen be
	// "explicit, scoped, approved," and a locked book being reopened is a
	// materially more sensitive event than closing one on schedule. Keeping
	// it a separate action lets this be granted to a narrower, more senior
	// group than ordinary close initiation.
	actionPeriodReopen = "PERIOD_REOPEN"

	// actionSubledgerControlRun is ACC-06's own action, distinct from
	// actionCloseInitiate — running a control reconciliation is not part of
	// initiating a close (readiness can call this any time, not just at
	// lock), and granting it separately lets an org authorize reconciliation
	// work to a wider group than period-lock itself.
	actionSubledgerControlRun  = "SUBLEDGER_CONTROL_RUN"
	actionSubledgerControlView = "SUBLEDGER_CONTROL_VIEW"

	// ACC-07 (Accruals) actions. Create/Submit are one authority (the
	// preparer); Approve is deliberately separate (segregation of duties —
	// the same principal creating a schedule and approving it is exactly
	// what an approval step exists to prevent). Recognition and
	// amend/cancel are their own actions too: running a recognition posts
	// a real ledger entry and amending/cancelling changes a schedule's
	// future, both materially different acts from viewing or approving one.
	actionAccrualCreate    = "ACCRUAL_CREATE"
	actionAccrualApprove   = "ACCRUAL_APPROVE"
	actionAccrualRecognize = "ACCRUAL_RECOGNIZE"
	actionAccrualAmend     = "ACCRUAL_AMEND"
	actionAccrualCancel    = "ACCRUAL_CANCEL"
	actionAccrualView      = "ACCRUAL_VIEW"

	// ACC-08 (Prepayments & Deferrals) actions — same segregation-of-duties
	// posture as ACC-07's: approve is its own action, separate from create.
	actionPrepaymentCreate    = "PREPAYMENT_CREATE"
	actionPrepaymentApprove   = "PREPAYMENT_APPROVE"
	actionPrepaymentRecognize = "PREPAYMENT_RECOGNIZE"
	actionPrepaymentModify    = "PREPAYMENT_MODIFY"
	actionPrepaymentTerminate = "PREPAYMENT_TERMINATE"
	actionPrepaymentView      = "PREPAYMENT_VIEW"

	// ACC-09 (Allocation Engine) actions — same segregation-of-duties
	// posture: approving a rule is its own action, separate from creating
	// one, and executing/reprocessing (both post real ledger entries) are
	// separate again from viewing.
	actionAllocationRuleCreate  = "ALLOCATION_RULE_CREATE"
	actionAllocationRuleApprove = "ALLOCATION_RULE_APPROVE"
	actionAllocationExecute     = "ALLOCATION_EXECUTE"
	actionAllocationView        = "ALLOCATION_VIEW"

	// ACC-10 (FX Revaluation) actions — same segregation-of-duties posture:
	// approve and post are each their own action, and both are more
	// sensitive than starting or viewing a run.
	actionFXRevaluationStart   = "FX_REVALUATION_START"
	actionFXRevaluationApprove = "FX_REVALUATION_APPROVE"
	actionFXRevaluationPost    = "FX_REVALUATION_POST"
	actionFXRevaluationView    = "FX_REVALUATION_VIEW"

	// ACC-17 (Opening Balance & Migration) actions. Approve and Certify
	// are each their own action — segregation of duties again, and
	// certification is the final, most sensitive sign-off in the whole
	// pipeline, so it is deliberately its own grant, separate even from
	// approval.
	actionMigrationBatchCreate   = "MIGRATION_BATCH_CREATE"
	actionMigrationBatchValidate = "MIGRATION_BATCH_VALIDATE"
	actionMigrationBatchApprove  = "MIGRATION_BATCH_APPROVE"
	actionMigrationBatchCommit   = "MIGRATION_BATCH_COMMIT"
	actionMigrationBatchCertify  = "MIGRATION_BATCH_CERTIFY"
	actionMigrationBatchView     = "MIGRATION_BATCH_VIEW"

	// ACC-16 (Signed Financial Snapshot) actions. Seal, Certify and
	// Supersede are each their own action — the same escalating
	// segregation-of-duties posture as every other stateful ACC pipeline
	// this pass has built.
	actionSnapshotCreate    = "SNAPSHOT_CREATE"
	actionSnapshotSeal      = "SNAPSHOT_SEAL"
	actionSnapshotCertify   = "SNAPSHOT_CERTIFY"
	actionSnapshotSupersede = "SNAPSHOT_SUPERSEDE"
	actionSnapshotView      = "SNAPSHOT_VIEW"

	// ACC-18 (Source-to-Report Traceability) actions. Rebuilding the
	// projection is a heavier operation than reading it, so it gets its
	// own action rather than reusing the view grant.
	actionLineageView    = "LINEAGE_VIEW"
	actionLineageRebuild = "LINEAGE_REBUILD"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	clients   Clients
	// signingKey is the HMAC secret for close evidence. See signEvidence.
	signingKey []byte
	log        *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, clients Clients, signingKey []byte, log *zap.Logger) *Handler {
	return &Handler{
		store:      store,
		publisher:  publisher,
		authz:      authz,
		clients:    clients,
		signingKey: signingKey,
		log:        log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/close/periods", func(r chi.Router) {
		r.Post("/", h.CreateFiscalPeriod)
		r.Get("/", h.ListFiscalPeriods)
		r.Get("/status", h.GetPeriodStatus)
		r.Get("/{id}/readiness", h.GetPeriodReadiness)
		r.Post("/{id}/lock", h.LockPeriod)
		r.Post("/{id}/reopen", h.ReopenPeriod)
	})
	r.Route("/v1/subledger-control/runs", func(r chi.Router) {
		r.Post("/", h.RunSubledgerControl)
		r.Get("/", h.ListSubledgerControlRuns)
	})
	r.Route("/v1/accruals", func(r chi.Router) {
		r.Post("/", h.CreateAccrual)
		r.Get("/", h.ListAccruals)
		r.Get("/{id}", h.GetAccrual)
		r.Post("/{id}/submit", h.SubmitAccrual)
		r.Post("/{id}/approve", h.ApproveAccrual)
		r.Post("/{id}/amend", h.AmendFutureSchedule)
		r.Post("/{id}/cancel", h.CancelFutureAccrual)
		r.Post("/{id}/recognize", h.RunAccrualRecognition)
		r.Get("/{id}/recognitions", h.ListRecognitions)
	})
	r.Route("/v1/prepayments", func(r chi.Router) {
		r.Post("/", h.CreatePrepayment)
		r.Get("/", h.ListPrepayments)
		r.Get("/{id}", h.GetPrepayment)
		r.Get("/{id}/remaining-balance", h.GetPrepaymentRemainingBalance)
		r.Post("/{id}/approve", h.ApprovePrepayment)
		r.Post("/{id}/modify", h.ModifyPrepayment)
		r.Post("/{id}/recognize", h.RunPrepaymentRecognition)
		r.Post("/{id}/terminate", h.TerminatePrepayment)
		r.Get("/{id}/recognitions", h.ListPrepaymentRecognitions)
	})
	r.Route("/v1/allocation-rules", func(r chi.Router) {
		r.Post("/", h.CreateAllocationRule)
		r.Get("/", h.ListAllocationRules)
		r.Get("/{id}", h.GetAllocationRule)
		r.Post("/{id}/approve", h.ApproveAllocationRule)
	})
	r.Route("/v1/allocation-runs", func(r chi.Router) {
		r.Post("/", h.ExecuteAllocation)
		r.Get("/exceptions", h.ListAllocationExceptions)
		r.Get("/{id}", h.GetAllocationRun)
		r.Post("/{id}/reprocess", h.ReprocessAllocationRun)
	})
	r.Route("/v1/fx-revaluations", func(r chi.Router) {
		r.Post("/", h.StartRevaluation)
		r.Post("/reverse", h.ReversePriorRevaluation)
		r.Get("/", h.ListFXRevaluations)
		r.Get("/{id}", h.GetFXRevaluation)
		r.Post("/{id}/approve", h.ApproveRevaluation)
		r.Post("/{id}/post", h.PostRevaluation)
	})
	r.Route("/v1/migration-batches", func(r chi.Router) {
		r.Post("/", h.CreateMigrationAccountingBatch)
		r.Get("/exceptions", h.GetMigrationExceptions)
		r.Get("/{id}", h.GetMigrationBatchHandler)
		r.Post("/{id}/validate", h.ValidateOpeningBalances)
		r.Post("/{id}/approve", h.ApproveMigrationBatchHandler)
		r.Post("/{id}/commit", h.CommitOpeningPosting)
		r.Post("/{id}/certify", h.CertifyMigrationAccounting)
	})
	r.Route("/v1/financial-snapshots", func(r chi.Router) {
		r.Post("/", h.CreateFinancialSnapshot)
		r.Get("/supersession", h.ListSnapshotSupersession)
		r.Get("/{id}", h.GetFinancialSnapshotHandler)
		r.Post("/{id}/seal", h.SealSnapshot)
		r.Post("/{id}/certify", h.CertifySnapshot)
		r.Post("/{id}/supersede", h.SupersedeSnapshot)
	})
	r.Route("/v1/lineage", func(r chi.Router) {
		r.Get("/journals/{id}/source", h.TraceJournalToSource)
		r.Get("/verify", h.VerifyLineageCompleteness)
		r.Get("/status", h.GetLineageProjectionStatusHandler)
		r.Post("/rebuild", h.RebuildLineageProjection)
	})
}

// ── POST /v1/close/periods ────────────────────────────────────────────────────────

func (h *Handler) CreateFiscalPeriod(w http.ResponseWriter, r *http.Request) {
	var req domain.PeriodCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.LegalEntityID == "" || req.PeriodName == "" || req.PeriodStart.IsZero() || req.PeriodEnd.IsZero() {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, period_name, period_start, period_end are required")
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCloseConfig); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if !req.PeriodEnd.After(req.PeriodStart) {
		// A period that ends before it begins contains nothing, so every
		// readiness check trivially passes and it locks clean — an empty close
		// over a window that cannot hold a transaction.
		writeError(w, http.StatusBadRequest, "invalid_period_range", "period_end must be after period_start")
		return
	}

	fp := &domain.FiscalPeriod{
		FiscalPeriodID: uuid.NewString(),
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		PeriodName:     req.PeriodName,
		PeriodStart:    req.PeriodStart.UTC(),
		PeriodEnd:      req.PeriodEnd.UTC(),
		CloseStatus:    "OPEN",
	}

	created, err := h.store.CreateFiscalPeriod(r.Context(), fp)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}
	if !created {
		// Replay of a prior request for the same (legal_entity_id, period_name)
		// — return the original period rather than erroring.
		writeJSON(w, http.StatusOK, fp)
		return
	}

	writeJSON(w, http.StatusCreated, fp)
}

// ── GET /v1/close/periods ─────────────────────────────────────────────────────────

func (h *Handler) ListFiscalPeriods(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")

	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// Checked here and not left to the store. Every store method does reject an
	// empty tenant, but relying on that puts the boundary one layer further in
	// than the request it guards — and a handler that never states the
	// requirement will not have it when a future read reaches for a different
	// method. Fail closed at the edge.
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionCloseView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListFiscalPeriods(r.Context(), legalEntityID)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}

	// A nil slice marshals to JSON null, which every caller then has to
	// special-case. An entity with no periods registered is an empty list.
	if list == nil {
		list = []domain.FiscalPeriod{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/close/periods/status ──────────────────────────────────────────────────

func (h *Handler) GetPeriodStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")
	periodName := q.Get("period_name")

	if legalEntityID == "" || periodName == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and period_name are required")
		return
	}

	// Deliberately not authorized: this is the endpoint general-ledger-svc calls
	// before every journal create, post and reverse, service to service and with
	// no end-user principal to check. It IS tenant isolated, and the scope is
	// required rather than optional — see below for why that matters more here
	// than anywhere else in this service.
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	fp, err := h.store.GetFiscalPeriodByName(r.Context(), legalEntityID, periodName)
	if errors.Is(err, domain.ErrFiscalPeriodNotFound) {
		// A period nobody registered is open. That is the intended default —
		// this service does not own the calendar, and refusing to post to any
		// period until someone had registered it would make the ledger
		// unusable rather than safe.
		//
		// It is also why the tenant scope above is mandatory. This is the ONE
		// place in the estate that answers a security-relevant question by
		// failing OPEN, and general-ledger-svc fails CLOSED on everything else
		// specifically so that a locked period cannot be posted into. A missing
		// or wrong tenant scope makes any period look unregistered, so without
		// that check the whole period lock could be stepped around by omitting
		// a header — the caller would be told OPEN and the ledger would believe
		// it.
		writeJSON(w, http.StatusOK, map[string]string{
			"period_name":  periodName,
			"close_status": "OPEN",
		})
		return
	}
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"fiscal_period_id": fp.FiscalPeriodID,
		"period_name":      fp.PeriodName,
		"close_status":     fp.CloseStatus,
	})
}

// ── POST /v1/close/periods/{id}/lock ──────────────────────────────────────────────

func (h *Handler) LockPeriod(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = uuid.NewString()
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	fp, err := h.store.GetFiscalPeriod(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionCloseInitiate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if fp.CloseStatus != "OPEN" {
		writeError(w, http.StatusUnprocessableEntity, "period_already_locked", string(domain.ErrPeriodAlreadyLocked))
		return
	}

	h.publisher.PublishCloseStarted(r.Context(), correlationID, principalID, *fp)

	// Step 1: Run Readiness Checks (FAIL CLOSED on any dependency query error)
	blockingIssues, err := h.checkReadiness(r.Context(), tenantID, fp)
	if err != nil {
		h.writeReadinessErr(w, err)
		return
	}

	if len(blockingIssues) > 0 {
		h.publisher.PublishCloseBlocked(r.Context(), correlationID, principalID, *fp, blockingIssues)
		h.log.Warn("period close blocked by outstanding items", zap.String("period_id", id), zap.Strings("reasons", blockingIssues))
		writeJSON(w, http.StatusUnprocessableEntity, domain.ReadinessCheckResponse{
			IsReady:        false,
			BlockingIssues: blockingIssues,
		})
		return
	}

	// Step 2: Compile Trial Balance & Generate Evidence (FAIL CLOSED on error)
	balances, err := h.clients.CompileTrialBalance(r.Context(), tenantID, fp.LegalEntityID, fp.PeriodName, principalID)
	if err != nil {
		h.log.Error("failed to compile trial balance", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "close_failed", "cannot compile trial balance: "+err.Error())
		return
	}

	// Upload evidence to Document Vault (exposes file UUID)
	docID, err := h.clients.UploadCloseEvidence(r.Context(), tenantID, fp.LegalEntityID, fp.PeriodName, balances, principalID)
	if err != nil {
		h.log.Error("failed to upload close evidence document", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "close_failed", "failed to record close evidence in vault")
		return
	}

	// Calculate verification hash & cryptographic signature
	keys := make([]string, 0, len(balances))
	for k := range balances {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	for _, k := range keys {
		// Two decimal places, not %f's six. The balances are money summed from
		// NUMERIC(18,2) ledger lines, so anything past the second place is
		// float64 noise — and hashing that noise makes the "verification hash"
		// depend on artefacts of binary floating point rather than on the
		// balance an accountant would read.
		buf.WriteString(fmt.Sprintf("%s:%.2f;", k, balances[k]))
	}

	hashBytes := sha256.Sum256(buf.Bytes())
	trialBalanceHash := hex.EncodeToString(hashBytes[:])
	signature := h.signEvidence(hashBytes[:])

	now := time.Now().UTC()

	// Update DB record lock state
	if err := h.store.LockFiscalPeriod(r.Context(), id, now, docID); err != nil {
		if errors.Is(err, domain.ErrPeriodAlreadyLocked) {
			// Replay of a prior request that already succeeded (e.g. a client
			// timeout on a lock call that actually completed server-side) —
			// return the current locked state rather than misreporting this
			// as a store outage.
			current, getErr := h.store.GetFiscalPeriod(r.Context(), id)
			if getErr != nil {
				h.log.Error("failed to fetch already-locked period", zap.Error(getErr))
				writeError(w, http.StatusServiceUnavailable, "store_unavailable", getErr.Error())
				return
			}
			writeJSON(w, http.StatusOK, domain.PeriodLockResponse{
				FiscalPeriodID:     current.FiscalPeriodID,
				PeriodName:         current.PeriodName,
				CloseStatus:        current.CloseStatus,
				CloseLockedAt:      derefTime(current.CloseLockedAt),
				EvidenceDocumentID: derefString(current.EvidenceDocumentID),
			})
			return
		}
		h.log.Error("failed to lock fiscal period", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// Insert close evidence.
	//
	// A failure here used to be logged and swallowed, on the reasoning that the
	// period was already locked. But the evidence row IS the close: the trial
	// balance hash and its signature are the only record of what the books said
	// at the moment they were sealed, and the response below returns a
	// verification_hash that would, in that case, exist nowhere but in this
	// reply. Reporting success would be vouching for evidence that was never
	// written. The period stays locked — it is, and pretending otherwise would
	// be a second lie — and the caller is told plainly that the evidence is
	// missing, which is a condition someone has to act on.
	evidence := &domain.CloseEvidence{
		EvidenceID:       uuid.NewString(),
		TenantID:         tenantID,
		FiscalPeriodID:   id,
		TrialBalanceHash: trialBalanceHash,
		Signature:        signature,
		GeneratedAt:      now,
	}
	if err := h.store.CreateCloseEvidence(r.Context(), evidence); err != nil {
		h.log.Error("period locked but close evidence could not be recorded",
			zap.String("period_id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "evidence_not_recorded",
			string(domain.ErrEvidenceNotRecorded)+
				" — the period IS locked and the trial balance document was uploaded to the vault ("+docID+
				"), but the signed hash was not persisted. Do not treat this close as evidenced.")
		return
	}

	h.publisher.PublishClosed(r.Context(), correlationID, principalID, *fp, docID)

	writeJSON(w, http.StatusOK, domain.PeriodLockResponse{
		FiscalPeriodID:     id,
		PeriodName:         fp.PeriodName,
		CloseStatus:        "LOCKED",
		CloseLockedAt:      now,
		EvidenceDocumentID: docID,
		VerificationHash:   trialBalanceHash,
	})
}

// ── POST /v1/close/periods/{id}/reopen ────────────────────────────────────────────
//
// ACC-14 invariant #6: "Hard-closed periods reject ordinary posting; reopen
// is explicit, scoped, approved and evidenced." Before this handler existed,
// LOCKED was a terminal state with no code path back — this is that path,
// built to the same four requirements the invariant names literally:
//   - explicit: its own POST endpoint, never a side effect of another call
//   - scoped: only a LOCKED period reopens (ReopenFiscalPeriod's WHERE guard)
//   - approved: its own authz action (actionPeriodReopen), distinct from and
//     more sensitive than actionCloseInitiate
//   - evidenced: a mandatory, non-empty reason recorded in a permanent,
//     database-enforced append-only period_reopen_events row
func (h *Handler) ReopenPeriod(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = uuid.NewString()
	}

	var req domain.ReopenPeriodRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason_required", string(domain.ErrReopenReasonRequired))
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	fp, err := h.store.GetFiscalPeriod(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionPeriodReopen); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if fp.CloseStatus != "LOCKED" {
		writeError(w, http.StatusUnprocessableEntity, "period_not_locked", string(domain.ErrPeriodNotLocked))
		return
	}

	now := time.Now().UTC()
	if err := h.store.ReopenFiscalPeriod(r.Context(), id, now); err != nil {
		if errors.Is(err, domain.ErrPeriodNotLocked) {
			writeError(w, http.StatusUnprocessableEntity, "period_not_locked", string(domain.ErrPeriodNotLocked))
			return
		}
		h.log.Error("failed to reopen fiscal period", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// The state transition already happened — the period IS open again.
	// Reporting success without the evidence row would mean the one thing
	// the invariant requires ("evidenced") silently didn't happen, the same
	// mistake CreateCloseEvidence's own doc comment above already refuses
	// to make for the close path. Surfaced plainly, not swallowed.
	event := &domain.PeriodReopenEvent{
		ReopenEventID:         uuid.NewString(),
		FiscalPeriodID:        id,
		Reason:                req.Reason,
		ReopenedByPrincipalID: principalID,
		ReopenedAt:            now,
	}
	if err := h.store.CreateReopenEvent(r.Context(), event); err != nil {
		h.log.Error("period reopened but the reopen event could not be recorded",
			zap.String("period_id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "reopen_event_not_recorded",
			string(domain.ErrReopenEventNotRecorded)+" — the period IS open again, but no evidence of why was persisted.")
		return
	}

	h.publisher.PublishReopened(r.Context(), correlationID, principalID, *fp, req.Reason)

	fp.CloseStatus = "OPEN"
	fp.CloseLockedAt = nil
	fp.EvidenceDocumentID = nil
	writeJSON(w, http.StatusOK, fp)
}

// ── POST /v1/subledger-control/runs ───────────────────────────────────────────────
//
// ACC-06 (Subledger Control): a genuine balance comparison between a
// subledger's own total and its GL control account — not an existence-count
// of open items, which is all this service's readiness checks ever were
// (see master-register-findings-2026-08-27.md's audit of this gap). The
// control account is never hardcoded or guessed: it is resolved fresh, on
// every run, from the caller-declared ACC-02 mapping key, so a re-mapped
// account is picked up automatically and a mapping that was never set
// fails loudly instead of reconciling against nothing.
//
// matchToleranceAmount absorbs float64 summation noise across many
// invoice/journal rows, same reasoning as LockPeriod's %.2f hash
// formatting: a one-cent rounding artefact is not a real discrepancy, and
// treating it as one would make every run an EXCEPTION.
const matchToleranceAmount = 0.01

func (h *Handler) RunSubledgerControl(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = uuid.NewString()
	}

	var req domain.RunSubledgerControlRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.FiscalPeriod == "" || req.ControlAccountMappingKey == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, fiscal_period and control_account_mapping_key are required")
		return
	}
	if req.Subledger != "AP" && req.Subledger != "AR" {
		writeError(w, http.StatusBadRequest, "invalid_subledger", string(domain.ErrInvalidSubledger))
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionSubledgerControlRun); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	controlAccountCode, err := h.clients.GetControlAccountCode(r.Context(), tenantID, req.ControlAccountMappingKey)
	if err != nil {
		h.writeSubledgerControlErr(w, err, "general-ledger-svc")
		return
	}

	var subledgerTotal float64
	if req.Subledger == "AP" {
		subledgerTotal, err = h.clients.GetAPSubledgerTotal(r.Context(), tenantID, req.LegalEntityID)
		if err != nil {
			h.writeSubledgerControlErr(w, err, "accounts-payable-svc")
			return
		}
	} else {
		subledgerTotal, err = h.clients.GetARSubledgerTotal(r.Context(), tenantID, req.LegalEntityID)
		if err != nil {
			h.writeSubledgerControlErr(w, err, "accounts-receivable-svc")
			return
		}
	}

	balances, err := h.clients.CompileTrialBalance(r.Context(), tenantID, req.LegalEntityID, req.FiscalPeriod, principalID)
	if err != nil {
		h.writeSubledgerControlErr(w, err, "general-ledger-svc")
		return
	}
	glBalance, found := balances[controlAccountCode]
	if !found {
		writeError(w, http.StatusUnprocessableEntity, "control_account_balance_not_found", string(domain.ErrControlAccountBalanceNotFound))
		return
	}

	difference := subledgerTotal - glBalance
	status := "MATCHED"
	if difference > matchToleranceAmount || difference < -matchToleranceAmount {
		status = "EXCEPTION"
	}

	run := &domain.SubledgerControlRun{
		ControlRunID:           uuid.NewString(),
		TenantID:               tenantID,
		LegalEntityID:          req.LegalEntityID,
		FiscalPeriod:           req.FiscalPeriod,
		Subledger:              req.Subledger,
		ControlAccountCode:     controlAccountCode,
		SubledgerTotalAmount:   subledgerTotal,
		GLControlBalanceAmount: glBalance,
		DifferenceAmount:       difference,
		Status:                 status,
		RunAt:                  time.Now().UTC(),
		RunByPrincipalID:       principalID,
	}

	if err := h.store.CreateControlRun(r.Context(), run); err != nil {
		h.log.Error("failed to record subledger control run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if status == "EXCEPTION" {
		h.publisher.PublishSubledgerControlException(r.Context(), correlationID, principalID, *run)
	}

	writeJSON(w, http.StatusCreated, run)
}

// ── GET /v1/subledger-control/runs ────────────────────────────────────────────────

func (h *Handler) ListSubledgerControlRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")
	fiscalPeriod := q.Get("fiscal_period")
	if legalEntityID == "" || fiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and fiscal_period are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionSubledgerControlView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListControlRuns(r.Context(), legalEntityID, fiscalPeriod)
	if err != nil {
		h.log.Error("ListSubledgerControlRuns: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.SubledgerControlRun{}
	}
	writeJSON(w, http.StatusOK, list)
}

// writeSubledgerControlErr maps an upstream dependency failure encountered
// while running ACC-06 to the status it actually means — a mapping that was
// never set is the caller's setup being wrong (422), a truncated page or a
// dependency outage is 503, neither of which should be conflated with each
// other or logged as if they were the same fault.
func (h *Handler) writeSubledgerControlErr(w http.ResponseWriter, err error, dependency string) {
	if errors.Is(err, domain.ErrControlAccountMappingNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "control_account_mapping_not_found", string(domain.ErrControlAccountMappingNotFound))
		return
	}
	if errors.Is(err, domain.ErrSubledgerPageTruncated) {
		writeError(w, http.StatusServiceUnavailable, "subledger_page_truncated", string(domain.ErrSubledgerPageTruncated))
		return
	}
	if errors.Is(err, domain.ErrLedgerPageTruncated) {
		writeError(w, http.StatusServiceUnavailable, "ledger_page_truncated", string(domain.ErrLedgerPageTruncated))
		return
	}
	h.log.Error("subledger control run: dependency unavailable", zap.String("dependency", dependency), zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", dependency+": "+err.Error())
}

// ── ACC-07 (Accruals) ──────────────────────────────────────────────────────────────
//
// "owns AccrualSchedule, basis/evidence, recognition instances and reversal
// plan. Must never own: Direct ledger writes" — recognition always posts
// through general-ledger-svc's own journal lifecycle (see
// PostAccrualRecognitionJournal), never a table this service writes to
// itself.
//
// periodMonthLayout/periodIndex/periodAt implement the schedule's period
// arithmetic against this platform's monthly fiscal_period convention
// ("YYYY-MM", the same format FiscalPeriod.PeriodName already uses).
const periodMonthLayout = "2006-01"

// periodIndex returns how many months target is after start (0 for start
// itself). A negative or unparseable result is out of range.
func periodIndex(start, target string) (int, error) {
	s, err := time.Parse(periodMonthLayout, start)
	if err != nil {
		return 0, err
	}
	t, err := time.Parse(periodMonthLayout, target)
	if err != nil {
		return 0, err
	}
	months := (t.Year()-s.Year())*12 + int(t.Month()) - int(s.Month())
	return months, nil
}

func (h *Handler) CreateAccrual(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAccrualRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.Description == "" || req.PolicyVersion == "" ||
		req.StartFiscalPeriod == "" || req.DebitAccountCode == "" || req.CreditAccountCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"legal_entity_id, description, policy_version, start_fiscal_period, debit_account_code and credit_account_code are required")
		return
	}
	if req.TotalAmount <= 0 || req.PeriodCount < 1 {
		writeError(w, http.StatusBadRequest, "invalid_amount", string(domain.ErrInvalidAccrualAmount))
		return
	}
	if _, err := time.Parse(periodMonthLayout, req.StartFiscalPeriod); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_field", "start_fiscal_period must be YYYY-MM")
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionAccrualCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	sch := &domain.AccrualSchedule{
		ScheduleID:           uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		Description:          req.Description,
		PolicyVersion:        req.PolicyVersion,
		TotalAmount:          req.TotalAmount,
		StartFiscalPeriod:    req.StartFiscalPeriod,
		PeriodCount:          req.PeriodCount,
		DebitAccountCode:     req.DebitAccountCode,
		CreditAccountCode:    req.CreditAccountCode,
		Status:               domain.AccrualStatusDraft,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateAccrualSchedule(r.Context(), sch); err != nil {
		h.log.Error("failed to create accrual schedule", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sch)
}

func (h *Handler) GetAccrual(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sch, err := h.store.GetAccrualSchedule(r.Context(), id)
	if err != nil {
		h.writeAccrualStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionAccrualView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sch)
}

func (h *Handler) ListAccruals(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionAccrualView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListAccrualSchedules(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("ListAccruals: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.AccrualSchedule{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) SubmitAccrual(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sch, err := h.store.GetAccrualSchedule(r.Context(), id)
	if err != nil {
		h.writeAccrualStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionAccrualCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.SubmitAccrualSchedule(r.Context(), id, principalID, now); err != nil {
		h.writeAccrualStoreErr(w, err)
		return
	}
	sch.Status = domain.AccrualStatusPendingApproval
	sch.SubmittedAt, sch.SubmittedByPrincipalID = &now, &principalID
	writeJSON(w, http.StatusOK, sch)
}

// ApproveAccrual is deliberately its own action (actionAccrualApprove) and
// its own endpoint — segregation of duties, the whole reason an approval
// step exists, requires it be independently grantable from
// actionAccrualCreate rather than the preparer being able to approve their
// own schedule.
func (h *Handler) ApproveAccrual(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sch, err := h.store.GetAccrualSchedule(r.Context(), id)
	if err != nil {
		h.writeAccrualStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionAccrualApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ApproveAccrualSchedule(r.Context(), id, principalID, now); err != nil {
		h.writeAccrualStoreErr(w, err)
		return
	}
	sch.Status = domain.AccrualStatusApproved
	sch.ApprovedAt, sch.ApprovedByPrincipalID = &now, &principalID
	writeJSON(w, http.StatusOK, sch)
}

// AmendFutureSchedule changes an APPROVED/ACTIVE schedule's total_amount
// and period_count going FORWARD only — periods already recognized are
// permanent evidence (migration 000005) and are never invalidated by a
// later amendment, the spec's own negative-path requirement ("Accrual
// changed after some periods recognized" must not produce an unauthorized
// or duplicate accounting consequence).
func (h *Handler) AmendFutureSchedule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.AmendFutureScheduleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TotalAmount <= 0 || req.PeriodCount < 1 {
		writeError(w, http.StatusBadRequest, "invalid_amount", string(domain.ErrInvalidAccrualAmount))
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sch, err := h.store.GetAccrualSchedule(r.Context(), id)
	if err != nil {
		h.writeAccrualStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionAccrualAmend); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	recognized, err := h.store.ListRecognitionInstances(r.Context(), id)
	if err != nil {
		h.log.Error("AmendFutureSchedule: failed to count recognized periods", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if req.PeriodCount < len(recognized) {
		writeError(w, http.StatusUnprocessableEntity, "would_drop_recognized_periods", string(domain.ErrAmendWouldDropRecognizedPeriods))
		return
	}

	if err := h.store.AmendAccrualSchedule(r.Context(), id, req.TotalAmount, req.PeriodCount); err != nil {
		h.writeAccrualStoreErr(w, err)
		return
	}
	sch.TotalAmount, sch.PeriodCount = req.TotalAmount, req.PeriodCount
	writeJSON(w, http.StatusOK, sch)
}

func (h *Handler) CancelFutureAccrual(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sch, err := h.store.GetAccrualSchedule(r.Context(), id)
	if err != nil {
		h.writeAccrualStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionAccrualCancel); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if sch.Status == domain.AccrualStatusCompleted || sch.Status == domain.AccrualStatusCancelled {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidAccrualTransition))
		return
	}
	now := time.Now().UTC()
	if err := h.store.CancelAccrualSchedule(r.Context(), id, sch.Status, principalID, now); err != nil {
		h.writeAccrualStoreErr(w, err)
		return
	}
	sch.Status = domain.AccrualStatusCancelled
	sch.CancelledAt, sch.CancelledByPrincipalID = &now, &principalID
	writeJSON(w, http.StatusOK, sch)
}

// RunAccrualRecognition posts one period's recognition journal through
// general-ledger-svc and records the permanent evidence row. Idempotent on
// replay (PostAccrualRecognitionJournal's correlation_id and
// CreateRecognitionInstance's UNIQUE constraint both key on
// schedule_id+fiscal_period), and refuses a period this service's own
// fiscal_periods register shows as LOCKED — the spec's own negative-path
// requirement that a hard-closed period reject an accrual posting.
func (h *Handler) RunAccrualRecognition(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.RunAccrualRecognitionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.FiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "fiscal_period is required")
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

	sch, err := h.store.GetAccrualSchedule(r.Context(), id)
	if err != nil {
		h.writeAccrualStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionAccrualRecognize); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if sch.Status != domain.AccrualStatusApproved && sch.Status != domain.AccrualStatusActive {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidAccrualTransition))
		return
	}

	index, err := periodIndex(sch.StartFiscalPeriod, req.FiscalPeriod)
	if err != nil || index < 0 || index >= sch.PeriodCount {
		writeError(w, http.StatusUnprocessableEntity, "period_out_of_range", string(domain.ErrRecognitionPeriodOutOfRange))
		return
	}

	// A period nobody registered with financial-close-svc is treated as
	// OPEN — same doctrine as GetPeriodStatus, since this service does not
	// own the calendar. Only a period explicitly recorded LOCKED blocks
	// the posting.
	fp, err := h.store.GetFiscalPeriodByName(r.Context(), sch.LegalEntityID, req.FiscalPeriod)
	if err != nil && !errors.Is(err, domain.ErrFiscalPeriodNotFound) {
		h.log.Error("RunAccrualRecognition: failed to check period status", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if fp != nil && fp.CloseStatus == "LOCKED" {
		writeError(w, http.StatusUnprocessableEntity, "period_locked", string(domain.ErrRecognitionPeriodLocked))
		return
	}

	amount := recognitionAmountFor(sch, index)
	correlationID := sch.ScheduleID + ":" + req.FiscalPeriod
	description := sch.Description + " — recognition " + req.FiscalPeriod

	journalID, err := h.clients.PostAccrualRecognitionJournal(r.Context(), tenantID, sch.LegalEntityID, req.FiscalPeriod,
		correlationID, principalID, description, sch.DebitAccountCode, sch.CreditAccountCode, amount)
	if err != nil {
		h.log.Error("RunAccrualRecognition: journal posting failed", zap.String("schedule_id", id), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
		return
	}

	inst := &domain.RecognitionInstance{
		RecognitionInstanceID:   uuid.NewString(),
		ScheduleID:              id,
		FiscalPeriod:            req.FiscalPeriod,
		RecognizedAmount:        amount,
		JournalID:               journalID,
		RecognizedAt:            time.Now().UTC(),
		RecognizedByPrincipalID: principalID,
	}
	created, err := h.store.CreateRecognitionInstance(r.Context(), inst)
	if err != nil {
		h.log.Error("accrual recognized on the ledger but the evidence row could not be recorded",
			zap.String("schedule_id", id), zap.String("journal_id", journalID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "recognition_not_recorded",
			"the recognition journal IS posted to general-ledger-svc ("+journalID+"), but its evidence row was not persisted.")
		return
	}

	h.recordLineageEdge(r.Context(), sch.LegalEntityID, "accrual_recognition", inst.RecognitionInstanceID, "journal", journalID)

	if err := h.store.ActivateAccrualSchedule(r.Context(), id); err != nil {
		h.log.Error("failed to activate accrual schedule after first recognition", zap.String("schedule_id", id), zap.Error(err))
	}
	if all, err := h.store.ListRecognitionInstances(r.Context(), id); err == nil && len(all) >= sch.PeriodCount {
		if err := h.store.CompleteAccrualSchedule(r.Context(), id); err != nil {
			h.log.Error("failed to mark accrual schedule COMPLETED", zap.String("schedule_id", id), zap.Error(err))
		}
	}

	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, inst)
}

// recognitionAmountFor computes the installment amount for periodIndex,
// with the LAST period absorbing whatever rounding remainder the equal
// split leaves — same reasoning as LockPeriod's %.2f hashing: money is
// NUMERIC(18,2), and an installment scheme that doesn't sum exactly to
// total_amount is wrong, not merely imprecise.
func recognitionAmountFor(sch *domain.AccrualSchedule, periodIndex int) float64 {
	base := roundCents(sch.TotalAmount / float64(sch.PeriodCount))
	if periodIndex == sch.PeriodCount-1 {
		return roundCents(sch.TotalAmount - base*float64(sch.PeriodCount-1))
	}
	return base
}

func roundCents(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func (h *Handler) ListRecognitions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sch, err := h.store.GetAccrualSchedule(r.Context(), id)
	if err != nil {
		h.writeAccrualStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionAccrualView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListRecognitionInstances(r.Context(), id)
	if err != nil {
		h.log.Error("ListRecognitions: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.RecognitionInstance{}
	}
	writeJSON(w, http.StatusOK, list)
}

// writeAccrualStoreErr maps an accrual store failure to the status it
// actually means — not-found is 404, an invalid lifecycle transition is
// 422 (the caller's request is wrong for the schedule's current state, not
// a store outage), everything else is 503.
func (h *Handler) writeAccrualStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAccrualNotFound):
		writeError(w, http.StatusNotFound, "accrual_not_found", "")
	case errors.Is(err, domain.ErrInvalidAccrualTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidAccrualTransition))
	case errors.Is(err, domain.ErrIdentityMissing), errors.Is(err, domain.ErrTenantScopeMissing):
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", string(domain.ErrTenantScopeMissing))
	default:
		h.log.Error("accrual store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

// ── ACC-08 (Prepayments & Deferrals) ─────────────────────────────────────────────
//
// "owns RecognitionSchedule, remaining balance, period recognition
// instances and schedule versions. Must never own: Direct ledger writes."
// Economically the mirror of ACC-07: recognizes an already-paid prepaid
// asset into expense over time. Recognition reuses the same
// PostAccrualRecognitionJournal client call as ACC-07 — it is a generic
// "post one balanced two-line journal" primitive, not something specific
// to accruals, so a second, materially identical client method would only
// have duplicated it.

func (h *Handler) CreatePrepayment(w http.ResponseWriter, r *http.Request) {
	var req domain.CreatePrepaymentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.Description == "" || req.StartFiscalPeriod == "" ||
		req.DebitAccountCode == "" || req.CreditAccountCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"legal_entity_id, description, start_fiscal_period, debit_account_code and credit_account_code are required")
		return
	}
	if req.TotalAmount <= 0 || req.PeriodCount < 1 {
		writeError(w, http.StatusBadRequest, "invalid_amount", string(domain.ErrInvalidPrepaymentAmount))
		return
	}
	if _, err := time.Parse(periodMonthLayout, req.StartFiscalPeriod); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_field", "start_fiscal_period must be YYYY-MM")
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionPrepaymentCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	sch := &domain.PrepaymentSchedule{
		ScheduleID:           uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		Description:          req.Description,
		TotalAmount:          req.TotalAmount,
		StartFiscalPeriod:    req.StartFiscalPeriod,
		PeriodCount:          req.PeriodCount,
		DebitAccountCode:     req.DebitAccountCode,
		CreditAccountCode:    req.CreditAccountCode,
		Status:               domain.PrepaymentStatusDraft,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreatePrepaymentSchedule(r.Context(), sch); err != nil {
		h.log.Error("failed to create prepayment schedule", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sch)
}

func (h *Handler) GetPrepayment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sch, err := h.store.GetPrepaymentSchedule(r.Context(), id)
	if err != nil {
		h.writePrepaymentStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionPrepaymentView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sch)
}

func (h *Handler) ListPrepayments(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionPrepaymentView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListPrepaymentSchedules(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("ListPrepayments: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.PrepaymentSchedule{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) ApprovePrepayment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sch, err := h.store.GetPrepaymentSchedule(r.Context(), id)
	if err != nil {
		h.writePrepaymentStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionPrepaymentApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ApprovePrepaymentSchedule(r.Context(), id, principalID, now); err != nil {
		h.writePrepaymentStoreErr(w, err)
		return
	}
	sch.Status = domain.PrepaymentStatusApproved
	sch.ApprovedAt, sch.ApprovedByPrincipalID = &now, &principalID
	writeJSON(w, http.StatusOK, sch)
}

// ModifyPrepayment implements ACC-08's ModifyFutureSchedule command — the
// spec's own negative path, "Backdate schedule change over recognized
// periods," is blocked the same way as ACC-07's AmendFutureSchedule: a
// period_count that would drop below the number already recognized is
// refused, since recognized history is permanent evidence.
func (h *Handler) ModifyPrepayment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.ModifyFutureScheduleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TotalAmount <= 0 || req.PeriodCount < 1 {
		writeError(w, http.StatusBadRequest, "invalid_amount", string(domain.ErrInvalidPrepaymentAmount))
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sch, err := h.store.GetPrepaymentSchedule(r.Context(), id)
	if err != nil {
		h.writePrepaymentStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionPrepaymentModify); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	recognized, err := h.store.ListPrepaymentRecognitions(r.Context(), id)
	if err != nil {
		h.log.Error("ModifyPrepayment: failed to count recognized periods", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if req.PeriodCount < len(recognized) {
		writeError(w, http.StatusUnprocessableEntity, "would_drop_recognized_periods", string(domain.ErrModifyWouldDropRecognizedPeriods))
		return
	}

	if err := h.store.ModifyFuturePrepaymentSchedule(r.Context(), id, req.TotalAmount, req.PeriodCount); err != nil {
		h.writePrepaymentStoreErr(w, err)
		return
	}
	sch.TotalAmount, sch.PeriodCount = req.TotalAmount, req.PeriodCount
	writeJSON(w, http.StatusOK, sch)
}

// RunPrepaymentRecognition mirrors ACC-07's RunAccrualRecognition. See
// that handler's doc comment for the shared idempotency and
// hard-closed-period reasoning — identical here.
func (h *Handler) RunPrepaymentRecognition(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.RunPrepaymentRecognitionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.FiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "fiscal_period is required")
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

	sch, err := h.store.GetPrepaymentSchedule(r.Context(), id)
	if err != nil {
		h.writePrepaymentStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionPrepaymentRecognize); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if sch.Status != domain.PrepaymentStatusApproved && sch.Status != domain.PrepaymentStatusActive {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidPrepaymentTransition))
		return
	}

	index, err := periodIndex(sch.StartFiscalPeriod, req.FiscalPeriod)
	if err != nil || index < 0 || index >= sch.PeriodCount {
		writeError(w, http.StatusUnprocessableEntity, "period_out_of_range", string(domain.ErrPrepaymentPeriodOutOfRange))
		return
	}

	fp, err := h.store.GetFiscalPeriodByName(r.Context(), sch.LegalEntityID, req.FiscalPeriod)
	if err != nil && !errors.Is(err, domain.ErrFiscalPeriodNotFound) {
		h.log.Error("RunPrepaymentRecognition: failed to check period status", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if fp != nil && fp.CloseStatus == "LOCKED" {
		writeError(w, http.StatusUnprocessableEntity, "period_locked", string(domain.ErrPrepaymentPeriodLocked))
		return
	}

	amount := prepaymentRecognitionAmountFor(sch, index)
	correlationID := sch.ScheduleID + ":" + req.FiscalPeriod
	description := sch.Description + " — recognition " + req.FiscalPeriod

	journalID, err := h.clients.PostAccrualRecognitionJournal(r.Context(), tenantID, sch.LegalEntityID, req.FiscalPeriod,
		correlationID, principalID, description, sch.DebitAccountCode, sch.CreditAccountCode, amount)
	if err != nil {
		h.log.Error("RunPrepaymentRecognition: journal posting failed", zap.String("schedule_id", id), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
		return
	}

	inst := &domain.PrepaymentRecognitionInstance{
		RecognitionInstanceID:   uuid.NewString(),
		ScheduleID:              id,
		FiscalPeriod:            req.FiscalPeriod,
		RecognizedAmount:        amount,
		JournalID:               journalID,
		RecognizedAt:            time.Now().UTC(),
		RecognizedByPrincipalID: principalID,
	}
	created, err := h.store.CreatePrepaymentRecognition(r.Context(), inst)
	if err != nil {
		h.log.Error("prepayment recognized on the ledger but the evidence row could not be recorded",
			zap.String("schedule_id", id), zap.String("journal_id", journalID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "recognition_not_recorded",
			"the recognition journal IS posted to general-ledger-svc ("+journalID+"), but its evidence row was not persisted.")
		return
	}

	h.recordLineageEdge(r.Context(), sch.LegalEntityID, "prepayment_recognition", inst.RecognitionInstanceID, "journal", journalID)

	if err := h.store.ActivatePrepaymentSchedule(r.Context(), id); err != nil {
		h.log.Error("failed to activate prepayment schedule after first recognition", zap.String("schedule_id", id), zap.Error(err))
	}
	if all, err := h.store.ListPrepaymentRecognitions(r.Context(), id); err == nil && len(all) >= sch.PeriodCount {
		if err := h.store.CompletePrepaymentSchedule(r.Context(), id); err != nil {
			h.log.Error("failed to mark prepayment schedule COMPLETED", zap.String("schedule_id", id), zap.Error(err))
		}
	}

	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, inst)
}

// prepaymentRecognitionAmountFor mirrors ACC-07's recognitionAmountFor —
// same rounding-residual policy (last period absorbs it), which is this
// service's explicit answer to the spec's own negative path, "Rounding
// creates unallocated residual."
func prepaymentRecognitionAmountFor(sch *domain.PrepaymentSchedule, periodIdx int) float64 {
	base := roundCents(sch.TotalAmount / float64(sch.PeriodCount))
	if periodIdx == sch.PeriodCount-1 {
		return roundCents(sch.TotalAmount - base*float64(sch.PeriodCount-1))
	}
	return base
}

func (h *Handler) ListPrepaymentRecognitions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sch, err := h.store.GetPrepaymentSchedule(r.Context(), id)
	if err != nil {
		h.writePrepaymentStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionPrepaymentView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListPrepaymentRecognitions(r.Context(), id)
	if err != nil {
		h.log.Error("ListPrepaymentRecognitions: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.PrepaymentRecognitionInstance{}
	}
	writeJSON(w, http.StatusOK, list)
}

// GetPrepaymentRemainingBalance answers ACC-08's own GetRemainingBalance
// query — total_amount less whatever has actually been recognized so far,
// computed from the permanent recognition-instance evidence rather than
// tracked as a separately-mutable counter that could drift from it.
func (h *Handler) GetPrepaymentRemainingBalance(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	sch, err := h.store.GetPrepaymentSchedule(r.Context(), id)
	if err != nil {
		h.writePrepaymentStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionPrepaymentView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	recognized, err := h.store.ListPrepaymentRecognitions(r.Context(), id)
	if err != nil {
		h.log.Error("GetPrepaymentRemainingBalance: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	var recognizedTotal float64
	for _, inst := range recognized {
		recognizedTotal += inst.RecognizedAmount
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schedule_id":       sch.ScheduleID,
		"total_amount":      sch.TotalAmount,
		"recognized_total":  recognizedTotal,
		"remaining_balance": roundCents(sch.TotalAmount - recognizedTotal),
	})
}

// TerminatePrepayment implements ACC-08's TerminateSchedule command. The
// spec's own negative path — "Terminate without final balance treatment"
// — is enforced as real validation: final_balance_treatment is required
// and must be one of the two named values, never defaulted.
//
// RECOGNIZE_REMAINING posts one final settlement journal for whatever
// balance is left, recorded under the fixed TerminationPseudoPeriod key so
// it can never collide with — or be confused with — an ordinary periodic
// recognition, and so a replayed terminate call is idempotent for free via
// the same UNIQUE(schedule_id, fiscal_period) constraint every other
// recognition instance relies on. WRITE_OFF posts nothing: the remaining
// balance is recorded as permanently unrecognized, by explicit choice, not
// silence.
func (h *Handler) TerminatePrepayment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.TerminatePrepaymentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason_required", "reason is required to terminate a prepayment schedule")
		return
	}
	if req.FinalBalanceTreatment != domain.TerminationTreatmentWriteOff && req.FinalBalanceTreatment != domain.TerminationTreatmentRecognizeRemaining {
		writeError(w, http.StatusBadRequest, "final_balance_treatment_required", string(domain.ErrFinalBalanceTreatmentRequired))
		return
	}
	if req.FinalBalanceTreatment == domain.TerminationTreatmentRecognizeRemaining && req.FiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "fiscal_period_required", string(domain.ErrTerminationFiscalPeriodRequired))
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
	sch, err := h.store.GetPrepaymentSchedule(r.Context(), id)
	if err != nil {
		h.writePrepaymentStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionPrepaymentTerminate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if sch.Status == domain.PrepaymentStatusCompleted || sch.Status == domain.PrepaymentStatusTerminated {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidPrepaymentTransition))
		return
	}

	if req.FinalBalanceTreatment == domain.TerminationTreatmentRecognizeRemaining {
		recognized, err := h.store.ListPrepaymentRecognitions(r.Context(), id)
		if err != nil {
			h.log.Error("TerminatePrepayment: failed to compute remaining balance", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
			return
		}
		var recognizedTotal float64
		for _, inst := range recognized {
			if inst.FiscalPeriod == domain.TerminationPseudoPeriod {
				continue
			}
			recognizedTotal += inst.RecognizedAmount
		}
		remaining := roundCents(sch.TotalAmount - recognizedTotal)
		if remaining > 0 {
			fp, err := h.store.GetFiscalPeriodByName(r.Context(), sch.LegalEntityID, req.FiscalPeriod)
			if err != nil && !errors.Is(err, domain.ErrFiscalPeriodNotFound) {
				h.log.Error("TerminatePrepayment: failed to check period status", zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
				return
			}
			if fp != nil && fp.CloseStatus == "LOCKED" {
				writeError(w, http.StatusUnprocessableEntity, "period_locked", string(domain.ErrPrepaymentPeriodLocked))
				return
			}

			journalID, err := h.clients.PostAccrualRecognitionJournal(r.Context(), tenantID, sch.LegalEntityID, req.FiscalPeriod,
				sch.ScheduleID+":"+domain.TerminationPseudoPeriod, principalID,
				sch.Description+" — termination final settlement", sch.DebitAccountCode, sch.CreditAccountCode, remaining)
			if err != nil {
				h.log.Error("TerminatePrepayment: final settlement journal posting failed", zap.String("schedule_id", id), zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
				return
			}
			finalInst := &domain.PrepaymentRecognitionInstance{
				RecognitionInstanceID:   uuid.NewString(),
				ScheduleID:              id,
				FiscalPeriod:            domain.TerminationPseudoPeriod,
				RecognizedAmount:        remaining,
				JournalID:               journalID,
				RecognizedAt:            time.Now().UTC(),
				RecognizedByPrincipalID: principalID,
			}
			if _, err := h.store.CreatePrepaymentRecognition(r.Context(), finalInst); err != nil {
				h.log.Error("prepayment final settlement posted but the evidence row could not be recorded",
					zap.String("schedule_id", id), zap.String("journal_id", journalID), zap.Error(err))
				writeError(w, http.StatusInternalServerError, "recognition_not_recorded",
					"the final settlement journal IS posted to general-ledger-svc ("+journalID+"), but its evidence row was not persisted.")
				return
			}
			h.recordLineageEdge(r.Context(), sch.LegalEntityID, "prepayment_recognition", finalInst.RecognitionInstanceID, "journal", journalID)
		}
	}

	now := time.Now().UTC()
	if err := h.store.TerminatePrepaymentSchedule(r.Context(), id, sch.Status, principalID, req.Reason, req.FinalBalanceTreatment, now); err != nil {
		h.writePrepaymentStoreErr(w, err)
		return
	}
	sch.Status = domain.PrepaymentStatusTerminated
	sch.TerminatedAt, sch.TerminatedByPrincipalID = &now, &principalID
	sch.TerminationReason, sch.TerminationFinalTreatment = &req.Reason, &req.FinalBalanceTreatment
	writeJSON(w, http.StatusOK, sch)
}

func (h *Handler) writePrepaymentStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrPrepaymentNotFound):
		writeError(w, http.StatusNotFound, "prepayment_not_found", "")
	case errors.Is(err, domain.ErrInvalidPrepaymentTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidPrepaymentTransition))
	case errors.Is(err, domain.ErrIdentityMissing), errors.Is(err, domain.ErrTenantScopeMissing):
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", string(domain.ErrTenantScopeMissing))
	default:
		h.log.Error("prepayment store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

// ── ACC-09 (Allocation Engine) ────────────────────────────────────────────────────
//
// "owns Allocation rules/runs. Must never own: Source population or
// ledger truth." Enforced by never accepting a caller-declared source
// amount: ExecuteAllocation always READS the source account's balance
// from general-ledger-svc's own trial balance (ACC-15) at run time.

const allocationWeightTolerance = 0.01

func (h *Handler) CreateAllocationRule(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAllocationRuleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.Name == "" || req.SourceAccountCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, name and source_account_code are required")
		return
	}
	if len(req.Drivers) == 0 {
		writeError(w, http.StatusBadRequest, "no_drivers", string(domain.ErrNoDriversDefined))
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionAllocationRuleCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	ruleID := uuid.NewString()
	rule := &domain.AllocationRule{
		RuleVersionID:        ruleID,
		RuleID:               ruleID,
		Version:              1,
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		Name:                 req.Name,
		SourceAccountCode:    req.SourceAccountCode,
		Drivers:              req.Drivers,
		Status:               domain.AllocationRuleStatusDraft,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateAllocationRule(r.Context(), rule); err != nil {
		h.log.Error("failed to create allocation rule", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (h *Handler) GetAllocationRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	rule, err := h.store.GetCurrentAllocationRule(r.Context(), id)
	if err != nil {
		h.writeAllocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, rule.LegalEntityID, actionAllocationView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (h *Handler) ListAllocationRules(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionAllocationView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListAllocationRules(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("ListAllocationRules: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.AllocationRule{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ApproveAllocationRule enforces two of ACC-09's own negative paths as
// real, blocking validation: "Drivers do not sum/cover source" (weights
// must sum to exactly 100, within float rounding tolerance) and
// "Recipient dimension invalid" (every driver's recipient account must
// resolve to a real, ACTIVE chart-registered account via GL). Both are
// checked HERE, at approval, rather than at every execution — a rule that
// passed approval can never later fail on these grounds mid-run.
func (h *Handler) ApproveAllocationRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	rule, err := h.store.GetCurrentAllocationRule(r.Context(), id)
	if err != nil {
		h.writeAllocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, rule.LegalEntityID, actionAllocationRuleApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	var totalWeight float64
	for _, d := range rule.Drivers {
		totalWeight += d.WeightPercentage
	}
	if totalWeight > 100+allocationWeightTolerance || totalWeight < 100-allocationWeightTolerance {
		writeError(w, http.StatusUnprocessableEntity, "drivers_do_not_sum_to_100", string(domain.ErrDriversDoNotSumTo100))
		return
	}

	for _, d := range rule.Drivers {
		status, err := h.clients.GetAccountStatus(r.Context(), tenantID, principalID, d.RecipientAccountCode)
		if err != nil {
			if errors.Is(err, domain.ErrRecipientAccountInvalid) {
				writeError(w, http.StatusUnprocessableEntity, "recipient_account_invalid",
					string(domain.ErrRecipientAccountInvalid)+": "+d.RecipientAccountCode)
				return
			}
			h.log.Error("ApproveAllocationRule: failed to verify recipient account", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", err.Error())
			return
		}
		if status != "ACTIVE" {
			writeError(w, http.StatusUnprocessableEntity, "recipient_account_invalid",
				string(domain.ErrRecipientAccountInvalid)+": "+d.RecipientAccountCode+" is not ACTIVE")
			return
		}
	}

	now := time.Now().UTC()
	if err := h.store.ApproveAllocationRule(r.Context(), rule.RuleVersionID, principalID, now); err != nil {
		h.writeAllocationErr(w, err)
		return
	}
	rule.Status = domain.AllocationRuleStatusApproved
	rule.ApprovedAt, rule.ApprovedByPrincipalID = &now, &principalID
	writeJSON(w, http.StatusOK, rule)
}

// allocationAmountsFor computes each driver's share of sourceAmount,
// exactly as ACC-07/08's recognitionAmountFor: the LAST driver absorbs
// whatever rounding remainder the split leaves, so the shares always sum
// to exactly sourceAmount — this platform's standing answer to "Rounding
// creates unallocated residual."
func allocationAmountsFor(drivers []domain.AllocationDriver, sourceAmount float64) []domain.AllocationJournalLine {
	lines := make([]domain.AllocationJournalLine, len(drivers))
	var runningTotal float64
	for i, d := range drivers {
		if i == len(drivers)-1 {
			lines[i] = domain.AllocationJournalLine{AccountCode: d.RecipientAccountCode, Amount: roundCents(sourceAmount - runningTotal)}
			continue
		}
		amount := roundCents(sourceAmount * d.WeightPercentage / 100)
		lines[i] = domain.AllocationJournalLine{AccountCode: d.RecipientAccountCode, Amount: amount}
		runningTotal += amount
	}
	return lines
}

// ExecuteAllocation posts one allocation run. Idempotent on
// (rule_id, fiscal_period): a rule can produce at most one run per
// period, ever — the spec's own negative path, "Rerun duplicates
// posting," enforced by the migration's own UNIQUE constraint and checked
// here before any calculation happens.
func (h *Handler) ExecuteAllocation(w http.ResponseWriter, r *http.Request) {
	var req domain.ExecuteAllocationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RuleID == "" || req.FiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "rule_id and fiscal_period are required")
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

	rule, err := h.store.GetCurrentAllocationRule(r.Context(), req.RuleID)
	if err != nil {
		h.writeAllocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, rule.LegalEntityID, actionAllocationExecute); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if rule.Status != domain.AllocationRuleStatusApproved && rule.Status != domain.AllocationRuleStatusActive {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidAllocationRuleTransition))
		return
	}

	existing, err := h.store.GetAllocationRunByRuleAndPeriod(r.Context(), req.RuleID, req.FiscalPeriod)
	if err == nil {
		if existing.Status == domain.AllocationRunStatusFailed {
			writeError(w, http.StatusUnprocessableEntity, "run_failed_use_reprocess",
				"a FAILED run already exists for this rule and period — use POST /v1/allocation-runs/"+existing.RunID+"/reprocess")
			return
		}
		full, err := h.store.GetAllocationRun(r.Context(), existing.RunID)
		if err != nil {
			h.log.Error("ExecuteAllocation: failed to load existing run", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, full)
		return
	}
	if !errors.Is(err, domain.ErrAllocationRunNotFound) {
		h.log.Error("ExecuteAllocation: failed to check for an existing run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	balances, err := h.clients.CompileTrialBalance(r.Context(), tenantID, rule.LegalEntityID, req.FiscalPeriod, principalID)
	if err != nil {
		h.log.Error("ExecuteAllocation: failed to compile trial balance", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", err.Error())
		return
	}
	sourceAmount, found := balances[rule.SourceAccountCode]
	if !found {
		writeError(w, http.StatusUnprocessableEntity, "source_balance_not_found", string(domain.ErrSourceBalanceNotFound))
		return
	}

	now := time.Now().UTC()
	run := &domain.AllocationRun{
		RunID:                uuid.NewString(),
		LegalEntityID:        rule.LegalEntityID,
		RuleID:               rule.RuleID,
		RuleVersionID:        rule.RuleVersionID,
		FiscalPeriod:         req.FiscalPeriod,
		SourceAccountCode:    rule.SourceAccountCode,
		SourceAmount:         sourceAmount,
		Status:               domain.AllocationRunStatusPlanned,
		CreatedAt:            now,
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateAllocationRun(r.Context(), run); err != nil {
		h.log.Error("failed to create allocation run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	debitLines := allocationAmountsFor(rule.Drivers, sourceAmount)
	resultLines := make([]domain.AllocationResultLine, len(debitLines))
	for i, l := range debitLines {
		resultLines[i] = domain.AllocationResultLine{ResultLineID: uuid.NewString(), RunID: run.RunID, RecipientAccountCode: l.AccountCode, AllocatedAmount: l.Amount}
	}
	if err := h.store.CreateAllocationResultLines(r.Context(), run.RunID, resultLines); err != nil {
		h.log.Error("allocation run planned but result lines could not be recorded", zap.String("run_id", run.RunID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "result_lines_not_recorded", err.Error())
		return
	}
	if err := h.store.MarkAllocationRunCalculated(r.Context(), run.RunID, sourceAmount, now); err != nil {
		h.log.Error("failed to mark allocation run CALCULATED", zap.String("run_id", run.RunID), zap.Error(err))
	}

	description := rule.Name + " — allocation " + req.FiscalPeriod
	journalID, err := h.clients.PostAllocationJournal(r.Context(), tenantID, rule.LegalEntityID, req.FiscalPeriod,
		run.RunID, principalID, description, rule.SourceAccountCode, sourceAmount, debitLines)
	if err != nil {
		h.log.Error("ExecuteAllocation: journal posting failed", zap.String("run_id", run.RunID), zap.Error(err))
		if markErr := h.store.MarkAllocationRunFailed(r.Context(), run.RunID, err.Error()); markErr != nil {
			h.log.Error("failed to mark allocation run FAILED", zap.String("run_id", run.RunID), zap.Error(markErr))
		}
		writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
		return
	}
	if err := h.store.MarkAllocationRunPosted(r.Context(), run.RunID, journalID, time.Now().UTC()); err != nil {
		h.log.Error("allocation journal posted but the run could not be marked POSTED",
			zap.String("run_id", run.RunID), zap.String("journal_id", journalID), zap.Error(err))
	}
	h.recordLineageEdge(r.Context(), rule.LegalEntityID, "allocation_run", run.RunID, "journal", journalID)
	if err := h.store.ActivateAllocationRule(r.Context(), rule.RuleVersionID); err != nil {
		h.log.Error("failed to activate allocation rule after first execution", zap.String("rule_version_id", rule.RuleVersionID), zap.Error(err))
	}

	full, err := h.store.GetAllocationRun(r.Context(), run.RunID)
	if err != nil {
		h.log.Error("ExecuteAllocation: failed to reload posted run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, full)
}

// ReprocessAllocationRun retries posting for a FAILED run ONLY — the
// spec's own negative path, "Rerun duplicates posting," means a run that
// already reached POSTED must never be reprocessed. It replays the
// EXACT result lines already calculated and recorded as permanent
// evidence rather than recomputing them, so a reprocess can never post a
// different amount than what was originally calculated.
func (h *Handler) ReprocessAllocationRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	run, err := h.store.GetAllocationRun(r.Context(), id)
	if err != nil {
		h.writeAllocationErr(w, err)
		return
	}
	rule, err := h.store.GetAllocationRuleVersion(r.Context(), run.RuleVersionID)
	if err != nil {
		h.writeAllocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, rule.LegalEntityID, actionAllocationExecute); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if run.Status != domain.AllocationRunStatusFailed {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidAllocationRunTransition))
		return
	}

	debitLines := make([]domain.AllocationJournalLine, len(run.ResultLines))
	for i, l := range run.ResultLines {
		debitLines[i] = domain.AllocationJournalLine{AccountCode: l.RecipientAccountCode, Amount: l.AllocatedAmount}
	}
	description := rule.Name + " — allocation " + run.FiscalPeriod
	journalID, err := h.clients.PostAllocationJournal(r.Context(), tenantID, rule.LegalEntityID, run.FiscalPeriod,
		run.RunID, principalID, description, rule.SourceAccountCode, run.SourceAmount, debitLines)
	if err != nil {
		h.log.Error("ReprocessAllocationRun: journal posting failed again", zap.String("run_id", run.RunID), zap.Error(err))
		if markErr := h.store.MarkAllocationRunFailed(r.Context(), run.RunID, err.Error()); markErr != nil {
			h.log.Error("failed to re-mark allocation run FAILED", zap.String("run_id", run.RunID), zap.Error(markErr))
		}
		writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
		return
	}
	if err := h.store.MarkAllocationRunPosted(r.Context(), run.RunID, journalID, time.Now().UTC()); err != nil {
		h.log.Error("allocation journal posted but the run could not be marked POSTED", zap.String("run_id", run.RunID), zap.Error(err))
	}
	h.recordLineageEdge(r.Context(), rule.LegalEntityID, "allocation_run", run.RunID, "journal", journalID)

	full, err := h.store.GetAllocationRun(r.Context(), run.RunID)
	if err != nil {
		h.log.Error("ReprocessAllocationRun: failed to reload posted run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, full)
}

func (h *Handler) GetAllocationRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetAllocationRun(r.Context(), id)
	if err != nil {
		h.writeAllocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionAllocationView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h *Handler) ListAllocationExceptions(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionAllocationView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListAllocationExceptions(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("ListAllocationExceptions: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.AllocationRun{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) writeAllocationErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAllocationRuleNotFound):
		writeError(w, http.StatusNotFound, "allocation_rule_not_found", "")
	case errors.Is(err, domain.ErrAllocationRunNotFound):
		writeError(w, http.StatusNotFound, "allocation_run_not_found", "")
	case errors.Is(err, domain.ErrInvalidAllocationRuleTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidAllocationRuleTransition))
	case errors.Is(err, domain.ErrInvalidAllocationRunTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidAllocationRunTransition))
	case errors.Is(err, domain.ErrIdentityMissing), errors.Is(err, domain.ErrTenantScopeMissing):
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", string(domain.ErrTenantScopeMissing))
	default:
		h.log.Error("allocation store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

// ── ACC-10 (Foreign Currency Revaluation) ─────────────────────────────────────────
//
// "owns FX revaluation runs/item calculations. Must never own: FX
// reference master or ledger write bypass." Rates are always
// caller-declared per run (never a platform-wide master this service
// would own); book balances are always read from GL's own trial balance,
// never caller-declared — see domain.FXRevaluationItem's doc comment.

// buildRevaluationItems validates and computes every item for a NEW
// (non-reversal) run. Two of the spec's own negative paths are enforced
// here, before any run row exists: "Rate set missing one currency" and
// "Non-monetary item included."
func (h *Handler) buildRevaluationItems(ctx context.Context, tenantID, principalID, legalEntityID, fiscalPeriod string, items []domain.RevaluationItemInput, rateSet map[string]float64) ([]domain.FXRevaluationItem, error) {
	balances, err := h.clients.CompileTrialBalance(ctx, tenantID, legalEntityID, fiscalPeriod, principalID)
	if err != nil {
		return nil, err
	}

	out := make([]domain.FXRevaluationItem, len(items))
	for i, in := range items {
		rate, ok := rateSet[in.CurrencyCode]
		if !ok {
			return nil, domain.ErrRateMissingForCurrency
		}
		accountType, err := h.clients.GetAccountType(ctx, tenantID, principalID, in.AccountCode)
		if err != nil {
			return nil, err
		}
		if accountType != domain.AccountTypeAsset && accountType != domain.AccountTypeLiability {
			return nil, domain.ErrNonMonetaryItemIncluded
		}
		bookAmount, found := balances[in.AccountCode]
		if !found {
			return nil, domain.ErrRevaluationBookBalanceNotFound
		}
		revalued := roundCents(in.ForeignAmount * rate)
		out[i] = domain.FXRevaluationItem{
			ItemID:           uuid.NewString(),
			AccountCode:      in.AccountCode,
			AccountType:      accountType,
			CurrencyCode:     in.CurrencyCode,
			ForeignAmount:    in.ForeignAmount,
			BookAmount:       bookAmount,
			ClosingRate:      rate,
			RevaluedAmount:   revalued,
			AdjustmentAmount: roundCents(revalued - bookAmount),
		}
	}
	return out, nil
}

func (h *Handler) StartRevaluation(w http.ResponseWriter, r *http.Request) {
	var req domain.StartRevaluationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.FiscalPeriod == "" || req.FXGainLossAccountCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, fiscal_period and fx_gain_loss_account_code are required")
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "no_items", string(domain.ErrNoRevaluationItems))
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionFXRevaluationStart); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	items, err := h.buildRevaluationItems(r.Context(), tenantID, principalID, req.LegalEntityID, req.FiscalPeriod, req.Items, req.RateSet)
	if err != nil {
		h.writeFXRevaluationCalcErr(w, err)
		return
	}

	run := &domain.FXRevaluationRun{
		RunID:                 uuid.NewString(),
		LegalEntityID:         req.LegalEntityID,
		FiscalPeriod:          req.FiscalPeriod,
		FXGainLossAccountCode: req.FXGainLossAccountCode,
		Status:                domain.FXRevaluationStatusReview,
		CreatedAt:             time.Now().UTC(),
		CreatedByPrincipalID:  principalID,
		Items:                 items,
	}
	if err := h.store.CreateFXRevaluationRun(r.Context(), run); err != nil {
		h.log.Error("failed to create FX revaluation run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// ReversePriorRevaluation implements "corrected/reversed via new run" —
// there is no in-place reversal of a POSTED run. The new run's items are
// the EXACT negation of the prior run's own recorded items (same
// account/currency/rate/book-amount lineage, AdjustmentAmount negated),
// never recomputed against current data — a reversal must undo exactly
// what was posted, not re-derive a different number.
func (h *Handler) ReversePriorRevaluation(w http.ResponseWriter, r *http.Request) {
	var req domain.ReversePriorRevaluationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.PriorRunID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "prior_run_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	prior, err := h.store.GetFXRevaluationRun(r.Context(), req.PriorRunID)
	if err != nil {
		h.writeFXRevaluationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, prior.LegalEntityID, actionFXRevaluationStart); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if prior.Status != domain.FXRevaluationStatusPosted {
		writeError(w, http.StatusUnprocessableEntity, "prior_not_posted", string(domain.ErrPriorRevaluationNotPosted))
		return
	}

	items := make([]domain.FXRevaluationItem, len(prior.Items))
	for i, p := range prior.Items {
		items[i] = domain.FXRevaluationItem{
			ItemID:           uuid.NewString(),
			AccountCode:      p.AccountCode,
			AccountType:      p.AccountType,
			CurrencyCode:     p.CurrencyCode,
			ForeignAmount:    p.ForeignAmount,
			BookAmount:       p.BookAmount,
			ClosingRate:      p.ClosingRate,
			RevaluedAmount:   p.RevaluedAmount,
			AdjustmentAmount: -p.AdjustmentAmount,
		}
	}

	priorRunID := prior.RunID
	run := &domain.FXRevaluationRun{
		RunID:                 uuid.NewString(),
		LegalEntityID:         prior.LegalEntityID,
		FiscalPeriod:          prior.FiscalPeriod,
		FXGainLossAccountCode: prior.FXGainLossAccountCode,
		Status:                domain.FXRevaluationStatusReview,
		ReversalOfRunID:       &priorRunID,
		CreatedAt:             time.Now().UTC(),
		CreatedByPrincipalID:  principalID,
		Items:                 items,
	}
	if err := h.store.CreateFXRevaluationRun(r.Context(), run); err != nil {
		h.log.Error("failed to create reversal FX revaluation run", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (h *Handler) ApproveRevaluation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetFXRevaluationRun(r.Context(), id)
	if err != nil {
		h.writeFXRevaluationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionFXRevaluationApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ApproveFXRevaluationRun(r.Context(), id, principalID, now); err != nil {
		h.writeFXRevaluationErr(w, err)
		return
	}
	run.Status = domain.FXRevaluationStatusApproved
	run.ApprovedAt, run.ApprovedByPrincipalID = &now, &principalID
	writeJSON(w, http.StatusOK, run)
}

// PostRevaluation is idempotent — the spec's own negative path,
// "Revaluation replay duplicates journal": a run already POSTED returns
// its existing state untouched rather than posting a second journal.
func (h *Handler) PostRevaluation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	run, err := h.store.GetFXRevaluationRun(r.Context(), id)
	if err != nil {
		h.writeFXRevaluationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionFXRevaluationPost); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if run.Status == domain.FXRevaluationStatusPosted {
		writeJSON(w, http.StatusOK, run)
		return
	}
	if run.Status != domain.FXRevaluationStatusApproved {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidFXRevaluationTransition))
		return
	}

	lines, netGain := fxRevaluationJournalLines(run.Items, run.FXGainLossAccountCode)
	if len(lines) == 0 {
		// Every item's adjustment rounded to exactly zero — nothing to
		// post, but the run is still real evidence that a revaluation was
		// performed and found no material movement. Marked POSTED with no
		// journal rather than forced to invent one.
		now := time.Now().UTC()
		if err := h.store.MarkFXRevaluationPosted(r.Context(), id, "", principalID, now); err != nil {
			h.writeFXRevaluationErr(w, err)
			return
		}
		run.Status, run.PostedAt, run.PostedByPrincipalID = domain.FXRevaluationStatusPosted, &now, &principalID
		writeJSON(w, http.StatusOK, run)
		return
	}

	description := "FX revaluation " + run.FiscalPeriod
	if run.ReversalOfRunID != nil {
		description = "FX revaluation reversal of " + *run.ReversalOfRunID + " — " + run.FiscalPeriod
	}
	journalID, err := h.clients.PostMultiLineJournal(r.Context(), tenantID, run.LegalEntityID, run.FiscalPeriod, run.RunID, principalID, description, lines)
	if err != nil {
		h.log.Error("PostRevaluation: journal posting failed", zap.String("run_id", id), zap.Float64("net_gain", netGain), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
		return
	}

	now := time.Now().UTC()
	if err := h.store.MarkFXRevaluationPosted(r.Context(), id, journalID, principalID, now); err != nil {
		h.log.Error("FX revaluation journal posted but the run could not be marked POSTED",
			zap.String("run_id", id), zap.String("journal_id", journalID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "run_not_recorded",
			"the revaluation journal IS posted to general-ledger-svc ("+journalID+"), but the run could not be marked POSTED.")
		return
	}
	h.recordLineageEdge(r.Context(), run.LegalEntityID, "fx_revaluation_run", run.RunID, "journal", journalID)
	run.Status, run.JournalID, run.PostedAt, run.PostedByPrincipalID = domain.FXRevaluationStatusPosted, &journalID, &now, &principalID
	writeJSON(w, http.StatusOK, run)
}

// fxRevaluationJournalLines turns a run's items into a balanced,
// mixed-sign journal: each item moves its own monetary account by its
// AdjustmentAmount (direction depends on AccountType — a growing
// liability is a LOSS, a growing asset is a GAIN), and the run's single
// fxGainLossAccountCode absorbs the NET signed total. Items whose
// adjustment rounds to exactly zero are skipped — there is nothing to
// post for them.
func fxRevaluationJournalLines(items []domain.FXRevaluationItem, fxGainLossAccountCode string) (lines []domain.JournalLineInput, netGain float64) {
	for _, item := range items {
		if item.AdjustmentAmount == 0 {
			continue
		}
		var itemGain float64
		switch item.AccountType {
		case domain.AccountTypeAsset:
			itemGain = item.AdjustmentAmount
			if item.AdjustmentAmount >= 0 {
				lines = append(lines, domain.JournalLineInput{AccountCode: item.AccountCode, DebitAmount: item.AdjustmentAmount})
			} else {
				lines = append(lines, domain.JournalLineInput{AccountCode: item.AccountCode, CreditAmount: -item.AdjustmentAmount})
			}
		case domain.AccountTypeLiability:
			itemGain = -item.AdjustmentAmount
			if item.AdjustmentAmount >= 0 {
				lines = append(lines, domain.JournalLineInput{AccountCode: item.AccountCode, CreditAmount: item.AdjustmentAmount})
			} else {
				lines = append(lines, domain.JournalLineInput{AccountCode: item.AccountCode, DebitAmount: -item.AdjustmentAmount})
			}
		}
		netGain += itemGain
	}
	if len(lines) == 0 {
		return nil, 0
	}
	netGain = roundCents(netGain)
	if netGain >= 0 {
		lines = append(lines, domain.JournalLineInput{AccountCode: fxGainLossAccountCode, CreditAmount: netGain})
	} else {
		lines = append(lines, domain.JournalLineInput{AccountCode: fxGainLossAccountCode, DebitAmount: -netGain})
	}
	return lines, netGain
}

func (h *Handler) GetFXRevaluation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	run, err := h.store.GetFXRevaluationRun(r.Context(), id)
	if err != nil {
		h.writeFXRevaluationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, run.LegalEntityID, actionFXRevaluationView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h *Handler) ListFXRevaluations(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	fiscalPeriod := r.URL.Query().Get("fiscal_period")
	if legalEntityID == "" || fiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and fiscal_period are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionFXRevaluationView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListFXRevaluationRuns(r.Context(), legalEntityID, fiscalPeriod)
	if err != nil {
		h.log.Error("ListFXRevaluations: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.FXRevaluationRun{}
	}
	writeJSON(w, http.StatusOK, list)
}

// writeFXRevaluationCalcErr maps an error from buildRevaluationItems —
// distinguishing the caller's own bad input (422) from a dependency
// outage (503).
func (h *Handler) writeFXRevaluationCalcErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrRateMissingForCurrency):
		writeError(w, http.StatusUnprocessableEntity, "rate_missing_for_currency", string(domain.ErrRateMissingForCurrency))
	case errors.Is(err, domain.ErrNonMonetaryItemIncluded):
		writeError(w, http.StatusUnprocessableEntity, "non_monetary_item_included", string(domain.ErrNonMonetaryItemIncluded))
	case errors.Is(err, domain.ErrRevaluationBookBalanceNotFound):
		writeError(w, http.StatusUnprocessableEntity, "book_balance_not_found", string(domain.ErrRevaluationBookBalanceNotFound))
	default:
		h.log.Error("FX revaluation calculation: dependency unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", err.Error())
	}
}

func (h *Handler) writeFXRevaluationErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrFXRevaluationRunNotFound):
		writeError(w, http.StatusNotFound, "fx_revaluation_run_not_found", "")
	case errors.Is(err, domain.ErrInvalidFXRevaluationTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidFXRevaluationTransition))
	case errors.Is(err, domain.ErrIdentityMissing), errors.Is(err, domain.ErrTenantScopeMissing):
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", string(domain.ErrTenantScopeMissing))
	default:
		h.log.Error("FX revaluation store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

// ── ACC-17 (Opening Balance & Migration) ──────────────────────────────────────────
//
// "owns Migration accounting batch/crosswalk/certification. Must never
// own: Bypass of ACC-04/05" — opening balances always post through GL's
// real Create/Validate/Post journal lifecycle and this service's own real
// period status, exactly like every other capability. There is no
// separate "bulk import" path.

// CreateMigrationAccountingBatch loads a batch's crosswalk entries
// synchronously (Planned collapses into LOADED — see domain package doc
// comment). Idempotent on (legal_entity_id, fiscal_period,
// source_system_name): a retried create for the same source system and
// period returns the EXISTING batch.
func (h *Handler) CreateMigrationAccountingBatch(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateMigrationBatchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.FiscalPeriod == "" || req.SourceSystemName == "" || req.SourceExtractHash == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"legal_entity_id, fiscal_period, source_system_name and source_extract_hash are required")
		return
	}
	if len(req.Entries) == 0 {
		writeError(w, http.StatusBadRequest, "no_entries", string(domain.ErrNoMigrationEntries))
		return
	}
	seen := make(map[string]bool, len(req.Entries))
	for _, e := range req.Entries {
		if seen[e.SourceReferenceID] {
			writeError(w, http.StatusBadRequest, "duplicate_source_reference", string(domain.ErrDuplicateSourceReference)+": "+e.SourceReferenceID)
			return
		}
		seen[e.SourceReferenceID] = true
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionMigrationBatchCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if existing, err := h.store.GetMigrationBatchBySourceSystem(r.Context(), req.LegalEntityID, req.FiscalPeriod, req.SourceSystemName); err == nil {
		writeJSON(w, http.StatusOK, existing)
		return
	} else if !errors.Is(err, domain.ErrMigrationBatchNotFound) {
		h.log.Error("CreateMigrationAccountingBatch: failed to check for an existing batch", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	entries := make([]domain.MigrationCrosswalkEntry, len(req.Entries))
	for i, e := range req.Entries {
		entries[i] = domain.MigrationCrosswalkEntry{
			EntryID:           uuid.NewString(),
			SourceReferenceID: e.SourceReferenceID,
			SourceAccountCode: e.SourceAccountCode,
			TargetAccountCode: e.TargetAccountCode,
			DebitAmount:       e.DebitAmount,
			CreditAmount:      e.CreditAmount,
		}
	}
	batch := &domain.MigrationBatch{
		BatchID:              uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		FiscalPeriod:         req.FiscalPeriod,
		SourceSystemName:     req.SourceSystemName,
		SourceExtractHash:    req.SourceExtractHash,
		ExpectedRowCount:     req.ExpectedRowCount,
		ExpectedTotalDebits:  req.ExpectedTotalDebits,
		ExpectedTotalCredits: req.ExpectedTotalCredits,
		Status:               domain.MigrationBatchStatusLoaded,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
		Entries:              entries,
	}
	if err := h.store.CreateMigrationBatch(r.Context(), batch); err != nil {
		h.log.Error("failed to create migration batch", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, batch)
}

// ValidateOpeningBalances enforces three of the spec's own negative
// paths, in order, quarantining the batch (with a permanent, evidenced
// reason) on the first one it finds rather than merely answering an
// ephemeral HTTP error while the batch stays silently stuck:
//  1. "Non-monetary"/invalid target accounts — every target_account_code
//     must resolve to a real, ACTIVE GL account.
//  2. "Opening TB forced with suspense plug" — no target account may name
//     itself a suspense account, AND total debits must equal total
//     credits exactly with no plug.
//  3. "Source-target row counts match but values differ" — the loaded
//     entries must match the SOURCE system's own declared control totals
//     (row count, total debits, total credits), independently declared
//     at CreateMigrationAccountingBatch time.
func (h *Handler) ValidateOpeningBalances(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	batch, err := h.store.GetMigrationBatch(r.Context(), id)
	if err != nil {
		h.writeMigrationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, batch.LegalEntityID, actionMigrationBatchValidate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if batch.Status != domain.MigrationBatchStatusLoaded {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidMigrationBatchTransition))
		return
	}

	var totalDebits, totalCredits float64
	for _, e := range batch.Entries {
		totalDebits = roundCents(totalDebits + e.DebitAmount)
		totalCredits = roundCents(totalCredits + e.CreditAmount)

		if strings.Contains(strings.ToUpper(e.TargetAccountCode), "SUSPENSE") {
			h.quarantineMigrationBatch(r.Context(), id, string(domain.ErrSuspenseAccountNotAllowed)+": "+e.TargetAccountCode)
			writeError(w, http.StatusUnprocessableEntity, "suspense_account_not_allowed", string(domain.ErrSuspenseAccountNotAllowed)+": "+e.TargetAccountCode)
			return
		}
		status, err := h.clients.GetAccountStatus(r.Context(), tenantID, principalID, e.TargetAccountCode)
		if err != nil {
			if errors.Is(err, domain.ErrRecipientAccountInvalid) {
				h.quarantineMigrationBatch(r.Context(), id, string(domain.ErrMigrationTargetAccountInvalid)+": "+e.TargetAccountCode)
				writeError(w, http.StatusUnprocessableEntity, "target_account_invalid", string(domain.ErrMigrationTargetAccountInvalid)+": "+e.TargetAccountCode)
				return
			}
			h.log.Error("ValidateOpeningBalances: failed to verify target account", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", err.Error())
			return
		}
		if status != "ACTIVE" {
			h.quarantineMigrationBatch(r.Context(), id, string(domain.ErrMigrationTargetAccountInvalid)+": "+e.TargetAccountCode+" is not ACTIVE")
			writeError(w, http.StatusUnprocessableEntity, "target_account_invalid", string(domain.ErrMigrationTargetAccountInvalid)+": "+e.TargetAccountCode+" is not ACTIVE")
			return
		}
	}

	if totalDebits != totalCredits {
		h.quarantineMigrationBatch(r.Context(), id, string(domain.ErrOpeningTBDoesNotBalance))
		writeError(w, http.StatusUnprocessableEntity, "opening_tb_does_not_balance", string(domain.ErrOpeningTBDoesNotBalance))
		return
	}
	if len(batch.Entries) != batch.ExpectedRowCount || totalDebits != roundCents(batch.ExpectedTotalDebits) || totalCredits != roundCents(batch.ExpectedTotalCredits) {
		h.quarantineMigrationBatch(r.Context(), id, string(domain.ErrControlTotalsMismatch))
		writeError(w, http.StatusUnprocessableEntity, "control_totals_mismatch", string(domain.ErrControlTotalsMismatch))
		return
	}

	now := time.Now().UTC()
	if err := h.store.MarkMigrationBatchValidated(r.Context(), id, now); err != nil {
		h.writeMigrationErr(w, err)
		return
	}
	batch.Status, batch.ValidatedAt = domain.MigrationBatchStatusValidated, &now
	writeJSON(w, http.StatusOK, batch)
}

// quarantineMigrationBatch is best-effort: the caller is about to answer
// the real validation failure regardless, and a failure to persist the
// QUARANTINED transition itself is logged, not allowed to mask the
// original, more important error.
func (h *Handler) quarantineMigrationBatch(ctx context.Context, batchID, reason string) {
	if err := h.store.QuarantineMigrationBatch(ctx, batchID, domain.MigrationBatchStatusLoaded, reason); err != nil {
		h.log.Error("failed to record migration batch quarantine", zap.String("batch_id", batchID), zap.Error(err))
	}
}

func (h *Handler) ApproveMigrationBatchHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	batch, err := h.store.GetMigrationBatch(r.Context(), id)
	if err != nil {
		h.writeMigrationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, batch.LegalEntityID, actionMigrationBatchApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ApproveMigrationBatch(r.Context(), id, principalID, now); err != nil {
		h.writeMigrationErr(w, err)
		return
	}
	batch.Status, batch.ApprovedAt, batch.ApprovedByPrincipalID = domain.MigrationBatchStatusApproved, &now, &principalID
	writeJSON(w, http.StatusOK, batch)
}

// CommitOpeningPosting is idempotent — the spec's own negative path,
// "Commit repeated after timeout": a batch already POSTED (or beyond)
// returns its existing state rather than posting a second journal.
// Reconciliation happens automatically, immediately after a successful
// post, rather than as a separately-triggered step — a deliberate v1
// scope decision, see the findings section for why.
func (h *Handler) CommitOpeningPosting(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	batch, err := h.store.GetMigrationBatch(r.Context(), id)
	if err != nil {
		h.writeMigrationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, batch.LegalEntityID, actionMigrationBatchCommit); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if batch.Status == domain.MigrationBatchStatusPosted || batch.Status == domain.MigrationBatchStatusReconciled || batch.Status == domain.MigrationBatchStatusCertified {
		writeJSON(w, http.StatusOK, batch)
		return
	}
	if batch.Status != domain.MigrationBatchStatusApproved {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidMigrationBatchTransition))
		return
	}

	fp, err := h.store.GetFiscalPeriodByName(r.Context(), batch.LegalEntityID, batch.FiscalPeriod)
	if err != nil && !errors.Is(err, domain.ErrFiscalPeriodNotFound) {
		h.log.Error("CommitOpeningPosting: failed to check period status", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if fp != nil && fp.CloseStatus == "LOCKED" {
		writeError(w, http.StatusUnprocessableEntity, "period_locked", string(domain.ErrMigrationPeriodLocked))
		return
	}

	lines := make([]domain.JournalLineInput, len(batch.Entries))
	for i, e := range batch.Entries {
		lines[i] = domain.JournalLineInput{AccountCode: e.TargetAccountCode, DebitAmount: e.DebitAmount, CreditAmount: e.CreditAmount}
	}
	description := "Opening balance migration from " + batch.SourceSystemName + " — " + batch.FiscalPeriod
	journalID, err := h.clients.PostMultiLineJournal(r.Context(), tenantID, batch.LegalEntityID, batch.FiscalPeriod, batch.BatchID, principalID, description, lines)
	if err != nil {
		h.log.Error("CommitOpeningPosting: journal posting failed", zap.String("batch_id", id), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "journal_posting_failed", err.Error())
		return
	}

	now := time.Now().UTC()
	if err := h.store.MarkMigrationBatchPosted(r.Context(), id, journalID, now); err != nil {
		h.log.Error("opening balances posted but the batch could not be marked POSTED",
			zap.String("batch_id", id), zap.String("journal_id", journalID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "batch_not_recorded",
			"the opening balance journal IS posted to general-ledger-svc ("+journalID+"), but the batch could not be marked POSTED.")
		return
	}
	h.recordLineageEdge(r.Context(), batch.LegalEntityID, "migration_batch", batch.BatchID, "journal", journalID)
	if err := h.store.MarkMigrationBatchReconciled(r.Context(), id, now); err != nil {
		h.log.Error("failed to mark migration batch RECONCILED", zap.String("batch_id", id), zap.Error(err))
	}

	full, err := h.store.GetMigrationBatch(r.Context(), id)
	if err != nil {
		h.log.Error("CommitOpeningPosting: failed to reload posted batch", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, full)
}

func (h *Handler) CertifyMigrationAccounting(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.CertifyMigrationBatchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason_required", "reason is required to certify a migration batch")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	batch, err := h.store.GetMigrationBatch(r.Context(), id)
	if err != nil {
		h.writeMigrationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, batch.LegalEntityID, actionMigrationBatchCertify); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.CertifyMigrationBatch(r.Context(), id, principalID, req.Reason, now); err != nil {
		h.writeMigrationErr(w, err)
		return
	}
	batch.Status, batch.CertifiedAt, batch.CertifiedByPrincipalID, batch.CertificationReason = domain.MigrationBatchStatusCertified, &now, &principalID, &req.Reason
	writeJSON(w, http.StatusOK, batch)
}

func (h *Handler) GetMigrationBatchHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	batch, err := h.store.GetMigrationBatch(r.Context(), id)
	if err != nil {
		h.writeMigrationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, batch.LegalEntityID, actionMigrationBatchView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func (h *Handler) GetMigrationExceptions(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionMigrationBatchView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListQuarantinedMigrationBatches(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("GetMigrationExceptions: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.MigrationBatch{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) writeMigrationErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrMigrationBatchNotFound):
		writeError(w, http.StatusNotFound, "migration_batch_not_found", "")
	case errors.Is(err, domain.ErrInvalidMigrationBatchTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidMigrationBatchTransition))
	case errors.Is(err, domain.ErrIdentityMissing), errors.Is(err, domain.ErrTenantScopeMissing):
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", string(domain.ErrTenantScopeMissing))
	default:
		h.log.Error("migration batch store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

// ── ACC-16 (Signed Financial Snapshot) ────────────────────────────────────────────
//
// "owns Signed financial snapshots/manifests. Must never own: Mutable
// live balances." Content is fixed entirely at creation — there is no
// endpoint anywhere that updates it, sealed or not, so "alter snapshot
// content after seal" (the spec's own negative path) is satisfied
// structurally rather than by a runtime check.

func (h *Handler) CreateFinancialSnapshot(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateFinancialSnapshotRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.Purpose == "" || req.Content == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, purpose and content are required")
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionSnapshotCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	snap := &domain.FinancialSnapshot{
		SnapshotID:              uuid.NewString(),
		TenantID:                tenantID,
		LegalEntityID:           req.LegalEntityID,
		Purpose:                 req.Purpose,
		Content:                 req.Content,
		SourceReferences:        req.SourceReferences,
		HasUnresolvedExceptions: req.HasUnresolvedExceptions,
		Status:                  domain.SnapshotStatusDraft,
		CreatedAt:               time.Now().UTC(),
		CreatedByPrincipalID:    principalID,
	}
	if err := h.store.CreateFinancialSnapshot(r.Context(), snap); err != nil {
		h.log.Error("failed to create financial snapshot", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, snap)
}

// SealSnapshot computes the content hash and HMAC signature over the
// snapshot's own already-fixed Content + SourceReferences and freezes it
// — the spec's own negative path, "signing key invalid/revoked," is
// checked directly: a service that somehow reached this handler with no
// real signing key refuses to seal rather than producing a signature
// that verifies against nothing.
func (h *Handler) SealSnapshot(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	snap, err := h.store.GetFinancialSnapshot(r.Context(), id)
	if err != nil {
		h.writeSnapshotErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, snap.LegalEntityID, actionSnapshotSeal); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if len(h.signingKey) == 0 {
		writeError(w, http.StatusServiceUnavailable, "signing_key_unavailable", string(domain.ErrSigningKeyUnavailable))
		return
	}

	manifest := snap.Purpose + "|" + snap.Content + "|" + snap.SourceReferences
	hashBytes := sha256.Sum256([]byte(manifest))
	contentHash := hex.EncodeToString(hashBytes[:])
	signature := h.signEvidence(hashBytes[:])

	now := time.Now().UTC()
	if err := h.store.SealFinancialSnapshot(r.Context(), id, contentHash, signature, now); err != nil {
		h.writeSnapshotErr(w, err)
		return
	}
	snap.Status, snap.ContentHash, snap.Signature, snap.SealedAt = domain.SnapshotStatusSealed, &contentHash, &signature, &now
	writeJSON(w, http.StatusOK, snap)
}

// CertifySnapshot enforces the spec's own negative path, "Certify
// snapshot with unresolved prohibited exception": a snapshot created (or
// later found) with HasUnresolvedExceptions still true cannot be
// certified.
func (h *Handler) CertifySnapshot(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.CertifySnapshotRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason_required", "reason is required to certify a financial snapshot")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	snap, err := h.store.GetFinancialSnapshot(r.Context(), id)
	if err != nil {
		h.writeSnapshotErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, snap.LegalEntityID, actionSnapshotCertify); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if snap.HasUnresolvedExceptions {
		writeError(w, http.StatusUnprocessableEntity, "unresolved_exception", string(domain.ErrCertifyWithUnresolvedException))
		return
	}

	now := time.Now().UTC()
	if err := h.store.CertifyFinancialSnapshot(r.Context(), id, principalID, req.Reason, now); err != nil {
		h.writeSnapshotErr(w, err)
		return
	}
	snap.Status, snap.CertifiedAt, snap.CertifiedByPrincipalID, snap.CertificationReason = domain.SnapshotStatusCertified, &now, &principalID, &req.Reason
	writeJSON(w, http.StatusOK, snap)
}

// SupersedeSnapshot creates a brand-new DRAFT snapshot and, in the same
// request, marks the PRIOR snapshot SUPERSEDED pointing at it — the
// supersession chain is a linked list of otherwise-independent snapshots,
// never a destructive overwrite of the prior one's sealed content.
func (h *Handler) SupersedeSnapshot(w http.ResponseWriter, r *http.Request) {
	priorID := chi.URLParam(r, "id")
	var req domain.CreateFinancialSnapshotRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Purpose == "" || req.Content == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "purpose and content are required")
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
	prior, err := h.store.GetFinancialSnapshot(r.Context(), priorID)
	if err != nil {
		h.writeSnapshotErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, prior.LegalEntityID, actionSnapshotSupersede); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if prior.Status != domain.SnapshotStatusSealed && prior.Status != domain.SnapshotStatusCertified {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidSnapshotTransition))
		return
	}

	next := &domain.FinancialSnapshot{
		SnapshotID:              uuid.NewString(),
		TenantID:                tenantID,
		LegalEntityID:           prior.LegalEntityID,
		Purpose:                 req.Purpose,
		Content:                 req.Content,
		SourceReferences:        req.SourceReferences,
		HasUnresolvedExceptions: req.HasUnresolvedExceptions,
		Status:                  domain.SnapshotStatusDraft,
		CreatedAt:               time.Now().UTC(),
		CreatedByPrincipalID:    principalID,
	}
	if err := h.store.CreateFinancialSnapshot(r.Context(), next); err != nil {
		h.log.Error("failed to create superseding financial snapshot", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	now := time.Now().UTC()
	if err := h.store.SupersedeFinancialSnapshot(r.Context(), priorID, prior.Status, next.SnapshotID, now); err != nil {
		h.log.Error("superseding snapshot created but the prior snapshot could not be marked SUPERSEDED",
			zap.String("prior_snapshot_id", priorID), zap.String("new_snapshot_id", next.SnapshotID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "supersession_not_recorded",
			"a new snapshot ("+next.SnapshotID+") was created, but the prior snapshot could not be marked SUPERSEDED.")
		return
	}
	writeJSON(w, http.StatusCreated, next)
}

func (h *Handler) GetFinancialSnapshotHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	snap, err := h.store.GetFinancialSnapshot(r.Context(), id)
	if err != nil {
		h.writeSnapshotErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, snap.LegalEntityID, actionSnapshotView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (h *Handler) ListSnapshotSupersession(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	purpose := r.URL.Query().Get("purpose")
	if legalEntityID == "" || purpose == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and purpose are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionSnapshotView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListSnapshotSupersession(r.Context(), legalEntityID, purpose)
	if err != nil {
		h.log.Error("ListSnapshotSupersession: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.FinancialSnapshot{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) writeSnapshotErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrFinancialSnapshotNotFound):
		writeError(w, http.StatusNotFound, "financial_snapshot_not_found", "")
	case errors.Is(err, domain.ErrInvalidSnapshotTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", string(domain.ErrInvalidSnapshotTransition))
	case errors.Is(err, domain.ErrIdentityMissing), errors.Is(err, domain.ErrTenantScopeMissing):
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", string(domain.ErrTenantScopeMissing))
	default:
		h.log.Error("financial snapshot store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

// ── ACC-18 (Source-to-Report Traceability) ────────────────────────────────────────
//
// "owns Lineage graph/index and verification results. Must never own:
// Underlying accounting/business facts." recordLineageEdge is called by
// every OTHER posting capability this pass built (ACC-07/08/09/10/17)
// immediately after a successful GL post — this is the only place an
// edge is ever written, and it is never inferred or guessed (the spec's
// own negative path, "Lineage service invents inferred source").

// recordLineageEdge is best-effort: the source capability's own posting
// already succeeded and must not be rolled back or fail because lineage
// bookkeeping had a problem. A failure here degrades the PROJECTION
// (visibly, via lineage_projection_status), never the original
// operation — the spec's own negative path, "Projection stale after
// adjustment," made a real, visible state instead of a silent gap.
func (h *Handler) recordLineageEdge(ctx context.Context, legalEntityID, fromType, fromID, toType, toID string) {
	edge := &domain.LineageEdge{
		EdgeID:        uuid.NewString(),
		LegalEntityID: legalEntityID,
		FromType:      fromType,
		FromID:        fromID,
		ToType:        toType,
		ToID:          toID,
		RecordedAt:    time.Now().UTC(),
	}
	if err := h.store.RecordLineageEdge(ctx, edge); err != nil {
		h.log.Error("failed to record lineage edge — marking projection DEGRADED",
			zap.String("legal_entity_id", legalEntityID), zap.String("from_type", fromType), zap.String("from_id", fromID), zap.Error(err))
		reason := "failed to record edge for " + fromType + ":" + fromID + ": " + err.Error()
		if upsertErr := h.store.UpsertLineageProjectionStatus(ctx, legalEntityID, domain.LineageProjectionDegraded, &reason, nil); upsertErr != nil {
			h.log.Error("failed to mark lineage projection DEGRADED", zap.Error(upsertErr))
		}
	}
}

// TraceJournalToSource answers ACC-18's own TraceJournalToSource query —
// every recorded edge pointing AT this journal. No edges is reported
// plainly as "no lineage recorded," never inferred from the journal_id's
// own shape or any other heuristic.
func (h *Handler) TraceJournalToSource(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	edges, err := h.store.ListLineageEdgesTo(r.Context(), "journal", journalID)
	if err != nil {
		h.log.Error("TraceJournalToSource: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if len(edges) == 0 {
		writeJSON(w, http.StatusOK, []domain.LineageEdge{})
		return
	}
	// A caller can only drill into an entity within their own authorized
	// legal entity — the spec's own negative path, "User drills into
	// unauthorized entity." All edges to one journal share a legal
	// entity, so the first is authoritative for the check.
	if err := h.authz.CheckAllowed(r.Context(), principalID, edges[0].LegalEntityID, actionLineageView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, edges)
}

// VerifyLineageCompleteness answers ACC-18's own VerifyLineageCompleteness
// query — the spec's own negative path, "Missing journal-source link,"
// reported as an explicit, honest gap list rather than silently assumed
// complete.
func (h *Handler) VerifyLineageCompleteness(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionLineageView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	report, err := h.buildCompletenessReport(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("VerifyLineageCompleteness: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *Handler) buildCompletenessReport(ctx context.Context, legalEntityID string) (*domain.LineageCompletenessReport, error) {
	refs, err := h.store.ListPostedJournalRefs(ctx, legalEntityID)
	if err != nil {
		return nil, err
	}
	report := &domain.LineageCompletenessReport{LegalEntityID: legalEntityID, CheckedCount: len(refs), Gaps: []domain.PostedJournalRef{}}
	for _, ref := range refs {
		edges, err := h.store.ListLineageEdgesTo(ctx, "journal", ref.JournalID)
		if err != nil {
			return nil, err
		}
		found := false
		for _, e := range edges {
			if e.FromType == ref.FromType && e.FromID == ref.FromID {
				found = true
				break
			}
		}
		if !found {
			report.Gaps = append(report.Gaps, ref)
		}
	}
	report.Complete = len(report.Gaps) == 0
	return report, nil
}

// RebuildLineageProjection re-derives every missing edge from the
// already-built ACC capabilities' own posted records (never fabricating
// one) and, if the projection was DEGRADED, restores it to CURRENT once
// every gap is closed.
func (h *Handler) RebuildLineageProjection(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionLineageRebuild); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.UpsertLineageProjectionStatus(r.Context(), legalEntityID, domain.LineageProjectionRebuilding, nil, nil); err != nil {
		h.log.Error("RebuildLineageProjection: failed to mark REBUILDING", zap.Error(err))
	}

	report, err := h.buildCompletenessReport(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("RebuildLineageProjection: failed to compute gaps", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	for _, gap := range report.Gaps {
		h.recordLineageEdge(r.Context(), legalEntityID, gap.FromType, gap.FromID, "journal", gap.JournalID)
	}

	now := time.Now().UTC()
	if err := h.store.UpsertLineageProjectionStatus(r.Context(), legalEntityID, domain.LineageProjectionCurrent, nil, &now); err != nil {
		h.log.Error("RebuildLineageProjection: failed to mark CURRENT", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	status, err := h.store.GetLineageProjectionStatus(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("RebuildLineageProjection: failed to reload status", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *Handler) GetLineageProjectionStatusHandler(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionLineageView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	status, err := h.store.GetLineageProjectionStatus(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("GetLineageProjectionStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// ── GET /v1/close/periods/{id}/readiness ─────────────────────────────────────────
//
// The same three checks the lock runs, with no side effects: nothing is
// written, nothing is published, and the period is not touched.
//
// Without this the only way to learn whether a period could close was to
// attempt the close — which emits close.started and close.blocked events for
// what was really a question, and leaves an audit trail of attempted closes
// that nobody attempted. A month-end is checked repeatedly and locked once.
func (h *Handler) GetPeriodReadiness(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	fp, err := h.store.GetFiscalPeriod(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}

	// Reading readiness is a view, not an initiation — it changes nothing.
	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionCloseView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// A period that is already locked is not "ready to close" — it is closed.
	// Answering is_ready:true would invite a lock that then fails 422.
	if fp.CloseStatus != "OPEN" {
		writeJSON(w, http.StatusOK, domain.ReadinessCheckResponse{
			IsReady:        false,
			BlockingIssues: []string{"period_already_locked: this period is " + fp.CloseStatus + " and cannot be closed again"},
		})
		return
	}

	blockingIssues, err := h.checkReadiness(r.Context(), tenantID, fp)
	if err != nil {
		h.writeReadinessErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, domain.ReadinessCheckResponse{
		IsReady: len(blockingIssues) == 0,
		// Never nil: a JSON null forces every caller to special-case it, and
		// "no blocking issues" is an empty list.
		BlockingIssues: append([]string{}, blockingIssues...),
	})
}

// checkReadiness runs the three dependency checks for a period.
//
// Fails closed: a dependency that cannot be queried returns an error, never an
// empty issue list. "We could not check" and "there is nothing to report" are
// opposite answers, and conflating them would close a period on the strength of
// a service being down.
func (h *Handler) checkReadiness(ctx context.Context, tenantID string, fp *domain.FiscalPeriod) ([]string, error) {
	var issues []string

	unposted, err := h.clients.GetUnpostedJournalsCount(ctx, tenantID, fp.LegalEntityID, fp.PeriodName)
	if err != nil {
		h.log.Error("failed to verify outstanding journals", zap.Error(err))
		return nil, fmt.Errorf("general-ledger-svc: %w", err)
	}
	if unposted > 0 {
		issues = append(issues, fmt.Sprintf("unposted_journals_exist: %d %s in PENDING or VALIDATED status",
			unposted, plural(unposted, "journal is", "journals are")))
	}

	unsettledAP, err := h.clients.GetUnsettledAPInvoicesCount(ctx, tenantID, fp.LegalEntityID, fp.PeriodStart, fp.PeriodEnd)
	if err != nil {
		h.log.Error("failed to verify AP invoices", zap.Error(err))
		return nil, fmt.Errorf("accounts-payable-svc: %w", err)
	}
	if unsettledAP > 0 {
		issues = append(issues, fmt.Sprintf("unsettled_ap_invoices_exist: %d %s due in this period not fully payment requested",
			unsettledAP, plural(unsettledAP, "invoice is", "invoices are")))
	}

	unsettledAR, err := h.clients.GetUnsettledARInvoicesCount(ctx, tenantID, fp.LegalEntityID, fp.PeriodStart, fp.PeriodEnd)
	if err != nil {
		h.log.Error("failed to verify AR invoices", zap.Error(err))
		return nil, fmt.Errorf("accounts-receivable-svc: %w", err)
	}
	if unsettledAR > 0 {
		issues = append(issues, fmt.Sprintf("unsettled_ar_invoices_exist: %d %s due in this period not PAID",
			unsettledAR, plural(unsettledAR, "invoice is", "invoices are")))
	}

	return issues, nil
}

// plural picks the singular or plural wording for a count. These strings are
// rendered verbatim in the console's blocking-issue list, and "1 journals are
// in PENDING status" is the kind of detail that makes a careful message read as
// generated noise.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// signEvidence produces the HMAC over the trial balance hash.
//
// The key used to be the tenant ID. A tenant ID is not a secret — it travels in
// the X-Tenant-Id header of every request, sits in the console's URLs, and is
// printed in this service's own responses — so anyone who had ever seen a
// request could forge a signature over any trial balance they liked. A field
// named `signature`, stored beside the hash it covers, states that the evidence
// is attributable and tamper-evident; keyed with a public value it stated
// something untrue, which is worse than storing no signature at all.
//
// The key now comes from CLOSE_SIGNING_KEY and the service refuses to start
// without one (see cmd/server), so this can never silently fall back to
// something guessable.
func (h *Handler) signEvidence(hash []byte) string {
	mac := hmac.New(sha256.New, h.signingKey)
	mac.Write(hash)
	return hex.EncodeToString(mac.Sum(nil))
}

// ── Helpers ──────────────────────────────────────────────────────────────────────

// writeReadinessErr reports a dependency that could not be queried. Always 503
// and always a refusal: a close is never allowed to proceed on an unchecked
// dependency.
func (h *Handler) writeReadinessErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrLedgerPageTruncated) {
		writeError(w, http.StatusServiceUnavailable, "ledger_page_truncated", string(domain.ErrLedgerPageTruncated))
		return
	}
	writeError(w, http.StatusServiceUnavailable, "readiness_check_failed", err.Error()+" — close blocked")
}

// writeStoreErr maps a store failure to the status it actually means. A missing
// tenant scope is the caller's request being wrong (401) and an unknown period
// is a 404; neither is the database being down, and both used to answer 503.
func (h *Handler) writeStoreErr(w http.ResponseWriter, err error, notFoundCode string) {
	switch {
	case errors.Is(err, domain.ErrFiscalPeriodNotFound):
		writeError(w, http.StatusNotFound, notFoundCode, "")
	case errors.Is(err, domain.ErrIdentityMissing), errors.Is(err, domain.ErrTenantScopeMissing):
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", string(domain.ErrTenantScopeMissing))
	default:
		h.log.Error("store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

// requireTenant reads the caller's verified tenant scope from X-Tenant-Id, set
// by the gateway's ForwardAuth step alongside X-Principal-Id. A request without
// one never passed verification and is refused rather than served under a
// tenant it names itself.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", string(domain.ErrTenantScopeMissing))
		return "", false
	}
	return tenantID, true
}

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

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
