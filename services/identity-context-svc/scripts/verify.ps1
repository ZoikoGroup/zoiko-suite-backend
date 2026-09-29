# PowerShell verification walkthrough — identity-context-svc (GOV-01)
# Windows PowerShell 5.1 compatible. Run from anywhere.

$ErrorActionPreference = 'Continue'

$TEN  = '11111111-1111-1111-1111-111111111111'
$PRIN = '33333333-3333-3333-3333-333333333333'
$ENT  = '22222222-2222-2222-2222-222222222222'
$SUP  = '44444444-4444-4444-4444-444444444444'
$API  = 'http://localhost:8080'
$WEB  = 'http://localhost:3000'

function New-Envelope {
    param([string]$Rid = [guid]::NewGuid().ToString())
    @{
        'Content-Type'      = 'application/json'
        'X-Tenant-Id'       = $TEN
        'X-Principal-Id'    = $PRIN
        'X-Legal-Entity-Id' = $ENT
        'X-Source-Channel'  = 'api'
        'X-Request-Id'      = $Rid
        'Idempotency-Key'   = $Rid
        'X-Correlation-ID'  = $Rid
    }
}

function Show-Step { param([string]$T) Write-Host "`n=== $T ===" -ForegroundColor Cyan }

# ---------------------------------------------------------------- STEP 1
Show-Step 'STEP 1 - health'
$h = Invoke-RestMethod -Uri "$API/health" -TimeoutSec 20
"status : $($h.status)"
$h.checks | Format-List | Out-String | Write-Host

# ---------------------------------------------------------------- STEP 2
Show-Step 'STEP 2 - login through the console'
$login = Invoke-RestMethod -Uri "$WEB/api/auth/login" -Method Post `
    -ContentType 'application/json' `
    -Body '{"email":"admin@zoikosuite.com","password":"Zoiko@Governance1"}' -TimeoutSec 30
"success          : $($login.success)"
"identityProvider : $($login.identityProvider)"
"user             : $($login.user.name)"

Show-Step 'STEP 2b - wrong password must be refused'
try {
    Invoke-RestMethod -Uri "$WEB/api/auth/login" -Method Post -ContentType 'application/json' `
        -Body '{"email":"admin@zoikosuite.com","password":"wrong"}' -TimeoutSec 30 | Out-Null
    Write-Host 'UNEXPECTED: wrong password accepted' -ForegroundColor Red
} catch {
    "refused with HTTP $([int]$_.Exception.Response.StatusCode)  (401 expected)"
}

# ---------------------------------------------------------------- STEP 3
Show-Step 'STEP 3 - authenticate directly'
$authBody = @{ tenant_id = $TEN; email = 'admin@zoikosuite.com'; password = 'Zoiko@Governance1' } | ConvertTo-Json
$auth = Invoke-RestMethod -Uri "$API/v1/authenticate" -Method Post -ContentType 'application/json' -Body $authBody -TimeoutSec 30
$TOKEN = $auth.access_token
"token_type : $($auth.token_type)  expires_in: $($auth.expires_in)s"
"principal  : $($auth.principal_id)"

# ---------------------------------------------------------------- STEP 4
Show-Step 'OP1 - ResolveTenantContext'
$resolveBody = @{ bearer_token = $TOKEN; legal_entity_id = $ENT; correlation_id = 'ps-1' } | ConvertTo-Json
$r = Invoke-RestMethod -Uri "$API/v1/context/resolve" -Method Post -Headers (New-Envelope) -Body $resolveBody -TimeoutSec 30
$SID = $r.session_context_id
"session_context_id : $SID"
"evidence_id        : $($r.evidence_id)"
"envelope_jwt       : $($r.envelope_jwt.Substring(0,40))..."

Show-Step 'OP2 - GetEffectiveContext'
$s = Invoke-WebRequest -UseBasicParsing -Uri "$API/v1/context/session/$SID" -Headers (New-Envelope) -TimeoutSec 30
"HTTP $($s.StatusCode)"

Show-Step 'OP3 - ExplainContextResolution'
$e = Invoke-RestMethod -Uri "$API/v1/context/session/$SID/explain" -Headers (New-Envelope) -TimeoutSec 30
"outcome        : $($e.outcome)"
"decision_id    : $($e.decision_id)"
"environment    : $($e.environment)"
"ingress_source : $($e.ingress_source)"

Show-Step 'OP4 - RefreshTenantContextCache'
$c4 = Invoke-RestMethod -Uri "$API/v1/context/cache/refresh" -Method Post -Headers (New-Envelope) `
    -Body '{"correlation_id":"ps-4"}' -TimeoutSec 30
"bindings_refreshed : $($c4.bindings_refreshed)   evidence_id: $($c4.evidence_id)"

Show-Step 'OP5 - InvalidateTenantContext'
$b5 = @{ correlation_id = 'ps-5'; justification = 'PowerShell walkthrough: verifying the command surface' } | ConvertTo-Json
$c5 = Invoke-RestMethod -Uri "$API/v1/context/tenant/invalidate" -Method Post -Headers (New-Envelope) -Body $b5 -TimeoutSec 30
"sessions_revoked   : $($c5.sessions_revoked)   evidence_id: $($c5.evidence_id)"

Show-Step 'OP6 - AttachSupportContext (break-glass)'
$b6 = @{
    tenant_id             = $TEN
    support_principal_id  = $SUP
    approver_principal_id = $PRIN
    reason_code           = 'INCIDENT_RESPONSE'
    justification         = 'PowerShell walkthrough: exercising the privileged command'
    ticket_ref            = 'PS-1'
    ttl_seconds           = 300
} | ConvertTo-Json
$c6 = Invoke-RestMethod -Uri "$API/v1/context/support" -Method Post -Headers (New-Envelope) -Body $b6 -TimeoutSec 30
$SC = $c6.support_context_id
"support_context_id : $SC"
"expires_at         : $($c6.expires_at)"

# ---------------------------------------------------------------- STEP 5
Show-Step 'NP1 - spoofed tenant header must be IGNORED (200 is correct)'
$np1 = New-Envelope
$np1['X-Tenant-Id'] = '88888888-8888-8888-8888-888888888888'
$rb = @{ bearer_token = $TOKEN; legal_entity_id = $ENT } | ConvertTo-Json
$n1 = Invoke-RestMethod -Uri "$API/v1/context/resolve" -Method Post -Headers $np1 -Body $rb -TimeoutSec 30
$payload = $n1.envelope_jwt.Split('.')[1].Replace('-', '+').Replace('_', '/')
while ($payload.Length % 4) { $payload += '=' }
$claims = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($payload)) | ConvertFrom-Json
"asked for tenant  : 88888888-8888-8888-8888-888888888888"
"envelope bound to : $($claims.principal.tenant_id)"
if ($claims.principal.tenant_id -eq $TEN) { Write-Host 'PASS - header ignored' -ForegroundColor Green }
else { Write-Host 'FAIL - spoof honoured' -ForegroundColor Red }

