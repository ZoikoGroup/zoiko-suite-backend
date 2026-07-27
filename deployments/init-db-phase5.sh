#!/bin/bash
# init-db-phase5.sh — Creates Phase 5 databases and applies their migrations.
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

# ── Contract Lifecycle ────────────────────────────────────────────────────────
create_db contract_lifecycle
apply_migration contract_lifecycle /migrations/contract-lifecycle/000001_initial_schema.up.sql

# ── Clause Template ───────────────────────────────────────────────────────────
create_db clause_template
apply_migration clause_template /migrations/clause-template/000001_initial_schema.up.sql

# ── Obligation Tracking ───────────────────────────────────────────────────────
create_db obligation_tracking
apply_migration obligation_tracking /migrations/obligation-tracking/000001_initial_schema.up.sql

# ── Board Resolutions ─────────────────────────────────────────────────────────
create_db board_resolutions
apply_migration board_resolutions /migrations/board-resolutions/000001_initial_schema.up.sql

# ── Corporate Actions ─────────────────────────────────────────────────────────
create_db corporate_actions
apply_migration corporate_actions /migrations/corporate-actions/000001_initial_schema.up.sql

# ── Counterparty Management ───────────────────────────────────────────────────
create_db counterparty_management
apply_migration counterparty_management /migrations/counterparty-management/000001_initial_schema.up.sql

# ── Tax Rules ─────────────────────────────────────────────────────────────────
create_db tax_rules
apply_migration tax_rules /migrations/tax-rules/000001_initial_schema.up.sql

# ── Tax Determination ─────────────────────────────────────────────────────────
create_db tax_determination
apply_migration tax_determination /migrations/tax-determination/000001_initial_schema.up.sql

# ── VAT/GST ───────────────────────────────────────────────────────────────────
create_db vat_gst
apply_migration vat_gst /migrations/vat-gst/000001_initial_schema.up.sql

# ── Corporate Tax ─────────────────────────────────────────────────────────────
create_db corporate_tax
apply_migration corporate_tax /migrations/corporate-tax/000001_initial_schema.up.sql

# ── Withholding Tax ───────────────────────────────────────────────────────────
create_db withholding_tax
apply_migration withholding_tax /migrations/withholding-tax/000001_initial_schema.up.sql

# ── Filing Preparation ────────────────────────────────────────────────────────
create_db filing_preparation
apply_migration filing_preparation /migrations/filing-preparation/000001_initial_schema.up.sql

# ── Filing Tracker ────────────────────────────────────────────────────────────
create_db filing_tracker
apply_migration filing_tracker /migrations/filing-tracker/000001_initial_schema.up.sql

# ── Compliance Status ─────────────────────────────────────────────────────────
create_db compliance_status
apply_migration compliance_status /migrations/compliance-status/000001_initial_schema.up.sql

# ── Exception & Escalation ───────────────────────────────────────────────────
create_db exception_escalation
apply_migration exception_escalation /migrations/exception-escalation/000001_initial_schema.up.sql

echo "=== Phase 5 databases initialised successfully ==="
