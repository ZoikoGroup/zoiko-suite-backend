package ar07

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Module is AR-07. It implements kit.OpenItems (consumed by AR-04/06/08/09/10).
type Module struct {
	deps     kit.Deps
	invoices kit.Invoices // AR-04; late-bound (AR-04 needs AR-07 to be built first); nil = the invoice dimension is not mirrored
}

var _ kit.OpenItems = (*Module)(nil)

// New builds the module. Bind AR-04 afterwards with SetInvoices.
func New(deps kit.Deps) *Module { return &Module{deps: deps} }

// SetInvoices late-binds the AR-04 invoice dimension writer (settlement / dispute mirroring inside the adjustment tx).
// Call it once during wiring, before serving; nil disables the mirror.
func (m *Module) SetInvoices(inv kit.Invoices) { m.invoices = inv }

// Register mounts the spec paths verbatim at the router root.
func (m *Module) Register(r chi.Router) {
	for _, q := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"getOpenItem", m.getOpenItem},
		{"getCustomerReceivables", m.getCustomerReceivables},
		{"getAging", m.getAging},
		{"getBalanceAsOf", m.getBalanceAsOf},
		{"getOpenItemHistory", m.getOpenItemHistory},
		{"getAvailableActions", m.getAvailableActions},
	} {
		h := m.deps.Query(q.h)
		r.Get("/internal/v1/ar07/"+q.name, h)
		r.Post("/internal/v1/ar07/"+q.name, h)
	}
	for _, c := range []struct {
		path, op string
		h        http.HandlerFunc
	}{
		{"createOpenItemFromDocument", "AR07.CreateOpenItemFromDocument", m.createOpenItemFromDocument},
		{"applyReceivableAdjustment", "AR07.ApplyReceivableAdjustment", m.applyReceivableAdjustment},
		{"openDispute", "AR07.OpenDispute", m.openDisputeCmd},
		{"resolveDispute", "AR07.ResolveDispute", m.resolveDisputeCmd},
		{"placeCollectionHold", "AR07.PlaceCollectionHold", m.placeCollectionHold},
		{"releaseCollectionHold", "AR07.ReleaseCollectionHold", m.releaseCollectionHold},
		{"closeOpenItem", "AR07.CloseOpenItem", m.closeOpenItem},
	} {
		r.Post("/internal/v1/ar07/commands/"+c.path, m.deps.Cmd(c.op, c.h))
	}
}

func (m *Module) read(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return m.deps.DB.WithTx(ctx, tenantID, fn)
}

