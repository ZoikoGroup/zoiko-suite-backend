package store_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/data-ingestion-svc/internal/authz"
	"zoiko.io/data-ingestion-svc/internal/handler"
	"zoiko.io/data-ingestion-svc/internal/imports"
)

// fakeAuthz is a stand-in authorization-svc speaking the real wire format. It records every
// question and denies per (principal, action).
type fakeAuthz struct {
	srv    *httptest.Server
	mu     sync.Mutex
	asked  []string
	deny   map[string]bool
	outage bool
}

func newFakeAuthz(t *testing.T) *fakeAuthz {
	f := &fakeAuthz{deny: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q map[string]string
		_ = json.NewDecoder(r.Body).Decode(&q)
		f.mu.Lock()
		f.asked = append(f.asked, q["principal_id"]+"|"+q["legal_entity_id"]+"|"+q["action_type"])
		denied := f.deny[q["principal_id"]+"|"+q["action_type"]] || f.deny["*|"+q["action_type"]]
		outage := f.outage
		f.mu.Unlock()
		if outage {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		out := "GRANTED"
		if denied {
			out = "DENIED"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": out})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAuthz) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.asked) == 0 {
		return ""
	}
	return f.asked[len(f.asked)-1]
}

func (f *fakeAuthz) askedAbout(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.asked {
		if strings.Contains(a, s) {
			return true
		}
	}
	return false
}

type impAPI struct {
	e     *impEnv
	authz *fakeAuthz
	h     http.Handler
}

func newImpAPI(t *testing.T, o ...impOpts) *impAPI {
	e := newImpEnv(t, o...)
	az := newFakeAuthz(t)
	ac := authz.NewClient(az.srv.URL, zap.NewNop())
	ih := handler.NewImportHandler(e.svc, ac, zap.NewNop(), int64(e.cfg.MaxChunkBytes))
	hh := handler.NewHandler(e.st, ac, zap.NewNop())
	return &impAPI{e: e, authz: az, h: handler.NewRouter(hh, func(r chi.Router) { handler.RegisterImportRoutes(r, ih) })}
}

type apiResp struct {
	Code   int
	Header http.Header
	Raw    string
	JSON   map[string]any
}

func (a *impAPI) do(method, path, tenant, principal string, body any, extra ...string) apiResp {
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/v1/data-ingestion"+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenant)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	req.Header.Set("X-Legal-Entity-Id", entityOne)
	req.Header.Set("X-Request-Id", "req-"+uuid.NewString()[:8])
	req.Header.Set("X-Correlation-ID", "corr-http")
	req.Header.Set("X-Source-Channel", "api")
	req.Header.Set("X-Purpose-Context", "bulk-import")
	req.Header.Set("X-Timezone", "UTC")
	req.Header.Set("X-Jurisdiction-Context", "GB")
	req.Header.Set("Idempotency-Key", "k-"+uuid.NewString())
	for i := 0; i+1 < len(extra); i += 2 {
		if extra[i+1] == "" {
			req.Header.Del(extra[i])
		} else {
			req.Header.Set(extra[i], extra[i+1])
		}
	}
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, req)
	out := apiResp{Code: w.Code, Header: w.Header(), Raw: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &out.JSON)
	return out
}

func (a *impAPI) tenant(method, path, principal string, body any, extra ...string) apiResp {
	return a.do(method, path, a.e.tenant, principal, body, extra...)
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func (a *impAPI) profile(spec imports.Spec) string {
	a.e.t.Helper()
	key := "http-" + strings.ToLower(uuid.NewString()[:8])
	r := a.do("POST", "/import-profiles", imports.PlatformScopeID, platAuth, map[string]any{"profile_key": key, "spec": spec})
	if r.Code != 201 {
		a.e.t.Fatalf("publish: %d %s", r.Code, r.Raw)
	}
	c := a.do("POST", "/import-profiles/"+key+"/versions/1:certify", imports.PlatformScopeID, platCert, nil)
	if c.Code != 200 {
		a.e.t.Fatalf("certify: %d %s", c.Code, c.Raw)
	}
	return key
}

func createBodyFor(key, content string, rows int, control string) map[string]any {
	b := map[string]any{"legal_entity_id": entityOne, "profile_key": key, "source_system": "erp-x", "file_name": "i.csv",
		"content_type": "text/csv", "sha256": sha([]byte(content))}
	if rows >= 0 {
		b["declared_rows"] = rows
	}
	if control != "" {
		b["declared_control_total"] = control
	}
	return b
}

func (a *impAPI) put(id string, idx int, data []byte, declaredSHA string) apiResp {
	return a.tenant("PUT", fmt.Sprintf("/imports/%s/chunks/%d", id, idx), uploader, data, "Content-Type", "application/octet-stream", "X-Chunk-SHA256", declaredSHA)
}

func TestImportAPI_FullLifecycleOverHTTP(t *testing.T) {
	a := newImpAPI(t)
	key := a.profile(materialSpec())
	content := csvOf(goodRows(12, entityOne)...)

	c := a.tenant("POST", "/imports", uploader, createBodyFor(key, content, 12, "120"), "Idempotency-Key", "create-1")
	if c.Code != 201 || str(c.JSON, "status") != "CREATED" || !strings.HasPrefix(c.Header.Get("Location"), "/v1/data-ingestion/imports/imb_") {
		t.Fatalf("create: %d %s", c.Code, c.Raw)
	}
	if got := a.authz.last(); got != uploader+"|"+entityOne+"|"+handler.ActionImportCreate {
		t.Fatalf("create was authorized as %q", got)
	}
	id := str(c.JSON, "batch_id")
	// a retried create answers with the batch it already made
	rp := a.tenant("POST", "/imports", uploader, createBodyFor(key, content, 12, "120"), "Idempotency-Key", "create-1")
	if rp.Code != 200 || str(rp.JSON, "batch_id") != id || rp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %s", rp.Code, rp.Raw)
	}
	// the same key for a different request is refused
	re := a.tenant("POST", "/imports", uploader, createBodyFor(key, content+"x", 12, "120"), "Idempotency-Key", "create-1")
	if re.Code != 409 || str(re.JSON, "code") != "INT-IDEM-005" {
		t.Fatalf("key reuse: %d %s", re.Code, re.Raw)
	}

	// chunks
	raw := []byte(content)
	parts := [][]byte{raw[:100], raw[100:200], raw[200:]}
	for i, p := range parts {
		r := a.put(id, i, p, sha(p))
		if r.Code != 201 || r.JSON["stored"] != true {
			t.Fatalf("chunk %d: %d %s", i, r.Code, r.Raw)
		}
	}
	if r := a.put(id, 1, parts[1], sha(parts[1])); r.Code != 200 || r.JSON["stored"] != false {
		t.Fatalf("a resumed chunk: %d %s", r.Code, r.Raw)
	}
	st := a.tenant("GET", "/imports/"+id+"/upload", uploader, nil)
	if st.Code != 200 || len(st.JSON["chunks"].([]any)) != 3 || str(st.JSON, "status") != "UPLOADING" {
		t.Fatalf("upload state: %d %s", st.Code, st.Raw)
	}
	sl := a.tenant("POST", "/imports/"+id+":seal", uploader, map[string]any{"chunk_count": 3})
	if sl.Code != 200 || str(sl.JSON, "status") != "UPLOADED" || str(sl.JSON, "content_sha256") != sha(raw) {
		t.Fatalf("seal: %d %s", sl.Code, sl.Raw)
	}
	if v := a.tenant("POST", "/imports/"+id+":validate", uploader, nil); v.Code != 202 {
		t.Fatalf("validate: %d %s", v.Code, v.Raw)
	}
	if n, failed := a.e.val.RunOnce(ictx); n == 0 || failed {
		t.Fatalf("worker %d %v", n, failed)
	}
	pv := a.tenant("POST", "/imports/"+id+":preview", uploader, nil)
	fp := str(pv.JSON, "preview_fingerprint")
	if pv.Code != 200 || fp == "" || pv.JSON["preview"].(map[string]any)["will_be_sent"].(float64) != 12 {
		t.Fatalf("preview: %d %s", pv.Code, pv.Raw)
	}
	if g := a.tenant("GET", "/imports/"+id+"/preview", uploader, nil); g.Code != 200 || str(g.JSON, "preview_fingerprint") != fp {
		t.Fatalf("get preview: %d %s", g.Code, g.Raw)
	}
	if ct := a.tenant("GET", "/imports/"+id+"/control-totals", uploader, nil); ct.Code != 200 || str(ct.JSON, "declared") != "120" || str(ct.JSON, "computed") != "120" {
		t.Fatalf("control totals: %d %s", ct.Code, ct.Raw)
	}

	// approval: not the creator, and only for what was previewed
	self := a.tenant("POST", "/imports/"+id+":approve", uploader, map[string]any{"preview_fingerprint": fp, "reason": "me"})
	if self.Code != 403 || str(self.JSON, "code") != "INT-AUTH-003" || self.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("self-approval: %d %s", self.Code, self.Raw)
	}
	wrong := a.tenant("POST", "/imports/"+id+":approve", approver, map[string]any{"preview_fingerprint": strings.Repeat("0", 64)})
	if wrong.Code != 409 || str(wrong.JSON, "code") != "INT-FILE-014" {
		t.Fatalf("wrong fingerprint: %d %s", wrong.Code, wrong.Raw)
	}
	ap := a.tenant("POST", "/imports/"+id+":approve", approver, map[string]any{"preview_fingerprint": fp, "reason": "agreed with the ledger"})
	if ap.Code != 200 || str(ap.JSON, "status") != "APPROVED" || str(ap.JSON, "approved_by") != approver {
		t.Fatalf("approve: %d %s", ap.Code, ap.Raw)
	}
	if got := a.authz.last(); got != approver+"|"+entityOne+"|"+handler.ActionImportApprove {
		t.Fatalf("approve was authorized as %q", got)
	}
	ex := a.tenant("POST", "/imports/"+id+":execute", executor, nil)
	if ex.Code != 202 || str(ex.JSON, "status") != "IMPORTING" {
		t.Fatalf("execute: %d %s", ex.Code, ex.Raw)
	}
	done := a.e.drain(&imports.Batch{BatchID: id})
	if done.Status != imports.StatusCompleted {
		t.Fatalf("%s", done.Status)
	}
	items := a.tenant("GET", "/imports/"+id+"/items?status=IMPORTED&limit=5", uploader, nil)
	list := items.JSON["items"].([]any)
	if items.Code != 200 || len(list) != 5 || list[0].(map[string]any)["result_ref"] == "" || items.JSON["next_after"].(float64) != 5 {
		t.Fatalf("items: %d %s", items.Code, items.Raw)
	}
	next := a.tenant("GET", "/imports/"+id+"/items?status=IMPORTED&limit=100&after=5", uploader, nil)
	if len(next.JSON["items"].([]any)) != 7 {
		t.Fatalf("pagination: %s", next.Raw)
	}
	rc := a.tenant("POST", "/imports/"+id+":reconcile", approver, nil)
	if rc.Code != 200 || str(rc.JSON["batch"].(map[string]any), "status") != "RECONCILED" || rc.JSON["reconciliation"].(map[string]any)["matched"] != true {
		t.Fatalf("reconcile: %d %s", rc.Code, rc.Raw)
	}
	if g := a.tenant("GET", "/imports/"+id+"/reconciliation", uploader, nil); g.Code != 200 || g.JSON["matched"] != true {
		t.Fatalf("get reconciliation: %d %s", g.Code, g.Raw)
	}
	if l := a.tenant("GET", "/imports/"+id+"/log", uploader, nil); l.Code != 200 || len(l.JSON["log"].([]any)) < 10 {
		t.Fatalf("log: %d %s", l.Code, l.Raw)
	}
	if l := a.tenant("GET", "/imports", uploader, nil); l.Code != 200 || len(l.JSON["imports"].([]any)) == 0 {
		t.Fatalf("list: %d %s", l.Code, l.Raw)
	}
}

