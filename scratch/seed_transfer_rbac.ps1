$ErrorActionPreference = "Stop"

$authz = "http://127.0.0.1:8089"
$roleId = "44444444-4444-4444-4444-444444444444"
$tenantId = "11111111-1111-1111-1111-111111111111"
$legalEntity = "22222222-2222-2222-2222-222222222222"
$platformScope = "00000000-0000-0000-0000-00000000f001"
$makerId = "33333333-3333-3333-3333-333333333333"

function New-Headers {
    param([string]$Scope = $legalEntity)
    return @{
        "Content-Type"      = "application/json"
        "X-Tenant-Id"       = $tenantId
        "X-Principal-Id"    = $makerId
        "X-Legal-Entity-Id" = $Scope
        "X-Correlation-ID"  = [guid]::NewGuid().ToString()
        "X-Request-Id"      = [guid]::NewGuid().ToString()
        "X-Source-System"   = "console-seed"
        "X-Source-Channel"  = "api"
        "Idempotency-Key"   = [guid]::NewGuid().ToString()
    }
}

$transferActions = @(
    "PRIVACY_TRANSFER_RELATIONSHIP_MANAGE",
    "PRIVACY_TRANSFER_MECHANISM_MANAGE",
    "PRIVACY_TRANSFER_ASSESSMENT_RECORD"
)

Write-Host "Registering PRIVACY_TRANSFER_FULL permission bundle on role $roleId..."
$bundleBody = @{
    bundle_code = "PRIVACY_TRANSFER_FULL"
    permitted_actions = $transferActions
} | ConvertTo-Json

try {
    $res = Invoke-RestMethod -Uri "$authz/v1/admin/roles/$roleId/permission-bundles" -Method Post -Body $bundleBody -Headers (New-Headers)
    Write-Host "Bundle added: status=200, actions=$($res.permitted_actions -join ', ')"
} catch {
    Write-Host "Bundle add note: $_"
}

# Verify authorization check
foreach ($act in $transferActions) {
    foreach ($sc in @($platformScope, $tenantId)) {
        $dBody = @{
            principal_id = $makerId
            legal_entity_id = $sc
            action_type = $act
        } | ConvertTo-Json
        $d = Invoke-RestMethod -Uri "$authz/v1/authorize" -Method Post -Body $dBody -Headers (New-Headers -Scope $sc)
        Write-Host "$act on $sc : $($d.decision_outcome)"
    }
}
