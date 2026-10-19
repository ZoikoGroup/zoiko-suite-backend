package store_test

// INT-10 provisioning end to end through the real store, as an ORDINARY database role, with fakes only for the
// two services federation calls (access-control-svc and the session cache).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/federation"
)

const (
	pRoleBaseline = "11111111-1111-4111-8111-111111111111"
	pRoleSecond   = "22222222-2222-4222-8222-222222222222"
	pRolePriv     = "99999999-9999-4999-8999-999999999999"
	pEntity       = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

type fakeAC struct {
	mu          sync.Mutex
	unavailable bool
	privileged  map[string]bool // roles access control would refuse as non-baseline
	refuse      error
	requests    []federation.AssignmentRequest
	revokes     []string
	byCorr      map[string]string
	revokeErr   error
	eligibleErr error
}

func newFakeAC() *fakeAC {
	return &fakeAC{privileged: map[string]bool{pRolePriv: true}, byCorr: map[string]string{}}
}

func (f *fakeAC) RoleEligibility(_ context.Context, _, role string) (federation.Eligibility, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.eligibleErr != nil {
		return federation.Eligibility{}, f.eligibleErr
	}
	if f.privileged[role] {
		return federation.Eligibility{Eligible: false, RiskTier: "CRITICAL", Reason: "payment.release=CRITICAL"}, nil
	}
	return federation.Eligibility{Eligible: true, RiskTier: "STANDARD"}, nil
}

func (f *fakeAC) RequestAssignment(_ context.Context, r federation.AssignmentRequest) (federation.AssignmentOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if f.unavailable {
		return federation.AssignmentOutcome{}, fmt.Errorf("%w: down", federation.ErrAccessControlUnavailable)
	}
	if f.privileged[r.RoleDefinitionID] {
		return federation.AssignmentOutcome{}, federation.ErrPrivilegedRefused
	}
	if f.refuse != nil {
		return federation.AssignmentOutcome{}, f.refuse
	}
	// idempotent on the correlation id, like access control
	if id, ok := f.byCorr[r.CorrelationID]; ok {
		return federation.AssignmentOutcome{RequestID: id, Status: "PROVISIONED"}, nil
	}
	id := fmt.Sprintf("req-%d", len(f.byCorr)+1)
	f.byCorr[r.CorrelationID] = id
	return federation.AssignmentOutcome{RequestID: id, Status: "PROVISIONED"}, nil
}

func (f *fakeAC) RevokeAssignment(_ context.Context, _, _, requestID, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailable {
		return fmt.Errorf("%w: down", federation.ErrAccessControlUnavailable)
	}
	if f.revokeErr != nil {
		return f.revokeErr
	}
	f.revokes = append(f.revokes, requestID)
	return nil
}

func (f *fakeAC) set(fn func())  { f.mu.Lock(); fn(); f.mu.Unlock() }
func (f *fakeAC) nRequests() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.requests) }
func (f *fakeAC) nRevokes() int  { f.mu.Lock(); defer f.mu.Unlock(); return len(f.revokes) }

type fakeEvict struct {
	mu    sync.Mutex
	fail  bool
	calls []string
}

func (f *fakeEvict) EvictAllForPrincipal(_ context.Context, principalID, tenantID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tenantID+"/"+principalID)
	if f.fail {
		return 0, errors.New("redis down")
	}
	return 1, nil
}
func (f *fakeEvict) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

type provRig struct {
	*fedRig
	prov  *federation.Provisioning
	ac    *fakeAC
	sess  *fakeEvict
	conn  string
	cfgFP string
}

