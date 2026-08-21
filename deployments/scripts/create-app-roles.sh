#!/bin/bash
#
# Give each finished service an ORDINARY database role, so the row-level
# security policies on its tables actually apply to it.
#
# WHY THIS EXISTS
#
# Every service in compose connects as `postgres`, and in this image that is a
# SUPERUSER. Postgres exempts superusers and BYPASSRLS holders from the row
# security system altogether, so every tenant_isolation_policy in this platform
# has been inert for every query ever made — including after the force_rls
# migrations, which only bind the table OWNER. The policies read as isolation
# without being it.
#
# What isolates tenants today is the explicit `tenant_id = $n` predicate each
# store carries, over a tenant taken from the verified X-Tenant-Id and nowhere
# else. That is a real control and it is load-bearing. This script adds the
# second one back: a role that the policy binds, so a query which forgets its
# predicate returns nothing instead of everything.
#
# WHAT A ROLE HERE CAN AND CANNOT DO
#
#   - NOSUPERUSER NOBYPASSRLS: the policy applies. That is the whole point.
#   - Not the table OWNER: `postgres` still owns every table (migrations run as
#     it), so the app role is a plain grantee and RLS applies to it whether or
#     not FORCE was set. FORCE stays correct as defence in depth.
#   - DML only, no DDL: an app role cannot ALTER or DROP the tables it reads.
#     Migrations keep running as the owner.
#   - One role per database, so a compromised service reaches its own data and
#     no more. A single shared app role would undo half the point.
#
# ORDER MATTERS: run this AFTER migrations. It grants on the tables that exist
# now, and sets ALTER DEFAULT PRIVILEGES so tables created by LATER migrations
# are reachable too — without that, the next migration's table is invisible to
# the service and the failure looks like a missing row rather than a missing
# grant.
#
# IDEMPOTENT: safe to re-run. Existing roles have their password reset and their
# grants re-applied.
#
# THE PASSWORD is taken from APP_DB_PASSWORD. The local default matches what
# compose already uses so a dev stack keeps working; a real deployment supplies
# per-service secrets from its secret manager and does not use this default.
set -euo pipefail

PGUSER="${POSTGRES_USER:-postgres}"
APP_DB_PASSWORD="${APP_DB_PASSWORD:-postgres}"

# database:role — one line per finished service. Every one listed has had its
# store integration suite run against a NOSUPERUSER NOBYPASSRLS role, which is
# the only check that shows whether each query installs app.tenant_id: as a
# superuser, a query that forgets the scope still returns rows, and as an
# ordinary role the same query returns nothing.
#
# obligations-svc joined the list only after its store was changed to run every
# statement inside the tenant-scoped transaction it already had a helper for.
# Nine of its queries went straight to the pool, so under an ordinary role its
# entire compliance register would have read as empty.
SERVICES="
governance_decision_log:app_governance_decision_log
policy:app_policy
configuration_feature_flag:app_configuration_feature_flag
secret_vault_integration:app_secret_vault_integration
purchase_request:app_purchase_request
purchase_order:app_purchase_order
spend_controls:app_spend_controls
vendor_due_diligence:app_vendor_due_diligence
evidence_requirements:app_evidence_requirements
accounts_payable:app_accounts_payable
accounts_receivable:app_accounts_receivable
general_ledger:app_general_ledger
financial_close:app_financial_close
bank_reconciliation:app_bank_reconciliation
notification:app_notification
board_resolutions:app_board_resolutions
schema_registry:app_schema_registry
jurisdiction_rules:app_jurisdiction_rules
delegated_authority:app_delegated_authority
document_vault:app_document_vault
obligations:app_obligations
"

created=0
for entry in $SERVICES; do
    db="${entry%%:*}"
    role="${entry##*:}"

    # The role is a cluster-level object, so it is created once against any
    # database; the grants below are per-database and must run inside $db.
    psql -v ON_ERROR_STOP=1 --username "$PGUSER" --dbname postgres <<-EOSQL
	DO \$\$
	BEGIN
	    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '$role') THEN
	        CREATE ROLE $role LOGIN PASSWORD '$APP_DB_PASSWORD'
	            NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
	    ELSE
	        ALTER ROLE $role LOGIN PASSWORD '$APP_DB_PASSWORD'
	            NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
	    END IF;
	END
	\$\$;
	-- REVOKE FROM PUBLIC, not just from the role. Postgres grants CONNECT on a
	-- new database to PUBLIC, which every role holds -- so granting CONNECT to
	-- one role isolates nothing on its own. Verified: before this line,
	-- app_general_ledger could open a session on accounts_payable, read the
	-- catalog, and reach anything PUBLIC had been granted there. Superusers are
	-- exempt from the check, so the services still connecting as postgres are
	-- unaffected by the revoke.
	REVOKE CONNECT ON DATABASE $db FROM PUBLIC;
	REVOKE ALL ON DATABASE $db FROM $role;
	GRANT CONNECT ON DATABASE $db TO $role;
	EOSQL

    psql -v ON_ERROR_STOP=1 --username "$PGUSER" --dbname "$db" <<-EOSQL
	GRANT USAGE ON SCHEMA public TO $role;

	-- DML on what exists now. No TRUNCATE (it bypasses RLS entirely and no
	-- service needs it) and no DDL.
	GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO $role;
	GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO $role;

	-- ...and on what later migrations add. Applies only to objects created by
	-- the role running the migrations, which is why it names $PGUSER.
	ALTER DEFAULT PRIVILEGES FOR ROLE $PGUSER IN SCHEMA public
	    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO $role;
	ALTER DEFAULT PRIVILEGES FOR ROLE $PGUSER IN SCHEMA public
	    GRANT USAGE, SELECT ON SEQUENCES TO $role;
	EOSQL

    echo "  $role -> $db"
    created=$((created + 1))
done

echo "Provisioned $created application roles."
echo
echo "These roles are not in use until a service's DB_USER names one. Flipping"
echo "DB_USER is the step that makes the policies bite, and it is the step that"
echo "needs verifying per service: a query that never sets app.tenant_id returns"
echo "ZERO rows under an ordinary role, where as superuser it returned everything."
