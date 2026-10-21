package store_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/data-ingestion-svc/internal/imports"
)

func iMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func iIs(t *testing.T, what string, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: got %v, want %v", what, err, target)
	}
}

func sumControl(rows int) string { return fmt.Sprintf("%d", rows*10) }

// ── profiles ────────────────────────────────────────────────────────────────

func TestImportProfile_PublishCertifyAndImmutability(t *testing.T) {
	e := newImpEnv(t)
	_, err := e.svc.PublishProfile(ictx, e.actor(platAuth), "tenant-made", plainSpec())
	iIs(t, "a tenant publishing a platform profile", err, imports.ErrNotPlatformScope)

	p, err := e.svc.PublishProfile(ictx, e.platform(platAuth), "prof-"+strings.ToLower(e.tenant[:8]), plainSpec())
	iMust(t, err)
	if p.Status != imports.ProfileDraft || p.Version != 1 || p.SpecHash == "" {
		t.Fatalf("%+v", p)
	}
	// nothing can be imported under a draft
	if _, err := e.create(p, csvOf("INV-1,le-1,1"), declared{}); !errors.Is(err, imports.ErrProfileNotCertified) {
		t.Errorf("a draft profile accepted a batch: %v", err)
	}
	_, err = e.svc.CertifyProfile(ictx, e.platform(platAuth), p.Key, p.Version)
	iIs(t, "self-certification", err, imports.ErrProfileSelfCertify)

	unknown := plainSpec()
	unknown.Target.Key = "not-configured"
	u, err := e.svc.PublishProfile(ictx, e.platform(platAuth), "unk-"+strings.ToLower(e.tenant[:8]), unknown)
	iMust(t, err)
	_, err = e.svc.CertifyProfile(ictx, e.platform(platCert), u.Key, u.Version)
	iIs(t, "a target this deployment does not have", err, imports.ErrTargetNotConfigured)

	c, err := e.svc.CertifyProfile(ictx, e.platform(platCert), p.Key, p.Version)
	iMust(t, err)
	if c.Status != imports.ProfileCertified || c.CertifiedBy != platCert {
		t.Fatalf("%+v", c)
	}
	// the database refuses what the service would never do
	for name, q := range map[string]string{
		"change a certified spec": `UPDATE import_profiles SET spec = '{}'::jsonb WHERE profile_key = $1`,
		"flip the material flag":  `UPDATE import_profiles SET material = NOT material WHERE profile_key = $1`,
		"certify as the author":   `UPDATE import_profiles SET certified_by = created_by WHERE profile_key = $1`,
		"delete":                  `DELETE FROM import_profiles WHERE profile_key = $1`,
		"go back to draft":        `UPDATE import_profiles SET status = 'DRAFT' WHERE profile_key = $1`,
		"rename the key":          `UPDATE import_profiles SET profile_key = 'other-key-xx' WHERE profile_key = $1`,
	} {
		if _, err := e.admin.Exec(ictx, q, p.Key); err == nil {
			t.Errorf("%s: the database allowed it", name)
		}
	}
	// a new version, not an edit
	p2, err := e.svc.PublishProfile(ictx, e.platform(platAuth), p.Key, plainSpec())
	iMust(t, err)
	if p2.Version != 2 {
		t.Fatalf("version %d", p2.Version)
	}
	// withdrawing stops new batches
	_, err = e.svc.RetireProfile(ictx, e.platform(platCert), p.Key, 1, imports.ProfileWithdrawn)
	iMust(t, err)
	if _, err := e.create(c, csvOf("INV-1,le-1,1"), declared{}); !errors.Is(err, imports.ErrProfileNotCertified) {
		t.Errorf("a withdrawn profile accepted a batch: %v", err)
	}
	// profiles are readable by every tenant, writable only from the platform
	if _, err := e.svc.GetProfile(ictx, p.Key, 1); err != nil {
		t.Errorf("a tenant must be able to read a profile: %v", err)
	}
	if _, err := e.app.Exec(ictx, `INSERT INTO import_profiles (profile_key, version, import_type, material, spec, spec_hash, created_by)
		VALUES ('sneaky-profile', 1, 'x', false, '{}', repeat('a', 64), 'tenant')`); err == nil {
		t.Error("the application role wrote a profile outside the platform scope")
	}
}

// ── the whole path, on a material profile ───────────────────────────────────

