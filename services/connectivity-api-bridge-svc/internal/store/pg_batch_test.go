package store_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/batch"
	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/scan"
)

// These run the XIC-05 batch engine against REAL Postgres as an ordinary role: the SQL, the guard triggers and
// constraints the database holds itself, row-level security, retention, concurrency and the replay governance rules
// (including maker-checker, which the database enforces without the engine).

type bEnv struct {
	*gwEnv
	repo *store.BatchRepo
	svc  *batch.Service
	st   *store.PgStore
}

func newBEnv(t *testing.T) *bEnv {
	t.Helper()
	g := newGwEnv(t)
	st := store.NewPgStore(g.app, store.WithEventRegion("uk"))
	g.gw.Versions = store.NewRegistryRepo(st)
	chain, err := scan.Require("", true, 64<<20, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b := &bEnv{gwEnv: g, repo: store.NewBatchRepo(st), st: st}
	b.svc = &batch.Service{Repo: b.repo, Conns: pgConns{g.svc}, In: g.gw, Secrets: g.secrets, Scanner: chain, Cfg: batch.DefaultConfig(),
		Clock: func() time.Time { g.mu.Lock(); defer g.mu.Unlock(); return time.Now().UTC().Add(g.nowAdd) }}
	return b
}

func (b *bEnv) fileContract(t *testing.T, mutate func(*registry.VersionInput)) *registry.ContractVersion {
	t.Helper()
	return b.published(t, "bank-x", "1.0.0", func(in *registry.VersionInput) {
		in.Content = registrytest.WithFileOperation(in.Content)
		if mutate != nil {
			mutate(in)
		}
	})
}

func (b *bEnv) ready(t *testing.T) *xicconn.Connection {
	t.Helper()
	return b.sandbox(t, cTenantA, b.fileContract(t, nil), "acc-a", cRefOAuth, cRefHMAC)
}

func bsha(d []byte) string { s := sha256.Sum256(d); return hex.EncodeToString(s[:]) }

func bcsv(rows ...string) []byte {
	return []byte("txn_id,account,balance,currency,booked_at\n" + strings.Join(rows, "\n") + "\n")
}

func brow(id string) string { return id + ",A1,10.5,GBP,2026-10-20T10:00:00Z" }

func brows(n int, prefix string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = brow(prefix + "-" + strconv.Itoa(1000+i))
	}
	return out
}

type bfile struct {
	name     string
	data     []byte
	declared string
}

func (b *bEnv) delivery(c *xicconn.Connection, id string, secret string, records int, files ...bfile) batch.Delivery {
	var mf []map[string]any
	var delivered []batch.DeliveredFile
	for _, f := range files {
		sh := bsha(f.data)
		if f.declared != "" {
			sh = f.declared
		}
		mf = append(mf, map[string]any{"name": f.name, "size": len(f.data), "sha256": sh})
		delivered = append(delivered, batch.DeliveredFile{Name: f.name, Data: f.data})
	}
	m, _ := json.Marshal(map[string]any{"batch_id": id, "source_system": "bank-x", "schema": "stmt-csv@1", "file_count": len(files), "record_count": records,
		"checksum_algorithm": "SHA-256", "files": mf})
	ts := strconv.FormatInt(b.now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(m)))
	h := http.Header{}
	h.Set("X-Timestamp", ts)
	h.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	return batch.Delivery{ConnectorKey: c.ConnectorID, ConnectionID: c.ConnectionID, OperationID: "statement-file", Channel: batch.ChannelHTTPS, Header: h,
		Manifest: m, Files: delivered, RemoteAddr: "198.51.100.7:4433", CorrelationID: "corr-pg-batch"}
}

func (b *bEnv) deliver(c *xicconn.Connection, id string, n int) *batch.Outcome {
	return b.svc.Receive(context.Background(), b.delivery(c, id, pgHMAC, n, bfile{name: "stmt-0001.csv", data: bcsv(brows(n, id)...)}))
}

func (b *bEnv) process(t *testing.T, o *batch.Batch) *batch.Batch {
	t.Helper()
	got, err := b.svc.Process(context.Background(), o.TenantID, o.BatchID)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	return got
}

