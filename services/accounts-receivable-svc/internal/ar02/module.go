package ar02

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
)

// OrderLine is a quoted line handed to the order service.
type OrderLine struct {
	LineNo      int
	Description string
	ProductRef  string
	Quantity    kit.Amount
	UnitPrice   kit.Amount
	LineNet     kit.Amount
}

// OrderFromQuote is the commitment AR-03 turns into a sales order.
type OrderFromQuote struct {
	QuoteID         string
	QuoteNumber     string
	VersionNo       int
	ContentHash     string
	LegalEntityID   string
	CustomerPartyID string
	Currency        string
	Lines           []OrderLine
	Subtotal        kit.Amount
	DiscountPercent kit.Amount
	DiscountAmount  kit.Amount
	NetTotal        kit.Amount
	PriceSourceRef  string
	Terms           string
	// Ref is the stable source reference "quote:<quote_id>:v<version_no>". An
	// implementation must be idempotent on it: a repeat returns the same order.
	Ref string
}

// OrderCreator is implemented by AR-03 (sales orders). CreateFromQuote runs inside
// the conversion transaction, so the order and the quote's CONVERTED state commit
// together or not at all. It must be idempotent on in.Ref.
type OrderCreator interface {
	CreateFromQuote(ctx context.Context, tx pgx.Tx, c kit.Caller, in OrderFromQuote) (orderID string, err error)
}

// Module is AR-02.
type Module struct {
	deps     kit.Deps
	profiles kit.ProfileResolver // AR-01
	tax      taxdet.Client       // optional estimate source; nil = estimates unavailable
	orders   OrderCreator        // AR-03; nil until wired -> ConvertQuoteToOrder is 503
}

// New builds the module. profiles is required (AR-01's Module implements it). tax
// may be nil (a quote that requests an estimate then cannot be approved). orders
// may be nil until AR-03 exists; conversion then answers 503 and changes nothing.
func New(deps kit.Deps, profiles kit.ProfileResolver, tax taxdet.Client, orders OrderCreator) *Module {
	return &Module{deps: deps, profiles: profiles, tax: tax, orders: orders}
}

// Register mounts the spec paths verbatim at the router root.
func (m *Module) Register(r chi.Router) {
	for _, q := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"getQuote", m.getQuote},
		{"getQuoteVersion", m.getQuoteVersion},
		{"listQuotes", m.listQuotes},
		{"getAvailableActions", m.getAvailableActions},
		{"getQuoteHistory", m.getQuoteHistory},
	} {
		h := m.deps.Query(q.h)
		r.Get("/internal/v1/ar02/"+q.name, h)
		r.Post("/internal/v1/ar02/"+q.name, h)
	}
	for _, c := range []struct {
		path, op string
		h        http.HandlerFunc
	}{
		{"createQuote", "AR02.CreateQuote", m.createQuote},
		{"amendQuote", "AR02.AmendQuote", m.amendQuote},
		{"submitQuoteForApproval", "AR02.SubmitQuoteForApproval", m.submitQuoteForApproval},
		{"approveQuote", "AR02.ApproveQuote", m.approveQuote},
		{"sendQuote", "AR02.SendQuote", m.sendQuote},
		{"recordAcceptance", "AR02.RecordAcceptance", m.recordAcceptance},
		{"rejectQuote", "AR02.RejectQuote", m.rejectQuote},
		{"expireQuote", "AR02.ExpireQuote", m.expireQuote},
		{"convertQuoteToOrder", "AR02.ConvertQuoteToOrder", m.convertQuoteToOrder},
	} {
		r.Post("/internal/v1/ar02/commands/"+c.path, m.deps.Cmd(c.op, c.h))
	}
}

// read runs fn in a tenant-scoped transaction (sets app.tenant_id so FORCE'd RLS binds).
func (m *Module) read(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return m.deps.DB.WithTx(ctx, tenantID, fn)
}

func (m *Module) today() time.Time { return m.deps.Now() }

// profile resolves the customer's ACTIVE financial profile at the quote date via
// AR-01. No answer is never treated as "active": an unreachable resolver is 503.
func (m *Module) profile(ctx context.Context, tenantID string, h *Header) (*kit.ProfileView, error) {
	if m.profiles == nil {
		return nil, kit.Unavailable("customer financial profile service not wired")
	}
	v, err := m.profiles.ActiveAt(ctx, tenantID, h.LegalEntityID, h.CustomerPartyID, m.today())
	if err != nil {
		var p *kit.Problem
		if errors.As(err, &p) {
			return nil, err
		}
		m.deps.Log.Error("ar02 profile resolution failed", zap.Error(err))
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
		m.deps.Log.Error("ar02 request failed", zap.Error(err))
	}
	kit.WriteErr(w, err)
}
