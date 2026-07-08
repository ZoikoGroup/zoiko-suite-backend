#!/bin/bash
set -e

# Create all 6 databases required by the services
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
    CREATE DATABASE audit_event_store;
    CREATE DATABASE tenant_entity_registry;
    CREATE DATABASE jurisdiction_rules;
    CREATE DATABASE governance_decision_log;
    CREATE DATABASE identity_context;
    CREATE DATABASE policy;
EOSQL

echo "Databases created successfully. Running migration scripts..."

# Apply migrations for audit-event-store-svc
echo "Applying migrations for audit_event_store..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "audit_event_store" -f /migrations/audit-event-store/000001_initial_schema.up.sql

# Apply migrations for identity-context-svc
echo "Applying migrations for identity_context..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "identity_context" -f /migrations/identity-context/000001_initial_schema.up.sql

# Apply migrations for tenant-entity-registry-svc
echo "Applying migrations for tenant_entity_registry..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "tenant_entity_registry" -f /migrations/tenant-entity-registry/000001_initial_schema.up.sql
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "tenant_entity_registry" -f /migrations/tenant-entity-registry/000002_add_tenant_id_to_junction_tables.up.sql

# Apply migrations for jurisdiction-rules-svc
echo "Applying migrations for jurisdiction_rules..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "jurisdiction_rules" -f /migrations/jurisdiction-rules/000001_initial_schema.up.sql
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "jurisdiction_rules" -f /migrations/jurisdiction-rules/000002_add_audit_columns.up.sql

# Apply migrations for governance-decision-log-svc
echo "Applying migrations for governance_decision_log..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "governance_decision_log" -f /migrations/governance-decision-log/000001_initial_schema.up.sql

# Apply migrations for policy-svc
echo "Applying migrations for policy..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "policy" -f /migrations/policy/000001_initial_schema.up.sql

echo "All migrations applied successfully."