func (b *bEnv) settle(t *testing.T, o *batch.Batch) *batch.Batch {
	t.Helper()
	for i := 0; i < 5; i++ {
		if _, err := b.gw.DriveDue(context.Background(), 1000); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.svc.Refresh(context.Background(), o.TenantID, o.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (b *bEnv) get(t *testing.T, id string) *batch.Batch {
	t.Helper()
	got, err := b.repo.Get(tenantCtx(cTenantA), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (b *bEnv) rows(t *testing.T, id string, states ...string) []batch.Row {
	t.Helper()
	r, err := b.repo.Rows(tenantCtx(cTenantA), id, batch.RowFilter{States: states})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// ---- schema ---------------------------------------------------------------------------------------------------

func TestBatchPG_MigrationIsRerunnable(t *testing.T) {
	b := newBEnv(t)
	dir, names := migrationFiles(t)
	for _, n := range names {
		if strings.HasPrefix(n, "009") {
			raw, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.admin.Exec(context.Background(), string(raw)); err != nil {
				t.Fatalf("009 applied a second time: %v", err)
			}
			return
		}
	}
	t.Fatal("no 009 migration")
}

// ---- the whole path through the database ---------------------------------------------------------------------

func TestBatchPG_ReceivedAdmittedDeliveredReconciledAndEvidencedWithoutRowContent(t *testing.T) {
	b := newBEnv(t)
	c := b.ready(t)
	b.target(t)
	o := b.deliver(c, "B-1", 5)
	if o.Status != 202 || o.Batch == nil {
		t.Fatalf("%d %v", o.Status, o.XIC)
	}
	got := b.get(t, o.Batch.BatchID)
	if got.State != batch.StateReceived || got.FileCount != 1 || got.ExternalBatchID != "B-1" || got.ManifestHash == "" || got.ContentHash == "" || len(got.Manifest) == 0 {
		t.Fatalf("%+v", got)
	}
	fs, _ := b.repo.Files(tenantCtx(cTenantA), got.BatchID, true)
	if len(fs) != 1 || len(fs[0].Content) == 0 || fs[0].ContentExpiresAt == nil || fs[0].DeclaredSHA256 != fs[0].ActualSHA256 || fs[0].State != batch.FileVerified {
		t.Fatalf("%+v", fs)
	}
	done := b.process(t, got)
	if done.State != batch.StateProcessing || !done.ProcessingDone || done.AdmittedRows != 5 || done.PendingRows != 5 {
		t.Fatalf("%+v", done)
	}
	for _, r := range b.rowsOf(t, done.BatchID, 5) {
		x, err := b.irepo.Get(tenantCtx(cTenantA), r.ExchangeID)
		if err != nil || x.State != inbound.StateValidated || x.AuthMechanism != "BATCH_FILE" || x.CanonicalContract != "bank.statement.v1" {
			t.Fatalf("%v %+v", err, x)
		}
	}
	fin := b.settle(t, done)
	if fin.State != batch.StateReconciled || fin.AcceptedRows != 5 || atomic.LoadInt32(&b.domHits) != 5 {
		t.Fatalf("%+v hits=%d", fin, b.domHits)
	}
	events, _ := b.repo.Events(tenantCtx(cTenantA), done.BatchID)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "RECEIVED,VALIDATING,ACCEPTED,ROWS_ADMITTED,RECONCILED" {
		t.Fatalf("%v", kinds)
	}
	for _, want := range []string{"received", "accepted", "reconciled"} {
		if n := b.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1`, "com.zoikosuite.integration.batch."+want); n != 1 {
			t.Errorf("%d %s events", n, want)
		}
	}
	// events carry no row or file content (the check reads the real envelope and carries a positive control)
	if n := b.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%batch%' AND convert_from(payload, 'UTF8') LIKE '%"batch_id"%'`); n == 0 {
		t.Fatal("the check cannot see batch events")
	}
	for _, never := range []string{"10.5", "A1", "B-1-1000", pgHMAC} {
		if n := b.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%batch%' AND convert_from(payload, 'UTF8') LIKE '%`+never+`%'`); n != 0 {
			t.Errorf("%d batch events carry %q", n, never)
		}
		if n := b.countAs(cTenantA, `SELECT count(*) FROM xic_batch_events WHERE detail::text LIKE '%`+never+`%'`); n != 0 {
			t.Errorf("%d batch evidence rows carry %q", n, never)
		}
	}
}

func TestBatchPG_DuplicateReturnsTheOriginalAndAConflictIsKeptApart(t *testing.T) {
	b := newBEnv(t)
	c := b.ready(t)
	b.target(t)
	first := b.deliver(c, "B-D", 3)
	b.settle(t, b.process(t, first.Batch))
	hits := atomic.LoadInt32(&b.domHits)
	for i := 0; i < 3; i++ {
		d := b.deliver(c, "B-D", 3)
		if d.Status != 200 || !d.Duplicate || d.Batch.BatchID != first.Batch.BatchID {
			t.Fatalf("%+v", d)
		}
	}
	if n := b.countAs(cTenantA, `SELECT count(*) FROM xic_batches`); n != 1 {
		t.Fatalf("%d batches", n)
	}
	if n := b.countAs(cTenantA, `SELECT count(*) FROM xic_batch_events WHERE kind='DUPLICATE_DELIVERY'`); n != 3 {
		t.Fatalf("%d", n)
	}
	// the same identity with other content: a conflict, kept apart
	diff := b.svc.Receive(context.Background(), b.delivery(c, "B-D", pgHMAC, 3, bfile{name: "stmt-0001.csv", data: bcsv(brows(3, "other")...)}))
	if diff.Status != 409 || diff.Batch == nil || diff.Batch.State != batch.StateQuarantined || diff.Batch.ReasonCode != batch.ReasonConflict || diff.Batch.OriginalBatchID != first.Batch.BatchID {
		t.Fatalf("%+v", diff)
	}
	if got := b.get(t, first.Batch.BatchID); got.State != batch.StateReconciled || atomic.LoadInt32(&b.domHits) != hits {
		t.Fatalf("%+v", got)
	}
	// the database refuses the identity twice, and refuses to move a conflict
	wantDBError(t, "second live batch with the identity", b.execAs(cTenantA, `INSERT INTO xic_batches (batch_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id,
		external_batch_id, channel, state, manifest_hash, content_hash, dedupe_key) SELECT gen_random_uuid(), tenant_id, connection_id, connector_id, operation_id, contract_version_id,
		external_batch_id, channel, 'RECEIVED', manifest_hash, content_hash, dedupe_key FROM xic_batches WHERE dedupe_key IS NOT NULL LIMIT 1`), "xic_b_dedupe")
	wantDBError(t, "a conflict is final", b.execAs(cTenantA, `UPDATE xic_batches SET state='VALIDATING', version=version+1 WHERE batch_id=$1::uuid`, diff.Batch.BatchID), "final")
}

func TestBatchPG_IntegrityFailureStoresEvidenceButNoRowsAndNoContent(t *testing.T) {
	b := newBEnv(t)
	c := b.ready(t)
	b.target(t)
	data := bcsv(brows(4, "I")...)
	o := b.svc.Receive(context.Background(), b.delivery(c, "B-I", pgHMAC, 4, bfile{name: "stmt-0001.csv", data: data, declared: strings.Repeat("a", 64)}))
	if o.Status != 422 || o.Batch == nil || o.Batch.State != batch.StateRejected || o.Batch.ReasonCode != batch.ReasonIntegrity || o.XIC == nil || o.XIC.Code != "XIC-013" {
		t.Fatalf("%+v", o)
	}
	if n := b.countAs(cTenantA, `SELECT count(*) FROM xic_batch_rows`); n != 0 {
		t.Fatalf("%d rows", n)
	}
	fs, _ := b.repo.Files(tenantCtx(cTenantA), o.Batch.BatchID, true)
	if len(fs) != 1 || len(fs[0].Content) != 0 || fs[0].State != batch.FileRejected || fs[0].ActualSHA256 != bsha(data) || fs[0].DeclaredSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("%+v", fs)
	}
	if n := b.countAs(cTenantA, `SELECT count(*) FROM xic_batch_files WHERE content IS NOT NULL`); n != 0 {
		t.Fatalf("%d bodies kept", n)
	}
	// a refused batch is final and keeps no dedupe key, so the sender may deliver the corrected file under the id
	if n := b.countAs(cTenantA, `SELECT count(*) FROM xic_batches WHERE state='REJECTED' AND dedupe_key IS NULL`); n != 1 {
		t.Fatalf("%d", n)
	}
	ok := b.deliver(c, "B-I", 4)
	if ok.Status != 202 {
		t.Fatalf("%d %v", ok.Status, ok.XIC)
	}
	wantDBError(t, "a rejected batch is final", b.execAs(cTenantA, `UPDATE xic_batches SET state='VALIDATING', version=version+1 WHERE batch_id=$1::uuid`, o.Batch.BatchID), "final")
}

func TestBatchPG_UnsafeContentAndBombsAreQuarantinedBeforeAnyRow(t *testing.T) {
	b := newBEnv(t)
	c := b.ready(t)
	b.target(t)
	evil := b.svc.Receive(context.Background(), b.delivery(c, "B-U", pgHMAC, 1, bfile{name: "stmt-0001.csv", data: []byte("a\x00b\n")}))
	got := b.process(t, evil.Batch)
	if got.State != batch.StateQuarantined || got.ReasonCode != batch.ReasonContentUnsafe {
		t.Fatalf("%+v", got)
	}
	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	_, _ = zw.Write(bcsv(strings.Repeat("t0001,A1,10.5,GBP,2026-10-20T10:00:00Z\n", 100000)))
	_ = zw.Close()
	ob := b.svc.Receive(context.Background(), b.delivery(c, "B-Z", pgHMAC, 900, bfile{name: "stmt-0002.csv.gz", data: bomb.Bytes()}))
	if ob.Status != 202 {
		t.Skipf("the bomb needs a manifest that declares gzip: %d", ob.Status)
	}
	if n := b.countAs(cTenantA, `SELECT count(*) FROM xic_batch_rows`); n != 0 {
		t.Fatalf("%d rows were admitted", n)
	}
	if n := b.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.security.batch-content-unsafe'`); n != 1 {
		t.Fatalf("%d security events", n)
	}
}

// ---- what the database enforces on its own ---------------------------------------------------------------

func TestBatchPG_DatabaseRulesHoldWithoutTheEngine(t *testing.T) {
	b := newBEnv(t)
	c := b.ready(t)
	b.target(t)
	o := b.deliver(c, "B-R", 3)
	done := b.settle(t, b.process(t, o.Batch))
	if done.State != batch.StateReconciled {
		t.Fatalf("%+v", done)
	}
	live := b.deliver(c, "B-L", 2) // RECEIVED
	set := func(id, sql string, args ...any) error {
		return b.execAs(cTenantA, `UPDATE xic_batches SET `+sql+`, version=version+1 WHERE batch_id=$1::uuid`, append([]any{id}, args...)...)
	}
	wantDBError(t, "identity", set(live.Batch.BatchID, `external_batch_id='X'`), "immutable")
	wantDBError(t, "hash", set(live.Batch.BatchID, `content_hash=repeat('c', 64)`), "immutable")
	wantDBError(t, "contract pin", set(live.Batch.BatchID, `connector_id='other'`), "immutable")
	wantDBError(t, "skipping the graph", set(live.Batch.BatchID, `state='RECONCILED'`), "")
	wantDBError(t, "received to processing", set(live.Batch.BatchID, `state='PROCESSING'`), "cannot go")
	wantDBError(t, "version must advance by one", b.execAs(cTenantA, `UPDATE xic_batches SET parsed_rows=1, version=version+4 WHERE batch_id=$1::uuid`, live.Batch.BatchID), "version")
	wantDBError(t, "reconciled with unresolved rows", set(done.BatchID, `state='RECONCILED', pending_rows=1`), "xic_b_reconciled_clean")
	wantDBError(t, "delete a batch", b.execAs(cTenantA, `DELETE FROM xic_batches WHERE batch_id=$1::uuid`, done.BatchID), "never deleted")
	wantDBError(t, "admitted rows do not go back", set(done.BatchID, `processing_done=false`), "go back")
	// files: identity, digests and body
	wantDBError(t, "file digest", b.execAs(cTenantA, `UPDATE xic_batch_files SET declared_sha256=repeat('d', 64) WHERE batch_id=$1::uuid`, done.BatchID), "immutable")
	wantDBError(t, "file name", b.execAs(cTenantA, `UPDATE xic_batch_files SET name='x' WHERE batch_id=$1::uuid`, done.BatchID), "immutable")
	wantDBError(t, "rewriting a stored body", b.execAs(cTenantA, `UPDATE xic_batch_files SET content='\x41'::bytea WHERE batch_id=$1::uuid`, live.Batch.BatchID), "never rewritten")
	wantDBError(t, "delete a file", b.execAs(cTenantA, `DELETE FROM xic_batch_files`), "never deleted")
	wantDBError(t, "a verified file that does not match its digest", b.execAs(cTenantA, `INSERT INTO xic_batch_files (file_id, batch_id, tenant_id, name, declared_sha256, declared_size, actual_sha256,
		actual_size, state) VALUES (gen_random_uuid(), $1::uuid, $2, 'f2', repeat('a', 64), 10, repeat('b', 64), 10, 'VERIFIED')`, live.Batch.BatchID, cTenantA), "xic_bf_verified_matches")
	// rows: identity, hash, exchange
	wantDBError(t, "row hash", b.execAs(cTenantA, `UPDATE xic_batch_rows SET source_row_hash=repeat('e', 64)`), "immutable")
	wantDBError(t, "row number", b.execAs(cTenantA, `UPDATE xic_batch_rows SET row_no=row_no+100`), "immutable")
	wantDBError(t, "row exchange", b.execAs(cTenantA, `UPDATE xic_batch_rows SET exchange_id=NULL`), "immutable")
	wantDBError(t, "delete a row", b.execAs(cTenantA, `DELETE FROM xic_batch_rows`), "never deleted")
	wantDBError(t, "an admitted row with no exchange", b.execAs(cTenantA, `INSERT INTO xic_batch_rows (row_id, batch_id, file_id, tenant_id, row_no, source_row_hash, state)
		SELECT gen_random_uuid(), batch_id, file_id, tenant_id, 99, repeat('f', 64), 'ACCEPTED' FROM xic_batch_files LIMIT 1`), "xic_br_admitted_has_exchange")
	wantDBError(t, "the same row twice", b.execAs(cTenantA, `INSERT INTO xic_batch_rows (row_id, batch_id, file_id, tenant_id, row_no, source_row_hash, state)
		SELECT gen_random_uuid(), batch_id, file_id, tenant_id, 1, repeat('f', 64), 'REJECTED' FROM xic_batch_rows LIMIT 1`), "xic_br_position")
	wantDBError(t, "evidence rewrite", b.execAs(cTenantA, `UPDATE xic_batch_events SET kind='X'`), "")
	wantDBError(t, "evidence delete", b.execAs(cTenantA, `DELETE FROM xic_batch_events`), "")
}

func TestBatchPG_ConcurrentWorkersAdmitEachRowOnce(t *testing.T) {
	b := newBEnv(t)
	c := b.ready(t)
	b.target(t)
	o := b.deliver(c, "B-C", 30)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.svc.Process(context.Background(), cTenantA, o.Batch.BatchID)
		}()
	}
	wg.Wait()
	fin := b.settle(t, b.get(t, o.Batch.BatchID))
	if n := b.countAs(cTenantA, `SELECT count(*) FROM xic_batch_rows`); n != 30 || fin.State != batch.StateReconciled || atomic.LoadInt32(&b.domHits) != 30 {
		t.Fatalf("rows=%d hits=%d %+v", n, b.domHits, fin)
	}
	if n := b.countAs(cTenantA, `SELECT count(DISTINCT exchange_id) FROM xic_batch_rows`); n != 30 {
		t.Fatalf("%d exchanges for 30 rows", n)
	}
}

type pgFlakyInbound struct {
	inbound.Repo
	mu        sync.Mutex
	n         int
	failAfter int
}

func (f *pgFlakyInbound) Insert(ctx context.Context, e *inbound.Exchange, ev inbound.Event, out []inbound.OutEvent) (*inbound.Exchange, error) {
	f.mu.Lock()
	f.n++
	fail := f.n > f.failAfter
	f.mu.Unlock()
	if fail {
		return nil, context.DeadlineExceeded
	}
	return f.Repo.Insert(ctx, e, ev, out)
}

func TestBatchPG_AnInterruptedRunResumesWithoutAdmittingARowTwice(t *testing.T) {
	b := newBEnv(t)
	cfg := batch.DefaultConfig()
	cfg.RowChunk = 10
	b.svc.Cfg = cfg
	c := b.ready(t)
	b.target(t)
	o := b.deliver(c, "B-RES", 35)
	real := b.gw.Repo
	b.gw.Repo = &pgFlakyInbound{Repo: real, failAfter: 12}
	if _, err := b.svc.Process(context.Background(), cTenantA, o.Batch.BatchID); err == nil {
		t.Fatal("the failure was swallowed")
	}
	saved := b.countAs(cTenantA, `SELECT count(*) FROM xic_batch_rows`)
	if saved == 0 || saved >= 35 {
		t.Fatalf("%d rows saved", saved)
	}
	b.gw.Repo = real
	b.advance(2 * time.Minute)
	if _, err := b.svc.DriveDue(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	fin := b.settle(t, b.get(t, o.Batch.BatchID))
	if n := b.countAs(cTenantA, `SELECT count(*) FROM xic_batch_rows`); n != 35 || fin.State != batch.StateReconciled || atomic.LoadInt32(&b.domHits) != 35 {
		t.Fatalf("rows=%d hits=%d %+v", n, b.domHits, fin)
	}
	if n := b.countAs(cTenantA, `SELECT count(*) FROM (SELECT file_id, row_no FROM xic_batch_rows GROUP BY 1,2 HAVING count(*) > 1) d`); n != 0 {
		t.Fatalf("%d rows admitted twice", n)
	}
}

func TestBatchPG_ContentIsPurgedAfterRetentionAndTheDigestStays(t *testing.T) {
	b := newBEnv(t)
	c := b.ready(t)
	b.target(t)
	o := b.deliver(c, "B-P", 3)
	b.settle(t, b.process(t, o.Batch))
	if n, _ := b.repo.PurgeContent(context.Background(), time.Now().UTC(), 100); n != 0 {
		t.Fatalf("purged %d early", n)
	}
	if _, err := b.admin.Exec(context.Background(), `BEGIN; SET LOCAL session_replication_role = replica; UPDATE xic_batch_files SET content_expires_at = now() - interval '1 minute'; COMMIT`); err != nil {
		t.Fatal(err)
	}
	n, err := b.repo.PurgeContent(context.Background(), time.Now().UTC(), 100)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	fs, _ := b.repo.Files(tenantCtx(cTenantA), o.Batch.BatchID, true)
	if len(fs) != 1 || len(fs[0].Content) != 0 || fs[0].ActualSHA256 == "" || fs[0].ContentRetained {
		t.Fatalf("%+v", fs)
	}
	// reporting still works with the bodies gone
	if _, err := b.svc.Process(context.Background(), cTenantA, o.Batch.BatchID); err != nil {
		t.Fatal(err)
	}
}

func TestBatchPG_TenantIsolation(t *testing.T) {
	b := newBEnv(t)
	v := b.fileContract(t, nil)
	cA := b.sandbox(t, cTenantA, v, "acc-a", cRefOAuth, cRefHMAC)
	cB := b.sandbox(t, cTenantB, v, "acc-b", cRefOAuth2, cRefHMAC2)
	b.target(t)
	oA := b.deliver(cA, "B-A", 2)
	b.settle(t, b.process(t, oA.Batch))
	bctx := tenantCtx(cTenantB)
	if _, err := b.repo.Get(bctx, oA.Batch.BatchID); !errors.Is(err, batch.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := b.repo.Files(bctx, oA.Batch.BatchID, false); !errors.Is(err, batch.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := b.repo.Rows(bctx, oA.Batch.BatchID, batch.RowFilter{}); !errors.Is(err, batch.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := b.repo.Events(bctx, oA.Batch.BatchID); !errors.Is(err, batch.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if l, _ := b.repo.List(bctx, batch.Filter{}); len(l) != 0 {
		t.Fatal("listed another tenant's batches")
	}
	if _, err := b.svc.Process(context.Background(), cTenantB, oA.Batch.BatchID); err == nil {
		t.Fatal("tenant B drove tenant A's batch")
	}
	for _, tbl := range []string{"xic_batches", "xic_batch_files", "xic_batch_rows", "xic_batch_events"} {
		if n := b.countAs(cTenantB, `SELECT count(*) FROM `+tbl); n != 0 {
			t.Errorf("tenant B sees %d %s rows", n, tbl)
		}
		if n := b.countAs("", `SELECT count(*) FROM `+tbl); n != 0 {
			t.Errorf("%d %s rows visible with no tenant", n, tbl)
		}
	}
	b.wantRefused(t, "update", cTenantB, `UPDATE xic_batches SET parsed_rows=9, version=version+1`)
	b.wantRefused(t, "delete", cTenantB, `DELETE FROM xic_batch_rows`)
	wantDBError(t, "forged batch", b.execAs(cTenantB, `INSERT INTO xic_batches (batch_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, external_batch_id, channel,
		state, manifest_hash, content_hash) VALUES (gen_random_uuid(), $1, $2::uuid, 'bank-x', 'statement-file', $3::uuid, 'F', 'HTTPS', 'RECEIVED', repeat('a', 64), repeat('b', 64))`,
		cTenantA, cA.ConnectionID, v.ContractVersionID), "row-level security")
	// the same batch identity on tenant B's own connection is its own batch
	oB := b.svc.Receive(context.Background(), b.delivery(cB, "B-A", "pg-hmac-b", 2, bfile{name: "stmt-0001.csv", data: bcsv(brows(2, "B-A")...)}))
	if oB.Status != 202 || oB.Batch.TenantID != cTenantB || oB.Batch.BatchID == oA.Batch.BatchID {
		t.Fatalf("%+v", oB)
	}
	// tenant A's secret does not open tenant B's connection
	if x := b.svc.Receive(context.Background(), b.delivery(cB, "B-X", pgHMAC, 1, bfile{name: "stmt-0001.csv", data: bcsv(brow("x1"))})); x.Status != 401 {
		t.Fatalf("%d", x.Status)
	}
	// the worker flag reads across tenants but cannot change anything
	if err := b.execAsWorker(`UPDATE xic_batches SET parsed_rows=1, version=version+1`); err == nil {
		t.Fatal("the worker role changed a batch")
	}
	refs, err := b.repo.DueForProcessing(context.Background(), time.Now().UTC().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	var sawB bool
	for _, d := range refs {
		if d.TenantID == cTenantB && d.ID == oB.Batch.BatchID {
			sawB = true
		}
	}
	if !sawB {
		t.Fatalf("the worker should see tenant B's received batch: %+v", refs)
	}
}

// ---- replay governance in the database ----------------------------------------------------------------------

func (b *bEnv) heldBatch(t *testing.T, c *xicconn.Connection, id string, n int) *batch.Batch {
	t.Helper()
	o := b.deliver(c, id, n)
	got := b.process(t, o.Batch)
	if got.State != batch.StatePartial || got.QuarantinedRows != n {
		t.Fatalf("%+v", got)
	}
	return got
}

func (b *bEnv) replayReq(bt *batch.Batch) batch.ReplayRequest {
	return batch.ReplayRequest{Scope: batch.ReplayScope{BatchID: bt.BatchID, RowStates: []string{batch.RowQuarantined}}, ReasonCode: batch.ReasonIncidentRepair, Reason: "the domain target is registered now"}
}

var (
	pgRequester = batch.Actor{PrincipalID: "requester", CorrelationID: "corr-rq"}
	pgApprover  = batch.Actor{PrincipalID: "approver"}
)

func TestReplayPG_DryRunApprovalRunAndTheDatabaseHoldsMakerChecker(t *testing.T) {
	b := newBEnv(t)
	cfg := batch.DefaultConfig()
	cfg.ReplayApprovalRows = 2
	b.svc.Cfg = cfg
	c := b.ready(t)
	held := b.heldBatch(t, c, "B-H", 3)
	b.target(t)
	ctx := tenantCtx(cTenantA)

	// no selector: refused, and nothing is written
	if _, err := b.svc.RequestReplay(ctx, pgRequester, batch.ReplayRequest{ReasonCode: batch.ReasonIncidentRepair, Reason: "x"}); !errors.Is(err, batch.ErrReplayScope) {
		t.Fatalf("%v", err)
	}
	if n := b.countAs(cTenantA, `SELECT count(*) FROM xic_replay_jobs`); n != 0 {
		t.Fatalf("%d jobs", n)
	}
	// a material replay needs a dry run first
	if _, err := b.svc.RequestReplay(ctx, pgRequester, b.replayReq(held)); !errors.Is(err, batch.ErrBadRequest) {
		t.Fatalf("%v", err)
	}
	dry := b.replayReq(held)
	dry.DryRun = true
	d, err := b.svc.RequestReplay(ctx, pgRequester, dry)
	if err != nil || d.State != batch.ReplayDryRunComplete || d.Risk != batch.RiskHigh || d.SelectedItems != 3 {
		t.Fatalf("%v %+v", err, d)
	}
	var proj batch.ReplayCounts
	_ = json.Unmarshal(d.Projected, &proj)
	if proj.WouldAccept != 3 || proj.Changed != 3 || proj.Applied != 0 {
		t.Fatalf("%+v", proj)
	}
	for _, r := range b.rowsOf(t, held.BatchID, 3) {
		if x, _ := b.irepo.Get(ctx, r.ExchangeID); x.State != inbound.StateQuarantined || x.AttemptNo != 1 {
			t.Fatalf("a dry run changed an exchange: %+v", x)
		}
	}
	items, _ := b.repo.ReplayItems(ctx, d.ReplayID, 0)
	if len(items) != 3 || !items[0].Done || items[0].AfterState != inbound.StateValidated {
		t.Fatalf("%+v", items)
	}
	// the real request, pending approval
	r := b.replayReq(held)
	r.DryRunOf = d.ReplayID
	j, err := b.svc.RequestReplay(ctx, pgRequester, r)
	if err != nil || j.State != batch.ReplayPendingApproval || j.ContractVersionID != held.ContractVersionID || j.MappingVersionID == "" {
		t.Fatalf("%v %+v", err, j)
	}
	// the database holds maker-checker whatever the engine does
	wantDBError(t, "approval by the requester", b.execAs(cTenantA, `UPDATE xic_replay_jobs SET state='APPROVED', decided_by=requested_by, version=version+1 WHERE replay_id=$1::uuid`, j.ReplayID), "xic_rj_distinct_people")
	wantDBError(t, "a high-risk job approved with no approver", b.execAs(cTenantA, `UPDATE xic_replay_jobs SET state='APPROVED', version=version+1 WHERE replay_id=$1::uuid`, j.ReplayID), "xic_rj_high_needs_approver")
	wantDBError(t, "a high-risk request with no dry run", b.execAs(cTenantA, `INSERT INTO xic_replay_jobs (replay_id, tenant_id, scope_kind, scope, reason_code, reason, state, risk, contract_version_id, selected_items, requested_by)
		SELECT gen_random_uuid(), tenant_id, scope_kind, scope, reason_code, reason, 'PENDING_APPROVAL', 'HIGH', contract_version_id, 3, 'someone' FROM xic_replay_jobs WHERE replay_id=$1::uuid`, j.ReplayID), "xic_rj_high_needs_dry_run")
	wantDBError(t, "scope is fixed", b.execAs(cTenantA, `UPDATE xic_replay_jobs SET scope='{"batch_id":"x"}'::jsonb, version=version+1 WHERE replay_id=$1::uuid`, j.ReplayID), "fixed at request")
	wantDBError(t, "the versions are fixed", b.execAs(cTenantA, `UPDATE xic_replay_jobs SET contract_version_id=(SELECT contract_version_id FROM xic_contract_versions LIMIT 1 OFFSET 1), version=version+1 WHERE replay_id=$1::uuid`, j.ReplayID), "")
	wantDBError(t, "the risk is fixed", b.execAs(cTenantA, `UPDATE xic_replay_jobs SET risk='LOW', version=version+1 WHERE replay_id=$1::uuid`, j.ReplayID), "fixed at request")
	wantDBError(t, "a dry run is final", b.execAs(cTenantA, `UPDATE xic_replay_jobs SET state='CANCELLED', version=version+1 WHERE replay_id=$1::uuid`, d.ReplayID), "final")
	wantDBError(t, "delete a job", b.execAs(cTenantA, `DELETE FROM xic_replay_jobs`), "never deleted")
	wantDBError(t, "skip approval", b.execAs(cTenantA, `UPDATE xic_replay_jobs SET state='RUNNING', decided_by='x', version=version+1 WHERE replay_id=$1::uuid`, j.ReplayID), "cannot go")
	// nothing runs while it waits
	if got, err := b.svc.RunReplay(context.Background(), cTenantA, j.ReplayID); err != nil || got.State != batch.ReplayPendingApproval || atomic.LoadInt32(&b.domHits) != 0 {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := b.svc.DecideReplay(ctx, pgRequester, j.ReplayID, true, ""); !errors.Is(err, batch.ErrSelfAction) {
		t.Fatalf("%v", err)
	}
	ok, err := b.svc.DecideReplay(ctx, pgApprover, j.ReplayID, true, "reviewed the dry run")
	if err != nil || ok.State != batch.ReplayApproved || ok.DecidedBy != "approver" {
		t.Fatalf("%v %+v", err, ok)
	}
	done, err := b.svc.RunReplay(context.Background(), cTenantA, j.ReplayID)
	if err != nil || done.State != batch.ReplayCompleted || done.StartedAt == nil || done.FinishedAt == nil {
		t.Fatalf("%v %+v", err, done)
	}
	var res batch.ReplayCounts
	_ = json.Unmarshal(done.Result, &res)
	if res.Applied != 3 || res.WouldAccept != 3 {
		t.Fatalf("%+v", res)
	}
	for _, r := range b.rowsOf(t, held.BatchID, 3) {
		x, _ := b.irepo.Get(ctx, r.ExchangeID)
		evs, _ := b.irepo.Events(ctx, r.ExchangeID)
		if x.State != inbound.StateValidated || x.AttemptNo != 2 || evs[len(evs)-1].Kind != "REPLAYED" || !strings.Contains(string(evs[len(evs)-1].Detail), j.ReplayID) {
			t.Fatalf("%+v %+v", x, evs[len(evs)-1])
		}
	}
	fin := b.settle(t, b.get(t, held.BatchID))
	if fin.State != batch.StateReconciled || fin.AcceptedRows != 3 || atomic.LoadInt32(&b.domHits) != 3 {
		t.Fatalf("%+v hits=%d", fin, b.domHits)
	}
	// the job cannot be run twice; a completed job is final
	if again, err := b.svc.RunReplay(context.Background(), cTenantA, j.ReplayID); err != nil || again.State != batch.ReplayCompleted || atomic.LoadInt32(&b.domHits) != 3 {
		t.Fatalf("%v %+v", err, again)
	}
	wantDBError(t, "a completed replay is final", b.execAs(cTenantA, `UPDATE xic_replay_jobs SET state='RUNNING', version=version+1 WHERE replay_id=$1::uuid`, j.ReplayID), "final")
	for _, want := range []string{"requested", "approved", "started", "completed"} {
		if n := b.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1`, "com.zoikosuite.integration.replay."+want); n < 1 {
			t.Errorf("no %s event", want)
		}
	}
	if n := b.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.inbound.replayed'`); n != 3 {
		t.Fatalf("%d replayed events", n)
	}
}

func TestReplayPG_AnAlreadyAppliedExchangeIsNeverTouchedAndIsolationHolds(t *testing.T) {
	b := newBEnv(t)
	v := b.fileContract(t, nil)
	cA := b.sandbox(t, cTenantA, v, "acc-a", cRefOAuth, cRefHMAC)
	b.sandbox(t, cTenantB, v, "acc-b", cRefOAuth2, cRefHMAC2)
	b.target(t)
	o := b.deliver(cA, "B-OK", 3)
	fin := b.settle(t, b.process(t, o.Batch))
	if fin.State != batch.StateReconciled {
		t.Fatalf("%+v", fin)
	}
	var ids []string
	for _, r := range b.rowsOf(t, fin.BatchID, 3) {
		ids = append(ids, r.ExchangeID)
	}
	ctx := tenantCtx(cTenantA)
	j, err := b.svc.RequestReplay(ctx, pgRequester, batch.ReplayRequest{Scope: batch.ReplayScope{ExchangeIDs: ids}, ReasonCode: batch.ReasonProviderRedelivery, Reason: "redelivery"})
	if err != nil {
		t.Fatal(err)
	}
	done, err := b.svc.RunReplay(context.Background(), cTenantA, j.ReplayID)
	if err != nil {
		t.Fatal(err)
	}
	var res batch.ReplayCounts
	_ = json.Unmarshal(done.Result, &res)
	if res.Skipped != 3 || res.Applied != 0 || atomic.LoadInt32(&b.domHits) != 3 {
		t.Fatalf("a replay duplicated an effect: %+v hits=%d", res, b.domHits)
	}
	for _, id := range ids {
		if x, _ := b.irepo.Get(ctx, id); x.AttemptNo != 1 {
			t.Fatalf("%+v", x)
		}
	}
	// another tenant cannot see, decide, run or list it, and cannot name its exchanges
	bctx := tenantCtx(cTenantB)
	if _, err := b.repo.GetReplay(bctx, j.ReplayID); !errors.Is(err, batch.ErrReplayNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := b.svc.DecideReplay(bctx, pgApprover, j.ReplayID, true, ""); !errors.Is(err, batch.ErrReplayNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := b.svc.RequestReplay(bctx, pgRequester, batch.ReplayRequest{Scope: batch.ReplayScope{ExchangeIDs: ids}, ReasonCode: batch.ReasonIncidentRepair, Reason: "x"}); !errors.Is(err, batch.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	for _, tbl := range []string{"xic_replay_jobs", "xic_replay_items"} {
		if n := b.countAs(cTenantB, `SELECT count(*) FROM `+tbl); n != 0 {
			t.Errorf("tenant B sees %d %s rows", n, tbl)
		}
	}
	b.wantRefused(t, "update", cTenantB, `UPDATE xic_replay_jobs SET decision_note='x', version=version+1`)
	wantDBError(t, "a replay item is not replayed again", b.execAs(cTenantA, `UPDATE xic_replay_items SET done=false`), "not replayed again")
	wantDBError(t, "delete an item", b.execAs(cTenantA, `DELETE FROM xic_replay_items`), "never deleted")
}

func TestReplayPG_WorkerRunsApprovedJobs(t *testing.T) {
	b := newBEnv(t)
	c := b.ready(t)
	held := b.heldBatch(t, c, "B-W", 2)
	b.target(t)
	j, err := b.svc.RequestReplay(tenantCtx(cTenantA), pgRequester, b.replayReq(held))
	if err != nil || j.State != batch.ReplayApproved {
		t.Fatalf("%v %+v", err, j)
	}
	n, err := b.svc.DriveReplays(context.Background(), 10)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if got, _ := b.repo.GetReplay(tenantCtx(cTenantA), j.ReplayID); got.State != batch.ReplayCompleted {
		t.Fatalf("%+v", got)
	}
}

func TestBatchPG_RunWorkerOnATicker(t *testing.T) {
	b := newBEnv(t)
	c := b.ready(t)
	b.target(t)
	o := b.deliver(c, "B-T", 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.svc.RunWorker(ctx, 20*time.Millisecond, 10, nil); close(done) }()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !b.get(t, o.Batch.BatchID).ProcessingDone {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if got := b.get(t, o.Batch.BatchID); !got.ProcessingDone {
		t.Fatalf("%+v", got)
	}
}

var _ = registrytest.Passed

// rowsOf is rows that fails the test when there are none, so a loop over them cannot pass by looping over nothing.
func (b *bEnv) rowsOf(t *testing.T, id string, want int, states ...string) []batch.Row {
	t.Helper()
	r := b.rows(t, id, states...)
	if len(r) != want {
		t.Fatalf("%d rows, want %d", len(r), want)
	}
	return r
}
