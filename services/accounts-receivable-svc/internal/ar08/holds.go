package ar08

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Receipt cash holds (migration 001000). AR-10 refunds earmark a receipt's UNAPPLIED cash through these methods; AR-10 never
// touches ar08_receipts or ar08_receipt_holds itself. Source of a refund = the unapplied cash (overpayment) of a registered receipt.
//
//	available = unapplied_amount - sum(holds RESERVED or CONSUMED)
//
// A database trigger on ar08_receipts refuses any application that would apply held / refunded cash.

// Aggregate / hold statuses.
const (
	HoldReserved = "RESERVED"
	HoldConsumed = "CONSUMED"
	HoldReleased = "RELEASED"

	AggregateReceiptHold = "ar08_receipt_hold"
)

// ReceiptCash is the cash position of a receipt as AR-10 needs it.
type ReceiptCash struct {
	ReceiptID       string
	LegalEntityID   string
	CustomerPartyID string
	Currency        string
	Amount          kit.Amount
	Unapplied       kit.Amount
	Held            kit.Amount // RESERVED + CONSUMED
	Available       kit.Amount // Unapplied - Held
	Blocked         bool       // Banking no longer confirms the settlement
	Version         int
}

func cashOf(r *Receipt, held kit.Amount) *ReceiptCash {
	return &ReceiptCash{ReceiptID: r.ReceiptID, LegalEntityID: r.LegalEntityID, CustomerPartyID: r.CustomerPartyID, Currency: r.Currency, Amount: r.Amount,
		Unapplied: r.UnappliedAmount, Held: held, Available: r.UnappliedAmount.Sub(held), Blocked: r.Blocked, Version: r.Version}
}

func heldAmount(ctx context.Context, q querier, tenantID, receiptID string) (kit.Amount, error) {
	var held kit.Amount
	err := q.QueryRow(ctx, `SELECT COALESCE(sum(amount),0) FROM ar08_receipt_holds WHERE tenant_id = $1 AND receipt_id = $2 AND status IN ('RESERVED','CONSUMED')`,
		tenantID, receiptID).Scan(&held)
	return held, err
}

// GetReceiptCash reads the cash position of a receipt (own transaction).
func (m *Module) GetReceiptCash(ctx context.Context, tenantID, receiptID string) (*ReceiptCash, error) {
	var out *ReceiptCash
	err := m.read(ctx, tenantID, func(tx pgx.Tx) error {
		r, err := getReceipt(ctx, tx, tenantID, receiptID, false)
		if err != nil {
			return err
		}
		held, err := heldAmount(ctx, tx, tenantID, receiptID)
		if err != nil {
			return err
		}
		out = cashOf(r, held)
		return nil
	})
	return out, kit.MapPgErr(err)
}

