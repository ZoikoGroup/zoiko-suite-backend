package ar01

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

const headerCols = `profile_id::text, tenant_id::text, legal_entity_id::text, customer_party_id::text, state, credit_hold,
	COALESCE(hold_reason_code,''), COALESCE(hold_placed_by,''), hold_placed_at, credit_limit_policy_max::text,
	created_by, version, created_at, updated_at`

func scanHeader(row pgx.Row) (*Header, error) {
	var h Header
	var state string
	if err := row.Scan(&h.ProfileID, &h.TenantID, &h.LegalEntityID, &h.CustomerPartyID, &state, &h.CreditHold,
		&h.HoldReasonCode, &h.HoldPlacedBy, &h.HoldPlacedAt, &h.PolicyMax, &h.CreatedBy, &h.Version, &h.CreatedAt, &h.UpdatedAt); err != nil {
		return nil, err
	}
	h.State = State(state)
	return &h, nil
}

func getHeader(ctx context.Context, tx pgx.Tx, tenantID, profileID string, forUpdate bool) (*Header, error) {
	q := `SELECT ` + headerCols + ` FROM ar01_financial_profiles WHERE tenant_id = $1 AND profile_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	h, err := scanHeader(tx.QueryRow(ctx, q, tenantID, profileID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("financial profile not found")
	}
	return h, kit.MapPgErr(err)
}

// getHeaderByParty returns the live profile of a customer in an entity, else the most recent retired one.
func getHeaderByParty(ctx context.Context, tx pgx.Tx, tenantID, entityID, partyID string) (*Header, error) {
	h, err := scanHeader(tx.QueryRow(ctx, `SELECT `+headerCols+` FROM ar01_financial_profiles
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND customer_party_id = $3
		ORDER BY (state <> 'RETIRED') DESC, created_at DESC LIMIT 1`, tenantID, entityID, partyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("financial profile not found")
	}
	return h, kit.MapPgErr(err)
}

const attrCols = `seq, effective_from::text, credit_limit::text, credit_currency, invoice_preferences, statement_preferences,
	dunning_profile, ar_account_policy, risk_attributes, change_kind, COALESCE(reason_code,''), COALESCE(evidence_id,''),
	created_by, COALESCE(approved_by,''), COALESCE(approval_request_id::text,''), recorded_at`

func scanAttr(row pgx.Row) (*AttrVersion, error) {
	var a AttrVersion
	var inv, stm, risk []byte
	if err := row.Scan(&a.Seq, &a.EffectiveFrom, &a.CreditLimit, &a.CreditCurrency, &inv, &stm, &a.DunningProfile, &a.ARAccountPolicy,
		&risk, &a.ChangeKind, &a.ReasonCode, &a.EvidenceID, &a.CreatedBy, &a.ApprovedBy, &a.ApprovalRequestID, &a.RecordedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(inv, &a.InvoicePreferences); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(stm, &a.StatementPreferences); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(risk, &a.RiskAttributes); err != nil {
		return nil, err
	}
	return &a, nil
}

func queryAttrs(ctx context.Context, tx pgx.Tx, q string, args ...any) ([]AttrVersion, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttrVersion
	for rows.Next() {
		a, err := scanAttr(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// fillAttrEnds derives each version's effective_to from the next version's start.
func fillAttrEnds(vs []AttrVersion) {
	for i := range vs {
		if i+1 < len(vs) && vs[i+1].EffectiveFrom != vs[i].EffectiveFrom {
			vs[i].EffectiveTo = vs[i+1].EffectiveFrom
		}
	}
}

// allAttrs lists every attribute version in recorded order.
func allAttrs(ctx context.Context, tx pgx.Tx, tenantID, profileID string) ([]AttrVersion, error) {
	vs, err := queryAttrs(ctx, tx, `SELECT `+attrCols+` FROM ar01_profile_versions
		WHERE tenant_id = $1 AND profile_id = $2 ORDER BY seq`, tenantID, profileID)
	fillAttrEnds(vs)
	return vs, err
}

func headAttr(ctx context.Context, tx pgx.Tx, tenantID, profileID string) (*AttrVersion, error) {
	vs, err := queryAttrs(ctx, tx, `SELECT `+attrCols+` FROM ar01_profile_versions
		WHERE tenant_id = $1 AND profile_id = $2 ORDER BY seq DESC LIMIT 1`, tenantID, profileID)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, kit.NotFound("financial profile has no attribute version")
	}
	return &vs[0], nil
}

// attrAt returns the single attribute version in force on a date: the latest
// effective_from not after it, the highest seq winning a same-day tie (a
// same-day change supersedes the earlier record from that date). nil = none yet.
func attrAt(ctx context.Context, tx pgx.Tx, tenantID, profileID, date string) (*AttrVersion, error) {
	vs, err := queryAttrs(ctx, tx, `SELECT `+attrCols+` FROM ar01_profile_versions
		WHERE tenant_id = $1 AND profile_id = $2 AND effective_from <= $3::date
		ORDER BY effective_from DESC, seq DESC LIMIT 1`, tenantID, profileID, date)
	if err != nil || len(vs) == 0 {
		return nil, err
	}
	// Derive the end of its interval for display.
	var next *string
	if err := tx.QueryRow(ctx, `SELECT min(effective_from)::text FROM ar01_profile_versions
		WHERE tenant_id = $1 AND profile_id = $2 AND effective_from > $3::date`, tenantID, profileID, vs[0].EffectiveFrom).Scan(&next); err != nil {
		return nil, err
	}
	if next != nil {
		vs[0].EffectiveTo = *next
	}
	return &vs[0], nil
}

func upcomingAttrs(ctx context.Context, tx pgx.Tx, tenantID, profileID, date string) ([]AttrVersion, error) {
	return queryAttrs(ctx, tx, `SELECT `+attrCols+` FROM ar01_profile_versions
		WHERE tenant_id = $1 AND profile_id = $2 AND effective_from > $3::date ORDER BY effective_from, seq`, tenantID, profileID, date)
}

const termCols = `seq, payment_terms_code, payment_terms_days, effective_from::text, COALESCE(effective_to::text,''),
	COALESCE(reason_code,''), COALESCE(evidence_id,''), created_by, recorded_at`

func queryTerms(ctx context.Context, tx pgx.Tx, q string, args ...any) ([]TermVersion, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TermVersion
	for rows.Next() {
		var t TermVersion
		if err := rows.Scan(&t.Seq, &t.Code, &t.Days, &t.EffectiveFrom, &t.EffectiveTo, &t.ReasonCode, &t.EvidenceID, &t.CreatedBy, &t.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// termsAt returns every payment-terms version whose interval contains the date.
// More than one is an ambiguity the caller must refuse (the schema makes it impossible).
func termsAt(ctx context.Context, tx pgx.Tx, tenantID, profileID, date string) ([]TermVersion, error) {
	return queryTerms(ctx, tx, `SELECT `+termCols+` FROM ar01_payment_term_versions
		WHERE tenant_id = $1 AND profile_id = $2 AND effective_from <= $3::date AND (effective_to IS NULL OR effective_to > $3::date)
		ORDER BY effective_from`, tenantID, profileID, date)
}

func allTerms(ctx context.Context, tx pgx.Tx, tenantID, profileID string) ([]TermVersion, error) {
	return queryTerms(ctx, tx, `SELECT `+termCols+` FROM ar01_payment_term_versions
		WHERE tenant_id = $1 AND profile_id = $2 ORDER BY effective_from`, tenantID, profileID)
}

func upcomingTerms(ctx context.Context, tx pgx.Tx, tenantID, profileID, date string) ([]TermVersion, error) {
	return queryTerms(ctx, tx, `SELECT `+termCols+` FROM ar01_payment_term_versions
		WHERE tenant_id = $1 AND profile_id = $2 AND effective_from > $3::date ORDER BY effective_from`, tenantID, profileID, date)
}

func latestTerm(ctx context.Context, tx pgx.Tx, tenantID, profileID string) (*TermVersion, error) {
	ts, err := queryTerms(ctx, tx, `SELECT `+termCols+` FROM ar01_payment_term_versions
		WHERE tenant_id = $1 AND profile_id = $2 ORDER BY effective_from DESC LIMIT 1`, tenantID, profileID)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return nil, kit.NotFound("financial profile has no payment terms version")
	}
	return &ts[0], nil
}

const reqCols = `request_id::text, requested_limit::text, currency, prior_limit::text, policy_max_at_request::text, requested_by,
	reason_code, COALESCE(evidence_id,''), status, COALESCE(decided_by,''), decided_at, COALESCE(decision_reason,''), created_at`

func scanReq(row pgx.Row) (*LimitRequest, error) {
	var r LimitRequest
	if err := row.Scan(&r.RequestID, &r.RequestedLimit, &r.Currency, &r.PriorLimit, &r.PolicyMax, &r.RequestedBy, &r.ReasonCode,
		&r.EvidenceID, &r.Status, &r.DecidedBy, &r.DecidedAt, &r.DecisionReason, &r.CreatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

func pendingRequest(ctx context.Context, tx pgx.Tx, tenantID, profileID string, forUpdate bool) (*LimitRequest, error) {
	q := `SELECT ` + reqCols + ` FROM ar01_credit_limit_requests WHERE tenant_id = $1 AND profile_id = $2 AND status = 'PENDING'`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	r, err := scanReq(tx.QueryRow(ctx, q, tenantID, profileID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ── writes ───────────────────────────────────────────────────────────────────

func jsonb(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func insertAttr(ctx context.Context, tx pgx.Tx, c kit.Caller, profileID string, seq int, effFrom, kind, reason, evidence, approvedBy, requestID string, a Attributes) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO ar01_profile_versions (version_id, tenant_id, legal_entity_id, profile_id, seq, effective_from, credit_limit, credit_currency,
			invoice_preferences, statement_preferences, dunning_profile, ar_account_policy, risk_attributes, change_kind, reason_code,
			evidence_id, created_by, approved_by, approval_request_id)
		VALUES ($1,$2,$3,$4,$5,$6::date,$7::numeric,$8,$9::jsonb,$10::jsonb,$11,$12,$13::jsonb,$14,NULLIF($15,''),NULLIF($16,''),$17,NULLIF($18,''),NULLIF($19,'')::uuid)`,
		uuid.NewString(), c.TenantID, c.LegalEntityID, profileID, seq, effFrom, a.CreditLimit.String(), a.CreditCurrency,
		jsonb(a.InvoicePreferences), jsonb(a.StatementPreferences), a.DunningProfile, a.ARAccountPolicy, jsonb(a.RiskAttributes),
		kind, reason, evidence, c.PrincipalID, approvedBy, requestID)
	return mapVersionErr(err)
}

