<#
.SYNOPSIS
    Grant the admin console's demo principal every permission the wired services
    need, via authorization-svc's admin API.

.DESCRIPTION
    authorization-svc denies by default: with no role assignment it answers
    DENIED / no_grant, and every service in this platform fails closed on that,
    so writes from the console are refused. That is correct behaviour, not a bug
    -- but it means a fresh stack has a console that can read and write nothing.

    RUN seed-demo-registry.ps1 AS WELL. This script answers "may the demo principal
    act on the demo legal entity"; it does not answer "does that legal entity
    exist". Nothing had ever registered the demo tenant or entity in
    tenant-entity-registry-svc, and no service noticed until
    accounts-receivable-svc started reconciling the legal entity on a write -- at
    which point every invoice is refused with legal_entity_not_in_tenant no matter
    what this script grants. The two are separate scripts because they seed
    different services, and a fresh stack needs both.

    This script creates the grant chain the console's demo identity expects:

        role (CONSOLE_DEMO_OPERATOR)
          -> one permission bundle per service
            -> assignment to the demo principal on the demo legal entity
            -> a second assignment scoped to the TENANT (see below)

    The UUIDs below MUST match DEMO_IDENTITY in the frontend's lib/auth.ts.
    They are UUIDs because the backend stores them in uuid columns -- a
    readable id like "demo-tenant" fails inside the driver and surfaces as a
    503 that reads like an outage rather than a validation error.

    Two assignments, not one. evidence-requirements-svc falls back to the TENANT
    as the authorization scope when a request omits legal_entity_id, so a
    console option like "tenant-wide" is refused by a legal-entity-only
    assignment. Granting both is what makes the tenant-scoped path reachable.

    Idempotent, and safe to re-run after this script gains new actions -- which
    is the failure this version specifically avoids. The old version probed a
    single action and returned early if it was GRANTED, so on any existing
    volume it would see PO_ISSUE already granted and never add the bundles that
    had been appended since. It looked like it had run successfully and had
    granted nothing.

.PARAMETER AuthzUrl
    Base URL of authorization-svc itself. Use this for the gateway-less mode the
    console's .env.local actually runs in (ZOIKO_USE_GATEWAY=false), where
    starting Traefik would drag in eight support containers for a routing layer.

.PARAMETER GatewayUrl
    Base URL of the single-port Traefik gateway, when you ARE running it. The
    service is reached at $GatewayUrl/authorization-svc.

.EXAMPLE
    # Gateway-less -- matches the console's default .env.local.
    ./seed-demo-rbac.ps1 -AuthzUrl http://localhost:8089

.EXAMPLE
    # Through the gateway.
    ./seed-demo-rbac.ps1 -GatewayUrl http://localhost:8000
#>
[CmdletBinding(DefaultParameterSetName = "Direct")]
param(
    [Parameter(ParameterSetName = "Direct")]
    [string] $AuthzUrl = "http://localhost:8089",

    [Parameter(ParameterSetName = "Gateway", Mandatory)]
    [string] $GatewayUrl,

    # The Postgres container the IAM bootstrap writes through (development only).
    [string] $PgContainer = "zoiko-postgres"
)

$ErrorActionPreference = "Stop"

# Must match DEMO_IDENTITY in zoiko-suite-fe/lib/auth.ts.
$TENANT_ID    = "11111111-1111-1111-1111-111111111111"
$LEGAL_ENTITY = "22222222-2222-2222-2222-222222222222"
$PRINCIPAL_ID = "33333333-3333-3333-3333-333333333333"
$APPROVER_ID  = "66666666-6666-6666-6666-666666666666"
$ROLE_ID      = "44444444-4444-4444-4444-444444444444"

# The seed's own IAM administrator. It holds IAM_ADMIN_FULL through a role of
# its own and never holds $ROLE_ID, because authorization-svc refuses a
# principal changing the permissions of a role they hold (self_grant_not_allowed)
# -- and both demo principals hold $ROLE_ID, so neither may write its bundles.
$SEED_ADMIN_ID      = "5eed0000-0000-4000-8000-00000000a001"
$SEED_ADMIN_ROLE_ID = "5eed0000-0000-4000-8000-00000000a002"

# access-control-svc's own identity (ACS_SERVICE_PRINCIPAL_ID). It EXECUTES the
# REVOKE an access reviewer decides (Group 1 audit gap S9-B), and opens the
# EVENT_TRIGGERED reviews HR events trigger (S9-C2), so it holds
# iam.assignment.revoke and iam.assignment.read and nothing else: it can read
# and remove access, never grant it.
$ACS_SERVICE_ID      = "svc-access-control"
$ACS_SERVICE_ROLE_ID = "5eed0000-0000-4000-8000-00000000a003"

# policy-svc's own identity (POLICY_LEDGER_PRINCIPAL_ID). Its evidence relay
# writes every policy evaluation to governance-decision-log-svc as this
# principal (decision D-02), so it holds GOVERNANCE_DECISION_RECORD and nothing
# else: on the platform scope (evaluations no entity scopes) and on the legal
# entity (evaluations scoped to it).
$POLICY_SERVICE_ID      = "svc-policy"
$POLICY_SERVICE_ROLE_ID = "5eed0000-0000-4000-8000-00000000a004"

$ROLE_CODE = "CONSOLE_DEMO_OPERATOR"

# Platform-scope records (decisions, config, flags, secret policies, policy
# definitions) authorize against this id -- see the platform-scope assignment
# below and the verification loop at the bottom.
$PLATFORM_SCOPE = "00000000-0000-0000-0000-00000000f001"

