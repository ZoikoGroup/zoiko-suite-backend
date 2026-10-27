package ar10

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// params merges the query string with (for POST) the string / number fields of a JSON body.
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
					case bool:
						out[k] = strconv.FormatBool(t)
					}
				}
			}
		}
	}
	return out
}

func pageOf(p map[string]string) (limit, offset int) {
	limit, offset = 50, 0
	if n, err := strconv.Atoi(p["limit"]); err == nil && n > 0 {
		limit = n
	}
	if limit > 200 {
		limit = 200
	}
	if n, err := strconv.Atoi(p["offset"]); err == nil && n > 0 {
		offset = n
	}
	return
}

func (m *Module) getRefund(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	ctx := r.Context()
	var rf *Refund
	var exc []ControlException
	err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (e error) { rf, e = getRefund(ctx, tx, c.TenantID, p["refund_id"], false); return })
	if err == nil {
		err = m.read(ctx, c.TenantID, func(tx pgx.Tx) (e error) { exc, e = m.refundExceptions(ctx, tx, c.TenantID, rf); return })
	}
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"refund": rf, "control_exceptions": exc})
}

// getRefundPaymentStatus returns the locally recorded payment state; live=true additionally asks the payment path (read-only).
func (m *Module) getRefundPaymentStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	ctx := r.Context()
	var rf *Refund
	if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (e error) { rf, e = getRefund(ctx, tx, c.TenantID, p["refund_id"], false); return }); err != nil {
		m.fail(w, err)
		return
	}
	out := map[string]any{"refund_id": rf.RefundID, "state": rf.State, "external_attempt_id": rf.ExternalAttemptID, "external_state": rf.ExternalState,
		"provider_request_id": rf.ProviderRequestID, "payment_status_id": rf.PaymentStatusID, "payment_state": rf.PaymentState, "state_unknown": rf.State == RfUnknown,
		"settlement_owner": "payment-status-svc"}
	if p["live"] == "true" && m.gateway != nil {
		c.LegalEntityID = rf.LegalEntityID
		live := map[string]any{}
		if rf.ExternalAttemptID != "" {
			if a, err := m.gateway.GetAttempt(ctx, c, rf.ExternalAttemptID); err != nil {
				live["attempt_error"] = "payment path unavailable"
			} else {
				live["attempt_status"] = a.Status
			}
		}
		if rf.PaymentStatusID != "" {
			if ps, err := m.gateway.GetPayment(ctx, c, rf.PaymentStatusID); err != nil {
				live["payment_error"] = "payment-status-svc unavailable"
			} else {
				live["payment_status"] = ps.Status
			}
		}
		out["live"] = live
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) getWriteOff(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	ctx := r.Context()
	var wo *WriteOff
	var exc []ControlException
	err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (e error) {
		if wo, e = getWriteOff(ctx, tx, c.TenantID, p["write_off_id"], false); e != nil {
			return
		}
		exc, e = m.writeOffExceptions(ctx, tx, c.TenantID, wo)
		return
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"write_off": wo, "control_exceptions": exc})
}

type actionView struct {
	Command string `json:"command"`
	Allowed bool   `json:"allowed"`
	Blocked string `json:"blocked_reason,omitempty"`
}

