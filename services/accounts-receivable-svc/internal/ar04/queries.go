package ar04

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// params merges the query string with, for POST, the top-level scalar fields of a JSON body (the
// spec wireframe allows GET or POST for queries).
func params(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	if r.Method == http.MethodPost && r.Body != nil {
		raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, kit.MaxBodyBytes))
		if err == nil && len(raw) > 0 {
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil {
				for k, v := range m {
					switch t := v.(type) {
					case string:
						out[k] = t
					case float64:
						out[k] = strconv.FormatFloat(t, 'f', -1, 64)
					}
				}
			}
		}
	}
	return out
}

// resolveHeader finds the invoice by invoice_id, or by legal_entity_id + invoice_number
// (+ optional numbering_series).
func resolveHeader(r *http.Request, tx pgx.Tx, tenantID string, p map[string]string) (*Header, error) {
	if id := p["invoice_id"]; id != "" {
		if err := mustUUID("invoice_id", id); err != nil {
			return nil, err
		}
		return getHeader(r.Context(), tx, tenantID, id, false)
	}
	if p["legal_entity_id"] != "" && p["invoice_number"] != "" {
		if err := mustUUID("legal_entity_id", p["legal_entity_id"]); err != nil {
			return nil, err
		}
		return getHeaderByNumber(r.Context(), tx, tenantID, p["legal_entity_id"], p["numbering_series"], p["invoice_number"])
	}
	return nil, kit.Invalid("invoice_id, or legal_entity_id with invoice_number, is required")
}

func (m *Module) getInvoice(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var out *Invoice
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		h, err := resolveHeader(r, tx, c.TenantID, p)
		if err != nil {
			return err
		}
		out, err = loadInvoice(r.Context(), tx, c.TenantID, h)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// getInvoiceAsIssued returns the sealed issued representation exactly as it was frozen, with its
// sha256 and a proof that the stored text still hashes to it. A non-issued invoice is NOT_FOUND:
// nothing downstream may act on a document that was never authoritative.
func (m *Module) getInvoiceAsIssued(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var h *Header
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		if h, err = resolveHeader(r, tx, c.TenantID, p); err != nil {
			return err
		}
		if !h.State.IsIssued() {
			return kit.NotFound("invoice not found")
		}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"invoice_id": h.InvoiceID, "invoice_number": h.InvoiceNumber, "numbering_series": h.NumberingSeries,
		"lifecycle_state": h.State, "issued_at": h.IssuedAt, "issued_by": h.IssuedBy, "schema": SchemaIssuedInvoice,
		"issued_content_hash": h.IssuedContentHash, "integrity_verified": HashOf(h.issuedCanonical) == h.IssuedContentHash,
		"canonical_text": h.issuedCanonical, "representation": json.RawMessage(h.issuedCanonical)})
}

func (m *Module) getInvoiceStateDimensions(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var h *Header
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		h, err = resolveHeader(r, tx, c.TenantID, p)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"invoice_id": h.InvoiceID, "invoice_number": h.InvoiceNumber, "version": h.Version,
		"dimensions": map[string]string{"lifecycle": string(h.State), "delivery": h.Delivery, "einvoice": h.EInvoice, "settlement": h.Settlement,
			"dispute": h.Dispute, "attention": h.Attention, "accounting": h.AccountingState},
		"lifecycle_state": h.State, "delivery_state": h.Delivery, "einvoice_state": h.EInvoice, "settlement_state": h.Settlement,
		"dispute_state": h.Dispute, "attention_state": h.Attention, "accounting_state": h.AccountingState})
}

type actionState struct {
	Action     string   `json:"action"`
	Permission []string `json:"permissions"`
	Available  bool     `json:"available"`
	Reason     string   `json:"reason,omitempty"`
}

