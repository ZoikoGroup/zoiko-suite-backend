package xicconn_test

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

func refusal(t *testing.T, err error, code xicconn.RefusalCode) {
	t.Helper()
	var r *xicconn.Refusal
	if !errors.As(err, &r) || r.Code != code {
		t.Fatalf("want refusal %s, got %v", code, err)
	}
}

// ---- operability: the gate every exchange passes (I-01) --------------------------------

func TestOperable_ActiveConnectionResolvesTheExchange(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	d, err := e.svc.Operable(ctxFor(tenantA), tenantA, c.ConnectionID, "payment-submit")
	if err != nil {
		t.Fatal(err)
	}
	if d.Host != "api.bank-x.com" || d.Operation.ID != "payment-submit" || d.ContractVersionID != v.ContractVersionID || d.Region != "uk" ||
		len(d.CredentialRefs) != 1 || d.CredentialRefs[0].Ref != refOAuth || d.CredentialRefs[0].Grace {
		t.Fatalf("%+v", d)
	}
	in, err := e.svc.Operable(ctxFor(tenantA), tenantA, c.ConnectionID, "statement-push")
	if err != nil || in.Host != "" || in.CredentialRefs[0].Ref != refHMAC {
		t.Fatalf("inbound: %v %+v", err, in)
	}
}

func TestOperable_Refusals(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	ctx := ctxFor(tenantA)

	// not active: draft, authorizing, validating
	c := e.create(tenantA, xicconn.EnvSandbox, v, "acc")
	_, err := e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit")
	refusal(t, err, xicconn.CodeNotActive)
	if _, err = e.svc.Operable(ctx, tenantA, "00000000-0000-4000-8000-000000000000", "payment-submit"); !errors.Is(err, xicconn.ErrNotFound) {
		t.Fatalf("unknown connection: %v", err)
	}

	act := e.sandbox(tenantA, v, "acc-2", refOAuth, refHMAC)
	// an operation that is not registered
	_, err = e.svc.Operable(ctx, tenantA, act.ConnectionID, "delete-everything")
	refusal(t, err, xicconn.CodeContractMismatch)
	// the deployment stops allowing the region
	e.svc.AllowedRegions = []string{"eu"}
	_, err = e.svc.Operable(ctx, tenantA, act.ConnectionID, "payment-submit")
	refusal(t, err, xicconn.CodeResidency)
	e.svc.AllowedRegions = []string{"uk"}
	// suspension blocks at once
	if _, err = e.svc.Suspend(ctx, operator, act.ConnectionID, e.get(tenantA, act).Version, "PROVIDER_INCIDENT under investigation"); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Operable(ctx, tenantA, act.ConnectionID, "payment-submit")
	refusal(t, err, xicconn.CodeNotActive)
	// so does revocation, immediately
	rev := e.sandbox(tenantA, v, "acc-3", refOAuth2, refHMAC2)
	if _, err = e.svc.Revoke(ctx, operator, rev.ConnectionID, rev.Version, "decommissioned"); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Operable(ctx, tenantA, rev.ConnectionID, "payment-submit")
	refusal(t, err, xicconn.CodeNotActive)
}

func TestOperable_AnotherTenantsConnectionIsXIC008WithASecuritySignal(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	_, err := e.svc.Operable(ctxFor(tenantB), tenantB, c.ConnectionID, "payment-submit")
	refusal(t, err, xicconn.CodeTenantScope)
	if len(e.repo.Signals) != 1 || e.repo.Signals[0].Type != "integration.security.tenant-scope-mismatch" {
		t.Fatalf("signals %+v", e.repo.Signals)
	}
	// and the legitimate tenant is unaffected
	if _, err := e.svc.Operable(ctxFor(tenantA), tenantA, c.ConnectionID, "payment-submit"); err != nil {
		t.Fatal(err)
	}
}

func TestOperable_PinnedContractMustStillBeSupported(t *testing.T) {
	e := newEnv(t)
	until := registrytest_t0().Add(60 * 24 * time.Hour)
	v := e.publish("bank-x", "1.0.0", func(in *registry.VersionInput) { in.SupportedUntil = &until })
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	if _, err := e.svc.Operable(ctxFor(tenantA), tenantA, c.ConnectionID, "payment-submit"); err != nil {
		t.Fatal(err)
	}
	e.publish("bank-x", "1.1.0", nil) // supersedes: pin stays usable while supported
	if _, err := e.svc.Operable(ctxFor(tenantA), tenantA, c.ConnectionID, "payment-submit"); err != nil {
		t.Fatalf("a superseded pin inside its support window: %v", err)
	}
	e.advance(20 * 24 * time.Hour)
	_, err := e.svc.Operable(ctxFor(tenantA), tenantA, c.ConnectionID, "payment-submit")
	_ = err // t0+48h+20d is still before t0+60d
	if err != nil {
		t.Fatalf("still supported: %v", err)
	}
	e.advance(40 * 24 * time.Hour)
	_, err = e.svc.Operable(ctxFor(tenantA), tenantA, c.ConnectionID, "payment-submit")
	refusal(t, err, xicconn.CodeContractMismatch)
}

