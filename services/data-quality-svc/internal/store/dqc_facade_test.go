package store_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/data-quality-svc/internal/domain"
)

// ZS-SVC-Z-001 section 11 (docs/architecture/DQC_GAP_MATRIX.md, step F4): the idempotency rule behind each
// /dqc/* route - the same observation, evidence, failing result or certification basis returns the existing
// record rather than a second one.

func (f *fixture) observation(asset string, start time.Time, watermark, hash string) domain.RecordObservationRequest {
	return domain.RecordObservationRequest{AssetID: asset, WindowStart: start, WindowEnd: start.Add(24 * time.Hour), SourceWatermark: watermark,
		SourceSystem: "erp-x", RowCount: 1000, EvidenceHash: hash}
}

func (f *fixture) observe(org string, req domain.RecordObservationRequest) (*domain.DQObservation, error) {
	return f.s.RecordObservation(f.ctx, org, req, "observer", f.claim("RecordObservation", req.AssetID+req.SourceWatermark))
}

var (
	hashA = hashLike("evidence-a")
	hashB = hashLike("evidence-b")
	day   = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
)

func TestObservation_RecordedOnceAndAppendOnly(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "obs.asset", contractBody(domain.CompatBackward))
	req := f.observation(c.AssetID, day, "wm-100", hashA)
	first, err := f.observe(orgA, req)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if first.Deduplicated || first.RecordedBy != "observer" || first.EvidenceHash != hashA || first.AssetID != c.AssetID {
		t.Fatalf("observation: %+v", first)
	}
	// The same asset + window + watermark with the same evidence is the same observation.
	again, err := f.observe(orgA, req)
	if err != nil || !again.Deduplicated || again.ObservationRunID != first.ObservationRunID {
		t.Fatalf("a repeat: %+v %v", again, err)
	}
	var n int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM dq_observations WHERE asset_id = $1`, c.AssetID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d observation rows for one observation", n)
	}
	got, err := f.s.GetObservation(f.ctx, orgA, first.ObservationRunID)
	if err != nil || got.EvidenceHash != hashA {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := f.s.GetObservation(f.ctx, orgA, "dqo_00000000-0000-0000-0000-000000000000"); !errors.Is(err, domain.ErrObservationNotFound) {
		t.Fatalf("an unknown observation: %v", err)
	}
}

// Late-arriving evidence is a NEW observation at a later watermark; it never edits an earlier one.
func TestObservation_SameKeyDifferentEvidenceIsAConflict_LaterWatermarkIsNew(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "obs.late", contractBody(domain.CompatBackward))
	if _, err := f.observe(orgA, f.observation(c.AssetID, day, "wm-100", hashA)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.observe(orgA, f.observation(c.AssetID, day, "wm-100", hashB)); !errors.Is(err, domain.ErrObservationConflict) {
		t.Fatalf("different evidence at the same watermark: %v", err)
	}
	// The same evidence but a different row count or source is also not the same observation.
	other := f.observation(c.AssetID, day, "wm-100", hashA)
	other.RowCount = 999
	if _, err := f.observe(orgA, other); !errors.Is(err, domain.ErrObservationConflict) {
		t.Fatalf("a different row count at the same watermark: %v", err)
	}
	late, err := f.observe(orgA, f.observation(c.AssetID, day, "wm-101", hashB))
	if err != nil || late.Deduplicated {
		t.Fatalf("the late evidence at a later watermark: %+v %v", late, err)
	}
	// A different window is a different observation too.
	if _, err := f.observe(orgA, f.observation(c.AssetID, day.Add(48*time.Hour), "wm-100", hashA)); err != nil {
		t.Fatalf("another window: %v", err)
	}
}

func TestObservation_RefusesWhatItCannotRecord(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "obs.bad", contractBody(domain.CompatBackward))
	good := f.observation(c.AssetID, day, "wm-1", hashA)
	cases := map[string]func(*domain.RecordObservationRequest){
		"no asset":        func(r *domain.RecordObservationRequest) { r.AssetID = "" },
		"no watermark":    func(r *domain.RecordObservationRequest) { r.SourceWatermark = "  " },
		"no source":       func(r *domain.RecordObservationRequest) { r.SourceSystem = "" },
		"window reversed": func(r *domain.RecordObservationRequest) { r.WindowEnd = r.WindowStart.Add(-time.Hour) },
		"empty window":    func(r *domain.RecordObservationRequest) { r.WindowEnd = r.WindowStart },
		"no window":       func(r *domain.RecordObservationRequest) { r.WindowStart, r.WindowEnd = time.Time{}, time.Time{} },
		"negative rows":   func(r *domain.RecordObservationRequest) { r.RowCount = -1 },
		"short hash":      func(r *domain.RecordObservationRequest) { r.EvidenceHash = "abc" },
		"uppercase hash":  func(r *domain.RecordObservationRequest) { r.EvidenceHash = "A" + hashA[1:] },
		"future window end": func(r *domain.RecordObservationRequest) {
			r.WindowEnd = time.Now().Add(72 * time.Hour)
			r.WindowStart = time.Now().Add(-time.Hour)
		},
	}
	for name, mutate := range cases {
		r := good
		mutate(&r)
		if _, err := f.observe(orgA, r); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// Proof that those were about their defect: the unmodified request is accepted.
	if _, err := f.observe(orgA, good); err != nil {
		t.Fatalf("a good observation was refused: %v", err)
	}
	// An asset that is not registered has nothing to be observed against (DQC-001).
	ghost := f.observation("dca_00000000-0000-0000-0000-000000000000", day, "wm-1", hashA)
	if _, err := f.observe(orgA, ghost); !errors.Is(err, domain.ErrContractNotFound) {
		t.Fatalf("an unregistered asset: %v", err)
	}
	// ...and another tenant's asset is not visible.
	if _, err := f.observe(orgB, f.observation(c.AssetID, day, "wm-9", hashA)); !errors.Is(err, domain.ErrContractNotFound) {
		t.Fatalf("tenant B observed tenant A's asset: %v", err)
	}
}

func TestObservation_DatabaseKeepsItAppendOnly(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "obs.db", contractBody(domain.CompatBackward))
	o, err := f.observe(orgA, f.observation(c.AssetID, day, "wm-1", hashA))
	if err != nil {
		t.Fatal(err)
	}
	for name, stmt := range map[string]string{
		"change the evidence": `UPDATE dq_observations SET evidence_hash = repeat('b', 64) WHERE observation_run_id = $1`,
		"delete":              `DELETE FROM dq_observations WHERE observation_run_id = $1`,
	} {
		if _, err := f.admin.Exec(f.ctx, stmt, o.ObservationRunID); err == nil {
			t.Fatalf("the database allowed: %s", name)
		}
	}
	insert := `INSERT INTO dq_observations (observation_run_id, tenant_id, asset_id, window_start, window_end, source_watermark, source_system, row_count, evidence_hash, recorded_by)
		VALUES ($1, $2, $3, $4, $5, 'wm-x', 's', 1, $6, 'x')`
	id := func() string { return "dqo_" + newTestRunID()[4:] }
	if _, err := f.admin.Exec(f.ctx, insert, id(), orgA, c.AssetID, day, day, hashA); err == nil {
		t.Fatal("an empty window was accepted")
	}
	if _, err := f.admin.Exec(f.ctx, insert, id(), orgA, c.AssetID, day, day.Add(time.Hour), "nothex"); err == nil {
		t.Fatal("a malformed hash was accepted")
	}
	if _, err := f.admin.Exec(f.ctx, insert, id(), orgA, c.AssetID, day, day.Add(time.Hour), hashA); err != nil {
		t.Fatalf("a valid observation row was refused: %v", err)
	}
}

func TestObservation_EventCarriesTheSpecFields(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "obs.evt", contractBody(domain.CompatBackward))
	req := f.observation(c.AssetID, day, "wm-evt", hashA)
	o, err := f.observe(orgA, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.observe(orgA, req); err != nil { // a repeat
		t.Fatal(err)
	}
	var n int
	var asset, run, wm, hash string
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*), COALESCE(max(payload->>'asset_id'),''), COALESCE(max(payload->>'observation_run_id'),''),
			COALESCE(max(payload->>'watermark'),''), COALESCE(max(payload->>'evidence_hash'),'')
		FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'dqc.observation.completed'`, o.ObservationRunID).Scan(&n, &asset, &run, &wm, &hash); err != nil {
		t.Fatal(err)
	}
	if n != 1 || asset != c.AssetID || run != o.ObservationRunID || wm != "wm-evt" || hash != hashA {
		t.Fatalf("events=%d asset=%q run=%q watermark=%q hash=%q", n, asset, run, wm, hash)
	}
}

