<#
.SYNOPSIS
    Grant the admin console's demo principal the purchase-order permissions it
    needs, via authorization-svc's admin API.

.DESCRIPTION
    authorization-svc denies by default: with no role assignment it answers
    DENIED / no_grant, and purchase-order-svc fails closed on that, so every
    issue / amend / close from the console is refused. That is correct
    behaviour, not a bug -- but it means a fresh stack has a console that can
    read purchase orders and write none of them.

    This script creates the grant chain the console's demo identity expects:

        role (PROCUREMENT_OFFICER)
          -> permission bundle (PO_ISSUE, PO_AMEND, PO_CLOSE)
            -> assignment to the demo principal on the demo legal entity

    The UUIDs below MUST match DEMO_IDENTITY in the frontend's lib/auth.ts.
    They are UUIDs because the backend stores them in uuid columns -- a
    readable id like "demo-tenant" fails inside the driver and surfaces as a
    503 that reads like an outage rather than a validation error.

    Idempotent. Re-running is safe: role creation is idempotent on
    (tenant_id, role_code) server-side, and the assignment is skipped when an
    authorize probe already answers GRANTED. That probe matters -- the
    assignment insert has no upsert, so a blind re-run would either duplicate
    the row or fail on the primary key.

.PARAMETER GatewayUrl
    Base URL of the single-port Traefik gateway. Defaults to the compose
    default of 8000.

.EXAMPLE
    ./seed-demo-rbac.ps1
    ./seed-demo-rbac.ps1 -GatewayUrl http://localhost:80
#>
[CmdletBinding()]
param(
    [string] $GatewayUrl = "http://localhost:8000"
)

$ErrorActionPreference = "Stop"

# Must match DEMO_IDENTITY in zoiko-suite-fe/lib/auth.ts.
$TENANT_ID    = "11111111-1111-1111-1111-111111111111"
$LEGAL_ENTITY = "22222222-2222-2222-2222-222222222222"
$PRINCIPAL_ID = "33333333-3333-3333-3333-333333333333"
$ROLE_ID      = "44444444-4444-4444-4444-444444444444"

$ROLE_CODE = "PROCUREMENT_OFFICER"
$ACTIONS   = @("PO_ISSUE", "PO_AMEND", "PO_CLOSE")

$AUTHZ = "$($GatewayUrl.TrimEnd('/'))/authorization-svc"

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

function Test-AlreadyGranted {
    # A single probe covers the whole chain: GRANTED can only come from a live
    # role + bundle + assignment, so there is nothing left to seed.
    $result = Invoke-Authz -Path "/v1/authorize" -Body @{
        principal_id    = $PRINCIPAL_ID
        legal_entity_id = $LEGAL_ENTITY
        action_type     = $ACTIONS[0]
    }
    return $result.body.decision_outcome -eq "GRANTED"
}

Write-Host "Seeding demo RBAC via $AUTHZ" -ForegroundColor Cyan

# Fail early and clearly if the stack isn't up, rather than mid-chain.
try {
    Invoke-WebRequest -Uri "$AUTHZ/healthz" -UseBasicParsing -TimeoutSec 5 | Out-Null
} catch {
    throw "authorization-svc is not reachable at $AUTHZ. Start the stack first:`n" +
          "  `$env:GATEWAY_PORT = '8000'`n" +
          "  docker compose -f deployments/docker-compose.yml up -d gateway purchase-order-svc"
}

if (Test-AlreadyGranted) {
    Write-Host "Already granted -- $PRINCIPAL_ID holds $($ACTIONS[0]) on $LEGAL_ENTITY. Nothing to do." -ForegroundColor Green
    return
}

Write-Host "1/3  role $ROLE_CODE" -NoNewline
$role = Invoke-Authz -Path "/v1/admin/roles" -Body @{
    role_id                 = $ROLE_ID
    tenant_id               = $TENANT_ID
    role_code               = $ROLE_CODE
    role_name               = "Procurement Officer"
    role_scope_type         = "LEGAL_ENTITY"
    created_by_principal_id = $PRINCIPAL_ID
}
Write-Host "  -> $($role.status) $(if ($role.status -eq 200) { '(already existed)' } else { '(created)' })"

Write-Host "2/3  permission bundle PO_FULL" -NoNewline
$bundle = Invoke-Authz -Path "/v1/admin/roles/$ROLE_ID/permission-bundles" -Body @{
    bundle_code       = "PO_FULL"
    permitted_actions = $ACTIONS
}
Write-Host "  -> $($bundle.status) [$($bundle.body.permitted_actions -join ', ')]"

Write-Host "3/3  assign role to $PRINCIPAL_ID" -NoNewline
$assignment = Invoke-Authz -Path "/v1/admin/role-assignments" -Body @{
    principal_id    = $PRINCIPAL_ID
    role_id         = $ROLE_ID
    legal_entity_id = $LEGAL_ENTITY
    effective_from  = "2020-01-01T00:00:00Z"
    assigned_by     = $PRINCIPAL_ID
}
Write-Host "  -> $($assignment.status) $($assignment.body.principal_role_assignment_id)"

# Confirm through the same path the services use, rather than trusting that
# three 201s add up to a working grant.
foreach ($action in $ACTIONS) {
    $decision = Invoke-Authz -Path "/v1/authorize" -Body @{
        principal_id    = $PRINCIPAL_ID
        legal_entity_id = $LEGAL_ENTITY
        action_type     = $action
    }
    $outcome = $decision.body.decision_outcome
    $colour  = if ($outcome -eq "GRANTED") { "Green" } else { "Red" }
    Write-Host "     $action -> $outcome ($($decision.body.decision_basis))" -ForegroundColor $colour
    if ($outcome -ne "GRANTED") {
        throw "Seed completed but $action is still $outcome. The console's writes will be refused."
    }
}

Write-Host "Done. The admin console can now issue, amend, and close purchase orders." -ForegroundColor Green
