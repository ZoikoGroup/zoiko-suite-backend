package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"zoiko.io/data-ingestion-svc/internal/imports"
)

// ZS-SVC-Z-001 DQC-02 / INV-11 / INV-12 / TC-07 / TC-08 / NP-16
// (docs/architecture/DQC_GAP_MATRIX.md, step E).
//
// Before this change a quarantined import item was simply retried: the retry wiped who had
// quarantined it, why, and who put it back (the item's own columns were nulled and nothing durable
// recorded it); the same permission quarantined and released, and the same person could do both;
// nothing recorded what had been corrected in the owning domain, or how re-validation ended.

const releaseController = "release-controller-1"

// quarantineScenario executes a batch of five good rows in which one item is held back by a person
// (approver, before it is sent) and one is refused by the owning domain, so the executor quarantines
// it. It returns the finished batch and the two row numbers.
func quarantineScenario(t *testing.T, e *impEnv) (b *imports.Batch, byPerson, bySystem int) {
	t.Helper()
	e.dom.records["INV-4"] = "already-there" // the owning domain already has INV-4 and refuses a duplicate
	b = e.executing(e.profile(plainSpec()), csvOf(goodRows(5, entityOne)...), declared{})
	byPerson = 2
	iMust(t, e.svc.QuarantineItem(ictx, e.actor(approver), b.BatchID, byPerson, "customer disputes this invoice", "ap-team"))
	b = e.drain(b)
	if b.Status != imports.StatusCompletedWithExceptions {
		t.Fatalf("setup: batch is %s", b.Status)
	}
	quar, err := e.svc.Items(ictx, e.tenant, b.BatchID, imports.ItemQuarantined, 0, 20)
	iMust(t, err)
	for _, it := range quar {
		if it.RowNo != byPerson {
			bySystem = it.RowNo
		}
	}
	if len(quar) != 2 || bySystem == 0 {
		t.Fatalf("setup: want 2 quarantined items (one by a person, one by the executor), got %+v", quar)
	}
	return b, byPerson, bySystem
}

func ledgerOf(t *testing.T, e *impEnv, batchID string, row int) []imports.LedgerEntry {
	t.Helper()
	v, err := e.svc.QuarantineView(ictx, e.tenant, batchID, row)
	iMust(t, err)
	return v.Ledger
}

func kinds(es []imports.LedgerEntry) []imports.LedgerKind {
	out := make([]imports.LedgerKind, len(es))
	for i, e := range es {
		out[i] = e.Kind
	}
	return out
}

func sameKinds(got []imports.LedgerKind, want ...imports.LedgerKind) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TC-07: both a person's quarantine and the executor's are recorded durably, with who and why.
func TestQuarantine_LedgerRecordsAPersonsAndTheExecutorsQuarantine(t *testing.T) {
	e := newImpEnv(t)
	b, person, system := quarantineScenario(t, e)

	p := ledgerOf(t, e, b.BatchID, person)
	if len(p) != 1 || p[0].Kind != imports.LedgerQuarantined || p[0].Actor != approver ||
		p[0].Reason != "customer disputes this invoice" || p[0].Owner != "ap-team" || p[0].CorrelationID != "corr-"+approver {
		t.Fatalf("a person's quarantine: %+v", p)
	}
	s := ledgerOf(t, e, b.BatchID, system)
	if len(s) != 1 || s[0].Kind != imports.LedgerQuarantined || s[0].Actor != imports.SystemActor ||
		s[0].Reason != "DOMAIN_REJECTED_409" || s[0].Owner != uploader {
		t.Fatalf("the executor's quarantine: %+v", s)
	}
}

