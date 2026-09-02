// Integration tests for the credential store. Skipped unless
// TEST_DATABASE_URL is set, matching pg_store_test.go.
//
// These exist because the parts of the credential path that are easiest to get
// wrong live in SQL, not Go: the lockout counter is computed and applied in one
// statement so concurrent guesses cannot both read the same count, and RLS is
// what stops an unscoped connection reading every tenant's digests. Neither is
// observable from a unit test with a mocked store.
package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/store"
)

// A syntactically valid argon2id digest. The store never inspects it beyond a
// prefix check, so these tests do not need a real derivation.
const testDigest = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2ExMg$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGgx"

func seedCredential(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *store.PgStore, principalID, tenantID, subject string) {
	t.Helper()
	insertPrincipal(t, ctx, pool, principalID, tenantID, subject, "ACTIVE")
	if err := s.UpsertPasswordCredential(ctx, principalID, tenantID, testDigest, "argon2id"); err != nil {
		t.Fatalf("failed to seed credential: %v", err)
	}
}

func TestPgStore_FindActiveCredentialByEmail_Integration(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	seedCredential(t, ctx, pool, s, "p-1", "t-1", "idp|alice")

	principal, cred, err := s.FindActiveCredentialByEmail(ctx, "idp|alice@zoiko.io", "t-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if principal == nil || principal.PrincipalID != "p-1" {
		t.Fatalf("expected principal p-1, got %+v", principal)
	}
	if cred == nil || cred.SecretHash != testDigest {
		t.Fatalf("expected the seeded digest, got %+v", cred)
	}
	if cred.FailedAttemptCount != 0 || cred.LockedUntil != nil {
		t.Fatalf("a fresh credential must start unlocked with a zero counter, got %+v", cred)
	}
}

// The email a human types is not case-sensitive, and the unique index is built
// on LOWER(email), so the lookup has to match it.
func TestPgStore_FindActiveCredentialByEmail_IsCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	seedCredential(t, ctx, pool, s, "p-1", "t-1", "idp|alice")

	for _, typed := range []string{"IDP|ALICE@ZOIKO.IO", "  idp|alice@zoiko.io  ", "Idp|Alice@Zoiko.Io"} {
		principal, _, err := s.FindActiveCredentialByEmail(ctx, typed, "t-1")
		if err != nil {
			t.Fatalf("lookup of %q failed: %v", typed, err)
		}
		if principal == nil {
			t.Fatalf("lookup of %q found nothing", typed)
		}
	}
}

// A principal in another tenant must be invisible even though the email is an
// exact match. This is the isolation the RLS policy exists to enforce.
func TestPgStore_FindActiveCredentialByEmail_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	seedCredential(t, ctx, pool, s, "p-1", "t-1", "idp|alice")

	_, _, err := s.FindActiveCredentialByEmail(ctx, "idp|alice@zoiko.io", "t-2")
	if !errors.Is(err, domain.ErrPrincipalNotFound) {
		t.Fatalf("expected ErrPrincipalNotFound across tenants, got %v", err)
	}
}

// A principal with no password row must be reported distinctly from a missing
// principal — the caller needs the difference for the evidence log even though
// it collapses both on the wire.
func TestPgStore_FindActiveCredentialByEmail_PrincipalWithoutCredential(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|alice", "ACTIVE")

	principal, cred, err := s.FindActiveCredentialByEmail(ctx, "idp|alice@zoiko.io", "t-1")
	if !errors.Is(err, domain.ErrCredentialNotFound) {
		t.Fatalf("expected ErrCredentialNotFound, got %v", err)
	}
	if principal == nil {
		t.Fatalf("the principal must still be returned so the denial can be logged against it")
	}
	if cred != nil {
		t.Fatalf("expected no credential, got %+v", cred)
	}
}

