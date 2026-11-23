package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// REF-01 aliases and code resolution against a real Postgres: a code means one
// jurisdiction at a time (enforced by the database), history is kept, and
// resolution never guesses.

func aliasParams(jurisdictionID, system, code string, from time.Time) domain.AddAliasParams {
	return domain.AddAliasParams{
		JurisdictionID: jurisdictionID, AliasSystem: system, AliasCode: code,
		SourceStandard: "ISO 3166-1", SourceVersion: "2024-06", MappingRationale: "assigned by the standard",
		ValidFrom: from, ActorID: "steward-1",
	}
}

func mustAddAlias(t *testing.T, ctx context.Context, s *store.PgStore, p domain.AddAliasParams) *domain.JurisdictionAlias {
	t.Helper()
	a, created, _, err := s.AddJurisdictionAliasWithQuerier(ctx, s.Pool(), p)
	if err != nil || !created {
		t.Fatalf("add alias %s/%s: created=%v err=%v", p.AliasSystem, p.AliasCode, created, err)
	}
	return a
}

func now() time.Time { return time.Now().UTC() }

func TestAlias_ResolvesAnExternalCodeToTheStableJurisdiction(t *testing.T) {
	s, _, ctx := newTestStore(t)
	gb := mustCreateJurisdiction(t, s, ctx, "GB", nil)
	mustAddAlias(t, ctx, s, aliasParams(gb.JurisdictionID, domain.SystemISO3166Alpha2, "GB", epoch))
	mustAddAlias(t, ctx, s, aliasParams(gb.JurisdictionID, domain.SystemISO3166Alpha3, "GBR", epoch))
	mustAddAlias(t, ctx, s, aliasParams(gb.JurisdictionID, domain.SystemISO3166Numeric, "826", epoch))

	for _, c := range []struct{ system, code string }{
		{domain.SystemISO3166Alpha2, "GB"}, {domain.SystemISO3166Alpha3, "GBR"}, {domain.SystemISO3166Numeric, "826"},
		{domain.SystemISO3166Alpha2, "gb"}, {domain.SystemISO3166Alpha2, "  Gb "}, // case and spacing are not meaning
	} {
		got, err := s.ResolveCode(ctx, domain.ResolveQuery{System: c.system, Code: c.code, AsOf: now()})
		if err != nil {
			t.Fatalf("%s %q: %v", c.system, c.code, err)
		}
		if got.JurisdictionID != gb.JurisdictionID || got.MatchedVia != c.system || got.Alias == nil || got.Alias.SourceStandard != "ISO 3166-1" {
			t.Errorf("%s %q resolved to %+v", c.system, c.code, got)
		}
	}
}

// Never a near match: the same characters in another system, a prefix, a
// different jurisdiction's code.
func TestAlias_NeverGuesses(t *testing.T) {
	s, _, ctx := newTestStore(t)
	gb := mustCreateJurisdiction(t, s, ctx, "GB", nil)
	mustAddAlias(t, ctx, s, aliasParams(gb.JurisdictionID, domain.SystemISO3166Alpha2, "GB", epoch))
	mustAddAlias(t, ctx, s, aliasParams(gb.JurisdictionID, domain.SystemISO3166Alpha3, "GBR", epoch))

	for name, q := range map[string]domain.ResolveQuery{
		"alpha-2 code asked in the alpha-3 system": {System: domain.SystemISO3166Alpha3, Code: "GB"},
		"a prefix of a real code":                  {System: domain.SystemISO3166Alpha3, Code: "GB"},
		"a longer code":                            {System: domain.SystemISO3166Alpha2, Code: "GBR"},
		"a code in a system that has none":         {System: "SOME-ERP", Code: "GB"},
		"a similar-looking code":                   {System: domain.SystemISO3166Alpha2, Code: "G8"},
		"an alias asked as an internal code":       {Code: "GBR"},
	} {
		q.AsOf = now()
		if _, err := s.ResolveCode(ctx, q); !errors.Is(err, domain.ErrUnknownJurisdictionCode) {
			t.Errorf("%s: %v, want ErrUnknownJurisdictionCode", name, err)
		}
	}
}

