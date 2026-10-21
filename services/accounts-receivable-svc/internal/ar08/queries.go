package ar08

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/domain"
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

// accountingView is the accounting mirror of one posting; found=false is a visible control exception.
type accountingView struct {
	State        string `json:"state"`
	JournalID    string `json:"journal_id,omitempty"`
	Error        string `json:"error,omitempty"`
	PostingFound bool   `json:"posting_found"`
}

func appAccounting(ctx context.Context, q querier, tenantID string, a *Application) (apply accountingView, unapply *accountingView, exceptions []string, err error) {
	look := func(key string) (bool, error) {
		var n int
		e := q.QueryRow(ctx, `SELECT count(*) FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND source_event_id = $2`, tenantID, key).Scan(&n)
		return n > 0, e
	}
	exceptions = []string{}
	apply = accountingView{State: a.AccountingState, JournalID: a.JournalID, Error: a.AccountingError}
	if apply.PostingFound, err = look(a.ApplicationID); err != nil {
		return
	}
	if !apply.PostingFound {
		exceptions = append(exceptions, "NO_POSTING: the cash application has no accounting posting request")
	}
	if a.Status == AppUnapplied {
		u := &accountingView{State: a.UnapplyState, JournalID: a.UnapplyJournalID}
		if u.PostingFound, err = look(a.ApplicationID + ":unapply"); err != nil {
			return
		}
		if !u.PostingFound {
			exceptions = append(exceptions, "NO_POSTING: the unapplication has no reversing posting request")
		}
		unapply = u
	}
	return
}

func (m *Module) getApplication(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	ctx := r.Context()
	var a *Application
	var rc *Receipt
	var ap accountingView
	var un *accountingView
	var exc []string
	err := m.read(ctx, c.TenantID, func(tx pgx.Tx) error {
		var err error
		switch {
		case p["application_id"] != "":
			a, err = getApplication(ctx, tx, c.TenantID, p["application_id"], false)
		case p["receipt_id"] != "":
			if _, err = getReceipt(ctx, tx, c.TenantID, p["receipt_id"], false); err != nil {
				return err
			}
			if a, err = activeApplication(ctx, tx, c.TenantID, p["receipt_id"], false); err == nil && a == nil {
				a, err = latestUnappliedApplication(ctx, tx, c.TenantID, p["receipt_id"])
			}
		default:
			return kit.Invalid("application_id or receipt_id is required")
		}
		if err != nil {
			return err
		}
		if a == nil {
			return notFound("application")
		}
		if rc, err = getReceipt(ctx, tx, c.TenantID, a.ReceiptID, false); err != nil {
			return err
		}
		ap, un, exc, err = appAccounting(ctx, tx, c.TenantID, a)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	if rc.Blocked {
		exc = append(exc, rc.ControlException+": Banking no longer confirms this settlement")
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"application": a, "receipt": rc, "accounting": map[string]any{"apply": ap, "unapply": un},
		"control_exceptions": exc})
}

func (m *Module) getUnappliedCash(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	limit, offset := pageOf(p)
	ctx := r.Context()
	out := []*Receipt{}
	var total kit.Amount
	err := m.read(ctx, c.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+receiptCols+` FROM ar08_receipts WHERE tenant_id = $1 AND ($2 = '' OR legal_entity_id::text = $2)
			AND ($3 = '' OR currency = $3) AND unapplied_amount > 0 ORDER BY value_date, receipt_id LIMIT $4 OFFSET $5`, c.TenantID, p["legal_entity_id"], p["currency"], limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			rc, err := scanReceipt(rows)
			if err != nil {
				return err
			}
			out = append(out, rc)
			total = total.Add(rc.UnappliedAmount)
		}
		return rows.Err()
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"receipts": out, "page_unapplied_total": total, "limit": limit, "offset": offset})
}