# One bundle per service, so a service's grants can be read off in one place and
# a newly wired service is one entry rather than an edit in four places.
#
# Action names are free strings to authorization-svc -- it never validates them
# against a registry -- so a name that does not match what the service checks
# produces a GRANTED probe here and a 403 there. Each set below is copied from
# the `action*` constants in that service's internal/handler.
$BUNDLES = @(
    @{
        Code    = "PO_FULL"
        Service = "purchase-order-svc"
        # PO_ISSUE/PO_AMEND/PO_CLOSE are the original three; the rest gate the
        # governed lifecycle (AP-03 section 6): create, submit, approve (plus the
        # high-value tier), hold/release, cancel, the supplier-hold exception, and
        # the receipt/invoice progress push. Copied from the action* constants in
        # purchase-order-svc/internal/handler. Maker-checker is NOT decided here:
        # authorization-svc's own-object SoD refuses a preparer approving their own
        # order even when they hold PO_APPROVE.
        Actions = @("PO_ISSUE", "PO_AMEND", "PO_CLOSE",
                    "PO_CREATE", "PO_SUBMIT", "PO_APPROVE", "PO_APPROVE_HIGHVALUE",
                    "PO_HOLD", "PO_CANCEL", "PO_SUPPLIER_EXCEPTION", "PO_PROGRESS_RECORD")
    },
    @{
        # goods-service-receipt-svc (AP-04): the spec's receipt.read/create/confirm/
        # reverse and service.accept, plus the "approved exception" a confirmer
        # needs to pass PO tolerance. Copied from the constants in
        # goods-service-receipt-svc/internal/handler. As with the PO bundle,
        # authorization-svc's own-object SoD still stops a receiver certifying
        # their own receipt where independent acceptance is required.
        Code    = "GOODS_RECEIPT_FULL"
        Service = "goods-service-receipt-svc"
        Actions = @("GOODS_SERVICE_RECEIPT_READ", "GOODS_SERVICE_RECEIPT_CREATE", "GOODS_SERVICE_RECEIPT_CONFIRM",
                    "GOODS_SERVICE_RECEIPT_REVERSE", "SERVICE_ACCEPT", "GOODS_SERVICE_RECEIPT_TOLERANCE_OVERRIDE")
    },
    @{
        Code    = "PR_FULL"
        Service = "purchase-request-svc"
        Actions = @("PR_REQUEST_CREATE", "PR_REQUEST_APPROVE", "PR_REQUEST_REJECT")
    },
    @{
        Code    = "EVIDENCE_FULL"
        Service = "evidence-requirements-svc"
        Actions = @("EVIDENCE_REQUIREMENT_CREATE", "EVIDENCE_REQUIREMENT_RETIRE")
    },
    @{
        # accounts-payable-svc gates each lifecycle hop on its own action, so
        # holding one does not imply the next -- that separation is the point,
        # and it means all four are needed to walk an invoice to payment.
        Code    = "AP_FULL"
        Service = "accounts-payable-svc"
        Actions = @("AP_INVOICE_CREATE", "AP_INVOICE_VALIDATE", "AP_INVOICE_APPROVE", "AP_PAYMENT_REQUEST")
    },
    @{
        # general-ledger-svc gates each Tri-Phase Commit hop on its own action,
        # plus reversal. Holding one does not imply the next -- that separation
        # is the point, and it means all four are needed to walk a journal from
        # a draft to the books and back off them again. Its reads are NOT
        # authorized (they are scoped by the verified X-Tenant-Id instead), so
        # unlike spend-controls there is no VIEW action to grant.
        Code    = "GL_FULL"
        Service = "general-ledger-svc"
        Actions = @("GL_JOURNAL_CREATE", "GL_JOURNAL_VALIDATE", "GL_JOURNAL_POST", "GL_JOURNAL_REVERSE")
    },
    @{
        # accounts-receivable-svc had NO bundle at all, so all four of its write
        # routes answered 403 to every principal -- the fourth time this shape
        # has turned up, after jurisdiction-rules-svc, delegated-authority-svc
        # and document-vault-svc. The service has authorized every write since it
        # was written; nothing had ever granted it, which is why the console's
        # invoice form could only ever have appeared to work (and did appear to,
        # because the frontend substituted mock data for the refusal).
        #
        # Each lifecycle hop is its own action, as in AP_FULL and GL_FULL:
        # issuing an invoice, sending it, declaring it late and recording the
        # money against it are four different authorities. Its two READ routes
        # are deliberately not authorized -- they are scoped by the verified
        # X-Tenant-Id instead, the same posture as general-ledger-svc (whose
        # journals this service verifies a payment against) and
        # bank-reconciliation-svc -- so there is no VIEW action to grant.
        Code    = "AR_FULL"
        Service = "accounts-receivable-svc"
        Actions = @("AR_INVOICE_ISSUE", "AR_INVOICE_SEND", "AR_MARK_OVERDUE", "AR_PAYMENT_RECEIVE")
    },
    @{
        # AR-01 Customer Financial Profile (ZS-SVC-C-001). Maintaining a profile
        # and approving an out-of-policy credit-limit increase are separate roles
        # on purpose: the service refuses self-approval (SEGREGATION_OF_DUTIES), and
        # a role holding both would let one person staff both sides in a demo.
        Code    = "AR01_PROFILE_MANAGER"
        Service = "accounts-receivable-svc"
        Actions = @("AR01_PROFILE_MANAGE", "AR01_CREDIT_LIMIT_MANAGE", "AR01_CREDIT_HOLD_MANAGE")
    },
    @{
        Code    = "AR01_CREDIT_APPROVER"
        Service = "accounts-receivable-svc"
        Actions = @("AR01_CREDIT_LIMIT_APPROVE")
    },
    @{
        # AR-02 Quote / Estimate. Preparing, approving and converting are separate
        # roles; the service refuses a discount above 10% approved by its preparer.
        Code    = "AR02_QUOTE_PREPARER"
        Service = "accounts-receivable-svc"
        Actions = @("AR02_QUOTE_MANAGE", "AR02_QUOTE_CONVERT")
    },
    @{
        Code    = "AR02_QUOTE_APPROVER"
        Service = "accounts-receivable-svc"
        Actions = @("AR02_QUOTE_APPROVE", "AR02_QUOTE_DISCOUNT_APPROVE")
    },
    @{
        # AR-03 Sales Order. Managing, approving and the credit-exception authority
        # are separate roles; the service refuses self-approval above thresholds and
        # release of a CREDIT hold by the principal who needs it released.
        Code    = "AR03_ORDER_MANAGER"
        Service = "accounts-receivable-svc"
        Actions = @("AR03_ORDER_MANAGE", "AR03_ORDER_CONFIRM", "AR03_ORDER_HOLD", "AR03_ORDER_CANCEL")
    },
    @{
        Code    = "AR03_ORDER_APPROVER"
        Service = "accounts-receivable-svc"
        Actions = @("AR03_ORDER_APPROVE", "AR03_ORDER_CREDIT_EXCEPTION_APPROVE")
    },
    @{
        # AR-04 Customer Invoice. Drafting/issuing, approval and legal-cancellation
        # decisions are separate roles: the service refuses an approver who is the
        # creator or submitter, and the issuer approving a material invoice.
        Code    = "AR04_INVOICE_BILLER"
        Service = "accounts-receivable-svc"
        Actions = @("AR04_INVOICE_MANAGE", "AR04_INVOICE_ISSUE", "AR04_INVOICE_CANCEL_REQUEST",
                    "AR04_INVOICE_CORRECTION_LINK", "AR04_INVOICE_DELIVERY_RECORD")
    },
    @{
        Code    = "AR04_INVOICE_APPROVER"
        Service = "accounts-receivable-svc"
        Actions = @("AR04_INVOICE_APPROVE", "AR04_INVOICE_CANCEL_APPROVE")
    },
    @{
        # AR-05 Electronic Invoice. Submitting, reconciling against the provider and
        # the manual-evidence admin path are separate: the issuer needs no provider
        # permission, and the principal who started an attempt cannot resolve it by
        # manual evidence.
        Code    = "AR05_EINVOICE_OPERATOR"
        Service = "accounts-receivable-svc"
        Actions = @("AR05_EINVOICE_SUBMIT", "AR05_EINVOICE_RECONCILE")
    },
    @{
        Code    = "AR05_ADAPTER_ADMIN"
        Service = "accounts-receivable-svc"
        Actions = @("AR05_ADAPTER_ADMIN", "AR05_EINVOICE_RECONCILE")
    },
    @{
        # AR-07 Receivables. Disputes/holds are operational; a privileged balance
        # correction is a separate authority. No role can set a balance directly.
        Code    = "AR07_RECEIVABLES_OPERATOR"
        Service = "accounts-receivable-svc"
        Actions = @("AR07_DISPUTE_MANAGE", "AR07_HOLD_MANAGE", "AR07_RECEIVABLE_MANAGE")
    },
    @{
        Code    = "AR07_RECEIVABLES_CORRECTOR"
        Service = "accounts-receivable-svc"
        Actions = @("AR07_RECEIVABLE_ADJUST")
    },
    @{
        # AR-06 Credit / Debit Note. Preparing, approving and issuing are separate
        # roles; the service refuses an approver who is the creator or submitter.
        Code    = "AR06_CORRECTION_PREPARER"
        Service = "accounts-receivable-svc"
        Actions = @("AR06_CORRECTION_MANAGE", "AR06_CORRECTION_ISSUE", "AR06_CORRECTION_LINK")
    },
    @{
        Code    = "AR06_CORRECTION_APPROVER"
        Service = "accounts-receivable-svc"
        Actions = @("AR06_CORRECTION_APPROVE")
    },
    @{
        # AR-08 Cash Application. Registering/applying cash, approving a manual
        # match and unapplying are separate authorities; the service refuses the
        # proposer or registrant as approver and the applier as unapplier.
        Code    = "AR08_CASH_APPLIER"
        Service = "accounts-receivable-svc"
        Actions = @("AR08_RECEIPT_REGISTER", "AR08_APPLICATION_MANAGE")
    },
    @{
        Code    = "AR08_CASH_APPROVER"
        Service = "accounts-receivable-svc"
        Actions = @("AR08_MANUALMATCH_APPROVE", "AR08_UNAPPLY", "AR08_REAPPLY")
    },
    @{
        # AR-10 Refund / Write-Off. Requesting, approving, submitting for payment,
        # tax review and committing are separate authorities; write-off approval is
        # tiered (T1 <= 500, T2 <= 5,000, T3 <= 50,000, T4 unlimited) and the service
        # refuses the requester, dispute resolver or collector as approver.
        Code    = "AR10_REFUND_REQUESTER"
        Service = "accounts-receivable-svc"
        Actions = @("AR10_REFUND_CREATE", "AR10_REFUND_SUBMIT", "AR10_REFUND_RECONCILE", "AR10_WRITEOFF_CREATE")
    },
    @{
        # AR-09 Collections & Dunning. No role here carries any write-off, refund,
        # credit or approval authority: collections never changes a balance.
        Code    = "AR09_COLLECTOR"
        Service = "accounts-receivable-svc"
        Actions = @("AR09_COLLECTIONS_MANAGE", "AR09_DUNNING_SEND", "AR09_HOLD_MANAGE")
    },
    @{
        # AR-to-GL controls. Running a reconciliation and acknowledging its findings
        # are separate: the principal who ran it cannot acknowledge its findings, and
        # an acknowledgement only annotates (it never clears) a finding.
        Code    = "AR_CTL_RECONCILER"
        Service = "accounts-receivable-svc"
        Actions = @("AR_CTL_RUN_RECONCILIATION")
    },
    @{
        Code    = "AR_CTL_REVIEWER"
        Service = "accounts-receivable-svc"
        Actions = @("AR_CTL_ACKNOWLEDGE_FINDING")
    },
    @{
        Code    = "AR09_COLLECTIONS_LEAD"
        Service = "accounts-receivable-svc"
        Actions = @("AR09_COLLECTIONS_ESCALATE", "AR09_POLICY_MANAGE")
    },
    @{
        Code    = "AR10_REFUND_APPROVER"
        Service = "accounts-receivable-svc"
        Actions = @("AR10_REFUND_APPROVE")
    },
    @{
        Code    = "AR10_WRITEOFF_APPROVER_T1"
        Service = "accounts-receivable-svc"
        Actions = @("AR10_WRITEOFF_APPROVE_T1", "AR10_WRITEOFF_COMMIT")
    },
    @{
        Code    = "AR10_WRITEOFF_APPROVER_T4"
        Service = "accounts-receivable-svc"
        Actions = @("AR10_WRITEOFF_APPROVE_T1", "AR10_WRITEOFF_APPROVE_T2", "AR10_WRITEOFF_APPROVE_T3",
                    "AR10_WRITEOFF_APPROVE_T4", "AR10_WRITEOFF_TAX_REVIEW", "AR10_WRITEOFF_COMMIT")
    },
    @{
        # bank-reconciliation-svc gates ingest, match, exception and statement
        # completion separately. Its two READ routes are not authorized at all
        # -- they are scoped by the verified X-Tenant-Id instead, the same
        # posture as general-ledger-svc, whose journals this service
        # reconciles against -- so there is no VIEW action to grant.
        Code    = "BANKREC_FULL"
        Service = "bank-reconciliation-svc"
        Actions = @("BANKREC_STATEMENT_INGEST", "BANKREC_MATCH", "BANKREC_FLAG_EXCEPTION", "BANKREC_COMPLETE_STATEMENT")
    },
    @{
        # financial-close-svc separates registering a period from reading the
        # register and from initiating a close. VIEW is needed for the console's
        # register AND for the readiness check, which is a read: without it the
        # period register answers 403 rather than an empty list.
        Code    = "CLOSE_FULL"
        Service = "financial-close-svc"
        Actions = @("PERIOD_CLOSE_CONFIG", "PERIOD_CLOSE_VIEW", "PERIOD_CLOSE_INITIATE")
    },
    @{
        # financial-control-svc (ZS-CONTROL-001). DEFINE / APPROVE_RULE / EXECUTE / CERTIFY /
        # EXCEPTION_WAIVE / REPERFORM / REVOKE are deliberately separate actions: a real tenant
        # should grant them through separate roles. The service itself also refuses a certifier
        # who created or took part in a run, a waiver from the exception's own owner, and a
        # revoker who certified the run being revoked, whatever is granted here.
        Code    = "FINCTRL_FULL"
        Service = "financial-control-svc"
        Actions = @("FINCTRL_DEFINE", "FINCTRL_APPROVE_RULE", "FINCTRL_POLICY_MANAGE", "FINCTRL_RUN_CREATE",
                    "FINCTRL_READ", "FINCTRL_EXECUTE", "FINCTRL_EXCEPTION_ASSIGN", "FINCTRL_CERTIFY",
                    "FINCTRL_EXCEPTION_RESOLVE", "FINCTRL_EXCEPTION_WAIVE", "FINCTRL_REPERFORM",
                    "FINCTRL_EVIDENCE_EXPORT", "FINCTRL_REVOKE")
    },
    @{
        # The source services authorize the HUMAN caller of a control population (the control service
        # forwards the caller's identity), so whoever executes a control needs the read action of every
        # source that control reads. One action per source service, exactly as their handlers declare.
        Code    = "FINCTRL_POPULATION_READ"
        Service = "financial-control-svc"
        Actions = @("AR_CONTROL_POPULATION_READ", "AP_CONTROL_POPULATION_READ", "GL_CONTROL_POPULATION_READ",
                    "BANKING_CONTROL_POPULATION_READ", "PAYROLL_CONTROL_POPULATION_READ",
                    "INVENTORY_CONTROL_POPULATION_READ", "INTERCOMPANY_CONTROL_POPULATION_READ",
                    "CONSOLIDATION_CONTROL_POPULATION_READ", "FINANCIAL_CLOSE_CONTROL_POPULATION_READ",
                    "PAYEE_BANKING_CONTROL_POPULATION_READ", "TAX_AUTHORITY_CONTROL_POPULATION_READ",
                    "PAYMENT_RUN_CONTROL_POPULATION_READ", "TREASURY_CONTROL_POPULATION_READ",
                    "PAYMENT_INITIATION_CONTROL_POPULATION_READ", "DATA_INGESTION_CONTROL_POPULATION_READ")
    },
    @{
        # reconciliation-engine-svc (DATA-07, ZS-SVC-Z-001 DQC-03). DEFINE/RUN/MATCH stay under
        # MANAGE as preparation steps; CERTIFY, ACCEPT_RISK, APPROVE_RULES and REVOKE are
        # deliberately separate actions, the same posture as FINCTRL_FULL above — a real tenant
        # should grant them through separate roles. The service itself also refuses a certifier
        # who prepared the run, a revoker who certified it, and a self-accepted risk, whatever is
        # granted here. None of these five actions had a permission_definitions row before
        # migration 000008 (MANAGE/READ) and 000018 (the other four) — without that row no role
        # can hold the action at all.
        Code    = "RECONCILIATION_FULL"
        Service = "reconciliation-engine-svc"
        Actions = @("RECONCILIATION_MANAGE", "RECONCILIATION_READ", "RECONCILIATION_CERTIFY",
                    "RECONCILIATION_ACCEPT_RISK", "RECONCILIATION_APPROVE_RULES", "RECONCILIATION_REVOKE")
    },
    @{
        # spend-controls-svc separates setting a limit from spending against it,
        # and checks VIEW on its two read routes as well -- the reads are
        # authorized unconditionally, so without VIEW the console's registers
        # answer 403 rather than an empty list.
        Code    = "SPEND_FULL"
        Service = "spend-controls-svc"
        Actions = @("SPEND_POLICY_MANAGE", "SPEND_POLICY_VIEW", "SPEND_CHECK_SUBMIT")
    },
    @{
        # vendor-due-diligence-svc separates starting a screening from reading the
        # register, and its list route is authorized unconditionally -- it used to
        # skip the check entirely when legal_entity_id was omitted, so without VIEW
        # the console's register would have answered 403 rather than an empty list.
        # VENDOR_DD_VIEW is also needed on the TENANT scope, below, because the
        # unfiltered register falls back to the tenant as its scope.
        Code    = "VENDOR_DD_FULL"
        Service = "vendor-due-diligence-svc"
        Actions = @("VENDOR_DD_INITIATE", "VENDOR_DD_VIEW")
    },
    @{
        # governance-decision-log-svc authorizes every decision write against
        # authzPlatformScopeID -- decisions are platform records, not
        # entity-scoped ones -- so this grant is checked on the platform scope
        # below, never on the legal entity or tenant.
        Code    = "GOVERNANCE_FULL"
        Service = "governance-decision-log-svc"
        Actions = @("GOVERNANCE_DECISION_RECORD")
    },
    @{
        # policy-svc checks its actions against the legal entity when the
        # policy is entity-scoped and against authzPlatformScopeID when it is
        # not, so the platform-scope assignment below carries them too. The
        # GOV-05 lifecycle (14 Oct 2026) added review, approval, suspension,
        # retirement, the _GLOBAL variants, reads and evaluation. Both demo
        # principals hold this role; policy-svc itself refuses the author
        # approving or activating, and the suspender reviewing, so the
        # maker-checker paths stay testable with $PRINCIPAL_ID and $APPROVER_ID.
        Code    = "POLICY_FULL"
        Service = "policy-svc"
        Actions = @("POLICY_CREATE", "POLICY_CREATE_GLOBAL", "POLICY_READ", "POLICY_EVALUATE",
                    "POLICY_VERSION_CREATE", "POLICY_VERSION_CREATE_GLOBAL",
                    "POLICY_VERSION_APPROVE", "POLICY_VERSION_APPROVE_GLOBAL",
                    "POLICY_VERSION_ACTIVATE", "POLICY_VERSION_ACTIVATE_GLOBAL",
                    "POLICY_VERSION_SUSPEND", "POLICY_VERSION_SUSPEND_GLOBAL",
                    "POLICY_VERSION_RETIRE", "POLICY_VERSION_RETIRE_GLOBAL")
    },
    @{
        # policy-svc's control tests and attestations (doc7 E3/E6) authorize
        # against the platform scope.
        Code    = "CONTROL_TESTS_FULL"
        Service = "policy-svc"
        Actions = @("CONTROL_TEST_DEFINITION_CREATE", "CONTROL_TEST_DEFINITION_READ",
                    "CONTROL_TEST_EXECUTION_RECORD", "CONTROL_TEST_EXECUTION_READ",
                    "CONTROL_EFFECTIVENESS_READ", "ATTESTATION_CREATE", "ATTESTATION_READ",
                    "ATTESTATION_REVOKE")
    },
    @{
        # configuration-feature-flag-svc authorizes config and flag writes
        # against authzPlatformScopeID -- a config value or flag shapes
        # platform behaviour, so it is a platform action, not an entity one.
        #
        # FOUR actions, not two. A config entry or flag written with no
        # tenant_id is the environment-wide DEFAULT: it applies to every tenant
        # that has not set its own value. Writing one and writing your own
        # organisation's value are not the same act, and used to be the same
        # grant -- so a principal provisioned to manage one organisation could
        # change what every other organisation reads. RLS cannot catch it
        # either: a global row genuinely belongs to no tenant, so the policy's
        # WITH CHECK admits a NULL tenant_id unconditionally.
        #
        # Both are in this bundle because this seeds a DEVELOPMENT stack,
        # exactly as RETENTION_FULL pairs create and release -- in a real
        # deployment "change my organisation's settings" and "change the
        # default for everyone" are the pair you would want held by different
        # people.
        Code    = "CONFIG_FULL"
        Service = "configuration-feature-flag-svc"
        Actions = @(
            "CONFIGURATION_WRITE", "CONFIGURATION_GLOBAL_WRITE",
            "FEATURE_FLAG_WRITE", "FEATURE_FLAG_GLOBAL_WRITE"
        )
    },
    @{
        # notification-svc authorizes sends against the target legal entity
        # and views against the legal entity being queried, so the console
        # grants these on the legal entity scope.
        #
        # The ZS-SVC-Y-001 control plane (30 Sep 2026) also authorizes intent
        # and template authorship (TEMPLATE_MANAGE), their activation and
        # publication (TEMPLATE_APPROVE), suppression and circuit/stream
        # controls (NOTIFICATION_SUPPRESS) and UNKNOWN reconciliation /
        # operator evidence (NOTIFICATION_RESOLVE_OUTCOME). Without them every
        # NCD authoring or approval call is a 403 on this stack. Maker-checker
        # is enforced by the service (author != approver), not by withholding
        # a grant, so one development bundle can hold both halves.
        Code    = "NOTIFICATION_FULL"
        Service = "notification-svc"
        Actions = @(
            "NOTIFICATION_SEND", "NOTIFICATION_VIEW", "TEMPLATE_MANAGE", "TEMPLATE_APPROVE",
            "NOTIFICATION_SUPPRESS", "NOTIFICATION_RESOLVE_OUTCOME"
        )
    },
    @{
        # board-resolutions-svc authorizes meeting creation against the
        # meeting's legal entity and resolution create / vote / pass against
        # the resolution's own legal entity, so the console grants these on
        # the legal entity scope.
        Code    = "BOARD_FULL"
        Service = "board-resolutions-svc"
        Actions = @("MEETING_CREATE", "RESOLUTION_CREATE", "RESOLUTION_VOTE", "RESOLUTION_PASS")
    },
    @{
        # secret-vault-integration-svc authorizes every mutation against
        # authzPlatformScopeID (its handler falls back to the platform scope
        # when a request carries no legal_entity_id, which is the console's
        # normal posture). Holds the policy, material, lease and rotate grants.
        Code    = "VAULT_FULL"
        Service = "secret-vault-integration-svc"
        Actions = @(
            "SECRET_POLICY_CREATE", "SECRET_POLICY_VERSION_CREATE", "SECRET_POLICY_VERSION_ACTIVATE",
            "SECRET_MATERIAL_WRITE", "SECRET_LEASE_REVOKE", "SECRET_ROTATE"
        )
    },
    @{
        # contract-lifecycle-svc authorizes each lifecycle transition against
        # the contract's own legal entity: CONTRACT_CREATE on draft, then a
        # distinct grant per transition. The FE comment that once claimed this
        # service skipped authorization was stale -- every mutation goes
        # through authorization-svc and fails closed on a denial.
        Code    = "CONTRACT_FULL"
        Service = "contract-lifecycle-svc"
        Actions = @(
            "CONTRACT_CREATE", "CONTRACT_UPDATE", "CONTRACT_SUBMIT_FOR_APPROVAL",
            "CONTRACT_ACTIVATE", "CONTRACT_TERMINATE"
        )
    },
    @{
        # schema-registry-svc gates every schema registration on SCHEMA_PUBLISH.
        #
        # This bundle was MISSING, and the console's "Register a version" form
        # worked anyway on this machine only by accident: a hand-made bundle
        # (SRB on role SR_1255) had been left in the dev database by some
        # earlier ad-hoc seeding. On a fresh volume, in CI, or on anyone else's
        # machine, nothing granted SCHEMA_PUBLISH and every registration was a
        # 403 -- the whole write path dead, with the page still rendering
        # normally. A grant that exists only in one developer's database is
        # indistinguishable from a working feature right up until someone else
        # tries it.
        #
        # Event contracts are platform-wide reference data with no legal entity
        # of their own, so the service authorizes against
        # AUTHZ_PLATFORM_SCOPE_ID when a request carries no legal entity --
        # the same synthetic scope jurisdiction-rules-svc uses. Both scopes are
        # granted below for that reason.
        Code    = "SCHEMA_FULL"
        Service = "schema-registry-svc"
        Actions = @("SCHEMA_PUBLISH")
    },
    @{
        # jurisdiction-rules-svc gates all five of its admin routes. Like
        # schema-registry-svc's, this bundle was missing entirely, so the whole
        # write surface was 403 for every principal -- and unlike that one there
        # was not even a stray hand-made grant making it appear to work. This
        # was recorded as an open gap when the service was hardened on 5 Aug and
        # is only now closed.
        #
        # Jurisdictions are platform-wide reference data: "GB" is not owned by a
        # legal entity. The service authorizes every mutation against
        # AUTHZ_PLATFORM_SCOPE_ID (it has no per-entity scope at all and
        # requires that variable at startup), so these are verified on the
        # platform scope below.
        #
        # The action strings are derived by the service as
        # upper(resource + "_" + action) -- see internal/authz ActionType -- so
        # they must match the resource/action pairs its handlers pass, not a
        # naming convention invented here.
        # obligations-svc gates its three mutations against the obligation's
        # own LEGAL ENTITY -- the authority to raise or close a statutory
        # obligation is entity-scoped, not platform-wide, so this bundle is
        # verified on the legal entity rather than the platform scope.
        #
        # There was no bundle at all because there was no authorization at all:
        # the service shipped with a documented deferral ("Authorization
        # Service ... doesn't exist") that had gone stale, leaving an open
        # write surface on a compliance register.
        #
        # APPLICABILITY_DECISION_RECORD was added when the applicability write
        # was gated. That route had been the one ungated write left in the
        # service: its file's header still claimed obligations-svc had no authz
        # client, which had been true when it was written and was not true after
        # the other three were gated. Recording whether an obligation APPLIES is
        # the strongest of the four -- filings, evidence and aging are all
        # derived from that answer -- so it is a distinct action, not folded into
        # OBLIGATION_CREATE.
        Code    = "OBLIGATION_FULL"
        Service = "obligations-svc"
        Actions = @("OBLIGATION_CREATE", "OBLIGATION_STATUS_UPDATE", "FILING_REQUIREMENT_CREATE",
                    "APPLICABILITY_DECISION_RECORD")
    },
    @{
        # delegated-authority-svc had NO bundle at all, so every one of its
        # four routes answered 403 to every principal -- the same shape as
        # jurisdiction-rules-svc before 17 Aug. The service has enforced
        # authorization since it was written; nothing had ever granted it.
        #
        # DELEGATION_ADMINISTER is deliberately NOT in this bundle. It is the
        # authority to create a delegation naming SOMEONE ELSE as delegator,
        # and handing it to the demo principal alongside DELEGATION_CREATE
        # would restore exactly the escalation this pass closed. It belongs to
        # a delegation administrator, granted separately and deliberately --
        # see BOARD_SMOKE_PASSER for the same "second principal, on purpose"
        # pattern.
        # document-vault-svc had no bundle because it had no authorization: every
        # route, including the download, answered anything that reached the port.
        #
        # DOWNLOAD is granted here alongside READ because the demo console needs
        # to fetch content, but note they are DIFFERENT actions on purpose --
        # knowing a document exists and reading its bytes are different
        # disclosures, and a real deployment should be able to grant one without
        # the other. ACCESS_LOG_READ is likewise separate: it is the record an
        # investigator consults.
        Code    = "DOCUMENT_FULL"
        Service = "document-vault-svc"
        Actions = @("DOCUMENT_CREATE", "DOCUMENT_READ", "DOCUMENT_DOWNLOAD",
            "DOCUMENT_VERSION_CREATE", "DOCUMENT_ACCESS_LOG_READ")
    },
    @{
        Code    = "DELEGATION_FULL"
        Service = "delegated-authority-svc"
        Actions = @("DELEGATION_CREATE", "DELEGATION_VIEW", "DELEGATION_REVOKE")
    },
    @{
        # SUPERSEDE is separate from CREATE on purpose, and the split is the
        # whole point of the pair. Recording a new precedence rule adds a
        # ranking; ending one CHANGES WHICH SOURCE THE PLATFORM BELIEVES for a
        # field family, silently and everywhere, from the moment it takes
        # effect. Someone who may propose a ranking should not automatically be
        # able to switch the platform's system of record.
        #
        # VIEW is separate from both because the register is the platform's
        # trust topology -- which connected systems are believed over which --
        # and reading it is a disclosure even though the rows belong to no
        # tenant. It ran no authorization at all until this pass.
        #
        # NORMALIZED_FACT_VIEW is separate again: unlike the rules, facts are
        # tenant business data and the route returns raw fact values.
        Code    = "SOURCE_AUTHORITY_FULL"
        Service = "source-authority-svc"
        Actions = @(
            "SOURCE_AUTHORITY_MAP_CREATE", "SOURCE_AUTHORITY_MAP_VIEW",
            "SOURCE_AUTHORITY_MAP_SUPERSEDE",
            "NORMALIZED_FACT_RECORD", "NORMALIZED_FACT_VIEW"
        )
    },
    @{
        Code    = "JURISDICTION_FULL"
        Service = "jurisdiction-rules-svc"
        Actions = @(
            "JURISDICTION_CREATE", "JURISDICTION_DEACTIVATE",
            "JURISDICTION_RULE_CREATE", "JURISDICTION_RULE_TRANSITION",
            "JURISDICTION_RULE_RECORD_DRIFT"
        )
    },
    @{
        # ZS-JUR-001 Wave 2 runtime resolver: what a CALLING service (tax
        # determination, invoicing, filing) needs. Deliberately separate from
        # the registry/admin bundle: a caller can resolve a rule and read its
        # decision evidence, and cannot author, sign or certify anything.
        Code    = "JURISDICTION_RESOLVER_CALLER"
        Service = "jurisdiction-rules-svc"
        Actions = @(
            "RULE_RESOLUTION_RESOLVE", "RULE_DECISION_VIEW",
            "OBLIGATION_CALCULATION_CALCULATE", "REGULATORY_CALCULATION_EXECUTE", "PAYROLL_PARAMETERS_RESOLVE", "RETENTION_RULE_RESOLVE", "ACCOUNTING_MAPPING_RESOLVE", "SUBMISSION_PROFILE_RESOLVE", "REGULATORY_SUBMISSION_CREATE", "REGULATORY_SUBMISSION_VIEW", "REGULATORY_SUBMISSION_RECORD_EVENT", "JURISDICTION_SUPPORT_VIEW"
        )
    },
    @{
        # ZS-JUR-001 Wave 0 registries. The demo bundle grants every action to
        # one role; real tenants must split authoring (create), independent
        # review/approval (REVIEW, APPROVE) and pack submission across roles.
        # The service additionally refuses a reviewer/approver who authored
        # the record, so one demo principal can create but not self-approve.
        Code    = "JURISDICTION_PACK_REGISTRY_FULL"
        Service = "jurisdiction-rules-svc"
        Actions = @(
            "REGULATORY_REGIME_CREATE",
            "REGULATORY_SOURCE_CREATE", "REGULATORY_SOURCE_REVIEW", "REGULATORY_SOURCE_SUPERSEDE",
            "INTERPRETATION_RECORD_CREATE", "INTERPRETATION_RECORD_APPROVE",
            "JURISDICTION_PACK_CREATE",
            "JURISDICTION_PACK_VERSION_CREATE", "JURISDICTION_PACK_VERSION_SUBMIT",
            # Wave 1: compile, sign and verify are separate capabilities, as are
            # registering/retiring/revoking trusted signing keys. Grant them to
            # different roles in real environments.
            "JURISDICTION_PACK_VERSION_COMPILE", "JURISDICTION_PACK_VERSION_SIGN", "JURISDICTION_PACK_VERSION_VERIFY",
            "PACK_SIGNING_KEY_REGISTER", "PACK_SIGNING_KEY_RETIRE", "PACK_SIGNING_KEY_REVOKE",
            # Wave 3: test authoring, test execution, independent review and
            # certification are separate capabilities. The service also refuses
            # a reviewer/certifier who built the version, so even this single
            # demo role cannot certify its own work with one principal.
            "JURISDICTION_PACK_VERSION_AUTHOR_TESTS", "JURISDICTION_PACK_VERSION_EXECUTE_TESTS",
            "JURISDICTION_PACK_VERSION_REVIEW", "JURISDICTION_PACK_VERSION_CERTIFY",
            # Wave 2: dropping the resolver's verified-artifact cache is an
            # operator action (after a key revocation or a withdrawal).
            "RESOLVER_CACHE_INVALIDATE",
            # Wave 7: release and rollout are separate capabilities, as is the
            # emergency stop and hotfix declaration. Grant publish, deploy,
            # rollback and block/unblock to different roles in real
            # environments; the demo bundle gives one role all of them.
            "JURISDICTION_PACK_VERSION_PUBLISH", "JURISDICTION_PACK_VERSION_WITHDRAW",
            "JURISDICTION_PACK_VERSION_BLOCK", "JURISDICTION_PACK_VERSION_UNBLOCK",
            "JURISDICTION_PACK_VERSION_DEPLOY", "JURISDICTION_PACK_VERSION_ROLLBACK",
            "JURISDICTION_PACK_VERSION_DECLARE_HOTFIX", "JURISDICTION_PACK_VERSION_COMPLETE_RETROSPECTIVE",
            "DEPLOYMENT_REGION_CREATE", "DEPLOYMENT_RING_CREATE",
            "SOURCE_CHANGE_NOTICE_CREATE", "SOURCE_CHANGE_NOTICE_REVIEW", "SOURCE_CHANGE_NOTICE_CLOSE",
            "PACK_OPS_METRICS_VIEW",
            # Wave 4 (calendar half): authoring, sourcing and publishing of
            # calendars and obligation rules are separate capabilities, and the
            # service refuses a publisher who authored the same record.
            "REGULATORY_CALENDAR_CREATE", "REGULATORY_CALENDAR_VERSION_CREATE",
            "REGULATORY_CALENDAR_VERSION_SET_SOURCES", "REGULATORY_CALENDAR_VERSION_PUBLISH",
            "OBLIGATION_RULE_CREATE", "OBLIGATION_RULE_SET_PROVENANCE", "OBLIGATION_RULE_PUBLISH",
            # Wave 4 (tax half): typed calculation parameters (rates, bands, rounding).
            "JURISDICTION_RULE_SET_PARAMETERS",
            # Wave 8: jurisdiction rollout governance (portfolio, readiness evidence, expert approval, launch).
            "JURISDICTION_ROLLOUT_CREATE", "JURISDICTION_ROLLOUT_SEED", "JURISDICTION_ROLLOUT_VIEW", "JURISDICTION_ROLLOUT_SET_OWNER", "JURISDICTION_ROLLOUT_LINK_PACKS",
            "JURISDICTION_ROLLOUT_ATTEST", "JURISDICTION_ROLLOUT_EXPERT_APPROVE", "JURISDICTION_ROLLOUT_TRANSITION", "JURISDICTION_SUPPORT_VIEW",
            "JURISDICTION_RULE_SET_PROVENANCE"
        )
    },
    @{
        # access-control-svc is the governed authoring layer in front of
        # authorization-svc's own admin API. Without this grant the /admin/
        # access-control page reads fine and every write 403s, which looks like
        # a broken page rather than an ungranted action.
        #
        # Entity-scoped, not platform: the service authorizes ROLE_MANAGE
        # against the legal_entity_id in the request body.
        Code    = "ACCESS_CONTROL_FULL"
        Service = "access-control-svc"
        Actions = @("ROLE_MANAGE")
    },
    @{
        # authorization-svc admin API. These actions gate the 22 /v1/admin/* routes
        # that create/manage roles, bundles, assignments, SoD rules, ABAC rules,
        # delegations, PAM, break-glass and support sessions. They are
        # PLATFORM-SCOPED: authorization-svc is the platform's authorization
        # engine, so managing its configuration is a platform-wide act.
        #
        # The demo principal gets this on the platform scope assignment below.
        Code    = "IAM_ADMIN_FULL"
        Service = "authorization-svc"
        Actions = @(
            "iam.role.manage",
            "iam.permission_bundle.manage",
            "iam.assignment.grant",
            "iam.assignment.revoke",
            # Reading another principal's grants (entity-scope/validate for
            # someone else) -- the access-control console's audit page.
            "iam.assignment.read",
            # Security approval of a CRITICAL-risk assignment request in
            # access-control-svc (its migration 000013). The demo approver
            # approves the audit's CRITICAL requests, so it holds this.
            "iam.assignment.approve_privileged",
            "iam.sod_rule.manage",
            # Authorization Standard Appendix A, as authorization-svc now gates:
            # the full decision log and its rationale (iam.policy.read), the
            # GOV-03 cache invalidation command (iam.policy.publish), the SoD
            # exception register (iam.sod_rule.read) and approving a GOV-04
            # compensating control (iam.sod_rule.publish).
            "iam.policy.read",
            "iam.policy.publish",
            "iam.sod_rule.read",
            "iam.sod_rule.publish",
            # The admin registers' read permissions (§21 "no broad IAM
            # discovery"): roles and bundles, delegations.
            "iam.role.read",
            "iam.delegation.read",
            "iam.abac_rule.manage",
            "iam.delegation.grant",
            "iam.delegation.revoke",
            "iam.pam.manage",
            "iam.break_glass.manage",
            "iam.support.manage"
        )
    },
    @{
        # retention-registry-svc. LEGAL_HOLD_RELEASE is deliberately in the same
        # bundle as LEGAL_HOLD_CREATE for the demo principal only because this
        # seeds a DEVELOPMENT stack -- in a real deployment engaging a freeze and
        # lifting one are exactly the pair you would want held by different
        # people, since releasing a hold unblocks deletion of records an
        # authority ordered frozen.
        #
        # NOTE ON SCOPE. This service passes the request's tenant_id as the
        # authorization scope rather than a legal_entity_id, and falls back to the
        # platform scope when it is absent. The role below is assigned on both the
        # legal entity AND the tenant, so a tenant-scoped hold is covered without
        # anything special here. A PLATFORM-WIDE hold or policy sends no tenant
        # and so authorizes against the platform scope, which is why this bundle
        # is also listed in $PLATFORM_SCOPED_ACTION_CODES -- the console offers
        # that as a checkbox, so both paths are reachable from the UI.
        Code    = "RETENTION_FULL"
        Service = "retention-registry-svc"
        Actions = @("RETENTION_POLICY_CREATE", "LEGAL_HOLD_CREATE", "LEGAL_HOLD_RELEASE")
    },
    @{
        # search-indexer-svc (ZS-SVC-AB-001 ESR-01..ESR-05). Its CONTROL plane
        # -- registering a source, drafting and publishing a contract, building
        # and activating an index generation -- authorizes against the PLATFORM
        # scope, not a legal entity: a search contract describes a shape shared
        # by every tenant, and a generation is one physical index serving all of
        # them, so a tenant-scoped grant must not be able to change what every
        # other tenant can search.
        #
        # SEARCH_GENERATION_ACTIVATE is a separate action from
        # SEARCH_GENERATION_CREATE on purpose. Building a generation is routine
        # and touches nothing anyone is querying; ACTIVATING one changes what
        # every caller sees in a single atomic alias swap. They are in the same
        # bundle here because this seeds a DEVELOPMENT stack, exactly as
        # RETENTION_FULL pairs create and release -- in a real deployment they
        # are the pair you would want held by different people.
        #
        # SEARCH_EXPORT is in this bundle and is NOT implied by the ability to
        # search. INV-29: "search exports/downloads require separate export
        # authorization; search permission does not imply bulk-exfiltration
        # permission." A principal that can search but holds no SEARCH_EXPORT
        # gets a correct ESR-016 from /v1/search-exports.
        #
        # SEARCH_RESTRICTION_APPLY is the erasure/disposition path PRV and DRC
        # call to remove search visibility for a specific record.
        Code    = "SEARCH_FULL"
        Service = "search-indexer-svc"
        Actions = @(
            "SEARCH_SOURCE_REGISTER", "SEARCH_CONTRACT_CREATE", "SEARCH_CONTRACT_TRANSITION",
            "SEARCH_GENERATION_CREATE", "SEARCH_GENERATION_ACTIVATE",
            "SEARCH_RESTRICTION_APPLY", "SEARCH_EXPORT"
        )
    },
    @{
        # commercial-account-svc's COM-01 price book checks every write
        # against AUTHZ_PLATFORM_SCOPE_ID, not a legal entity -- same
        # posture as governance-decision-log-svc etc. above. Propose,
        # approve and publish are separate actions (COM-CTRL-004 maker-
        # checker: the handler itself refuses an approve/publish from the
        # same principal who proposed, regardless of what this bundle
        # grants -- RBAC only answers "is this principal allowed to call
        # this action at all", never "are these two principals distinct").
        Code    = "COMMERCIAL_PRICE_BOOK_FULL"
        Service = "commercial-account-svc"
        Actions = @(
            "COMMERCIAL_PRICE_BOOK_PROPOSE", "COMMERCIAL_PRICE_BOOK_APPROVE",
            "COMMERCIAL_PRICE_BOOK_PUBLISH", "COMMERCIAL_PRICE_BOOK_RETIRE",
            "COMMERCIAL_PRICE_BOOK_READ",
            # Currency registration is a prerequisite seller-authority action
            # for creating any price version at all (pricebook_handler.go's
            # PutCurrency route) -- same platform scope as the price-book
            # actions above.
            "COMMERCIAL_CURRENCY_MANAGE"
        )
    }
)