func TestImport_MaterialHappyPathToReconciled(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(materialSpec())
	content := csvOf(goodRows(25, entityOne)...)
	b, err := e.create(p, content, declared{rows: ip(25), control: sumControl(25)})
	iMust(t, err)
	if b.Status != imports.StatusCreated || !b.Material {
		t.Fatalf("%+v", b)
	}

	// upload out of order, then resume: the same chunk again is a no-op, not a second copy
	n := e.upload(b, content, 100, true)
	again, err := e.svc.UploadChunk(ictx, e.actor(uploader), b.BatchID, 0, []byte(content[:100]), sha([]byte(content[:100])))
	iMust(t, err)
	if again {
		t.Error("the same chunk again must not be stored twice")
	}
	state, err := e.svc.UploadState(ictx, e.tenant, b.BatchID)
	iMust(t, err)
	if len(state) != n {
		t.Fatalf("%d chunks stored, want %d", len(state), n)
	}
	sealed, err := e.svc.Seal(ictx, e.actor(uploader), b.BatchID, n)
	iMust(t, err)
	if sealed.Status != imports.StatusUploaded || sealed.ContentSHA256 != sha([]byte(content)) || sealed.ChainHash == "" || sealed.ChunkCount != n {
		t.Fatalf("%+v", sealed)
	}
	r, err := e.svc.RequestValidation(ictx, e.actor(uploader), b.BatchID)
	iMust(t, err)
	if r.ValidateRequestedAt == nil {
		t.Fatal("validation was not requested")
	}
	// the worker, not the request, runs the pipeline
	if done, failed := e.val.RunOnce(ictx); done == 0 || failed {
		t.Fatalf("the validation worker did nothing (%d, failed=%v)", done, failed)
	}
	v := e.batch(b.BatchID)
	if v.Status != imports.StatusValidated || v.RowCount != 25 || v.ValidRows != 25 || v.InvalidRows != 0 || v.ComputedControl != "250" ||
		v.ScanVerdict != "CLEAN" || !strings.Contains(v.ScanEngine, "builtin-content-checks") || v.ScannedAt == nil || v.MappingHash == "" {
		t.Fatalf("%+v", v)
	}
	pv, err := e.svc.Preview(ictx, e.actor(uploader), b.BatchID)
	iMust(t, err)
	if pv.Status != imports.StatusPreviewed || pv.Preview == nil || pv.PreviewFingerprint == "" || pv.Preview.WillBeSent != 25 || len(pv.Preview.SampleValid) != 20 ||
		pv.Preview.ControlTotal != "250" || pv.Preview.Material != true {
		t.Fatalf("%+v", pv.Preview)
	}

	// a material import is approved by someone else, for exactly what was previewed
	_, err = e.svc.Approve(ictx, e.actor(uploader), b.BatchID, pv.PreviewFingerprint, "me")
	iIs(t, "the creator approving a material import", err, imports.ErrSelfApproval)
	_, err = e.svc.Approve(ictx, e.actor(approver), b.BatchID, strings.Repeat("0", 64), "wrong")
	iIs(t, "approving content other than what was previewed", err, imports.ErrPreviewMismatch)
	_, err = e.svc.Approve(ictx, e.actor(approver), b.BatchID, "", "empty")
	iIs(t, "approving with no fingerprint", err, imports.ErrPreviewMismatch)
	if e.batch(b.BatchID).Status != imports.StatusPreviewed {
		t.Fatal("refused approvals must change nothing")
	}
	ap, err := e.svc.Approve(ictx, e.actor(approver), b.BatchID, pv.PreviewFingerprint, "agreed with the ledger")
	iMust(t, err)
	if ap.Status != imports.StatusApproved || ap.ApprovedBy != approver || ap.ApprovedFingerprint != pv.PreviewFingerprint {
		t.Fatalf("%+v", ap)
	}
	if e.dom.callCount() != 0 {
		t.Fatal("nothing may reach the owning domain before execution")
	}

	ex, err := e.svc.Execute(ictx, e.actor(executor), b.BatchID)
	iMust(t, err)
	if ex.Status != imports.StatusImporting || ex.ExecutedBy != executor {
		t.Fatalf("%+v", ex)
	}
	done := e.drain(ex)
	if done.Status != imports.StatusCompleted || done.ImportedItems != 25 || done.QuarantinedItems != 0 || done.FailedItems != 0 {
		t.Fatalf("%+v", done)
	}
	if e.dom.recordCount() != 25 || e.dom.callCount() != 25 {
		t.Fatalf("%d records from %d calls: each item must reach the domain exactly once", e.dom.recordCount(), e.dom.callCount())
	}
	// every command carried the contract the owning domains enforce, and the item's own key
	for _, c := range e.dom.allCalls() {
		h := c.Header
		if h.Get("Idempotency-Key") == "" || !strings.HasPrefix(h.Get("Idempotency-Key"), "imp-") || h.Get("X-Tenant-Id") != e.tenant ||
			h.Get("X-Legal-Entity-Id") != entityOne || h.Get("X-Principal-Id") != executor || h.Get("X-Source-Channel") != "import" ||
			h.Get("X-Source-System") != "erp-x" || h.Get("X-Request-Id") == "" || h.Get("X-Correlation-Id") == "" {
			t.Fatalf("headers: %v", h)
		}
		if c.Body["origin"] != "import" || c.Body["legal_entity_id"] != entityOne || c.Body["number"] == nil || c.Body["amount"] == nil {
			t.Fatalf("body: %v", c.Body)
		}
		if _, leaked := c.Body["entity"]; leaked {
			t.Fatal("only mapped fields are sent")
		}
	}
	items, err := e.svc.Items(ictx, e.tenant, b.BatchID, imports.ItemImported, 0, 100)
	iMust(t, err)
	for _, it := range items {
		if it.DomainStatus == nil || *it.DomainStatus != 201 || !strings.HasPrefix(it.ResultRef, "dom-") {
			t.Fatalf("an imported item must carry the domain's answer: %+v", it)
		}
	}
	ct, err := e.svc.ControlTotals(ictx, e.tenant, b.BatchID)
	iMust(t, err)
	if ct.Declared != "250" || ct.Computed != "250" || ct.Imported != "250" {
		t.Fatalf("%+v", ct)
	}

	rec, err := e.svc.Reconcile(ictx, e.actor(approver), b.BatchID)
	iMust(t, err)
	if rec.Status != imports.StatusReconciled || rec.Reconciliation == nil || !rec.Reconciliation.Matched || rec.Reconciliation.ImportedControl != "250" {
		t.Fatalf("%+v", rec.Reconciliation)
	}
	types := e.outboxTypes()
	for _, want := range []string{imports.EventValidated, imports.EventPreviewApproved, imports.EventStarted, imports.EventCompleted, imports.EventReconciled} {
		if types[want] != 1 {
			t.Errorf("event %s emitted %d times: %v", want, types[want], types)
		}
	}
	log, err := e.svc.Log(ictx, e.tenant, b.BatchID, 100)
	iMust(t, err)
	var kinds []string
	for _, l := range log {
		kinds = append(kinds, l.Kind)
	}
	want := "CREATED,UPLOAD_STARTED,UPLOAD_SEALED,VALIDATION_REQUESTED,SCANNED,PARSED,VALIDATED,PREVIEWED,APPROVED,EXECUTION_STARTED,EXECUTION_FINISHED,RECONCILED"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("log: %s\nwant %s", strings.Join(kinds, ","), want)
	}
	// reconciled is terminal
	if _, err := e.svc.Cancel(ictx, e.actor(uploader), b.BatchID, "too late"); !errors.Is(err, imports.ErrInvalidState) {
		t.Errorf("a reconciled batch was cancelled: %v", err)
	}
}

// ── INT-NP: malware ─────────────────────────────────────────────────────────

