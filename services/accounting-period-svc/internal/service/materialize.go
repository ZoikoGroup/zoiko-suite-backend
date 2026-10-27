package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/events"
)

// MaterializeInput is MaterializePeriods.
type MaterializeInput struct {
	Meta
	LegalEntityID string
	CalendarID    string
	FiscalYear    int
	BookScope     string
	// CalendarVersionID pins the fiscal-calendar version. When empty the version
	// is resolved through REF-04 at ResolveDate.
	CalendarVersionID string
	// ResolveDate (YYYY-MM-DD) is the date used for version resolution when
	// CalendarVersionID is empty. Default: <fiscal_year>-01-01. The resolved
	// calendar_id must equal CalendarID and the preview's fiscal_year must equal
	// FiscalYear, so a wrong guess fails rather than materialising wrong periods.
	ResolveDate string
}

// MaterializeResult is what a materialisation did. Re-running returns the
// existing periods and creates nothing.
type MaterializeResult struct {
	CalendarID        string `json:"calendar_id"`
	CalendarVersionID string `json:"calendar_version_id"`
	FiscalYear        int    `json:"fiscal_year"`
	BookScope         string `json:"book_scope"`
	Created           int    `json:"created"`
	Existing          int    `json:"existing"`
	// BoundaryDrift lists period_keys that already exist with boundaries that
	// differ from what the calendar version previews now. The existing period is
	// NEVER changed; this is a signal for operators.
	BoundaryDrift []string        `json:"boundary_drift"`
	Periods       []domain.Period `json:"periods"`
}

func validDate(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	return t, err == nil
}

// validatePreview checks the REF-04 preview before anything is stored. Contiguity
// is asserted over NORMAL periods only (SPECIAL periods such as an adjustment
// period may legitimately overlap the last normal period).
func validatePreview(in MaterializeInput, versionID string, pv *CalendarPreview) error {
	if pv.CalendarID != in.CalendarID {
		return domain.Errf(domain.CodeContextInvalid, "calendar version %s belongs to calendar %q, not %q", versionID, pv.CalendarID, in.CalendarID)
	}
	if pv.VersionID != "" && pv.VersionID != versionID {
		return domain.Errf(domain.CodeSourceUnverified, "calendar service answered for version %q, not %q", pv.VersionID, versionID)
	}
	if pv.FiscalYear != in.FiscalYear {
		return domain.Errf(domain.CodeContextInvalid, "calendar preview is for fiscal year %d, not %d", pv.FiscalYear, in.FiscalYear)
	}
	if pv.LegalEntityID != "" && pv.LegalEntityID != in.LegalEntityID {
		return domain.Errf(domain.CodeContextInvalid, "calendar preview is for legal entity %q, not %q", pv.LegalEntityID, in.LegalEntityID)
	}
	if len(pv.Periods) == 0 {
		return domain.Errf(domain.CodeContextInvalid, "calendar version %s defines no periods for fiscal year %d", versionID, in.FiscalYear)
	}
	if len(pv.Periods) > MaxPeriodsPerMaterialize {
		return domain.Errf(domain.CodeContextInvalid, "calendar preview has %d periods; the limit is %d", len(pv.Periods), MaxPeriodsPerMaterialize)
	}
	keys, nos := map[string]bool{}, map[int]bool{}
	var normals []CalendarPeriod
	for _, p := range pv.Periods {
		if p.PeriodKey == "" || !domain.Kind(p.Kind).Valid() {
			return domain.Errf(domain.CodeContextInvalid, "calendar preview has a period with an empty key or unknown kind %q", p.Kind)
		}
		if keys[p.PeriodKey] || nos[p.PeriodNo] {
			return domain.Errf(domain.CodeContextInvalid, "calendar preview repeats period_key %q or period_no %d", p.PeriodKey, p.PeriodNo)
		}
		keys[p.PeriodKey], nos[p.PeriodNo] = true, true
		st, ok1 := validDate(p.StartDate)
		en, ok2 := validDate(p.EndDate)
		if !ok1 || !ok2 || en.Before(st) {
			return domain.Errf(domain.CodeContextInvalid, "period %q has invalid dates %q..%q", p.PeriodKey, p.StartDate, p.EndDate)
		}
		if domain.Kind(p.Kind) == domain.KindNormal {
			normals = append(normals, p)
		}
	}
	sort.Slice(normals, func(i, j int) bool { return normals[i].StartDate < normals[j].StartDate })
	for i := 1; i < len(normals); i++ {
		prevEnd, _ := validDate(normals[i-1].EndDate)
		if normals[i].StartDate != prevEnd.AddDate(0, 0, 1).Format("2006-01-02") {
			return domain.Errf(domain.CodeContextInvalid,
				"normal periods %q and %q are not contiguous (gap or overlap between %s and %s)",
				normals[i-1].PeriodKey, normals[i].PeriodKey, normals[i-1].EndDate, normals[i].StartDate)
		}
	}
	return nil
}

