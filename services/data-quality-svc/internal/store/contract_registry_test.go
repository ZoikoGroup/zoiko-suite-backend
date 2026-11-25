package store_test

import (
	"errors"
	"testing"

	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/store"
)

// ZS-SVC-Z-001 DQC-01 contract registry (docs/architecture/DQC_GAP_MATRIX.md, step F), against real
// Postgres as the NOSUPERUSER NOBYPASSRLS role.

const (
	author    = "contract-author"
	reviewer  = "contract-reviewer"
	publisher = "contract-publisher"
)

func contractBody(mode domain.Compatibility) domain.ContractBody {
	return domain.ContractBody{
		Schema: []domain.SchemaField{
			{Name: "amount", Type: "decimal"},
			{Name: "invoice_no", Type: "string"},
			{Name: "memo", Type: "string", Nullable: true},
		},
		Predicates: []domain.SemanticPredicate{{Name: "positive", Expression: "amount > 0", Fields: []string{"amount"}}},
		Rules: []domain.ContractRule{
			{RuleKey: "amount-positive", Dimension: "validity", Severity: domain.SeverityBlocker, Predicate: "positive"},
			{RuleKey: "memo-present", Dimension: "completeness", Severity: domain.SeverityInfo},
		},
		Compatibility: mode,
	}
}

func (f *fixture) create(org, key string, body domain.ContractBody) (*domain.ContractVersion, error) {
	f.t.Helper()
	return f.s.CreateContract(f.ctx, org, domain.CreateContractRequest{AssetKey: key, AssetKind: "dataset", Body: body}, author, f.claim("CreateContract", key))
}

func (f *fixture) mustCreate(org, key string, body domain.ContractBody) *domain.ContractVersion {
	f.t.Helper()
	v, err := f.create(org, key, body)
	if err != nil {
		f.t.Fatalf("create: %v", err)
	}
	return v
}

func (f *fixture) submit(v *domain.ContractVersion, actor string) (*domain.ContractVersion, error) {
	return f.s.SubmitContract(f.ctx, v.TenantID, v.AssetID, v.VersionNumber, actor, f.claim("SubmitContract", v.ContractID))
}

func (f *fixture) publish(v *domain.ContractVersion, actor string, ack bool) (*domain.ContractVersion, error) {
	return f.s.PublishContract(f.ctx, v.TenantID, v.AssetID, v.VersionNumber, actor, domain.PublishContractRequest{AcknowledgeBreaking: ack}, f.claim("PublishContract", v.ContractID))
}

// published runs a version through DRAFT -> REVIEW -> PUBLISHED with independent people.
func (f *fixture) published(org, key string, body domain.ContractBody) *domain.ContractVersion {
	f.t.Helper()
	v := f.mustCreate(org, key, body)
	if _, err := f.submit(v, reviewer); err != nil {
		f.t.Fatalf("submit: %v", err)
	}
	p, err := f.publish(v, publisher, false)
	if err != nil {
		f.t.Fatalf("publish: %v", err)
	}
	return p
}

func TestContract_FullLifecycle(t *testing.T) {
	f := newContractFixture(t)
	v := f.mustCreate(orgA, "ap.invoice", contractBody(domain.CompatBackward))
	if v.Status != domain.ContractDraft || v.VersionNumber != 1 || v.AssetKey != "ap.invoice" {
		t.Fatalf("created: %+v", v)
	}
	if len(v.ContentSHA256) != 64 {
		t.Fatalf("content hash %q", v.ContentSHA256)
	}
	if got := v.Body.CertificationProfile.MandatoryRules; len(got) != 1 || got[0] != "amount-positive" {
		t.Fatalf("the default certification profile was not stored: %v", got)
	}
	sub, err := f.submit(v, reviewer)
	if err != nil || sub.Status != domain.ContractReview || *sub.SubmittedBy != reviewer {
		t.Fatalf("submit: %+v %v", sub, err)
	}
	pub, err := f.publish(v, publisher, false)
	if err != nil || pub.Status != domain.ContractPublished || *pub.PublishedBy != publisher || pub.PublishedAt == nil {
		t.Fatalf("publish: %+v %v", pub, err)
	}
	if pub.ComparedToVer != nil {
		t.Fatalf("the first version has nothing to be compared with: %+v", pub)
	}
	dep, err := f.s.DeprecateContract(f.ctx, orgA, v.AssetID, 1, publisher, "superseded by v2", f.claim("Deprecate", v.ContractID))
	if err != nil || dep.Status != domain.ContractDeprecated || dep.DeprecatedAt == nil {
		t.Fatalf("deprecate: %+v %v", dep, err)
	}
	ret, err := f.s.RetireContract(f.ctx, orgA, v.AssetID, 1, publisher, "no consumers left", f.claim("Retire", v.ContractID))
	if err != nil || ret.Status != domain.ContractRetired || ret.RetiredAt == nil {
		t.Fatalf("retire: %+v %v", ret, err)
	}
	// Retired is not deleted: the exact version is still readable, with its content and history.
	got, err := f.s.GetContractVersion(f.ctx, orgA, v.AssetID, 1)
	if err != nil || got.Status != domain.ContractRetired || got.ContentSHA256 != v.ContentSHA256 || got.PublishedBy == nil {
		t.Fatalf("retired version: %+v %v", got, err)
	}
}

