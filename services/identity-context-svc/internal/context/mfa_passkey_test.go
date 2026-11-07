package context_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/webauthn"
	"zoiko.io/identity-context-svc/internal/webauthn/webauthntest"
)

// Passkeys through the MFA service and its routes (AU-11), with the real
// WebAuthn service and a software authenticator.

const (
	pkRPID   = "id.zoiko.test"
	pkOrigin = "https://id.zoiko.test"
)

type passkeyFixture struct {
	*mfaFixture
	keys *webauthntest.MemoryStore
	pk   *webauthn.Service
}

func newPasskeyFixture(t *testing.T) *passkeyFixture {
	t.Helper()
	fx := newMFAFixture(t)
	keys := webauthntest.NewMemoryStore()
	pk, err := webauthn.New(webauthn.Config{RPID: pkRPID, RPOrigins: []string{pkOrigin}}, keys,
		webauthn.NewMemoryCeremonies(), nil, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	fx.svc.WithPasskeys(pk)
	return &passkeyFixture{mfaFixture: fx, keys: keys, pk: pk}
}

// registerPasskey gives principal a first passkey from a fresh password-only
// login.
func (fx *passkeyFixture) registerPasskey(t *testing.T, principal string) *webauthntest.Authenticator {
	t.Helper()
	return fx.registerPasskeyAs(t, fx.caller(t, fx.session(principal, time.Minute, false)))
}

// registerPasskeyAs registers a passkey for c's principal.
func (fx *passkeyFixture) registerPasskeyAs(t *testing.T, c *identityctx.MFACaller) *webauthntest.Authenticator {
	t.Helper()
	ctx := context.Background()
	opts, handle, err := fx.svc.BeginPasskeyRegistration(ctx, c, "corr")
	if err != nil {
		t.Fatalf("begin registration: %v", err)
	}
	a := webauthntest.New(t, pkRPID, pkOrigin)
	if _, err := fx.svc.FinishPasskeyRegistration(ctx, c, handle, "laptop", a.Create(t, opts), "corr"); err != nil {
		t.Fatalf("finish registration: %v", err)
	}
	return a
}

func TestAU11_FirstPasskeyNeedsRecentLogin(t *testing.T) {
	fx := newPasskeyFixture(t)
	stale := fx.caller(t, fx.session(alice, time.Hour, false))
	if _, _, err := fx.svc.BeginPasskeyRegistration(context.Background(), stale, "c"); !errors.Is(err, identityctx.ErrReauthRequired) {
		t.Fatalf("stale login registering a first passkey: %v", err)
	}
	if !contains(fx.store.decisions, "MFA_ENROL:DENIED:reauthentication_required") {
		t.Fatalf("refusal not evidenced: %v", fx.store.decisions)
	}
	fx.registerPasskey(t, alice)
}

func TestAU11_PasskeyBesideAnExistingFactorNeedsStepUp(t *testing.T) {
	ctx := context.Background()

	// Beside TOTP: a stolen password alone must not add the thief's passkey.
	fx := newPasskeyFixture(t)
	fx.enrolAndActivate(t, alice)
	pwd := fx.caller(t, fx.session(alice, time.Minute, false))
	if _, _, err := fx.svc.BeginPasskeyRegistration(ctx, pwd, "c"); !errors.Is(err, identityctx.ErrMFAStepUpRequired) {
		t.Fatalf("password-only session adding a passkey beside TOTP: %v", err)
	}
	if _, _, err := fx.svc.BeginPasskeyRegistration(ctx, fx.caller(t, fx.session(alice, time.Minute, true)), "c"); err != nil {
		t.Fatalf("stepped-up session: %v", err)
	}

	// Beside another passkey: the same rule, for a passkey and for TOTP.
	fx = newPasskeyFixture(t)
	fx.registerPasskey(t, alice)
	if _, _, err := fx.svc.BeginPasskeyRegistration(ctx, fx.caller(t, fx.session(alice, 2*time.Minute, false)), "c"); !errors.Is(err, identityctx.ErrMFAStepUpRequired) {
		t.Fatalf("password-only session adding a second passkey: %v", err)
	}
	if _, err := fx.svc.Enrol(ctx, fx.caller(t, fx.session(alice, 3*time.Minute, false)), "c"); !errors.Is(err, identityctx.ErrMFAStepUpRequired) {
		t.Fatalf("password-only session adding TOTP beside a passkey: %v", err)
	}
}

func TestAU11_PasskeyStepUpMintsAPhishingResistantGrant(t *testing.T) {
	fx := newPasskeyFixture(t)
	ctx := context.Background()
	c := fx.caller(t, fx.session(alice, time.Minute, false))
	if _, _, err := fx.svc.BeginPasskeyStepUp(ctx, c, "c"); !errors.Is(err, identityctx.ErrMFANotEnrolled) {
		t.Fatalf("step-up without a passkey: %v", err)
	}
	a := fx.registerPasskey(t, alice)

	opts, handle, err := fx.svc.BeginPasskeyStepUp(ctx, c, "c")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := fx.svc.FinishPasskeyStepUp(ctx, c, handle, a.Get(t, opts, alice), "c")
	if err != nil {
		t.Fatalf("passkey step-up: %v", err)
	}
	last := fx.issuer.issued[len(fx.issuer.issued)-1]
	if !last.mfaDone || strings.Join(last.amr, ",") != "pwd,hwk" || last.clientID != "zoiko-console" {
		t.Fatalf("minted grant: %+v", last)
	}
	if domain.AssuranceFor(resp.AMR) != domain.AssurancePhishingResistant {
		t.Fatalf("assurance for %v = %s, want PHISHING_RESISTANT", resp.AMR, domain.AssuranceFor(resp.AMR))
	}
	if d := fx.keys.LastDecision(); d.Action != "MFA_STEP_UP" || d.Outcome != "ALLOWED" {
		t.Fatalf("step-up decision = %+v", d)
	}
	// The ceremony is spent: the same answer again is refused, not re-minted.
	n := len(fx.issuer.issued)
	if _, err := fx.svc.FinishPasskeyStepUp(ctx, c, handle, a.Get(t, opts, alice), "c"); !errors.Is(err, webauthn.ErrCeremonyInvalid) {
		t.Fatalf("replayed ceremony: %v", err)
	}
	if len(fx.issuer.issued) != n {
		t.Fatal("a refused step-up minted a token")
	}
}

func TestAU11_RevokePasskeyNeedsAStepUp(t *testing.T) {
	fx := newPasskeyFixture(t)
	ctx := context.Background()
	a := fx.registerPasskey(t, alice)
	if err := fx.svc.RevokePasskey(ctx, fx.caller(t, fx.session(alice, time.Minute, false)), a.CredentialID(), "lost", "c"); !errors.Is(err, identityctx.ErrMFAStepUpRequired) {
		t.Fatalf("password-only revoke: %v", err)
	}
	if err := fx.svc.RevokePasskey(ctx, fx.caller(t, fx.session(alice, time.Minute, true)), a.CredentialID(), "lost", "c"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := fx.svc.RevokePasskey(ctx, fx.caller(t, fx.session(alice, 2*time.Minute, true)), a.CredentialID(), "lost", "c"); !errors.Is(err, identityctx.ErrMFAFactorNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
}

func TestAU11_AdminResetRetiresPasskeysToo(t *testing.T) {
	fx := newPasskeyFixture(t)
	ctx := context.Background()
	fx.enrolAndActivate(t, alice)
	a := fx.registerPasskeyAs(t, fx.caller(t, fx.session(alice, time.Minute, true)))

	n, err := fx.svc.ResetFactors(ctx, fx.caller(t, fx.session(admin, time.Minute, true)), alice, "LOST_DEVICE", "ticket 7", "c")
	if err != nil || n != 2 {
		t.Fatalf("admin reset: n=%d err=%v, want the TOTP factor and the passkey", n, err)
	}
	if fx.keys.Creds[a.CredentialID()].Status != "RETIRED" {
		t.Fatal("the passkey survived an administrator's reset")
	}
	var reset bool
	for _, e := range fx.keys.Events {
		reset = reset || e.EventType == "identity.mfa_factor.reset"
	}
	if !reset {
		t.Fatal("no reset event for the passkey")
	}
	if has, _ := fx.pk.HasPasskey(ctx, mfaTenant, alice); has {
		t.Fatal("HasPasskey after reset")
	}
}

func TestAU11_FactorListShowsPasskeys(t *testing.T) {
	fx := newPasskeyFixture(t)
	a := fx.registerPasskey(t, alice)
	factors, err := fx.svc.List(context.Background(), fx.caller(t, fx.session(alice, time.Minute, false)))
	if err != nil {
		t.Fatal(err)
	}
	if len(factors) != 1 || factors[0].Type != domain.CredentialTypeWebAuthn || factors[0].FactorID != a.CredentialID() {
		t.Fatalf("factors = %+v", factors)
	}
}

func TestAU11_PasskeyRoutes(t *testing.T) {
	fx := newPasskeyFixture(t)
	r := mfaRouter(fx.mfaFixture)
	b := fx.session(alice, time.Minute, false)

	w := mfaDo(r, http.MethodPost, "/v1/mfa/passkeys/register/begin", b, nil, nil)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("register begin: %d %s", w.Code, w.Body)
	}
	var begin struct {
		Options  json.RawMessage `json:"options"`
		Ceremony string          `json:"ceremony"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &begin)
	a := webauthntest.New(t, pkRPID, pkOrigin)
	w = mfaDo(r, http.MethodPost, "/v1/mfa/passkeys/register/finish", b, nil,
		map[string]any{"ceremony": begin.Ceremony, "nickname": "laptop", "credential": a.Create(t, begin.Options)})
	if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), "public_key") {
		t.Fatalf("register finish: %d %s", w.Code, w.Body)
	}
	if w := mfaDo(r, http.MethodGet, "/v1/mfa/passkeys", b, nil, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), a.CredentialID()) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}

	w = mfaDo(r, http.MethodPost, "/v1/authenticate/step-up/passkey/begin", b, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("step-up begin: %d %s", w.Code, w.Body)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &begin)
	w = mfaDo(r, http.MethodPost, "/v1/authenticate/step-up/passkey/finish", b, nil,
		map[string]any{"ceremony": begin.Ceremony, "credential": a.Get(t, begin.Options, alice)})
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"amr":["pwd","hwk"]`) {
		t.Fatalf("step-up finish: %d %s", w.Code, w.Body)
	}

	// The same ceremony again; a forged one; a wrong answer.
	if w := mfaDo(r, http.MethodPost, "/v1/authenticate/step-up/passkey/finish", b, nil,
		map[string]any{"ceremony": begin.Ceremony, "credential": a.Get(t, begin.Options, alice)}); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "ceremony_invalid") {
		t.Fatalf("spent ceremony: %d %s", w.Code, w.Body)
	}
	w = mfaDo(r, http.MethodPost, "/v1/authenticate/step-up/passkey/begin", b, nil, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &begin)
	a.Origin = "https://phish.example"
	if w := mfaDo(r, http.MethodPost, "/v1/authenticate/step-up/passkey/finish", b, nil,
		map[string]any{"ceremony": begin.Ceremony, "credential": a.Get(t, begin.Options, alice)}); w.Code != http.StatusUnauthorized ||
		!strings.Contains(w.Body.String(), "passkey_rejected") {
		t.Fatalf("phished assertion: %d %s", w.Code, w.Body)
	}
	if w := mfaDo(r, http.MethodPost, "/v1/mfa/passkeys/register/finish", b, nil, map[string]any{"ceremony": "x"}); w.Code != http.StatusBadRequest {
		t.Fatalf("finish without a credential: %d", w.Code)
	}

	// Revoke: password-only is refused; a stepped-up session may.
	if w := mfaDo(r, http.MethodPost, "/v1/mfa/passkeys/revoke", b, nil, map[string]string{"credential_id": a.CredentialID()}); w.Code != http.StatusForbidden {
		t.Fatalf("password-only revoke: %d %s", w.Code, w.Body)
	}
	if w := mfaDo(r, http.MethodPost, "/v1/mfa/passkeys/revoke", fx.session(alice, time.Minute, true), nil,
		map[string]string{"credential_id": a.CredentialID(), "reason_code": "LOST"}); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
}

func TestAU11_UnconfiguredPasskeysAnswer501(t *testing.T) {
	fx := newMFAFixture(t) // no WithPasskeys
	r := mfaRouter(fx)
	b := fx.session(alice, time.Minute, false)
	for _, path := range []string{"/v1/mfa/passkeys/register/begin", "/v1/authenticate/step-up/passkey/begin"} {
		if w := mfaDo(r, http.MethodPost, path, b, nil, nil); w.Code != http.StatusNotImplemented ||
			!strings.Contains(w.Body.String(), "passkeys_not_configured") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
}