func (m *Module) getMatchCandidates(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	ctx := r.Context()
	gen, _ := strconv.Atoi(p["generation"])
	var cands []Candidate
	var rc *Receipt
	var pending *ManualMatch
	err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) {
		if rc, err = getReceipt(ctx, tx, c.TenantID, p["receipt_id"], false); err != nil {
			return err
		}
		if gen, cands, err = loadCandidates(ctx, tx, c.TenantID, rc.ReceiptID, gen); err != nil {
			return err
		}
		if id, err := pendingMatchID(ctx, tx, c.TenantID, rc.ReceiptID); err != nil {
			return err
		} else if id != "" {
			pending, _, err = getMatch(ctx, tx, c.TenantID, id, false)
			return err
		}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"receipt_id": rc.ReceiptID, "receipt_version": rc.Version, "generation": gen, "candidates": cands,
		"auto_apply_min_score": AutoApplyMinScore, "auto_apply_high_value_limit": AutoApplyHighValueLimit, "pending_manual_match": pending})
}

func (m *Module) getApplicationReconciliation(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	ctx := r.Context()
	var b *Batch
	var rec *Reconciliation
	var live Reconciliation
	err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) {
		if b, err = getBatch(ctx, tx, c.TenantID, p["batch_id"], false); err != nil {
			return err
		}
		if b.ReceiptIDs, err = batchReceiptIDs(ctx, tx, c.TenantID, b.BatchID); err != nil {
			return err
		}
		var fj []byte
		x := Reconciliation{BatchID: b.BatchID}
		switch e := tx.QueryRow(ctx, `SELECT reconciliation_id::text, currency, status, receipts_total, applied_total, unapplied_total, difference_total, findings, performed_by, performed_at
			FROM ar08_reconciliations WHERE tenant_id = $1 AND batch_id = $2 ORDER BY performed_at DESC, reconciliation_id DESC LIMIT 1`, c.TenantID, b.BatchID).
			Scan(&x.ReconciliationID, &x.Currency, &x.Status, &x.ReceiptsTotal, &x.AppliedTotal, &x.UnappliedTotal, &x.DifferenceTotal, &fj, &x.PerformedBy, &x.PerformedAt); {
		case e == nil:
			_ = json.Unmarshal(fj, &x.Findings)
			rec = &x
		case !errors.Is(e, pgx.ErrNoRows):
			return e
		}
		// current totals, read now (a recorded run can be stale)
		return tx.QueryRow(ctx, `SELECT COALESCE(sum(r.amount),0), COALESCE(sum(r.applied_amount),0), COALESCE(sum(r.unapplied_amount),0), COALESCE(sum(r.difference_amount),0)
			FROM ar08_batch_receipts br JOIN ar08_receipts r ON r.receipt_id = br.receipt_id AND r.tenant_id = br.tenant_id WHERE br.tenant_id = $1 AND br.batch_id = $2`,
			c.TenantID, b.BatchID).Scan(&live.ReceiptsTotal, &live.AppliedTotal, &live.UnappliedTotal, &live.DifferenceTotal)
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	balanced := live.ReceiptsTotal == live.AppliedTotal.Add(live.UnappliedTotal).Add(live.DifferenceTotal)
	kit.WriteJSON(w, http.StatusOK, map[string]any{"batch": b, "latest_reconciliation": rec, "current_totals": map[string]any{
		"receipts_total": live.ReceiptsTotal, "applied_total": live.AppliedTotal, "unapplied_total": live.UnappliedTotal, "difference_total": live.DifferenceTotal,
		"balanced": balanced}})
}

type actionState struct {
	Action     string   `json:"action"`
	Permission []string `json:"permission"`
	Available  bool     `json:"available"`
	Reason     string   `json:"reason,omitempty"`
}

