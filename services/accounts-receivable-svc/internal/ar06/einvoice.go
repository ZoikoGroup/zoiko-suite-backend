package ar06

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/ar05"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// noteSource is the ar05.DocumentSource of one note kind. main.go registers it in ar05.Collaborators.Sources:
//
//	Sources: map[string]ar05.DocumentSource{kit.DocCreditNote: ar06m.EInvoiceSource(ar06.KindCredit), kit.DocDebitNote: ar06m.EInvoiceSource(ar06.KindDebit)}
type noteSource struct {
	m    *Module
	kind Kind
}

var _ ar05.DocumentSource = noteSource{}

// EInvoiceSource returns the document source for CREDIT_NOTE / DEBIT_NOTE e-invoicing.
func (m *Module) EInvoiceSource(kind Kind) ar05.DocumentSource { return noteSource{m: m, kind: kind} }

// Get returns the immutable issued representation; anything not issued (or of the other kind) is NOT_FOUND, so a draft can never be transmitted.
func (s noteSource) Get(ctx context.Context, tenantID, documentID string) (*ar05.SourceDocument, error) {
	var cor *Correction
	err := s.m.read(ctx, tenantID, func(tx pgx.Tx) error {
		h, err := getHeader(ctx, tx, tenantID, documentID, false)
		if err != nil {
			return err
		}
		if !h.State.IsIssued() || h.Kind != s.kind {
			return kit.NotFound("document not found")
		}
		cor, err = loadCorrection(ctx, tx, tenantID, h)
		return err
	})
	if err != nil {
		return nil, err
	}
	issue, _ := time.Parse("2006-01-02", cor.IssueDate)
	due, _ := time.Parse("2006-01-02", cor.DueDate)
	if due.IsZero() {
		due = issue // a credit note has no payment due date
	}
	var ids, packs []string
	seen := map[string]bool{}
	for _, g := range cor.TaxGroups {
		ids = append(ids, g.DeterminationID)
		if !seen[g.Provenance] {
			seen[g.Provenance] = true
			packs = append(packs, g.Provenance)
		}
	}
	d := &ar05.SourceDocument{DocumentType: string(s.kind), DocumentID: cor.CorrectionID, LegalEntityID: cor.LegalEntityID, CustomerPartyID: cor.CustomerPartyID,
		Number: cor.DocumentNumber, Currency: cor.Currency, Net: cor.NetTotal, Tax: cor.TaxTotal, Gross: cor.GrossTotal, IssueDate: issue, DueDate: due,
		TaxDeterminationID: strings.Join(ids, ","), TaxRulePackVersion: strings.Join(packs, ","), ContentHash: cor.IssuedContentHash}
	for _, l := range cor.Lines {
		d.Lines = append(d.Lines, ar05.SourceLine{LineNumber: l.LineNo, Description: l.Description, Quantity: l.Quantity, UnitPrice: l.UnitPrice, Net: l.Net, Tax: l.Tax, TaxCode: l.TaxCategory})
	}
	return d, nil
}

// SetEInvoiceState mirrors the AR-05 submission state onto the note's e-invoice dimension ONLY (never the issued content).
func (s noteSource) SetEInvoiceState(ctx context.Context, tx pgx.Tx, c kit.Caller, documentID, state string) error {
	if !einvoiceStates[state] {
		return kit.Invalid("state is not a valid e-invoice state")
	}
	h, err := getHeader(ctx, tx, c.TenantID, documentID, true)
	if err != nil {
		return err
	}
	if !h.State.IsIssued() || h.Kind != s.kind {
		return kit.Conflict(kit.CodeInvalidTransition, "the document is not an issued "+string(s.kind)+": its e-invoice dimension is not open to change")
	}
	if h.EInvoiceState == state {
		return nil
	}
	c.LegalEntityID = h.LegalEntityID
	now := s.m.today()
	v, err := bump(ctx, tx, `UPDATE ar06_corrections SET einvoice_state = $3, version = version + 1, updated_at = $4
		WHERE tenant_id = $1 AND correction_id = $2 AND version = $5 RETURNING version`, c.TenantID, h.CorrectionID, state, now.UTC(), h.Version)
	if err != nil {
		return err
	}
	nh := *h
	nh.Version = v
	return record(ctx, tx, c, &nh, hist{At: now, Command: "setEInvoiceState", ContentHash: h.IssuedContentHash,
		Prior: map[string]any{"state": h.EInvoiceState}, New: map[string]any{"state": state}}, nil)
}
