package batch_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/batch"
	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/eventing/integration"
)

func assertOut(t *testing.T, o *batch.Outcome, status int, code integration.XICCode) {
	t.Helper()
	if o.Status != status {
		t.Fatalf("status %d (want %d): reason=%s xic=%v", o.Status, status, o.Reason, o.XIC)
	}
	if code != "" && (o.XIC == nil || o.XIC.Code != code) {
		t.Fatalf("code %v (want %s)", o.XIC, code)
	}
}

// ---- the whole path: received, verified, admitted row by row, delivered, reconciled -----------------------

func TestBatch_ReceivedVerifiedAdmittedDeliveredAndReconciled(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	o := e.deliver(simple(c, "B-001", 3))
	assertOut(t, o, 202, "")
	if o.Batch.State != batch.StateReceived || o.Batch.ExternalBatchID != "B-001" || o.Batch.FileCount != 1 || o.Batch.TenantID != tenantA {
		t.Fatalf("%+v", o.Batch)
	}
	if e.domHits != 0 || len(e.rowsOf(tenantA, o.Batch.BatchID)) != 0 {
		t.Fatal("receiving a file admitted something")
	}
	b := e.process(o.Batch)
	if b.State != batch.StateProcessing || !b.ProcessingDone || b.ParsedRows != 3 || b.AdmittedRows != 3 || b.PendingRows != 3 || b.AcceptedRows != 0 {
		t.Fatalf("file accepted is not rows accepted: %+v", b)
	}
	// every row is its own exchange, with its own evidence
	rs := e.rowsOf(tenantA, b.BatchID)
	if len(rs) != 3 {
		t.Fatalf("%d rows", len(rs))
	}
	for i, r := range rs {
		if r.RowNo != i+1 || r.State != batch.RowPending || r.ExchangeID == "" || r.SourceRowHash == "" {
			t.Fatalf("%+v", r)
		}
		x, err := e.inRepo.Get(ctxFor(tenantA), r.ExchangeID)
		if err != nil || x.State != inbound.StateValidated || x.CanonicalContract != "bank.statement.v1" || !strings.Contains(string(x.Canonical), `"balance":"10.50"`) || x.AuthMechanism != "BATCH_FILE" {
			t.Fatalf("%v %+v", err, x)
		}
	}
	// the domain answers; the batch reports
	b = e.settle(b)
	if b.State != batch.StateReconciled || b.AcceptedRows != 3 || b.PendingRows != 0 || e.domHits != 3 {
		t.Fatalf("%+v hits=%d", b, e.domHits)
	}
	types := e.repo.Types()
	for _, want := range []string{"integration.batch.received", "integration.batch.accepted", "integration.batch.reconciled"} {
		if !hasType(types, want) {
			t.Fatalf("%s missing: %v", want, types)
		}
	}
	// no row value and no file content in any batch event
	for _, ev := range e.repo.Out {
		bs, _ := json.Marshal(ev.Data)
		if strings.Contains(string(bs), "10.5") || strings.Contains(string(bs), "A1") || strings.Contains(string(bs), "B-001-0001") {
			t.Fatalf("%s carries row content: %s", ev.Type, bs)
		}
	}
}

func TestBatch_FileAcceptedNeverMeansEveryRowAccepted_NP36(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	good := rows(98, "g")
	data := csvFile(append(append(good, "bad1,A1,not-a-number,GBP,2026-10-20T10:00:00Z"), "short,only,three")...)
	o := e.deliver(bspec{conn: c, id: "B-MIX", records: intp(100), files: []fileSpec{{name: "stmt-0001.csv", data: []byte(data)}}})
	assertOut(t, o, 202, "")
	b := e.process(o.Batch)
	b = e.settle(b)
	// 98 delivered, one held for its value, one rejected for its shape: a partial batch, never a successful one
	if b.State != batch.StatePartial || b.AcceptedRows != 98 || b.QuarantinedRows != 1 || b.RejectedRows != 1 || b.PendingRows != 0 || b.ParsedRows != 100 {
		t.Fatalf("%+v", b)
	}
	if rs := e.rowsOf(tenantA, b.BatchID, batch.RowRejected); len(rs) != 1 || rs[0].ReasonCode != batch.ReasonRowStructure || rs[0].ExchangeID != "" {
		t.Fatalf("%+v", rs)
	}
	q := e.rowsOf(tenantA, b.BatchID, batch.RowQuarantined)
	if len(q) != 1 || q[0].ExchangeID == "" {
		t.Fatalf("%+v", q)
	}
	if x, _ := e.inRepo.Get(ctxFor(tenantA), q[0].ExchangeID); x.QuarantineClass != inbound.ClassMapping {
		t.Fatalf("%+v", x)
	}
	if !hasType(e.repo.Types(), "integration.batch.partial") || hasType(e.repo.Types(), "integration.batch.reconciled") {
		t.Fatalf("%v", e.repo.Types())
	}
	for _, ev := range e.repo.Out {
		if ev.Type == "integration.batch.partial" && (ev.Data["accepted_count"] != 98 || ev.Data["rejected_count"] != 1 || ev.Data["unresolved_count"] != 1 || ev.Data["duplicate_count"] != 0) {
			t.Fatalf("%v", ev.Data)
		}
	}
}

