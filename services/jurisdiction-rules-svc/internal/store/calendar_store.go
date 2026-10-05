package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 Wave 4 (calendar half) persistence: regulatory calendars with
// immutable versions, and obligation rules. Both are DRAFT until published;
// PUBLISHED is final (database-enforced), so an amendment is a new version.

type Calendar struct {
	CalendarID     string    `json:"calendar_id"`
	CalendarCode   string    `json:"calendar_code"`
	JurisdictionID string    `json:"jurisdiction_id"`
	Authority      string    `json:"authority"`
	Description    *string   `json:"description"`
	CreatedAt      time.Time `json:"created_at"`
	CreatedBy      string    `json:"created_by_principal_id"`
}

type CalendarVersionRecord struct {
	CalendarVersionID string           `json:"calendar_version_id"`
	CalendarCode      string           `json:"calendar_code"`
	Version           int              `json:"version"`
	EffectiveFrom     string           `json:"effective_from"`
	Timezone          string           `json:"timezone"`
	WeekendDays       []int            `json:"weekend_days"`
	CutoffTime        *string          `json:"cutoff_time"`
	Status            string           `json:"status"`
	Holidays          []domain.Holiday `json:"holidays"`
	SourceIDs         []string         `json:"source_ids"`
	CreatedAt         time.Time        `json:"created_at"`
	CreatedBy         string           `json:"created_by_principal_id"`
	PublishedAt       *time.Time       `json:"published_at"`
	PublishedBy       *string          `json:"published_by"`
}

type ObligationRuleRecord struct {
	ObligationRuleID          string     `json:"obligation_rule_id"`
	JurisdictionID            string     `json:"jurisdiction_id"`
	RegimeID                  *string    `json:"regime_id"`
	InterpretationID          *string    `json:"interpretation_id"`
	ObligationCode            string     `json:"obligation_code"`
	RuleVersion               int        `json:"rule_version"`
	Name                      string     `json:"name"`
	PeriodBasis               string     `json:"period_basis"`
	Anchor                    string     `json:"anchor"`
	OffsetMonths              int        `json:"offset_months"`
	OffsetDays                int        `json:"offset_days"`
	OffsetToMonthEnd          bool       `json:"offset_to_month_end"`
	BusinessDayAdjustment     string     `json:"business_day_adjustment"`
	CalendarCode              *string    `json:"calendar_code"`
	EffectiveFrom             string     `json:"effective_from"`
	EffectiveTo               *string    `json:"effective_to"`
	ExtensionAllowed          bool       `json:"extension_allowed"`
	MaxExtensionDays          int        `json:"max_extension_days"`
	ExtensionRequiresEvidence bool       `json:"extension_requires_evidence"`
	CutoffApplies             bool       `json:"cutoff_applies"`
	EscalationOwner           *string    `json:"escalation_owner"`
	EscalationSLAHours        *int       `json:"escalation_sla_hours"`
	Status                    string     `json:"status"`
	SourceIDs                 []string   `json:"source_ids"`
	CreatedAt                 time.Time  `json:"created_at"`
	CreatedBy                 string     `json:"created_by_principal_id"`
	PublishedAt               *time.Time `json:"published_at"`
	PublishedBy               *string    `json:"published_by"`
}

type CreateObligationParams struct {
	JurisdictionID, ObligationCode, Name, PeriodBasis, Anchor, BusinessDayAdjustment string
	OffsetMonths, OffsetDays                                                         int
	OffsetToMonthEnd                                                                 bool
	CalendarCode                                                                     string
	EffectiveFrom                                                                    string
	EffectiveTo                                                                      *string
	ExtensionAllowed                                                                 bool
	MaxExtensionDays                                                                 int
	ExtensionRequiresEvidence, CutoffApplies                                         bool
	EscalationOwner                                                                  *string
	EscalationSLAHours                                                               *int
	CreatedBy                                                                        string
}

// ── calendars ───────────────────────────────────────────────────────────────