func TestAlias_NotMappedAtThatTimeIsDifferentFromUnknown(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := epochJurisdiction(t, ctx, s, "NM")
	a := mustAddAlias(t, ctx, s, aliasParams(j.JurisdictionID, "SOME-ERP", "NM-1", daysAfter(100)))
	if _, _, _, err := s.EndJurisdictionAliasWithQuerier(ctx, s.Pool(), domain.EndAliasParams{
		AliasID: a.AliasID, JurisdictionID: j.JurisdictionID, ValidTo: daysAfter(200), ActorID: "x", Reason: "system retired the code"}); err != nil {
		t.Fatal(err)
	}

	_, err := s.ResolveCode(ctx, domain.ResolveQuery{System: "SOME-ERP", Code: "NM-1", AsOf: daysAfter(10)})
	var ne *domain.AliasNotEffectiveError
	if !errors.As(err, &ne) || !errors.Is(err, domain.ErrAliasNotEffective) || !strings.Contains(err.Error(), "not yet mapped") {
		t.Errorf("before the mapping: %v, want not-yet-mapped", err)
	}
	_, err = s.ResolveCode(ctx, domain.ResolveQuery{System: "SOME-ERP", Code: "NM-1", AsOf: daysAfter(300)})
	if !errors.Is(err, domain.ErrAliasNotEffective) || !strings.Contains(err.Error(), "no longer mapped") {
		t.Errorf("after the mapping ended: %v, want no-longer-mapped", err)
	}
	got, err := s.ResolveCode(ctx, domain.ResolveQuery{System: "SOME-ERP", Code: "NM-1", AsOf: daysAfter(150)})
	if err != nil || got.JurisdictionID != j.JurisdictionID {
		t.Errorf("while mapped: %v %v", got, err)
	}
}

func TestAlias_ARetiredJurisdictionIsRetiredNotUnknown_AndStillResolvesForItsPast(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	old := mustCreateJurisdiction(t, s, ctx, "OLD", nil)
	succ := mustCreateJurisdiction(t, s, ctx, "NEW", nil)
	mustAddAlias(t, ctx, s, aliasParams(old.JurisdictionID, "SOME-ERP", "OLD-1", epoch))
	before := now().Add(-time.Minute)
	if _, _, err := s.RetireJurisdictionWithQuerier(ctx, pool, domain.RetireJurisdictionParams{
		JurisdictionID: old.JurisdictionID, ActorID: "a", Reason: "dissolved", EvidenceRef: "ev-1", SupersededBy: &succ.JurisdictionID}); err != nil {
		t.Fatal(err)
	}

	// Today: explicit, with where to go instead.
	_, err := s.ResolveCode(ctx, domain.ResolveQuery{System: "SOME-ERP", Code: "OLD-1", AsOf: now().Add(time.Hour)})
	var re *domain.RetiredError
	if !errors.As(err, &re) || !errors.Is(err, domain.ErrJurisdictionRetired) {
		t.Fatalf("retired code: %v, want a RetiredError", err)
	}
	if re.SupersededBy == nil || *re.SupersededBy != succ.JurisdictionID || re.Reason == nil || *re.Reason != "dissolved" || re.RetiredAt == nil {
		t.Errorf("retirement detail = %+v", re)
	}
	// The same through the internal code.
	_, err = s.ResolveCode(ctx, domain.ResolveQuery{Code: "OLD", AsOf: now().Add(time.Hour)})
	if !errors.Is(err, domain.ErrJurisdictionRetired) {
		t.Errorf("retired internal code: %v", err)
	}
	// As of when it was in force, it resolves: history is not rewritten.
	got, err := s.ResolveCode(ctx, domain.ResolveQuery{System: "SOME-ERP", Code: "OLD-1", AsOf: before})
	if err != nil || got.JurisdictionID != old.JurisdictionID {
		t.Errorf("as of before retirement: %v %v", got, err)
	}
}

