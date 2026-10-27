package ar07

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// params merges the query string with, for POST, the top-level scalar fields of a JSON body.
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

// ledgerBalanceSQL is the exact balance of open item alias o as of an exclusive upper bound ($N): the original amount plus
// every signed ledger effect recorded before it. The balance is reconstructed from the ledger, never read from a stored total.
func ledgerBalanceSQL(boundParam string) string {
	return `(o.original_amount + COALESCE((SELECT sum(a.signed_effect) FROM ar07_adjustments a
		WHERE a.tenant_id = o.tenant_id AND a.open_item_id = o.open_item_id AND a.recorded_at < ` + boundParam + `), 0))`
}

// endOfDayUTC is the exclusive bound of a calendar day: midnight UTC of the following day.
func endOfDayUTC(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, time.UTC)
}

func (m *Module) getOpenItem(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	if p["open_item_id"] == "" && p["source_document_id"] == "" {
		m.fail(w, kit.Invalid("open_item_id or source_document_id is required"))
		return
	}
	var out struct {
		OpenItem    *OpenItem    `json:"open_item"`
		Adjustments []Adjustment `json:"adjustments"`
		Disputes    []Dispute    `json:"disputes"`
	}
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		if id := p["open_item_id"]; id != "" {
			out.OpenItem, err = getItem(r.Context(), tx, c.TenantID, id, false)
		} else if sd := p["source_document_id"]; sd != "" {
			out.OpenItem, err = getItemBySource(r.Context(), tx, c.TenantID, sd)
		} else {
			return kit.Invalid("open_item_id or source_document_id is required")
		}
		if err != nil {
			return err
		}
		if out.Adjustments, err = loadAdjustments(r.Context(), tx, c.TenantID, out.OpenItem.OpenItemID); err != nil {
			return err
		}
		out.Disputes, err = loadDisputes(r.Context(), tx, c.TenantID, out.OpenItem.OpenItemID)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) getOpenItemHistory(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var out struct {
		OpenItem    *OpenItem      `json:"open_item"`
		History     []HistoryEvent `json:"history"`
		Adjustments []Adjustment   `json:"adjustments"`
		Disputes    []Dispute      `json:"disputes"`
	}
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		if out.OpenItem, err = getItem(r.Context(), tx, c.TenantID, p["open_item_id"], false); err != nil {
			return err
		}
		id := out.OpenItem.OpenItemID
		if out.History, err = loadHistory(r.Context(), tx, c.TenantID, id); err != nil {
			return err
		}
		if out.Adjustments, err = loadAdjustments(r.Context(), tx, c.TenantID, id); err != nil {
			return err
		}
		out.Disputes, err = loadDisputes(r.Context(), tx, c.TenantID, id)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

type customerSummary struct {
	Currency         string     `json:"currency"`
	OpenItemCount    int        `json:"open_item_count"`
	OriginalAmount   kit.Amount `json:"original_amount"`
	Residual         kit.Amount `json:"residual_amount"`
	DisputedResidual kit.Amount `json:"disputed_residual"`
	HeldResidual     kit.Amount `json:"held_residual"`
}

// getCustomerReceivables is the derived balance projection of one customer: computed from the open items at read time and
// never stored, so it cannot drift from them.
func (m *Module) getCustomerReceivables(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	if err := firstErr(mustUUID("legal_entity_id", p["legal_entity_id"]), mustUUID("customer_party_id", p["customer_party_id"])); err != nil {
		m.fail(w, err)
		return
	}
	limit := 100
	if v, err := strconv.Atoi(p["limit"]); err == nil && v > 0 {
		limit = v
	}
	if limit > 200 {
		limit = 200
	}
	if p["after"] != "" {
		if err := mustUUID("after", p["after"]); err != nil {
			m.fail(w, err)
			return
		}
	}
	out := struct {
		LegalEntityID   string            `json:"legal_entity_id"`
		CustomerPartyID string            `json:"customer_party_id"`
		Basis           string            `json:"basis"`
		Summaries       []customerSummary `json:"summaries"`
		OpenItems       []OpenItem        `json:"open_items"`
		NextCursor      string            `json:"next_cursor,omitempty"`
	}{LegalEntityID: p["legal_entity_id"], CustomerPartyID: p["customer_party_id"],
		Basis: "derived from the open items at read time; never stored", Summaries: []customerSummary{}, OpenItems: []OpenItem{}}
	cur := strings.ToUpper(p["currency"])
	err := m.deps.DB.WithTxOpts(r.Context(), c.TenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT currency, count(*) FILTER (WHERE residual_amount > 0), COALESCE(sum(original_amount) FILTER (WHERE residual_amount > 0),0),
				COALESCE(sum(residual_amount),0), COALESCE(sum(residual_amount) FILTER (WHERE disputed),0), COALESCE(sum(residual_amount) FILTER (WHERE collection_hold),0)
			FROM ar07_open_items WHERE tenant_id = $1 AND legal_entity_id = $2 AND customer_party_id = $3 AND ($4::text = '' OR currency = $4)
			GROUP BY currency ORDER BY currency`, c.TenantID, p["legal_entity_id"], p["customer_party_id"], cur)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s customerSummary
			if err := rows.Scan(&s.Currency, &s.OpenItemCount, &s.OriginalAmount, &s.Residual, &s.DisputedResidual, &s.HeldResidual); err != nil {
				rows.Close()
				return err
			}
			out.Summaries = append(out.Summaries, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		irows, err := tx.Query(r.Context(), `SELECT `+itemCols+` FROM ar07_open_items
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND customer_party_id = $3 AND ($4::text = '' OR currency = $4) AND residual_amount > 0
			  AND ($5::text = '' OR open_item_id::text > $5)
			ORDER BY open_item_id LIMIT $6`, c.TenantID, p["legal_entity_id"], p["customer_party_id"], cur, p["after"], limit+1)
		if err != nil {
			return err
		}
		defer irows.Close()
		for irows.Next() {
			o, err := scanItem(irows)
			if err != nil {
				return err
			}
			out.OpenItems = append(out.OpenItems, *o)
		}
		return irows.Err()
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	if len(out.OpenItems) > limit {
		out.OpenItems = out.OpenItems[:limit]
		out.NextCursor = out.OpenItems[limit-1].OpenItemID
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

type agingBucket struct {
	Bucket string     `json:"bucket"`
	Count  int        `json:"count"`
	Amount kit.Amount `json:"amount"`
}

type agingClass struct {
	Total   kit.Amount    `json:"total"`
	Count   int           `json:"count"`
	Buckets []agingBucket `json:"buckets"`
}

type agingCurrency struct {
	Currency string                 `json:"currency"`
	Total    kit.Amount             `json:"total"`
	Count    int                    `json:"count"`
	Classes  map[string]*agingClass `json:"classes"` // normal | disputed | held (disputed takes precedence over held)
}

// getAging buckets residuals by days past due as of an EXPLICIT date. The response states the date and basis it used. The
// residual is reconstructed from the ledger as of the end of that day, so aging is reproducible for any past date.
func (m *Module) getAging(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	if err := mustUUID("legal_entity_id", p["legal_entity_id"]); err != nil {
		m.fail(w, err)
		return
	}
	if p["customer_party_id"] != "" {
		if err := mustUUID("customer_party_id", p["customer_party_id"]); err != nil {
			m.fail(w, err)
			return
		}
	}
	if p["as_of"] == "" {
		m.fail(w, kit.Invalid("as_of (YYYY-MM-DD) is required: aging is always computed as of an explicit date"))
		return
	}
	asOf, err := parseDate("as_of", p["as_of"])
	if err != nil {
		m.fail(w, err)
		return
	}
	cur, calendar := strings.ToUpper(p["currency"]), p["business_calendar_id"]
	bound := endOfDayUTC(asOf)
	byCur := map[string]*agingCurrency{}
	err = m.deps.DB.WithTxOpts(r.Context(), c.TenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT currency, business_calendar_id, class, bucket, count(*), sum(bal) FROM (
				SELECT o.currency, o.business_calendar_id,
				       CASE WHEN o.disputed THEN 'disputed' WHEN o.collection_hold THEN 'held' ELSE 'normal' END AS class,
				       CASE WHEN ($5::date - o.due_date) <= 0 THEN 'current' WHEN ($5::date - o.due_date) <= 30 THEN '1-30'
				            WHEN ($5::date - o.due_date) <= 60 THEN '31-60' WHEN ($5::date - o.due_date) <= 90 THEN '61-90' ELSE '90+' END AS bucket,
				       `+ledgerBalanceSQL("$6")+` AS bal
				FROM ar07_open_items o
				WHERE o.tenant_id = $1 AND o.legal_entity_id = $2 AND ($3::text = '' OR o.customer_party_id::text = $3) AND ($4::text = '' OR o.currency = $4)
				  AND o.issue_date <= $5::date) s
			WHERE bal > 0 GROUP BY currency, business_calendar_id, class, bucket`, c.TenantID, p["legal_entity_id"], p["customer_party_id"], cur, asOf, bound)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var currency, cal, class, bucket string
			var n int
			var sum kit.Amount
			if err := rows.Scan(&currency, &cal, &class, &bucket, &n, &sum); err != nil {
				return err
			}
			if calendar != "" && cal != calendar {
				return kit.Unprocessable(kit.CodeValidationFailed, "BUSINESS_CALENDAR_MISMATCH: open items are aged on calendar "+cal+", not "+calendar)
			}
			ac := byCur[currency]
			if ac == nil {
				ac = &agingCurrency{Currency: currency, Classes: map[string]*agingClass{}}
				byCur[currency] = ac
			}
			cl := ac.Classes[class]
			if cl == nil {
				cl = &agingClass{}
				ac.Classes[class] = cl
			}
			cl.Buckets = append(cl.Buckets, agingBucket{Bucket: bucket, Count: n, Amount: sum})
			cl.Total, cl.Count = cl.Total.Add(sum), cl.Count+n
			ac.Total, ac.Count = ac.Total.Add(sum), ac.Count+n
		}
		return rows.Err()
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	currencies := make([]*agingCurrency, 0, len(byCur))
	for _, ac := range byCur {
		for _, cl := range ac.Classes {
			sort.Slice(cl.Buckets, func(i, j int) bool { return bucketRank(cl.Buckets[i].Bucket) < bucketRank(cl.Buckets[j].Bucket) })
		}
		currencies = append(currencies, ac)
	}
	sort.Slice(currencies, func(i, j int) bool { return currencies[i].Currency < currencies[j].Currency })
	kit.WriteJSON(w, http.StatusOK, map[string]any{
		"legal_entity_id": p["legal_entity_id"], "customer_party_id": p["customer_party_id"],
		"as_of": day(asOf),
		"basis": map[string]any{
			"as_of":                  day(asOf),
			"business_calendar_id":   calendarBasis(calendar),
			"day_count":              "calendar days between as_of and the stored due_date; <= 0 is current",
			"residual_basis":         "reconstructed from the adjustment ledger through the end of as_of (UTC)",
			"flags_basis":            "disputed / held flags as currently recorded",
			"disputed_held_reported": "separately; an item that is both is reported as disputed",
			"buckets":                AgingBuckets,
		},
		"currencies": currencies,
	})
}

