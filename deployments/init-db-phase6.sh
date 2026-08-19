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

# ── Authorization Service ───────────────────────────────────────────────────
create_db authorization_svc
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

# ── Runtime role: zoiko_app (least-privilege, RLS-respecting) ──────────────
# See init-db.sh (main stack) for the full rationale — every service must
# stop connecting as the Postgres superuser, which bypasses Row-Level
# Security unconditionally regardless of how correct the policies are.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE ROLE zoiko_app WITH LOGIN PASSWORD 'zoiko_app_password' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
EOSQL

for db in authorization_svc anomaly_detection forecasting compliance_risk_scoring \
    reconciliation_intelligence reporting_orchestration migration_integrity; do
    psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$db" <<-EOSQL
        GRANT CONNECT ON DATABASE $db TO zoiko_app;
        GRANT USAGE ON SCHEMA public TO zoiko_app;
        GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO zoiko_app;
        GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO zoiko_app;
        ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO zoiko_app;
        ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO zoiko_app;
EOSQL
done
echo "zoiko_app role provisioned across Phase 6 databases."





