#!/bin/sh
# apply-migrations.sh — runs on the postgres container's first boot.
#
# Applies every *.up.sql under /migrations in lexical order, the same rule
# deployments/init-db.sh and deployments/supabase use. Mounting the directory
# instead of listing files one by one means a new migration can never be
# silently left out of the local stack again (this compose file used to list
# only 000001-000006, so a local GL had no chart of accounts, posting engine
# or outbox while every test still passed).
#
# Two files share the 000006_ prefix (acc03_journal_inputs, add_trial_balance).
# Both are applied; lexical order puts acc03 first, which is the order the
# store tests already run them in. They must NOT be renumbered: the shared
# runners record applied migrations by filename, so a rename would re-run an
# already-applied migration on every existing database.
set -eu

found=0
for f in $(LC_ALL=C ls /migrations/*.up.sql 2>/dev/null | LC_ALL=C sort); do
    found=1
    echo "applying $(basename "$f")"
    psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" -f "$f"
done

if [ "$found" -eq 0 ]; then
    echo "FATAL: no *.up.sql under /migrations -- general_ledger would have no schema" >&2
    exit 1
fi
