// Command seed-demo-principals gives the console's seven demo accounts a real
// principal and a real password credential in identity-context-svc.
//
// WHY THIS EXISTS. zoiko-suite-fe/lib/auth.ts opens with "Mock authentication
// for the ZoikoSuite admin console demo. There is no backend/identity provider
// yet" and carries seven email/password pairs in the frontend bundle. This
// service is that identity provider, and migration 000004 added the credential
// table it needs -- but nothing ever populated it, so POST /v1/authenticate had
// no row to match and the console kept validating passwords in the browser.
//
// The accounts below are copied from GOVERNED_USER_ACCOUNTS verbatim, so the
// same credential works against the mock login and against this service. When
// the console is switched to call /v1/authenticate for real, nothing about the
// login changes for the user.
//
// WHY A SEEDER RATHER THAN AN API CALL. This service exposes no endpoint that
// creates a principal -- the routes are authenticate, context/resolve, the two
// session operations, and three principal reads. A principal is provisioned
// material, not something a caller mints, and the console's identities have
// FIXED ids that other services already hold grants against. Same reasoning as
// deployments/scripts/seed-demo-registry.sql for the tenant fixture.
//
// Idempotent. Re-running leaves principals alone and RE-HASHES the passwords,
// so it doubles as "reset the demo credentials" when one has drifted or a
// lockout needs clearing.
//
//	go run ./cmd/seed-demo-principals            # apply
//	go run ./cmd/seed-demo-principals -dry-run   # report, change nothing
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/identity-context-svc/internal/credential"
)

// schema is this service's schema on the managed single-database host. Under
// the docker/local provider each service owns a whole database and this is the
// database name instead; both resolve the same way through search_path.
const schema = "identity_context"

// demoTenant is the console's DEMO_IDENTITY tenant. Every account below belongs
// to it, and principal_credentials is RLS-FORCEd on exactly this column.
const demoTenant = "11111111-1111-1111-1111-111111111111"

// account is one row of the console's GOVERNED_USER_ACCOUNTS.
type account struct {
	PrincipalID string
	Email       string
	Password    string
	DisplayName string
}

// accounts mirrors zoiko-suite-fe/lib/auth.ts GOVERNED_USER_ACCOUNTS. Kept as a
// literal rather than parsed out of the TypeScript: a seeder that silently
// picks up whatever the frontend happens to contain would make a credential
// change in a UI file a change to what the identity provider accepts.
var accounts = []account{
	{"33333333-3333-3333-3333-333333333333", "admin@zoikosuite.com", "Zoiko@Governance1", "Lingaraj (Super Admin)"},
	{"44444444-4444-4444-4444-444444444444", "tax.officer@zoikosuite.com", "Zoiko@Tax2026!", "Dr. Alistair Vance"},
	{"55555555-5555-5555-5555-555555555555", "cfo@zoikosuite.com", "Zoiko@Finance2026!", "Elena Rostova"},
	{"66666666-6666-6666-6666-666666666666", "legal.counsel@zoikosuite.com", "Zoiko@Legal2026!", "James Okafor, Esq."},
	{"77777777-7777-7777-7777-777777777777", "hr.director@zoikosuite.com", "Zoiko@People2026!", "Sophie Laurent"},
	{"88888888-8888-8888-8888-888888888888", "procurement@zoikosuite.com", "Zoiko@Commercial2026!", "Marcus Sterling"},
	{"99999999-9999-9999-9999-999999999999", "security.audit@zoikosuite.com", "Zoiko@Audit2026!", "Dr. Maya Lin"},
}