func TestAlias_ACodeMeansOneJurisdictionAtATime(t *testing.T) {
	s, _, ctx := newTestStore(t)
	a := epochJurisdiction(t, ctx, s, "AA")
	b := epochJurisdiction(t, ctx, s, "BB")
	first := mustAddAlias(t, ctx, s, aliasParams(a.JurisdictionID, "SOME-ERP", "X1", daysAfter(0)))

	// Overlapping, from open-ended: refused.
	if _, _, _, err := s.AddJurisdictionAliasWithQuerier(ctx, s.Pool(), aliasParams(b.JurisdictionID, "SOME-ERP", "x1", daysAfter(50))); !errors.Is(err, domain.ErrAliasConflict) {
		t.Fatalf("overlapping mapping of the same code (different case): %v, want ErrAliasConflict", err)
	}
	// Ending the first frees the code from then on, and the new mapping is exact.
	if _, _, _, err := s.EndJurisdictionAliasWithQuerier(ctx, s.Pool(), domain.EndAliasParams{
		AliasID: first.AliasID, JurisdictionID: a.JurisdictionID, ValidTo: daysAfter(100), ActorID: "x", Reason: "reassigned"}); err != nil {
		t.Fatal(err)
	}
	mustAddAlias(t, ctx, s, aliasParams(b.JurisdictionID, "SOME-ERP", "X1", daysAfter(100)))

	for day, want := range map[int]string{50: a.JurisdictionID, 99: a.JurisdictionID, 100: b.JurisdictionID, 500: b.JurisdictionID} {
		got, err := s.ResolveCode(ctx, domain.ResolveQuery{System: "SOME-ERP", Code: "X1", AsOf: daysAfter(day)})
		if err != nil || got.JurisdictionID != want {
			t.Errorf("day %d: %v %v, want %s", day, got, err, want)
		}
	}
}

// Two amendments of the same code racing: the database lets exactly one win.
func TestAlias_ConcurrentAmendmentsOfOneCodeHaveOneWinner(t *testing.T) {
	s, _, ctx := newTestStore(t)
	var ids []string
	for i := 0; i < 6; i++ {
		ids = append(ids, mustCreateJurisdiction(t, s, ctx, "RC"+string(rune('A'+i)), nil).JurisdictionID)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, errs[i] = s.AddJurisdictionAliasWithQuerier(ctx, s.Pool(), aliasParams(id, "SOME-ERP", "RACE", epoch))
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, domain.ErrAliasConflict):
			t.Errorf("a loser failed with %v, want ErrAliasConflict", err)
		}
	}
	if wins != 1 {
		t.Errorf("%d concurrent amendments succeeded, want exactly 1", wins)
	}
}

func TestAlias_ReplayIsIdempotentButDifferentEvidenceIsAConflict(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "RP", nil)
	p := aliasParams(j.JurisdictionID, "SOME-ERP", "RP-1", epoch)
	first := mustAddAlias(t, ctx, s, p)
	before := j.RowVersion

	again, created, _, err := s.AddJurisdictionAliasWithQuerier(ctx, pool, p)
	if err != nil || created || again.AliasID != first.AliasID {
		t.Fatalf("replay: created=%v err=%v id=%v, want the original and not created", created, err, again)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM jurisdiction_aliases WHERE jurisdiction_id = $1`, j.JurisdictionID).Scan(&n)
	if n != 1 {
		t.Errorf("%d rows after a replay, want 1", n)
	}
	cur, _ := s.FindByID(ctx, j.JurisdictionID)
	if cur.RowVersion != before+1 {
		t.Errorf("row_version %d, want %d: the add bumped it once and the replay not at all", cur.RowVersion, before+1)
	}

	p.MappingRationale = "a different story"
	if _, _, _, err := s.AddJurisdictionAliasWithQuerier(ctx, pool, p); !errors.Is(err, domain.ErrAliasConflict) {
		t.Errorf("same mapping on different evidence: %v, want ErrAliasConflict", err)
	}
}

func TestAlias_Validation(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "VL", nil)
	retired := mustCreateJurisdiction(t, s, ctx, "VR", nil)
	if _, _, err := s.RetireJurisdictionWithQuerier(ctx, pool, domain.RetireJurisdictionParams{JurisdictionID: retired.JurisdictionID, ActorID: "a"}); err != nil {
		t.Fatal(err)
	}
	ok := aliasParams(j.JurisdictionID, domain.SystemISO3166Alpha2, "VL", epoch)
	mutate := func(f func(*domain.AddAliasParams)) domain.AddAliasParams { p := ok; f(&p); return p }
	to := epoch.Add(-time.Hour)

	for name, p := range map[string]domain.AddAliasParams{
		"reserved system":       mutate(func(p *domain.AddAliasParams) { p.AliasSystem = domain.SystemInternal }),
		"lower-case system":     mutate(func(p *domain.AddAliasParams) { p.AliasSystem = "some-erp" }),
		"bad alpha-2":           mutate(func(p *domain.AddAliasParams) { p.AliasCode = "G1" }),
		"bad alpha-3":           mutate(func(p *domain.AddAliasParams) { p.AliasSystem, p.AliasCode = domain.SystemISO3166Alpha3, "GB" }),
		"bad numeric":           mutate(func(p *domain.AddAliasParams) { p.AliasSystem, p.AliasCode = domain.SystemISO3166Numeric, "82" }),
		"bad subdivision":       mutate(func(p *domain.AddAliasParams) { p.AliasSystem, p.AliasCode = domain.SystemISO3166Sub, "GB" }),
		"empty code":            mutate(func(p *domain.AddAliasParams) { p.AliasCode = "  " }),
		"hostile external code": mutate(func(p *domain.AddAliasParams) { p.AliasSystem, p.AliasCode = "SOME-ERP", "x';--" }),
		"no source standard":    mutate(func(p *domain.AddAliasParams) { p.SourceStandard = "" }),
		"no source version":     mutate(func(p *domain.AddAliasParams) { p.SourceVersion = " " }),
		"no mapping rationale":  mutate(func(p *domain.AddAliasParams) { p.MappingRationale = "" }),
		"no valid_from":         mutate(func(p *domain.AddAliasParams) { p.ValidFrom = time.Time{} }),
		"valid_to before from":  mutate(func(p *domain.AddAliasParams) { p.ValidTo = &to }),
		"over-long standard":    mutate(func(p *domain.AddAliasParams) { p.SourceStandard = strings.Repeat("x", 129) }),
	} {
		if _, _, _, err := s.AddJurisdictionAliasWithQuerier(ctx, pool, p); !errors.Is(err, domain.ErrInvalidAlias) {
			t.Errorf("%s: %v, want ErrInvalidAlias", name, err)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM jurisdiction_aliases`).Scan(&n)
	if n != 0 {
		t.Errorf("%d aliases were stored by refused requests", n)
	}

	if _, _, _, err := s.AddJurisdictionAliasWithQuerier(ctx, pool, aliasParams(uuid.New().String(), "SOME-ERP", "Z", epoch)); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("unknown jurisdiction: %v", err)
	}
	if _, _, _, err := s.AddJurisdictionAliasWithQuerier(ctx, pool, aliasParams(retired.JurisdictionID, "SOME-ERP", "Z", epoch)); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("a retired jurisdiction was given a new code: %v", err)
	}
}