func insertTerm(ctx context.Context, tx pgx.Tx, c kit.Caller, profileID string, seq int, code string, days int, effFrom, reason, evidence string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO ar01_payment_term_versions (term_version_id, tenant_id, legal_entity_id, profile_id, seq, payment_terms_code,
			payment_terms_days, effective_from, reason_code, evidence_id, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::date,NULLIF($9,''),NULLIF($10,''),$11)`,
		uuid.NewString(), c.TenantID, c.LegalEntityID, profileID, seq, code, days, effFrom, reason, evidence, c.PrincipalID)
	return mapVersionErr(err)
}

func closeTerm(ctx context.Context, tx pgx.Tx, tenantID, profileID string, seq int, effTo string) error {
	_, err := tx.Exec(ctx, `UPDATE ar01_payment_term_versions SET effective_to = $4::date
		WHERE tenant_id = $1 AND profile_id = $2 AND seq = $3 AND effective_to IS NULL`, tenantID, profileID, seq, effTo)
	return mapVersionErr(err)
}

// mapVersionErr turns the AR-01 trigger exceptions into stable problems. The
// triggers are the backstop; the service checks the same rules first under the
// profile row lock, so a caller normally sees the friendly path.
func mapVersionErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "P0001" {
		switch {
		case strings.HasPrefix(pgErr.Message, "AR01_PAYMENT_TERMS_OVERLAP"), strings.HasPrefix(pgErr.Message, "AR01_VERSION_OVERLAP"),
			strings.HasPrefix(pgErr.Message, "AR01_VERSION_SEQUENCE"):
			return errOverlap(pgErr.Message)
		}
	}
	return kit.MapPgErr(err)
}

type hist struct {
	At            time.Time // injected clock: recorded_at drives as-of state reconstruction
	Command       string
	Reason        string
	Evidence      string
	ApproverID    string
	EffectiveFrom string
	Prior         map[string]any
	New           map[string]any
}

// commit bumps the header (optimistic concurrency already held by the row lock),
// appends the history row and the kit audit record, and emits events — all in tx.
func commit(ctx context.Context, tx pgx.Tx, c kit.Caller, cur *Header, next State, h hist, events func(*Header) []kit.Event) (*Header, error) {
	nh := *cur
	nh.State = next
	nh.CreditHold = next == StateOnHold
	if !nh.CreditHold {
		nh.HoldReasonCode, nh.HoldPlacedBy, nh.HoldPlacedAt = "", "", nil
	}
	if next == StateOnHold && cur.State != StateOnHold {
		now := h.At.UTC()
		nh.HoldReasonCode, nh.HoldPlacedBy, nh.HoldPlacedAt = h.Reason, c.PrincipalID, &now
	}
	if err := tx.QueryRow(ctx, `UPDATE ar01_financial_profiles SET state = $3, credit_hold = $4, hold_reason_code = NULLIF($5,''),
			hold_placed_by = NULLIF($6,''), hold_placed_at = $7, version = version + 1, updated_at = $9
		WHERE tenant_id = $1 AND profile_id = $2 AND version = $8 RETURNING version, updated_at`,
		c.TenantID, cur.ProfileID, string(nh.State), nh.CreditHold, nh.HoldReasonCode, nh.HoldPlacedBy, nh.HoldPlacedAt, cur.Version, h.At.UTC()).
		Scan(&nh.Version, &nh.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, kit.ErrStaleVersion
		}
		return nil, kit.MapPgErr(err)
	}
	policyRef := PolicyRefLimitMax + "=" + cur.PolicyMax.String()
	var eff any
	if h.EffectiveFrom != "" {
		eff = h.EffectiveFrom
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ar01_profile_history (history_id, tenant_id, legal_entity_id, profile_id, profile_version, command,
			state_after, credit_hold_after, actor_id, approver_id, reason_code, evidence_id, effective_from, prior_values, new_values,
			policy_ref, correlation_id, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),$13::date,$14::jsonb,$15::jsonb,$16,$17,$18)`,
		uuid.NewString(), c.TenantID, cur.LegalEntityID, cur.ProfileID, nh.Version, h.Command, string(nh.State), nh.CreditHold,
		c.PrincipalID, h.ApproverID, h.Reason, h.Evidence, eff, jsonb(h.Prior), jsonb(h.New), policyRef, c.CorrelationID, h.At.UTC()); err != nil {
		return nil, kit.MapPgErr(err)
	}
	detail := map[string]any{"profile_version": nh.Version, "policy_ref": policyRef, "customer_party_id": cur.CustomerPartyID}
	if h.ApproverID != "" {
		detail["approver_id"] = h.ApproverID
	}
	if h.EffectiveFrom != "" {
		detail["effective_from"] = h.EffectiveFrom
	}
	if err := kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: cur.ProfileID, Command: h.Command,
		ReasonCode: h.Reason, EvidenceID: h.Evidence, Before: h.Prior, After: h.New, Detail: detail}); err != nil {
		return nil, err
	}
	if events != nil {
		if err := kit.Emit(ctx, tx, c, events(&nh)...); err != nil {
			return nil, err
		}
	}
	return &nh, nil
}