func TestImportAPI_AuthorizationAndIdentity(t *testing.T) {
	a := newImpAPI(t)
	key := a.profile(plainSpec())
	content := csvOf(goodRows(3, entityOne)...)
	body := createBodyFor(key, content, -1, "")

	if r := a.tenant("POST", "/imports", "", body); r.Code != 401 {
		t.Errorf("no principal: %d %s", r.Code, r.Raw)
	}
	if r := a.do("POST", "/imports", "", uploader, body); r.Code != 401 {
		t.Errorf("no tenant: %d", r.Code)
	}
	a.authz.deny["*|"+handler.ActionImportCreate] = true
	if r := a.tenant("POST", "/imports", uploader, body); r.Code != 403 || str(r.JSON, "code") != "INT-AUTH-003" {
		t.Errorf("denied: %d %s", r.Code, r.Raw)
	}
	if bs, _ := a.e.svc.List(ictx, a.e.tenant, "", 10); len(bs) != 0 {
		t.Error("a denied create must create nothing")
	}
	delete(a.authz.deny, "*|"+handler.ActionImportCreate)
	a.authz.outage = true
	if r := a.tenant("POST", "/imports", uploader, body); r.Code != 403 {
		t.Errorf("an authorization outage must fail closed: %d %s", r.Code, r.Raw)
	}
	a.authz.outage = false

	b := a.e.approved(a.e.profile(plainSpec()), content, declared{})
	for name, c := range map[string]struct{ method, path, action string }{
		"read":      {"GET", "", handler.ActionImportRead},
		"approve":   {"POST", ":approve", handler.ActionImportApprove},
		"execute":   {"POST", ":execute", handler.ActionImportExecute},
		"cancel":    {"POST", ":cancel", handler.ActionImportCancel},
		"reconcile": {"POST", ":reconcile", handler.ActionImportReconcile},
		"validate":  {"POST", ":validate", handler.ActionImportValidate},
		"upload":    {"POST", ":seal", handler.ActionImportUpload},
		"log":       {"GET", "/log", handler.ActionImportRead},
		"items":     {"GET", "/items", handler.ActionImportRead},
	} {
		a.authz.deny["*|"+c.action] = true
		r := a.tenant(c.method, "/imports/"+b.BatchID+c.path, approver, map[string]any{"reason": "x", "preview_fingerprint": b.PreviewFingerprint, "chunk_count": 1})
		if r.Code != 403 {
			t.Errorf("%s without %s: %d %s", name, c.action, r.Code, r.Raw)
		}
		delete(a.authz.deny, "*|"+c.action)
	}
	if got := a.e.batch(b.BatchID); got.Status != imports.StatusApproved {
		t.Fatalf("denied commands must change nothing: %s", got.Status)
	}
	// profiles: published from the platform scope, by someone entitled to
	spec := plainSpec()
	a.authz.deny["*|"+handler.ActionImportProfilePublish] = true
	if r := a.do("POST", "/import-profiles", imports.PlatformScopeID, platAuth, map[string]any{"profile_key": "denied-prof", "spec": spec}); r.Code != 403 {
		t.Errorf("profile publish without the action: %d", r.Code)
	}
	delete(a.authz.deny, "*|"+handler.ActionImportProfilePublish)
	if r := a.tenant("POST", "/import-profiles", platAuth, map[string]any{"profile_key": "tenant-prof", "spec": spec}); r.Code != 403 {
		t.Errorf("a tenant publishing a platform profile: %d %s", r.Code, r.Raw)
	}
	if !a.authz.askedAbout(imports.PlatformScopeID) {
		t.Error("profile commands must be authorized against the platform scope")
	}
}

