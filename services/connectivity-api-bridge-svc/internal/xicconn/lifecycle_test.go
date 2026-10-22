package xicconn_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// authCodeContract publishes a contract whose outbound auth is an OAuth authorization-code profile.
func (e *env) authCodeContract(connectorID string) *registry.ContractVersion {
	return e.publish(connectorID, "1.0.0", func(in *registry.VersionInput) {
		for i := range in.Content.Profiles {
			if in.Content.Profiles[i].Name == "oauth" {
				in.Content.Profiles[i].Spec = json.RawMessage(`{"mechanism":"OAUTH2_AUTH_CODE","audience":"https://api.bank-x.com","scopes_required":["payments.write","payments.read"],"credential_class":"PROVIDER_CREDENTIAL"}`)
			}
		}
	})
}

// ---- create ---------------------------------------------------------------------------

func TestCreate_PinsAPublishedContractAndStoresOnlyAHashOfTheAccount(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.create(tenantA, xicconn.EnvSandbox, v, "ACC-12345-SECRET")
	if c.Status != xicconn.StatusDraft || c.ContractVersionID != v.ContractVersionID || c.Version != 1 || c.ConfigFingerprint == "" {
		t.Fatalf("%+v", c)
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "ACC-12345-SECRET") {
		t.Fatalf("the raw provider account reference was kept: %s", raw)
	}
	if c.ProviderAccountHash != xicconn.HashAccountRef(tenantA, "bank-x", "ACC-12345-SECRET") || len(c.ProviderAccountHash) != 64 {
		t.Fatalf("hash %q", c.ProviderAccountHash)
	}
	// the hash is salted by tenant and connector: the same account under another tenant differs
	if xicconn.HashAccountRef(tenantB, "bank-x", "ACC-12345-SECRET") == c.ProviderAccountHash {
		t.Fatal("account hash is not tenant-salted")
	}
	if !hasEvent(e.repo.Types(), "integration.connection.created") {
		t.Fatal("no created event")
	}
}

func TestCreate_Validation(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	other := e.publish("bank-y", "1.0.0", nil)
	base := xicconn.CreateInput{Environment: "SANDBOX", ConnectorID: "bank-x", ContractVersionID: v.ContractVersionID, Region: "uk",
		Endpoints: map[string]string{"bank-api": "api.bank-x.com"}, ProviderAccountRef: "acc"}
	cases := map[string]struct {
		mutate func(*xicconn.CreateInput)
		want   string
	}{
		"bad environment":       {func(in *xicconn.CreateInput) { in.Environment = "DEV" }, "environment"},
		"no region":             {func(in *xicconn.CreateInput) { in.Region = "" }, "region"},
		"no account":            {func(in *xicconn.CreateInput) { in.ProviderAccountRef = "  " }, "provider_account_ref"},
		"account too long":      {func(in *xicconn.CreateInput) { in.ProviderAccountRef = strings.Repeat("a", 300) }, "provider_account_ref"},
		"missing endpoint":      {func(in *xicconn.CreateInput) { in.Endpoints = map[string]string{} }, "endpoints.bank-api"},
		"free-form host (I-18)": {func(in *xicconn.CreateInput) { in.Endpoints = map[string]string{"bank-api": "evil.example"} }, "endpoints.bank-api"},
		"metadata address":      {func(in *xicconn.CreateInput) { in.Endpoints = map[string]string{"bank-api": "169.254.169.254"} }, "endpoints.bank-api"},
		"url instead of host":   {func(in *xicconn.CreateInput) { in.Endpoints = map[string]string{"bank-api": "https://api.bank-x.com/"} }, "endpoints.bank-api"},
		"unknown endpoint profile": {func(in *xicconn.CreateInput) {
			in.Endpoints = map[string]string{"bank-api": "api.bank-x.com", "extra": "api.bank-x.com"}
		}, "endpoints.extra"},
		"region the provider does not serve": {func(in *xicconn.CreateInput) { in.Region = "us" }, "region"},
		"wrong connector":                    {func(in *xicconn.CreateInput) { in.ContractVersionID = other.ContractVersionID }, "contract_version_id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := base
			in.Endpoints = map[string]string{"bank-api": "api.bank-x.com"}
			tc.mutate(&in)
			_, _, err := e.svc.Create(ctxFor(tenantA), creator, in, "")
			if err == nil || !strings.Contains(issuePaths(err), tc.want) {
				t.Fatalf("want an issue at %q, got %v", tc.want, err)
			}
		})
	}
	// a region this DEPLOYMENT may not send to, even though the provider serves it
	e.svc.AllowedRegions = []string{"eu"}
	if _, _, err := e.svc.Create(ctxFor(tenantA), creator, base, ""); !errors.Is(err, xicconn.ErrRegionDenied) {
		t.Fatalf("%v", err)
	}
	e.svc.AllowedRegions = nil // empty means none
	if _, _, err := e.svc.Create(ctxFor(tenantA), creator, base, ""); !errors.Is(err, xicconn.ErrRegionDenied) {
		t.Fatalf("empty allowlist must allow nothing: %v", err)
	}
}

