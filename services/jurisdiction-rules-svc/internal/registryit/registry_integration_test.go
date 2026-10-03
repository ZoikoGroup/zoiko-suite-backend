//go:build integration

// End-to-end tests for the ZS-JUR-001 Wave 0 registries: real Postgres
// (embedded), the real store, the real HTTP handlers. They prove the
// document's invariants hold at the database as well as in the Go code, by
// attempting the forbidden writes directly in SQL.
//
// Run: go test -v -tags=integration -count=1 -timeout=240s ./internal/registryit/
package registryit_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/envelope"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

var (
	pool *pgxpool.Pool
	st   *store.PgStore
)

func migrations(t interface{ Fatalf(string, ...any) }, suffix string) []string {
	files, err := filepath.Glob("../../deployments/migrations/*" + suffix)
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(files)
	return files
}

func applyAll(ctx context.Context, files []string) error {
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func TestMain(m *testing.M) {
	port := uint32(17101 + uint32(os.Getpid()%499))
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).Port(port).Database("jur_test").Username("postgres").Password("postgres"))
	if err := pg.Start(); err != nil {
		fmt.Printf("embedded postgres: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()
	var err error
	pool, err = pgxpool.New(ctx, fmt.Sprintf("host=localhost port=%d dbname=jur_test user=postgres password=postgres sslmode=disable", port))
	if err == nil {
		for i := 0; i < 75; i++ {
			if err = pool.Ping(ctx); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if err == nil {
		files, _ := filepath.Glob("../../deployments/migrations/*.up.sql")
		sort.Strings(files)
		err = applyAll(ctx, files)
	}
	if err != nil {
		fmt.Printf("setup: %v\n", err)
		_ = pg.Stop()
		os.Exit(1)
	}
	log := zap.NewNop()
	if os.Getenv("JUR_TEST_LOG") != "" { // set to see the store's own error logs while debugging
		log, _ = zap.NewDevelopment()
	}
	st = store.New(pool, log)
	code := m.Run()
	pool.Close()
	_ = pg.Stop()
	os.Exit(code)
}

// ── harness ─────────────────────────────────────────────────────────────────

type spyPub struct {
	events.Publisher
	mu   sync.Mutex
	seen []string
}

func (p *spyPub) PublishRegistryEvent(_ context.Context, t, _, _, _ string, _ map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, t)
	return nil
}
func (p *spyPub) count(t string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.seen {
		if s == t {
			n++
		}
	}
	return n
}

// recordingAuthz permits everything and remembers resource/action pairs.
type recordingAuthz struct {
	mu    sync.Mutex
	pairs map[string]bool
	deny  string // resource.action to refuse
}

func (a *recordingAuthz) Authorize(_ context.Context, _, _, resource, action string, _ *envelope.Envelope) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pairs == nil {
		a.pairs = map[string]bool{}
	}
	a.pairs[resource+"."+action] = true
	if a.deny == resource+"."+action {
		return authz.ErrUnauthorized
	}
	return nil
}

type env struct {
	srv  http.Handler
	pub  *spyPub
	auth *recordingAuthz
}

func newEnv() *env { return newEnvWithSigner(nil) }

func newEnvWithSigner(signer domain.Signer) *env {
	pub := &spyPub{}
	auth := &recordingAuthz{}
	r := chi.NewRouter()
	h := handler.New(st, auth, pub, "platform-scope", zap.NewNop()).WithRegistry(st, pub)
	if signer != nil {
		h.WithSigner(signer)
	}
	handler.RegisterRoutes(r, h)
	return &env{srv: r, pub: pub, auth: auth}
}

func (e *env) do(method, path, principal string, body any) (int, map[string]any) {
	return e.doWithHeaders(method, path, principal, body, nil)
}

func (e *env) doWithHeaders(method, path, principal string, body any, headers map[string]string) (int, map[string]any) {
	var buf bytes.Buffer
	switch b := body.(type) {
	case nil:
	case []byte:
		buf.Write(b)
	default:
		_ = json.NewEncoder(&buf).Encode(b)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	e.srv.ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

var ctx = context.Background()

func seedJurisdiction(t *testing.T, code string) *domain.Jurisdiction {
	t.Helper()
	j, _, err := st.CreateJurisdiction(ctx, domain.CreateJurisdictionParams{
		JurisdictionCode: code + "-" + uuid.NewString()[:6], JurisdictionName: "Jurisdiction " + code, JurisdictionType: "COUNTRY",
		AuthorityType: "FEDERAL", EffectiveFrom: time.Now().UTC().Add(-48 * time.Hour), ActiveFlag: true, CreatedByPrincipalID: "seed"})
	require.NoError(t, err)
	return j
}

func seedRule(t *testing.T, jurID, code string) *domain.JurisdictionRule {
	t.Helper()
	r, _, err := st.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionID: jurID, RuleDomain: "TAX", RuleCode: code, RuleName: code,
		EffectiveFrom: time.Now().UTC().Add(-24 * time.Hour), RulePayload: []byte(`{}`), RuleStatus: "DRAFT", CreatedByPrincipalID: "seed"})
	require.NoError(t, err)
	return r
}

func newSnapshot() string {
	sum := sha256.Sum256([]byte(uuid.NewString()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sourceBody(jurID string) map[string]any {
	return map[string]any{
		"jurisdiction_id": jurID, "authority": "HMRC", "source_type": "STATUTE", "authority_level": "BINDING_LAW",
		"title": "VAT Act 1994 s.2", "location": "https://www.legislation.gov.uk/ukpga/1994/23", "snapshot_hash": newSnapshot(),
		"published_on": "1994-07-05", "effective_on": "1994-07-05"}
}

func mustStatus(t *testing.T, want, got int, body map[string]any) {
	t.Helper()
	require.Equal(t, want, got, "body: %v", body)
}

// ── source, interpretation, segregation ─────────────────────────────────────

func TestSourceToApprovedInterpretation_EnforcesIndependentReview(t *testing.T) {
	e := newEnv()
	j := seedJurisdiction(t, "GB")

	code, src := e.do("POST", "/v1/admin/sources", "author", sourceBody(j.JurisdictionID))
	mustStatus(t, 201, code, src)
	sid := src["source_id"].(string)

	// Replaying the identical capture is idempotent and emits once.
	body := sourceBody(j.JurisdictionID)
	body["snapshot_hash"] = src["snapshot_hash"]
	body["title"] = src["title"]
	body["location"] = src["location"]
	code, again := e.do("POST", "/v1/admin/sources", "author", body)
	mustStatus(t, 200, code, again)
	assert.Equal(t, sid, again["source_id"])
	body["title"] = "A different title for the same captured bytes"
	code, conflict := e.do("POST", "/v1/admin/sources", "author", body)
	mustStatus(t, 409, code, conflict)
	assert.Equal(t, 1, e.pub.count(events.EventSourceCaptured), "one capture event despite the replay")

	code, interp := e.do("POST", "/v1/admin/interpretations", "author", map[string]any{
		"jurisdiction_id": j.JurisdictionID, "subject": "Standard rate applies to digital services",
		"decision": "Apply 20% to B2C digital services", "rationale": "s.2 read with Sch.4", "source_ids": []string{sid}})
	mustStatus(t, 201, code, interp)
	iid := interp["interpretation_id"].(string)
	assert.Equal(t, "PENDING", interp["status"])

	// The author cannot approve; an unreviewed source blocks even an independent approver.
	code, r := e.do("POST", "/v1/admin/interpretations/"+iid+"/approve", "author", nil)
	mustStatus(t, 403, code, r)
	assert.Equal(t, "segregation_of_duties", r["error"])
	code, r = e.do("POST", "/v1/admin/interpretations/"+iid+"/approve", "approver", nil)
	mustStatus(t, 409, code, r)
	assert.Equal(t, "source_not_ready", r["error"])

	// The author cannot review their own source; an independent reviewer can, once.
	code, r = e.do("POST", "/v1/admin/sources/"+sid+"/review", "author", nil)
	mustStatus(t, 403, code, r)
	code, r = e.do("POST", "/v1/admin/sources/"+sid+"/review", "reviewer", nil)
	mustStatus(t, 200, code, r)
	assert.Equal(t, "reviewer", r["reviewed_by_principal_id"])
	code, r = e.do("POST", "/v1/admin/sources/"+sid+"/review", "someone-else", nil)
	mustStatus(t, 409, code, r)

	code, r = e.do("POST", "/v1/admin/interpretations/"+iid+"/approve", "approver", nil)
	mustStatus(t, 200, code, r)
	assert.Equal(t, "APPROVED", r["status"])
	assert.Equal(t, "approver", r["approved_by_principal_id"])

	// Distinct, separately grantable actions were used (s28).
	for _, p := range []string{"regulatory_source.create", "regulatory_source.review", "interpretation_record.create", "interpretation_record.approve"} {
		assert.True(t, e.auth.pairs[p], p)
	}
}

func TestEvidenceIsImmutableAtTheDatabase(t *testing.T) {
	e := newEnv()
	j := seedJurisdiction(t, "DE")
	_, src := e.do("POST", "/v1/admin/sources", "author", sourceBody(j.JurisdictionID))
	sid := src["source_id"].(string)

	_, err := pool.Exec(ctx, `UPDATE regulatory_sources SET title='tampered' WHERE source_id=$1`, sid)
	require.Error(t, err, "a captured source's content cannot be edited")
	_, err = pool.Exec(ctx, `DELETE FROM regulatory_sources WHERE source_id=$1`, sid)
	require.Error(t, err, "evidence is never deleted")
	_, err = pool.Exec(ctx, `UPDATE regulatory_sources SET reviewed_by_principal_id='author', reviewed_at=NOW() WHERE source_id=$1`, sid)
	require.Error(t, err, "the reviewer cannot be the author, even by direct SQL")

	// An approved interpretation is frozen.
	_, _ = e.do("POST", "/v1/admin/sources/"+sid+"/review", "reviewer", nil)
	_, interp := e.do("POST", "/v1/admin/interpretations", "author", map[string]any{
		"jurisdiction_id": j.JurisdictionID, "subject": "s", "decision": "d", "rationale": "r", "source_ids": []string{sid}})
	iid := interp["interpretation_id"].(string)
	code, r := e.do("POST", "/v1/admin/interpretations/"+iid+"/approve", "approver", nil)
	mustStatus(t, 200, code, r)
	_, err = pool.Exec(ctx, `UPDATE interpretation_records SET decision='changed' WHERE interpretation_id=$1`, iid)
	require.Error(t, err, "an approved interpretation is immutable")
}

func TestSupersedingASourcePreservesIt(t *testing.T) {
	e := newEnv()
	j := seedJurisdiction(t, "FR")
	_, old := e.do("POST", "/v1/admin/sources", "author", sourceBody(j.JurisdictionID))
	_, repl := e.do("POST", "/v1/admin/sources", "author", sourceBody(j.JurisdictionID))
	oid, rid := old["source_id"].(string), repl["source_id"].(string)

	code, r := e.do("POST", "/v1/admin/sources/"+oid+"/supersede", "author", map[string]any{"replacement_source_id": rid})
	mustStatus(t, 200, code, r)
	assert.Equal(t, rid, r["superseded_by_source_id"])
	code, r = e.do("GET", "/v1/sources/"+oid, "", nil)
	mustStatus(t, 200, code, r) // still readable: JUR-NEG-06 reproducibility
	code, r = e.do("POST", "/v1/admin/sources/"+oid+"/supersede", "author", map[string]any{"replacement_source_id": oid})
	assert.Equal(t, 409, code, "already superseded: %v", r)
	assert.Equal(t, 1, e.pub.count(events.EventSourceSuperseded))

	// A superseded source cannot back a new interpretation.
	code, r = e.do("POST", "/v1/admin/interpretations", "author", map[string]any{
		"jurisdiction_id": j.JurisdictionID, "subject": "s", "decision": "d", "rationale": "r", "source_ids": []string{oid}})
	mustStatus(t, 400, code, r)
}

// ── rule provenance ─────────────────────────────────────────────────────────

func TestRuleProvenance_NeedsApprovedInterpretation_AndFreezesWhenActive(t *testing.T) {
	e := newEnv()
	j := seedJurisdiction(t, "IN")
	rule := seedRule(t, j.JurisdictionID, "GST-STD")

	_, regime := e.do("POST", "/v1/admin/regimes", "author", map[string]any{"regime_code": "GST-" + uuid.NewString()[:5], "regime_name": "Goods and services tax"})
	rid := regime["regime_id"].(string)
	_, src := e.do("POST", "/v1/admin/sources", "author", sourceBody(j.JurisdictionID))
	sid := src["source_id"].(string)
	_, interp := e.do("POST", "/v1/admin/interpretations", "author", map[string]any{
		"jurisdiction_id": j.JurisdictionID, "regime_id": rid, "subject": "s", "decision": "d", "rationale": "r", "source_ids": []string{sid}})
	iid := interp["interpretation_id"].(string)

	path := "/v1/admin/rules/" + rule.JurisdictionRuleID + "/provenance"
	prec := 10
	req := map[string]any{"regime_id": rid, "interpretation_id": iid, "precedence": prec, "published_on": "2026-04-01", "source_ids": []string{sid}}

	code, r := e.do("PUT", path, "author", req)
	mustStatus(t, 409, code, r)
	assert.Equal(t, "interpretation_not_approved", r["error"], "JUR-NEG-18: unapproved interpretation cannot back a rule")

	_, _ = e.do("POST", "/v1/admin/sources/"+sid+"/review", "reviewer", nil)
	code, r = e.do("POST", "/v1/admin/interpretations/"+iid+"/approve", "approver", nil)
	mustStatus(t, 200, code, r)

	code, r = e.do("PUT", path, "author", req)
	mustStatus(t, 200, code, r)
	assert.Equal(t, iid, r["interpretation_id"])
	assert.Equal(t, []any{sid}, r["source_ids"])
	code, r = e.do("GET", "/v1/rules/"+rule.JurisdictionRuleID+"/provenance", "", nil)
	mustStatus(t, 200, code, r)
	assert.Equal(t, "2026-04-01", r["published_on"])

	// An unknown regime is a client error, never a store outage.
	bad := map[string]any{"regime_id": uuid.NewString()}
	code, r = e.do("PUT", path, "author", bad)
	mustStatus(t, 400, code, r)

	// Once the rule leaves DRAFT, provenance is frozen in the API and in SQL.
	_, err := pool.Exec(ctx, `UPDATE jurisdiction_rules SET rule_status='ACTIVE' WHERE jurisdiction_rule_id=$1`, rule.JurisdictionRuleID)
	require.NoError(t, err)
	code, r = e.do("PUT", path, "author", req)
	mustStatus(t, 409, code, r)
	assert.Equal(t, "not_draft", r["error"])
	_, err = pool.Exec(ctx, `UPDATE jurisdiction_rules SET precedence=99 WHERE jurisdiction_rule_id=$1`, rule.JurisdictionRuleID)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM rule_sources WHERE jurisdiction_rule_id=$1`, rule.JurisdictionRuleID)
	require.Error(t, err)
}

func TestRuleProvenance_SupersedesMustBeSameRuleCode(t *testing.T) {
	e := newEnv()
	j := seedJurisdiction(t, "AU")
	a := seedRule(t, j.JurisdictionID, "RULE-A")
	b := seedRule(t, j.JurisdictionID, "RULE-B")
	code, r := e.do("PUT", "/v1/admin/rules/"+a.JurisdictionRuleID+"/provenance", "author", map[string]any{"supersedes_rule_id": b.JurisdictionRuleID})
	mustStatus(t, 400, code, r)
	code, r = e.do("PUT", "/v1/admin/rules/"+a.JurisdictionRuleID+"/provenance", "author", map[string]any{"supersedes_rule_id": a.JurisdictionRuleID})
	mustStatus(t, 400, code, r)
}

// ── packs ───────────────────────────────────────────────────────────────────

func manifest(ref, version, jurCode string, extra map[string]any) map[string]any {
	m := map[string]any{
		"pack_id": ref, "pack_version": version, "jurisdiction_ids": []string{jurCode}, "regimes": []string{"VAT"},
		"effective_from": "2026-08-01", "dependencies": []map[string]string{{"ref": "ref.iso4217", "version": "2026.06"}},
		"source_register_version": 14}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestPackVersioning_Immutability_AndDigest(t *testing.T) {
	e := newEnv()
	j := seedJurisdiction(t, "GBP")
	_, _ = e.do("POST", "/v1/admin/regimes", "author", map[string]any{"regime_code": "VAT", "regime_name": "Value added tax"})
	ref := "jur.t" + strings.ToLower(uuid.NewString()[:6]) + ".tax.core"
	code, p := e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": ref, "pack_name": "Core tax", "owner": "Global Tax Engineering"})
	mustStatus(t, 201, code, p)
	code, p = e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": ref, "pack_name": "Core tax", "owner": "Global Tax Engineering"})
	mustStatus(t, 200, code, p)

	vpath := "/v1/admin/packs/" + ref + "/versions"
	m1 := manifest(ref, "2026.08.1", j.JurisdictionCode, nil)
	code, v := e.do("POST", vpath, "author", m1)
	mustStatus(t, 201, code, v)
	assert.Equal(t, "DRAFT", v["status"])
	digest, _ := v["manifest_digest"].(string)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, digest)

	// Replay (even with reordered keys) is idempotent and yields the same digest.
	code, v2 := e.do("POST", vpath, "author", m1)
	mustStatus(t, 200, code, v2)
	assert.Equal(t, digest, v2["manifest_digest"])

	// JUR-NEG-20: same version, different content -> refused.
	changed := manifest(ref, "2026.08.1", j.JurisdictionCode, map[string]any{"effective_from": "2026-09-01"})
	code, r := e.do("POST", vpath, "author", changed)
	mustStatus(t, 409, code, r)

	// Versions must increase; numeric ordering (10 > 9).
	code, r = e.do("POST", vpath, "author", manifest(ref, "2026.08.0", j.JurisdictionCode, nil))
	mustStatus(t, 409, code, r)
	assert.Equal(t, "version_not_newer", r["error"])
	code, r = e.do("POST", vpath, "author", manifest(ref, "2026.08.9", j.JurisdictionCode, nil))
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", vpath, "author", manifest(ref, "2026.08.10", j.JurisdictionCode, nil))
	mustStatus(t, 201, code, r)

	// Server-owned fields and floating dependencies are rejected.
	code, r = e.do("POST", vpath, "author", manifest(ref, "2026.09.1", j.JurisdictionCode, map[string]any{"status": "released"}))
	mustStatus(t, 400, code, r)
	code, r = e.do("POST", vpath, "author", manifest(ref, "2026.09.1", j.JurisdictionCode, map[string]any{"signature": "forged"}))
	mustStatus(t, 400, code, r)
	code, r = e.do("POST", vpath, "author", manifest(ref, "2026.09.1", j.JurisdictionCode,
		map[string]any{"dependencies": []map[string]string{{"ref": "global.tax.core", "version": "latest"}}}))
	mustStatus(t, 400, code, r)
	code, r = e.do("POST", vpath, "author", manifest(ref, "2026.09.1", "NOPE-NOT-REAL", nil))
	mustStatus(t, 400, code, r)
	code, r = e.do("POST", vpath, "author", manifest("jur.other.tax.core", "2026.09.1", j.JurisdictionCode, nil))
	mustStatus(t, 400, code, r)
	assert.Equal(t, "pack_mismatch", r["error"])

	// DRAFT -> REVIEW once; afterwards the content is frozen in SQL too.
	code, r = e.do("POST", vpath+"/2026.08.1/submit-review", "author", nil)
	mustStatus(t, 200, code, r)
	assert.Equal(t, "REVIEW", r["status"])
	code, r = e.do("POST", vpath+"/2026.08.1/submit-review", "author", nil)
	mustStatus(t, 200, code, r)
	assert.Equal(t, 1, e.pub.count(events.EventPackVersionSubmitted), "replay emits no second event")

	_, err := pool.Exec(ctx, `UPDATE jurisdiction_pack_versions SET manifest='{"x":1}'::jsonb WHERE manifest_digest=$1`, digest)
	require.Error(t, err, "manifest of a version past DRAFT is immutable")
	_, err = pool.Exec(ctx, `UPDATE jurisdiction_pack_versions SET status='RELEASED' WHERE manifest_digest=$1`, digest)
	require.Error(t, err, "REVIEW cannot jump to RELEASED: certification is mandatory")
	_, err = pool.Exec(ctx, `DELETE FROM jurisdiction_pack_versions WHERE manifest_digest=$1`, digest)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO pack_dependencies SELECT pack_version_id, 'x.y', '1' FROM jurisdiction_pack_versions WHERE manifest_digest=$1`, digest)
	require.Error(t, err, "dependencies freeze with the version")

	code, got := e.do("GET", "/v1/packs/"+ref+"/versions/2026.08.1", "", nil)
	mustStatus(t, 200, code, got)
	deps := 0
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM pack_dependencies d JOIN jurisdiction_pack_versions v USING (pack_version_id) WHERE v.manifest_digest=$1`, digest).Scan(&deps))
	assert.Equal(t, 1, deps)
}

func TestDigestIsCanonical(t *testing.T) {
	a, err := domain.DigestOf([]byte(`{"b":1,"a":[1,2],"c":{"y":1,"x":2}}`))
	require.NoError(t, err)
	b, err := domain.DigestOf([]byte("{ \"c\":{\"x\":2,\"y\":1}, \"a\":[1,2], \"b\":1 }"))
	require.NoError(t, err)
	assert.Equal(t, a, b, "key order and whitespace never change the digest")
	c, _ := domain.DigestOf([]byte(`{"b":2,"a":[1,2],"c":{"y":1,"x":2}}`))
	assert.NotEqual(t, a, c)
}

// ── authorization and API hygiene ───────────────────────────────────────────

func TestAdminRoutesRequireIdentityAndAuthorization(t *testing.T) {
	e := newEnv()
	j := seedJurisdiction(t, "CA")
	code, _ := e.do("POST", "/v1/admin/sources", "", sourceBody(j.JurisdictionID))
	assert.Equal(t, 401, code)

	e.auth.deny = "regulatory_source.create"
	code, _ = e.do("POST", "/v1/admin/sources", "author", sourceBody(j.JurisdictionID))
	assert.Equal(t, 403, code)
	e.auth.deny = "interpretation_record.approve"
	code, _ = e.do("POST", "/v1/admin/interpretations/"+uuid.NewString()+"/approve", "approver", nil)
	assert.Equal(t, 403, code, "authorization is checked before any lookup")

	// Reads are public like the existing registry; unknown ids are 404, not 5xx.
	for _, p := range []string{"/v1/sources/" + uuid.NewString(), "/v1/sources/not-a-uuid", "/v1/packs/jur.no.such.pack",
		"/v1/packs/jur.no.such.pack/versions", "/v1/interpretations/" + uuid.NewString(), "/v1/regimes/NOPE",
		"/v1/rules/" + uuid.NewString() + "/provenance"} {
		code, _ := e.do("GET", p, "", nil)
		assert.Equal(t, 404, code, p)
	}

	// Unknown fields and oversize bodies are rejected, like the existing endpoints.
	code, _ = e.do("POST", "/v1/admin/regimes", "author", map[string]any{"regime_code": "X", "regime_name": "Y", "bogus": 1})
	assert.Equal(t, 400, code)
}

func TestExistingEndpointsAreUnaffected(t *testing.T) {
	e := newEnv()
	j := seedJurisdiction(t, "NZ")
	seedRule(t, j.JurisdictionID, "R1")
	code, r := e.do("GET", "/v1/jurisdictions/"+j.JurisdictionID, "", nil)
	mustStatus(t, 200, code, r)
	code, r = e.do("GET", "/v1/jurisdictions/"+j.JurisdictionID+"/rule-pack", "", nil)
	mustStatus(t, 200, code, r)
	code, r = e.do("GET", "/v1/jurisdictions/"+j.JurisdictionID+"/rules", "", nil)
	mustStatus(t, 200, code, r)
}

func TestRegistryRoutesAbsentWithoutRegistry(t *testing.T) {
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(st, authz.NewStubAuthZClient(zap.NewNop()), events.NewNoopPublisher(zap.NewNop()), "s", zap.NewNop()))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/regimes", nil))
	assert.Equal(t, 404, rr.Code, "the original constructor exposes exactly the original routes")
}

// ── migration round trip ────────────────────────────────────────────────────

func TestMigrations000005To000015_DownThenUp(t *testing.T) {
	// Each migration depends on the earlier ones: down newest-first, up oldest-first.
	names := []string{"000005_jurisdiction_pack_registries", "000006_pack_artifacts_and_signing_keys", "000007_pack_certification", "000008_rule_decision_evidence",
		"000009_pack_release_operations", "000010_calendars_and_obligations", "000011_rule_parameters", "000012_payroll_parameter_evidence", "000013_records_evidence", "000014_regulatory_submissions", "000015_jurisdiction_rollout"}
	var downs, ups []string
	for i := len(names) - 1; i >= 0; i-- {
		downs = append(downs, migrations(t, names[i]+".down.sql")...)
	}
	for _, n := range names {
		ups = append(ups, migrations(t, n+".up.sql")...)
	}
	require.NoError(t, applyAll(ctx, downs), "down")
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_name IN
		('regulatory_sources','jurisdiction_packs','jurisdiction_pack_versions','rule_sources','regulatory_regimes')`).Scan(&n))
	assert.Equal(t, 0, n, "down removes every table it created")
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_name IN ('regulatory_calendars','obligation_rules','pack_release_events','pack_deployments','pack_hotfixes','source_change_notices','rule_decision_evidence','pack_artifacts','pack_signing_keys','pack_certifications','pack_reviews','pack_test_runs','pack_test_bundles')`).Scan(&n))
	assert.Equal(t, 0, n, "000006 down removes its tables")
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name='jurisdiction_rules' AND column_name IN ('regime_id','precedence','interpretation_id')`).Scan(&n))
	assert.Equal(t, 0, n, "down removes the provenance columns")
	require.NoError(t, applyAll(ctx, ups), "up again")
}