func TestBatch_ManifestCountVsFileIsAVarianceNotASuccess_NP35(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	s := simple(c, "B-VAR", 8)
	s.records = intp(10) // the manifest says 10 rows; the file has 8
	o := e.deliver(s)
	assertOut(t, o, 202, "")
	b := e.settle(e.process(o.Batch))
	if b.State != batch.StatePartial || b.VarianceRows != 2 || b.AcceptedRows != 8 || b.ParsedRows != 8 || b.DeclaredRows == nil || *b.DeclaredRows != 10 {
		t.Fatalf("every row accepted but the count is short: still partial. %+v", b)
	}
	for _, ev := range e.repo.Out {
		if ev.Type == "integration.batch.partial" && ev.Data["variance_rows"] != 2 {
			t.Fatalf("%v", ev.Data)
		}
	}
}

// ---- authentication and the transport ------------------------------------------------------------------------

func TestReceive_AuthenticationFailureIsUniformAndNothingIsStored(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	var ref *batch.Outcome
	for name, s := range map[string]bspec{
		"bad secret":   func() bspec { s := simple(c, "B-1", 1); s.secret = "wrong"; return s }(),
		"no signature": func() bspec { s := simple(c, "B-1", 1); s.sigOff = true; return s }(),
		"tampered manifest": func() bspec {
			s := simple(c, "B-1", 1)
			return s
		}(),
		"wrong connection": func() bspec { s := simple(c, "B-1", 1); return s }(),
	} {
		d := e.delivery(s)
		if name == "tampered manifest" {
			d.Manifest = []byte(`{"batch_id":"B-EVIL"}`) // signed for the real manifest, then altered in transit
		}
		if name == "wrong connection" {
			d.ConnectionID = "00000000-0000-4000-8000-000000000000"
		}
		o := e.svc.Receive(context.Background(), d)
		assertOut(t, o, 401, integration.XICAuthenticationFailed)
		if o.Batch != nil {
			t.Errorf("%s: the answer carries a batch", name)
		}
		if ref == nil {
			ref = o
		} else if o.XIC.Detail != ref.XIC.Detail {
			t.Errorf("%s is distinguishable", name)
		}
	}
	if l, _ := e.repo.List(ctxFor(tenantA), batch.Filter{}); len(l) != 0 {
		t.Fatalf("%d batches from unauthenticated deliveries", len(l))
	}
	// an operation that is not a file operation, or a connector key that is not the connection's
	d := e.delivery(simple(c, "B-1", 1))
	d.OperationID = "statement-push"
	assertOut(t, e.svc.Receive(context.Background(), d), 401, "")
	d = e.delivery(simple(c, "B-1", 1))
	d.ConnectorKey = "bank-y"
	assertOut(t, e.svc.Receive(context.Background(), d), 401, "")
}

func TestReceive_ValidSignatureOutsideTheReplayWindowIsEvidenced(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	s := simple(c, "B-OLD", 1)
	s.tsOff = -3 * time.Hour
	o := e.deliver(s)
	assertOut(t, o, 401, integration.XICReplayWindowFailed)
	if o.Batch == nil || o.Batch.State != batch.StateRejected || o.Batch.ReasonCode != "REPLAY_WINDOW" {
		t.Fatalf("%+v", o.Batch)
	}
	if len(e.rowsOf(tenantA, o.Batch.BatchID)) != 0 || e.domHits != 0 {
		t.Fatal("a stale delivery was processed")
	}
}

func TestReceive_VaultOutageIsRetryableAndNothingIsStored(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.secrets.down = true
	o := e.deliver(simple(c, "B-1", 1))
	if o.Status != 503 || o.RetryAfter == 0 {
		t.Fatalf("%+v", o)
	}
	if l, _ := e.repo.List(ctxFor(tenantA), batch.Filter{}); len(l) != 0 {
		t.Fatal("stored")
	}
	e.secrets.down = false
	assertOut(t, e.deliver(simple(c, "B-1", 1)), 202, "")
}