func (s *PgStore) CreateCalendar(ctx context.Context, code, jurisdictionID, authority string, description *string, actor string) (*Calendar, bool, error) {
	if _, err := s.FindByIDAny(ctx, jurisdictionID); err != nil {
		return nil, false, err
	}
	const cols = `calendar_id::text, calendar_code, jurisdiction_id::text, authority, description, created_at, created_by_principal_id`
	scan := func(r pgx.Row) (*Calendar, error) {
		var c Calendar
		err := r.Scan(&c.CalendarID, &c.CalendarCode, &c.JurisdictionID, &c.Authority, &c.Description, &c.CreatedAt, &c.CreatedBy)
		return &c, err
	}
	c, err := scan(s.pool.QueryRow(ctx, `INSERT INTO regulatory_calendars (calendar_code, jurisdiction_id, authority, description, created_by_principal_id)
		VALUES ($1,$2::uuid,$3,$4,$5) ON CONFLICT (calendar_code) DO NOTHING RETURNING `+cols, code, jurisdictionID, authority, description, actor))
	if err == nil {
		return c, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, s.registryFail("CreateCalendar", err, nil)
	}
	c, err = scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM regulatory_calendars WHERE calendar_code=$1`, code))
	if err != nil {
		return nil, false, s.registryFail("CreateCalendar lookup", err, nil)
	}
	if c.JurisdictionID != jurisdictionID || c.Authority != authority {
		return nil, false, domain.ErrConflict
	}
	return c, false, nil
}

func (s *PgStore) GetCalendar(ctx context.Context, code string) (*Calendar, error) {
	var c Calendar
	err := s.pool.QueryRow(ctx, `SELECT calendar_id::text, calendar_code, jurisdiction_id::text, authority, description, created_at, created_by_principal_id
		FROM regulatory_calendars WHERE calendar_code=$1`, code).Scan(&c.CalendarID, &c.CalendarCode, &c.JurisdictionID, &c.Authority, &c.Description, &c.CreatedAt, &c.CreatedBy)
	if err != nil {
		return nil, s.registryFail("GetCalendar", err, domain.ErrCalendarNotFound)
	}
	return &c, nil
}

func (s *PgStore) ListCalendars(ctx context.Context) ([]*Calendar, error) {
	rows, err := s.pool.Query(ctx, `SELECT calendar_id::text, calendar_code, jurisdiction_id::text, authority, description, created_at, created_by_principal_id
		FROM regulatory_calendars ORDER BY calendar_code`)
	if err != nil {
		return nil, s.registryFail("ListCalendars", err, nil)
	}
	defer rows.Close()
	out := []*Calendar{}
	for rows.Next() {
		var c Calendar
		if err := rows.Scan(&c.CalendarID, &c.CalendarCode, &c.JurisdictionID, &c.Authority, &c.Description, &c.CreatedAt, &c.CreatedBy); err != nil {
			return nil, s.registryFail("ListCalendars scan", err, nil)
		}
		out = append(out, &c)
	}
	return out, s.registryFail("ListCalendars rows", rows.Err(), nil)
}

const calVersionCols = `v.calendar_version_id::text, c.calendar_code, v.version, to_char(v.effective_from,'YYYY-MM-DD'), v.timezone,
	v.weekend_days::int[], to_char(v.cutoff_time,'HH24:MI'), v.status, v.created_at, v.created_by_principal_id, v.published_at, v.published_by`

func (s *PgStore) loadCalendarVersion(ctx context.Context, q querier, where string, args ...any) (*CalendarVersionRecord, error) {
	var r CalendarVersionRecord
	var weekend []int32
	err := q.QueryRow(ctx, `SELECT `+calVersionCols+` FROM regulatory_calendar_versions v JOIN regulatory_calendars c USING (calendar_id) WHERE `+where, args...).
		Scan(&r.CalendarVersionID, &r.CalendarCode, &r.Version, &r.EffectiveFrom, &r.Timezone, &weekend, &r.CutoffTime, &r.Status,
			&r.CreatedAt, &r.CreatedBy, &r.PublishedAt, &r.PublishedBy)
	if err != nil {
		return nil, err
	}
	r.WeekendDays = make([]int, len(weekend))
	for i, w := range weekend {
		r.WeekendDays[i] = int(w)
	}
	rows, err := q.Query(ctx, `SELECT to_char(holiday_date,'YYYY-MM-DD'), name FROM regulatory_holidays WHERE calendar_version_id::text=$1 ORDER BY holiday_date`, r.CalendarVersionID)
	if err != nil {
		return nil, err
	}
	r.Holidays = []domain.Holiday{}
	for rows.Next() {
		var h domain.Holiday
		if err := rows.Scan(&h.Date, &h.Name); err != nil {
			rows.Close()
			return nil, err
		}
		r.Holidays = append(r.Holidays, h)
	}
	rows.Close()
	srows, err := q.Query(ctx, `SELECT source_id::text FROM calendar_version_sources WHERE calendar_version_id::text=$1 ORDER BY 1`, r.CalendarVersionID)
	if err != nil {
		return nil, err
	}
	r.SourceIDs = []string{}
	for srows.Next() {
		var sid string
		if err := srows.Scan(&sid); err != nil {
			srows.Close()
			return nil, err
		}
		r.SourceIDs = append(r.SourceIDs, sid)
	}
	srows.Close()
	return &r, nil
}

// CreateCalendarVersion drafts the next version of a calendar. The version
// number is assigned here (max + 1), never by the caller.
func (s *PgStore) CreateCalendarVersion(ctx context.Context, code, effectiveFrom, tz string, weekend []int, cutoff *string, holidays []domain.Holiday, actor string) (*CalendarVersionRecord, error) {
	from, perr := time.Parse("2006-01-02", effectiveFrom)
	if perr != nil {
		return nil, domain.ErrInvalidReference
	}
	if err := domain.ValidateCalendarVersion(domain.CalendarVersion{Timezone: tz, WeekendDays: weekend, CutoffTime: cutoff, Holidays: holidays, EffectiveFrom: from}); err != nil {
		return nil, err
	}
	tx, err := s.begin(ctx, "CreateCalendarVersion")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var calID string
	if err := tx.QueryRow(ctx, `SELECT calendar_id::text FROM regulatory_calendars WHERE calendar_code=$1 FOR UPDATE`, code).Scan(&calID); err != nil {
		return nil, s.registryFail("CreateCalendarVersion lock", err, domain.ErrCalendarNotFound)
	}
	wk := make([]int32, len(weekend))
	for i, w := range weekend {
		wk[i] = int32(w)
	}
	var vid string
	if err := tx.QueryRow(ctx, `INSERT INTO regulatory_calendar_versions (calendar_id, version, effective_from, timezone, weekend_days, cutoff_time, created_by_principal_id)
		VALUES ($1::uuid, COALESCE((SELECT MAX(version) FROM regulatory_calendar_versions WHERE calendar_id=$1::uuid),0)+1, $2::date, $3, $4::smallint[], $5::time, $6)
		RETURNING calendar_version_id::text`, calID, effectiveFrom, tz, wk, cutoff, actor).Scan(&vid); err != nil {
		return nil, s.registryFail("CreateCalendarVersion insert", err, nil)
	}
	for _, h := range holidays {
		if _, err := tx.Exec(ctx, `INSERT INTO regulatory_holidays (calendar_version_id, holiday_date, name) VALUES ($1::uuid,$2::date,$3)`, vid, h.Date, h.Name); err != nil {
			return nil, s.registryFail("CreateCalendarVersion holiday", err, nil)
		}
	}
	rec, err := s.loadCalendarVersion(ctx, tx, `v.calendar_version_id::text=$1`, vid)
	if err != nil {
		return nil, s.registryFail("CreateCalendarVersion read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("CreateCalendarVersion commit", err, nil)
	}
	return rec, nil
}

func (s *PgStore) GetCalendarVersion(ctx context.Context, code string, version int) (*CalendarVersionRecord, error) {
	r, err := s.loadCalendarVersion(ctx, s.pool, `c.calendar_code=$1 AND v.version=$2`, code, version)
	if err != nil {
		return nil, s.registryFail("GetCalendarVersion", err, domain.ErrCalendarNotFound)
	}
	return r, nil
}

func (s *PgStore) ListCalendarVersions(ctx context.Context, code string) ([]*CalendarVersionRecord, error) {
	if _, err := s.GetCalendar(ctx, code); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT v.version FROM regulatory_calendar_versions v JOIN regulatory_calendars c USING (calendar_id) WHERE c.calendar_code=$1 ORDER BY v.version`, code)
	if err != nil {
		return nil, s.registryFail("ListCalendarVersions", err, nil)
	}
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, s.registryFail("ListCalendarVersions scan", err, nil)
		}
		versions = append(versions, v)
	}
	rows.Close()
	out := []*CalendarVersionRecord{}
	for _, v := range versions {
		r, err := s.GetCalendarVersion(ctx, code, v)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// liveSourceCount counts how many of ids exist and are not superseded.
func (s *PgStore) liveSourceCount(ctx context.Context, q querier, ids []string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT COUNT(DISTINCT source_id) FROM regulatory_sources WHERE source_id::text = ANY($1) AND superseded_by_source_id IS NULL`, ids).Scan(&n)
	return n, err
}

// SetCalendarVersionSources replaces the sources of a DRAFT calendar version.
func (s *PgStore) SetCalendarVersionSources(ctx context.Context, code string, version int, sourceIDs []string) (*CalendarVersionRecord, error) {
	tx, err := s.begin(ctx, "SetCalendarVersionSources")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	cur, err := s.loadCalendarVersion(ctx, tx, `c.calendar_code=$1 AND v.version=$2`, code, version)
	if err != nil {
		return nil, s.registryFail("SetCalendarVersionSources load", err, domain.ErrCalendarNotFound)
	}
	if cur.Status != "DRAFT" {
		return nil, domain.ErrNotDraft
	}
	if n, err := s.liveSourceCount(ctx, tx, sourceIDs); err != nil || n != len(uniq(sourceIDs)) {
		return nil, domain.ErrInvalidReference
	}
	if _, err := tx.Exec(ctx, `DELETE FROM calendar_version_sources WHERE calendar_version_id::text=$1`, cur.CalendarVersionID); err != nil {
		return nil, s.registryFail("SetCalendarVersionSources unlink", err, nil)
	}
	for _, sid := range uniq(sourceIDs) {
		if _, err := tx.Exec(ctx, `INSERT INTO calendar_version_sources VALUES ($1::uuid,$2::uuid)`, cur.CalendarVersionID, sid); err != nil {
			return nil, s.registryFail("SetCalendarVersionSources link", err, nil)
		}
	}
	out, err := s.loadCalendarVersion(ctx, tx, `v.calendar_version_id::text=$1`, cur.CalendarVersionID)
	if err != nil {
		return nil, s.registryFail("SetCalendarVersionSources read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("SetCalendarVersionSources commit", err, nil)
	}
	return out, nil
}

// unreadySources counts a record's sources that are unreviewed or superseded.
func (s *PgStore) unreadySources(ctx context.Context, q querier, table, idCol, id string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT COUNT(*) FROM `+table+` l JOIN regulatory_sources rs USING (source_id)
		WHERE l.`+idCol+`::text=$1 AND (rs.reviewed_by_principal_id IS NULL OR rs.superseded_by_source_id IS NOT NULL)`, id).Scan(&n)
	return n, err
}

