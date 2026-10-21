package connector

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
	"zoiko.io/eventing/egress"
)

const (
	tenantA  = "tenant-a"
	tenantB  = "tenant-b"
	credOK   = "0a000000-0000-4000-8000-000000000001"
	credOK2  = "0a000000-0000-4000-8000-000000000002"
	credGone = "0a000000-0000-4000-8000-0000000000ff"
)

var (
	maker   = Actor{PrincipalID: "maker", CorrelationID: "corr-maker"}
	checker = Actor{PrincipalID: "checker", CorrelationID: "corr-checker"}
	editor  = Actor{PrincipalID: "editor"}
	admin   = Actor{PrincipalID: "platform-admin"}
)

type fakeVault struct {
	mu   sync.Mutex
	refs map[string]error // nil error = valid
	down bool
	seen []vault.Caller
}

func (f *fakeVault) Verify(_ context.Context, ref string, c vault.Caller) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, c)
	if f.down {
		return vault.ErrUnavailable
	}
	if err, ok := f.refs[strings.ToLower(ref)]; ok {
		return err
	}
	return vault.ErrNotFound
}

type fakeResolver struct {
	mu    sync.Mutex
	hosts map[string][]string
}

func (r *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ips, ok := r.hosts[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	var out []net.IPAddr
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

type env struct {
	svc   *Service
	mem   *store.MemoryStore
	vault *fakeVault
	dns   *fakeResolver
	now   *time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	mem := store.NewMemoryStore()
	v := &fakeVault{refs: map[string]error{credOK: nil, credOK2: nil}}
	d := &fakeResolver{hosts: map[string][]string{"api.bank-x.example": {"93.184.216.34"}, "feeds.bank-x.example": {"93.184.216.35"}}}
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	e := &env{mem: mem, vault: v, dns: d, now: &now}
	e.svc = &Service{
		Store: mem, Vault: v, Egress: egress.Policy{Resolver: d}, EgressAllowedRegions: []string{"uk", "eu-west"},
		Now: func() time.Time { return *e.now },
	}
	// the platform publishes and certifies bank-x v2
	pctx := middleware.WithTenant(context.Background(), store.PlatformScopeID)
	e.publish(t, pctx, "bank-x", 2, []string{"uk"})
	e.mem.ResetEvents() // the seed is not what the tests assert on
	return e
}

func (e *env) publish(t *testing.T, pctx context.Context, key string, version int, regions []string) {
	t.Helper()
	p := &domain.Profile{ProviderKey: key, ProviderType: "BANK", Version: version, AllowedRegions: regions,
		AllowedHostSuffixes: []string{"bank-x.example"}, SupportedScopes: []string{"ingest", "statements.read", "payments.write"}}
	if err := e.svc.PublishProfile(pctx, store.PlatformScopeID, admin, p); err != nil {
		t.Fatalf("publish profile: %v", err)
	}
	if _, err := e.svc.SetProfileStatus(pctx, store.PlatformScopeID, admin, key, version, domain.ProfileCertified); err != nil {
		t.Fatalf("certify profile: %v", err)
	}
}

func ctxOf(tenant string) context.Context { return middleware.WithTenant(context.Background(), tenant) }

func goodRequest() CreateInput {
	return CreateInput{CreateBridgeRequest: domain.CreateBridgeRequest{
		LegalEntityID: "le-1", BridgeName: "Bank X statements", Protocol: "REST",
		EndpointURL: "https://api.bank-x.example/v2/feed", AuthType: "oauth2",
		Purpose: "bank-statement-import", ProviderKey: "bank-x", ProviderProfileVersion: 2,
		CredentialRef: credOK, DestinationRegion: "uk", SyncMode: "pull",
		Scopes: []string{"ingest", "statements.read"},
	}}
}

// created returns a DRAFT connector made by maker.
func (e *env) created(t *testing.T) *domain.ApiBridge {
	t.Helper()
	b, created, err := e.svc.Create(ctxOf(tenantA), maker, goodRequest())
	if err != nil || !created {
		t.Fatalf("create: %v created=%v", err, created)
	}
	return b
}

func (e *env) pending(t *testing.T) *domain.ApiBridge {
	t.Helper()
	b := e.created(t)
	b, err := e.svc.Submit(ctxOf(tenantA), tenantA, maker, b.BridgeID, b.Version)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return b
}

func (e *env) approved(t *testing.T) *domain.ApiBridge {
	t.Helper()
	b := e.pending(t)
	b, err := e.svc.Approve(ctxOf(tenantA), tenantA, checker, b.BridgeID, b.Version, "looks right")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return b
}

func (e *env) active(t *testing.T) *domain.ApiBridge {
	t.Helper()
	b := e.approved(t)
	b, err := e.svc.Activate(ctxOf(tenantA), tenantA, checker, b.BridgeID, b.Version)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	return b
}

func get(t *testing.T, e *env, tenant, id string) *domain.ApiBridge {
	t.Helper()
	b, err := e.mem.GetBridgeByID(ctxOf(tenant), id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- create --------------------------------------------------------------------

func TestCreateStartsAsDraftAndNeverLive(t *testing.T) {
	e := newEnv(t)
	b := e.created(t)
	if b.Status != domain.StatusDraft || b.Version != 1 || b.CreatedBy != "maker" || b.LastChangedBy != "maker" {
		t.Fatalf("%+v", b)
	}
	if b.Live() {
		t.Fatal("a new connector must not be usable before approval")
	}
	if b.ConnectionID != "" && b.ConnectionID != b.BridgeID {
		t.Fatal("connection id aliasing")
	}
	if b.DestinationHost != "api.bank-x.example" {
		t.Fatalf("destination host is derived from the endpoint: %q", b.DestinationHost)
	}
	if got := e.mem.EventTypes(); len(got) != 1 || got[0] != "bridge.created" {
		t.Fatalf("events = %v", got)
	}
	ev := e.mem.Events()[0].Event
	if ev.LegacyType != "connectivity.bridge.created" {
		t.Fatalf("legacy event type must be kept for existing consumers: %q", ev.LegacyType)
	}
}

func TestCreateNormalisesAndAcceptsLegacyFieldNames(t *testing.T) {
	e := newEnv(t)
	in := CreateInput{CreateBridgeRequest: domain.CreateBridgeRequest{
		LegalEntityID: "le-1", ProviderName: "Legacy Name", BaseURL: "https://api.bank-x.example/x", AuthType: "OAuth2"}}
	b, _, err := e.svc.Create(ctxOf(tenantA), maker, in)
	if err != nil {
		t.Fatal(err)
	}
	if b.BridgeName != "Legacy Name" || b.EndpointURL != "https://api.bank-x.example/x" || b.AuthType != "OAUTH2" {
		t.Fatalf("%+v", b)
	}
}

func TestCreateRefusesAnythingThatCouldBeASecret_INT_NP_019(t *testing.T) {
	e := newEnv(t)
	for name, mutate := range map[string]func(*CreateInput){
		"userinfo in the url":    func(c *CreateInput) { c.EndpointURL = "https://user:hunter2@api.bank-x.example/x" },
		"api key in the query":   func(c *CreateInput) { c.EndpointURL = "https://api.bank-x.example/x?api_key=sk_live_123" },
		"token in the query":     func(c *CreateInput) { c.EndpointURL = "https://api.bank-x.example/x?access_token=abc" },
		"secret as credential":   func(c *CreateInput) { c.CredentialRef = "sk_live_REALSECRET" }, // gitleaks:allow -- deliberately fake secret: the test proves a credential-shaped value is REFUSED as a credential_ref
		"password as credential": func(c *CreateInput) { c.CredentialRef = "hunter2" },
		"bearer as credential":   func(c *CreateInput) { c.CredentialRef = "Bearer abcdef" },
	} {
		in := goodRequest()
		mutate(&in)
		_, _, err := e.svc.Create(ctxOf(tenantA), maker, in)
		if !errors.Is(err, domain.ErrIncompleteConfig) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, leak := range []string{"hunter2", "sk_live", "REALSECRET", "abcdef"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: the error echoes the secret: %v", name, err)
			}
		}
	}
	if got := e.mem.EventTypes(); len(got) != 0 {
		t.Fatalf("a refused create must emit nothing: %v", got)
	}
}

func TestCreateRequiresNameAndEntity(t *testing.T) {
	e := newEnv(t)
	for _, in := range []CreateInput{
		{CreateBridgeRequest: domain.CreateBridgeRequest{BridgeName: "x"}},
		{CreateBridgeRequest: domain.CreateBridgeRequest{LegalEntityID: "le"}},
	} {
		if _, _, err := e.svc.Create(ctxOf(tenantA), maker, in); !errors.Is(err, ErrValidation) {
			t.Errorf("%v", err)
		}
	}
}

func TestCreateIdempotency(t *testing.T) {
	e := newEnv(t)
	in := goodRequest()
	in.IdempotencyKey = "key-1"
	first, created, err := e.svc.Create(ctxOf(tenantA), maker, in)
	if err != nil || !created {
		t.Fatal(err)
	}
	again, created, err := e.svc.Create(ctxOf(tenantA), maker, in)
	if err != nil || created || again.BridgeID != first.BridgeID {
		t.Fatalf("a replay must return the original: %v created=%v", err, created)
	}
	if n := len(e.mem.Events()); n != 1 {
		t.Fatalf("a replay must not emit again: %d events", n)
	}
	// the same key for a DIFFERENT request is a conflict, not a duplicate
	diff := goodRequest()
	diff.IdempotencyKey = "key-1"
	diff.BridgeName = "Something else"
	if _, _, err := e.svc.Create(ctxOf(tenantA), maker, diff); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("conflicting reuse of a key: %v", err)
	}
	// another tenant's use of the same key is unrelated
	if b, created, err := e.svc.Create(ctxOf(tenantB), maker, in); err != nil || !created || b.BridgeID == first.BridgeID {
		t.Fatalf("keys are per tenant: %v", err)
	}
}

// ---- submit / external checks ------------------------------------------------

func TestSubmitHappyPath(t *testing.T) {
	e := newEnv(t)
	b := e.pending(t)
	if b.Status != domain.StatusPendingApproval || b.ConfigFingerprint == "" || b.CredentialVerifiedAt == nil {
		t.Fatalf("%+v", b)
	}
	if len(e.vault.seen) != 1 || e.vault.seen[0].PrincipalID != "maker" || e.vault.seen[0].TenantID != tenantA {
		t.Fatalf("the vault must be asked on behalf of the person submitting: %+v", e.vault.seen)
	}
	changes, _ := e.mem.ListChanges(ctxOf(tenantA), b.BridgeID)
	if len(changes) != 2 || changes[0].Kind != domain.ChangeCreate || changes[1].Kind != domain.ChangeSubmit {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestSubmitRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(*env, *domain.ApiBridge)
		want   error
	}{
		"profile not published":  {func(e *env, b *domain.ApiBridge) { b.ProviderKey = "ghost-bank" }, ErrProfileUnavailable},
		"profile version absent": {func(e *env, b *domain.ApiBridge) { b.ProviderProfileVersion = 9 }, ErrProfileUnavailable},
		"region not in this deployment": {func(e *env, b *domain.ApiBridge) {
			e.svc.EgressAllowedRegions = []string{"us-east"}
		}, ErrRegionDenied},
		"region not in the profile": {func(e *env, b *domain.ApiBridge) { b.DestinationRegion = "eu-west" }, ErrRegionDenied},
		"no deployment regions configured (fail closed)": {func(e *env, b *domain.ApiBridge) {
			e.svc.EgressAllowedRegions = nil
		}, ErrRegionDenied},
		"scope the profile does not offer": {func(e *env, b *domain.ApiBridge) { b.Scopes = []string{"ingest", "admin.everything"} }, ErrScopeNotSupported},
		"host not on the provider allowlist": {func(e *env, b *domain.ApiBridge) {
			b.EndpointURL = "https://api.evil.example/v2/feed"
			e.dns.hosts["api.evil.example"] = []string{"93.184.216.99"}
		}, ErrHostNotAllowed},
		"destination resolves to a private address": {func(e *env, b *domain.ApiBridge) {
			e.dns.hosts["api.bank-x.example"] = []string{"10.0.0.7"}
		}, ErrEgressDenied},
		"destination resolves to cloud metadata": {func(e *env, b *domain.ApiBridge) {
			e.dns.hosts["api.bank-x.example"] = []string{"169.254.169.254"}
		}, ErrEgressDenied},
		"destination does not resolve": {func(e *env, b *domain.ApiBridge) {
			delete(e.dns.hosts, "api.bank-x.example")
		}, ErrEgressDenied},
		"destination is an internal name": {func(e *env, b *domain.ApiBridge) { b.EndpointURL = "https://erp.example.internal/api" }, ErrEgressDenied},
		"credential unknown to the vault": {func(e *env, b *domain.ApiBridge) { b.CredentialRef = credGone }, ErrCredentialInvalid},
		"vault refuses the caller": {func(e *env, b *domain.ApiBridge) {
			e.vault.refs[credOK] = vault.ErrForbidden
		}, ErrCredentialInvalid},
		"vault down (fail closed)": {func(e *env, b *domain.ApiBridge) { e.vault.down = true }, ErrDependencyUnavailable},
		"incomplete":               {func(e *env, b *domain.ApiBridge) { b.Purpose = "" }, domain.ErrIncompleteConfig},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			b := e.created(t)
			// change the stored connector the way an editor would, then check at submit
			cfg := func(b *domain.ApiBridge) {}
			_ = cfg
			mut := *b
			tc.mutate(e, &mut)
			if mut.ProviderKey != b.ProviderKey || mut.ProviderProfileVersion != b.ProviderProfileVersion ||
				mut.DestinationRegion != b.DestinationRegion || mut.EndpointURL != b.EndpointURL ||
				mut.CredentialRef != b.CredentialRef || mut.Purpose != b.Purpose || strings.Join(mut.Scopes, ",") != strings.Join(b.Scopes, ",") {
				p := Patch{EndpointURL: &mut.EndpointURL, ProviderKey: &mut.ProviderKey, ProviderProfileVersion: &mut.ProviderProfileVersion,
					DestinationRegion: &mut.DestinationRegion, CredentialRef: &mut.CredentialRef, Purpose: &mut.Purpose, Scopes: &mut.Scopes}
				var err error
				b, err = e.svc.Update(ctxOf(tenantA), maker, b.BridgeID, b.Version, p)
				if err != nil {
					t.Fatalf("update: %v", err)
				}
			}
			_, err := e.svc.Submit(ctxOf(tenantA), tenantA, maker, b.BridgeID, b.Version)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if got := get(t, e, tenantA, b.BridgeID); got.Status != domain.StatusDraft {
				t.Fatalf("a refused submit must leave the draft a draft: %s", got.Status)
			}
		})
	}
}

