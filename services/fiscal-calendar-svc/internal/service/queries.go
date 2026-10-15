package service

import (
	"context"

	"zoiko.io/fiscal-calendar-svc/internal/domain"
)

// CalendarView is GetFiscalCalendar / GetCalendarAsOf: the calendar header and
// the version in force on the requested date (null when none is).
type CalendarView struct {
	Calendar *domain.FiscalCalendar        `json:"calendar"`
	AsOf     domain.Date                   `json:"as_of"`
	Version  *domain.FiscalCalendarVersion `json:"version"`
}

// PeriodsPreview is the REF-04 -> REF-05 contract (see openapi.yaml).
type PeriodsPreview struct {
	CalendarID    string          `json:"calendar_id"`
	VersionID     string          `json:"version_id"`
	VersionNo     int             `json:"version_no"`
	LegalEntityID string          `json:"legal_entity_id"`
	FiscalYear    int             `json:"fiscal_year"`
	Periods       []domain.Period `json:"periods"`
}

// Resolution is the answer to a calendar resolve.
type Resolution struct {
	CalendarID    string       `json:"calendar_id"`
	VersionID     string       `json:"version_id"`
	VersionNo     int          `json:"version_no"`
	LegalEntityID string       `json:"legal_entity_id"`
	Scope         string       `json:"scope"`
	Date          domain.Date  `json:"date"`
	EffectiveFrom domain.Date  `json:"effective_from"`
	EffectiveTo   *domain.Date `json:"effective_to"`
}

// versionInForceAt picks the ACTIVE/SUPERSEDED version whose effective interval
// contains the date. Intervals of in-force versions never overlap for one
// (entity, scope), so at most one matches; more than one would be corruption
// and is reported rather than guessed.
func versionInForceAt(vs []domain.FiscalCalendarVersion, d domain.Date) (*domain.FiscalCalendarVersion, error) {
	var found *domain.FiscalCalendarVersion
	for i := range vs {
		v := &vs[i]
		if !v.Status.InForce() || !v.Interval().Contains(d) {
			continue
		}
		if found != nil {
			return nil, domain.Errf(domain.CodeRuleAmbiguous, "more than one calendar version is in force on %s (%s and %s)", d, found.VersionID, v.VersionID)
		}
		cp := *v
		found = &cp
	}
	return found, nil
}

// GetCalendar returns the calendar and the version in force on asOf. When asOf
// is nil the version in force today (UTC) is returned, or null. When asOf is
// given and no version covers it, NOT_FOUND is returned: history is never guessed.
func (s *Service) GetCalendar(ctx context.Context, tenantID, id string, asOf *domain.Date) (*CalendarView, error) {
	if tenantID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "tenant context is required")
	}
	var out *CalendarView
	err := s.store.InTx(ctx, tenantID, func(tx Tx) error {
		cal, err := tx.GetCalendarByID(ctx, id, false)
		if err != nil {
			return err
		}
		if cal == nil {
			return domain.Errf(domain.CodeNotFound, "fiscal calendar %s not found", id)
		}
		vs, err := tx.ListVersions(ctx, cal.CalendarID)
		if err != nil {
			return err
		}
		d := domain.FromTime(s.now())
		if asOf != nil {
			d = *asOf
		}
		v, err := versionInForceAt(vs, d)
		if err != nil {
			return err
		}
		if v == nil && asOf != nil {
			return domain.Errf(domain.CodeNotFound, "no version of calendar %s was in force on %s", id, d)
		}
		out = &CalendarView{Calendar: cal, AsOf: d, Version: v}
		return nil
	})
	return out, err
}

// ListVersions returns every version of a calendar, historical ones included.
func (s *Service) ListVersions(ctx context.Context, tenantID, calendarID string) ([]domain.FiscalCalendarVersion, error) {
	if tenantID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "tenant context is required")
	}
	var out []domain.FiscalCalendarVersion
	err := s.store.InTx(ctx, tenantID, func(tx Tx) error {
		cal, err := tx.GetCalendarByID(ctx, calendarID, false)
		if err != nil {
			return err
		}
		if cal == nil {
			return domain.Errf(domain.CodeNotFound, "fiscal calendar %s not found", calendarID)
		}
		out, err = tx.ListVersions(ctx, calendarID)
		return err
	})
	return out, err
}

