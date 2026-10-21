package ar10

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// SubmitSettle is how long a prepared-but-unsubmitted attempt is left alone before reconciliation may send it (the first submit
// may still be in flight). The adapter passes its idempotency key to the provider, so even then the provider dedups.
const SubmitSettle = 2 * time.Minute

// refundOut is the response of every refund command: the refund plus, for an ambiguous external state, the stable code.
type refundOut struct {
	*Refund
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func refundPayload(c kit.Caller, r *Refund, extra map[string]any) map[string]any {
	p := map[string]any{"refund_id": r.RefundID, "receipt_id": r.ReceiptID, "legal_entity_id": r.LegalEntityID, "customer_party_id": r.CustomerPartyID,
		"amount": r.Amount.String(), "currency": r.Currency, "state": r.State, "version": r.Version, "external_attempt_id": r.ExternalAttemptID,
		"correlation_id": c.CorrelationID}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func emitRefund(ctx context.Context, tx pgx.Tx, c kit.Caller, r *Refund, event string, extra map[string]any) error {
	return kit.Emit(ctx, tx, c, kit.Event{AggregateType: AggregateRefund, AggregateID: r.RefundID, EventType: event, Payload: refundPayload(c, r, extra)})
}

type refundBase struct {
	LegalEntityID   string `json:"legal_entity_id,omitempty"`
	RefundID        string `json:"refund_id"`
	ExpectedVersion *int   `json:"expected_version"`
}

// prepareRefund loads the refund, refuses a body entity that disagrees with the stored one, and guards the action against the
// STORED legal entity (never trust the body).
func (m *Module) prepareRefund(w http.ResponseWriter, r *http.Request, b refundBase, action string) (kit.Caller, *Refund, bool) {
	c0, ok := m.deps.Identify(w, r)
	if !ok {
		return kit.Caller{}, nil, false
	}
	if err := mustUUID("refund_id", b.RefundID); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	var rf *Refund
	if err := m.read(r.Context(), c0.TenantID, func(tx pgx.Tx) (e error) { rf, e = getRefund(r.Context(), tx, c0.TenantID, b.RefundID, false); return }); err != nil {
		m.fail(w, err)
		return kit.Caller{}, nil, false
	}
	if b.LegalEntityID != "" && b.LegalEntityID != rf.LegalEntityID {
		m.fail(w, errScope("the refund belongs to another legal entity"))
		return kit.Caller{}, nil, false
	}
	c, ok := m.deps.Guard(w, r, rf.LegalEntityID, action)
	return c, rf, ok
}

// lockRefund runs fn on the locked refund inside one transaction and returns the refund as committed.
func (m *Module) lockRefund(ctx context.Context, c kit.Caller, id string, fn func(tx pgx.Tx, cur *Refund) error) (*Refund, error) {
	var out *Refund
	err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		cur, err := getRefund(ctx, tx, c.TenantID, id, true)
		if err != nil {
			return err
		}
		if err := fn(tx, cur); err != nil {
			return err
		}
		out, err = getRefund(ctx, tx, c.TenantID, id, false)
		return err
	})
	return out, err
}

// setRefund applies a column update and bumps the version.
func setRefund(ctx context.Context, tx pgx.Tx, tenantID, id, set string, args ...any) error {
	a := append([]any{tenantID, id}, args...)
	_, err := tx.Exec(ctx, `UPDATE ar10_refunds SET `+set+`, version = version + 1, updated_at = now() WHERE tenant_id = $1 AND refund_id = $2`, a...)
	return err
}

// ── CreateRefundRequest ──────────────────────────────────────────────────────

type createRefundReq struct {
	LegalEntityID     string     `json:"legal_entity_id"`
	ReceiptID         string     `json:"receipt_id"`
	Amount            kit.Amount `json:"amount"`
	DestinationRef    string     `json:"destination_ref"`
	PayerAccountRef   string     `json:"payer_account_ref"`
	ReasonCode        string     `json:"reason_code"`
	EvidenceID        string     `json:"evidence_id"`
	RelatedOpenItemID string     `json:"related_open_item_id,omitempty"`
}