func TestImportAPI_ProblemsAndCodes(t *testing.T) {
	a := newImpAPI(t)
	key := a.profile(plainSpec())
	content := csvOf(goodRows(4, entityOne)...)

	for name, c := range map[string]struct {
		body any
		code int
	}{
		"not json":      {`{`, 400},
		"unknown field": {map[string]any{"legal_entity_id": entityOne, "surprise": 1}, 400},
		"no hash":       {map[string]any{"legal_entity_id": entityOne, "profile_key": key, "source_system": "x", "content_type": "text/csv"}, 400},
		"bad hash":      {map[string]any{"legal_entity_id": entityOne, "profile_key": key, "source_system": "x", "content_type": "text/csv", "sha256": "xyz"}, 400},
		"unknown prof":  {map[string]any{"legal_entity_id": entityOne, "profile_key": "no-such-profile", "source_system": "x", "content_type": "text/csv", "sha256": sha([]byte("x"))}, 404},
	} {
		r := a.tenant("POST", "/imports", uploader, c.body)
		if r.Code != c.code {
			t.Errorf("%s: %d %s", name, r.Code, r.Raw)
		}
	}
	if r := a.tenant("POST", "/imports", uploader, createBodyFor(key, content, -1, ""), "Idempotency-Key", ""); r.Code != 400 {
		t.Errorf("a create with no idempotency key: %d %s", r.Code, r.Raw)
	}
	c := a.tenant("POST", "/imports", uploader, createBodyFor(key, content, -1, ""))
	id := str(c.JSON, "batch_id")
	if dup := a.tenant("POST", "/imports", uploader, createBodyFor(key, content, -1, "")); dup.Code != 409 || str(dup.JSON, "code") != "INT-IDEM-005" {
		t.Errorf("duplicate file: %d %s", dup.Code, dup.Raw)
	}
	if r := a.put(id, 0, []byte("abc"), sha([]byte("different"))); r.Code != 409 || str(r.JSON, "code") != "INT-FILE-014" {
		t.Errorf("chunk hash: %d %s", r.Code, r.Raw)
	}
	if r := a.put(id, 0, []byte("abc"), ""); r.Code != 409 {
		t.Errorf("chunk with no hash header: %d %s", r.Code, r.Raw)
	}
	if r := a.tenant("PUT", "/imports/"+id+"/chunks/zero", uploader, []byte("abc"), "X-Chunk-SHA256", sha([]byte("abc"))); r.Code != 400 {
		t.Errorf("chunk index: %d", r.Code)
	}
	if r := a.put(id, 0, bytes.Repeat([]byte("a"), a.e.cfg.MaxChunkBytes+10), "x"); r.Code != 413 || str(r.JSON, "code") != "INT-FILE-014" {
		t.Errorf("oversize chunk: %d %s", r.Code, r.Raw)
	}
	// seal with a content hash that is not the declared one: the batch is quarantined and says so
	raw := []byte("invoice_no,entity,amount\nINV-9,le-1,1\n")
	a.put(id, 0, raw, sha(raw))
	sl := a.tenant("POST", "/imports/"+id+":seal", uploader, map[string]any{"chunk_count": 1})
	if sl.Code != 409 || str(sl.JSON, "code") != "INT-FILE-014" || str(sl.JSON["batch"].(map[string]any), "status") != "QUARANTINED" {
		t.Errorf("hash mismatch at seal: %d %s", sl.Code, sl.Raw)
	}
	if r := a.tenant("GET", "/imports/imb_00000000-0000-0000-0000-000000000000", uploader, nil); r.Code != 404 {
		t.Errorf("unknown batch: %d", r.Code)
	}
	if r := a.tenant("GET", "/imports/not-an-id", uploader, nil); r.Code != 404 {
		t.Errorf("malformed id: %d", r.Code)
	}
	if r := a.tenant("POST", "/imports/"+id+":cancel", uploader, map[string]any{"reason": "x"}); r.Code != 409 || str(r.JSON, "code") != "INT-SCOPE-013" {
		t.Errorf("cancelling a quarantined batch: %d %s", r.Code, r.Raw)
	}
}