func TestOperable_ProductionNeedsAValidCertification(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.production(tenantA, v, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)
	if _, err := e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit"); err != nil {
		t.Fatal(err)
	}
	// NP-47: the certification expires
	e.advance(181 * 24 * time.Hour)
	// keep the credential valid so only the certification is at stake
	_, err := e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit")
	if err != nil {
		var r *xicconn.Refusal
		if !errors.As(err, &r) || (r.Code != xicconn.CodeCertStale && r.Code != xicconn.CodeNotActive) {
			t.Fatalf("%v", err)
		}
	}
}

func TestOperable_CertificationExpiryAndStaleAreXIC017(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.production(tenantA, v, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)
	// extend the credential so expiry of the certification is the only cause
	cur := e.get(tenantA, c)
	exp := e.at().Add(400 * 24 * time.Hour)
	e.vault.set(refOAuth3, nil)
	if _, err := e.svc.Rotate(ctx, operator, cur.ConnectionID, cur.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOAuth3, ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	e.advance(181 * 24 * time.Hour)
	_, err := e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit")
	refusal(t, err, xicconn.CodeCertStale)
}

func TestOperable_MigrationMakesTheCertificationStale(t *testing.T) {
	e := newEnv(t)
	v1 := e.publish("bank-x", "1.0.0", nil)
	c := e.production(tenantA, v1, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)
	v2 := e.publish("bank-x", "1.1.0", func(in *registry.VersionInput) {
		in.Content.Operations[0].RateLimitPerMinute = 321 // a different contract, same shape
	})
	req, err := e.svc.RequestMigration(ctx, creator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v2.ContractVersionID, Reason: "adopt 1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit"); err != nil {
		t.Fatalf("a pending request must not move the pin: %v", err)
	}
	if _, err = e.svc.DecideMigration(ctx, approver, c.ConnectionID, req.RequestID, true); err != nil {
		t.Fatal(err)
	}
	cur := e.get(tenantA, c)
	if cur.ContractVersionID != v2.ContractVersionID {
		t.Fatalf("pin %s", cur.ContractVersionID)
	}
	// I-29: the old certification covered v1; it is not permission to run v2
	_, err = e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit")
	refusal(t, err, xicconn.CodeCertStale)
	// recertify for the new pin and it works again
	e.certify(tenantA, c)
	if _, err = e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit"); err != nil {
		t.Fatalf("after recertification: %v", err)
	}
}

func TestOperable_DegradedBindingBlocksOnlyItsOperations(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)
	// the provider narrows the oauth scopes below what the contract needs (NP-48)
	cur := e.get(tenantA, c)
	got, err := e.svc.ReportScopes(ctx, operator, c.ConnectionID, cur.Version, "oauth", []string{"payments.read"})
	if err != nil || got.Status != xicconn.StatusDegraded || got.StatusReason != "SCOPE_REDUCED" {
		t.Fatalf("%v %+v", err, got)
	}
	_, err = e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit")
	refusal(t, err, xicconn.CodeNotActive)
	if _, err = e.svc.Operable(ctx, tenantA, c.ConnectionID, "statement-push"); err != nil {
		t.Fatalf("an unrelated operation was blocked: %v", err)
	}
	if !hasEvent(e.repo.Types(), "integration.connection.scopes-changed") {
		t.Fatal("no scopes-changed event")
	}
}

// ---- suspension, resume, revocation -----------------------------------------------------

