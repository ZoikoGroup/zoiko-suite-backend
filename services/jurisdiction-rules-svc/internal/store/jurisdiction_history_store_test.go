package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// REF-01 against a real Postgres: versions are kept by the database, the past
// can be read exactly, and a retired jurisdiction is still readable as of the
// time it was in force but refused for anything new.

var epoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func daysAfter(n int) time.Time { return epoch.AddDate(0, 0, n) }

// seedJurisdiction inserts a jurisdiction effective from `from`, returning its id.
func seedJurisdiction(t *testing.T, ctx context.Context, pool *pgxpool.Pool, code, jType string, parent *string, from time.Time) string {
	t.Helper()
	id := uuid.New().String()
	_, err := pool.Exec(ctx, `
		INSERT INTO jurisdictions (jurisdiction_id, jurisdiction_code, jurisdiction_name, jurisdiction_type,
		                           parent_jurisdiction_id, authority_type, effective_from, active_flag, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, 'FEDERAL', $6, true, 'admin')`, id, code, "Jurisdiction "+code, jType, parent, from)
	if err != nil {
		t.Fatalf("seed %s: %v", code, err)
	}
	return id
}

// moveParent re-parents a jurisdiction the way an import would, with the change
// taking effect at `at` (app.valid_from), in one transaction.
func moveParent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, newParent *string, at time.Time) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.valid_from', $1, true), set_config('app.reason', 'source reorganisation', true)", at.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE jurisdictions SET parent_jurisdiction_id = $2, updated_by_principal_id = 'importer' WHERE jurisdiction_id = $1`, id, newParent); err != nil {
		t.Fatalf("move: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

type versionRow struct {
	No         int
	Name       string
	Parent     *string
	From       time.Time
	To         *time.Time
	ChangeType string
	Reason     *string
}

func versionsOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) []versionRow {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT version_no, jurisdiction_name, parent_jurisdiction_id, valid_from, valid_to, change_type, reason
		  FROM jurisdiction_versions WHERE jurisdiction_id = $1 ORDER BY version_no`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []versionRow
	for rows.Next() {
		var v versionRow
		if err := rows.Scan(&v.No, &v.Name, &v.Parent, &v.From, &v.To, &v.ChangeType, &v.Reason); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func TestVersions_ACreatedJurisdictionHasItsFirstVersionOpen(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "VC1", nil)

	vs := versionsOf(t, ctx, pool, j.JurisdictionID)
	if len(vs) != 1 {
		t.Fatalf("%d versions, want 1", len(vs))
	}
	v := vs[0]
	if v.No != 1 || v.ChangeType != "ADDED" || v.To != nil || !v.From.Equal(j.EffectiveFrom) {
		t.Errorf("version = %+v, want an open ADDED version 1 from %v", v, j.EffectiveFrom)
	}
}

func TestVersions_AParentChangeClosesTheOldVersionAndOpensTheNext(t *testing.T) {
	_, pool, ctx := newTestStore(t)
	c1 := seedJurisdiction(t, ctx, pool, "C1", "COUNTRY", nil, epoch)
	c2 := seedJurisdiction(t, ctx, pool, "C2", "COUNTRY", nil, epoch)
	st := seedJurisdiction(t, ctx, pool, "ST", "STATE_PROVINCE", &c1, epoch)

	moveParent(t, ctx, pool, st, &c2, daysAfter(100))

	vs := versionsOf(t, ctx, pool, st)
	if len(vs) != 2 {
		t.Fatalf("%d versions, want 2", len(vs))
	}
	if vs[0].To == nil || !vs[0].To.Equal(daysAfter(100)) {
		t.Errorf("version 1 closed at %v, want %v", vs[0].To, daysAfter(100))
	}
	if vs[1].No != 2 || vs[1].ChangeType != "HIERARCHY_CHANGED" || vs[1].To != nil || !vs[1].From.Equal(daysAfter(100)) {
		t.Errorf("version 2 = %+v", vs[1])
	}
	if vs[1].Parent == nil || *vs[1].Parent != c2 || vs[0].Parent == nil || *vs[0].Parent != c1 {
		t.Errorf("parents: v1=%v v2=%v, want %s then %s", vs[0].Parent, vs[1].Parent, c1, c2)
	}
	if vs[1].Reason == nil || *vs[1].Reason != "source reorganisation" {
		t.Errorf("reason = %v, want the one given for the change", vs[1].Reason)
	}
}

func TestVersions_ARenameIsAnUpdateNotAHierarchyChange(t *testing.T) {
	_, pool, ctx := newTestStore(t)
	c := seedJurisdiction(t, ctx, pool, "RN", "COUNTRY", nil, epoch)
	if _, err := pool.Exec(ctx, `UPDATE jurisdictions SET jurisdiction_name = 'Renamed' WHERE jurisdiction_id = $1`, c); err != nil {
		t.Fatal(err)
	}
	vs := versionsOf(t, ctx, pool, c)
	if len(vs) != 2 || vs[1].ChangeType != "UPDATED" || vs[1].Name != "Renamed" || vs[0].Name != "Jurisdiction RN" {
		t.Fatalf("versions = %+v", vs)
	}
}

// A change that touches nothing the history records (an audit column) makes no version.
func TestVersions_AnUnrelatedUpdateMakesNoVersion(t *testing.T) {
	_, pool, ctx := newTestStore(t)
	c := seedJurisdiction(t, ctx, pool, "NV", "COUNTRY", nil, epoch)
	if _, err := pool.Exec(ctx, `UPDATE jurisdictions SET updated_by_principal_id = 'someone' WHERE jurisdiction_id = $1`, c); err != nil {
		t.Fatal(err)
	}
	if n := len(versionsOf(t, ctx, pool, c)); n != 1 {
		t.Fatalf("%d versions after an audit-only update, want 1", n)
	}
}

func TestAsOf_ReadsThePastExactlyAcrossAMove(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	c1 := seedJurisdiction(t, ctx, pool, "A1", "COUNTRY", nil, epoch)
	c2 := seedJurisdiction(t, ctx, pool, "A2", "COUNTRY", nil, epoch)
	st := seedJurisdiction(t, ctx, pool, "AS", "STATE_PROVINCE", &c1, epoch)
	moveParent(t, ctx, pool, st, &c2, daysAfter(100))

	before, err := s.GetJurisdictionAsOf(ctx, st, daysAfter(50))
	if err != nil {
		t.Fatal(err)
	}
	if before.ParentJurisdictionID == nil || *before.ParentJurisdictionID != c1 || before.VersionNo != 1 {
		t.Errorf("before the move: parent=%v version=%d, want %s v1", before.ParentJurisdictionID, before.VersionNo, c1)
	}
	after, err := s.GetJurisdictionAsOf(ctx, st, daysAfter(150))
	if err != nil {
		t.Fatal(err)
	}
	if after.ParentJurisdictionID == nil || *after.ParentJurisdictionID != c2 || after.VersionNo != 2 {
		t.Errorf("after the move: parent=%v version=%d, want %s v2", after.ParentJurisdictionID, after.VersionNo, c2)
	}
	at, err := s.GetJurisdictionAsOf(ctx, st, daysAfter(100)) // the half-open boundary: the new version owns the instant
	if err != nil || at.VersionNo != 2 {
		t.Errorf("at the move instant: version=%v err=%v, want 2", at, err)
	}
}

func TestAsOf_NotYetInForceAndUnknownAreDifferentAnswers(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := seedJurisdiction(t, ctx, pool, "NY", "COUNTRY", nil, daysAfter(100))

	_, err := s.GetJurisdictionAsOf(ctx, j, daysAfter(10))
	var ne *domain.NotEffectiveError
	if !errors.As(err, &ne) || !errors.Is(err, domain.ErrJurisdictionNotEffective) {
		t.Fatalf("before it began: %v, want a NotEffectiveError", err)
	}
	if !ne.EffectiveFrom.Equal(daysAfter(100)) || !strings.Contains(err.Error(), "not yet in force") {
		t.Errorf("error = %v (from %v)", err, ne.EffectiveFrom)
	}

	if _, err := s.GetJurisdictionAsOf(ctx, uuid.New().String(), daysAfter(10)); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("unknown id: %v, want ErrJurisdictionNotFound", err)
	}
	if _, err := s.GetJurisdictionAsOf(ctx, "not-a-uuid", daysAfter(10)); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("malformed id: %v, want ErrJurisdictionNotFound, never an outage", err)
	}
}

func TestRetire_RecordsEvidenceAndStaysReadableAsOfTheirPast(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	old := mustCreateJurisdiction(t, s, ctx, "OLD", nil)
	successor := mustCreateJurisdiction(t, s, ctx, "NEW", nil)
	before := time.Now().UTC().Add(-time.Minute)

	j, changed, err := s.RetireJurisdictionWithQuerier(ctx, pool, domain.RetireJurisdictionParams{
		JurisdictionID: old.JurisdictionID, ActorID: "steward-1", Reason: "state dissolved",
		EvidenceRef: "iso-3166-newsletter-2026-3", SupersededBy: &successor.JurisdictionID,
	})
	if err != nil || !changed {
		t.Fatalf("retire: changed=%v err=%v", changed, err)
	}
	if j.ActiveFlag || j.EffectiveTo == nil {
		t.Errorf("retired jurisdiction: active=%v effective_to=%v", j.ActiveFlag, j.EffectiveTo)
	}
	if j.RetirementReason == nil || *j.RetirementReason != "state dissolved" ||
		j.RetirementEvidenceRef == nil || *j.RetirementEvidenceRef != "iso-3166-newsletter-2026-3" ||
		j.SupersededByJurisdictionID == nil || *j.SupersededByJurisdictionID != successor.JurisdictionID {
		t.Errorf("evidence = %v / %v / %v", j.RetirementReason, j.RetirementEvidenceRef, j.SupersededByJurisdictionID)
	}

	// Readable as of when it was in force, and says it has since been retired.
	got, err := s.GetJurisdictionAsOf(ctx, old.JurisdictionID, before)
	if err != nil {
		t.Fatalf("as-of while in force: %v", err)
	}
	if !got.Retired || got.RetiredAt == nil || got.SupersededByJurisdictionID == nil || *got.SupersededByJurisdictionID != successor.JurisdictionID {
		t.Errorf("as-of = %+v, want retired with its successor", got)
	}

	// After retirement it is not in force: explicit, not a guess.
	_, err = s.GetJurisdictionAsOf(ctx, old.JurisdictionID, time.Now().UTC().Add(time.Hour))
	if !errors.Is(err, domain.ErrJurisdictionNotEffective) || !strings.Contains(err.Error(), "no longer in force") {
		t.Errorf("after retirement: %v, want no-longer-in-force", err)
	}

	// Refused for anything new.
	if _, err := s.FindByID(ctx, old.JurisdictionID); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("FindByID of a retired jurisdiction: %v, want not found", err)
	}
	_, _, err = s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionID: old.JurisdictionID, RuleDomain: "TAX", RuleCode: "RATE", RuleName: "x",
		EffectiveFrom: epoch, CreatedByPrincipalID: "admin",
	})
	if !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("a rule for a retired jurisdiction: %v, want it refused", err)
	}

	// The version history was closed at the retirement.
	vs := versionsOf(t, ctx, pool, old.JurisdictionID)
	if len(vs) != 1 || vs[0].To == nil {
		t.Errorf("versions = %+v, want the one version closed", vs)
	}
}

func TestRetire_ReplayChangesNothingAndKeepsTheOriginalEvidence(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "RP", nil)
	if _, _, err := s.RetireJurisdictionWithQuerier(ctx, pool, domain.RetireJurisdictionParams{
		JurisdictionID: j.JurisdictionID, ActorID: "a", Reason: "first", EvidenceRef: "ev-1"}); err != nil {
		t.Fatal(err)
	}
	again, changed, err := s.RetireJurisdictionWithQuerier(ctx, pool, domain.RetireJurisdictionParams{
		JurisdictionID: j.JurisdictionID, ActorID: "b", Reason: "second", EvidenceRef: "ev-2"})
	if err != nil || changed {
		t.Fatalf("replay: changed=%v err=%v, want unchanged", changed, err)
	}
	if again.RetirementReason == nil || *again.RetirementReason != "first" || *again.RetirementEvidenceRef != "ev-1" {
		t.Errorf("replay overwrote the evidence: %v / %v", again.RetirementReason, again.RetirementEvidenceRef)
	}
}

func TestRetire_SuccessorMustBeAnotherActiveJurisdiction(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "S1", nil)
	gone := mustCreateJurisdiction(t, s, ctx, "S2", nil)
	if _, _, err := s.RetireJurisdictionWithQuerier(ctx, pool, domain.RetireJurisdictionParams{JurisdictionID: gone.JurisdictionID, ActorID: "a"}); err != nil {
		t.Fatal(err)
	}
	unknown := uuid.New().String()

	for name, succ := range map[string]*string{"itself": &j.JurisdictionID, "unknown": &unknown, "already retired": &gone.JurisdictionID} {
		_, _, err := s.RetireJurisdictionWithQuerier(ctx, pool, domain.RetireJurisdictionParams{JurisdictionID: j.JurisdictionID, ActorID: "a", SupersededBy: succ})
		if !errors.Is(err, domain.ErrInvalidSuccessor) {
			t.Errorf("successor %s: %v, want ErrInvalidSuccessor", name, err)
		}
	}
	cur, err := s.FindByID(ctx, j.JurisdictionID)
	if err != nil || !cur.ActiveFlag {
		t.Fatalf("a refused retirement changed the jurisdiction: %v %v", cur, err)
	}
}

func TestRetire_WithNoEvidenceStillWorksAsBefore(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "NE", nil)
	got, changed, err := s.RetireJurisdictionWithQuerier(ctx, pool, domain.RetireJurisdictionParams{JurisdictionID: j.JurisdictionID, ActorID: "a"})
	if err != nil || !changed || got.RetirementReason != nil || got.RetirementEvidenceRef != nil || got.SupersededByJurisdictionID != nil {
		t.Fatalf("bare retirement: %+v changed=%v err=%v", got, changed, err)
	}
	if _, _, err := s.RetireJurisdictionWithQuerier(ctx, pool, domain.RetireJurisdictionParams{JurisdictionID: uuid.New().String(), ActorID: "a"}); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

func codes(n *domain.HierarchyNode) []string {
	out := []string{n.JurisdictionCode}
	for _, c := range n.Children {
		out = append(out, codes(c)...)
	}
	return out
}

func TestHierarchy_IsExactAsOfADateAcrossAMoveAndARetirement(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	root := seedJurisdiction(t, ctx, pool, "R", "COUNTRY", nil, epoch)
	a := seedJurisdiction(t, ctx, pool, "RA", "STATE_PROVINCE", &root, epoch)
	b := seedJurisdiction(t, ctx, pool, "RB", "STATE_PROVINCE", &root, epoch)
	leaf := seedJurisdiction(t, ctx, pool, "RB1", "TAX_AUTHORITY", &b, epoch)
	moveParent(t, ctx, pool, b, &a, daysAfter(100)) // RB moves under RA (and RB1 with it)
	if _, err := pool.Exec(ctx, `UPDATE jurisdictions SET active_flag = false, effective_to = $2 WHERE jurisdiction_id = $1`, leaf, daysAfter(200)); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		day  int
		want []string
	}{
		{50, []string{"R", "RA", "RB", "RB1"}}, // RB directly under R
		{150, []string{"R", "RA", "RB", "RB1"}},
		{250, []string{"R", "RA", "RB"}}, // RB1 retired on day 200
	}
	for _, c := range cases {
		h, err := s.GetHierarchy(ctx, root, daysAfter(c.day), 5)
		if err != nil {
			t.Fatalf("day %d: %v", c.day, err)
		}
		if got := codes(h.Root); strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("day %d: tree = %v, want %v", c.day, got, c.want)
		}
		if h.Count != len(c.want) {
			t.Errorf("day %d: count = %d, want %d", c.day, h.Count, len(c.want))
		}
	}

	// Where RB sat is the point: under R before the move, under RA after.
	before, _ := s.GetHierarchy(ctx, root, daysAfter(50), 5)
	after, _ := s.GetHierarchy(ctx, root, daysAfter(150), 5)
	childCodes := func(n *domain.HierarchyNode) []string {
		var out []string
		for _, c := range n.Children {
			out = append(out, c.JurisdictionCode)
		}
		return out
	}
	if got := childCodes(before.Root); strings.Join(got, ",") != "RA,RB" {
		t.Errorf("before the move R's children = %v, want RA,RB", got)
	}
	if got := childCodes(after.Root); strings.Join(got, ",") != "RA" {
		t.Errorf("after the move R's children = %v, want only RA", got)
	}
}

func TestHierarchy_DepthLimitsTheWalkAndIsValidated(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	root := seedJurisdiction(t, ctx, pool, "D0", "COUNTRY", nil, epoch)
	d1 := seedJurisdiction(t, ctx, pool, "D1", "STATE_PROVINCE", &root, epoch)
	d2 := seedJurisdiction(t, ctx, pool, "D2", "TAX_AUTHORITY", &d1, epoch)
	_ = seedJurisdiction(t, ctx, pool, "D3", "TAX_AUTHORITY", &d2, epoch)

	for depth, want := range map[int]string{0: "D0", 1: "D0,D1", 2: "D0,D1,D2", 3: "D0,D1,D2,D3"} {
		h, err := s.GetHierarchy(ctx, root, time.Now().UTC(), depth)
		if err != nil || strings.Join(codes(h.Root), ",") != want {
			t.Errorf("depth %d: %v (%v), want %s", depth, h, err, want)
		}
	}
	for _, bad := range []int{-1, domain.MaxHierarchyDepth + 1} {
		if _, err := s.GetHierarchy(ctx, root, time.Now().UTC(), bad); !errors.Is(err, domain.ErrInvalidDepth) {
			t.Errorf("depth %d: %v, want ErrInvalidDepth", bad, err)
		}
	}
	if _, err := s.GetHierarchy(ctx, uuid.New().String(), time.Now().UTC(), 3); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("unknown root: %v", err)
	}
	if _, err := s.GetHierarchy(ctx, root, daysAfter(-1000), 3); !errors.Is(err, domain.ErrJurisdictionNotEffective) {
		t.Errorf("a root not yet in force: %v, want NotEffective", err)
	}
}

// If versions ever formed a loop at some instant, the walk must still end.
func TestHierarchy_ALoopInTheDataCannotMakeTheWalkRunForever(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	a := seedJurisdiction(t, ctx, pool, "LA", "COUNTRY", nil, epoch)
	b := seedJurisdiction(t, ctx, pool, "LB", "STATE_PROVINCE", &a, epoch)
	// Corrupt the current state the way a bad direct write could: A under B.
	moveParent(t, ctx, pool, a, &b, daysAfter(10))

	h, err := s.GetHierarchy(ctx, b, daysAfter(20), domain.MaxHierarchyDepth)
	if err != nil {
		t.Fatal(err)
	}
	if h.Count > 2 {
		t.Errorf("count = %d: the loop was walked more than once", h.Count)
	}
}

// ── the version history is evidence ──────────────────────────────────────────

func TestVersions_AreAppendOnly(t *testing.T) {
	_, pool, ctx := newTestStore(t)
	c1 := seedJurisdiction(t, ctx, pool, "AO1", "COUNTRY", nil, epoch)
	c2 := seedJurisdiction(t, ctx, pool, "AO2", "COUNTRY", nil, epoch)
	st := seedJurisdiction(t, ctx, pool, "AOS", "STATE_PROVINCE", &c1, epoch)
	moveParent(t, ctx, pool, st, &c2, daysAfter(10)) // version 1 is now closed

	for name, sql := range map[string]string{
		"rename a version":      `UPDATE jurisdiction_versions SET jurisdiction_name = 'x' WHERE jurisdiction_id = $1 AND version_no = 1`,
		"re-parent a version":   `UPDATE jurisdiction_versions SET parent_jurisdiction_id = NULL WHERE jurisdiction_id = $1 AND version_no = 1`,
		"back-date a version":   `UPDATE jurisdiction_versions SET valid_from = valid_from - interval '1 day' WHERE jurisdiction_id = $1 AND version_no = 2`,
		"re-close a closed one": `UPDATE jurisdiction_versions SET valid_to = valid_to + interval '1 day' WHERE jurisdiction_id = $1 AND version_no = 1`,
		"delete a version":      `DELETE FROM jurisdiction_versions WHERE jurisdiction_id = $1`,
		"a second open version": `INSERT INTO jurisdiction_versions (jurisdiction_id, version_no, jurisdiction_name, jurisdiction_type, authority_type, valid_from, change_type, changed_by) VALUES ($1, 9, 'x', 'COUNTRY', 'FEDERAL', now(), 'UPDATED', 'x')`,
		"an inverted interval":  `INSERT INTO jurisdiction_versions (jurisdiction_id, version_no, jurisdiction_name, jurisdiction_type, authority_type, valid_from, valid_to, change_type, changed_by) VALUES ($1, 8, 'x', 'COUNTRY', 'FEDERAL', now(), now() - interval '1 day', 'UPDATED', 'x')`,
		"its own parent":        `INSERT INTO jurisdiction_versions (jurisdiction_id, version_no, jurisdiction_name, jurisdiction_type, parent_jurisdiction_id, authority_type, valid_from, valid_to, change_type, changed_by) VALUES ($1, 7, 'x', 'COUNTRY', $1, 'FEDERAL', now(), now() + interval '1 day', 'UPDATED', 'x')`,
	} {
		if _, err := pool.Exec(ctx, sql, st); err == nil {
			t.Errorf("%s: the database allowed it", name)
		}
	}
	if _, err := pool.Exec(ctx, `TRUNCATE jurisdiction_versions`); err == nil {
		t.Error("TRUNCATE of the version history was allowed")
	}
	if n := len(versionsOf(t, ctx, pool, st)); n != 2 {
		t.Errorf("%d versions after the refused writes, want the original 2", n)
	}
}

// ── the migration ────────────────────────────────────────────────────────────

func migration(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "deployments", "migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A database that already holds jurisdictions gets one version each, once, and
// running the migration again changes nothing.
func TestMigration000027_BackfillsExistingJurisdictionsAndIsRerunnable(t *testing.T) {
	_, pool, ctx := newTestStore(t)
	c := seedJurisdiction(t, ctx, pool, "BF", "COUNTRY", nil, epoch)
	st := seedJurisdiction(t, ctx, pool, "BFS", "STATE_PROVINCE", &c, epoch)

	// Rewind to "before the migration": no versions, no maintainer.
	for _, q := range []string{
		`ALTER TABLE jurisdiction_versions DISABLE TRIGGER USER`,
		`DELETE FROM jurisdiction_versions`,
		`ALTER TABLE jurisdiction_versions ENABLE TRIGGER USER`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	up := migration(t, "000027_jurisdiction_versions.up.sql")
	for i := 1; i <= 2; i++ {
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("run %d of the migration: %v", i, err)
		}
	}
	for _, id := range []string{c, st} {
		vs := versionsOf(t, ctx, pool, id)
		if len(vs) != 1 || vs[0].ChangeType != "ADDED" || vs[0].To != nil || !strings.Contains(*vs[0].Reason, "backfilled") {
			t.Errorf("backfill of %s = %+v, want exactly one open backfilled version", id, vs)
		}
	}
	if vs := versionsOf(t, ctx, pool, st); vs[0].Parent == nil || *vs[0].Parent != c {
		t.Errorf("backfilled parent = %v, want %s", vs[0].Parent, c)
	}
}

func TestMigration000027_DownIsRefusedWhileHistoryExists(t *testing.T) {
	_, pool, ctx := newTestStore(t)
	c1 := seedJurisdiction(t, ctx, pool, "DN1", "COUNTRY", nil, epoch)
	c2 := seedJurisdiction(t, ctx, pool, "DN2", "COUNTRY", nil, epoch)
	st := seedJurisdiction(t, ctx, pool, "DNS", "STATE_PROVINCE", &c1, epoch)
	down, up := migration(t, "000027_jurisdiction_versions.down.sql"), migration(t, "000027_jurisdiction_versions.up.sql")

	moveParent(t, ctx, pool, st, &c2, daysAfter(10))
	if _, err := pool.Exec(ctx, down); err == nil || !strings.Contains(err.Error(), "000027 down refused") {
		t.Fatalf("down with history present: %v, want it refused", err)
	}

	// With only first versions and no retirement evidence it applies, and the up applies again.
	if _, err := pool.Exec(ctx, `ALTER TABLE jurisdiction_versions DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM jurisdiction_versions WHERE version_no > 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE jurisdiction_versions ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatalf("down with nothing to lose: %v", err)
	}
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

var _ = store.New

// A retirement conditional on a version (If-Match, API-3) applies only to that
// version: a stale one changes nothing and is ErrVersionConflict.
func TestRetire_ExpectedRowVersionIsEnforced(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "VER", nil)
	stale := j.RowVersion + 1

	if _, _, err := s.RetireJurisdictionWithQuerier(ctx, pool, domain.RetireJurisdictionParams{
		JurisdictionID: j.JurisdictionID, ActorID: "steward-1", Reason: "stale", ExpectedRowVersion: &stale,
	}); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("stale version: %v, want ErrVersionConflict", err)
	}
	still, err := s.FindByID(ctx, j.JurisdictionID)
	if err != nil || !still.ActiveFlag {
		t.Fatalf("a refused retirement changed the jurisdiction: %+v (err %v)", still, err)
	}

	current := j.RowVersion
	got, changed, err := s.RetireJurisdictionWithQuerier(ctx, pool, domain.RetireJurisdictionParams{
		JurisdictionID: j.JurisdictionID, ActorID: "steward-1", Reason: "current", ExpectedRowVersion: &current,
	})
	if err != nil || !changed || got.ActiveFlag {
		t.Fatalf("current version: changed=%v err=%v %+v", changed, err, got)
	}
}
