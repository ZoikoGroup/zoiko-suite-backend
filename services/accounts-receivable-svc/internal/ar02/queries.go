package ar02

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

// resolveHeader finds the quote by quote_id, or by legal_entity_id + quote_number.
func resolveHeader(r *http.Request, tx pgx.Tx, tenantID string, p map[string]string) (*Header, error) {
	if id := p["quote_id"]; id != "" {
		if err := mustUUID("quote_id", id); err != nil {
			return nil, err
		}
		return getHeader(r.Context(), tx, tenantID, id, false)
	}
	if p["legal_entity_id"] != "" && p["quote_number"] != "" {
		if err := mustUUID("legal_entity_id", p["legal_entity_id"]); err != nil {
			return nil, err
		}
		return getHeaderByNumber(r.Context(), tx, tenantID, p["legal_entity_id"], p["quote_number"])
	}
	return nil, kit.Invalid("quote_id, or legal_entity_id with quote_number, is required")
}

func (m *Module) getQuote(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var out *Quote
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		h, err := resolveHeader(r, tx, c.TenantID, p)
		if err != nil {
			return err
		}
		out, err = loadQuote(r.Context(), tx, c.TenantID, h, 0)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) getQuoteVersion(w http.ResponseWriter, r *http.Request) {
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
	var out *Quote
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		h, err := resolveHeader(r, tx, c.TenantID, p)
		if err != nil {
			return err
		}
		out, err = loadQuote(r.Context(), tx, c.TenantID, h, vno)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, out)
}

// ListItem is a quote header with the headline figures of its current version.
type ListItem struct {
	Header
	ValidityDate    string     `json:"validity_date"`
	NetTotal        kit.Amount `json:"net_total"`
	DiscountPercent kit.Amount `json:"discount_percent"`
}

