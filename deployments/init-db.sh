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
    CREATE DATABASE workflow_history;
    CREATE DATABASE general_ledger;
    CREATE DATABASE accounts_payable;
    CREATE DATABASE accounts_receivable;
    CREATE DATABASE purchase_request;
    CREATE DATABASE treasury;
    CREATE DATABASE financial_close;
    CREATE DATABASE bank_reconciliation;
    CREATE DATABASE intercompany_accounting;
    CREATE DATABASE consolidation_svc;
    CREATE DATABASE invoice_approval;
    CREATE DATABASE employee_master;
    CREATE DATABASE employment_contracts;
    CREATE DATABASE payroll_run;
    CREATE DATABASE compensation;
    CREATE DATABASE benefits;
    CREATE DATABASE payroll_tax;
    CREATE DATABASE payroll_exceptions;
    CREATE DATABASE leave_absence;
    CREATE DATABASE org_structure;
    CREATE DATABASE offboarding_severance;
    CREATE DATABASE workforce_compliance;
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

# Apply migrations for workflow-history-svc
echo "Applying migrations for workflow_history..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "workflow_history" -f /migrations/workflow-history/000001_initial_schema.up.sql

# Apply migrations for general-ledger-svc
echo "Applying migrations for general_ledger..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "general_ledger" -f /migrations/general-ledger/000001_initial_schema.up.sql

# Apply migrations for accounts-payable-svc
echo "Applying migrations for accounts_payable..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "accounts_payable" -f /migrations/accounts-payable/000001_initial_schema.up.sql

# Apply migrations for accounts-receivable-svc
echo "Applying migrations for accounts_receivable..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "accounts_receivable" -f /migrations/accounts-receivable/000001_initial_schema.up.sql

# Apply migrations for purchase-request-svc
echo "Applying migrations for purchase_request..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "purchase_request" -f /migrations/purchase-request/000001_initial_schema.up.sql

# Apply migrations for treasury-svc
echo "Applying migrations for treasury..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "treasury" -f /migrations/treasury/000001_initial_schema.up.sql

# Apply migrations for financial-close-svc
echo "Applying migrations for financial_close..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "financial_close" -f /migrations/financial-close/000001_initial_schema.up.sql

# Apply migrations for bank-reconciliation-svc
echo "Applying migrations for bank_reconciliation..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "bank_reconciliation" -f /migrations/bank-reconciliation/000001_initial_schema.up.sql

# Apply migrations for intercompany-accounting-svc
echo "Applying migrations for intercompany_accounting..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "intercompany_accounting" -f /migrations/intercompany-accounting/000001_initial_schema.up.sql

# Apply migrations for consolidation-svc
echo "Applying migrations for consolidation..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "consolidation_svc" -f /migrations/consolidation/000001_initial_schema.up.sql

# Apply migrations for invoice-approval-svc
echo "Applying migrations for invoice_approval..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "invoice_approval" -f /migrations/invoice-approval/000001_initial_schema.up.sql

# Apply migrations for employee-master-svc
echo "Applying migrations for employee_master..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "employee_master" -f /migrations/employee-master/000001_initial_schema.up.sql

# Apply migrations for employment-contracts-svc
echo "Applying migrations for employment_contracts..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "employment_contracts" -f /migrations/employment-contracts/000001_initial_schema.up.sql

# Apply migrations for payroll-run-svc
echo "Applying migrations for payroll_run..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "payroll_run" -f /migrations/payroll-run/000001_initial_schema.up.sql

# Apply migrations for compensation-svc
echo "Applying migrations for compensation..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "compensation" -f /migrations/compensation/000001_initial_schema.up.sql

# Apply migrations for benefits-svc
echo "Applying migrations for benefits..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "benefits" -f /migrations/benefits/000001_initial_schema.up.sql

# Apply migrations for payroll-tax-svc
echo "Applying migrations for payroll_tax..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "payroll_tax" -f /migrations/payroll-tax/000001_initial_schema.up.sql

