package store_test

// INT-10 federation persistence, against Postgres as an ORDINARY role (NOSUPERUSER, NOBYPASSRLS). The store
// under test is built on that role's pool, so every assertion about tenant isolation runs through the policy.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/federation"
	"zoiko.io/identity-context-svc/internal/store"
)

type fedRig struct {
	s     *store.PgStore
	admin *pgxpool.Pool
	app   *pgxpool.Pool
}

func newFedRig(t *testing.T) *fedRig {
	t.Helper()
	admin := openTestPool(t)
	app := ordinaryRolePool(t, admin)
	ctx := context.Background()
	insertPrincipal(t, ctx, admin, "p-1", "t-1", "fed|p1", "ACTIVE")
	insertPrincipal(t, ctx, admin, "p-2", "t-1", "fed|p2", "ACTIVE")
	insertPrincipal(t, ctx, admin, "p-9", "t-2", "fed|p9", "ACTIVE")
	return &fedRig{s: store.New(app, zap.NewNop()), admin: admin, app: app}
}

func fedCfg(issuer string) federation.Config {
	return federation.Config{Issuer: issuer, Audience: "zoiko", JWKSURI: "https://idp.example.com/jwks",
		AllowedAlgs: []string{"RS256"}}.Normalize()
}

func (r *fedRig) propose(t *testing.T, tenant, id, issuer, maker string) federation.Version {
	t.Helper()
	cfg := fedCfg(issuer)
	v := federation.Version{ConnectionID: id, TenantID: tenant, Config: cfg, Fingerprint: cfg.Fingerprint(), CreatedBy: maker}
	err := r.s.CreateConnection(context.Background(),
		federation.Connection{ConnectionID: id, TenantID: tenant, Name: "conn " + id, CreatedBy: maker}, v, maker, "corr")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return v
}

func (r *fedRig) approve(t *testing.T, tenant, id string, version int, fp, checker string) (*federation.ConnectionDetail, error) {
	t.Helper()
	return r.s.DecideVersion(context.Background(), tenant, id, version, true, checker, fp, "ok", "corr")
}

func (r *fedRig) active(t *testing.T, tenant, id, issuer string) {
	t.Helper()
	v := r.propose(t, tenant, id, issuer, "maker")
	if _, err := r.approve(t, tenant, id, 1, v.Fingerprint, "checker"); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

func (r *fedRig) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := r.admin.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFederation_LifecycleMakerChecker(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	v := r.propose(t, "t-1", "c-1", "https://idp.example.com", "maker")

	// pending: authenticates nobody
	if c, _, err := r.s.FindLoginConnection(ctx, "t-1", "https://idp.example.com"); err != nil || c != nil {
		t.Fatalf("a connection awaiting approval must not be found for login: %+v %v", c, err)
	}
	d, err := r.s.GetConnection(ctx, "t-1", "c-1")
	if err != nil || d.Status != federation.ConnPendingApproval || d.ActiveVersion != nil || len(d.Versions) != 1 {
		t.Fatalf("%+v %v", d, err)
	}

	// the author cannot approve their own version
	if _, err := r.approve(t, "t-1", "c-1", 1, v.Fingerprint, "maker"); !errors.Is(err, federation.ErrSelfApproval) {
		t.Fatalf("self approval: %v", err)
	}
	// a fingerprint other than the one proposed (the approver saw something else) is refused, as is none
	for _, fp := range []string{"", "deadbeef", fedCfg("https://other.example.com").Fingerprint()} {
		if _, err := r.approve(t, "t-1", "c-1", 1, fp, "checker"); !errors.Is(err, federation.ErrFingerprint) {
			t.Fatalf("fingerprint %q: %v", fp, err)
		}
	}
	d, err = r.approve(t, "t-1", "c-1", 1, v.Fingerprint, "checker")
	if err != nil || d.Status != federation.ConnActive || *d.ActiveVersion != 1 || d.Versions[0].Status != federation.VerApproved || d.Versions[0].DecidedBy != "checker" {
		t.Fatalf("%+v %v", d, err)
	}
	c, ver, err := r.s.FindLoginConnection(ctx, "t-1", "https://idp.example.com")
	if err != nil || c == nil || ver.Config.Audience != "zoiko" || ver.Version != 1 {
		t.Fatalf("an approved connection is found for login: %+v %+v %v", c, ver, err)
	}
	// a decision is recorded once
	if _, err := r.approve(t, "t-1", "c-1", 1, v.Fingerprint, "checker2"); !errors.Is(err, federation.ErrNotPending) {
		t.Fatalf("decided twice: %v", err)
	}

	// evidence and events, in the same transaction as the change
	if n := r.count(t, `SELECT count(*) FROM federation_connection_events WHERE connection_id='c-1'`); n != 2 {
		t.Fatalf("trail rows: %d", n)
	}
	if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.federation.version_proposed' AND tenant_id='t-1'`) != 1 ||
		r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.federation.activated' AND tenant_id='t-1'`) != 1 {
		t.Fatal("federation_activated is enqueued with the approval")
	}
}

