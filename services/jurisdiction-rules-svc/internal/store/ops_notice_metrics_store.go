package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 Wave 7: source-change intake (s29 "source-change watch", s31)
// and the operations metrics (s29).

// SourceChangeNotice is a signal that an authority's material may have
// changed. It opens a controlled review; it never edits or publishes a rule.
type SourceChangeNotice struct {
	NoticeID               string     `json:"notice_id"`
	JurisdictionID         *string    `json:"jurisdiction_id"`
	Authority              string     `json:"authority"`
	ChangeType             string     `json:"change_type"`
	Title                  string     `json:"title"`
	Location               *string    `json:"location"`
	ObservedHash           *string    `json:"observed_hash"`
	AffectedSourceID       *string    `json:"affected_source_id"`
	Detail                 *string    `json:"detail"`
	Status                 string     `json:"status"`
	CreatedAt              time.Time  `json:"created_at"`
	CreatedBy              string     `json:"created_by_principal_id"`
	Reviewer               *string    `json:"reviewer"`
	ReviewStartedAt        *time.Time `json:"review_started_at"`
	ClosedAt               *time.Time `json:"closed_at"`
	ClosedBy               *string    `json:"closed_by"`
	Outcome                *string    `json:"outcome"`
	OutcomeNote            *string    `json:"outcome_note"`
	LinkedInterpretationID *string    `json:"linked_interpretation_id"`
}

type CreateNoticeParams struct {
	JurisdictionID, AffectedSourceID *string
	Authority, ChangeType, Title     string
	Location, ObservedHash, Detail   *string
	CreatedBy                        string
}

const noticeCols = `notice_id::text, jurisdiction_id::text, authority, change_type, title, location, observed_hash, affected_source_id::text,
	detail, status, created_at, created_by_principal_id, reviewer, review_started_at, closed_at, closed_by, outcome, outcome_note,
	linked_interpretation_id::text`

func scanNotice(r pgx.Row) (*SourceChangeNotice, error) {
	var n SourceChangeNotice
	err := r.Scan(&n.NoticeID, &n.JurisdictionID, &n.Authority, &n.ChangeType, &n.Title, &n.Location, &n.ObservedHash, &n.AffectedSourceID,
		&n.Detail, &n.Status, &n.CreatedAt, &n.CreatedBy, &n.Reviewer, &n.ReviewStartedAt, &n.ClosedAt, &n.ClosedBy, &n.Outcome,
		&n.OutcomeNote, &n.LinkedInterpretationID)
	return &n, err
}

func (s *PgStore) CreateNotice(ctx context.Context, p CreateNoticeParams) (*SourceChangeNotice, error) {
	n, err := scanNotice(s.pool.QueryRow(ctx, `INSERT INTO source_change_notices (jurisdiction_id, authority, change_type, title, location,
			observed_hash, affected_source_id, detail, created_by_principal_id) VALUES ($1::uuid,$2,$3,$4,$5,$6,$7::uuid,$8,$9)
		RETURNING `+noticeCols, p.JurisdictionID, p.Authority, p.ChangeType, p.Title, p.Location, p.ObservedHash, p.AffectedSourceID, p.Detail, p.CreatedBy))
	if err != nil {
		return nil, s.registryFail("CreateNotice", err, nil)
	}
	return n, nil
}

func (s *PgStore) GetNotice(ctx context.Context, id string) (*SourceChangeNotice, error) {
	n, err := scanNotice(s.pool.QueryRow(ctx, `SELECT `+noticeCols+` FROM source_change_notices WHERE notice_id::text=$1`, id))
	if err != nil {
		return nil, s.registryFail("GetNotice", err, domain.ErrNoticeNotFound)
	}
	return n, nil
}

func (s *PgStore) ListNotices(ctx context.Context, status string, limit, offset int) ([]*SourceChangeNotice, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+noticeCols+` FROM source_change_notices WHERE ($1='' OR status=$1)
		ORDER BY created_at DESC, notice_id LIMIT $2 OFFSET $3`, status, clampLimit(limit, 50, 200), offset)
	if err != nil {
		return nil, s.registryFail("ListNotices", err, nil)
	}
	defer rows.Close()
	out := []*SourceChangeNotice{}
	for rows.Next() {
		n, err := scanNotice(rows)
		if err != nil {
			return nil, s.registryFail("ListNotices scan", err, nil)
		}
		out = append(out, n)
	}
	return out, s.registryFail("ListNotices rows", rows.Err(), nil)
}

