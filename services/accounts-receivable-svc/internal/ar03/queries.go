package ar03

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// params merges the query string with, for POST, the top-level scalar fields of a JSON body
// (the spec wireframe allows GET or POST for queries).
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

// resolveHeader finds the order by order_id, or by legal_entity_id + order_number.
func resolveHeader(r *http.Request, tx pgx.Tx, tenantID string, p map[string]string) (*Header, error) {
	if id := p["order_id"]; id != "" {
		if err := mustUUID("order_id", id); err != nil {
			return nil, err
		}
		return getHeader(r.Context(), tx, tenantID, id, false)
	}
	if p["legal_entity_id"] != "" && p["order_number"] != "" {
		if err := mustUUID("legal_entity_id", p["legal_entity_id"]); err != nil {
			return nil, err
		}
		return getHeaderByNumber(r.Context(), tx, tenantID, p["legal_entity_id"], p["order_number"])
	}
	return nil, kit.Invalid("order_id, or legal_entity_id with order_number, is required")
}

func (m *Module) getSalesOrder(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	vno := 0
	if s := p["version_no"]; s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			m.fail(w, kit.Invalid("version_no must be a positive integer"))
			return
		}
		vno = n
	}
	var out *Order
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		h, err := resolveHeader(r, tx, c.TenantID, p)
		if err != nil {
			return err
		}
		out, err = loadOrder(r.Context(), tx, c.TenantID, h, vno)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// getOrderBillingStatus is the deterministic ordered / invoiced / remaining position per
// line and in total, with the milestones that claim part of the remainder.
func (m *Module) getOrderBillingStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var o *Order
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		h, err := resolveHeader(r, tx, c.TenantID, p)
		if err != nil {
			return err
		}
		o, err = loadOrder(r.Context(), tx, c.TenantID, h, 0)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	var claimed kit.Amount
	for _, x := range o.Milestones {
		if x.State == MsRecorded {
			claimed = claimed.Add(x.ScheduledAmount)
		}
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"order_id": o.OrderID, "order_number": o.OrderNumber, "state": o.State, "hold_active": o.HoldActive,
		"currency": o.Currency, "version": o.Version, "lines": o.Lines, "billing": o.Billing, "milestones": o.Milestones,
		"eligible_unbilled_net": claimed.String(), "unscheduled_net": o.Billing.RemainingNet.Sub(claimed).String()})
}

// eligibilityItem is one milestone's billable verdict.
type eligibilityItem struct {
	MilestoneID string     `json:"milestone_id"`
	ExternalRef string     `json:"external_ref,omitempty"`
	LineNo      int        `json:"line_no,omitempty"`
	Eligible    bool       `json:"eligible"`
	EligibleNet kit.Amount `json:"eligible_net"`
	Quantity    kit.Amount `json:"scheduled_qty"`
	Code        string     `json:"code,omitempty"`
	Reason      string     `json:"reason,omitempty"`
}

// getInvoiceEligibility answers, read-only, what InvoiceEligibility would answer: per
// milestone (or the one named by milestone_id), eligible or the ORDER_NOT_BILLABLE reason.
func (m *Module) getInvoiceEligibility(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var h *Header
	var items []eligibilityItem
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		if h, err = resolveHeader(r, tx, c.TenantID, p); err != nil {
			return err
		}
		lines, err := getLines(r.Context(), tx, c.TenantID, h.OrderID, false)
		if err != nil {
			return err
		}
		verdict := func(ms *Milestone) eligibilityItem {
			it := eligibilityItem{MilestoneID: ms.MilestoneID, ExternalRef: ms.ExternalRef, LineNo: ms.LineNo, EligibleNet: ms.ScheduledAmount, Quantity: ms.ScheduledQty}
			prob := CheckBillable(h, ms)
			if prob == nil {
				prob = checkWithinRemaining(lines, ms, ms.ScheduledAmount)
			}
			if prob != nil {
				it.EligibleNet, it.Code, it.Reason = 0, prob.Code, prob.Msg
				return it
			}
			it.Eligible = true
			return it
		}
		if id := p["milestone_id"]; id != "" {
			ms, err := getMilestone(r.Context(), tx, c.TenantID, h.OrderID, id, false)
			if err != nil {
				return err
			}
			if ms == nil {
				prob := CheckBillable(h, nil)
				if prob == nil {
					prob = errNotBillable("the billable milestone is unknown for this order")
				}
				items = []eligibilityItem{{MilestoneID: id, Code: prob.Code, Reason: prob.Msg}}
				return nil
			}
			items = []eligibilityItem{verdict(ms)}
			return nil
		}
		all, err := getMilestones(r.Context(), tx, c.TenantID, h.OrderID)
		if err != nil {
			return err
		}
		for i := range all {
			items = append(items, verdict(&all[i]))
		}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	if items == nil {
		items = []eligibilityItem{}
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"order_id": h.OrderID, "state": h.State, "hold_active": h.HoldActive, "version": h.Version, "items": items})
}