// The whole governed path, end to end, on an item the owning domain refused.
func TestRelease_GovernedLifecycle_PreservesTheEvidenceAndRecordsRevalidation(t *testing.T) {
	e := newImpEnv(t)
	b, _, row := quarantineScenario(t, e)

	// A quarantined item is not simply retried.
	iIs(t, "retrying a quarantined item", e.svc.RetryItem(ictx, e.actor(approver), b.BatchID, row, "the domain is back"), imports.ErrReleaseRequired)

	// A request must say why and name the correction made in the owning domain (INV-12).
	iIs(t, "request without a reason", e.svc.RequestRelease(ictx, e.actor(uploader), b.BatchID, row, "", "CN-1"), imports.ErrValidation)
	iIs(t, "request without a correction reference", e.svc.RequestRelease(ictx, e.actor(uploader), b.BatchID, row, "fixed", "  "), imports.ErrValidation)

	// Nothing to decide yet.
	iIs(t, "approving with no request", e.svc.ApproveRelease(ictx, e.actor(releaseController), b.BatchID, row, "ok"), imports.ErrNoOpenRelease)

	iMust(t, e.svc.RequestRelease(ictx, e.actor(uploader), b.BatchID, row, "duplicate removed from the AP ledger", "AP-ticket-7731"))
	iIs(t, "a second open request", e.svc.RequestRelease(ictx, e.actor(uploader), b.BatchID, row, "again", "AP-ticket-7732"), imports.ErrReleaseOpen)

	// The requester cannot approve their own request.
	iIs(t, "self-approval by the requester", e.svc.ApproveRelease(ictx, e.actor(uploader), b.BatchID, row, "ok"), imports.ErrNotIndependent)
	if st := e.counts(b.BatchID)[imports.ItemQuarantined]; st != 2 {
		t.Fatalf("a refused approval released something: %d quarantined", st)
	}

	// The owning domain is corrected, and an independent authority releases the item.
	delete(e.dom.records, "INV-4")
	iMust(t, e.svc.ApproveRelease(ictx, e.actor(releaseController), b.BatchID, row, "AP ticket verified"))
	if got := e.batch(b.BatchID); got.Status != imports.StatusImporting {
		t.Fatalf("a batch with a released item is importing again, is %s", got.Status)
	}
	done := e.drain(e.batch(b.BatchID))
	if done.Status != imports.StatusCompletedWithExceptions { // the other quarantined item is still held
		t.Fatalf("batch after the release: %s", done.Status)
	}

	// The owning domain saw a NEW delivery, under a key of its own. Re-sending under the original key
	// would have replayed the refusal it cached, and no release could ever take effect.
	var keys []string
	for _, c := range e.dom.allCalls() {
		if n, _ := c.Body["number"].(string); n == "INV-4" {
			keys = append(keys, c.Key)
		}
	}
	if len(keys) < 2 || keys[0] == keys[len(keys)-1] || !strings.HasSuffix(keys[len(keys)-1], ":rel1") || strings.HasSuffix(keys[0], ":rel1") {
		t.Fatalf("delivery keys for the released item: %v; the release must be a new delivery under its own key", keys)
	}

	// The owning domain re-validated it, and the whole story is on the ledger.
	l := ledgerOf(t, e, b.BatchID, row)
	if !sameKinds(kinds(l), imports.LedgerQuarantined, imports.LedgerReleaseRequested, imports.LedgerReleased, imports.LedgerRevalidated) {
		t.Fatalf("ledger kinds: %v", kinds(l))
	}
	if l[0].Actor != imports.SystemActor || l[0].Reason != "DOMAIN_REJECTED_409" || l[0].Owner != uploader {
		t.Fatalf("the original quarantine was not preserved: %+v", l[0])
	}
	if l[1].Actor != uploader || l[1].CorrectionRef != "AP-ticket-7731" || l[1].Reason != "duplicate removed from the AP ledger" {
		t.Fatalf("the request: %+v", l[1])
	}
	if l[2].Actor != releaseController || l[2].AnswersLedgerID == nil || *l[2].AnswersLedgerID != l[1].LedgerID || l[2].Reason != "AP ticket verified" {
		t.Fatalf("the release: %+v", l[2])
	}
	if l[3].Actor != imports.SystemActor || l[3].RevalidationOutcome != "IMPORTED" || l[3].AnswersLedgerID == nil || *l[3].AnswersLedgerID != l[2].LedgerID {
		t.Fatalf("the re-validation: %+v", l[3])
	}
	it, err := e.st.GetItem(ictx, e.tenant, b.BatchID, row)
	iMust(t, err)
	if it.Status != imports.ItemImported || it.QuarantineReason != "" {
		t.Fatalf("item after release: %+v", it)
	}
}

// NP-16: nobody releases what they themselves quarantined, even through someone else's request.
func TestRelease_ThePersonWhoQuarantinedAnItemCannotApproveItsRelease(t *testing.T) {
	e := newImpEnv(t)
	b, person, _ := quarantineScenario(t, e) // quarantined by approver
	iMust(t, e.svc.RequestRelease(ictx, e.actor(uploader), b.BatchID, person, "dispute resolved", "credit-note CN-2041"))

	iIs(t, "release by the quarantiner", e.svc.ApproveRelease(ictx, e.actor(approver), b.BatchID, person, "ok"), imports.ErrNotIndependent)
	// ...but the quarantiner may refuse (that leaves the item exactly where it was).
	iMust(t, e.svc.ApproveRelease(ictx, e.actor(releaseController), b.BatchID, person, "credit note verified"))
}