func TestCreate_OnlyAPublishedContractCanBePinned(t *testing.T) {
	e := newEnv(t)
	// a draft is invisible to a tenant, so it cannot be pinned
	pctx := ctxFor(platform)
	_, _, _ = e.regSvc.CreateConnector(pctx, platform, registry.Actor{PrincipalID: "author"}, registry.Connector{ConnectorID: "bank-x", Family: "BANK_FEED", ProviderClass: "x", Owner: "y"}, "")
	d, _, err := e.regSvc.CreateVersion(pctx, platform, registry.Actor{PrincipalID: "author"}, "bank-x", func() registry.VersionInput {
		in := registrytest_input("1.0.0")
		return in
	}(), "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.svc.Create(ctxFor(tenantA), creator, xicconn.CreateInput{Environment: "SANDBOX", ConnectorID: "bank-x", ContractVersionID: d.ContractVersionID,
		Region: "uk", Endpoints: map[string]string{"bank-api": "api.bank-x.com"}, ProviderAccountRef: "a"}, "")
	if !errors.Is(err, xicconn.ErrContractUnavailable) {
		t.Fatalf("pinned a draft: %v", err)
	}
	// a superseded contract cannot take NEW connections either
	v1 := e.publish("bank-z", "1.0.0", nil)
	e.publish("bank-z", "1.1.0", nil)
	_, _, err = e.svc.Create(ctxFor(tenantA), creator, xicconn.CreateInput{Environment: "SANDBOX", ConnectorID: "bank-z", ContractVersionID: v1.ContractVersionID,
		Region: "uk", Endpoints: map[string]string{"bank-api": "api.bank-x.com"}, ProviderAccountRef: "a"}, "")
	if err == nil || !strings.Contains(issuePaths(err), "contract_version_id") {
		t.Fatalf("pinned a superseded contract: %v", err)
	}
}

func TestCreate_ExclusivityAndIdempotency(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	in := xicconn.CreateInput{Environment: "SANDBOX", ConnectorID: "bank-x", ContractVersionID: v.ContractVersionID, Region: "uk",
		Endpoints: map[string]string{"bank-api": "api.bank-x.com"}, ProviderAccountRef: "acc-1"}
	first, created, err := e.svc.Create(ctxFor(tenantA), creator, in, "k1")
	if err != nil || !created {
		t.Fatal(err)
	}
	again, created, err := e.svc.Create(ctxFor(tenantA), creator, in, "k1")
	if err != nil || created || again.ConnectionID != first.ConnectionID {
		t.Fatalf("replay: %v %v", err, created)
	}
	in2 := in
	in2.Region = "uk"
	in2.ProviderAccountRef = "acc-2"
	if _, _, err = e.svc.Create(ctxFor(tenantA), creator, in2, "k1"); !errors.Is(err, xicconn.ErrIdempotency) {
		t.Fatalf("same key, different request: %v", err)
	}
	// one live connection per (tenant, environment, connector, account)
	if _, _, err = e.svc.Create(ctxFor(tenantA), creator, in, "k2"); !errors.Is(err, xicconn.ErrExists) {
		t.Fatalf("duplicate account: %v", err)
	}
	// a different environment, account or tenant is a different connection
	prod := in
	prod.Environment = "PRODUCTION"
	if _, _, err = e.svc.Create(ctxFor(tenantA), creator, prod, ""); err != nil {
		t.Fatalf("other environment: %v", err)
	}
	if _, _, err = e.svc.Create(ctxFor(tenantB), creator, in, ""); err != nil {
		t.Fatalf("other tenant: %v", err)
	}
	// revoking frees the account
	if _, err = e.svc.Revoke(ctxFor(tenantA), creator, first.ConnectionID, first.Version, "decommissioned"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = e.svc.Create(ctxFor(tenantA), creator, in, ""); err != nil {
		t.Fatalf("after revoke: %v", err)
	}
}

// ---- authorize --------------------------------------------------------------------------

func TestAuthorize_OAuthSessionIsSingleUseAndRedirectIsPreRegistered(t *testing.T) {
	e := newEnv(t)
	v := e.authCodeContract("bank-x")
	c := e.create(tenantA, xicconn.EnvSandbox, v, "acc")
	ctx := ctxFor(tenantA)
	good := "https://app.zoiko.example/oauth/callback"
	for name, uri := range map[string]string{
		"unregistered":      "https://evil.example/cb",
		"prefix match":      good + "/../evil",
		"extra path":        good + "/x",
		"http":              "http://app.zoiko.example/oauth/callback",
		"userinfo":          "https://app.zoiko.example@evil.example/oauth/callback",
		"fragment":          good + "#x",
		"query":             good + "?next=evil",
		"empty":             "",
		"case-changed host": "https://APP.zoiko.example/oauth/callback",
	} {
		if _, err := e.svc.Authorize(ctx, creator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "OAUTH_AUTH_CODE", RedirectURI: uri}); !errors.Is(err, xicconn.ErrRedirectNotRegistered) {
			t.Errorf("%s: %v", name, err)
		}
	}
	res, err := e.svc.Authorize(ctx, creator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "OAUTH_AUTH_CODE", RedirectURI: good})
	if err != nil || res.State == "" || res.SessionID == "" || res.Connection.Status != xicconn.StatusAuthorizing || len(res.State) < 40 {
		t.Fatalf("%v %+v", err, res)
	}
	// the state is never part of what a read returns
	raw, _ := json.Marshal(e.get(tenantA, c))
	if strings.Contains(string(raw), res.State) {
		t.Fatal("authorization state leaked into the connection")
	}
	exp := e.at().Add(48 * time.Hour)
	bind := func(state string) error {
		cur := e.get(tenantA, c)
		_, err := e.svc.Bind(ctx, creator, c.ConnectionID, cur.Version, xicconn.BindInput{AuthProfile: "oauth", CredentialRef: refOAuth,
			GrantedScopes: oauthScopes, ConsentActor: "tenant-admin", ExpiresAt: &exp, State: state})
		return err
	}
	if err := bind(""); !errors.Is(err, xicconn.ErrAuthState) {
		t.Fatalf("no state: %v", err)
	}
	if err := bind("wrong-state-wrong-state-wrong-state-wrong-state"); !errors.Is(err, xicconn.ErrAuthState) {
		t.Fatalf("wrong state: %v", err)
	}
	if err := bind(res.State); err != nil {
		t.Fatalf("right state: %v", err)
	}
	// replay of the same state
	if err := bind(res.State); !errors.Is(err, xicconn.ErrAuthState) {
		t.Fatalf("replayed state: %v", err)
	}
}

