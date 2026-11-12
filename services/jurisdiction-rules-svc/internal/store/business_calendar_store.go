package store

// REF-10 Business Calendar (migration 000030): the import of a holiday list, the
// lookup of the calendar version that governs a date, and what a version changes.
// Calendars, versions and the publish maker-checker are those of calendar_store.go.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// Domain is the record as the calendar logic reads it.
func (r *CalendarVersionRecord) Domain() domain.CalendarVersion {
	from, _ := time.Parse("2006-01-02", r.EffectiveFrom)
	return domain.CalendarVersion{
		CalendarVersionID: r.CalendarVersionID, CalendarCode: r.CalendarCode, Version: r.Version, EffectiveFrom: from,
		Timezone: r.Timezone, WeekendDays: r.WeekendDays, CutoffTime: r.CutoffTime, Holidays: r.Holidays, SourceIDs: r.SourceIDs,
	}
}

// Provenance is what a business-day answer says about the version that gave it.
func (r *CalendarVersionRecord) Provenance(pinned bool) domain.CalendarProvenance {
	return domain.CalendarProvenance{
		CalendarCode: r.CalendarCode, CalendarVersion: r.Version, CalendarVersionID: r.CalendarVersionID, ContentDigest: r.ContentDigest,
		Timezone: r.Timezone, EffectiveFrom: r.EffectiveFrom, SourceAuthority: r.SourceAuthority, SourceVersion: r.SourceVersion,
		ImportHash: r.ImportHash, Pinned: pinned,
	}
}

// insertHolidays writes a draft version's holidays, working each one's observed
// date out from its rule and the calendar's weekend here, so the stored calendar
// never depends on the caller's arithmetic.
func (s *PgStore) insertHolidays(ctx context.Context, q querier, versionID string, holidays []domain.Holiday, weekend []int) error {
	withObserved, err := domain.WithObservedDates(holidays, weekend)
	if err != nil {
		return err
	}
	for _, h := range withObserved {
		var observed *string
		if h.ObservedDate != "" {
			observed = &h.ObservedDate
		}
		if _, err := q.Exec(ctx, `INSERT INTO regulatory_holidays (calendar_version_id, holiday_date, name, observed_rule, observed_date)
			VALUES ($1::uuid, $2::date, $3, $4, $5::date)`, versionID, h.Date, h.Name, h.ObservedRule, observed); err != nil {
			return s.registryFail("insert holiday", err, nil)
		}
	}
	return nil
}

// ImportHolidayCalendar is ImportHolidayCalendar (REF-10): a holiday list from an
// authority becomes the next DRAFT version of a calendar, with the evidence of
// where it came from. It returns the version, and true when this call created it.
//
// One source version is imported once per calendar. The same edition with the
// same content again is a replay and returns the first version; the same edition
// with different content is ErrCalendarImportConflict, never a second version.
// The version is published afterwards, by someone else, like any other.
func (s *PgStore) ImportHolidayCalendar(ctx context.Context, p domain.ImportCalendarParams) (*CalendarVersionRecord, bool, error) {
	if err := p.Validate(); err != nil {
		return nil, false, err
	}
	hash := domain.CalendarImportHash(p)
	sourceVersion := strings.TrimSpace(p.SourceVersion)
	tx, err := s.begin(ctx, "ImportHolidayCalendar")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var calID string
	if err := tx.QueryRow(ctx, `SELECT calendar_id::text FROM regulatory_calendars WHERE calendar_code=$1 FOR UPDATE`, p.CalendarCode).Scan(&calID); err != nil {
		return nil, false, s.registryFail("ImportHolidayCalendar lock", err, domain.ErrCalendarNotFound)
	}

	var existingHash string
	err = tx.QueryRow(ctx, `SELECT import_hash FROM regulatory_calendar_versions WHERE calendar_id=$1::uuid AND source_version=$2`,
		calID, sourceVersion).Scan(&existingHash)
	switch {
	case err == nil:
		if existingHash != hash {
			return nil, false, domain.ErrCalendarImportConflict
		}
		rec, err := s.loadCalendarVersion(ctx, tx, `c.calendar_code=$1 AND v.source_version=$2`, p.CalendarCode, sourceVersion)
		if err != nil {
			return nil, false, s.registryFail("ImportHolidayCalendar replay", err, nil)
		}
		return rec, false, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, false, s.registryFail("ImportHolidayCalendar lookup", err, nil)
	}

	if len(p.SourceIDs) > 0 {
		if n, err := s.liveSourceCount(ctx, tx, p.SourceIDs); err != nil || n != len(uniq(p.SourceIDs)) {
			return nil, false, domain.ErrInvalidReference
		}
	}
	wk := make([]int32, len(p.WeekendDays))
	for i, w := range p.WeekendDays {
		wk[i] = int32(w)
	}
	var vid string
	if err := tx.QueryRow(ctx, `
		INSERT INTO regulatory_calendar_versions
			(calendar_id, version, effective_from, timezone, weekend_days, cutoff_time, created_by_principal_id,
			 source_authority, source_reference, source_version, import_hash)
		VALUES ($1::uuid, COALESCE((SELECT MAX(version) FROM regulatory_calendar_versions WHERE calendar_id=$1::uuid),0)+1,
		        $2::date, $3, $4::smallint[], $5::time, $6, $7, $8, $9, $10)
		RETURNING calendar_version_id::text`,
		calID, p.EffectiveFrom, p.Timezone, wk, p.CutoffTime, p.Actor,
		strings.TrimSpace(p.SourceAuthority), strings.TrimSpace(p.SourceReference), sourceVersion, hash).Scan(&vid); err != nil {
		return nil, false, s.registryFail("ImportHolidayCalendar insert", err, nil)
	}
	if err := s.insertHolidays(ctx, tx, vid, p.Holidays, p.WeekendDays); err != nil {
		return nil, false, err
	}
	for _, sid := range uniq(p.SourceIDs) {
		if _, err := tx.Exec(ctx, `INSERT INTO calendar_version_sources VALUES ($1::uuid,$2::uuid)`, vid, sid); err != nil {
			return nil, false, s.registryFail("ImportHolidayCalendar link source", err, nil)
		}
	}
	rec, err := s.loadCalendarVersion(ctx, tx, `v.calendar_version_id::text=$1`, vid)
	if err != nil {
		return nil, false, s.registryFail("ImportHolidayCalendar read", err, nil)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, s.registryFail("ImportHolidayCalendar commit", err, nil)
	}
	return rec, true, nil
}