// A SUSPENDED principal must not be able to authenticate itself back in.
// FindByIDPSubject admits SUSPENDED so an administrator can still resolve
// their context; this path must not.
func TestPgStore_FindActiveCredentialByEmail_ExcludesNonActivePrincipals(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	for _, status := range []string{"SUSPENDED", "DISABLED"} {
		id := "p-" + strings.ToLower(status)
		insertPrincipal(t, ctx, pool, id, "t-1", "idp|"+strings.ToLower(status), status)
		if err := s.UpsertPasswordCredential(ctx, id, "t-1", testDigest, "argon2id"); err != nil {
			t.Fatalf("seed failed: %v", err)
		}

		_, _, err := s.FindActiveCredentialByEmail(ctx, "idp|"+strings.ToLower(status)+"@zoiko.io", "t-1")
		if !errors.Is(err, domain.ErrPrincipalNotFound) {
			t.Fatalf("a %s principal must not authenticate, got %v", status, err)
		}
	}
}

// The counter increments and the lock engages exactly at the threshold, in the
// same statement — the property that makes concurrent guessing bounded.
func TestPgStore_RecordAuthFailure_LocksAtThreshold(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	seedCredential(t, ctx, pool, s, "p-1", "t-1", "idp|alice")
	_, cred, err := s.FindActiveCredentialByEmail(ctx, "idp|alice@zoiko.io", "t-1")
	if err != nil {
		t.Fatalf("seed lookup failed: %v", err)
	}

	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		locked, count, err := s.RecordAuthFailure(ctx, cred.CredentialID, "p-1", "t-1", "corr", maxAttempts, 15*time.Minute)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if count != attempt {
			t.Fatalf("attempt %d: expected count %d, got %d", attempt, attempt, count)
		}
		wantLocked := attempt >= maxAttempts
		if locked != wantLocked {
			t.Fatalf("attempt %d: expected locked=%v, got %v", attempt, wantLocked, locked)
		}
	}
}

// A success must clear both the counter and the lock, so a user who mistypes
// twice and then gets it right does not stay two attempts from a lockout.
func TestPgStore_RecordAuthSuccess_ClearsLockoutState(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	seedCredential(t, ctx, pool, s, "p-1", "t-1", "idp|alice")
	_, cred, err := s.FindActiveCredentialByEmail(ctx, "idp|alice@zoiko.io", "t-1")
	if err != nil {
		t.Fatalf("seed lookup failed: %v", err)
	}

	if _, _, err := s.RecordAuthFailure(ctx, cred.CredentialID, "p-1", "t-1", "corr", 5, time.Minute); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if err := s.RecordAuthSuccess(ctx, cred.CredentialID, "p-1", "t-1", "corr", ""); err != nil {
		t.Fatalf("record success: %v", err)
	}

	_, after, err := s.FindActiveCredentialByEmail(ctx, "idp|alice@zoiko.io", "t-1")
	if err != nil {
		t.Fatalf("re-lookup failed: %v", err)
	}
	if after.FailedAttemptCount != 0 {
		t.Fatalf("expected the counter cleared, got %d", after.FailedAttemptCount)
	}
	if after.LockedUntil != nil {
		t.Fatalf("expected no lock, got %v", after.LockedUntil)
	}
	if after.LastAuthenticatedAt == nil {
		t.Fatalf("a successful authentication must be stamped")
	}
}

