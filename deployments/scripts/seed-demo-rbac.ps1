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
    accounts-receivable-svc started reconciling the legal entity on a write — at
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
    [string] $GatewayUrl
)

$ErrorActionPreference = "Stop"

# Must match DEMO_IDENTITY in zoiko-suite-fe/lib/auth.ts.
$TENANT_ID    = "11111111-1111-1111-1111-111111111111"
$LEGAL_ENTITY = "22222222-2222-2222-2222-222222222222"
$PRINCIPAL_ID = "33333333-3333-3333-3333-333333333333"
$APPROVER_ID  = "66666666-6666-6666-6666-666666666666"
$ROLE_ID      = "44444444-4444-4444-4444-444444444444"

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
        Actions = @("PO_ISSUE", "PO_AMEND", "PO_CLOSE")
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
        # policy-svc checks POLICY_CREATE / POLICY_VERSION_CREATE /
        # POLICY_VERSION_ACTIVATE against the legal entity when the policy is
        # entity-scoped and against authzPlatformScopeID when it is not. The
        # console always creates entity-scoped policies, but the platform-scope
        # assignment below also carries these so both paths are granted.
        Code    = "POLICY_FULL"
        Service = "policy-svc"
        Actions = @("POLICY_CREATE", "POLICY_VERSION_CREATE", "POLICY_VERSION_ACTIVATE")
    },
    @{
        # configuration-feature-flag-svc authorizes config and flag writes
        # against authzPlatformScopeID -- a config value or flag shapes
        # platform behaviour, so it is a platform action, not an entity one.
        Code    = "CONFIG_FULL"
        Service = "configuration-feature-flag-svc"
        Actions = @("CONFIGURATION_WRITE", "FEATURE_FLAG_WRITE")
    },
    @{
        # notification-svc authorizes sends against the target legal entity
        # and views against the legal entity being queried, so the console
        # grants these on the legal entity scope.
        Code    = "NOTIFICATION_FULL"
        Service = "notification-svc"
        Actions = @("NOTIFICATION_SEND", "NOTIFICATION_VIEW")
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
        # 403 — the whole write path dead, with the page still rendering
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
        Code    = "JURISDICTION_FULL"
        Service = "jurisdiction-rules-svc"
        Actions = @(
            "JURISDICTION_CREATE", "JURISDICTION_DEACTIVATE",
            "JURISDICTION_RULE_CREATE", "JURISDICTION_RULE_TRANSITION",
            "JURISDICTION_RULE_RECORD_DRIFT"
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
    "GOVERNANCE_FULL", "CONFIG_FULL", "VAULT_FULL", "POLICY_FULL",
    "SCHEMA_FULL", "JURISDICTION_FULL",
    # retention-registry-svc falls back to the platform scope when a hold or
    # policy names no tenant -- the console offers that as a "platform-wide"
    # checkbox, so the grant has to exist on that scope too.
    "RETENTION_FULL")
$PLATFORM_SCOPED_ACTIONS = $BUNDLES |
    Where-Object { $PLATFORM_SCOPED_ACTION_CODES -contains $_.Code } |
    ForEach-Object { $_.Actions }

if ($PSCmdlet.ParameterSetName -eq "Gateway") {
    $AUTHZ = "$($GatewayUrl.TrimEnd('/'))/authorization-svc"
} else {
    $AUTHZ = $AuthzUrl.TrimEnd('/')
}

function Invoke-Authz {
    param(
        [Parameter(Mandatory)] [string] $Path,
        [Parameter(Mandatory)] $Body
    )
    $json = $Body | ConvertTo-Json -Compress -Depth 5
    try {
        $response = Invoke-WebRequest -Uri "$AUTHZ$Path" -Method POST -Body $json `
            -ContentType "application/json" -UseBasicParsing -TimeoutSec 10
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
        [Parameter(Mandatory)] [string] $Scope
    )
    $result = Invoke-Authz -Path "/v1/authorize" -Body @{
        principal_id    = $PRINCIPAL_ID
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
        $result = Invoke-Authz -Path "/v1/admin/roles/$ROLE_ID/permission-bundles" -Body @{
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
            $assignment = Invoke-Authz -Path "/v1/admin/role-assignments" -Body @{
                principal_id    = $PRINCIPAL_ID
                role_id         = $ROLE_ID
                legal_entity_id = $scope.Id
                effective_from  = "2020-01-01T00:00:00Z"
                assigned_by     = $PRINCIPAL_ID
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
        $assignment = Invoke-Authz -Path "/v1/admin/role-assignments" -Body @{
            principal_id    = $PRINCIPAL_ID
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

if ($failed.Count -gt 0) {
    throw "Seed completed but these are still not GRANTED: $($failed -join ', '). The console's writes will be refused."
}

Write-Host ""
Write-Host "Done. The console can write to all $($BUNDLES.Count) wired services." -ForegroundColor Green