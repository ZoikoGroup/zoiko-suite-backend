#!/bin/bash
set -e

# Create all 11 databases required by the services. configuration_feature_flag,
# secret_vault_integration, and obligations were added alongside the
# Observability Baseline retrofit (docs/architecture/observability-baseline-plan.md)
# — those three services never had a compose entry or a database provisioned
# here before, a pre-existing gap found and closed in the same pass.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
    CREATE DATABASE audit_event_store;
    CREATE DATABASE tenant_entity_registry;
    CREATE DATABASE jurisdiction_rules;
    CREATE DATABASE governance_decision_log;
    CREATE DATABASE identity_context;
    CREATE DATABASE policy;
    CREATE DATABASE authorization_svc;
    CREATE DATABASE workflow;
    CREATE DATABASE configuration_feature_flag;
    CREATE DATABASE secret_vault_integration;
    CREATE DATABASE obligations;
    CREATE DATABASE schema_registry;
    CREATE DATABASE document_vault;
    CREATE DATABASE evidence_manifest;
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
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "tenant_entity_registry" -f /migrations/tenant-entity-registry/000003_add_residency_region_to_policies.up.sql

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
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "policy" -f /migrations/policy/000002_add_activation_audit.up.sql

# Apply migrations for authorization-svc
echo "Applying migrations for authorization_svc..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "authorization_svc" -f /migrations/authorization/000001_initial_schema.up.sql

# Apply migrations for workflow-svc
echo "Applying migrations for workflow..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "workflow" -f /migrations/workflow/000001_initial_schema.up.sql

# Apply migrations for configuration-feature-flag-svc
echo "Applying migrations for configuration_feature_flag..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "configuration_feature_flag" -f /migrations/configuration-feature-flag/000001_initial_schema.up.sql

# Apply migrations for secret-vault-integration-svc
echo "Applying migrations for secret_vault_integration..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "secret_vault_integration" -f /migrations/secret-vault-integration/000001_initial_schema.up.sql

# Apply migrations for obligations-svc
echo "Applying migrations for obligations..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "obligations" -f /migrations/obligations/000001_initial_schema.up.sql

# Apply migrations for schema-registry-svc
echo "Applying migrations for schema_registry..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "schema_registry" -f /migrations/schema-registry/000001_initial_schema.up.sql

# Apply migrations for document-vault-svc
echo "Applying migrations for document_vault..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "document_vault" -f /migrations/document-vault/000001_initial_schema.up.sql

# Apply migrations for evidence-manifest-svc
echo "Applying migrations for evidence_manifest..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "evidence_manifest" -f /migrations/evidence-manifest/000001_initial_schema.up.sql

echo "All migrations applied successfully."
