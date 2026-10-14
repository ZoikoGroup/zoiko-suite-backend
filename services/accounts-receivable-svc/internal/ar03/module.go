package ar03

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/ar02"
	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Module is AR-03. It implements kit.Orders (consumed by AR-04) and ar02.OrderCreator
// (consumed by AR-02's ConvertQuoteToOrder).
type Module struct {
	deps      kit.Deps
	profiles  kit.ProfileResolver // AR-01
	openItems kit.OpenItems       // AR-07; nil until it is wired -> credit exposure is reported unavailable, never guessed
}

var (
	_ kit.Orders        = (*Module)(nil)
	_ ar02.OrderCreator = (*Module)(nil)
)

// New builds the module. profiles is required (AR-01's Module implements it). openItems
// may be nil until AR-07 exists: confirmation then compares only the order's own value
// with the credit limit and records exposure_available=false in the evidence.
func New(deps kit.Deps, profiles kit.ProfileResolver, openItems kit.OpenItems) *Module {
	return &Module{deps: deps, profiles: profiles, openItems: openItems}
}

// Register mounts the spec paths verbatim at the router root.
func (m *Module) Register(r chi.Router) {
	for _, q := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"getSalesOrder", m.getSalesOrder},
		{"getOrderBillingStatus", m.getOrderBillingStatus},
		{"getInvoiceEligibility", m.getInvoiceEligibility},
		{"getAvailableActions", m.getAvailableActions},
		{"getOrderHistory", m.getOrderHistory},
	} {
		h := m.deps.Query(q.h)
		r.Get("/internal/v1/ar03/"+q.name, h)
		r.Post("/internal/v1/ar03/"+q.name, h)
	}
	for _, c := range []struct {
		path, op string
		h        http.HandlerFunc
	}{
		{"createSalesOrder", "AR03.CreateSalesOrder", m.createSalesOrder},
		{"amendSalesOrder", "AR03.AmendSalesOrder", m.amendSalesOrder},
		{"submitOrder", "AR03.SubmitOrder", m.submitOrder},
		{"approveOrder", "AR03.ApproveOrder", m.approveOrder},
		{"confirmOrder", "AR03.ConfirmOrder", m.confirmOrder},
		{"placeOrderHold", "AR03.PlaceOrderHold", m.placeOrderHold},
		{"releaseOrderHold", "AR03.ReleaseOrderHold", m.releaseOrderHold},
		{"cancelOrder", "AR03.CancelOrder", m.cancelOrder},
		{"recordBillableMilestone", "AR03.RecordBillableMilestone", m.recordBillableMilestone},
		{"closeOrder", "AR03.CloseOrder", m.closeOrder},
	} {
		r.Post("/internal/v1/ar03/commands/"+c.path, m.deps.Cmd(c.op, c.h))
	}
}

// read runs fn in a tenant-scoped transaction (sets app.tenant_id so FORCE'd RLS binds).
func (m *Module) read(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return m.deps.DB.WithTx(ctx, tenantID, fn)
}

func (m *Module) today() time.Time { return m.deps.Now() }

// profile resolves the customer's ACTIVE financial profile via AR-01. No answer is never
// treated as "active": an unreachable resolver is 503.
func (m *Module) profile(ctx context.Context, tenantID, entityID, partyID string) (*kit.ProfileView, error) {
	if m.profiles == nil {
		return nil, kit.Unavailable("customer financial profile service not wired")
	}
	v, err := m.profiles.ActiveAt(ctx, tenantID, entityID, partyID, m.today())
	if err != nil {
		var p *kit.Problem
		if errors.As(err, &p) {
			return nil, err
		}
		m.deps.Log.Error("ar03 profile resolution failed", zap.Error(err))
		return nil, kit.Unavailable("customer financial profile unavailable")
	}
	if v == nil {
		return nil, kit.Unprocessable(kit.CodeCustomerProfileInactive, "no active financial profile for this customer")
	}
	return v, nil
}

