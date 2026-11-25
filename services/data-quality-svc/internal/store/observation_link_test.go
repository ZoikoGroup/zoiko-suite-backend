package store_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/data-quality-svc/internal/domain"
)

// ZS-SVC-Z-001 DQC-02 (docs/architecture/DQC_GAP_MATRIX.md, step G): a validation run can name the observation its
// population was taken from, and then it must BE what was observed.

func (f *fixture) linkedRequest(c *domain.ContractVersion, obs *domain.DQObservation, outcomes ...domain.RuleOutcome) domain.RunDQRequest {
	return domain.RunDQRequest{ContractAssetID: c.AssetID, ContractVersion: c.VersionNumber, ObservationID: obs.ObservationRunID,
		PopulationRef: "pop-link", PopulationRowCount: obs.RowCount, PopulationContentHash: obs.EvidenceHash, RuleOutcomes: outcomes}
}

func (f *fixture) runLinked(req domain.RunDQRequest) (*domain.DQRun, error) {
	return f.s.RunDQ(f.ctx, orgA, req, "test-operator", f.claim("RunDQ", req.PopulationRef+req.ObservationID))
}

func TestObservationLink_RunTakesItsEvidenceFromTheObservation(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "link.ok", profiled(domain.CompatBackward))
	obs, err := f.observe(orgA, f.observation(c.AssetID, day, "wm-link", hashA))
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.runLinked(f.linkedRequest(c, obs, allPass()...))
	if err != nil {
		t.Fatalf("a run linked to its observation: %v", err)
	}
	if run.ObservationID == nil || *run.ObservationID != obs.ObservationRunID {
		t.Fatalf("the run does not name its observation: %+v", run)
	}
	// With no evidence_as_of of its own, the evidence is as old as the observation that recorded it.
	if run.EvidenceAsOf == nil || !run.EvidenceAsOf.Equal(obs.RecordedAt) {
		t.Fatalf("evidence_as_of = %v, want the observation's recorded_at %v", run.EvidenceAsOf, obs.RecordedAt)
	}
	got, err := f.s.GetRun(f.ctx, orgA, run.RunID)
	if err != nil || got.ObservationID == nil || *got.ObservationID != obs.ObservationRunID {
		t.Fatalf("the stored run: %+v %v", got, err)
	}
}