// PublishCalendarVersion freezes a DRAFT version. The publisher must differ
// from the author, and the version must cite at least one reviewed,
// non-superseded source (the official calendar it was transcribed from).
func (s *PgStore) PublishCalendarVersion(ctx context.Context, code string, version int, actor string) (*CalendarVersionRecord, bool, error) {
	cur, err := s.GetCalendarVersion(ctx, code, version)
	if err != nil {
		return nil, false, err
	}
	if cur.Status == "PUBLISHED" {
		return cur, false, nil
	}
	if cur.CreatedBy == actor {
		return nil, false, domain.ErrNotIndependent
	}
	if len(cur.SourceIDs) == 0 {
		return nil, false, domain.ErrSourceNotReady
	}
	if n, err := s.unreadySources(ctx, s.pool, "calendar_version_sources", "calendar_version_id", cur.CalendarVersionID); err != nil {
		return nil, false, s.registryFail("PublishCalendarVersion sources", err, nil)
	} else if n > 0 {
		return nil, false, domain.ErrSourceNotReady
	}
	if _, err := s.pool.Exec(ctx, `UPDATE regulatory_calendar_versions SET status='PUBLISHED', published_at=NOW(), published_by=$2
		WHERE calendar_version_id::text=$1 AND status='DRAFT'`, cur.CalendarVersionID, actor); err != nil {
		return nil, false, s.registryFail("PublishCalendarVersion", err, nil)
	}
	out, err := s.GetCalendarVersion(ctx, code, version)
	return out, true, err
}