// Maker-checker: the author cannot publish, the submitter cannot publish, and an independent
// person can. The last assertion proves it was independence that decided it.
func TestContract_PublisherMustBeIndependent(t *testing.T) {
	f := newContractFixture(t)
	v := f.mustCreate(orgA, "mc.asset", contractBody(domain.CompatBackward))
	if _, err := f.submit(v, reviewer); err != nil {
		t.Fatal(err)
	}
	if _, err := f.publish(v, author, false); !errors.Is(err, domain.ErrContractNotIndependent) {
		t.Fatalf("the author published their own contract: %v", err)
	}
	if _, err := f.publish(v, reviewer, false); !errors.Is(err, domain.ErrContractNotIndependent) {
		t.Fatalf("the submitter published: %v", err)
	}
	got, _ := f.s.GetContractVersion(f.ctx, orgA, v.AssetID, 1)
	if got.Status != domain.ContractReview {
		t.Fatalf("a refused publish changed the status to %s", got.Status)
	}
	if _, err := f.publish(v, publisher, false); err != nil {
		t.Fatalf("an independent publisher was refused: %v", err)
	}
}

// The same rule, held by the database: a statement that skips the application is still refused.
func TestContract_DatabaseRefusesSelfPublication(t *testing.T) {
	f := newContractFixture(t)
	v := f.mustCreate(orgA, "mc.db", contractBody(domain.CompatBackward))
	if _, err := f.submit(v, reviewer); err != nil {
		t.Fatal(err)
	}
	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE dqc_contract_versions SET status='PUBLISHED', published_by=created_by, published_at=now() WHERE contract_id=$1`, v.ContractID); err == nil {
		t.Fatal("the database let the author publish")
	}
}

func TestContract_OnlyForwardTransitions(t *testing.T) {
	f := newContractFixture(t)
	v := f.mustCreate(orgA, "fwd.asset", contractBody(domain.CompatBackward))
	// A DRAFT cannot be published without review.
	if _, err := f.publish(v, publisher, false); !errors.Is(err, domain.ErrContractTransition) {
		t.Fatalf("published a DRAFT: %v", err)
	}
	if _, err := f.submit(v, reviewer); err != nil {
		t.Fatal(err)
	}
	if _, err := f.submit(v, reviewer); !errors.Is(err, domain.ErrContractTransition) {
		t.Fatalf("submitted twice: %v", err)
	}
	pub, err := f.publish(v, publisher, false)
	if err != nil {
		t.Fatal(err)
	}
	// A PUBLISHED version cannot be retired directly, nor deprecated twice, nor published again.
	if _, err := f.s.RetireContract(f.ctx, orgA, pub.AssetID, 1, publisher, "x", f.claim("R", "r1")); !errors.Is(err, domain.ErrContractTransition) {
		t.Fatalf("retired a PUBLISHED version directly: %v", err)
	}
	if _, err := f.publish(v, "someone-else", false); !errors.Is(err, domain.ErrContractTransition) {
		t.Fatalf("re-published: %v", err)
	}
	if _, err := f.s.DeprecateContract(f.ctx, orgA, pub.AssetID, 1, publisher, "old", f.claim("D", "d1")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DeprecateContract(f.ctx, orgA, pub.AssetID, 1, publisher, "again", f.claim("D", "d2")); !errors.Is(err, domain.ErrContractTransition) {
		t.Fatalf("deprecated twice: %v", err)
	}
	// A reason is mandatory for leaving the lifecycle.
	if _, err := f.s.RetireContract(f.ctx, orgA, pub.AssetID, 1, publisher, "", f.claim("R", "r2")); !errors.Is(err, domain.ErrContractInvalid) {
		t.Fatalf("retired with no reason: %v", err)
	}
}

// NP-02: a published contract cannot be edited in place - not by the service, and not by SQL.
func TestContract_PublishedContentCannotBeEdited(t *testing.T) {
	f := newContractFixture(t)
	p := f.published(orgA, "imm.asset", contractBody(domain.CompatBackward))
	for name, stmt := range map[string]string{
		"body":            `UPDATE dqc_contract_versions SET body = '{}'::jsonb WHERE contract_id = $1`,
		"content hash":    `UPDATE dqc_contract_versions SET content_sha256 = repeat('0',64) WHERE contract_id = $1`,
		"published_by":    `UPDATE dqc_contract_versions SET published_by = 'someone-else' WHERE contract_id = $1`,
		"status backward": `UPDATE dqc_contract_versions SET status = 'DRAFT' WHERE contract_id = $1`,
		"delete":          `DELETE FROM dqc_contract_versions WHERE contract_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := f.app.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(f.ctx) //nolint:errcheck
			if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(f.ctx, stmt, p.ContractID); err == nil {
				t.Fatalf("the database allowed an edit of a published contract (%s)", name)
			}
		})
	}
	// Proof that the guard is about content, not about everything: the lifecycle still moves forward.
	if _, err := f.s.DeprecateContract(f.ctx, orgA, p.AssetID, 1, publisher, "retiring", f.claim("D", "imm")); err != nil {
		t.Fatalf("a legitimate transition was refused: %v", err)
	}
}