// loadProfile assembles the read model for a date. asOfTS non-nil reconstructs
// the lifecycle state/hold as recorded at that instant.
func loadProfile(ctx context.Context, tx pgx.Tx, tenantID string, h *Header, date string, asOfTS *time.Time) (*Profile, error) {
	p := &Profile{Header: *h}
	if asOfTS != nil {
		p.AsOf = date
		var state string
		var hold bool
		var version int
		var recorded time.Time
		err := tx.QueryRow(ctx, `SELECT state_after, credit_hold_after, profile_version, recorded_at FROM ar01_profile_history
			WHERE tenant_id = $1 AND profile_id = $2 AND recorded_at <= $3 ORDER BY profile_version DESC LIMIT 1`,
			tenantID, h.ProfileID, *asOfTS).Scan(&state, &hold, &version, &recorded)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, kit.NotFound("financial profile did not exist at the requested date")
		}
		if err != nil {
			return nil, err
		}
		p.State, p.CreditHold, p.Version, p.UpdatedAt = State(state), hold, version, recorded
		if !hold {
			p.HoldReasonCode, p.HoldPlacedBy, p.HoldPlacedAt = "", "", nil
		}
	}
	var err error
	if p.Attributes, err = attrAt(ctx, tx, tenantID, h.ProfileID, date); err != nil {
		return nil, err
	}
	ts, err := termsAt(ctx, tx, tenantID, h.ProfileID, date)
	if err != nil {
		return nil, err
	}
	if len(ts) > 1 {
		return nil, kit.Invalid("ambiguous payment terms: overlapping effective versions")
	}
	if len(ts) == 1 {
		p.PaymentTerms = &ts[0]
	}
	if asOfTS == nil {
		if p.UpcomingTerms, err = upcomingTerms(ctx, tx, tenantID, h.ProfileID, date); err != nil {
			return nil, err
		}
		if p.UpcomingAttributes, err = upcomingAttrs(ctx, tx, tenantID, h.ProfileID, date); err != nil {
			return nil, err
		}
		if p.PendingCreditRequest, err = pendingRequest(ctx, tx, tenantID, h.ProfileID, false); err != nil {
			return nil, err
		}
	}
	return p, nil
}
