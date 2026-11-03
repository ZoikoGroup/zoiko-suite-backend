package webauthn

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/webauthn/webauthntest"
)

// The tests drive the real library with webauthntest's software
// authenticator: nothing in the verification path is mocked.

const (
	testRPID   = "id.zoiko.test"
	testOrigin = "https://id.zoiko.test"
)

// ── harness ──────────────────────────────────────────────────────────────────

func newService(t *testing.T) (*Service, *webauthntest.MemoryStore) {
	t.Helper()
	st := webauthntest.NewMemoryStore()
	s, err := New(Config{RPID: testRPID, RPOrigins: []string{testOrigin}}, st, NewMemoryCeremonies(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

func principal(id string) *domain.Principal {
	return &domain.Principal{PrincipalID: id, TenantID: "tenant-a", Email: id + "@example.test",
		PrincipalType: domain.PrincipalTypeHuman, Status: domain.PrincipalStatusActive}
}

func register(t *testing.T, s *Service, p *domain.Principal, a *webauthntest.Authenticator) *domain.WebAuthnCredential {
	t.Helper()
	ctx := context.Background()
	opts, handle, err := s.BeginRegistration(ctx, p)
	if err != nil {
		t.Fatalf("begin registration: %v", err)
	}
	cred, err := s.FinishRegistration(ctx, p, handle, "laptop", a.Create(t, opts), "corr-reg")
	if err != nil {
		t.Fatalf("finish registration: %v", err)
	}
	return cred
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestAU11_PasskeyRegistersAndStepsUp(t *testing.T) {
	s, st := newService(t)
	p, a := principal("p-1"), webauthntest.New(t, testRPID, testOrigin)
	ctx := context.Background()

	cred := register(t, s, p, a)
	if cred.CredentialID != a.CredentialID() || !cred.UserVerified || cred.Nickname != "laptop" {
		t.Fatalf("registered credential = %+v", cred)
	}
	if cred.PublicKey != nil {
		t.Fatal("the registration answer must not carry key material")
	}
	if len(st.Events) != 1 || st.Events[0].EventType != "identity.mfa_factor.enrolled" {
		t.Fatalf("registration events = %+v", st.Events)
	}
	if has, _ := s.HasPasskey(ctx, p.TenantID, p.PrincipalID); !has {
		t.Fatal("HasPasskey false after registration")
	}

	opts, handle, err := s.BeginStepUp(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStepUp(ctx, p, handle, a.Get(t, opts, p.PrincipalID), "corr-1"); err != nil {
		t.Fatalf("step-up refused: %v", err)
	}
	if d := st.LastDecision(); d.Action != ActionStepUp || d.Outcome != "ALLOWED" {
		t.Fatalf("step-up decision = %+v", d)
	}
	if got := st.Creds[cred.CredentialID].SignCount; got != 1 {
		t.Fatalf("stored sign count = %d, want 1", got)
	}
	if last := st.Events[len(st.Events)-1]; last.EventType != "identity.step_up.succeeded" {
		t.Fatalf("step-up event = %q, want identity.step_up.succeeded with the counter", last.EventType)
	}
}

func TestAU11_HostedLoginAssertion(t *testing.T) {
	s, st := newService(t)
	p, a := principal("p-1"), webauthntest.New(t, testRPID, testOrigin)
	register(t, s, p, a)
	opts, handle, err := s.BeginLogin(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishLogin(context.Background(), p, handle, a.Get(t, opts, p.PrincipalID)); err != nil {
		t.Fatalf("login assertion refused: %v", err)
	}
	if d := st.LastDecision(); d.Action != ActionLogin || d.Outcome != "ALLOWED" {
		t.Fatalf("login decision = %+v", d)
	}
}

func TestAU11_CeremonyIsSingleUse(t *testing.T) {
	s, _ := newService(t)
	p, a := principal("p-1"), webauthntest.New(t, testRPID, testOrigin)
	register(t, s, p, a)
	ctx := context.Background()
	opts, handle, _ := s.BeginStepUp(ctx, p)
	answer := a.Get(t, opts, p.PrincipalID)
	if err := s.FinishStepUp(ctx, p, handle, answer, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStepUp(ctx, p, handle, answer, ""); !errors.Is(err, ErrCeremonyInvalid) {
		t.Fatalf("replayed assertion: err = %v, want ErrCeremonyInvalid", err)
	}
}

func TestAU11_CeremonyBoundToPrincipalAndPurpose(t *testing.T) {
	s, _ := newService(t)
	p, a := principal("p-1"), webauthntest.New(t, testRPID, testOrigin)
	register(t, s, p, a)
	ctx := context.Background()

	// A login challenge cannot finish a step-up.
	opts, handle, _ := s.BeginLogin(ctx, p)
	if err := s.FinishStepUp(ctx, p, handle, a.Get(t, opts, p.PrincipalID), ""); !errors.Is(err, ErrCeremonyInvalid) {
		t.Fatalf("login ceremony used for step-up: err = %v", err)
	}
	// Someone else's challenge cannot be finished by p, even with p's key.
	other, b := principal("p-2"), webauthntest.New(t, testRPID, testOrigin)
	register(t, s, other, b)
	opts, handle, _ = s.BeginStepUp(ctx, other)
	if err := s.FinishStepUp(ctx, p, handle, a.Get(t, opts, p.PrincipalID), ""); !errors.Is(err, ErrCeremonyInvalid) {
		t.Fatalf("another principal's ceremony: err = %v", err)
	}
}

func TestAU11_WrongOriginRefused(t *testing.T) {
	s, st := newService(t)
	p, a := principal("p-1"), webauthntest.New(t, testRPID, testOrigin)
	register(t, s, p, a)
	ctx := context.Background()
	a.Origin = "https://phish.example"
	opts, handle, _ := s.BeginStepUp(ctx, p)
	if err := s.FinishStepUp(ctx, p, handle, a.Get(t, opts, p.PrincipalID), ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("assertion from another origin: err = %v, want ErrRejected", err)
	}
	if d := st.LastDecision(); d.Outcome != "DENIED" || d.Basis != "passkey_assertion_rejected" {
		t.Fatalf("refusal decision = %+v", d)
	}
}

func TestAU11_RegistrationFromAnotherOriginRefused(t *testing.T) {
	s, st := newService(t)
	p, a := principal("p-1"), webauthntest.New(t, testRPID, testOrigin)
	a.Origin = "https://phish.example"
	opts, handle, _ := s.BeginRegistration(context.Background(), p)
	if _, err := s.FinishRegistration(context.Background(), p, handle, "", a.Create(t, opts), ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("registration from another origin: err = %v", err)
	}
	if len(st.Creds) != 0 {
		t.Fatal("a refused registration stored a credential")
	}
}

func TestAU11_TamperedSignatureRefused(t *testing.T) {
	s, _ := newService(t)
	p, a := principal("p-1"), webauthntest.New(t, testRPID, testOrigin)
	register(t, s, p, a)
	ctx := context.Background()
	opts, handle, _ := s.BeginStepUp(ctx, p)
	a.Counter++
	bad := a.Sign(t, webauthntest.Challenge(t, opts), p.PrincipalID, func(sig []byte) { sig[len(sig)-1] ^= 0xff })
	if err := s.FinishStepUp(ctx, p, handle, bad, ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("tampered signature: err = %v", err)
	}
}

func TestAU11_ClonedAuthenticatorRefused(t *testing.T) {
	s, st := newService(t)
	p, a := principal("p-1"), webauthntest.New(t, testRPID, testOrigin)
	register(t, s, p, a)
	ctx := context.Background()
	opts, handle, _ := s.BeginStepUp(ctx, p)
	if err := s.FinishStepUp(ctx, p, handle, a.Get(t, opts, p.PrincipalID), ""); err != nil {
		t.Fatal(err)
	}
	// A second copy of the key answers with a counter that has not advanced.
	opts, handle, _ = s.BeginStepUp(ctx, p)
	stale := a.Sign(t, webauthntest.Challenge(t, opts), p.PrincipalID, nil)
	if err := s.FinishStepUp(ctx, p, handle, stale, ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("non-advancing counter: err = %v, want ErrRejected", err)
	}
	if d := st.LastDecision(); d.Basis != "passkey_clone_warning" {
		t.Fatalf("clone refusal basis = %q", d.Basis)
	}
}

func TestAU11_RetiredPasskeyCannotAssert(t *testing.T) {
	s, _ := newService(t)
	p, a := principal("p-1"), webauthntest.New(t, testRPID, testOrigin)
	cred := register(t, s, p, a)
	ctx := context.Background()

	// Retired between begin and finish: the open ceremony no longer works.
	opts, handle, _ := s.BeginStepUp(ctx, p)
	if n, err := s.Retire(ctx, p.TenantID, p.PrincipalID, cred.CredentialID, "owner_revoked", p.PrincipalID, ActionRevoke, ""); err != nil || n != 1 {
		t.Fatalf("retire: n=%d err=%v", n, err)
	}
	if err := s.FinishStepUp(ctx, p, handle, a.Get(t, opts, p.PrincipalID), ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("assertion with a retired passkey: err = %v", err)
	}
	if _, _, err := s.BeginStepUp(ctx, p); !errors.Is(err, ErrNoPasskey) {
		t.Fatalf("begin after retire: err = %v, want ErrNoPasskey", err)
	}
}

func TestAU11_RegisteredPasskeyIsExcludedFromTheNextRegistration(t *testing.T) {
	s, _ := newService(t)
	p, a := principal("p-1"), webauthntest.New(t, testRPID, testOrigin)
	cred := register(t, s, p, a)
	opts, _, err := s.BeginRegistration(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var o struct {
		PublicKey struct {
			Exclude []struct {
				ID string `json:"id"`
			} `json:"excludeCredentials"`
			Selection struct {
				UV string `json:"userVerification"`
			} `json:"authenticatorSelection"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(opts, &o); err != nil {
		t.Fatal(err)
	}
	if len(o.PublicKey.Exclude) != 1 || o.PublicKey.Exclude[0].ID != cred.CredentialID {
		t.Fatalf("excludeCredentials = %+v, want the registered passkey", o.PublicKey.Exclude)
	}
	if o.PublicKey.Selection.UV != "required" {
		t.Fatalf("userVerification = %q, want required", o.PublicKey.Selection.UV)
	}
}

func TestAU11_StoreOutageIsNotAWrongAnswer(t *testing.T) {
	s, st := newService(t)
	p, a := principal("p-1"), webauthntest.New(t, testRPID, testOrigin)
	register(t, s, p, a)
	st.FailList = true
	if _, _, err := s.BeginStepUp(context.Background(), p); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("store outage: err = %v, want ErrUnavailable", err)
	}
}

func TestNewRefusesAnIncompleteRelyingParty(t *testing.T) {
	if _, err := New(Config{RPOrigins: []string{testOrigin}}, webauthntest.NewMemoryStore(), NewMemoryCeremonies(), nil, nil); err == nil {
		t.Fatal("built without an RP ID")
	}
	if _, err := New(Config{RPID: testRPID}, webauthntest.NewMemoryStore(), NewMemoryCeremonies(), nil, nil); err == nil {
		t.Fatal("built without an origin")
	}
}