// ── obligation rules ────────────────────────────────────────────────────────

const obligationCols = `o.obligation_rule_id::text, o.jurisdiction_id::text, o.regime_id::text, o.interpretation_id::text, o.obligation_code, o.rule_version,
	o.name, o.period_basis, o.anchor, o.offset_months, o.offset_days, o.offset_to_month_end, o.business_day_adjustment, o.calendar_code,
	to_char(o.effective_from,'YYYY-MM-DD'), to_char(o.effective_to,'YYYY-MM-DD'), o.extension_allowed, o.max_extension_days,
	o.extension_requires_evidence, o.cutoff_applies, o.escalation_owner, o.escalation_sla_hours, o.status, o.created_at,
	o.created_by_principal_id, o.published_at, o.published_by`

func (s *PgStore) loadObligation(ctx context.Context, q querier, where string, args ...any) (*ObligationRuleRecord, error) {
	var r ObligationRuleRecord
	err := q.QueryRow(ctx, `SELECT `+obligationCols+` FROM obligation_rules o WHERE `+where, args...).
		Scan(&r.ObligationRuleID, &r.JurisdictionID, &r.RegimeID, &r.InterpretationID, &r.ObligationCode, &r.RuleVersion, &r.Name, &r.PeriodBasis,
			&r.Anchor, &r.OffsetMonths, &r.OffsetDays, &r.OffsetToMonthEnd, &r.BusinessDayAdjustment, &r.CalendarCode, &r.EffectiveFrom, &r.EffectiveTo,
			&r.ExtensionAllowed, &r.MaxExtensionDays, &r.ExtensionRequiresEvidence, &r.CutoffApplies, &r.EscalationOwner, &r.EscalationSLAHours,
			&r.Status, &r.CreatedAt, &r.CreatedBy, &r.PublishedAt, &r.PublishedBy)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT source_id::text FROM obligation_rule_sources WHERE obligation_rule_id::text=$1 ORDER BY 1`, r.ObligationRuleID)
	if err != nil {
		return nil, err
	}
	r.SourceIDs = []string{}
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return nil, err
		}
		r.SourceIDs = append(r.SourceIDs, sid)
	}
	rows.Close()
	return &r, nil
}

// CreateObligationRule drafts the next version of an obligation rule (the
// version number is assigned here, per jurisdiction and obligation code).
func (s *PgStore) CreateObligationRule(ctx context.Context, p CreateObligationParams) (*ObligationRuleRecord, error) {
	from, perr := time.Parse("2006-01-02", p.EffectiveFrom)
	if perr != nil {
		return nil, domain.ErrInvalidReference
	}
	rule := domain.ObligationRule{ObligationCode: p.ObligationCode, Name: p.Name, PeriodBasis: p.PeriodBasis, Anchor: p.Anchor, OffsetMonths: p.OffsetMonths,
		OffsetDays: p.OffsetDays, OffsetToMonthEnd: p.OffsetToMonthEnd, BusinessDayAdjustment: p.BusinessDayAdjustment, CalendarCode: p.CalendarCode,
		EffectiveFrom: from, ExtensionAllowed: p.ExtensionAllowed, MaxExtensionDays: p.MaxExtensionDays, ExtensionRequiresEvidence: p.ExtensionRequiresEvidence,
		CutoffApplies: p.CutoffApplies, EscalationSLAHours: p.EscalationSLAHours}
	if p.EffectiveTo != nil {
		to, err := time.Parse("2006-01-02", *p.EffectiveTo)
		if err != nil {
			return nil, domain.ErrInvalidReference
		}
		rule.EffectiveTo = &to
	}
	if err := domain.ValidateObligationRule(rule); err != nil {
		return nil, err
	}
	if _, err := s.FindByIDAny(ctx, p.JurisdictionID); err != nil {
		return nil, err
	}
	tx, err := s.begin(ctx, "CreateObligationRule")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize version assignment per (jurisdiction, code).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, p.JurisdictionID+"|"+p.ObligationCode); err != nil {
		return nil, s.registryFail("CreateObligationRule lock", err, nil)
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO obligation_rules (jurisdiction_id, obligation_code, rule_version, name, period_basis, anchor, offset_months, offset_days,
			offset_to_month_end, business_day_adjustment, calendar_code, effective_from, effective_to, extension_allowed, max_extension_days,
			extension_requires_evidence, cutoff_applies, escalation_owner, escalation_sla_hours, created_by_principal_id)
		VALUES ($1::uuid,$2::varchar, COALESCE((SELECT MAX(rule_version) FROM obligation_rules WHERE jurisdiction_id=$1::uuid AND obligation_code=$2::varchar),0)+1,
			$3,$4,$5,$6,$7,$8,$9,$10,$11::date,$12::date,$13,$14,$15,$16,$17,$18,$19) RETURNING obligation_rule_id::text`,
		p.JurisdictionID, p.ObligationCode, p.Name, p.PeriodBasis, p.Anchor, p.OffsetMonths, p.OffsetDays, p.OffsetToMonthEnd, p.BusinessDayAdjustment,
		nullIfEmpty(p.CalendarCode), p.EffectiveFrom, p.EffectiveTo, p.ExtensionAllowed, p.MaxExtensionDays, p.ExtensionRequiresEvidence, p.CutoffApplies,
		p.EscalationOwner, p.EscalationSLAHours, p.CreatedBy).Scan(&id); err != nil {
		return nil, s.registryFail("CreateObligationRule insert", err, nil)
	}
	rec, err := s.loadObligation(ctx, tx, `o.obligation_rule_id::text=$1`, id)
	if err != nil {
		return nil, s.registryFail("CreateObligationRule read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("CreateObligationRule commit", err, nil)
	}
	return rec, nil
}

