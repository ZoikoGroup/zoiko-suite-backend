//go:build integration

package ar04

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// ── segregation of duties / maker-checker ────────────────────────────────────

func TestSoD_ApproverIsNeitherCreatorNorSubmitter(t *testing.T) {
	e := newEnv(t)
	inv := e.submit(e.create(pBoss, nil)) // pBoss created; pCreator submitted
	require.Equal(t, pCreator, inv.SubmittedBy)
	code, body := e.mk(pBoss, "approveInvoice", inv, map[string]any{"content_hash": inv.ContentHash})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	require.Equal(t, 1, e.denials(inv.InvoiceID, "approveInvoice"), "the refused approval leaves audit evidence")
	require.Equal(t, StatePendingApproval, e.get(inv.InvoiceID).State)

	// The submitter cannot approve either, even holding the approve action.
	b := e.create(pCreator, nil)
	b = e.step(pBoss, "submitInvoiceForApproval", b, nil)
	code, body = e.mk(pBoss, "approveInvoice", b, map[string]any{"content_hash": b.ContentHash})
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body), string(body))
	// A principal without the approve action is simply forbidden.
	code, body = e.mk(pCreator, "approveInvoice", b, map[string]any{"content_hash": b.ContentHash})
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, kit.CodeForbidden, errCode(t, body))
	require.Equal(t, StateApproved, e.approveAs(pApprover, b).State)

	// The database refuses it independently of the service.
	c := e.submit(e.create(pCreator, nil))
	requireRefused(t, e.exec(`UPDATE ar04_invoices SET approved_by = created_by WHERE invoice_id = $1`, c.InvoiceID), "23514", "")
	requireRefused(t, e.exec(`INSERT INTO ar04_invoice_approvals (approval_id, tenant_id, legal_entity_id, invoice_id, content_hash, gross_total, created_by, submitted_by, approved_by)
		VALUES (gen_random_uuid(), $1, $2, $3, 'h', 1, 'a', 'b', 'a')`, e.tenant, e.entity, c.InvoiceID), "23514", "")
}

func TestSoD_MaterialInvoiceIssuerCannotBeTheApprover(t *testing.T) {
	old := MaterialInvoiceThreshold
	MaterialInvoiceThreshold = mustAmount("1000")
	defer func() { MaterialInvoiceThreshold = old }()
	e := newEnv(t)
	inv := e.approveAs(pBoss, e.submit(e.create(pCreator, nil))) // gross 1500.60 > 1000
	code, body := e.mk(pBoss, "issueInvoice", inv, nil)
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	require.Equal(t, 1, e.denials(inv.InvoiceID, "issueInvoice"))
	require.Equal(t, StateApproved, e.get(inv.InvoiceID).State)
	require.Zero(t, e.counter())
	code, body = e.query(pBoss, "getAvailableActions", "invoice_id="+inv.InvoiceID)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), "other than its approver")
	require.Equal(t, StateIssued, e.issue(pIssuer, inv).State)

	// Below the threshold the issuer may also be the approver.
	MaterialInvoiceThreshold = mustAmount("100000")
	small := e.approveAs(pBoss, e.submit(e.create(pCreator, nil)))
	require.Equal(t, StateIssued, e.issue(pBoss, small).State)
}

// ── approval is bound to the content hash ────────────────────────────────────