func TestAlias_ReplacingAMappingEndsTheOldOneAtomically(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	a := mustCreateJurisdiction(t, s, ctx, "RA", nil)
	b := mustCreateJurisdiction(t, s, ctx, "RB", nil)
	old := mustAddAlias(t, ctx, s, aliasParams(a.JurisdictionID, "SOME-ERP", "MV", daysAfter(0)))

	p := aliasParams(b.JurisdictionID, "SOME-ERP", "MV", daysAfter(100))
	p.ReplacesAliasID = old.AliasID
	moved := mustAddAlias(t, ctx, s, p)
	if moved.JurisdictionID != b.JurisdictionID {
		t.Fatalf("moved = %+v", moved)
	}
	aliases, _ := s.ListJurisdictionAliases(ctx, a.JurisdictionID, nil)
	if len(aliases) != 1 || aliases[0].ValidTo == nil || !aliases[0].ValidTo.Equal(daysAfter(100)) ||
		aliases[0].EndedBy == nil || *aliases[0].EndedBy != "steward-1" || aliases[0].EndReason == nil {
		t.Errorf("the replaced mapping = %+v, want ended at day 100 with who and why", aliases)
	}

	// If the new mapping cannot be recorded, the old one must not have been ended.
	c := mustCreateJurisdiction(t, s, ctx, "RC", nil)
	mustAddAlias(t, ctx, s, aliasParams(c.JurisdictionID, "SOME-ERP", "KEEP", daysAfter(0)))
	d := mustCreateJurisdiction(t, s, ctx, "RD", nil)
	mustAddAlias(t, ctx, s, aliasParams(d.JurisdictionID, "SOME-ERP", "KEEP-2", daysAfter(0)))
	keep, _ := s.ListJurisdictionAliases(ctx, c.JurisdictionID, nil)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bad := aliasParams(d.JurisdictionID, "SOME-ERP", "KEEP", daysAfter(50))
	bad.ReplacesAliasID = keep[0].AliasID
	bad.SourceStandard = "" // refused after validation of the rest: nothing may be left half done
	if _, _, _, err := s.AddJurisdictionAliasWithQuerier(ctx, tx, bad); err == nil {
		t.Fatal("expected a refusal")
	}
	_ = tx.Rollback(ctx)
	after, _ := s.ListJurisdictionAliases(ctx, c.JurisdictionID, nil)
	if after[0].ValidTo != nil || after[0].EndedAt != nil {
		t.Errorf("a refused replacement ended the old mapping: %+v", after[0])
	}
}