Show-Step 'NP3a - self-approved break-glass must be refused'
$bad = @{
    tenant_id = $TEN; support_principal_id = $PRIN; approver_principal_id = $PRIN
    reason_code = 'INCIDENT_RESPONSE'; justification = 'walkthrough: self-approval must be refused'
    ticket_ref = 'PS-2'; ttl_seconds = 300
} | ConvertTo-Json
try {
    Invoke-RestMethod -Uri "$API/v1/context/support" -Method Post -Headers (New-Envelope) -Body $bad -TimeoutSec 30 | Out-Null
    Write-Host 'UNEXPECTED: self-approval accepted' -ForegroundColor Red
} catch {
    $err = ($_.ErrorDetails.Message | ConvertFrom-Json)
    "HTTP $([int]$_.Exception.Response.StatusCode) - $($err.error)"
}

Show-Step 'NP3b - excessive TTL must be refused'
$long = @{
    tenant_id = $TEN; support_principal_id = $SUP; approver_principal_id = $PRIN
    reason_code = 'INCIDENT_RESPONSE'; justification = 'walkthrough: TTL beyond the ceiling'
    ticket_ref = 'PS-3'; ttl_seconds = 999999
} | ConvertTo-Json
try {
    Invoke-RestMethod -Uri "$API/v1/context/support" -Method Post -Headers (New-Envelope) -Body $long -TimeoutSec 30 | Out-Null
    Write-Host 'UNEXPECTED: excessive TTL accepted' -ForegroundColor Red
} catch {
    $err = ($_.ErrorDetails.Message | ConvertFrom-Json)
    "HTTP $([int]$_.Exception.Response.StatusCode) - $($err.error)"
}

Show-Step 'NP4 - revocation is prompt and idempotent'
foreach ($i in 1..2) {
    $d = Invoke-WebRequest -UseBasicParsing -Uri "$API/v1/context/support/$SC" -Method Delete -Headers (New-Envelope) -TimeoutSec 30
    "revoke $i -> HTTP $($d.StatusCode)"
}

Show-Step 'Envelope contract - a bare request must be refused'
try {
    Invoke-RestMethod -Uri "$API/v1/context/resolve" -Method Post -ContentType 'application/json' -Body '{}' -TimeoutSec 30 | Out-Null
    Write-Host 'UNEXPECTED: bare request accepted' -ForegroundColor Red
} catch {
    $body = ($_.ErrorDetails.Message | ConvertFrom-Json)
    "HTTP $([int]$_.Exception.Response.StatusCode) - $($body.error)"
    "missing: $($body.detail)"
}

# ---------------------------------------------------------------- STEP 6
Show-Step 'Telemetry'
$m = (Invoke-WebRequest -UseBasicParsing -Uri "$API/metrics" -TimeoutSec 30).Content -split "`n" | Where-Object { $_ -like 'identity_context*' }
"identity_context_* series: $($m.Count)"
$m | Select-Object -First 6 | ForEach-Object { "  $_" }

Show-Step 'Prometheus - scrape target and alert rules'
$up = Invoke-RestMethod -Uri 'http://localhost:9090/api/v1/query?query=up%7Bjob%3D%22identity-context-svc%22%7D' -TimeoutSec 20
if ($up.data.result.Count -gt 0) { "scrape up = $($up.data.result[0].value[1])" } else { 'NO TARGET' }
$rules = Invoke-RestMethod -Uri 'http://localhost:9090/api/v1/rules' -TimeoutSec 20
$gov = $rules.data.groups | Where-Object { $_.name -like '*gov-01*' }
"GOV-01 rules loaded: $($gov.rules.Count)"
$gov.rules | ForEach-Object { "  {0,-40} {1}" -f $_.name, $_.state }

Show-Step 'Outbox - must be 0 pending at rest'
docker exec zoiko-postgres psql -U postgres -d identity_context -tAc "select 'pending=' || count(*) filter (where published_at is null) || '  published=' || count(published_at) from event_outbox;"

Write-Host "`nWalkthrough complete." -ForegroundColor Green