func (s *PgStore) GetObligationRule(ctx context.Context, id string) (*ObligationRuleRecord, error) {
	r, err := s.loadObligation(ctx, s.pool, `o.obligation_rule_id::text=$1`, id)
	if err != nil {
		return nil, s.registryFail("GetObligationRule", err, domain.ErrObligationRuleNotFound)
	}
	return r, nil
}

func (s *PgStore) ListObligationRules(ctx context.Context, jurisdictionID, code string, limit, offset int) ([]*ObligationRuleRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT obligation_rule_id::text FROM obligation_rules WHERE ($1='' OR jurisdiction_id::text=$1) AND ($2='' OR obligation_code=$2)
		ORDER BY obligation_code, rule_version, obligation_rule_id LIMIT $3 OFFSET $4`, jurisdictionID, code, clampLimit(limit, 50, 200), offset)
	if err != nil {
		return nil, s.registryFail("ListObligationRules", err, nil)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, s.registryFail("ListObligationRules scan", err, nil)
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []*ObligationRuleRecord{}
	for _, id := range ids {
		r, err := s.GetObligationRule(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// SetObligationProvenance replaces the regime, interpretation and sources of
// a DRAFT obligation rule. The interpretation must be APPROVED and belong to
// the rule's jurisdiction or an ancestor.
func (s *PgStore) SetObligationProvenance(ctx context.Context, id string, regimeID, interpretationID *string, sourceIDs []string) (*ObligationRuleRecord, error) {
	tx, err := s.begin(ctx, "SetObligationProvenance")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var jurID, status string
	if err := tx.QueryRow(ctx, `SELECT jurisdiction_id::text, status FROM obligation_rules WHERE obligation_rule_id::text=$1 FOR UPDATE`, id).Scan(&jurID, &status); err != nil {
		return nil, s.registryFail("SetObligationProvenance lock", err, domain.ErrObligationRuleNotFound)
	}
	if status != "DRAFT" {
		return nil, domain.ErrNotDraft
	}
	if regimeID != nil {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM regulatory_regimes WHERE regime_id::text=$1`, *regimeID).Scan(&n); err != nil || n == 0 {
			return nil, domain.ErrInvalidReference
		}
	}
	if interpretationID != nil {
		var istatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM interpretation_records WHERE interpretation_id::text=$1`, *interpretationID).Scan(&istatus); err != nil {
			return nil, domain.ErrInvalidReference
		}
		if istatus != "APPROVED" {
			return nil, domain.ErrUnapprovedInterpretation
		}
		var inChain bool
		if err := tx.QueryRow(ctx, `WITH RECURSIVE chain AS (
				SELECT jurisdiction_id, parent_jurisdiction_id FROM jurisdictions WHERE jurisdiction_id::text=$1
				UNION ALL SELECT j.jurisdiction_id, j.parent_jurisdiction_id FROM jurisdictions j JOIN chain c ON j.jurisdiction_id = c.parent_jurisdiction_id)
			SELECT EXISTS (SELECT 1 FROM chain c JOIN interpretation_records i ON i.jurisdiction_id = c.jurisdiction_id WHERE i.interpretation_id::text=$2)`,
			jurID, *interpretationID).Scan(&inChain); err != nil || !inChain {
			return nil, domain.ErrInvalidReference
		}
	}
	if len(sourceIDs) > 0 {
		if n, err := s.liveSourceCount(ctx, tx, sourceIDs); err != nil || n != len(uniq(sourceIDs)) {
			return nil, domain.ErrInvalidReference
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE obligation_rules SET regime_id=$2::uuid, interpretation_id=$3::uuid WHERE obligation_rule_id::text=$1`, id, regimeID, interpretationID); err != nil {
		return nil, s.registryFail("SetObligationProvenance update", err, nil)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM obligation_rule_sources WHERE obligation_rule_id::text=$1`, id); err != nil {
		return nil, s.registryFail("SetObligationProvenance unlink", err, nil)
	}
	for _, sid := range uniq(sourceIDs) {
		if _, err := tx.Exec(ctx, `INSERT INTO obligation_rule_sources VALUES ($1::uuid,$2::uuid)`, id, sid); err != nil {
			return nil, s.registryFail("SetObligationProvenance link", err, nil)
		}
	}
	out, err := s.loadObligation(ctx, tx, `o.obligation_rule_id::text=$1`, id)
	if err != nil {
		return nil, s.registryFail("SetObligationProvenance read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("SetObligationProvenance commit", err, nil)
	}
	return out, nil
}

// PublishObligationRule freezes a DRAFT rule. The publisher must differ from
// the author; the rule needs a regime, an APPROVED interpretation, at least one
// reviewed non-superseded source, and an existing calendar when it names one.
func (s *PgStore) PublishObligationRule(ctx context.Context, id, actor string) (*ObligationRuleRecord, bool, error) {
	cur, err := s.GetObligationRule(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if cur.Status == "PUBLISHED" {
		return cur, false, nil
	}
	if cur.CreatedBy == actor {
		return nil, false, domain.ErrNotIndependent
	}
	if cur.RegimeID == nil || cur.InterpretationID == nil || len(cur.SourceIDs) == 0 {
		return nil, false, domain.ErrSourceNotReady
	}
	var istatus string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM interpretation_records WHERE interpretation_id::text=$1`, *cur.InterpretationID).Scan(&istatus); err != nil || istatus != "APPROVED" {
		return nil, false, domain.ErrUnapprovedInterpretation
	}
	if n, err := s.unreadySources(ctx, s.pool, "obligation_rule_sources", "obligation_rule_id", id); err != nil {
		return nil, false, s.registryFail("PublishObligationRule sources", err, nil)
	} else if n > 0 {
		return nil, false, domain.ErrSourceNotReady
	}
	if cur.CalendarCode != nil {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM regulatory_calendars WHERE calendar_code=$1`, *cur.CalendarCode).Scan(&n); err != nil || n == 0 {
			return nil, false, domain.ErrInvalidReference
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE obligation_rules SET status='PUBLISHED', published_at=NOW(), published_by=$2
		WHERE obligation_rule_id::text=$1 AND status='DRAFT'`, id, actor); err != nil {
		return nil, false, s.registryFail("PublishObligationRule", err, nil)
	}
	out, err := s.GetObligationRule(ctx, id)
	return out, true, err
}