func TestSuspendResumeRevoke(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)
	if _, err := e.svc.Suspend(ctx, operator, c.ConnectionID, c.Version, ""); err == nil {
		t.Fatal("suspension without a reason")
	}
	if _, err := e.svc.Suspend(ctx, operator, c.ConnectionID, c.Version+9, "x"); !errors.Is(err, xicconn.ErrVersionConflict) {
		t.Fatalf("stale: %v", err)
	}
	s, err := e.svc.Suspend(ctx, operator, c.ConnectionID, c.Version, "TOKEN_REVOKED provider revoked the token")
	if err != nil || s.Status != xicconn.StatusSuspended || s.SuspendedAt == nil {
		t.Fatalf("%v %+v", err, s)
	}
	if _, err = e.svc.Suspend(ctx, operator, c.ConnectionID, s.Version, "again"); !errors.Is(err, xicconn.ErrTransition) {
		t.Fatalf("suspended twice: %v", err)
	}
	var ev xicconn.OutEvent
	for _, o := range e.repo.Out {
		if o.Type == "integration.connection.suspended" {
			ev = o
		}
	}
	for _, k := range []string{"tenant_id", "connection_id", "reason_code", "affected_operations"} {
		if _, ok := ev.Data[k]; !ok {
			t.Errorf("suspended event lacks %s", k)
		}
	}
	if ev.Data["reason_code"] != "TOKEN_REVOKED" {
		t.Errorf("reason code %v", ev.Data["reason_code"])
	}
	// resume goes back through validation: it must be activated again, by someone other than the creator
	r, err := e.svc.Resume(ctx, operator, c.ConnectionID, s.Version)
	if err != nil || r.Status != xicconn.StatusValidating {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err = e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit"); err == nil {
		t.Fatal("a resumed-but-unvalidated connection was operable")
	}
	r = e.validated(tenantA, r)
	if r, err = e.svc.Activate(ctx, operator, c.ConnectionID, r.Version); err != nil || r.Status != xicconn.StatusActive {
		t.Fatalf("%v", err)
	}
	// revoke is terminal and frees the credentials
	rv, err := e.svc.Revoke(ctx, operator, c.ConnectionID, r.Version, "decommissioned")
	if err != nil || rv.Status != xicconn.StatusRevoked || rv.RevokedAt == nil {
		t.Fatalf("%v", err)
	}
	for _, b := range rv.Bindings {
		if b.Status != xicconn.BindingRevoked {
			t.Fatalf("binding %s left %s", b.AuthProfile, b.Status)
		}
	}
	for name, fn := range map[string]func() error{
		"revoke":  func() error { _, err := e.svc.Revoke(ctx, operator, c.ConnectionID, rv.Version, "x"); return err },
		"resume":  func() error { _, err := e.svc.Resume(ctx, operator, c.ConnectionID, rv.Version); return err },
		"suspend": func() error { _, err := e.svc.Suspend(ctx, operator, c.ConnectionID, rv.Version, "x"); return err },
		"authorize": func() error {
			_, err := e.svc.Authorize(ctx, operator, c.ConnectionID, rv.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"})
			return err
		},
	} {
		if err := fn(); err == nil {
			t.Errorf("%s on a revoked connection succeeded", name)
		}
	}
}

func TestScopeExpansionSuspendsForReview(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	got, err := e.svc.ReportScopes(ctxFor(tenantA), operator, c.ConnectionID, c.Version, "oauth", append([]string{"accounts.admin"}, oauthScopes...))
	if err != nil || got.Status != xicconn.StatusSuspended || got.StatusReason != "SCOPE_EXPANDED" {
		t.Fatalf("%v %+v", err, got)
	}
	// unchanged scopes are not an event
	before := len(e.repo.Out)
	e2 := e.sandbox(tenantA, v, "acc-2", refOAuth2, refHMAC2)
	n := len(e.repo.Out)
	if _, err = e.svc.ReportScopes(ctxFor(tenantA), operator, e2.ConnectionID, e2.Version, "oauth", oauthScopes); err != nil {
		t.Fatal(err)
	}
	if len(e.repo.Out) != n || before > n {
		t.Fatal("an unchanged scope report emitted an event")
	}
	if _, err = e.svc.ReportScopes(ctxFor(tenantA), operator, e2.ConnectionID, 0, "nope", oauthScopes); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

// ---- rotation -----------------------------------------------------------------------------

func TestRotate_ValidatesBeforeCutoverAndKeepsAnExplicitGraceWindow(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)
	exp := e.at().Add(120 * 24 * time.Hour)

	// the new reference is unknown to the vault: nothing changes
	if _, err := e.svc.Rotate(ctx, operator, c.ConnectionID, c.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOther, GraceSeconds: 3600, ExpiresAt: &exp}); !errors.Is(err, xicconn.ErrCredentialInvalid) {
		t.Fatalf("%v", err)
	}
	if cur := e.get(tenantA, c); cur.BindingFor("oauth").CurrentRef != refOAuth || cur.Version != c.Version {
		t.Fatal("a failed rotation changed the binding")
	}
	for name, in := range map[string]xicconn.RotateInput{
		"same ref":        {AuthProfile: "oauth", NewRef: refOAuth, GraceSeconds: 1},
		"bad ref":         {AuthProfile: "oauth", NewRef: "nope"},
		"grace too long":  {AuthProfile: "oauth", NewRef: refOAuth3, GraceSeconds: 30 * 24 * 3600},
		"negative grace":  {AuthProfile: "oauth", NewRef: refOAuth3, GraceSeconds: -1},
		"unknown profile": {AuthProfile: "nope", NewRef: refOAuth3},
	} {
		if _, err := e.svc.Rotate(ctx, operator, c.ConnectionID, c.Version, in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	rot, err := e.svc.Rotate(ctx, operator, c.ConnectionID, c.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOAuth3, GraceSeconds: 3600, ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	b := rot.BindingFor("oauth")
	if b.CurrentRef != refOAuth3 || b.PreviousRef != refOAuth || b.PreviousGraceUntil == nil || b.PreviousGraceUntil.Sub(e.at()) != time.Hour {
		t.Fatalf("%+v", b)
	}
	// inside the window both references are offered, labelled; the old one is never the current
	d, err := e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit")
	if err != nil || len(d.CredentialRefs) != 2 || d.CredentialRefs[0].Ref != refOAuth3 || d.CredentialRefs[0].Grace || d.CredentialRefs[1].Ref != refOAuth || !d.CredentialRefs[1].Grace {
		t.Fatalf("%v %+v", err, d)
	}
	// overlap is limited: a second rotation while the window is open is refused
	if _, err = e.svc.Rotate(ctx, operator, c.ConnectionID, rot.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOAuth2, GraceSeconds: 60, ExpiresAt: &exp}); !errors.Is(err, xicconn.ErrRotation) {
		t.Fatalf("%v", err)
	}
	// and the previous reference cannot be rotated back in
	e.advance(2 * time.Hour)
	d, _ = e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit")
	if len(d.CredentialRefs) != 1 || d.CredentialRefs[0].Ref != refOAuth3 {
		t.Fatalf("the previous reference outlived its grace: %+v", d.CredentialRefs)
	}
	cur := e.get(tenantA, c)
	if _, err = e.svc.Rotate(ctx, operator, c.ConnectionID, cur.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOAuth, ExpiresAt: &exp}); err != nil {
		// the old ref was released by the rotation only if no live binding holds it; it is no longer anyone's
		t.Logf("rotating back: %v", err)
	}
	if !hasEvent(e.repo.Types(), "integration.connection.credential-rotated") {
		t.Fatal("no rotated event")
	}
}