func TestApprovalIsBoundToTheContentHash(t *testing.T) {
	e := newEnv(t)
	inv := e.submit(e.create(pCreator, nil))
	code, body := e.mk(pApprover, "approveInvoice", inv, map[string]any{"content_hash": "deadbeef"})
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeInvoiceApprovalInvalidated, errCode(t, body))
	approved := e.approveAs(pApprover, inv)
	old := approved.ContentHash

	// Any amendment after approval invalidates it: back to draft, tax result and approval gone.
	d := e.step(pCreator, "amendInvoiceDraft", approved, map[string]any{"notes": "changed after approval"})
	require.Equal(t, StateDraft, d.State)
	require.Empty(t, d.ApprovedHash)
	require.Empty(t, d.TaxGroups)
	require.NotEqual(t, old, d.ContentHash)
	code, body = e.mk(pIssuer, "issueInvoice", d, nil)
	require.Equal(t, kit.CodeInvalidTransition, errCode(t, body), string(body))
	re := e.submit(d)
	code, body = e.mk(pApprover, "approveInvoice", re, map[string]any{"content_hash": old})
	require.Equal(t, kit.CodeInvoiceApprovalInvalidated, errCode(t, body), "an approval of old content cannot be replayed on new content")
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar04_invoice_approvals WHERE invoice_id = $1`, inv.InvoiceID), "the original approval stays as evidence")
	require.Equal(t, StateIssued, e.issue(pIssuer, e.approveAs(pApprover, re)).State)
}

// ── drafting, amending, tax cannot be overridden ─────────────────────────────

func TestAmendDraft_RecomputesAndNeverTakesTaxFromTheEditor(t *testing.T) {
	e := newEnv(t)
	inv := e.create(pCreator, nil)
	code, body := e.mk(pCreator, "amendInvoiceDraft", inv, map[string]any{"tax_amount": "1.00"})
	require.Equal(t, http.StatusBadRequest, code, string(body))
	code, body = e.mk(pCreator, "amendInvoiceDraft", inv, map[string]any{"notes": "Q4 licence"})
	require.Equal(t, http.StatusBadRequest, code, "an amendment that changes nothing is refused: "+string(body))

	a := e.step(pCreator, "amendInvoiceDraft", inv, map[string]any{"lines": []map[string]any{lineBody("Only line", "3", "100.00", "SOFTWARE", "DIGITAL_SERVICES")}, "customer_reference": "PO-2"})
	require.Equal(t, StateDraft, a.State)
	require.Equal(t, inv.Version+1, a.Version)
	require.Equal(t, mustAmount("300"), a.NetTotal)
	require.Len(t, a.Lines, 1)
	require.Equal(t, "PO-2", a.Content.CustomerRef)
	require.NotEqual(t, inv.ContentHash, a.ContentHash)

	// Amending a pending invoice discards the determinations and returns it to draft.
	p := e.submit(a)
	require.Len(t, p.TaxGroups, 1)
	require.Equal(t, mustAmount("360.00"), p.GrossTotal)
	d := e.step(pCreator, "amendInvoiceDraft", p, map[string]any{"notes": "n"})
	require.Equal(t, StateDraft, d.State)
	require.Empty(t, d.TaxGroups)
	require.Zero(t, d.TaxTotal)
	require.Zero(t, e.count(`SELECT count(*) FROM ar04_invoice_tax_determinations WHERE invoice_id = $1`, d.InvoiceID))
	for _, l := range d.Lines {
		require.Zero(t, l.Tax)
	}
	// Incomplete tax inputs are refused at submit (validated before TAX is called).
	e.tax.reset()
	x := e.step(pCreator, "amendInvoiceDraft", d, map[string]any{"tax_context": map[string]any{"supply_type": "B2B"}})
	code, body = e.mk(pCreator, "submitInvoiceForApproval", x, nil)
	require.Equal(t, http.StatusBadRequest, code, string(body))
	require.Empty(t, e.tax.requests, "TAX is not called with an incomplete request")
	// Customer and currency are identity: they cannot be amended.
	code, _ = e.mk(pCreator, "amendInvoiceDraft", x, map[string]any{"currency": "EUR"})
	require.Equal(t, http.StatusBadRequest, code)
}

func TestIdempotentCreateAndAvailableActions(t *testing.T) {
	e := newEnv(t)
	key := uuid.NewString()
	b := createBody(e.entity, e.party)
	c1, b1, _ := call(t, e.h, http.MethodPost, base+"commands/createInvoiceDraft", e.tenant, pCreator, key, b)
	c2, b2, hdr := call(t, e.h, http.MethodPost, base+"commands/createInvoiceDraft", e.tenant, pCreator, key, b)
	require.Equal(t, http.StatusCreated, c1)
	require.Equal(t, http.StatusCreated, c2)
	require.Equal(t, "true", hdr.Get("Idempotent-Replay"))
	require.JSONEq(t, string(b1), string(b2))
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar04_invoices WHERE tenant_id = $1`, e.tenant))

	inv := asInvoice(t, b1)
	avail := func(principal string, id string) map[string]actionState {
		code, body := e.query(principal, "getAvailableActions", "invoice_id="+id)
		require.Equal(t, http.StatusOK, code, string(body))
		var r struct {
			Actions []actionState `json:"actions"`
		}
		require.NoError(t, jsonDecode(body, &r))
		out := map[string]actionState{}
		for _, a := range r.Actions {
			out[a.Action] = a
		}
		return out
	}
	a := avail(pCreator, inv.InvoiceID)
	require.True(t, a["AmendInvoiceDraft"].Available)
	require.True(t, a["SubmitInvoiceForApproval"].Available)
	require.False(t, a["ApproveInvoice"].Available)
	require.False(t, a["IssueInvoice"].Available)
	require.False(t, avail(pNobody, inv.InvoiceID)["AmendInvoiceDraft"].Available, "permission is part of availability")
	require.Contains(t, avail(pNobody, inv.InvoiceID)["AmendInvoiceDraft"].Reason, "lacks")
	p := e.submit(inv)
	require.False(t, avail(pCreator, p.InvoiceID)["ApproveInvoice"].Available, "the submitter cannot approve")
	require.True(t, avail(pApprover, p.InvoiceID)["ApproveInvoice"].Available)
	ap := e.approveAs(pApprover, p)
	require.True(t, avail(pIssuer, ap.InvoiceID)["IssueInvoice"].Available)
	is := e.issue(pIssuer, ap)
	a = avail(pBoss, is.InvoiceID)
	require.False(t, a["AmendInvoiceDraft"].Available)
	require.False(t, a["IssueInvoice"].Available)
	require.False(t, a["CancelUnissuedInvoice"].Available)
	require.True(t, a["RequestLegalCancellation"].Available)
	require.True(t, a["MarkDeliveryResult"].Available)
	require.True(t, a["LinkCorrectionDocument"].Available)
}

