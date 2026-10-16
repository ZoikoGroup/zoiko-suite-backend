package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

const credRef = "0a000000-0000-4000-8000-000000000001"

func draft() *ApiBridge {
	return &ApiBridge{
		BridgeID: "c1", TenantID: "t1", LegalEntityID: "le1", Status: StatusDraft, Version: 1,
		Protocol: "REST", EndpointURL: "https://api.bank-x.example/v2/feed", AuthType: "OAUTH2",
		Purpose: "bank-statement-import", ProviderKey: "bank-x", ProviderProfileVersion: 2,
		CredentialRef: credRef, DestinationRegion: "uk", SyncMode: SyncPull,
		Scopes: []string{"ingest", "statements.read"}, CreatedBy: "maker", LastChangedBy: "maker",
	}
}

func approved(t *testing.T) *ApiBridge {
	t.Helper()
	b := draft()
	if err := b.Submit("maker", t0); err != nil {
		t.Fatal(err)
	}
	if err := b.Approve("checker", t0); err != nil {
		t.Fatal(err)
	}
	return b
}

func active(t *testing.T) *ApiBridge {
	t.Helper()
	b := approved(t)
	if err := b.Activate(t0); err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- fingerprint -------------------------------------------------------------

func TestFingerprintIsStableAndOrderInsensitiveForScopes(t *testing.T) {
	a, b := draft(), draft()
	b.Scopes = []string{"statements.read", "ingest", "INGEST", " ingest "}
	if a.Protected().Fingerprint() != b.Protected().Fingerprint() {
		t.Fatal("equal configurations must have equal fingerprints regardless of scope order, case or duplicates")
	}
	if !strings.HasPrefix(a.Protected().Fingerprint(), "sha256:") {
		t.Fatal("fingerprint format")
	}
}

func TestFingerprintCoversEveryProtectedField(t *testing.T) {
	base := draft().Protected().Fingerprint()
	for name, mutate := range map[string]func(*ApiBridge){
		"endpoint":       func(b *ApiBridge) { b.EndpointURL = "https://other.bank-x.example/v2/feed" },
		"region":         func(b *ApiBridge) { b.DestinationRegion = "eu-west" },
		"purpose":        func(b *ApiBridge) { b.Purpose = "payroll-export" },
		"provider":       func(b *ApiBridge) { b.ProviderKey = "bank-y" },
		"profile":        func(b *ApiBridge) { b.ProviderProfileVersion = 3 },
		"credential ref": func(b *ApiBridge) { b.CredentialRef = "0a000000-0000-4000-8000-000000000002" },
		"auth type":      func(b *ApiBridge) { b.AuthType = "MTLS" },
		"sync mode":      func(b *ApiBridge) { b.SyncMode = SyncPush },
		"scope added":    func(b *ApiBridge) { b.Scopes = append(b.Scopes, "payments.write") },
		"scope removed":  func(b *ApiBridge) { b.Scopes = []string{"ingest"} },
	} {
		b := draft()
		mutate(b)
		if b.Protected().Fingerprint() == base {
			t.Errorf("changing %s did not change the fingerprint", name)
		}
	}
	// and what is NOT protected must not
	b := draft()
	b.BridgeName = "renamed"
	b.LegalEntityID = "le2"
	b.Status = StatusActive
	b.Version = 99
	if b.Protected().Fingerprint() != base {
		t.Fatal("non-protected fields must not affect the fingerprint")
	}
}

func TestDiffNamesFieldsNotValues(t *testing.T) {
	old := draft().Protected()
	n := draft()
	n.CredentialRef = "0a000000-0000-4000-8000-000000000009"
	n.Scopes = []string{"ingest", "payments.write"}
	fields, added, removed := old.Diff(n.Protected())
	joined := strings.Join(fields, ",")
	if !strings.Contains(joined, "credential_ref") || !strings.Contains(joined, "scopes") {
		t.Fatalf("fields = %v", fields)
	}
	if strings.Contains(joined, "0a000000") {
		t.Fatal("a diff must not repeat a credential reference")
	}
	if len(added) != 1 || added[0] != "payments.write" || len(removed) != 1 || removed[0] != "statements.read" {
		t.Fatalf("added %v removed %v", added, removed)
	}
}

// ---- validation: nothing secret is storable ------------------------------------

func TestEndpointSyntaxRefusesCredentials_INT_NP_019(t *testing.T) {
	for name, u := range map[string]string{
		"userinfo":            "https://user:pass@api.example.com/x",
		"username only":       "https://apikey@api.example.com/x",
		"api_key query":       "https://api.example.com/x?api_key=abc",
		"token query":         "https://api.example.com/x?access_token=abc",
		"client_secret query": "https://api.example.com/x?client_secret=abc",
		"password query":      "https://api.example.com/x?password=abc",
		"signature query":     "https://api.example.com/x?sig=abc",
		"fragment":            "https://api.example.com/x#frag",
		"not absolute":        "/relative/path",
		"scheme":              "ftp://api.example.com/x",
		"no host":             "https:///x",
		"too long":            "https://api.example.com/" + strings.Repeat("a", 600),
	} {
		if ValidateEndpointSyntax(u) == "" {
			t.Errorf("%s: %q accepted", name, u)
		}
	}
	for _, ok := range []string{
		"https://api.example.com/v1/feed", "https://api.example.com:8443/v1?page=2&limit=50", "http://api.example.com/x",
	} {
		if msg := ValidateEndpointSyntax(ok); msg != "" {
			t.Errorf("%q refused: %s", ok, msg)
		}
	}
}

func TestSyntaxMessagesNeverEchoValues(t *testing.T) {
	c := draft().Protected()
	c.EndpointURL = "https://user:SUPERSECRET@api.example.com/x"
	c.CredentialRef = "sk_live_REALSECRETVALUE"
	for _, msg := range c.ValidateSyntax() {
		if strings.Contains(msg, "SUPERSECRET") || strings.Contains(msg, "REALSECRET") {
			t.Fatalf("message echoes a value: %q", msg)
		}
	}
	if len(c.ValidateSyntax()) < 2 {
		t.Fatal("both problems must be reported")
	}
}

func TestCredentialRefMustBeAnIDNotACredential(t *testing.T) {
	for _, bad := range []string{"sk_live_abc123", "password123", "Bearer abc", "https://vault/x", "0a000000", ""} {
		if bad != "" && ValidCredentialRef(bad) {
			t.Errorf("%q accepted as a credential reference", bad)
		}
	}
	if !ValidCredentialRef(credRef) || !ValidCredentialRef(strings.ToUpper(credRef)) {
		t.Error("a vault policy id must be accepted, in either case")
	}
}

func TestCompleteness(t *testing.T) {
	if msgs := draft().Protected().ValidateComplete(); len(msgs) != 0 {
		t.Fatalf("a full configuration was refused: %v", msgs)
	}
	for name, mutate := range map[string]func(*ApiBridge){
		"no endpoint":               func(b *ApiBridge) { b.EndpointURL = "" },
		"no purpose":                func(b *ApiBridge) { b.Purpose = "" },
		"no provider":               func(b *ApiBridge) { b.ProviderKey = "" },
		"no profile":                func(b *ApiBridge) { b.ProviderProfileVersion = 0 },
		"no region":                 func(b *ApiBridge) { b.DestinationRegion = "" },
		"no auth":                   func(b *ApiBridge) { b.AuthType = "" },
		"no sync":                   func(b *ApiBridge) { b.SyncMode = "" },
		"no scopes":                 func(b *ApiBridge) { b.Scopes = nil },
		"auth needs a credential":   func(b *ApiBridge) { b.CredentialRef = "" },
		"NONE forbids a credential": func(b *ApiBridge) { b.AuthType = "NONE" },
		"bad auth type":             func(b *ApiBridge) { b.AuthType = "PLAINTEXT" },
		"bad scope":                 func(b *ApiBridge) { b.Scopes = []string{"has space"} },
		"too many scopes": func(b *ApiBridge) {
			b.Scopes = nil
			for i := 0; i < MaxScopes+1; i++ {
				b.Scopes = append(b.Scopes, "s"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26)))
			}
		},
	} {
		b := draft()
		mutate(b)
		if len(b.Protected().ValidateComplete()) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	b := draft()
	b.AuthType, b.CredentialRef = "NONE", ""
	if msgs := b.Protected().ValidateComplete(); len(msgs) != 0 {
		t.Fatalf("an unauthenticated connector with no credential is valid: %v", msgs)
	}
}