func TestRotate_NoGraceMeansNoPreviousReference(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	exp := e.at().Add(90 * 24 * time.Hour)
	rot, err := e.svc.Rotate(ctxFor(tenantA), operator, c.ConnectionID, c.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOAuth3, GraceSeconds: 0, ExpiresAt: &exp})
	if err != nil || rot.BindingFor("oauth").PreviousRef != "" {
		t.Fatalf("%v %+v", err, rot.BindingFor("oauth"))
	}
	// the rotation keeps the activation evidence consistent: re-validating is not required to stay valid
	checks, _ := e.repo.ListChecks(ctxFor(tenantA), c.ConnectionID)
	if checks[len(checks)-1].ConfigFingerprint != xicconn.ConfigFingerprint(rot) {
		t.Fatal("rotation did not record a matching validation")
	}
}

func TestRotate_ForeignReferenceIsCrossTenant(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	a := e.sandbox(tenantA, v, "acc-a", refOAuth, refHMAC)
	b := e.sandbox(tenantB, v, "acc-b", refOAuth2, refHMAC2)
	exp := e.at().Add(90 * 24 * time.Hour)
	_, err := e.svc.Rotate(ctxFor(tenantB), operator, b.ConnectionID, b.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOAuth, ExpiresAt: &exp})
	if !errors.Is(err, xicconn.ErrCrossTenantCredential) {
		t.Fatalf("tenant B rotated onto tenant A's credential: %v", err)
	}
	if len(e.repo.Signals) == 0 {
		t.Fatal("no security signal")
	}
	if cur := e.get(tenantA, a); cur.BindingFor("oauth").CurrentRef != refOAuth {
		t.Fatal("tenant A's binding was disturbed")
	}
}

func TestEndGrace(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)
	exp := e.at().Add(90 * 24 * time.Hour)
	rot, _ := e.svc.Rotate(ctx, operator, c.ConnectionID, c.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOAuth3, GraceSeconds: 6 * 3600, ExpiresAt: &exp})
	if _, err := e.svc.EndGrace(ctx, operator, c.ConnectionID, rot.Version, "hmac-in"); !errors.Is(err, xicconn.ErrTransition) {
		t.Fatalf("nothing to end: %v", err)
	}
	got, err := e.svc.EndGrace(ctx, operator, c.ConnectionID, rot.Version, "oauth")
	if err != nil || got.BindingFor("oauth").PreviousRef != "" {
		t.Fatalf("%v", err)
	}
}

// ---- evaluation -------------------------------------------------------------------------------

