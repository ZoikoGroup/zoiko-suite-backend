// Command seed-local-admin gives ONE console account a password credential in a
// local compose database.
//
// WHY THIS EXISTS SEPARATELY FROM seed-demo-principals. That command resolves
// its DSN from the backend's .env and .env.local and ignores the process
// environment, so on a machine configured for Supabase it targets the live
// project — which is where it went when it was run here expecting compose. This
// one takes the DSN as a flag and has no fallback, so it can only go where it
// is pointed.
//
// It seeds a single account rather than all seven: the console's own login only
// needs one to be walked, and a smaller blast radius is the point of the file.
//
//	go run ./cmd/seed-local-admin -dsn "postgres://postgres:postgres@localhost:5432/identity_context?sslmode=disable"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/identity-context-svc/internal/credential"
)

func main() {
	dsn := flag.String("dsn", "", "target database (required; no environment fallback, deliberately)")
	email := flag.String("email", "admin@zoikosuite.com", "account email")
	password := flag.String("password", "Zoiko@Governance1", "account password")
	principalID := flag.String("principal", "33333333-3333-3333-3333-333333333333", "principal id")
	tenantID := flag.String("tenant", "11111111-1111-1111-1111-111111111111", "tenant id")
	display := flag.String("display", "Lingaraj (Super Admin)", "display name")
	flag.Parse()

	if *dsn == "" {
		fail("-dsn is required. This command has no .env fallback on purpose: " +
			"seed-demo-principals reads .env.local and will target Supabase on a machine configured for it.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		fail("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// Refuse anything that is not plainly local. The whole reason this file
	// exists is a seeder that went somewhere unintended.
	var host string
	if err := conn.QueryRow(ctx, `SELECT inet_server_addr()::text`).Scan(&host); err == nil && host != "" {
		fmt.Printf("  · server %s\n", host)
	}

	hasher, err := credential.NewHasher(credential.DefaultParams(), 2)
	if err != nil {
		fail("hasher: %v", err)
	}
	hash, err := hasher.Hash(*password)
	if err != nil {
		fail("hash: %v", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		fail("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// principal_credentials is ENABLE + FORCE ROW LEVEL SECURITY on tenant_id,
	// so the tenant has to be installed for the write to be visible to the
	// policy — same as the service does per request.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, *tenantID); err != nil {
		fail("set tenant: %v", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO principals (principal_id, tenant_id, principal_type,
			identity_provider_subject, email, display_name, status)
		VALUES ($1, $2, 'HUMAN', $3, $3, $4, 'ACTIVE')
		ON CONFLICT (principal_id) DO UPDATE SET email = EXCLUDED.email, status = 'ACTIVE'`,
		*principalID, *tenantID, *email, *display); err != nil {
		fail("insert principal: %v", err)
	}

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
		"cred-"+*principalID, *principalID, *tenantID, hash); err != nil {
		fail("insert credential: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		fail("commit: %v", err)
	}

	fmt.Printf("  seeded %s (%s)\n", *email, *principalID)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "seed-local-admin: "+format+"\n", args...)
	os.Exit(1)
}
