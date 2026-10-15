package ar04

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
)

// Module is AR-04. It implements kit.Invoices (consumed by AR-05/06/07/08/09/10) and
// kit.PostingListener for kit.SourceInvoice.
type Module struct {
	deps      kit.Deps
	profiles  kit.ProfileResolver     // AR-01
	orders    kit.Orders              // AR-03; nil = order-based invoices are refused (503), direct invoices still work
	openItems kit.OpenItems           // AR-07; nil until wired -> IssueInvoice answers 503 and changes nothing
	tax       taxdet.Client           // tax-determination-svc; nil -> submit / issue answer 503
	periods   PeriodGate              // accounting-period-svc; nil -> issue answers 503 (fail closed)
	einvoice  kit.EInvoiceRequirement // AR-05; nil -> e-invoice is NOT_REQUIRED
}

var (
	_ kit.Invoices        = (*Module)(nil)
	_ kit.PostingListener = (*Module)(nil)
)

// New builds the module. profiles is required (AR-01's Module implements it). Pass a literal nil
// (never a typed-nil pointer) for a collaborator that is not wired yet.
func New(deps kit.Deps, profiles kit.ProfileResolver, orders kit.Orders, openItems kit.OpenItems, tax taxdet.Client, periods PeriodGate, einvoice kit.EInvoiceRequirement) *Module {
	return &Module{deps: deps, profiles: profiles, orders: orders, openItems: openItems, tax: tax, periods: periods, einvoice: einvoice}
}

// Register mounts the spec paths verbatim at the router root. decideLegalCancellation is the
// approval half of RequestLegalCancellation (spec permission invoice.cancel.approve).
func (m *Module) Register(r chi.Router) {
	for _, q := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"getInvoice", m.getInvoice},
		{"getInvoiceAsIssued", m.getInvoiceAsIssued},
		{"getInvoiceStateDimensions", m.getInvoiceStateDimensions},
		{"getAvailableActions", m.getAvailableActions},
		{"getInvoiceHistory", m.getInvoiceHistory},
		{"getInvoiceAccountingStatus", m.getInvoiceAccountingStatus},
	} {
		h := m.deps.Query(q.h)
		r.Get("/internal/v1/ar04/"+q.name, h)
		r.Post("/internal/v1/ar04/"+q.name, h)
	}
	for _, c := range []struct {
		path, op string
		h        http.HandlerFunc
	}{
		{"createInvoiceDraft", "AR04.CreateInvoiceDraft", m.createInvoiceDraft},
		{"amendInvoiceDraft", "AR04.AmendInvoiceDraft", m.amendInvoiceDraft},
		{"submitInvoiceForApproval", "AR04.SubmitInvoiceForApproval", m.submitInvoiceForApproval},
		{"approveInvoice", "AR04.ApproveInvoice", m.approveInvoice},
		{"issueInvoice", "AR04.IssueInvoice", m.issueInvoice},
		{"cancelUnissuedInvoice", "AR04.CancelUnissuedInvoice", m.cancelUnissuedInvoice},
		{"requestLegalCancellation", "AR04.RequestLegalCancellation", m.requestLegalCancellation},
		{"decideLegalCancellation", "AR04.DecideLegalCancellation", m.decideLegalCancellation},
		{"markDeliveryResult", "AR04.MarkDeliveryResult", m.markDeliveryResult},
		{"linkCorrectionDocument", "AR04.LinkCorrectionDocument", m.linkCorrectionDocument},
	} {
		r.Post("/internal/v1/ar04/commands/"+c.path, m.deps.Cmd(c.op, c.h))
	}
}

// read runs fn in a tenant-scoped transaction (sets app.tenant_id so FORCE'd RLS binds).
func (m *Module) read(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return m.deps.DB.WithTx(ctx, tenantID, fn)
}

func (m *Module) today() time.Time { return m.deps.Now() }

