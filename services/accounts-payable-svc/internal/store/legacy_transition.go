package store

import (
	"context"
	"time"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

// applyCreateDefaults fills the AP-05 v2 columns a caller that only knows the
// header/lines contract (the POST /v1/invoices handler) does not set. They are
// NOT NULL or CHECK-constrained since migration 000008, so an empty value would
// be refused by the database.
//
// It returns the canonical source payload, which becomes the stored
// source_payload and is what source_hash is the digest of.
func applyCreateDefaults(inv *domain.VendorInvoice) []byte {
	if inv.DocumentType == "" {
		inv.DocumentType = domain.DocInvoice
	}
	if inv.IntakeState == "" {
		inv.IntakeState = domain.IntakeReceived
	}
	if inv.MatchState == "" {
		inv.MatchState = domain.MatchNotMatched
	}
	if inv.ApprovalState == "" {
		inv.ApprovalState = domain.ApprovalNone
	}
	if inv.AccountingState == "" {
		inv.AccountingState = domain.AccountingNotRequested
	}
	if inv.SettlementState == "" {
		inv.SettlementState = domain.SettlementUnsettled
	}
	if inv.HoldState == "" {
		inv.HoldState = domain.HoldNone
	}
	if inv.DuplicateState == "" {
		inv.DuplicateState = domain.DuplicateClear
	}
	if inv.TaxState == "" {
		inv.TaxState = domain.TaxNotVerified
	}
	if inv.PayeeState == "" {
		inv.PayeeState = domain.PayeeNotProvided
	}
	if inv.SourceChannel == "" {
		inv.SourceChannel = "api"
	}
	if inv.InvoiceNumberNormalized == "" {
		inv.InvoiceNumberNormalized = domain.NormalizeInvoiceNumber(inv.InvoiceNumber)
	}
	inv.Status = domain.DeriveStatus(inv)

	sp := domain.SourcePayload{
		DocumentType: inv.DocumentType, LegalEntityID: inv.LegalEntityID, VendorID: inv.VendorID,
		InvoiceNumber: inv.InvoiceNumber, Amount: inv.Amount, CurrencyCode: inv.CurrencyCode,
		InvoiceDate: inv.InvoiceDate.Time.UTC().Format("2006-01-02"),
		SupplyDate:  inv.SupplyDate.Time.UTC().Format("2006-01-02"),
		DueDate:     inv.DueDate.UTC().Format("2006-01-02"),
		Lines:       []domain.CreateVendorInvoiceLineInput{},
	}
	if inv.PurchaseOrderID != nil {
		sp.PurchaseOrder = *inv.PurchaseOrderID
	}
	if inv.GoodsReceiptRef != nil {
		sp.GoodsReceipt = *inv.GoodsReceiptRef
	}
	if inv.InvoiceDocumentID != nil {
		sp.DocumentID = *inv.InvoiceDocumentID
	}
	for _, l := range inv.Lines {
		sp.Lines = append(sp.Lines, domain.CreateVendorInvoiceLineInput{
			Description: l.Description, Quantity: l.Quantity, UnitPrice: l.UnitPrice, NetAmount: l.NetAmount,
			TaxCode: l.TaxCode, TaxAmount: l.TaxAmount, TaxDeterminationID: l.TaxDeterminationID,
			POLineReference: l.POLineReference, Dimensions: l.Dimensions,
		})
	}
	raw, hash, err := sp.Canonical()
	if err != nil {
		return nil
	}
	if inv.SourceHash == "" {
		inv.SourceHash = hash
	}
	return raw
}

// TransitionInvoice is the status-to-status contract that POST
// /v1/invoices/{id}/validate|approve|request-payment still uses. It is a thin
// adapter over MutateInvoice -- the one primitive every AP-05 change goes
// through -- so the transition, its history row and its outbox event commit in
// one transaction exactly as the v2 commands do.
//
// The invoice row is locked FOR UPDATE and its current (derived) status must equal
// fromStatus, which preserves the old atomic "UPDATE ... WHERE status = from"
// guarantee: a concurrent loser gets domain.ErrInvalidTransition.
//
// A malformed or unknown id, and another tenant's invoice, are
// domain.ErrInvoiceNotFound.
func (s *PgStore) TransitionInvoice(ctx context.Context, tenantID, invoiceID string, fromStatus, toStatus domain.InvoiceStatus, actorPrincipalID string) error {
	_, err := s.MutateInvoice(ctx, tenantID, invoiceID, nil, func(inv *domain.VendorInvoice) (*domain.Mutation, error) {
		if inv.Status != fromStatus {
			return nil, domain.ErrInvalidTransition
		}
		now := time.Now().UTC()
		actor := actorPrincipalID
		mut := &domain.Mutation{Actor: actorPrincipalID, CorrelationID: inv.CorrelationID}

		switch toStatus {
		case domain.InvoiceStatusValidated:
			if err := inv.SetIntake(domain.IntakeValidated); err != nil {
				return nil, err
			}
			inv.ValidatedByPrincipalID, inv.ValidatedAt = &actor, &now
			// Whether this document must carry a cleared AP-06 match is decided here.
			inv.MatchRequired = inv.RequiresMatch()
			// Validation is the point at which this service says the document is
			// real and complete, so the source representation freezes here.
			if inv.SourceAcceptedAt == nil {
				inv.SourceAcceptedAt = &now
			}
			mut.Command = "ValidateSupplierInvoice"
			mut.Events = []domain.OutboxEvent{{EventType: "vendor.invoice.validated", Payload: map[string]any{"invoice_id": inv.InvoiceID}}}

		case domain.InvoiceStatusApproved:
			// The gates derivable from the invoice's own state. TAX / payee
			// re-verification needs the v2 command's live lookups and is not
			// attempted on this path.
			switch {
			case inv.HoldState != domain.HoldNone, inv.DuplicateState == domain.DuplicateSuspected,
				inv.RequiresMatch() && !inv.MatchCleared:
				return nil, domain.ErrInvalidTransition
			}
			if inv.ApprovalState == domain.ApprovalNone {
				if err := inv.SetApproval(domain.ApprovalPending); err != nil {
					return nil, err
				}
			}
			if err := inv.SetApproval(domain.ApprovalApproved); err != nil {
				return nil, err
			}
			if inv.AccountingState == domain.AccountingNotRequested {
				if err := inv.SetAccounting(domain.AccountingRequested); err != nil {
					return nil, err
				}
			}
			inv.ApprovedByPrincipalID, inv.ApprovedAt = &actor, &now
			mut.Command = "ApproveSupplierInvoice"
			// The accounting obligation commits WITH the approval: one durable ACC-04 request
			// per invoice (idempotent on source_event_id = invoice id), delivered by the
			// accounting dispatcher with retry and quarantine.
			posting := domain.BuildAccountingPosting(inv, mut.CorrelationID, s.postingKeys())
			mut.AccountingPosting = &posting
			mut.Events = []domain.OutboxEvent{{EventType: "vendor.invoice.approved", Payload: map[string]any{"invoice_id": inv.InvoiceID}}}

		case domain.InvoiceStatusPaymentRequested:
			if inv.ApprovalState != domain.ApprovalApproved || inv.PaymentRequestedAt != nil {
				return nil, domain.ErrInvalidTransition
			}
			inv.PaymentRequestedByPrincipalID, inv.PaymentRequestedAt = &actor, &now
			mut.Command = "RequestPayment"
			mut.Events = []domain.OutboxEvent{{EventType: "payment.requested", Payload: map[string]any{
				"invoice_id": inv.InvoiceID, "amount": inv.Amount, "currency_code": inv.CurrencyCode,
			}}}

		default:
			return nil, domain.ErrInvalidTransition
		}
		return mut, nil
	})
	return err
}

// WithPostingMappingKeys sets the ACC-02 mapping keys approvals post against. Left
// unset, domain.DefaultPostingMappingKeys applies.
func (s *PgStore) WithPostingMappingKeys(k domain.PostingMappingKeys) *PgStore {
	s.keys = k
	return s
}

func (s *PgStore) postingKeys() domain.PostingMappingKeys {
	d := domain.DefaultPostingMappingKeys()
	if s.keys.Expense != "" {
		d.Expense = s.keys.Expense
	}
	if s.keys.TaxInput != "" {
		d.TaxInput = s.keys.TaxInput
	}
	if s.keys.PayableControl != "" {
		d.PayableControl = s.keys.PayableControl
	}
	return d
}