func TestAlias_ReplacementRefusals(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	a := mustCreateJurisdiction(t, s, ctx, "FA", nil)
	other := mustAddAlias(t, ctx, s, aliasParams(a.JurisdictionID, "SOME-ERP", "OTHER", daysAfter(0)))
	target := mustAddAlias(t, ctx, s, aliasParams(a.JurisdictionID, "SOME-ERP", "TGT", daysAfter(0)))

	try := func(name string, mutate func(*domain.AddAliasParams), want error) {
		p := aliasParams(a.JurisdictionID, "SOME-ERP", "TGT", daysAfter(100))
		p.ReplacesAliasID = target.AliasID
		mutate(&p)
		if _, _, _, err := s.AddJurisdictionAliasWithQuerier(ctx, pool, p); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
	try("a different code", func(p *domain.AddAliasParams) { p.ReplacesAliasID = other.AliasID }, domain.ErrInvalidAlias)
	try("not after the old one starts", func(p *domain.AddAliasParams) { p.ValidFrom = daysAfter(0) }, domain.ErrInvalidAlias)
	try("an unknown alias", func(p *domain.AddAliasParams) { p.ReplacesAliasID = uuid.New().String() }, domain.ErrAliasNotFound)
	try("a malformed alias id", func(p *domain.AddAliasParams) { p.ReplacesAliasID = "not-a-uuid" }, domain.ErrAliasNotFound)
}

func TestAlias_EndRecordsWhoAndWhyAndOnlyBringsAnEndForward(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "EN", nil)
	a := mustAddAlias(t, ctx, s, aliasParams(j.JurisdictionID, "SOME-ERP", "EN-1", daysAfter(0)))
	end := func(id, jur string, to time.Time, reason string) (*domain.JurisdictionAlias, bool, error) {
		x, changed, _, err := s.EndJurisdictionAliasWithQuerier(ctx, pool, domain.EndAliasParams{AliasID: id, JurisdictionID: jur, ValidTo: to, ActorID: "ender", Reason: reason})
		return x, changed, err
	}

	if _, _, err := end(a.AliasID, j.JurisdictionID, daysAfter(100), ""); !errors.Is(err, domain.ErrInvalidAlias) {
		t.Errorf("no reason: %v", err)
	}
	if _, _, err := end(a.AliasID, j.JurisdictionID, daysAfter(0), "r"); !errors.Is(err, domain.ErrInvalidAlias) {
		t.Errorf("end not after the start: %v", err)
	}
	if _, _, err := end(a.AliasID, uuid.New().String(), daysAfter(100), "r"); !errors.Is(err, domain.ErrAliasNotFound) {
		t.Errorf("another jurisdiction's id: %v", err)
	}
	ended, changed, err := end(a.AliasID, j.JurisdictionID, daysAfter(100), "code withdrawn")
	if err != nil || !changed || ended.EndedBy == nil || *ended.EndedBy != "ender" || *ended.EndReason != "code withdrawn" || ended.EndedAt == nil {
		t.Fatalf("end: %+v changed=%v err=%v", ended, changed, err)
	}
	again, changed, err := end(a.AliasID, j.JurisdictionID, daysAfter(50), "second thoughts")
	if err != nil || changed || *again.EndReason != "code withdrawn" || !again.ValidTo.Equal(daysAfter(100)) {
		t.Errorf("a replay must keep the first end: %+v changed=%v err=%v", again, changed, err)
	}

	// A bounded mapping can be cut short but not stretched.
	b := aliasParams(j.JurisdictionID, "SOME-ERP", "EN-2", daysAfter(0))
	until := daysAfter(200)
	b.ValidTo = &until
	bounded := mustAddAlias(t, ctx, s, b)
	if _, _, err := end(bounded.AliasID, j.JurisdictionID, daysAfter(300), "r"); !errors.Is(err, domain.ErrInvalidAlias) {
		t.Errorf("extending a bounded mapping: %v, want refused", err)
	}
	if _, changed, err := end(bounded.AliasID, j.JurisdictionID, daysAfter(150), "cut short"); err != nil || !changed {
		t.Errorf("shortening: changed=%v err=%v", changed, err)
	}
}