// creditCheck evaluates the order against the credit profile. Exposure comes from AR-07
// when it is wired and a limit exists; otherwise it is reported unavailable, never assumed.
func (m *Module) creditCheck(ctx context.Context, h *Header, view *kit.ProfileView) (CreditResult, error) {
	var exposure kit.Amount
	available := false
	if m.openItems != nil && view.CreditLimit.IsPositive() && (view.CreditCurrency == "" || view.CreditCurrency == h.Currency) {
		e, err := m.openItems.Exposure(ctx, h.TenantID, h.LegalEntityID, h.CustomerPartyID, h.Currency)
		if err != nil {
			var p *kit.Problem
			if errors.As(err, &p) {
				return CreditResult{}, err
			}
			m.deps.Log.Error("ar03 credit exposure unavailable - failing closed", zap.Error(err))
			return CreditResult{}, kit.Unavailable("receivable exposure unavailable: the credit limit cannot be verified")
		}
		exposure, available = e, true
	}
	return EvaluateCredit(view, h.Currency, h.NetTotal, exposure, available), nil
}

func (m *Module) fail(w http.ResponseWriter, err error) {
	var p *kit.Problem
	if !errors.As(err, &p) {
		m.deps.Log.Error("ar03 request failed", zap.Error(err))
	}
	kit.WriteErr(w, err)
}

// checkAction is Authorize without writing the response (used inside step hooks).
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

// denial is a refusal that leaves audit evidence (<command>.denied) of the attempt: it
// wraps the stable Problem so WriteErr still answers with its status and code.
type denial struct {
	*kit.Problem
	Command string
	Detail  map[string]any
}

func (d *denial) Unwrap() error { return d.Problem }

func deny(p *kit.Problem, command string, detail map[string]any) error {
	return &denial{Problem: p, Command: command, Detail: detail}
}

// recordDenial keeps evidence of a refused attempt (the command wrote nothing else, so the
// audit is its own transaction).
func (m *Module) recordDenial(ctx context.Context, c kit.Caller, orderID string, d *denial) {
	detail := map[string]any{"denial": d.Code}
	for k, v := range d.Detail {
		detail[k] = v
	}
	_ = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: orderID, Command: d.Command + ".denied", Detail: detail})
	})
}

// ── kit.Orders (consumed by AR-04) ───────────────────────────────────────────