func TestImport_MalwareIsQuarantinedBeforeAnythingIsParsed(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(plainSpec())
	eicar := `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`
	b := e.validated(p, csvOf("INV-1,le-1,10", "INV-2,le-1,"+eicar), declared{})
	if b.Status != imports.StatusQuarantined || b.StatusReason != imports.ReasonMalware || b.ScanVerdict != "INFECTED" || !strings.HasPrefix(b.ScanSignature, "Eicar") {
		t.Fatalf("%+v", b)
	}
	if c := e.counts(b.BatchID); len(c) != 0 {
		t.Fatalf("an infected file was parsed: %v", c)
	}
	if _, err := e.svc.Preview(ictx, e.actor(uploader), b.BatchID); !errors.Is(err, imports.ErrInvalidState) {
		t.Errorf("an infected batch was previewed: %v", err)
	}
	if _, err := e.svc.Execute(ictx, e.actor(executor), b.BatchID); !errors.Is(err, imports.ErrInvalidState) {
		t.Errorf("an infected batch was executed: %v", err)
	}
	if e.dom.callCount() != 0 {
		t.Fatal("the owning domain was contacted")
	}
	if e.outboxTypes()[imports.EventBatchQuarantined] != 1 {
		t.Fatalf("%v", e.outboxTypes())
	}
	// the content is kept as evidence, and a quarantined batch is frozen
	var n int
	iMust(t, e.admin.QueryRow(ictx, `SELECT count(*) FROM import_chunks WHERE batch_id = $1`, b.BatchID).Scan(&n))
	if n == 0 {
		t.Error("quarantined content must be retained")
	}
	if _, err := e.admin.Exec(ictx, `UPDATE import_batches SET status = 'CREATED' WHERE batch_id = $1`, b.BatchID); err == nil {
		t.Error("a quarantined batch was revived")
	}
	// a file that is not what it claims is stopped the same way
	exe := "MZ\x90\x00\x03\x00\x00\x00 not a csv at all"
	x := e.validated(p, exe, declared{})
	if x.Status != imports.StatusQuarantined {
		t.Fatalf("a renamed binary was accepted: %+v", x)
	}
}

// ── INT-NP: hash mismatch ───────────────────────────────────────────────────

func TestImport_HashMismatchQuarantinesAtSeal(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(plainSpec())
	content := csvOf(goodRows(5, entityOne)...)
	b, err := e.create(p, content, declared{sha: sha([]byte("what the sender thought they sent"))})
	iMust(t, err)
	n := e.upload(b, content, 50, false)
	got, err := e.svc.Seal(ictx, e.actor(uploader), b.BatchID, n)
	iIs(t, "content that does not match its declared hash", err, imports.ErrHashMismatch)
	if got == nil || got.Status != imports.StatusQuarantined || got.StatusReason != imports.ReasonHashMismatch || got.ContentSHA256 != sha([]byte(content)) {
		t.Fatalf("the computed hash must be kept as evidence: %+v", got)
	}
	if _, err := e.svc.RequestValidation(ictx, e.actor(uploader), b.BatchID); !errors.Is(err, imports.ErrInvalidState) {
		t.Errorf("a hash-mismatched batch went on to validation: %v", err)
	}
	if e.counts(b.BatchID)[imports.ItemValid] != 0 {
		t.Error("nothing may be parsed")
	}
}

func TestImport_ChunkLevelIntegrity(t *testing.T) {
	e := newImpEnv(t, impOpts{cfg: func(c *imports.Config) { c.MaxChunkBytes, c.MaxBatchBytes = 100, 260 }})
	p := e.profile(plainSpec())
	b, err := e.create(p, "x", declared{sha: strings.Repeat("a", 64)})
	iMust(t, err)
	chunk := func(i int, data string, declaredSHA string) error {
		_, err := e.svc.UploadChunk(ictx, e.actor(uploader), b.BatchID, i, []byte(data), declaredSHA)
		return err
	}
	iIs(t, "a chunk that does not match its hash", chunk(0, "abc", sha([]byte("different"))), imports.ErrHashMismatch)
	iIs(t, "an empty chunk", chunk(0, "", sha(nil)), imports.ErrValidation)
	iIs(t, "an over-long chunk", chunk(0, strings.Repeat("a", 101), sha([]byte(strings.Repeat("a", 101)))), imports.ErrValidation)
	iIs(t, "an index out of range", chunk(99999, "abc", sha([]byte("abc"))), imports.ErrValidation)
	iMust(t, chunk(0, strings.Repeat("a", 100), sha([]byte(strings.Repeat("a", 100)))))
	iMust(t, chunk(1, strings.Repeat("b", 100), sha([]byte(strings.Repeat("b", 100)))))
	iIs(t, "a different chunk at a used index", chunk(1, strings.Repeat("c", 100), sha([]byte(strings.Repeat("c", 100)))), imports.ErrChunkConflict)
	iIs(t, "content beyond the batch cap", chunk(2, strings.Repeat("d", 100), sha([]byte(strings.Repeat("d", 100)))), imports.ErrTooLarge)
	iMust(t, chunk(2, strings.Repeat("d", 50), sha([]byte(strings.Repeat("d", 50)))))

	_, err = e.svc.Seal(ictx, e.actor(uploader), b.BatchID, 5)
	iIs(t, "sealing with chunks missing", err, imports.ErrIncompleteUpload)
	iMust(t, chunk(4, "tail", sha([]byte("tail"))))
	_, err = e.svc.Seal(ictx, e.actor(uploader), b.BatchID, 4)
	iIs(t, "a gap in the chunk indexes", err, imports.ErrIncompleteUpload)
	if e.batch(b.BatchID).Status != imports.StatusUploading {
		t.Fatal("a refused seal must leave the upload open")
	}
	// content cannot be added behind the service's back once sealed (see also the database test)
	q := e.profile(plainSpec())
	empty, err := e.create(q, "y", declared{sha: strings.Repeat("b", 64)})
	iMust(t, err)
	_, err = e.svc.Seal(ictx, e.actor(uploader), empty.BatchID, 1)
	iIs(t, "sealing before any upload", err, imports.ErrIncompleteUpload)
}

// ── INT-NP: schema mismatch ─────────────────────────────────────────────────