func TestAuthorize_StateExpires(t *testing.T) {
	e := newEnv(t)
	v := e.authCodeContract("bank-x")
	c := e.create(tenantA, xicconn.EnvSandbox, v, "acc")
	res, err := e.svc.Authorize(ctxFor(tenantA), creator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "OAUTH_AUTH_CODE", RedirectURI: "https://app.zoiko.example/oauth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	e.advance(11 * time.Minute)
	exp := e.at().Add(time.Hour)
	_, err = e.svc.Bind(ctxFor(tenantA), creator, c.ConnectionID, res.Connection.Version, xicconn.BindInput{AuthProfile: "oauth", CredentialRef: refOAuth,
		GrantedScopes: oauthScopes, ConsentActor: "x", ExpiresAt: &exp, State: res.State})
	if !errors.Is(err, xicconn.ErrAuthState) {
		t.Fatalf("expired state accepted: %v", err)
	}
}

func TestAuthorize_Modes(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil) // client-credentials contract: no authorization-code profile
	c := e.create(tenantA, xicconn.EnvSandbox, v, "acc")
	ctx := ctxFor(tenantA)
	for name, in := range map[string]xicconn.AuthorizeInput{
		"no mode":                {},
		"unknown mode":           {Mode: "MAGIC"},
		"oauth without profile":  {Mode: "OAUTH_AUTH_CODE", RedirectURI: "https://app.zoiko.example/oauth/callback"},
		"redirect without oauth": {Mode: "PRE_PROVISIONED", RedirectURI: "https://app.zoiko.example/oauth/callback"},
	} {
		if _, err := e.svc.Authorize(ctx, creator, c.ConnectionID, c.Version, in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := e.svc.Authorize(ctx, creator, c.ConnectionID, 99, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"}); !errors.Is(err, xicconn.ErrVersionConflict) {
		t.Fatalf("stale: %v", err)
	}
	res, err := e.svc.Authorize(ctx, creator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"})
	if err != nil || res.State != "" {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err = e.svc.Authorize(ctx, creator, c.ConnectionID, res.Connection.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"}); !errors.Is(err, xicconn.ErrTransition) {
		t.Fatalf("authorized twice: %v", err)
	}
}

// ---- bind -------------------------------------------------------------------------------

func authorizing(e *env, v *registry.ContractVersion, tenant, account string) *xicconn.Connection {
	c := e.create(tenant, xicconn.EnvSandbox, v, account)
	res, err := e.svc.Authorize(ctxFor(tenant), creator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"})
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Connection
}

func TestBind_Rules(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := authorizing(e, v, tenantA, "acc")
	ctx := ctxFor(tenantA)
	exp := e.at().Add(24 * time.Hour)
	past := e.at().Add(-time.Hour)
	ok := xicconn.BindInput{AuthProfile: "oauth", CredentialRef: refOAuth, GrantedScopes: oauthScopes, ConsentActor: "tenant-admin", ExpiresAt: &exp}
	try := func(mutate func(*xicconn.BindInput)) error {
		in := ok
		mutate(&in)
		cur := e.get(tenantA, c)
		_, err := e.svc.Bind(ctx, creator, c.ConnectionID, cur.Version, in)
		return err
	}
	e.vault.set(refOther, vault.ErrForbidden)
	e.vault.set("0a000000-0000-4000-8000-0000000000c1", vault.ErrNotActive)

	if err := try(func(in *xicconn.BindInput) { in.GrantedScopes = []string{"payments.write"} }); !errors.Is(err, xicconn.ErrScopesInsufficient) {
		t.Errorf("insufficient scopes: %v", err)
	}
	if err := try(func(in *xicconn.BindInput) { in.GrantedScopes = nil }); !errors.Is(err, xicconn.ErrScopesInsufficient) {
		t.Errorf("no scopes: %v", err)
	}
	if err := try(func(in *xicconn.BindInput) { in.GrantedScopes = append([]string{"admin.all"}, oauthScopes...) }); !errors.Is(err, xicconn.ErrExcessScopes) {
		t.Errorf("excess scopes without review: %v", err)
	}
	if err := try(func(in *xicconn.BindInput) { in.ExpiresAt = &past }); !errors.Is(err, xicconn.ErrCredentialExpired) {
		t.Errorf("expired: %v", err)
	}
	if err := try(func(in *xicconn.BindInput) { in.ConsentActor = " " }); err == nil {
		t.Error("no consent actor accepted")
	}
	if err := try(func(in *xicconn.BindInput) { in.CredentialRef = "not-a-ref" }); err == nil {
		t.Error("malformed ref accepted")
	}
	if err := try(func(in *xicconn.BindInput) { in.CredentialRef = "https://secret.example/key" }); err == nil {
		t.Error("a URL as credential ref accepted")
	}
	if err := try(func(in *xicconn.BindInput) { in.AuthProfile = "nope" }); err == nil {
		t.Error("unknown profile accepted")
	}
	if err := try(func(in *xicconn.BindInput) { in.CredentialRef = refOther }); !errors.Is(err, xicconn.ErrCredentialInvalid) {
		t.Errorf("vault forbids: %v", err)
	}
	if err := try(func(in *xicconn.BindInput) { in.CredentialRef = "0a000000-0000-4000-8000-0000000000c1" }); !errors.Is(err, xicconn.ErrCredentialInvalid) {
		t.Errorf("vault inactive: %v", err)
	}
	if err := try(func(in *xicconn.BindInput) { in.CredentialRef = "0a000000-0000-4000-8000-0000000000c9" }); !errors.Is(err, xicconn.ErrCredentialInvalid) {
		t.Errorf("vault unknown: %v", err)
	}
	if err := try(func(in *xicconn.BindInput) { in.State = "x" }); err == nil {
		t.Error("state on a non-code profile accepted")
	}
	e.vault.down = true
	if err := try(func(in *xicconn.BindInput) {}); !errors.Is(err, xicconn.ErrDependency) {
		t.Errorf("vault outage must fail closed as a dependency error: %v", err)
	}
	e.vault.down = false
	if got := e.get(tenantA, c); len(got.Bindings) != 0 {
		t.Fatalf("a refused bind left a binding: %+v", got.Bindings)
	}
	// reviewed excess scopes are allowed and recorded
	if err := try(func(in *xicconn.BindInput) {
		in.GrantedScopes = append([]string{"accounts.read"}, oauthScopes...)
		in.AcceptExcessScopes = true
	}); err != nil {
		t.Fatalf("reviewed excess: %v", err)
	}
	b := e.get(tenantA, c).BindingFor("oauth")
	if b == nil || b.ExcessScopeAckBy != "creator" || b.CurrentRef != refOAuth || b.ScopeFingerprint != xicconn.ScopeFingerprint(b.GrantedScopes) {
		t.Fatalf("%+v", b)
	}
	// the stored binding is a reference and metadata: never a secret
	raw, _ := json.Marshal(e.get(tenantA, c))
	if strings.Contains(strings.ToLower(string(raw)), "secret") {
		t.Fatalf("binding output mentions a secret: %s", raw)
	}
}

func TestBind_MTLSCarriesTheCertificateExpiry(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", func(in *registry.VersionInput) {
		for i := range in.Content.Profiles {
			if in.Content.Profiles[i].Name == "hmac-in" {
				in.Content.Profiles[i].Spec = json.RawMessage(`{"mechanism":"MTLS","subject_binding":"CONNECTION","credential_class":"CALLBACK_SECRET"}`)
			}
		}
	})
	c := authorizing(e, v, tenantA, "acc")
	cur := e.get(tenantA, c)
	_, err := e.svc.Bind(ctxFor(tenantA), creator, c.ConnectionID, cur.Version, xicconn.BindInput{AuthProfile: "hmac-in", CredentialRef: refHMAC, ConsentActor: "x"})
	if err == nil || !strings.Contains(issuePaths(err), "expires_at") {
		t.Fatalf("an mTLS binding without a certificate expiry: %v", err)
	}
}

func TestBind_WrongStateOfTheConnection(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.create(tenantA, xicconn.EnvSandbox, v, "acc") // DRAFT
	exp := e.at().Add(time.Hour)
	if _, err := e.svc.Bind(ctxFor(tenantA), creator, c.ConnectionID, c.Version, xicconn.BindInput{AuthProfile: "oauth", CredentialRef: refOAuth,
		GrantedScopes: oauthScopes, ConsentActor: "x", ExpiresAt: &exp}); !errors.Is(err, xicconn.ErrTransition) {
		t.Fatalf("bound a draft: %v", err)
	}
	cur := e.get(tenantA, authorizing(e, v, tenantA, "acc2"))
	if _, err := e.svc.Bind(ctxFor(tenantA), creator, cur.ConnectionID, cur.Version+3, xicconn.BindInput{AuthProfile: "oauth", CredentialRef: refOAuth,
		GrantedScopes: oauthScopes, ConsentActor: "x", ExpiresAt: &exp}); !errors.Is(err, xicconn.ErrVersionConflict) {
		t.Fatalf("stale bind: %v", err)
	}
}

func TestBind_ACredentialReferenceBelongsToOneBindingPlatformWide(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	a := authorizing(e, v, tenantA, "acc-a")
	b := authorizing(e, v, tenantB, "acc-b")
	exp := e.at().Add(24 * time.Hour)
	in := xicconn.BindInput{AuthProfile: "oauth", CredentialRef: refOAuth, GrantedScopes: oauthScopes, ConsentActor: "x", ExpiresAt: &exp}
	if _, err := e.svc.Bind(ctxFor(tenantA), creator, a.ConnectionID, a.Version, in); err != nil {
		t.Fatal(err)
	}
	// tenant B presents the same reference: refused, and a security signal is raised even though the bind failed
	_, err := e.svc.Bind(ctxFor(tenantB), creator, b.ConnectionID, b.Version, in)
	if !errors.Is(err, xicconn.ErrCrossTenantCredential) {
		t.Fatalf("a reference bound in two tenants: %v", err)
	}
	if len(e.repo.Signals) != 1 || e.repo.Signals[0].Type != "integration.security.cross-tenant-credential" {
		t.Fatalf("signals %+v", e.repo.Signals)
	}
	if got := e.get(tenantB, b); len(got.Bindings) != 0 {
		t.Fatal("the foreign reference was bound")
	}
	// after tenant A revokes, the reference is free again
	cur := e.get(tenantA, a)
	if _, err := e.svc.Revoke(ctxFor(tenantA), creator, a.ConnectionID, cur.Version, "gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Bind(ctxFor(tenantB), creator, b.ConnectionID, b.Version, in); err != nil {
		t.Fatalf("after revocation: %v", err)
	}
}

// ---- validate ---------------------------------------------------------------------------

func TestValidate_FailureIsRecordedAndDoesNotAdvance(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := authorizing(e, v, tenantA, "acc") // nothing bound yet
	res, err := e.svc.Validate(ctxFor(tenantA), operator, c.ConnectionID, c.Version, "")
	var ve *xicconn.Validation
	if !errors.As(err, &ve) || res == nil || res.Check.Outcome != xicconn.Failed {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.Contains(issuePaths(err), "bindings.oauth") || !strings.Contains(issuePaths(err), "bindings.hmac-in") {
		t.Fatalf("%v", issuePaths(err))
	}
	if res.Connection.Status != xicconn.StatusAuthorizing {
		t.Fatalf("a failed validation advanced the connection: %s", res.Connection.Status)
	}
	checks, _ := e.repo.ListChecks(ctxFor(tenantA), c.ConnectionID)
	if len(checks) != 1 || checks[0].Outcome != xicconn.Failed {
		t.Fatalf("%+v", checks)
	}
}

func TestValidate_PassAdvancesAndSnapshotsCapabilities(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.bound(tenantA, e.create(tenantA, xicconn.EnvSandbox, v, "acc"), refOAuth, refHMAC)
	res, err := e.svc.Validate(ctxFor(tenantA), operator, c.ConnectionID, c.Version, "")
	if err != nil || res.Connection.Status != xicconn.StatusValidating || res.Check.Outcome != xicconn.Passed {
		t.Fatalf("%v %+v", err, res)
	}
	var caps []struct {
		Operation string `json:"operation_id"`
		Ready     bool   `json:"ready"`
	}
	if err := json.Unmarshal(res.Connection.CapabilitySnapshot, &caps); err != nil || len(caps) != 3 {
		t.Fatalf("%v %s", err, res.Connection.CapabilitySnapshot)
	}
	for _, c := range caps {
		if !c.Ready {
			t.Errorf("%s not ready", c.Operation)
		}
	}
	if res.Connection.ConfigFingerprint != xicconn.ConfigFingerprint(res.Connection) {
		t.Fatal("fingerprint not pinned")
	}
}

func TestValidate_IdempotentRunKey(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.bound(tenantA, e.create(tenantA, xicconn.EnvSandbox, v, "acc"), refOAuth, refHMAC)
	first, err := e.svc.Validate(ctxFor(tenantA), operator, c.ConnectionID, c.Version, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.Validate(ctxFor(tenantA), operator, c.ConnectionID, first.Connection.Version, "run-1")
	if err != nil || again.Check.CheckID != first.Check.CheckID {
		t.Fatalf("a repeated run key made a new run: %v", err)
	}
	if checks, _ := e.repo.ListChecks(ctxFor(tenantA), c.ConnectionID); len(checks) != 1 {
		t.Fatalf("%d checks for one run key", len(checks))
	}
}

func TestValidate_ChecksTheRealWorld(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	mk := func(account string) *xicconn.Connection {
		return e.bound(tenantA, e.create(tenantA, xicconn.EnvSandbox, v, account), refOAuth, refHMAC)
	}
	ctx := ctxFor(tenantA)

	// a vault outage is not a failed validation: nothing is recorded, the caller retries
	c := mk("acc-1")
	e.vault.down = true
	if _, err := e.svc.Validate(ctx, operator, c.ConnectionID, c.Version, ""); !errors.Is(err, xicconn.ErrDependency) {
		t.Fatalf("%v", err)
	}
	e.vault.down = false
	if checks, _ := e.repo.ListChecks(ctx, c.ConnectionID); len(checks) != 0 {
		t.Fatal("an outage was recorded as a failed validation")
	}
	// the vault no longer confirms the reference
	e.vault.set(refOAuth, vault.ErrNotActive)
	_, err := e.svc.Validate(ctx, operator, c.ConnectionID, c.Version, "")
	if err == nil || !strings.Contains(issuePaths(err), "bindings.oauth") {
		t.Fatalf("%v", err)
	}
	e.vault.set(refOAuth, nil)
	// a destination that resolves into a private range fails egress policy before any credential is attached
	e.dns["api.bank-x.com"] = "10.0.0.5"
	cur := e.get(tenantA, c)
	_, err = e.svc.Validate(ctx, operator, cur.ConnectionID, cur.Version, "")
	if err == nil || !strings.Contains(issuePaths(err), "endpoints.bank-api") {
		t.Fatalf("a private destination passed validation: %v", err)
	}
	e.dns["api.bank-x.com"] = "169.254.169.254"
	cur = e.get(tenantA, c)
	if _, err = e.svc.Validate(ctx, operator, cur.ConnectionID, cur.Version, ""); err == nil {
		t.Fatal("the metadata address passed validation")
	}
	e.dns["api.bank-x.com"] = "93.184.216.34"
	// the deployment stops allowing the region
	e.svc.AllowedRegions = []string{"eu"}
	cur = e.get(tenantA, c)
	if _, err = e.svc.Validate(ctx, operator, cur.ConnectionID, cur.Version, ""); err == nil || !strings.Contains(issuePaths(err), "region") {
		t.Fatalf("%v", err)
	}
	e.svc.AllowedRegions = []string{"uk"}
	// an expired credential
	e.advance(61 * 24 * time.Hour)
	cur = e.get(tenantA, c)
	if _, err = e.svc.Validate(ctx, operator, cur.ConnectionID, cur.Version, ""); err == nil || !strings.Contains(issuePaths(err), "bindings.oauth") {
		t.Fatalf("an expired credential validated: %v", err)
	}
}

func TestValidate_PinnedContractSupportAndSupersession(t *testing.T) {
	e := newEnv(t)
	v1 := e.publish("bank-x", "1.0.0", nil)
	c := e.bound(tenantA, e.create(tenantA, xicconn.EnvSandbox, v1, "acc"), refOAuth, refHMAC)
	// a newer version supersedes the pin: the connection is still valid (I-04)
	e.publish("bank-x", "1.1.0", nil)
	if _, err := e.svc.Validate(ctxFor(tenantA), operator, c.ConnectionID, c.Version, ""); err != nil {
		t.Fatalf("a superseded pin must stay valid: %v", err)
	}
}

func TestValidate_ActiveDegradesOnFailureAndRecovers(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	e.vault.set(refHMAC, vault.ErrNotActive)
	res, err := e.svc.Validate(ctxFor(tenantA), operator, c.ConnectionID, c.Version, "")
	if err == nil || res.Connection.Status != xicconn.StatusDegraded || res.Connection.StatusReason != "VALIDATION_FAILED" {
		t.Fatalf("%v %+v", err, res)
	}
	e.vault.set(refHMAC, nil)
	res, err = e.svc.Validate(ctxFor(tenantA), operator, c.ConnectionID, res.Connection.Version, "")
	if err != nil || res.Connection.Status != xicconn.StatusActive || res.Connection.StatusReason != "" {
		t.Fatalf("recovery: %v %+v", err, res)
	}
}

// ---- probe, certify, activate ------------------------------------------------------------

func TestProbe_AccountIdentityAndCapabilities(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.validated(tenantA, e.bound(tenantA, e.create(tenantA, xicconn.EnvProduction, v, "acc-real"), refOAuth, refHMAC))
	ctx := ctxFor(tenantA)
	all := []string{"statement-push", "payment-submit", "payment-status"}
	ck, err := e.svc.ReportProbe(ctx, operator, c.ConnectionID, 0, xicconn.ProbeInput{AccountRef: "someone-elses-account", Capabilities: all})
	if err != nil || ck.Outcome != xicconn.Failed || !strings.Contains(issuePaths2(ck), "account_ref") {
		t.Fatalf("a different provider account passed: %v %+v", err, ck)
	}
	ck, err = e.svc.ReportProbe(ctx, operator, c.ConnectionID, 0, xicconn.ProbeInput{AccountRef: "acc-real", Capabilities: all[:2]})
	if err != nil || ck.Outcome != xicconn.Failed || !strings.Contains(issuePaths2(ck), "capabilities.payment-status") {
		t.Fatalf("a missing capability passed: %v %+v", err, ck)
	}
	ck, err = e.svc.ReportProbe(ctx, operator, c.ConnectionID, 0, xicconn.ProbeInput{AccountRef: "acc-real", Capabilities: all})
	if err != nil || ck.Outcome != xicconn.Passed {
		t.Fatalf("%v %+v", err, ck)
	}
	raw, _ := json.Marshal(ck)
	if strings.Contains(string(raw), "acc-real") || strings.Contains(string(raw), "someone-elses") {
		t.Fatalf("a raw account reference was recorded: %s", raw)
	}
}

func issuePaths2(ck *xicconn.Check) string {
	var ps []string
	for _, i := range ck.Issues {
		ps = append(ps, i.Path)
	}
	return strings.Join(ps, ",")
}

func TestCertify_Rules(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.validated(tenantA, e.bound(tenantA, e.create(tenantA, xicconn.EnvProduction, v, "acc"), refOAuth, refHMAC))
	ctx := ctxFor(tenantA)
	if _, err := e.svc.Certify(ctx, creator, c.ConnectionID, xicconn.CertifyInput{TestPackVersion: "tp-2026.10", Passed: passedTests()}); !errors.Is(err, xicconn.ErrSelfAction) {
		t.Fatalf("self certification: %v", err)
	}
	if _, err := e.svc.Certify(ctx, certifier, c.ConnectionID, xicconn.CertifyInput{TestPackVersion: "other", Passed: passedTests()}); err == nil {
		t.Fatal("wrong test pack accepted")
	}
	if _, err := e.svc.Certify(ctx, certifier, c.ConnectionID, xicconn.CertifyInput{TestPackVersion: "tp-2026.10", Passed: []string{"NP-01"}}); err == nil {
		t.Fatal("a certification that skips required tests was recorded")
	}
	if _, err := e.svc.Certify(ctx, certifier, c.ConnectionID, xicconn.CertifyInput{TestPackVersion: "tp-2026.10", Passed: passedTests(), Outcome: "MAYBE"}); err == nil {
		t.Fatal("bad outcome accepted")
	}
	certs, _ := e.repo.ListCertifications(ctx, c.ConnectionID)
	if len(certs) != 0 {
		t.Fatalf("%d certifications recorded by refused requests", len(certs))
	}
	cert, err := e.svc.Certify(ctx, certifier, c.ConnectionID, xicconn.CertifyInput{TestPackVersion: "tp-2026.10", Passed: passedTests()})
	if err != nil || cert.Outcome != xicconn.Passed || cert.ExpiresAt.Sub(cert.At) != 180*24*time.Hour || cert.Environment != xicconn.EnvProduction {
		t.Fatalf("%v %+v", err, cert)
	}
	if !hasEvent(e.repo.Types(), "integration.certification.passed") {
		t.Fatal("no certification event")
	}
	// a draft cannot be certified
	d := e.create(tenantA, xicconn.EnvProduction, v, "acc-2")
	if _, err := e.svc.Certify(ctx, certifier, d.ConnectionID, xicconn.CertifyInput{TestPackVersion: "tp-2026.10", Passed: passedTests()}); !errors.Is(err, xicconn.ErrTransition) {
		t.Fatalf("certified a draft: %v", err)
	}
}

func TestCertificationStateIsComputedFromWhatItCovers(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.production(tenantA, v, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)
	state := func() string {
		cur := e.get(tenantA, c)
		certs, _ := e.repo.ListCertifications(ctx, c.ConnectionID)
		pv, _ := e.contracts.GetVersion(ctx, e.svcPin(cur))
		st, _ := xicconn.CertificationState(cur, pv, certs, e.at())
		return st
	}
	if st := state(); st != xicconn.CertValid {
		t.Fatalf("%s", st)
	}
	e.advance(181 * 24 * time.Hour)
	if st := state(); st != xicconn.CertExpired {
		t.Fatalf("%s", st)
	}
	// sandbox needs none
	sb := e.sandbox(tenantA, v, "acc-sb", refOAuth2, refHMAC2)
	if st, _ := xicconn.CertificationState(sb, v, nil, e.at()); st != xicconn.CertNotRequired {
		t.Fatalf("%s", st)
	}
	// a failed run is a failed certification, not a missing one
	failed := &xicconn.Certification{Outcome: xicconn.Failed}
	if st, _ := xicconn.CertificationState(e.get(tenantA, c), v, []xicconn.Certification{*failed}, e.at()); st != xicconn.CertFailed {
		t.Fatalf("%s", st)
	}
	if st, _ := xicconn.CertificationState(e.get(tenantA, c), v, nil, e.at()); st != xicconn.CertMissing {
		t.Fatalf("%s", st)
	}
}

func (e *env) svcPin(c *xicconn.Connection) string { return c.ContractVersionID }

func TestActivate_Rules(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	ctx := ctxFor(tenantA)

	// sandbox: needs a validation and a different person
	c := e.bound(tenantA, e.create(tenantA, xicconn.EnvSandbox, v, "acc"), refOAuth, refHMAC)
	if _, err := e.svc.Activate(ctx, operator, c.ConnectionID, c.Version); !errors.Is(err, xicconn.ErrTransition) {
		t.Fatalf("activated an unvalidated connection: %v", err)
	}
	c = e.validated(tenantA, c)
	if _, err := e.svc.Activate(ctx, creator, c.ConnectionID, c.Version); !errors.Is(err, xicconn.ErrSelfAction) {
		t.Fatalf("the creator activated their own connection: %v", err)
	}
	if _, err := e.svc.Activate(ctx, operator, c.ConnectionID, c.Version+4); !errors.Is(err, xicconn.ErrVersionConflict) {
		t.Fatalf("stale: %v", err)
	}
	// changing the configuration after validation invalidates it
	exp := e.at().Add(90 * 24 * time.Hour)
	e.vault.set(refOAuth3, nil)
	res, err := e.svc.Rotate(ctx, operator, c.ConnectionID, c.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOAuth3, ExpiresAt: &exp})
	if err == nil {
		// rotation is only for live connections; this one is not yet
		t.Fatalf("rotated a connection that was never active: %+v", res)
	}
	act, err := e.svc.Activate(ctx, operator, c.ConnectionID, c.Version)
	if err != nil || act.Status != xicconn.StatusActive || act.ActivatedBy != "operator" || act.ActivatedAt == nil {
		t.Fatalf("%v %+v", err, act)
	}
	if !hasEvent(e.repo.Types(), "integration.connection.activated") {
		t.Fatal("no activated event")
	}
}

func TestActivate_ProductionNeedsProbeAndCertification(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	ctx := ctxFor(tenantA)
	c := e.validated(tenantA, e.bound(tenantA, e.create(tenantA, xicconn.EnvProduction, v, "acc"), refOAuth, refHMAC))
	if _, err := e.svc.Activate(ctx, operator, c.ConnectionID, c.Version); !errors.Is(err, xicconn.ErrNotValidated) {
		t.Fatalf("no probe: %v", err)
	}
	e.probeOK(tenantA, c, "acc", v)
	cur := e.get(tenantA, c)
	if _, err := e.svc.Activate(ctx, operator, c.ConnectionID, cur.Version); !errors.Is(err, xicconn.ErrNotCertified) {
		t.Fatalf("no certification: %v", err)
	}
	e.certify(tenantA, c)
	cur = e.get(tenantA, c)
	act, err := e.svc.Activate(ctx, operator, c.ConnectionID, cur.Version)
	if err != nil || act.Status != xicconn.StatusActive {
		t.Fatalf("%v", err)
	}
}

func TestActivate_ContractNotYetEffective(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", func(in *registry.VersionInput) { in.EffectiveFrom = registrytest_t0().Add(30 * 24 * time.Hour) })
	c := e.validated(tenantA, e.bound(tenantA, e.create(tenantA, xicconn.EnvSandbox, v, "acc"), refOAuth, refHMAC))
	if _, err := e.svc.Activate(ctxFor(tenantA), operator, c.ConnectionID, c.Version); !errors.Is(err, xicconn.ErrContractUnavailable) {
		t.Fatalf("activated before the contract is effective: %v", err)
	}
	e.advance(31 * 24 * time.Hour)
	// credentials issued in the test clock expire at 60d; still fine at +31d
	c = e.get(tenantA, c)
	res, err := e.svc.Validate(ctxFor(tenantA), operator, c.ConnectionID, c.Version, "again")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Activate(ctxFor(tenantA), operator, c.ConnectionID, res.Connection.Version); err != nil {
		t.Fatalf("after the effective date: %v", err)
	}
}