// profile resolves the customer's ACTIVE financial profile at a date via AR-01. No answer is never
// treated as "active": an unreachable resolver is 503.
func (m *Module) profile(ctx context.Context, tenantID, entityID, partyID string, at time.Time) (*kit.ProfileView, error) {
	if m.profiles == nil {
		return nil, kit.Unavailable("customer financial profile service not wired")
	}
	v, err := m.profiles.ActiveAt(ctx, tenantID, entityID, partyID, at)
	if err != nil {
		var p *kit.Problem
		if errors.As(err, &p) {
			return nil, err
		}
		m.deps.Log.Error("ar04 profile resolution failed", zap.Error(err))
		return nil, kit.Unavailable("customer financial profile unavailable")
	}
	if v == nil {
		return nil, kit.Unprocessable(kit.CodeCustomerProfileInactive, "no active financial profile for this customer")
	}
	return v, nil
}

func (m *Module) fail(w http.ResponseWriter, err error) {
	var p *kit.Problem
	if !errors.As(err, &p) {
		m.deps.Log.Error("ar04 request failed", zap.Error(err))
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

// recordDenial keeps evidence of a refused attempt (the command wrote nothing else, so the audit is
// its own transaction).
func (m *Module) recordDenial(ctx context.Context, c kit.Caller, invoiceID string, d *denial) {
	detail := map[string]any{"denial": d.Code}
	for k, v := range d.Detail {
		detail[k] = v
	}
	_ = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: invoiceID, Command: d.Command + ".denied", Detail: detail})
	})
}

// ── kit.Invoices (consumed by AR-05/06/07/08/09/10) ──────────────────────────

func toIssued(inv *Invoice) *kit.IssuedInvoice {
	out := &kit.IssuedInvoice{InvoiceID: inv.InvoiceID, LegalEntityID: inv.LegalEntityID, CustomerPartyID: inv.CustomerPartyID, InvoiceNumber: inv.InvoiceNumber,
		Currency: inv.Currency, Net: inv.NetTotal, Tax: inv.TaxTotal, Gross: inv.GrossTotal, ContentHash: inv.IssuedContentHash}
	out.IssueDate, _ = time.Parse("2006-01-02", inv.IssueDate)
	out.DueDate, _ = time.Parse("2006-01-02", inv.DueDate)
	var ids, packs []string
	seenPack := map[string]bool{}
	for _, g := range inv.TaxGroups {
		ids = append(ids, g.DeterminationID)
		if !seenPack[g.Provenance] {
			seenPack[g.Provenance] = true
			packs = append(packs, g.Provenance)
		}
	}
	// kit.IssuedInvoice carries one id / one provenance; with several tax groups they are comma-joined.
	out.TaxDeterminationID, out.TaxRulePackVersion = strings.Join(ids, ","), strings.Join(packs, ",")
	for _, l := range inv.Lines {
		out.Lines = append(out.Lines, kit.IssuedInvoiceLine{LineNumber: l.LineNo, Description: l.Description, Quantity: l.Quantity, UnitPrice: l.UnitPrice,
			Net: l.Net, Tax: l.Tax, TaxCode: l.TaxCategory})
	}
	return out
}