func TestImport_SchemaMismatchStopsBeforePreview(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(plainSpec())
	for name, content := range map[string]string{
		"missing required column": "invoice_no,entity\nINV-1,le-1\n",
		"unknown column":          "invoice_no,entity,amount,surprise\nINV-1,le-1,1,x\n",
		"no header":               "",
	} {
		if content == "" {
			continue // an empty file is refused earlier, by the content check
		}
		b := e.validated(p, content, declared{})
		if b.Status != imports.StatusFailed || b.StatusReason != imports.ReasonSchemaMismatch {
			t.Errorf("%s: %s (%s)", name, b.Status, b.StatusReason)
		}
		if _, err := e.svc.Preview(ictx, e.actor(uploader), b.BatchID); !errors.Is(err, imports.ErrInvalidState) {
			t.Errorf("%s: a schema-mismatched batch was previewed: %v", name, err)
		}
	}
}

// ── INT-NP: duplicate file ──────────────────────────────────────────────────

func TestImport_DuplicateFileIsRefused(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(plainSpec())
	content := csvOf(goodRows(3, entityOne)...)
	first, err := e.create(p, content, declared{})
	iMust(t, err)
	_, err = e.create(p, content, declared{})
	iIs(t, "the same file again", err, imports.ErrDuplicateBatch)
	// another entity or another profile is a different import of the same bytes
	if _, err := e.create(p, content, declared{entity: "le-2"}); err != nil {
		t.Errorf("another entity: %v", err)
	}
	// once the first attempt is dead the file may be submitted again
	_, err = e.svc.Cancel(ictx, e.actor(uploader), first.BatchID, "wrong entity")
	iMust(t, err)
	if _, err := e.create(p, content, declared{}); err != nil {
		t.Errorf("after cancellation: %v", err)
	}
	// and a live duplicate is refused by the database as well, not only by the service
	_, err = e.admin.Exec(ictx, `INSERT INTO import_batches (batch_id, tenant_id, legal_entity_id, profile_key, profile_version, material, source_system,
		declared_content_type, declared_sha256, created_by) SELECT 'imb_' || gen_random_uuid(), tenant_id, legal_entity_id, profile_key, profile_version,
		material, source_system, declared_content_type, declared_sha256, created_by FROM import_batches
		WHERE tenant_id = $1 AND declared_sha256 = $2 AND status = 'CREATED' LIMIT 1`, e.tenant, sha([]byte(content)))
	if err == nil {
		t.Error("the database allowed a duplicate live batch")
	}
}

// ── INT-NP: preview differs from execution ──────────────────────────────────

func TestImport_ExecutionRefusesAnythingThatDiffersFromTheApprovedPreview(t *testing.T) {
	cases := map[string]func(e *impEnv, b *imports.Batch){
		"the stored content was altered": func(e *impEnv, b *imports.Batch) {
			// storage corruption, simulated by switching the guard off for one statement
			iMust(t, execAll(e, `ALTER TABLE import_chunks DISABLE TRIGGER trg_import_chunk_guard`,
				fmt.Sprintf(`UPDATE import_chunks SET content = overlay(content placing 'X' from 1 for 1) WHERE batch_id = '%s' AND chunk_index = 0`, b.BatchID),
				`ALTER TABLE import_chunks ENABLE TRIGGER trg_import_chunk_guard`))
		},
		"the counts moved after approval": func(e *impEnv, b *imports.Batch) {
			iMust(t, execAll(e, fmt.Sprintf(`UPDATE import_batches SET row_count = row_count + 1 WHERE batch_id = '%s'`, b.BatchID)))
		},
		"the control total moved after approval": func(e *impEnv, b *imports.Batch) {
			iMust(t, execAll(e, fmt.Sprintf(`UPDATE import_batches SET computed_control_total = '999' WHERE batch_id = '%s'`, b.BatchID)))
		},
		"an item went missing": func(e *impEnv, b *imports.Batch) {
			iMust(t, execAll(e, `ALTER TABLE import_items DISABLE TRIGGER trg_import_item_guard`,
				fmt.Sprintf(`DELETE FROM import_items WHERE batch_id = '%s' AND row_no = 1`, b.BatchID),
				`ALTER TABLE import_items ENABLE TRIGGER trg_import_item_guard`))
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			e := newImpEnv(t)
			p := e.profile(materialSpec())
			content := csvOf(goodRows(6, entityOne)...)
			b := e.approved(p, content, declared{rows: ip(6), control: sumControl(6)})
			tamper(e, b)
			out, err := e.svc.Execute(ictx, e.actor(executor), b.BatchID)
			if !errors.Is(err, imports.ErrPreviewMismatch) {
				t.Fatalf("execution went ahead of a changed preview: %v %+v", err, out)
			}
			got := e.batch(b.BatchID)
			if got.Status != imports.StatusFailed || got.StatusReason != imports.ReasonPreviewDiffers {
				t.Fatalf("%s (%s)", got.Status, got.StatusReason)
			}
			if e.dom.callCount() != 0 {
				t.Fatal("nothing may be sent when the preview no longer matches")
			}
		})
	}
}