// createRefundRequest records a refund of (part of) a receipt's unapplied cash, reserves that cash in AR-08 and leaves the request
// PENDING_APPROVAL. The destination must be an ACTIVE verified destination of the paying customer; the approval is later bound to it.
func (m *Module) createRefundRequest(w http.ResponseWriter, r *http.Request) {
	var req createRefundReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, ok := m.deps.Guard(w, r, req.LegalEntityID, ActionRefundCreate)
	if !ok {
		return
	}
	ctx := r.Context()
	for f, v := range map[string]string{"receipt_id": req.ReceiptID} {
		if err := mustUUID(f, v); err != nil {
			m.fail(w, err)
			return
		}
	}
	if req.RelatedOpenItemID != "" {
		if err := mustUUID("related_open_item_id", req.RelatedOpenItemID); err != nil {
			m.fail(w, err)
			return
		}
	}
	switch {
	case strings.TrimSpace(req.DestinationRef) == "" || strings.TrimSpace(req.PayerAccountRef) == "":
		m.fail(w, kit.Invalid("destination_ref and payer_account_ref are required (references only; bank details are never accepted)"))
		return
	case !req.Amount.IsPositive():
		m.fail(w, kit.Invalid("amount must be positive"))
		return
	case strings.TrimSpace(req.ReasonCode) == "":
		m.fail(w, reasonRequired())
		return
	case strings.TrimSpace(req.EvidenceID) == "":
		m.fail(w, evidenceRequired("for a refund"))
		return
	}
	if m.receipts == nil || m.dest == nil {
		m.fail(w, kit.Unavailable("AR-08 receipts / destination verification not wired: no refund was recorded"))
		return
	}
	cash, err := m.receipts.GetReceiptCash(ctx, c.TenantID, req.ReceiptID)
	if err != nil {
		m.fail(w, err)
		return
	}
	switch {
	case cash.LegalEntityID != c.LegalEntityID:
		m.fail(w, errScope("the receipt belongs to another legal entity"))
		return
	case !req.Amount.FitsCurrency(cash.Currency):
		m.fail(w, kit.Invalid("amount has more decimals than the currency allows"))
		return
	}
	if req.RelatedOpenItemID != "" {
		if m.items == nil {
			m.fail(w, kit.Unavailable("AR-07 receivables not wired"))
			return
		}
		it, err := m.items.GetByID(ctx, c.TenantID, req.RelatedOpenItemID)
		if err != nil {
			m.fail(w, err)
			return
		}
		if it.LegalEntityID != c.LegalEntityID || (cash.CustomerPartyID != "" && it.CustomerPartyID != cash.CustomerPartyID) {
			m.fail(w, errScope("the related open item belongs to another entity or customer than the receipt"))
			return
		}
	}
	info, err := m.dest.Verify(ctx, c, req.DestinationRef)
	if err != nil {
		m.fail(w, err)
		return
	}
	if err := checkDestination(info, cash.CustomerPartyID, cash.Currency); err != nil {
		m.fail(w, err)
		return
	}
	now := m.deps.Now()
	rf := &Refund{RefundID: newID(), LegalEntityID: c.LegalEntityID, CustomerPartyID: cash.CustomerPartyID, ReceiptID: cash.ReceiptID, RelatedOpenItemID: req.RelatedOpenItemID,
		Currency: cash.Currency, Amount: req.Amount, DestinationRef: req.DestinationRef, DestinationFingerprint: info.Fingerprint, DestinationVerification: info.Evidence,
		PayerAccountRef: req.PayerAccountRef, ReasonCode: req.ReasonCode, EvidenceID: req.EvidenceID, State: RfPendingApproval, RequestedBy: c.PrincipalID, Version: 1}
	rf.BindingHash = rf.Binding()
	var out *Refund
	err = m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := m.receipts.ReserveUnapplied(ctx, tx, c, rf.ReceiptID, "REFUND:"+rf.RefundID, rf.Amount); err != nil {
			return err
		}
		ver, _ := json.Marshal(rf.DestinationVerification)
		if _, err := tx.Exec(ctx, `INSERT INTO ar10_refunds (refund_id, tenant_id, legal_entity_id, customer_party_id, receipt_id, related_open_item_id, currency, amount, destination_ref,
				destination_fingerprint, destination_verification, payer_account_ref, reason_code, evidence_id, state, binding_hash, requested_by, created_at, updated_at)
			VALUES ($1,$2,$3,NULLIF($4,'')::uuid,$5,NULLIF($6,'')::uuid,$7,$8,$9,$10,$11,$12,$13,$14,'PENDING_APPROVAL',$15,$16,$17,$17)`,
			rf.RefundID, c.TenantID, rf.LegalEntityID, rf.CustomerPartyID, rf.ReceiptID, rf.RelatedOpenItemID, rf.Currency, rf.Amount, rf.DestinationRef,
			rf.DestinationFingerprint, ver, rf.PayerAccountRef, rf.ReasonCode, rf.EvidenceID, rf.BindingHash, rf.RequestedBy, now); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "REFUND", rf.RefundID, "createRefundRequest", RfDraft, RfPendingApproval, map[string]any{"amount": rf.Amount.String(), "destination_ref": rf.DestinationRef}); err != nil {
			return err
		}
		var e error
		if out, e = getRefund(ctx, tx, c.TenantID, rf.RefundID, false); e != nil {
			return e
		}
		if err := audit(ctx, tx, c, AggregateRefund, rf.RefundID, "createRefundRequest", rf.ReasonCode, rf.EvidenceID, nil, out,
			map[string]any{"binding_hash": rf.BindingHash, "source": "UNAPPLIED_CASH", "destination_verification": rf.DestinationVerification}); err != nil {
			return err
		}
		return emitRefund(ctx, tx, c, out, EvRefundRequested, map[string]any{"reason_code": rf.ReasonCode, "evidence_id": rf.EvidenceID})
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusCreated, refundOut{Refund: out})
}