func (m *Module) fail(w http.ResponseWriter, err error) {
	err = kit.MapPgErr(err)
	var p *kit.Problem
	if !errors.As(err, &p) {
		m.deps.Log.Error("ar07 request failed", zap.Error(err))
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

// ---- kit.OpenItems ----

// CreateFromDocument opens the receivable of an issued invoice / debit note inside the caller's transaction. c.LegalEntityID
// must be the document's legal entity. A replay for the same document returns the existing item.
func (m *Module) CreateFromDocument(ctx context.Context, tx pgx.Tx, c kit.Caller, in kit.OpenItemFromDocument) (kit.OpenItemRef, error) {
	o, _, err := m.createOpenItem(ctx, tx, c, in, "")
	if err != nil {
		return kit.OpenItemRef{}, err
	}
	return kit.OpenItemRef{OpenItemID: o.OpenItemID, Version: o.Version}, nil
}

// ApplyAdjustment is the port writer of a residual. Replays of in.EventKey are no-ops returning the current view.
func (m *Module) ApplyAdjustment(ctx context.Context, tx pgx.Tx, c kit.Caller, in kit.OpenItemAdjustment) (*kit.OpenItemView, error) {
	o, _, err := m.applyAdjustment(ctx, tx, c, in, adjustExtra{})
	if err != nil {
		return nil, err
	}
	return o.View(), nil
}

func (m *Module) GetByID(ctx context.Context, tenantID, openItemID string) (*kit.OpenItemView, error) {
	var o *OpenItem
	err := m.read(ctx, tenantID, func(tx pgx.Tx) (err error) { o, err = getItem(ctx, tx, tenantID, openItemID, false); return })
	if err != nil {
		return nil, err
	}
	return o.View(), nil
}

func (m *Module) GetBySourceDocument(ctx context.Context, tenantID, sourceDocumentID string) (*kit.OpenItemView, error) {
	var o *OpenItem
	err := m.read(ctx, tenantID, func(tx pgx.Tx) (err error) { o, err = getItemBySource(ctx, tx, tenantID, sourceDocumentID); return })
	if err != nil {
		return nil, err
	}
	return o.View(), nil
}

// Exposure is the customer's total residual (all open items, disputed ones included) in one currency.
func (m *Module) Exposure(ctx context.Context, tenantID, legalEntityID, customerPartyID, currency string) (kit.Amount, error) {
	if currency == "" {
		return 0, kit.Invalid("currency is required: exposure is never summed across currencies")
	}
	var sum kit.Amount
	err := m.read(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(sum(residual_amount),0) FROM ar07_open_items
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND customer_party_id = $3 AND currency = $4`, tenantID, legalEntityID, customerPartyID, currency).Scan(&sum)
	})
	return sum, kit.MapPgErr(err)
}

// CollectionBlocked reports whether any of the customer's open items carries an
// active dispute or a collection hold (kit.OpenItems contract: collection
// contact must be suppressed on either).
func (m *Module) CollectionBlocked(ctx context.Context, tenantID, legalEntityID, customerPartyID string) (bool, error) {
	var held bool
	err := m.read(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ar07_open_items WHERE tenant_id = $1 AND ($2 = '' OR legal_entity_id::text = $2)
			AND customer_party_id = $3 AND (collection_hold OR disputed) AND closed_at IS NULL)`, tenantID, legalEntityID, customerPartyID).Scan(&held)
	})
	return held, kit.MapPgErr(err)
}

// ListOpenForCustomer lists the customer's open items with a residual (oldest due first), for AR-08 cash application.
// currency "" lists every currency.
func (m *Module) ListOpenForCustomer(ctx context.Context, tenantID, legalEntityID, customerPartyID, currency string) ([]kit.OpenItemView, error) {
	out := []kit.OpenItemView{}
	err := m.read(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+itemCols+` FROM ar07_open_items
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND customer_party_id = $3 AND ($4::text = '' OR currency = $4) AND residual_amount > 0 AND closed_at IS NULL
			ORDER BY due_date, issue_date, open_item_id`, tenantID, legalEntityID, customerPartyID, currency)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanItem(rows)
			if err != nil {
				return err
			}
			out = append(out, *o.View())
		}
		return rows.Err()
	})
	return out, kit.MapPgErr(err)
}

// DisputeParticipants returns who opened and who resolved the item's latest dispute ("" when none / unresolved), for
// AR-10's write-off segregation of duties.
func (m *Module) DisputeParticipants(ctx context.Context, tenantID, openItemID string) (opener, resolver string, err error) {
	if err := mustUUID("open_item_id", openItemID); err != nil {
		return "", "", err
	}
	err = m.read(ctx, tenantID, func(tx pgx.Tx) (e error) {
		opener, resolver, e = disputeParticipants(ctx, tx, tenantID, openItemID)
		return
	})
	return opener, resolver, kit.MapPgErr(err)
}

// ---- AR -> GL completeness control ----

// ExpectedDocument is one issued document the receivable subledger must carry exactly once.
type ExpectedDocument struct {
	DocumentID string
	Currency   string
	Amount     kit.Amount // gross
}