// The original lifecycle (activate, drift, supersede) must keep working with
// the new provenance triggers installed.
func TestOriginalRuleLifecycleUnaffectedByProvenanceTriggers(t *testing.T) {
	j := seedJurisdiction(t, "SG")
	rule := seedRule(t, j.JurisdictionID, "LIFECYCLE")

	active, changed, err := st.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID: rule.JurisdictionRuleID, NewStatus: "ACTIVE", AllowedPriors: []string{"DRAFT"}, ActorID: "approver"})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "ACTIVE", active.RuleStatus)

	_, ev, changed, err := st.RecordDrift(ctx, domain.RecordDriftParams{
		JurisdictionRuleID: rule.JurisdictionRuleID, ToState: "DRIFTED", RecordedByPrincipalID: "monitor"})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.NotNil(t, ev)

	sup, _, err := st.TransitionRuleStatus(ctx, store.TransitionParams{
		RuleID: rule.JurisdictionRuleID, NewStatus: "SUPERSEDED", AllowedPriors: []string{"ACTIVE"}, EndDate: true, ActorID: "approver"})
	require.NoError(t, err)
	assert.Equal(t, "SUPERSEDED", sup.RuleStatus)
	assert.NotNil(t, sup.EffectiveTo)

	_, err = st.DeactivateJurisdiction(ctx, j.JurisdictionID, "admin")
	require.NoError(t, err)
}
