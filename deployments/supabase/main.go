// Command supabase-bootstrap provisions the login-path services' schemas on a
// single-database Postgres host such as Supabase.
//
// # WHY THIS EXISTS INSTEAD OF init-db.sh
//
// deployments/init-db.sh issues CREATE DATABASE 63 times, one per service. A
// managed provider gives a project exactly one database, so that script cannot
// run there at all. This tool does the same job with schemas: every service
// gets a schema whose name is the database name it used to have, and its
// migrations are applied with search_path pointing at it. The migration SQL is
// unchanged and unaware — it says CREATE TABLE principals, which lands wherever
// search_path points.
//
// It is written in Go rather than as a psql script because psql is not
// installed on every machine that needs to run this, and the Go toolchain and
// the pgx driver already are.
//
// DIFFERENCES FROM init-db.sh THAT MATTER
//
//   - init-db.sh runs exactly once, on an empty Docker volume, and has no
//     record of what it applied. A hosted database is persistent, so this tool
//     records every applied migration and refuses to apply one twice.
//   - It also records a checksum. A migration file edited after it was applied
//     is reported rather than skipped silently: the database and the repository
//     disagree at that point, and which one is right is not a decision a
//     bootstrap tool should make on its own.
//   - Each file is applied inside its own transaction, so a failure leaves the
//     schema at the last complete migration instead of half-way through one.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// service maps a repository directory to the schema and role it owns.
//
// Only the services the login path actually needs are listed. The other 59 are
// deliberately absent: this is a staged cutover, and a tool that provisions
// everything would have to be trusted all at once.
//
// Schema names are the database names deployments/init-db.sh used, so a
// connection string reads the same either way and nothing has to be renamed if
// the estate ever moves back to a database per service.
type service struct {
	Dir    string // directory under services/
	Schema string // Postgres schema, formerly the database name
	Role   string // login role the service connects as
	Why    string // why the login path needs it
}

var services = []service{
	{
		Dir:    "identity-context-svc",
		Schema: "identity_context",
		Role:   "app_identity_context",
		Why:    "holds principals and their password credentials; mints the identity envelope",
	},
	{
		Dir:    "tenant-entity-registry-svc",
		Schema: "tenant_entity_registry",
		Role:   "app_tenant_entity_registry",
		Why:    "the root of all scope; resolve fails closed without it",
	},
	{
		Dir:    "authorization-svc",
		Schema: "authorization_svc",
		Role:   "app_authorization",
		Why:    "every write on the platform is checked against it",
	},
	{
		Dir:    "governance-decision-log-svc",
		Schema: "governance_decision_log",
		Role:   "app_governance_decision_log",
		Why:    "the console's landing page reads it; an empty panel otherwise",
	},
	{
		Dir:    "access-control-svc",
		Schema: "access_control",
		Role:   "app_access_control",
		Why:    "owns role definitions and their permission bundles; identity-context-svc resolves Dimension 4 against it, and without it a principal holding any role cannot be resolved at all",
	},
}

// bookkeepingSchema holds the migration ledger. Kept out of every service's
// schema so that dropping one service's schema does not erase the record of
// what the others have applied.
const bookkeepingSchema = "zoiko_platform"

