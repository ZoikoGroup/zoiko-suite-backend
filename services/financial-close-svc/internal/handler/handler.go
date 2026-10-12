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
	CreateCloseRequirement(ctx context.Context, cr *domain.CloseRequirement) (created bool, err error)
	GetCloseRequirement(ctx context.Context, requirementID string) (*domain.CloseRequirement, error)
	ListCloseRequirements(ctx context.Context, legalEntityID string) ([]domain.CloseRequirement, error)
	RemoveCloseRequirement(ctx context.Context, requirementID, principalID, reason string, at time.Time) (*domain.CloseRequirement, error)
	CreateFiscalPeriod(ctx context.Context, fp *domain.FiscalPeriod) (created bool, err error)
	GetFiscalPeriod(ctx context.Context, id string) (*domain.FiscalPeriod, error)
	GetFiscalPeriodByName(ctx context.Context, legalEntityID, name string) (*domain.FiscalPeriod, error)
	ListFiscalPeriods(ctx context.Context, legalEntityID string) ([]domain.FiscalPeriod, error)
	// ApplyPeriodTransition moves a period to u.To from one of allowedFrom,
	// recording the transition atomically; ErrInvalidPeriodTransition (with
	// the current period) when its state no longer allows it.
	ApplyPeriodTransition(ctx context.Context, id string, allowedFrom []string, u domain.PeriodUpdate) (*domain.FiscalPeriod, error)
	CreateReopenRequest(ctx context.Context, req *domain.ReopenRequest) error
	GetReopenRequest(ctx context.Context, requestID string) (*domain.ReopenRequest, error)
	ApproveReopenRequest(ctx context.Context, requestID, approverID, reason string, at time.Time) (*domain.ReopenRequest, *domain.FiscalPeriod, error)
	RejectReopenRequest(ctx context.Context, requestID, deciderID, reason string, at time.Time) (*domain.ReopenRequest, error)
	GetCloseHistory(ctx context.Context, fiscalPeriodID string) (*domain.CloseHistory, error)
	CreateCloseEvidence(ctx context.Context, evidence *domain.CloseEvidence) error
	ListCloseEvidence(ctx context.Context, fiscalPeriodID string) ([]domain.CloseEvidence, error)
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
	GetRecognitionInstanceByPeriod(ctx context.Context, scheduleID, fiscalPeriod string) (*domain.RecognitionInstance, error)
	GetRecognitionReversalByInstance(ctx context.Context, recognitionInstanceID string) (*domain.RecognitionReversal, error)
	CreateRecognitionReversal(ctx context.Context, rev *domain.RecognitionReversal) (created bool, err error)

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
	// SupersedeAllocationRule is ACC-09's own SupersedeAllocationRule
	// command — see its own doc comment in internal/store.
	SupersedeAllocationRule(ctx context.Context, ruleID string, newVersion *domain.AllocationRule, supersededAt time.Time) error
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
	// ListLineageEdgesToAsOf/CreateTracePathVerification/
	// CreateQuarantinedLineageGap/ListQuarantinedLineageGaps back
	// GetLineageAsOf/VerifyTracePath/QuarantineBrokenLineage — see their
	// own doc comments in internal/store.
	ListLineageEdgesToAsOf(ctx context.Context, toType, toID string, asOf time.Time) ([]domain.LineageEdge, error)
	CreateTracePathVerification(ctx context.Context, v *domain.TracePathVerification) error
	CreateQuarantinedLineageGap(ctx context.Context, g *domain.QuarantinedLineageGap) error
	ListQuarantinedLineageGaps(ctx context.Context, legalEntityID string) ([]domain.QuarantinedLineageGap, error)
	ListPostedJournalRefs(ctx context.Context, legalEntityID string) ([]domain.PostedJournalRef, error)
	GetLineageProjectionStatus(ctx context.Context, legalEntityID string) (*domain.LineageProjectionStatus, error)
	UpsertLineageProjectionStatus(ctx context.Context, legalEntityID, status string, degradedReason *string, at *time.Time) error
}

