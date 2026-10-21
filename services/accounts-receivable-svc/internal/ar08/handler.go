package ar08

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

func reasonRequired() *kit.Problem {
	return kit.Err(http.StatusBadRequest, kit.CodeReasonRequired, "reason_code is required")
}
func evidenceRequired(why string) *kit.Problem {
	return kit.Err(http.StatusBadRequest, kit.CodeEvidenceRequired, "evidence_id is required "+why)
}

// prepareReceipt identifies the caller, loads the receipt to learn its legal entity, refuses a body entity that disagrees
// (spec invariant 16, #26) and runs Guard (tenant + entity reconciliation before authorization).
func (m *Module) prepareReceipt(w http.ResponseWriter, r *http.Request, receiptID, bodyEntity, action string) (kit.Caller, *Receipt, bool) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return kit.Caller{}, nil, false
	}
	var rc *Receipt
	if err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		rc, err = getReceipt(r.Context(), tx, c.TenantID, receiptID, false)
		return err
	}); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if bodyEntity != "" && bodyEntity != rc.LegalEntityID {
		m.fail(w, errScope("legal_entity_id does not match the receipt's legal entity"))
		return kit.Caller{}, nil, false
	}
	c, ok = m.deps.Guard(w, r, rc.LegalEntityID, action)
	if !ok {
		return kit.Caller{}, nil, false
	}
	return c, rc, true
}

func (m *Module) period(ctx context.Context, c kit.Caller, at time.Time) (string, error) {
	if m.periods == nil {
		return at.Format("2006-01"), nil
	}
	k, err := m.periods.PostingPeriod(ctx, c, c.LegalEntityID, at)
	if err != nil {
		var p *kit.Problem
		if errors.As(err, &p) {
			return "", err
		}
		return "", kit.Unavailable("accounting period service unavailable")
	}
	return k, nil
}

// ── RegisterConfirmedReceipt ─────────────────────────────────────────────────

type registerReq struct {
	LegalEntityID     string `json:"legal_entity_id"`
	BankTransactionID string `json:"bank_transaction_id"`
	CustomerPartyID   string `json:"customer_party_id,omitempty"`
	RemittanceText    string `json:"remittance_text,omitempty"`
	RelatedParty      bool   `json:"related_party,omitempty"`
}

