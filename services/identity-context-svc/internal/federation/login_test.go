package federation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/siem"
)

type fakePrincipals struct {
	byID map[string]*domain.Principal
	err  error
}

func (f *fakePrincipals) FindByID(_ context.Context, id, tenant string) (*domain.Principal, error) {
	if f.err != nil {
		return nil, f.err
	}
	p := f.byID[id]
	if p == nil || p.TenantID != tenant {
		return nil, nil
	}
	return p, nil
}

type fakeIssuer struct {
	mu     sync.Mutex
	issued []struct {
		subject, tenant string
		mfa             bool
	}
}

func (f *fakeIssuer) Issue(subject, tenant string, mfa bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued = append(f.issued, struct {
		subject, tenant string
		mfa             bool
	}{subject, tenant, mfa})
	return "bearer-for-" + subject, nil
}
func (f *fakeIssuer) TTLSeconds() int { return 300 }

type fakeAlerts struct {
	mu     sync.Mutex
	events []string
}

func (f *fakeAlerts) Stream(_ context.Context, _, eventType string, _ siem.Severity, _ string) {
	f.mu.Lock()
	f.events = append(f.events, eventType)
	f.mu.Unlock()
}

type loginRig struct {
	idp    *testIdP
	repo   *memRepo
	issuer *fakeIssuer
	alerts *fakeAlerts
	people *fakePrincipals
	login  *Login
}

func newLoginRig(t *testing.T) *loginRig {
	t.Helper()
	idp := newTestIdP(t)
	repo := newMemRepo()
	cfg := testConfig()
	repo.conn = ptr(testConnection())
	repo.ver = &Version{ConnectionID: tConn, TenantID: tTenant, Version: 1, Status: VerApproved, Config: cfg, Fingerprint: cfg.Fingerprint()}
	fetch := &fakeFetcher{doc: idp.jwks()}
	km := NewKeyManager(repo, fetch, repo, nil)
	rig := &loginRig{idp: idp, repo: repo, issuer: &fakeIssuer{}, alerts: &fakeAlerts{},
		people: &fakePrincipals{byID: map[string]*domain.Principal{
			"p-1": {PrincipalID: "p-1", TenantID: tTenant, IdentityProviderSubject: "federated:conn-1:user-123", Status: domain.PrincipalStatusActive},
		}}}
	rig.login = NewLogin(repo, NewVerifier(km), rig.people, rig.issuer, rig.alerts, nil)
	repo.links[tTenant+"|"+tIssuer+"|user-123"] = &Link{LinkID: "l-1", TenantID: tTenant, ConnectionID: tConn, Issuer: tIssuer,
		Subject: "user-123", PrincipalID: "p-1", Status: LinkActive}
	return rig
}

func ptr[T any](v T) *T { return &v }

func (r *loginRig) req(token string) LoginRequest {
	return LoginRequest{TenantID: tTenant, IDToken: token, Nonce: "n-1", CorrelationID: "corr-1"}
}

func TestLoginIssuesTheFirstPartyTokenForTheLinkedPrincipal(t *testing.T) {
	r := newLoginRig(t)
	resp, err := r.login.Authenticate(context.Background(), r.req(r.idp.mint(t, r.idp.claims())))
	if err != nil {
		t.Fatal(err)
	}
	if resp.PrincipalID != "p-1" || resp.TenantID != tTenant || resp.TokenType != "Bearer" || resp.MFARequired {
		t.Fatalf("%+v", resp)
	}
	if len(r.issuer.issued) != 1 || r.issuer.issued[0].subject != "federated:conn-1:user-123" || r.issuer.issued[0].mfa {
		t.Fatalf("the token asserts the principal's IdP subject, with no MFA claim: %+v", r.issuer.issued)
	}
	if len(r.repo.logins) != 1 || r.repo.logins[0].Outcome != OutcomeSucceeded || r.repo.logins[0].PrincipalID != "p-1" {
		t.Fatalf("evidence: %+v", r.repo.logins)
	}
}

