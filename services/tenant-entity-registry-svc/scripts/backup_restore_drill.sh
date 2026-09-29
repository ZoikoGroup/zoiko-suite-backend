#!/usr/bin/env bash
#
# Backup / restore drill for tenant-entity-registry-svc (ORG §9.2 gate 7;
# §8 NP48 "Restore master DB to earlier snapshot after financial actions →
# reconcile versions/events; no silent rollback of authoritative history").
#
# Dumps the live database, restores it into a throwaway database, and proves
# the restore is faithful: every table's row count, the row-level security
# posture (FORCE RLS, policies, named capabilities), the constraints that
# carry controls, and the history invariants. Then drops the copy. Reports the
# measured restore time — the local RTO evidence.
#
# It never writes to the source database.
#
# Usage:  scripts/backup_restore_drill.sh
#         PG_CONTAINER=... DB=... scripts/backup_restore_drill.sh

set -uo pipefail

# Git Bash on Windows rewrites /tmp/... arguments into Windows paths; the dump
# path is inside the container, so it must reach docker untouched.
export MSYS_NO_PATHCONV=1

PG_CONTAINER="${PG_CONTAINER:-zoiko-postgres}"
DB="${DB:-tenant_entity_registry}"
COPY="${DB}_restore_drill_$$"
DUMP="/tmp/${COPY}.dump"
PASS=0
FAIL=0

ok()   { if [ "$2" = "0" ]; then printf '  PASS %s\n' "$1"; PASS=$((PASS+1)); else printf '  FAIL %s\n' "$1"; FAIL=$((FAIL+1)); fi; }
psql_on() { docker exec "$PG_CONTAINER" psql -U postgres -d "$1" -tAc "$2" 2>/dev/null; }

cleanup() {
  docker exec "$PG_CONTAINER" psql -U postgres -tAc "DROP DATABASE IF EXISTS \"$COPY\"" >/dev/null 2>&1
  docker exec "$PG_CONTAINER" rm -f "$DUMP" >/dev/null 2>&1
}
trap cleanup EXIT

printf 'Backup/restore drill — %s → %s\n\n' "$DB" "$COPY"

t0=$(date +%s)
docker exec "$PG_CONTAINER" pg_dump -U postgres -Fc -d "$DB" -f "$DUMP" || { echo "pg_dump failed"; exit 1; }
t1=$(date +%s)
docker exec "$PG_CONTAINER" psql -U postgres -tAc "CREATE DATABASE \"$COPY\"" >/dev/null || exit 1
docker exec "$PG_CONTAINER" pg_restore -U postgres -d "$COPY" --no-owner "$DUMP" || { echo "pg_restore reported errors"; FAIL=$((FAIL+1)); }
t2=$(date +%s)
size=$(docker exec "$PG_CONTAINER" stat -c %s "$DUMP")
printf 'dump %ss (%s bytes), restore %ss\n\n' "$((t1-t0))" "$size" "$((t2-t1))"

echo "1. Every table restored with every row"
tables=$(psql_on "$DB" "SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY 1")
for t in $tables; do
  a=$(psql_on "$DB" "SELECT count(*) FROM \"$t\"")
  b=$(psql_on "$COPY" "SELECT count(*) FROM \"$t\"")
  ok "$t: $a rows → $b" "$([ "$a" = "$b" ] && echo 0 || echo 1)"
done

echo "2. Security posture restored"
q_force="SELECT string_agg(relname, ',' ORDER BY relname) FROM pg_class WHERE relkind='r' AND relforcerowsecurity"
ok "FORCE row-level security on the same tables" "$([ "$(psql_on "$DB" "$q_force")" = "$(psql_on "$COPY" "$q_force")" ] && echo 0 || echo 1)"
q_pol="SELECT string_agg(tablename||':'||policyname||':'||coalesce(qual,'')||':'||coalesce(with_check,''), '|' ORDER BY tablename, policyname) FROM pg_policies WHERE schemaname='public'"
ok "every policy, including the named capabilities, identical" "$([ "$(psql_on "$DB" "$q_pol")" = "$(psql_on "$COPY" "$q_pol")" ] && echo 0 || echo 1)"
q_con="SELECT string_agg(conname, ',' ORDER BY conname) FROM pg_constraint WHERE contype='c'"
ok "control-carrying CHECK constraints identical (self-approval, evidence, intervals)" "$([ "$(psql_on "$DB" "$q_con")" = "$(psql_on "$COPY" "$q_con")" ] && echo 0 || echo 1)"

echo "3. History invariants hold on the restored copy"
overlap=$(psql_on "$COPY" "SELECT count(*) FROM (SELECT legal_entity_id FROM legal_entity_profile_versions WHERE effective_to IS NULL GROUP BY 1 HAVING count(DISTINCT effective_from) > 1) x")
ok "no entity has two open profile versions (found ${overlap:-?})" "$([ "$overlap" = "0" ] && echo 0 || echo 1)"
ver=$(psql_on "$COPY" "SELECT count(*) FROM tenants t WHERE t.record_version < 1")
ok "tenant versions intact" "$([ "$ver" = "0" ] && echo 0 || echo 1)"
unpub=$(psql_on "$COPY" "SELECT count(*) FROM event_outbox WHERE published_at IS NULL")
printf '  INFO %s outbox events unpublished in the copy — a restore re-delivers them (at-least-once; consumers dedupe on event_id)\n' "${unpub:-?}"

printf '\nResult: %s passed, %s failed. Restore time %ss.\n' "$PASS" "$FAIL" "$((t2-t1))"
[ "$FAIL" -eq 0 ]