// registerConfirmedReceipt is the ONLY way money becomes an AR receipt: the bank_transaction_id is verified against Banking
// (NORMALIZED, positive, valid currency); amount, currency, value date and payer text come from Banking, not the caller (#22).
func (m *Module) registerConfirmedReceipt(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionReceiptRegister)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := mustUUID("bank_transaction_id", req.BankTransactionID); err != nil {
		m.fail(w, err)
		return
	}
	if req.CustomerPartyID != "" {
		if err := mustUUID("customer_party_id", req.CustomerPartyID); err != nil {
			m.fail(w, err)
			return
		}
	}
	if len(req.RemittanceText) > 500 {
		m.fail(w, kit.Invalid("remittance_text is at most 500 characters"))
		return
	}
	txn, err := m.confirmed(ctx, c, req.BankTransactionID)
	if err != nil {
		m.fail(w, err)
		return
	}
	now := m.deps.Now()
	rc := &Receipt{ReceiptID: newID(), LegalEntityID: c.LegalEntityID, BankTransactionID: strings.ToLower(txn.TransactionID), CustomerPartyID: req.CustomerPartyID,
		SettlementRef: "BANK_TXN:" + strings.ToLower(txn.TransactionID), Currency: txn.Currency, Amount: txn.Amount, UnappliedAmount: txn.Amount,
		ValueDate: day(txn.TransactionDate), PayerText: clip(txn.Counterparty, 500), RemittanceText: req.RemittanceText, RelatedParty: req.RelatedParty,
		State: RcptUnapplied, RegisteredBy: c.PrincipalID, Version: 1}
	if txn.StatementLineID != "" {
		rc.SettlementRef = clip(rc.SettlementRef+"/LINE:"+txn.StatementLineID, 255)
	}
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO ar08_receipts (receipt_id, tenant_id, legal_entity_id, bank_transaction_id, settlement_ref, customer_party_id, currency,
				amount, applied_amount, unapplied_amount, difference_amount, value_date, payer_text, remittance_text, related_party, state, registered_by, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,NULLIF($6,'')::uuid,$7,$8,0,$8,0,$9::date,$10,$11,$12,'UNAPPLIED',$13,$14,$14)`,
			rc.ReceiptID, c.TenantID, rc.LegalEntityID, rc.BankTransactionID, rc.SettlementRef, rc.CustomerPartyID, rc.Currency, rc.Amount, rc.ValueDate,
			rc.PayerText, rc.RemittanceText, rc.RelatedParty, rc.RegisteredBy, now); err != nil {
			if kit.IsUniqueViolation(err, "uq_ar08_receipt_bank_txn") {
				return errDuplicate("this bank transaction is already registered as a receipt")
			}
			return err
		}
		return audit(ctx, tx, c, AggregateReceipt, rc.ReceiptID, "registerConfirmedReceipt", "", "", nil, rc,
			map[string]any{"settlement_ref": rc.SettlementRef, "bank_status": txn.Status, "settlement_owner": "BANKING"})
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, rc)
}

// ── CreateApplicationBatch ───────────────────────────────────────────────────

type createBatchReq struct {
	LegalEntityID string   `json:"legal_entity_id"`
	Currency      string   `json:"currency"`
	Reference     string   `json:"reference,omitempty"`
	ReceiptIDs    []string `json:"receipt_ids"`
}

func (m *Module) createApplicationBatch(w http.ResponseWriter, r *http.Request) {
	var req createBatchReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionApplicationManage)
	if !ok {
		return
	}
	ctx := r.Context()
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	if !currencyRE.MatchString(req.Currency) {
		m.fail(w, kit.Invalid("currency must be a three-letter code"))
		return
	}
	if n := len(req.ReceiptIDs); n == 0 || n > MaxBatchReceipts {
		m.fail(w, kit.Invalid(fmt.Sprintf("receipt_ids must hold 1..%d receipts", MaxBatchReceipts)))
		return
	}
	if len(req.Reference) > 128 {
		m.fail(w, kit.Invalid("reference is at most 128 characters"))
		return
	}
	ids := append([]string(nil), req.ReceiptIDs...)
	sort.Strings(ids)
	for i, id := range ids {
		if err := mustUUID("receipt_ids", id); err != nil {
			m.fail(w, err)
			return
		}
		if i > 0 && ids[i-1] == id {
			m.fail(w, kit.Invalid("a receipt may appear once in a batch"))
			return
		}
	}
	now := m.deps.Now()
	b := &Batch{BatchID: newID(), LegalEntityID: c.LegalEntityID, Currency: req.Currency, Reference: req.Reference, State: BatchUnapplied, Version: 1, CreatedBy: c.PrincipalID, ReceiptIDs: ids}
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO ar08_batches (batch_id, tenant_id, legal_entity_id, currency, reference, state, created_by, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,'UNAPPLIED',$6,$7,$7)`, b.BatchID, c.TenantID, b.LegalEntityID, b.Currency, b.Reference, b.CreatedBy, now); err != nil {
			return err
		}
		for _, id := range ids {
			rc, err := getReceipt(ctx, tx, c.TenantID, id, true)
			if err != nil {
				return err
			}
			switch {
			case rc.LegalEntityID != b.LegalEntityID:
				return errScope("a receipt belongs to a different legal entity")
			case rc.Currency != b.Currency:
				return errCurrency()
			case rc.Blocked:
				return errNotConfirmed("a receipt is blocked by a settlement control exception: " + rc.ControlException)
			case rc.State != RcptUnapplied:
				return errTransition("only an UNAPPLIED receipt can join a batch")
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ar08_batch_receipts (tenant_id, batch_id, receipt_id) VALUES ($1,$2,$3)`, c.TenantID, b.BatchID, id); err != nil {
				if kit.IsUniqueViolation(err, "uq_ar08_receipt_one_batch") {
					return errDuplicate("a receipt already belongs to another application batch")
				}
				return err
			}
		}
		return audit(ctx, tx, c, AggregateBatch, b.BatchID, "createApplicationBatch", "", "", nil, b, map[string]any{"receipts": len(ids)})
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, b)
}

// ── GenerateMatchCandidates ──────────────────────────────────────────────────

type generateReq struct {
	LegalEntityID   string `json:"legal_entity_id,omitempty"`
	BatchID         string `json:"batch_id"`
	ReceiptID       string `json:"receipt_id"`
	CustomerPartyID string `json:"customer_party_id,omitempty"`
	ExpectedVersion *int   `json:"expected_version,omitempty"`
}

func (m *Module) generateMatchCandidates(w http.ResponseWriter, r *http.Request) {
	var req generateReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, rc, ok := m.prepareReceipt(w, r, req.ReceiptID, req.LegalEntityID, ActionApplicationManage)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := mustUUID("batch_id", req.BatchID); err != nil {
		m.fail(w, err)
		return
	}
	if m.finder == nil {
		m.fail(w, kit.Unavailable("AR-07 receivables not wired: candidates cannot be generated"))
		return
	}
	if req.ExpectedVersion != nil && *req.ExpectedVersion != rc.Version {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	if rc.Blocked {
		m.fail(w, errNotConfirmed("the receipt is blocked by a settlement control exception: "+rc.ControlException))
		return
	}
	if rc.State != RcptUnapplied {
		m.fail(w, errTransition("candidates are generated only for an UNAPPLIED receipt"))
		return
	}
	// Customer: asserted on the receipt or the command, else inferred from the payer text. Never guessed.
	customer, basis := rc.CustomerPartyID, BasisCustomer
	if req.CustomerPartyID != "" {
		if err := mustUUID("customer_party_id", req.CustomerPartyID); err != nil {
			m.fail(w, err)
			return
		}
		if customer != "" && customer != req.CustomerPartyID {
			m.fail(w, kit.Unprocessable(kit.CodeValidationFailed, "customer_party_id disagrees with the customer recorded on the receipt"))
			return
		}
		customer = req.CustomerPartyID
	}
	if customer == "" {
		if customer = InferCustomer(rc.PayerText, rc.RemittanceText); customer == "" {
			m.fail(w, kit.Unprocessable(kit.CodeValidationFailed, "the customer cannot be determined from the receipt: supply customer_party_id"))
			return
		}
		basis = BasisInferred
	}
	items, err := m.finder.ListOpenForCustomer(ctx, c.TenantID, rc.LegalEntityID, customer, rc.Currency)
	if err != nil {
		m.fail(w, err)
		return
	}
	for _, it := range items {
		if it.LegalEntityID != "" && it.LegalEntityID != rc.LegalEntityID {
			m.fail(w, errScope("an open item belongs to a different legal entity"))
			return
		}
	}
	cands := ScoreCandidates(rc, items, basis)
	now := m.deps.Now()
	var gen int
	var batch *Batch
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getReceipt(ctx, tx, c.TenantID, rc.ReceiptID, true)
		if err != nil {
			return err
		}
		if cur.Version != rc.Version || cur.State != RcptUnapplied || cur.Blocked {
			return kit.ErrStaleVersion
		}
		if bid, err := batchOfReceipt(ctx, tx, c.TenantID, cur.ReceiptID); err != nil {
			return err
		} else if bid != req.BatchID {
			return kit.NotFound("the receipt is not part of this batch")
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(generation),0) + 1 FROM ar08_candidates WHERE tenant_id = $1 AND receipt_id = $2`, c.TenantID, cur.ReceiptID).Scan(&gen); err != nil {
			return err
		}
		for _, cd := range cands {
			basisJSON, _ := json.Marshal(cd.Basis)
			if _, err := tx.Exec(ctx, `INSERT INTO ar08_candidates (candidate_id, tenant_id, legal_entity_id, receipt_id, generation, rank, open_item_id, customer_party_id, score,
					basis, residual_amount, suggested_amount, disputed, collection_hold, generated_by, generated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, newID(), c.TenantID, cur.LegalEntityID, cur.ReceiptID, gen, cd.Rank, cd.OpenItemID,
				cd.CustomerPartyID, cd.Score, basisJSON, cd.Residual, cd.Suggested, cd.Disputed, cd.CollectionHold, c.PrincipalID, now); err != nil {
				return err
			}
		}
		if batch, err = recomputeBatch(ctx, tx, c.TenantID, req.BatchID, now); err != nil {
			return err
		}
		ev := map[string]any{"receipt_id": cur.ReceiptID, "batch_id": req.BatchID, "generation": gen, "candidates": len(cands), "customer_party_id": customer,
			"customer_basis": basis, "correlation_id": c.CorrelationID}
		if err := kit.Emit(ctx, tx, c, kit.Event{AggregateType: AggregateReceipt, AggregateID: cur.ReceiptID, EventType: EvProposed, Payload: ev}); err != nil {
			return err
		}
		return audit(ctx, tx, c, AggregateReceipt, cur.ReceiptID, "generateMatchCandidates", "", "", nil, nil, ev)
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"receipt_id": rc.ReceiptID, "batch_id": req.BatchID, "generation": gen, "customer_party_id": customer,
		"customer_basis": basis, "candidates": cands, "batch_state": batch.State})
}

// ── apply plumbing ───────────────────────────────────────────────────────────

type plan struct {
	receiptID, batchID, mode, hash       string
	lines                                []Line
	split                                Split
	diffKind                             string
	customer                             string
	reason, evidence, matchID, reapplies string
	approvedBy, periodKey, appliedBy     string
	expected                             int
}

// checkItems verifies every open item against the receipt: same tenant (the port is tenant-scoped), same legal entity (#26),
// same currency, one customer, and the line within the residual. AR-07 enforces the residual again, atomically, at adjustment.
func (m *Module) checkItems(ctx context.Context, c kit.Caller, r *Receipt, lines []Line) (string, error) {
	if m.items == nil {
		return "", kit.Unavailable("AR-07 receivables not wired: nothing can be applied")
	}
	customer := r.CustomerPartyID
	for _, l := range lines {
		v, err := m.items.GetByID(ctx, c.TenantID, l.OpenItemID)
		if err != nil {
			var p *kit.Problem
			if errors.As(err, &p) {
				return "", err
			}
			return "", kit.Unavailable("open item unavailable")
		}
		switch {
		case v.LegalEntityID != r.LegalEntityID:
			return "", errScope("an open item belongs to a different legal entity than the receipt")
		case v.Currency != r.Currency:
			return "", errCurrency()
		}
		if customer == "" {
			customer = v.CustomerPartyID
		} else if v.CustomerPartyID != customer {
			return "", kit.Unprocessable(kit.CodeValidationFailed, "every open item must belong to the receipt's customer")
		}
		if l.Amount > v.Residual {
			return "", kit.Unprocessable(kit.CodeReceivableResidualInvalid, "a line exceeds the open item's residual "+v.Residual.String())
		}
	}
	return customer, nil
}

// settle re-reads Banking and confirms the receipt is still a confirmed settlement of the same amount and currency.
func (m *Module) settle(ctx context.Context, c kit.Caller, r *Receipt) error {
	if r.Blocked {
		return errNotConfirmed("the receipt is blocked by a settlement control exception: " + r.ControlException)
	}
	t, err := m.confirmed(ctx, c, r.BankTransactionID)
	if err != nil {
		return err
	}
	if t.Amount != r.Amount || t.Currency != r.Currency {
		return errNotConfirmed("Banking now reports a different amount or currency for this settlement")
	}
	return nil
}

func descLines(lines []Line) []map[string]any {
	out := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		out = append(out, map[string]any{"line_no": l.LineNo, "open_item_id": l.OpenItemID, "amount": l.Amount.String()})
	}
	return out
}

func appPayload(c kit.Caller, a *Application, bankTxn string) map[string]any {
	return map[string]any{"application_id": a.ApplicationID, "receipt_id": a.ReceiptID, "batch_id": a.BatchID, "bank_transaction_id": bankTxn, "legal_entity_id": a.LegalEntityID,
		"currency": a.Currency, "receipt_amount": a.ReceiptAmount.String(), "applied_amount": a.AppliedAmount.String(), "unapplied_amount": a.UnappliedAmount.String(),
		"difference_amount": a.DifferenceAmount.String(), "difference_kind": a.DifferenceKind, "mode": a.Mode, "lines": descLines(a.Lines), "applied_by": a.AppliedBy,
		"approved_by": a.ApprovedBy, "status": a.Status, "version": a.Version, "correlation_id": c.CorrelationID}
}

// execute is the atomic apply: ONE transaction holds the receipt lock, the application and lines, the AR-07 adjustments, the
// receipt update, the ACC-04 posting request, the batch state, the outbox event and the audit. Any failure leaves nothing.
func (m *Module) execute(ctx context.Context, c kit.Caller, p *plan) (*Application, *Receipt, bool, error) {
	now := m.deps.Now()
	var app *Application
	var rc *Receipt
	replay := false
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		r, err := getReceipt(ctx, tx, c.TenantID, p.receiptID, true)
		if err != nil {
			return err
		}
		if r.LegalEntityID != c.LegalEntityID {
			return errScope("the receipt belongs to a different legal entity")
		}
		if ex, err := activeApplication(ctx, tx, c.TenantID, r.ReceiptID, false); err != nil {
			return err
		} else if ex != nil {
			if ex.BatchID == p.batchID && ex.RequestHash == p.hash {
				app, rc, replay = ex, r, true // a retry (e.g. after a timeout) of the same application: no second consequence
				return nil
			}
			return errDuplicate("the receipt already has an active application; unapply it first")
		}
		if p.expected != r.Version {
			return kit.ErrStaleVersion
		}
		if r.Blocked {
			return errNotConfirmed("the receipt is blocked by a settlement control exception: " + r.ControlException)
		}
		if r.State != RcptUnapplied {
			return errTransition("the receipt is " + r.State)
		}
		if r.CustomerPartyID != "" && p.customer != "" && r.CustomerPartyID != p.customer {
			return kit.Unprocessable(kit.CodeValidationFailed, "the open items belong to a different customer than the receipt")
		}
		if bid, err := batchOfReceipt(ctx, tx, c.TenantID, r.ReceiptID); err != nil {
			return err
		} else if bid != p.batchID {
			return kit.NotFound("the receipt is not part of this batch")
		}
		applier := p.appliedBy
		if applier == "" {
			applier = c.PrincipalID
		}
		a := &Application{ApplicationID: newID(), LegalEntityID: r.LegalEntityID, ReceiptID: r.ReceiptID, BatchID: p.batchID, Status: AppApplied, Mode: p.mode, RequestHash: p.hash,
			Currency: r.Currency, ReceiptAmount: r.Amount, AppliedAmount: p.split.Applied, UnappliedAmount: p.split.Unapplied, DifferenceAmount: p.split.Difference,
			DifferenceKind: p.diffKind, ReasonCode: p.reason, EvidenceID: p.evidence, MatchID: p.matchID, Reapplies: p.reapplies, AppliedBy: applier,
			ApprovedBy: p.approvedBy, AppliedAt: now, AccountingState: "PENDING", Version: 1, Lines: p.lines}
		if _, err := tx.Exec(ctx, `INSERT INTO ar08_applications (application_id, tenant_id, legal_entity_id, receipt_id, batch_id, status, mode, request_hash, currency,
				receipt_amount, applied_amount, unapplied_amount, difference_amount, difference_kind, reason_code, evidence_id, match_id, reapplies_application_id,
				applied_by, approved_by, applied_at, accounting_state)
			VALUES ($1,$2,$3,$4,$5,'APPLIED',$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,''),NULLIF($15,''),NULLIF($16,'')::uuid,NULLIF($17,'')::uuid,$18,NULLIF($19,''),$20,'PENDING')`,
			a.ApplicationID, c.TenantID, a.LegalEntityID, a.ReceiptID, a.BatchID, a.Mode, a.RequestHash, a.Currency, a.ReceiptAmount, a.AppliedAmount, a.UnappliedAmount,
			a.DifferenceAmount, a.DifferenceKind, a.ReasonCode, a.EvidenceID, a.MatchID, a.Reapplies, a.AppliedBy, a.ApprovedBy, a.AppliedAt); err != nil {
			if kit.IsUniqueViolation(err, "uq_ar08_one_active_application") {
				return errDuplicate("the receipt already has an active application")
			}
			return err
		}
		for _, l := range p.lines { // sorted by open item id: a global lock order across concurrent applications
			if _, err := tx.Exec(ctx, `INSERT INTO ar08_application_lines (line_id, tenant_id, application_id, line_no, open_item_id, amount) VALUES ($1,$2,$3,$4,$5,$6)`,
				newID(), c.TenantID, a.ApplicationID, l.LineNo, l.OpenItemID, l.Amount); err != nil {
				return err
			}
			if _, err := m.items.ApplyAdjustment(ctx, tx, c, kit.OpenItemAdjustment{OpenItemID: l.OpenItemID, Kind: kit.AdjCashApplication, Amount: l.Amount,
				SourceRefType: "CASH_APPLICATION", SourceRefID: a.ApplicationID, EventKey: fmt.Sprintf("%s:%d:apply", a.ApplicationID, l.LineNo),
				Reason: "cash application of " + r.SettlementRef}); err != nil {
				return err
			}
		}
		before := *r
		r.AppliedAmount, r.UnappliedAmount, r.DifferenceAmount = p.split.Applied, p.split.Unapplied, p.split.Difference
		r.State = ReceiptState(p.split)
		if r.CustomerPartyID == "" {
			r.CustomerPartyID = p.customer
		}
		r.Version++
		if _, err := tx.Exec(ctx, `UPDATE ar08_receipts SET applied_amount = $3, unapplied_amount = $4, difference_amount = $5, state = $6,
				customer_party_id = COALESCE(customer_party_id, NULLIF($7,'')::uuid), version = $8, updated_at = $9 WHERE tenant_id = $1 AND receipt_id = $2`,
			c.TenantID, r.ReceiptID, r.AppliedAmount, r.UnappliedAmount, r.DifferenceAmount, r.State, p.customer, r.Version, now); err != nil {
			return err
		}
		if err := kit.QueuePosting(ctx, tx, c, kit.SourceCashApplication, a.ApplicationID, kit.PostingEvent{FiscalPeriod: p.periodKey,
			Description: "Cash application " + r.SettlementRef, SourceEventID: a.ApplicationID, TransactionCurrency: r.Currency, DocumentDate: r.ValueDate,
			PostingDate: day(now), Lines: BuildPosting(a, false)}); err != nil {
			return err
		}
		if p.matchID != "" {
			if _, err := tx.Exec(ctx, `UPDATE ar08_manual_matches SET status = 'APPLIED', approved_by = $3, application_id = $4, decided_at = $5 WHERE tenant_id = $1 AND match_id = $2`,
				c.TenantID, p.matchID, p.approvedBy, a.ApplicationID, now); err != nil {
				return err
			}
		}
		if _, err := recomputeBatch(ctx, tx, c.TenantID, a.BatchID, now); err != nil {
			return err
		}
		ev := appPayload(c, a, r.BankTransactionID)
		if err := kit.Emit(ctx, tx, c, kit.Event{AggregateType: AggregateApplication, AggregateID: a.ApplicationID, EventType: EvApplied, Payload: ev}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateApplication, a.ApplicationID, "applyReceipt", p.reason, p.evidence, nil, a,
			map[string]any{"settlement_ref": r.SettlementRef, "mode": a.Mode, "approved_by": p.approvedBy, "match_id": p.matchID, "fiscal_period": p.periodKey}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateReceipt, r.ReceiptID, "applyReceipt", p.reason, p.evidence,
			map[string]any{"state": before.State, "applied": before.AppliedAmount.String(), "unapplied": before.UnappliedAmount.String(), "version": before.Version},
			map[string]any{"state": r.State, "applied": r.AppliedAmount.String(), "unapplied": r.UnappliedAmount.String(), "difference": r.DifferenceAmount.String(), "version": r.Version},
			map[string]any{"application_id": a.ApplicationID}); err != nil {
			return err
		}
		app, rc = a, r
		return nil
	})
	return app, rc, replay, err
}

type applyResult struct {
	Application *Application `json:"application"`
	Receipt     *Receipt     `json:"receipt"`
	Replayed    bool         `json:"replayed"`
}

func (m *Module) writeApplied(w http.ResponseWriter, a *Application, rc *Receipt, replay bool) {
	st := http.StatusCreated
	if replay {
		st = http.StatusOK
	}
	kit.WriteJSON(w, st, applyResult{Application: a, Receipt: rc, Replayed: replay})
}

// ── ApplyReceipt ─────────────────────────────────────────────────────────────

type applyReq struct {
	LegalEntityID    string      `json:"legal_entity_id,omitempty"`
	BatchID          string      `json:"batch_id"`
	ReceiptID        string      `json:"receipt_id"`
	ExpectedVersion  *int        `json:"expected_version"`
	Mode             string      `json:"mode"`
	Lines            []Line      `json:"lines,omitempty"`
	DifferenceAmount kit.Amount  `json:"difference_amount,omitempty"`
	DifferenceKind   string      `json:"difference_kind,omitempty"`
	UnappliedAmount  *kit.Amount `json:"unapplied_amount,omitempty"`
	ReasonCode       string      `json:"reason_code,omitempty"`
	EvidenceID       string      `json:"evidence_id,omitempty"`
}

// applyReceipt: mode AUTO applies the persisted candidate allocation when policy allows (score, no dispute / hold, below the
// high-value limit, no related party, no difference); anything else is refused with MANUAL_APPROVAL_REQUIRED. mode MANUAL never
// moves money: it records a proposal that a DIFFERENT principal holding AR08_MANUALMATCH_APPROVE must approve.
func (m *Module) applyReceipt(w http.ResponseWriter, r *http.Request) {
	var req applyReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, rc, ok := m.prepareReceipt(w, r, req.ReceiptID, req.LegalEntityID, ActionApplicationManage)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := mustUUID("batch_id", req.BatchID); err != nil {
		m.fail(w, err)
		return
	}
	if req.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	}
	if req.Mode != ModeAuto && req.Mode != ModeManual {
		m.fail(w, kit.Invalid("mode must be AUTO or MANUAL"))
		return
	}
	var cands []Candidate
	lines, diff := req.Lines, req.DifferenceAmount
	if req.Mode == ModeAuto {
		if len(lines) > 0 || diff.IsPositive() || req.DifferenceKind != "" {
			m.fail(w, kit.Invalid("an AUTO application takes its lines from the persisted candidates; supply none (use MANUAL for explicit lines or differences)"))
			return
		}
		if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) {
			_, cands, err = loadCandidates(ctx, tx, c.TenantID, rc.ReceiptID, 0)
			return err
		}); err != nil {
			m.fail(w, err)
			return
		}
		if len(cands) == 0 {
			m.fail(w, kit.Unprocessable(kit.CodeValidationFailed, "no match candidates exist for this receipt: run GenerateMatchCandidates first"))
			return
		}
		lines = AllocateAuto(rc.Amount, cands)
		if len(lines) == 0 {
			m.fail(w, errApprovalRequired("no candidate reaches the auto-apply confidence; propose a MANUAL match"))
			return
		}
	} else {
		if !kit.RequireReason(w, req.ReasonCode) {
			return
		}
		SortLines(lines)
	}
	// Replay short-circuit (a timeout retry under a new key): no external call, no new consequence.
	hash := RequestHash(req.BatchID, rc.ReceiptID, req.Mode, lines, diff, req.DifferenceKind)
	var ex *Application
	if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) {
		ex, err = activeApplication(ctx, tx, c.TenantID, rc.ReceiptID, false)
		return
	}); err != nil {
		m.fail(w, err)
		return
	}
	if ex != nil && req.Mode == ModeAuto {
		if ex.BatchID == req.BatchID && ex.RequestHash == hash {
			m.writeApplied(w, ex, rc, true)
			return
		}
		m.fail(w, errDuplicate("the receipt already has an active application; unapply it first"))
		return
	}
	if ex != nil {
		m.fail(w, errDuplicate("the receipt already has an active application; unapply it first"))
		return
	}
	split, err := ValidateLines(rc, lines, diff, req.DifferenceKind, req.UnappliedAmount)
	if err != nil {
		m.fail(w, err)
		return
	}
	if rc.State != RcptUnapplied {
		m.fail(w, errTransition("the receipt is "+rc.State))
		return
	}
	if rc.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	if err := m.settle(ctx, c, rc); err != nil {
		m.fail(w, err)
		return
	}
	customer, err := m.checkItems(ctx, c, rc, lines)
	if err != nil {
		m.fail(w, err)
		return
	}
	requires := ApprovalRequirement(rc, req.Mode, split.Applied, split.Difference, cands, lines)
	if req.Mode == ModeAuto {
		if requires != "" {
			m.fail(w, errApprovalRequired("this application needs independent approval ("+requires+"): propose a MANUAL match"))
			return
		}
		pk, err := m.period(ctx, c, m.deps.Now())
		if err != nil {
			m.fail(w, err)
			return
		}
		a, rcpt, replay, err := m.execute(ctx, c, &plan{receiptID: rc.ReceiptID, batchID: req.BatchID, mode: ModeAuto, hash: hash, lines: lines, split: split,
			customer: customer, reason: req.ReasonCode, evidence: req.EvidenceID, expected: *req.ExpectedVersion, periodKey: pk})
		if err != nil {
			m.fail(w, err)
			return
		}
		m.writeApplied(w, a, rcpt, replay)
		return
	}
	// MANUAL: record the proposal only.
	if (rc.RelatedParty || split.Difference.IsPositive() || split.Applied.Add(split.Difference) > AutoApplyHighValueLimit) && req.EvidenceID == "" {
		m.fail(w, evidenceRequired("for a high-value, related-party or difference match"))
		return
	}
	reqs := "MANUAL_MATCH"
	if rc.RelatedParty {
		reqs += ",RELATED_PARTY"
	}
	if split.Applied.Add(split.Difference) > AutoApplyHighValueLimit {
		reqs += ",HIGH_VALUE"
	}
	now := m.deps.Now()
	match := &ManualMatch{MatchID: newID(), ReceiptID: rc.ReceiptID, BatchID: req.BatchID, Lines: lines, DifferenceAmount: split.Difference, DifferenceKind: req.DifferenceKind,
		ReasonCode: req.ReasonCode, EvidenceID: req.EvidenceID, Requires: reqs, Status: MatchPending, ProposedBy: c.PrincipalID}
	var batch *Batch
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getReceipt(ctx, tx, c.TenantID, rc.ReceiptID, true)
		if err != nil {
			return err
		}
		if cur.Version != rc.Version || cur.State != RcptUnapplied || cur.Blocked {
			return kit.ErrStaleVersion
		}
		if bid, err := batchOfReceipt(ctx, tx, c.TenantID, cur.ReceiptID); err != nil {
			return err
		} else if bid != req.BatchID {
			return kit.NotFound("the receipt is not part of this batch")
		}
		if pid, err := pendingMatchID(ctx, tx, c.TenantID, cur.ReceiptID); err != nil {
			return err
		} else if pid != "" {
			return errDuplicate("a manual match is already pending for this receipt")
		}
		lj, _ := json.Marshal(lines)
		if _, err := tx.Exec(ctx, `INSERT INTO ar08_manual_matches (match_id, tenant_id, legal_entity_id, receipt_id, batch_id, lines, difference_amount, difference_kind,
				reason_code, evidence_id, requires, status, proposed_by, proposed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'PENDING',$12,$13)`,
			match.MatchID, c.TenantID, cur.LegalEntityID, cur.ReceiptID, req.BatchID, lj, split.Difference, req.DifferenceKind, req.ReasonCode, req.EvidenceID, reqs,
			c.PrincipalID, now); err != nil {
			return err
		}
		if batch, err = recomputeBatch(ctx, tx, c.TenantID, req.BatchID, now); err != nil {
			return err
		}
		ev := map[string]any{"receipt_id": cur.ReceiptID, "batch_id": req.BatchID, "match_id": match.MatchID, "lines": descLines(lines), "difference_amount": split.Difference.String(),
			"requires": reqs, "proposed_by": c.PrincipalID, "correlation_id": c.CorrelationID}
		if err := kit.Emit(ctx, tx, c, kit.Event{AggregateType: AggregateReceipt, AggregateID: cur.ReceiptID, EventType: EvProposed, Payload: ev}); err != nil {
			return err
		}
		return audit(ctx, tx, c, AggregateReceipt, cur.ReceiptID, "proposeManualMatch", req.ReasonCode, req.EvidenceID, nil, nil, ev)
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusAccepted, map[string]any{"match": match, "receipt_version": rc.Version, "batch_state": batch.State,
		"detail": "proposal recorded; no money moved. A different principal holding " + ActionManualMatchApprove + " must approve it."})
}

// ── ApproveManualMatch ───────────────────────────────────────────────────────

type approveReq struct {
	LegalEntityID   string `json:"legal_entity_id,omitempty"`
	MatchID         string `json:"match_id"`
	ExpectedVersion *int   `json:"expected_version"` // the RECEIPT version the match was proposed against
	ReasonCode      string `json:"reason_code,omitempty"`
	EvidenceID      string `json:"evidence_id,omitempty"`
}

func (m *Module) approveManualMatch(w http.ResponseWriter, r *http.Request) {
	var req approveReq
	if !kit.Decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	c0, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	if req.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	}
	var mt *ManualMatch
	var entity string
	if err := m.read(ctx, c0.TenantID, func(tx pgx.Tx) (err error) {
		mt, entity, err = getMatch(ctx, tx, c0.TenantID, req.MatchID, false)
		return
	}); err != nil {
		m.fail(w, err)
		return
	}
	if req.LegalEntityID != "" && req.LegalEntityID != entity {
		m.fail(w, errScope("legal_entity_id does not match the match's legal entity"))
		return
	}
	// Authority first: a principal without the approve action never learns anything about the match's state.
	c, ok := m.deps.Guard(w, r, entity, ActionManualMatchApprove)
	if !ok {
		return
	}
	var rc *Receipt
	if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) { rc, err = getReceipt(ctx, tx, c.TenantID, mt.ReceiptID, false); return }); err != nil {
		m.fail(w, err)
		return
	}
	switch {
	case c.PrincipalID == mt.ProposedBy:
		m.fail(w, errSoD("a manual match must be approved by a principal other than the one who proposed it"))
		return
	case c.PrincipalID == rc.RegisteredBy:
		m.fail(w, errSoD("the principal who registered the receipt gains no allocation override: it must be approved by someone else"))
		return
	case mt.Status != MatchPending:
		m.fail(w, errTransition("the manual match is "+mt.Status))
		return
	case rc.Version != *req.ExpectedVersion:
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	lines := append([]Line(nil), mt.Lines...)
	SortLines(lines)
	split, err := ValidateLines(rc, lines, mt.DifferenceAmount, mt.DifferenceKind, nil)
	if err != nil {
		m.fail(w, err)
		return
	}
	if err := m.settle(ctx, c, rc); err != nil {
		m.fail(w, err)
		return
	}
	customer, err := m.checkItems(ctx, c, rc, lines)
	if err != nil {
		m.fail(w, err)
		return
	}
	pk, err := m.period(ctx, c, m.deps.Now())
	if err != nil {
		m.fail(w, err)
		return
	}
	hash := RequestHash(mt.BatchID, rc.ReceiptID, ModeManual, lines, mt.DifferenceAmount, mt.DifferenceKind)
	a, rcpt, replay, err := m.execute(ctx, c, &plan{receiptID: rc.ReceiptID, batchID: mt.BatchID, mode: ModeManual, hash: hash, lines: lines, split: split, diffKind: mt.DifferenceKind,
		customer: customer, reason: mt.ReasonCode, evidence: mt.EvidenceID, matchID: mt.MatchID, approvedBy: c.PrincipalID, appliedBy: mt.ProposedBy, expected: *req.ExpectedVersion, periodKey: pk})
	if err != nil {
		m.fail(w, err)
		return
	}
	m.writeApplied(w, a, rcpt, replay)
}

// ── UnapplyReceipt ───────────────────────────────────────────────────────────

type unapplyReq struct {
	LegalEntityID   string `json:"legal_entity_id,omitempty"`
	ReceiptID       string `json:"receipt_id"`
	ExpectedVersion *int   `json:"expected_version"`
	ReasonCode      string `json:"reason_code"`
	EvidenceID      string `json:"evidence_id"`
}

// unapplyReceipt reverses the receipt's active application: AdjCashUnapply adjustments restore each residual, a reversing posting
// is queued and the receipt returns to UNAPPLIED. Restricted action; the principal must differ from whoever applied or approved
// the application; the accounting period must accept the reversal (PERIOD_CLOSED otherwise).
func (m *Module) unapplyReceipt(w http.ResponseWriter, r *http.Request) {
	var req unapplyReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, rc, ok := m.prepareReceipt(w, r, req.ReceiptID, req.LegalEntityID, ActionUnapply)
	if !ok {
		return
	}
	ctx := r.Context()
	if req.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	}
	if strings.TrimSpace(req.ReasonCode) == "" {
		m.fail(w, reasonRequired())
		return
	}
	if strings.TrimSpace(req.EvidenceID) == "" {
		m.fail(w, evidenceRequired("to unapply cash"))
		return
	}
	if m.items == nil {
		m.fail(w, kit.Unavailable("AR-07 receivables not wired: nothing can be unapplied"))
		return
	}
	pk, err := m.period(ctx, c, m.deps.Now())
	if err != nil {
		m.fail(w, err)
		return
	}
	now := m.deps.Now()
	var app *Application
	var out *Receipt
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getReceipt(ctx, tx, c.TenantID, rc.ReceiptID, true)
		if err != nil {
			return err
		}
		a, err := activeApplication(ctx, tx, c.TenantID, cur.ReceiptID, true)
		if err != nil {
			return err
		}
		if a == nil {
			return errTransition("the receipt has no active application to unapply")
		}
		if cur.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		if c.PrincipalID == a.AppliedBy || (a.ApprovedBy != "" && c.PrincipalID == a.ApprovedBy) {
			return errSoD("an application must be unapplied by a principal other than the one who applied or approved it")
		}
		if a.AccountingState == kit.PostingFailed || a.AccountingState == kit.PostingQuarantined {
			return errTransition("the application's accounting is " + a.AccountingState + ": resolve the posting before reversing it")
		}
		for _, l := range a.Lines {
			if _, err := m.items.ApplyAdjustment(ctx, tx, c, kit.OpenItemAdjustment{OpenItemID: l.OpenItemID, Kind: kit.AdjCashUnapply, Amount: l.Amount,
				SourceRefType: "CASH_APPLICATION", SourceRefID: a.ApplicationID, EventKey: fmt.Sprintf("%s:%d:unapply", a.ApplicationID, l.LineNo),
				Reason: "unapply of " + cur.SettlementRef + ": " + req.ReasonCode}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE ar08_applications SET status = 'UNAPPLIED', unapplied_by = $3, unapplied_at = $4, unapply_reason_code = $5, unapply_evidence_id = $6,
				unapply_accounting_state = 'PENDING', version = version + 1 WHERE tenant_id = $1 AND application_id = $2`,
			c.TenantID, a.ApplicationID, c.PrincipalID, now, req.ReasonCode, req.EvidenceID); err != nil {
			return err
		}
		before := *cur
		if _, err := tx.Exec(ctx, `UPDATE ar08_receipts SET applied_amount = 0, unapplied_amount = amount, difference_amount = 0, state = 'UNAPPLIED', version = version + 1, updated_at = $3
			WHERE tenant_id = $1 AND receipt_id = $2`, c.TenantID, cur.ReceiptID, now); err != nil {
			return err
		}
		a.Status, a.UnappliedBy, a.UnapplyReason = AppUnapplied, c.PrincipalID, req.ReasonCode
		if err := kit.QueuePosting(ctx, tx, c, kit.SourceCashUnapply, a.ApplicationID, kit.PostingEvent{FiscalPeriod: pk, Description: "Unapply cash " + cur.SettlementRef,
			SourceEventID: a.ApplicationID + ":unapply", TransactionCurrency: cur.Currency, DocumentDate: cur.ValueDate, PostingDate: day(now), Lines: BuildPosting(a, true)}); err != nil {
			return err
		}
		if _, err := recomputeBatch(ctx, tx, c.TenantID, a.BatchID, now); err != nil {
			return err
		}
		ev := appPayload(c, a, cur.BankTransactionID)
		ev["unapply_reason_code"], ev["unapplied_by"] = req.ReasonCode, c.PrincipalID
		evs := []kit.Event{{AggregateType: AggregateApplication, AggregateID: a.ApplicationID, EventType: EvUnapplied, Payload: ev}}
		if req.ReasonCode == "CORRECTION" {
			evs = append(evs, kit.Event{AggregateType: AggregateApplication, AggregateID: a.ApplicationID, EventType: EvUnappliedByCorrection, Payload: ev})
		}
		if err := kit.Emit(ctx, tx, c, evs...); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateApplication, a.ApplicationID, "unapplyReceipt", req.ReasonCode, req.EvidenceID,
			map[string]any{"status": AppApplied}, map[string]any{"status": AppUnapplied}, map[string]any{"fiscal_period": pk}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateReceipt, cur.ReceiptID, "unapplyReceipt", req.ReasonCode, req.EvidenceID,
			map[string]any{"state": before.State, "applied": before.AppliedAmount.String(), "version": before.Version},
			map[string]any{"state": RcptUnapplied, "applied": "0.0000", "version": before.Version + 1}, map[string]any{"application_id": a.ApplicationID}); err != nil {
			return err
		}
		app = a
		out, err = getReceipt(ctx, tx, c.TenantID, cur.ReceiptID, false)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, applyResult{Application: app, Receipt: out})
}