// ── ChangeRefundDestination ──────────────────────────────────────────────────

type changeDestReq struct {
	refundBase
	DestinationRef  string `json:"destination_ref"`
	PayerAccountRef string `json:"payer_account_ref,omitempty"`
	ReasonCode      string `json:"reason_code"`
	EvidenceID      string `json:"evidence_id"`
}

// changeRefundDestination replaces the destination (and optionally the payer account) before submission. It always sends the refund
// back to PENDING_APPROVAL with the approval cleared: an approval never covers a destination it was not given for (#30).
func (m *Module) changeRefundDestination(w http.ResponseWriter, r *http.Request) {
	var req changeDestReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, cur, ok := m.prepareRefund(w, r, req.refundBase, ActionRefundCreate)
	if !ok {
		return
	}
	ctx := r.Context()
	switch {
	case req.ExpectedVersion == nil:
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	case strings.TrimSpace(req.DestinationRef) == "":
		m.fail(w, kit.Invalid("destination_ref is required"))
		return
	case strings.TrimSpace(req.ReasonCode) == "":
		m.fail(w, reasonRequired())
		return
	case strings.TrimSpace(req.EvidenceID) == "":
		m.fail(w, evidenceRequired("to change a refund destination"))
		return
	case m.dest == nil:
		m.fail(w, kit.Unavailable("destination verification not wired: the destination was not changed"))
		return
	case cur.State != RfPendingApproval && cur.State != RfApproved:
		m.fail(w, errTransition("the destination of a "+cur.State+" refund cannot be changed"))
		return
	}
	info, err := m.dest.Verify(ctx, c, req.DestinationRef)
	if err != nil {
		m.fail(w, err)
		return
	}
	if err := checkDestination(info, cur.CustomerPartyID, cur.Currency); err != nil {
		m.fail(w, err)
		return
	}
	payer := req.PayerAccountRef
	if payer == "" {
		payer = cur.PayerAccountRef
	}
	out, err := m.lockRefund(ctx, c, cur.RefundID, func(tx pgx.Tx, p *Refund) error {
		if p.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		if p.State != RfPendingApproval && p.State != RfApproved {
			return errTransition("the destination of a " + p.State + " refund cannot be changed")
		}
		before := *p
		nr := *p
		nr.DestinationRef, nr.DestinationFingerprint, nr.PayerAccountRef = req.DestinationRef, info.Fingerprint, payer
		ver, _ := json.Marshal(info.Evidence)
		if err := setRefund(ctx, tx, c.TenantID, p.RefundID, `destination_ref = $3, destination_fingerprint = $4, destination_verification = $5, payer_account_ref = $6,
			binding_hash = $7, approved_hash = NULL, approved_by = NULL, approved_at = NULL, state = 'PENDING_APPROVAL'`,
			nr.DestinationRef, nr.DestinationFingerprint, ver, nr.PayerAccountRef, nr.Binding()); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "REFUND", p.RefundID, "changeRefundDestination", p.State, RfPendingApproval,
			map[string]any{"from_destination": before.DestinationRef, "to_destination": nr.DestinationRef, "approval_invalidated": before.State == RfApproved}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateRefund, p.RefundID, "changeRefundDestination", req.ReasonCode, req.EvidenceID,
			map[string]any{"state": before.State, "destination_ref": before.DestinationRef, "binding_hash": before.BindingHash},
			map[string]any{"state": RfPendingApproval, "destination_ref": nr.DestinationRef, "binding_hash": nr.Binding()},
			map[string]any{"destination_verification": info.Evidence}); err != nil {
			return err
		}
		if before.State == RfApproved {
			nr.State, nr.Version = RfPendingApproval, p.Version+1
			return emitRefund(ctx, tx, c, &nr, EvRefundInvalidated, map[string]any{"reason": "destination changed after approval", "reason_code": req.ReasonCode})
		}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, refundOut{Refund: out})
}