// CompletenessReport compares the distinct issued documents with the open items of a legal entity: count AND sum per
// currency AND per document, so an AR total that matches the GL while an open item is duplicated or missing is caught (#21).
type CompletenessReport struct {
	ExpectedCount      int                   `json:"expected_count"`
	ExpectedSum        map[string]kit.Amount `json:"expected_sum"`
	OpenItemRows       int                   `json:"open_item_rows"`
	OpenItemDistinct   int                   `json:"open_item_distinct_documents"`
	OpenItemSum        map[string]kit.Amount `json:"open_item_sum"`
	Missing            []string              `json:"missing_open_items"`
	Unexpected         []string              `json:"unexpected_open_items"`
	AmountMismatch     []string              `json:"amount_mismatch"`
	DuplicateDocuments []string              `json:"duplicate_documents"`
	Balanced           bool                  `json:"balanced"`
}

// Err is the explicit control exception for an unbalanced report (no silent plug).
func (r *CompletenessReport) Err() error {
	if r.Balanced {
		return nil
	}
	return kit.Conflict(kit.CodeARGLReconciliationBreak, "issued documents and receivable open items do not reconcile (count, sum or per-document)")
}

// Completeness reads the legal entity's open items in one snapshot and compares them with docs, which must be the FULL
// set of issued invoices / debit notes of that entity (AR-04 provides it).
func (m *Module) Completeness(ctx context.Context, tenantID, legalEntityID string, docs []ExpectedDocument) (*CompletenessReport, error) {
	rep := &CompletenessReport{ExpectedSum: map[string]kit.Amount{}, OpenItemSum: map[string]kit.Amount{}, Missing: []string{}, Unexpected: []string{},
		AmountMismatch: []string{}, DuplicateDocuments: []string{}}
	want := map[string]ExpectedDocument{}
	for _, d := range docs {
		if _, dup := want[d.DocumentID]; dup {
			rep.DuplicateDocuments = append(rep.DuplicateDocuments, d.DocumentID)
			continue
		}
		want[d.DocumentID] = d
		rep.ExpectedSum[d.Currency] = rep.ExpectedSum[d.Currency].Add(d.Amount)
	}
	rep.ExpectedCount = len(want)
	have := map[string]int{}
	err := m.deps.DB.WithTxOpts(ctx, tenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT source_document_id::text, currency, original_amount FROM ar07_open_items WHERE tenant_id = $1 AND legal_entity_id = $2`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, cur string
			var amt kit.Amount
			if err := rows.Scan(&id, &cur, &amt); err != nil {
				return err
			}
			rep.OpenItemRows++
			have[id]++
			rep.OpenItemSum[cur] = rep.OpenItemSum[cur].Add(amt)
			if w, ok := want[id]; ok && (w.Amount != amt || w.Currency != cur) {
				rep.AmountMismatch = append(rep.AmountMismatch, id)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	rep.OpenItemDistinct = len(have)
	for id, n := range have {
		if _, ok := want[id]; !ok {
			rep.Unexpected = append(rep.Unexpected, id)
		}
		if n > 1 {
			rep.DuplicateDocuments = append(rep.DuplicateDocuments, id)
		}
	}
	for id := range want {
		if have[id] == 0 {
			rep.Missing = append(rep.Missing, id)
		}
	}
	for _, s := range [][]string{rep.Missing, rep.Unexpected, rep.AmountMismatch, rep.DuplicateDocuments} {
		sort.Strings(s)
	}
	rep.Balanced = len(rep.Missing)+len(rep.Unexpected)+len(rep.AmountMismatch)+len(rep.DuplicateDocuments) == 0 &&
		rep.ExpectedCount == rep.OpenItemRows && rep.OpenItemRows == rep.OpenItemDistinct && sumsEqual(rep.ExpectedSum, rep.OpenItemSum)
	return rep, nil
}

func sumsEqual(a, b map[string]kit.Amount) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func asProblem(err error, target **kit.Problem) bool { return errors.As(err, target) }
