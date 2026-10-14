package ar09

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Every query filters tenant_id explicitly (the service may connect as an RLS-bypassing role). Nothing here touches an
// AR-07 table: open items are read through the Receivables port only.

const caseCols = `case_id::text, tenant_id::text, legal_entity_id::text, customer_party_id::text, currency, state, COALESCE(state_before_hold,''),
	owner_principal, dunning_profile, policy_id::text, dunning_stage, aging_snapshot, COALESCE(referral_reason,''), COALESCE(escalated_by,''),
	escalated_at, resolution, resolved_at, COALESCE(closed_by,''), COALESCE(close_reason,''), version, created_by, created_at, updated_at`

func scanCase(row pgx.Row) (*Case, error) {
	var c Case
	var aging, res []byte
	err := row.Scan(&c.CaseID, &c.TenantID, &c.LegalEntityID, &c.CustomerPartyID, &c.Currency, &c.State, &c.StateBefore, &c.OwnerPrincipal,
		&c.DunningProfile, &c.PolicyID, &c.DunningStage, &aging, &c.ReferralReason, &c.EscalatedBy, &c.EscalatedAt, &res, &c.ResolvedAt,
		&c.ClosedBy, &c.CloseReason, &c.Version, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if len(aging) > 0 {
		c.AgingSnapshot = &Aging{}
		if err := json.Unmarshal(aging, c.AgingSnapshot); err != nil {
			return nil, err
		}
	}
	if len(res) > 0 {
		var v any
		if err := json.Unmarshal(res, &v); err != nil {
			return nil, err
		}
		c.Resolution = v
	}
	return &c, nil
}

// getCase loads a case scoped by tenant; lock takes FOR UPDATE (skip: SKIP LOCKED, returns notFound when another worker holds it).
func getCase(ctx context.Context, q pgx.Tx, tenantID, caseID string, lock, skip bool) (*Case, error) {
	if _, err := uuid.Parse(caseID); err != nil {
		return nil, kit.Invalid("case_id is not a valid UUID")
	}
	sql := `SELECT ` + caseCols + ` FROM ar09_collection_cases WHERE tenant_id = $1 AND case_id = $2`
	if lock {
		sql += ` FOR UPDATE`
		if skip {
			sql += ` SKIP LOCKED`
		}
	}
	c, err := scanCase(q.QueryRow(ctx, sql, tenantID, caseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound()
	}
	return c, err
}

// lockForCommand loads the case FOR UPDATE, enforces the caller's legal entity (server-derived, never the body's) and the
// mandatory expected_version.
func lockForCommand(ctx context.Context, tx pgx.Tx, c kit.Caller, caseID string, expected *int) (*Case, error) {
	cs, err := getCase(ctx, tx, c.TenantID, caseID, true, false)
	if err != nil {
		return nil, err
	}
	if cs.LegalEntityID != c.LegalEntityID {
		return nil, notFound() // cross-entity looks identical to absent
	}
	if expected == nil {
		return nil, kit.ErrExpectedVersionRequired
	}
	if cs.Version != *expected {
		return nil, kit.ErrStaleVersion
	}
	return cs, nil
}

// saveCase persists the mutable columns and bumps the version. The row is already locked by this tx.
func saveCase(ctx context.Context, tx pgx.Tx, cs *Case, now time.Time) error {
	var res []byte
	if cs.Resolution != nil {
		var err error
		if res, err = json.Marshal(cs.Resolution); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE ar09_collection_cases SET state = $3, state_before_hold = NULLIF($4,''), dunning_stage = $5,
		referral_reason = NULLIF($6,''), escalated_by = NULLIF($7,''), escalated_at = $8, resolution = $9, resolved_at = $10,
		closed_by = NULLIF($11,''), close_reason = NULLIF($12,''), owner_principal = $13, version = version + 1, updated_at = $14
		WHERE tenant_id = $1 AND case_id = $2 AND version = $15`,
		cs.TenantID, cs.CaseID, cs.State, cs.StateBefore, cs.DunningStage, cs.ReferralReason, cs.EscalatedBy, cs.EscalatedAt, res, cs.ResolvedAt,
		cs.ClosedBy, cs.CloseReason, cs.OwnerPrincipal, now, cs.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return kit.ErrStaleVersion
	}
	cs.Version++
	cs.UpdatedAt = now
	return nil
}

func insertCase(ctx context.Context, tx pgx.Tx, cs *Case, itemIDs []string) error {
	aging, err := json.Marshal(cs.AgingSnapshot)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ar09_collection_cases (case_id, tenant_id, legal_entity_id, customer_party_id, currency, state, owner_principal,
		dunning_profile, policy_id, dunning_stage, aging_snapshot, version, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0,$10,1,$11,$12,$12)`,
		cs.CaseID, cs.TenantID, cs.LegalEntityID, cs.CustomerPartyID, cs.Currency, cs.State, cs.OwnerPrincipal, cs.DunningProfile, cs.PolicyID,
		aging, cs.CreatedBy, cs.CreatedAt); err != nil {
		return err
	}
	for _, id := range itemIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO ar09_case_items (tenant_id, case_id, open_item_id, added_at) VALUES ($1,$2,$3,$4)`,
			cs.TenantID, cs.CaseID, id, cs.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

func caseItemIDs(ctx context.Context, q pgx.Tx, tenantID, caseID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT open_item_id::text FROM ar09_case_items WHERE tenant_id = $1 AND case_id = $2 ORDER BY open_item_id`, tenantID, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- history ----

func addHistory(ctx context.Context, tx pgx.Tx, c kit.Caller, cs *Case, event, from, to, reason string, detail map[string]any) error {
	var d []byte
	if detail != nil {
		var err error
		if d, err = json.Marshal(detail); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO ar09_history (history_id, tenant_id, legal_entity_id, case_id, event, from_state, to_state, version_after, actor_id, reason_code, detail)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,$9,NULLIF($10,''),$11)`,
		uuid.NewString(), c.TenantID, cs.LegalEntityID, cs.CaseID, event, from, to, cs.Version, c.PrincipalID, reason, d)
	return err
}

func listHistory(ctx context.Context, tx pgx.Tx, tenantID, caseID string, limit int) ([]HistoryEvent, error) {
	rows, err := tx.Query(ctx, `SELECT seq, event, COALESCE(from_state,''), COALESCE(to_state,''), version_after, actor_id, COALESCE(reason_code,''), detail, at
		FROM ar09_history WHERE tenant_id = $1 AND case_id = $2 ORDER BY seq LIMIT $3`, tenantID, caseID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryEvent{}
	for rows.Next() {
		var h HistoryEvent
		var d []byte
		if err := rows.Scan(&h.Seq, &h.Event, &h.FromState, &h.ToState, &h.VersionAfter, &h.ActorID, &h.ReasonCode, &d, &h.At); err != nil {
			return nil, err
		}
		if len(d) > 0 {
			_ = json.Unmarshal(d, &h.Detail)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---- policies ----

const policyCols = `policy_id::text, tenant_id::text, legal_entity_id::text, dunning_profile, version, to_char(effective_from,'YYYY-MM-DD'), stages,
	blackout_weekdays, COALESCE((SELECT array_agg(to_char(d,'YYYY-MM-DD') ORDER BY d) FROM unnest(blackout_dates) d), '{}'),
	max_notices_per_window, window_days, promise_grace_days, published_by, published_at`

func scanPolicy(row pgx.Row) (*Policy, error) {
	var p Policy
	var stages []byte
	var wd []int16
	if err := row.Scan(&p.PolicyID, &p.TenantID, &p.LegalEntityID, &p.DunningProfile, &p.Version, &p.EffectiveFrom, &stages, &wd, &p.BlackoutDates,
		&p.MaxNoticesPerWindow, &p.WindowDays, &p.PromiseGraceDays, &p.PublishedBy, &p.PublishedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(stages, &p.Stages); err != nil {
		return nil, err
	}
	p.BlackoutWeekdays = make([]int, 0, len(wd))
	for _, d := range wd {
		p.BlackoutWeekdays = append(p.BlackoutWeekdays, int(d))
	}
	if p.BlackoutDates == nil {
		p.BlackoutDates = []string{}
	}
	return &p, nil
}

// insertPolicy publishes the next version of a profile (immutable once written).
func insertPolicy(ctx context.Context, tx pgx.Tx, c kit.Caller, p *Policy) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, c.TenantID+"|"+p.LegalEntityID+"|"+p.DunningProfile); err != nil {
		return err
	}
	var prevFrom *string
	var maxV int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0), (SELECT to_char(effective_from,'YYYY-MM-DD') FROM ar09_dunning_policies
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND dunning_profile = $3 ORDER BY version DESC LIMIT 1)
		FROM ar09_dunning_policies WHERE tenant_id = $1 AND legal_entity_id = $2 AND dunning_profile = $3`,
		c.TenantID, p.LegalEntityID, p.DunningProfile).Scan(&maxV, &prevFrom); err != nil {
		return err
	}
	if prevFrom != nil && p.EffectiveFrom <= *prevFrom {
		return kit.Unprocessable(kit.CodeValidationFailed, "effective_from must be later than the previous version's ("+*prevFrom+"): versions do not overlap")
	}
	stages, err := json.Marshal(p.Stages)
	if err != nil {
		return err
	}
	wd := make([]int16, 0, len(p.BlackoutWeekdays))
	for _, d := range p.BlackoutWeekdays {
		wd = append(wd, int16(d))
	}
	dates := make([]time.Time, 0, len(p.BlackoutDates))
	for _, d := range p.BlackoutDates {
		t, _ := time.Parse("2006-01-02", d)
		dates = append(dates, t)
	}
	p.PolicyID, p.TenantID, p.Version, p.PublishedBy = uuid.NewString(), c.TenantID, maxV+1, c.PrincipalID
	return tx.QueryRow(ctx, `INSERT INTO ar09_dunning_policies (policy_id, tenant_id, legal_entity_id, dunning_profile, version, effective_from, stages,
		blackout_weekdays, blackout_dates, max_notices_per_window, window_days, promise_grace_days, published_by)
		VALUES ($1,$2,$3,$4,$5,$6::date,$7,$8,$9,$10,$11,$12,$13) RETURNING published_at`,
		p.PolicyID, p.TenantID, p.LegalEntityID, p.DunningProfile, p.Version, p.EffectiveFrom, stages, wd, dates,
		p.MaxNoticesPerWindow, p.WindowDays, p.PromiseGraceDays, p.PublishedBy).Scan(&p.PublishedAt)
}

// policyAt resolves the version in force at a date (the latest whose effective_from has been reached).
func policyAt(ctx context.Context, q pgx.Tx, tenantID, entityID, profile string, at time.Time) (*Policy, error) {
	p, err := scanPolicy(q.QueryRow(ctx, `SELECT `+policyCols+` FROM ar09_dunning_policies
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND dunning_profile = $3 AND effective_from <= $4::date ORDER BY version DESC LIMIT 1`,
		tenantID, entityID, profile, at.UTC().Format("2006-01-02")))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.Unprocessable(kit.CodeValidationFailed, "no dunning policy is in force for profile "+profile)
	}
	return p, err
}

func policyByID(ctx context.Context, q pgx.Tx, tenantID, policyID string) (*Policy, error) {
	p, err := scanPolicy(q.QueryRow(ctx, `SELECT `+policyCols+` FROM ar09_dunning_policies WHERE tenant_id = $1 AND policy_id = $2`, tenantID, policyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound()
	}
	return p, err
}

// ---- actions ----

const actionCols = `action_id::text, case_id::text, stage_no, channel, template_key, delivery_result, COALESCE(suppression_reason,''), COALESCE(failure_detail,''),
	COALESCE(provider_ref,''), COALESCE(notice_artifact_hash,''), aging_snapshot, actor_id, automated, recorded_at`

func scanAction(row pgx.Row) (*Action, error) {
	var a Action
	var aging []byte
	if err := row.Scan(&a.ActionID, &a.CaseID, &a.StageNo, &a.Channel, &a.TemplateKey, &a.DeliveryResult, &a.SuppressionReason, &a.FailureDetail,
		&a.ProviderRef, &a.NoticeArtifactHash, &aging, &a.ActorID, &a.Automated, &a.RecordedAt); err != nil {
		return nil, err
	}
	if len(aging) > 0 {
		a.AgingSnapshot = &Aging{}
		if err := json.Unmarshal(aging, a.AgingSnapshot); err != nil {
			return nil, err
		}
	}
	return &a, nil
}

func insertAction(ctx context.Context, tx pgx.Tx, c kit.Caller, cs *Case, a *Action, body string, at time.Time) error {
	var aging []byte
	if a.AgingSnapshot != nil {
		var err error
		if aging, err = json.Marshal(a.AgingSnapshot); err != nil {
			return err
		}
	}
	a.ActionID, a.CaseID, a.RecordedAt = uuid.NewString(), cs.CaseID, at
	_, err := tx.Exec(ctx, `INSERT INTO ar09_dunning_actions (action_id, tenant_id, legal_entity_id, case_id, customer_party_id, stage_no, channel, template_key,
		delivery_result, suppression_reason, failure_detail, provider_ref, notice_artifact_hash, notice_body, aging_snapshot, actor_id, automated, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),NULLIF($13,''),NULLIF($14,''),$15,$16,$17,$18)`,
		a.ActionID, c.TenantID, cs.LegalEntityID, cs.CaseID, cs.CustomerPartyID, a.StageNo, a.Channel, a.TemplateKey, a.DeliveryResult, a.SuppressionReason,
		a.FailureDetail, a.ProviderRef, a.NoticeArtifactHash, body, aging, a.ActorID, a.Automated, at)
	return err
}

// stageSent reports a SENT/DELIVERED notice for the stage (the unique index enforces it; this lets callers skip cleanly).
func stageSent(ctx context.Context, tx pgx.Tx, tenantID, caseID string, stage int) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ar09_dunning_actions WHERE tenant_id = $1 AND case_id = $2 AND stage_no = $3
		AND delivery_result IN ('SENT','DELIVERED'))`, tenantID, caseID, stage).Scan(&ok)
	return ok, err
}

// lastSuppression returns the reason of the latest action of the stage when it is a suppression ("" otherwise).
func lastSuppression(ctx context.Context, tx pgx.Tx, tenantID, caseID string, stage int) (string, error) {
	var r string
	err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT CASE WHEN delivery_result = 'SUPPRESSED' THEN suppression_reason ELSE '' END FROM ar09_dunning_actions
		WHERE tenant_id = $1 AND case_id = $2 AND stage_no = $3 ORDER BY seq DESC LIMIT 1), '')`, tenantID, caseID, stage).Scan(&r)
	return r, err
}