// Regression: an expired lock resets the counter to 1, and must ALSO clear
// locked_until. Leaving the stale timestamp made every subsequent failure match
// the "lock expired" branch again, pinning the count at 1 and disabling lockout
// for that credential permanently.
func TestPgStore_RecordAuthFailure_ExpiredLockDoesNotDisableLockout(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	seedCredential(t, ctx, pool, s, "p-1", "t-1", "idp|alice")
	_, cred, err := s.FindActiveCredentialByEmail(ctx, "idp|alice@zoiko.io", "t-1")
	if err != nil {
		t.Fatalf("seed lookup failed: %v", err)
	}

	// Lock it, then force the window into the past.
	const maxAttempts = 2
	for i := 0; i < maxAttempts; i++ {
		if _, _, err := s.RecordAuthFailure(ctx, cred.CredentialID, "p-1", "t-1", "corr", maxAttempts, time.Hour); err != nil {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		UPDATE principal_credentials SET locked_until = NOW() - INTERVAL '1 minute'
		WHERE credential_id = $1
	`, cred.CredentialID); err != nil {
		t.Fatalf("failed to expire the lock: %v", err)
	}

	// First failure after expiry restarts the run at 1 and must not be locked.
	locked, count, err := s.RecordAuthFailure(ctx, cred.CredentialID, "p-1", "t-1", "corr", maxAttempts, time.Hour)
	if err != nil {
		t.Fatalf("post-expiry failure: %v", err)
	}
	if count != 1 || locked {
		t.Fatalf("expected count=1 unlocked after expiry, got count=%d locked=%v", count, locked)
	}

	// The next one must reach the threshold again. If locked_until had been
	// left stale, this would report count=1 unlocked forever.
	locked, count, err = s.RecordAuthFailure(ctx, cred.CredentialID, "p-1", "t-1", "corr", maxAttempts, time.Hour)
	if err != nil {
		t.Fatalf("second post-expiry failure: %v", err)
	}
	if count != 2 || !locked {
		t.Fatalf("lockout must re-engage after an expired window, got count=%d locked=%v", count, locked)
	}
}

// Rotation retires rather than overwrites, so the superseded digest survives
// as evidence, and only one row stays ACTIVE.
func TestPgStore_UpsertPasswordCredential_RetiresPrevious(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	seedCredential(t, ctx, pool, s, "p-1", "t-1", "idp|alice")

	rotated := strings.Replace(testDigest, "aGFzaGhhc2", "bmV3aGFzaG", 1)
	if err := s.UpsertPasswordCredential(ctx, "p-1", "t-1", rotated, "argon2id"); err != nil {
		t.Fatalf("rotation failed: %v", err)
	}

	_, cred, err := s.FindActiveCredentialByEmail(ctx, "idp|alice@zoiko.io", "t-1")
	if err != nil {
		t.Fatalf("lookup after rotation failed: %v", err)
	}
	if cred.SecretHash != rotated {
		t.Fatalf("expected the rotated digest to be live")
	}

	var total, active int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE status = 'ACTIVE')
		FROM principal_credentials WHERE principal_id = 'p-1'
	`).Scan(&total, &active); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if total != 2 {
		t.Fatalf("the superseded digest must be kept, got %d rows", total)
	}
	if active != 1 {
		t.Fatalf("exactly one credential may be ACTIVE, got %d", active)
	}
}

// The one caller error that would be catastrophic: writing a plaintext
// password into secret_hash.
func TestPgStore_UpsertPasswordCredential_RejectsNonDigest(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|alice", "ACTIVE")

	if err := s.UpsertPasswordCredential(ctx, "p-1", "t-1", "Zoiko@Governance1", "argon2id"); err == nil {
		t.Fatal("a plaintext password must never be accepted as a digest")
	}
	if err := s.UpsertPasswordCredential(ctx, "p-1", "t-1", "", "argon2id"); err == nil {
		t.Fatal("an empty digest must be refused")
	}
}

// Every authentication outcome lands in access_decision_log, which is what
// makes "who tried to log in as whom, and was it allowed" answerable.
func TestPgStore_AuthAttempts_AppendToAccessDecisionLog(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	seedCredential(t, ctx, pool, s, "p-1", "t-1", "idp|alice")
	_, cred, err := s.FindActiveCredentialByEmail(ctx, "idp|alice@zoiko.io", "t-1")
	if err != nil {
		t.Fatalf("seed lookup failed: %v", err)
	}

	if _, _, err := s.RecordAuthFailure(ctx, cred.CredentialID, "p-1", "t-1", "corr-fail", 5, time.Minute); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if err := s.RecordAuthDenied(ctx, "p-1", "t-1", "account_locked", "corr-denied"); err != nil {
		t.Fatalf("record denied: %v", err)
	}
	if err := s.RecordAuthSuccess(ctx, cred.CredentialID, "p-1", "t-1", "corr-ok", ""); err != nil {
		t.Fatalf("record success: %v", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT decision_outcome, decision_basis FROM access_decision_log
		WHERE principal_id = 'p-1' AND action_type = 'AUTHENTICATE'
		ORDER BY decision_log_id
	`)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var outcome, basis string
		if err := rows.Scan(&outcome, &basis); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		got = append(got, outcome+"/"+basis)
	}
	if len(got) != 3 {
		t.Fatalf("expected one row per attempt, got %v", got)
	}
	want := []string{"DENIED/password_mismatch", "DENIED/account_locked", "ALLOWED/password_verified"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("row %d: expected %q, got %q", i, w, got[i])
		}
	}
}

// ordinaryRolePool returns a pool connected as a purpose-created role that is
// NOSUPERUSER and NOBYPASSRLS.
//
// This exists because TEST_DATABASE_URL points at the postgres superuser both
// locally and in CI, and a superuser bypasses row security unconditionally -
// FORCE included. An RLS assertion made over that connection proves nothing: it
// never reaches the policy at all. The same trap is recorded in
// docs/architecture/backend-completion-tracker.md as the first lesson of
// Priority 1, and it is why this test asserted a property it could not observe.
func ordinaryRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	const role = "identity_rls_test"
	const password = "identity_rls_test_pw"

	if _, err := admin.Exec(ctx, `DO $do$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+role+`') THEN
			CREATE ROLE `+role+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
		END IF;
	END $do$;`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	for _, stmt := range []string{
		`ALTER ROLE ` + role + ` WITH LOGIN PASSWORD '` + password + `' NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE ON SCHEMA public TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("grant (%s): %v", stmt, err)
		}
	}

	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.User = url.UserPassword(role, password)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// An unscoped connection must read no credential rows at all. This is the
// backstop for the explicit tenant predicate in every query above: if one were
// ever dropped, RLS still returns nothing rather than every tenant's digests.
func TestPgStore_CredentialsAreRLSProtected(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	s := store.New(admin, zap.NewNop())

	seedCredential(t, ctx, admin, s, "p-1", "t-1", "idp|alice")

	// Read back as an ordinary role. Seeding stays on the admin pool: the point
	// under test is the read, and a role that cannot bypass RLS also cannot
	// insert the row to be hidden.
	pool := ordinaryRolePool(t, admin)

	// Negative control. Without it this test would also pass against a role that
	// simply cannot see the table at all, which is a different failure wearing
	// the same result.
	var scoped int
	if err := pool.QueryRow(ctx,
		`SELECT set_config('app.tenant_id', 't-1', false)`).Scan(new(string)); err != nil {
		t.Fatalf("set tenant scope: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM principal_credentials`).Scan(&scoped); err != nil {
		t.Fatalf("scoped count failed: %v", err)
	}
	if scoped != 1 {
		t.Fatalf("the credential's own tenant must see it, got %d - the policy is "+
			"too restrictive, or the role cannot read the table at all", scoped)
	}

	// No app.tenant_id set on this connection.
	unscoped := ordinaryRolePool(t, admin)
	var count int
	if err := unscoped.QueryRow(ctx, `SELECT COUNT(*) FROM principal_credentials`).Scan(&count); err != nil {
		t.Fatalf("unscoped count failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("an unscoped read must return no credential rows, got %d "+
			"(is the test connecting as a superuser or the table owner? RLS is FORCEd, "+
			"but a BYPASSRLS role still sees through it)", count)
	}
}