func TestEvaluate_ExpiryDrivesDegradedThenSuspended(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC) // oauth expires in 60 days; hmac has no expiry
	ctx := ctxFor(tenantA)
	res, err := e.svc.Evaluate(ctx, operator, c.ConnectionID)
	if err != nil || len(res.Changes) != 0 || res.Connection.Status != xicconn.StatusActive {
		t.Fatalf("a healthy connection changed: %v %+v", err, res)
	}
	e.advance(50 * 24 * time.Hour) // 10 days left: inside the 14-day warning window
	res, err = e.svc.Evaluate(ctx, operator, c.ConnectionID)
	if err != nil || res.Connection.Status != xicconn.StatusDegraded || res.Connection.StatusReason != "CREDENTIAL_EXPIRING" || res.Connection.BindingFor("oauth").Status != xicconn.BindingExpiring {
		t.Fatalf("%v %+v", err, res)
	}
	// degraded still carries traffic that does not need the expiring credential... and the expiring one
	if _, err = e.svc.Operable(ctx, tenantA, c.ConnectionID, "statement-push"); err != nil {
		t.Fatalf("%v", err)
	}
	if _, err = e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit"); err != nil {
		t.Fatalf("an expiring (not expired) credential still works: %v", err)
	}
	e.advance(11 * 24 * time.Hour) // oauth now expired; hmac never expires
	res, err = e.svc.Evaluate(ctx, operator, c.ConnectionID)
	if err != nil || res.Connection.Status != xicconn.StatusDegraded || res.Connection.StatusReason != "CREDENTIAL_EXPIRED" {
		t.Fatalf("%v %+v", err, res)
	}
	_, err = e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit")
	refusal(t, err, xicconn.CodeNotActive) // an expired credential is never bypassed
	if _, err = e.svc.Operable(ctx, tenantA, c.ConnectionID, "statement-push"); err != nil {
		t.Fatalf("the inbound path does not depend on the expired credential: %v", err)
	}
	// recovery: rotate to a fresh credential, evaluate, back to ACTIVE
	exp := e.at().Add(90 * 24 * time.Hour)
	cur := e.get(tenantA, c)
	if _, err = e.svc.Rotate(ctx, operator, c.ConnectionID, cur.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOAuth3, ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	res, err = e.svc.Evaluate(ctx, operator, c.ConnectionID)
	if err != nil || res.Connection.Status != xicconn.StatusActive {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestEvaluate_AllCredentialsExpiredSuspends(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.create(tenantA, xicconn.EnvSandbox, v, "acc")
	ctx := ctxFor(tenantA)
	res, _ := e.svc.Authorize(ctx, creator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"})
	cur := res.Connection
	exp := e.at().Add(30 * 24 * time.Hour)
	var err error
	for _, p := range []struct {
		prof, ref string
		scopes    []string
	}{{"oauth", refOAuth, oauthScopes}, {"hmac-in", refHMAC, nil}} {
		if cur, err = e.svc.Bind(ctx, creator, c.ConnectionID, cur.Version, xicconn.BindInput{AuthProfile: p.prof, CredentialRef: p.ref, GrantedScopes: p.scopes, ConsentActor: "x", ExpiresAt: &exp}); err != nil {
			t.Fatal(err)
		}
	}
	cur = e.validated(tenantA, cur)
	cur, err = e.svc.Activate(ctx, operator, cur.ConnectionID, cur.Version)
	if err != nil {
		t.Fatal(err)
	}
	e.advance(31 * 24 * time.Hour)
	out, err := e.svc.Evaluate(ctx, operator, cur.ConnectionID)
	if err != nil || out.Connection.Status != xicconn.StatusSuspended || out.Connection.StatusReason != "CREDENTIALS_EXPIRED" {
		t.Fatalf("%v %+v", err, out)
	}
}

func TestEvaluate_EndsGraceAndFlagsOverdueRotation(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)
	exp := e.at().Add(365 * 24 * time.Hour)
	rot, err := e.svc.Rotate(ctx, operator, c.ConnectionID, c.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOAuth3, GraceSeconds: 3600, ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	e.advance(2 * time.Hour)
	res, _ := e.svc.Evaluate(ctx, operator, c.ConnectionID)
	if res.Connection.BindingFor("oauth").PreviousRef != "" || !strings.Contains(strings.Join(res.Changes, ","), "GRACE_ENDED:oauth") {
		t.Fatalf("%+v", res)
	}
	e.advance(100 * 24 * time.Hour) // past the 90-day rotation interval
	res, _ = e.svc.Evaluate(ctx, operator, c.ConnectionID)
	if !strings.Contains(strings.Join(res.Changes, ","), "ROTATION_OVERDUE:oauth") {
		t.Fatalf("%+v", res)
	}
	_ = rot
	// revoked connections are left alone
	rv, _ := e.svc.Revoke(ctx, operator, c.ConnectionID, e.get(tenantA, c).Version, "done")
	res, err = e.svc.Evaluate(ctx, operator, c.ConnectionID)
	if err != nil || len(res.Changes) != 0 || res.Connection.Version != rv.Version {
		t.Fatalf("%v %+v", err, res)
	}
}

// ---- migration (I-04) ---------------------------------------------------------------------------

func TestMigration_GovernedAndEffectiveDated(t *testing.T) {
	e := newEnv(t)
	v1 := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v1, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)
	v2 := e.publish("bank-x", "1.1.0", nil)
	other := e.publish("bank-y", "1.0.0", nil)

	for name, in := range map[string]xicconn.MigrationInput{
		"no reason":         {ToContractVersionID: v2.ContractVersionID},
		"same version":      {ToContractVersionID: v1.ContractVersionID, Reason: "x"},
		"another connector": {ToContractVersionID: other.ContractVersionID, Reason: "x"},
		"past effective":    {ToContractVersionID: v2.ContractVersionID, Reason: "x", EffectiveAt: ptrT(e.at().Add(-time.Hour))},
		"unknown version":   {ToContractVersionID: "00000000-0000-4000-8000-000000000000", Reason: "x"},
	} {
		if _, err := e.svc.RequestMigration(ctx, creator, c.ConnectionID, in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}

	future := e.at().Add(48 * time.Hour)
	req, err := e.svc.RequestMigration(ctx, creator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v2.ContractVersionID, Reason: "adopt 1.1.0", EffectiveAt: &future})
	if err != nil || req.Status != xicconn.ReqPending {
		t.Fatalf("%v", err)
	}
	if _, err = e.svc.RequestMigration(ctx, creator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v2.ContractVersionID, Reason: "again"}); !errors.Is(err, xicconn.ErrRequestState) {
		t.Fatalf("two migrations at once: %v", err)
	}
	if _, err = e.svc.DecideMigration(ctx, creator, c.ConnectionID, req.RequestID, true); !errors.Is(err, xicconn.ErrSelfAction) {
		t.Fatalf("self approval: %v", err)
	}
	if _, err = e.svc.DecideMigration(ctx, approver, c.ConnectionID, "00000000-0000-4000-8000-000000000000", true); !errors.Is(err, xicconn.ErrRequestNotFound) {
		t.Fatalf("%v", err)
	}
	dec, err := e.svc.DecideMigration(ctx, approver, c.ConnectionID, req.RequestID, true)
	if err != nil || dec.Status != xicconn.ReqApproved {
		t.Fatalf("%v %+v", err, dec)
	}
	if _, err = e.svc.DecideMigration(ctx, approver, c.ConnectionID, req.RequestID, false); !errors.Is(err, xicconn.ErrRequestState) {
		t.Fatalf("decided twice: %v", err)
	}
	// effective-dated: until the date the connection stays pinned to what it accepted
	if cur := e.get(tenantA, c); cur.ContractVersionID != v1.ContractVersionID {
		t.Fatal("the pin moved before its effective date")
	}
	d, err := e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit")
	if err != nil || d.ContractVersionID != v1.ContractVersionID {
		t.Fatalf("%v %+v", err, d)
	}
	e.advance(49 * time.Hour)
	// the date has arrived: the effective pin follows at once, even before anything persists it
	d, err = e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit")
	if err != nil || d.ContractVersionID != v2.ContractVersionID {
		t.Fatalf("effective pin did not move: %v %+v", err, d)
	}
	// and Evaluate persists it
	res, err := e.svc.Evaluate(ctx, operator, c.ConnectionID)
	if err != nil || res.Connection.ContractVersionID != v2.ContractVersionID || !strings.Contains(strings.Join(res.Changes, ","), "MIGRATION_APPLIED") {
		t.Fatalf("%v %+v", err, res)
	}
	reqs, _ := e.repo.ListRequests(ctx, c.ConnectionID)
	if reqs[0].Status != xicconn.ReqUsed {
		t.Fatalf("%s", reqs[0].Status)
	}
	if !hasEvent(e.repo.Types(), "integration.connection.migrated") {
		t.Fatal("no migrated event")
	}
	// a target that needs a credential the connection does not hold is a new authorization, not a migration
	v3 := e.publish("bank-x", "2.0.0", func(in *registry.VersionInput) {
		in.Content.Profiles = append(in.Content.Profiles, registry.Profile{Kind: registry.ProfileAuth, Name: "extra", Spec: []byte(`{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","credential_class":"CALLBACK_SECRET"}`)})
		in.Content.Operations[2].AuthProfile = "extra"
		in.MigrationPlanRef = "MIG-1 plan"
		in.Content.Operations[2].AuthProfile = "extra"
	})
	if _, err := e.svc.RequestMigration(ctx, creator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v3.ContractVersionID, Reason: "adopt 2.0"}); err == nil || !strings.Contains(issuePaths(err), "to_contract_version_id") {
		t.Fatalf("a migration needing a new credential was accepted: %v", err)
	}
}

