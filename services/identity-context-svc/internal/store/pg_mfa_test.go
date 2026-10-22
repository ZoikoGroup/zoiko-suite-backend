package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/store"
)

// Migration 000012 and the TOTP store methods against real Postgres, through
// a NOSUPERUSER NOBYPASSRLS role so row security is in force (a superuser
// would hide an RLS defect).

func pendingFactor(id, principal, tenant string) domain.TOTPFactorSecret {
	return domain.TOTPFactorSecret{FactorID: id, PrincipalID: principal, TenantID: tenant,
		Status: domain.CredentialStatusPending, Ciphertext: "ct-" + id, KeyID: "k1"}
}

func TestPgStore_TOTPLifecycle_Integration(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	seedCredential(t, ctx, admin, store.New(admin, zap.NewNop()), "p-1", "t-1", "idp|alice")
	s := store.New(ordinaryRolePool(t, admin), zap.NewNop())

	if err := s.CreatePendingTOTP(ctx, pendingFactor("f-1", "p-1", "t-1")); err != nil {
		t.Fatalf("create pending: %v", err)
	}
	// A second enrolment retires the first (one PENDING per principal).
	if err := s.CreatePendingTOTP(ctx, pendingFactor("f-2", "p-1", "t-1")); err != nil {
		t.Fatalf("second pending: %v", err)
	}
	f1, err := s.FindTOTPFactor(ctx, "f-1", "p-1", "t-1")
	if err != nil || f1.Status != domain.CredentialStatusRetired {
		t.Fatalf("superseded enrolment: %+v %v", f1, err)
	}
	if has, _ := s.HasActiveTOTP(ctx, "p-1", "t-1"); has {
		t.Fatal("a PENDING factor counted as active")
	}

	// Activation, then the replay guard.
	rec, _ := events.Render(events.EventMFAFactorEnrolled, "t-1", "", "p-1", "corr", "p-1", map[string]any{"factor_id": "f-2"})
	if err := s.AcceptTOTPCode(ctx, domain.TOTPAcceptance{FactorID: "f-2", PrincipalID: "p-1", TenantID: "t-1",
		Step: 100, Activate: true, Action: "MFA_ACTIVATE", CorrelationID: "corr"}, rec); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if has, _ := s.HasActiveTOTP(ctx, "p-1", "t-1"); !has {
		t.Fatal("activated factor not active")
	}
	active, err := s.FindActiveTOTP(ctx, "p-1", "t-1")
	if err != nil || active.FactorID != "f-2" || active.LastUsedStep == nil || *active.LastUsedStep != 100 {
		t.Fatalf("active factor: %+v %v", active, err)
	}
	for _, step := range []int64{100, 99} {
		err := s.AcceptTOTPCode(ctx, domain.TOTPAcceptance{FactorID: "f-2", PrincipalID: "p-1", TenantID: "t-1",
			Step: step, Action: "MFA_STEP_UP"}, outbox.Record{})
		if !errors.Is(err, domain.ErrTOTPReplayed) {
			t.Fatalf("step %d after 100: want ErrTOTPReplayed, got %v", step, err)
		}
	}
	if err := s.AcceptTOTPCode(ctx, domain.TOTPAcceptance{FactorID: "f-2", PrincipalID: "p-1", TenantID: "t-1",
		Step: 101, Action: "MFA_STEP_UP"}, outbox.Record{}); err != nil {
		t.Fatalf("next step: %v", err)
	}
	// Another principal cannot use the factor.
	if err := s.AcceptTOTPCode(ctx, domain.TOTPAcceptance{FactorID: "f-2", PrincipalID: "p-2", TenantID: "t-1",
		Step: 500, Action: "MFA_STEP_UP"}, outbox.Record{}); !errors.Is(err, domain.ErrTOTPReplayed) {
		t.Fatalf("someone else's factor: %v", err)
	}

	// Lockout, then even a fresh step is refused while locked.
	var locked bool
	for i := 0; i < 3; i++ {
		locked, _, err = s.RecordTOTPFailure(ctx, "f-2", "p-1", "t-1", "MFA_STEP_UP", "corr", 3, time.Minute)
		if err != nil {
			t.Fatalf("record failure: %v", err)
		}
	}
	if !locked {
		t.Fatal("factor not locked after max failures")
	}
	if err := s.AcceptTOTPCode(ctx, domain.TOTPAcceptance{FactorID: "f-2", PrincipalID: "p-1", TenantID: "t-1",
		Step: 200, Action: "MFA_STEP_UP"}, outbox.Record{}); !errors.Is(err, domain.ErrTOTPReplayed) {
		t.Fatalf("locked factor accepted a code: %v", err)
	}

	// Re-enrolment activation retires the old ACTIVE factor in the same tx.
	if err := s.CreatePendingTOTP(ctx, pendingFactor("f-3", "p-1", "t-1")); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptTOTPCode(ctx, domain.TOTPAcceptance{FactorID: "f-3", PrincipalID: "p-1", TenantID: "t-1",
		Step: 300, Activate: true, Action: "MFA_ACTIVATE"}, outbox.Record{}); err != nil {
		t.Fatalf("replacement activation: %v", err)
	}
	if old, _ := s.FindTOTPFactor(ctx, "f-2", "p-1", "t-1"); old.Status != domain.CredentialStatusRetired {
		t.Fatalf("replaced factor still %s", old.Status)
	}

	// Retire all; the password credential is untouched and still logs in.
	reset, _ := events.Render(events.EventMFAFactorReset, "t-1", "", "admin", "corr", "p-1", map[string]any{})
	n, err := s.RetireTOTPFactors(ctx, "p-1", "t-1", "", "admin_reset:LOST", "admin", "MFA_RESET", "corr", reset)
	if err != nil || n != 1 {
		t.Fatalf("retire: n=%d %v", n, err)
	}
	if n, _ := s.RetireTOTPFactors(ctx, "p-1", "t-1", "", "again", "admin", "MFA_RESET", "corr", outbox.Record{}); n != 0 {
		t.Fatalf("second retire retired %d", n)
	}
	if _, cred, err := s.FindActiveCredentialByEmail(ctx, "idp|alice@zoiko.io", "t-1"); err != nil || cred.CredentialType != domain.CredentialTypePassword {
		t.Fatalf("password lookup with TOTP rows present: %+v %v", cred, err)
	}
	list, err := s.ListMFAFactors(ctx, "p-1", "t-1")
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %d %v", len(list), err)
	}

	// Evidence and events committed with the changes.
	var decisions, outboxRows int
	_ = admin.QueryRow(ctx, `SELECT COUNT(*) FROM access_decision_log WHERE principal_id = 'p-1' AND action_type LIKE 'MFA_%'`).Scan(&decisions)
	_ = admin.QueryRow(ctx, `SELECT COUNT(*) FROM event_outbox WHERE event_type IN ('identity.mfa_factor.enrolled', 'identity.mfa_factor.reset')`).Scan(&outboxRows)
	if decisions < 7 || outboxRows != 2 {
		t.Fatalf("evidence rows %d, outbox rows %d", decisions, outboxRows)
	}
}