// getAvailableActions is derived from state, segregation of duties, wiring and the caller's
// permissions, never hard-coded.
func (m *Module) getAvailableActions(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var h *Header
	var open *CancellationRequest
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		if h, err = resolveHeader(r, tx, c.TenantID, p); err != nil {
			return err
		}
		if h.State.IsIssued() {
			open, err = openCancellation(r.Context(), tx, c.TenantID, h.InvoiceID, false)
		}
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	canT := func(t Transition) bool { _, e := Next(h.State, t); return e == nil }
	preparer := c.PrincipalID == h.CreatedBy || c.PrincipalID == h.SubmittedBy
	type cand struct {
		name  string
		perms []string
		legal bool
		why   string
	}
	approveLegal, approveWhy := canT(TrApprove), "the invoice is not pending approval"
	if approveLegal && preparer {
		approveLegal, approveWhy = false, "an invoice must be approved by a principal who neither created nor submitted it"
	}
	issueLegal, issueWhy := canT(TrIssue), "the invoice is not approved (state "+string(h.State)+")"
	switch {
	case issueLegal && m.openItems == nil:
		issueLegal, issueWhy = false, "AR-07 receivables not wired"
	case issueLegal && IsMaterial(h.GrossTotal) && c.PrincipalID == h.ApprovedBy:
		issueLegal, issueWhy = false, "a material invoice must be issued by a principal other than its approver"
	}
	cancelReqLegal, cancelReqWhy := h.State == StateIssued && h.Attention != AttentionCancelReq && h.Attention != AttentionCancelOK, "a legal cancellation can be requested only for an issued invoice without an open request"
	decideLegal, decideWhy := h.Attention == AttentionCancelReq && open != nil && open.Status == "REQUESTED", "there is no pending cancellation request"
	if decideLegal && open.RequestedBy == c.PrincipalID {
		decideLegal, decideWhy = false, "a cancellation request must be decided by a principal other than the requester"
	}
	deliveryLegal := false
	if h.State.IsIssued() {
		for _, to := range []string{DeliverySent, DeliveryDelivered, DeliveryFailed} {
			if NextDelivery(h.Delivery, to) == nil {
				deliveryLegal = true
			}
		}
	}
	cands := []cand{
		{"AmendInvoiceDraft", []string{ActionInvoiceManage}, h.State.CanAmend(), "the invoice is final in state " + string(h.State) + ": issued content is corrected by a correction document"},
		{"SubmitInvoiceForApproval", []string{ActionInvoiceManage}, canT(TrSubmit), "the invoice is not a draft"},
		{"ApproveInvoice", []string{ActionInvoiceApprove}, approveLegal, approveWhy},
		{"IssueInvoice", []string{ActionInvoiceIssue}, issueLegal, issueWhy},
		{"CancelUnissuedInvoice", []string{ActionInvoiceManage}, canT(TrCancel), "an issued invoice cannot be cancelled: request a legal cancellation / correction document"},
		{"RequestLegalCancellation", []string{ActionInvoiceCancelRequest}, cancelReqLegal, cancelReqWhy},
		{"DecideLegalCancellation", []string{ActionInvoiceCancelApprove}, decideLegal, decideWhy},
		{"MarkDeliveryResult", []string{ActionInvoiceDeliveryRecord}, deliveryLegal, "the invoice is not issued or its delivery is final"},
		{"LinkCorrectionDocument", []string{ActionInvoiceCorrectionLink}, h.State == StateIssued, "a correction can be linked only to an issued invoice"},
	}
	allowed := map[string]bool{}
	for _, cd := range cands {
		for _, perm := range cd.perms {
			if _, done := allowed[perm]; done {
				continue
			}
			e := m.deps.Authz.CheckAllowed(r.Context(), c.PrincipalID, h.LegalEntityID, perm)
			switch {
			case e == nil:
				allowed[perm] = true
			case errors.Is(e, domain.ErrAuthorizationDenied):
				allowed[perm] = false
			default:
				m.fail(w, kit.Unavailable("authorization service unavailable"))
				return
			}
		}
	}
	out := make([]actionState, 0, len(cands))
	for _, cd := range cands {
		permitted, missing := true, ""
		for _, perm := range cd.perms {
			if !allowed[perm] {
				permitted, missing = false, perm
				break
			}
		}
		a := actionState{Action: cd.name, Permission: cd.perms, Available: cd.legal && permitted}
		switch {
		case !cd.legal:
			a.Reason = cd.why
		case !permitted:
			a.Reason = "caller lacks " + missing
		}
		out = append(out, a)
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"invoice_id": h.InvoiceID, "lifecycle_state": h.State, "version": h.Version, "actions": out})
}