// StartNoticeReview assigns an independent reviewer (not the intake author).
func (s *PgStore) StartNoticeReview(ctx context.Context, id, reviewer string) (*SourceChangeNotice, bool, error) {
	cur, err := s.GetNotice(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if cur.CreatedBy == reviewer {
		return nil, false, domain.ErrNotIndependent
	}
	if cur.Status == "UNDER_REVIEW" && cur.Reviewer != nil && *cur.Reviewer == reviewer {
		return cur, false, nil
	}
	if cur.Status != "OPEN" {
		return nil, false, domain.ErrAlreadyDecided
	}
	n, err := scanNotice(s.pool.QueryRow(ctx, `UPDATE source_change_notices SET status='UNDER_REVIEW', reviewer=$2, review_started_at=NOW()
		WHERE notice_id::text=$1 AND status='OPEN' RETURNING `+noticeCols, id, reviewer))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, domain.ErrAlreadyDecided
		}
		return nil, false, s.registryFail("StartNoticeReview", err, nil)
	}
	return n, true, nil
}

// CloseNotice records the review's conclusion. Only the assigned reviewer may
// close, and a conclusion of INTERPRETATION_RECORDED must name the approved
// interpretation it produced. Closing never touches a rule or a pack.
func (s *PgStore) CloseNotice(ctx context.Context, id, closer, outcome, note string, linkedInterpretation *string) (*SourceChangeNotice, bool, error) {
	cur, err := s.GetNotice(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if cur.Status == "CLOSED" {
		if cur.ClosedBy != nil && *cur.ClosedBy == closer && cur.Outcome != nil && *cur.Outcome == outcome {
			return cur, false, nil
		}
		return nil, false, domain.ErrAlreadyDecided
	}
	if cur.Status != "UNDER_REVIEW" || cur.Reviewer == nil || *cur.Reviewer != closer {
		return nil, false, domain.ErrNotAssignedReviewer
	}
	if outcome == "INTERPRETATION_RECORDED" {
		if linkedInterpretation == nil {
			return nil, false, domain.ErrInvalidReference
		}
		var st string
		if err := s.pool.QueryRow(ctx, `SELECT status FROM interpretation_records WHERE interpretation_id::text=$1`, *linkedInterpretation).Scan(&st); err != nil || st != "APPROVED" {
			return nil, false, domain.ErrUnapprovedInterpretation
		}
	}
	n, err := scanNotice(s.pool.QueryRow(ctx, `UPDATE source_change_notices SET status='CLOSED', closed_at=NOW(), closed_by=$2, outcome=$3,
			outcome_note=$4, linked_interpretation_id=$5::uuid WHERE notice_id::text=$1 AND status='UNDER_REVIEW' RETURNING `+noticeCols,
		id, closer, outcome, note, linkedInterpretation))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, domain.ErrAlreadyDecided
		}
		return nil, false, s.registryFail("CloseNotice", err, nil)
	}
	return n, true, nil
}

// ── operations metrics (s29) ────────────────────────────────────────────────

type MetricPackVersion struct {
	PackRef string `json:"pack_ref"`
	Version string `json:"version"`
	Count   int    `json:"count"`
}

type SkewEntry struct {
	PackRef          string            `json:"pack_ref"`
	RingCode         string            `json:"ring_code"`
	VersionsByRegion map[string]string `json:"newest_active_version_by_region"`
	Skewed           bool              `json:"skewed"`
}

type CertAgeEntry struct {
	PackRef     string    `json:"pack_ref"`
	Version     string    `json:"version"`
	CertifiedAt time.Time `json:"certified_at"`
	AgeDays     int       `json:"age_days"`
	Stale       bool      `json:"stale"`
}

type UpcomingEntry struct {
	PackRef       string `json:"pack_ref"`
	Version       string `json:"version"`
	Status        string `json:"status"`
	EffectiveFrom string `json:"effective_from"`
}

type RetroOverdue struct {
	PackRef string    `json:"pack_ref"`
	Version string    `json:"version"`
	DueAt   time.Time `json:"retro_due_at"`
}

// OpsMetrics is the regulatory operations feed (s29). Nothing here is a
// dashboard; it is the data a dashboard or an alert would read.
type OpsMetrics struct {
	GeneratedAt time.Time `json:"generated_at"`
	WindowHours int       `json:"window_hours"`

	ResolutionsByOutcome  map[string]int      `json:"resolutions_by_outcome"`
	ResolutionsByVersion  []MetricPackVersion `json:"resolved_by_pack_version"`
	VerificationFailures  []MetricPackVersion `json:"verification_failures_by_pack_version"`
	UpcomingEffective     []UpcomingEntry     `json:"upcoming_effective_changes"`
	DeploymentSkew        []SkewEntry         `json:"deployment_skew"`
	ReleasedNeverDeployed []MetricPackVersion `json:"released_but_not_deployed"`
	CertificationAge      []CertAgeEntry      `json:"certification_age"`
	VersionsByStatus      map[string]int      `json:"pack_versions_by_status"`
	OpenSourceChanges     int                 `json:"source_changes_open"`
	SourceChangesInReview int                 `json:"source_changes_under_review"`
	OldestOpenChangeHours *float64            `json:"oldest_unclosed_source_change_hours"`
	HotfixRetrosOverdue   []RetroOverdue      `json:"hotfix_retrospectives_overdue"`
	NotMeasured           []string            `json:"not_measured"`
}