// ── orthogonal dimensions ────────────────────────────────────────────────────

func TestDimensionsAreWrittenIndependently(t *testing.T) {
	e := newEnv(t)
	inv := e.issued()
	ctx := context.Background()
	caller := e.caller()
	cols := map[string]string{"einvoice": "einvoice_state", "settlement": "settlement_state", "dispute": "dispute_state"}
	run := map[string]func(pgx.Tx, string) error{
		"einvoice":   func(tx pgx.Tx, v string) error { return e.m.SetEInvoiceState(ctx, tx, caller, inv.InvoiceID, v) },
		"settlement": func(tx pgx.Tx, v string) error { return e.m.SetSettlementState(ctx, tx, caller, inv.InvoiceID, v) },
		"dispute":    func(tx pgx.Tx, v string) error { return e.m.SetDisputeState(ctx, tx, caller, inv.InvoiceID, v) },
	}
	values := map[string]string{"einvoice": "PENDING_UNKNOWN", "settlement": "PARTIALLY_PAID", "dispute": "DISPUTED"}
	changing := []string{"delivery_state", "einvoice_state", "settlement_state", "dispute_state", "attention_state", "version", "updated_at", "settlement_changed_at"}
	for _, dim := range []string{"einvoice", "settlement", "dispute"} {
		before := e.row(inv.InvoiceID)
		require.NoError(t, e.tx(func(tx pgx.Tx) error { return run[dim](tx, values[dim]) }))
		after := e.row(inv.InvoiceID)
		require.Equal(t, values[dim], after[cols[dim]], dim)
		for k, v := range before {
			skip := false
			for _, c := range changing {
				if k == c {
					skip = true
				}
			}
			if !skip {
				require.Equal(t, v, after[k], dim+": column "+k+" must not change")
			}
		}
		for other, col := range cols {
			if other != dim && other != "einvoice" {
				require.Equal(t, before[col], after[col], dim+" must not move "+col)
			}
		}
		require.Equal(t, before["delivery_state"], after["delivery_state"])
		require.Equal(t, before["lifecycle_state"], after["lifecycle_state"])
		require.Equal(t, before["attention_state"], after["attention_state"])
		require.EqualValues(t, before["version"].(float64)+1, after["version"])
	}
	// Idempotent: an unchanged value is a no-op (no version bump, no history).
	v := e.get(inv.InvoiceID).Version
	require.NoError(t, e.tx(func(tx pgx.Tx) error { return run["dispute"](tx, "DISPUTED") }))
	require.Equal(t, v, e.get(inv.InvoiceID).Version)
	// Every dimension is visible and separate.
	code, body := e.query(pNobody, "getInvoiceStateDimensions", "invoice_id="+inv.InvoiceID)
	require.Equal(t, http.StatusOK, code, string(body))
	for _, want := range []string{`"lifecycle":"ISSUED"`, `"delivery":"NOT_SENT"`, `"einvoice":"PENDING_UNKNOWN"`, `"settlement":"PARTIALLY_PAID"`, `"dispute":"DISPUTED"`, `"attention":"NONE"`, `"accounting":"QUEUED"`} {
		require.Contains(t, string(body), want)
	}
	// Invalid values and non-issued invoices are refused; the table CHECKs hold for every column.
	require.Equal(t, kit.CodeValidationFailed, mustProblem(t, e.tx(func(tx pgx.Tx) error { return run["settlement"](tx, "ACCEPTED") })).Code)
	d := e.create(pCreator, nil)
	err := e.tx(func(tx pgx.Tx) error { return e.m.SetSettlementState(ctx, tx, caller, d.InvoiceID, "PAID") })
	require.Equal(t, kit.CodeInvalidTransition, mustProblem(t, err).Code)
	err = e.tx(func(tx pgx.Tx) error { return e.m.SetSettlementState(ctx, tx, caller, uuid.NewString(), "PAID") })
	require.Equal(t, kit.CodeNotFound, mustProblem(t, err).Code)
	for _, col := range []string{"delivery_state", "einvoice_state", "settlement_state", "dispute_state", "attention_state", "accounting_state"} {
		requireRefused(t, e.exec(`UPDATE ar04_invoices SET `+col+` = 'BOGUS' WHERE invoice_id = $1`, inv.InvoiceID), "23514", "")
	}
	// A pre-issue invoice cannot carry a non-initial dimension.
	requireRefused(t, e.exec(`UPDATE ar04_invoices SET settlement_state = 'PAID' WHERE invoice_id = $1`, d.InvoiceID), "23514", "ar04_dimensions_only_when_issued")
	// Every port write left history.
	cmds := e.historyCmds(inv.InvoiceID)
	require.Equal(t, []string{"setEInvoiceState", "setSettlementState", "setDisputeState"}, cmds[len(cmds)-3:])
}