// GetIssued returns the invoice only if it is ISSUED (or CORRECTED). Anything else, including a
// foreign tenant's invoice, is NOT_FOUND.
func (m *Module) GetIssued(ctx context.Context, tenantID, invoiceID string) (*kit.IssuedInvoice, error) {
	var out *kit.IssuedInvoice
	err := m.read(ctx, tenantID, func(tx pgx.Tx) error {
		h, err := getHeader(ctx, tx, tenantID, invoiceID, false)
		if err != nil {
			return err
		}
		if !h.State.IsIssued() {
			return kit.NotFound("invoice not found")
		}
		inv, err := loadInvoice(ctx, tx, tenantID, h)
		if err != nil {
			return err
		}
		out = toIssued(inv)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// lockIssued locks an issued invoice for a port write inside the caller's transaction and adopts its
// legal entity onto the caller.
func lockIssued(ctx context.Context, tx pgx.Tx, c *kit.Caller, invoiceID string) (*Header, error) {
	h, err := getHeader(ctx, tx, c.TenantID, invoiceID, true)
	if err != nil {
		return nil, err
	}
	if !h.State.IsIssued() {
		return nil, kit.Conflict(kit.CodeInvalidTransition, "the invoice is not issued: its settlement, e-invoice and dispute dimensions are not open to change")
	}
	c.LegalEntityID = h.LegalEntityID
	return h, nil
}

// setDimension is the shared body of the three dimension ports: lock, validate, write ONLY the owning
// dimension, append history and audit. An unchanged value is an idempotent no-op.
func (m *Module) setDimension(ctx context.Context, tx pgx.Tx, c kit.Caller, invoiceID, command, to string, valid func(string) bool,
	current func(*Header) string, write func(context.Context, pgx.Tx, string, *Header, string, time.Time) (int, error)) error {
	if !valid(to) {
		return kit.Invalid("state is not a valid value of this dimension")
	}
	h, err := lockIssued(ctx, tx, &c, invoiceID)
	if err != nil {
		return err
	}
	prior := current(h)
	if prior == to {
		return nil
	}
	now := m.today()
	v, err := write(ctx, tx, c.TenantID, h, to, now)
	if err != nil {
		return err
	}
	nh := *h
	nh.Version = v
	return record(ctx, tx, c, &nh, hist{At: now, Command: command, ContentHash: h.IssuedContentHash,
		Prior: map[string]any{"state": prior}, New: map[string]any{"state": to}}, nil)
}

// SetEInvoiceState mirrors the AR-05 submission state onto the e-invoice dimension only.
func (m *Module) SetEInvoiceState(ctx context.Context, tx pgx.Tx, c kit.Caller, invoiceID, state string) error {
	return m.setDimension(ctx, tx, c, invoiceID, "setEInvoiceState", state, ValidEInvoiceState, func(h *Header) string { return h.EInvoice }, writeEInvoice)
}

// SetSettlementState mirrors the AR-07 residual onto the settlement dimension only.
func (m *Module) SetSettlementState(ctx context.Context, tx pgx.Tx, c kit.Caller, invoiceID, state string) error {
	return m.setDimension(ctx, tx, c, invoiceID, "setSettlementState", state, ValidSettlementState, func(h *Header) string { return h.Settlement }, writeSettlement)
}

// SetDisputeState mirrors an AR-07 dispute onto the dispute dimension only.
func (m *Module) SetDisputeState(ctx context.Context, tx pgx.Tx, c kit.Caller, invoiceID, state string) error {
	return m.setDimension(ctx, tx, c, invoiceID, "setDisputeState", state, ValidDisputeState, func(h *Header) string { return h.Dispute }, writeDispute)
}

// LinkCorrection records a correction document against an issued invoice inside the caller's
// transaction (InvoiceCorrectionLinked). It never edits issued content; a FULL_CORRECTION moves the
// lifecycle ISSUED -> CORRECTED, which is the one lifecycle move an issued invoice may make.
func (m *Module) LinkCorrection(ctx context.Context, tx pgx.Tx, c kit.Caller, invoiceID, correctionID, kind string) error {
	_, err := m.linkCorrection(ctx, tx, c, invoiceID, correctionID, kind, "", "", nil)
	return err
}

func (m *Module) linkCorrection(ctx context.Context, tx pgx.Tx, c kit.Caller, invoiceID, correctionID, kind, reason, evidence string, expected *int) (*Header, error) {
	correctionID = strings.TrimSpace(correctionID)
	switch {
	case correctionID == "" || len(correctionID) > 255:
		return nil, kit.Invalid("correction_id is required (max 255 characters)")
	case !correctionKinds[kind]:
		return nil, kit.Invalid("kind must be CREDIT_NOTE, DEBIT_NOTE or FULL_CORRECTION")
	}
	h, err := lockIssued(ctx, tx, &c, invoiceID)
	if err != nil {
		return nil, err
	}
	if expected != nil && h.Version != *expected {
		return nil, kit.ErrStaleVersion
	}
	var exists int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM ar04_correction_links WHERE tenant_id = $1 AND invoice_id = $2 AND correction_id = $3`,
		c.TenantID, invoiceID, correctionID).Scan(&exists); err != nil {
		return nil, kit.MapPgErr(err)
	}
	if exists > 0 {
		return h, nil // idempotent replay: the link was already recorded
	}
	now := m.today()
	if _, err := tx.Exec(ctx, `INSERT INTO ar04_correction_links (link_id, tenant_id, legal_entity_id, invoice_id, correction_id, kind, reason_code, evidence_id, linked_by, linked_at)
		VALUES (gen_random_uuid(),$1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,$9)`,
		c.TenantID, h.LegalEntityID, invoiceID, correctionID, kind, reason, evidence, c.PrincipalID, now.UTC()); err != nil {
		return nil, kit.MapPgErr(err)
	}
	newState, newAttention := h.State, h.Attention
	if kind == KindFullCorrection {
		newState = StateCorrected
	}
	open, err := openCancellation(ctx, tx, c.TenantID, invoiceID, true)
	if err != nil {
		return nil, err
	}
	if open != nil && open.Status == "APPROVED" {
		if _, err := tx.Exec(ctx, `UPDATE ar04_cancellation_requests SET status = 'FULFILLED' WHERE tenant_id = $1 AND request_id = $2`, c.TenantID, open.RequestID); err != nil {
			return nil, kit.MapPgErr(err)
		}
		newAttention = AttentionCorrected
	}
	v, err := bump(ctx, tx, `UPDATE ar04_invoices SET lifecycle_state = $3, attention_state = $4, version = version + 1, updated_at = $5
		WHERE tenant_id = $1 AND invoice_id = $2 AND version = $6 RETURNING version`, c.TenantID, invoiceID, string(newState), newAttention, now.UTC(), h.Version)
	if err != nil {
		return nil, err
	}
	nh := *h
	nh.Version, nh.State, nh.Attention = v, newState, newAttention
	err = record(ctx, tx, c, &nh, hist{At: now, Command: "linkCorrectionDocument", Reason: reason, Evidence: evidence, ContentHash: h.IssuedContentHash,
		Prior: map[string]any{"lifecycle_state": h.State, "attention_state": h.Attention},
		New:   map[string]any{"lifecycle_state": newState, "attention_state": newAttention, "correction_id": correctionID, "kind": kind}},
		func(o *Header) []kit.Event {
			return []kit.Event{{AggregateType: AggregateType, AggregateID: o.InvoiceID, EventType: EventInvoiceCorrectionLinked,
				Payload: evPayload(c, o, map[string]any{"correction_id": correctionID, "kind": kind})}}
		})
	if err != nil {
		return nil, err
	}
	return &nh, nil
}

// ── kit.PostingListener ──────────────────────────────────────────────────────

// OnPostingOutcome mirrors the ACC-04 outcome (POSTED / FAILED / QUARANTINED + journal id) onto the
// invoice's accounting fields. It returns an error when the invoice cannot be found so the queue
// row stays claimed and is re-delivered: a lost outcome must stay visible.
func (m *Module) OnPostingOutcome(ctx context.Context, q kit.QueuedPosting, o kit.PostingOutcome) error {
	if q.SourceType != kit.SourceInvoice {
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
		return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: h.InvoiceID, Command: "accountingOutcome",
			Before: map[string]any{"accounting_state": h.AccountingState},
			After:  map[string]any{"accounting_state": o.Status, "journal_id": o.JournalID, "posting_execution_id": o.ExecutionID, "error": o.Error},
			Detail: map[string]any{"invoice_number": h.InvoiceNumber, "source_event_id": q.SourceEventID}})
	})
}