// gatherCalendarObligationModules reads the Wave 4 modules a manifest names
// into the compiler input, and adds the sources and interpretations they cite
// to the sets the caller loads afterwards.
func (s *PgStore) gatherCalendarObligationModules(ctx context.Context, q querier, m domain.PackManifest, in *domain.CompileInput, sourceIDs, interpIDs map[string]bool) error {
	for _, id := range m.CalendarModules {
		rec, err := s.loadCalendarVersion(ctx, q, `v.calendar_version_id::text=$1`, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				in.MissingCalendarIDs = append(in.MissingCalendarIDs, id)
				continue
			}
			return s.registryFail("compile calendar", err, nil)
		}
		var jur string
		if err := q.QueryRow(ctx, `SELECT c.jurisdiction_id::text FROM regulatory_calendar_versions v JOIN regulatory_calendars c USING (calendar_id)
			WHERE v.calendar_version_id::text=$1`, id).Scan(&jur); err != nil {
			return s.registryFail("compile calendar jurisdiction", err, nil)
		}
		from, _ := time.Parse("2006-01-02", rec.EffectiveFrom)
		in.Calendars = append(in.Calendars, domain.CompileCalendar{
			CalendarVersion: domain.CalendarVersion{CalendarVersionID: rec.CalendarVersionID, CalendarCode: rec.CalendarCode, Version: rec.Version,
				EffectiveFrom: from, Timezone: rec.Timezone, WeekendDays: rec.WeekendDays, CutoffTime: rec.CutoffTime, Holidays: rec.Holidays, SourceIDs: rec.SourceIDs},
			JurisdictionID: jur, Published: rec.Status == "PUBLISHED"})
		for _, sid := range rec.SourceIDs {
			sourceIDs[sid] = true
		}
	}
	for _, id := range m.ObligationModules {
		rec, err := s.loadObligation(ctx, q, `o.obligation_rule_id::text=$1`, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				in.MissingObligationIDs = append(in.MissingObligationIDs, id)
				continue
			}
			return s.registryFail("compile obligation", err, nil)
		}
		from, _ := time.Parse("2006-01-02", rec.EffectiveFrom)
		r := domain.ObligationRule{ObligationRuleID: rec.ObligationRuleID, JurisdictionID: rec.JurisdictionID, ObligationCode: rec.ObligationCode,
			RuleVersion: rec.RuleVersion, Name: rec.Name, PeriodBasis: rec.PeriodBasis, Anchor: rec.Anchor, OffsetMonths: rec.OffsetMonths,
			OffsetDays: rec.OffsetDays, OffsetToMonthEnd: rec.OffsetToMonthEnd, BusinessDayAdjustment: rec.BusinessDayAdjustment, EffectiveFrom: from,
			ExtensionAllowed: rec.ExtensionAllowed, MaxExtensionDays: rec.MaxExtensionDays, ExtensionRequiresEvidence: rec.ExtensionRequiresEvidence,
			CutoffApplies: rec.CutoffApplies, EscalationOwner: rec.EscalationOwner, EscalationSLAHours: rec.EscalationSLAHours,
			RegimeID: rec.RegimeID, InterpretationID: rec.InterpretationID, SourceIDs: rec.SourceIDs}
		if rec.CalendarCode != nil {
			r.CalendarCode = *rec.CalendarCode
		}
		if rec.EffectiveTo != nil {
			if to, err := time.Parse("2006-01-02", *rec.EffectiveTo); err == nil {
				r.EffectiveTo = &to
			}
		}
		in.Obligations = append(in.Obligations, domain.CompileObligation{ObligationRule: r, Published: rec.Status == "PUBLISHED"})
		for _, sid := range rec.SourceIDs {
			sourceIDs[sid] = true
		}
		if rec.InterpretationID != nil {
			interpIDs[*rec.InterpretationID] = true
		}
	}
	return nil
}