$ALL_ACTIONS = $BUNDLES | ForEach-Object { $_.Actions } | Sort-Object -Unique

# Actions whose services authorize against AUTHZ_PLATFORM_SCOPE_ID rather than
# a legal entity: governance-decision-log, configuration-feature-flag,
# secret-vault, the non-entity-scoped policy paths, and schema-registry (an
# event contract belongs to the platform, not to a legal entity).
#
# Defined here rather than beside the final verification because the "is there
# anything to do" probe below needs it too. That probe used to look only at the
# legal entity, which is the very failure this script's header warns about one
# level down: a platform-scoped action added later could already be granted on
# the legal entity by some earlier seeding, the probe would find nothing
# missing, and the run would skip the bundles entirely -- never granting the
# platform scope. It then reported success and had granted nothing.
$PLATFORM_SCOPED_ACTION_CODES = @(
    "GOVERNANCE_FULL", "CONFIG_FULL", "VAULT_FULL", "POLICY_FULL", "CONTROL_TESTS_FULL",
    "SCHEMA_FULL", "JURISDICTION_FULL", "JURISDICTION_PACK_REGISTRY_FULL", "JURISDICTION_RESOLVER_CALLER",
    # retention-registry-svc falls back to the platform scope when a hold or
    # policy names no tenant -- the console offers that as a "platform-wide"
    # checkbox, so the grant has to exist on that scope too.
    "RETENTION_FULL",
    # search-indexer-svc's whole control plane authorizes against the platform
    # scope. A grant made only on the legal entity would be invisible to every
    # one of its checks -- silently, and fail-closed, so it would read as
    # "no_grant" rather than as a scope mismatch.
    "SEARCH_FULL",
    # authorization-svc admin API manages the platform's authorization engine
    "IAM_ADMIN_FULL",
    # commercial-account-svc's COM-01 price book (propose/approve/publish/
    # retire/read) checks every write against platformScopeID -- see
    # handler.go's platformScopeID constant and pricebook_handler.go's
    # h.authz.CheckAllowed(ctx, principal, platformScopeID, action) call.
    "COMMERCIAL_PRICE_BOOK_FULL")