func newProvRig(t *testing.T) *provRig {
	t.Helper()
	base := newFedRig(t)
	ac, sess := newFakeAC(), &fakeEvict{}
	prov, err := federation.NewProvisioning(base.s, base.s, ac, sess, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	prov.BackoffBase, prov.BackoffMax, prov.RevokeBackoffMax = time.Millisecond, 5*time.Millisecond, 5*time.Millisecond
	r := &provRig{fedRig: base, prov: prov, ac: ac, sess: sess, conn: "c-prov"}
	r.active(t, "t-1", r.conn, "https://idp.example.com")
	r.mapping(t, "t-1", r.conn, baselineMapping())
	return r
}

func baselineMapping() federation.MappingConfig {
	return federation.MappingConfig{Entries: []federation.MappingEntry{
		{Attribute: "groups", Value: "AP-Clerks", RoleDefinitionID: pRoleBaseline, LegalEntityID: pEntity},
		{Attribute: "department", Value: "Finance", RoleDefinitionID: pRoleSecond, LegalEntityID: pEntity},
	}}
}

func (r *provRig) mapping(t *testing.T, tenant, conn string, cfg federation.MappingConfig) *federation.MappingVersion {
	t.Helper()
	ctx := context.Background()
	m, err := r.prov.ConfigureMapping(ctx, tenant, "maker", "corr", conn, cfg)
	if err != nil {
		t.Fatalf("configure mapping: %v", err)
	}
	if _, err := r.prov.ApproveMapping(ctx, tenant, "checker", "corr", conn, m.Version, m.Fingerprint, "ok"); err != nil {
		t.Fatalf("approve mapping: %v", err)
	}
	return m
}

func (r *provRig) token(t *testing.T, tenant, conn string) string {
	t.Helper()
	tok, err := r.prov.IssueSCIMToken(context.Background(), tenant, "admin", "corr", conn)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (r *provRig) target(t *testing.T, tenant, conn string) *federation.SCIMTarget {
	t.Helper()
	tg, err := r.prov.AuthenticateSCIM(context.Background(), tenant, conn, r.token(t, tenant, conn))
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func userJSON(ext, email string, active bool, groups ...string) string {
	var g []string
	for _, x := range groups {
		g = append(g, fmt.Sprintf(`{"value":%q,"display":%q}`, x, x))
	}
	return fmt.Sprintf(`{"userName":%q,"externalId":%q,"displayName":"User %s","emails":[{"value":%q,"primary":true}],"active":%v,"groups":[%s]}`,
		email, ext, ext, email, active, strings.Join(g, ","))
}

func req(method, path, body, event string) federation.SCIMRequest {
	return federation.SCIMRequest{Method: method, Path: path, Body: []byte(body), EventID: event, CorrelationID: "corr-scim"}
}

func (r *provRig) create(t *testing.T, tg *federation.SCIMTarget, ext, email string, groups ...string) *federation.Outcome {
	t.Helper()
	out, err := r.prov.CreateUser(context.Background(), tg, req("POST", "/Users", userJSON(ext, email, true, groups...), ""))
	if err != nil {
		t.Fatalf("create %s: %v", ext, err)
	}
	return out
}

func (r *provRig) assignments(t *testing.T, linkID string) map[string]string {
	t.Helper()
	rows, err := r.admin.Query(context.Background(), `SELECT role_definition_id||'|'||status FROM federation_assignments WHERE link_id = $1`, linkID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		parts := strings.SplitN(s, "|", 2)
		out[parts[0]] = parts[1]
	}
	return out
}

func (r *provRig) principalStatus(t *testing.T, principalID string) string {
	t.Helper()
	var s string
	if err := r.admin.QueryRow(context.Background(), `SELECT status FROM principals WHERE principal_id = $1`, principalID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// ── mapping governance ───────────────────────────────────────────────────────

func TestProvisioning_MappingNeedsABaselineRoleAndAnIndependentApprover(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	priv := federation.MappingConfig{Entries: []federation.MappingEntry{{Attribute: "groups", Value: "Treasury-Releasers", RoleDefinitionID: pRolePriv, LegalEntityID: pEntity}}}
	if _, err := r.prov.ConfigureMapping(ctx, "t-1", "maker", "corr", r.conn, priv); !errors.Is(err, federation.ErrInvalid) {
		t.Fatalf("a privileged role cannot be put in a mapping: %v", err)
	}
	// access control unreachable: nothing is created, and it is an availability error, never "eligible"
	r.ac.set(func() { r.ac.eligibleErr = fmt.Errorf("%w: down", federation.ErrAccessControlUnavailable) })
	if _, err := r.prov.ConfigureMapping(ctx, "t-1", "maker", "corr", r.conn, baselineMapping()); !errors.Is(err, federation.ErrAccessControlUnavailable) {
		t.Fatalf("%v", err)
	}
	r.ac.set(func() { r.ac.eligibleErr = nil })
	if n := r.count(t, `SELECT count(*) FROM federation_mapping_versions WHERE status = 'PENDING_APPROVAL'`); n != 0 {
		t.Fatalf("a refused mapping must leave no pending version: %d", n)
	}

	m, err := r.prov.ConfigureMapping(ctx, "t-1", "maker", "corr", r.conn, federation.MappingConfig{Entries: []federation.MappingEntry{
		{Attribute: "groups", Value: "AP-Readers", RoleDefinitionID: pRoleSecond, LegalEntityID: pEntity}}})
	if err != nil || m.Status != federation.MapPending || m.Version != 2 {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := r.prov.ConfigureMapping(ctx, "t-1", "maker", "corr", r.conn, baselineMapping()); !errors.Is(err, federation.ErrConflict) {
		t.Fatalf("one proposal at a time: %v", err)
	}
	if _, err := r.prov.ApproveMapping(ctx, "t-1", "maker", "corr", r.conn, 2, m.Fingerprint, ""); !errors.Is(err, federation.ErrSelfApproval) {
		t.Fatalf("the author cannot approve: %v", err)
	}
	if _, err := r.prov.ApproveMapping(ctx, "t-1", "checker", "corr", r.conn, 2, "wrong", ""); !errors.Is(err, federation.ErrFingerprint) {
		t.Fatalf("a different fingerprint: %v", err)
	}
	// between proposal and approval the role gained a privileged action: approval re-checks and refuses
	r.ac.set(func() { r.ac.privileged[pRoleSecond] = true })
	if _, err := r.prov.ApproveMapping(ctx, "t-1", "checker", "corr", r.conn, 2, m.Fingerprint, ""); !errors.Is(err, federation.ErrInvalid) {
		t.Fatalf("a role that became privileged cannot be approved into a mapping: %v", err)
	}
	r.ac.set(func() { r.ac.privileged[pRoleSecond] = false })
	got, err := r.prov.ApproveMapping(ctx, "t-1", "checker", "corr", r.conn, 2, m.Fingerprint, "ok")
	if err != nil || got.Status != federation.MapApproved {
		t.Fatalf("%+v %v", got, err)
	}
	st, _ := r.prov.GetMapping(ctx, "t-1", r.conn)
	if st.Active == nil || st.Active.Version != 2 || len(st.Versions) != 2 || st.Versions[1].Status != federation.MapSuperseded {
		t.Fatalf("approval supersedes the earlier version: %+v", st)
	}
	// an approved mapping can never be rewritten
	if _, err := r.admin.Exec(ctx, `UPDATE federation_mapping_versions SET config = '{"entries":[]}' WHERE version = 2`); err == nil {
		t.Fatal("a mapping version is immutable")
	}
	if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'identity.federation.mapping_approved'`) != 2 {
		t.Fatal("approvals are announced")
	}
}

// ── SCIM tokens ──────────────────────────────────────────────────────────────

func TestProvisioning_SCIMTokens(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	tok1 := r.token(t, "t-1", r.conn)
	if !strings.HasPrefix(tok1, "zscim_") || len(tok1) < 40 {
		t.Fatalf("%q", tok1)
	}
	var stored int
	_ = r.admin.QueryRow(ctx, `SELECT count(*) FROM federation_scim_tokens WHERE token_hash = $1`, tok1).Scan(&stored)
	if stored != 0 {
		t.Fatal("the token itself must never be stored")
	}
	if _, err := r.prov.AuthenticateSCIM(ctx, "t-1", r.conn, tok1); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ tenant, conn, tok string }{
		"wrong token":        {"t-1", r.conn, "zscim_nope"},
		"empty token":        {"t-1", r.conn, ""},
		"another tenant":     {"t-2", r.conn, tok1},
		"another connection": {"t-1", "other", tok1},
		"oversized":          {"t-1", r.conn, strings.Repeat("a", 500)},
	} {
		if _, err := r.prov.AuthenticateSCIM(ctx, c.tenant, c.conn, c.tok); !errors.Is(err, federation.ErrTokenInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// rotation revokes the earlier token
	tok2 := r.token(t, "t-1", r.conn)
	if _, err := r.prov.AuthenticateSCIM(ctx, "t-1", r.conn, tok1); !errors.Is(err, federation.ErrTokenInvalid) {
		t.Fatalf("the old token must stop working: %v", err)
	}
	if _, err := r.prov.AuthenticateSCIM(ctx, "t-1", r.conn, tok2); err != nil {
		t.Fatal(err)
	}
	if n, err := r.prov.RevokeSCIMToken(ctx, "t-1", "admin", "corr", r.conn); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := r.prov.AuthenticateSCIM(ctx, "t-1", r.conn, tok2); !errors.Is(err, federation.ErrTokenInvalid) {
		t.Fatalf("%v", err)
	}
	// a suspended or revoked connection: suspended still authenticates (so it can deprovision), revoked does not
	tok3 := r.token(t, "t-1", r.conn)
	_, _ = r.s.SetStatus(ctx, "t-1", r.conn, federation.ConnSuspended, "op", "stop", "corr")
	if tg, err := r.prov.AuthenticateSCIM(ctx, "t-1", r.conn, tok3); err != nil || tg.Conn.Status != federation.ConnSuspended {
		t.Fatalf("a suspended connection still takes deprovisioning: %v", err)
	}
	_, _ = r.s.SetStatus(ctx, "t-1", r.conn, federation.ConnRevoked, "op", "left", "corr")
	if _, err := r.prov.AuthenticateSCIM(ctx, "t-1", r.conn, tok3); !errors.Is(err, federation.ErrTokenInvalid) {
		t.Fatalf("a revoked connection takes nothing: %v", err)
	}
}

// ── creating people ──────────────────────────────────────────────────────────

func TestProvisioning_CreateGrantsOnlyWhatTheApprovedMappingNames(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	tg := r.target(t, "t-1", r.conn)

	out := r.create(t, tg, "dir-1", "alice@corp.example", "AP-Clerks", "Admins", "PLATFORM_ADMIN", pRolePriv)
	if out.Result == nil || !out.Result.Created || out.Result.Planned != 1 {
		t.Fatalf("exactly one baseline role is planned, the directory's own claims are not entitlements: %+v", out.Result)
	}
	link := out.View.Link
	if link.Source != federation.SourceSCIM || link.Status != federation.LinkActive || link.Issuer != "https://idp.example.com" {
		t.Fatalf("%+v", link)
	}
	if got := r.assignments(t, link.LinkID); len(got) != 1 || got[pRoleBaseline] != federation.AsgProvisioned {
		t.Fatalf("the baseline role was requested and granted inline: %v", got)
	}
	if r.ac.nRequests() != 1 || r.ac.requests[0].RoleDefinitionID != pRoleBaseline || r.ac.requests[0].TargetPrincipalID != link.PrincipalID {
		t.Fatalf("access control was asked for exactly one baseline role: %+v", r.ac.requests)
	}
	if !strings.HasPrefix(r.ac.requests[0].CorrelationID, "fed-") {
		t.Fatalf("a fixed correlation per work item makes a retry a replay: %q", r.ac.requests[0].CorrelationID)
	}
	if r.principalStatus(t, link.PrincipalID) != "ACTIVE" {
		t.Fatal("the principal is active")
	}
	var subj string
	_ = r.admin.QueryRow(ctx, `SELECT identity_provider_subject FROM principals WHERE principal_id = $1`, link.PrincipalID).Scan(&subj)
	if subj != "federated:"+r.conn+":dir-1" {
		t.Fatalf("the principal's subject is bound to the connection and the stable id, never the email: %q", subj)
	}
	if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.link.created'`) != 1 ||
		r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.provisioning.applied'`) != 1 {
		t.Fatal("identity_link_created and provisioning_applied are announced")
	}
	if r.count(t, `SELECT count(*) FROM federation_provisioning_events WHERE status='APPLIED' AND source='SCIM' AND operation='CREATE'`) != 1 {
		t.Fatal("the event is in the ledger, applied")
	}
	if got, _ := out.Event.Result["ignored_values"].(int); got != 3 {
		t.Fatalf("ignored directory values are counted: %v", out.Event.Result)
	}
}

func TestProvisioning_DuplicateEventsAreAbsorbed(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	tg := r.target(t, "t-1", r.conn)
	body := userJSON("dir-1", "alice@corp.example", true, "AP-Clerks")

	first, err := r.prov.CreateUser(ctx, tg, req("POST", "/Users", body, "evt-1"))
	if err != nil || first.Replayed {
		t.Fatalf("%+v %v", first, err)
	}
	again, err := r.prov.CreateUser(ctx, tg, req("POST", "/Users", body, "evt-1"))
	if err != nil || !again.Replayed || again.View.Link.LinkID != first.View.Link.LinkID {
		t.Fatalf("the same event id is answered from the record: %+v %v", again, err)
	}
	if r.ac.nRequests() != 1 || r.count(t, `SELECT count(*) FROM principals WHERE tenant_id='t-1' AND identity_provider_subject LIKE 'federated:%'`) != 1 {
		t.Fatal("a duplicate event has no second effect")
	}
	if r.count(t, `SELECT count(*) FROM federation_provisioning_events`) != 1 {
		t.Fatal("one ledger row")
	}
	// the same event id with a different payload is refused, not applied
	if _, err := r.prov.CreateUser(ctx, tg, req("POST", "/Users", userJSON("dir-2", "bob@corp.example", true), "evt-1")); !errors.Is(err, federation.ErrDedupeMismatch) {
		t.Fatalf("%v", err)
	}
	// without an id, an identical retry within the window is absorbed...
	b2 := userJSON("dir-3", "carol@corp.example", true)
	a, _ := r.prov.CreateUser(ctx, tg, req("POST", "/Users", b2, ""))
	b, err := r.prov.CreateUser(ctx, tg, req("POST", "/Users", b2, ""))
	if err != nil || !b.Replayed || b.View.Link.LinkID != a.View.Link.LinkID {
		t.Fatalf("%+v %v", b, err)
	}
	// ...and a create for a person who already exists (a different event) is answered as that person, not duplicated
	c, err := r.prov.CreateUser(ctx, tg, req("POST", "/Users", b2, "evt-other"))
	if err != nil || c.Result.Created || c.View.Link.LinkID != a.View.Link.LinkID {
		t.Fatalf("an existing person is not created twice: %+v %v", c, err)
	}
}

func TestProvisioning_EmailCollisionQuarantinesInsteadOfMerging(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	tg := r.target(t, "t-1", r.conn)
	first := r.create(t, tg, "dir-1", "shared@corp.example", "AP-Clerks")

	// a different directory subject asserts the SAME address: a different person, or a reassigned mailbox
	_, err := r.prov.CreateUser(ctx, tg, req("POST", "/Users", userJSON("dir-2", "Shared@Corp.Example", true, "AP-Clerks"), "evt-2"))
	var se *federation.SCIMError
	if !errors.As(err, &se) || se.Status != 409 || se.Type != "uniqueness" {
		t.Fatalf("%v", err)
	}
	if r.count(t, `SELECT count(*) FROM principals WHERE tenant_id='t-1' AND identity_provider_subject LIKE 'federated:%'`) != 1 {
		t.Fatal("the second person must not be created, nor merged into the first")
	}
	if r.count(t, `SELECT count(*) FROM federation_links WHERE subject = 'dir-2'`) != 0 {
		t.Fatal("no link for the quarantined subject")
	}
	if r.ac.nRequests() != 1 {
		t.Fatal("nothing is granted to a quarantined record")
	}
	ex, _ := r.prov.Exceptions(ctx, "t-1", r.conn, "OPEN", 10)
	if len(ex) != 1 || ex[0].Kind != federation.ExcAmbiguousIdentity || ex[0].EventID == "" {
		t.Fatalf("it is listed for a human: %+v", ex)
	}
	// the directory's retry of the same event gets the same answer, with no second exception
	_, err = r.prov.CreateUser(ctx, tg, req("POST", "/Users", userJSON("dir-2", "Shared@Corp.Example", true, "AP-Clerks"), "evt-2"))
	if !errors.As(err, &se) || se.Status != 409 {
		t.Fatalf("a quarantined event stays quarantined: %v", err)
	}
	if ex, _ = r.prov.Exceptions(ctx, "t-1", r.conn, "OPEN", 10); len(ex) != 1 {
		t.Fatalf("%d", len(ex))
	}
	// the first person is untouched, and resolving the exception is a human's recorded decision
	if r.principalStatus(t, first.View.Link.PrincipalID) != "ACTIVE" {
		t.Fatal("untouched")
	}
	if _, err := r.prov.ResolveException(ctx, "t-1", "admin", ex[0].ExceptionID, "  "); !errors.Is(err, federation.ErrInvalid) {
		t.Fatalf("a note is required: %v", err)
	}
	if e, err := r.prov.ResolveException(ctx, "t-1", "admin", ex[0].ExceptionID, "confirmed distinct people; renaming the mailbox"); err != nil || e.Status != "RESOLVED" || e.ResolvedBy != "admin" {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := r.prov.ResolveException(ctx, "t-1", "admin", ex[0].ExceptionID, "again"); !errors.Is(err, federation.ErrExceptionState) {
		t.Fatalf("%v", err)
	}
	if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.provisioning.rejected'`) != 1 {
		t.Fatal("provisioning_rejected is announced")
	}
	// an address that exists on someone ELSE'S principal (not provisioned by federation at all) also quarantines
	insertPrincipal(t, ctx, r.admin, "p-local", "t-1", "local|x", "ACTIVE")
	var email string
	_ = r.admin.QueryRow(ctx, `SELECT email FROM principals WHERE principal_id = 'p-local'`).Scan(&email)
	if _, err := r.prov.CreateUser(ctx, tg, req("POST", "/Users", userJSON("dir-9", email, true), "evt-9")); !errors.As(err, &se) || se.Status != 409 {
		t.Fatalf("an address held by any principal in the tenant: %v", err)
	}
}

func TestProvisioning_EmailCollisionAcrossTenantsIsNotACollision(t *testing.T) {
	r := newProvRig(t)
	tg := r.target(t, "t-1", r.conn)
	r.create(t, tg, "dir-1", "alice@corp.example")
	r.active(t, "t-2", "c-t2", "https://idp.example.com")
	tg2 := r.target(t, "t-2", "c-t2")
	r.create(t, tg2, "dir-1", "alice@corp.example") // same address, same subject id, another tenant
	if r.count(t, `SELECT count(*) FROM federation_links WHERE subject='dir-1'`) != 2 {
		t.Fatal("tenants are isolated universes")
	}
}

func TestProvisioning_AmbiguousMappingQuarantinesAPerson(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	cfg := federation.MappingConfig{MaxRolesPerUser: 1, Entries: baselineMapping().Entries}
	r.mapping(t, "t-1", r.conn, cfg)
	tg := r.target(t, "t-1", r.conn)
	body := `{"userName":"a@corp.example","externalId":"dir-a","emails":[{"value":"a@corp.example"}],"groups":[{"value":"AP-Clerks"}],"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":{"department":"Finance"}}`
	_, err := r.prov.CreateUser(ctx, tg, req("POST", "/Users", body, "evt-a"))
	var se *federation.SCIMError
	if !errors.As(err, &se) || se.Status != 409 {
		t.Fatalf("%v", err)
	}
	if r.count(t, `SELECT count(*) FROM federation_links`) != 0 {
		t.Fatal("no principal for an ambiguous record")
	}
	ex, _ := r.prov.Exceptions(ctx, "t-1", r.conn, "OPEN", 10)
	if len(ex) != 1 || ex[0].Kind != federation.ExcAmbiguousMapping {
		t.Fatalf("%+v", ex)
	}
}

// ── privilege can never arrive through the directory ─────────────────────────

func TestProvisioning_AccessControlRefusalIsFinalAndRaised(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	tg := r.target(t, "t-1", r.conn)
	// access control declares the role privileged AFTER the mapping was approved
	r.ac.set(func() { r.ac.privileged[pRoleBaseline] = true })
	out := r.create(t, tg, "dir-1", "alice@corp.example", "AP-Clerks")
	got := r.assignments(t, out.View.Link.LinkID)
	if got[pRoleBaseline] != federation.AsgRefused {
		t.Fatalf("access control's refusal is final: %v", got)
	}
	ex, _ := r.prov.Exceptions(ctx, "t-1", r.conn, "OPEN", 10)
	if len(ex) != 1 || ex[0].Kind != federation.ExcPrivilegedRefused || ex[0].AssignmentID == "" {
		t.Fatalf("a human is told: %+v", ex)
	}
	// the identity exists with no access; nothing queued to retry
	if r.principalStatus(t, out.View.Link.PrincipalID) != "ACTIVE" {
		t.Fatal("the person exists")
	}
	n := r.ac.nRequests()
	if _, err := r.prov.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if r.ac.nRequests() != n {
		t.Fatal("a refused request is not retried")
	}
	// a definite refusal that is not about privilege (SoD) is also final
	r.ac.set(func() {
		r.ac.privileged[pRoleBaseline] = false
		r.ac.refuse = &federation.RequestRefused{Status: 403, Code: "sod_conflict"}
	})
	out2 := r.create(t, tg, "dir-2", "bob@corp.example", "AP-Clerks")
	if got := r.assignments(t, out2.View.Link.LinkID); got[pRoleBaseline] != federation.AsgRefused {
		t.Fatalf("%v", got)
	}
	ex, _ = r.prov.Exceptions(ctx, "t-1", r.conn, "OPEN", 10)
	kinds := map[string]bool{}
	for _, e := range ex {
		kinds[e.Kind] = true
	}
	if !kinds[federation.ExcRequestRefused] {
		t.Fatalf("%+v", ex)
	}
}

func TestProvisioning_AccessControlOutageRetriesThenRecovers(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	r.prov.ExceptionAfter = 3
	tg := r.target(t, "t-1", r.conn)
	r.ac.set(func() { r.ac.unavailable = true })
	out := r.create(t, tg, "dir-1", "alice@corp.example", "AP-Clerks")
	link := out.View.Link
	if got := r.assignments(t, link.LinkID); got[pRoleBaseline] != federation.AsgPlanned {
		t.Fatalf("with access control down the work stays queued, neither granted nor refused: %v", got)
	}
	if out.View.Link.Status != federation.LinkActive {
		t.Fatal("the identity is created regardless")
	}
	for i := 0; i < 4; i++ {
		time.Sleep(8 * time.Millisecond)
		if _, err := r.prov.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ex, _ := r.prov.Exceptions(ctx, "t-1", r.conn, "OPEN", 10)
	if len(ex) != 1 || ex[0].Kind != federation.ExcAccessControlDown {
		t.Fatalf("persistent failure is raised once: %+v", ex)
	}
	var attempts int
	_ = r.admin.QueryRow(ctx, `SELECT attempts FROM federation_assignments WHERE link_id = $1`, link.LinkID).Scan(&attempts)
	if attempts < 3 {
		t.Fatalf("attempts are counted: %d", attempts)
	}
	r.ac.set(func() { r.ac.unavailable = false })
	time.Sleep(10 * time.Millisecond)
	res, err := r.prov.Sweep(ctx)
	if err != nil || res.Assignments == 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if got := r.assignments(t, link.LinkID); got[pRoleBaseline] != federation.AsgProvisioned {
		t.Fatalf("recovered: %v", got)
	}
	// every attempt used the same correlation, so access control saw one request however many times we asked
	r.ac.mu.Lock()
	corrs := map[string]bool{}
	for _, q := range r.ac.requests {
		corrs[q.CorrelationID] = true
	}
	r.ac.mu.Unlock()
	if len(corrs) != 1 {
		t.Fatalf("a retry must be a replay: %d distinct correlations", len(corrs))
	}
}

// ── changing attributes ──────────────────────────────────────────────────────

func TestProvisioning_PatchChangesAccessOnlyThroughTheMapping(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	tg := r.target(t, "t-1", r.conn)
	out := r.create(t, tg, "dir-1", "alice@corp.example", "AP-Clerks")
	id := out.View.Link.LinkID

	// joining a mapped group plans that role; joining an unmapped one does nothing
	_, err := r.prov.PatchUser(ctx, tg, id, req("PATCH", "/Users/"+id, `{"Operations":[{"op":"add","path":"groups","value":[{"value":"Treasury-Admins"}]}]}`, ""))
	if err != nil {
		t.Fatal(err)
	}
	if n := r.ac.nRequests(); n != 1 {
		t.Fatalf("an unmapped group requests nothing: %d", n)
	}
	_, err = r.prov.PatchUser(ctx, tg, id, req("PATCH", "/Users/"+id, `{"Operations":[{"op":"add","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department","value":"Finance"}]}`, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.assignments(t, id); got[pRoleSecond] != federation.AsgProvisioned || got[pRoleBaseline] != federation.AsgProvisioned {
		t.Fatalf("%v", got)
	}
	// leaving the group that entitled a role revokes it
	_, err = r.prov.PatchUser(ctx, tg, id, req("PATCH", "/Users/"+id, `{"Operations":[{"op":"remove","path":"groups","value":[{"value":"AP-Clerks"}]}]}`, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.assignments(t, id); got[pRoleBaseline] != federation.AsgRevoked || got[pRoleSecond] != federation.AsgProvisioned {
		t.Fatalf("%v", got)
	}
	if r.ac.nRevokes() != 1 {
		t.Fatalf("the revoked role was revoked at access control: %d", r.ac.nRevokes())
	}
	// an unsupported operation changes nothing and is a 400
	before := r.count(t, `SELECT count(*) FROM federation_provisioning_events`)
	_, err = r.prov.PatchUser(ctx, tg, id, req("PATCH", "/Users/"+id, `{"Operations":[{"op":"add","path":"role_definition_id","value":"x"}]}`, ""))
	var se *federation.SCIMError
	if !errors.As(err, &se) || se.Status != 400 {
		t.Fatalf("%v", err)
	}
	if r.count(t, `SELECT count(*) FROM federation_provisioning_events`) != before {
		t.Fatal("a malformed instruction is not a provisioning event")
	}
	// an unknown user
	if _, err := r.prov.PatchUser(ctx, tg, "nope", req("PATCH", "/Users/nope", `{"Operations":[{"op":"replace","path":"active","value":false}]}`, "")); !errors.As(err, &se) || se.Status != 404 {
		t.Fatalf("%v", err)
	}
}

func TestProvisioning_ChangingTheEntitlingAttributeDoesNotChurnTheRole(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	cfg := federation.MappingConfig{Entries: []federation.MappingEntry{
		{Attribute: "groups", Value: "AP-Clerks", RoleDefinitionID: pRoleBaseline, LegalEntityID: pEntity},
		{Attribute: "department", Value: "Finance", RoleDefinitionID: pRoleBaseline, LegalEntityID: pEntity},
	}}
	r.mapping(t, "t-1", r.conn, cfg)
	tg := r.target(t, "t-1", r.conn)
	out := r.create(t, tg, "dir-1", "alice@corp.example", "AP-Clerks")
	id := out.View.Link.LinkID
	reqs := r.ac.nRequests()
	// she moves from the group to the department: same role, different reason
	_, err := r.prov.ReplaceUser(ctx, tg, id, req("PUT", "/Users/"+id,
		`{"userName":"alice@corp.example","externalId":"dir-1","emails":[{"value":"alice@corp.example"}],"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":{"department":"Finance"}}`, ""))
	if err != nil {
		t.Fatal(err)
	}
	if r.ac.nRequests() != reqs || r.ac.nRevokes() != 0 {
		t.Fatal("the role is neither revoked nor regranted")
	}
}

// ── deprovisioning ───────────────────────────────────────────────────────────

func TestProvisioning_DeprovisionEndsAccessFirstAndAsksQuestionsAfterwards(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	tg := r.target(t, "t-1", r.conn)
	out := r.create(t, tg, "dir-1", "alice@corp.example", "AP-Clerks")
	link := out.View.Link

	// access control is DOWN when the directory deprovisions her
	r.ac.set(func() { r.ac.unavailable = true })
	dep, err := r.prov.PatchUser(ctx, tg, link.LinkID, req("PATCH", "/Users/"+link.LinkID, `{"Operations":[{"op":"replace","path":"active","value":false}]}`, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !dep.Result.Deprovisioned || dep.View.Link.Status != federation.LinkDeprovisioned {
		t.Fatalf("%+v", dep.Result)
	}
	if r.principalStatus(t, link.PrincipalID) != "DISABLED" {
		t.Fatal("the principal is disabled in the same transaction: she cannot sign in, whatever else is down")
	}
	if r.sess.n() != 1 || r.count(t, `SELECT count(*) FROM federation_links WHERE link_id=$1 AND sessions_evicted_at IS NOT NULL`, link.LinkID) != 1 {
		t.Fatalf("her live sessions were ended: %d", r.sess.n())
	}
	if got := r.assignments(t, link.LinkID); got[pRoleBaseline] != federation.AsgRevokePending {
		t.Fatalf("the revocation is queued durably: %v", got)
	}
	if r.count(t, `SELECT count(*) FROM federation_links WHERE link_id=$1 AND access_revoked_at IS NOT NULL`, link.LinkID) != 0 {
		t.Fatal("not complete until access control has revoked")
	}
	if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.federation.deprovision_applied'`) != 1 {
		t.Fatal("deprovision_applied is announced")
	}
	if r.count(t, `SELECT count(*) FROM access_decision_log WHERE principal_id=$1 AND action_type='FEDERATION_DEPROVISION'`, link.PrincipalID) != 1 {
		t.Fatal("the decision is evidence")
	}
	// her federated login is refused even though the connection is healthy
	if l, _ := r.s.FindLink(ctx, "t-1", "https://idp.example.com", "dir-1"); l == nil || l.Status != federation.LinkDeprovisioned {
		t.Fatalf("%+v", l)
	}

	// a repeat is idempotent
	again, err := r.prov.DeleteUser(ctx, tg, link.LinkID, req("DELETE", "/Users/"+link.LinkID, "", ""))
	if err != nil || !again.Result.Unchanged {
		t.Fatalf("%+v %v", again, err)
	}
	if r.sess.n() != 1 {
		t.Fatalf("no second eviction for a repeat: %d", r.sess.n())
	}

	// access control comes back: the worker finishes the job
	r.ac.set(func() { r.ac.unavailable = false })
	time.Sleep(10 * time.Millisecond)
	if _, err := r.prov.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.assignments(t, link.LinkID); got[pRoleBaseline] != federation.AsgRevoked || r.ac.nRevokes() != 1 {
		t.Fatalf("%v revokes=%d", got, r.ac.nRevokes())
	}
	if r.count(t, `SELECT count(*) FROM federation_links WHERE link_id=$1 AND access_revoked_at IS NOT NULL`, link.LinkID) != 1 {
		t.Fatal("complete once every assignment is revoked")
	}
	lat, err := r.prov.Latency(ctx, "t-1", r.conn, 10)
	if err != nil || lat.Completed != 1 || lat.Pending != 0 || len(lat.Rows) != 1 || lat.Rows[0].LocalMillis == nil || lat.Rows[0].CompleteMillis == nil {
		t.Fatalf("%+v %v", lat, err)
	}
}

func TestProvisioning_SessionEvictionFailureIsRetriedAndEscalated(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	tg := r.target(t, "t-1", r.conn)
	out := r.create(t, tg, "dir-1", "alice@corp.example")
	r.sess.mu.Lock()
	r.sess.fail = true
	r.sess.mu.Unlock()
	if _, err := r.prov.DeleteUser(ctx, tg, out.View.Link.LinkID, req("DELETE", "/x", "", "")); err != nil {
		t.Fatal(err)
	}
	if r.principalStatus(t, out.View.Link.PrincipalID) != "DISABLED" {
		t.Fatal("she cannot sign in even though her sessions could not be ended yet")
	}
	if r.count(t, `SELECT count(*) FROM federation_links WHERE sessions_evicted_at IS NOT NULL AND link_id=$1`, out.View.Link.LinkID) != 0 {
		t.Fatal("not marked evicted")
	}
	// the worker keeps trying
	before := r.sess.n()
	_, _ = r.prov.Sweep(ctx)
	if r.sess.n() <= before {
		t.Fatal("the worker retries the eviction")
	}
	// ...and past the objective it is an incident, raised exactly once
	r.prov.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	res, err := r.prov.Sweep(ctx)
	if err != nil || res.Escalations != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if res, _ = r.prov.Sweep(ctx); res.Escalations != 0 {
		t.Fatal("escalated once")
	}
	ex, _ := r.prov.Exceptions(ctx, "t-1", r.conn, "OPEN", 10)
	if len(ex) != 1 || ex[0].Kind != federation.ExcDeprovisionLate || !strings.Contains(ex[0].Detail, "sessions are still live") {
		t.Fatalf("%+v", ex)
	}
	if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.federation.deprovision_escalated'`) != 1 {
		t.Fatal("the escalation is announced")
	}
	lat, _ := r.prov.Latency(ctx, "t-1", r.conn, 10)
	if lat.Escalated != 1 || lat.Pending != 1 {
		t.Fatalf("%+v", lat)
	}
	// when redis returns, the eviction completes
	r.sess.mu.Lock()
	r.sess.fail = false
	r.sess.mu.Unlock()
	r.prov.SetClock(time.Now)
	_, _ = r.prov.Sweep(ctx)
	if r.count(t, `SELECT count(*) FROM federation_links WHERE sessions_evicted_at IS NOT NULL AND link_id=$1`, out.View.Link.LinkID) != 1 {
		t.Fatal("recovered")
	}
}

func TestProvisioning_ReactivationWaitsForTheEarlierDeprovisioning(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	tg := r.target(t, "t-1", r.conn)
	out := r.create(t, tg, "dir-1", "alice@corp.example", "AP-Clerks")
	id := out.View.Link.LinkID
	r.ac.set(func() { r.ac.unavailable = true })
	if _, err := r.prov.DeleteUser(ctx, tg, id, req("DELETE", "/x", "", "")); err != nil {
		t.Fatal(err)
	}
	// the directory re-enables her while a revocation is still owed: that would resurrect access, so it waits
	react := `{"Operations":[{"op":"replace","path":"active","value":true}]}`
	_, err := r.prov.PatchUser(ctx, tg, id, req("PATCH", "/x", react, "re-1"))
	var se *federation.SCIMError
	if !errors.As(err, &se) || se.Status != 409 {
		t.Fatalf("%v", err)
	}
	if r.principalStatus(t, out.View.Link.PrincipalID) != "DISABLED" {
		t.Fatal("still disabled")
	}
	r.ac.set(func() { r.ac.unavailable = false })
	time.Sleep(10 * time.Millisecond)
	_, _ = r.prov.Sweep(ctx)
	// the same event, retried now that the first deprovisioning is complete
	got, err := r.prov.PatchUser(ctx, tg, id, req("PATCH", "/x", react, "re-1"))
	if err != nil || !got.Result.Reactivated {
		t.Fatalf("%+v %v", got, err)
	}
	if r.principalStatus(t, out.View.Link.PrincipalID) != "ACTIVE" || got.View.Link.Status != federation.LinkActive {
		t.Fatal("reactivated")
	}
	if a := r.assignments(t, id); a[pRoleBaseline] != federation.AsgProvisioned {
		// her group membership still stands in the directory, so the baseline role is requested afresh
		t.Fatalf("%v", a)
	}
}

func TestProvisioning_SuspendedConnectionStillTakesAccessAway(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	tg := r.target(t, "t-1", r.conn)
	out := r.create(t, tg, "dir-1", "alice@corp.example", "AP-Clerks")
	_, _ = r.s.SetStatus(ctx, "t-1", r.conn, federation.ConnSuspended, "op", "incident", "corr")
	tg2, err := r.prov.AuthenticateSCIM(ctx, "t-1", r.conn, r.token(t, "t-1", r.conn))
	if err != nil {
		t.Fatal(err)
	}
	var se *federation.SCIMError
	if _, err := r.prov.CreateUser(ctx, tg2, req("POST", "/Users", userJSON("dir-2", "bob@corp.example", true, "AP-Clerks"), "")); !errors.As(err, &se) || se.Status != 403 {
		t.Fatalf("a suspended connection grants nothing new: %v", err)
	}
	if _, err := r.prov.PatchUser(ctx, tg2, out.View.Link.LinkID, req("PATCH", "/x", `{"Operations":[{"op":"replace","path":"active","value":false}]}`, "")); err != nil {
		t.Fatalf("but it always takes access away: %v", err)
	}
	if r.principalStatus(t, out.View.Link.PrincipalID) != "DISABLED" {
		t.Fatal("disabled")
	}
}

// ── reconciliation ───────────────────────────────────────────────────────────

func TestProvisioning_Reconcile(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	tg := r.target(t, "t-1", r.conn)
	var ids []string
	for i := 1; i <= 12; i++ {
		o := r.create(t, tg, fmt.Sprintf("dir-%d", i), fmt.Sprintf("u%d@corp.example", i))
		ids = append(ids, o.View.Link.PrincipalID)
	}
	all := func(skip ...int) []string {
		var s []string
		for i := 1; i <= 12; i++ {
			drop := false
			for _, k := range skip {
				drop = drop || k == i
			}
			if !drop {
				s = append(s, fmt.Sprintf("dir-%d", i))
			}
		}
		return s
	}

	// an incomplete snapshot reports and changes nothing
	rep, err := r.prov.Reconcile(ctx, "t-1", "admin", "rc-1", r.conn, federation.ReconcileRequest{Subjects: all(3), Complete: false})
	if err != nil || rep.Applied || rep.WouldDeprovision != 1 || rep.Deprovisioned != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if r.principalStatus(t, ids[2]) != "ACTIVE" {
		t.Fatal("untouched")
	}
	// an empty snapshot would switch everyone off: refused
	if _, err := r.prov.Reconcile(ctx, "t-1", "admin", "rc-2", r.conn, federation.ReconcileRequest{Subjects: nil, Complete: true}); !errors.Is(err, federation.ErrInvalid) {
		t.Fatalf("%v", err)
	}
	// so would an implausibly destructive one
	if _, err := r.prov.Reconcile(ctx, "t-1", "admin", "rc-3", r.conn, federation.ReconcileRequest{Subjects: all(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11), Complete: true}); !errors.Is(err, federation.ErrConflict) {
		t.Fatalf("a broken export must not be able to deprovision a tenant: %v", err)
	}
	for i := 0; i < 12; i++ {
		if r.principalStatus(t, ids[i]) != "ACTIVE" {
			t.Fatal("nobody was deprovisioned")
		}
	}
	// a sane complete snapshot deprovisions the one person the directory dropped, and reports who it did not know
	snap := append(all(3), "dir-stranger")
	rep, err = r.prov.Reconcile(ctx, "t-1", "admin", "rc-4", r.conn, federation.ReconcileRequest{Subjects: snap, Complete: true})
	if err != nil || !rep.Applied || rep.Deprovisioned != 1 || rep.UnknownCount != 1 || rep.UnknownSubjects[0] != "dir-stranger" {
		t.Fatalf("%+v %v", rep, err)
	}
	if r.principalStatus(t, ids[2]) != "DISABLED" || r.principalStatus(t, ids[0]) != "ACTIVE" {
		t.Fatal("only the missing person")
	}
	if _, err := r.prov.Reconcile(ctx, "t-1", "admin", "rc-4", r.conn, federation.ReconcileRequest{Subjects: snap, Complete: true}); !errors.Is(err, federation.ErrConflict) {
		t.Fatalf("a reconciliation is run once: %v", err)
	}
	if r.count(t, `SELECT count(*) FROM federation_provisioning_events WHERE source='RECONCILE' AND status='RECONCILED'`) < 2 {
		t.Fatal("reconciliations are in the ledger")
	}
	// a malformed snapshot is refused
	if _, err := r.prov.Reconcile(ctx, "t-1", "admin", "rc-5", r.conn, federation.ReconcileRequest{Subjects: []string{"has space"}, Complete: true}); !errors.Is(err, federation.ErrInvalid) {
		t.Fatalf("%v", err)
	}
}

// ── just in time ─────────────────────────────────────────────────────────────

func TestProvisioning_JustInTime(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	d, _ := r.s.GetConnection(ctx, "t-1", r.conn)
	conn, ver := d.Connection, d.Versions[0]
	v := federation.Verified{Issuer: ver.Config.Issuer, Subject: "jit-1", TokenHash: "tokhash-1", Email: "jit@corp.example", DisplayName: "Jit One",
		Attrs: federation.Attributes{"groups": {"AP-Clerks", "Admins"}}}
	l, err := r.prov.JustInTime(ctx, conn, ver, v, "corr")
	if err != nil || l.Source != federation.SourceJIT || l.Status != federation.LinkActive {
		t.Fatalf("%+v %v", l, err)
	}
	if got := r.assignments(t, l.LinkID); len(got) != 1 || got[pRoleBaseline] != federation.AsgProvisioned {
		t.Fatalf("JIT is held to the same mapping: %v", got)
	}
	// the same token again is the same event
	again, err := r.prov.JustInTime(ctx, conn, ver, v, "corr")
	if err != nil || again.LinkID != l.LinkID {
		t.Fatalf("%+v %v", again, err)
	}
	// an unusable email is refused, and an email held by another principal is quarantined
	if _, err := r.prov.JustInTime(ctx, conn, ver, federation.Verified{Subject: "jit-2", TokenHash: "t2", Email: ""}, "c"); err == nil {
		t.Fatal("no email, no principal")
	}
	if _, err := r.prov.JustInTime(ctx, conn, ver, federation.Verified{Subject: "jit-3", TokenHash: "t3", Email: "JIT@corp.example"}, "c"); err == nil {
		t.Fatal("a collision is quarantined, not merged")
	}
}

// ── queries and isolation ────────────────────────────────────────────────────

func TestProvisioning_StatusAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	tg := r.target(t, "t-1", r.conn)
	a := r.create(t, tg, "dir-1", "alice@corp.example", "AP-Clerks")
	r.create(t, tg, "dir-2", "bob@corp.example")
	_, _ = r.prov.CreateUser(ctx, tg, req("POST", "/Users", userJSON("dir-3", "alice@corp.example", true), "dup"))
	st, err := r.prov.Status(ctx, "t-1", r.conn)
	if err != nil || st.Links["ACTIVE"] != 2 || st.Events["APPLIED"] != 2 || st.Events["REJECTED"] != 1 || st.OpenExceptions != 1 || st.Assignments["PROVISIONED"] != 1 || st.ActiveMapping == nil {
		t.Fatalf("%+v %v", st, err)
	}
	evs, _ := r.prov.Events(ctx, "t-1", r.conn, "REJECTED", 10)
	if len(evs) != 1 {
		t.Fatalf("%+v", evs)
	}

	// another tenant reaches none of it, by any door
	if _, err := r.prov.Status(ctx, "t-2", r.conn); !errors.Is(err, federation.ErrNotFound) {
		t.Fatalf("status: %v", err)
	}
	if evs, _ := r.prov.Events(ctx, "t-2", r.conn, "", 10); len(evs) != 0 {
		t.Fatalf("events: %+v", evs)
	}
	if ex, _ := r.prov.Exceptions(ctx, "t-2", r.conn, "", 10); len(ex) != 0 {
		t.Fatalf("exceptions: %+v", ex)
	}
	if _, err := r.s.GetUser(ctx, "t-2", r.conn, a.View.Link.LinkID); !errors.Is(err, federation.ErrUserNotFound) {
		t.Fatalf("user: %v", err)
	}
	if _, err := r.prov.GetMapping(ctx, "t-2", r.conn); !errors.Is(err, federation.ErrNotFound) {
		t.Fatalf("mapping: %v", err)
	}
	if _, err := r.prov.IssueSCIMToken(ctx, "t-2", "admin", "c", r.conn); !errors.Is(err, federation.ErrNotFound) {
		t.Fatalf("token: %v", err)
	}
	if _, err := r.prov.ResolveException(ctx, "t-2", "admin", "x", "note"); !errors.Is(err, federation.ErrNotFound) {
		t.Fatalf("resolve: %v", err)
	}
	// the ordinary role, unscoped, reads nothing from any provisioning table (negative control: the admin does)
	for _, tbl := range []string{"federation_scim_tokens", "federation_mapping_versions", "federation_provisioning_events", "federation_assignments", "federation_sync_exceptions"} {
		var asApp, asAdmin int
		_ = r.app.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&asApp)
		_ = r.admin.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&asAdmin)
		if asApp != 0 || asAdmin == 0 {
			t.Errorf("%s: unscoped app=%d admin=%d", tbl, asApp, asAdmin)
		}
	}
	// the worker's cross-tenant scan is a named scope: without it the same read is empty
	var n int
	_ = r.app.QueryRow(ctx, `SELECT count(*) FROM federation_assignments WHERE status = 'PROVISIONED'`).Scan(&n)
	if n != 0 {
		t.Fatalf("the worker scope is not ambient: %d", n)
	}
	for _, q := range []string{`UPDATE federation_provisioning_events SET payload_hash = 'x'`, `DELETE FROM federation_provisioning_events`, `UPDATE federation_provisioning_events SET status='APPLIED', reason='tampered' WHERE status='REJECTED'`} {
		if _, err := r.admin.Exec(ctx, q); err == nil {
			t.Errorf("%s: the ledger is append-only and a decision is final", q)
		}
	}
}

func TestProvisioning_WorkerScansAcrossTenantsAndSettlesEachInItsOwn(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	r.active(t, "t-2", "c-t2", "https://idp.example.com")
	r.mapping(t, "t-2", "c-t2", baselineMapping())
	r.ac.set(func() { r.ac.unavailable = true })
	a := r.create(t, r.target(t, "t-1", r.conn), "dir-1", "a@corp.example", "AP-Clerks")
	b := r.create(t, r.target(t, "t-2", "c-t2"), "dir-1", "b@corp.example", "AP-Clerks")
	r.ac.set(func() { r.ac.unavailable = false })
	time.Sleep(10 * time.Millisecond)
	res, err := r.prov.Sweep(ctx)
	if err != nil || res.Assignments != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, l := range []federation.Link{a.View.Link, b.View.Link} {
		if got := r.assignments(t, l.LinkID); got[pRoleBaseline] != federation.AsgProvisioned {
			t.Fatalf("%s: %v", l.TenantID, got)
		}
	}
	r.ac.mu.Lock()
	tenants := map[string]string{}
	for _, q := range r.ac.requests {
		tenants[q.TargetPrincipalID] = q.TenantID
	}
	r.ac.mu.Unlock()
	if tenants[a.View.Link.PrincipalID] != "t-1" || tenants[b.View.Link.PrincipalID] != "t-2" {
		t.Fatalf("each request is made under its own tenant: %v", tenants)
	}
}

func TestProvisioning_ConcurrentSweepsDoNotDoubleProcess(t *testing.T) {
	ctx := context.Background()
	r := newProvRig(t)
	r.ac.set(func() { r.ac.unavailable = true })
	tg := r.target(t, "t-1", r.conn)
	for i := 0; i < 6; i++ {
		r.create(t, tg, fmt.Sprintf("dir-%d", i), fmt.Sprintf("u%d@corp.example", i), "AP-Clerks")
	}
	r.ac.set(func() { r.ac.unavailable = false; r.ac.requests = nil })
	time.Sleep(10 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.prov.Sweep(ctx) }()
	}
	wg.Wait()
	if n := r.ac.nRequests(); n != 6 {
		t.Fatalf("each queued assignment is leased to one worker: %d requests for 6 assignments", n)
	}
}