func TestFederation_VersionsAreImmutableAndEvidenceAppendOnly(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	r.active(t, "t-1", "c-1", "https://idp.example.com")

	// even the table owner / superuser cannot rewrite what was approved
	for _, q := range []string{
		`UPDATE federation_connection_versions SET issuer = 'https://evil.example.com' WHERE connection_id = 'c-1'`,
		`UPDATE federation_connection_versions SET jwks_uri = 'https://evil.example.com/jwks' WHERE connection_id = 'c-1'`,
		`UPDATE federation_connection_versions SET fingerprint = repeat('a',64) WHERE connection_id = 'c-1'`,
		`UPDATE federation_connection_versions SET status = 'PENDING_APPROVAL' WHERE connection_id = 'c-1'`,
		`UPDATE federation_connection_versions SET decided_by = 'someone' WHERE connection_id = 'c-1'`,
		`DELETE FROM federation_connection_versions WHERE connection_id = 'c-1'`,
		`DELETE FROM federation_connection_events WHERE connection_id = 'c-1'`,
		`UPDATE federation_connection_events SET actor_id = 'x' WHERE connection_id = 'c-1'`,
	} {
		for who, pool := range map[string]*pgxpool.Pool{"admin": r.admin, "app": r.app} {
			if who == "app" {
				// the app role is tenant scoped: scope it, so the trigger (not RLS) is what refuses
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id','t-1',true)`)
				_, err = tx.Exec(ctx, q)
				_ = tx.Rollback(ctx)
				if err == nil {
					t.Errorf("app role: %s must be refused", q)
				}
				continue
			}
			if _, err := pool.Exec(ctx, q); err == nil {
				t.Errorf("admin: %s must be refused", q)
			}
		}
	}
	// the data refuses a self-decided version even when the service is bypassed
	r.propose(t, "t-1", "c-2", "https://idp2.example.com", "maker")
	if _, err := r.admin.Exec(ctx, `UPDATE federation_connection_versions SET status='APPROVED', decided_by='maker',
		approved_fingerprint=fingerprint WHERE connection_id='c-2'`); err == nil {
		t.Fatal("maker-checker must hold in the data")
	}
	if _, err := r.admin.Exec(ctx, `UPDATE federation_connection_versions SET status='APPROVED', decided_by='other',
		approved_fingerprint=repeat('b',64) WHERE connection_id='c-2'`); err == nil {
		t.Fatal("an approval must record exactly the fingerprint it approved")
	}
	// an active connection must have an approved version behind it
	if _, err := r.admin.Exec(ctx, `UPDATE federation_connections SET status='ACTIVE', active_version=NULL WHERE connection_id='c-2'`); err == nil {
		t.Fatal("a connection that can authenticate needs an approved version")
	}
}