// ReserveUnapplied earmarks amount of the receipt's unapplied cash for ownerRef inside the caller's transaction. It locks the
// receipt, so it serialises with ApplyReceipt. A replay for the same ownerRef returns the existing reservation's receipt position.
// Errors: not found; TENANT_SCOPE_VIOLATION when legalEntityID is not the receipt's; SETTLEMENT_NOT_CONFIRMED when the receipt is
// blocked; APPLICATION_OUT_OF_BALANCE when the amount exceeds the available unapplied cash.
func (m *Module) ReserveUnapplied(ctx context.Context, tx pgx.Tx, c kit.Caller, receiptID, ownerRef string, amount kit.Amount) (*ReceiptCash, error) {
	if ownerRef == "" || !amount.IsPositive() {
		return nil, kit.Invalid("a reservation needs an owner reference and a positive amount")
	}
	r, err := getReceipt(ctx, tx, c.TenantID, receiptID, true)
	if err != nil {
		return nil, err
	}
	if r.LegalEntityID != c.LegalEntityID {
		return nil, errScope("the receipt belongs to another legal entity")
	}
	if !amount.FitsCurrency(r.Currency) {
		return nil, kit.Invalid("amount has more decimals than the currency allows")
	}
	var existing kit.Amount
	var exReceipt string
	switch err := tx.QueryRow(ctx, `SELECT amount, receipt_id::text FROM ar08_receipt_holds WHERE tenant_id = $1 AND owner_ref = $2`, c.TenantID, ownerRef).Scan(&existing, &exReceipt); {
	case err == nil:
		if existing != amount || exReceipt != r.ReceiptID {
			return nil, kit.Unprocessable(kit.CodeIdempotencyReused, "the reservation owner already holds a different reservation")
		}
		held, err := heldAmount(ctx, tx, c.TenantID, r.ReceiptID)
		if err != nil {
			return nil, err
		}
		return cashOf(r, held), nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	if r.Blocked {
		return nil, errNotConfirmed("Banking no longer confirms this receipt: its cash cannot be refunded")
	}
	held, err := heldAmount(ctx, tx, c.TenantID, r.ReceiptID)
	if err != nil {
		return nil, err
	}
	if amount > r.UnappliedAmount.Sub(held) {
		return nil, errOutOfBalance("the refund amount " + amount.String() + " exceeds the receipt's available unapplied cash " + r.UnappliedAmount.Sub(held).String())
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ar08_receipt_holds (hold_id, tenant_id, legal_entity_id, receipt_id, owner_ref, amount, status, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,'RESERVED',$7)`, uuid.NewString(), c.TenantID, r.LegalEntityID, r.ReceiptID, ownerRef, amount, c.PrincipalID); err != nil {
		return nil, err
	}
	if err := audit(ctx, tx, c, AggregateReceiptHold, r.ReceiptID, "reserveUnapplied", "", "", nil,
		map[string]any{"owner_ref": ownerRef, "amount": amount.String(), "status": HoldReserved}, map[string]any{"available_before": r.UnappliedAmount.Sub(held).String()}); err != nil {
		return nil, err
	}
	return cashOf(r, held.Add(amount)), nil
}

func (m *Module) closeHold(ctx context.Context, tx pgx.Tx, c kit.Caller, ownerRef, to, command string) error {
	var id, receiptID, status string
	var amount kit.Amount
	err := tx.QueryRow(ctx, `SELECT hold_id::text, receipt_id::text, status, amount FROM ar08_receipt_holds WHERE tenant_id = $1 AND owner_ref = $2 FOR UPDATE`,
		c.TenantID, ownerRef).Scan(&id, &receiptID, &status, &amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound("receipt hold")
	}
	if err != nil {
		return err
	}
	if status == to {
		return nil // idempotent
	}
	if status != HoldReserved {
		return errTransition("the receipt hold is " + status + " and cannot become " + to)
	}
	if _, err := tx.Exec(ctx, `UPDATE ar08_receipt_holds SET status = $3, updated_at = now() WHERE tenant_id = $1 AND hold_id = $2`, c.TenantID, id, to); err != nil {
		return err
	}
	return audit(ctx, tx, c, AggregateReceiptHold, receiptID, command, "", "",
		map[string]any{"owner_ref": ownerRef, "status": status}, map[string]any{"owner_ref": ownerRef, "amount": amount.String(), "status": to}, nil)
}

// ReleaseUnapplied returns a RESERVED hold to the receipt (the refund failed or was cancelled). Idempotent; a CONSUMED hold cannot be released.
func (m *Module) ReleaseUnapplied(ctx context.Context, tx pgx.Tx, c kit.Caller, ownerRef string) error {
	return m.closeHold(ctx, tx, c, ownerRef, HoldReleased, "releaseUnapplied")
}

// ConsumeUnapplied permanently removes a RESERVED hold's cash from the receipt (the refund settled). Idempotent; a RELEASED hold cannot be consumed.
func (m *Module) ConsumeUnapplied(ctx context.Context, tx pgx.Tx, c kit.Caller, ownerRef string) error {
	return m.closeHold(ctx, tx, c, ownerRef, HoldConsumed, "consumeUnapplied")
}

// UnappliedHoldStatus reports a hold's status (found=false when none exists), for AR-10's refund control-exception check.
func (m *Module) UnappliedHoldStatus(ctx context.Context, tenantID, ownerRef string) (status string, found bool, err error) {
	err = m.read(ctx, tenantID, func(tx pgx.Tx) error {
		e := tx.QueryRow(ctx, `SELECT status FROM ar08_receipt_holds WHERE tenant_id = $1 AND owner_ref = $2`, tenantID, ownerRef).Scan(&status)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		found = e == nil
		return e
	})
	return status, found, kit.MapPgErr(err)
}