// ResolveCalendarVersion is the published calendar version that governs a date.
//
// Only PUBLISHED versions govern: a draft never answers. Without a pin it is the
// highest version effective on or before the date AND already published at
// knownAt (default now), so a correction published later never changes what a
// past date was answered with when asked as it was known then. With a pin it is
// that version, whatever its effective date, so a calculation can be reproduced
// exactly. ErrNoCalendarVersion says nothing governs. The bool is whether the
// version was pinned.
func (s *PgStore) ResolveCalendarVersion(ctx context.Context, code, on string, knownAt *time.Time, pin *int) (*CalendarVersionRecord, bool, error) {
	if _, err := s.GetCalendar(ctx, code); err != nil {
		return nil, false, err
	}
	known := time.Now().UTC()
	if knownAt != nil {
		known = *knownAt
	}
	var id string
	var err error
	if pin != nil {
		err = s.db(ctx).QueryRow(ctx, `
			SELECT v.calendar_version_id::text FROM regulatory_calendar_versions v JOIN regulatory_calendars c USING (calendar_id)
			WHERE c.calendar_code=$1 AND v.version=$2 AND v.status='PUBLISHED' AND v.published_at <= $3`, code, *pin, known).Scan(&id)
	} else {
		err = s.db(ctx).QueryRow(ctx, `
			SELECT v.calendar_version_id::text FROM regulatory_calendar_versions v JOIN regulatory_calendars c USING (calendar_id)
			WHERE c.calendar_code=$1 AND v.status='PUBLISHED' AND v.effective_from <= $2::date AND v.published_at <= $3
			ORDER BY v.version DESC LIMIT 1`, code, on, known).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, domain.ErrNoCalendarVersion
	}
	if err != nil {
		return nil, false, s.registryFail("ResolveCalendarVersion", err, nil)
	}
	rec, err := s.loadCalendarVersion(ctx, s.db(ctx), `v.calendar_version_id::text=$1`, id)
	if err != nil {
		return nil, false, s.registryFail("ResolveCalendarVersion read", err, nil)
	}
	return rec, pin != nil, nil
}

// CalendarHolidayChanges is what a published calendar version changes relative
// to the published version before it (all of its holidays, for the first).
func (s *PgStore) CalendarHolidayChanges(ctx context.Context, code string, version int) ([]domain.HolidayChange, error) {
	cur, err := s.GetCalendarVersion(ctx, code, version)
	if err != nil {
		return nil, err
	}
	var prev []domain.Holiday
	var prevID string
	err = s.db(ctx).QueryRow(ctx, `
		SELECT v.calendar_version_id::text FROM regulatory_calendar_versions v JOIN regulatory_calendars c USING (calendar_id)
		WHERE c.calendar_code=$1 AND v.version < $2 AND v.status='PUBLISHED' ORDER BY v.version DESC LIMIT 1`, code, version).Scan(&prevID)
	switch {
	case err == nil:
		rec, err := s.loadCalendarVersion(ctx, s.db(ctx), `v.calendar_version_id::text=$1`, prevID)
		if err != nil {
			return nil, s.registryFail("CalendarHolidayChanges read", err, nil)
		}
		prev = rec.Holidays
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, s.registryFail("CalendarHolidayChanges previous", err, nil)
	}
	return domain.DiffHolidays(prev, cur.Holidays), nil
}