// InvoiceEligibility returns the billable basis of one milestone, or ORDER_NOT_BILLABLE as
// a *kit.Problem for a cancelled, held, unconfirmed, finished order, an unknown / voided /
// already-billed milestone, or a milestone that would bill beyond the line's remaining
// quantity or amount. An unknown order is NOT_FOUND (cross-tenant looks identical).
func (m *Module) InvoiceEligibility(ctx context.Context, tenantID, orderID, milestoneID string) (*kit.BillingBasis, error) {
	if err := mustUUID("order_id", orderID); err != nil {
		return nil, err
	}
	var basis *kit.BillingBasis
	err := m.read(ctx, tenantID, func(tx pgx.Tx) error {
		h, err := getHeader(ctx, tx, tenantID, orderID, false)
		if err != nil {
			return err
		}
		if strings.TrimSpace(milestoneID) == "" {
			return errNotBillable("a recorded billable milestone is required: direct-amount billing is not supported")
		}
		ms, err := getMilestone(ctx, tx, tenantID, orderID, milestoneID, false)
		if err != nil {
			return err
		}
		if p := CheckBillable(h, ms); p != nil {
			return p
		}
		lines, err := getLines(ctx, tx, tenantID, orderID, false)
		if err != nil {
			return err
		}
		if p := checkWithinRemaining(lines, ms, ms.ScheduledAmount); p != nil {
			return p
		}
		basis = &kit.BillingBasis{OrderID: orderID, MilestoneID: ms.MilestoneID, CustomerParty: h.CustomerPartyID, Currency: h.Currency,
			EligibleNet: ms.ScheduledAmount, Version: h.Version}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return basis, nil
}

// checkWithinRemaining refuses a billing movement beyond the line's remaining amount or quantity.
func checkWithinRemaining(lines []Line, ms *Milestone, net kit.Amount) *kit.Problem {
	for _, l := range lines {
		if l.LineNo != ms.LineNo {
			continue
		}
		if net > l.RemainingAmount || ms.ScheduledQty > l.RemainingQty {
			return errNotBillable("billing would exceed the remaining eligible quantity or amount of the order line")
		}
		return nil
	}
	return errNotBillable("the milestone's order line no longer exists")
}

// RecordInvoiced moves invoiced / remaining inside the caller's (invoice issue) transaction.
// It is idempotent per (milestone, invoiceID): a replay changes nothing. It refuses, with
// ORDER_NOT_BILLABLE, anything InvoiceEligibility refuses and any net that is not exactly
// the milestone's eligible net or exceeds the line's remaining amount. When the order
// crosses a threshold SalesOrderPartiallyBilled / SalesOrderFullyBilled is emitted in the
// same transaction.
func (m *Module) RecordInvoiced(ctx context.Context, tx pgx.Tx, c kit.Caller, orderID, milestoneID, invoiceID string, net kit.Amount) error {
	if err := mustUUID("order_id", orderID); err != nil {
		return err
	}
	if strings.TrimSpace(invoiceID) == "" || len(invoiceID) > 255 {
		return kit.Invalid("invoice_id is required")
	}
	if !net.IsPositive() {
		return kit.Invalid("the invoiced net must be greater than zero")
	}
	h, err := getHeader(ctx, tx, c.TenantID, orderID, true) // serialises every invoicing of this order
	if err != nil {
		return err
	}
	if strings.TrimSpace(milestoneID) == "" {
		return errNotBillable("a recorded billable milestone is required: direct-amount billing is not supported")
	}
	if err := mustUUID("milestone_id", milestoneID); err != nil {
		return errNotBillable("the billable milestone is unknown for this order")
	}
	var seen int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM ar03_invoiced_ledger WHERE tenant_id = $1 AND order_id = $2 AND milestone_id = $3 AND invoice_id = $4`,
		c.TenantID, orderID, milestoneID, invoiceID).Scan(&seen); err != nil {
		return kit.MapPgErr(err)
	}
	if seen > 0 {
		return nil // idempotent replay: the movement was already applied
	}
	ms, err := getMilestone(ctx, tx, c.TenantID, orderID, milestoneID, true)
	if err != nil {
		return err
	}
	if p := CheckBillable(h, ms); p != nil {
		return p
	}
	if net != ms.ScheduledAmount {
		return errNotBillable("the invoiced net " + net.String() + " is not the milestone's eligible net " + ms.ScheduledAmount.String())
	}
	if !net.FitsCurrency(h.Currency) {
		return kit.Invalid("the invoiced net has more precision than the currency allows")
	}
	lines, err := getLines(ctx, tx, c.TenantID, orderID, true)
	if err != nil {
		return err
	}
	if p := checkWithinRemaining(lines, ms, net); p != nil {
		return p
	}
	now := m.today()
	if _, err := tx.Exec(ctx, `UPDATE ar03_order_lines SET invoiced_qty = invoiced_qty + $4::numeric, invoiced_amount = invoiced_amount + $5::numeric
		WHERE tenant_id = $1 AND order_id = $2 AND line_no = $3`, c.TenantID, orderID, ms.LineNo, ms.ScheduledQty.String(), net.String()); err != nil {
		return mapBilledErr(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE ar03_billing_milestones SET state = 'INVOICED', invoice_id = $3, invoiced_net = $4::numeric, invoiced_at = $5
		WHERE tenant_id = $1 AND milestone_id = $2`, c.TenantID, milestoneID, invoiceID, net.String(), now.UTC()); err != nil {
		return kit.MapPgErr(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ar03_invoiced_ledger (ledger_id, tenant_id, legal_entity_id, order_id, milestone_id, invoice_id, net, recorded_by, recorded_at)
		VALUES (gen_random_uuid(),$1,$2,$3,$4,$5,$6::numeric,$7,$8)`, c.TenantID, h.LegalEntityID, orderID, milestoneID, invoiceID, net.String(), c.PrincipalID, now.UTC()); err != nil {
		if kit.IsUniqueViolation(err, "") {
			return errNotBillable("the milestone is already billed")
		}
		return kit.MapPgErr(err)
	}
	after, err := getLines(ctx, tx, c.TenantID, orderID, false)
	if err != nil {
		return err
	}
	sum := Summarize(after)
	c.LegalEntityID = h.LegalEntityID
	nh := *h
	nh.State = StateAfterBilling(h.State, sum)
	payload := map[string]any{"milestone_id": milestoneID, "invoice_id": invoiceID, "net": net.String(), "invoiced_net": sum.InvoicedNet.String(),
		"remaining_net": sum.RemainingNet.String(), "line_no": ms.LineNo}
	_, err = commit(ctx, tx, c, h, &nh, hist{At: now, Command: "recordInvoiced", ContentHash: "",
		Prior: map[string]any{"state": h.State, "invoiced_net": Summarize(lines).InvoicedNet.String()},
		New: map[string]any{"state": nh.State, "milestone_id": milestoneID, "invoice_id": invoiceID, "net": net.String(), "invoiced_net": sum.InvoicedNet.String(),
			"remaining_net": sum.RemainingNet.String()}},
		func(o *Header) []kit.Event {
			var ev []kit.Event
			switch {
			case nh.State == StateFullyBilled:
				ev = append(ev, kit.Event{AggregateType: AggregateType, AggregateID: o.OrderID, EventType: EventSalesOrderFullyBilled, Payload: evPayload(c, o, payload)})
			case nh.State == StatePartiallyBilled && h.State != StatePartiallyBilled:
				ev = append(ev, kit.Event{AggregateType: AggregateType, AggregateID: o.OrderID, EventType: EventSalesOrderPartiallyBilled, Payload: evPayload(c, o, payload)})
			}
			return ev
		})
	return err
}

// ── ar02.OrderCreator (consumed by AR-02 ConvertQuoteToOrder) ────────────────

// CreateFromQuote creates a DRAFT order carrying the quote's lines, prices, discount,
// terms and source reference inside AR-02's conversion transaction. It is idempotent on
// in.Ref (quote:<id>:v<n>): a second call returns the original order id, never a second order.
func (m *Module) CreateFromQuote(ctx context.Context, tx pgx.Tx, c kit.Caller, in ar02.OrderFromQuote) (string, error) {
	ref := strings.TrimSpace(in.Ref)
	if ref == "" || len(ref) > 255 || !refRe.MatchString(ref) {
		return "", kit.Invalid("the source reference quote:<id>:v<n> is required")
	}
	if err := mustUUID("customer_party_id", in.CustomerPartyID); err != nil {
		return "", err
	}
	if err := mustUUID("legal_entity_id", in.LegalEntityID); err != nil {
		return "", err
	}
	// Serialise concurrent conversions of the same source so the unique reference is never raced.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 301))`, c.TenantID+"|"+ref); err != nil {
		return "", err
	}
	if existing, err := getHeaderBySourceRef(ctx, tx, c.TenantID, ref); err != nil {
		return "", err
	} else if existing != nil {
		return existing.OrderID, nil
	}
	inputs := make([]LineInput, len(in.Lines))
	for i, l := range in.Lines {
		inputs[i] = LineInput{Description: l.Description, ProductRef: l.ProductRef, Quantity: l.Quantity, UnitPrice: l.UnitPrice}
	}
	lines, err := BuildLines(in.Currency, inputs)
	if err != nil {
		return "", err
	}
	for i := range lines {
		if lines[i].ListNet != in.Lines[i].LineNet {
			return "", kit.Invalid("a quote line net does not equal quantity x unit price: the quote cannot be converted")
		}
	}
	totals, err := ApplyDiscountAmount(in.Currency, lines, in.DiscountAmount)
	if err != nil {
		return "", err
	}
	if totals.NetTotal != in.NetTotal || totals.Subtotal != in.Subtotal {
		return "", kit.Invalid("the quote totals do not match its lines: the quote cannot be converted")
	}
	content := Content{Currency: in.Currency, Terms: in.Terms, PriceSourceRef: in.PriceSourceRef, DiscountPercent: in.DiscountPercent, Lines: lines}
	if err := content.Validate(); err != nil {
		return "", err
	}
	c.LegalEntityID = in.LegalEntityID
	h, err := createOrderTx(ctx, tx, c, createInput{Content: content, Totals: totals, CustomerPartyID: in.CustomerPartyID, SourceQuoteRef: ref, SourceRef: ref,
		Command: "createFromQuote", Evidence: ref}, m.today())
	if err != nil {
		return "", err
	}
	return h.OrderID, nil
}
