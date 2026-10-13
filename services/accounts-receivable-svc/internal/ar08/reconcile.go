package ar08

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Finding is one reconciliation exception. There is no plug: every difference is recorded, never absorbed.
type Finding struct {
	ReceiptID string `json:"receipt_id"`
	Code      string `json:"code"`
	Detail    string `json:"detail"`
}

// Settlement re-verification outcomes (what Banking says NOW).
const (
	SettleConfirmed  = "CONFIRMED"
	SettleSuperseded = "SUPERSEDED"
	SettleQuarantine = "QUARANTINED"
	SettleNotFound   = "NOT_FOUND"
	SettleChanged    = "CHANGED"
	SettleUnverified = "UNVERIFIED"
)

// Reconciliation is the persisted evidence of one ReconcileApplicationBatch run.
type Reconciliation struct {
	ReconciliationID string     `json:"reconciliation_id"`
	BatchID          string     `json:"batch_id"`
	Currency         string     `json:"currency"`
	Status           string     `json:"status"`
	ReceiptsTotal    kit.Amount `json:"receipts_total"`
	AppliedTotal     kit.Amount `json:"applied_total"`
	UnappliedTotal   kit.Amount `json:"unapplied_total"`
	DifferenceTotal  kit.Amount `json:"difference_total"`
	Findings         []Finding  `json:"findings"`
	PerformedBy      string     `json:"performed_by"`
	PerformedAt      time.Time  `json:"performed_at"`
	BatchState       string     `json:"batch_state,omitempty"`
	BatchVersion     int        `json:"batch_version,omitempty"`
}

// recheck asks Banking again about a receipt's settlement. It never changes AR state.
func (m *Module) recheck(ctx context.Context, c kit.Caller, r *Receipt) string {
	if m.source == nil {
		return SettleUnverified
	}
	t, err := m.source.Get(ctx, c, r.BankTransactionID)
	switch {
	case errors.Is(err, ErrBankTxnNotFound):
		return SettleNotFound
	case err != nil:
		return SettleUnverified
	case t.Status == BankSuperseded:
		return SettleSuperseded
	case t.Status == BankQuarantined:
		return SettleQuarantine
	case VerifyConfirmed(t, r.BankTransactionID, c.TenantID) != nil:
		return SettleChanged
	case t.Amount != r.Amount || t.Currency != r.Currency:
		return SettleChanged
	}
	return SettleConfirmed
}

