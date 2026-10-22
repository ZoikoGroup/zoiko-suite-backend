package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// A12: a certification binds what it certified — the content hash of every
// side of the generation certified, every manual match with its reason and
// evidence, and the certifier's traceable authority.

// The manual-match run: REF-1 auto-matches, and X/Y only pair manually (the
// keys differ; 1003 against 1000 is inside the 5-minor-unit tolerance).
func manualMatchRun(f *fixture) (*domain.ReconciliationRun, string, string) {
	f.t.Helper()
	def := f.defineReconciliation(orgA, "manual-evidence", 5)
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100), item("BANK-X", 1003)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 100), item("GL-Y", 1000)}},
	})
	f.match(run.RunID)
	var bank, ledger string
	for _, e := range f.exceptions(run.RunID) {
		if e.Kind != domain.ExceptionKindItem {
			continue
		}
		if e.Side == "Bank" {
			bank = e.ItemID
		} else {
			ledger = e.ItemID
		}
	}
	if bank == "" || ledger == "" {
		f.t.Fatalf("expected one unmatched item per side: %+v", f.exceptions(run.RunID))
	}
	return run, bank, ledger
}

func TestCertification_BindsPopulationsManualMatchesAndAuthority(t *testing.T) {
	f := newFixture(t)
	run, bank, ledger := manualMatchRun(f)
	const reason = "same settlement; bank reference truncated by the processor"
	if _, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{ManualMatches: []domain.ManualMatchInput{
		{ItemAID: bank, ItemBID: ledger, Reason: reason}}}, "reviewer", f.claim("Match", "manual")); err != nil {
		t.Fatalf("manual match: %v", err)
	}

	// The match records its evidence when it is made.
	results, err := f.s.GetMatchResults(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var manual *domain.MatchResult
	for i := range results {
		if results[i].Manual {
			manual = &results[i]
		} else if results[i].Evidence != nil {
			t.Fatalf("an automatic match carries manual evidence: %+v", results[i])
		}
	}
	if manual == nil || manual.Evidence == nil {
		t.Fatalf("manual match without evidence: %+v", results)
	}
	ev := manual.Evidence
	if ev.Generation != 1 || ev.ItemA.ItemID != bank || ev.ItemB.ItemID != ledger || !ev.KeyOverridden ||
		ev.ItemA.RefID != "BANK-X" || ev.ItemB.RefID != "GL-Y" || ev.ItemA.AmountMinorUnits != 1003 || ev.ItemB.AmountMinorUnits != 1000 ||
		ev.Currency != "USD" || ev.DifferenceMinorUnits != 3 || ev.AllowedMinorUnits != 5 ||
		ev.Tolerance.Currencies["USD"].AbsoluteMinorUnits == nil || *ev.Tolerance.Currencies["USD"].AbsoluteMinorUnits != 5 ||
		ev.ItemA.SourceRecordID == "" || ev.ItemB.SourceRecordID == "" {
		t.Fatalf("manual match evidence: %+v", ev)
	}

	var exceptionIDs []string
	for _, e := range f.exceptions(run.RunID) {
		if e.Kind == domain.ExceptionKindItem {
			exceptionIDs = append(exceptionIDs, e.ExceptionID)
		}
	}
	sort.Strings(exceptionIDs)

	authority := grant("certifier")
	cert, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", authority, f.claim("Certify", run.RunID))
	if err != nil {
		t.Fatalf("certify: %v", err)
	}

	// Populations: both sides of generation 1, by the frozen snapshots' hashes.
	snaps, err := f.s.GetPopulationSnapshots(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	assertPopulations(t, cert.Populations, snaps, 1)

	// Manual matches: reason, who, evidence and the exceptions resolved.
	if len(cert.ManualMatches) != 1 {
		t.Fatalf("manual matches: %+v", cert.ManualMatches)
	}
	m := cert.ManualMatches[0]
	if m.MatchResultID != manual.MatchResultID || m.Reason != reason || m.MatchedBy != "reviewer" || m.ItemAID != bank ||
		m.ItemBID != ledger || m.Evidence == nil || m.Evidence.DifferenceMinorUnits != 3 ||
		strings.Join(m.ResolvedExceptionIDs, ",") != strings.Join(exceptionIDs, ",") || len(exceptionIDs) != 2 {
		t.Fatalf("certified manual match: %+v (exceptions %v)", m, exceptionIDs)
	}

	// Authority: the certifier's own decision, as handed in.
	if cert.Authority == nil || cert.Authority.AccessDecisionID != "adl-certifier" || cert.Authority.PrincipalID != "certifier" ||
		cert.Authority.DecisionBasis != authority.DecisionBasis || cert.Authority.PolicySetVersion != "psv-1" {
		t.Fatalf("certification authority: %+v", cert.Authority)
	}

	// The read serves the same record.
	got, err := f.s.GetCertification(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ManifestSHA256 != cert.ManifestSHA256 || got.Authority == nil || got.Authority.AccessDecisionID != "adl-certifier" ||
		len(got.Populations) != 2 || len(got.ManualMatches) != 1 || got.ManualMatches[0].Evidence == nil {
		t.Fatalf("certification read back: %+v", got)
	}

	// The record cannot be written around: a row whose authority names no
	// access decision, or another principal, breaks the table's check.
	for name, authorityExpr := range map[string]string{
		"no access decision": `authority_snapshot - 'access_decision_id'`,
		"another principal":  `jsonb_set(authority_snapshot, '{principal_id}', '"someone-else"')`,
	} {
		_, err := f.admin.Exec(f.ctx, `INSERT INTO certifications (certification_id, tenant_id, run_id, definition_version,
				matched_count, exception_count, control_totals, result_state, assessment, authority_snapshot, populations,
				manual_matches, manifest_sha256, sealed_at, sealed_by)
			SELECT 'rct_00000000-0000-0000-0000-000000000000', tenant_id, run_id, definition_version, matched_count,
				exception_count, control_totals, result_state, assessment, `+authorityExpr+`, populations, manual_matches,
				manifest_sha256, sealed_at, sealed_by
			FROM certifications WHERE run_id = $1`, run.RunID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "certifications_evidence_check" {
			t.Fatalf("%s: inserted or failed for another reason: %v", name, err)
		}
	}
}

// The populations certified are the latest generation's, by their own hashes:
// a reperformance's re-read, not the first freeze.
func TestCertification_BindsTheGenerationCertified(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "generation-evidence", 0)
	run := f.startRun(orgA, def, cleanPops())
	f.match(run.RunID)
	if _, err := f.s.Reperform(f.ctx, orgA, run.RunID, frozenAll(cleanPops()), "test-operator", f.claim("Reperform", run.RunID)); err != nil {
		t.Fatalf("reperform: %v", err)
	}
	cert, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", grant("certifier"), f.claim("Certify", run.RunID))
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	snaps, err := f.s.GetPopulationSnapshots(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 4 {
		t.Fatalf("want two generations of two sides: %+v", snaps)
	}
	assertPopulations(t, cert.Populations, snaps, 2)
	gen1 := map[string]string{}
	for _, s := range snaps {
		if s.Generation == 1 {
			gen1[s.Side] = s.ContentHash
		}
	}
	for _, p := range cert.Populations {
		if p.ContentHash == gen1[p.Side] {
			t.Fatalf("%s: certified hash is generation 1's", p.Side)
		}
	}
	if cert.ManualMatches == nil || len(cert.ManualMatches) != 0 {
		t.Fatalf("a run without manual matches certifies an empty list, not none: %+v", cert.ManualMatches)
	}
}

// assertPopulations checks the certification lists exactly the given
// generation's snapshots, by id and hash.
func assertPopulations(t *testing.T, got []domain.CertifiedPopulation, snaps []domain.PopulationSnapshot, generation int) {
	t.Helper()
	want := map[string]domain.PopulationSnapshot{}
	for _, s := range snaps {
		if s.Generation == generation {
			want[s.Side] = s
		}
	}
	if len(got) != len(want) || len(got) != 2 {
		t.Fatalf("certified populations %+v, want generation %d of %d sides", got, generation, len(want))
	}
	for _, p := range got {
		s, ok := want[p.Side]
		if !ok || p.SnapshotID != s.SnapshotID || p.ContentHash != s.ContentHash || p.Generation != generation ||
			p.SpecRef != s.SpecRef || p.SourceWatermark != s.SourceWatermark || p.ItemCount != s.ItemCount || len(p.ContentHash) != 64 {
			t.Fatalf("certified %s population %+v, frozen %+v", p.Side, p, s)
		}
	}
}

// A certification whose authority cannot be traced to the certifier's own
// decision is refused, and nothing is certified.
func TestCertification_RefusesUntraceableAuthority(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "untraceable", 0)
	run := f.startRun(orgA, def, cleanPops())
	f.match(run.RunID)

	noID := grant("certifier")
	noID.AccessDecisionID = ""
	denied := grant("certifier")
	denied.DecisionOutcome = "DENIED"
	for name, a := range map[string]*domain.AuthoritySnapshot{
		"none": nil, "no access decision": noID, "not granted": denied, "another principal's": grant("someone-else"),
	} {
		if _, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", a, f.claim("Certify", run.RunID)); !errors.Is(err, domain.ErrAuthorityUntraceable) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := f.s.GetCertification(f.ctx, orgA, run.RunID); !errors.Is(err, domain.ErrRunNotFound) {
		t.Fatalf("a refused certification was recorded: %v", err)
	}
	r, _ := f.s.GetRun(f.ctx, orgA, run.RunID)
	if r.Status == domain.RunCertified || r.CertificationState == domain.CertCertified {
		t.Fatalf("a refused certification changed the run: %s/%s", r.Status, r.CertificationState)
	}
}

// 000010's down refuses while a certification or manual match records its
// evidence, and round-trips while none does.
func TestMigration000010_DownRefusesWhileEvidenceExists(t *testing.T) {
	f := newFixture(t)
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	down, err := os.ReadFile(filepath.Join(dir, "000010_certification_evidence.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile(filepath.Join(dir, "000010_certification_evidence.up.sql"))
	if err != nil {
		t.Fatal(err)
	}

	// Nothing recorded yet: down and up again.
	if _, err := f.admin.Exec(f.ctx, string(down)); err != nil {
		t.Fatalf("down with no evidence: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, string(up)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}

	run, bank, ledger := manualMatchRun(f)
	if _, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{ManualMatches: []domain.ManualMatchInput{
		{ItemAID: bank, ItemBID: ledger, Reason: "same settlement"}}}, "reviewer", f.claim("Match", "manual")); err != nil {
		t.Fatalf("manual match: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, string(down)); err == nil || !strings.Contains(err.Error(), "cannot roll back 000010") {
		t.Fatalf("down with a manual match's evidence: %v", err)
	}
	var n int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM match_results WHERE evidence IS NOT NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("refused down changed the evidence: %d, %v", n, err)
	}
}