// ── ReapplyReceipt ───────────────────────────────────────────────────────────

type reapplyReq struct {
	LegalEntityID    string      `json:"legal_entity_id,omitempty"`
	ReceiptID        string      `json:"receipt_id"`
	ExpectedVersion  *int        `json:"expected_version"`
	Lines            []Line      `json:"lines"`
	DifferenceAmount kit.Amount  `json:"difference_amount,omitempty"`
	DifferenceKind   string      `json:"difference_kind,omitempty"`
	UnappliedAmount  *kit.Amount `json:"unapplied_amount,omitempty"`
	ReasonCode       string      `json:"reason_code"`
	EvidenceID       string      `json:"evidence_id"`
}

// reapplyReceipt creates a NEW application for a receipt whose previous application was unapplied. Restricted action; the
// principal must differ from the one who made the reversed application; a related-party / high-value / difference reapplication
// additionally needs the manual-match-approve authority.
func (m *Module) reapplyReceipt(w http.ResponseWriter, r *http.Request) {
	var req reapplyReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, rc, ok := m.prepareReceipt(w, r, req.ReceiptID, req.LegalEntityID, ActionReapply)
	if !ok {
		return
	}
	ctx := r.Context()
	if req.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	}
	if strings.TrimSpace(req.ReasonCode) == "" {
		m.fail(w, reasonRequired())
		return
	}
	if strings.TrimSpace(req.EvidenceID) == "" {
		m.fail(w, evidenceRequired("to reapply cash"))
		return
	}
	var prior *Application
	var batchID string
	if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) {
		if prior, err = latestUnappliedApplication(ctx, tx, c.TenantID, rc.ReceiptID); err != nil {
			return err
		}
		batchID, err = batchOfReceipt(ctx, tx, c.TenantID, rc.ReceiptID)
		return err
	}); err != nil {
		m.fail(w, err)
		return
	}
	if prior == nil || rc.State != RcptUnapplied {
		m.fail(w, errTransition("reapply needs a receipt whose application was unapplied"))
		return
	}
	if c.PrincipalID == prior.AppliedBy || (prior.ApprovedBy != "" && c.PrincipalID == prior.ApprovedBy) {
		m.fail(w, errSoD("a reapplication must be made by a principal other than the one who made the reversed application"))
		return
	}
	lines := append([]Line(nil), req.Lines...)
	SortLines(lines)
	split, err := ValidateLines(rc, lines, req.DifferenceAmount, req.DifferenceKind, req.UnappliedAmount)
	if err != nil {
		m.fail(w, err)
		return
	}
	if rc.Version != *req.ExpectedVersion {
		m.fail(w, kit.ErrStaleVersion)
		return
	}
	if rc.RelatedParty || split.Difference.IsPositive() || split.Applied.Add(split.Difference) > AutoApplyHighValueLimit {
		if err := m.checkAction(ctx, c, ActionManualMatchApprove); err != nil {
			m.fail(w, errApprovalRequired("a related-party, high-value or difference reapplication needs "+ActionManualMatchApprove+" authority"))
			return
		}
	}
	if err := m.settle(ctx, c, rc); err != nil {
		m.fail(w, err)
		return
	}
	customer, err := m.checkItems(ctx, c, rc, lines)
	if err != nil {
		m.fail(w, err)
		return
	}
	pk, err := m.period(ctx, c, m.deps.Now())
	if err != nil {
		m.fail(w, err)
		return
	}
	hash := RequestHash(batchID, rc.ReceiptID, ModeReapply, lines, split.Difference, req.DifferenceKind)
	a, rcpt, replay, err := m.execute(ctx, c, &plan{receiptID: rc.ReceiptID, batchID: batchID, mode: ModeReapply, hash: hash, lines: lines, split: split, diffKind: req.DifferenceKind,
		customer: customer, reason: req.ReasonCode, evidence: req.EvidenceID, reapplies: prior.ApplicationID, expected: *req.ExpectedVersion, periodKey: pk})
	if err != nil {
		m.fail(w, err)
		return
	}
	m.writeApplied(w, a, rcpt, replay)
}