func TestSubmitWithAuthNoneNeedsNoCredentialAndNoVault(t *testing.T) {
	e := newEnv(t)
	e.vault.down = true // would fail closed if consulted
	in := goodRequest()
	in.AuthType, in.CredentialRef = "NONE", ""
	b, _, err := e.svc.Create(ctxOf(tenantA), maker, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Submit(ctxOf(tenantA), tenantA, maker, b.BridgeID, b.Version); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestSubmitWrongStateAndStaleVersion(t *testing.T) {
	e := newEnv(t)
	b := e.pending(t)
	if _, err := e.svc.Submit(ctxOf(tenantA), tenantA, maker, b.BridgeID, b.Version); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("submit a pending connector: %v", err)
	}
	d := e.created(t)
	if _, err := e.svc.Submit(ctxOf(tenantA), tenantA, maker, d.BridgeID, d.Version+5); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("stale version: %v", err)
	}
}

// ---- approval -----------------------------------------------------------------

func TestApprovalNeedsADifferentPerson_SoD(t *testing.T) {
	e := newEnv(t)
	b := e.pending(t)
	if _, err := e.svc.Approve(ctxOf(tenantA), tenantA, maker, b.BridgeID, b.Version, ""); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("self-approval: %v", err)
	}
	if _, err := e.svc.Approve(ctxOf(tenantA), tenantA, Actor{}, b.BridgeID, b.Version, ""); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("anonymous approval: %v", err)
	}
	if got := get(t, e, tenantA, b.BridgeID); got.Status != domain.StatusPendingApproval || got.ApprovedBy != "" {
		t.Fatal("a refused approval must change nothing")
	}
}