func TestPgStore_TOTPRowsAreTenantScoped_Integration(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	seedCredential(t, ctx, admin, store.New(admin, zap.NewNop()), "p-1", "t-1", "idp|alice")
	s := store.New(ordinaryRolePool(t, admin), zap.NewNop())
	if err := s.CreatePendingTOTP(ctx, pendingFactor("f-1", "p-1", "t-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindTOTPFactor(ctx, "f-1", "p-1", "t-2"); !errors.Is(err, domain.ErrCredentialNotFound) {
		t.Fatalf("factor visible from another tenant: %v", err)
	}
	if n, _ := s.RetireTOTPFactors(ctx, "p-1", "t-2", "", "x", "y", "MFA_RESET", "c", outbox.Record{}); n != 0 {
		t.Fatalf("another tenant retired %d factors", n)
	}
}

func TestMigration000012_ShapeAndDown_Integration(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	seedCredential(t, ctx, admin, store.New(admin, zap.NewNop()), "p-1", "t-1", "idp|alice")

	// A TOTP row holding a digest, or with no ciphertext, is refused.
	for _, bad := range []string{
		`INSERT INTO principal_credentials (credential_id, principal_id, tenant_id, credential_type, secret_hash, status)
		 VALUES ('x1', 'p-1', 't-1', 'TOTP', '$argon2id$x', 'PENDING')`,
		`INSERT INTO principal_credentials (credential_id, principal_id, tenant_id, credential_type, status)
		 VALUES ('x2', 'p-1', 't-1', 'TOTP', 'PENDING')`,
		`INSERT INTO principal_credentials (credential_id, principal_id, tenant_id, credential_type, secret_ciphertext, secret_key_id, status)
		 VALUES ('x3', 'p-1', 't-1', 'TOTP', 'ct', 'k1', 'ACTIVE')`, // ACTIVE without activated_at
		`INSERT INTO principal_credentials (credential_id, principal_id, tenant_id, credential_type, secret_hash, status)
		 VALUES ('x4', 'p-1', 't-1', 'PASSWORD', '$argon2id$x', 'PENDING')`, // PENDING is TOTP-only
	} {
		if _, err := admin.Exec(ctx, bad); err == nil {
			t.Fatalf("accepted: %s", bad)
		}
	}

	_, filename, _, _ := runtime.Caller(0)
	down, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "../../deployments/migrations/000012_totp_second_factor.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	s := store.New(admin, zap.NewNop())
	if err := s.CreatePendingTOTP(ctx, pendingFactor("f-1", "p-1", "t-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, string(down)); err == nil {
		t.Fatal("down migration dropped live TOTP factors")
	}
	if _, err := admin.Exec(ctx, `DELETE FROM principal_credentials WHERE credential_type = 'TOTP'`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down with no factors: %v", err)
	}
}