func TestFederation_ChangingAnActiveConnectionNeverInterruptsIt(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	r.active(t, "t-1", "c-1", "https://idp.example.com")

	// propose a change of trust (a different audience) while v1 serves
	cfg2 := fedCfg("https://idp.example.com")
	cfg2.Audience = "changed-client"
	v2, err := r.s.AddVersion(ctx, "t-1", "c-1", federation.Version{Config: cfg2, Fingerprint: cfg2.Fingerprint(), CreatedBy: "maker"}, "maker", "corr")
	if err != nil || v2.Version != 2 || v2.Status != federation.VerPending {
		t.Fatalf("%+v %v", v2, err)
	}
	// still serving v1's configuration: "mapping change after approval" does not take effect unapproved
	_, ver, _ := r.s.FindLoginConnection(ctx, "t-1", "https://idp.example.com")
	if ver == nil || ver.Version != 1 || ver.Config.Audience != "zoiko" {
		t.Fatalf("the approved version keeps serving: %+v", ver)
	}
	// one proposal at a time
	if _, err := r.s.AddVersion(ctx, "t-1", "c-1", federation.Version{Config: cfg2, Fingerprint: cfg2.Fingerprint(), CreatedBy: "maker"}, "maker", "corr"); !errors.Is(err, federation.ErrConflict) {
		t.Fatalf("second pending: %v", err)
	}
	// rejecting it changes nothing
	if _, err := r.s.DecideVersion(ctx, "t-1", "c-1", 2, false, "checker", "", "no", "corr"); err != nil {
		t.Fatal(err)
	}
	if _, ver, _ = r.s.FindLoginConnection(ctx, "t-1", "https://idp.example.com"); ver.Version != 1 {
		t.Fatalf("after a rejection: %+v", ver)
	}
	// propose again and approve: v1 is superseded, v2 serves
	v3, err := r.s.AddVersion(ctx, "t-1", "c-1", federation.Version{Config: cfg2, Fingerprint: cfg2.Fingerprint(), CreatedBy: "maker"}, "maker", "corr")
	if err != nil || v3.Version != 3 {
		t.Fatalf("%+v %v", v3, err)
	}
	d, err := r.approve(t, "t-1", "c-1", 3, v3.Fingerprint, "checker")
	if err != nil || *d.ActiveVersion != 3 || d.Status != federation.ConnActive {
		t.Fatalf("%+v %v", d, err)
	}
	got := map[int]string{}
	for _, v := range d.Versions {
		got[v.Version] = v.Status
	}
	if got[1] != federation.VerSuperseded || got[2] != federation.VerRejected || got[3] != federation.VerApproved {
		t.Fatalf("version statuses: %v", got)
	}
	if _, ver, _ = r.s.FindLoginConnection(ctx, "t-1", "https://idp.example.com"); ver.Version != 3 || ver.Config.Audience != "changed-client" {
		t.Fatalf("%+v", ver)
	}
	// a later approval event, not another activation
	if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.federation.activated'`) != 1 ||
		r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.federation.version_approved'`) != 1 {
		t.Fatal("activation is announced once; later approvals are version approvals")
	}
}

func TestFederation_OneLiveConnectionPerIssuerPerTenant(t *testing.T) {
	r := newFedRig(t)
	r.active(t, "t-1", "c-1", "https://idp.example.com")
	v := r.propose(t, "t-1", "c-2", "https://idp.example.com", "maker")
	if _, err := r.approve(t, "t-1", "c-2", 1, v.Fingerprint, "checker"); !errors.Is(err, federation.ErrConflict) {
		t.Fatalf("a token's issuer must select exactly one configuration: %v", err)
	}
	// another tenant may trust the same issuer
	r.active(t, "t-2", "c-3", "https://idp.example.com")
}