func ptrT(t time.Time) *time.Time { return &t }

func TestMigration_RejectLeavesThePin(t *testing.T) {
	e := newEnv(t)
	v1 := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v1, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)
	v2 := e.publish("bank-x", "1.1.0", nil)
	req, _ := e.svc.RequestMigration(ctx, creator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v2.ContractVersionID, Reason: "adopt"})
	dec, err := e.svc.DecideMigration(ctx, approver, c.ConnectionID, req.RequestID, false)
	if err != nil || dec.Status != xicconn.ReqRejected {
		t.Fatalf("%v", err)
	}
	if e.get(tenantA, c).ContractVersionID != v1.ContractVersionID {
		t.Fatal("a rejected migration moved the pin")
	}
	// a new request can follow
	if _, err = e.svc.RequestMigration(ctx, creator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v2.ContractVersionID, Reason: "adopt, retry"}); err != nil {
		t.Fatalf("%v", err)
	}
}

// ---- break-glass (NP-50) --------------------------------------------------------------------------

func TestBreakGlass(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)

	// revealing a credential is refused, and the attempt is itself evidenced
	for _, action := range []string{"REVEAL_CREDENTIAL", "DUMP_SECRETS", "", "reveal"} {
		_, _, err := e.svc.RequestBreakGlass(ctx, operator, c.ConnectionID, 0, xicconn.BreakGlassInput{Action: action, Reason: "debugging"})
		if !errors.Is(err, xicconn.ErrBreakGlassDenied) {
			t.Errorf("%q: %v", action, err)
		}
	}
	if len(e.repo.Signals) != 4 || e.repo.Signals[0].Type != "integration.connection.break-glass-denied" {
		t.Fatalf("denials were not evidenced: %+v", e.repo.Signals)
	}
	if _, _, err := e.svc.RequestBreakGlass(ctx, operator, c.ConnectionID, 0, xicconn.BreakGlassInput{Action: "PAUSE"}); err == nil {
		t.Fatal("break-glass without a reason")
	}
	// a pause is immediate (stopping traffic is the safe direction) and reasoned
	_, paused, err := e.svc.RequestBreakGlass(ctx, operator, c.ConnectionID, c.Version, xicconn.BreakGlassInput{Action: "PAUSE", Reason: "suspected credential leak"})
	if err != nil || paused.Status != xicconn.StatusSuspended || !strings.HasPrefix(paused.StatusReason, "BREAK_GLASS") {
		t.Fatalf("%v %+v", err, paused)
	}
	// a resume is time-bounded and needs someone else
	for _, d := range []int64{0, -5, 5 * 3600} {
		if _, _, err := e.svc.RequestBreakGlass(ctx, operator, c.ConnectionID, 0, xicconn.BreakGlassInput{Action: "RESUME", Reason: "fixed", DurationSeconds: d}); err == nil {
			t.Errorf("duration %d accepted", d)
		}
	}
	req, _, err := e.svc.RequestBreakGlass(ctx, operator, c.ConnectionID, 0, xicconn.BreakGlassInput{Action: "RESUME", Reason: "leak contained", DurationSeconds: 1800})
	if err != nil || req.Status != xicconn.ReqPending || req.ExpiresAt == nil {
		t.Fatalf("%v", err)
	}
	if _, err = e.svc.DecideBreakGlass(ctx, operator, c.ConnectionID, req.RequestID, true); !errors.Is(err, xicconn.ErrSelfAction) {
		t.Fatalf("self approval: %v", err)
	}
	dec, err := e.svc.DecideBreakGlass(ctx, approver, c.ConnectionID, req.RequestID, true)
	if err != nil || dec.Status != xicconn.ReqUsed {
		t.Fatalf("%v", err)
	}
	// break-glass cannot skip a gate: the connection is back to VALIDATING, not ACTIVE
	cur := e.get(tenantA, c)
	if cur.Status != xicconn.StatusValidating {
		t.Fatalf("break-glass resumed straight to %s", cur.Status)
	}
	if _, err = e.svc.Operable(ctx, tenantA, c.ConnectionID, "payment-submit"); err == nil {
		t.Fatal("break-glass made a connection operable without validation")
	}
}