$PLATFORM_SCOPED_ACTIONS = $BUNDLES |
    Where-Object { $PLATFORM_SCOPED_ACTION_CODES -contains $_.Code } |
    ForEach-Object { $_.Actions }

if ($PSCmdlet.ParameterSetName -eq "Gateway") {
    $AUTHZ = "$($GatewayUrl.TrimEnd('/'))/authorization-svc"
} else {
    $AUTHZ = $AuthzUrl.TrimEnd('/')
}

# section 4 canonical envelope (ZS-ARCH-SVC-001 section 4). Every request to the service has
# carried these headers since envelope enforcement made admin writes fail
# closed with 401 `envelope_incomplete`; this script predates that and was
# seeding a volume that its own writes could never populate. The header set
# here mirrors what the console and access-control-svc actually send.
#
# Non-writes (/v1/authorize probes) omit Idempotency-Key: the service classifies
# that path as non-material, so it requires no idempotency key and the probe stays
# representative of a real read-path call.
function New-AuthzEnvelope {
    param(
        [Parameter(Mandatory)] [string] $Path,
        [string] $Actor = $PRINCIPAL_ID
    )
    $headers = @{
        "X-Tenant-Id"       = $TENANT_ID
        "X-Principal-Id"    = $Actor
        "X-Legal-Entity-Id" = $LEGAL_ENTITY
        "X-Correlation-ID"  = [guid]::NewGuid().ToString()
        "X-Request-Id"      = [guid]::NewGuid().ToString()
        "X-Source-System"   = "console-seed"
        "X-Source-Channel"  = "api"
    }
    if ($Path -ne "/v1/authorize") {
        $headers["Idempotency-Key"] = [guid]::NewGuid().ToString()
        $headers["X-Occurred-At"]   = [DateTime]::UtcNow.ToString("o")
        $headers["X-Operation"]     = "admin_seed"
    }
    return $headers
}