// MaterializePeriods creates the OPEN period instances of one fiscal year of
// one calendar version for one legal entity and book scope. It is idempotent
// per (entity, calendar_version, fiscal_year, book_scope): a re-run returns the
// existing periods, never duplicates and never moves a boundary. A different
// calendar version yields SEPARATE periods; the old ones are untouched.
func (s *Service) MaterializePeriods(ctx context.Context, in MaterializeInput) (*MaterializeResult, bool, error) {
	if err := in.Meta.check(); err != nil {
		return nil, false, err
	}
	if in.LegalEntityID == "" || in.CalendarID == "" {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "legal_entity_id and calendar_id are required")
	}
	if in.FiscalYear < 1900 || in.FiscalYear > 2200 {
		return nil, false, domain.Errf(domain.CodeContextInvalid, "fiscal_year must be between 1900 and 2200")
	}
	if in.ResolveDate != "" {
		if _, ok := validDate(in.ResolveDate); !ok {
			return nil, false, domain.Errf(domain.CodeContextInvalid, "resolve_date must be YYYY-MM-DD")
		}
	}
	op := "materialize:" + in.LegalEntityID + ":" + in.CalendarID + ":" + strconv.Itoa(in.FiscalYear) + ":" + in.BookScope + ":" + in.CalendarVersionID

	if prev, err := peekReplay[MaterializeResult](ctx, s.store, in.Meta, op); err != nil || prev != nil {
		return prev, prev != nil, err
	}

	versionID := in.CalendarVersionID
	if versionID == "" {
		date := in.ResolveDate
		if date == "" {
			date = fmt.Sprintf("%04d-01-01", in.FiscalYear)
		}
		ref, err := s.calendar.Resolve(ctx, in.TenantID, in.LegalEntityID, in.BookScope, date)
		if err != nil {
			return nil, false, err
		}
		if ref.CalendarID != in.CalendarID {
			return nil, false, domain.Errf(domain.CodeContextInvalid, "calendar %q resolved for the entity at %s, not the requested %q", ref.CalendarID, date, in.CalendarID)
		}
		versionID = ref.VersionID
	}
	pv, err := s.calendar.PeriodsPreview(ctx, in.TenantID, versionID, in.FiscalYear)
	if err != nil {
		return nil, false, err
	}
	if err := validatePreview(in, versionID, pv); err != nil {
		return nil, false, err
	}

	var res *MaterializeResult
	var replayed bool
	err = s.store.InTx(ctx, in.TenantID, func(tx Tx) error {
		out, rep, err := idempotent(ctx, tx, in.Meta, op, func() (*MaterializeResult, error) {
			return s.doMaterialize(ctx, tx, in, versionID, pv)
		})
		if err != nil {
			return err
		}
		res, replayed = out, rep
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return res, replayed, nil
}

func (s *Service) doMaterialize(ctx context.Context, tx Tx, in MaterializeInput, versionID string, pv *CalendarPreview) (*MaterializeResult, error) {
	// Two concurrent materialisations of the same year serialise here; the
	// loser then finds the winner's periods and creates nothing.
	if err := tx.LockKey(ctx, "materialize:"+in.TenantID+":"+in.LegalEntityID+":"+versionID+":"+strconv.Itoa(in.FiscalYear)+":"+in.BookScope); err != nil {
		return nil, err
	}
	res := &MaterializeResult{CalendarID: in.CalendarID, CalendarVersionID: versionID, FiscalYear: in.FiscalYear,
		BookScope: in.BookScope, BoundaryDrift: []string{}, Periods: []domain.Period{}}
	now := s.now()
	periods := append([]CalendarPeriod(nil), pv.Periods...)
	sort.Slice(periods, func(i, j int) bool { return periods[i].PeriodNo < periods[j].PeriodNo })
	for _, cp := range periods {
		existing, err := tx.FindPeriodByKey(ctx, in.LegalEntityID, versionID, in.BookScope, "", cp.PeriodKey)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			res.Existing++
			if existing.StartDate != cp.StartDate || existing.EndDate != cp.EndDate {
				res.BoundaryDrift = append(res.BoundaryDrift, cp.PeriodKey)
			}
			res.Periods = append(res.Periods, *existing)
			continue
		}
		p := domain.Period{
			PeriodID: s.newID(), TenantID: in.TenantID, LegalEntityID: in.LegalEntityID,
			CalendarID: in.CalendarID, CalendarVersionID: versionID, BookScope: in.BookScope, ModuleScope: "",
			PeriodKey: cp.PeriodKey, FiscalYear: in.FiscalYear, PeriodNo: cp.PeriodNo,
			StartDate: cp.StartDate, EndDate: cp.EndDate, Kind: domain.Kind(cp.Kind),
			State: domain.StateOpen, Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.InsertPeriod(ctx, &p); err != nil {
			return nil, err
		}
		if err := tx.InsertHistory(ctx, &domain.HistoryEntry{
			HistoryID: s.newID(), TenantID: in.TenantID, PeriodID: p.PeriodID, FromState: "", ToState: domain.StateOpen,
			Command: domain.CmdMaterialize, RequestedBy: in.Actor, Reason: in.Reason, RecordedAt: now,
			ExpectedVersion: 0, ResultingVersion: 1, CorrelationID: in.CorrelationID,
		}); err != nil {
			return nil, err
		}
		if err := s.enqueue(ctx, tx, events.Event{
			Type: events.EventPeriodOpened, TenantID: in.TenantID, Scope: events.ScopeTenant,
			ObjectType: events.ObjectTypeAccountingPeriod, ObjectID: p.PeriodID, ObjectVersion: p.Version,
			EffectiveAt: now, RecordedAt: now, Actor: in.Actor, CorrelationID: in.CorrelationID, CausationID: in.CausationID,
			Data: periodEventData(&p),
		}); err != nil {
			return nil, err
		}
		res.Created++
		res.Periods = append(res.Periods, p)
	}
	return res, nil
}

func periodEventData(p *domain.Period) map[string]any {
	return map[string]any{
		"legal_entity_id": p.LegalEntityID, "calendar_id": p.CalendarID, "calendar_version_id": p.CalendarVersionID,
		"book_scope": p.BookScope, "module_scope": p.ModuleScope, "period_key": p.PeriodKey,
		"fiscal_year": p.FiscalYear, "period_no": p.PeriodNo, "start_date": p.StartDate, "end_date": p.EndDate,
		"kind": string(p.Kind), "state": string(p.State),
	}
}
