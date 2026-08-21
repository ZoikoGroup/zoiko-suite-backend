#!/bin/bash
#
# Postgres first-run initialization for the whole stack: one database per
# service, then every migration that service has on disk.
#
# WHY THIS IS A LOOP AND NOT A LIST OF FILES.
#
# This script used to name each migration explicitly -- 121 `psql -f` lines. A
# migration was therefore applied only if somebody also remembered to edit this
# file, and twelve had not been: every force_rls migration across the finance and
# commercial-ops services, which is precisely the tenant isolation a fresh
# environment most needs. Nothing failed; the databases simply came up without
# them. Globbing the directory removes the class of defect rather than the twelve
# instances of it.
#
# ORDERING. `sort` over the 000001_, 000002_ ... prefixes is the migration order,
# and `LC_ALL=C` keeps that sort locale-independent. Only *.up.sql is applied;
# the .down.sql files are the manual rollback path and are never run here.
#
# THIS RUNS ONLY ON AN EMPTY VOLUME. docker-entrypoint-initdb.d is skipped
# entirely when PGDATA already exists, so adding a migration does NOT reach an
# existing stack -- it has to be applied by hand there. That is a property of the
# postgres image, not of this script, and it is why a migration added today may
# be live in CI and absent locally.
set -euo pipefail

DB_COUNT=0
MIGRATION_COUNT=0

create_database() {
    local db="$1"
    psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
        -c "CREATE DATABASE $db"
    DB_COUNT=$((DB_COUNT + 1))
}