func TestReceive_RateLimit(t *testing.T) {
	e := newEnv(t)
	cfg := batch.DefaultConfig()
	cfg.ConnRatePerMin = 3
	e.svc.Cfg = cfg
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	var limited *batch.Outcome
	for i := 0; i < 10 && limited == nil; i++ {
		s := simple(c, "B-RL", 1)
		s.secret = "wrong"
		if o := e.deliver(s); o.Status == 429 {
			limited = o
		}
	}
	if limited == nil || limited.RetryAfter == 0 || limited.XIC == nil || limited.XIC.Code != integration.XICRateLimited {
		t.Fatalf("%+v", limited)
	}
}

func TestReceive_ConnectionNotActiveIsRecordedAndRefused(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	cur, _ := e.connSvc.Connection(ctxFor(tenantA), c.ConnectionID)
	if _, err := e.connSvc.Suspend(ctxFor(tenantA), operator, c.ConnectionID, cur.Version, "provider incident"); err != nil {
		t.Fatal(err)
	}
	o := e.deliver(simple(c, "B-1", 1))
	assertOut(t, o, 409, integration.XICConnectionNotActive)
	if o.Batch == nil || o.Batch.State != batch.StateRejected {
		t.Fatalf("%+v", o.Batch)
	}
}

// ---- integrity is proven before a row is read (I-21, NP-33) ----------------------------------------------------

func TestVerify_FilesMustMatchTheirManifestBeforeAnyRowIsRead_NP33(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	data := []byte(csvFile(rows(5, "t")...))
	wrong := strings.Repeat("a", 64)
	short := int64(len(data) - 1)
	cases := map[string]bspec{
		"checksum differs":            {conn: c, id: "B-1", records: intp(5), files: []fileSpec{{name: "stmt-0001.csv", data: data, declared: &wrong}}},
		"size differs":                {conn: c, id: "B-2", records: intp(5), files: []fileSpec{{name: "stmt-0001.csv", data: data, size: &short}}},
		"declared file not delivered": {conn: c, id: "B-3", records: intp(5), files: []fileSpec{{name: "stmt-0001.csv", data: data}}, skip: []string{"stmt-0001.csv"}},
		"undeclared file delivered":   {conn: c, id: "B-4", records: intp(5), files: []fileSpec{{name: "stmt-0001.csv", data: data}}, extra: []batchDelivered{{Name: "stmt-0002.csv", Data: data}}},
		"name the contract refuses":   {conn: c, id: "B-5", records: intp(5), files: []fileSpec{{name: "payload.sh", data: data}}},
	}
	wantReason := map[string]string{"checksum differs": batch.ReasonIntegrity, "size differs": batch.ReasonIntegrity, "declared file not delivered": batch.ReasonFileMissing,
		"undeclared file delivered": batch.ReasonFileUnexpected, "name the contract refuses": batch.ReasonFileName}
	for name, s := range cases {
		o := e.deliver(s)
		assertOut(t, o, 422, integration.XICBatchIntegrityFailed)
		if o.Batch == nil || o.Batch.State != batch.StateRejected || o.Batch.ReasonCode != wantReason[name] {
			t.Errorf("%s: %+v", name, o.Batch)
			continue
		}
		// nothing was admitted and no content was kept
		if len(e.rowsOf(tenantA, o.Batch.BatchID)) != 0 {
			t.Errorf("%s: rows were admitted", name)
		}
		fs, _ := e.repo.Files(ctxFor(tenantA), o.Batch.BatchID, true)
		for _, f := range fs {
			if len(f.Content) != 0 {
				t.Errorf("%s: a refused file's content was kept", name)
			}
		}
	}
	if e.domHits != 0 {
		t.Fatal("the domain was called")
	}
	// the refusal is evidence of what was declared against what arrived, as hashes
	o := e.deliver(cases["checksum differs"])
	fs, _ := e.repo.Files(ctxFor(tenantA), o.Batch.BatchID, false)
	if len(fs) != 1 || fs[0].DeclaredSHA256 != wrong || fs[0].ActualSHA256 != sha(data) || fs[0].State != batch.FileRejected {
		t.Fatalf("%+v", fs)
	}
}

type batchDelivered = batch.DeliveredFile

