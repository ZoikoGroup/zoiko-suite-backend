#!/bin/bash
# First-boot init for the migration-integrity-svc <-> general-ledger-svc
# wiring end-to-end proof. Minimal, standalone slice: THIS creates only the
# databases this proof needs (migration_integrity, general_ledger,
# authorization_svc, financial_control, tenant_entity_registry), not the
# whole platform's ~90-database schema that deployments/init-db.sh
# provisions. tenant_entity_registry backs authorization-svc's tenant-from-
# legal-entity resolution (AUTHZ_ENTITY_REGISTRY_URL) -- without it,
# authorization-svc's checks for financial-control-svc/general-ledger-svc
# calls that omit tenant_id need a manual SQL seed instead. Mirrors that
# script's two rules:
# apply every *.up.sql under each mounted /migrations/<dir> in lexical
# order, and fail loudly if a directory is unmounted or empty rather than
# silently leaving a service with no schema.
set -euo pipefail

create_database() {
    psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres -c "CREATE DATABASE $1"
}

apply_migrations() {
    local db="$1" dir="$2" path="/migrations/$2" applied=0 f
    if [ ! -d "$path" ]; then
        echo "FATAL: $path not mounted -- $db would have no schema" >&2
        exit 1
    fi
    for f in $(LC_ALL=C ls "$path"/*.up.sql 2>/dev/null | LC_ALL=C sort); do
        echo "    $db <- $(basename "$f")"
        psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$db" -f "$f"
        applied=$((applied + 1))
    done
    if [ "$applied" -eq 0 ]; then
        echo "FATAL: $path mounted but holds no *.up.sql -- $db would have no schema" >&2
        exit 1
    fi
}

echo "Creating databases..."
create_database migration_integrity
create_database general_ledger
create_database authorization_svc
create_database financial_control
create_database tenant_entity_registry

echo "Applying migrations..."
apply_migrations migration_integrity migration-integrity
apply_migrations general_ledger general-ledger
apply_migrations authorization_svc authorization
apply_migrations financial_control financial-control
apply_migrations tenant_entity_registry tenant-entity-registry

# ── Application roles (NOSUPERUSER NOBYPASSRLS), general-ledger-svc and
# authorization-svc only, matching deployments/scripts/create-app-roles.sh's
# role names exactly (app_general_ledger, app_authorization) so this slice's
# RLS enforcement matches the full stack's. migration-integrity-svc keeps
# connecting as the superuser for this proof -- its RLS policies are
# exercised by its own store integration tests, not by this script.
APP_DB_PASSWORD="${APP_DB_PASSWORD:-postgres}"
for entry in general_ledger:app_general_ledger authorization_svc:app_authorization financial_control:app_financial_control; do
    db="${entry%%:*}"; role="${entry##*:}"
    psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<-EOSQL
        DO \$\$
        BEGIN
            IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '$role') THEN
                CREATE ROLE $role LOGIN PASSWORD '$APP_DB_PASSWORD' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
            END IF;
        END
        \$\$;
        REVOKE CONNECT ON DATABASE $db FROM PUBLIC;
        REVOKE ALL ON DATABASE $db FROM $role;
        GRANT CONNECT ON DATABASE $db TO $role;
EOSQL
    psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$db" <<-EOSQL
        GRANT USAGE ON SCHEMA public TO $role;
        GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO $role;
        GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO $role;
        ALTER DEFAULT PRIVILEGES FOR ROLE $POSTGRES_USER IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO $role;
        ALTER DEFAULT PRIVILEGES FOR ROLE $POSTGRES_USER IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO $role;
EOSQL
    echo "  $role -> $db"
done

echo "migration-e2e init-db.sh complete."
