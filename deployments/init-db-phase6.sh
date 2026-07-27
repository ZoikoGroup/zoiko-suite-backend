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

# ── Forecasting Service ──────────────────────────────────────────────────────
create_db forecasting
apply_migration forecasting /migrations/forecasting/000001_initial_schema.up.sql

# ── Compliance Risk Scoring Service ──────────────────────────────────────────
create_db compliance_risk_scoring
apply_migration compliance_risk_scoring /migrations/compliance-risk-scoring/000001_initial_schema.up.sql

# ── Reconciliation Intelligence Service ──────────────────────────────────────
create_db reconciliation_intelligence
apply_migration reconciliation_intelligence /migrations/reconciliation-intelligence/000001_initial_schema.up.sql

# ── Reporting Orchestration Service ──────────────────────────────────────────
create_db reporting_orchestration
apply_migration reporting_orchestration /migrations/reporting-orchestration/000001_initial_schema.up.sql

# ── Migration Integrity Service ───────────────────────────────────────────────
create_db migration_integrity
apply_migration migration_integrity /migrations/migration-integrity/000001_initial_schema.up.sql

echo "=== Phase 6 databases initialised successfully ==="