func execAll(e *impEnv, stmts ...string) error {
	for _, s := range stmts {
		if _, err := e.admin.Exec(ictx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

// ── INT-NP: direct database write attempt ───────────────────────────────────

func TestImport_TheDatabaseRefusesDirectWrites(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(materialSpec())
	content := csvOf(goodRows(4, entityOne)...)
	b := e.approved(p, content, declared{rows: ip(4), control: sumControl(4)})
	asTenant := func(sql string, args ...any) error {
		tx, err := e.app.Begin(ictx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ictx) //nolint:errcheck
		if _, err := tx.Exec(ictx, "SELECT set_config('app.tenant_id', $1, true)", e.tenant); err != nil {
			return err
		}
		_, err = tx.Exec(ictx, sql, args...)
		return err
	}
	for name, sql := range map[string]string{
		// no code path may claim an import the owning domain did not answer for
		"mark an item imported with no domain answer": `UPDATE import_items SET status = 'IMPORTED' WHERE batch_id = $1 AND row_no = 1`,
		"mark an item imported without ever sending":  `UPDATE import_items SET status = 'IMPORTED', domain_status = 201 WHERE batch_id = $1 AND row_no = 1`,
		"rewrite an item's values":                    `UPDATE import_items SET item_values = '{"amount":"1"}'::jsonb WHERE batch_id = $1 AND row_no = 1`,
		"rewrite an item's idempotency key":           `UPDATE import_items SET idempotency_key = 'x' WHERE batch_id = $1 AND row_no = 1`,
		"add content after the upload was sealed":     `INSERT INTO import_chunks (batch_id, chunk_index, tenant_id, content, size_bytes, chunk_sha256) SELECT batch_id, 99, tenant_id, 'x'::bytea, 1, repeat('a',64) FROM import_batches WHERE batch_id = $1`,
		"change stored content":                       `UPDATE import_chunks SET content = 'x'::bytea, size_bytes = 1 WHERE batch_id = $1`,
		"delete content":                              `DELETE FROM import_chunks WHERE batch_id = $1`,
		"rewrite the sealed hash":                     `UPDATE import_batches SET content_sha256 = repeat('b', 64) WHERE batch_id = $1`,
		"rewrite the declared hash":                   `UPDATE import_batches SET declared_sha256 = repeat('b', 64) WHERE batch_id = $1`,
		"change the approval":                         `UPDATE import_batches SET approved_fingerprint = repeat('c', 64) WHERE batch_id = $1`,
		"approve as the creator":                      `UPDATE import_batches SET approved_by = created_by WHERE batch_id = $1`,
		"skip from approved to reconciled":            `UPDATE import_batches SET status = 'RECONCILED' WHERE batch_id = $1`,
		"go back to previewed":                        `UPDATE import_batches SET status = 'PREVIEWED' WHERE batch_id = $1`,
		"change the profile it pins":                  `UPDATE import_batches SET profile_version = 2 WHERE batch_id = $1`,
		"change the entity":                           `UPDATE import_batches SET legal_entity_id = 'le-2' WHERE batch_id = $1`,
		"turn off the material flag":                  `UPDATE import_batches SET material = false WHERE batch_id = $1`,
		"delete the batch":                            `DELETE FROM import_batches WHERE batch_id = $1`,
		"delete the history":                          `DELETE FROM import_batch_log WHERE batch_id = $1`,
		"rewrite the history":                         `UPDATE import_batch_log SET actor = 'someone-else' WHERE batch_id = $1`,
		"delete an item":                              `DELETE FROM import_items WHERE batch_id = $1`,
	} {
		if err := asTenant(sql, b.BatchID); err == nil {
			t.Errorf("%s: the database allowed it", name)
		}
	}
	// nothing above changed anything
	if got := e.batch(b.BatchID); got.Status != imports.StatusApproved || got.ApprovedFingerprint != b.ApprovedFingerprint {
		t.Fatalf("%+v", got)
	}
	// and an unapproved material batch cannot be approved by its creator even by SQL
	m, err := e.create(p, csvOf(goodRows(2, entityOne)...), declared{rows: ip(2), control: sumControl(2)})
	iMust(t, err)
	if _, err := e.admin.Exec(ictx, `UPDATE import_batches SET approved_by = created_by WHERE batch_id = $1`, m.BatchID); err == nil {
		t.Error("a material batch was approved by its creator")
	}
}

// ── INT-NP: partial failure ─────────────────────────────────────────────────

func TestImport_PartialFailureIsExplicitAndNeverReconciled(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(plainSpec()) // SKIP_INVALID
	rows := goodRows(6, entityOne)
	rows[1] = "INV-2,le-OTHER,10.00" // cross-entity: skipped
	rows[2] = "BAD-3,le-1,10.00"     // pattern: skipped
	content := csvOf(rows...)
	e.dom.records["INV-4"] = "already-there" // the domain already has INV-4: a duplicate it refuses
	e.dom.failFirst["INV-5"] = struct {
		n    int
		code int
	}{99, 503} // the domain keeps failing for INV-5

	b := e.executing(p, content, declared{})
	if b.ValidRows != 4 || b.InvalidRows != 2 {
		t.Fatalf("%+v", b)
	}
	done := e.drain(b)
	if done.Status != imports.StatusCompletedWithExceptions {
		t.Fatalf("a batch with items that are not imported reported %s", done.Status)
	}
	c := e.counts(b.BatchID)
	if c[imports.ItemImported] != 2 || c[imports.ItemQuarantined] != 1 || c[imports.ItemFailed] != 1 || c[imports.ItemSkipped] != 2 {
		t.Fatalf("%v", c)
	}
	if done.ImportedItems != 2 || done.QuarantinedItems != 1 || done.FailedItems != 1 || done.SkippedItems != 2 {
		t.Fatalf("%+v", done)
	}
	quar, err := e.svc.Items(ictx, e.tenant, b.BatchID, imports.ItemQuarantined, 0, 10)
	iMust(t, err)
	if len(quar) != 1 || quar[0].QuarantineReason != "DOMAIN_REJECTED_409" || quar[0].QuarantineOwner != uploader || quar[0].DomainStatus == nil || *quar[0].DomainStatus != 409 {
		t.Fatalf("a refused item is kept with its reason and an owner: %+v", quar)
	}
	failed, err := e.svc.Items(ictx, e.tenant, b.BatchID, imports.ItemFailed, 0, 10)
	iMust(t, err)
	if len(failed) != 1 || failed[0].Attempts != e.cfg.ItemMaxAttempts || !strings.HasSuffix(failed[0].LastErrorClass, "_exhausted") {
		t.Fatalf("a transient failure is retried to its budget, then FAILED: %+v", failed)
	}
	if e.outboxTypes()[imports.EventItemQuarantined] != 1 || e.outboxTypes()[imports.EventCompleted] != 1 {
		t.Fatalf("%v", e.outboxTypes())
	}

	// the batch cannot be reconciled while items are missing; the findings are recorded
	rec, err := e.svc.Reconcile(ictx, e.actor(approver), b.BatchID)
	iIs(t, "reconciling a batch with exceptions", err, imports.ErrReconciliationFailed)
	if rec == nil || rec.Status != imports.StatusCompletedWithExceptions || rec.Reconciliation == nil || rec.Reconciliation.Matched || len(rec.Reconciliation.Problems) == 0 {
		t.Fatalf("%+v", rec)
	}
	if e.outboxTypes()[imports.EventReconciliationFail] != 1 {
		t.Fatalf("%v", e.outboxTypes())
	}

	// a person decides the failed item should be tried again; the domain has recovered
	e.dom.mu.Lock()
	e.dom.failFirst["INV-5"] = struct {
		n    int
		code int
	}{0, 0}
	e.dom.mu.Unlock()
	iIs(t, "retrying an imported item", e.svc.RetryItem(ictx, e.actor(approver), b.BatchID, 1, "no"), imports.ErrRetryNotAllowed)
	iIs(t, "retrying without a reason", e.svc.RetryItem(ictx, e.actor(approver), b.BatchID, failed[0].RowNo, ""), imports.ErrValidation)
	iMust(t, e.svc.RetryItem(ictx, e.actor(approver), b.BatchID, failed[0].RowNo, "the domain is back"))
	if e.batch(b.BatchID).Status != imports.StatusImporting {
		t.Fatal("a retry must resume the batch")
	}
	again := e.drain(e.batch(b.BatchID))
	if again.Status != imports.StatusCompletedWithExceptions || e.counts(b.BatchID)[imports.ItemImported] != 3 {
		t.Fatalf("%s %v", again.Status, e.counts(b.BatchID))
	}
	// the quarantined and the skipped rows are still exceptions: still not reconciled
	if _, err := e.svc.Reconcile(ictx, e.actor(approver), b.BatchID); !errors.Is(err, imports.ErrReconciliationFailed) {
		t.Fatalf("%v", err)
	}
}

// ── INT-NP: missing control totals, validation failures ─────────────────────

func TestImport_ControlTotals(t *testing.T) {
	e := newImpEnv(t)
	mat := e.profile(materialSpec())
	content := csvOf(goodRows(4, entityOne)...)

	_, err := e.create(mat, content, declared{rows: ip(4)})
	iIs(t, "a material import with no control total", err, imports.ErrValidation)
	_, err = e.create(mat, content, declared{control: "40"})
	iIs(t, "a material import with no row count", err, imports.ErrValidation)
	_, err = e.create(mat, content, declared{rows: ip(4), control: "forty"})
	iIs(t, "a control total that is not a number", err, imports.ErrValidation)

	wrongTotal := e.validated(mat, content, declared{rows: ip(4), control: "41"})
	if wrongTotal.Status != imports.StatusFailed || wrongTotal.StatusReason != imports.ReasonControlMismatch {
		t.Errorf("%s (%s)", wrongTotal.Status, wrongTotal.StatusReason)
	}
	c2 := csvOf(goodRows(4, entityOne)...) + "\n"
	wrongRows := e.validated(mat, c2+" ", declared{rows: ip(9), control: "40"})
	if wrongRows.Status != imports.StatusFailed {
		t.Errorf("%s (%s)", wrongRows.Status, wrongRows.StatusReason)
	}
	// a non-material profile may omit them, but a declared one is still checked
	plain := e.profile(plainSpec())
	if b := e.validated(plain, csvOf(goodRows(3, entityOne)...), declared{}); b.Status != imports.StatusValidated {
		t.Errorf("an optional control total: %s (%s)", b.Status, b.StatusReason)
	}
	if b := e.validated(plain, csvOf(goodRows(2, entityOne)...), declared{control: "999"}); b.Status != imports.StatusFailed || b.StatusReason != imports.ReasonControlMismatch {
		t.Errorf("a declared control total is always checked: %s (%s)", b.Status, b.StatusReason)
	}
	// decimals are exact: a million cents add up to the cent
	many := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		many = append(many, fmt.Sprintf("INV-%d,le-1,0.10", i+1))
	}
	exact := e.validated(plain, csvOf(many...), declared{control: "100"})
	if exact.Status != imports.StatusValidated || exact.ComputedControl != "100" {
		t.Errorf("1000 x 0.10 must be exactly 100: %s %q", exact.Status, exact.ComputedControl)
	}
}

// ── INT-NP: cross-entity ────────────────────────────────────────────────────

func TestImport_CrossEntityRowsAreRefused(t *testing.T) {
	e := newImpEnv(t)
	mat := e.profile(materialSpec())
	rows := goodRows(3, entityOne)
	rows[1] = "INV-2,le-2,10.00" // another entity's row in this entity's file
	b := e.validated(mat, csvOf(rows...), declared{rows: ip(3), control: sumControl(3)})
	if b.Status != imports.StatusFailed || b.StatusReason != imports.ReasonValidationErrors || b.InvalidRows != 1 {
		t.Fatalf("a material import with a cross-entity row: %s (%s) %+v", b.Status, b.StatusReason, b)
	}
	bad, err := e.svc.Items(ictx, e.tenant, b.BatchID, imports.ItemInvalid, 0, 10)
	iMust(t, err)
	if len(bad) != 1 || len(bad[0].ErrorCodes) != 1 || bad[0].ErrorCodes[0].Code != imports.CodeCrossEntity || bad[0].ErrorCodes[0].Column != "entity" {
		t.Fatalf("%+v", bad)
	}
	// on a skip-invalid profile the row is listed and skipped, never sent
	plain := e.profile(plainSpec())
	ok := e.executing(plain, csvOf(rows...), declared{entity: entityOne})
	done := e.drain(ok)
	if done.SkippedItems != 1 || e.dom.recordCount() != 2 {
		t.Fatalf("skipped=%d records=%d", done.SkippedItems, e.dom.recordCount())
	}
	for _, c := range e.dom.allCalls() {
		if c.Body["number"] == "INV-2" {
			t.Fatal("a cross-entity row reached the owning domain")
		}
	}
}

// ── INT-NP: duplicate domain item / retried delivery ────────────────────────

func TestImport_ARetriedDeliveryNeverCreatesASecondDomainRecord(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(plainSpec())
	// the domain commits INV-2, then the connection drops before the answer arrives
	e.dom.dropAfterCommit["INV-2"] = 1
	b := e.executing(p, csvOf(goodRows(3, entityOne)...), declared{})
	done := e.drain(b)
	if done.Status != imports.StatusCompleted || done.ImportedItems != 3 {
		t.Fatalf("%+v", done)
	}
	if e.dom.recordCount() != 3 {
		t.Fatalf("%d domain records for 3 items", e.dom.recordCount())
	}
	items, err := e.svc.Items(ictx, e.tenant, b.BatchID, imports.ItemImported, 0, 10)
	iMust(t, err)
	var retried int
	for _, it := range items {
		if e.dom.hitsFor(it.IdempotencyKey) > 1 {
			retried++
			if it.Attempts < 2 {
				t.Errorf("the retried item records %d attempts", it.Attempts)
			}
		}
	}
	if retried != 1 {
		t.Fatalf("expected exactly one item to be redelivered under its original key, got %d", retried)
	}
	// every item was delivered under its own distinct key
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it.IdempotencyKey] {
			t.Fatal("two items share an idempotency key")
		}
		seen[it.IdempotencyKey] = true
	}
}