// A change is a new version; the old one is untouched and still read exactly.
func TestContract_ChangeIsANewVersion_AndExactReadsNeverUseLatest(t *testing.T) {
	f := newContractFixture(t)
	v1 := f.published(orgA, "ver.asset", contractBody(domain.CompatBackward))
	b2 := contractBody(domain.CompatBackward)
	b2.Schema = append(b2.Schema, domain.SchemaField{Name: "note", Type: "string", Nullable: true})
	v2 := f.mustCreate(orgA, "ver.asset", b2)
	if v2.VersionNumber != 2 || v2.AssetID != v1.AssetID || v2.ContractID == v1.ContractID {
		t.Fatalf("v2 = %+v", v2)
	}
	got1, err := f.s.GetContractVersion(f.ctx, orgA, v1.AssetID, 1)
	if err != nil || got1.ContentSHA256 != v1.ContentSHA256 || len(got1.Body.Schema) != 3 {
		t.Fatalf("v1 changed: %+v %v", got1, err)
	}
	if _, err := f.s.GetContractVersion(f.ctx, orgA, v1.AssetID, 99); !errors.Is(err, domain.ErrContractVersionUnknown) {
		t.Fatalf("unknown version (NP-01 / DQC-002): %v", err)
	}
	if _, err := f.s.GetContractVersion(f.ctx, orgA, "dca_00000000-0000-0000-0000-000000000000", 1); !errors.Is(err, domain.ErrContractNotFound) {
		t.Fatalf("unknown asset (DQC-001): %v", err)
	}
	all, err := f.s.ListContractVersions(f.ctx, orgA, v1.AssetID)
	if err != nil || len(all) != 2 || all[0].VersionNumber != 1 || all[1].VersionNumber != 2 {
		t.Fatalf("list: %+v %v", all, err)
	}
}

func TestContract_OnlyOneOpenDraftPerAsset(t *testing.T) {
	f := newContractFixture(t)
	v := f.mustCreate(orgA, "draft.asset", contractBody(domain.CompatBackward))
	if _, err := f.create(orgA, "draft.asset", contractBody(domain.CompatBackward)); !errors.Is(err, domain.ErrContractDraftOpen) {
		t.Fatalf("a second open draft was created: %v", err)
	}
	// Withdrawing the draft (RETIRED without ever publishing) frees the asset.
	if _, err := f.s.RetireContract(f.ctx, orgA, v.AssetID, 1, author, "abandoned", f.claim("R", "w")); err != nil {
		t.Fatal(err)
	}
	v2, err := f.create(orgA, "draft.asset", contractBody(domain.CompatBackward))
	if err != nil || v2.VersionNumber != 2 {
		t.Fatalf("a new draft after withdrawal: %+v %v", v2, err)
	}
}