type Publisher interface {
	PublishCloseStarted(ctx context.Context, correlationID, actorID string, fp domain.FiscalPeriod)
	PublishCloseBlocked(ctx context.Context, correlationID, actorID string, fp domain.FiscalPeriod, reasons []string)
	PublishClosed(ctx context.Context, correlationID, actorID string, fp domain.FiscalPeriod, evidenceID string)
	PublishReopened(ctx context.Context, correlationID, actorID string, fp domain.FiscalPeriod, reason string)
	// PublishPeriodTransition announces a close state change that has no
	// dedicated event: period.soft_closed, period.close_review_started,
	// period.reopen_requested, period.reopen_rejected, period.reclosed.
	PublishPeriodTransition(ctx context.Context, eventType, correlationID, actorID string, fp domain.FiscalPeriod, details map[string]any)
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
	GetPostingBacklog(ctx context.Context, tenantID, principalID, legalEntityID string, cutoff time.Time) (domain.PostingBacklog, error)
	ListBankAccounts(ctx context.Context, tenantID, principalID, legalEntityID string) ([]domain.BankAccountRef, error)
	GetBankReconciliationStatus(ctx context.Context, tenantID, principalID, legalEntityID string, start, end time.Time) ([]domain.BankAccountReconStatus, error)
	// CheckARInvoiceExists/CheckAPInvoiceExists back ACC-17's own closure
	// of "Open AR included both in history and opening state" — see their
	// doc comments in internal/clients.
	CheckARInvoiceExists(ctx context.Context, tenantID, legalEntityID, customerID, invoiceNumber string) (bool, error)
	CheckAPInvoiceExists(ctx context.Context, tenantID, legalEntityID, vendorID, invoiceNumber string) (bool, error)
	UploadCloseEvidence(ctx context.Context, tenantID, legalEntityID, periodName string, trialBalance map[string]float64, principalID string) (string, error)
	// GetControlAccountCode resolves an ACC-06 caller-declared mapping key to
	// the real chart-registered account code it currently names, via GL's
	// ACC-02 mapping endpoint.
	GetControlAccountCode(ctx context.Context, tenantID, mappingKey string) (string, error)
	GetAPSubledgerTotal(ctx context.Context, tenantID, legalEntityID string) (float64, error)
	GetARSubledgerTotal(ctx context.Context, tenantID, legalEntityID string) (float64, error)
	// GetAssetNetBookValueTotal is ACC-06's third subledger source — see
	// its doc comment in internal/clients for why bookID is required.
	GetAssetNetBookValueTotal(ctx context.Context, tenantID, legalEntityID, bookID string) (float64, error)
	// GetAssetDepreciationCompleteness is ACC-06's DEPRECIATION_COMPLETENESS
	// source — see its doc comment in internal/clients.
	GetAssetDepreciationCompleteness(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (covered, eligible int, err error)
	// GetInventoryNegativeOnHandCount is ACC-06's INVENTORY_QUANTITY
	// source — see its doc comment in internal/clients.
	GetInventoryNegativeOnHandCount(ctx context.Context, tenantID, legalEntityID string) (int, error)
	// GetCloseGate is financial-control-svc's period-end close gate — see its
	// doc comment in internal/clients. Only consulted when the gate is enforced.
	GetCloseGate(ctx context.Context, tenantID, principalID, legalEntityID, periodID string) (*domain.CloseGateResponse, error)
	// GetInventoryValueTotal is ACC-06's INVENTORY_VALUE source — see its
	// doc comment in internal/clients.
	GetInventoryValueTotal(ctx context.Context, tenantID, legalEntityID string) (float64, error)
	// GetProjectPostedRevenueTotal is ACC-06's PROJECT_REVENUE source —
	// see its doc comment in internal/clients.
	GetProjectPostedRevenueTotal(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (float64, error)
	// GetInventoryUnapprovedVarianceCount is ACC-06's STOCK_COUNT source
	// — see its doc comment in internal/clients.
	GetInventoryUnapprovedVarianceCount(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (int, error)
	// PostAccrualRecognitionJournal is ACC-07's only path to the ledger —
	// see its doc comment in internal/clients for why.
	PostAccrualRecognitionJournal(ctx context.Context, tenantID, legalEntityID, fiscalPeriod, correlationID, principalID, description, debitAccountCode, creditAccountCode string, amount float64) (journalID string, err error)
	// ReverseGLJournal is ACC-07's own closure of "Auto-reversal
	// duplicates" — see its doc comment in internal/clients.
	ReverseGLJournal(ctx context.Context, tenantID, principalID, journalID, reason, correlationID string) (reversingJournalID string, err error)
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
	actionCloseConfig = "PERIOD_CLOSE_CONFIG"
	// actionCloseExclusionApprove takes a bank account out of the close's
	// reconciliation requirement. Separate from PERIOD_CLOSE_CONFIG because
	// it removes cash from a control rather than adding one: the person who
	// configures checklists is not automatically trusted to waive them.
	actionCloseExclusionApprove = "PERIOD_CLOSE_EXCLUSION_APPROVE"
	actionCloseView             = "PERIOD_CLOSE_VIEW"
	actionCloseInitiate         = "PERIOD_CLOSE_INITIATE"
	// actionPeriodReopen is deliberately its own action, not reused from
	// actionCloseInitiate — ACC-14 invariant #6 requires reopen be
	// "explicit, scoped, approved," and a locked book being reopened is a
	// materially more sensitive event than closing one on schedule. Keeping
	// it a separate action lets this be granted to a narrower, more senior
	// group than ordinary close initiation.
	actionPeriodReopen = "PERIOD_REOPEN"
	// actionCloseApprove hard-closes and recloses: the spec's
	// period.close.approve, distinct from preparing the close
	// (period.close.manage = PERIOD_CLOSE_INITIATE).
	actionCloseApprove = "PERIOD_CLOSE_APPROVE"
	// actionPeriodReopenApprove approves a reopen request someone else made
	// (period.reopen.approve). PERIOD_REOPEN is period.reopen.request.
	actionPeriodReopenApprove = "PERIOD_REOPEN_APPROVE"

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
	// actionAccrualReverse is deliberately its own action, not reused from
	// actionAccrualRecognize — reversing a posted recognition is a
	// materially more sensitive act than posting one in the first place
	// (it un-does an already-evidenced accounting fact), the same
	// reasoning actionPeriodReopen already applies to reopening a locked
	// period versus closing one.
	actionAccrualReverse = "ACCRUAL_RECOGNITION_REVERSE"

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
	actionAllocationRuleCreate    = "ALLOCATION_RULE_CREATE"
	actionAllocationRuleApprove   = "ALLOCATION_RULE_APPROVE"
	actionAllocationRuleSupersede = "ALLOCATION_RULE_SUPERSEDE"
	actionAllocationExecute       = "ALLOCATION_EXECUTE"
	actionAllocationView          = "ALLOCATION_VIEW"

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
	actionLineageView       = "LINEAGE_VIEW"
	actionLineageRebuild    = "LINEAGE_REBUILD"
	actionLineageVerifyPath = "LINEAGE_VERIFY_PATH"
	// actionLineageQuarantine is deliberately its own action, distinct
	// from actionLineageRebuild — accepting a gap as permanently known
	// (rather than fixing it) is a materially more sensitive decision
	// than an ordinary rebuild, and it changes what
	// VerifyLineageCompleteness reports from that point on.
	actionLineageQuarantine = "LINEAGE_QUARANTINE"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	clients   Clients
	// signingKey is the HMAC secret for close evidence. See signEvidence.
	signingKey []byte
	log        *zap.Logger
	// enforceCloseGate makes period close depend on financial-control-svc's
	// close gate. Off by default; see SetCloseGateEnforced.
	enforceCloseGate bool
	// subledgerGateOff disables the ACC-06 subledger-agreement close
	// blocker. Named for the exception so the zero value enforces: a handler
	// built without an explicit choice must not close on unchecked
	// subledgers. See SetSubledgerControlGateEnforced.
	subledgerGateOff bool
	// bankReconGateOff disables the bank reconciliation blocker; the zero
	// value enforces, for the same reason as subledgerGateOff.
	bankReconGateOff bool
	// bankReconCutoffDays — see config.BankReconCutoffDays. Zero means the
	// statement must be dated on the period's last day itself.
	bankReconCutoffDays int


	// REF-05 cutover phase 1 (see period_mirror.go). All nil/empty by default,
	// which means: no mirroring, and the workflow-ref endpoint refuses everyone.
	mirror             PeriodMirror
	workflowRefs       WorkflowRefReader
	workflowRefCallers map[string]struct{}
}

// SetCloseGateEnforced turns the financial-control-svc close-gate dependency
// on or off. Off (the zero value) never calls the service and adds no issue.
func (h *Handler) SetCloseGateEnforced(enforce bool) *Handler {
	h.enforceCloseGate = enforce
	return h
}

// SetSubledgerControlGateEnforced turns the ACC-06 subledger-agreement
// blocker on (the default) or off. Off exists only for an environment whose
// AR/AP control-account mappings are not configured yet, where no control run
// could ever pass; it is reported loudly at startup.
func (h *Handler) SetSubledgerControlGateEnforced(enforce bool) *Handler {
	h.subledgerGateOff = !enforce
	return h
}

// SetBankReconciliationGate turns the bank reconciliation blocker on (the
// default) or off and sets the statement cut-off in days before period end.
func (h *Handler) SetBankReconciliationGate(enforce bool, cutoffDays int) *Handler {
	h.bankReconGateOff = !enforce
	h.bankReconCutoffDays = cutoffDays
	return h
}

// defaultBankReconCutoffDays matches config's default for a handler built
// without SetBankReconciliationGate (tests, and any future constructor).
const defaultBankReconCutoffDays = 4

func New(store Store, publisher Publisher, authz AuthZClient, clients Clients, signingKey []byte, log *zap.Logger) *Handler {
	return &Handler{
		store:               store,
		publisher:           publisher,
		authz:               authz,
		clients:             clients,
		signingKey:          signingKey,
		log:                 log,
		bankReconCutoffDays: defaultBankReconCutoffDays,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/close/reopen-requests", func(r chi.Router) {
		r.Post("/{request_id}/approve", h.ApproveReopen)
		r.Post("/{request_id}/reject", h.RejectReopen)
	})
	r.Route("/v1/close/requirements", func(r chi.Router) {
		r.Get("/", h.GetCloseChecklist)
		r.Post("/", h.AddCloseRequirement)
		r.Post("/{id}/remove", h.RemoveCloseRequirement)
	})
	r.Get("/v1/control-populations/migration-batch-tieout", h.GetMigrationBatchTieoutPopulation)
	r.Get("/v1/close/workflow-refs/{ref}", h.GetWorkflowRef)
	r.Get("/v1/close/workflow-refs/{ref}", h.GetWorkflowRef)
	r.Route("/v1/close/periods", func(r chi.Router) {
		r.Post("/", h.CreateFiscalPeriod)
		r.Get("/", h.ListFiscalPeriods)
		r.Get("/status", h.GetPeriodStatus)
		r.Get("/{id}/readiness", h.GetPeriodReadiness)
		r.Get("/{id}/evidence", h.GetPeriodEvidence)
		r.Post("/{id}/soft-close", h.StartSoftClose)
		r.Post("/{id}/close-review", h.EnterCloseReview)
		r.Post("/{id}/hard-close", h.HardClosePeriod)
		// /lock predates the state machine; it is hard close and, like it,
		// requires CLOSE_REVIEW.
		r.Post("/{id}/lock", h.HardClosePeriod)
		r.Post("/{id}/reclose", h.ReclosePeriod)
		r.Post("/{id}/reopen-requests", h.RequestReopen)
		r.Post("/{id}/reopen", h.RetiredReopen)
		r.Get("/{id}/history", h.GetCloseHistory)
		r.Get("/{id}/available-actions", h.GetAvailableCloseActions)
		r.Post("/{id}:mirror-to-period-service", h.MirrorPeriodToPeriodService)
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
		r.Post("/{id}/recognitions/{fiscal_period}/reverse", h.ReverseAccrualRecognition)
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
		r.Post("/{id}/supersede", h.SupersedeAllocationRule)
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
		r.Get("/journals/{id}/source/as-of", h.GetLineageAsOf)
		r.Get("/verify", h.VerifyLineageCompleteness)
		r.Get("/status", h.GetLineageProjectionStatusHandler)
		r.Post("/rebuild", h.RebuildLineageProjection)
		r.Post("/verify-path", h.VerifyTracePath)
		r.Post("/quarantine", h.QuarantineBrokenLineage)
	})
}

// ── Close checklist (ACC-14) ───────────────────────────────────────────────────

// closeRequirementAction is the permission a requirement needs: adding a
// control is configuration, waiving a bank account is an approval.
func closeRequirementAction(kind string) string {
	if kind == domain.CloseRequirementBankAccountExclusion {
		return actionCloseExclusionApprove
	}
	return actionCloseConfig
}

// validateCloseRequirement checks a request's shape before it reaches the
// database (whose CHECK constraint enforces the same rules), so a caller gets
// a message naming the problem rather than a constraint name.
func validateCloseRequirement(req *domain.CloseRequirementRequest) (code, detail string) {
	req.Subledger = strings.ToUpper(strings.TrimSpace(req.Subledger))
	req.BookID = strings.TrimSpace(req.BookID)
	req.BankAccountID = strings.TrimSpace(req.BankAccountID)
	req.Reason = strings.TrimSpace(req.Reason)
	if strings.TrimSpace(req.LegalEntityID) == "" {
		return "missing_fields", "legal_entity_id is required"
	}
	switch req.Kind {
	case domain.CloseRequirementSubledgerControl:
		if req.BankAccountID != "" {
			return "invalid_requirement", "bank_account_id does not apply to a SUBLEDGER_CONTROL requirement"
		}
		for _, base := range domain.BaselineSubledgerControls {
			if req.Subledger == base {
				return "baseline_requirement", req.Subledger + " is required for every entity already and cannot be added or removed"
			}
		}
		known := false
		for _, opt := range domain.OptionalSubledgerControls {
			known = known || req.Subledger == opt
		}
		if !known {
			return "invalid_subledger", "subledger must be one of " + strings.Join(domain.OptionalSubledgerControls, ", ")
		}
		if req.Subledger == "ASSETS" && req.BookID == "" {
			return "missing_fields", "book_id is required for an ASSETS requirement: the control reconciles one asset book"
		}
		if req.Subledger != "ASSETS" && req.BookID != "" {
			return "invalid_requirement", "book_id applies only to ASSETS"
		}
	case domain.CloseRequirementBankAccountExclusion:
		if req.Subledger != "" || req.BookID != "" {
			return "invalid_requirement", "subledger and book_id do not apply to a BANK_ACCOUNT_EXCLUSION"
		}
		if req.BankAccountID == "" {
			return "missing_fields", "bank_account_id is required"
		}
		if req.Reason == "" {
			return "missing_fields", "reason is required: an excluded account is a waived control and must say why it is immaterial"
		}
	default:
		return "invalid_kind", "kind must be SUBLEDGER_CONTROL or BANK_ACCOUNT_EXCLUSION"
	}
	return "", ""
}

// ── GET /v1/close/requirements?legal_entity_id= ─────────────────────────────

func (h *Handler) GetCloseChecklist(w http.ResponseWriter, r *http.Request) {
	legalEntityID := strings.TrimSpace(r.URL.Query().Get("legal_entity_id"))
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionCloseView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	reqs, err := h.store.ListCloseRequirements(r.Context(), legalEntityID)
	if err != nil {
		h.writeStoreErr(w, err, "requirement_not_found")
		return
	}
	writeJSON(w, http.StatusOK, buildCloseChecklist(legalEntityID, reqs))
}

func buildCloseChecklist(legalEntityID string, reqs []domain.CloseRequirement) domain.CloseChecklist {
	out := domain.CloseChecklist{
		LegalEntityID:             legalEntityID,
		BaselineSubledgerControls: append([]string{}, domain.BaselineSubledgerControls...),
		RequiredSubledgerControls: []domain.CloseRequirement{},
		ExcludedBankAccounts:      []domain.CloseRequirement{},
	}
	for _, cr := range reqs {
		if cr.Kind == domain.CloseRequirementBankAccountExclusion {
			out.ExcludedBankAccounts = append(out.ExcludedBankAccounts, cr)
		} else {
			out.RequiredSubledgerControls = append(out.RequiredSubledgerControls, cr)
		}
	}
	return out
}

// ── POST /v1/close/requirements ─────────────────────────────────────────────

func (h *Handler) AddCloseRequirement(w http.ResponseWriter, r *http.Request) {
	var req domain.CloseRequirementRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if code, detail := validateCloseRequirement(&req); code != "" {
		writeError(w, http.StatusBadRequest, code, detail)
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, closeRequirementAction(req.Kind)); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if req.Kind == domain.CloseRequirementBankAccountExclusion {
		accounts, err := h.clients.ListBankAccounts(r.Context(), tenantID, principalID, req.LegalEntityID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "treasury_unavailable",
				"cannot confirm the bank account belongs to this entity: "+err.Error())
			return
		}
		known := false
		for _, a := range accounts {
			known = known || a.BankAccountID == req.BankAccountID
		}
		if !known {
			// An exclusion for an account that is not the entity's would
			// waive nothing today and silently waive whatever later reuses
			// the id; refuse it.
			writeError(w, http.StatusUnprocessableEntity, "unknown_bank_account", string(domain.ErrUnknownBankAccount))
			return
		}
	}
	cr := &domain.CloseRequirement{
		RequirementID: uuid.NewString(), TenantID: tenantID, LegalEntityID: req.LegalEntityID,
		Kind: req.Kind, Subledger: req.Subledger, BookID: req.BookID, BankAccountID: req.BankAccountID,
		Reason: req.Reason, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	created, err := h.store.CreateCloseRequirement(r.Context(), cr)
	if err != nil {
		h.writeStoreErr(w, err, "requirement_not_found")
		return
	}
	if !created {
		// Already on the checklist: a replay, answered with the existing item.
		writeJSON(w, http.StatusOK, cr)
		return
	}
	writeJSON(w, http.StatusCreated, cr)
}

// ── POST /v1/close/requirements/{id}/remove ─────────────────────────────────

func (h *Handler) RemoveCloseRequirement(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.RemoveCloseRequirementRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	existing, err := h.store.GetCloseRequirement(r.Context(), id)
	if errors.Is(err, domain.ErrCloseRequirementNotFound) {
		writeError(w, http.StatusNotFound, "requirement_not_found", "")
		return
	}
	if err != nil {
		h.writeStoreErr(w, err, "requirement_not_found")
		return
	}
	// Same permission as adding it: un-waiving an account restores a control
	// but is still a change to the waiver, and dropping a required control
	// weakens the close just as adding one strengthens it.
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, closeRequirementAction(existing.Kind)); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	removed, err := h.store.RemoveCloseRequirement(r.Context(), id, principalID, strings.TrimSpace(req.Reason), time.Now().UTC())
	if errors.Is(err, domain.ErrCloseRequirementNotFound) {
		writeError(w, http.StatusConflict, "requirement_already_removed", "")
		return
	}
	if err != nil {
		h.writeStoreErr(w, err, "requirement_not_found")
		return
	}
	writeJSON(w, http.StatusOK, removed)
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
		writeJSON(w, http.StatusOK, domain.PeriodStatusView{
			PeriodName: periodName, CloseStatus: "OPEN", PeriodState: domain.PeriodOpen, PostingPolicy: domain.PostingOpen,
		})
		return
	}
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}

	now := time.Now().UTC()
	writeJSON(w, http.StatusOK, domain.PeriodStatusView{
		FiscalPeriodID:  fp.FiscalPeriodID,
		PeriodName:      fp.PeriodName,
		CloseStatus:     fp.LegacyCloseStatus(now),
		PeriodState:     fp.CloseStatus,
		PostingPolicy:   fp.PostingPolicy(now),
		ReopenExpiresAt: fp.ReopenExpiresAt,
	})
}