// getAvailableActions derives the commands the caller can run now from the aggregate's state, the caller's permissions and the
// segregation-of-duties rules; it never returns a hard-coded true.
func (m *Module) getAvailableActions(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	ctx := r.Context()
	acts := []actionView{}
	add := func(cmd string, allowed bool, why string) {
		if allowed {
			why = ""
		}
		acts = append(acts, actionView{Command: cmd, Allowed: allowed, Blocked: why})
	}
	perm := func(action string) (bool, string) {
		if err := m.deps.Authz.CheckAllowed(ctx, c.PrincipalID, c.LegalEntityID, action); err != nil {
			return false, "the caller lacks " + action
		}
		return true, ""
	}
	switch {
	case p["refund_id"] != "":
		var rf *Refund
		if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (e error) { rf, e = getRefund(ctx, tx, c.TenantID, p["refund_id"], false); return }); err != nil {
			m.fail(w, err)
			return
		}
		c.LegalEntityID = rf.LegalEntityID
		own := c.PrincipalID == rf.RequestedBy
		switch rf.State {
		case RfPendingApproval, RfApproved:
			okc, whyc := perm(ActionRefundCreate)
			add("changeRefundDestination", okc, whyc)
			add("cancelUnsubmittedRefund", okc, whyc)
			if rf.State == RfPendingApproval {
				oka, whya := perm(ActionRefundApprove)
				if oka && own {
					oka, whya = false, "the requester cannot approve their own refund"
				}
				add("approveRefund", oka, whya)
			} else {
				oks, whys := perm(ActionRefundSubmit)
				if oks && own {
					oks, whys = false, "the requester cannot release their own refund"
				}
				if oks && (m.gateway == nil) {
					oks, whys = false, "the payment path is not wired"
				}
				add("submitRefundForPayment", oks, whys)
			}
		case RfSubmitted, RfPending, RfUnknown, RfSettled:
			okr, whyr := perm(ActionRefundReconcile)
			add("reconcileRefundState", okr, whyr)
		}
	case p["write_off_id"] != "":
		var wo *WriteOff
		if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (e error) { wo, e = getWriteOff(ctx, tx, c.TenantID, p["write_off_id"], false); return }); err != nil {
			m.fail(w, err)
			return
		}
		c.LegalEntityID = wo.LegalEntityID
		own := c.PrincipalID == wo.RequestedBy
		switch wo.State {
		case WoPendingApproval, WoApproved:
			okt, whyt := perm(ActionWriteOffTaxReview)
			if okt && own {
				okt, whyt = false, "the requester cannot decide the tax treatment"
			}
			add("resolveWriteOffTaxTreatment", okt, whyt)
		}
		switch wo.State {
		case WoPendingApproval:
			oka, whya := false, "authority policy not wired"
			if m.authority != nil {
				held, err := m.authority.Held(ctx, c)
				switch {
				case err != nil:
					whya = "authority cannot be evaluated"
				case own:
					whya = "the requester cannot approve their own write-off"
				case held == 0:
					whya = "the caller holds no write-off approval authority"
				case held < wo.RequiredTier:
					whya = "the write-off needs approval authority tier " + strconv.Itoa(wo.RequiredTier)
				default:
					oka, whya = true, ""
				}
			}
			add("approveWriteOff", oka, whya)
		case WoApproved:
			okc, whyc := perm(ActionWriteOffCommit)
			if okc && wo.TaxTreatment == TaxReviewNeeded {
				okc, whyc = false, "the tax treatment is unresolved"
			}
			add("commitWriteOff", okc, whyc)
		}
	default:
		m.fail(w, kit.Invalid("refund_id or write_off_id is required"))
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"actions": acts})
}

// getHistory returns the append-only lifecycle of one refund / write-off, or the latest entries of a legal entity.
func (m *Module) getHistory(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	limit, offset := pageOf(p)
	ctx := r.Context()
	id := p["refund_id"]
	if id == "" {
		id = p["write_off_id"]
	}
	if id == "" && p["legal_entity_id"] == "" {
		m.fail(w, kit.Invalid("refund_id, write_off_id or legal_entity_id is required"))
		return
	}
	if id != "" {
		if err := mustUUID("id", id); err != nil {
			m.fail(w, err)
			return
		}
	}
	out := []HistoryEntry{}
	err := m.read(ctx, c.TenantID, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if id != "" {
			rows, err = tx.Query(ctx, `SELECT seq, aggregate_type, aggregate_id::text, command, from_state, to_state, actor_id, detail, recorded_at FROM ar10_history
				WHERE tenant_id = $1 AND aggregate_id = $2 ORDER BY seq LIMIT $3 OFFSET $4`, c.TenantID, id, limit, offset)
		} else {
			rows, err = tx.Query(ctx, `SELECT seq, aggregate_type, aggregate_id::text, command, from_state, to_state, actor_id, detail, recorded_at FROM ar10_history
				WHERE tenant_id = $1 AND legal_entity_id = $2 ORDER BY seq DESC LIMIT $3 OFFSET $4`, c.TenantID, p["legal_entity_id"], limit, offset)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h HistoryEntry
			var d []byte
			if err := rows.Scan(&h.Seq, &h.AggregateType, &h.AggregateID, &h.Command, &h.FromState, &h.ToState, &h.ActorID, &d, &h.RecordedAt); err != nil {
				return err
			}
			_ = json.Unmarshal(d, &h.Detail)
			out = append(out, h)
		}
		return rows.Err()
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"history": out})
}

// getControlExceptions lists the open control exceptions of a legal entity (settled refund without its AR consequence or posting,
// failed postings, UNKNOWN refunds, committed write-offs without a posting).
func (m *Module) getControlExceptions(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	if err := mustUUID("legal_entity_id", p["legal_entity_id"]); err != nil {
		m.fail(w, err)
		return
	}
	ex, err := m.ControlExceptions(r.Context(), c.TenantID, p["legal_entity_id"])
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"control_exceptions": ex})
}