func main() {
	var (
		dbURL       = flag.String("url", os.Getenv("SUPABASE_DB_URL"), "Postgres connection URL (default $SUPABASE_DB_URL)")
		appPassword = flag.String("app-password", os.Getenv("APP_DB_PASSWORD"), "password for the per-service login roles (default $APP_DB_PASSWORD)")
		dryRun      = flag.Bool("dry-run", false, "report what would be applied, change nothing")
		servicesDir = flag.String("services", "", "path to the services/ directory (default: resolved relative to this file's usual location)")
		skipRoles   = flag.Bool("skip-roles", false, "apply migrations only; leave roles and grants alone")
		rolesOnly   = flag.Bool("roles-only", false, "provision roles and grants only; apply no migrations")
		list        = flag.Bool("list", false, "list the migrations that would be applied and exit; needs no database")
	)
	flag.Parse()

	// -list runs before any connection is attempted, so migration discovery can
	// be checked before credentials exist. A missing or misnamed migration
	// directory is worth finding here rather than half-way through a run
	// against a real database.
	if *list {
		root, err := resolveServicesDir(*servicesDir)
		if err != nil {
			fail("%v", err)
		}
		if err := listMigrations(root); err != nil {
			fail("%v", err)
		}
		return
	}

	if *dbURL == "" {
		fail("no connection URL: pass -url or set SUPABASE_DB_URL\n\n" +
			"Take it from the Supabase dashboard: Project Settings -> Database ->\n" +
			"Connection string -> Session pooler (port 5432). Use the SESSION pooler\n" +
			"here, not the transaction pooler on 6543: this tool issues DDL and\n" +
			"advisory locks that must stay on one connection for the whole run.")
	}
	if *skipRoles && *rolesOnly {
		fail("-skip-roles and -roles-only ask for opposite halves of the run; pass one or neither")
	}
	if *appPassword == "" && !*skipRoles && !*dryRun {
		fail("no app role password: pass -app-password or set APP_DB_PASSWORD\n" +
			"(or pass -skip-roles to apply migrations only)")
	}

	root, err := resolveServicesDir(*servicesDir)
	if err != nil {
		fail("%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, *dbURL)
	if err != nil {
		fail("connect: %v\n\n%s", err, connectHint(err))
	}
	defer conn.Close(context.Background())

	var serverVersion string
	if err := conn.QueryRow(ctx, "SELECT version()").Scan(&serverVersion); err != nil {
		fail("could not query server version: %v", err)
	}
	var currentUser, currentDB string
	if err := conn.QueryRow(ctx, "SELECT current_user, current_database()").Scan(&currentUser, &currentDB); err != nil {
		fail("could not identify the connection: %v", err)
	}
	fmt.Printf("connected as %q to database %q\n%s\n\n", currentUser, currentDB, truncate(serverVersion, 78))

	if *dryRun {
		fmt.Println("DRY RUN — nothing will be written.")
	}

	if err := run(ctx, conn, root, *appPassword, *dryRun, *skipRoles, *rolesOnly); err != nil {
		fail("%v", err)
	}
}

func run(ctx context.Context, conn *pgx.Conn, root, appPassword string, dryRun, skipRoles, rolesOnly bool) error {
	if !dryRun {
		if err := ensureBookkeeping(ctx, conn); err != nil {
			return err
		}
	}

	// -roles-only exists because the two halves of this run were coupled, and
	// they are independent. A single migration that cannot be applied -- a table
	// created outside this tool, so an unguarded CREATE TABLE collides with it --
	// returned before provisionRoles was ever reached, leaving ZERO roles for
	// FOUR services, three of which had migrated perfectly. The services then
	// could not start at all, for a reason that had nothing to do with them.
	//
	// Grants are applied to the tables that exist now, so on a partially
	// migrated schema the role gets DML on those and ALTER DEFAULT PRIVILEGES
	// covers whatever later migrations add.
	if rolesOnly {
		if dryRun {
			fmt.Println("would provision roles only; no migration would be applied")
			return nil
		}
		return provisionRoles(ctx, conn, appPassword)
	}

	for _, svc := range services {
		fmt.Printf("── %s → schema %q\n   %s\n", svc.Dir, svc.Schema, svc.Why)

		migrations, err := loadMigrations(root, svc)
		if err != nil {
			return err
		}

		if !dryRun {
			// IF NOT EXISTS so a re-run is a no-op rather than an error, and
			// so a partially-completed earlier run can be resumed.
			if _, err := conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoteIdent(svc.Schema)); err != nil {
				return fmt.Errorf("create schema %s: %w", svc.Schema, err)
			}
		}

		applied, skipped := 0, 0
		for _, m := range migrations {
			state, err := migrationState(ctx, conn, svc.Schema, m, dryRun)
			if err != nil {
				return err
			}
			switch state {
			case stateApplied:
				skipped++
				continue
			case stateChanged:
				return fmt.Errorf(
					"%s/%s was already applied but its contents have changed.\n"+
						"The database and the repository disagree about what this migration says.\n"+
						"Resolve it deliberately — write a NEW migration for the difference, or\n"+
						"drop and rebuild the %q schema if it holds nothing you need",
					svc.Dir, m.name, svc.Schema)
			}

			fmt.Printf("     %s\n", m.name)
			if dryRun {
				applied++
				continue
			}
			if err := applyMigration(ctx, conn, svc, m); err != nil {
				return err
			}
			applied++
		}

		switch {
		case applied == 0 && skipped > 0:
			fmt.Printf("   up to date (%d migrations already applied)\n\n", skipped)
		case skipped > 0:
			fmt.Printf("   applied %d, skipped %d already-applied\n\n", applied, skipped)
		default:
			fmt.Printf("   applied %d migrations\n\n", applied)
		}
	}

	if skipRoles {
		fmt.Println("roles skipped (-skip-roles)")
		return nil
	}
	if dryRun {
		fmt.Println("would provision per-schema login roles:")
		for _, svc := range services {
			fmt.Printf("     %-28s → USAGE on %s only\n", svc.Role, svc.Schema)
		}
		return nil
	}
	return provisionRoles(ctx, conn, appPassword)
}

// ── Migrations ───────────────────────────────────────────────────────────────

type migration struct {
	name     string
	path     string
	sql      string
	checksum string
}

func loadMigrations(root string, svc service) ([]migration, error) {
	dir := filepath.Join(root, svc.Dir, "deployments", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations for %s: %w", svc.Dir, err)
	}

	var out []migration
	for _, e := range entries {
		name := e.Name()
		// Only *.up.sql. The .down.sql files are the manual rollback path and
		// applying one here would undo the schema this tool just built.
		if e.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		sum := sha256.Sum256(raw)
		out = append(out, migration{
			name:     name,
			path:     path,
			sql:      string(raw),
			checksum: hex.EncodeToString(sum[:]),
		})
	}
	if len(out) == 0 {
		// The same failure init-db.sh calls FATAL: a service whose schema was
		// never created comes up and answers 500 to every read, which reads as
		// a service bug rather than a missing migration.
		return nil, fmt.Errorf("no *.up.sql under %s — %s would have no schema", dir, svc.Dir)
	}

	// Lexical order over the 000001_, 000002_ prefixes is the migration order.
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// listMigrations prints what would be applied, in order, without connecting.
func listMigrations(root string) error {
	total := 0
	for _, svc := range services {
		migrations, err := loadMigrations(root, svc)
		if err != nil {
			return err
		}
		fmt.Printf("── %s → schema %q → role %q\n", svc.Dir, svc.Schema, svc.Role)
		for _, m := range migrations {
			fmt.Printf("     %s  (%s)\n", m.name, m.checksum[:12])
		}
		fmt.Println()
		total += len(migrations)
	}
	fmt.Printf("%d migrations across %d services.\n", total, len(services))
	return nil
}

type state int

const (
	stateNew state = iota
	stateApplied
	stateChanged
)

func migrationState(ctx context.Context, conn *pgx.Conn, schema string, m migration, dryRun bool) (state, error) {
	if dryRun {
		// The ledger may not exist yet on a first dry run.
		var exists bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			                WHERE table_schema = $1 AND table_name = 'schema_migrations')`,
			bookkeepingSchema).Scan(&exists); err != nil || !exists {
			return stateNew, nil
		}
	}

	var recorded string
	err := conn.QueryRow(ctx,
		`SELECT checksum FROM `+quoteIdent(bookkeepingSchema)+`.schema_migrations
		 WHERE schema_name = $1 AND filename = $2`,
		schema, m.name).Scan(&recorded)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return stateNew, nil
	case err != nil:
		return stateNew, fmt.Errorf("read migration ledger: %w", err)
	case recorded != m.checksum:
		return stateChanged, nil
	default:
		return stateApplied, nil
	}
}

// applyMigration runs one file and records it, in a single transaction.
//
// SET LOCAL search_path is what puts the migration's unqualified CREATE TABLE
// into this service's schema. LOCAL scopes it to the transaction, so it cannot
// leak into the next migration if this one fails part-way.
//
// pg_catalog is appended so a migration can still reach built-ins like
// gen_random_uuid() (authorization-svc's tables default their primary keys to
// it). public is deliberately NOT in the path: nothing in these migrations
// belongs there, and leaving it out means a table that would have landed in
// public fails loudly instead of quietly becoming visible to Supabase's Data
// API.
func applyMigration(ctx context.Context, conn *pgx.Conn, svc service, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+quoteIdent(svc.Schema)+", pg_catalog"); err != nil {
		return fmt.Errorf("set search_path for %s: %w", m.name, err)
	}
	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("%s/%s: %w", svc.Dir, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO `+quoteIdent(bookkeepingSchema)+`.schema_migrations
		     (schema_name, filename, checksum, applied_at)
		 VALUES ($1, $2, $3, NOW())`,
		svc.Schema, m.name, m.checksum); err != nil {
		return fmt.Errorf("record %s: %w", m.name, err)
	}
	return tx.Commit(ctx)
}

func ensureBookkeeping(ctx context.Context, conn *pgx.Conn) error {
	stmts := []string{
		"CREATE SCHEMA IF NOT EXISTS " + quoteIdent(bookkeepingSchema),
		`CREATE TABLE IF NOT EXISTS ` + quoteIdent(bookkeepingSchema) + `.schema_migrations (
			schema_name  TEXT NOT NULL,
			filename     TEXT NOT NULL,
			checksum     TEXT NOT NULL,
			applied_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (schema_name, filename)
		)`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			return fmt.Errorf("create migration ledger: %w", err)
		}
	}
	return nil
}

