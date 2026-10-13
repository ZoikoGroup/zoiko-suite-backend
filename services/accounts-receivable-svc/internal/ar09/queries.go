package ar09

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/google/uuid"
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

func limitParam(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	if n > 200 {
		return 200
	}
	return n
}

// liveItem is an open item as AR-07 shows it now.
type liveItem struct {
	OpenItemID   string     `json:"open_item_id"`
	DocumentKind string     `json:"document_kind"`
	DueDate      string     `json:"due_date"`
	DaysPastDue  int        `json:"days_past_due"`
	Residual     kit.Amount `json:"residual"`
	State        string     `json:"item_state"`
	Disputed     bool       `json:"disputed"`
	Held         bool       `json:"collection_hold"`
}

func (m *Module) getCollectionCase(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	var out map[string]any
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		cs, err := getCase(r.Context(), tx, c.TenantID, params(r)["case_id"], false, false)
		if err != nil {
			return err
		}
		now := m.deps.Now()
		lv, err := m.live(r.Context(), tx, cs, now)
		if err != nil {
			return err
		}
		items := make([]liveItem, 0, len(lv.views))
		for _, v := range lv.views {
			items = append(items, liveItem{OpenItemID: v.OpenItemID, DocumentKind: v.DocumentKind, DueDate: v.DueDate.Format("2006-01-02"),
				DaysPastDue: DaysPastDue(v.DueDate, now), Residual: v.Residual, State: v.State, Disputed: v.Disputed, Held: v.CollectionHold})
		}
		promise, err := latestPromise(r.Context(), tx, c.TenantID, cs.CaseID)
		if err != nil {
			return err
		}
		hold, err := activeHold(r.Context(), tx, c.TenantID, cs.CaseID)
		if err != nil {
			return err
		}
		actions, err := listActions(r.Context(), tx, `tenant_id = $1 AND case_id = $2`, 50, c.TenantID, cs.CaseID)
		if err != nil {
			return err
		}
		hist, err := listHistory(r.Context(), tx, c.TenantID, cs.CaseID, 200)
		if err != nil {
			return err
		}
		out = map[string]any{"case": cs, "items": items, "aging_now": lv.aging, "promise": promise, "active_hold": hold, "actions": actions, "history": hist}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

type scheduleStage struct {
	Stage
	TriggerDay int     `json:"trigger_day"`
	Due        bool    `json:"due"`
	Reached    bool    `json:"reached"`
	Notice     *Action `json:"latest_action,omitempty"`
}

func (m *Module) getDunningSchedule(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	var out map[string]any
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		cs, err := getCase(r.Context(), tx, c.TenantID, params(r)["case_id"], false, false)
		if err != nil {
			return err
		}
		pol, err := policyByID(r.Context(), tx, c.TenantID, cs.PolicyID)
		if err != nil {
			return err
		}
		now := m.deps.Now()
		lv, err := m.live(r.Context(), tx, cs, now)
		if err != nil {
			return err
		}
		dpd := MaxDaysPastDue(lv.aging)
		stages := make([]scheduleStage, 0, len(pol.Stages))
		for _, st := range pol.Stages {
			ss := scheduleStage{Stage: st, TriggerDay: st.TriggerDay(), Due: dpd >= st.TriggerDay(), Reached: st.StageNo <= cs.DunningStage}
			if as, err := listActions(r.Context(), tx, `tenant_id = $1 AND case_id = $2 AND stage_no = $3`, 1, c.TenantID, cs.CaseID, st.StageNo); err != nil {
				return err
			} else if len(as) > 0 {
				ss.Notice = &as[0]
			}
			stages = append(stages, ss)
		}
		out = map[string]any{"case_id": cs.CaseID, "state": cs.State, "current_stage": cs.DunningStage, "days_past_due": dpd, "policy": pol,
			"stages": stages, "blackout_today": pol.BlackoutReason(now) != ""}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) getCustomerCollectionHistory(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	if _, err := parseUUID("customer_party_id", p["customer_party_id"]); err != nil {
		m.fail(w, err)
		return
	}
	limit := limitParam(p["limit"], 50)
	var out map[string]any
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT `+caseCols+` FROM ar09_collection_cases WHERE tenant_id = $1 AND customer_party_id = $2
			AND ($3 = '' OR legal_entity_id::text = $3) ORDER BY created_at DESC, case_id LIMIT $4`, c.TenantID, p["customer_party_id"], p["legal_entity_id"], limit)
		if err != nil {
			return err
		}
		cases := []*Case{}
		for rows.Next() {
			cs, err := scanCase(rows)
			if err != nil {
				rows.Close()
				return err
			}
			cases = append(cases, cs)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		actions, err := listActions(r.Context(), tx, `tenant_id = $1 AND customer_party_id = $2 AND ($3 = '' OR legal_entity_id::text = $3)`, limit,
			c.TenantID, p["customer_party_id"], p["legal_entity_id"])
		if err != nil {
			return err
		}
		promises := []Promise{}
		for _, cs := range cases {
			ps, err := listPromises(r.Context(), tx, c.TenantID, cs.CaseID)
			if err != nil {
				return err
			}
			promises = append(promises, ps...)
		}
		out = map[string]any{"customer_party_id": p["customer_party_id"], "cases": cases, "actions": actions, "promises": promises}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// getAvailableActions derives the commands from the case state, the stage progress and the caller's permissions: never a
// hard-coded true.
func (m *Module) getAvailableActions(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	var cs *Case
	var sent bool
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		if cs, err = getCase(r.Context(), tx, c.TenantID, params(r)["case_id"], false, false); err != nil {
			return err
		}
		sent, err = stageSent(r.Context(), tx, c.TenantID, cs.CaseID, max(cs.DunningStage, 1))
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	c.LegalEntityID = cs.LegalEntityID
	available, denied := []string{}, []string{}
	for _, name := range AvailableForState(cs.State) {
		switch {
		case name == "SendDunningNotice" && (cs.DunningStage < 1 || sent):
			continue
		case name == "EscalateCollectionCase" && cs.DunningStage < 1:
			continue
		}
		if err := m.checkAction(r.Context(), c, ActionFor[name]); err != nil {
			denied = append(denied, name)
			continue
		}
		available = append(available, name)
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"case_id": cs.CaseID, "state": cs.State, "version": cs.Version, "dunning_stage": cs.DunningStage,
		"available": available, "denied_by_permission": denied})
}

type dueRow struct {
	Promise
	CustomerPartyID string     `json:"customer_party_id"`
	CaseState       string     `json:"case_state"`
	OutstandingNow  kit.Amount `json:"outstanding_now"` // AR-07 live, not stored
}

func (m *Module) getPromisesDue(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	asOf := m.deps.Now()
	if p["as_of"] != "" {
		t, err := parseDate("as_of", p["as_of"])
		if err != nil {
			m.fail(w, err)
			return
		}
		asOf = t
	}
	limit := limitParam(p["limit"], 50)
	rows := []dueRow{}
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		rs, err := tx.Query(r.Context(), `SELECT p.promise_id::text, p.case_id::text, p.version_no, to_char(p.promised_date,'YYYY-MM-DD'), p.promised_amount, p.currency,
				COALESCE(p.supersedes::text,''), COALESCE(p.reason_code,''), p.recorded_by, p.recorded_at, c.customer_party_id::text, c.state
			FROM ar09_promises p JOIN ar09_collection_cases c ON c.tenant_id = p.tenant_id AND c.case_id = p.case_id
			WHERE p.tenant_id = $1 AND ($2 = '' OR p.legal_entity_id::text = $2) AND p.promised_date <= $3::date AND c.state NOT IN ('RESOLVED','CLOSED')
			  AND p.version_no = (SELECT max(x.version_no) FROM ar09_promises x WHERE x.tenant_id = p.tenant_id AND x.case_id = p.case_id)
			ORDER BY p.promised_date, p.promise_id LIMIT $4`, c.TenantID, p["legal_entity_id"], asOf.UTC().Format("2006-01-02"), limit)
		if err != nil {
			return err
		}
		for rs.Next() {
			var d dueRow
			if err := rs.Scan(&d.PromiseID, &d.CaseID, &d.VersionNo, &d.PromisedDate, &d.PromisedAmount, &d.Currency, &d.Supersedes, &d.ReasonCode,
				&d.RecordedBy, &d.RecordedAt, &d.CustomerPartyID, &d.CaseState); err != nil {
				rs.Close()
				return err
			}
			rows = append(rows, d)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return err
		}
		for i := range rows {
			lv, err := m.live(r.Context(), tx, &Case{TenantID: c.TenantID, CaseID: rows[i].CaseID, Currency: rows[i].Currency}, m.deps.Now())
			if err != nil {
				return err
			}
			rows[i].OutstandingNow = lv.aging.Total
		}
		return nil
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"as_of": asOf.UTC().Format("2006-01-02"), "promises": rows})
}

func parseUUID(field, s string) (string, error) {
	if _, err := uuid.Parse(s); err != nil {
		return "", kit.Invalid(field + " must be a UUID")
	}
	return s, nil
}