# Apply migrations for payroll-exceptions-svc
echo "Applying migrations for payroll_exceptions..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "payroll_exceptions" -f /migrations/payroll-exceptions/000001_initial_schema.up.sql

# Apply migrations for leave-absence-svc
echo "Applying migrations for leave_absence..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "leave_absence" -f /migrations/leave-absence/000001_initial_schema.up.sql

# Apply migrations for org-structure-svc
echo "Applying migrations for org_structure..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "org_structure" -f /migrations/org-structure/000001_initial_schema.up.sql

# Apply migrations for offboarding-severance-svc
echo "Applying migrations for offboarding_severance..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "offboarding_severance" -f /migrations/offboarding-severance/000001_initial_schema.up.sql

# Apply migrations for workforce-compliance-svc
echo "Applying migrations for workforce_compliance..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "workforce_compliance" -f /migrations/workforce-compliance/000001_initial_schema.up.sql

# ── Phase 5 ─────────────────────────────────────────────────────────────────

# Create database for contract-lifecycle-svc
echo "Creating database: contract_lifecycle..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE DATABASE contract_lifecycle;
EOSQL

# Apply migrations for contract-lifecycle-svc
echo "Applying migrations for contract_lifecycle..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "contract_lifecycle" -f /migrations/contract-lifecycle/000001_initial_schema.up.sql

# Create database for clause-template-svc
echo "Creating database: clause_template..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE DATABASE clause_template;
EOSQL

# Apply migrations for clause-template-svc
echo "Applying migrations for clause_template..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "clause_template" -f /migrations/clause-template/000001_initial_schema.up.sql

# Create database for obligation-tracking-svc
echo "Creating database: obligation_tracking..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE DATABASE obligation_tracking;
EOSQL

# Apply migrations for obligation-tracking-svc
echo "Applying migrations for obligation_tracking..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "obligation_tracking" -f /migrations/obligation-tracking/000001_initial_schema.up.sql

# Create database for board-resolutions-svc
echo "Creating database: board_resolutions..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE DATABASE board_resolutions;
EOSQL

# Apply migrations for board-resolutions-svc
echo "Applying migrations for board_resolutions..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "board_resolutions" -f /migrations/board-resolutions/000001_initial_schema.up.sql

# Create database for corporate-actions-svc
echo "Creating database: corporate_actions..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE DATABASE corporate_actions;
EOSQL

# Apply migrations for corporate-actions-svc
echo "Applying migrations for corporate_actions..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "corporate_actions" -f /migrations/corporate-actions/000001_initial_schema.up.sql

# Create database for counterparty-management-svc
echo "Creating database: counterparty_management..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE DATABASE counterparty_management;
EOSQL

# Apply migrations for counterparty-management-svc
echo "Applying migrations for counterparty_management..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "counterparty_management" -f /migrations/counterparty-management/000001_initial_schema.up.sql

# Create database for tax-rules-svc
echo "Creating database: tax_rules..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE DATABASE tax_rules;
EOSQL

# Apply migrations for tax-rules-svc
echo "Applying migrations for tax_rules..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "tax_rules" -f /migrations/tax-rules/000001_initial_schema.up.sql

# Create database for tax-determination-svc
echo "Creating database: tax_determination..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE DATABASE tax_determination;
EOSQL

# Apply migrations for tax-determination-svc
echo "Applying migrations for tax_determination..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "tax_determination" -f /migrations/tax-determination/000001_initial_schema.up.sql

# Create database for vat-gst-svc
echo "Creating database: vat_gst..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE DATABASE vat_gst;
EOSQL

# Apply migrations for vat-gst-svc
echo "Applying migrations for vat_gst..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "vat_gst" -f /migrations/vat-gst/000001_initial_schema.up.sql

# Create database for corporate-tax-svc
echo "Creating database: corporate_tax..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE DATABASE corporate_tax;
EOSQL

# Apply migrations for corporate-tax-svc
echo "Applying migrations for corporate_tax..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "corporate_tax" -f /migrations/corporate-tax/000001_initial_schema.up.sql

echo "All migrations applied successfully."
