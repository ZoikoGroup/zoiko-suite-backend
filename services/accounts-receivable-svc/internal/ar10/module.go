package ar10

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/ar04"
	"zoiko.io/accounts-receivable-svc/internal/ar08"
	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// ReceiptCashPort is the AR-08 hold port (implemented by *ar08.Module). The tx-taking methods commit atomically with AR-10's own change.
type ReceiptCashPort interface {
	GetReceiptCash(ctx context.Context, tenantID, receiptID string) (*ar08.ReceiptCash, error)
	ReserveUnapplied(ctx context.Context, tx pgx.Tx, c kit.Caller, receiptID, ownerRef string, amount kit.Amount) (*ar08.ReceiptCash, error)
	ReleaseUnapplied(ctx context.Context, tx pgx.Tx, c kit.Caller, ownerRef string) error
	ConsumeUnapplied(ctx context.Context, tx pgx.Tx, c kit.Caller, ownerRef string) error
	UnappliedHoldStatus(ctx context.Context, tenantID, ownerRef string) (status string, found bool, err error)
}

// ItemParties tells who handled an open item's dispute (implemented by *ar07.Module).
type ItemParties interface {
	DisputeParticipants(ctx context.Context, tenantID, openItemID string) (opener, resolver string, err error)
}

// Module is AR-10. It implements kit.PostingListener for kit.SourceRefund and kit.SourceWriteOff.
type Module struct {
	deps      kit.Deps
	items     kit.OpenItems       // AR-07; nil -> write-off create/commit answer 503, nothing changes
	periods   ar04.PeriodGate     // accounting-period-svc; nil -> CommitWriteOff answers 503 (refund settlement never waits on it)
	receipts  ReceiptCashPort     // AR-08 holds; nil -> every refund command that needs the source answers 503
	gateway   PaymentGateway      // BNK-06/07; nil -> submit / reconcile answer 503, nothing changes
	dest      DestinationVerifier // payee-banking-identity-svc; nil -> create / approve / submit answer 503
	parties   ItemParties         // AR-07 dispute participants; nil -> write-off approval answers 503 (SoD cannot be evaluated)
	authority AuthorityPolicy
}

var _ kit.PostingListener = (*Module)(nil)

// New builds the module. Pass a literal nil (never a typed-nil pointer) for a collaborator that is not wired. A nil authority
// uses ConstantTierPolicy over deps.Authz.
func New(deps kit.Deps, items kit.OpenItems, periods ar04.PeriodGate, receipts ReceiptCashPort, gateway PaymentGateway, dest DestinationVerifier,
	parties ItemParties, authority AuthorityPolicy) *Module {
	if authority == nil && deps.Authz != nil {
		authority = NewConstantTierPolicy(deps.Authz)
	}
	return &Module{deps: deps, items: items, periods: periods, receipts: receipts, gateway: gateway, dest: dest, parties: parties, authority: authority}
}

// Register mounts the spec paths verbatim at the router root.
func (m *Module) Register(r chi.Router) {
	for _, q := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"getRefund", m.getRefund},
		{"getRefundPaymentStatus", m.getRefundPaymentStatus},
		{"getWriteOff", m.getWriteOff},
		{"getAvailableActions", m.getAvailableActions},
		{"getRefundWriteOffHistory", m.getHistory},
		{"getControlExceptions", m.getControlExceptions},
	} {
		h := m.deps.Query(q.h)
		r.Get("/internal/v1/ar10/"+q.name, h)
		r.Post("/internal/v1/ar10/"+q.name, h)
	}
	for _, c := range []struct {
		path, op string
		h        http.HandlerFunc
	}{
		{"createRefundRequest", "AR10.CreateRefundRequest", m.createRefundRequest},
		{"changeRefundDestination", "AR10.ChangeRefundDestination", m.changeRefundDestination},
		{"approveRefund", "AR10.ApproveRefund", m.approveRefund},
		{"submitRefundForPayment", "AR10.SubmitRefundForPayment", m.submitRefundForPayment},
		{"reconcileRefundState", "AR10.ReconcileRefundState", m.reconcileRefundState},
		{"cancelUnsubmittedRefund", "AR10.CancelUnsubmittedRefund", m.cancelUnsubmittedRefund},
		{"createWriteOffRequest", "AR10.CreateWriteOffRequest", m.createWriteOffRequest},
		{"resolveWriteOffTaxTreatment", "AR10.ResolveWriteOffTaxTreatment", m.resolveWriteOffTaxTreatment},
		{"approveWriteOff", "AR10.ApproveWriteOff", m.approveWriteOff},
		{"commitWriteOff", "AR10.CommitWriteOff", m.commitWriteOff},
	} {
		r.Post("/internal/v1/ar10/commands/"+c.path, m.deps.Cmd(c.op, c.h))
	}
}