// attemptsInWindow counts the customer's real contact attempts (sent or failed, not suppressed) since the window start.
func attemptsInWindow(ctx context.Context, tx pgx.Tx, tenantID, entityID, customerID string, since time.Time) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM ar09_dunning_actions WHERE tenant_id = $1 AND legal_entity_id = $2 AND customer_party_id = $3
		AND delivery_result <> 'SUPPRESSED' AND recorded_at > $4`, tenantID, entityID, customerID, since).Scan(&n)
	return n, err
}

func listActions(ctx context.Context, q pgx.Tx, where string, limit int, args ...any) ([]Action, error) {
	rows, err := q.Query(ctx, `SELECT `+actionCols+` FROM ar09_dunning_actions WHERE `+where+` ORDER BY seq DESC LIMIT `+itoa(limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Action{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// ---- promises ----

const promiseCols = `promise_id::text, case_id::text, version_no, to_char(promised_date,'YYYY-MM-DD'), promised_amount, currency, COALESCE(supersedes::text,''),
	COALESCE(reason_code,''), recorded_by, recorded_at`

func scanPromise(row pgx.Row) (*Promise, error) {
	var p Promise
	if err := row.Scan(&p.PromiseID, &p.CaseID, &p.VersionNo, &p.PromisedDate, &p.PromisedAmount, &p.Currency, &p.Supersedes, &p.ReasonCode, &p.RecordedBy, &p.RecordedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

func latestPromise(ctx context.Context, q pgx.Tx, tenantID, caseID string) (*Promise, error) {
	p, err := scanPromise(q.QueryRow(ctx, `SELECT `+promiseCols+` FROM ar09_promises WHERE tenant_id = $1 AND case_id = $2 ORDER BY version_no DESC LIMIT 1`, tenantID, caseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func listPromises(ctx context.Context, q pgx.Tx, tenantID, caseID string) ([]Promise, error) {
	rows, err := q.Query(ctx, `SELECT `+promiseCols+` FROM ar09_promises WHERE tenant_id = $1 AND case_id = $2 ORDER BY version_no`, tenantID, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Promise{}
	for rows.Next() {
		p, err := scanPromise(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ---- holds ----

const holdCols = `hold_id::text, case_id::text, reason_code, COALESCE(evidence_id,''), placed_by, placed_at, COALESCE(released_by,''), released_at, COALESCE(release_reason,'')`

func scanHold(row pgx.Row) (*Hold, error) {
	var h Hold
	if err := row.Scan(&h.HoldID, &h.CaseID, &h.ReasonCode, &h.EvidenceID, &h.PlacedBy, &h.PlacedAt, &h.ReleasedBy, &h.ReleasedAt, &h.ReleaseReason); err != nil {
		return nil, err
	}
	return &h, nil
}

func activeHold(ctx context.Context, q pgx.Tx, tenantID, caseID string) (*Hold, error) {
	h, err := scanHold(q.QueryRow(ctx, `SELECT `+holdCols+` FROM ar09_holds WHERE tenant_id = $1 AND case_id = $2 AND released_at IS NULL`, tenantID, caseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return h, err
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