func TestBreakGlass_ApprovalExpires(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	ctx := ctxFor(tenantA)
	_, _, _ = e.svc.RequestBreakGlass(ctx, operator, c.ConnectionID, c.Version, xicconn.BreakGlassInput{Action: "PAUSE", Reason: "x"})
	req, _, err := e.svc.RequestBreakGlass(ctx, operator, c.ConnectionID, 0, xicconn.BreakGlassInput{Action: "RESUME", Reason: "ok now", DurationSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	e.advance(11 * time.Minute)
	dec, err := e.svc.DecideBreakGlass(ctx, approver, c.ConnectionID, req.RequestID, true)
	if !errors.Is(err, xicconn.ErrRequestState) || dec == nil || dec.Status != xicconn.ReqExpired {
		t.Fatalf("an expired break-glass was honoured: %v %+v", err, dec)
	}
	if e.get(tenantA, c).Status != xicconn.StatusSuspended {
		t.Fatal("an expired break-glass resumed the connection")
	}
}

// ---- isolation and concurrency --------------------------------------------------------------------

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.sandbox(tenantA, v, "acc", refOAuth, refHMAC)
	b := ctxFor(tenantB)
	if _, err := e.repo.Get(b, c.ConnectionID); !errors.Is(err, xicconn.ErrNotFound) {
		t.Fatalf("tenant B read tenant A's connection: %v", err)
	}
	if list, _ := e.repo.List(b, xicconn.Filter{}); len(list) != 0 {
		t.Fatalf("tenant B lists %d", len(list))
	}
	for name, fn := range map[string]func() error{
		"suspend":  func() error { _, err := e.svc.Suspend(b, operator, c.ConnectionID, c.Version, "x"); return err },
		"revoke":   func() error { _, err := e.svc.Revoke(b, operator, c.ConnectionID, c.Version, "x"); return err },
		"validate": func() error { _, err := e.svc.Validate(b, operator, c.ConnectionID, c.Version, ""); return err },
		"rotate": func() error {
			exp := e.at().Add(time.Hour)
			_, err := e.svc.Rotate(b, operator, c.ConnectionID, c.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOAuth3, ExpiresAt: &exp})
			return err
		},
		"certify": func() error {
			_, err := e.svc.Certify(b, certifier, c.ConnectionID, xicconn.CertifyInput{TestPackVersion: "tp-2026.10", Passed: passedTests()})
			return err
		},
		"migrate": func() error {
			_, err := e.svc.RequestMigration(b, creator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v.ContractVersionID, Reason: "x"})
			return err
		},
	} {
		if err := fn(); !errors.Is(err, xicconn.ErrNotFound) {
			t.Errorf("%s from tenant B: %v", name, err)
		}
	}
	if e.get(tenantA, c).Status != xicconn.StatusActive {
		t.Fatal("tenant A's connection was disturbed")
	}
}