func TestEmailCollisionAcrossIssuersCannotReachAnotherPrincipal(t *testing.T) {
	r := newLoginRig(t)
	// the SAME address is asserted by two issuers for two different people; the email plays no part
	c := r.idp.claims()
	c["email"] = "alice@corp.example"
	c["sub"] = "user-999" // an unlinked subject at this issuer, with the same email as p-1's person
	if _, err := r.login.Authenticate(context.Background(), r.req(r.idp.mint(t, c))); !errors.Is(err, ErrLoginRejected) {
		t.Fatalf("an unlinked subject is refused even if its email matches a linked person: %v", err)
	}
	if len(r.issuer.issued) != 0 {
		t.Fatal("no token for an unlinked subject")
	}
	// a link at ANOTHER issuer does not apply here
	r.repo.links[tTenant+"|https://other.example.com|user-999"] = &Link{LinkID: "l-2", TenantID: tTenant, Issuer: "https://other.example.com",
		Subject: "user-999", PrincipalID: "p-1", Status: LinkActive}
	if _, err := r.login.Authenticate(context.Background(), r.req(r.idp.mint(t, c))); !errors.Is(err, ErrLoginRejected) {
		t.Fatalf("a link is keyed by issuer too: %v", err)
	}
}

func TestEveryRefusalLooksTheSameToTheCaller(t *testing.T) {
	r := newLoginRig(t)
	bad := map[string]string{}
	c := r.idp.claims()
	c["iss"] = "https://evil.example.com"
	bad["unknown issuer"] = r.idp.mint(t, c)
	c = r.idp.claims()
	c["aud"] = "other"
	bad["wrong audience"] = r.idp.mint(t, c)
	c = r.idp.claims()
	c["nonce"] = "other"
	bad["wrong nonce"] = r.idp.mint(t, c)
	c = r.idp.claims()
	c["sub"] = "stranger"
	bad["no link"] = r.idp.mint(t, c)
	bad["garbage"] = "not-a-token"

	for name, tok := range bad {
		_, err := r.login.Authenticate(context.Background(), r.req(tok))
		if err != ErrLoginRejected { // the identical sentinel, not merely a wrapped one
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(r.issuer.issued) != 0 {
		t.Fatal("nothing was issued")
	}
}

func TestUnauthenticatedInputWritesNothingDurable(t *testing.T) {
	r := newLoginRig(t)
	c := r.idp.claims()
	c["iss"] = "https://evil.example.com"
	for _, tok := range []string{r.idp.mint(t, c), "garbage", strings.Repeat("a", 20000)} {
		_, _ = r.login.Authenticate(context.Background(), r.req(tok))
	}
	// forged: right issuer and key id, wrong key
	other := newTestIdP(t)
	other.kid = r.idp.kid
	_, _ = r.login.Authenticate(context.Background(), r.req(other.mint(t, other.claims())))
	// expired tokens are also refused before anything is trusted
	e := r.idp.claims()
	e["exp"] = int64(1)
	_, _ = r.login.Authenticate(context.Background(), r.req(r.idp.mint(t, e)))

	if len(r.repo.denied) != 0 || len(r.repo.logins) != 0 {
		t.Fatalf("a sender that has not proven a signature wrote evidence: denied=%d logins=%d", len(r.repo.denied), len(r.repo.logins))
	}
	if len(r.alerts.events) == 0 {
		t.Fatal("refusals are streamed to the SIEM instead")
	}
}

func TestVerifiedButRefusedLoginsAreEvidence(t *testing.T) {
	r := newLoginRig(t)
	mint := func(sub string) string {
		c := r.idp.claims()
		c["sub"] = sub
		return r.idp.mint(t, c)
	}
	// no link
	_, _ = r.login.Authenticate(context.Background(), r.req(mint("stranger")))
	// suspended link
	r.repo.links[tTenant+"|"+tIssuer+"|gone"] = &Link{LinkID: "l-3", TenantID: tTenant, Issuer: tIssuer, Subject: "gone", PrincipalID: "p-1", Status: LinkDeprovisioned}
	_, _ = r.login.Authenticate(context.Background(), r.req(mint("gone")))
	// inactive principal
	r.people.byID["p-2"] = &domain.Principal{PrincipalID: "p-2", TenantID: tTenant, Status: domain.PrincipalStatusSuspended}
	r.repo.links[tTenant+"|"+tIssuer+"|frozen"] = &Link{LinkID: "l-4", TenantID: tTenant, Issuer: tIssuer, Subject: "frozen", PrincipalID: "p-2", Status: LinkActive}
	_, _ = r.login.Authenticate(context.Background(), r.req(mint("frozen")))
	// wrong nonce under a valid signature
	c := r.idp.claims()
	c["nonce"] = "other"
	_, _ = r.login.Authenticate(context.Background(), r.req(r.idp.mint(t, c)))

	reasons := map[string]bool{}
	for _, d := range r.repo.denied {
		reasons[d.Reason] = true
		if d.Outcome != OutcomeDenied || d.ConnectionID != tConn || d.ConfigVersion != 1 || d.KeyID == "" {
			t.Errorf("incomplete evidence: %+v", d)
		}
	}
	for _, want := range []string{"no_link", "link_inactive", "principal_inactive", "nonce_mismatch"} {
		if !reasons[want] {
			t.Errorf("missing evidence for %q: %v", want, reasons)
		}
	}
	if len(r.issuer.issued) != 0 {
		t.Fatal("none of these may produce a token")
	}
}

func TestATokenCanBeExchangedOnce(t *testing.T) {
	r := newLoginRig(t)
	tok := r.idp.mint(t, r.idp.claims())
	if _, err := r.login.Authenticate(context.Background(), r.req(tok)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.login.Authenticate(context.Background(), r.req(tok)); !errors.Is(err, ErrLoginRejected) {
		t.Fatalf("replay: %v", err)
	}
	if len(r.repo.denied) != 1 || r.repo.denied[0].Reason != "token_replayed" {
		t.Fatalf("the replay is evidenced: %+v", r.repo.denied)
	}
}

func TestOutagesAreNotReportedAsWrongCredentials(t *testing.T) {
	r := newLoginRig(t)
	r.repo.findErr = errBoom
	if _, err := r.login.Authenticate(context.Background(), r.req(r.idp.mint(t, r.idp.claims()))); !errors.Is(err, ErrLoginUnavailable) {
		t.Fatalf("connection lookup outage: %v", err)
	}
	r.repo.findErr = nil

	r.people.err = errBoom
	if _, err := r.login.Authenticate(context.Background(), r.req(r.idp.mint(t, r.idp.claims()))); !errors.Is(err, ErrLoginUnavailable) {
		t.Fatalf("principal store outage: %v", err)
	}
	r.people.err = nil

	// the IdP's keys cannot be had: 503, and nothing durable written
	r2 := newLoginRig(t)
	km := NewKeyManager(r2.repo, &fakeFetcher{err: errBoom}, r2.repo, nil)
	r2.login = NewLogin(r2.repo, NewVerifier(km), r2.people, r2.issuer, r2.alerts, nil)
	if _, err := r2.login.Authenticate(context.Background(), r2.req(r2.idp.mint(t, r2.idp.claims()))); !errors.Is(err, ErrLoginUnavailable) {
		t.Fatalf("IdP outage: %v", err)
	}
	if len(r2.repo.denied) != 0 || len(r2.repo.logins) != 0 {
		t.Fatal("an outage is not evidence against anyone")
	}
}

func TestSuspendedConnectionAdmitsNoOne(t *testing.T) {
	r := newLoginRig(t)
	r.repo.conn = nil // FindLoginConnection returns only ACTIVE/DEGRADED connections with an approved version
	if _, err := r.login.Authenticate(context.Background(), r.req(r.idp.mint(t, r.idp.claims()))); !errors.Is(err, ErrLoginRejected) {
		t.Fatalf("%v", err)
	}
}

func TestLoginRequestValidation(t *testing.T) {
	r := newLoginRig(t)
	for _, req := range []LoginRequest{{IDToken: "x"}, {TenantID: tTenant}, {TenantID: tTenant, IDToken: "   "}} {
		if _, err := r.login.Authenticate(context.Background(), req); !errors.Is(err, ErrLoginRequestInvalid) {
			t.Errorf("%+v: %v", req, err)
		}
	}
}

func TestMFAFlowsOnlyFromApprovedValues(t *testing.T) {
	r := newLoginRig(t)
	r.repo.ver.Config.MFAAMRValues = []string{"otp"}
	c := r.idp.claims()
	c["amr"] = []string{"pwd", "otp"}
	if _, err := r.login.Authenticate(context.Background(), r.req(r.idp.mint(t, c))); err != nil {
		t.Fatal(err)
	}
	if !r.issuer.issued[0].mfa || !r.repo.logins[0].MFAAsserted {
		t.Fatal("an approved second factor is carried to the token and the evidence")
	}
}

type fakeJIT struct {
	calls int
	link  *Link
	err   error
	got   Verified
}

func (f *fakeJIT) JustInTime(_ context.Context, _ Connection, _ Version, v Verified, _ string) (*Link, error) {
	f.calls++
	f.got = v
	return f.link, f.err
}

func TestJustInTimeOnlyWhenTheConnectionOptsIn(t *testing.T) {
	newTok := func(r *loginRig) string {
		c := r.idp.claims()
		c["sub"] = "newcomer"
		c["email"] = "new@corp.example"
		c["name"] = "New Person"
		c["groups"] = []string{"AP-Clerks", "Admins"}
		return r.idp.mint(t, c)
	}
	linked := func(r *loginRig) *Link {
		return &Link{LinkID: "l-new", TenantID: tTenant, ConnectionID: tConn, Issuer: tIssuer, Subject: "newcomer", PrincipalID: "p-1", Status: LinkActive}
	}

	// disabled on the connection: never called
	r := newLoginRig(t)
	j := &fakeJIT{link: linked(r)}
	r.login.WithProvisioner(j)
	if _, err := r.login.Authenticate(context.Background(), r.req(newTok(r))); !errors.Is(err, ErrLoginRejected) || j.calls != 0 {
		t.Fatalf("a connection that did not opt in provisions nobody: %v calls=%d", err, j.calls)
	}

	// enabled: the verified claims are handed over, and login proceeds as the provisioned person
	r = newLoginRig(t)
	r.repo.ver.Config.JITEnabled = true
	j = &fakeJIT{link: linked(r)}
	r.login.WithProvisioner(j)
	resp, err := r.login.Authenticate(context.Background(), r.req(newTok(r)))
	if err != nil || resp.PrincipalID != "p-1" || j.calls != 1 {
		t.Fatalf("%+v %v calls=%d", resp, err, j.calls)
	}
	if j.got.Subject != "newcomer" || j.got.Email != "new@corp.example" || j.got.DisplayName != "New Person" || len(j.got.Attrs["groups"]) != 2 {
		t.Fatalf("directory claims reach provisioning only from a verified token: %+v", j.got)
	}

	// no provisioner wired: the flag alone does nothing
	r = newLoginRig(t)
	r.repo.ver.Config.JITEnabled = true
	if _, err := r.login.Authenticate(context.Background(), r.req(newTok(r))); !errors.Is(err, ErrLoginRejected) {
		t.Fatalf("%v", err)
	}

	// provisioning refuses (a collision, no email...): denied with evidence, no token
	r = newLoginRig(t)
	r.repo.ver.Config.JITEnabled = true
	j = &fakeJIT{err: errors.New("quarantined")}
	r.login.WithProvisioner(j)
	if _, err := r.login.Authenticate(context.Background(), r.req(newTok(r))); !errors.Is(err, ErrLoginRejected) {
		t.Fatalf("%v", err)
	}
	if len(r.repo.denied) != 1 || r.repo.denied[0].Reason != "jit_refused" || len(r.issuer.issued) != 0 {
		t.Fatalf("%+v", r.repo.denied)
	}

	// an already linked person is never re-provisioned
	r = newLoginRig(t)
	r.repo.ver.Config.JITEnabled = true
	j = &fakeJIT{link: linked(r)}
	r.login.WithProvisioner(j)
	if _, err := r.login.Authenticate(context.Background(), r.req(r.idp.mint(t, r.idp.claims()))); err != nil || j.calls != 0 {
		t.Fatalf("%v calls=%d", err, j.calls)
	}

	// an unverified address from the IdP is dropped before it can reach provisioning
	r = newLoginRig(t)
	r.repo.ver.Config.JITEnabled = true
	j = &fakeJIT{link: linked(r)}
	r.login.WithProvisioner(j)
	c := r.idp.claims()
	c["sub"], c["email"], c["email_verified"] = "newcomer", "new@corp.example", false
	_, _ = r.login.Authenticate(context.Background(), r.req(r.idp.mint(t, c)))
	if j.got.Email != "" {
		t.Fatalf("an address the IdP does not vouch for is not used: %q", j.got.Email)
	}
}