func TestVerify_ManifestAndContractLimits(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	data := []byte(csvFile(rows(2, "t")...))
	base := func() bspec {
		return bspec{conn: c, id: "B-X", records: intp(2), files: []fileSpec{{name: "stmt-0001.csv", data: data}}}
	}
	for name, tc := range map[string]struct {
		mut    func(*bspec)
		status int
		code   integration.XICCode
		reason string
	}{
		"not a manifest":         {func(s *bspec) { s.manifest = []byte(`{"batch_id":"B-X","bogus":1}`) }, 422, integration.XICBatchIntegrityFailed, batch.ReasonManifestInvalid},
		"manifest not json":      {func(s *bspec) { s.manifest = []byte(`nope`) }, 422, integration.XICBatchIntegrityFailed, batch.ReasonManifestInvalid},
		"schema version":         {func(s *bspec) { s.schema = "stmt-csv@9" }, 422, integration.XICSchemaVersionUnsupported, batch.ReasonSchemaVersion},
		"record count required":  {func(s *bspec) { s.omitRec = true }, 422, integration.XICBatchIntegrityFailed, batch.ReasonRecordCount},
		"more rows than allowed": {func(s *bspec) { s.records = intp(5000) }, 422, integration.XICBatchIntegrityFailed, batch.ReasonTooManyRows},
		"too many files": {func(s *bspec) {
			for i := 2; i <= 7; i++ {
				s.files = append(s.files, fileSpec{name: "stmt-000" + string(rune('0'+i)) + ".csv", data: data})
			}
		}, 422, integration.XICBatchIntegrityFailed, batch.ReasonTooManyFiles},
	} {
		s := base()
		tc.mut(&s)
		o := e.deliver(s)
		assertOut(t, o, tc.status, tc.code)
		if o.Batch == nil || o.Batch.ReasonCode != tc.reason {
			t.Errorf("%s: %+v", name, o.Batch)
		}
	}
	// a file larger than the service stores
	cfg := batch.DefaultConfig()
	cfg.MaxFileBytes = 10
	e.svc.Cfg = cfg
	o := e.deliver(base())
	assertOut(t, o, 413, integration.XICBatchIntegrityFailed)
	if o.Batch.ReasonCode != batch.ReasonFileTooLarge {
		t.Fatalf("%+v", o.Batch)
	}
}

// ---- duplicates and conflicts (NP-37, NP-38) -------------------------------------------------------------------

func TestBatch_SameBatchIdAndContentReturnsTheOriginalWithoutReprocessing_NP37(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	first := e.deliver(simple(c, "B-DUP", 4))
	assertOut(t, first, 202, "")
	b := e.settle(e.process(first.Batch))
	hits := e.domHits
	events := len(e.repo.Out)
	for i := 0; i < 3; i++ {
		d := e.deliver(simple(c, "B-DUP", 4))
		if d.Status != 200 || !d.Duplicate || d.Batch.BatchID != first.Batch.BatchID {
			t.Fatalf("%+v", d)
		}
	}
	if got := e.process(first.Batch); got.Version == 0 {
		t.Fatal("unreachable")
	}
	b2 := e.get(tenantA, b.BatchID)
	if b2.State != batch.StateReconciled || b2.AdmittedRows != 4 || e.domHits != hits {
		t.Fatalf("a repeat was reprocessed: %+v hits=%d", b2, e.domHits)
	}
	if l, _ := e.repo.List(ctxFor(tenantA), batch.Filter{}); len(l) != 1 {
		t.Fatalf("%d batches", len(l))
	}
	if n := countType(e.repo.Types(), "integration.batch.duplicate"); n != 3 {
		t.Fatalf("%d duplicate events", n)
	}
	_ = events
	evs, _ := e.repo.Events(ctxFor(tenantA), b.BatchID)
	var dups int
	for _, ev := range evs {
		if ev.Kind == "DUPLICATE_DELIVERY" {
			dups++
		}
	}
	if dups != 3 {
		t.Fatalf("%d", dups)
	}
}

func TestBatch_SameBatchIdWithDifferentContentIsAConflictNotABatch_NP38(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	first := e.deliver(simple(c, "B-CF", 3))
	assertOut(t, first, 202, "")
	e.settle(e.process(first.Batch))
	other := simple(c, "B-CF", 3)
	other.files[0].data = []byte(csvFile(rows(3, "x")...)) // same identity, different rows
	o := e.deliver(other)
	assertOut(t, o, 409, integration.XICBatchIntegrityFailed)
	if o.Batch == nil || o.Batch.State != batch.StateQuarantined || o.Batch.ReasonCode != batch.ReasonConflict || o.Batch.OriginalBatchID != first.Batch.BatchID {
		t.Fatalf("%+v", o.Batch)
	}
	// the conflicting arrival is never processed, and the original is untouched
	if len(e.rowsOf(tenantA, o.Batch.BatchID)) != 0 {
		t.Fatal("the conflict was processed")
	}
	if got := e.get(tenantA, first.Batch.BatchID); got.State != batch.StateReconciled || got.AcceptedRows != 3 {
		t.Fatalf("%+v", got)
	}
	if !hasType(e.repo.Types(), "integration.batch.conflict") {
		t.Fatalf("%v", e.repo.Types())
	}
	if _, err := e.svc.Process(context.Background(), tenantA, o.Batch.BatchID); err != nil {
		t.Fatal(err)
	}
	if e.get(tenantA, o.Batch.BatchID).State != batch.StateQuarantined || e.domHits != 3 {
		t.Fatal("the conflict moved")
	}
}