func (m *Module) getInvoiceHistory(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	limit := 50
	if s := p["limit"]; s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			m.fail(w, kit.Invalid("limit must be a positive integer"))
			return
		}
		limit = min(n, maxListLimit)
	}
	after := 0
	if s := p["after_version"]; s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			m.fail(w, kit.Invalid("after_version must be a non-negative integer"))
			return
		}
		after = n
	}
	var items []HistoryEntry
	var approvals []ApprovalRecord
	var cancellations []CancellationRequest
	var links []CorrectionLink
	var h *Header
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		ctx := r.Context()
		if h, err = resolveHeader(r, tx, c.TenantID, p); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT invoice_version, command, lifecycle_after, actor_id, COALESCE(approver_id,''), COALESCE(reason_code,''),
				COALESCE(evidence_id,''), COALESCE(content_hash,''), prior_values, new_values, correlation_id, recorded_at
			FROM ar04_invoice_history WHERE tenant_id = $1 AND invoice_id = $2 AND invoice_version > $3 ORDER BY invoice_version LIMIT $4`,
			c.TenantID, h.InvoiceID, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e HistoryEntry
			var st string
			var prior, nw []byte
			if err := rows.Scan(&e.InvoiceVersion, &e.Command, &st, &e.ActorID, &e.ApproverID, &e.ReasonCode, &e.EvidenceID, &e.ContentHash,
				&prior, &nw, &e.CorrelationID, &e.RecordedAt); err != nil {
				return err
			}
			e.LifecycleAfter = State(st)
			_ = json.Unmarshal(prior, &e.PriorValues)
			_ = json.Unmarshal(nw, &e.NewValues)
			items = append(items, e)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		if after != 0 {
			return nil
		}
		if approvals, err = listApprovals(ctx, tx, c.TenantID, h.InvoiceID); err != nil {
			return err
		}
		if cancellations, err = listCancellations(ctx, tx, c.TenantID, h.InvoiceID); err != nil {
			return err
		}
		links, err = listCorrections(ctx, tx, c.TenantID, h.InvoiceID)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	resp := map[string]any{"invoice_id": h.InvoiceID, "items": items}
	if len(items) > limit {
		resp["items"] = items[:limit]
		resp["next_after_version"] = items[limit-1].InvoiceVersion
	}
	if items == nil {
		resp["items"] = []HistoryEntry{}
	}
	if after == 0 {
		resp["approvals"], resp["cancellation_requests"], resp["correction_links"] = approvals, cancellations, links
	}
	kit.WriteJSON(w, http.StatusOK, resp)
}

// Accounting status values reported beside the queue's own.
const (
	AcctNotApplicable = "NOT_APPLICABLE"
	AcctMissing       = "MISSING"
)

// getInvoiceAccountingStatus reports the ACC-04 posting state of the invoice (source_event_id =
// invoice id). An issued invoice with NO posting request is reported MISSING and flagged as a
// control exception (spec §18 #35): the missing accounting event is visible, never silent.
func (m *Module) getInvoiceAccountingStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var h *Header
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		h, err = resolveHeader(r, tx, c.TenantID, p)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	resp := map[string]any{"invoice_id": h.InvoiceID, "invoice_number": h.InvoiceNumber, "lifecycle_state": h.State, "accounting_state": h.AccountingState,
		"source_event_id": h.InvoiceID, "control_exception": false}
	if !h.State.IsIssued() {
		resp["posting_status"] = AcctNotApplicable
		kit.WriteJSON(w, http.StatusOK, resp)
		return
	}
	status, journal, lastErr, found, err := m.deps.DB.PostingStatusFor(r.Context(), c.TenantID, h.InvoiceID)
	if err != nil {
		m.fail(w, err)
		return
	}
	switch {
	case !found:
		resp["posting_status"] = AcctMissing
		resp["control_exception"], resp["exception"] = true, "ISSUED_INVOICE_WITHOUT_POSTING_REQUEST"
	default:
		resp["posting_status"], resp["journal_id"], resp["last_error"] = status, journal, lastErr
		switch status {
		case kit.PostingFailed:
			resp["control_exception"], resp["exception"] = true, "POSTING_FAILED"
		case kit.PostingQuarantined:
			resp["control_exception"], resp["exception"] = true, "POSTING_QUARANTINED"
		}
		resp["mirror_in_sync"] = h.AccountingState == status || (h.AccountingState == "QUEUED" && (status == kit.PostingPending || status == kit.PostingInProgress))
	}
	kit.WriteJSON(w, http.StatusOK, resp)
}