// postingProblem is "" when the posting request exists and is not failed / quarantined.
func postingProblem(ctx context.Context, q querier, tenantID, sourceEventID string) (string, error) {
	var status string
	err := q.QueryRow(ctx, `SELECT status FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, tenantID, sourceEventID).Scan(&status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "NO_POSTING", nil
	case err != nil:
		return "", err
	case status == kit.PostingFailed || status == kit.PostingQuarantined:
		return "POSTING_" + status, nil
	}
	return "", nil
}

type reconcileReq struct {
	LegalEntityID   string `json:"legal_entity_id,omitempty"`
	BatchID         string `json:"batch_id"`
	ExpectedVersion *int   `json:"expected_version"`
}

// reconcileApplicationBatch verifies, per receipt and for the batch, receipts = applied + unapplied + differences, that the lines
// sum to the applied amount, that every application has its accounting posting, and (re-verification) that Banking still
// confirms the settlement. A superseded / quarantined / vanished settlement flags a control exception on the receipt and blocks
// further application; it NEVER changes money. The run is recorded in ar08_reconciliations; the batch becomes RECONCILED only when
// there are no findings.
func (m *Module) reconcileApplicationBatch(w http.ResponseWriter, r *http.Request) {
	var req reconcileReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c0, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if req.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	}
	var b *Batch
	var ids []string
	var rcs []*Receipt
	if err := m.read(ctx, c0.TenantID, func(tx pgx.Tx) (err error) {
		if b, err = getBatch(ctx, tx, c0.TenantID, req.BatchID, false); err != nil {
			return err
		}
		if ids, err = batchReceiptIDs(ctx, tx, c0.TenantID, req.BatchID); err != nil {
			return err
		}
		for _, id := range ids {
			rc, err := getReceipt(ctx, tx, c0.TenantID, id, false)
			if err != nil {
				return err
			}
			rcs = append(rcs, rc)
		}
		return nil
	}); err != nil {
		m.fail(w, err)
		return
	}
	if req.LegalEntityID != "" && req.LegalEntityID != b.LegalEntityID {
		m.fail(w, errScope("legal_entity_id does not match the batch's legal entity"))
		return
	}
	c, ok := m.deps.Guard(w, r, b.LegalEntityID, ActionApplicationManage)
	if !ok {
		return
	}
	if b.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	settle := map[string]string{}
	for _, rc := range rcs { // external reads happen before the transaction; they only ever flag, never move money
		settle[rc.ReceiptID] = m.recheck(ctx, c, rc)
	}
	now := m.deps.Now()
	rec := &Reconciliation{ReconciliationID: newID(), BatchID: b.BatchID, Currency: b.Currency, Findings: []Finding{}, PerformedBy: c.PrincipalID, PerformedAt: now}
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getBatch(ctx, tx, c.TenantID, b.BatchID, false)
		if err != nil {
			return err
		}
		// lock order: receipts (by id) first, then the batch - the same order as apply / unapply
		locked := make([]*Receipt, 0, len(ids))
		for _, id := range ids {
			rc, err := getReceipt(ctx, tx, c.TenantID, id, true)
			if err != nil {
				return err
			}
			locked = append(locked, rc)
		}
		if cur, err = getBatch(ctx, tx, c.TenantID, b.BatchID, true); err != nil {
			return err
		}
		if cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		add := func(rc *Receipt, code, detail string) {
			rec.Findings = append(rec.Findings, Finding{ReceiptID: rc.ReceiptID, Code: code, Detail: detail})
		}
		for _, rc := range locked {
			rec.ReceiptsTotal = rec.ReceiptsTotal.Add(rc.Amount)
			rec.AppliedTotal = rec.AppliedTotal.Add(rc.AppliedAmount)
			rec.UnappliedTotal = rec.UnappliedTotal.Add(rc.UnappliedAmount)
			rec.DifferenceTotal = rec.DifferenceTotal.Add(rc.DifferenceAmount)
			if rc.Amount != rc.AppliedAmount.Add(rc.UnappliedAmount).Add(rc.DifferenceAmount) {
				add(rc, "APPLICATION_OUT_OF_BALANCE", "receipt "+rc.Amount.String()+" <> applied + unapplied + difference")
			}
			app, err := activeApplication(ctx, tx, c.TenantID, rc.ReceiptID, false)
			if err != nil {
				return err
			}
			if app != nil {
				var lines kit.Amount
				for _, l := range app.Lines {
					lines = lines.Add(l.Amount)
				}
				if lines != app.AppliedAmount || app.AppliedAmount != rc.AppliedAmount || app.UnappliedAmount != rc.UnappliedAmount || app.DifferenceAmount != rc.DifferenceAmount {
					add(rc, "APPLICATION_OUT_OF_BALANCE", "application lines / amounts do not agree with the receipt")
				}
				if code, err := postingProblem(ctx, tx, c.TenantID, app.ApplicationID); err != nil {
					return err
				} else if code != "" {
					add(rc, code, "the cash application has no healthy accounting posting")
				}
			} else if rc.AppliedAmount.IsPositive() || rc.State != RcptUnapplied {
				add(rc, "APPLICATION_OUT_OF_BALANCE", "the receipt shows applied cash but has no active application")
			}
			rows, err := tx.Query(ctx, `SELECT application_id::text FROM ar08_applications WHERE tenant_id = $1 AND receipt_id = $2 AND status = 'UNAPPLIED'`, c.TenantID, rc.ReceiptID)
			if err != nil {
				return err
			}
			var reversed []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				reversed = append(reversed, id)
			}
			rows.Close()
			for _, id := range reversed {
				for _, key := range []string{id, id + ":unapply"} {
					if code, err := postingProblem(ctx, tx, c.TenantID, key); err != nil {
						return err
					} else if code != "" {
						add(rc, code, "a reversed application has no healthy posting ("+key+")")
					}
				}
			}
			switch s := settle[rc.ReceiptID]; s {
			case SettleConfirmed:
				if rc.Blocked {
					add(rc, "RECEIPT_BLOCKED", rc.ControlException)
				}
			case SettleUnverified:
				add(rc, "SETTLEMENT_UNVERIFIED", "Banking could not be asked; the settlement is unverified")
			default:
				msg := "Banking reports the settlement as " + s + ": further application is blocked; any existing application needs a governed unapply"
				add(rc, "SETTLEMENT_"+s, msg)
				if !rc.Blocked {
					if _, err := tx.Exec(ctx, `UPDATE ar08_receipts SET blocked = TRUE, control_exception = $3, version = version + 1, updated_at = $4 WHERE tenant_id = $1 AND receipt_id = $2`,
						c.TenantID, rc.ReceiptID, "SETTLEMENT_"+s, now); err != nil {
						return err
					}
					if err := audit(ctx, tx, c, AggregateReceipt, rc.ReceiptID, "settlementControlException", "", "", map[string]any{"blocked": false},
						map[string]any{"blocked": true, "control_exception": "SETTLEMENT_" + s}, map[string]any{"bank_transaction_id": rc.BankTransactionID, "money_moved": false}); err != nil {
						return err
					}
				}
			}
		}
		rec.Status = "RECONCILED"
		if len(rec.Findings) > 0 || rec.ReceiptsTotal != rec.AppliedTotal.Add(rec.UnappliedTotal).Add(rec.DifferenceTotal) {
			rec.Status = "EXCEPTION"
		}
		fj, _ := json.Marshal(rec.Findings)
		if _, err := tx.Exec(ctx, `INSERT INTO ar08_reconciliations (reconciliation_id, tenant_id, legal_entity_id, batch_id, currency, status, receipts_total, applied_total,
				unapplied_total, difference_total, findings, performed_by, performed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			rec.ReconciliationID, c.TenantID, cur.LegalEntityID, cur.BatchID, cur.Currency, rec.Status, rec.ReceiptsTotal, rec.AppliedTotal, rec.UnappliedTotal,
			rec.DifferenceTotal, fj, c.PrincipalID, now); err != nil {
			return err
		}
		nb, err := recomputeBatch(ctx, tx, c.TenantID, cur.BatchID, now)
		if err != nil {
			return err
		}
		if rec.Status == "RECONCILED" && (nb.State == BatchPartial || nb.State == BatchFull) {
			if _, err := tx.Exec(ctx, `UPDATE ar08_batches SET state = 'RECONCILED', version = version + 1, updated_at = $3 WHERE tenant_id = $1 AND batch_id = $2`, c.TenantID, cur.BatchID, now); err != nil {
				return err
			}
			nb.State, nb.Version = BatchReconciled, nb.Version+1
			ev := map[string]any{"batch_id": cur.BatchID, "reconciliation_id": rec.ReconciliationID, "currency": cur.Currency, "receipts_total": rec.ReceiptsTotal.String(),
				"applied_total": rec.AppliedTotal.String(), "unapplied_total": rec.UnappliedTotal.String(), "difference_total": rec.DifferenceTotal.String(), "correlation_id": c.CorrelationID}
			if err := kit.Emit(ctx, tx, c, kit.Event{AggregateType: AggregateBatch, AggregateID: cur.BatchID, EventType: EvReconciled, Payload: ev}); err != nil {
				return err
			}
		}
		rec.BatchState, rec.BatchVersion = nb.State, nb.Version
		return audit(ctx, tx, c, AggregateBatch, cur.BatchID, "reconcileApplicationBatch", "", "", map[string]any{"state": cur.State, "version": cur.Version},
			map[string]any{"state": nb.State, "version": nb.Version}, map[string]any{"reconciliation_id": rec.ReconciliationID, "status": rec.Status, "findings": len(rec.Findings)})
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, rec)
}