func main() {
	dryRun := flag.Bool("dry-run", false, "report what would be applied, change nothing")
	flag.Parse()

	env := loadEnv()

	dsn := env["SUPABASE_DB_URL"]
	if dsn == "" {
		dsn = env["DATABASE_URL"]
	}
	if dsn == "" {
		fail("neither SUPABASE_DB_URL nor DATABASE_URL is set in .env or .env.local")
	}
	dsn = withParam(dsn, "search_path", schema)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	fmt.Printf("  · %s (schema %s)\n", hostOf(dsn), schema)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fail("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var hasTable bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		                WHERE table_schema = $1 AND table_name = 'principal_credentials')`,
		schema).Scan(&hasTable); err != nil {
		fail("probe schema: %v", err)
	}
	if !hasTable {
		fail("schema %q has no principal_credentials table — migration 000004 has not been applied", schema)
	}
	fmt.Println("  ✓ schema present")

	// principal_credentials is ENABLE + FORCE ROW LEVEL SECURITY, so the policy
	// applies to the table owner too -- unlike every table in
	// tenant-entity-registry-svc, where only ENABLE is set and the owner
	// bypasses it. Without this the INSERT fails its WITH CHECK and the SELECT
	// below returns nothing, both silently.
	if _, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, demoTenant); err != nil {
		fail("set app.tenant_id: %v", err)
	}

	// Cost factors come from the same defaults the running service uses, so a
	// digest written here verifies there without a rehash on first login.
	hasher, err := credential.NewHasher(credential.DefaultParams(), 4)
	if err != nil {
		fail("build hasher: %v", err)
	}

	if *dryRun {
		fmt.Printf("\n  dry run — would seed %d principals into %s\n", len(accounts), schema)
		for _, a := range accounts {
			fmt.Printf("    %-32s %s\n", a.Email, a.PrincipalID)
		}
		return
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		fail("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, a := range accounts {
		hash, err := hasher.Hash(a.Password)
		if err != nil {
			fail("hash %s: %v", a.Email, err)
		}

		// identity_provider_subject is NOT NULL and unique per tenant. There is
		// no external IdP here, so the email doubles as the subject -- which is
		// what a local password provider would assert anyway.
		if _, err := tx.Exec(ctx, `
			INSERT INTO principals (principal_id, tenant_id, principal_type,
				identity_provider_subject, email, display_name, status)
			VALUES ($1, $2, 'HUMAN', $3, $3, $4, 'ACTIVE')
			ON CONFLICT (principal_id) DO NOTHING`,
			a.PrincipalID, demoTenant, a.Email, a.DisplayName); err != nil {
			fail("insert principal %s: %v", a.Email, err)
		}

		// A deterministic credential_id keeps re-runs idempotent without needing
		// to read the existing row first. DO UPDATE rather than DO NOTHING so a
		// re-run resets the password and clears a lockout, which is the whole
		// reason to run this twice.
		if _, err := tx.Exec(ctx, `
			INSERT INTO principal_credentials (credential_id, principal_id, tenant_id,
				credential_type, secret_hash, algorithm, status,
				failed_attempt_count, locked_until, secret_updated_at)
			VALUES ($1, $2, $3, 'PASSWORD', $4, 'argon2id', 'ACTIVE', 0, NULL, NOW())
			ON CONFLICT (credential_id) DO UPDATE SET
				secret_hash          = EXCLUDED.secret_hash,
				status               = 'ACTIVE',
				failed_attempt_count = 0,
				locked_until         = NULL,
				secret_updated_at    = NOW(),
				updated_at           = NOW()`,
			"cred-"+a.PrincipalID, a.PrincipalID, demoTenant, hash); err != nil {
			fail("insert credential %s: %v", a.Email, err)
		}

		fmt.Printf("  ✓ %-32s %s\n", a.Email, a.PrincipalID)
	}

	if err := tx.Commit(ctx); err != nil {
		fail("commit: %v", err)
	}

	// Prove it rather than trusting the inserts: this is the join the
	// authentication path performs.
	var n int
	if err := conn.QueryRow(ctx, `
		SELECT COUNT(*) FROM principals p
		JOIN principal_credentials c ON c.principal_id = p.principal_id
		WHERE p.tenant_id = $1 AND p.status = 'ACTIVE' AND c.status = 'ACTIVE'`,
		demoTenant).Scan(&n); err != nil {
		fail("verify: %v", err)
	}

	fmt.Printf("\nSEEDED  %d principals with an ACTIVE password credential\n", n)
}

// loadEnv reads the backend's .env then .env.local, later files winning --
// servicectl's own precedence (tools/servicectl/env.go DefaultEnvFiles), so this
// command and the service it seeds for cannot disagree about which database
// they mean. An empty value never overrides a real one set earlier.
func loadEnv() map[string]string {
	_, self, _, _ := runtime.Caller(0)
	// .../services/identity-context-svc/cmd/seed-demo-principals/main.go
	backendRoot := filepath.Clean(filepath.Join(filepath.Dir(self), "..", "..", "..", ".."))

	out := map[string]string{}
	for _, name := range []string{".env", ".env.local"} {
		f, err := os.Open(filepath.Join(backendRoot, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if v == "" {
				continue
			}
			out[strings.TrimSpace(k)] = v
		}
		_ = f.Close()
		fmt.Printf("  · read %s\n", name)
	}
	return out
}

func withParam(dsn, key, val string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + key + "=" + val
}

// hostOf renders just the host, so no credential reaches stdout.
func hostOf(dsn string) string {
	if i := strings.LastIndex(dsn, "@"); i >= 0 {
		rest := dsn[i+1:]
		if j := strings.IndexAny(rest, "/?"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return "the configured host"
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\nFAILED: "+format+"\n", a...)
	os.Exit(1)
}