func TestImportAPI_NoScannerIsA503NotACleanVerdict(t *testing.T) {
	a := newImpAPI(t, impOpts{noScan: true})
	key := a.profile(plainSpec())
	r := a.tenant("POST", "/imports", uploader, createBodyFor(key, csvOf(goodRows(1, entityOne)...), -1, ""))
	if r.Code != 503 || r.Header.Get("Retry-After") == "" || str(r.JSON, "code") != "INT-QUAR-008" {
		t.Fatalf("%d %s", r.Code, r.Raw)
	}
}

func TestImportAPI_ValidationErrorsNameColumnsNeverValues(t *testing.T) {
	a := newImpAPI(t)
	key := a.profile(plainSpec())
	secret := "SECRET-9999-XYZ"
	content := csvOf("INV-1,le-1,10", secret+",le-1,10", "INV-3,le-OTHER,10")
	b, err := a.e.create(a.e.profileFor(key), content, declared{})
	iMust(t, err)
	n := a.e.upload(b, content, 60, false)
	_, err = a.e.svc.Seal(ictx, a.e.actor(uploader), b.BatchID, n)
	iMust(t, err)
	_, err = a.e.svc.RequestValidation(ictx, a.e.actor(uploader), b.BatchID)
	iMust(t, err)
	_, err = a.e.svc.RunPipeline(ictx, a.e.tenant, b.BatchID)
	iMust(t, err)
	r := a.tenant("GET", "/imports/"+b.BatchID+"/validation-errors", uploader, nil)
	if r.Code != 200 || len(r.JSON["errors"].([]any)) != 2 {
		t.Fatalf("%d %s", r.Code, r.Raw)
	}
	if strings.Contains(r.Raw, secret) || strings.Contains(r.Raw, "le-OTHER") {
		t.Fatalf("a validation error echoed row data: %s", r.Raw)
	}
	for _, e := range r.JSON["errors"].([]any) {
		em := e.(map[string]any)
		if em["values"] != nil {
			t.Fatalf("an invalid row's values were returned: %v", em)
		}
	}
	pv := a.tenant("POST", "/imports/"+b.BatchID+":preview", uploader, nil)
	if pv.Code != 200 || strings.Contains(pv.Raw, secret) {
		t.Fatalf("the preview must show error codes, not invalid row data: %d %s", pv.Code, pv.Raw)
	}
	if pv.JSON["preview"].(map[string]any)["error_counts"].(map[string]any)["PATTERN"].(float64) != 1 {
		t.Fatalf("%s", pv.Raw)
	}
}