function Invoke-Authz {
    param(
        [Parameter(Mandatory)] [string] $Path,
        [Parameter(Mandatory)] $Body,
        # Who makes the call. Role assignments to the demo principal are made by
        # the approver: authorization-svc refuses a principal assigning a role to
        # themselves (self_grant_not_allowed, section 10.1 "no self-grant of protected
        # privilege").
        [string] $Actor = $PRINCIPAL_ID
    )
    $json = $Body | ConvertTo-Json -Compress -Depth 5
    $headers = New-AuthzEnvelope -Path $Path -Actor $Actor
    # The legal entity mirrors the body's own scope when the call carries one
    # (Get-Decision probes different scopes per call), falling back to the demo
    # legal entity for admin calls that scope themselves some other way.
    if ($Body.legal_entity_id) { $headers["X-Legal-Entity-Id"] = $Body.legal_entity_id }
    try {
        $response = Invoke-WebRequest -Uri "$AUTHZ$Path" -Method POST -Body $json `
            -ContentType "application/json" -Headers $headers `
            -UseBasicParsing -TimeoutSec 10
        return @{ status = [int] $response.StatusCode; body = $response.Content | ConvertFrom-Json }
    } catch {
        $status = if ($_.Exception.Response) { [int] $_.Exception.Response.StatusCode.value__ } else { 0 }
        # ErrorDetails carries the service's JSON body; Exception.Message is all
        # there is when the failure was at the transport layer. Not using `??`
        # here on purpose -- this has to run under Windows PowerShell 5.1.
        $detail = if ($_.ErrorDetails -and $_.ErrorDetails.Message) { $_.ErrorDetails.Message } else { $_.Exception.Message }
        throw "POST $Path failed ($status): $detail"
    }
}

function Get-Decision {
    param(
        [Parameter(Mandatory)] [string] $Action,
        [Parameter(Mandatory)] [string] $Scope,
        [string] $Principal = $PRINCIPAL_ID
    )
    $result = Invoke-Authz -Path "/v1/authorize" -Body @{
        principal_id    = $Principal
        legal_entity_id = $Scope
        action_type     = $Action
    }
    return $result.body
}

Write-Host "Seeding demo RBAC via $AUTHZ" -ForegroundColor Cyan

# Fail early and clearly if the stack isn't up, rather than mid-chain.
try {
    Invoke-WebRequest -Uri "$AUTHZ/healthz" -UseBasicParsing -TimeoutSec 5 | Out-Null
} catch {
    throw "authorization-svc is not reachable at $AUTHZ.`n" +
          "  Gateway-less:  docker compose -f deployments/docker-compose.yml up -d authorization-svc`n" +
          "  Via gateway:   `$env:GATEWAY_PORT = '8000'; docker compose -f deployments/docker-compose.yml up -d gateway"
}

# Probe EVERY action on EVERY scope it is actually checked on, not just one
# action and not just one scope. A partial grant is the normal state of an
# existing volume once this script gains a service, and it is precisely the case
# the old single-action early return got wrong.
#
# The scope half matters as much as the action half: SCHEMA_PUBLISH was already
# granted on the legal entity by a hand-made bundle left in one developer's
# database, so a legal-entity-only probe found nothing missing, skipped the
# bundles, and never granted the platform scope that schema-registry-svc
# actually authorizes against for an entity-less registration.
$missing = @()
foreach ($action in $ALL_ACTIONS) {
    if ((Get-Decision -Action $action -Scope $LEGAL_ENTITY).decision_outcome -ne "GRANTED") {
        $missing += $action
    }
}
foreach ($action in $PLATFORM_SCOPED_ACTIONS) {
    if ((Get-Decision -Action $action -Scope $PLATFORM_SCOPE).decision_outcome -ne "GRANTED") {
        $missing += "$action (platform scope)"
    }
}