func TestApprovalIsRecordedAgainstTheExactConfiguration(t *testing.T) {
	e := newEnv(t)
	b := e.approved(t)
	if b.ApprovedBy != "checker" || b.ApprovedFingerprint == "" || b.ApprovedFingerprint != b.ConfigFingerprint {
		t.Fatalf("%+v", b)
	}
	approvals, _ := e.mem.ListApprovals(ctxOf(tenantA), b.BridgeID)
	if len(approvals) != 1 || approvals[0].Decision != domain.DecisionApproved || approvals[0].RequestedBy != "maker" ||
		approvals[0].DecidedBy != "checker" || approvals[0].Fingerprint != b.ApprovedFingerprint {
		t.Fatalf("%+v", approvals)
	}
	if e.vault.seen[len(e.vault.seen)-1].PrincipalID != "checker" {
		t.Fatal("the credential reference is re-confirmed on behalf of the approver")
	}
}

func TestApprovalRechecksTheWorldAsItIsNow_INT_NP_018(t *testing.T) {
	// DNS moves to a private address between submission and approval.
	e := newEnv(t)
	b := e.pending(t)
	e.dns.hosts["api.bank-x.example"] = []string{"10.1.1.1"}
	if _, err := e.svc.Approve(ctxOf(tenantA), tenantA, checker, b.BridgeID, b.Version, ""); !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("rebinding at approval: %v", err)
	}
	// the profile is deprecated between submission and approval
	e = newEnv(t)
	b = e.pending(t)
	if _, err := e.svc.SetProfileStatus(ctxOf(store.PlatformScopeID), store.PlatformScopeID, admin, "bank-x", 2, domain.ProfileDeprecated); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Approve(ctxOf(tenantA), tenantA, checker, b.BridgeID, b.Version, ""); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("approval against a deprecated profile: %v", err)
	}
	// the credential is retired between submission and approval
	e = newEnv(t)
	b = e.pending(t)
	delete(e.vault.refs, credOK)
	if _, err := e.svc.Approve(ctxOf(tenantA), tenantA, checker, b.BridgeID, b.Version, ""); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("approval of a retired credential: %v", err)
	}
	// and a vault outage blocks approval rather than waving it through
	e = newEnv(t)
	b = e.pending(t)
	e.vault.down = true
	if _, err := e.svc.Approve(ctxOf(tenantA), tenantA, checker, b.BridgeID, b.Version, ""); !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("vault outage: %v", err)
	}
}

func TestRejectReturnsToDraftWithReasonAndEvidence(t *testing.T) {
	e := newEnv(t)
	b := e.pending(t)
	if _, err := e.svc.Reject(ctxOf(tenantA), checker, b.BridgeID, b.Version, ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("a rejection needs a reason: %v", err)
	}
	if _, err := e.svc.Reject(ctxOf(tenantA), maker, b.BridgeID, b.Version, "no"); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("self-rejection: %v", err)
	}
	r, err := e.svc.Reject(ctxOf(tenantA), checker, b.BridgeID, b.Version, "destination not agreed")
	if err != nil || r.Status != domain.StatusDraft || r.StatusReason != "destination not agreed" {
		t.Fatalf("%v %+v", err, r)
	}
	a, _ := e.mem.ListApprovals(ctxOf(tenantA), b.BridgeID)
	if len(a) != 1 || a[0].Decision != domain.DecisionRejected {
		t.Fatalf("%+v", a)
	}
}

// ---- activation and use --------------------------------------------------------

func TestActivateAndIngest(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	if b.Status != domain.StatusActive {
		t.Fatalf("%s", b.Status)
	}
	log, err := e.svc.Ingest(ctxOf(tenantA), maker, b.BridgeID, IngestInput{PayloadSummary: "batch 994"})
	if err != nil || log.LogID == "" || log.IngestionStatus != domain.IngestionSuccess {
		t.Fatalf("%v %+v", err, log)
	}
	types := strings.Join(e.mem.EventTypes(), ",")
	for _, want := range []string{"bridge.created", "connector.approval-requested", "connector.approved", "connector.activated", "bridge.ingested"} {
		if !strings.Contains(types, want) {
			t.Errorf("missing event %s in %s", want, types)
		}
	}
}