// ---- lifecycle -----------------------------------------------------------------

func TestHappyPath(t *testing.T) {
	b := draft()
	steps := []struct {
		do   func() error
		want string
	}{
		{func() error { return b.Submit("maker", t0) }, StatusPendingApproval},
		{func() error { return b.Approve("checker", t0) }, StatusPendingApproval},
		{func() error { return b.Activate(t0) }, StatusActive},
		{func() error { return b.MarkDegraded("credential expiring", t0) }, StatusDegraded},
		{func() error { return b.Recover(t0) }, StatusActive},
		{func() error { return b.Suspend("investigation", t0) }, StatusSuspended},
		{func() error { return b.Activate(t0) }, StatusActive},
		{func() error { return b.Revoke("consent withdrawn", t0) }, StatusRevoked},
		{func() error { return b.Archive(t0) }, StatusArchived},
	}
	for i, s := range steps {
		before := b.Version
		if err := s.do(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if b.Status != s.want {
			t.Fatalf("step %d: status %s, want %s", i, b.Status, s.want)
		}
		if b.Version != before+1 {
			t.Fatalf("step %d: version %d, want %d", i, b.Version, before+1)
		}
	}
}

func TestSubmitRefusesIncompleteAndWrongState(t *testing.T) {
	b := draft()
	b.Scopes = nil
	if err := b.Submit("maker", t0); !errors.Is(err, ErrIncompleteConfig) {
		t.Fatalf("incomplete: %v", err)
	}
	if b.Status != StatusDraft || b.Version != 1 || b.ConfigFingerprint != "" {
		t.Fatalf("a refused transition must change nothing: %+v", b)
	}
	if err := active(t).Submit("maker", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("submit from active: %v", err)
	}
}

func TestSeparationOfDuties(t *testing.T) {
	b := draft()
	_ = b.Submit("maker", t0)
	if err := b.Approve("maker", t0); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self-approval: %v", err)
	}
	if err := b.Approve("", t0); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("anonymous approval: %v", err)
	}
	if err := b.Reject("maker", "no", t0); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self-rejection: %v", err)
	}
	if b.ApprovedBy != "" || b.Status != StatusPendingApproval {
		t.Fatal("a refused approval must change nothing")
	}
	if err := b.Approve("checker", t0); err != nil {
		t.Fatal(err)
	}
}