// ── ApproveRefund ────────────────────────────────────────────────────────────

type approveRefundReq struct {
	refundBase
	ReasonCode string `json:"reason_code,omitempty"`
}

// approveRefund approves the refund as currently bound (source + amount + destination + payer account). The approver is never the
// requester. The destination is re-verified first: if it no longer matches what was requested the approval is refused.
func (m *Module) approveRefund(w http.ResponseWriter, r *http.Request) {
	var req approveRefundReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, cur, ok := m.prepareRefund(w, r, req.refundBase, ActionRefundApprove)
	if !ok {
		return
	}
	ctx := r.Context()
	if req.ExpectedVersion == nil {
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	}
	if m.dest == nil {
		m.fail(w, kit.Unavailable("destination verification not wired: the refund was not approved"))
		return
	}
	if cur.State != RfPendingApproval {
		m.fail(w, errTransition("only a PENDING_APPROVAL refund can be approved (it is "+cur.State+")"))
		return
	}
	if c.PrincipalID == cur.RequestedBy {
		m.fail(w, m.denied(ctx, c, AggregateRefund, cur.RefundID, "approveRefund", errSoD("a refund cannot be approved by the principal who requested it"), nil))
		return
	}
	info, err := m.dest.Verify(ctx, c, cur.DestinationRef)
	if err != nil {
		m.fail(w, err)
		return
	}
	if !info.Active() || info.Fingerprint != cur.DestinationFingerprint {
		m.fail(w, m.denied(ctx, c, AggregateRefund, cur.RefundID, "approveRefund",
			errDestinationChanged("the destination no longer matches the verified destination of the request: change the refund destination and re-approve"),
			map[string]any{"destination_status": info.Status}))
		return
	}
	now := m.deps.Now()
	out, err := m.lockRefund(ctx, c, cur.RefundID, func(tx pgx.Tx, p *Refund) error {
		if p.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		if p.State != RfPendingApproval {
			return errTransition("only a PENDING_APPROVAL refund can be approved (it is " + p.State + ")")
		}
		if c.PrincipalID == p.RequestedBy {
			return errSoD("a refund cannot be approved by the principal who requested it")
		}
		if p.Binding() != p.BindingHash {
			return errDestinationChanged("the refund content no longer matches its binding hash")
		}
		if err := setRefund(ctx, tx, c.TenantID, p.RefundID, `state = 'APPROVED', approved_hash = binding_hash, approved_by = $3, approved_at = $4`, c.PrincipalID, now); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "REFUND", p.RefundID, "approveRefund", p.State, RfApproved, map[string]any{"approved_hash": p.BindingHash}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateRefund, p.RefundID, "approveRefund", req.ReasonCode, "", map[string]any{"state": p.State},
			map[string]any{"state": RfApproved, "approved_by": c.PrincipalID, "approved_hash": p.BindingHash}, map[string]any{"destination_status": info.Status}); err != nil {
			return err
		}
		na := *p
		na.State, na.Version = RfApproved, p.Version+1
		return emitRefund(ctx, tx, c, &na, EvRefundApproved, map[string]any{"approved_by": c.PrincipalID, "approved_hash": p.BindingHash})
	})
	if err != nil {
		var p *kit.Problem
		if errors.As(err, &p) && p.Code == kit.CodeSegregationOfDuties {
			err = m.denied(ctx, c, AggregateRefund, cur.RefundID, "approveRefund", err, nil)
		}
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, refundOut{Refund: out})
}