func TestImport_ExecutionIsBoundedAndDeliversEachItemOnce(t *testing.T) {
	e := newImpEnv(t, impOpts{cfg: func(c *imports.Config) { c.ExecWorkers, c.ClaimBatch = 4, 20 }})
	e.dom.delay = 15 * time.Millisecond
	p := e.profile(plainSpec())
	b := e.executing(p, csvOf(goodRows(120, entityOne)...), declared{})
	// two executors working the same batch at once: claims must not overlap
	var wg sync.WaitGroup
	var rounds int32
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				cur, err := e.svc.Get(ictx, e.tenant, b.BatchID)
				if err != nil || cur.Status != imports.StatusImporting {
					return
				}
				if _, err := e.exec.ProcessBatch(ictx, imports.Ref{TenantID: e.tenant, BatchID: b.BatchID}); err != nil {
					t.Error(err)
					return
				}
				atomic.AddInt32(&rounds, 1)
			}
		}()
	}
	wg.Wait()
	done := e.batch(b.BatchID)
	if done.Status != imports.StatusCompleted || done.ImportedItems != 120 {
		t.Fatalf("%s %+v", done.Status, e.counts(b.BatchID))
	}
	if e.dom.recordCount() != 120 || e.dom.callCount() != 120 {
		t.Fatalf("%d records from %d calls: every item exactly once", e.dom.recordCount(), e.dom.callCount())
	}
	if e.dom.maxConc > int32(2*4) { // two executors, four workers each
		t.Fatalf("the worker pools are not bounded: %d concurrent requests", e.dom.maxConc)
	}
}