func calendarBasis(requested string) string {
	if requested == "" {
		return "each item's own business_calendar_id"
	}
	return requested
}

func bucketRank(b string) int {
	for i, x := range AgingBuckets {
		if x == b {
			return i
		}
	}
	return len(AgingBuckets)
}

type asOfItem struct {
	OpenItemID string     `json:"open_item_id"`
	Currency   string     `json:"currency"`
	Original   kit.Amount `json:"original_amount"`
	Balance    kit.Amount `json:"balance"`
}

// getBalanceAsOf reconstructs the exact balance from the ledger at an instant (RFC3339) or the end of a day (YYYY-MM-DD).
// The reconstruction at "now" must equal the stored residual; a difference is an explicit control exception (never plugged).
func (m *Module) getBalanceAsOf(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	if p["as_of"] == "" {
		m.fail(w, kit.Invalid("as_of (RFC3339 instant or YYYY-MM-DD) is required"))
		return
	}
	var bound time.Time // exclusive
	var existsBy time.Time
	if t, err := time.Parse(time.RFC3339Nano, p["as_of"]); err == nil {
		bound, existsBy = t.UTC().Add(time.Microsecond), t.UTC()
	} else if d, err := time.Parse("2006-01-02", p["as_of"]); err == nil {
		bound, existsBy = endOfDayUTC(d), d
	} else {
		m.fail(w, kit.Invalid("as_of must be an RFC3339 instant or YYYY-MM-DD"))
		return
	}
	var scopeSQL string
	args := []any{c.TenantID, bound, existsBy}
	switch {
	case p["open_item_id"] != "":
		if err := mustUUID("open_item_id", p["open_item_id"]); err != nil {
			m.fail(w, err)
			return
		}
		scopeSQL, args = ` AND o.open_item_id = $4`, append(args, p["open_item_id"])
	case p["customer_party_id"] != "":
		if err := firstErr(mustUUID("legal_entity_id", p["legal_entity_id"]), mustUUID("customer_party_id", p["customer_party_id"])); err != nil {
			m.fail(w, err)
			return
		}
		scopeSQL, args = ` AND o.legal_entity_id = $4 AND o.customer_party_id = $5 AND ($6::text = '' OR o.currency = $6)`,
			append(args, p["legal_entity_id"], p["customer_party_id"], strings.ToUpper(p["currency"]))
	default:
		m.fail(w, kit.Invalid("open_item_id, or legal_entity_id + customer_party_id, is required"))
		return
	}
	items := []asOfItem{}
	totals := map[string]kit.Amount{}
	err := m.deps.DB.WithTxOpts(r.Context(), c.TenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT o.open_item_id::text, o.currency, o.original_amount, `+ledgerBalanceSQL("$2")+`,
				`+ledgerBalanceSQL("'infinity'::timestamptz")+` AS current_ledger, o.residual_amount
			FROM ar07_open_items o WHERE o.tenant_id = $1 AND o.issue_date <= ($3::timestamptz AT TIME ZONE 'UTC')::date`+scopeSQL+` ORDER BY o.open_item_id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it asOfItem
			var ledgerNow, stored kit.Amount
			if err := rows.Scan(&it.OpenItemID, &it.Currency, &it.Original, &it.Balance, &ledgerNow, &stored); err != nil {
				return err
			}
			if ledgerNow != stored {
				m.deps.Log.Error("ar07 projection mismatch: stored residual differs from the ledger")
				return kit.Conflict(kit.CodeARGLReconciliationBreak, "control exception: open item "+it.OpenItemID+" residual "+stored.String()+" differs from its ledger balance "+ledgerNow.String())
			}
			items = append(items, it)
			totals[it.Currency] = totals[it.Currency].Add(it.Balance)
		}
		return rows.Err()
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	if p["open_item_id"] != "" && len(items) == 0 {
		// Either unknown / foreign (identical to absent) or the document did not yet exist at as_of.
		m.fail(w, notFound())
		return
	}
	cur := make([]string, 0, len(totals))
	for k := range totals {
		cur = append(cur, k)
	}
	sort.Strings(cur)
	perCur := []map[string]any{}
	for _, k := range cur {
		perCur = append(perCur, map[string]any{"currency": k, "balance": totals[k]})
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"as_of": p["as_of"], "basis": "ledger reconstruction: original amount plus every adjustment recorded before the as_of boundary; items exist from their issue date",
		"projection_verified": true, "items": items, "balances": perCur})
}