// ── POST /v1/close/periods/{id}/lock ──────────────────────────────────────────────

// closeMode is the one difference between a hard close and a reclose.
type closeMode struct {
	from, to string
	// reperform: every control run and bank reconciliation the close relies
	// on must postdate the reopen (negative path #4, "Reclose without
	// reperformance") — the evidence of the original close does not count.
	reperform bool
}

var (
	hardCloseMode = closeMode{from: domain.PeriodCloseReview, to: domain.PeriodHardClosed}
	recloseMode   = closeMode{from: domain.PeriodAuthorizedReopen, to: domain.PeriodReclosed, reperform: true}
)

// HardClosePeriod — POST /v1/close/periods/{id}/hard-close (and /lock):
// CLOSE_REVIEW → HARD_CLOSED once every close gate passes, with signed
// evidence. PERIOD_CLOSE_APPROVE.
func (h *Handler) HardClosePeriod(w http.ResponseWriter, r *http.Request) {
	h.closeWithEvidence(w, r, hardCloseMode)
}

// ReclosePeriod — POST /v1/close/periods/{id}/reclose: AUTHORIZED_REOPEN →
// RECLOSED, with new evidence built only from controls and reconciliations
// performed after the reopen. PERIOD_CLOSE_APPROVE.
func (h *Handler) ReclosePeriod(w http.ResponseWriter, r *http.Request) {
	h.closeWithEvidence(w, r, recloseMode)
}