if ($missing.Count -eq 0) {
    Write-Host "Already granted -- $PRINCIPAL_ID holds all $($ALL_ACTIONS.Count) actions on $LEGAL_ENTITY and the platform scope." -ForegroundColor Green
} else {
    Write-Host "$($missing.Count) of $($ALL_ACTIONS.Count) actions missing: $($missing -join ', ')" -ForegroundColor Yellow

    # -- Bootstrap: the admin API is gated by the grants this script creates --
    #
    # Since 30 Sep every /v1/admin/* route requires an iam.* action (iam.role.
    # manage, iam.permission_bundle.manage, ...) held through a TENANT-scope
    # assignment. On a database where nobody holds them yet -- any fresh or
    # rebuilt volume -- the first call below is refused 403 and nothing can ever
    # be granted through the API: the grant that opens the gate can only be
    # made through the gate. This writes the minimum directly into
    # authorization_svc (role, the IAM_ADMIN_FULL bundle, the tenant-scope
    # assignment), idempotently, and the API does the rest. DEVELOPMENT ONLY:
    # a real deployment's first IAM administrator is a governed act, not this.
    # The approver is bootstrapped as well: it makes the demo principal's own
    # role assignments below, which the demo principal may not make itself.
    if ((Get-Decision -Action "iam.role.manage" -Scope $TENANT_ID).decision_outcome -ne "GRANTED" -or
        (Get-Decision -Action "iam.assignment.grant" -Scope $TENANT_ID -Principal $APPROVER_ID).decision_outcome -ne "GRANTED" -or
        (Get-Decision -Action "iam.permission_bundle.manage" -Scope $TENANT_ID -Principal $SEED_ADMIN_ID).decision_outcome -ne "GRANTED" -or
        (Get-Decision -Action "iam.assignment.grant" -Scope $PLATFORM_SCOPE -Principal $SEED_ADMIN_ID).decision_outcome -ne "GRANTED" -or
        (Get-Decision -Action "iam.assignment.revoke" -Scope $TENANT_ID -Principal $ACS_SERVICE_ID).decision_outcome -ne "GRANTED" -or
        (Get-Decision -Action "iam.assignment.read" -Scope $TENANT_ID -Principal $ACS_SERVICE_ID).decision_outcome -ne "GRANTED") {
        Write-Host "0  bootstrap the IAM admin grant directly (the admin API refuses everyone until one exists)" -NoNewline
        $iamActions = ($BUNDLES | Where-Object { $_.Code -eq "IAM_ADMIN_FULL" }).Actions | ConvertTo-Json -Compress
        $sql = @"
INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_by_principal_id)
VALUES ('$ROLE_ID', '$TENANT_ID', '$ROLE_CODE', 'Console Demo Operator', 'LEGAL_ENTITY', true, 'seed-bootstrap')
ON CONFLICT DO NOTHING;
INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions, active_flag)
VALUES ('$ROLE_ID', 'IAM_ADMIN_FULL', '$iamActions'::jsonb, true)
ON CONFLICT (role_id, bundle_code) DO UPDATE SET permitted_actions = EXCLUDED.permitted_actions, active_flag = true;
INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by)
SELECT '$PRINCIPAL_ID', '$ROLE_ID', '$TENANT_ID', now() - interval '1 minute', 'seed-bootstrap'
WHERE NOT EXISTS (SELECT 1 FROM principal_role_assignments
                  WHERE principal_id = '$PRINCIPAL_ID' AND role_id = '$ROLE_ID' AND legal_entity_id = '$TENANT_ID');
INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by)
SELECT '$APPROVER_ID', '$ROLE_ID', '$TENANT_ID', now() - interval '1 minute', 'seed-bootstrap'
WHERE NOT EXISTS (SELECT 1 FROM principal_role_assignments
                  WHERE principal_id = '$APPROVER_ID' AND role_id = '$ROLE_ID' AND legal_entity_id = '$TENANT_ID');
INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_by_principal_id)
VALUES ('$SEED_ADMIN_ROLE_ID', '$TENANT_ID', 'SEED_IAM_ADMIN', 'Seed IAM Administrator', 'LEGAL_ENTITY', true, 'seed-bootstrap')
ON CONFLICT DO NOTHING;
INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions, active_flag)
VALUES ('$SEED_ADMIN_ROLE_ID', 'IAM_ADMIN_FULL', '$iamActions'::jsonb, true)
ON CONFLICT (role_id, bundle_code) DO UPDATE SET permitted_actions = EXCLUDED.permitted_actions, active_flag = true;
INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by)
SELECT '$SEED_ADMIN_ID', '$SEED_ADMIN_ROLE_ID', '$TENANT_ID', now() - interval '1 minute', 'seed-bootstrap'
WHERE NOT EXISTS (SELECT 1 FROM principal_role_assignments
                  WHERE principal_id = '$SEED_ADMIN_ID' AND role_id = '$SEED_ADMIN_ROLE_ID' AND legal_entity_id = '$TENANT_ID');
-- Platform scope too: an assignment on the platform-scope entity can only be
-- made by a holder of iam.assignment.grant AT platform scope.
INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by)
SELECT '$SEED_ADMIN_ID', '$SEED_ADMIN_ROLE_ID', '$PLATFORM_SCOPE', now() - interval '1 minute', 'seed-bootstrap'
WHERE NOT EXISTS (SELECT 1 FROM principal_role_assignments
                  WHERE principal_id = '$SEED_ADMIN_ID' AND role_id = '$SEED_ADMIN_ROLE_ID' AND legal_entity_id = '$PLATFORM_SCOPE');
INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_by_principal_id)
VALUES ('$ACS_SERVICE_ROLE_ID', '$TENANT_ID', 'SVC_ACCESS_CONTROL', 'access-control-svc review executor', 'LEGAL_ENTITY', true, 'seed-bootstrap')
ON CONFLICT DO NOTHING;
INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions, active_flag)
VALUES ('$ACS_SERVICE_ROLE_ID', 'REVIEW_REVOKE', '["iam.assignment.revoke", "iam.assignment.read"]'::jsonb, true)
ON CONFLICT (role_id, bundle_code) DO UPDATE SET permitted_actions = EXCLUDED.permitted_actions, active_flag = true;
INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by)
SELECT '$ACS_SERVICE_ID', '$ACS_SERVICE_ROLE_ID', '$TENANT_ID', now() - interval '1 minute', 'seed-bootstrap'
WHERE NOT EXISTS (SELECT 1 FROM principal_role_assignments
                  WHERE principal_id = '$ACS_SERVICE_ID' AND role_id = '$ACS_SERVICE_ROLE_ID' AND legal_entity_id = '$TENANT_ID');
"@
        $sql | docker exec -i $PgContainer psql -U postgres -d authorization_svc -v ON_ERROR_STOP=1 -q | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "IAM bootstrap into authorization_svc failed (container $PgContainer)" }
        # A write made behind the service's back is not seen by its grant cache
        # until the entry expires (AUTHZ_CACHE_TTL_SECONDS, default 5), and the
        # probe above has just cached "nothing granted".
        Start-Sleep -Seconds 6
        Write-Host "  -> done" -ForegroundColor Green
    }

    # Re-creating an existing role is NOT idempotent, despite what this script
    # used to claim: authorization-svc answers 503 `store_unavailable` (its own
    # instance of the platform-wide habit of reporting a constraint violation as
    # an outage). So a failure here is tolerated and the run continues to the
    # bundles -- the end-of-script verification is the real gate, and it fails
    # loudly if the role genuinely is not there.
    Write-Host "1  role $ROLE_CODE" -NoNewline
    try {
        $role = Invoke-Authz -Path "/v1/admin/roles" -Body @{
            role_id                 = $ROLE_ID
            tenant_id               = $TENANT_ID
            role_code               = $ROLE_CODE
            role_name               = "Console Demo Operator"
            role_scope_type         = "LEGAL_ENTITY"
            created_by_principal_id = $PRINCIPAL_ID
        }
        if ($role.status -eq 200) { $roleNote = "(already existed)" } else { $roleNote = "(created)" }
        Write-Host "  -> $($role.status) $roleNote"
    } catch {
        Write-Host "  -> already exists, or could not be created; continuing to the bundles" -ForegroundColor DarkGray
    }

    $step = 2
    foreach ($bundle in $BUNDLES) {
        Write-Host "$step  bundle $($bundle.Code) for $($bundle.Service)" -NoNewline
        $result = Invoke-Authz -Actor $SEED_ADMIN_ID -Path "/v1/admin/roles/$ROLE_ID/permission-bundles" -Body @{
            bundle_code       = $bundle.Code
            permitted_actions = $bundle.Actions
        }
        Write-Host "  -> $($result.status) [$($result.body.permitted_actions -join ', ')]"
        $step++
    }

    # Two scopes. The legal entity is what most services check; the tenant is
    # what evidence-requirements-svc falls back to when legal_entity_id is
    # omitted, and without it the console's tenant-wide option answers 403.
    foreach ($scope in @(@{ Id = $LEGAL_ENTITY; Label = "legal entity" }, @{ Id = $TENANT_ID; Label = "tenant" })) {
        Write-Host "$step  assign role to $PRINCIPAL_ID on the $($scope.Label)" -NoNewline
        try {
            $assignment = Invoke-Authz -Actor $APPROVER_ID -Path "/v1/admin/role-assignments" -Body @{
                principal_id    = $PRINCIPAL_ID
                role_id         = $ROLE_ID
                legal_entity_id = $scope.Id
                effective_from  = "2020-01-01T00:00:00Z"
                assigned_by     = $APPROVER_ID
            }
            Write-Host "  -> $($assignment.status) $($assignment.body.principal_role_assignment_id)"
        } catch {
            # The assignment insert has no upsert, so re-running against a volume
            # that already holds one row fails on the primary key. That is a
            # no-op, not a failure -- the grant it would have created exists.
            if ("$_" -match "409|23505|duplicate|already") {
                Write-Host "  -> already assigned" -ForegroundColor DarkGray
            } else {
                throw
            }
        }
        $step++
    }

    # Third scope: the platform scope. governance-decision-log-svc,
    # configuration-feature-flag-svc, secret-vault-integration-svc and the
    # non-entity-scoped paths of policy-svc all authorize against
    # AUTHZ_PLATFORM_SCOPE_ID (00000000-0000-0000-0000-00000000f001 in
    # deployments/docker-compose.yml) -- decisions, config values, flags and
    # secret policies are platform records, not entity-scoped ones. Without an
    # assignment on that scope the console's Governance Log, Settings and
    # Secret Vault writes answer 403 even though every legal-entity grant above
    # is in place.
    Write-Host "$step  assign role to $PRINCIPAL_ID on the platform scope" -NoNewline
    try {
        # The seed admin, not the approver: an assignment on the platform scope
        # needs iam.assignment.grant held AT platform scope.
        $assignment = Invoke-Authz -Actor $SEED_ADMIN_ID -Path "/v1/admin/role-assignments" -Body @{
            principal_id    = $PRINCIPAL_ID
            role_id         = $ROLE_ID
            legal_entity_id = $PLATFORM_SCOPE
            effective_from  = "2020-01-01T00:00:00Z"
            assigned_by     = $APPROVER_ID
        }
        Write-Host "  -> $($assignment.status) $($assignment.body.principal_role_assignment_id)"
    } catch {
        if ("$_" -match "409|23505|duplicate|already") {
            Write-Host "  -> already assigned" -ForegroundColor DarkGray
        } else {
            throw
        }
    }
    $step++
}

