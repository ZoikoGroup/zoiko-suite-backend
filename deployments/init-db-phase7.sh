#!/usr/bin/env bash
# init-db-phase7.sh
# Creates all Phase 7 databases and runs migrations
set -euo pipefail

PGUSER="${POSTGRES_USER:-postgres}"

create_db_and_migrate() {
  DB="$1"
  MIGRATION_FILE="$2"
  echo "Creating database: $DB..."
  psql -v ON_ERROR_STOP=1 --username "$PGUSER" --dbname postgres <<-EOSQL
    CREATE DATABASE $DB;
EOSQL
  echo "Applying migration to $DB from $MIGRATION_FILE..."
  psql -v ON_ERROR_STOP=1 --username "$PGUSER" --dbname "$DB" -f "$MIGRATION_FILE"
  echo "Database $DB ready."
}

create_db_and_migrate "zoiko_connectivity_api_bridge" "/migrations/connectivity-api-bridge/001_init.sql"
create_db_and_migrate "zoiko_banking_connector" "/migrations/banking-connector/001_init.sql"
create_db_and_migrate "zoiko_hris_connector" "/migrations/hris-connector/001_init.sql"
create_db_and_migrate "zoiko_tax_authority_interface" "/migrations/tax-authority-interface/001_init.sql"
create_db_and_migrate "zoiko_esignature_integration" "/migrations/esignature-integration/001_init.sql"
create_db_and_migrate "zoiko_external_data_feed" "/migrations/external-data-feed/001_init.sql"

echo "Phase 7 databases provisioned & migrated successfully."

# ── Runtime role: zoiko_app (least-privilege, RLS-respecting) ──────────────
# See init-db.sh (main stack) for the full rationale — every service must
# stop connecting as the Postgres superuser, which bypasses Row-Level
# Security unconditionally regardless of how correct the policies are.
psql -v ON_ERROR_STOP=1 --username "$PGUSER" --dbname "postgres" <<-EOSQL
    CREATE ROLE zoiko_app WITH LOGIN PASSWORD 'zoiko_app_password' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
EOSQL

for db in zoiko_connectivity_api_bridge zoiko_banking_connector zoiko_hris_connector \
    zoiko_tax_authority_interface zoiko_esignature_integration zoiko_external_data_feed; do
    psql -v ON_ERROR_STOP=1 --username "$PGUSER" --dbname "$db" <<-EOSQL
        GRANT CONNECT ON DATABASE $db TO zoiko_app;
        GRANT USAGE ON SCHEMA public TO zoiko_app;
        GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO zoiko_app;
        GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO zoiko_app;
        ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO zoiko_app;
        ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO zoiko_app;
EOSQL
done
echo "zoiko_app role provisioned across Phase 7 databases."