func TestFederation_SuspendResumeRevoke(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	r.active(t, "t-1", "c-1", "https://idp.example.com")

	if _, err := r.s.SetStatus(ctx, "t-1", "c-1", federation.ConnActive, "op", "x", "corr"); !errors.Is(err, federation.ErrTransition) {
		t.Fatalf("resume of an active connection: %v", err)
	}
	c, err := r.s.SetStatus(ctx, "t-1", "c-1", federation.ConnSuspended, "op", "incident", "corr")
	if err != nil || c.Status != federation.ConnSuspended || c.StatusReason != "incident" {
		t.Fatalf("%+v %v", c, err)
	}
	if c, _, _ := r.s.FindLoginConnection(ctx, "t-1", "https://idp.example.com"); c != nil {
		t.Fatal("a suspended connection admits no login")
	}
	if _, err := r.s.SetStatus(ctx, "t-1", "c-1", federation.ConnSuspended, "op", "again", "corr"); !errors.Is(err, federation.ErrTransition) {
		t.Fatalf("suspend twice: %v", err)
	}
	if c, err = r.s.SetStatus(ctx, "t-1", "c-1", federation.ConnActive, "op", "cleared", "corr"); err != nil || c.Status != federation.ConnActive {
		t.Fatalf("%+v %v", c, err)
	}
	if c, _, _ := r.s.FindLoginConnection(ctx, "t-1", "https://idp.example.com"); c == nil {
		t.Fatal("resumed")
	}
	if _, err := r.s.SetStatus(ctx, "t-1", "c-1", federation.ConnRevoked, "op", "left", "corr"); err != nil {
		t.Fatal(err)
	}
	// revocation is final
	for _, to := range []string{federation.ConnActive, federation.ConnSuspended, federation.ConnRevoked} {
		if _, err := r.s.SetStatus(ctx, "t-1", "c-1", to, "op", "x", "corr"); !errors.Is(err, federation.ErrTransition) {
			t.Errorf("revoked -> %s: %v", to, err)
		}
	}
	cfg := fedCfg("https://idp.example.com")
	if _, err := r.s.AddVersion(ctx, "t-1", "c-1", federation.Version{Config: cfg, Fingerprint: cfg.Fingerprint(), CreatedBy: "m"}, "m", "corr"); !errors.Is(err, federation.ErrTransition) {
		t.Fatalf("a version on a revoked connection: %v", err)
	}
	if c, _, _ := r.s.FindLoginConnection(ctx, "t-1", "https://idp.example.com"); c != nil {
		t.Fatal("revoked")
	}
	for _, evt := range []string{"suspended", "resumed", "revoked"} {
		if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type=$1`, "identity.federation."+evt) != 1 {
			t.Errorf("event %s", evt)
		}
	}
}

func TestFederation_KeyHealthTransitionsOnce(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	r.active(t, "t-1", "c-1", "https://idp.example.com")
	for i := 0; i < 3; i++ {
		if err := r.s.SetKeyHealth(ctx, "t-1", "c-1", true, "keys unreachable"); err != nil {
			t.Fatal(err)
		}
	}
	d, _ := r.s.GetConnection(ctx, "t-1", "c-1")
	if d.Status != federation.ConnDegraded || d.StatusReason != "keys unreachable" {
		t.Fatalf("%+v", d.Connection)
	}
	if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.federation.degraded'`) != 1 {
		t.Fatal("federation_degraded is announced once, on the transition")
	}
	// a degraded connection still authenticates (it may be on cached keys)
	if c, _, _ := r.s.FindLoginConnection(ctx, "t-1", "https://idp.example.com"); c == nil {
		t.Fatal("degraded is not suspended")
	}
	_ = r.s.SetKeyHealth(ctx, "t-1", "c-1", false, "")
	_ = r.s.SetKeyHealth(ctx, "t-1", "c-1", false, "")
	if d, _ = r.s.GetConnection(ctx, "t-1", "c-1"); d.Status != federation.ConnActive {
		t.Fatalf("recovered: %+v", d.Connection)
	}
	if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.federation.recovered'`) != 1 {
		t.Fatal("recovery is announced once")
	}
	// health never overrides an operator's decision
	_, _ = r.s.SetStatus(ctx, "t-1", "c-1", federation.ConnSuspended, "op", "stop", "corr")
	_ = r.s.SetKeyHealth(ctx, "t-1", "c-1", false, "")
	_ = r.s.SetKeyHealth(ctx, "t-1", "c-1", true, "x")
	if d, _ = r.s.GetConnection(ctx, "t-1", "c-1"); d.Status != federation.ConnSuspended {
		t.Fatalf("a suspended connection stays suspended: %+v", d.Connection)
	}
}