# SoD approver assignment runs even on the already-granted path so the smoke
# tests can exercise reject-the-creator paths (accounts-payable-svc refuses a
# creator approving their own invoice). The role and its bundles exist either
# way by this point.
Write-Host "assign role to $APPROVER_ID on the legal entity (SoD approver)" -NoNewline
try {
    $assignment = Invoke-Authz -Path "/v1/admin/role-assignments" -Body @{
        principal_id    = $APPROVER_ID
        role_id         = $ROLE_ID
        legal_entity_id = $LEGAL_ENTITY
        effective_from  = "2020-01-01T00:00:00Z"
        assigned_by     = $PRINCIPAL_ID
    }
    Write-Host "  -> $($assignment.status) $($assignment.body.principal_role_assignment_id)"
} catch {
    if ("$_" -match "409|23505|duplicate|already") {
        Write-Host "  -> already assigned" -ForegroundColor DarkGray
    } else {
        throw
    }
}

# -- policy-svc's ledger writer -----------------------------------------------------
#
# Runs on every invocation, like the SoD approver above: it is one role with one
# bundle and two assignments, written idempotently. Direct SQL, as for the other
# service identities in the bootstrap block -- DEVELOPMENT ONLY.
if ((Get-Decision -Action "GOVERNANCE_DECISION_RECORD" -Scope $PLATFORM_SCOPE -Principal $POLICY_SERVICE_ID).decision_outcome -ne "GRANTED" -or
    (Get-Decision -Action "GOVERNANCE_DECISION_RECORD" -Scope $LEGAL_ENTITY -Principal $POLICY_SERVICE_ID).decision_outcome -ne "GRANTED") {
    Write-Host "grant $POLICY_SERVICE_ID GOVERNANCE_DECISION_RECORD (policy evidence relay)" -NoNewline
    $sql = @"
INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_by_principal_id)
VALUES ('$POLICY_SERVICE_ROLE_ID', '$TENANT_ID', 'SVC_POLICY_LEDGER', 'policy-svc evidence relay', 'LEGAL_ENTITY', true, 'seed-bootstrap')
ON CONFLICT DO NOTHING;
INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions, active_flag)
VALUES ('$POLICY_SERVICE_ROLE_ID', 'LEDGER_WRITE', '["GOVERNANCE_DECISION_RECORD"]'::jsonb, true)
ON CONFLICT (role_id, bundle_code) DO UPDATE SET permitted_actions = EXCLUDED.permitted_actions, active_flag = true;
INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by)
SELECT '$POLICY_SERVICE_ID', '$POLICY_SERVICE_ROLE_ID', scope, now() - interval '1 minute', 'seed-bootstrap'
FROM (VALUES ('$PLATFORM_SCOPE'::uuid), ('$LEGAL_ENTITY'::uuid)) AS s(scope)
WHERE NOT EXISTS (SELECT 1 FROM principal_role_assignments
                  WHERE principal_id = '$POLICY_SERVICE_ID' AND role_id = '$POLICY_SERVICE_ROLE_ID' AND legal_entity_id = s.scope);
"@
    $sql | docker exec -i $PgContainer psql -U postgres -d authorization_svc -v ON_ERROR_STOP=1 -q | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "policy-svc ledger grant into authorization_svc failed (container $PgContainer)" }
    Start-Sleep -Seconds 6
    Write-Host "  -> done" -ForegroundColor Green
}

# -- REF-04 / REF-05 maker-checker roles -------------------------------------------
#
# Which action belongs to which service (copied from the `Action*` constants in
# each service's internal/handler/handler.go -- re-grep them before editing):
#
#   currency-registry-svc    CURRENCY_IMPORT, CURRENCY_ACTIVATE, CURRENCY_RESTRICT,
#                            CURRENCY_RETIRE, CURRENCY_TENANT_ENABLE, CURRENCY_TENANT_DISABLE
#   fiscal-calendar-svc      FISCAL_CALENDAR_PROPOSE, FISCAL_CALENDAR_APPROVE,
#                            FISCAL_CALENDAR_ACTIVATE
#                            (transition-plan creation uses PROPOSE and plan
#                            approve/reject uses APPROVE -- there is no separate plan action)
#   accounting-period-svc    PERIOD_STATE_COMMAND, PERIOD_MATERIALIZE
#
# These are deliberately NOT appended to $BUNDLES. $BUNDLES all land on
# CONSOLE_DEMO_OPERATOR, which is assigned to BOTH the demo principal and the
# SoD approver, so adding them there would hand each of them both sides of
# every pair below. Instead there are two roles, one per side:
#
#   REF05_PERIOD_MAKER    -> $PRINCIPAL_ID  proposes, imports, materialises
#   REF05_PERIOD_CHECKER  -> $APPROVER_ID   approves, activates, restricts, retires
#
# SoD pairs the services enforce on a DIFFERENT PRINCIPAL (so they are only
# testable if no one principal holds both actions):
#   FISCAL_CALENDAR_PROPOSE  vs FISCAL_CALENDAR_APPROVE   (proposer may not approve)
#   CURRENCY_IMPORT          vs CURRENCY_ACTIVATE         (importer may not activate)
# PERIOD_STATE_COMMAND is ONE action covering soft close and hard close, and
# accounting-period-svc refuses a hard close by whoever requested the soft
# close. The action cannot be split, so both roles carry it and the service's own
# actor check is the control; the demo principals are still distinct.
#
# Currency actions authorize against the PLATFORM scope when a request names no
# legal entity (currency-registry-svc falls back to it), so both roles are also
# assigned there.
$REF05_ROLES = @(
    @{
        RoleId     = "88888888-8888-8888-8888-888888888881"
        Code       = "REF05_PERIOD_MAKER"
        Name       = "REF-04/05 Period Maker"
        Principal  = $PRINCIPAL_ID
        BundleCode = "REF05_MAKER"
        Actions    = @(
            "CURRENCY_IMPORT", "CURRENCY_TENANT_ENABLE", "CURRENCY_TENANT_DISABLE",
            "FISCAL_CALENDAR_PROPOSE",
            "PERIOD_MATERIALIZE", "PERIOD_STATE_COMMAND"
        )
    },
    @{
        RoleId     = "88888888-8888-8888-8888-888888888882"
        Code       = "REF05_PERIOD_CHECKER"
        Name       = "REF-04/05 Period Checker"
        Principal  = $APPROVER_ID
        BundleCode = "REF05_CHECKER"
        Actions    = @(
            "CURRENCY_ACTIVATE", "CURRENCY_RESTRICT", "CURRENCY_RETIRE",
            "FISCAL_CALENDAR_APPROVE", "FISCAL_CALENDAR_ACTIVATE",
            "PERIOD_STATE_COMMAND"
        )
    }
)
$REF05_SOD_PAIRS = @(
    @("FISCAL_CALENDAR_PROPOSE", "FISCAL_CALENDAR_APPROVE"),
    @("CURRENCY_IMPORT", "CURRENCY_ACTIVATE")
)

function Add-Ref05Assignment {
    param([string] $PrincipalId, [string] $RoleId, [string] $Scope, [string] $Label)
    Write-Host "assign $RoleId to $PrincipalId on the $Label" -NoNewline
    try {
        $assignment = Invoke-Authz -Path "/v1/admin/role-assignments" -Body @{
            principal_id    = $PrincipalId
            role_id         = $RoleId
            legal_entity_id = $Scope
            effective_from  = "2020-01-01T00:00:00Z"
            assigned_by     = $PRINCIPAL_ID
        }
        Write-Host "  -> $($assignment.status) $($assignment.body.principal_role_assignment_id)"
    } catch {
        if ("$_" -match "409|23505|duplicate|already") {
            Write-Host "  -> already assigned" -ForegroundColor DarkGray
        } else {
            throw
        }
    }
}

foreach ($r in $REF05_ROLES) {
    # Probe as the principal the role is FOR, on both scopes, so a re-run on a
    # volume that already holds the grant does nothing (bundle posts have no upsert).
    $roleMissing = @()
    foreach ($action in $r.Actions) {
        foreach ($scope in @($LEGAL_ENTITY, $PLATFORM_SCOPE)) {
            $probe = Invoke-Authz -Path "/v1/authorize" -Body @{
                principal_id = $r.Principal; legal_entity_id = $scope; action_type = $action
            }
            if ($probe.body.decision_outcome -ne "GRANTED") { $roleMissing += "$action@$scope" }
        }
    }
    if ($roleMissing.Count -eq 0) {
        Write-Host "$($r.Code): already granted to $($r.Principal)" -ForegroundColor Green
        continue
    }
    Write-Host "$($r.Code): $($roleMissing.Count) grants missing" -ForegroundColor Yellow

    Write-Host "role $($r.Code)" -NoNewline
    try {
        $role = Invoke-Authz -Path "/v1/admin/roles" -Body @{
            role_id                 = $r.RoleId
            tenant_id               = $TENANT_ID
            role_code               = $r.Code
            role_name               = $r.Name
            role_scope_type         = "LEGAL_ENTITY"
            created_by_principal_id = $PRINCIPAL_ID
        }
        Write-Host "  -> $($role.status)"
    } catch {
        Write-Host "  -> already exists, or could not be created; continuing to the bundle" -ForegroundColor DarkGray
    }
    $bundle = Invoke-Authz -Path "/v1/admin/roles/$($r.RoleId)/permission-bundles" -Body @{
        bundle_code       = $r.BundleCode
        permitted_actions = $r.Actions
    }
    Write-Host "bundle $($r.BundleCode)  -> $($bundle.status) [$($bundle.body.permitted_actions -join ', ')]"
    Add-Ref05Assignment -PrincipalId $r.Principal -RoleId $r.RoleId -Scope $LEGAL_ENTITY -Label "legal entity"
    Add-Ref05Assignment -PrincipalId $r.Principal -RoleId $r.RoleId -Scope $PLATFORM_SCOPE -Label "platform scope"
}

# $APPROVER_ID also needs the platform scope: commercial-account-svc's COM-01
# price book (and every other PLATFORM_SCOPED_ACTION_CODES bundle) checks
# against platformScopeID, not the legal entity. Without this, a genuine SoD
# approve/publish test (a second principal distinct from the proposer) has no
# principal to use for any platform-scoped action.
Write-Host "assign role to $APPROVER_ID on the platform scope (SoD approver)" -NoNewline
try {
    $assignment = Invoke-Authz -Path "/v1/admin/role-assignments" -Body @{
        principal_id    = $APPROVER_ID
        role_id         = $ROLE_ID
        legal_entity_id = $PLATFORM_SCOPE
        effective_from  = "2020-01-01T00:00:00Z"
        assigned_by     = $PRINCIPAL_ID
    }
    Write-Host "  -> $($assignment.status) $($assignment.body.principal_role_assignment_id)"
} catch {
    if ("$_" -match "409|23505|duplicate|already") {
        Write-Host "  -> already assigned" -ForegroundColor DarkGray
    } else {
        throw
    }
}