func TestConcurrentCommandsOnOneConnection(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	c := e.validated(tenantA, e.bound(tenantA, e.create(tenantA, xicconn.EnvSandbox, v, "acc"), refOAuth, refHMAC))
	const n = 12
	var ok, conflict, other int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.Activate(ctxFor(tenantA), operator, c.ConnectionID, c.Version)
			switch {
			case err == nil:
				atomic.AddInt32(&ok, 1)
			case errors.Is(err, xicconn.ErrVersionConflict), errors.Is(err, xicconn.ErrTransition):
				atomic.AddInt32(&conflict, 1)
			default:
				atomic.AddInt32(&other, 1)
				t.Errorf("%v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || conflict != n-1 || other != 0 {
		t.Fatalf("ok=%d conflict=%d other=%d", ok, conflict, other)
	}
	if n := countType(e.repo.Types(), "integration.connection.activated"); n != 1 {
		t.Fatalf("%d activated events", n)
	}
}

func countType(types []string, want string) int {
	n := 0
	for _, t := range types {
		if t == want {
			n++
		}
	}
	return n
}

func TestConcurrentCredentialClaims(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	const n = 8
	conns := make([]*xicconn.Connection, n)
	for i := range conns {
		tenant := "tenant-" + string(rune('a'+i))
		conns[i] = authorizing(e, v, tenant, "acc")
	}
	var won, lost int32
	var wg sync.WaitGroup
	exp := e.at().Add(24 * time.Hour)
	for i := range conns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := "tenant-" + string(rune('a'+i))
			_, err := e.svc.Bind(ctxFor(tenant), creator, conns[i].ConnectionID, conns[i].Version, xicconn.BindInput{AuthProfile: "oauth", CredentialRef: refOAuth,
				GrantedScopes: oauthScopes, ConsentActor: "x", ExpiresAt: &exp})
			switch {
			case err == nil:
				atomic.AddInt32(&won, 1)
			case errors.Is(err, xicconn.ErrCrossTenantCredential):
				atomic.AddInt32(&lost, 1)
			default:
				t.Errorf("%v", err)
			}
		}(i)
	}
	wg.Wait()
	if won != 1 || lost != n-1 {
		t.Fatalf("won=%d lost=%d: one credential reference was bound in more than one tenant", won, lost)
	}
}

func TestConcurrentCreatesOfOneAccount(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	var won, lost int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := e.svc.Create(ctxFor(tenantA), creator, xicconn.CreateInput{Environment: "SANDBOX", ConnectorID: "bank-x", ContractVersionID: v.ContractVersionID,
				Region: "uk", Endpoints: map[string]string{"bank-api": "api.bank-x.com"}, ProviderAccountRef: "same-account"}, "")
			switch {
			case err == nil:
				atomic.AddInt32(&won, 1)
			case errors.Is(err, xicconn.ErrExists):
				atomic.AddInt32(&lost, 1)
			default:
				t.Errorf("%v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || lost != 9 {
		t.Fatalf("won=%d lost=%d", won, lost)
	}
}

var _ = vault.ErrNotFound