func (h *Handler) closeWithEvidence(w http.ResponseWriter, r *http.Request, mode closeMode) {
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionCloseApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if fp.CloseStatus != mode.from {
		if fp.CloseStatus == mode.to {
			writeError(w, http.StatusUnprocessableEntity, "period_already_locked", string(domain.ErrPeriodAlreadyLocked))
			return
		}
		writeError(w, http.StatusConflict, "invalid_period_transition",
			fmt.Sprintf("%s; the period is %s and this command needs %s", domain.ErrInvalidPeriodTransition, fp.CloseStatus, mode.from))
		return
	}

	h.publisher.PublishCloseStarted(r.Context(), correlationID, principalID, *fp)

	var notBefore *time.Time
	if mode.reperform {
		notBefore = fp.ReopenedAt
	}

	// Step 1: Run Readiness Checks (FAIL CLOSED on any dependency query error)
	blockingIssues, reliance, err := h.checkReadiness(r.Context(), tenantID, principalID, fp, notBefore)
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

	// Update DB record lock state. The transition re-checks the state under a
	// row lock, so a concurrent transition between the check above and here
	// is refused rather than overwritten.
	lockedAt, evidenceDoc := now, docID
	if current, err := h.store.ApplyPeriodTransition(r.Context(), id, []string{mode.from}, domain.PeriodUpdate{
		To: mode.to, PrincipalID: principalID, At: now, LockedAt: &lockedAt, EvidenceDocID: &evidenceDoc,
		ClearReopen: mode.reperform,
	}); err != nil {
		if errors.Is(err, domain.ErrInvalidPeriodTransition) && current != nil && current.CloseStatus != mode.to {
			// Moved by someone else between the check above and now: refused,
			// not reported as this command's success.
			h.writeTransitionErr(w, err, current, mode.from)
			return
		}
		if errors.Is(err, domain.ErrInvalidPeriodTransition) && current != nil {
			// Replay of a prior request that already succeeded (e.g. a client
			// timeout on a close call that actually completed server-side) —
			// return the current closed state rather than misreporting this
			// as a store outage.
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
	// What the close relied on, pinned to it and signed (ZS-CONTROL-001 §22).
	manifest, err := json.Marshal(reliance)
	if err != nil {
		h.log.Error("period locked but the reliance manifest could not be rendered", zap.String("period_id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "evidence_not_recorded",
			string(domain.ErrEvidenceNotRecorded)+" — the period IS locked, but what it relied on could not be recorded. Do not treat this close as evidenced.")
		return
	}
	manifestHash := sha256.Sum256(manifest)
	evidence.RelianceManifest = string(manifest)
	evidence.RelianceHash = hex.EncodeToString(manifestHash[:])
	evidence.RelianceSignature = h.signEvidence(manifestHash[:])
	if err := h.store.CreateCloseEvidence(r.Context(), evidence); err != nil {
		h.log.Error("period locked but close evidence could not be recorded",
			zap.String("period_id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "evidence_not_recorded",
			string(domain.ErrEvidenceNotRecorded)+
				" — the period IS locked and the trial balance document was uploaded to the vault ("+docID+
				"), but the signed hash was not persisted. Do not treat this close as evidenced.")
		return
	}

	if mode.reperform {
		h.publisher.PublishPeriodTransition(r.Context(), "period.reclosed", correlationID, principalID, *fp,
			map[string]any{"evidence_document_id": docID})
	} else {
		h.publisher.PublishClosed(r.Context(), correlationID, principalID, *fp, docID)
	}

	// REF-05 phase 1: best-effort mirror. Runs only when PERIOD_SERVICE_MIRROR=on,
	// is bounded by a short deadline, and can neither fail nor change this response.
	h.mirrorLock(r.Context(), tenantID, correlationID, principalID, fp, docID, blockingIssues)

	writeJSON(w, http.StatusOK, domain.PeriodLockResponse{
		FiscalPeriodID:     id,
		PeriodName:         fp.PeriodName,
		CloseStatus:        mode.to,
		CloseLockedAt:      now,
		EvidenceDocumentID: docID,
		VerificationHash:   trialBalanceHash,
	})
}

// RetiredReopen — POST /v1/close/periods/{id}/reopen. This used to reopen a
// locked period in one step, for one person, permanently: the spec's
// negative path #3, "Reopen without approval". A closed period now reopens
// by request and independent approval, for a bounded time. 410 so a caller
// learns where to go rather than retrying.
func (h *Handler) RetiredReopen(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusGone, "reopen_requires_approval", string(domain.ErrReopenRetired))
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
	if req.LegalEntityID == "" || req.FiscalPeriod == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and fiscal_period are required")
		return
	}
	if req.Subledger != "AP" && req.Subledger != "AR" && req.Subledger != "ASSETS" && req.Subledger != "DEPRECIATION_COMPLETENESS" && req.Subledger != "INVENTORY_QUANTITY" && req.Subledger != "INVENTORY_VALUE" && req.Subledger != "PROJECT_REVENUE" && req.Subledger != "STOCK_COUNT" {
		writeError(w, http.StatusBadRequest, "invalid_subledger", string(domain.ErrInvalidSubledger))
		return
	}
	if req.Subledger == "ASSETS" && req.BookID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", string(domain.ErrBookIDRequiredForAssets))
		return
	}
	// DEPRECIATION_COMPLETENESS and INVENTORY_QUANTITY are integrity
	// checks, not GL balance comparisons (see writeup below) — neither
	// ever resolves a control account or compiles a trial balance, so no
	// mapping key applies to either.
	isCompletenessType := req.Subledger == "DEPRECIATION_COMPLETENESS" || req.Subledger == "INVENTORY_QUANTITY" || req.Subledger == "STOCK_COUNT"
	if !isCompletenessType && req.ControlAccountMappingKey == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "control_account_mapping_key is required")
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

	// DEPRECIATION_COMPLETENESS repurposes this run's own generic
	// "actual total vs expected total, MATCHED/EXCEPTION" shape for a
	// coverage check rather than a GL balance tie-out — the AST/INV/PRJ
	// domain spec's own §9 "Depreciation completeness" assertion has no
	// GL side at all (it is entirely subledger-internal: did every
	// eligible schedule actually get depreciated this period?).
	// subledger_total_amount holds the real, live COVERED count;
	// gl_control_balance_amount holds the real, live ELIGIBLE count —
	// neither is a GL balance, and control_account_code is a fixed
	// sentinel rather than a resolved account, stated honestly rather
	// than forcing a GL call this assertion has no use for.
	if req.Subledger == "DEPRECIATION_COMPLETENESS" {
		covered, eligible, err := h.clients.GetAssetDepreciationCompleteness(r.Context(), tenantID, req.LegalEntityID, req.FiscalPeriod)
		if err != nil {
			h.writeSubledgerControlErr(w, err, "asset-management-svc")
			return
		}
		h.recordAndRespondControlRun(w, r, correlationID, principalID, tenantID, req, "N/A (completeness check — no GL account)", float64(covered), float64(eligible))
		return
	}

	// INVENTORY_QUANTITY is the AST/INV/PRJ domain spec's own §9
	// "Inventory quantity" assertion: an integrity check (is any
	// item/location's own derived on-hand negative?), not a balance
	// comparison — same non-GL shape as DEPRECIATION_COMPLETENESS above.
	// subledger_total_amount holds the real, live count of negative
	// on-hand combinations found (the ACTUAL violation count);
	// gl_control_balance_amount is always 0 (the EXPECTED violation
	// count — this invariant should never be violated at all). A
	// non-zero actual count is therefore always an EXCEPTION by
	// construction, the same MATCHED/EXCEPTION threshold logic every
	// other subledger type already uses.
	if req.Subledger == "INVENTORY_QUANTITY" {
		negativeCount, err := h.clients.GetInventoryNegativeOnHandCount(r.Context(), tenantID, req.LegalEntityID)
		if err != nil {
			h.writeSubledgerControlErr(w, err, "inventory-management-svc")
			return
		}
		h.recordAndRespondControlRun(w, r, correlationID, principalID, tenantID, req, "N/A (integrity check — no GL account)", float64(negativeCount), 0)
		return
	}

	// STOCK_COUNT is the AST/INV/PRJ domain spec's own §9 "Stock count"
	// assertion — an integrity check (does any counted line carry a
	// real, observed variance that was never approved?), same non-GL
	// shape as INVENTORY_QUANTITY/DEPRECIATION_COMPLETENESS.
	if req.Subledger == "STOCK_COUNT" {
		unapprovedCount, err := h.clients.GetInventoryUnapprovedVarianceCount(r.Context(), tenantID, req.LegalEntityID, req.FiscalPeriod)
		if err != nil {
			h.writeSubledgerControlErr(w, err, "inventory-management-svc")
			return
		}
		h.recordAndRespondControlRun(w, r, correlationID, principalID, tenantID, req, "N/A (integrity check — no GL account)", float64(unapprovedCount), 0)
		return
	}

	controlAccountCode, err := h.clients.GetControlAccountCode(r.Context(), tenantID, req.ControlAccountMappingKey)
	if err != nil {
		h.writeSubledgerControlErr(w, err, "general-ledger-svc")
		return
	}

	var subledgerTotal float64
	switch req.Subledger {
	case "AP":
		subledgerTotal, err = h.clients.GetAPSubledgerTotal(r.Context(), tenantID, req.LegalEntityID)
		if err != nil {
			h.writeSubledgerControlErr(w, err, "accounts-payable-svc")
			return
		}
	case "AR":
		subledgerTotal, err = h.clients.GetARSubledgerTotal(r.Context(), tenantID, req.LegalEntityID)
		if err != nil {
			h.writeSubledgerControlErr(w, err, "accounts-receivable-svc")
			return
		}
	case "ASSETS":
		subledgerTotal, err = h.clients.GetAssetNetBookValueTotal(r.Context(), tenantID, req.LegalEntityID, req.BookID)
		if err != nil {
			h.writeSubledgerControlErr(w, err, "asset-management-svc")
			return
		}
	case "INVENTORY_VALUE":
		subledgerTotal, err = h.clients.GetInventoryValueTotal(r.Context(), tenantID, req.LegalEntityID)
		if err != nil {
			h.writeSubledgerControlErr(w, err, "inventory-management-svc")
			return
		}
	case "PROJECT_REVENUE":
		subledgerTotal, err = h.clients.GetProjectPostedRevenueTotal(r.Context(), tenantID, req.LegalEntityID, req.FiscalPeriod)
		if err != nil {
			h.writeSubledgerControlErr(w, err, "project-accounting-svc")
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

	h.recordAndRespondControlRun(w, r, correlationID, principalID, tenantID, req, controlAccountCode, subledgerTotal, glBalance)
}

// recordAndRespondControlRun is the shared tail every subledger type
// reaches: diff, threshold, persist (append-only evidence), publish an
// exception if one occurred, respond. Factored out once
// DEPRECIATION_COMPLETENESS needed to reach this same tail without the
// GL-comparison steps above it.
func (h *Handler) recordAndRespondControlRun(w http.ResponseWriter, r *http.Request, correlationID, principalID, tenantID string, req domain.RunSubledgerControlRequest, controlAccountCode string, subledgerTotal, glBalance float64) {
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
		BookID:                 assetBook(req),
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
	// Accrual recognition is a designated close journal: allowed through soft
	// close and close review, refused once hard closed.
	if fp != nil && !fp.CloseJournalsAllowed(time.Now().UTC()) {
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

// ReverseAccrualRecognition closes the spec's own "Auto-reversal
// duplicates" negative path: reverses one already-recognized period's
// journal via general-ledger-svc's real ReverseJournal, and is idempotent
// — a retried or replayed reverse call for the same recognition instance
// returns the SAME reversal, never a second reversing journal.
//
// Checked in this order deliberately: the existing-reversal lookup runs
// BEFORE calling general-ledger-svc, so a replay never even issues a
// second GL call; CreateRecognitionReversal's own UNIQUE constraint (see
// migration 000012) is the second, database-enforced line of defense
// against a race between two concurrent reverse requests for the same
// instance.
func (h *Handler) ReverseAccrualRecognition(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	fiscalPeriod := chi.URLParam(r, "fiscal_period")

	var req domain.ReverseAccrualRecognitionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrReversalReasonRequired.Error())
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, sch.LegalEntityID, actionAccrualReverse); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	inst, err := h.store.GetRecognitionInstanceByPeriod(r.Context(), id, fiscalPeriod)
	if errors.Is(err, domain.ErrRecognitionInstanceNotFound) {
		writeError(w, http.StatusNotFound, "recognition_instance_not_found", err.Error())
		return
	}
	if err != nil {
		h.log.Error("ReverseAccrualRecognition: failed to fetch recognition instance", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// Idempotency: return the existing reversal rather than calling
	// general-ledger-svc a second time.
	if existing, err := h.store.GetRecognitionReversalByInstance(r.Context(), inst.RecognitionInstanceID); err != nil {
		h.log.Error("ReverseAccrualRecognition: failed to check for an existing reversal", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	} else if existing != nil {
		writeJSON(w, http.StatusOK, existing)
		return
	}

	// Distinct from the recognition's own correlation_id (schedule_id +
	// fiscal_period) — reversing is a different idempotent operation from
	// recognizing, and general-ledger-svc's ReverseJournal keys its own
	// replay detection on whatever correlation_id this call sends.
	reverseCorrelationID := "reverse:" + inst.RecognitionInstanceID
	reversingJournalID, err := h.clients.ReverseGLJournal(r.Context(), tenantID, principalID, inst.JournalID, req.Reason, reverseCorrelationID)
	if err != nil {
		h.log.Error("ReverseAccrualRecognition: journal reversal failed", zap.String("schedule_id", id), zap.String("journal_id", inst.JournalID), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "journal_reversal_failed", err.Error())
		return
	}

	rev := &domain.RecognitionReversal{
		RecognitionReversalID: uuid.NewString(),
		ScheduleID:            id,
		RecognitionInstanceID: inst.RecognitionInstanceID,
		ReversingJournalID:    reversingJournalID,
		Reason:                req.Reason,
		ReversedAt:            time.Now().UTC(),
		ReversedByPrincipalID: principalID,
	}
	created, err := h.store.CreateRecognitionReversal(r.Context(), rev)
	if err != nil {
		h.log.Error("accrual recognition reversed on the ledger but the evidence row could not be recorded",
			zap.String("schedule_id", id), zap.String("reversing_journal_id", reversingJournalID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "reversal_not_recorded",
			"the reversing journal IS posted to general-ledger-svc ("+reversingJournalID+"), but its evidence row was not persisted.")
		return
	}

	h.recordLineageEdge(r.Context(), sch.LegalEntityID, "accrual_recognition_reversal", rev.RecognitionReversalID, "journal", reversingJournalID)

	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, rev)
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
	if fp != nil && !fp.CloseJournalsAllowed(time.Now().UTC()) {
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
			if fp != nil && !fp.CloseJournalsAllowed(time.Now().UTC()) {
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

// SupersedeAllocationRule is ACC-09's own SupersedeAllocationRule command
// — closes the gap left by CreateAllocationRule always minting a brand
// new, unrelated rule_id: without this, nothing stopped two concurrently
// ACTIVE rules from existing for the same source_account_code, each
// eligible to post its own allocation run against the same source balance.
func (h *Handler) SupersedeAllocationRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.SupersedeAllocationRuleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" || req.SourceAccountCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "name and source_account_code are required")
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
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	current, err := h.store.GetCurrentAllocationRule(r.Context(), id)
	if err != nil {
		h.writeAllocationErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, current.LegalEntityID, actionAllocationRuleSupersede); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	now := time.Now().UTC()
	newVersion := &domain.AllocationRule{
		RuleVersionID:        uuid.NewString(),
		LegalEntityID:        current.LegalEntityID,
		Name:                 req.Name,
		SourceAccountCode:    req.SourceAccountCode,
		Drivers:              req.Drivers,
		CreatedAt:            now,
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.SupersedeAllocationRule(r.Context(), id, newVersion, now); err != nil {
		if errors.Is(err, domain.ErrNoCurrentRuleToSupersede) {
			writeError(w, http.StatusUnprocessableEntity, "no_current_rule_to_supersede", err.Error())
			return
		}
		h.log.Error("failed to supersede allocation rule", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	newVersion.RuleID = id
	newVersion.TenantID = current.TenantID
	newVersion.Status = domain.AllocationRuleStatusDraft
	writeJSON(w, http.StatusCreated, newVersion)
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
			EntryID:             uuid.NewString(),
			SourceReferenceID:   e.SourceReferenceID,
			SourceAccountCode:   e.SourceAccountCode,
			TargetAccountCode:   e.TargetAccountCode,
			DebitAmount:         e.DebitAmount,
			CreditAmount:        e.CreditAmount,
			SourceReferenceType: e.SourceReferenceType,
			PartyID:             e.PartyID,
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

		// The spec's own negative path, "Open AR included both in history
		// and opening state" — only checked for entries the caller flagged
		// as a real open AR/AP item; an ordinary GL balance line has no
		// subledger history to double-book against.
		if e.SourceReferenceType != nil {
			if e.PartyID == nil || *e.PartyID == "" {
				h.quarantineMigrationBatch(r.Context(), id, string(domain.ErrPartyIDRequiredForOpenItem)+": "+e.SourceReferenceID)
				writeError(w, http.StatusUnprocessableEntity, "party_id_required", string(domain.ErrPartyIDRequiredForOpenItem)+": "+e.SourceReferenceID)
				return
			}
			var exists bool
			var checkErr error
			switch *e.SourceReferenceType {
			case domain.MigrationCrosswalkTypeAROpenItem:
				exists, checkErr = h.clients.CheckARInvoiceExists(r.Context(), tenantID, batch.LegalEntityID, *e.PartyID, e.SourceReferenceID)
			case domain.MigrationCrosswalkTypeAPOpenItem:
				exists, checkErr = h.clients.CheckAPInvoiceExists(r.Context(), tenantID, batch.LegalEntityID, *e.PartyID, e.SourceReferenceID)
			}
			if checkErr != nil {
				h.log.Error("ValidateOpeningBalances: failed to check open item against subledger history", zap.Error(checkErr))
				writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", checkErr.Error())
				return
			}
			if exists {
				h.quarantineMigrationBatch(r.Context(), id, string(domain.ErrOpenItemAlreadyExistsInHistory)+": "+e.SourceReferenceID)
				writeError(w, http.StatusUnprocessableEntity, "open_item_already_exists_in_history", string(domain.ErrOpenItemAlreadyExistsInHistory)+": "+e.SourceReferenceID)
				return
			}
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
	// Opening balances are ordinary postings, not close journals: only into an
	// open period or a reopen window.
	if fp != nil && !fp.OrdinaryPostingAllowed(time.Now().UTC()) {
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
	quarantined, err := h.store.ListQuarantinedLineageGaps(ctx, legalEntityID)
	if err != nil {
		return nil, err
	}
	isQuarantined := make(map[string]bool, len(quarantined))
	for _, q := range quarantined {
		isQuarantined[q.FromType+"|"+q.FromID+"|"+q.ToType+"|"+q.ToID] = true
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
		if found {
			continue
		}
		// A quarantined gap was deliberately accepted as known — it stays
		// out of Gaps but is still counted, never silently vanished.
		if isQuarantined[ref.FromType+"|"+ref.FromID+"|journal|"+ref.JournalID] {
			report.QuarantinedCount++
			continue
		}
		report.Gaps = append(report.Gaps, ref)
	}
	report.Complete = len(report.Gaps) == 0
	return report, nil
}

// GetLineageAsOf answers ACC-18's own GetLineageAsOf query — a
// point-in-time reconstruction of TraceJournalToSource, since
// lineage_edges is append-only and every edge's own recorded_at is a
// stable position that never changes retroactively.
func (h *Handler) GetLineageAsOf(w http.ResponseWriter, r *http.Request) {
	journalID := chi.URLParam(r, "id")
	asOfRaw := r.URL.Query().Get("as_of")
	if asOfRaw == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrLineageAsOfRequired.Error())
		return
	}
	asOf, err := time.Parse(time.RFC3339, asOfRaw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_field", domain.ErrLineageAsOfRequired.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	edges, err := h.store.ListLineageEdgesToAsOf(r.Context(), "journal", journalID, asOf)
	if err != nil {
		h.log.Error("GetLineageAsOf: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if len(edges) == 0 {
		writeJSON(w, http.StatusOK, []domain.LineageEdge{})
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, edges[0].LegalEntityID, actionLineageView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, edges)
}

// VerifyTracePath answers ACC-18's own VerifyTracePath command — checks
// whether ONE specific edge is recorded, and permanently records the
// verification result (the spec's own "verification results" ownership).
// Unlike VerifyLineageCompleteness (which scans every posted journal for
// a legal entity), this verifies a single, caller-named path — the
// narrower, targeted check the wireframe names as its own command.
func (h *Handler) VerifyTracePath(w http.ResponseWriter, r *http.Request) {
	var req domain.VerifyTracePathRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.FromType == "" || req.FromID == "" || req.ToID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, from_type, from_id and to_id are required")
		return
	}
	toType := req.ToType
	if toType == "" {
		toType = "journal"
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionLineageVerifyPath); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	edges, err := h.store.ListLineageEdgesTo(r.Context(), toType, req.ToID)
	if err != nil {
		h.log.Error("VerifyTracePath: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	verified := false
	for _, e := range edges {
		if e.FromType == req.FromType && e.FromID == req.FromID {
			verified = true
			break
		}
	}

	result := &domain.TracePathVerification{
		VerificationID: uuid.NewString(), LegalEntityID: req.LegalEntityID,
		FromType: req.FromType, FromID: req.FromID, ToType: toType, ToID: req.ToID,
		Verified: verified, VerifiedAt: time.Now().UTC(), VerifiedByPrincipalID: principalID,
	}
	if err := h.store.CreateTracePathVerification(r.Context(), result); err != nil {
		h.log.Error("VerifyTracePath: failed to record verification result", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// QuarantineBrokenLineage answers ACC-18's own QuarantineBrokenLineage
// command — the ONLY way a gap stops appearing in
// VerifyLineageCompleteness's own Gaps list, so it always requires a
// reason: a deliberate, evidenced decision, never a silent exclusion.
func (h *Handler) QuarantineBrokenLineage(w http.ResponseWriter, r *http.Request) {
	var req domain.QuarantineBrokenLineageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.FromType == "" || req.FromID == "" || req.ToID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, from_type, from_id and to_id are required")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrLineageQuarantineReasonRequired.Error())
		return
	}
	toType := req.ToType
	if toType == "" {
		toType = "journal"
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionLineageQuarantine); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	gap := &domain.QuarantinedLineageGap{
		QuarantineID: uuid.NewString(), LegalEntityID: req.LegalEntityID,
		FromType: req.FromType, FromID: req.FromID, ToType: toType, ToID: req.ToID,
		Reason: req.Reason, QuarantinedAt: time.Now().UTC(), QuarantinedByPrincipalID: principalID,
	}
	if err := h.store.CreateQuarantinedLineageGap(r.Context(), gap); err != nil {
		h.log.Error("QuarantineBrokenLineage: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, gap)
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
	if fp.CloseStatus == domain.PeriodHardClosed || fp.CloseStatus == domain.PeriodReclosed {
		writeJSON(w, http.StatusOK, domain.ReadinessCheckResponse{
			IsReady:        false,
			BlockingIssues: []string{"period_already_locked: this period is " + fp.CloseStatus + " and cannot be closed again"},
		})
		return
	}

	var notBefore *time.Time
	if fp.CloseStatus == domain.PeriodAuthorizedReopen {
		notBefore = fp.ReopenedAt
	}
	blockingIssues, _, err := h.checkReadiness(r.Context(), tenantID, principalID, fp, notBefore)
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

// checkReadiness runs the close blockers for a period.
//
// WHAT BLOCKS A CLOSE. ACC-14 (ZS-SVC-B-001 §17) lists the close's inputs as
// "subledger controls; bank recon; tax/payroll/assets/inventory status;
// unresolved exceptions; approvals; posting backlog", and ACC-06 adds that
// "unresolved material exceptions block certification/close". Every check
// below asks whether the books for the period are complete and agree:
//
//   - unposted journals: manual journals still in draft for the period;
//   - posting backlog: accounting events GL accepted before period end but
//     never committed (catches a subledger fact GL took in and failed on);
//   - subledger agreement (ACC-06): for AR, AP and every control the entity's
//     close checklist adds (assets per book, depreciation, inventory,
//     projects, stock count), the latest run for the period exists and
//     MATCHED — e.g. each subledger's total equals its GL control account,
//     which catches a fact that never reached GL at all;
//   - bank reconciliation (BNK-05): every operational bank account of the
//     entity not excluded on its checklist has a certified reconciliation
//     for a statement dated in the period, within the cut-off of period end;
//   - financial controls, when that gate is enforced.
//
// What does NOT block, deliberately: an invoice or bill that is simply unpaid.
// The close used to refuse any period with a receivable not yet PAID or a
// payable not yet PAYMENT_REQUESTED. An open receivable at month end is a
// correct balance, so that rule stopped ordinary months from closing, while a
// fact whose accounting never reached the ledger passed straight through.
//
// Fails closed: a dependency that cannot be queried returns an error, never an
// empty issue list. "We could not check" and "there is nothing to report" are
// opposite answers, and conflating them would close a period on the strength of
// a service being down.
func (h *Handler) checkReadiness(ctx context.Context, tenantID, principalID string, fp *domain.FiscalPeriod, notBefore *time.Time) ([]string, *domain.CloseReliance, error) {
	var issues []string
	reliance := &domain.CloseReliance{
		SubledgerControlGate:         gateMode(!h.subledgerGateOff),
		BankReconciliationGate:       gateMode(!h.bankReconGateOff),
		BankReconciliationCutoffDays: h.bankReconCutoffDays,
		FinancialControlGate:         gateMode(h.enforceCloseGate),
		PostingBacklogCutoff:         postingBacklogCutoff(fp),
		ChecklistRequirementIDs:      []string{},
		SubledgerControls:            []domain.RelianceControlRun{},
		BankReconciliations:          []domain.RelianceBankRecon{},
		ExcludedBankAccounts:         []domain.RelianceBankExclusion{},
		ReperformedAfter:             notBefore,
	}

	unposted, err := h.clients.GetUnpostedJournalsCount(ctx, tenantID, fp.LegalEntityID, fp.PeriodName)
	if err != nil {
		h.log.Error("failed to verify outstanding journals", zap.Error(err))
		return nil, nil, fmt.Errorf("general-ledger-svc: %w", err)
	}
	if unposted > 0 {
		issues = append(issues, fmt.Sprintf("unposted_journals_exist: %d %s in PENDING or VALIDATED status",
			unposted, plural(unposted, "journal is", "journals are")))
	}

	backlog, err := h.clients.GetPostingBacklog(ctx, tenantID, principalID, fp.LegalEntityID, postingBacklogCutoff(fp))
	if err != nil {
		h.log.Error("failed to verify the posting backlog", zap.Error(err))
		return nil, nil, fmt.Errorf("general-ledger-svc: %w", err)
	}
	if issue := postingBacklogIssue(backlog); issue != "" {
		issues = append(issues, issue)
	}

	var checklist []domain.CloseRequirement
	if !h.subledgerGateOff || !h.bankReconGateOff {
		checklist, err = h.store.ListCloseRequirements(ctx, fp.LegalEntityID)
		if err != nil {
			h.log.Error("failed to read the close checklist", zap.Error(err))
			return nil, nil, fmt.Errorf("close checklist: %w", err)
		}
		for _, cr := range checklist {
			reliance.ChecklistRequirementIDs = append(reliance.ChecklistRequirementIDs, cr.RequirementID)
		}
	}

	if !h.bankReconGateOff {
		accounts, err := h.clients.ListBankAccounts(ctx, tenantID, principalID, fp.LegalEntityID)
		if err != nil {
			h.log.Error("failed to list bank accounts", zap.Error(err))
			return nil, nil, fmt.Errorf("treasury-svc: %w", err)
		}
		recon, err := h.clients.GetBankReconciliationStatus(ctx, tenantID, principalID, fp.LegalEntityID, fp.PeriodStart, fp.PeriodEnd)
		if err != nil {
			h.log.Error("failed to read bank reconciliation status", zap.Error(err))
			return nil, nil, fmt.Errorf("bank-reconciliation-svc: %w", err)
		}
		bankIssues, proofs, waived := bankReconciliationIssues(accounts, recon, checklist, fp, h.bankReconCutoffDays, notBefore)
		issues = append(issues, bankIssues...)
		reliance.BankReconciliations = append(reliance.BankReconciliations, proofs...)
		reliance.ExcludedBankAccounts = append(reliance.ExcludedBankAccounts, waived...)
	}

	if !h.subledgerGateOff {
		runs, err := h.store.ListControlRuns(ctx, fp.LegalEntityID, fp.PeriodName)
		if err != nil {
			h.log.Error("failed to read subledger control runs", zap.Error(err))
			return nil, nil, fmt.Errorf("subledger control runs: %w", err)
		}
		controlIssues, matched := requiredControlIssues(requiredControls(checklist), runs, fp.PeriodName, notBefore)
		issues = append(issues, controlIssues...)
		reliance.SubledgerControls = append(reliance.SubledgerControls, matched...)
	}

	if h.enforceCloseGate {
		gate, err := h.clients.GetCloseGate(ctx, tenantID, principalID, fp.LegalEntityID, fp.PeriodName)
		if err != nil {
			h.log.Error("failed to verify financial control close gate", zap.Error(err))
			return nil, nil, fmt.Errorf("financial-control-svc: %w", err)
		}
		if !gate.Open {
			issues = append(issues, fmt.Sprintf("financial_controls: %d mandatory control(s) not certified", gate.BlockingCount))
		}
		if !gate.Configured {
			issues = append(issues, "financial_controls: no mandatory controls configured for this entity")
		}
	}

	return issues, reliance, nil
}

// assetBook is the book an ASSETS run reconciles; every other subledger has
// none. A book_id sent with a non-ASSETS run is ignored rather than stored,
// so the record never claims a book it did not use.
func assetBook(req domain.RunSubledgerControlRequest) string {
	if req.Subledger == "ASSETS" {
		return req.BookID
	}
	return ""
}

// postingBacklogCutoff is the start of the day after the period ends: an
// event GL accepted at any time on the period's last day is in scope.
//
// Events are selected by when GL accepted them, not by fiscal period (a
// posting execution records no period). An October event first submitted in
// November is therefore outside this check; the subledger-agreement check is
// what covers it, since the subledger counts it in October regardless.
func postingBacklogCutoff(fp *domain.FiscalPeriod) time.Time {
	end := fp.PeriodEnd.UTC()
	return time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

// postingBacklogIssue renders the backlog as a blocker, naming a few events
// so whoever closes can act without opening another system. Empty when there
// is nothing to block on.
func postingBacklogIssue(b domain.PostingBacklog) string {
	if b.TooLarge {
		return "unposted_accounting_events: the posting backlog exceeds general-ledger-svc's population limit; more accounting events are unposted than can be listed"
	}
	if b.Count == 0 {
		return ""
	}
	var named []string
	for _, item := range b.Samples {
		detail := item.Reference + " (" + item.Status
		if item.FailureReason != "" {
			detail += ": " + item.FailureReason
		}
		named = append(named, detail+")")
	}
	msg := fmt.Sprintf("unposted_accounting_events: %d accounting %s accepted before period end not posted to the ledger",
		b.Count, plural(int(b.Count), "event", "events"))
	if len(named) > 0 {
		msg += ": " + strings.Join(named, "; ")
		if int64(len(named)) < b.Count {
			msg += fmt.Sprintf("; and %d more", b.Count-int64(len(named)))
		}
	}
	return msg
}

// bankReconciliationIssues requires, for every bank account of the entity in
// scope, a CERTIFIED, non-superseded reconciliation for a statement dated in
// the period and no more than cutoffDays before its last day.
//
// In scope: ACTIVE or SUSPENDED (a suspended account still holds money)
// accounts that existed by period end, minus those the entity's checklist
// excludes as immaterial (ZS-CONTROL-001 §22 "material accounts"). DRAFT and
// PENDING_VERIFICATION accounts are not operational; CLOSED accounts carry no
// closing date in treasury yet, so they are out.
//
// Why the statement must be dated IN the period: bank-reconciliation-svc
// assigns a run to the period of its statement date (its certification
// checks that period is open), so a 3 November statement belongs to
// November. Why the cut-off: without it, a close could pass on a reconciliation
// a week old while the last statement of the month has not arrived.
func bankReconciliationIssues(accounts []domain.BankAccountRef, recon []domain.BankAccountReconStatus,
	checklist []domain.CloseRequirement, fp *domain.FiscalPeriod, cutoffDays int, notBefore *time.Time) ([]string, []domain.RelianceBankRecon, []domain.RelianceBankExclusion) {
	excluded := map[string]domain.CloseRequirement{}
	for _, cr := range checklist {
		if cr.Kind == domain.CloseRequirementBankAccountExclusion {
			excluded[cr.BankAccountID] = cr
		}
	}
	var proofs []domain.RelianceBankRecon
	var waived []domain.RelianceBankExclusion
	status := map[string]domain.BankAccountReconStatus{}
	for _, st := range recon {
		status[st.BankAccountID] = st
	}
	end := fp.PeriodEnd.UTC()
	lastDay := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
	earliest := lastDay.AddDate(0, 0, -cutoffDays)

	var issues []string
	for _, a := range accounts {
		if a.AccountStatus != "ACTIVE" && a.AccountStatus != "SUSPENDED" {
			continue
		}
		if a.CreatedAt.After(lastDay.AddDate(0, 0, 1)) {
			continue
		}
		if cr, ok := excluded[a.BankAccountID]; ok {
			waived = append(waived, domain.RelianceBankExclusion{
				BankAccountID: a.BankAccountID, RequirementID: cr.RequirementID, Reason: cr.Reason})
			continue
		}
		name := a.AccountName
		if a.MaskedAccountNumber != "" {
			name += " (" + a.MaskedAccountNumber + ")"
		}
		st := status[a.BankAccountID]
		attempt := ""
		if st.LatestRun != nil && (st.LatestCertified == nil || st.LatestRun.RunID != st.LatestCertified.RunID) {
			attempt = fmt.Sprintf("; the %s run is %s", st.LatestRun.StatementDate, st.LatestRun.Status)
		}
		if st.LatestCertified == nil {
			if st.LatestRun == nil {
				issues = append(issues, fmt.Sprintf("bank_reconciliation_missing: %s has no reconciliation run for %s", name, fp.PeriodName))
			} else {
				issues = append(issues, fmt.Sprintf("bank_reconciliation_missing: %s has no certified reconciliation for %s%s", name, fp.PeriodName, attempt))
			}
			continue
		}
		certified, err := time.Parse("2006-01-02", st.LatestCertified.StatementDate)
		if err != nil || certified.Before(earliest) {
			issues = append(issues, fmt.Sprintf(
				"bank_reconciliation_stale: latest certified reconciliation for %s is for the %s statement; one dated on or after %s (within %d days of period end) is required%s",
				name, st.LatestCertified.StatementDate, earliest.Format("2006-01-02"), cutoffDays, attempt))
			continue
		}
		if notBefore != nil && (st.LatestCertified.CertifiedAt == nil || !st.LatestCertified.CertifiedAt.After(*notBefore)) {
			issues = append(issues, fmt.Sprintf(
				"bank_reconciliation_not_reperformed: the certified reconciliation for %s (%s statement) predates the reopen at %s; re-certify it before reclosing",
				name, st.LatestCertified.StatementDate, notBefore.UTC().Format(time.RFC3339)))
			continue
		}
		proofs = append(proofs, domain.RelianceBankRecon{
			BankAccountID: a.BankAccountID, RunID: st.LatestCertified.RunID, StatementDate: st.LatestCertified.StatementDate})
	}
	return issues, proofs, waived
}

func gateMode(enforced bool) string {
	if enforced {
		return "enforce"
	}
	return "off"
}

// GetPeriodEvidence — GET /v1/close/periods/{id}/evidence.
//
// Every close of the period (a reopened period closed again has several),
// each with what it relied on. reliance_manifest is the exact signed text:
// sha256 of it is reliance_hash, so anyone holding the signing key can verify
// the record was not altered; reliance is the same content decoded for reading.
func (h *Handler) GetPeriodEvidence(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionCloseView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	rows, err := h.store.ListCloseEvidence(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}
	out := make([]domain.CloseEvidenceView, 0, len(rows))
	for _, e := range rows {
		view := domain.CloseEvidenceView{CloseEvidence: e, RelianceManifestText: e.RelianceManifest}
		if e.RelianceManifest != "" {
			var rel domain.CloseReliance
			if err := json.Unmarshal([]byte(e.RelianceManifest), &rel); err == nil {
				view.Reliance = &rel
			}
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, out)
}

// controlKey identifies one required control: the subledger type, and for
// ASSETS the book it reconciles (an asset ledger has one net book value per
// book, and each book is its own control).
type controlKey struct {
	Subledger string
	BookID    string
}

func (k controlKey) label() string {
	if k.BookID != "" {
		return k.Subledger + " (book " + k.BookID + ")"
	}
	return k.Subledger
}

// completenessControls compare counts (covered vs eligible, items found vs
// expected), not a subledger total against a GL control account.
var completenessControls = map[string]bool{
	"DEPRECIATION_COMPLETENESS": true, "INVENTORY_QUANTITY": true, "STOCK_COUNT": true,
}

// completenessFinding states a failed completeness run in its own terms.
// RunSubledgerControl stores these as (found, expected): depreciation as
// (covered, eligible), the inventory checks as (problem count, 0).
func completenessFinding(run domain.SubledgerControlRun) string {
	found, expected := run.SubledgerTotalAmount, run.GLControlBalanceAmount
	switch run.Subledger {
	case "DEPRECIATION_COMPLETENESS":
		return fmt.Sprintf("depreciation posted for %.0f of %.0f eligible assets", found, expected)
	case "INVENTORY_QUANTITY":
		return fmt.Sprintf("%.0f inventory items have a negative quantity on hand", found)
	case "STOCK_COUNT":
		return fmt.Sprintf("%.0f stock-count variances are not approved", found)
	}
	return fmt.Sprintf("found %.0f where %.0f were expected", found, expected)
}

// requiredControls is what an entity's close must prove: the AR/AP baseline
// (ZS-CONTROL-001 §22, every entity) plus the controls its checklist adds.
func requiredControls(checklist []domain.CloseRequirement) []controlKey {
	var out []controlKey
	for _, ledger := range domain.BaselineSubledgerControls {
		out = append(out, controlKey{Subledger: ledger})
	}
	for _, cr := range checklist {
		if cr.Kind == domain.CloseRequirementSubledgerControl {
			out = append(out, controlKey{Subledger: cr.Subledger, BookID: cr.BookID})
		}
	}
	return out
}

// requiredControlIssues requires, for each required control, that the LATEST
// run for the period exists and MATCHED. Only the latest counts: a run that
// matched before further postings landed proves nothing about the books as
// they are now, and an EXCEPTION that was since fixed and re-run is
// superseded rather than blocking forever. An ASSETS run counts only for the
// book it reconciled.
func requiredControlIssues(required []controlKey, runs []domain.SubledgerControlRun, period string, notBefore *time.Time) ([]string, []domain.RelianceControlRun) {
	latest := map[controlKey]domain.SubledgerControlRun{}
	for _, run := range runs {
		key := controlKey{Subledger: run.Subledger, BookID: run.BookID}
		cur, seen := latest[key]
		if !seen || run.RunAt.After(cur.RunAt) {
			latest[key] = run
		}
	}
	var issues []string
	var matched []domain.RelianceControlRun
	for _, key := range required {
		run, ok := latest[key]
		switch {
		case !ok && completenessControls[key.Subledger]:
			issues = append(issues, fmt.Sprintf(
				"subledger_control_not_run: no %s control run for %s; run one (POST /v1/subledger-control/runs) before closing",
				key.label(), period))
		case !ok:
			issues = append(issues, fmt.Sprintf(
				"subledger_control_not_run: no %s subledger-to-GL control run for %s; run one (POST /v1/subledger-control/runs) before closing",
				key.label(), period))
		case notBefore != nil && !run.RunAt.After(*notBefore):
			issues = append(issues, fmt.Sprintf(
				"subledger_control_not_reperformed: the latest %s control run (%s) predates the reopen at %s; re-run it before reclosing",
				key.label(), run.ControlRunID, notBefore.UTC().Format(time.RFC3339)))
		case run.Status == "MATCHED":
			matched = append(matched, domain.RelianceControlRun{
				Subledger: key.Subledger, BookID: key.BookID, ControlRunID: run.ControlRunID, RunAt: run.RunAt})
		case completenessControls[key.Subledger]:
			issues = append(issues, fmt.Sprintf("subledger_control_exception: %s: %s (run %s)",
				key.label(), completenessFinding(run), run.ControlRunID))
		default:
			issues = append(issues, fmt.Sprintf(
				"subledger_control_exception: %s subledger total %.2f does not agree with GL control account %s balance %.2f (difference %.2f; run %s)",
				key.label(), run.SubledgerTotalAmount, run.ControlAccountCode, run.GLControlBalanceAmount, run.DifferenceAmount, run.ControlRunID))
		}
	}
	return issues, matched
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
	if errors.Is(err, domain.ErrPostingBacklogForbidden) {
		writeError(w, http.StatusForbidden, "posting_backlog_not_permitted", string(domain.ErrPostingBacklogForbidden)+"; close blocked")
		return
	}
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