func TestRelease_ARefusalLeavesTheItemQuarantinedAndAllowsABetterRequest(t *testing.T) {
	e := newImpEnv(t)
	b, _, row := quarantineScenario(t, e)
	iMust(t, e.svc.RequestRelease(ictx, e.actor(uploader), b.BatchID, row, "fixed", "AP-1"))
	iMust(t, e.svc.RefuseRelease(ictx, e.actor(releaseController), b.BatchID, row, "the ticket does not show a correction"))

	if e.counts(b.BatchID)[imports.ItemQuarantined] != 2 {
		t.Fatal("a refusal released an item")
	}
	iIs(t, "deciding an already-answered request", e.svc.ApproveRelease(ictx, e.actor(releaseController), b.BatchID, row, "ok"), imports.ErrNoOpenRelease)
	// A new, better-evidenced request is allowed once the first was answered.
	iMust(t, e.svc.RequestRelease(ictx, e.actor(uploader), b.BatchID, row, "fixed properly", "AP-2"))

	l := ledgerOf(t, e, b.BatchID, row)
	if !sameKinds(kinds(l), imports.LedgerQuarantined, imports.LedgerReleaseRequested, imports.LedgerReleaseRefused, imports.LedgerReleaseRequested) {
		t.Fatalf("ledger kinds: %v", kinds(l))
	}
	if l[2].Actor != releaseController || l[2].Reason != "the ticket does not show a correction" {
		t.Fatalf("the refusal: %+v", l[2])
	}
}

// TC-08: when the owning domain re-validates a released item and rejects it again, that is
// recorded, and a new quarantine episode begins — the release did not launder the item.
func TestRelease_AFailedRevalidationIsRecordedAndStartsANewEpisode(t *testing.T) {
	e := newImpEnv(t)
	b, _, row := quarantineScenario(t, e)
	iMust(t, e.svc.RequestRelease(ictx, e.actor(uploader), b.BatchID, row, "believed fixed", "AP-3"))
	// The owning domain was NOT actually corrected: INV-4 is still a duplicate.
	iMust(t, e.svc.ApproveRelease(ictx, e.actor(releaseController), b.BatchID, row, "trusting the ticket"))
	e.drain(e.batch(b.BatchID))

	l := ledgerOf(t, e, b.BatchID, row)
	if !sameKinds(kinds(l), imports.LedgerQuarantined, imports.LedgerReleaseRequested, imports.LedgerReleased,
		imports.LedgerRevalidated, imports.LedgerQuarantined) {
		t.Fatalf("ledger kinds: %v", kinds(l))
	}
	if l[3].RevalidationOutcome != "QUARANTINED" || l[4].Actor != imports.SystemActor {
		t.Fatalf("re-validation / new episode: %+v / %+v", l[3], l[4])
	}
	it, err := e.st.GetItem(ictx, e.tenant, b.BatchID, row)
	iMust(t, err)
	if it.Status != imports.ItemQuarantined {
		t.Fatalf("item = %s, want it quarantined again", it.Status)
	}
	// The new episode can itself be released, by a fresh request: the previous release does not carry over.
	iIs(t, "approving without a new request", e.svc.ApproveRelease(ictx, e.actor(releaseController), b.BatchID, row, "again"), imports.ErrNoOpenRelease)
}

func TestRelease_OnlyAQuarantinedItemCanHaveItsReleaseRequested(t *testing.T) {
	e := newImpEnv(t)
	b, _, _ := quarantineScenario(t, e)
	iIs(t, "an imported item", e.svc.RequestRelease(ictx, e.actor(uploader), b.BatchID, 1, "x", "y"), imports.ErrInvalidState)
	iIs(t, "an unknown item", e.svc.RequestRelease(ictx, e.actor(uploader), b.BatchID, 99, "x", "y"), imports.ErrItemNotFound)
}