// ── CancelUnsubmittedRefund ──────────────────────────────────────────────────

type cancelRefundReq struct {
	refundBase
	ReasonCode string `json:"reason_code"`
}

// cancelUnsubmittedRefund cancels a refund that has not left AR and releases its reservation. Once submitted the only way out is
// reconciliation with the payment path.
func (m *Module) cancelUnsubmittedRefund(w http.ResponseWriter, r *http.Request) {
	var req cancelRefundReq
	if !kit.Decode(w, r, &req) {
		return
	}
	c, cur, ok := m.prepareRefund(w, r, req.refundBase, ActionRefundCreate)
	if !ok {
		return
	}
	ctx := r.Context()
	switch {
	case req.ExpectedVersion == nil:
		m.fail(w, kit.ErrExpectedVersionRequired)
		return
	case strings.TrimSpace(req.ReasonCode) == "":
		m.fail(w, reasonRequired())
		return
	case m.receipts == nil:
		m.fail(w, kit.Unavailable("AR-08 receipts not wired: the reservation cannot be released, nothing changed"))
		return
	case !RefundCanCancel(cur.State):
		m.fail(w, errTransition("a "+cur.State+" refund can no longer be cancelled: it has been submitted for payment"))
		return
	}
	out, err := m.lockRefund(ctx, c, cur.RefundID, func(tx pgx.Tx, p *Refund) error {
		if p.Version != *req.ExpectedVersion {
			return kit.ErrStaleVersion
		}
		if !RefundCanCancel(p.State) {
			return errTransition("a " + p.State + " refund can no longer be cancelled: it has been submitted for payment")
		}
		if err := m.receipts.ReleaseUnapplied(ctx, tx, c, "REFUND:"+p.RefundID); err != nil {
			return err
		}
		if err := setRefund(ctx, tx, c.TenantID, p.RefundID, `state = 'CANCELLED', cancel_reason = $3`, req.ReasonCode); err != nil {
			return err
		}
		if err := addHistory(ctx, tx, c, "REFUND", p.RefundID, "cancelUnsubmittedRefund", p.State, RfCancelled, map[string]any{"reason_code": req.ReasonCode}); err != nil {
			return err
		}
		if err := audit(ctx, tx, c, AggregateRefund, p.RefundID, "cancelUnsubmittedRefund", req.ReasonCode, "", map[string]any{"state": p.State}, map[string]any{"state": RfCancelled}, nil); err != nil {
			return err
		}
		nc := *p
		nc.State, nc.Version = RfCancelled, p.Version+1
		return emitRefund(ctx, tx, c, &nc, EvRefundCancelled, map[string]any{"reason_code": req.ReasonCode})
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, refundOut{Refund: out})
}