// ── separation of duties on a non-material import ───────────────────────────

func TestImport_NonMaterialMayBeApprovedBySomeoneElseOrNot(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(plainSpec())
	b := e.previewed(p, csvOf(goodRows(2, entityOne)...), declared{})
	ap, err := e.svc.Approve(ictx, e.actor(uploader), b.BatchID, b.PreviewFingerprint, "own non-material import")
	iMust(t, err)
	if ap.Status != imports.StatusApproved {
		t.Fatal(ap.Status)
	}
}

// ── the scanner is never assumed clean ──────────────────────────────────────

func TestImport_NoScannerMeansNoImport(t *testing.T) {
	e := newImpEnv(t, impOpts{noScan: true})
	p := e.profile(plainSpec())
	_, err := e.create(p, csvOf(goodRows(1, entityOne)...), declared{})
	iIs(t, "creating a batch with no scanner configured", err, imports.ErrNoScanner)
	_, err = e.svc.RequestValidation(ictx, e.actor(uploader), "imb_x")
	iIs(t, "validating with no scanner configured", err, imports.ErrNoScanner)
	_, err = e.svc.RunPipeline(ictx, e.tenant, "imb_x")
	iIs(t, "running the pipeline with no scanner configured", err, imports.ErrNoScanner)
}

func TestImport_AnUnavailableScannerLeavesTheBatchUnscannedNotClean(t *testing.T) {
	fs := &flakyScanner{}
	fs.inner = scanChain()
	fs.down.Store(true)
	e := newImpEnv(t, impOpts{scanner: fs})
	p := e.profile(plainSpec())
	content := csvOf(goodRows(2, entityOne)...)
	b, err := e.create(p, content, declared{})
	iMust(t, err)
	n := e.upload(b, content, 100, false)
	_, err = e.svc.Seal(ictx, e.actor(uploader), b.BatchID, n)
	iMust(t, err)
	_, err = e.svc.RequestValidation(ictx, e.actor(uploader), b.BatchID)
	iMust(t, err)
	got, err := e.svc.RunPipeline(ictx, e.tenant, b.BatchID)
	iIs(t, "scanning with the scanner down", err, imports.ErrScannerUnavailable)
	if got.Status != imports.StatusUploaded || got.ScanVerdict != "" {
		t.Fatalf("no verdict must never become a clean one: %+v", got)
	}
	// the worker backs off rather than spinning
	if _, failed := e.val.RunOnce(ictx); !failed {
		t.Fatal("the validation worker must report the failed stage")
	}
	fs.down.Store(false)
	got, err = e.svc.RunPipeline(ictx, e.tenant, b.BatchID)
	iMust(t, err)
	if got.Status != imports.StatusValidated {
		t.Fatalf("once the scanner is back the same batch goes on: %s", got.Status)
	}
}

// ── resumable, deterministic pipeline ───────────────────────────────────────

