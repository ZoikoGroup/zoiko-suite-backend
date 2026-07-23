#!/usr/bin/env bash
# init-db-phase7.sh
# Creates all Phase 7 databases and runs migrations
set -euo pipefail

PGUSER="${POSTGRES_USER:-postgres}"

declare -A SERVICES
SERVICES=(
  ["zoiko_connectivity_api_bridge"]="services/connectivity-api-bridge-svc/deployments/migrations/001_init.sql"
  ["zoiko_banking_connector"]="services/banking-connector-svc/deployments/migrations/001_init.sql"
  ["zoiko_hris_connector"]="services/hris-connector-svc/deployments/migrations/001_init.sql"
  ["zoiko_tax_authority_interface"]="services/tax-authority-interface-svc/deployments/migrations/001_init.sql"
  ["zoiko_esignature_integration"]="services/esignature-integration-svc/deployments/migrations/001_init.sql"
  ["zoiko_external_data_feed"]="services/external-data-feed-svc/deployments/migrations/001_init.sql"
)

for DB in "${!SERVICES[@]}"; do
  echo "Creating database: $DB"
  psql -v ON_ERROR_STOP=1 --username "$PGUSER" --dbname postgres <<-EOSQL
    CREATE DATABASE $DB;
EOSQL
  echo "Database $DB created."
done

echo "Phase 7 databases provisioned successfully."
