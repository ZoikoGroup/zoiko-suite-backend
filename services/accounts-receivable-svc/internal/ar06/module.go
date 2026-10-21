package ar06

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/ar04"
	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
)

// Module is AR-06. It implements kit.PostingListener for kit.SourceCreditNote and kit.SourceDebitNote, and (via EInvoiceSource)
// the ar05.DocumentSource of issued credit / debit notes.
type Module struct {
	deps      kit.Deps
	invoices  kit.Invoices            // AR-04; nil -> every command that needs the source invoice answers 503
	openItems kit.OpenItems           // AR-07; nil until wired -> issue answers 503 and changes nothing
	tax       taxdet.Client           // tax-determination-svc; nil -> submit / issue answer 503
	periods   ar04.PeriodGate         // accounting-period-svc; nil -> issue answers 503 (fail closed)
	einvoice  kit.EInvoiceRequirement // AR-05; nil -> e-invoice is NOT_REQUIRED
}

var _ kit.PostingListener = (*Module)(nil)

// New builds the module. Pass a literal nil (never a typed-nil pointer) for a collaborator that is not wired yet.
func New(deps kit.Deps, invoices kit.Invoices, openItems kit.OpenItems, tax taxdet.Client, periods ar04.PeriodGate, einvoice kit.EInvoiceRequirement) *Module {
	return &Module{deps: deps, invoices: invoices, openItems: openItems, tax: tax, periods: periods, einvoice: einvoice}
}

// Register mounts the spec paths verbatim at the router root.
func (m *Module) Register(r chi.Router) {
	for _, q := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"getCorrection", m.getCorrection},
		{"getCorrectableAmount", m.getCorrectableAmount},
		{"getCorrectionChain", m.getCorrectionChain},
		{"getAvailableActions", m.getAvailableActions},
		{"getCorrectionAccountingStatus", m.getCorrectionAccountingStatus},
	} {
		h := m.deps.Query(q.h)
		r.Get("/internal/v1/ar06/"+q.name, h)
		r.Post("/internal/v1/ar06/"+q.name, h)
	}
	for _, c := range []struct {
		path, op string
		h        http.HandlerFunc
	}{
		{"createCorrectionDraft", "AR06.CreateCorrectionDraft", m.createCorrectionDraft},
		{"submitCorrectionForApproval", "AR06.SubmitCorrectionForApproval", m.submitCorrectionForApproval},
		{"approveCorrection", "AR06.ApproveCorrection", m.approveCorrection},
		{"issueCreditNote", "AR06.IssueCreditNote", func(w http.ResponseWriter, r *http.Request) { m.issueNote(w, r, KindCredit) }},
		{"issueDebitNote", "AR06.IssueDebitNote", func(w http.ResponseWriter, r *http.Request) { m.issueNote(w, r, KindDebit) }},
		{"cancelUnissuedCorrection", "AR06.CancelUnissuedCorrection", m.cancelUnissuedCorrection},
		{"linkReplacementInvoice", "AR06.LinkReplacementInvoice", m.linkReplacementInvoice},
	} {
		r.Post("/internal/v1/ar06/commands/"+c.path, m.deps.Cmd(c.op, c.h))
	}
}

func (m *Module) read(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return m.deps.DB.WithTx(ctx, tenantID, fn)
}

func (m *Module) today() time.Time { return m.deps.Now() }

func (m *Module) fail(w http.ResponseWriter, err error) {
	var p *kit.Problem
	if !errors.As(err, &p) {
		m.deps.Log.Error("ar06 request failed", zap.Error(err))
	}
	kit.WriteErr(w, err)
}

// denial is a refusal that leaves audit evidence (<command>.denied) of the attempt.
type denial struct {
	*kit.Problem
	Command string
	Detail  map[string]any
}

func (d *denial) Unwrap() error { return d.Problem }

func deny(p *kit.Problem, command string, detail map[string]any) error {
	return &denial{Problem: p, Command: command, Detail: detail}
}

func errSoD(msg string) *kit.Problem { return kit.Forbidden(kit.CodeSegregationOfDuties, msg) }

// recordDenial keeps evidence of a refused attempt (the command wrote nothing else, so the audit is its own transaction).
func (m *Module) recordDenial(ctx context.Context, c kit.Caller, correctionID string, d *denial) {
	detail := map[string]any{"denial": d.Code}
	for k, v := range d.Detail {
		detail[k] = v
	}
	_ = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: correctionID, Command: d.Command + ".denied", Detail: detail})
	})
}

func (m *Module) failStep(w http.ResponseWriter, ctx context.Context, c kit.Caller, correctionID string, err error) {
	var d *denial
	if errors.As(err, &d) {
		m.recordDenial(ctx, c, correctionID, d)
	}
	m.fail(w, err)
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

// source reads the ISSUED source invoice through the AR-04 port. A draft, unapproved, cancelled, unknown or foreign-tenant
// invoice is NOT_FOUND (spec section 18 #18: a correction can never be raised against a document that was never authoritative).
func (m *Module) source(ctx context.Context, tenantID, invoiceID string) (*kit.IssuedInvoice, error) {
	if m.invoices == nil {
		return nil, kit.Unavailable("AR-04 invoices not wired: the source invoice cannot be verified")
	}
	if err := mustUUID("source_invoice_id", invoiceID); err != nil {
		return nil, err
	}
	src, err := m.invoices.GetIssued(ctx, tenantID, invoiceID)
	if err != nil {
		var p *kit.Problem
		if errors.As(err, &p) {
			return nil, err
		}
		m.deps.Log.Error("ar06 source invoice lookup failed", zap.Error(err))
		return nil, kit.Unavailable("source invoice unavailable")
	}
	if src == nil {
		return nil, kit.NotFound("source invoice not found")
	}
	return src, nil
}

// ── kit.PostingListener ──────────────────────────────────────────────────────

// OnPostingOutcome mirrors the ACC-04 outcome (POSTED / FAILED / QUARANTINED + journal id) onto the note's accounting fields.
// It returns an error when the correction cannot be found so the queue row stays claimed and is re-delivered.
func (m *Module) OnPostingOutcome(ctx context.Context, q kit.QueuedPosting, o kit.PostingOutcome) error {
	if q.SourceType != kit.SourceCreditNote && q.SourceType != kit.SourceDebitNote {
		return nil
	}
	switch o.Status {
	case kit.PostingPosted, kit.PostingFailed, kit.PostingQuarantined:
	default:
		return nil
	}
	return m.deps.DB.WithTx(ctx, q.TenantID, func(tx pgx.Tx) error {
		h, err := getHeader(ctx, tx, q.TenantID, q.SourceID, false)
		if err != nil {
			return err
		}
		if _, err := mirrorAccounting(ctx, tx, q.TenantID, q.SourceID, o.Status, o.JournalID, o.Error, m.today()); err != nil {
			return err
		}
		c := kit.Caller{TenantID: q.TenantID, PrincipalID: "system:acc-04", CorrelationID: q.CorrelationID, LegalEntityID: h.LegalEntityID}
		return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: h.CorrectionID, Command: "accountingOutcome",
			Before: map[string]any{"accounting_state": h.AccountingState},
			After:  map[string]any{"accounting_state": o.Status, "journal_id": o.JournalID, "posting_execution_id": o.ExecutionID, "error": o.Error},
			Detail: map[string]any{"document_number": h.DocumentNumber, "source_event_id": q.SourceEventID}})
	})
}