// getAvailableActions is derived from the receipt's state, the pending proposal, the caller's SoD position and permissions.
func (m *Module) getAvailableActions(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	ctx := r.Context()
	var rc *Receipt
	var app, prior *Application
	var pending *ManualMatch
	var batchID string
	if err := m.read(ctx, c.TenantID, func(tx pgx.Tx) (err error) {
		if rc, err = getReceipt(ctx, tx, c.TenantID, p["receipt_id"], false); err != nil {
			return err
		}
		if app, err = activeApplication(ctx, tx, c.TenantID, rc.ReceiptID, false); err != nil {
			return err
		}
		if prior, err = latestUnappliedApplication(ctx, tx, c.TenantID, rc.ReceiptID); err != nil {
			return err
		}
		if batchID, err = batchOfReceipt(ctx, tx, c.TenantID, rc.ReceiptID); err != nil {
			return err
		}
		if id, err := pendingMatchID(ctx, tx, c.TenantID, rc.ReceiptID); err != nil || id == "" {
			return err
		} else {
			pending, _, err = getMatch(ctx, tx, c.TenantID, id, false)
			return err
		}
	}); err != nil {
		m.fail(w, err)
		return
	}
	c.LegalEntityID = rc.LegalEntityID
	usable := !rc.Blocked && rc.State == RcptUnapplied && app == nil
	why := func(ok bool, msg string) string {
		if ok {
			return ""
		}
		return msg
	}
	applyWhy := "the receipt is " + rc.State
	switch {
	case rc.Blocked:
		applyWhy = "blocked: " + rc.ControlException
	case batchID == "":
		usable, applyWhy = false, "the receipt is not part of an application batch"
	}
	approveOK := pending != nil && c.PrincipalID != pending.ProposedBy && c.PrincipalID != rc.RegisteredBy && !rc.Blocked
	approveWhy := "there is no pending manual match"
	if pending != nil && !approveOK {
		approveWhy = "a manual match is approved by a principal other than its proposer and the receipt's registrant"
	}
	unapplyOK := app != nil && c.PrincipalID != app.AppliedBy && c.PrincipalID != app.ApprovedBy
	unapplyWhy := "the receipt has no active application"
	if app != nil && !unapplyOK {
		unapplyWhy = "an application is unapplied by a principal other than the one who applied or approved it"
	}
	reapplyOK := usable && prior != nil && c.PrincipalID != prior.AppliedBy && c.PrincipalID != prior.ApprovedBy
	reapplyWhy := "reapply needs a receipt whose application was unapplied"
	if prior != nil && usable && !reapplyOK {
		reapplyWhy = "a reapplication is made by a principal other than the one who made the reversed application"
	}
	cands := []struct {
		name  string
		perms []string
		legal bool
		why   string
	}{
		{"GenerateMatchCandidates", []string{ActionApplicationManage}, usable && m.finder != nil, applyWhy},
		{"ApplyReceipt", []string{ActionApplicationManage}, usable, applyWhy},
		{"ApproveManualMatch", []string{ActionManualMatchApprove}, approveOK, approveWhy},
		{"UnapplyReceipt", []string{ActionUnapply}, unapplyOK, unapplyWhy},
		{"ReapplyReceipt", []string{ActionReapply}, reapplyOK, reapplyWhy},
		{"ReconcileApplicationBatch", []string{ActionApplicationManage}, batchID != "", "the receipt is not part of an application batch"},
	}
	allowed := map[string]bool{}
	for _, cd := range cands {
		for _, perm := range cd.perms {
			if _, done := allowed[perm]; done {
				continue
			}
			e := m.deps.Authz.CheckAllowed(ctx, c.PrincipalID, rc.LegalEntityID, perm)
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
		permitted := true
		for _, perm := range cd.perms {
			permitted = permitted && allowed[perm]
		}
		a := actionState{Action: cd.name, Permission: cd.perms, Available: cd.legal && permitted}
		switch {
		case !cd.legal:
			a.Reason = why(false, cd.why)
		case !permitted:
			a.Reason = "caller lacks " + cd.perms[0]
		}
		out = append(out, a)
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"receipt_id": rc.ReceiptID, "state": rc.State, "version": rc.Version, "batch_id": batchID, "actions": out})
}