# apply_migrations applies every *.up.sql under /migrations/$dir to $db.
#
# A mounted directory with no *.up.sql, or a pair whose directory is not mounted
# at all, is a FAILURE and not a quiet skip: both mean a service's schema was
# never created, and the service will come up and answer 500 to every read. The
# older form of that mistake took a whole day to find.
apply_migrations() {
    local db="$1" dir="$2" path="/migrations/$2" applied=0 f

    if [ ! -d "$path" ]; then
        echo "FATAL: $path is not mounted -- $db would have no schema" >&2
        exit 1
    fi

    for f in $(LC_ALL=C ls "$path"/*.up.sql 2>/dev/null | LC_ALL=C sort); do
        echo "    $db <- $(basename "$f")"
        psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$db" -f "$f"
        applied=$((applied + 1))
    done

    if [ "$applied" -eq 0 ]; then
        echo "FATAL: $path is mounted but holds no *.up.sql -- $db would have no schema" >&2
        exit 1
    fi
    MIGRATION_COUNT=$((MIGRATION_COUNT + applied))
}

# One entry per service: <database>:<directory under /migrations>. The two
# spellings differ (underscores for the database, hyphens for the directory), and
# both must match what docker-compose.yml mounts.
SERVICES="
audit_event_store:audit-event-store
identity_context:identity-context
tenant_entity_registry:tenant-entity-registry
commercial_account:commercial-account
capability_registry:capability-registry
ai_governance:ai-governance
kill_switch_registry:kill-switch-registry
retention_registry:retention-registry
metric_registry:metric-registry
source_authority:source-authority
jurisdiction_rules:jurisdiction-rules
governance_decision_log:governance-decision-log
policy:policy
authorization_svc:authorization
workflow:workflow
configuration_feature_flag:configuration-feature-flag
secret_vault_integration:secret-vault-integration
obligations:obligations
schema_registry:schema-registry
document_vault:document-vault
evidence_manifest:evidence-manifest
workflow_history:workflow-history
general_ledger:general-ledger
accounts_payable:accounts-payable
accounts_receivable:accounts-receivable
purchase_request:purchase-request
purchase_order:purchase-order
spend_controls:spend-controls
vendor_due_diligence:vendor-due-diligence
notification:notification
procurement_workflow:procurement-workflow
performance_review:performance-review
delegated_authority:delegated-authority
access_control:access-control
decision_support:decision-support
treasury:treasury
financial_close:financial-close
bank_reconciliation:bank-reconciliation
intercompany_accounting:intercompany-accounting
consolidation_svc:consolidation
invoice_approval:invoice-approval
employee_master:employee-master
employment_contracts:employment-contracts
payroll_run:payroll-run
compensation:compensation
benefits:benefits
payroll_tax:payroll-tax
payroll_exceptions:payroll-exceptions
leave_absence:leave-absence
org_structure:org-structure
offboarding_severance:offboarding-severance
workforce_compliance:workforce-compliance
contract_lifecycle:contract-lifecycle
clause_template:clause-template
obligation_tracking:obligation-tracking
board_resolutions:board-resolutions
corporate_actions:corporate-actions
counterparty_management:counterparty-management
tax_rules:tax-rules
tax_determination:tax-determination
vat_gst:vat-gst
corporate_tax:corporate-tax
evidence_requirements:evidence-requirements
"

echo "Creating databases..."
for entry in $SERVICES; do
    create_database "${entry%%:*}"
done
echo "Created $DB_COUNT databases."

echo "Applying migrations..."
for entry in $SERVICES; do
    db="${entry%%:*}"
    dir="${entry##*:}"
    echo "  $db"
    apply_migrations "$db" "$dir"
done

echo "All migrations applied: $MIGRATION_COUNT files across $DB_COUNT databases."

# Application roles, AFTER migrations: the grants cover the tables that now
# exist, plus ALTER DEFAULT PRIVILEGES for the ones later migrations add. Run
# last for that reason.
#
# Absent rather than fatal if unmounted: an older compose file that has not
# added the mount should still bring a stack up, just without the roles (and
# therefore with the policies still inert, as they were before).
if [ -f /scripts/create-app-roles.sh ]; then
    echo "Provisioning application roles..."
    bash /scripts/create-app-roles.sh
else
    echo "NOTE: /scripts/create-app-roles.sh not mounted -- services will keep"
    echo "      connecting as the superuser, and the RLS policies stay inert."
fi

# ── Runtime role: zoiko_app (least-privilege, RLS-respecting) ──────────────
#
# Every service has, until now, connected to Postgres AS THE SUPERUSER
# ($POSTGRES_USER, "postgres") for its normal runtime traffic, not just for
# running the migrations above. A Postgres superuser bypasses Row-Level
# Security unconditionally — regardless of how correct a service's RLS
# policies are, they simply never execute for that connection. 55 of the
# services provisioned by this script define real `CREATE POLICY` tenant-
# isolation policies (tenant-entity-registry-svc, general-ledger-svc,
# purchase-order-svc, and 52 others) — every one of them has been running
# with that guarantee silently disabled. This is the single most-cited
# critical gap against the original architecture spec's Doc 01 §11.2
# ("Row-level authorization enforced at the data access layer" as a stated
# minimum) and §17.1 ("least privilege" as a core security principle).
#
# The fix is the standard Postgres pattern: migrations continue to run as
# the superuser (DDL, extensions, and initial seed data need that), but a
# separate, non-superuser role is what every service actually connects as
# at runtime. A non-superuser, non-owner role is automatically subject to
# `ENABLE ROW LEVEL SECURITY` policies with no further action needed —
# `FORCE ROW LEVEL SECURITY` is only required to additionally restrict the
# table OWNER, which this role deliberately never is.
#
# zoiko_app_password is a throwaway local-dev value, exactly like
# POSTGRES_PASSWORD above — never used in staging/production, where secrets
# come from the secret vault / real KMS, not a plaintext compose file.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "postgres" <<-EOSQL
    CREATE ROLE zoiko_app WITH LOGIN PASSWORD 'zoiko_app_password' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
EOSQL

# zoiko_app takes the databases that have NO per-service app_ role of their own --
# everything create-app-roles.sh above did not claim.
#
# The skip below is what stops the two role designs cancelling each other out.
# create-app-roles.sh REVOKEs CONNECT FROM PUBLIC on each of its 21 databases and
# grants it back to that one role, and that revoke is the line doing the
# isolating: without it any role could open a session on any service's database.
# Granting zoiko_app CONNECT on those same 21 would hand exactly that reach to a
# role shared by the other 42 services, undoing the revoke for every one of them.
#
# Membership is read from pg_roles rather than listed again here, so moving a
# service from the shared role to its own needs no edit in this file -- adding it
# to create-app-roles.sh is enough, and the two lists cannot drift apart.
echo "Granting least-privilege runtime access to zoiko_app (databases with no per-service role)..."
zoiko_app_granted=0
zoiko_app_skipped=0
for db in \
    access_control accounts_payable accounts_receivable ai_governance audit_event_store \
    authorization_svc bank_reconciliation benefits board_resolutions capability_registry \
    clause_template commercial_account compensation configuration_feature_flag consolidation_svc \
    contract_lifecycle corporate_actions corporate_tax counterparty_management decision_support \
    delegated_authority document_vault employee_master employment_contracts evidence_manifest \
    evidence_requirements financial_close general_ledger governance_decision_log identity_context \
    intercompany_accounting invoice_approval jurisdiction_rules kill_switch_registry leave_absence \
    metric_registry notification obligation_tracking obligations offboarding_severance org_structure \
    payroll_exceptions payroll_run payroll_tax performance_review policy procurement_workflow \
    purchase_order purchase_request retention_registry schema_registry secret_vault_integration \
    source_authority spend_controls tax_determination tax_rules tenant_entity_registry treasury \
    vat_gst vendor_due_diligence workflow workflow_history workforce_compliance; do
    if [ "$(psql -tAX --username "$POSTGRES_USER" --dbname postgres \
            -c "SELECT 1 FROM pg_roles WHERE rolname = 'app_${db}'")" = "1" ]; then
        zoiko_app_skipped=$((zoiko_app_skipped + 1))
        continue
    fi
    zoiko_app_granted=$((zoiko_app_granted + 1))
    psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$db" <<-EOSQL
        GRANT CONNECT ON DATABASE $db TO zoiko_app;
        GRANT USAGE ON SCHEMA public TO zoiko_app;
        GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO zoiko_app;
        GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO zoiko_app;
        ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO zoiko_app;
        ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO zoiko_app;
EOSQL
done
echo "zoiko_app granted on $zoiko_app_granted databases; skipped $zoiko_app_skipped that have their own app_ role."