func (m *Module) read(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return m.deps.DB.WithTx(ctx, tenantID, fn)
}

func (m *Module) fail(w http.ResponseWriter, err error) {
	err = kit.MapPgErr(err)
	var p *kit.Problem
	if !errors.As(err, &p) {
		m.deps.Log.Error("ar10 request failed", zap.Error(err))
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

// denied records a refused privileged action as append-only evidence in its own transaction (the command's transaction rolled back)
// and returns the error to answer with. The command name carries the ".denied" suffix.
func (m *Module) denied(ctx context.Context, c kit.Caller, aggType, id, command string, err error, detail map[string]any) error {
	var p *kit.Problem
	if errors.As(err, &p) {
		if detail == nil {
			detail = map[string]any{}
		}
		detail["code"], detail["status"], detail["message"] = p.Code, p.Status, p.Msg
		if e := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
			return audit(ctx, tx, c, aggType, id, command+".denied", "", "", nil, nil, detail)
		}); e != nil {
			m.deps.Log.Error("ar10 could not record the denied action", zap.Error(e))
		}
	}
	return err
}

// period gates a write-off commit: the accounting period of today must accept the posting.
func (m *Module) period(ctx context.Context, c kit.Caller, at time.Time) (string, error) {
	if m.periods == nil {
		return "", kit.Unavailable("the accounting period gate is not wired: nothing was committed")
	}
	return m.periods.PostingPeriod(ctx, c, c.LegalEntityID, at)
}

// settlePeriod is the fiscal period a settled refund posts to. A settlement is a fact and is never refused because of AR's own
// period gate: when the gate cannot say, the calendar month is used and ACC-04 decides (a locked period stays PENDING with backoff).
func (m *Module) settlePeriod(ctx context.Context, c kit.Caller, at time.Time) string {
	if m.periods != nil {
		if pk, err := m.periods.PostingPeriod(ctx, c, c.LegalEntityID, at); err == nil && pk != "" {
			return pk
		}
	}
	return at.UTC().Format("2006-01")
}

// ── kit.PostingListener ──────────────────────────────────────────────────────

// OnPostingOutcome mirrors the ACC-04 outcome (POSTED / FAILED / QUARANTINED + journal id) onto the refund / write-off.
// q.SourceID is the aggregate id. It errors when the aggregate is not found so the queue row is re-delivered. A FAILED or
// QUARANTINED outcome stays visible through GetRefund / GetWriteOff / GetControlExceptions.
func (m *Module) OnPostingOutcome(ctx context.Context, q kit.QueuedPosting, o kit.PostingOutcome) error {
	if q.SourceType != kit.SourceRefund && q.SourceType != kit.SourceWriteOff {
		return nil
	}
	switch o.Status {
	case kit.PostingPosted, kit.PostingFailed, kit.PostingQuarantined:
	default:
		return nil
	}
	return m.deps.DB.WithTx(ctx, q.TenantID, func(tx pgx.Tx) error {
		table, col, aggType := "ar10_refunds", "refund_id", AggregateRefund
		if q.SourceType == kit.SourceWriteOff {
			table, col, aggType = "ar10_writeoffs", "writeoff_id", AggregateWriteOff
		}
		var entity, before string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id::text, COALESCE(accounting_state,'') FROM `+table+` WHERE tenant_id = $1 AND `+col+` = $2 FOR UPDATE`,
			q.TenantID, q.SourceID).Scan(&entity, &before); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return notFound("posting source")
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE `+table+` SET accounting_state = $3, journal_id = COALESCE(NULLIF($4,''), journal_id), accounting_error = NULLIF($5,''), updated_at = now()
			WHERE tenant_id = $1 AND `+col+` = $2`, q.TenantID, q.SourceID, o.Status, o.JournalID, o.Error); err != nil {
			return err
		}
		c := kit.Caller{TenantID: q.TenantID, PrincipalID: "system:acc-04", CorrelationID: q.CorrelationID, LegalEntityID: entity}
		return audit(ctx, tx, c, aggType, q.SourceID, "accountingOutcome", "", "", map[string]any{"accounting_state": before},
			map[string]any{"accounting_state": o.Status, "journal_id": o.JournalID, "posting_execution_id": o.ExecutionID, "error": o.Error},
			map[string]any{"source_type": q.SourceType, "source_event_id": q.SourceEventID})
	})
}

// ── control exceptions ───────────────────────────────────────────────────────