func TestIngestRefusedUnlessLive(t *testing.T) {
	e := newEnv(t)
	d := e.created(t)
	if _, err := e.svc.Ingest(ctxOf(tenantA), maker, d.BridgeID, IngestInput{}); !errors.Is(err, ErrNotLive) {
		t.Fatalf("draft: %v", err)
	}
	p := e.pending(t)
	_ = p
	b := e.active(t)
	if _, err := e.svc.Suspend(ctxOf(tenantA), maker, b.BridgeID, b.Version, "maintenance"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Ingest(ctxOf(tenantA), maker, b.BridgeID, IngestInput{}); !errors.Is(err, ErrNotLive) {
		t.Fatalf("suspended: %v", err)
	}
}

func TestIngestEnforcesApprovedScopes_INT_NP_021(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	if _, err := e.svc.Ingest(ctxOf(tenantA), maker, b.BridgeID, IngestInput{Scope: "statements.read"}); err != nil {
		t.Fatalf("an approved scope: %v", err)
	}
	if _, err := e.svc.Ingest(ctxOf(tenantA), maker, b.BridgeID, IngestInput{Scope: "payments.write"}); !errors.Is(err, ErrScopeDenied) {
		t.Fatalf("a scope that was never approved: %v", err)
	}
	if n := len(mustLogs(t, e, b.BridgeID)); n != 1 {
		t.Fatalf("a refused ingestion must not be logged as ingested: %d", n)
	}
}

func mustLogs(t *testing.T, e *env, id string) []domain.IngestionLog {
	t.Helper()
	l, err := e.mem.ListIngestionLogs(ctxOf(tenantA), id)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestActivateFailsWithoutApprovalAndRevalidates(t *testing.T) {
	e := newEnv(t)
	p := e.pending(t)
	if _, err := e.svc.Activate(ctxOf(tenantA), tenantA, checker, p.BridgeID, p.Version); !errors.Is(err, domain.ErrNotApproved) {
		t.Fatalf("activation before approval: %v", err)
	}
	a := e.approved(t)
	delete(e.vault.refs, credOK)
	if _, err := e.svc.Activate(ctxOf(tenantA), tenantA, checker, a.BridgeID, a.Version); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("activation of a connector whose credential was retired: %v", err)
	}
	if get(t, e, tenantA, a.BridgeID).Live() {
		t.Fatal("it must not be live")
	}
}

// ---- protected changes -----------------------------------------------------------

func TestProtectedChangeStopsALiveConnectorUntilReapproved_INT_NP_021_023(t *testing.T) {
	newScopes := []string{"ingest", "statements.read", "payments.write"}
	newURL := "https://feeds.bank-x.example/v2/feed"
	newRegion := "eu-west"
	newPurpose := "payroll-export"
	for name, p := range map[string]Patch{
		"scope widened":     {Scopes: &newScopes},
		"destination moved": {EndpointURL: &newURL},
		"region moved":      {DestinationRegion: &newRegion},
		"purpose changed":   {Purpose: &newPurpose},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			b := e.active(t)
			updated, err := e.svc.Update(ctxOf(tenantA), editor, b.BridgeID, b.Version, p)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Live() || updated.Status != domain.StatusDraft || updated.ApprovedFingerprint != "" {
				t.Fatalf("a protected change left the connector %s with an approval: %+v", updated.Status, updated)
			}
			if _, err := e.svc.Ingest(ctxOf(tenantA), maker, b.BridgeID, IngestInput{}); !errors.Is(err, ErrNotLive) {
				t.Fatalf("a changed connector kept moving data: %v", err)
			}
			if _, err := e.svc.Activate(ctxOf(tenantA), tenantA, checker, b.BridgeID, updated.Version); err == nil {
				t.Fatal("activated without a new approval")
			}
			changes, _ := e.mem.ListChanges(ctxOf(tenantA), b.BridgeID)
			last := changes[len(changes)-1]
			if last.Kind != domain.ChangeUpdate || last.ChangedBy != "editor" || len(last.ChangedFields) == 0 {
				t.Fatalf("the change must be on the record: %+v", last)
			}
		})
	}
}

func TestScopeWideningNamesTheScopesAndRaisesAnEvent(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	wider := []string{"ingest", "statements.read", "payments.write"}
	if _, err := e.svc.Update(ctxOf(tenantA), editor, b.BridgeID, b.Version, Patch{Scopes: &wider}); err != nil {
		t.Fatal(err)
	}
	changes, _ := e.mem.ListChanges(ctxOf(tenantA), b.BridgeID)
	last := changes[len(changes)-1]
	if len(last.ScopesAdded) != 1 || last.ScopesAdded[0] != "payments.write" || len(last.ScopesRemoved) != 0 {
		t.Fatalf("%+v", last)
	}
	if !strings.Contains(strings.Join(e.mem.EventTypes(), ","), "connector.scope-changed") {
		t.Fatalf("events = %v", e.mem.EventTypes())
	}
}

func TestUpdateRenameDoesNotNeedReapprovalAndKeepsSoD(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	name := "Renamed connector"
	u, err := e.svc.Update(ctxOf(tenantA), editor, b.BridgeID, b.Version, Patch{BridgeName: &name})
	if err != nil {
		t.Fatal(err)
	}
	if !u.Live() || u.BridgeName != name || u.ApprovedFingerprint != b.ApprovedFingerprint || u.Version != b.Version+1 {
		t.Fatalf("a rename is not a protected change: %+v", u)
	}
	if u.LastChangedBy != b.LastChangedBy {
		t.Fatal("a rename must not reset who last changed the configuration")
	}
}

func TestUpdateNoOpAndConflictAndSecrets(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	same := b.Purpose
	u, err := e.svc.Update(ctxOf(tenantA), editor, b.BridgeID, b.Version, Patch{Purpose: &same})
	if err != nil || u.Version != b.Version || !u.Live() {
		t.Fatalf("a patch that changes nothing must not disturb an approved connector: %v %+v", err, u)
	}
	other := "other-purpose"
	if _, err := e.svc.Update(ctxOf(tenantA), editor, b.BridgeID, b.Version+9, Patch{Purpose: &other}); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("stale version: %v", err)
	}
	bad := "https://u:p@api.bank-x.example/x"
	if _, err := e.svc.Update(ctxOf(tenantA), editor, b.BridgeID, b.Version, Patch{EndpointURL: &bad}); !errors.Is(err, domain.ErrIncompleteConfig) {
		t.Fatalf("a secret in an update: %v", err)
	}
	if !get(t, e, tenantA, b.BridgeID).Live() {
		t.Fatal("a refused update must not disturb the connector")
	}
}