// Metrics computes the operations feed from authoritative rows.
func (s *PgStore) Metrics(ctx context.Context, windowHours, certAgeWarnDays, upcomingDays int) (*OpsMetrics, error) {
	m := &OpsMetrics{WindowHours: windowHours, ResolutionsByOutcome: map[string]int{}, VersionsByStatus: map[string]int{},
		ResolutionsByVersion: []MetricPackVersion{}, VerificationFailures: []MetricPackVersion{}, UpcomingEffective: []UpcomingEntry{},
		DeploymentSkew: []SkewEntry{}, ReleasedNeverDeployed: []MetricPackVersion{}, CertificationAge: []CertAgeEntry{},
		HotfixRetrosOverdue: []RetroOverdue{},
		NotMeasured: []string{
			"historical replay drift (needs a replay job over stored decisions)",
			"e-invoice and filing rejection rates (no e-invoice or filing adapter exists)",
			"override and election usage (tenant overrides are not implemented)",
			"deadline calculation exceptions (no regulatory calendar exists)",
			"pack deployment skew between services (resolvers do not report the version they run); skew shown is between regions in the deployment registry",
		}}
	if err := s.pool.QueryRow(ctx, `SELECT NOW()`).Scan(&m.GeneratedAt); err != nil {
		return nil, s.registryFail("Metrics now", err, nil)
	}

	collect := func(query string, args []any, each func(pgx.Rows) error) error {
		rows, err := s.pool.Query(ctx, query, args...)
		if err != nil {
			return s.registryFail("Metrics query", err, nil)
		}
		defer rows.Close()
		for rows.Next() {
			if err := each(rows); err != nil {
				return s.registryFail("Metrics scan", err, nil)
			}
		}
		return s.registryFail("Metrics rows", rows.Err(), nil)
	}
	win := time.Duration(windowHours) * time.Hour

	if err := collect(`SELECT outcome, COUNT(*) FROM rule_decision_evidence WHERE created_at >= NOW() - make_interval(secs => $1) GROUP BY outcome`,
		[]any{win.Seconds()}, func(r pgx.Rows) error {
			var o string
			var n int
			if err := r.Scan(&o, &n); err != nil {
				return err
			}
			m.ResolutionsByOutcome[o] = n
			return nil
		}); err != nil {
		return nil, err
	}
	if err := collect(`SELECT pack_ref, pack_version, COUNT(*) FROM rule_decision_evidence WHERE outcome='RESOLVED' AND created_at >= NOW() - make_interval(secs => $1)
		GROUP BY pack_ref, pack_version ORDER BY pack_ref, pack_version`, []any{win.Seconds()}, func(r pgx.Rows) error {
		var x MetricPackVersion
		if err := r.Scan(&x.PackRef, &x.Version, &x.Count); err != nil {
			return err
		}
		m.ResolutionsByVersion = append(m.ResolutionsByVersion, x)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := collect(`SELECT p.pack_ref, v.version, COUNT(*) FROM pack_verification_failures f JOIN jurisdiction_pack_versions v USING (pack_version_id)
		JOIN jurisdiction_packs p USING (pack_id) WHERE f.detected_at >= NOW() - make_interval(secs => $1) GROUP BY p.pack_ref, v.version ORDER BY 1, 2`,
		[]any{win.Seconds()}, func(r pgx.Rows) error {
			var x MetricPackVersion
			if err := r.Scan(&x.PackRef, &x.Version, &x.Count); err != nil {
				return err
			}
			m.VerificationFailures = append(m.VerificationFailures, x)
			return nil
		}); err != nil {
		return nil, err
	}
	if err := collect(`SELECT p.pack_ref, v.version, v.status, to_char(v.effective_from,'YYYY-MM-DD') FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id)
		WHERE v.status IN ('CERTIFIED','RELEASED') AND v.effective_from > CURRENT_DATE AND v.effective_from <= CURRENT_DATE + $1::int ORDER BY v.effective_from, p.pack_ref`,
		[]any{upcomingDays}, func(r pgx.Rows) error {
			var x UpcomingEntry
			if err := r.Scan(&x.PackRef, &x.Version, &x.Status, &x.EffectiveFrom); err != nil {
				return err
			}
			m.UpcomingEffective = append(m.UpcomingEffective, x)
			return nil
		}); err != nil {
		return nil, err
	}

	// Skew: per pack and ring, the newest ACTIVE version in each region; skewed when regions disagree.
	type key struct{ pack, ring string }
	newest := map[key]map[string]string{}
	if err := collect(`SELECT p.pack_ref, d.ring_code, d.region_code, v.version FROM pack_deployments d JOIN jurisdiction_pack_versions v USING (pack_version_id)
		JOIN jurisdiction_packs p USING (pack_id) WHERE d.status='ACTIVE' AND v.status='RELEASED'`, nil, func(r pgx.Rows) error {
		var pack, ring, region, ver string
		if err := r.Scan(&pack, &ring, &region, &ver); err != nil {
			return err
		}
		k := key{pack, ring}
		if newest[k] == nil {
			newest[k] = map[string]string{}
		}
		if cur, ok := newest[k][region]; !ok || domain.CompareVersions(ver, cur) > 0 {
			newest[k][region] = ver
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for k, byRegion := range newest {
		e := SkewEntry{PackRef: k.pack, RingCode: k.ring, VersionsByRegion: byRegion}
		first := ""
		for _, v := range byRegion {
			if first == "" {
				first = v
			} else if v != first {
				e.Skewed = true
			}
		}
		m.DeploymentSkew = append(m.DeploymentSkew, e)
	}
	sortSkew(m.DeploymentSkew)

	if err := collect(`SELECT p.pack_ref, v.version FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id) WHERE v.status='RELEASED'
		AND NOT EXISTS (SELECT 1 FROM pack_deployments d WHERE d.pack_version_id=v.pack_version_id AND d.status='ACTIVE') ORDER BY 1, 2`, nil, func(r pgx.Rows) error {
		var x MetricPackVersion
		if err := r.Scan(&x.PackRef, &x.Version); err != nil {
			return err
		}
		m.ReleasedNeverDeployed = append(m.ReleasedNeverDeployed, x)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := collect(`SELECT p.pack_ref, v.version, c.certified_at, FLOOR(EXTRACT(EPOCH FROM (NOW() - c.certified_at)) / 86400)::int
		FROM pack_certifications c JOIN jurisdiction_pack_versions v USING (pack_version_id) JOIN jurisdiction_packs p USING (pack_id)
		WHERE v.status IN ('CERTIFIED','RELEASED','EMERGENCY_BLOCKED') ORDER BY c.certified_at, p.pack_ref`, nil, func(r pgx.Rows) error {
		var x CertAgeEntry
		if err := r.Scan(&x.PackRef, &x.Version, &x.CertifiedAt, &x.AgeDays); err != nil {
			return err
		}
		x.Stale = x.AgeDays >= certAgeWarnDays
		m.CertificationAge = append(m.CertificationAge, x)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := collect(`SELECT status, COUNT(*) FROM jurisdiction_pack_versions GROUP BY status`, nil, func(r pgx.Rows) error {
		var st string
		var n int
		if err := r.Scan(&st, &n); err != nil {
			return err
		}
		m.VersionsByStatus[st] = n
		return nil
	}); err != nil {
		return nil, err
	}
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE status='OPEN'), COUNT(*) FILTER (WHERE status='UNDER_REVIEW'),
			MAX(EXTRACT(EPOCH FROM (NOW() - created_at)) / 3600) FILTER (WHERE status <> 'CLOSED') FROM source_change_notices`).
		Scan(&m.OpenSourceChanges, &m.SourceChangesInReview, &m.OldestOpenChangeHours); err != nil {
		return nil, s.registryFail("Metrics notices", err, nil)
	}
	if err := collect(`SELECT p.pack_ref, v.version, h.retro_due_at FROM pack_hotfixes h JOIN jurisdiction_pack_versions v USING (pack_version_id)
		JOIN jurisdiction_packs p USING (pack_id) WHERE h.retro_completed_at IS NULL AND h.retro_due_at < NOW() ORDER BY h.retro_due_at`, nil, func(r pgx.Rows) error {
		var x RetroOverdue
		if err := r.Scan(&x.PackRef, &x.Version, &x.DueAt); err != nil {
			return err
		}
		m.HotfixRetrosOverdue = append(m.HotfixRetrosOverdue, x)
		return nil
	}); err != nil {
		return nil, err
	}
	return m, nil
}

func sortSkew(in []SkewEntry) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && (in[j].PackRef < in[j-1].PackRef || (in[j].PackRef == in[j-1].PackRef && in[j].RingCode < in[j-1].RingCode)); j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
