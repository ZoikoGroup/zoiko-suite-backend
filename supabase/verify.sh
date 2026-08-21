#!/usr/bin/env bash
# Apply every migration in migrations/ to a throwaway Postgres container, in
# filename order, and report the resulting RLS posture.
#
# This is not a substitute for the per-service behavioural tests — it proves the
# SQL applies and that no table ended up with RLS enabled but not forced. Run it
# after adding each service migration.
#
#   ./verify.sh
#
# Requires docker. Uses postgres:16-alpine, matching Supabase's major version
# closely enough for DDL; it stubs the `anon` / `authenticated` / `service_role`
# roles that Supabase provides out of the box.

set -euo pipefail

# Git Bash rewrites container-absolute paths like /tmp/x.sql into Windows paths
# before docker ever sees them. Without this the copies land somewhere else
# entirely and psql reports "No such file or directory" for a file that exists.
export MSYS_NO_PATHCONV=1

CONTAINER=zoiko-sb-verify
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$CONTAINER" \
    -e POSTGRES_PASSWORD=verify -e POSTGRES_DB=zoiko \
    postgres:16-alpine >/dev/null

# The postgres image starts a TEMPORARY server to run its init scripts, then
# shuts it down and starts the real one. pg_isready answers "ready" during that
# first phase, so a single successful probe is not evidence the server will
# still be there a moment later — the next command gets
# "FATAL: the database system is shutting down". Require several consecutive
# successful round-trips instead of one probe.
printf 'waiting for postgres'
streak=0
while [ "$streak" -lt 5 ]; do
    if docker exec "$CONTAINER" psql -U postgres -d zoiko -tAc 'SELECT 1' >/dev/null 2>&1; then
        streak=$((streak + 1))
    else
        streak=0
        printf '.'
    fi
done
echo ' ready'

# Roles Supabase ships with. service_role is created BYPASSRLS deliberately —
# it is what the supabase-js service key authenticates as, and reproducing that
# here is the point: it demonstrates that anything connecting with that key is
# exempt from every policy below.
docker exec -i "$CONTAINER" psql -q -U postgres -d zoiko <<'SQL'
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='anon')          THEN CREATE ROLE anon NOLOGIN; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='authenticated') THEN CREATE ROLE authenticated NOLOGIN; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='service_role')  THEN CREATE ROLE service_role NOLOGIN BYPASSRLS; END IF;
END $$;
SQL

failed=0
for f in "$HERE"/migrations/*.sql; do
    name=$(basename "$f")
    # MSYS_NO_PATHCONV=1 protects the CONTAINER path (/tmp/...) from being
    # rewritten, but it also stops the HOST path being translated out of
    # /c/Users/... form, which docker cannot resolve. Convert the host side
    # explicitly and leave the container side literal — both halves need
    # opposite treatment in the same command.
    docker cp "$(cygpath -w "$f" 2>/dev/null || echo "$f")" "$CONTAINER:/tmp/$name" >/dev/null
    if docker exec "$CONTAINER" psql -v ON_ERROR_STOP=1 -U postgres -d zoiko -q -f "/tmp/$name" >/dev/null 2>/tmp/err; then
        echo "  ok    $name"
    else
        echo "  FAIL  $name"
        docker exec "$CONTAINER" psql -v ON_ERROR_STOP=1 -U postgres -d zoiko -f "/tmp/$name" 2>&1 | tail -5
        failed=1
    fi
done

[ "$failed" -eq 0 ] || { echo; echo "MIGRATIONS FAILED"; exit 1; }

echo
echo "── RLS posture ─────────────────────────────────────────────"
# relrowsecurity without relforcerowsecurity is the estate's recurring defect:
# a policy that is present, correct, and never executed. Any row printed under
# "enabled but NOT forced" is that defect reintroduced.
docker exec "$CONTAINER" psql -U postgres -d zoiko -c "
SELECT n.nspname AS schema, c.relname AS table_name,
       c.relrowsecurity AS enabled, c.relforcerowsecurity AS forced,
       (SELECT count(*) FROM pg_policies p
         WHERE p.schemaname=n.nspname AND p.tablename=c.relname) AS policies
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE c.relkind='r'
  AND n.nspname NOT IN ('pg_catalog','information_schema','app','public')
ORDER BY 1,2;"

echo "── tables enabled but NOT forced (must be empty) ────────────"
docker exec "$CONTAINER" psql -U postgres -d zoiko -tAc "
SELECT n.nspname||'.'||c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE c.relkind='r' AND c.relrowsecurity AND NOT c.relforcerowsecurity
  AND n.nspname NOT IN ('pg_catalog','information_schema');"

echo "── tables with NO row-level security at all (must be empty) ─"
docker exec "$CONTAINER" psql -U postgres -d zoiko -tAc "
SELECT n.nspname||'.'||c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE c.relkind='r' AND NOT c.relrowsecurity
  AND n.nspname NOT IN ('pg_catalog','information_schema','app','public');"

echo
echo "container '$CONTAINER' left running for inspection; docker rm -f $CONTAINER when done"