// POST /dqc/validate: the same pinned contract and evidence is the same run.
func TestValidate_SameEvidenceIsTheSameRun(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "val.same", profiled(domain.CompatBackward))
	observed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	first, err := f.contractRunAt(c, "pop-val", &observed, allPass()...)
	if err != nil || first.Deduplicated {
		t.Fatalf("first: %+v %v", first, err)
	}
	// Same evidence, outcomes in a different order, a different idempotency key: the existing run.
	reordered := allPass()
	reordered[0], reordered[3] = reordered[3], reordered[0]
	again, err := f.contractRunAt(c, "pop-val", &observed, reordered...)
	if err != nil || !again.Deduplicated || again.RunID != first.RunID {
		t.Fatalf("the same evidence: %+v %v", again, err)
	}
	var n int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM dq_runs WHERE contract_id = $1`, c.ContractID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d runs for one piece of evidence", n)
	}
	var events int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'dqc.validation.completed'`, first.RunID).Scan(&events)
	if events != 1 {
		t.Fatalf("dqc.validation.completed written %d times for one run", events)
	}
	// Anything that changes the evidence makes it a different run: an outcome, the population, when it was observed.
	changed := allPass()
	changed[2] = fail("memo-present")
	if r, err := f.contractRunAt(c, "pop-val", &observed, changed...); err != nil || r.Deduplicated || r.RunID == first.RunID {
		t.Fatalf("a different outcome: %+v %v", r, err)
	}
	if r, err := f.contractRunAt(c, "pop-val-2", &observed, allPass()...); err != nil || r.Deduplicated {
		t.Fatalf("a different population: %+v %v", r, err)
	}
	// The same population NAME with different content, or a different row count, is different evidence.
	base := domain.RunDQRequest{ContractAssetID: c.AssetID, ContractVersion: 1, PopulationRef: "pop-val", PopulationRowCount: 100,
		PopulationContentHash: hashLike("pop-val"), EvidenceAsOf: &observed, RuleOutcomes: allPass()}
	rehashed, recounted := base, base
	rehashed.PopulationContentHash = hashLike("different content")
	recounted.PopulationRowCount = 101
	for name, req := range map[string]domain.RunDQRequest{"content hash": rehashed, "row count": recounted} {
		r, err := f.s.RunDQ(f.ctx, orgA, req, "test-operator", f.claim("RunDQ", name))
		if err != nil || r.Deduplicated || r.RunID == first.RunID {
			t.Fatalf("a different %s: %+v %v", name, r, err)
		}
	}
	later := observed.Add(time.Minute)
	if r, err := f.contractRunAt(c, "pop-val", &later, allPass()...); err != nil || r.Deduplicated {
		t.Fatalf("observed at a different time: %+v %v", r, err)
	}
}

