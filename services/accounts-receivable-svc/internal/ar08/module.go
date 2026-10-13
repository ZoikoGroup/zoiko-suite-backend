package ar08

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/ar04"
	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Module is AR-08. It implements kit.PostingListener for kit.SourceCashApplication and kit.SourceCashUnapply.
type Module struct {
	deps    kit.Deps
	source  ReceiptSource   // Banking (settlement authority); nil -> every receipt command answers SETTLEMENT_NOT_CONFIRMED (fail closed)
	items   kit.OpenItems   // AR-07; nil -> apply / unapply answer 503 and change nothing
	finder  OpenItemFinder  // AR-07 ListOpenForCustomer; nil -> GenerateMatchCandidates answers 503
	periods ar04.PeriodGate // accounting-period-svc; optional (nil = no period gate in AR; ACC-04 still enforces its own)
}

var _ kit.PostingListener = (*Module)(nil)

// New builds the module. Pass a literal nil (never a typed-nil pointer) for a collaborator that is not wired.
func New(deps kit.Deps, source ReceiptSource, items kit.OpenItems, finder OpenItemFinder, periods ar04.PeriodGate) *Module {
	return &Module{deps: deps, source: source, items: items, finder: finder, periods: periods}
}

// Register mounts the spec paths verbatim at the router root.
func (m *Module) Register(r chi.Router) {
	for _, q := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"getApplication", m.getApplication},
		{"getUnappliedCash", m.getUnappliedCash},
		{"getMatchCandidates", m.getMatchCandidates},
		{"getApplicationReconciliation", m.getApplicationReconciliation},
		{"getAvailableActions", m.getAvailableActions},
	} {
		h := m.deps.Query(q.h)
		r.Get("/internal/v1/ar08/"+q.name, h)
		r.Post("/internal/v1/ar08/"+q.name, h)
	}
	for _, c := range []struct {
		path, op string
		h        http.HandlerFunc
	}{
		{"registerConfirmedReceipt", "AR08.RegisterConfirmedReceipt", m.registerConfirmedReceipt},
		{"createApplicationBatch", "AR08.CreateApplicationBatch", m.createApplicationBatch},
		{"generateMatchCandidates", "AR08.GenerateMatchCandidates", m.generateMatchCandidates},
		{"applyReceipt", "AR08.ApplyReceipt", m.applyReceipt},
		{"approveManualMatch", "AR08.ApproveManualMatch", m.approveManualMatch},
		{"unapplyReceipt", "AR08.UnapplyReceipt", m.unapplyReceipt},
		{"reapplyReceipt", "AR08.ReapplyReceipt", m.reapplyReceipt},
		{"reconcileApplicationBatch", "AR08.ReconcileApplicationBatch", m.reconcileApplicationBatch},
	} {
		r.Post("/internal/v1/ar08/commands/"+c.path, m.deps.Cmd(c.op, c.h))
	}
}

func (m *Module) read(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return m.deps.DB.WithTx(ctx, tenantID, fn)
}

func (m *Module) fail(w http.ResponseWriter, err error) {
	err = kit.MapPgErr(err)
	var p *kit.Problem
	if !errors.As(err, &p) {
		m.deps.Log.Error("ar08 request failed", zap.Error(err))
	}
	kit.WriteErr(w, err)
}

// checkAction is Authorize without writing the response.
func (m *Module) checkAction(ctx context.Context, c kit.Caller, action string) error {
	err := m.deps.Authz.CheckAllowed(ctx, c.PrincipalID, c.LegalEntityID, action)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrAuthorizationDenied):
		return kit.Forbidden(kit.CodeForbidden, "")
	default:
		m.deps.Log.Error("authorization check failed - failing closed", zap.Error(err))
		return kit.Unavailable("authorization service unavailable")
	}
}

// confirmed asks Banking for the transaction and fails closed on anything but a clean NORMALIZED receipt.
func (m *Module) confirmed(ctx context.Context, c kit.Caller, bankTxnID string) (*BankTransaction, error) {
	if m.source == nil {
		return nil, errSettlementUnknown()
	}
	t, err := m.source.Get(ctx, c, bankTxnID)
	switch {
	case errors.Is(err, ErrBankTxnNotFound):
		return nil, errNotConfirmed("the bank transaction was not found")
	case err != nil:
		m.deps.Log.Warn("ar08 settlement source unavailable", zap.Error(err))
		return nil, errSettlementUnknown()
	}
	if err := VerifyConfirmed(t, bankTxnID, c.TenantID); err != nil {
		return nil, err
	}
	return t, nil
}

// ── kit.PostingListener ──────────────────────────────────────────────────────

// OnPostingOutcome mirrors the ACC-04 outcome (POSTED / FAILED / QUARANTINED + journal id) onto the application. q.SourceID is
// the application id. It errors when the application is not found so the queue row is re-delivered.
func (m *Module) OnPostingOutcome(ctx context.Context, q kit.QueuedPosting, o kit.PostingOutcome) error {
	if q.SourceType != kit.SourceCashApplication && q.SourceType != kit.SourceCashUnapply {
		return nil
	}
	switch o.Status {
	case kit.PostingPosted, kit.PostingFailed, kit.PostingQuarantined:
	default:
		return nil
	}
	return m.deps.DB.WithTx(ctx, q.TenantID, func(tx pgx.Tx) error {
		a, err := getApplication(ctx, tx, q.TenantID, q.SourceID, true)
		if err != nil {
			return err
		}
		if a == nil {
			return notFound("application")
		}
		col := "accounting_state = $3, journal_id = COALESCE(NULLIF($4,''), journal_id), accounting_error = NULLIF($5,'')"
		before := a.AccountingState
		if q.SourceType == kit.SourceCashUnapply {
			col = "unapply_accounting_state = $3, unapply_journal_id = COALESCE(NULLIF($4,''), unapply_journal_id), unapply_accounting_error = NULLIF($5,'')"
			before = a.UnapplyState
		}
		if _, err := tx.Exec(ctx, `UPDATE ar08_applications SET `+col+` WHERE tenant_id = $1 AND application_id = $2`,
			q.TenantID, q.SourceID, o.Status, o.JournalID, o.Error); err != nil {
			return err
		}
		c := kit.Caller{TenantID: q.TenantID, PrincipalID: "system:acc-04", CorrelationID: q.CorrelationID, LegalEntityID: a.LegalEntityID}
		return audit(ctx, tx, c, AggregateApplication, a.ApplicationID, "accountingOutcome", "", "",
			map[string]any{"accounting_state": before},
			map[string]any{"accounting_state": o.Status, "journal_id": o.JournalID, "posting_execution_id": o.ExecutionID, "error": o.Error},
			map[string]any{"source_type": q.SourceType, "source_event_id": q.SourceEventID})
	})
}