type actionInfo struct {
	Action    string `json:"action"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// getAvailableActions derives what the caller can do now from the item's state and the caller's permissions.
func (m *Module) getAvailableActions(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var o *OpenItem
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		o, err = getItem(r.Context(), tx, c.TenantID, p["open_item_id"], false)
		return
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	c.LegalEntityID = o.LegalEntityID
	closed, live := o.ClosedAt != nil, o.IsLive()
	state := func(ok bool, why string) (bool, string) {
		if ok {
			return true, ""
		}
		return false, why
	}
	defs := []struct {
		name, action string
		ok           bool
		why          string
	}{
		{"ApplyReceivableAdjustment", ActionReceivableAdjust, !closed && !IsTerminal(o.State) && o.Residual.IsPositive(), "the open item is closed, terminal or fully settled"},
		{"OpenDispute", ActionDisputeManage, live && !o.Disputed, "the open item is already disputed or has no live residual"},
		{"ResolveDispute", ActionDisputeManage, o.Disputed, "the open item has no open dispute"},
		{"PlaceCollectionHold", ActionHoldManage, live && !o.CollectionHold, "the open item is already held or has no live residual"},
		{"ReleaseCollectionHold", ActionHoldManage, o.CollectionHold, "the open item has no collection hold"},
		{"CloseOpenItem", ActionReceivableManage, !closed && o.Residual.IsZero() && !o.Disputed, "only a zero-residual, undisputed, unclosed item can be closed"},
	}
	out := make([]actionInfo, 0, len(defs))
	for _, d := range defs {
		okState, why := state(d.ok, d.why)
		if okState {
			if err := m.checkAction(r.Context(), c, d.action); err != nil {
				var pr *kit.Problem
				if asProblem(err, &pr) && pr.Code == kit.CodeForbidden {
					okState, why = false, "not permitted"
				} else {
					m.fail(w, err)
					return
				}
			}
		}
		out = append(out, actionInfo{Action: d.name, Available: okState, Reason: why})
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"open_item_id": o.OpenItemID, "state": o.State, "version": o.Version, "actions": out})
}