// Rule-set runs are not fingerprinted: nothing about them changed.
func TestValidate_RuleSetRunsAreNotDeduplicated(t *testing.T) {
	f := newContractFixture(t)
	v := f.publishRuleSet(orgA, "val-legacy", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	a, err := f.run(v, "pop-legacy", passingOutcome("r-a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.run(v, "pop-legacy", passingOutcome("r-a"))
	if err != nil || b.RunID == a.RunID || b.Deduplicated {
		t.Fatalf("a repeated rule-set run: %+v %v", b, err)
	}
}

// A reperform is new evidence by definition: reperforming with the SAME evidence would supersede a run with itself.
func TestValidate_ReperformWithIdenticalEvidenceIsRefused(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "val.rep", profiled(domain.CompatBackward))
	observed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	bad, err := f.contractRunAt(c, "pop-rep", &observed, fail("amount-positive"), pass("invoice-unique"), pass("memo-present"), pass("format-ok"))
	if err != nil || bad.Status != domain.RunExceptionsOpen {
		t.Fatalf("setup: %+v %v", bad, err)
	}
	req := domain.RunDQRequest{ContractAssetID: c.AssetID, ContractVersion: 1, PopulationRef: "pop-rep", PopulationRowCount: 100, PopulationContentHash: hashLike("pop-rep"),
		EvidenceAsOf: &observed, RuleOutcomes: []domain.RuleOutcome{fail("amount-positive"), pass("invoice-unique"), pass("memo-present"), pass("format-ok")}}
	if _, err := f.s.Reperform(f.ctx, orgA, bad.RunID, req, "test-operator", f.claim("Reperform", bad.RunID)); !errors.Is(err, domain.ErrEvidenceAlreadyRecorded) {
		t.Fatalf("a reperform of identical evidence: %v", err)
	}
	// With corrected evidence it is accepted: it was the identical evidence that was refused.
	req.RuleOutcomes = allPass()
	if _, err := f.s.Reperform(f.ctx, orgA, bad.RunID, req, "test-operator", f.claim("Reperform", bad.RunID)); err != nil {
		t.Fatalf("a reperform with new evidence: %v", err)
	}
}

// POST /dqc/exceptions: one exception per failing result, and the spec's event.
func TestException_OnePerResult_AndTheEvent(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "exc.asset", profiled(domain.CompatBackward))
	run := f.mustContractRun(c, "pop-exc", fail("amount-positive"), pass("invoice-unique"), pass("memo-present"), pass("format-ok"))
	res := resultFor(t, f, run.RunID, "amount-positive")
	raise := func(desc string) (*domain.DQIssue, error) {
		return f.s.RaiseIssue(f.ctx, orgA, domain.RaiseIssueRequest{RunID: run.RunID, ResultID: res.ResultID, Description: desc}, "test-operator", f.claim("RaiseIssue", res.ResultID+desc))
	}
	first, err := raise("amounts look wrong")
	if err != nil || first.Deduplicated {
		t.Fatalf("first: %+v %v", first, err)
	}
	// The same failing result reported again (a different key, a different description): the same exception.
	again, err := raise("still wrong, reported by another alert")
	if err != nil || !again.Deduplicated || again.IssueID != first.IssueID || again.Description != "amounts look wrong" {
		t.Fatalf("a second report of the same result: %+v %v", again, err)
	}
	var n, events int
	var severity, control, rule string
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM dq_issues WHERE result_id = $1`, res.ResultID).Scan(&n)
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*), COALESCE(max(payload->>'severity'),''), COALESCE(max(payload->>'control_id'),''), COALESCE(max(payload->>'rule_key'),'')
		FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'dqc.exception.opened'`, first.IssueID).Scan(&events, &severity, &control, &rule); err != nil {
		t.Fatal(err)
	}
	if n != 1 || events != 1 || severity != "BLOCKER" || control != c.ContractID || rule != "amount-positive" {
		t.Fatalf("issues=%d events=%d severity=%q control=%q rule=%q", n, events, severity, control, rule)
	}
	// A different failing result of the same run is a different exception.
	run2 := f.mustContractRun(c, "pop-exc-2", fail("amount-positive"), fail("invoice-unique"), pass("memo-present"), pass("format-ok"))
	r1, r2 := resultFor(t, f, run2.RunID, "amount-positive"), resultFor(t, f, run2.RunID, "invoice-unique")
	for _, r := range []domain.DQResult{r1, r2} {
		if i, err := f.s.RaiseIssue(f.ctx, orgA, domain.RaiseIssueRequest{RunID: run2.RunID, ResultID: r.ResultID, Description: "x"}, "test-operator", f.claim("RaiseIssue", r.ResultID)); err != nil || i.Deduplicated {
			t.Fatalf("a distinct result: %+v %v", i, err)
		}
	}
	// The database holds the same rule: a direct second insert for one result is refused.
	if _, err := f.admin.Exec(f.ctx, `INSERT INTO dq_issues (issue_id, tenant_id, run_id, result_id, description, raised_by) VALUES ($1, $2, $3, $4, 'dup', 'x')`,
		"dqi_"+newTestRunID()[4:], orgA, run.RunID, res.ResultID); err == nil {
		t.Fatal("the database accepted a second exception for one result")
	}
}

