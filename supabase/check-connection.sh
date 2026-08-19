#!/usr/bin/env bash
# Read the SUPABASE_DB_* values out of ../.env and prove they work.
#
#   ./supabase/check-connection.sh
#
# Answers, in order:
#   1. do the credentials connect at all
#   2. is the role the RIGHT one — not a superuser, not BYPASSRLS
#   3. is the schema actually there — 21 schemas, 42 tables
#   4. is row-level security forced on every table
#
# Uses psql inside a throwaway container, so nothing needs installing.

set -uo pipefail
export MSYS_NO_PATHCONV=1

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$HERE/../.env"

[ -f "$ENV_FILE" ] || { echo "no .env at $ENV_FILE"; exit 1; }

# Read only the keys we need, ignoring comments. Not `source`: the file is
# meant to be filled in by hand and a stray space or quote should not execute.
val() { grep -E "^$1=" "$ENV_FILE" | head -1 | cut -d= -f2- | tr -d '"'"'"' \r'; }

HOST=$(val SUPABASE_DB_HOST)
PORT=$(val SUPABASE_DB_PORT)
NAME=$(val SUPABASE_DB_NAME)
USER=$(val SUPABASE_DB_USER)
PASS=$(val SUPABASE_DB_PASSWORD)
SSL=$(val SUPABASE_DB_SSLMODE)

# Local var name and env key name differ for two of these, so the pairs are
# spelled out — otherwise the "not filled in" message names keys that do not
# exist in the file and sends you looking for them.
missing=""
for pair in "HOST:HOST" "PORT:PORT" "NAME:NAME" "USER:USER" "PASS:PASSWORD" "SSL:SSLMODE"; do
    var="${pair%%:*}"; key="${pair##*:}"
    [ -n "${!var}" ] || missing="$missing SUPABASE_DB_$key"
done
if [ -n "$missing" ]; then
    echo "Not filled in yet:$missing"
    echo "See the Supabase section of .env — six keys, four of them fixed values."
    exit 1
fi

# Password goes via PGPASSWORD, never into the URL: a password with @ or / in
# it silently corrupts a DSN and the failure looks like a wrong host.
DSN="postgresql://${USER}@${HOST}:${PORT}/${NAME}?sslmode=${SSL}"
echo "connecting to ${HOST}:${PORT}/${NAME} as ${USER}"
echo

run() { docker run --rm -e PGPASSWORD="$PASS" postgres:16-alpine \
        psql "$DSN" -tAc "$1" 2>&1; }

# ── 1. connectivity ──────────────────────────────────────────────────────────
out=$(run "SELECT 1")
if [ "$out" != "1" ]; then
    echo "FAILED to connect:"
    echo "$out" | sed 's/^/    /'
    echo
    case "$out" in
        *"Tenant or user not found"*)
            echo "  → On the pooler the username needs the project-ref suffix:"
            echo "    zoiko_backend.<your-project-ref>, not plain zoiko_backend." ;;
        *"password authentication failed"*)
            echo "  → Did you run: ALTER ROLE zoiko_backend WITH PASSWORD '...' ?"
            echo "    The password from project creation belongs to postgres, not to this role." ;;
        *"timeout"*|*"could not connect"*|*"Network is unreachable"*)
            echo "  → db.<ref>.supabase.co is IPv6-only without the paid IPv4 add-on."
            echo "    Use the Session pooler host instead: aws-0-<region>.pooler.supabase.com" ;;
        *"does not exist"*)
            echo "  → Run supabase/zoiko-suite-all.sql first; it creates the zoiko_backend role." ;;
    esac
    exit 1
fi
echo "  [ok] connected"

# ── 2. the role is the right kind ────────────────────────────────────────────
# This is the entire point of the migration. A superuser or a BYPASSRLS role
# ignores every policy in the schema, which is exactly the compose situation.
flags=$(run "SELECT rolsuper::text||' '||rolbypassrls::text FROM pg_roles WHERE rolname=current_user")
if [ "$flags" = "false false" ]; then
    echo "  [ok] role is NOSUPERUSER and NOBYPASSRLS — policies will apply"
else
    echo "  [!!] role has elevated flags (super bypassrls = $flags)"
    echo "       RLS will NOT apply. Are you connected as postgres instead of zoiko_backend?"
    exit 1
fi

# ── 3. the schema is there ───────────────────────────────────────────────────
# Scope every count to OUR schemas by name. Excluding a denylist of system
# schemas is not good enough on a real Supabase project: auth, storage,
# realtime, vault, extensions, graphql and supabase_migrations all carry tables
# of their own, and counting them reported 76 tables where 42 were expected.
# A check that miscounts is worse than no check — it reports a problem that
# isn't there and trains you to ignore it.
OURS="'app','jurisdiction_rules','delegated_authority','accounts_payable','purchase_request','bank_reconciliation','notification','schema_registry','governance_decision_log','configuration_feature_flag','purchase_order','spend_controls','vendor_due_diligence','evidence_requirements','general_ledger','financial_close','board_resolutions','obligations','document_vault','secret_vault_integration','policy'"

schemas=$(run "SELECT count(*) FROM information_schema.schemata WHERE schema_name IN ($OURS)")
# `app` holds helper functions and no tables, so it is in the schema list but
# contributes nothing here.
tables=$(run "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND n.nspname IN ($OURS)")

echo "  [$([ "$schemas" = 21 ] && echo ok || echo '!!')] schemas: $schemas/21"
echo "  [$([ "$tables" = 42 ] && echo ok || echo '!!')] tables:  $tables/42"
if [ "$tables" != 42 ]; then
    echo "       Run supabase/zoiko-suite-all.sql in the SQL editor."
    exit 1
fi

# ── 4. RLS is forced, not merely enabled ─────────────────────────────────────
# ENABLE without FORCE is the estate's recurring defect: a policy that reads as
# a control and never executes, because the owner is exempt.
# Scoped to our schemas for the same reason as above — Supabase's own tables
# make their own choices about row security and are not ours to judge.
unforced=$(run "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND c.relrowsecurity AND NOT c.relforcerowsecurity AND n.nspname IN ($OURS)")
norls=$(run "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND NOT c.relrowsecurity AND n.nspname IN ($OURS)")

echo "  [$([ "$unforced" = 0 ] && echo ok || echo '!!')] tables ENABLE-but-not-FORCE: $unforced (must be 0)"
echo "  [$([ "$norls" = 0 ] && echo ok || echo '!!')] tables with no RLS at all:   $norls (must be 0)"

echo
if [ "$unforced" = 0 ] && [ "$norls" = 0 ] && [ "$tables" = 42 ]; then
    echo "READY — credentials work and the schema enforces its own rules."
    echo
    echo "Still to do before a service can use this:"
    echo "  * docker-compose.yml hardcodes DB_HOST/DB_NAME per service and does"
    echo "    not read .env for them — needs the \${VAR:-default} pass."
    echo "  * config.DBConfig.DSN() builds no search_path, and the stores use"
    echo "    unqualified table names, so a service would connect and see nothing."
else
    echo "Schema is present but not in the expected posture — see the [!!] lines."
    exit 1
fi