func TestConcurrentUpdatesExactlyOneWins(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	var ok, conflict int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := "purpose-" + strings.Repeat("x", i+1)
			_, err := e.svc.Update(ctxOf(tenantA), editor, b.BridgeID, b.Version, Patch{Purpose: &p})
			switch {
			case err == nil:
				atomic.AddInt32(&ok, 1)
			case errors.Is(err, domain.ErrVersionConflict):
				atomic.AddInt32(&conflict, 1)
			default:
				t.Errorf("%v", err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 || conflict != 19 {
		t.Fatalf("optimistic concurrency: %d won, %d conflicted", ok, conflict)
	}
}

// ---- suspend / revoke / archive -----------------------------------------------

func TestSuspendResumeRevokeArchive(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	if _, err := e.svc.Suspend(ctxOf(tenantA), maker, b.BridgeID, b.Version, ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("a reason is required: %v", err)
	}
	s, err := e.svc.Suspend(ctxOf(tenantA), maker, b.BridgeID, b.Version, "investigation")
	if err != nil || s.Status != domain.StatusSuspended || s.StatusReason != "investigation" {
		t.Fatalf("%v %+v", err, s)
	}
	r, err := e.svc.Activate(ctxOf(tenantA), tenantA, maker, b.BridgeID, s.Version)
	if err != nil || !r.Live() {
		t.Fatalf("resume with the approval intact: %v", err)
	}
	v, err := e.svc.Revoke(ctxOf(tenantA), maker, b.BridgeID, r.Version, "consent withdrawn")
	if err != nil || v.Status != domain.StatusRevoked {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.Activate(ctxOf(tenantA), tenantA, maker, b.BridgeID, v.Version); err == nil {
		t.Fatal("a revoked connector was reactivated")
	}
	other := "x-purpose"
	if _, err := e.svc.Update(ctxOf(tenantA), editor, b.BridgeID, v.Version, Patch{Purpose: &other}); !errors.Is(err, domain.ErrConnectorTerminal) {
		t.Fatalf("a revoked connector was changed: %v", err)
	}
	ar, err := e.svc.Archive(ctxOf(tenantA), maker, b.BridgeID, v.Version)
	if err != nil || ar.Status != domain.StatusArchived {
		t.Fatalf("%v", err)
	}
	types := strings.Join(e.mem.EventTypes(), ",")
	for _, want := range []string{"connector.suspended", "connector.revoked", "connector.archived"} {
		if !strings.Contains(types, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// ---- consent / profile ---------------------------------------------------------

func TestConsentExpiryDegradesAndRefusesIngestion_INT_NP_020(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	exp := e.now.Add(48 * time.Hour)
	b, err := e.svc.RefreshConsent(ctxOf(tenantA), tenantA, maker, b.BridgeID, b.Version, nil, &exp)
	if err != nil || !b.Live() || b.ConsentExpiresAt == nil {
		t.Fatalf("extending consent on the same reference needs no approval: %v %+v", err, b)
	}
	if _, err := e.svc.Ingest(ctxOf(tenantA), maker, b.BridgeID, IngestInput{}); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	*e.now = e.now.Add(72 * time.Hour) // consent lapses
	if _, err := e.svc.Ingest(ctxOf(tenantA), maker, b.BridgeID, IngestInput{}); !errors.Is(err, ErrCredentialExpired) {
		t.Fatalf("after expiry: %v", err)
	}
	got := get(t, e, tenantA, b.BridgeID)
	if got.Status != domain.StatusDegraded {
		t.Fatalf("the connector must be degraded, is %s", got.Status)
	}
	h, _ := e.mem.GetHealth(ctxOf(tenantA), b.BridgeID)
	if h.State != domain.HealthDegraded || h.ReasonCode != domain.ReasonCredentialExpired {
		t.Fatalf("%+v", h)
	}
	tr, _ := e.mem.ListHealthTransitions(ctxOf(tenantA), b.BridgeID, 10)
	if len(tr) == 0 || tr[0].ToState != domain.HealthDegraded {
		t.Fatalf("transitions %+v", tr)
	}
	if !strings.Contains(strings.Join(e.mem.EventTypes(), ","), "connector.degraded") {
		t.Fatalf("events = %v", e.mem.EventTypes())
	}
	// a poll that "succeeds" does not paper over a lapsed credential
	if _, err := e.svc.ReportHealth(ctxOf(tenantA), maker, b.BridgeID, HealthReport{Success: true}); err != nil {
		t.Fatal(err)
	}
	if get(t, e, tenantA, b.BridgeID).Status != domain.StatusDegraded {
		t.Fatal("a lapsed credential recovers by refresh, not by polling")
	}
	// refreshing the consent recovers it
	exp2 := e.now.Add(24 * time.Hour)
	b2, err := e.svc.RefreshConsent(ctxOf(tenantA), tenantA, maker, b.BridgeID, get(t, e, tenantA, b.BridgeID).Version, nil, &exp2)
	if err != nil || b2.Status != domain.StatusActive {
		t.Fatalf("%v %+v", err, b2)
	}
	if _, err := e.svc.Ingest(ctxOf(tenantA), maker, b.BridgeID, IngestInput{}); err != nil {
		t.Fatalf("after refresh: %v", err)
	}
}

func TestRefreshWithADifferentReferenceNeedsReapproval(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	ref := credOK2
	u, err := e.svc.RefreshConsent(ctxOf(tenantA), tenantA, maker, b.BridgeID, b.Version, &ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u.Live() || u.CredentialRef != credOK2 {
		t.Fatalf("pointing at another credential is a protected change: %+v", u)
	}
	bad := credGone
	if _, err := e.svc.RefreshConsent(ctxOf(tenantA), tenantA, maker, b.BridgeID, u.Version, &bad, nil); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("%v", err)
	}
	notID := "sk_live_whatever"
	if _, err := e.svc.RefreshConsent(ctxOf(tenantA), tenantA, maker, b.BridgeID, u.Version, &notID, nil); !errors.Is(err, domain.ErrIncompleteConfig) {
		t.Fatalf("a secret offered as a reference: %v", err)
	}
	past := e.now.Add(-time.Hour)
	if _, err := e.svc.RefreshConsent(ctxOf(tenantA), tenantA, maker, b.BridgeID, u.Version, nil, &past); !errors.Is(err, ErrValidation) {
		t.Fatalf("an expiry in the past: %v", err)
	}
	if _, err := e.svc.RefreshConsent(ctxOf(tenantA), tenantA, maker, b.BridgeID, u.Version, nil, nil); !errors.Is(err, ErrValidation) {
		t.Fatalf("nothing to refresh: %v", err)
	}
}

func TestProfileWithdrawnDegradesAndRefuses_INT_NP_022(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	pctx := ctxOf(store.PlatformScopeID)
	if _, err := e.svc.SetProfileStatus(pctx, store.PlatformScopeID, admin, "bank-x", 2, domain.ProfileDeprecated); err != nil {
		t.Fatal(err)
	}
	// deprecated: still served, but flagged for upgrade, once
	for i := 0; i < 3; i++ {
		if _, err := e.svc.Ingest(ctxOf(tenantA), maker, b.BridgeID, IngestInput{}); err != nil {
			t.Fatalf("a deprecated profile is still served: %v", err)
		}
	}
	n := 0
	for _, t := range e.mem.EventTypes() {
		if t == "connector.profile-upgrade-required" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("upgrade_required must be raised once, not per request: %d", n)
	}
	ps, err := e.svc.ProfileStatusOf(ctxOf(tenantA), b.BridgeID)
	if err != nil || !ps.UpgradeRequired || ps.Status != domain.ProfileDeprecated {
		t.Fatalf("%v %+v", err, ps)
	}
	// unsupported: refused, no schema guessing (the window has run out)
	*e.now = e.now.Add(DeprecationWindow + time.Hour)
	if _, err := e.svc.SetProfileStatus(pctx, store.PlatformScopeID, admin, "bank-x", 2, domain.ProfileUnsupported); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Ingest(ctxOf(tenantA), maker, b.BridgeID, IngestInput{}); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("an unsupported profile: %v", err)
	}
	h, _ := e.mem.GetHealth(ctxOf(tenantA), b.BridgeID)
	if h.ReasonCode != domain.ReasonProfileUnsupported {
		t.Fatalf("%+v", h)
	}
}

func TestUpgradeProfileIsAProtectedChange(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	e.publish(t, ctxOf(store.PlatformScopeID), "bank-x", 3, []string{"uk"})
	if _, err := e.svc.UpgradeProfile(ctxOf(tenantA), maker, b.BridgeID, b.Version, 2); !errors.Is(err, ErrValidation) {
		t.Fatalf("not an upgrade: %v", err)
	}
	if _, err := e.svc.UpgradeProfile(ctxOf(tenantA), maker, b.BridgeID, b.Version, 9); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("no such version: %v", err)
	}
	u, err := e.svc.UpgradeProfile(ctxOf(tenantA), maker, b.BridgeID, b.Version, 3)
	if err != nil {
		t.Fatal(err)
	}
	if u.Live() || u.ProviderProfileVersion != 3 || u.ApprovedFingerprint != "" {
		t.Fatalf("an upgrade is never silent: %+v", u)
	}
	// an uncertified version cannot be pinned
	pctx := ctxOf(store.PlatformScopeID)
	if err := e.svc.PublishProfile(pctx, store.PlatformScopeID, admin, &domain.Profile{ProviderKey: "bank-x", ProviderType: "BANK", Version: 4,
		AllowedRegions: []string{"uk"}, AllowedHostSuffixes: []string{"bank-x.example"}, SupportedScopes: []string{"ingest"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpgradeProfile(ctxOf(tenantA), editor, b.BridgeID, u.Version, 4); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("a DRAFT profile: %v", err)
	}
}

// ---- health --------------------------------------------------------------------

func TestHealthThresholdAndRecovery(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	for i := 1; i < domain.FailureThreshold; i++ {
		h, err := e.svc.ReportHealth(ctxOf(tenantA), maker, b.BridgeID, HealthReport{Reason: domain.ReasonProviderOutage, Detail: "timeout"})
		if err != nil || h.State != domain.HealthHealthy || h.ConsecutiveFailures != i {
			t.Fatalf("failure %d below the threshold: %v %+v", i, err, h)
		}
	}
	if get(t, e, tenantA, b.BridgeID).Status != domain.StatusActive {
		t.Fatal("below the threshold the connector stays active")
	}
	h, _ := e.svc.ReportHealth(ctxOf(tenantA), maker, b.BridgeID, HealthReport{Reason: domain.ReasonProviderOutage})
	if h.State != domain.HealthDegraded || get(t, e, tenantA, b.BridgeID).Status != domain.StatusDegraded {
		t.Fatalf("at the threshold it degrades: %+v", h)
	}
	if got := get(t, e, tenantA, b.BridgeID); !got.Live() {
		t.Fatal("a degraded connector still moves data")
	}
	h, err := e.svc.ReportHealth(ctxOf(tenantA), maker, b.BridgeID, HealthReport{Success: true})
	if err != nil || h.State != domain.HealthHealthy || get(t, e, tenantA, b.BridgeID).Status != domain.StatusActive {
		t.Fatalf("a healthy report recovers an outage degradation: %v %+v", err, h)
	}
	if _, err := e.svc.ReportHealth(ctxOf(tenantA), maker, b.BridgeID, HealthReport{Reason: "MADE_UP"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown reason: %v", err)
	}
}

func TestCredentialRevokedMidSyncSuspendsAndKeepsTheCheckpoint_INT_NP_020(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	cp, err := e.svc.AdvanceCheckpoint(ctxOf(tenantA), maker, b.BridgeID, "statements", "cursor-100", ptrTime(e.now.Add(-time.Hour)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReportHealth(ctxOf(tenantA), maker, b.BridgeID, HealthReport{Reason: domain.ReasonCredentialRevoked}); err != nil {
		t.Fatal(err)
	}
	got := get(t, e, tenantA, b.BridgeID)
	if got.Status != domain.StatusSuspended {
		t.Fatalf("revoked authority must stop the connector: %s", got.Status)
	}
	if _, err := e.svc.Ingest(ctxOf(tenantA), maker, b.BridgeID, IngestInput{}); !errors.Is(err, ErrNotLive) {
		t.Fatalf("no unauthorized continuation: %v", err)
	}
	if _, err := e.svc.AdvanceCheckpoint(ctxOf(tenantA), maker, b.BridgeID, "statements", "cursor-200", ptrTime(*e.now), cp.Version); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a suspended connector must not advance its checkpoint: %v", err)
	}
	cps, _ := e.mem.ListCheckpoints(ctxOf(tenantA), b.BridgeID)
	if len(cps) != 1 || cps[0].Cursor != "cursor-100" || cps[0].Version != 1 {
		t.Fatalf("the checkpoint must be exactly as it was: %+v", cps)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// ---- checkpoints ---------------------------------------------------------------

func TestCheckpointAdvanceRules(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	ctx := ctxOf(tenantA)
	w1, w2, w0 := e.now.Add(-3*time.Hour), e.now.Add(-2*time.Hour), e.now.Add(-5*time.Hour)

	cp, err := e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "statements", "c1", &w1, 0)
	if err != nil || cp.Version != 1 {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "statements", "c1b", &w2, 0); !errors.Is(err, domain.ErrCheckpointConflict) {
		t.Fatalf("creating an existing stream: %v", err)
	}
	cp, err = e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "statements", "c2", &w2, 1)
	if err != nil || cp.Version != 2 || cp.Cursor != "c2" {
		t.Fatalf("%v %+v", err, cp)
	}
	if _, err := e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "statements", "c3", &w2, 1); !errors.Is(err, domain.ErrCheckpointConflict) {
		t.Fatalf("a stale writer: %v", err)
	}
	if _, err := e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "statements", "old", &w0, 2); !errors.Is(err, domain.ErrCheckpointRegress) {
		t.Fatalf("a watermark going backwards: %v", err)
	}
	if _, err := e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "statements", "nowm", nil, 2); !errors.Is(err, domain.ErrCheckpointRegress) {
		t.Fatalf("dropping the watermark is going backwards: %v", err)
	}
	if _, err := e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "Bad Stream!", "c", nil, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "s", strings.Repeat("x", 3000), nil, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("%v", err)
	}
	if got, _ := e.mem.ListCheckpoints(ctx, b.BridgeID); got[0].Cursor != "c2" || got[0].Version != 2 {
		t.Fatalf("refused advances must change nothing: %+v", got)
	}
	// another tenant cannot touch it
	if _, err := e.svc.AdvanceCheckpoint(ctxOf(tenantB), maker, b.BridgeID, "statements", "x", &w2, 2); !errors.Is(err, domain.ErrBridgeNotFound) {
		t.Fatalf("cross-tenant: %v", err)
	}
}

func TestCheckpointResetNeedsASecondPerson(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	ctx := ctxOf(tenantA)
	w1, w2 := e.now.Add(-3*time.Hour), e.now.Add(-2*time.Hour)
	_, _ = e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "statements", "c1", &w1, 0)
	_, _ = e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "statements", "c2", &w2, 1)

	if _, err := e.svc.RequestCheckpointReset(ctx, maker, b.BridgeID, "nope", "c0", &w1, "replay"); !errors.Is(err, domain.ErrCheckpointNotFound) {
		t.Fatalf("a stream that does not exist: %v", err)
	}
	if _, err := e.svc.RequestCheckpointReset(ctx, maker, b.BridgeID, "statements", "c1", &w1, ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("a reason is required: %v", err)
	}
	req, err := e.svc.RequestCheckpointReset(ctx, maker, b.BridgeID, "statements", "c1", &w1, "provider re-sent a batch")
	if err != nil || req.Status != domain.ResetPending {
		t.Fatalf("%v", err)
	}
	if cps, _ := e.mem.ListCheckpoints(ctx, b.BridgeID); cps[0].Cursor != "c2" {
		t.Fatal("requesting a reset must not move the checkpoint")
	}
	if _, err := e.svc.DecideCheckpointReset(ctx, maker, req.RequestID, true); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("self-approved reset: %v", err)
	}
	if _, err := e.svc.DecideCheckpointReset(ctxOf(tenantB), checker, req.RequestID, true); !errors.Is(err, domain.ErrResetNotFound) {
		t.Fatalf("another tenant's reset: %v", err)
	}
	done, err := e.svc.DecideCheckpointReset(ctx, checker, req.RequestID, true)
	if err != nil || done.Status != domain.ResetApplied || done.DecidedBy != "checker" {
		t.Fatalf("%v %+v", err, done)
	}
	cps, _ := e.mem.ListCheckpoints(ctx, b.BridgeID)
	if cps[0].Cursor != "c1" || cps[0].Version != 3 {
		t.Fatalf("the approved reset moves it back AND bumps the version: %+v", cps[0])
	}
	if _, err := e.svc.DecideCheckpointReset(ctx, checker, req.RequestID, true); !errors.Is(err, domain.ErrResetNotPending) {
		t.Fatalf("deciding twice: %v", err)
	}
	// a rejected reset moves nothing
	r2, _ := e.svc.RequestCheckpointReset(ctx, maker, b.BridgeID, "statements", "c0", nil, "x")
	if rej, err := e.svc.DecideCheckpointReset(ctx, checker, r2.RequestID, false); err != nil || rej.Status != domain.ResetRejected {
		t.Fatalf("%v", err)
	}
	if cps, _ := e.mem.ListCheckpoints(ctx, b.BridgeID); cps[0].Cursor != "c1" || cps[0].Version != 3 {
		t.Fatalf("%+v", cps[0])
	}
}

// ---- profiles ------------------------------------------------------------------

func TestOnlyThePlatformScopePublishesProfiles(t *testing.T) {
	e := newEnv(t)
	p := &domain.Profile{ProviderKey: "bank-z", ProviderType: "BANK", Version: 1, AllowedRegions: []string{"uk"},
		AllowedHostSuffixes: []string{"bank-z.example"}, SupportedScopes: []string{"ingest"}}
	if err := e.svc.PublishProfile(ctxOf(tenantA), tenantA, maker, p); !errors.Is(err, ErrNotPlatformScope) {
		t.Fatalf("a tenant publishing a profile: %v", err)
	}
	if _, err := e.svc.SetProfileStatus(ctxOf(tenantA), tenantA, maker, "bank-x", 2, domain.ProfileDeprecated); !errors.Is(err, ErrNotPlatformScope) {
		t.Fatalf("a tenant deprecating a profile: %v", err)
	}
	pctx := ctxOf(store.PlatformScopeID)
	if err := e.svc.PublishProfile(pctx, store.PlatformScopeID, admin, p); err != nil {
		t.Fatal(err)
	}
	dup := *p
	if err := e.svc.PublishProfile(pctx, store.PlatformScopeID, admin, &dup); !errors.Is(err, domain.ErrProfileExists) {
		t.Fatalf("a profile version is never replaced: %v", err)
	}
}

func TestProfileValidationAndForwardOnlyStatus(t *testing.T) {
	e := newEnv(t)
	pctx := ctxOf(store.PlatformScopeID)
	for name, mutate := range map[string]func(*domain.Profile){
		"no regions":       func(p *domain.Profile) { p.AllowedRegions = nil },
		"no hosts":         func(p *domain.Profile) { p.AllowedHostSuffixes = nil },
		"wildcard host":    func(p *domain.Profile) { p.AllowedHostSuffixes = []string{"*.example"} },
		"bare tld":         func(p *domain.Profile) { p.AllowedHostSuffixes = []string{"com"} },
		"host with a path": func(p *domain.Profile) { p.AllowedHostSuffixes = []string{"bank-z.example/x"} },
		"no scopes":        func(p *domain.Profile) { p.SupportedScopes = nil },
		"unknown type":     func(p *domain.Profile) { p.ProviderType = "SPACESHIP" },
		"version zero":     func(p *domain.Profile) { p.Version = 0 },
	} {
		p := &domain.Profile{ProviderKey: "bank-z", ProviderType: "BANK", Version: 1, AllowedRegions: []string{"uk"},
			AllowedHostSuffixes: []string{"bank-z.example"}, SupportedScopes: []string{"ingest"}}
		mutate(p)
		if err := e.svc.PublishProfile(pctx, store.PlatformScopeID, admin, p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := e.svc.SetProfileStatus(pctx, store.PlatformScopeID, admin, "bank-x", 2, domain.ProfileDraft); !errors.Is(err, ErrValidation) {
		t.Fatalf("DRAFT is not a target status: %v", err)
	}
	_, _ = e.svc.SetProfileStatus(pctx, store.PlatformScopeID, admin, "bank-x", 2, domain.ProfileDeprecated)
	if _, err := e.svc.SetProfileStatus(pctx, store.PlatformScopeID, admin, "bank-x", 2, domain.ProfileCertified); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("a profile never moves backwards: %v", err)
	}
	if _, err := e.svc.SetProfileStatus(pctx, store.PlatformScopeID, admin, "bank-x", 77, domain.ProfileDeprecated); !errors.Is(err, domain.ErrProfileNotFound) {
		t.Fatalf("%v", err)
	}
}

// ---- tenant isolation ------------------------------------------------------------

func TestOtherTenantsCannotSeeOrChangeAConnector(t *testing.T) {
	e := newEnv(t)
	b := e.active(t)
	cb := ctxOf(tenantB)
	name := "hijack"
	checks := map[string]func() error{
		"get": func() error { _, err := e.mem.GetBridgeByID(cb, b.BridgeID); return err },
		"update": func() error {
			_, err := e.svc.Update(cb, editor, b.BridgeID, b.Version, Patch{BridgeName: &name})
			return err
		},
		"submit":  func() error { _, err := e.svc.Submit(cb, tenantB, maker, b.BridgeID, b.Version); return err },
		"approve": func() error { _, err := e.svc.Approve(cb, tenantB, checker, b.BridgeID, b.Version, ""); return err },
		"suspend": func() error { _, err := e.svc.Suspend(cb, maker, b.BridgeID, b.Version, "x"); return err },
		"revoke":  func() error { _, err := e.svc.Revoke(cb, maker, b.BridgeID, b.Version, "x"); return err },
		"ingest":  func() error { _, err := e.svc.Ingest(cb, maker, b.BridgeID, IngestInput{}); return err },
		"health": func() error {
			_, err := e.svc.ReportHealth(cb, maker, b.BridgeID, HealthReport{Success: true})
			return err
		},
		"changes": func() error { _, err := e.mem.ListChanges(cb, b.BridgeID); return err },
		"profile": func() error { _, err := e.svc.ProfileStatusOf(cb, b.BridgeID); return err },
	}
	for name, f := range checks {
		if err := f(); !errors.Is(err, domain.ErrBridgeNotFound) {
			t.Errorf("%s: got %v, want not found (a distinct 'forbidden' would confirm the id exists)", name, err)
		}
	}
	if !get(t, e, tenantA, b.BridgeID).Live() || get(t, e, tenantA, b.BridgeID).BridgeName == "hijack" {
		t.Fatal("the owner's connector was disturbed")
	}
}

// ---- grandfathered connectors ------------------------------------------------------

func TestLegacyConnectorKeepsWorkingUntilFirstProtectedChange(t *testing.T) {
	e := newEnv(t)
	legacy := &domain.ApiBridge{LegalEntityID: "le-1", BridgeName: "Old bridge", Protocol: "REST",
		EndpointURL: "https://api.bank-x.example/old", AuthType: "OAUTH2", Status: domain.StatusActive,
		LegacyUnapproved: true, Scopes: []string{"ingest"}, CreatedBy: "legacy", LastChangedBy: "legacy"}
	if err := e.mem.CreateBridge(ctxOf(tenantA), legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Ingest(ctxOf(tenantA), maker, legacy.BridgeID, IngestInput{PayloadSummary: "x"}); err != nil {
		t.Fatalf("a grandfathered connector must keep working: %v", err)
	}
	if _, err := e.svc.Ingest(ctxOf(tenantA), maker, legacy.BridgeID, IngestInput{Scope: "payments.write"}); !errors.Is(err, ErrScopeDenied) {
		t.Fatalf("it has the single scope it always had: %v", err)
	}
	s, err := e.svc.Suspend(ctxOf(tenantA), maker, legacy.BridgeID, legacy.Version, "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	if r, err := e.svc.Activate(ctxOf(tenantA), tenantA, maker, legacy.BridgeID, s.Version); err != nil || !r.Live() {
		t.Fatalf("an operator-suspended grandfathered connector resumes as it was: %v", err)
	}
	purpose := "bank-statement-import"
	got := get(t, e, tenantA, legacy.BridgeID)
	u, err := e.svc.Update(ctxOf(tenantA), editor, legacy.BridgeID, got.Version, Patch{Purpose: &purpose})
	if err != nil {
		t.Fatal(err)
	}
	if u.LegacyUnapproved || u.Live() {
		t.Fatalf("its first protected change puts it through approval: %+v", u)
	}
}

// Provider change management: a deprecation announces a sunset at least DeprecationWindow away, and a profile is not
// withdrawn before it unless the withdrawal is declared an emergency with a reason.
func TestDeprecationWindowIsEnforcedBeforeAProfileIsWithdrawn(t *testing.T) {
	e := newEnv(t)
	pctx := ctxOf(store.PlatformScopeID)
	p := store.PlatformScopeID

	// withdrawing a certified profile outright is refused
	if _, err := e.svc.SetProfileStatus(pctx, p, admin, "bank-x", 2, domain.ProfileUnsupported); !errors.Is(err, ErrDeprecationWindow) {
		t.Fatalf("a certified profile cannot be withdrawn without notice: %v", err)
	}
	// a window shorter than the minimum is refused
	short := e.now.Add(DeprecationWindow - time.Hour)
	if _, err := e.svc.ChangeProfileStatus(pctx, p, admin, "bank-x", 2, ProfileChange{Status: domain.ProfileDeprecated, SunsetAt: &short}); !errors.Is(err, ErrValidation) {
		t.Fatalf("a short deprecation window: %v", err)
	}
	// deprecating with no date announces the minimum window
	dep, err := e.svc.SetProfileStatus(pctx, p, admin, "bank-x", 2, domain.ProfileDeprecated)
	if err != nil || dep.SunsetAt == nil || !dep.SunsetAt.Equal(e.now.Add(DeprecationWindow)) {
		t.Fatalf("a deprecation records its sunset: %v %+v", err, dep)
	}
	// still inside the window: refused, and the profile is still only deprecated
	*e.now = e.now.Add(DeprecationWindow - time.Hour)
	if _, err := e.svc.SetProfileStatus(pctx, p, admin, "bank-x", 2, domain.ProfileUnsupported); !errors.Is(err, ErrDeprecationWindow) {
		t.Fatalf("withdrawn inside its window: %v", err)
	}
	if got, _ := e.svc.Store.GetProfile(pctx, "bank-x", 2); got.Status != domain.ProfileDeprecated {
		t.Fatalf("the refused withdrawal changed the profile: %s", got.Status)
	}
	// an emergency needs a reason, and then is allowed inside the window
	if _, err := e.svc.ChangeProfileStatus(pctx, p, admin, "bank-x", 2, ProfileChange{Status: domain.ProfileUnsupported, Emergency: true}); !errors.Is(err, ErrValidation) {
		t.Fatalf("an emergency without a reason: %v", err)
	}
	if got, err := e.svc.ChangeProfileStatus(pctx, p, admin, "bank-x", 2, ProfileChange{Status: domain.ProfileUnsupported, Emergency: true, Reason: "provider disclosed a signing-key leak"}); err != nil || got.Status != domain.ProfileUnsupported {
		t.Fatalf("an emergency withdrawal: %v %+v", err, got)
	}
}

func TestAWithdrawalAfterTheWindowIsAllowed(t *testing.T) {
	e := newEnv(t)
	pctx := ctxOf(store.PlatformScopeID)
	if _, err := e.svc.SetProfileStatus(pctx, store.PlatformScopeID, admin, "bank-x", 2, domain.ProfileDeprecated); err != nil {
		t.Fatal(err)
	}
	*e.now = e.now.Add(DeprecationWindow)
	if got, err := e.svc.SetProfileStatus(pctx, store.PlatformScopeID, admin, "bank-x", 2, domain.ProfileUnsupported); err != nil || got.Status != domain.ProfileUnsupported {
		t.Fatalf("at the sunset the profile may be withdrawn: %v %+v", err, got)
	}
}