func TestContract_AssetIdentity(t *testing.T) {
	f := newContractFixture(t)
	f.mustCreate(orgA, "id.asset", contractBody(domain.CompatBackward))
	_, err := f.s.CreateContract(f.ctx, orgA, domain.CreateContractRequest{AssetKey: "id.asset", AssetKind: "stream", Body: contractBody(domain.CompatBackward)}, author, f.claim("C", "k"))
	if !errors.Is(err, domain.ErrContractAssetKindClash) {
		t.Fatalf("the same key with a different kind: %v", err)
	}
	for _, key := range []string{"", "Has Caps", "../x", "-lead"} {
		if _, err := f.create(orgA, key, contractBody(domain.CompatBackward)); !errors.Is(err, domain.ErrContractInvalid) {
			t.Fatalf("asset key %q accepted: %v", key, err)
		}
	}
	// Two tenants can use the same key without seeing each other.
	other := f.mustCreate(orgB, "id.asset", contractBody(domain.CompatBackward))
	mine, _ := f.s.ListContractVersions(f.ctx, orgA, other.AssetID)
	if len(mine) != 0 {
		t.Fatalf("tenant A listed tenant B's asset: %+v", mine)
	}
	if _, err := f.s.GetContractVersion(f.ctx, orgA, other.AssetID, 1); err == nil {
		t.Fatal("tenant A read tenant B's contract")
	}
}

// NP-03 end to end: a required field disappears; publishing is refused and carries the evidence.
func TestContract_BreakingChangeBlocksPublication(t *testing.T) {
	f := newContractFixture(t)
	f.published(orgA, "brk.asset", contractBody(domain.CompatBackward))
	b2 := contractBody(domain.CompatBackward)
	b2.Schema = b2.Schema[1:] // "amount" disappears; the rule that reads it must go too
	b2.Predicates = nil
	b2.Rules = []domain.ContractRule{{RuleKey: "memo-present", Dimension: "completeness", Severity: domain.SeverityInfo}}
	v2 := f.mustCreate(orgA, "brk.asset", b2)
	if _, err := f.submit(v2, reviewer); err != nil {
		t.Fatal(err)
	}
	_, err := f.publish(v2, publisher, true) // acknowledging does not help under BACKWARD
	var inc *store.IncompatibleError
	if !errors.Is(err, domain.ErrSchemaIncompatible) || !errors.As(err, &inc) {
		t.Fatalf("want DQC-003 with evidence, got %v", err)
	}
	if len(inc.Report.Breaking) != 1 || inc.Report.Breaking[0].Kind != domain.ErrKindFieldRemoved || inc.Report.Breaking[0].Field != "amount" {
		t.Fatalf("evidence: %+v", inc.Report)
	}
	if got, _ := f.s.GetContractVersion(f.ctx, orgA, v2.AssetID, 2); got.Status != domain.ContractReview {
		t.Fatalf("a blocked publication moved the version to %s", got.Status)
	}
}

// Under NONE a breaking change is allowed, but only when it is acknowledged, and the evidence is kept.
func TestContract_BreakingChangeUnderNoneNeedsAcknowledgement(t *testing.T) {
	f := newContractFixture(t)
	f.published(orgA, "none.asset", contractBody(domain.CompatBackward))
	b2 := contractBody(domain.CompatNone)
	b2.Schema[0].Type = "integer"
	v2 := f.mustCreate(orgA, "none.asset", b2)
	if _, err := f.submit(v2, reviewer); err != nil {
		t.Fatal(err)
	}
	if _, err := f.publish(v2, publisher, false); !errors.Is(err, domain.ErrBreakingNotAcknowledged) {
		t.Fatalf("an unacknowledged breaking change under NONE was published: %v", err)
	}
	p, err := f.publish(v2, publisher, true)
	if err != nil {
		t.Fatalf("an acknowledged breaking change was refused: %v", err)
	}
	if p.ComparedToVer == nil || *p.ComparedToVer != 1 || len(p.Compatibility) == 0 {
		t.Fatalf("the compatibility evidence was not stored: %+v", p)
	}
}

func TestContract_CompatibleChangePublishesAndStoresEvidence(t *testing.T) {
	f := newContractFixture(t)
	f.published(orgA, "ok.asset", contractBody(domain.CompatFull))
	b2 := contractBody(domain.CompatFull)
	b2.Schema = append(b2.Schema, domain.SchemaField{Name: "note", Type: "string", Nullable: true})
	v2 := f.mustCreate(orgA, "ok.asset", b2)
	if _, err := f.submit(v2, reviewer); err != nil {
		t.Fatal(err)
	}
	p, err := f.publish(v2, publisher, false)
	if err != nil || p.ComparedToVer == nil || *p.ComparedToVer != 1 {
		t.Fatalf("publish: %+v %v", p, err)
	}
}