func TestImportAPI_OtherTenantsSeeNothing(t *testing.T) {
	a := newImpAPI(t)
	b := a.e.approved(a.e.profile(plainSpec()), csvOf(goodRows(2, entityOne)...), declared{})
	other := "88888888-8888-8888-8888-888888888888"
	for _, c := range []struct{ method, path string }{
		{"GET", "/imports/" + b.BatchID}, {"GET", "/imports/" + b.BatchID + "/items"}, {"GET", "/imports/" + b.BatchID + "/log"},
		{"GET", "/imports/" + b.BatchID + "/upload"}, {"POST", "/imports/" + b.BatchID + ":execute"}, {"POST", "/imports/" + b.BatchID + ":cancel"},
		{"GET", "/imports/" + b.BatchID + "/control-totals"}, {"POST", "/imports/" + b.BatchID + "/items/1:retry"},
	} {
		r := a.do(c.method, c.path, other, approver, map[string]any{"reason": "x"})
		if r.Code != 404 {
			t.Errorf("%s %s as another tenant: %d %s", c.method, c.path, r.Code, r.Raw)
		}
	}
	if l := a.do("GET", "/imports", other, approver, nil); l.Code != 200 || len(l.JSON["imports"].([]any)) != 0 {
		t.Errorf("list leaked: %s", l.Raw)
	}
	if got := a.e.batch(b.BatchID); got.Status != imports.StatusApproved {
		t.Fatal("another tenant changed the batch")
	}
}