// The approver of an earlier configuration is not the approver of a later one,
// and a person who changed the connector last cannot approve it even if they
// were not the original creator.
func TestSomeoneWhoChangedItCannotApproveIt(t *testing.T) {
	b := draft()
	_, _, _, err := b.ApplyChange(func() ProtectedConfig { c := b.Protected(); c.Purpose = "other-purpose"; return c }(), "editor", t0)
	if err != nil {
		t.Fatal(err)
	}
	_ = b.Submit("editor", t0)
	if err := b.Approve("editor", t0); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("the last editor approved their own change: %v", err)
	}
	if err := b.Approve("maker", t0); err != nil {
		t.Fatalf("the original creator did not make THIS change and may approve it: %v", err)
	}
}

func TestReject(t *testing.T) {
	b := draft()
	_ = b.Submit("maker", t0)
	if err := b.Reject("checker", "destination not allowed", t0); err != nil {
		t.Fatal(err)
	}
	if b.Status != StatusDraft || b.StatusReason != "destination not allowed" || b.ConfigFingerprint != "" || b.ApprovedFingerprint != "" {
		t.Fatalf("%+v", b)
	}
	if err := b.Reject("checker", "x", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("reject from DRAFT must be refused")
	}
}

func TestActivateRequiresMatchingApproval(t *testing.T) {
	if err := draft().Activate(t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("activate a draft: %v", err)
	}
	b := draft()
	_ = b.Submit("maker", t0)
	if err := b.Activate(t0); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("activate before approval: %v", err)
	}
}