// GetVersion returns one version.
func (s *Service) GetVersion(ctx context.Context, tenantID, versionID string) (*domain.FiscalCalendarVersion, error) {
	if tenantID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "tenant context is required")
	}
	var out *domain.FiscalCalendarVersion
	err := s.store.InTx(ctx, tenantID, func(tx Tx) error {
		v, err := tx.GetVersionByID(ctx, versionID, false)
		if err != nil {
			return err
		}
		if v == nil {
			return domain.Errf(domain.CodeNotFound, "fiscal calendar version %s not found", versionID)
		}
		out = v
		return nil
	})
	return out, err
}

// GetPlan returns one transition plan.
func (s *Service) GetPlan(ctx context.Context, tenantID, planID string) (*domain.CalendarTransitionPlan, error) {
	if tenantID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "tenant context is required")
	}
	var out *domain.CalendarTransitionPlan
	err := s.store.InTx(ctx, tenantID, func(tx Tx) error {
		p, err := tx.GetPlanByID(ctx, planID, false)
		if err != nil {
			return err
		}
		if p == nil {
			return domain.Errf(domain.CodeNotFound, "calendar transition plan %s not found", planID)
		}
		out = p
		return nil
	})
	return out, err
}

// PreviewPeriods is the deterministic period preview of a version. It is only
// available once the version is APPROVED (or later): a DRAFT can still change
// its mind, and consumers must not pin to it.
func (s *Service) PreviewPeriods(ctx context.Context, tenantID, versionID string, fiscalYear int) (*PeriodsPreview, error) {
	v, err := s.GetVersion(ctx, tenantID, versionID)
	if err != nil {
		return nil, err
	}
	if v.Status == domain.VersionDraft {
		return nil, domain.Errf(domain.CodeInvalidTransition, "periods can be previewed only for APPROVED, ACTIVE or SUPERSEDED versions; version %s is DRAFT", versionID)
	}
	ps, err := domain.PreviewPeriods(v, fiscalYear)
	if err != nil {
		return nil, err
	}
	return &PeriodsPreview{
		CalendarID: v.CalendarID, VersionID: v.VersionID, VersionNo: v.VersionNo, LegalEntityID: v.LegalEntityID,
		FiscalYear: fiscalYear, Periods: ps,
	}, nil
}

// Resolve finds the calendar version in force for (legal entity, scope, date).
// No match is NOT_FOUND: there is no fallback to a nearest, default or latest
// calendar.
func (s *Service) Resolve(ctx context.Context, tenantID, legalEntityID, scope string, date domain.Date) (*Resolution, error) {
	if tenantID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "tenant context is required")
	}
	if legalEntityID == "" || scope == "" || date.IsZero() {
		return nil, domain.Errf(domain.CodeContextInvalid, "legal_entity_id, scope and date are required")
	}
	var out *Resolution
	err := s.store.InTx(ctx, tenantID, func(tx Tx) error {
		vs, err := tx.ListInForceVersions(ctx, legalEntityID, scope)
		if err != nil {
			return err
		}
		v, err := versionInForceAt(vs, date)
		if err != nil {
			return err
		}
		if v == nil {
			return domain.Errf(domain.CodeNotFound, "no fiscal calendar version is in force for this legal entity and scope on %s", date)
		}
		out = &Resolution{
			CalendarID: v.CalendarID, VersionID: v.VersionID, VersionNo: v.VersionNo, LegalEntityID: v.LegalEntityID,
			Scope: v.Scope, Date: date, EffectiveFrom: v.EffectiveFrom, EffectiveTo: v.EffectiveTo,
		}
		return nil
	})
	return out, err
}