func TestImport_ACrashedParseIsRerunFromTheImmutableSource(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(plainSpec())
	content := csvOf(goodRows(5, entityOne)...)
	b, err := e.create(p, content, declared{})
	iMust(t, err)
	n := e.upload(b, content, 100, false)
	_, err = e.svc.Seal(ictx, e.actor(uploader), b.BatchID, n)
	iMust(t, err)
	// the scan stage ran; a parse started, wrote a stray item, and died
	if _, err := e.admin.Exec(ictx, `UPDATE import_batches SET validate_requested_at = now() WHERE batch_id = $1`, b.BatchID); err != nil {
		t.Fatal(err)
	}
	e.cfg = imports.DefaultConfig()
	if _, err := e.svc.RunPipeline(ictx, e.tenant, b.BatchID); err != nil {
		t.Fatal(err)
	}
	// rewind to SCANNED with debris in the items table, as a crash would leave it
	if _, err := e.admin.Exec(ictx, `ALTER TABLE import_batches DISABLE TRIGGER trg_import_batch_guard`); err != nil {
		t.Fatal(err)
	}
	if err := execAll(e, fmt.Sprintf(`UPDATE import_batches SET status = 'SCANNED', row_count = 0, valid_rows = 0, invalid_rows = 0 WHERE batch_id = '%s'`, b.BatchID),
		`ALTER TABLE import_batches ENABLE TRIGGER trg_import_batch_guard`); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.RunPipeline(ictx, e.tenant, b.BatchID)
	iMust(t, err)
	if got.Status != imports.StatusValidated || got.RowCount != 5 || got.ValidRows != 5 {
		t.Fatalf("%+v", got)
	}
	if c := e.counts(b.BatchID); c[imports.ItemValid] != 5 {
		t.Fatalf("the rerun must rebuild the items, not add to them: %v", c)
	}
}

// ── cancellation ────────────────────────────────────────────────────────────

func TestImport_CancelOnlyBeforeExecution(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(plainSpec())
	b := e.approved(p, csvOf(goodRows(2, entityOne)...), declared{})
	_, err := e.svc.Cancel(ictx, e.actor(uploader), b.BatchID, "")
	iIs(t, "cancelling without a reason", err, imports.ErrValidation)
	c, err := e.svc.Cancel(ictx, e.actor(uploader), b.BatchID, "wrong period")
	iMust(t, err)
	if c.Status != imports.StatusCancelled {
		t.Fatal(c.Status)
	}
	if _, err := e.svc.Execute(ictx, e.actor(executor), b.BatchID); !errors.Is(err, imports.ErrInvalidState) {
		t.Errorf("a cancelled batch was executed: %v", err)
	}
	x := e.executing(p, csvOf(goodRows(3, entityOne)...), declared{})
	if _, err := e.svc.Cancel(ictx, e.actor(uploader), x.BatchID, "too late"); !errors.Is(err, imports.ErrInvalidState) {
		t.Errorf("a batch that is importing was cancelled: %v", err)
	}
	// quarantining a pending item before it is sent holds it back
	iMust(t, e.svc.QuarantineItem(ictx, e.actor(approver), x.BatchID, 2, "customer disputes this invoice", "ap-team"))
	iIs(t, "quarantining without an owner", e.svc.QuarantineItem(ictx, e.actor(approver), x.BatchID, 3, "x", ""), imports.ErrValidation)
	done := e.drain(x)
	if done.Status != imports.StatusCompletedWithExceptions || e.dom.recordCount() != 2 {
		t.Fatalf("%s, %d records", done.Status, e.dom.recordCount())
	}
}

// ── isolation ───────────────────────────────────────────────────────────────

func TestImport_TenantIsolation(t *testing.T) {
	e := newImpEnv(t)
	p := e.profile(plainSpec())
	b := e.approved(p, csvOf(goodRows(2, entityOne)...), declared{})
	other := imports.Actor{TenantID: "99999999-9999-9999-9999-999999999999", PrincipalID: approver}
	if _, err := e.svc.Get(ictx, other.TenantID, b.BatchID); !errors.Is(err, imports.ErrNotFound) {
		t.Errorf("get: %v", err)
	}
	if list, _ := e.svc.List(ictx, other.TenantID, "", 100); len(list) != 0 {
		t.Errorf("list leaked %d", len(list))
	}
	if _, err := e.svc.Execute(ictx, other, b.BatchID); !errors.Is(err, imports.ErrNotFound) {
		t.Errorf("execute: %v", err)
	}
	if _, err := e.svc.Cancel(ictx, other, b.BatchID, "x"); !errors.Is(err, imports.ErrNotFound) {
		t.Errorf("cancel: %v", err)
	}
	if _, err := e.svc.Items(ictx, other.TenantID, b.BatchID, "", 0, 10); !errors.Is(err, imports.ErrNotFound) {
		t.Errorf("items: %v", err)
	}
	if _, err := e.svc.UploadState(ictx, other.TenantID, b.BatchID); !errors.Is(err, imports.ErrNotFound) {
		t.Errorf("upload state: %v", err)
	}
	// row-level security below the service, as the application role
	count := func(setting, value, table string) int {
		tx, err := e.app.Begin(ictx)
		iMust(t, err)
		defer tx.Rollback(ictx) //nolint:errcheck
		_, err = tx.Exec(ictx, "SELECT set_config($1, $2, true)", setting, value)
		iMust(t, err)
		var n int
		iMust(t, tx.QueryRow(ictx, "SELECT count(*) FROM "+table).Scan(&n))
		return n
	}
	for _, table := range []string{"import_batches", "import_chunks", "import_items", "import_batch_log"} {
		if n := count("app.tenant_id", other.TenantID, table); n != 0 {
			t.Errorf("%s: another tenant sees %d rows", table, n)
		}
		if n := count("app.tenant_id", "", table); n != 0 {
			t.Errorf("%s: no tenant scope sees %d rows", table, n)
		}
		if n := count("app.tenant_id", e.tenant, table); n == 0 {
			t.Errorf("%s: the owner sees nothing", table)
		}
	}
	// the worker flag shows batches and items to claim, and never content or history
	for _, table := range []string{"import_batches", "import_items"} {
		if count("app.import_worker", "true", table) == 0 {
			t.Errorf("%s: the worker flag must expose rows to claim", table)
		}
	}
	for _, table := range []string{"import_chunks", "import_batch_log"} {
		if n := count("app.import_worker", "true", table); n != 0 {
			t.Errorf("%s: the worker flag exposed %d rows", table, n)
		}
	}
	// the executor's discovery sees this tenant's work only as references
	tx, err := e.app.Begin(ictx)
	iMust(t, err)
	defer tx.Rollback(ictx) //nolint:errcheck
	_, _ = tx.Exec(ictx, "SELECT set_config('app.import_worker', 'true', true)")
	if tag, _ := tx.Exec(ictx, `UPDATE import_batches SET status_reason = 'x' WHERE batch_id = $1`, b.BatchID); tag.RowsAffected() != 0 {
		t.Error("the worker flag allowed an UPDATE")
	}
}