func TestImportAPI_ItemQuarantineAndRetryOverHTTP(t *testing.T) {
	a := newImpAPI(t)
	b := a.e.executing(a.e.profile(plainSpec()), csvOf(goodRows(3, entityOne)...), declared{})
	q := a.tenant("POST", fmt.Sprintf("/imports/%s/items/2:quarantine", b.BatchID), approver, map[string]any{"reason": "disputed", "owner": "ap-team"})
	if q.Code != 200 {
		t.Fatalf("quarantine: %d %s", q.Code, q.Raw)
	}
	if r := a.tenant("POST", fmt.Sprintf("/imports/%s/items/3:quarantine", b.BatchID), approver, map[string]any{"reason": "x"}); r.Code != 400 {
		t.Errorf("a quarantine with no owner: %d %s", r.Code, r.Raw)
	}
	if r := a.tenant("POST", fmt.Sprintf("/imports/%s/items/99:quarantine", b.BatchID), approver, map[string]any{"reason": "x", "owner": "o"}); r.Code != 404 {
		t.Errorf("an unknown item: %d", r.Code)
	}
	done := a.e.drain(b)
	if done.Status != imports.StatusCompletedWithExceptions {
		t.Fatal(done.Status)
	}
	rc := a.tenant("POST", "/imports/"+b.BatchID+":reconcile", approver, nil)
	if rc.Code != 409 || str(rc.JSON, "code") != "INT-RECON-011" || str(rc.JSON["batch"].(map[string]any), "status") != "COMPLETED_WITH_EXCEPTIONS" {
		t.Fatalf("reconcile with an exception: %d %s", rc.Code, rc.Raw)
	}
	rt := a.tenant("POST", fmt.Sprintf("/imports/%s/items/2:retry", b.BatchID), approver, map[string]any{"reason": "dispute resolved"})
	if rt.Code != 202 {
		t.Fatalf("retry: %d %s", rt.Code, rt.Raw)
	}
	if after := a.e.drain(a.e.batch(b.BatchID)); after.Status != imports.StatusCompleted {
		t.Fatalf("once every item is imported the batch is COMPLETED: %s", after.Status)
	}
	if r := a.tenant("POST", "/imports/"+b.BatchID+":reconcile", approver, nil); r.Code != 200 {
		t.Fatalf("reconcile after the retry: %d %s", r.Code, r.Raw)
	}
	_ = time.Second
}