// Quarantine state: NONE / PARTIAL / RELEASE_PENDING, with the counts behind it.
func TestQuarantineState_FollowsTheItemsAndTheReleaseRequests(t *testing.T) {
	e := newImpEnv(t)
	b, _, row := quarantineScenario(t, e)
	state := func() imports.QuarantineSummary {
		v, err := e.svc.QuarantineView(ictx, e.tenant, b.BatchID, 0)
		iMust(t, err)
		return v.Summary
	}
	if s := state(); s.State != imports.QuarantinePartial || s.QuarantinedItems != 2 || s.TotalItems != 5 || s.OpenReleaseRequests != 0 {
		t.Fatalf("two of five quarantined: %+v", s)
	}
	iMust(t, e.svc.RequestRelease(ictx, e.actor(uploader), b.BatchID, row, "fixed", "AP-4"))
	if s := state(); s.State != imports.QuarantineReleasePending || s.OpenReleaseRequests != 1 || s.QuarantinedItems != 2 {
		t.Fatalf("with an open request: %+v", s)
	}
	iMust(t, e.svc.RefuseRelease(ictx, e.actor(releaseController), b.BatchID, row, "no"))
	if s := state(); s.State != imports.QuarantinePartial || s.OpenReleaseRequests != 0 {
		t.Fatalf("after the refusal: %+v", s)
	}
}

// ── the database enforces the governance, not only the application ──────────

func TestDatabase_TheLedgerIsAppendOnlyAndEnforcesIndependence(t *testing.T) {
	e := newImpEnv(t)
	b, person, _ := quarantineScenario(t, e) // quarantined by approver
	iMust(t, e.svc.RequestRelease(ictx, e.actor(uploader), b.BatchID, person, "fixed", "AP-5"))
	l := ledgerOf(t, e, b.BatchID, person)
	req := l[len(l)-1] // the open request

	ins := func(kind, actor string, answers *int64) error {
		_, err := e.admin.Exec(ictx, `INSERT INTO import_quarantine_ledger (tenant_id, batch_id, row_no, kind, actor, reason, answers_ledger_id)
			VALUES ($1, $2, $3, $4, $5, 'because', $6)`, e.tenant, b.BatchID, person, kind, actor, answers)
		return err
	}
	// Independence, in the database: the requester and the quarantiner cannot be the releaser.
	if err := ins("RELEASED", uploader, &req.LedgerID); err == nil || !strings.Contains(err.Error(), "requested a release cannot approve") {
		t.Fatalf("the database let the requester approve: %v", err)
	}
	if err := ins("RELEASED", approver, &req.LedgerID); err == nil || !strings.Contains(err.Error(), "quarantined an item cannot approve") {
		t.Fatalf("the database let the quarantiner approve: %v", err)
	}
	// A release must answer a request for the SAME item.
	l2 := ledgerOf(t, e, b.BatchID, 0)
	var otherRowQuarantine imports.LedgerEntry
	for _, x := range l2 {
		if x.RowNo != person && x.Kind == imports.LedgerQuarantined {
			otherRowQuarantine = x
		}
	}
	if err := ins("RELEASED", releaseController, &otherRowQuarantine.LedgerID); err == nil {
		t.Fatal("the database let a release answer something that is not a request for this item")
	}
	// Proof it was the rules and not a malformed row: an independent releaser is accepted, once.
	if err := ins("RELEASED", releaseController, &req.LedgerID); err != nil {
		t.Fatalf("the database refused a valid, independent release: %v", err)
	}
	if err := ins("RELEASE_REFUSED", releaseController, &req.LedgerID); err == nil {
		t.Fatal("the database let one request be answered twice")
	}

	// Append-only, even for a superuser.
	if _, err := e.admin.Exec(ictx, `UPDATE import_quarantine_ledger SET reason = 'rewritten' WHERE ledger_id = $1`, req.LedgerID); err == nil {
		t.Fatal("the database let the ledger be edited")
	}
	if _, err := e.admin.Exec(ictx, `DELETE FROM import_quarantine_ledger WHERE ledger_id = $1`, req.LedgerID); err == nil {
		t.Fatal("the database let the ledger be deleted from")
	}
}

func TestDatabase_ALedgerEntryMustHaveTheEvidenceItsKindRequires(t *testing.T) {
	e := newImpEnv(t)
	b, person, _ := quarantineScenario(t, e)
	raw := func(kind, owner, correction string) error {
		_, err := e.admin.Exec(ictx, `INSERT INTO import_quarantine_ledger (tenant_id, batch_id, row_no, kind, actor, reason, owner, correction_ref)
			VALUES ($1, $2, $3, $4, 'someone', 'because', NULLIF($5, ''), NULLIF($6, ''))`, e.tenant, b.BatchID, person, kind, owner, correction)
		return err
	}
	// A release request with no correction reference is refused by a CHECK (INV-12)...
	if err := raw("RELEASE_REQUESTED", "", ""); err == nil {
		t.Fatal("the database accepted a release request with no correction reference")
	}
	// ...and the same request with one is accepted, so it was the missing evidence that refused it.
	if err := raw("RELEASE_REQUESTED", "", "AP-ticket-9"); err != nil {
		t.Fatalf("the database refused a well-formed request: %v", err)
	}
	// A quarantine needs an owner.
	if err := raw("QUARANTINED", "", ""); err == nil {
		t.Fatal("the database accepted a quarantine with no owner")
	}
}