func TestAlias_ListShowsHistoryOrWhatWasInForce(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "LS", nil)
	old := mustAddAlias(t, ctx, s, aliasParams(j.JurisdictionID, "SOME-ERP", "L1", daysAfter(0)))
	if _, _, _, err := s.EndJurisdictionAliasWithQuerier(ctx, s.Pool(), domain.EndAliasParams{AliasID: old.AliasID, JurisdictionID: j.JurisdictionID, ValidTo: daysAfter(100), ActorID: "x", Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	mustAddAlias(t, ctx, s, aliasParams(j.JurisdictionID, "SOME-ERP", "L2", daysAfter(100)))

	all, err := s.ListJurisdictionAliases(ctx, j.JurisdictionID, nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("history: %d (%v), want both", len(all), err)
	}
	for day, want := range map[int]string{50: "L1", 150: "L2"} {
		at := daysAfter(day)
		got, _ := s.ListJurisdictionAliases(ctx, j.JurisdictionID, &at)
		if len(got) != 1 || got[0].AliasCode != want {
			t.Errorf("day %d: %+v, want only %s", day, got, want)
		}
	}
	none := daysAfter(-5)
	if got, _ := s.ListJurisdictionAliases(ctx, j.JurisdictionID, &none); got == nil || len(got) != 0 {
		t.Errorf("before any mapping: %#v, want an empty non-nil list", got)
	}
	if _, err := s.ListJurisdictionAliases(ctx, uuid.New().String(), nil); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("unknown jurisdiction: %v", err)
	}
}

func TestResolve_InternalCodeAmbiguityIsReportedNotChosen(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	country := seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)
	authority := seedJurisdiction(t, ctx, pool, "GB", "TAX_AUTHORITY", &country, epoch)
	unique := seedJurisdiction(t, ctx, pool, "FR", "COUNTRY", nil, epoch)

	_, err := s.ResolveCode(ctx, domain.ResolveQuery{Code: "gb", AsOf: now()})
	var amb *domain.AmbiguousError
	if !errors.As(err, &amb) || !errors.Is(err, domain.ErrAmbiguousJurisdictionCode) || len(amb.Candidates) != 2 {
		t.Fatalf("shared code: %v, want an AmbiguousError with both candidates", err)
	}
	// The caller, not the service, narrows it.
	got, err := s.ResolveCode(ctx, domain.ResolveQuery{Code: "GB", Type: "TAX_AUTHORITY", AsOf: now()})
	if err != nil || got.JurisdictionID != authority {
		t.Errorf("narrowed by type: %v %v, want the tax authority", got, err)
	}
	got, err = s.ResolveCode(ctx, domain.ResolveQuery{Code: "FR", AsOf: now()})
	if err != nil || got.JurisdictionID != unique || got.MatchedVia != domain.SystemInternal || got.Alias != nil {
		t.Errorf("a unique internal code: %v %v", got, err)
	}
	if _, err := s.ResolveCode(ctx, domain.ResolveQuery{Code: "ZZ", AsOf: now()}); !errors.Is(err, domain.ErrUnknownJurisdictionCode) {
		t.Errorf("unknown internal code: %v", err)
	}
	if _, err := s.ResolveCode(ctx, domain.ResolveQuery{Code: " ", AsOf: now()}); !errors.Is(err, domain.ErrInvalidAlias) {
		t.Errorf("empty code: %v", err)
	}
}

