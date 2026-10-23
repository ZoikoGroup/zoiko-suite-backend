package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/batch"
	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/scan"
)

type bHarness struct {
	*connHarness
	svc  *batch.Service
	repo *batch.MemRepo
	root chi.Router
	now  time.Time
	max  int64
}

func newBHarness(t *testing.T) *bHarness {
	t.Helper()
	ch := newConnHarnessWith(t, true)
	h := &bHarness{connHarness: ch, repo: batch.NewMemRepo(), now: registrytest.T0.Add(72 * time.Hour), max: 1 << 20}
	ir := inbound.NewMemRepo()
	secrets := memSecrets{m: map[string]string{strings.ToLower(hRefHMAC): hmacSecret}}
	gw := &inbound.Service{Repo: ir, Conns: ch.svc, Secrets: secrets, Cfg: inbound.DefaultConfig(), Clock: func() time.Time { return h.now }, Versions: ch.repo,
		Handoff: inbound.HandoffDeps{AllowedRegions: []string{"uk"}}}
	chain, err := scan.Require("", true, 64<<20, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h.svc = &batch.Service{Repo: h.repo, Conns: ch.svc, In: gw, Secrets: secrets, Scanner: chain, Cfg: batch.DefaultConfig(), Clock: func() time.Time { return h.now }}
	srv := httptest.NewServer(ch.authz.handler())
	t.Cleanup(srv.Close)
	api := chi.NewRouter()
	api.Use(middleware.TenantContext)
	RegisterBatchAdminRoutes(api, NewBatchAdmin(h.svc, h.repo, authz.NewClient(srv.URL), zap.NewNop()))
	RegisterConnectionRoutes(api, NewConnection(ch.svc, ch.conns, authz.NewClient(srv.URL), zap.NewNop()))
	root := chi.NewRouter()
	root.Mount("/v1/batch-intake", NewBatchIntake(h.svc, h.max, false, zap.NewNop()).Router())
	root.Mount("/", api)
	h.root = root
	return h
}

func (h *bHarness) adm(method, path, tenant, principal string, body any) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", principal)
	w := httptest.NewRecorder()
	h.root.ServeHTTP(w, req)
	return w
}

type hfile struct {
	name string
	data []byte
	sum  string // declared digest, when it should not match
}

func csvBody(id string, n int) []byte {
	var sb strings.Builder
	sb.WriteString("txn_id,account,balance,currency,booked_at\n")
	for i := 0; i < n; i++ {
		sb.WriteString(id + "-" + strconv.Itoa(1000+i) + ",A1,10.5,GBP,2026-10-20T10:00:00Z\n")
	}
	return []byte(sb.String())
}

// deliver posts a batch exactly as an external sender would: multipart, no tenant, no principal.
func (h *bHarness) deliver(c xicconn.Connection, id, secret string, files ...hfile) *httptest.ResponseRecorder {
	return h.deliverTo("/v1/batch-intake/"+c.ConnectorID+"/"+c.ConnectionID+"/statement-file", id, secret, true, files...)
}