func TestObservationLink_TheRunMustBeWhatWasObserved(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "link.bad", profiled(domain.CompatBackward))
	other := f.published(orgA, "link.other", profiled(domain.CompatBackward))
	obs, err := f.observe(orgA, f.observation(c.AssetID, day, "wm-1", hashA))
	if err != nil {
		t.Fatal(err)
	}
	otherObs, err := f.observe(orgA, f.observation(other.AssetID, day, "wm-1", hashA))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	good := f.linkedRequest(c, obs, allPass()...)

	cases := map[string]func(*domain.RunDQRequest){
		"a different row count":       func(r *domain.RunDQRequest) { r.PopulationRowCount = obs.RowCount + 1 },
		"a different hash":            func(r *domain.RunDQRequest) { r.PopulationContentHash = hashB },
		"another asset's observation": func(r *domain.RunDQRequest) { r.ObservationID = otherObs.ObservationRunID },
		"observed after it was recorded": func(r *domain.RunDQRequest) {
			later := obs.RecordedAt.Add(5 * time.Millisecond) // after the record, but not in the future
			r.EvidenceAsOf = &later
		},
	}
	for name, mutate := range cases {
		r := good
		r.PopulationRef = "pop-link-" + name
		mutate(&r)
		if _, err := f.runLinked(r); !errors.Is(err, domain.ErrObservationMismatch) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	unknown := good
	unknown.ObservationID = "dqo_00000000-0000-0000-0000-000000000000"
	if _, err := f.runLinked(unknown); !errors.Is(err, domain.ErrObservationNotFound) {
		t.Fatalf("an unknown observation: %v", err)
	}
	// A rule-set run has no asset to have observed.
	v := f.publishRuleSet(orgA, "link-legacy", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	ls := domain.RunDQRequest{RuleSetVersionID: v.VersionID, ObservationID: obs.ObservationRunID, PopulationRef: "p", PopulationRowCount: obs.RowCount,
		PopulationContentHash: obs.EvidenceHash, RuleOutcomes: []domain.RuleOutcome{passingOutcome("r-a")}}
	if _, err := f.runLinked(ls); !errors.Is(err, domain.ErrObservationMismatch) {
		t.Fatalf("an observation linked to a rule-set run: %v", err)
	}
	// Proof that every refusal above was about its own defect: the unmodified request is accepted.
	if _, err := f.runLinked(good); err != nil {
		t.Fatalf("the good request was refused: %v", err)
	}
	// ...and so is an evidence_as_of that is not after the observation was recorded.
	earlier := obs.RecordedAt.Add(-time.Minute)
	e := good
	e.PopulationRef = "pop-link-earlier"
	e.EvidenceAsOf = &earlier
	if _, err := f.runLinked(e); err != nil {
		t.Fatalf("evidence observed before the observation was recorded: %v", err)
	}
}

// Another tenant's observation does not exist for this tenant.
func TestObservationLink_IsTenantScoped(t *testing.T) {
	f := newContractFixture(t)
	cB := f.published(orgB, "link.tenantb", profiled(domain.CompatBackward))
	obsB, err := f.observe(orgB, f.observation(cB.AssetID, day, "wm-b", hashA))
	if err != nil {
		t.Fatal(err)
	}
	cA := f.published(orgA, "link.tenanta", profiled(domain.CompatBackward))
	req := f.linkedRequest(cA, obsB, allPass()...)
	if _, err := f.runLinked(req); !errors.Is(err, domain.ErrObservationNotFound) {
		t.Fatalf("tenant A linked tenant B's observation: %v", err)
	}
}

// A run linked to an observation is a different record from the same evidence with no link, so the evidence
// fingerprint does not fold them together.
func TestObservationLink_LinkedAndUnlinkedAreDifferentRuns(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "link.fp", profiled(domain.CompatBackward))
	obs, err := f.observe(orgA, f.observation(c.AssetID, day, "wm-fp", hashA))
	if err != nil {
		t.Fatal(err)
	}
	linked := f.linkedRequest(c, obs, allPass()...)
	unlinked := linked
	unlinked.ObservationID = ""
	asOf := obs.RecordedAt
	unlinked.EvidenceAsOf = &asOf
	a, err := f.runLinked(unlinked)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.runLinked(linked)
	if err != nil || b.Deduplicated || b.RunID == a.RunID || b.ObservationID == nil {
		t.Fatalf("linked after unlinked: %+v %v", b, err)
	}
	// The same linked request again is the same run.
	again, err := f.runLinked(linked)
	if err != nil || !again.Deduplicated || again.RunID != b.RunID {
		t.Fatalf("the same linked request: %+v %v", again, err)
	}
}

func TestObservationLink_DatabaseRequiresAContract(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "link.db", profiled(domain.CompatBackward))
	v := f.publishRuleSet(orgA, "link-db-legacy", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	obs, err := f.observe(orgA, f.observation(c.AssetID, day, "wm-db", hashA))
	if err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO dq_runs (run_id, tenant_id, ruleset_version_id, contract_id, population_ref, population_row_count, population_content_hash, status, created_by, observation_id)
		VALUES ($1, $2, $3, $4, 'p', 1, repeat('a',64), 'Running', 'x', $5)`
	if _, err := f.admin.Exec(f.ctx, insert, newTestRunID(), orgA, v.VersionID, nil, obs.ObservationRunID); err == nil {
		t.Fatal("a rule-set run naming an observation was accepted")
	}
	if _, err := f.admin.Exec(f.ctx, insert, newTestRunID(), orgA, v.VersionID, nil, nil); err != nil {
		t.Fatalf("a rule-set run naming none was refused: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, insert, newTestRunID(), orgA, nil, c.ContractID, obs.ObservationRunID); err != nil {
		t.Fatalf("a contract run naming an observation was refused: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, insert, newTestRunID(), orgA, nil, c.ContractID, "dqo_00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("a run naming an observation that does not exist was accepted")
	}
}