func TestException_RuleSetRunEventHasNoSeverity(t *testing.T) {
	f := newContractFixture(t)
	v := f.publishRuleSet(orgA, "exc-legacy", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	run, err := f.run(v, "pop-exc-legacy", domain.RuleOutcome{RuleKey: "r-a", PassCount: 90, FailCount: 10, FailureMagnitudeSum: "5"})
	if err != nil {
		t.Fatal(err)
	}
	res := resultFor(t, f, run.RunID, "r-a")
	i, err := f.s.RaiseIssue(f.ctx, orgA, domain.RaiseIssueRequest{RunID: run.RunID, ResultID: res.ResultID, Description: "x"}, "test-operator", f.claim("RaiseIssue", "legacy"))
	if err != nil {
		t.Fatal(err)
	}
	var sev *string
	var control string
	if err := f.admin.QueryRow(f.ctx, `SELECT payload->>'severity', payload->>'control_id' FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'dqc.exception.opened'`, i.IssueID).Scan(&sev, &control); err != nil {
		t.Fatal(err)
	}
	if sev != nil || control != v.VersionID {
		t.Fatalf("severity=%v control=%q: a rule-set run has no severity and its control is the rule-set version", sev, control)
	}
}

// POST /dqc/certifications: a basis conflict names the certification that already holds it.
func TestCertification_BasisConflictNamesTheExistingCertification(t *testing.T) {
	f := newContractFixture(t)
	v := f.publishRuleSet(orgA, "cert-conflict", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	observed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	mk := func() *domain.DQRun {
		r, err := f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{RuleSetVersionID: v.VersionID, PopulationRef: "pop-cc", PopulationRowCount: 100,
			PopulationContentHash: hashLike("pop-cc"), RuleOutcomes: []domain.RuleOutcome{passingOutcome("r-a")}, EvidenceAsOf: &observed}, "test-operator", f.claim("RunDQ", "cc"))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first, err := f.certify(mk())
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.certify(mk())
	var basis *domain.BasisExistsError
	if !errors.Is(err, domain.ErrCertificationBasisExists) || !errors.As(err, &basis) || basis.ExistingID != first.CertificationID {
		t.Fatalf("a conflicting basis: %v", err)
	}
}

// The same rule, held by the database: two runs cannot carry one evidence fingerprint, so two requests that race
// past the service's own check still cannot both be recorded.
func TestValidate_DatabaseRefusesTwoRunsForOneFingerprint(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "val.db", profiled(domain.CompatBackward))
	insert := `INSERT INTO dq_runs (run_id, tenant_id, contract_id, population_ref, population_row_count, population_content_hash, status, created_by, evidence_fingerprint)
		VALUES ($1, $2, $3, 'p', 1, repeat('a',64), 'Running', 'x', $4)`
	fp := hashLike("fingerprint")
	if _, err := f.admin.Exec(f.ctx, insert, newTestRunID(), orgA, c.ContractID, fp); err != nil {
		t.Fatalf("a first run with a fingerprint was refused: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, insert, newTestRunID(), orgA, c.ContractID, fp); err == nil {
		t.Fatal("a second run with the same fingerprint was accepted")
	}
	if _, err := f.admin.Exec(f.ctx, insert, newTestRunID(), orgA, c.ContractID, hashLike("another")); err != nil {
		t.Fatalf("a run with a different fingerprint was refused: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, insert, newTestRunID(), orgA, c.ContractID, "not-a-hash"); err == nil {
		t.Fatal("a malformed fingerprint was accepted")
	}
}