func TestFederation_KeySets(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	r.active(t, "t-1", "c-1", "https://idp.example.com")
	if rec, err := r.s.LoadKeySet(ctx, "t-1", "c-1"); err != nil || rec != nil {
		t.Fatalf("none yet: %+v %v", rec, err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := r.s.SaveKeySet(ctx, federation.KeySetRecord{ConnectionID: "c-1", TenantID: "t-1", JWKSURI: "https://idp.example.com/jwks",
		JWKS: []byte(`{"keys":[]}`), KeyCount: 2, FetchedAt: at, LastAttemptAt: at}); err != nil {
		t.Fatal(err)
	}
	rec, _ := r.s.LoadKeySet(ctx, "t-1", "c-1")
	if rec == nil || rec.KeyCount != 2 || rec.JWKSURI != "https://idp.example.com/jwks" || !rec.FetchedAt.Equal(at) {
		t.Fatalf("%+v", rec)
	}
	_ = r.s.RecordKeyFetchFailure(ctx, "t-1", "c-1", "https://idp.example.com/jwks", "timeout", at.Add(time.Minute))
	rec, _ = r.s.LoadKeySet(ctx, "t-1", "c-1")
	if rec.ConsecutiveFailures != 1 || rec.LastError != "timeout" || !rec.FetchedAt.Equal(at) {
		t.Fatalf("a failure notes the attempt and leaves the keys and their age alone: %+v", rec)
	}
	// a fetch resets the failure count
	_ = r.s.SaveKeySet(ctx, federation.KeySetRecord{ConnectionID: "c-1", TenantID: "t-1", JWKSURI: "https://idp.example.com/jwks2",
		JWKS: []byte(`{"keys":[]}`), KeyCount: 1, FetchedAt: at.Add(time.Hour), LastAttemptAt: at.Add(time.Hour)})
	rec, _ = r.s.LoadKeySet(ctx, "t-1", "c-1")
	if rec.ConsecutiveFailures != 0 || rec.LastError != "" || rec.JWKSURI != "https://idp.example.com/jwks2" {
		t.Fatalf("%+v", rec)
	}
}

func TestFederation_Links(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	r.active(t, "t-1", "c-1", "https://idp.example.com")
	mk := func(subject, principal string) (*federation.Link, error) {
		return r.s.CreateLink(ctx, federation.Link{TenantID: "t-1", ConnectionID: "c-1", Subject: subject, PrincipalID: principal,
			Source: federation.SourceAdmin, LinkedBy: "admin"}, "admin", "corr")
	}
	l, err := mk("user-1", "p-1")
	if err != nil || l.Issuer != "https://idp.example.com" || l.Status != federation.LinkActive || l.LinkID == "" {
		t.Fatalf("%+v %v", l, err)
	}
	if _, err := mk("user-1", "p-2"); !errors.Is(err, federation.ErrLinkConflict) {
		t.Fatalf("one external identity, one principal: %v", err)
	}
	if _, err := mk("user-2", "p-1"); !errors.Is(err, federation.ErrLinkConflict) {
		t.Fatalf("one principal per issuer: %v", err)
	}
	if _, err := mk("user-3", "nobody"); !errors.Is(err, federation.ErrUnknownPrincipal) {
		t.Fatalf("unknown principal: %v", err)
	}
	if _, err := mk("user-4", "p-9"); !errors.Is(err, federation.ErrUnknownPrincipal) {
		t.Fatalf("another tenant's principal is not linkable: %v", err)
	}
	got, _ := r.s.FindLink(ctx, "t-1", "https://idp.example.com", "user-1")
	if got == nil || got.PrincipalID != "p-1" {
		t.Fatalf("%+v", got)
	}
	// keyed by tenant + issuer + subject: the same subject at another issuer is another identity
	if got, _ := r.s.FindLink(ctx, "t-1", "https://other.example.com", "user-1"); got != nil {
		t.Fatalf("%+v", got)
	}
	if got, _ := r.s.FindLink(ctx, "t-2", "https://idp.example.com", "user-1"); got != nil {
		t.Fatalf("another tenant sees nothing: %+v", got)
	}
	if list, _ := r.s.ListLinks(ctx, "t-1", "p-1", "", 10); len(list) != 1 {
		t.Fatalf("%+v", list)
	}
	if list, _ := r.s.ListLinks(ctx, "t-1", "p-2", "", 10); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
	if _, err := r.s.GetLink(ctx, "t-1", "missing"); !errors.Is(err, federation.ErrLinkNotFound) {
		t.Fatalf("%v", err)
	}
	if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.link.created'`) != 1 {
		t.Fatal("identity_link_created is announced")
	}
	// the email address is nowhere in the link
	var cols int
	_ = r.admin.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='federation_links' AND column_name LIKE '%mail%'`).Scan(&cols)
	if cols != 0 {
		t.Fatal("an email address is not part of an identity link")
	}
}

func TestFederation_LoginEvidenceAndSingleUseTokens(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	r.active(t, "t-1", "c-1", "https://idp.example.com")
	l, _ := r.s.CreateLink(ctx, federation.Link{TenantID: "t-1", ConnectionID: "c-1", Subject: "user-1", PrincipalID: "p-1",
		Source: federation.SourceAdmin, LinkedBy: "admin"}, "admin", "corr")

	rec := federation.LoginRecord{TenantID: "t-1", ConnectionID: "c-1", ConfigVersion: 1, Issuer: "https://idp.example.com",
		Subject: "user-1", PrincipalID: "p-1", Outcome: federation.OutcomeSucceeded, Reason: "login", KeyID: "k1", CorrelationID: "corr"}
	exp := time.Now().Add(5 * time.Minute)
	if err := r.s.CompleteLogin(ctx, rec, l.LinkID, "hash-1", exp); err != nil {
		t.Fatal(err)
	}
	if err := r.s.CompleteLogin(ctx, rec, l.LinkID, "hash-1", exp); !errors.Is(err, federation.ErrTokenReplayed) {
		t.Fatalf("replay: %v", err)
	}
	if r.count(t, `SELECT count(*) FROM federation_login_log`) != 1 || r.count(t, `SELECT count(*) FROM federation_token_uses`) != 1 {
		t.Fatal("a replay leaves no second login row")
	}
	if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.federation.login_succeeded'`) != 1 {
		t.Fatal("the success is announced exactly once")
	}
	got, _ := r.s.GetLink(ctx, "t-1", l.LinkID)
	if got.LastLoginAt == nil {
		t.Fatal("the link records its last login")
	}

	// a refused-but-authentic login is evidence too
	rec.Reason = "link_inactive"
	rec.Outcome = ""
	if err := r.s.RecordDenied(ctx, rec); err != nil {
		t.Fatal(err)
	}
	logins, _ := r.s.ListLogins(ctx, "t-1", "c-1", 10)
	if len(logins) != 2 || logins[0].Outcome != federation.OutcomeDenied || logins[1].Outcome != federation.OutcomeSucceeded {
		t.Fatalf("%+v", logins)
	}
	if err := r.admin.QueryRow(ctx, `SELECT 1`).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE federation_login_log SET reason='x'`, `DELETE FROM federation_login_log`} {
		if _, err := r.admin.Exec(ctx, q); err == nil {
			t.Errorf("%s: login evidence is append-only", q)
		}
	}
	// the data only records a login whose signature verified
	if _, err := r.admin.Exec(ctx, `INSERT INTO federation_login_log (log_id,tenant_id,connection_id,config_version,issuer,subject,outcome,reason,signature_check)
		VALUES ('x','t-1','c-1',1,'i','s','DENIED','r','INVALID')`); err == nil {
		t.Fatal("an unverified signature never becomes login evidence")
	}
}

func TestFederation_ConcurrentExchangeOfOneTokenSucceedsOnce(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	r.active(t, "t-1", "c-1", "https://idp.example.com")
	l, _ := r.s.CreateLink(ctx, federation.Link{TenantID: "t-1", ConnectionID: "c-1", Subject: "user-1", PrincipalID: "p-1",
		Source: federation.SourceAdmin, LinkedBy: "admin"}, "admin", "corr")
	rec := federation.LoginRecord{TenantID: "t-1", ConnectionID: "c-1", ConfigVersion: 1, Issuer: "https://idp.example.com",
		Subject: "user-1", PrincipalID: "p-1", Outcome: federation.OutcomeSucceeded, Reason: "login"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, replayed := 0, 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.s.CompleteLogin(ctx, rec, l.LinkID, "same-token", time.Now().Add(time.Minute))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, federation.ErrTokenReplayed):
				replayed++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || replayed != 11 {
		t.Fatalf("one exchange wins: ok=%d replayed=%d", ok, replayed)
	}
}

func TestFederation_ConcurrentApprovalsDecideOnce(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	v := r.propose(t, "t-1", "c-1", "https://idp.example.com", "maker")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.s.DecideVersion(ctx, "t-1", "c-1", 1, true, "checker", v.Fingerprint, "ok", "corr"); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("a version is approved once: %d", ok)
	}
	if r.count(t, `SELECT count(*) FROM event_outbox WHERE event_type='identity.federation.activated'`) != 1 {
		t.Fatal("one activation event")
	}
}

func TestFederation_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	r := newFedRig(t)
	r.active(t, "t-1", "c-1", "https://idp.example.com")
	l, _ := r.s.CreateLink(ctx, federation.Link{TenantID: "t-1", ConnectionID: "c-1", Subject: "user-1", PrincipalID: "p-1",
		Source: federation.SourceAdmin, LinkedBy: "admin"}, "admin", "corr")
	_ = r.s.SaveKeySet(ctx, federation.KeySetRecord{ConnectionID: "c-1", TenantID: "t-1", JWKSURI: "u", JWKS: []byte(`{}`), KeyCount: 1,
		FetchedAt: time.Now(), LastAttemptAt: time.Now()})

	// tenant t-2 cannot reach t-1's connection through any door
	if _, err := r.s.GetConnection(ctx, "t-2", "c-1"); !errors.Is(err, federation.ErrNotFound) {
		t.Fatalf("get: %v", err)
	}
	if list, _ := r.s.ListConnections(ctx, "t-2"); len(list) != 0 {
		t.Fatalf("list: %+v", list)
	}
	if c, _, _ := r.s.FindLoginConnection(ctx, "t-2", "https://idp.example.com"); c != nil {
		t.Fatal("login lookup")
	}
	if _, err := r.s.DecideVersion(ctx, "t-2", "c-1", 1, true, "x", "y", "z", "corr"); !errors.Is(err, federation.ErrNotFound) {
		t.Fatalf("decide: %v", err)
	}
	if _, err := r.s.SetStatus(ctx, "t-2", "c-1", federation.ConnRevoked, "x", "y", "corr"); !errors.Is(err, federation.ErrNotFound) {
		t.Fatalf("revoke: %v", err)
	}
	cfg := fedCfg("https://idp.example.com")
	if _, err := r.s.AddVersion(ctx, "t-2", "c-1", federation.Version{Config: cfg, Fingerprint: cfg.Fingerprint(), CreatedBy: "x"}, "x", "corr"); !errors.Is(err, federation.ErrNotFound) {
		t.Fatalf("add version: %v", err)
	}
	if _, err := r.s.CreateLink(ctx, federation.Link{TenantID: "t-2", ConnectionID: "c-1", Subject: "s", PrincipalID: "p-9",
		Source: federation.SourceAdmin, LinkedBy: "x"}, "x", "corr"); !errors.Is(err, federation.ErrNotFound) {
		t.Fatalf("link on a foreign connection: %v", err)
	}
	if _, err := r.s.GetLink(ctx, "t-2", l.LinkID); !errors.Is(err, federation.ErrLinkNotFound) {
		t.Fatalf("get link: %v", err)
	}
	if rec, _ := r.s.LoadKeySet(ctx, "t-2", "c-1"); rec != nil {
		t.Fatal("key set")
	}
	if logs, _ := r.s.ListLogins(ctx, "t-2", "c-1", 10); len(logs) != 0 {
		t.Fatal("logins")
	}

	// and an unscoped connection as the ordinary role reads nothing from any federation table (negative control:
	// the admin sees the rows)
	for _, tbl := range []string{"federation_connections", "federation_connection_versions", "federation_connection_events",
		"federation_key_sets", "federation_links", "federation_login_log", "federation_token_uses"} {
		var asApp, asAdmin int
		_ = r.app.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&asApp)
		_ = r.admin.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&asAdmin)
		if asApp != 0 {
			t.Errorf("%s: an unscoped ordinary connection read %d rows", tbl, asApp)
		}
		if tbl != "federation_login_log" && tbl != "federation_token_uses" && asAdmin == 0 {
			t.Errorf("%s: negative control — the admin should see rows", tbl)
		}
	}
	// a write for a tenant other than the scoped one is refused by the policy itself
	tx, _ := r.app.Begin(ctx)
	_, _ = tx.Exec(ctx, `SELECT set_config('app.tenant_id','t-2',true)`)
	_, err := tx.Exec(ctx, `INSERT INTO federation_connections (connection_id,tenant_id,name,created_by) VALUES ('x','t-1','n','m')`)
	_ = tx.Rollback(ctx)
	if err == nil {
		t.Fatal("RLS must refuse a cross-tenant write")
	}
}