# Confirm through the same path the services use, rather than trusting that a
# pile of 201s adds up to a working grant.
Write-Host ""
Write-Host "Verifying every action on the legal entity:" -ForegroundColor Cyan
$failed = @()
foreach ($bundle in $BUNDLES) {
    foreach ($action in $bundle.Actions) {
        $decision = Get-Decision -Action $action -Scope $LEGAL_ENTITY
        $outcome  = $decision.decision_outcome
        if ($outcome -eq "GRANTED") { $colour = "Green" } else { $colour = "Red"; $failed += $action }
        Write-Host ("     {0,-28} {1,-30} -> {2} ({3})" -f $bundle.Service, $action, $outcome, $decision.decision_basis) -ForegroundColor $colour
    }
}

# Three console paths submit without a legal entity, and each needs the tenant to
# carry the grant: evidence's "tenant-wide" requirement option, spend-controls'
# unfiltered registers, and vendor-due-diligence's unfiltered screening register.
# All three authorize against the tenant when no entity is named.
#
# retention-registry-svc is the fourth, and it is not a fallback there but the
# normal case: that service passes the request's tenant_id as the authorization
# scope rather than a legal_entity_id, so a tenant-scoped hold or policy is ALWAYS
# checked against the tenant. Verified here rather than assumed from the tenant
# role assignment, because "the role is assigned on the tenant" and "this action
# is granted on the tenant" have come apart before.
$TENANT_SCOPED_ACTIONS = @(
    ($BUNDLES | Where-Object { $_.Code -eq "EVIDENCE_FULL" }).Actions
    "SPEND_POLICY_VIEW"
    "VENDOR_DD_VIEW"
    ($BUNDLES | Where-Object { $_.Code -eq "RETENTION_FULL" }).Actions
) | ForEach-Object { $_ }

Write-Host "Verifying the tenant-scoped fallback:" -ForegroundColor Cyan
foreach ($action in $TENANT_SCOPED_ACTIONS) {
    $decision = Get-Decision -Action $action -Scope $TENANT_ID
    $outcome  = $decision.decision_outcome
    if ($outcome -eq "GRANTED") { $colour = "Green" } else { $colour = "Red"; $failed += "$action (tenant scope)" }
    Write-Host ("     {0,-28} {1,-30} -> {2}" -f "tenant-wide", $action, $outcome) -ForegroundColor $colour
}

Write-Host "Verifying the platform-scoped actions:" -ForegroundColor Cyan
# governance-decision-log, configuration-feature-flag, secret-vault and the
# non-entity-scoped policy paths authorize against AUTHZ_PLATFORM_SCOPE_ID, so
# these are probed on that scope, not on the legal entity.
# $PLATFORM_SCOPED_ACTIONS is built near $ALL_ACTIONS, because the
# "is there anything to do" probe at the top of the run needs it too.
foreach ($action in $PLATFORM_SCOPED_ACTIONS) {
    $decision = Get-Decision -Action $action -Scope $PLATFORM_SCOPE
    $outcome  = $decision.decision_outcome
    if ($outcome -eq "GRANTED") { $colour = "Green" } else { $colour = "Red"; $failed += "$action (platform scope)" }
    Write-Host ("     {0,-28} {1,-30} -> {2}" -f "platform", $action, $outcome) -ForegroundColor $colour
}

# REF-04/05: verify each maker/checker role grants what it should, and that no
# principal holds both sides of an SoD pair (otherwise SoD is untestable and a
# hand-made grant has crept in).
Write-Host "Verifying REF-04/05 maker-checker roles:" -ForegroundColor Cyan
foreach ($r in $REF05_ROLES) {
    foreach ($action in $r.Actions) {
        foreach ($scope in @($LEGAL_ENTITY, $PLATFORM_SCOPE)) {
            $probe = Invoke-Authz -Path "/v1/authorize" -Body @{
                principal_id = $r.Principal; legal_entity_id = $scope; action_type = $action
            }
            if ($probe.body.decision_outcome -ne "GRANTED") { $failed += "$action ($($r.Code) on $scope)" }
        }
    }
}
foreach ($principal in @($PRINCIPAL_ID, $APPROVER_ID)) {
    foreach ($pair in $REF05_SOD_PAIRS) {
        $held = @()
        foreach ($action in $pair) {
            $probe = Invoke-Authz -Path "/v1/authorize" -Body @{
                principal_id = $principal; legal_entity_id = $LEGAL_ENTITY; action_type = $action
            }
            if ($probe.body.decision_outcome -eq "GRANTED") { $held += $action }
        }
        if ($held.Count -eq $pair.Count) {
            $failed += "$principal holds BOTH sides of an SoD pair ($($pair -join ' / '))"
            Write-Host "     $principal holds both $($pair -join ' / ')" -ForegroundColor Red
        } else {
            Write-Host ("     {0} holds {1} of [{2}]" -f $principal, $held.Count, ($pair -join ' / ')) -ForegroundColor Green
        }
    }
}

# -- REP-01 financial-statements-svc maker-checker roles --------------------------
#
# Actions (copied from the Action* constants in
# services/financial-statements-svc/internal/service/service.go -- re-grep
# them before editing): FINANCIAL_STATEMENT_SET_CREATE, _VIEW, _REFRESH,
# _VALIDATE, _REVIEW, _APPROVE, _CERTIFY, _PUBLISH.
#
# _REVIEW covers both SubmitForReview (maker side) and RequestChanges
# (checker side) -- one action, like accounting-period-svc's
# PERIOD_STATE_COMMAND above, because the service's own in-code Preparer()
# check is the real control, not an RBAC split. The genuine SoD boundary the
# service enforces is "the preparer may not also Approve or Certify their own
# work" (ZS-SVC-H-001 4.1) -- that is a per-row identity comparison against
# whoever created or last refreshed the set, never just "does this principal
# hold the APPROVE/CERTIFY grant". The two roles below exist so a demo run
# has two DISTINCT principals to prove that with, same purpose as REF05's
# maker/checker pair.
$REF01_ROLES = @(
    @{
        RoleId     = "88888888-8888-8888-8888-888888888883"
        Code       = "REF01_STATEMENT_MAKER"
        Name       = "REP-01 Statement Maker"
        Principal  = $PRINCIPAL_ID
        BundleCode = "REF01_MAKER"
        Actions    = @(
            "FINANCIAL_STATEMENT_SET_CREATE", "FINANCIAL_STATEMENT_SET_VIEW",
            "FINANCIAL_STATEMENT_SET_REFRESH", "FINANCIAL_STATEMENT_SET_VALIDATE",
            "FINANCIAL_STATEMENT_SET_REVIEW"
        )
    },
    @{
        RoleId     = "88888888-8888-8888-8888-888888888884"
        Code       = "REF01_STATEMENT_CHECKER"
        Name       = "REP-01 Statement Checker"
        Principal  = $APPROVER_ID
        BundleCode = "REF01_CHECKER"
        Actions    = @(
            "FINANCIAL_STATEMENT_SET_VIEW", "FINANCIAL_STATEMENT_SET_REVIEW",
            "FINANCIAL_STATEMENT_SET_APPROVE", "FINANCIAL_STATEMENT_SET_CERTIFY",
            "FINANCIAL_STATEMENT_SET_PUBLISH"
        )
    }
)
# Not RBAC-exclusive pairs (both are checked below only to confirm the two
# demo principals are genuinely distinct grants, not to assert the platform
# would refuse one principal holding both -- it would not; the service's own
# Preparer() check is what actually refuses that, and is NOT exercised by
# this seed script).
$REF01_SOD_PAIRS = @(
    @("FINANCIAL_STATEMENT_SET_CREATE", "FINANCIAL_STATEMENT_SET_APPROVE"),
    @("FINANCIAL_STATEMENT_SET_CREATE", "FINANCIAL_STATEMENT_SET_CERTIFY")
)

foreach ($r in $REF01_ROLES) {
    $roleMissing = @()
    foreach ($action in $r.Actions) {
        foreach ($scope in @($LEGAL_ENTITY, $PLATFORM_SCOPE)) {
            $probe = Invoke-Authz -Path "/v1/authorize" -Body @{
                principal_id = $r.Principal; legal_entity_id = $scope; action_type = $action
            }
            if ($probe.body.decision_outcome -ne "GRANTED") { $roleMissing += "$action@$scope" }
        }
    }
    if ($roleMissing.Count -eq 0) {
        Write-Host "$($r.Code): already granted to $($r.Principal)" -ForegroundColor Green
        continue
    }
    Write-Host "$($r.Code): $($roleMissing.Count) grants missing" -ForegroundColor Yellow
    Write-Host "role $($r.Code)" -NoNewline
    try {
        $role = Invoke-Authz -Path "/v1/admin/roles" -Body @{
            role_id                 = $r.RoleId
            tenant_id               = $TENANT_ID
            role_code               = $r.Code
            role_name               = $r.Name
            role_scope_type         = "LEGAL_ENTITY"
            created_by_principal_id = $PRINCIPAL_ID
        }
        Write-Host "  -> $($role.status)"
    } catch {
        Write-Host "  -> already exists, or could not be created; continuing to the bundle" -ForegroundColor DarkGray
    }
    $bundle = Invoke-Authz -Path "/v1/admin/roles/$($r.RoleId)/permission-bundles" -Body @{
        bundle_code       = $r.BundleCode
        permitted_actions = $r.Actions
    }
    Write-Host "bundle $($r.BundleCode)  -> $($bundle.status) [$($bundle.body.permitted_actions -join ', ')]"
    Add-Ref05Assignment -PrincipalId $r.Principal -RoleId $r.RoleId -Scope $LEGAL_ENTITY -Label "legal entity"
    Add-Ref05Assignment -PrincipalId $r.Principal -RoleId $r.RoleId -Scope $PLATFORM_SCOPE -Label "platform scope"
}

Write-Host "Verifying REP-01 maker-checker roles:" -ForegroundColor Cyan
foreach ($r in $REF01_ROLES) {
    foreach ($action in $r.Actions) {
        foreach ($scope in @($LEGAL_ENTITY, $PLATFORM_SCOPE)) {
            $probe = Invoke-Authz -Path "/v1/authorize" -Body @{
                principal_id = $r.Principal; legal_entity_id = $scope; action_type = $action
            }
            if ($probe.body.decision_outcome -ne "GRANTED") { $failed += "$action ($($r.Code) on $scope)" }
        }
    }
}
foreach ($pair in $REF01_SOD_PAIRS) {
    $makerHas = (Invoke-Authz -Path "/v1/authorize" -Body @{
        principal_id = $PRINCIPAL_ID; legal_entity_id = $LEGAL_ENTITY; action_type = $pair[1]
    }).body.decision_outcome -eq "GRANTED"
    if ($makerHas) {
        $failed += "$PRINCIPAL_ID (the REP-01 maker) unexpectedly holds the checker-only action $($pair[1])"
        Write-Host "     maker unexpectedly holds $($pair[1])" -ForegroundColor Red
    } else {
        Write-Host ("     maker/checker split holds for {0}" -f $pair[1]) -ForegroundColor Green
    }
}

if ($failed.Count -gt 0) {
    throw "Seed completed but these are still not GRANTED: $($failed -join ', '). The console's writes will be refused."
}

Write-Host ""
Write-Host "Done. The console can write to all $($BUNDLES.Count) wired services." -ForegroundColor Green