func (m *Module) listQuotes(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	limit, offset := 50, 0
	if s := p["limit"]; s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			m.fail(w, kit.Invalid("limit must be a positive integer"))
			return
		}
		limit = min(n, maxListLimit)
	}
	if s := p["offset"]; s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			m.fail(w, kit.Invalid("offset must be a non-negative integer"))
			return
		}
		offset = n
	}
	for _, f := range []string{"legal_entity_id", "customer_party_id"} {
		if p[f] != "" {
			if err := mustUUID(f, p[f]); err != nil {
				m.fail(w, err)
				return
			}
		}
	}
	if p["state"] != "" {
		if _, known := map[State]bool{StateDraft: true, StatePendingApproval: true, StateApproved: true, StateSent: true, StateAccepted: true,
			StateRejected: true, StateExpired: true, StateConverted: true}[State(p["state"])]; !known {
			m.fail(w, kit.Invalid("state is not a quote state"))
			return
		}
	}
	items := []ListItem{}
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `SELECT `+headerCols+` FROM ar02_quotes
			WHERE tenant_id = $1 AND ($2 = '' OR legal_entity_id::text = $2) AND ($3 = '' OR customer_party_id::text = $3) AND ($4 = '' OR state = $4)
			ORDER BY created_at DESC, quote_id LIMIT $5 OFFSET $6`, c.TenantID, p["legal_entity_id"], p["customer_party_id"], p["state"], limit, offset)
		if err != nil {
			return kit.MapPgErr(err)
		}
		var ids []string
		for rows.Next() {
			h, err := scanHeader(rows)
			if err != nil {
				rows.Close()
				return err
			}
			items = append(items, ListItem{Header: *h})
			ids = append(ids, h.QuoteID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		vr, err := tx.Query(r.Context(), `SELECT v.quote_id::text, v.validity_date::text, v.net_total::text, v.discount_percent::text
			FROM ar02_quote_versions v JOIN ar02_quotes q ON q.quote_id = v.quote_id AND q.current_version_no = v.version_no
			WHERE v.tenant_id = $1 AND q.tenant_id = $1 AND v.quote_id = ANY($2::uuid[])`, c.TenantID, ids)
		if err != nil {
			return err
		}
		defer vr.Close()
		byID := map[string]*ListItem{}
		for i := range items {
			byID[items[i].QuoteID] = &items[i]
		}
		for vr.Next() {
			var id, validity string
			var net, disc kit.Amount
			if err := vr.Scan(&id, &validity, &net, &disc); err != nil {
				return err
			}
			if it := byID[id]; it != nil {
				it.ValidityDate, it.NetTotal, it.DiscountPercent = validity, net, disc
			}
		}
		return vr.Err()
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	resp := map[string]any{"items": items, "limit": limit, "offset": offset}
	if len(items) == limit {
		resp["next_offset"] = offset + limit
	}
	kit.WriteJSON(w, http.StatusOK, resp)
}

type actionState struct {
	Action     string   `json:"action"`
	Permission []string `json:"permissions"`
	Available  bool     `json:"available"`
	Reason     string   `json:"reason,omitempty"`
}

// getAvailableActions is derived from state, expiry, approval integrity,
// segregation of duties and the caller's permissions, never hard-coded.
func (m *Module) getAvailableActions(w http.ResponseWriter, r *http.Request) {
	c, ok := m.deps.Identify(w, r)
	if !ok {
		return
	}
	p := params(r)
	var h *Header
	var v *Version
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		if h, err = resolveHeader(r, tx, c.TenantID, p); err != nil {
			return err
		}
		v, err = getVersion(r.Context(), tx, c.TenantID, h.QuoteID, h.CurrentVersionNo)
		return err
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	expired := Expired(v.ValidityDate, m.today())
	canT := func(t Transition) bool { _, e := Next(h.State, t); return e == nil }
	needsAuth := NeedsDiscountAuthority(v.DiscountPercent)
	preparer := c.PrincipalID == v.CreatedBy || c.PrincipalID == h.CreatedBy || c.PrincipalID == h.SubmittedBy
	intact := approvalIntact(h, v) == nil
	type cand struct {
		name  string
		perms []string
		legal bool
		why   string
	}
	approveWhy := "quote is not pending approval"
	approveLegal := canT(TrApprove)
	switch {
	case approveLegal && needsAuth && preparer:
		approveLegal, approveWhy = false, "a discount beyond the preparer's authority needs a different approver"
	case approveLegal && expired:
		approveLegal, approveWhy = false, "the quote validity has ended"
	}
	approvePerms := []string{ActionQuoteApprove}
	if needsAuth {
		approvePerms = append(approvePerms, ActionQuoteDiscountApprove)
	}
	cands := []cand{
		{"AmendQuote", []string{ActionQuoteManage}, h.State.CanAmend(), "the quote is final in state " + string(h.State)},
		{"SubmitQuoteForApproval", []string{ActionQuoteManage}, canT(TrSubmit) && !expired, ternary(expired && canT(TrSubmit), "the quote validity has ended", "quote is not a draft")},
		{"ApproveQuote", approvePerms, approveLegal, approveWhy},
		{"SendQuote", []string{ActionQuoteManage}, canT(TrSend) && !expired && intact, ternary(canT(TrSend), "the quote validity has ended or the approval no longer matches the content", "quote is not approved")},
		{"RecordAcceptance", []string{ActionQuoteManage}, canT(TrAccept) && !expired, ternary(canT(TrAccept), "the quote validity has ended", "quote has not been sent")},
		{"RejectQuote", []string{ActionQuoteManage}, canT(TrReject), "quote cannot be rejected in state " + string(h.State)},
		{"ExpireQuote", []string{ActionQuoteManage}, canT(TrExpire) && expired, ternary(canT(TrExpire), "the quote validity has not elapsed", "quote cannot expire in state "+string(h.State))},
		{"ConvertQuoteToOrder", []string{ActionQuoteConvert}, canT(TrConvert) && !expired && intact && m.orders != nil,
			ternary(canT(TrConvert), ternary(m.orders == nil, "the order service is not wired", "the quote validity has ended"), "quote is not accepted")},
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
		permitted := true
		missing := ""
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
	kit.WriteJSON(w, http.StatusOK, map[string]any{"quote_id": h.QuoteID, "state": h.State, "version": h.Version, "expired": expired, "actions": out})
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// VersionSummary is one row of the version list (full content via GetQuoteVersion).
type VersionSummary struct {
	VersionNo      int        `json:"version_no"`
	ContentHash    string     `json:"content_hash"`
	ChangeKind     string     `json:"change_kind"`
	MaterialChange bool       `json:"material_change"`
	ValidityDate   string     `json:"validity_date"`
	NetTotal       kit.Amount `json:"net_total"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
}

// ApprovalRecord is one approval bound to a version content hash.
type ApprovalRecord struct {
	VersionNo                 int       `json:"version_no"`
	ContentHash               string    `json:"content_hash"`
	DiscountPercent           string    `json:"discount_percent"`
	RequiresDiscountAuthority bool      `json:"requires_discount_authority"`
	PreparedBy                string    `json:"prepared_by"`
	ApprovedBy                string    `json:"approved_by"`
	ApprovedAt                time.Time `json:"approved_at"`
}

func (m *Module) getQuoteHistory(w http.ResponseWriter, r *http.Request) {
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
	var h *Header
	err := m.read(r.Context(), c.TenantID, func(tx pgx.Tx) (err error) {
		if h, err = resolveHeader(r, tx, c.TenantID, p); err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `SELECT quote_version, version_no, command, state_after, actor_id, COALESCE(approver_id,''),
				COALESCE(reason_code,''), COALESCE(evidence_id,''), COALESCE(content_hash,''), prior_values, new_values, correlation_id, recorded_at
			FROM ar02_quote_history WHERE tenant_id = $1 AND quote_id = $2 AND quote_version > $3 ORDER BY quote_version LIMIT $4`,
			c.TenantID, h.QuoteID, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e HistoryEntry
			var st string
			var prior, nw []byte
			if err := rows.Scan(&e.QuoteVersion, &e.VersionNo, &e.Command, &st, &e.ActorID, &e.ApproverID, &e.ReasonCode, &e.EvidenceID,
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
		vr, err := tx.Query(r.Context(), `SELECT version_no, content_hash, change_kind, material_change, validity_date::text, net_total::text, created_by, created_at
			FROM ar02_quote_versions WHERE tenant_id = $1 AND quote_id = $2 ORDER BY version_no`, c.TenantID, h.QuoteID)
		if err != nil {
			return err
		}
		defer vr.Close()
		for vr.Next() {
			var s VersionSummary
			if err := vr.Scan(&s.VersionNo, &s.ContentHash, &s.ChangeKind, &s.MaterialChange, &s.ValidityDate, &s.NetTotal, &s.CreatedBy, &s.CreatedAt); err != nil {
				return err
			}
			versions = append(versions, s)
		}
		if err := vr.Err(); err != nil {
			return err
		}
		vr.Close()
		ar, err := tx.Query(r.Context(), `SELECT version_no, content_hash, discount_percent::text, requires_discount_authority, prepared_by, approved_by, approved_at
			FROM ar02_quote_approvals WHERE tenant_id = $1 AND quote_id = $2 ORDER BY approved_at, version_no`, c.TenantID, h.QuoteID)
		if err != nil {
			return err
		}
		defer ar.Close()
		for ar.Next() {
			var a ApprovalRecord
			if err := ar.Scan(&a.VersionNo, &a.ContentHash, &a.DiscountPercent, &a.RequiresDiscountAuthority, &a.PreparedBy, &a.ApprovedBy, &a.ApprovedAt); err != nil {
				return err
			}
			approvals = append(approvals, a)
		}
		return ar.Err()
	})
	if err != nil {
		m.fail(w, err)
		return
	}
	resp := map[string]any{"quote_id": h.QuoteID, "items": items}
	if len(items) > limit {
		resp["items"] = items[:limit]
		resp["next_after_version"] = items[limit-1].QuoteVersion
	}
	if items == nil {
		resp["items"] = []HistoryEntry{}
	}
	if after == 0 {
		resp["versions"], resp["approvals"] = versions, approvals
	}
	kit.WriteJSON(w, http.StatusOK, resp)
}