// ---- content safety and expansion (NP-34) ---------------------------------------------------------------------

func TestProcess_GzipWithinLimitsIsReadAndABombIsQuarantined_NP34(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	// a normal gzip file is read
	data := gz(csvFile(rows(5, "t")...))
	o := e.deliver(bspec{conn: c, id: "B-GZ", records: intp(5), files: []fileSpec{{name: "stmt-0001.csv.gz", data: data, compression: "gzip"}}})
	assertOut(t, o, 202, "")
	if b := e.settle(e.process(o.Batch)); b.State != batch.StateReconciled || b.AcceptedRows != 5 {
		t.Fatalf("%+v", b)
	}
	// a bomb: 4 MB of a repeated line compresses to a few KB: past the contract's ratio and ceiling
	bomb := gz(csvFile(strings.Repeat("t0001,A1,10.5,GBP,2026-10-20T10:00:00Z\n", 100000)))
	o = e.deliver(bspec{conn: c, id: "B-BOMB", records: intp(100000), files: []fileSpec{{name: "stmt-0002.csv.gz", data: bomb, compression: "gzip"}}})
	// the manifest declares more rows than the contract allows: refused at the door. Declare a plausible count instead.
	o = e.deliver(bspec{conn: c, id: "B-BOMB2", records: intp(900), files: []fileSpec{{name: "stmt-0002.csv.gz", data: bomb, compression: "gzip"}}})
	assertOut(t, o, 202, "")
	b := e.process(o.Batch)
	if b.State != batch.StateQuarantined || b.ReasonCode != batch.ReasonDecompression {
		t.Fatalf("%+v", b)
	}
	if len(e.rowsOf(tenantA, b.BatchID)) != 0 {
		t.Fatal("rows of a bomb were admitted")
	}
	if !hasType(e.repo.Types(), "integration.security.batch-decompression-limit") {
		t.Fatalf("%v", e.repo.Types())
	}
	fs, _ := e.repo.Files(ctxFor(tenantA), b.BatchID, true)
	if len(fs) != 1 || len(fs[0].Content) != 0 || fs[0].State != batch.FileRejected {
		t.Fatalf("%+v", fs)
	}
}