func TestContract_CompatibilityCheckIsADryRunAgainstAnExactVersion(t *testing.T) {
	f := newContractFixture(t)
	v1 := f.published(orgA, "dry.asset", contractBody(domain.CompatBackward))
	cand := contractBody(domain.CompatBackward)
	cand.Schema[1].Type = "integer"
	rep, err := f.s.CheckContractCompatibility(f.ctx, orgA, v1.AssetID, domain.CompatibilityCheckRequest{AgainstVersion: 1, Candidate: cand})
	if err != nil || rep.Compatible || len(rep.Breaking) != 1 || rep.AgainstVersion != 1 {
		t.Fatalf("report: %+v %v", rep, err)
	}
	// "No version named" is refused rather than resolved to the latest.
	if _, err := f.s.CheckContractCompatibility(f.ctx, orgA, v1.AssetID, domain.CompatibilityCheckRequest{Candidate: cand}); !errors.Is(err, domain.ErrContractInvalid) {
		t.Fatalf("a check with no against_version: %v", err)
	}
	if _, err := f.s.CheckContractCompatibility(f.ctx, orgA, v1.AssetID, domain.CompatibilityCheckRequest{AgainstVersion: 7, Candidate: cand}); !errors.Is(err, domain.ErrContractVersionUnknown) {
		t.Fatalf("an unknown version: %v", err)
	}
	// It wrote nothing.
	if all, _ := f.s.ListContractVersions(f.ctx, orgA, v1.AssetID); len(all) != 1 {
		t.Fatalf("the dry run created a version: %d", len(all))
	}
	// A DRAFT is not a base to compare against.
	d := f.mustCreate(orgA, "dry.draft", contractBody(domain.CompatBackward))
	if _, err := f.s.CheckContractCompatibility(f.ctx, orgA, d.AssetID, domain.CompatibilityCheckRequest{AgainstVersion: 1, Candidate: cand}); !errors.Is(err, domain.ErrContractNotPublishedBase) {
		t.Fatalf("compared against a DRAFT: %v", err)
	}
}

// Each lifecycle step writes exactly one dqc.* event in the same transaction as the change.
func TestContract_EventsAreWrittenWithTheChange(t *testing.T) {
	f := newContractFixture(t)
	v := f.published(orgA, "evt.asset", contractBody(domain.CompatBackward))
	if _, err := f.s.DeprecateContract(f.ctx, orgA, v.AssetID, 1, publisher, "old", f.claim("D", "evt")); err != nil {
		t.Fatal(err)
	}
	rows, err := f.admin.Query(f.ctx, `SELECT event_type FROM outbox_events WHERE aggregate_id = $1 ORDER BY created_at, event_type`, v.ContractID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		got[e]++
	}
	for _, want := range []string{"dqc.contract.created", "dqc.contract.submitted", "dqc.contract.published", "dqc.contract.deprecated"} {
		if got[want] != 1 {
			t.Fatalf("event %s written %d times; all: %v", want, got[want], got)
		}
	}
	// A refused step writes no event.
	before := got["dqc.contract.published"]
	if _, err := f.publish(v, "x", false); err == nil {
		t.Fatal("re-publish succeeded")
	}
	var n int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='dqc.contract.published'`, v.ContractID).Scan(&n)
	if n != before {
		t.Fatalf("a refused publish wrote an event: %d", n)
	}
}

func TestContract_IdempotencyReplayAndReuse(t *testing.T) {
	f := newContractFixture(t)
	claim := f.claim("CreateContract", "idem.asset")
	req := domain.CreateContractRequest{AssetKey: "idem.asset", AssetKind: "dataset", Body: contractBody(domain.CompatBackward)}
	first, err := f.s.CreateContract(f.ctx, orgA, req, author, claim)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.CreateContract(f.ctx, orgA, req, author, claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) {
		t.Fatalf("a replay: %v", err)
	}
	if replay.ResourceID != first.ContractID {
		t.Fatalf("the replay named %s, want the original %s", replay.ResourceID, first.ContractID)
	}
	claim.RequestSHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if _, err := f.s.CreateContract(f.ctx, orgA, req, author, claim); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
		t.Fatalf("the key reused for a different request: %v", err)
	}
}

// newContractFixture starts each test from an empty registry: the test database persists between runs and
// asset keys are tenant-unique, so a previous run's assets would otherwise collide with this one's.
func newContractFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	if _, err := f.admin.Exec(f.ctx, `TRUNCATE dqc_contract_versions, dqc_assets CASCADE`); err != nil {
		t.Fatalf("reset registry: %v", err)
	}
	return f
}