// ── kit.Invoices ─────────────────────────────────────────────────────────────

func TestKitInvoices_GetIssuedOnlyForIssued_LinkCorrection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d := e.create(pCreator, nil)
	p := e.submit(d)
	a := e.approveAs(pApprover, p)
	for _, id := range []string{d.InvoiceID, p.InvoiceID, a.InvoiceID, uuid.NewString(), "not-a-uuid"} {
		_, err := e.m.GetIssued(ctx, e.tenant, id)
		require.Equal(t, kit.CodeNotFound, mustProblem(t, err).Code, id)
	}
	inv := e.issue(pIssuer, a)
	got, err := e.m.GetIssued(ctx, e.tenant, inv.InvoiceID)
	require.NoError(t, err)
	require.Equal(t, inv.InvoiceNumber, got.InvoiceNumber)
	_, err = e.m.GetIssued(ctx, uuid.NewString(), inv.InvoiceID)
	require.Equal(t, kit.CodeNotFound, mustProblem(t, err).Code, "another tenant cannot read it")

	link := func(id, kind string) error {
		return e.tx(func(tx pgx.Tx) error { return e.m.LinkCorrection(ctx, tx, e.caller(), id, id[:8]+"-"+kind, kind) })
	}
	require.Equal(t, kit.CodeInvalidTransition, mustProblem(t, link(e.create(pCreator, nil).InvoiceID, KindCreditNote)).Code, "only an issued invoice can be corrected")
	require.NoError(t, link(inv.InvoiceID, KindCreditNote))
	g := e.get(inv.InvoiceID)
	require.Equal(t, StateIssued, g.State, "a credit note keeps the original issued")
	require.Equal(t, inv.IssuedContentHash, g.IssuedContentHash)
	require.Equal(t, inv.Version+1, g.Version)
	require.Equal(t, 1, e.events(inv.InvoiceID, EventInvoiceCorrectionLinked))
	require.NoError(t, link(inv.InvoiceID, KindCreditNote), "idempotent on the correction id")
	require.Equal(t, 1, e.count(`SELECT count(*) FROM ar04_correction_links WHERE invoice_id = $1`, inv.InvoiceID))
	require.Equal(t, g.Version, e.get(inv.InvoiceID).Version)
	require.Equal(t, kit.CodeValidationFailed, mustProblem(t, link(inv.InvoiceID, "VOID")).Code)
	require.NoError(t, link(inv.InvoiceID, KindFullCorrection))
	g = e.get(inv.InvoiceID)
	require.Equal(t, StateCorrected, g.State)
	require.Equal(t, inv.NetTotal, g.NetTotal)
	got, err = e.m.GetIssued(ctx, e.tenant, inv.InvoiceID)
	require.NoError(t, err, "a corrected invoice is still an authoritative issued document")
	require.Equal(t, inv.IssuedContentHash, got.ContentHash)
	requireRefused(t, e.exec(`UPDATE ar04_invoices SET net_total = 1, gross_total = 1 WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "AR04_ISSUED_INVOICE_IMMUTABLE")
	requireRefused(t, e.exec(`UPDATE ar04_correction_links SET kind = 'DEBIT_NOTE' WHERE invoice_id = $1`, inv.InvoiceID), "P0001", "append-only")

	// The operator route is its own privileged action with reason and evidence.
	b := e.issued()
	code, body := e.mk(pLinker, "linkCorrectionDocument", b, map[string]any{"correction_id": "CN-77", "kind": "CREDIT_NOTE", "reason_code": "PRICE", "evidence_id": "E1"})
	require.Equal(t, http.StatusOK, code, string(body))
	code, body = e.mk(pCreator, "linkCorrectionDocument", b, map[string]any{"correction_id": "CN-78", "kind": "CREDIT_NOTE", "reason_code": "PRICE", "evidence_id": "E1"})
	require.Equal(t, http.StatusForbidden, code, string(body))
}

// ── legal cancellation request ───────────────────────────────────────────────

func TestLegalCancellationNeverEditsTheInvoice(t *testing.T) {
	e := newEnv(t)
	d := e.create(pCreator, nil)
	code, body := e.mk(pCancelReq, "requestLegalCancellation", d, map[string]any{"reason_code": "WRONG_CUSTOMER", "evidence_id": "E"})
	require.Equal(t, http.StatusConflict, code, "only an issued invoice: "+string(body))

	inv := e.issued()
	snapshot := e.row(inv.InvoiceID)
	code, body = e.mk(pCancelReq, "requestLegalCancellation", inv, map[string]any{"reason_code": "WRONG_CUSTOMER"})
	require.Equal(t, kit.CodeEvidenceRequired, errCode(t, body), string(body))
	req := e.step(pCancelReq, "requestLegalCancellation", inv, map[string]any{"reason_code": "WRONG_CUSTOMER", "evidence_id": "TKT-9"})
	require.Equal(t, AttentionCancelReq, req.Attention)
	require.Equal(t, StateIssued, req.State, "the invoice is never voided by the request")
	require.Equal(t, inv.IssuedContentHash, req.IssuedContentHash)
	require.Equal(t, inv.InvoiceNumber, req.InvoiceNumber)
	after := e.row(inv.InvoiceID)
	for k, v := range snapshot {
		if k != "attention_state" && k != "version" && k != "updated_at" {
			require.Equal(t, v, after[k], k)
		}
	}
	require.Equal(t, 1, e.events(inv.InvoiceID, EventInvoiceCancellationRequest))
	code, body = e.mk(pCancelReq, "requestLegalCancellation", req, map[string]any{"reason_code": "AGAIN", "evidence_id": "E"})
	require.Equal(t, http.StatusConflict, code, "one open request at a time: "+string(body))

	// A different principal decides; the requester cannot.
	code, _ = e.mk(pCancelReq, "decideLegalCancellation", req, map[string]any{"decision": "APPROVE", "reason_code": "OK"})
	require.Equal(t, http.StatusForbidden, code, "the requester role lacks the decide permission")
	selfDecider := e.step(pBoss, "requestLegalCancellation", e.issued(), map[string]any{"reason_code": "X", "evidence_id": "Y"})
	code, body = e.mk(pBoss, "decideLegalCancellation", selfDecider, map[string]any{"decision": "APPROVE", "reason_code": "OK"})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeSegregationOfDuties, errCode(t, body))
	require.Equal(t, 1, e.denials(selfDecider.InvoiceID, "decideLegalCancellation"))
	rejected := e.step(pCancelApp, "decideLegalCancellation", selfDecider, map[string]any{"decision": "REJECT", "reason_code": "NOT_LEGAL"})
	require.Equal(t, AttentionNone, rejected.Attention)
	require.Equal(t, StateIssued, rejected.State)

	approved := e.step(pCancelApp, "decideLegalCancellation", req, map[string]any{"decision": "APPROVE", "reason_code": "PERMITTED"})
	require.Equal(t, AttentionCancelOK, approved.Attention)
	require.Equal(t, StateIssued, approved.State, "approval still does not void: the correction document does")
	require.Equal(t, 1, e.events(inv.InvoiceID, EventInvoiceCancellationDecided))

	// The correction document (AR-06) fulfils the request and links to the original.
	require.NoError(t, e.tx(func(tx pgx.Tx) error {
		return e.m.LinkCorrection(context.Background(), tx, e.caller(), inv.InvoiceID, "CN-1", KindFullCorrection)
	}))
	g := e.get(inv.InvoiceID)
	require.Equal(t, AttentionCorrected, g.Attention)
	require.Equal(t, StateCorrected, g.State)
	require.Equal(t, inv.IssuedContentHash, g.IssuedContentHash)
	code, body = e.query(pNobody, "getInvoiceHistory", "invoice_id="+inv.InvoiceID)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), `"status":"FULFILLED"`)
}

// ── optimistic concurrency, tenant isolation ─────────────────────────────────

func TestOptimisticConcurrency(t *testing.T) {
	e := newEnv(t)
	inv := e.create(pCreator, nil)
	stale := ibody(inv, map[string]any{"notes": "x"})
	e.step(pCreator, "amendInvoiceDraft", inv, map[string]any{"notes": "first"})
	code, body := e.cmd(pCreator, "amendInvoiceDraft", stale)
	require.Equal(t, http.StatusConflict, code, string(body))
	require.Equal(t, kit.CodeStaleVersion, errCode(t, body))
	for _, name := range []string{"submitInvoiceForApproval", "cancelUnissuedInvoice"} {
		code, body = e.cmd(pBoss, name, map[string]any{"invoice_id": inv.InvoiceID, "expected_version": 1, "reason_code": "R"})
		require.Equal(t, kit.CodeStaleVersion, errCode(t, body), name)
	}
	code, body = e.cmd(pCreator, "submitInvoiceForApproval", map[string]any{"invoice_id": inv.InvoiceID})
	require.Equal(t, kit.CodeExpectedVersionMissing, errCode(t, body))

	// Two concurrent submits of the same version: exactly one wins.
	cur := e.get(inv.InvoiceID)
	codes := make([]int, 2)
	bodies := make([][]byte, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], bodies[i], _ = call(t, e.h, http.MethodPost, base+"commands/submitInvoiceForApproval", e.tenant, pCreator, "", ibody(cur, nil))
		}(i)
	}
	wg.Wait()
	wins := 0
	for i, c := range codes {
		if c == http.StatusOK {
			wins++
		} else {
			require.Equal(t, http.StatusConflict, c, string(bodies[i]))
		}
	}
	require.Equal(t, 1, wins)
	require.Equal(t, 2, e.count(`SELECT count(*) FROM ar04_invoice_tax_determinations WHERE invoice_id = $1`, inv.InvoiceID))
}

func TestTenantAndEntityIsolation(t *testing.T) {
	e := newEnv(t)
	inv := e.issued()
	other := uuid.NewString()
	for _, q := range []string{"getInvoice", "getInvoiceAsIssued", "getInvoiceStateDimensions", "getAvailableActions", "getInvoiceHistory", "getInvoiceAccountingStatus"} {
		code, body, _ := call(t, e.h, http.MethodGet, base+q+"?invoice_id="+inv.InvoiceID, other, pBoss, "", nil)
		require.Equal(t, http.StatusNotFound, code, q+string(body))
		require.Equal(t, kit.CodeNotFound, errCode(t, body))
		code, _, _ = call(t, e.h, http.MethodGet, base+q+"?legal_entity_id="+e.entity+"&invoice_number="+inv.InvoiceNumber, other, pBoss, "", nil)
		require.Equal(t, http.StatusNotFound, code, q)
	}
	for _, name := range []string{"amendInvoiceDraft", "submitInvoiceForApproval", "approveInvoice", "issueInvoice", "cancelUnissuedInvoice", "requestLegalCancellation",
		"decideLegalCancellation", "markDeliveryResult", "linkCorrectionDocument"} {
		code, body := e.cmdAs(other, pBoss, name, map[string]any{"invoice_id": inv.InvoiceID, "expected_version": inv.Version, "notes": "x", "content_hash": "h", "reason_code": "R",
			"evidence_id": "E", "result": "SENT", "decision": "APPROVE", "correction_id": "C", "kind": "CREDIT_NOTE"})
		require.GreaterOrEqual(t, code, 400, name)
		require.NotEqual(t, http.StatusOK, code, name+string(body))
	}
	require.Equal(t, inv.Version, e.get(inv.InvoiceID).Version, "foreign tenants changed nothing")
	// A body entity that disagrees with the stored one is refused.
	code, body := e.mk(pBoss, "markDeliveryResult", inv, map[string]any{"legal_entity_id": uuid.NewString(), "result": "SENT"})
	require.Equal(t, http.StatusForbidden, code, string(body))
	require.Equal(t, kit.CodeTenantScope, errCode(t, body))
	exists, err := e.m.NewPathInvoiceExists(context.Background(), other, "", inv.InvoiceID, "")
	require.NoError(t, err)
	require.False(t, exists)
}

// ── control population and the legacy hook ───────────────────────────────────

func TestControlPopulationAndLegacyLookup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b, c := e.issued(), e.issued(), e.issued()
	d := e.create(pCreator, nil) // a draft is never in the population
	_ = d
	require.NoError(t, e.tx(func(tx pgx.Tx) error { return e.m.SetSettlementState(ctx, tx, e.caller(), b.InvoiceID, "PAID") }))
	require.NoError(t, e.tx(func(tx pgx.Tx) error {
		return e.m.SetSettlementState(ctx, tx, e.caller(), c.InvoiceID, "PARTIALLY_PAID")
	}))

	q := domain.ControlPopulationQuery{TenantID: e.tenant, LegalEntityID: e.entity, Limit: 2}
	p1, err := e.m.ControlPopulation(ctx, q)
	require.NoError(t, err)
	require.EqualValues(t, 3, p1.DeclaredTotals.RowCount)
	require.Equal(t, "3001.20", p1.DeclaredTotals.Totals["USD"], "A and C outstanding at gross; B paid")
	require.Len(t, p1.Records, 2)
	require.NotEmpty(t, p1.NextCursor)
	q.AfterRecordID = p1.NextCursor
	p2, err := e.m.ControlPopulation(ctx, q)
	require.NoError(t, err)
	require.Len(t, p2.Records, 1)
	require.Empty(t, p2.NextCursor)
	require.Equal(t, p1.Watermark, p2.Watermark, "the watermark covers the whole set, not the page")
	seen := map[string]domain.ControlRecord{}
	for _, r := range append(p1.Records, p2.Records...) {
		seen[r.RecordID] = r
	}
	require.Len(t, seen, 3)
	require.Equal(t, "0.00", seen[b.InvoiceID].Amount)
	require.Equal(t, "1500.60", seen[a.InvoiceID].Amount)
	require.Equal(t, a.InvoiceNumber, seen[a.InvoiceID].Reference)
	require.Equal(t, "2026-10-12", seen[a.InvoiceID].Date)
	require.Equal(t, "PAID", seen[b.InvoiceID].Attributes["status"])
	require.Equal(t, e.party, seen[a.InvoiceID].Attributes["customer_id"])

	// Cut-off semantics: before issue nothing is in scope; on the settlement day the paid invoice is settled.
	before := testNow.AddDate(0, 0, -1)
	pc, err := e.m.ControlPopulation(ctx, domain.ControlPopulationQuery{TenantID: e.tenant, LegalEntityID: e.entity, CutOff: &before, Limit: 10})
	require.NoError(t, err)
	require.EqualValues(t, 0, pc.DeclaredTotals.RowCount)
	on := testNow
	pc, err = e.m.ControlPopulation(ctx, domain.ControlPopulationQuery{TenantID: e.tenant, LegalEntityID: e.entity, CutOff: &on, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, "3001.20", pc.DeclaredTotals.Totals["USD"])
	// A settlement change moves the watermark; tenant and entity scoping hold.
	require.NoError(t, e.tx(func(tx pgx.Tx) error { return e.m.SetSettlementState(ctx, tx, e.caller(), a.InvoiceID, "PAID") }))
	p3, err := e.m.ControlPopulation(ctx, domain.ControlPopulationQuery{TenantID: e.tenant, LegalEntityID: e.entity, Limit: 10})
	require.NoError(t, err)
	require.NotEqual(t, p1.Watermark, p3.Watermark)
	foreign, err := e.m.ControlPopulation(ctx, domain.ControlPopulationQuery{TenantID: uuid.NewString(), LegalEntityID: e.entity, Limit: 10})
	require.NoError(t, err)
	require.EqualValues(t, 0, foreign.DeclaredTotals.RowCount)
	otherEntity, err := e.m.ControlPopulation(ctx, domain.ControlPopulationQuery{TenantID: e.tenant, LegalEntityID: uuid.NewString(), Limit: 10})
	require.NoError(t, err)
	require.EqualValues(t, 0, otherEntity.DeclaredTotals.RowCount)

	// The legacy pay hook: by id, by number within the entity.
	for name, tc := range map[string]struct {
		tenant, entity, id, number string
		want                       bool
	}{
		"by id":           {e.tenant, "", a.InvoiceID, "", true},
		"by number":       {e.tenant, e.entity, "", a.InvoiceNumber, true},
		"number any ent.": {e.tenant, "", "", a.InvoiceNumber, true},
		"wrong entity":    {e.tenant, uuid.NewString(), "", a.InvoiceNumber, false},
		"other tenant":    {uuid.NewString(), "", a.InvoiceID, "", false},
		"unknown":         {e.tenant, "", uuid.NewString(), "INV-999999", false},
		"draft by id":     {e.tenant, "", d.InvoiceID, "", true},
	} {
		got, err := e.m.NewPathInvoiceExists(ctx, tc.tenant, tc.entity, tc.id, tc.number)
		require.NoError(t, err, name)
		require.Equal(t, tc.want, got, name)
	}
}