// With two jurisdictions sharing a code, only one still in force, the live one
// answers: ambiguity is about what is in force at the instant.
func TestResolve_AnInternalCodeSharedWithARetiredJurisdictionResolvesToTheLiveOne(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	dead := seedJurisdiction(t, ctx, pool, "XX", "COUNTRY", nil, epoch)
	live := seedJurisdiction(t, ctx, pool, "XX", "TAX_AUTHORITY", nil, epoch)
	if _, _, err := s.RetireJurisdictionWithQuerier(ctx, pool, domain.RetireJurisdictionParams{JurisdictionID: dead, ActorID: "a"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveCode(ctx, domain.ResolveQuery{Code: "XX", AsOf: now().Add(time.Hour)})
	if err != nil || got.JurisdictionID != live {
		t.Errorf("%v %v, want the live jurisdiction", got, err)
	}
}

func TestAlias_TheDatabaseEnforcesTheRulesToo(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "DB", nil)
	other := mustCreateJurisdiction(t, s, ctx, "DC", nil)
	a := mustAddAlias(t, ctx, s, aliasParams(j.JurisdictionID, "SOME-ERP", "DB-1", epoch))
	if _, _, _, err := s.EndJurisdictionAliasWithQuerier(ctx, pool, domain.EndAliasParams{AliasID: a.AliasID, JurisdictionID: j.JurisdictionID, ValidTo: daysAfter(10), ActorID: "x", Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	live := mustAddAlias(t, ctx, s, aliasParams(j.JurisdictionID, "SOME-ERP", "DB-2", epoch))

	for name, sql := range map[string]string{
		"re-point a mapping":    `UPDATE jurisdiction_aliases SET jurisdiction_id = $2 WHERE alias_id = $1`,
		"rewrite the code":      `UPDATE jurisdiction_aliases SET alias_code = 'HACK' WHERE alias_id = $1`,
		"rewrite the rationale": `UPDATE jurisdiction_aliases SET mapping_rationale = 'edited' WHERE alias_id = $1`,
		"backdate the start":    `UPDATE jurisdiction_aliases SET valid_from = valid_from - interval '1 day' WHERE alias_id = $1`,
		"re-end an ended one":   `UPDATE jurisdiction_aliases SET valid_to = valid_to + interval '1 day' WHERE alias_id = $1`,
		"delete a mapping":      `DELETE FROM jurisdiction_aliases WHERE alias_id = $1`,
	} {
		id := live.AliasID
		if strings.Contains(name, "re-end") {
			id = a.AliasID
		}
		var err error
		if strings.Contains(sql, "$2") {
			_, err = pool.Exec(ctx, sql, id, other.JurisdictionID)
		} else {
			_, err = pool.Exec(ctx, sql, id)
		}
		if err == nil {
			t.Errorf("%s: the database allowed it", name)
		}
	}
	if _, err := pool.Exec(ctx, `TRUNCATE jurisdiction_aliases`); err == nil {
		t.Error("TRUNCATE of the mappings was allowed")
	}
	// Straight to the table, bypassing the store: the code still means one thing at a time,
	// even spelled with different case and spacing.
	if _, err := pool.Exec(ctx, `
		INSERT INTO jurisdiction_aliases (jurisdiction_id, alias_system, alias_code, source_standard, source_version, mapping_rationale, valid_from, created_by)
		VALUES ($1, 'SOME-ERP', '  db-2 ', 's', 'v', 'r', now(), 'x')`, other.JurisdictionID); err == nil {
		t.Error("a second meaning of the same code was inserted directly")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jurisdiction_aliases (jurisdiction_id, alias_system, alias_code, source_standard, source_version, mapping_rationale, valid_from, created_by)
		VALUES ($1, 'SOME-ERP', 'NOPE', ' ', 'v', 'r', now(), 'x')`, other.JurisdictionID); err == nil {
		t.Error("a mapping with no source standard was inserted directly")
	}
}

func TestMigration000028_DownIsRefusedWhileMappingsExist(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "MG", nil)
	down, up := migration(t, "000028_jurisdiction_aliases.down.sql"), migration(t, "000028_jurisdiction_aliases.up.sql")
	mustAddAlias(t, ctx, s, aliasParams(j.JurisdictionID, "SOME-ERP", "MG-1", epoch))

	if _, err := pool.Exec(ctx, down); err == nil || !strings.Contains(err.Error(), "000028 down refused") {
		t.Fatalf("down with mappings present: %v, want it refused", err)
	}
	// The up migration is re-runnable.
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatalf("re-running up: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE jurisdiction_aliases DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM jurisdiction_aliases`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatalf("down with nothing to lose: %v", err)
	}
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// epochJurisdiction is a jurisdiction in force since 2020-01-01, for tests that
// resolve codes on dates in that range.
func epochJurisdiction(t *testing.T, ctx context.Context, s *store.PgStore, code string) *domain.Jurisdiction {
	t.Helper()
	id := seedJurisdiction(t, ctx, s.Pool().(*pgxpool.Pool), code, "COUNTRY", nil, epoch)
	j, err := s.FindByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