func TestProcess_ContentThatIsNotPlainTextIsQuarantinedBeforeParsing(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	for name, data := range map[string][]byte{
		"binary with a NUL":   []byte("txn_id,account\x00\n"),
		"an executable":       append([]byte("MZ\x90\x00"), make([]byte, 200)...),
		"the EICAR test file": []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*` + "\n"),
		"invalid UTF-8":       []byte("txn_id,account,balance,currency,booked_at\nt1,\xff\xfe,1,GBP,x\n"),
	} {
		o := e.deliver(bspec{conn: c, id: "B-" + strings.ReplaceAll(name, " ", "-"), records: intp(1), files: []fileSpec{{name: "stmt-0001.csv", data: data}}})
		assertOut(t, o, 202, "")
		b := e.process(o.Batch)
		if b.State != batch.StateQuarantined || b.ReasonCode != batch.ReasonContentUnsafe {
			t.Errorf("%s: %+v", name, b)
		}
		if len(e.rowsOf(tenantA, b.BatchID)) != 0 {
			t.Errorf("%s: rows were admitted", name)
		}
	}
	if n := countType(e.repo.Types(), "integration.security.batch-content-unsafe"); n != 4 {
		t.Fatalf("%d security events", n)
	}
	if e.domHits != 0 {
		t.Fatal("the domain was called")
	}
}

func TestProcess_NoScannerFailsClosedAndTheBatchWaits(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	e.svc.Scanner = nil
	o := e.deliver(simple(c, "B-NS", 2))
	assertOut(t, o, 202, "")
	if _, err := e.svc.Process(context.Background(), tenantA, o.Batch.BatchID); err == nil {
		t.Fatal("a batch was processed with no scanner")
	}
	b := e.get(tenantA, o.Batch.BatchID)
	if b.State != batch.StateValidating || b.NextProcessAt == nil || len(e.rowsOf(tenantA, b.BatchID)) != 0 {
		t.Fatalf("never failed to clear a queue, never scanned as clean: %+v", b)
	}
}

func TestProcess_HeaderAndEncoding(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	bad := []byte("id,acct,amt\nt1,A1,1\n")
	o := e.deliver(bspec{conn: c, id: "B-H", records: intp(1), files: []fileSpec{{name: "stmt-0001.csv", data: bad}}})
	b := e.process(o.Batch)
	if b.State != batch.StateRejected || b.ReasonCode != batch.ReasonHeaderMismatch || len(e.rowsOf(tenantA, b.BatchID)) != 0 {
		t.Fatalf("%+v", b)
	}
	// a byte order mark and CRLF line endings are tolerated
	bom := []byte("\xef\xbb\xbftxn_id,account,balance,currency,booked_at\r\n" + row("t1") + "\r\n" + row("t2") + "\r\n")
	o = e.deliver(bspec{conn: c, id: "B-BOM", records: intp(2), files: []fileSpec{{name: "stmt-0002.csv", data: bom}}})
	if b := e.settle(e.process(o.Batch)); b.State != batch.StateReconciled || b.AcceptedRows != 2 {
		t.Fatalf("%+v", b)
	}
}

// ---- rows keep their own truth ---------------------------------------------------------------------------------

func TestRows_DuplicatesWithinAndAcrossBatchesAreNotAppliedTwice(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	data := []byte(csvFile(row("t1"), row("t2"), row("t1"), "t2,A1,99.99,GBP,2026-10-20T10:00:00Z"))
	o := e.deliver(bspec{conn: c, id: "B-R", records: intp(4), files: []fileSpec{{name: "stmt-0001.csv", data: data}}})
	b := e.settle(e.process(o.Batch))
	// t1 repeated: a duplicate; t2 repeated with other values: a conflict held apart, never a second effect
	if b.State != batch.StatePartial || b.AcceptedRows != 2 || b.DuplicateRows != 1 || b.RejectedRows != 1 || e.domHits != 2 {
		t.Fatalf("%+v hits=%d", b, e.domHits)
	}
	// the same rows in a later batch: duplicates again
	o2 := e.deliver(bspec{conn: c, id: "B-R2", records: intp(2), files: []fileSpec{{name: "stmt-0001.csv", data: []byte(csvFile(row("t1"), row("t2")))}}})
	b2 := e.settle(e.process(o2.Batch))
	if b2.DuplicateRows != 2 || b2.AcceptedRows != 0 || e.domHits != 2 {
		t.Fatalf("%+v hits=%d", b2, e.domHits)
	}
}

func TestRows_NoDomainRegisteredHoldsRowsAsDomainPrecondition(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a") // no handoff target
	o := e.deliver(simple(c, "B-NT", 3))
	b := e.process(o.Batch)
	if b.State != batch.StatePartial || b.QuarantinedRows != 3 || b.AcceptedRows != 0 {
		t.Fatalf("%+v", b)
	}
	for _, r := range e.rowsOf(tenantA, b.BatchID) {
		x, _ := e.inRepo.Get(ctxFor(tenantA), r.ExchangeID)
		if x.QuarantineClass != inbound.ClassDomainPrecondition || x.ReasonCode != "NO_HANDOFF_TARGET" {
			t.Fatalf("%+v", x)
		}
	}
}

func TestRows_NdjsonFilesUseTheSamePipeline(t *testing.T) {
	e := newEnv(t)
	v := e.publish(func(in *registryInput) {
		for i := range in.Content.Schemas {
			if in.Content.Schemas[i].Ref == "stmt-csv@1" {
				in.Content.Schemas[i].MediaType = "application/x-ndjson"
				in.Content.Schemas[i].ValidationProfile = json.RawMessage(`{"type":"object","required":["txn_id","account","balance","currency","booked_at"],"properties":{"txn_id":{"type":"string"},"account":{"type":"string"},"balance":{"type":"string"},"currency":{"type":"string"},"booked_at":{"type":"string"}}}`)
			}
		}
		for i := range in.Content.Profiles {
			if in.Content.Profiles[i].Name == "batch-stmt" {
				in.Content.Profiles[i].Spec = json.RawMessage(`{"file_name_pattern":"^stmt-[0-9]{4}\\.ndjson$","max_files":5,"max_rows":1000,"checksum_algorithm":"SHA-256","require_record_count":true}`)
			}
		}
	})
	c := e.activeConn(tenantA, v, "acc-a")
	e.target()
	data := []byte(`{"txn_id":"n1","account":"A1","balance":"10.5","currency":"GBP","booked_at":"2026-10-20T10:00:00Z"}` + "\n\n" +
		`{"txn_id":"n2","account":"A1","balance":"11","currency":"GBP","booked_at":"2026-10-20T10:00:00Z"}` + "\n" + `[1,2]` + "\n" + `not json` + "\n")
	o := e.deliver(bspec{conn: c, id: "B-ND", records: intp(4), files: []fileSpec{{name: "stmt-0001.ndjson", data: data}}})
	assertOut(t, o, 202, "")
	b := e.settle(e.process(o.Batch))
	if b.State != batch.StatePartial || b.AcceptedRows != 2 || b.RejectedRows != 2 || b.ParsedRows != 4 {
		t.Fatalf("%+v", b)
	}
	if rs := e.rowsOf(tenantA, b.BatchID, batch.RowRejected); len(rs) != 2 || rs[0].ReasonCode != batch.ReasonRowNotJSON {
		t.Fatalf("%+v", rs)
	}
}

// ---- resilience ---------------------------------------------------------------------------------------------

func TestProcess_ResumesWhereAnInterruptedRunStoppedAndNeverAdmitsARowTwice(t *testing.T) {
	e := newEnv(t)
	cfg := batch.DefaultConfig()
	cfg.RowChunk = 10
	e.svc.Cfg = cfg
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	o := e.deliver(simple(c, "B-RES", 35))
	assertOut(t, o, 202, "")
	// the inbound store fails after 12 rows have been admitted
	flaky := &flakyInbound{Repo: e.inRepo, failAfter: 12}
	e.in.Repo = flaky
	if _, err := e.svc.Process(context.Background(), tenantA, o.Batch.BatchID); err == nil {
		t.Fatal("the failure was swallowed")
	}
	b := e.get(tenantA, o.Batch.BatchID)
	if b.ProcessingDone || b.NextProcessAt == nil {
		t.Fatalf("an interrupted batch is retried, not finished: %+v", b)
	}
	stored := len(e.rowsOf(tenantA, b.BatchID))
	if stored == 0 || stored >= 35 {
		t.Fatalf("%d rows were saved", stored)
	}
	// the store recovers; the next run continues
	e.in.Repo = e.inRepo
	e.advance(time.Minute)
	if _, err := e.svc.DriveDue(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	got := e.settle(e.get(tenantA, b.BatchID))
	if got.State != batch.StateReconciled || got.AdmittedRows != 35 || got.AcceptedRows != 35 {
		t.Fatalf("%+v", got)
	}
	if n := len(e.rowsOf(tenantA, b.BatchID)); n != 35 {
		t.Fatalf("%d rows for 35 in the file", n)
	}
	if e.domHits != 35 {
		t.Fatalf("%d domain deliveries for 35 rows", e.domHits)
	}
	seen := map[int]bool{}
	for _, r := range e.rowsOf(tenantA, b.BatchID) {
		if seen[r.RowNo] {
			t.Fatalf("row %d admitted twice", r.RowNo)
		}
		seen[r.RowNo] = true
	}
}

type flakyInbound struct {
	inbound.Repo
	mu        sync.Mutex
	n         int
	failAfter int
}

func (f *flakyInbound) Insert(ctx context.Context, e *inbound.Exchange, ev inbound.Event, out []inbound.OutEvent) (*inbound.Exchange, error) {
	f.mu.Lock()
	f.n++
	fail := f.n > f.failAfter
	f.mu.Unlock()
	if fail {
		return nil, context.DeadlineExceeded
	}
	return f.Repo.Insert(ctx, e, ev, out)
}

func TestProcess_ConcurrentWorkersAdmitEachRowOnce(t *testing.T) {
	e := newEnv(t)
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	o := e.deliver(simple(c, "B-CC", 40))
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.svc.Process(context.Background(), tenantA, o.Batch.BatchID)
		}()
	}
	wg.Wait()
	b := e.settle(e.get(tenantA, o.Batch.BatchID))
	if n := len(e.rowsOf(tenantA, b.BatchID)); n != 40 || b.State != batch.StateReconciled || e.domHits != 40 {
		t.Fatalf("rows=%d hits=%d %+v", n, e.domHits, b)
	}
}

func TestWorker_DriveDueProcessesReceivedBatchesAndPurgesExpiredContent(t *testing.T) {
	e := newEnv(t)
	cfg := batch.DefaultConfig()
	cfg.ContentRetention = time.Hour
	e.svc.Cfg = cfg
	c := e.activeConn(tenantA, e.publish(nil), "acc-a")
	e.target()
	o := e.deliver(simple(c, "B-W", 3))
	n, err := e.svc.DriveDue(context.Background(), 10)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if b := e.get(tenantA, o.Batch.BatchID); !b.ProcessingDone {
		t.Fatalf("%+v", b)
	}
	if fs, _ := e.repo.Files(ctxFor(tenantA), o.Batch.BatchID, true); len(fs) != 1 || len(fs[0].Content) == 0 || fs[0].ContentExpiresAt == nil {
		t.Fatalf("%+v", fs)
	}
	if n, _ := e.repo.PurgeContent(context.Background(), e.at(), 10); n != 0 {
		t.Fatalf("purged %d early", n)
	}
	e.advance(2 * time.Hour)
	if n, _ := e.repo.PurgeContent(context.Background(), e.at(), 10); n != 1 {
		t.Fatalf("%d", n)
	}
	fs, _ := e.repo.Files(ctxFor(tenantA), o.Batch.BatchID, true)
	if len(fs[0].Content) != 0 || fs[0].ActualSHA256 == "" {
		t.Fatalf("the digest stays as evidence, the body goes: %+v", fs[0])
	}
	// reporting still works after the bodies are gone
	if _, err := e.svc.Process(context.Background(), tenantA, o.Batch.BatchID); err != nil {
		t.Fatal(err)
	}
}

func TestWorker_ContentOfSensitiveDataIsKeptOnlyForProcessing(t *testing.T) {
	e := newEnv(t)
	v := e.publish(func(in *registryInput) {
		for i := range in.Content.Operations {
			if in.Content.Operations[i].ID == "statement-file" {
				in.Content.Operations[i].DataClasses = []string{"FINANCIAL", "PERSONAL"}
			}
		}
	})
	c := e.activeConn(tenantA, v, "acc-a")
	e.target()
	o := e.deliver(simple(c, "B-P", 2))
	fs, _ := e.repo.Files(ctxFor(tenantA), o.Batch.BatchID, true)
	if fs[0].ContentExpiresAt == nil || fs[0].ContentExpiresAt.Sub(e.at()) > 2*time.Hour {
		t.Fatalf("a PERSONAL file is held only for processing: %+v", fs[0].ContentExpiresAt)
	}
	e.process(o.Batch)
	fs, _ = e.repo.Files(ctxFor(tenantA), o.Batch.BatchID, true)
	if len(fs[0].Content) != 0 {
		t.Fatal("a PERSONAL file was kept after processing")
	}
}

// ---- isolation ----------------------------------------------------------------------------------------------

func TestTenantsNeverSeeOrDriveEachOthersBatches(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	cA := e.activeConn(tenantA, v, "acc-a")
	cB := e.activeConn(tenantB, v, "acc-b")
	e.target()
	oA := e.deliver(simple(cA, "B-A", 2))
	assertOut(t, oA, 202, "")
	// tenant A's secret does not open tenant B's connection
	s := simple(cB, "B-X", 1)
	s.secret = secretA
	assertOut(t, e.deliver(s), 401, "")
	// tenant B delivers the same batch identity on its own connection: its own batch
	oB := e.deliver(bspec{conn: cB, id: "B-A", records: intp(2), secret: secretB, files: []fileSpec{{name: "stmt-0001.csv", data: []byte(csvFile(rows(2, "t")...))}}})
	assertOut(t, oB, 202, "")
	if oB.Batch.BatchID == oA.Batch.BatchID || oB.Batch.TenantID != tenantB {
		t.Fatalf("%+v", oB.Batch)
	}
	if _, err := e.repo.Get(ctxFor(tenantB), oA.Batch.BatchID); err == nil {
		t.Fatal("tenant B read tenant A's batch")
	}
	if l, _ := e.repo.List(ctxFor(tenantB), batch.Filter{}); len(l) != 1 || l[0].TenantID != tenantB {
		t.Fatalf("%+v", l)
	}
	if _, err := e.svc.Process(context.Background(), tenantB, oA.Batch.BatchID); err == nil {
		t.Fatal("tenant B drove tenant A's batch")
	}
	if e.get(tenantA, oA.Batch.BatchID).State != batch.StateReceived {
		t.Fatal("moved")
	}
	e.process(oA.Batch)
	e.process(oB.Batch)
	for _, r := range e.rowsOf(tenantB, oB.Batch.BatchID) {
		if r.TenantID != tenantB {
			t.Fatalf("%+v", r)
		}
	}
	_ = http.StatusOK
}