type actionState struct {
	Action     string   `json:"action"`
	Permission []string `json:"permissions"`
	Available  bool     `json:"available"`
	Reason     string   `json:"reason,omitempty"`
}

// getAvailableActions is derived from state, hold, segregation of duties and the caller's
// permissions, never hard-coded.
func (m *Module) getAvailableActions(w http.ResponseWriter, r *http.Request) {
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
	canT := func(t Transition) bool { _, e := Next(h.State, t); return e == nil }
	preparer := c.PrincipalID == h.CreatedBy || c.PrincipalID == h.SubmittedBy
	type cand struct {
		name  string
		perms []string
		legal bool
		why   string
	}
	approveLegal, approveWhy := canT(TrApprove), "order is not pending approval"
	if approveLegal && h.NetTotal > CommercialApprovalThreshold && preparer {
		approveLegal, approveWhy = false, "an order above the commercial threshold needs a different approver"
	}
	releasePerms := []string{ActionOrderHold}
	releaseLegal, releaseWhy := h.HoldActive, "the order is not on hold"
	if h.HoldActive && h.HoldKind == HoldCredit {
		releasePerms = append(releasePerms, ActionCreditExceptionApprove)
		if c.PrincipalID == h.HeldBy {
			releaseLegal, releaseWhy = false, "a credit hold must be released by a different principal"
		}
	}
	cands := []cand{
		{"AmendSalesOrder", []string{ActionOrderManage}, h.State.CanAmendDraft() || h.State.CanAmendLive(), "the order is final in state " + string(h.State)},
		{"SubmitOrder", []string{ActionOrderManage}, canT(TrSubmit), "order is not a draft"},
		{"ApproveOrder", []string{ActionOrderApprove}, approveLegal, approveWhy},
		{"ConfirmOrder", []string{ActionOrderConfirm}, canT(TrConfirm) && !h.HoldActive,
			ternary(canT(TrConfirm), "the order is on hold", "order is not approved")},
		{"PlaceOrderHold", []string{ActionOrderHold}, canT(TrPlaceHold) && !h.HoldActive, ternary(canT(TrPlaceHold), "the order is already on hold", "a hold cannot be placed in state "+string(h.State))},
		{"ReleaseOrderHold", releasePerms, releaseLegal, releaseWhy},
		{"CancelOrder", []string{ActionOrderCancel}, canT(TrCancel), "the order cannot be cancelled in state " + string(h.State)},
		{"RecordBillableMilestone", []string{ActionOrderManage}, h.State.Billable(), "the order is not confirmed and live (state " + string(h.State) + ")"},
		{"CloseOrder", []string{ActionOrderManage}, canT(TrClose), "the order cannot be closed in state " + string(h.State)},
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
	kit.WriteJSON(w, http.StatusOK, map[string]any{"order_id": h.OrderID, "state": h.State, "hold_active": h.HoldActive, "version": h.Version, "actions": out})
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// VersionSummary is one row of the version list (full content via GetSalesOrder?version_no=).
type VersionSummary struct {
	VersionNo  int        `json:"version_no"`
	Hash       string     `json:"content_hash"`
	ChangeKind string     `json:"change_kind"`
	ReasonCode string     `json:"reason_code,omitempty"`
	EvidenceID string     `json:"evidence_id,omitempty"`
	NetTotal   kit.Amount `json:"net_total"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ApprovalRecord is one approval bound to a version content hash.
type ApprovalRecord struct {
	VersionNo   int        `json:"version_no"`
	ContentHash string     `json:"content_hash"`
	NetTotal    kit.Amount `json:"net_total"`
	SoDRequired bool       `json:"sod_required"`
	CreatedBy   string     `json:"created_by"`
	SubmittedBy string     `json:"submitted_by"`
	ApprovedBy  string     `json:"approved_by"`
	ApprovedAt  time.Time  `json:"approved_at"`
}

// CreditExceptionRecord is one approved credit exception.
type CreditExceptionRecord struct {
	ExceptionID string     `json:"exception_id"`
	VersionNo   int        `json:"version_no"`
	ContentHash string     `json:"content_hash"`
	OrderNet    kit.Amount `json:"order_net"`
	ReasonCode  string     `json:"reason_code"`
	EvidenceID  string     `json:"evidence_id"`
	ApprovedBy  string     `json:"approved_by"`
	RecordedAt  time.Time  `json:"recorded_at"`
}

func (m *Module) getOrderHistory(w http.ResponseWriter, r *http.Request) {
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
	var versions []VersionSummary
	var approvals []ApprovalRecord
	var exceptions []CreditExceptionRecord
	var h *Header
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		ctx := r.Context()
		if h, err = resolveHeader(r, tx, c.TenantID, p); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT order_version, version_no, command, state_after, actor_id, COALESCE(approver_id,''),
				COALESCE(reason_code,''), COALESCE(evidence_id,''), COALESCE(content_hash,''), prior_values, new_values, correlation_id, recorded_at
			FROM ar03_order_history WHERE tenant_id = $1 AND order_id = $2 AND order_version > $3 ORDER BY order_version LIMIT $4`,
			c.TenantID, h.OrderID, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e HistoryEntry
			var st string
			var prior, nw []byte
			if err := rows.Scan(&e.OrderVersion, &e.VersionNo, &e.Command, &st, &e.ActorID, &e.ApproverID, &e.ReasonCode, &e.EvidenceID,
				&e.ContentHash, &prior, &nw, &e.CorrelationID, &e.RecordedAt); err != nil {
				return err
			}
			e.StateAfter = State(st)
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
		vr, err := tx.Query(ctx, `SELECT version_no, content_hash, change_kind, COALESCE(reason_code,''), COALESCE(evidence_id,''), net_total::text, created_by, created_at
			FROM ar03_order_versions WHERE tenant_id = $1 AND order_id = $2 ORDER BY version_no`, c.TenantID, h.OrderID)
		if err != nil {
			return err
		}
		defer vr.Close()
		for vr.Next() {
			var s VersionSummary
			if err := vr.Scan(&s.VersionNo, &s.Hash, &s.ChangeKind, &s.ReasonCode, &s.EvidenceID, &s.NetTotal, &s.CreatedBy, &s.CreatedAt); err != nil {
				return err
			}
			versions = append(versions, s)
		}
		if err := vr.Err(); err != nil {
			return err
		}
		vr.Close()
		ar, err := tx.Query(ctx, `SELECT version_no, content_hash, net_total::text, sod_required, created_by, submitted_by, approved_by, approved_at
			FROM ar03_order_approvals WHERE tenant_id = $1 AND order_id = $2 ORDER BY approved_at, version_no`, c.TenantID, h.OrderID)
		if err != nil {
			return err
		}
		defer ar.Close()
		for ar.Next() {
			var a ApprovalRecord
			if err := ar.Scan(&a.VersionNo, &a.ContentHash, &a.NetTotal, &a.SoDRequired, &a.CreatedBy, &a.SubmittedBy, &a.ApprovedBy, &a.ApprovedAt); err != nil {
				return err
			}
			approvals = append(approvals, a)
		}
		if err := ar.Err(); err != nil {
			return err
		}
		ar.Close()
		er, err := tx.Query(ctx, `SELECT exception_id::text, version_no, content_hash, order_net::text, reason_code, evidence_id, approved_by, recorded_at
			FROM ar03_credit_exceptions WHERE tenant_id = $1 AND order_id = $2 ORDER BY recorded_at`, c.TenantID, h.OrderID)
		if err != nil {
			return err
		}
		defer er.Close()
		for er.Next() {
			var x CreditExceptionRecord
			if err := er.Scan(&x.ExceptionID, &x.VersionNo, &x.ContentHash, &x.OrderNet, &x.ReasonCode, &x.EvidenceID, &x.ApprovedBy, &x.RecordedAt); err != nil {
				return err
			}
			exceptions = append(exceptions, x)
		}
		return er.Err()
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	resp := map[string]any{"order_id": h.OrderID, "items": items}
	if len(items) > limit {
		resp["items"] = items[:limit]
		resp["next_after_version"] = items[limit-1].OrderVersion
	}
	if items == nil {
		resp["items"] = []HistoryEntry{}
	}
	if after == 0 {
		resp["versions"], resp["approvals"], resp["credit_exceptions"] = versions, approvals, exceptions
	}
	kit.WriteJSON(w, http.StatusOK, resp)
}
