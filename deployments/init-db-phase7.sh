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