// RefundExceptions lists the visible control breaks of a refund: a settled refund whose AR consequence (consumed reservation) or
// accounting event is missing / failed (#32, #35), a stuck reservation on a failed refund, an UNKNOWN state, a return after settlement.
func (m *Module) refundExceptions(ctx context.Context, q querier, tenantID string, r *Refund) ([]ControlException, error) {
	out := []ControlException{}
	add := func(kind, detail string) {
		out = append(out, ControlException{Kind: kind, AggregateID: r.RefundID, Detail: detail})
	}
	if m.receipts != nil && (r.State == RfSettled || r.State == RfFailed || r.State == RfCancelled) {
		st, found, err := m.receipts.UnappliedHoldStatus(ctx, tenantID, "REFUND:"+r.RefundID)
		if err != nil {
			return nil, err
		}
		want := "RELEASED"
		if r.State == RfSettled {
			want = "CONSUMED"
		}
		if !found || st != want {
			add(controlConsequence, "the refund is "+r.State+" but the receipt's cash reservation is "+orNone(st, found)+" (expected "+want+")")
		}
	}
	if r.State == RfSettled {
		st, found, err := postingStatus(ctx, q, tenantID, r.RefundID)
		if err != nil {
			return nil, err
		}
		switch {
		case !found:
			add(controlNoPosting, "the settled refund has no accounting posting request")
		case st == kit.PostingFailed || st == kit.PostingQuarantined || r.AccountingState == kit.PostingFailed || r.AccountingState == kit.PostingQuarantined:
			add(controlPostingFailed, "the settled refund's accounting posting is "+st+": "+r.AccountingError)
		}
	}
	if r.State == RfUnknown {
		add(kit.CodeRefundStateUnknown, "the external payment state is unknown: reconcile with payment-status-svc before any further action")
	}
	if r.ControlException != "" {
		add(r.ControlException, "recorded on the refund")
	}
	return out, nil
}

func orNone(st string, found bool) string {
	if !found {
		return "missing"
	}
	return st
}

func (m *Module) writeOffExceptions(ctx context.Context, q querier, tenantID string, w *WriteOff) ([]ControlException, error) {
	out := []ControlException{}
	if w.State != WoCommitted {
		return out, nil
	}
	st, found, err := postingStatus(ctx, q, tenantID, w.WriteOffID)
	if err != nil {
		return nil, err
	}
	switch {
	case !found:
		out = append(out, ControlException{Kind: controlNoPosting, AggregateID: w.WriteOffID, Detail: "the committed write-off has no accounting posting request"})
	case st == kit.PostingFailed || st == kit.PostingQuarantined || w.AccountingState == kit.PostingFailed || w.AccountingState == kit.PostingQuarantined:
		out = append(out, ControlException{Kind: controlPostingFailed, AggregateID: w.WriteOffID, Detail: "the committed write-off's accounting posting is " + st + ": " + w.AccountingError})
	}
	return out, nil
}

// ControlExceptions lists the open control exceptions of a legal entity (refunds and write-offs), at most 200 aggregates each.
func (m *Module) ControlExceptions(ctx context.Context, tenantID, legalEntityID string) ([]ControlException, error) {
	out := []ControlException{}
	var refunds []*Refund
	var writeoffs []*WriteOff
	err := m.read(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+refundCols+` FROM ar10_refunds WHERE tenant_id = $1 AND legal_entity_id = $2 AND state IN ('UNKNOWN','SETTLED','FAILED','CANCELLED')
			ORDER BY updated_at DESC LIMIT 200`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		for rows.Next() {
			r, err := scanRefund(rows)
			if err != nil {
				rows.Close()
				return err
			}
			refunds = append(refunds, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT `+writeOffCols+` FROM ar10_writeoffs WHERE tenant_id = $1 AND legal_entity_id = $2 AND state = 'COMMITTED' ORDER BY updated_at DESC LIMIT 200`,
			tenantID, legalEntityID)
		if err != nil {
			return err
		}
		for rows.Next() {
			w, err := scanWriteOff(rows)
			if err != nil {
				rows.Close()
				return err
			}
			writeoffs = append(writeoffs, w)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, w := range writeoffs {
			ex, err := m.writeOffExceptions(ctx, tx, tenantID, w)
			if err != nil {
				return err
			}
			out = append(out, ex...)
		}
		return nil
	})
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	for _, r := range refunds {
		var ex []ControlException
		err := m.read(ctx, tenantID, func(tx pgx.Tx) (e error) { ex, e = m.refundExceptions(ctx, tx, tenantID, r); return })
		if err != nil {
			return nil, kit.MapPgErr(err)
		}
		out = append(out, ex...)
	}
	return out, nil
}