// ── Roles ────────────────────────────────────────────────────────────────────

// provisionRoles creates one login role per schema.
//
// This is deployments/scripts/create-app-roles.sh translated from databases to
// schemas, and it exists for the same reason that script does: the role the
// migrations run as OWNS these tables, and a table owner is exempt from row
// security unless the table sets FORCE. Most services in this estate set
// ENABLE without FORCE, so connecting the application as the owner would leave
// every tenant_isolation_policy inert — the policies would read as isolation
// without being it.
//
// On a managed host the owner is the provider's superuser-equivalent role, so
// the risk is the same shape as it was with a local postgres superuser.
//
// The isolation boundary moves from CONNECT-on-database to USAGE-on-schema.
// A role is granted USAGE on its own schema and nothing else, so a compromised
// service reaches its own tables and no others.
func provisionRoles(ctx context.Context, conn *pgx.Conn, password string) error {
	var runner string
	if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&runner); err != nil {
		return fmt.Errorf("identify migration runner: %w", err)
	}

	fmt.Println("── login roles")
	for _, svc := range services {
		// The role is a cluster-wide object; the grants below are per-schema.
		// Written as a DO block so a re-run resets the password and re-applies
		// grants rather than failing on the existing role.
		if _, err := conn.Exec(ctx, `
			DO $$
			BEGIN
			    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+svc.Role+`') THEN
			        CREATE ROLE `+quoteIdent(svc.Role)+` LOGIN PASSWORD `+quoteLiteral(password)+`
			            NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
			    ELSE
			        ALTER ROLE `+quoteIdent(svc.Role)+` LOGIN PASSWORD `+quoteLiteral(password)+`
			            NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
			    END IF;
			END
			$$;`); err != nil {
			return fmt.Errorf("create role %s: %w", svc.Role, err)
		}

		grants := []string{
			// Start from nothing, so a re-run after this list shrinks does not
			// leave yesterday's wider grant in place.
			"REVOKE ALL ON SCHEMA " + quoteIdent(svc.Schema) + " FROM " + quoteIdent(svc.Role),
			"GRANT USAGE ON SCHEMA " + quoteIdent(svc.Schema) + " TO " + quoteIdent(svc.Role),

			// DML only. No TRUNCATE — it bypasses row security entirely and no
			// service needs it — and no DDL: an application role must not be
			// able to ALTER or DROP the tables whose policies constrain it.
			"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA " + quoteIdent(svc.Schema) + " TO " + quoteIdent(svc.Role),
			"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA " + quoteIdent(svc.Schema) + " TO " + quoteIdent(svc.Role),

			// ...and on whatever later migrations add. Without this, the next
			// migration's table is invisible to the service and the failure
			// looks like a missing row rather than a missing grant.
			"ALTER DEFAULT PRIVILEGES FOR ROLE " + quoteIdent(runner) + " IN SCHEMA " + quoteIdent(svc.Schema) +
				" GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO " + quoteIdent(svc.Role),
			"ALTER DEFAULT PRIVILEGES FOR ROLE " + quoteIdent(runner) + " IN SCHEMA " + quoteIdent(svc.Schema) +
				" GRANT USAGE, SELECT ON SEQUENCES TO " + quoteIdent(svc.Role),

			// Nothing of this platform's belongs in public, and an application
			// role that can create there could put a table in front of a
			// pg_catalog name for any session whose search_path includes it.
			"REVOKE ALL ON SCHEMA public FROM " + quoteIdent(svc.Role),
		}
		for _, g := range grants {
			if _, err := conn.Exec(ctx, g); err != nil {
				return fmt.Errorf("grant for %s: %w\n  statement: %s", svc.Role, err, g)
			}
		}
		fmt.Printf("     %-28s USAGE on %s only\n", svc.Role, svc.Schema)
	}

	fmt.Println()
	fmt.Println("Roles are not in use until each service's DB_USER names one.")
	fmt.Println("Through a Supabase pooler the username is ROLE.PROJECT_REF —")
	fmt.Println("for example app_identity_context.abcdefghijklmnop — not the bare role.")
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// resolveServicesDir finds services/ relative to the working directory, so the
// tool works when run from the repository root or from its own directory.
func resolveServicesDir(override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("-services %q: %w", override, err)
		}
		return override, nil
	}
	candidates := []string{
		filepath.Join("..", "..", "services"), // run from deployments/supabase
		filepath.Join("..", "services"),       // run from deployments
		"services",                            // run from the backend root
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "identity-context-svc")); err == nil && st.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("could not find the services/ directory — pass -services")
}

// quoteIdent renders s as a quoted SQL identifier.
//
// Every identifier here comes from the services table above rather than from
// input, so this is belt-and-braces; it is applied anyway because an
// unquoted identifier assembled by concatenation is the shape of the bug even
// when this instance of it is safe.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral renders s as a SQL string literal. Used for the role password,
// which DOES come from the environment and may contain a quote.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// connectHint turns the three connection failures that actually happen into
// the check that resolves each, rather than leaving a driver error to be
// searched for.
func connectHint(err error) string {
	msg := err.Error()
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "28P01" {
		// Reaching this code is itself good news: TLS completed and SASL
		// negotiated, so the host, port, TLS and username format are all fine
		// and only the password is wrong. Say so, because the two causes below
		// look identical and one of them is easy to chase for a long time.
		//
		// Note that Supavisor reports the role WITHOUT the project suffix
		// ("user \"postgres\"") whether or not the suffix was sent, so the
		// error text cannot be used to tell the two apart.
		return "TLS and SASL succeeded — only the password was rejected. Either:\n" +
			"  1. the password is wrong, or still a [YOUR-PASSWORD] placeholder; or\n" +
			"  2. the username is missing its project suffix. Through a Supabase\n" +
			"     pooler it must be ROLE.PROJECT_REF, never the bare role."
	}
	switch {
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "server misbehaving"):
		return "Host not resolvable. Copy the string from Project Settings -> Database;\n" +
			"the direct db.PROJECT_REF.supabase.co host is IPv6-only on newer projects,\n" +
			"so prefer the pooler host if your network has no IPv6 route."
	case strings.Contains(msg, "SSL"), strings.Contains(msg, "TLS"):
		return "Supabase refuses plaintext connections. The URL needs sslmode=require."
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "i/o timeout"):
		return "Timed out before the handshake. This is usually IPv6: try the pooler host\n" +
			"(aws-N-REGION.pooler.supabase.com), which is reachable over IPv4."
	}
	return "Check the URL against Project Settings -> Database -> Connection string."
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\nsupabase-bootstrap: "+format+"\n", args...)
	os.Exit(1)
}