// Another tenant sees nothing of this batch's ledger.
func TestQuarantineLedger_IsTenantIsolated(t *testing.T) {
	e := newImpEnv(t)
	b, person, _ := quarantineScenario(t, e)
	other := "99999999-9999-9999-9999-999999999999"
	got, err := e.st.QuarantineLedger(ictx, other, b.BatchID, person)
	iMust(t, err)
	if len(got) != 0 {
		t.Fatalf("another tenant read %d ledger entries", len(got))
	}
	q, total, open, err := e.st.QuarantineCounts(ictx, other, b.BatchID)
	iMust(t, err)
	if q != 0 || total != 0 || open != 0 {
		t.Fatalf("another tenant counted %d/%d/%d", q, total, open)
	}
	if err := e.st.RequestRelease(ictx, other, b.BatchID, person, "mallory", "r", "c", "", nil); !errors.Is(err, imports.ErrItemNotFound) {
		t.Fatalf("another tenant requested a release: %v", err)
	}
}

// ── the migration's backfill ────────────────────────────────────────────────

// Items that are quarantined when migration 000003 runs have only their own columns as evidence.
// The SQL is taken from the shipped migration file (not copied) and replayed over real rows,
// including the blank values the old CHECK allowed.
func TestMigration000003_BackfillsALedgerRowForItemsQuarantinedBeforeItExisted(t *testing.T) {
	e := newImpEnv(t)
	b, person, system := quarantineScenario(t, e)

	// Put the ledger back into the state it had before the migration: no rows for these items.
	if _, err := e.admin.Exec(ictx, `ALTER TABLE import_quarantine_ledger DISABLE TRIGGER trg_import_quarantine_ledger_immutable`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.admin.Exec(ictx, `ALTER TABLE import_quarantine_ledger ENABLE TRIGGER trg_import_quarantine_ledger_immutable`)
	})
	if _, err := e.admin.Exec(ictx, `DELETE FROM import_quarantine_ledger WHERE batch_id = $1`, b.BatchID); err != nil {
		t.Fatalf("remove the ledger rows: %v", err)
	}
	// One item has a blank reason and a blank owner, as the old CHECK permitted.
	if _, err := e.admin.Exec(ictx, `UPDATE import_items SET quarantine_reason = '  ', quarantine_owner = '' WHERE batch_id = $1 AND row_no = $2`,
		b.BatchID, system); err != nil {
		t.Fatalf("blank an item's evidence: %v", err)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations/000003_quarantine_governance.up.sql"))
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	sql := string(raw)
	start := strings.Index(sql, "INSERT INTO import_quarantine_ledger (tenant_id, batch_id, row_no, kind, actor, reason, owner, occurred_at)")
	if start < 0 {
		t.Fatal("the migration no longer contains the backfill statement this test replays")
	}
	end := strings.Index(sql[start:], ";\n")
	backfill := sql[start : start+end+1]
	for i := 0; i < 2; i++ { // twice: it must be idempotent
		if _, err := e.admin.Exec(ictx, backfill); err != nil {
			t.Fatalf("run the shipped backfill (pass %d): %v", i+1, err)
		}
	}

	p := ledgerOf(t, e, b.BatchID, person)
	if len(p) != 1 || p[0].Kind != imports.LedgerQuarantined || p[0].Actor != approver || p[0].Owner != "ap-team" ||
		p[0].Reason != "customer disputes this invoice" {
		t.Fatalf("a person's quarantine, backfilled from the item: %+v", p)
	}
	s := ledgerOf(t, e, b.BatchID, system)
	if len(s) != 1 {
		t.Fatalf("the backfill is not idempotent or missed an item: %+v", s)
	}
	if s[0].Actor != imports.SystemActor || s[0].Owner != "unassigned" || !strings.Contains(s[0].Reason, "no reason was recorded") {
		t.Fatalf("blank evidence must be named as such, never invented: %+v", s[0])
	}
}
