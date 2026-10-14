package ar01

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

// params merges the query string with, for POST, the top-level scalar fields of
// a JSON body (the spec wireframe allows GET or POST for queries).
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

// resolveHeader finds the profile by profile_id, or by legal_entity_id + customer_party_id.
func resolveHeader(r *http.Request, tx pgx.Tx, tenantID string, p map[string]string) (*Header, error) {
	if id := p["profile_id"]; id != "" {
		if err := mustUUID("profile_id", id); err != nil {
			return nil, err
		}
		return getHeader(r.Context(), tx, tenantID, id, false)
	}
	if p["legal_entity_id"] != "" && p["customer_party_id"] != "" {
		if err := mustUUID("legal_entity_id", p["legal_entity_id"]); err != nil {
			return nil, err
		}
		if err := mustUUID("customer_party_id", p["customer_party_id"]); err != nil {
			return nil, err
		}
		return getHeaderByParty(r.Context(), tx, tenantID, p["legal_entity_id"], p["customer_party_id"])
	}
	return nil, kit.Invalid("profile_id, or legal_entity_id with customer_party_id, is required")
}

func (m *Module) getFinancialProfile(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var out *Profile
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		h, err := resolveHeader(r, tx, c.TenantID, p)
		if err != nil {
			return err
		}
		out, err = loadProfile(r.Context(), tx, c.TenantID, h, m.today(), nil)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// parseAsOf accepts YYYY-MM-DD (end of that UTC day) or RFC3339.
func parseAsOf(s string) (date string, ts time.Time, err error) {
	if s == "" {
		return "", time.Time{}, kit.Invalid("as_of is required (YYYY-MM-DD or RFC3339)")
	}
	if t, e := time.Parse(dateLayout, s); e == nil {
		return s, t.Add(24*time.Hour - time.Nanosecond), nil
	}
	t, e := time.Parse(time.RFC3339, s)
	if e != nil {
		return "", time.Time{}, kit.Invalid("as_of must be YYYY-MM-DD or RFC3339")
	}
	return day(t), t.UTC(), nil
}

func (m *Module) getFinancialProfileAsOf(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	date, ts, err := parseAsOf(p["as_of"])
	if err != nil {
		m.fail(w, err)
		return
	}
	var out *Profile
	err = m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		h, err := resolveHeader(r, tx, c.TenantID, p)
		if err != nil {
			return err
		}
		out, err = loadProfile(r.Context(), tx, c.TenantID, h, date, &ts)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

type actionState struct {
	Action     string `json:"action"`
	Permission string `json:"permission"`
	Available  bool   `json:"available"`
	Reason     string `json:"reason,omitempty"`
}

// getAvailableActions is derived from state, the pending-request fact and the
// caller's permissions (authorization-svc), never hard-coded.
func (m *Module) getAvailableActions(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var h *Header
	var pending *LimitRequest
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		if h, err = resolveHeader(r, tx, c.TenantID, p); err != nil {
			return err
		}
		pending, err = pendingRequest(r.Context(), tx, c.TenantID, h.ProfileID, false)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	type cand struct {
		name, perm string
		legal      bool
		why        string
	}
	live := h.State != StateRetired
	canT := func(t Transition) bool { _, e := Next(h.State, t); return e == nil }
	cands := []cand{
		{"ActivateProfile", ActionProfileManage, canT(TrActivate), "profile is not DRAFT"},
		{"SuspendProfile", ActionProfileManage, canT(TrSuspend), "profile is not ACTIVE"},
		{"ReinstateProfile", ActionProfileManage, canT(TrReinstate), "profile is not SUSPENDED"},
		{"AmendFinancialProfile", ActionProfileManage, live, "profile is retired"},
		{"ChangePaymentTerms", ActionProfileManage, live, "profile is retired"},
		{"SetCreditLimit", ActionLimitManage, live && pending == nil, ternary(live, "a credit limit change is awaiting approval", "profile is retired")},
		{"ApproveCreditLimitChange", ActionLimitApprove, live && pending != nil && pending.RequestedBy != c.PrincipalID,
			ternary(pending == nil, "no credit limit change is pending", "the requester cannot approve their own change")},
		{"PlaceCreditHold", ActionHoldManage, canT(TrHold), "profile is not ACTIVE"},
		{"ReleaseCreditHold", ActionHoldManage, canT(TrRelease), "no credit hold is active"},
		{"RetireFinancialProfile", ActionProfileManage, canT(TrRetire), "profile is already retired"},
	}
	allowed := map[string]bool{}
	for _, cd := range cands {
		if _, done := allowed[cd.perm]; done {
			continue
		}
		e := m.deps.Authz.CheckAllowed(r.Context(), c.PrincipalID, h.LegalEntityID, cd.perm)
		switch {
		case e == nil:
			allowed[cd.perm] = true
		case errors.Is(e, domain.ErrAuthorizationDenied):
			allowed[cd.perm] = false
		default:
			m.fail(w, kit.Unavailable("authorization service unavailable"))
			return
		}
	}
	out := make([]actionState, 0, len(cands))
	for _, cd := range cands {
		a := actionState{Action: cd.name, Permission: cd.perm, Available: cd.legal && allowed[cd.perm]}
		switch {
		case !cd.legal:
			a.Reason = cd.why
		case !allowed[cd.perm]:
			a.Reason = "caller lacks " + cd.perm
		}
		out = append(out, a)
	}
	kit.WriteJSON(w, http.StatusOK, map[string]any{"profile_id": h.ProfileID, "state": h.State, "version": h.Version, "actions": out})
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

type creditStatus struct {
	ProfileID         string        `json:"profile_id"`
	CustomerPartyID   string        `json:"customer_party_id"`
	LegalEntityID     string        `json:"legal_entity_id"`
	State             State         `json:"state"`
	Version           int           `json:"version"`
	CreditLimit       kit.Amount    `json:"credit_limit"`
	CreditCurrency    string        `json:"credit_currency,omitempty"`
	LimitConfigured   bool          `json:"limit_configured"`
	LimitEffective    bool          `json:"limit_effective"`
	PolicyMax         kit.Amount    `json:"credit_limit_policy_max"`
	CurrentExposure   *kit.Amount   `json:"current_exposure,omitempty"`
	ExposureAvailable bool          `json:"exposure_available"`
	AvailableCredit   *kit.Amount   `json:"available_credit,omitempty"`
	OverLimit         bool          `json:"over_limit"`
	CreditHold        bool          `json:"credit_hold"`
	HoldReasonCode    string        `json:"hold_reason_code,omitempty"`
	HoldPlacedBy      string        `json:"hold_placed_by,omitempty"`
	HoldPlacedAt      *time.Time    `json:"hold_placed_at,omitempty"`
	PendingRequest    *LimitRequest `json:"pending_credit_limit_request,omitempty"`
}

func (m *Module) getCreditStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var h *Header
	var a *AttrVersion
	var pending *LimitRequest
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		if h, err = resolveHeader(r, tx, c.TenantID, p); err != nil {
			return err
		}
		if a, err = attrAt(r.Context(), tx, c.TenantID, h.ProfileID, m.today()); err != nil {
			return err
		}
		pending, err = pendingRequest(r.Context(), tx, c.TenantID, h.ProfileID, false)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	out := creditStatus{ProfileID: h.ProfileID, CustomerPartyID: h.CustomerPartyID, LegalEntityID: h.LegalEntityID, State: h.State, Version: h.Version,
		PolicyMax: h.PolicyMax, CreditHold: h.CreditHold, HoldReasonCode: h.HoldReasonCode, HoldPlacedBy: h.HoldPlacedBy,
		HoldPlacedAt: h.HoldPlacedAt, PendingRequest: pending}
	if a != nil {
		out.LimitEffective = true
		out.CreditLimit, out.CreditCurrency = a.CreditLimit, a.CreditCurrency
		out.LimitConfigured = a.CreditLimit.IsPositive()
		if m.openItems != nil {
			exp, err := m.openItems.Exposure(r.Context(), c.TenantID, h.LegalEntityID, h.CustomerPartyID, a.CreditCurrency)
			if err != nil {
				// Cannot answer is not "no exposure": fail closed.
				m.fail(w, kit.Unavailable("receivable exposure unavailable"))
				return
			}
			out.ExposureAvailable = true
			out.CurrentExposure = &exp
			if out.LimitConfigured {
				avail := a.CreditLimit.Sub(exp)
				out.AvailableCredit = &avail
				out.OverLimit = avail.IsNegative()
			}
		}
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) listProfileHistory(w http.ResponseWriter, r *http.Request) {
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
		limit = min(n, maxHistoryPageLimit)
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
	var attrs []AttrVersion
	var terms []TermVersion
	var h *Header
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		if h, err = resolveHeader(r, tx, c.TenantID, p); err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `SELECT profile_version, command, state_after, credit_hold_after, actor_id, COALESCE(approver_id,''),
				COALESCE(reason_code,''), COALESCE(evidence_id,''), COALESCE(effective_from::text,''), prior_values, new_values,
				COALESCE(policy_ref,''), correlation_id, recorded_at
			FROM ar01_profile_history WHERE tenant_id = $1 AND profile_id = $2 AND profile_version > $3
			ORDER BY profile_version LIMIT $4`, c.TenantID, h.ProfileID, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e HistoryEntry
			var st string
			var prior, nw []byte
			if err := rows.Scan(&e.ProfileVersion, &e.Command, &st, &e.CreditHoldAfter, &e.ActorID, &e.ApproverID, &e.ReasonCode, &e.EvidenceID,
				&e.EffectiveFrom, &prior, &nw, &e.PolicyRef, &e.CorrelationID, &e.RecordedAt); err != nil {
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
		if after == 0 {
			if attrs, err = allAttrs(r.Context(), tx, c.TenantID, h.ProfileID); err != nil {
				return err
			}
			terms, err = allTerms(r.Context(), tx, c.TenantID, h.ProfileID)
		}
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	resp := map[string]any{"profile_id": h.ProfileID, "items": items}
	if len(items) > limit {
		resp["items"] = items[:limit]
		resp["next_after_version"] = items[limit-1].ProfileVersion
	}
	if after == 0 {
		resp["attribute_versions"], resp["payment_term_versions"] = attrs, terms
	}
	if items == nil {
		resp["items"] = []HistoryEntry{}
	}
	kit.WriteJSON(w, http.StatusOK, resp)
}