func (h *bHarness) deliverTo(path, id, secret string, signed bool, files ...hfile) *httptest.ResponseRecorder {
	var mf []map[string]any
	rows := 0
	for _, f := range files {
		sum := sha256.Sum256(f.data)
		d := hex.EncodeToString(sum[:])
		if f.sum != "" {
			d = f.sum
		}
		mf = append(mf, map[string]any{"name": f.name, "size": len(f.data), "sha256": d})
		rows += bytes.Count(f.data, []byte("\n")) - 1
	}
	m, _ := json.Marshal(map[string]any{"batch_id": id, "source_system": "bank-x", "schema": "stmt-csv@1", "file_count": len(files), "record_count": rows,
		"checksum_algorithm": "SHA-256", "files": mf})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	pw, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="manifest"`}, "Content-Type": {"application/json"}})
	_, _ = pw.Write(m)
	for _, f := range files {
		fw, _ := mw.CreateFormFile("file", f.name)
		_, _ = fw.Write(f.data)
	}
	_ = mw.Close()
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if signed {
		ts := strconv.FormatInt(h.now.Unix(), 10)
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Signature", hsign(secret, ts, string(m)))
	}
	req.RemoteAddr = "198.51.100.9:5555"
	w := httptest.NewRecorder()
	h.root.ServeHTTP(w, req)
	return w
}

func (h *bHarness) conn(t *testing.T) xicconn.Connection {
	t.Helper()
	return h.active(t, hTenantA, "acc-batch")
}

func TestBatchIntakeHTTP_AcceptedDuplicateAndRefusals(t *testing.T) {
	h := newBHarness(t)
	c := h.conn(t)
	data := csvBody("B1", 3)
	w := h.deliver(c, "B-1", hmacSecret, hfile{name: "stmt-0001.csv", data: data})
	var acc map[string]any
	decodeInto(t, w, &acc)
	if w.Code != 202 || acc["transport"] != "ACCEPTED" || acc["state"] != "RECEIVED" || acc["batch_id"] == "" || acc["external_batch_id"] != "B-1" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "B1-1000") || strings.Contains(w.Body.String(), "GBP") {
		t.Fatal("the response echoes file content")
	}
	// the same delivery again: 200 with the original reference
	w2 := h.deliver(c, "B-1", hmacSecret, hfile{name: "stmt-0001.csv", data: data})
	var dup map[string]any
	decodeInto(t, w2, &dup)
	if w2.Code != 200 || dup["duplicate"] != true || dup["batch_id"] != acc["batch_id"] {
		t.Fatalf("%d %s", w2.Code, w2.Body.String())
	}
	// the same identity with other content: 409, the original untouched, no values in the answer
	w3 := h.deliver(c, "B-1", hmacSecret, hfile{name: "stmt-0001.csv", data: csvBody("OTHER", 3)})
	p := isProblem(t, w3, 409)
	if p["batch_id"] == nil || strings.Contains(w3.Body.String(), "OTHER-1000") {
		t.Fatalf("%v", p)
	}
	// a digest that does not match the file: XIC-013, nothing admitted
	w4 := h.deliver(c, "B-2", hmacSecret, hfile{name: "stmt-0001.csv", data: csvBody("B2", 2), sum: strings.Repeat("a", 64)})
	p = isProblem(t, w4, 422)
	if p["code"] != "XIC-013" || p["state"] != "REJECTED" {
		t.Fatalf("%v", p)
	}
	for _, k := range []string{"type", "title", "status", "code", "name", "retryable"} {
		if _, ok := p[k]; !ok {
			t.Errorf("problem lacks %s", k)
		}
	}
	// a file the contract's naming rule does not allow
	w5 := h.deliver(c, "B-3", hmacSecret, hfile{name: "anything.csv", data: csvBody("B3", 1)})
	if w5.Code != 422 {
		t.Fatalf("%d %s", w5.Code, w5.Body.String())
	}
}

func TestBatchIntakeHTTP_AuthenticationFailureIsUniformAndNeedsNoTenantHeaders(t *testing.T) {
	h := newBHarness(t)
	c := h.conn(t)
	f := hfile{name: "stmt-0001.csv", data: csvBody("B1", 1)}
	bodies := map[string]string{}
	base := "/v1/batch-intake/"
	for name, w := range map[string]*httptest.ResponseRecorder{
		"bad secret":         h.deliver(c, "B-1", "wrong", f),
		"unsigned":           h.deliverTo(base+c.ConnectorID+"/"+c.ConnectionID+"/statement-file", "B-1", "", false, f),
		"unknown connection": h.deliverTo(base+"bank-x/00000000-0000-4000-8000-000000000000/statement-file", "B-1", hmacSecret, true, f),
		"wrong connector":    h.deliverTo(base+"bank-y/"+c.ConnectionID+"/statement-file", "B-1", hmacSecret, true, f),
		"malformed id":       h.deliverTo(base+"bank-x/x/statement-file", "B-1", hmacSecret, true, f),
	} {
		p := isProblem(t, w, 401)
		delete(p, "trace_id")
		raw, _ := json.Marshal(p)
		bodies[name] = string(raw)
	}
	var first string
	for name, b := range bodies {
		if first == "" {
			first = b
		}
		if b != first {
			t.Errorf("%s differs from the others: %s vs %s", name, b, first)
		}
	}
	// nothing was recorded for any of them
	if l, _ := h.repo.List(middleware.WithTenant(context.Background(), hTenantA), batch.Filter{}); len(l) != 0 {
		t.Fatalf("%d batches were recorded for unauthenticated calls", len(l))
	}
}

func TestBatchIntakeHTTP_TransportRefusals(t *testing.T) {
	h := newBHarness(t)
	c := h.conn(t)
	path := "/v1/batch-intake/" + c.ConnectorID + "/" + c.ConnectionID + "/statement-file"
	// not multipart
	req := httptest.NewRequest("POST", path, strings.NewReader(`{"x":1}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.root.ServeHTTP(w, req)
	if w.Code != 415 {
		t.Fatalf("%d", w.Code)
	}
	// declared larger than the cap: refused before it is read
	req = httptest.NewRequest("POST", path, strings.NewReader("x"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	req.ContentLength = h.max + 1
	w = httptest.NewRecorder()
	h.root.ServeHTTP(w, req)
	if w.Code != 413 {
		t.Fatalf("%d", w.Code)
	}
	// a body that turns out larger than the cap although it did not say so
	big := hfile{name: "stmt-0001.csv", data: bytes.Repeat([]byte("x"), int(h.max)+10)}
	w = h.deliver(c, "B-BIG", hmacSecret, big)
	if w.Code != 413 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// no manifest at all
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "stmt-0001.csv")
	_, _ = fw.Write([]byte("a"))
	_ = mw.Close()
	req = httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Timestamp", strconv.FormatInt(h.now.Unix(), 10))
	req.Header.Set("X-Signature", "00")
	w = httptest.NewRecorder()
	h.root.ServeHTTP(w, req)
	if w.Code != 401 && w.Code != 400 && w.Code != 422 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// only POST
	req = httptest.NewRequest("GET", path, nil)
	w = httptest.NewRecorder()
	h.root.ServeHTTP(w, req)
	if w.Code != 405 && w.Code != 404 {
		t.Fatalf("%d", w.Code)
	}
}

func TestBatchAdminHTTP_ReadsAreTenantScopedAuthorizedAndNeverReturnContent(t *testing.T) {
	h := newBHarness(t)
	c := h.conn(t)
	w := h.deliver(c, "B-1", hmacSecret, hfile{name: "stmt-0001.csv", data: csvBody("B1", 3)})
	var acc map[string]any
	decodeInto(t, w, &acc)
	id := acc["batch_id"].(string)
	g := h.adm("GET", "/v1/batches/"+id, hTenantA, "reader", nil)
	var b batch.Batch
	decodeInto(t, g, &b)
	if g.Code != 200 || b.BatchID != id || b.State != batch.StateReceived || b.FileCount != 1 {
		t.Fatalf("%d %s", g.Code, g.Body.String())
	}
	if l := h.adm("GET", "/v1/batches?state=received", hTenantA, "reader", nil); l.Code != 200 || !strings.Contains(l.Body.String(), id) {
		t.Fatalf("%d %s", l.Code, l.Body.String())
	}
	files := h.adm("GET", "/v1/batches/"+id+"/files", hTenantA, "reader", nil)
	if files.Code != 200 || !strings.Contains(files.Body.String(), "stmt-0001.csv") || strings.Contains(files.Body.String(), "B1-1000") || strings.Contains(files.Body.String(), `"content"`) {
		t.Fatalf("%d %s", files.Code, files.Body.String())
	}
	if ev := h.adm("GET", "/v1/batches/"+id+"/events", hTenantA, "reader", nil); ev.Code != 200 || !strings.Contains(ev.Body.String(), "RECEIVED") {
		t.Fatalf("%d %s", ev.Code, ev.Body.String())
	}
	if r := h.adm("GET", "/v1/batches/"+id+"/rows?state=accepted,rejected&limit=5000", hTenantA, "reader", nil); r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	// another tenant sees nothing: 404, never 403
	for _, path := range []string{"", "/files", "/rows", "/events"} {
		if x := h.adm("GET", "/v1/batches/"+id+path, hTenantB, "reader", nil); x.Code != 404 {
			t.Errorf("tenant B reads %q: %d", path, x.Code)
		}
	}
	if l := h.adm("GET", "/v1/batches", hTenantB, "reader", nil); l.Code != 200 || strings.Contains(l.Body.String(), id) {
		t.Fatalf("%d %s", l.Code, l.Body.String())
	}
	// no principal, a denied principal, an unavailable authorization service
	if x := h.adm("GET", "/v1/batches/"+id, hTenantA, "", nil); x.Code != 401 && x.Code != 403 {
		t.Fatalf("%d", x.Code)
	}
	h.authz.deny("nobody", ActionBatchRead)
	if x := h.adm("GET", "/v1/batches/"+id, hTenantA, "nobody", nil); x.Code != 403 {
		t.Fatalf("%d", x.Code)
	}
	h.authz.mu.Lock()
	h.authz.down = true
	h.authz.mu.Unlock()
	if x := h.adm("GET", "/v1/batches/"+id, hTenantA, "fresh-reader", nil); x.Code != 503 {
		t.Fatalf("%d", x.Code)
	}
}

func TestReplayAdminHTTP_ValidationDecisionsAndAuthorization(t *testing.T) {
	h := newBHarness(t)
	c := h.conn(t)
	w := h.deliver(c, "B-1", hmacSecret, hfile{name: "stmt-0001.csv", data: csvBody("B1", 2)})
	var acc map[string]any
	decodeInto(t, w, &acc)
	id := acc["batch_id"].(string)
	// no scope: refused, nothing is stored (NP-39)
	x := h.adm("POST", "/v1/replays", hTenantA, "requester", map[string]any{"reason_code": batch.ReasonIncidentRepair, "reason": "r"})
	isProblem(t, x, 422)
	// a scope with nothing replayable in it
	x = h.adm("POST", "/v1/replays", hTenantA, "requester", map[string]any{"scope": map[string]any{"batch_id": id, "row_states": []string{"QUARANTINED"}}, "reason_code": batch.ReasonIncidentRepair, "reason": "r"})
	isProblem(t, x, 422)
	// a reason code that does not exist
	x = h.adm("POST", "/v1/replays", hTenantA, "requester", map[string]any{"scope": map[string]any{"batch_id": id, "row_states": []string{"QUARANTINED"}}, "reason_code": "BECAUSE", "reason": "r"})
	isProblem(t, x, 422)
	// another tenant's batch is not a scope
	x = h.adm("POST", "/v1/replays", hTenantB, "requester", map[string]any{"scope": map[string]any{"batch_id": id, "row_states": []string{"QUARANTINED"}}, "reason_code": batch.ReasonIncidentRepair, "reason": "r"})
	isProblem(t, x, 404)
	if l := h.adm("GET", "/v1/replays", hTenantA, "reader", nil); l.Code != 200 || !strings.Contains(l.Body.String(), `"total":0`) {
		t.Fatalf("%d %s", l.Code, l.Body.String())
	}
	// unknown job and unknown command
	isProblem(t, h.adm("GET", "/v1/replays/00000000-0000-4000-8000-000000000000", hTenantA, "reader", nil), 404)
	isProblem(t, h.adm("POST", "/v1/replays/00000000-0000-4000-8000-000000000000:approve", hTenantA, "approver", map[string]any{}), 404)
	isProblem(t, h.adm("POST", "/v1/replays/00000000-0000-4000-8000-000000000000:explode", hTenantA, "approver", map[string]any{}), 404)
	// each action has its own authorization: requesting is not deciding
	h.authz.deny("requester", ActionReplayDecide)
	x = h.adm("POST", "/v1/replays/00000000-0000-4000-8000-000000000000:approve", hTenantA, "requester", map[string]any{})
	if x.Code != 403 {
		t.Fatalf("%d", x.Code)
	}
	h.authz.deny("requester2", ActionReplayRequest)
	x = h.adm("POST", "/v1/replays", hTenantA, "requester2", map[string]any{"scope": map[string]any{"batch_id": id}, "reason_code": batch.ReasonIncidentRepair, "reason": "r"})
	if x.Code != 403 {
		t.Fatalf("%d", x.Code)
	}
	found := map[string]bool{}
	h.authz.mu.Lock()
	for _, a := range h.authz.actions {
		found[a] = true
	}
	h.authz.mu.Unlock()
	for _, a := range []string{ActionReplayRequest, ActionReplayDecide, ActionReplayRead} {
		if !found[a] {
			t.Errorf("action %s was never checked", a)
		}
	}
}