// INT-NP-021 / INT-NP-023: nothing about an approved connector can be altered
// behind the approval's back, whether through the API or the row.
func TestConfigurationChangedBehindTheApprovalCannotGoLive(t *testing.T) {
	b := approved(t)
	b.EndpointURL = "https://attacker.example/steal" // bypassing ApplyChange
	if err := b.Activate(t0); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("a configuration that differs from the approved one went live: %v", err)
	}
	b = approved(t)
	b.Scopes = append(b.Scopes, "payments.write")
	if err := b.Activate(t0); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("a widened scope went live without approval: %v", err)
	}
}

func TestProtectedChangeOnALiveConnectorWithdrawsApproval_INT_NP_021_023(t *testing.T) {
	for name, mutate := range map[string]func(*ProtectedConfig){
		"destination changes":  func(c *ProtectedConfig) { c.EndpointURL = "https://elsewhere.bank-x.example/v2/feed" },
		"scope widened":        func(c *ProtectedConfig) { c.Scopes = append(c.Scopes, "payments.write") },
		"region changes":       func(c *ProtectedConfig) { c.DestinationRegion = "eu-west" },
		"credential ref moves": func(c *ProtectedConfig) { c.CredentialRef = "0a000000-0000-4000-8000-000000000002" },
		"profile upgraded":     func(c *ProtectedConfig) { c.ProviderProfileVersion = 3 },
		"purpose changes":      func(c *ProtectedConfig) { c.Purpose = "different-purpose" },
	} {
		b := active(t)
		oldFP := b.ApprovedFingerprint
		cfg := b.Protected()
		mutate(&cfg)
		fields, _, _, err := b.ApplyChange(cfg, "editor", t0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(fields) == 0 {
			t.Errorf("%s: no changed fields reported", name)
		}
		if b.Live() || b.Status != StatusDraft {
			t.Errorf("%s: connector still %s after a protected change", name, b.Status)
		}
		if b.ApprovedFingerprint != "" || b.ApprovedBy != "" || b.ConfigFingerprint != "" {
			t.Errorf("%s: approval survived the change (was %s)", name, oldFP)
		}
		if err := b.Activate(t0); err == nil {
			t.Errorf("%s: activated without a new approval", name)
		}
		if b.LastChangedBy != "editor" {
			t.Errorf("%s: last_changed_by = %q", name, b.LastChangedBy)
		}
	}
}

func TestNoOpChangeLeavesApprovalIntact(t *testing.T) {
	b := active(t)
	v := b.Version
	fields, _, _, err := b.ApplyChange(b.Protected(), "editor", t0)
	if err != nil || len(fields) != 0 {
		t.Fatalf("%v %v", fields, err)
	}
	if b.Status != StatusActive || b.Version != v || b.LastChangedBy != "maker" {
		t.Fatal("a change that changes nothing must not disturb an approved connector")
	}
}

func TestChangeRefusesSecrets(t *testing.T) {
	b := draft()
	cfg := b.Protected()
	cfg.EndpointURL = "https://u:p@api.example.com/x"
	if _, _, _, err := b.ApplyChange(cfg, "e", t0); !errors.Is(err, ErrIncompleteConfig) {
		t.Fatalf("%v", err)
	}
	if b.EndpointURL != "https://api.bank-x.example/v2/feed" || b.Version != 1 {
		t.Fatal("a refused change must not be applied")
	}
}

func TestChangeWhilePendingRequiresResubmission(t *testing.T) {
	b := draft()
	_ = b.Submit("maker", t0)
	cfg := b.Protected()
	cfg.Purpose = "other-purpose"
	if _, _, _, err := b.ApplyChange(cfg, "editor", t0); err != nil {
		t.Fatal(err)
	}
	if b.Status != StatusDraft || b.ConfigFingerprint != "" {
		t.Fatalf("a pending submission describes the OLD configuration: %+v", b)
	}
}

func TestTerminalConnectorsCannotChange(t *testing.T) {
	for _, mk := range []func() *ApiBridge{
		func() *ApiBridge { b := active(t); _ = b.Revoke("x", t0); return b },
		func() *ApiBridge { b := active(t); _ = b.Revoke("x", t0); _ = b.Archive(t0); return b },
	} {
		b := mk()
		cfg := b.Protected()
		cfg.Purpose = "new-purpose"
		if _, _, _, err := b.ApplyChange(cfg, "e", t0); !errors.Is(err, ErrConnectorTerminal) {
			t.Fatalf("changed a %s connector: %v", b.Status, err)
		}
		if err := b.Revoke("again", t0); !errors.Is(err, ErrConnectorTerminal) {
			t.Fatalf("re-revoked: %v", err)
		}
		if err := b.Activate(t0); err == nil {
			t.Fatal("reactivated a revoked connector")
		}
		if err := b.Suspend("x", t0); err == nil {
			t.Fatal("suspended a revoked connector")
		}
	}
}

func TestSuspendKeepsApprovalSoResumeNeedsNoNewOne(t *testing.T) {
	b := active(t)
	fp := b.ApprovedFingerprint
	_ = b.Suspend("maintenance", t0)
	if b.ApprovedFingerprint != fp {
		t.Fatal("suspension must not discard the approval")
	}
	if err := b.Activate(t0); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

func TestChangeWhileSuspendedForcesReapproval(t *testing.T) {
	b := active(t)
	_ = b.Suspend("x", t0)
	cfg := b.Protected()
	cfg.Scopes = append(cfg.Scopes, "extra")
	if _, _, _, err := b.ApplyChange(cfg, "editor", t0); err != nil {
		t.Fatal(err)
	}
	if err := b.Activate(t0); err == nil {
		t.Fatal("a suspended connector with widened scopes resumed without approval")
	}
}

func TestDegradeAndRecoverOnlyFromTheRightStates(t *testing.T) {
	if err := draft().MarkDegraded("x", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("degrading a draft")
	}
	b := active(t)
	if err := b.Recover(t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("recovering an active connector")
	}
	_ = b.MarkDegraded("x", t0)
	if err := b.MarkDegraded("again", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("degrading twice")
	}
	if !b.Live() {
		t.Fatal("a degraded connector still moves data")
	}
}

func TestLegacyConnector(t *testing.T) {
	b := draft()
	b.Status, b.LegacyUnapproved, b.Version = StatusActive, true, 1
	b.ConfigFingerprint, b.ApprovedFingerprint, b.Purpose, b.ProviderKey, b.ProviderProfileVersion = "", "", "", "", 0
	b.Scopes = []string{"ingest"}

	if !b.Live() || !b.HasScope("ingest") {
		t.Fatal("a grandfathered connector keeps working")
	}
	// suspended by an operator -> may resume as it was
	_ = b.Suspend("maintenance", t0)
	if err := b.Activate(t0); err != nil {
		t.Fatalf("a grandfathered connector must be resumable: %v", err)
	}
	// but its first protected change puts it through approval
	cfg := b.Protected()
	cfg.Purpose, cfg.ProviderKey, cfg.ProviderProfileVersion = "bank-statement-import", "bank-x", 2
	if _, _, _, err := b.ApplyChange(cfg, "editor", t0); err != nil {
		t.Fatal(err)
	}
	if b.LegacyUnapproved || b.Status != StatusDraft {
		t.Fatalf("after its first change a connector is governed: %+v", b)
	}
}

func TestHasScope(t *testing.T) {
	b := draft()
	if !b.HasScope("INGEST") || !b.HasScope(" statements.read ") || b.HasScope("payments.write") || b.HasScope("") {
		t.Fatal("scope matching")
	}
}

func TestEndpointHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://API.Bank-X.example:8443/v1?a=b": "api.bank-x.example",
		"not a url %%":                           "",
		"https://[2001:db8::1]:443/x":            "2001:db8::1",
	} {
		if got := EndpointHost(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
