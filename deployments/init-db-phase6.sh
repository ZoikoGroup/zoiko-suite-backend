#!/bin/bash
# init-db-phase6.sh — Creates Phase 6 databases and applies their migrations.
set -e

run_sql() {
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$1" -c "$2"
}

create_db() {
  echo "Creating database: $1..."
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE DATABASE $1;
EOSQL
}

apply_migration() {
  echo "Applying migration: $1 -> $2"
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$1" -f "$2"
}

# ── Anomaly Detection ─────────────────────────────────────────────────────────
create_db anomaly_detection
apply_migration anomaly_detection /migrations/anomaly-detection/000001_initial_schema.up.sql

echo "=== Phase 6 databases initialised successfully ==="
