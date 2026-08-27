# Smoke test the four entry services running under tools/servicectl.
#
# WHY THIS EXISTS. Every endpoint here needs eight canonical envelope headers,
# and a write additionally needs an Idempotency-Key and X-Occurred-At. Assembling
# those by hand is where the time goes, and getting one wrong produces a 401
# envelope_incomplete that reads like a broken service rather than a malformed
# request. This builds them once.
#
#   pwsh deployments/scripts/smoke-entry-services.ps1
#   pwsh deployments/scripts/smoke-entry-services.ps1 -SkipWrites   # reads only, fast
#
# Assumes `servicectl serve` is running and the four services are up:
#
#   ./.servicectl/servicectl.exe serve --no-reap
#   curl -s -X POST "http://127.0.0.1:8079/v1/ensure" -H "Content-Type: application/json" `
#     -d '{"services":["identity-context-svc","tenant-entity-registry-svc","governance-decision-log-svc","authorization-svc"],"wait":true}'

[CmdletBinding()]
param(
    # The tenant, principal and legal entity the RBAC seed granted. Change all
    # three together, and reseed, if you want a different subject — the grant in
    # deployments/supabase/seed-rbac.sql names this principal specifically.
    [string] $TenantId      = '11111111-1111-1111-1111-111111111111',
    [string] $PrincipalId   = '33333333-3333-3333-3333-333333333333',
    [string] $LegalEntityId = '22222222-2222-2222-2222-222222222222',

    # Writes go through authorization-svc, which publishes an audit event to
    # Kafka INLINE on its response path. With no broker running, kafka-go retries
    # until it gives up and a single write can take ~40s. Skip them when you only
    # want to know the services are answering.
    [switch] $SkipWrites
)

$ErrorActionPreference = 'Stop'

$IDENTITY   = 'http://127.0.0.1:8080'
$TENANT     = 'http://127.0.0.1:8081'
$GOVERNANCE = 'http://127.0.0.1:8083'
$AUTHZ      = 'http://127.0.0.1:8089'

function New-Envelope {
    <#
      The canonical input contract, §4. Reads need the first six; a material
      write additionally needs Idempotency-Key (INV-08 replay protection) and
      X-Occurred-At. X-Source-Channel must be one of the seven permitted values —
      'api' here — and an unrecognised one is refused rather than coerced.
    #>
    param([string] $Operation, [switch] $Write)

    $h = @{
        'X-Tenant-Id'      = $TenantId
        'X-Principal-Id'   = $PrincipalId
        'X-Legal-Entity-Id' = $LegalEntityId
        'X-Correlation-ID' = 'smoke-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
        'X-Request-Id'     = [guid]::NewGuid().ToString()
        'X-Operation'      = $Operation
        'X-Source-System'  = 'smoke-entry-services'
        'X-Source-Channel' = 'api'
    }
    if ($Write) {
        $h['Idempotency-Key']  = [guid]::NewGuid().ToString()
        $h['X-Occurred-At']    = (Get-Date).ToUniversalTime().ToString('o')
        $h['X-Purpose-Context'] = 'TESTING'
    }
    return $h
}

function Invoke-Check {
    param(
        [string] $Label,
        [string] $Method,
        [string] $Uri,
        [hashtable] $Headers,
        [string] $Body,
        # Statuses that mean "working as designed" rather than "broken". A 401
        # from /v1/authenticate with no principal seeded IS the correct answer,
        # and a 404 for a record that does not exist is not a failure.
        [int[]] $Expect = @(200, 201)
    )
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $status = 0
    $content = ''
    try {
        # DisableKeepAlive because a slow response is normal here and a reused
        # connection is not: with no Kafka broker, authorization-svc's inline
        # audit publish retries for 17-40s, and PowerShell drops a kept-alive
        # socket in that window with "a connection that was expected to be kept
        # alive was closed by the server" -- reporting a FAIL for a request the
        # server answered 200. The failure was in the client, not the service.
        $args = @{ Uri = $Uri; Method = $Method; Headers = $Headers; UseBasicParsing = $true;
                   TimeoutSec = 180; DisableKeepAlive = $true }
        if ($Body) { $args['Body'] = $Body; $args['ContentType'] = 'application/json' }
        $r = Invoke-WebRequest @args
        $status = [int] $r.StatusCode
        $content = $r.Content
    } catch {
        if ($_.Exception.Response) {
            $status = [int] $_.Exception.Response.StatusCode
            try {
                $sr = New-Object System.IO.StreamReader($_.Exception.Response.GetResponseStream())
                $content = $sr.ReadToEnd()
            } catch { $content = $_.Exception.Message }
        } else {
            $content = $_.Exception.Message
        }
    }
    $sw.Stop()

    $ok = $Expect -contains $status
    $mark = if ($ok) { 'ok  ' } else { 'FAIL' }
    $secs = [math]::Round($sw.Elapsed.TotalSeconds, 2)
    Write-Host ("  {0} {1,-38} {2}  {3,6}s" -f $mark, $Label, $status, $secs)
    if ($content) {
        $trimmed = ($content -replace '\s+', ' ').Trim()
        if ($trimmed.Length -gt 150) { $trimmed = $trimmed.Substring(0, 150) + '...' }
        Write-Host ("       {0}" -f $trimmed) -ForegroundColor DarkGray
    }
    return [pscustomobject]@{ Label = $Label; Status = $status; Ok = $ok; Body = $content }
}

$results = New-Object System.Collections.Generic.List[object]

Write-Host ''
Write-Host 'IDENTITY  :8080' -ForegroundColor Cyan
$results.Add((Invoke-Check -Label 'GET  /health' -Method GET -Uri "$IDENTITY/health" -Headers @{}))
$results.Add((Invoke-Check -Label 'GET  /.well-known/jwks.json' -Method GET -Uri "$IDENTITY/.well-known/jwks.json" -Headers @{}))
# 401 is the CORRECT answer here: the identity_context schema has no principals,
# so there is no credential to match. It proves the envelope was accepted, the
# Supabase query ran, and argon2 refused — the whole login path bar a real user.
$results.Add((Invoke-Check -Label 'POST /v1/authenticate (no such user)' -Method POST -Uri "$IDENTITY/v1/authenticate" `
    -Headers (New-Envelope -Operation 'AUTHENTICATE' -Write) `
    -Body (@{ tenant_id = $TenantId; email = 'nobody@zoikogroup.com'; password = 'not-a-real-password' } | ConvertTo-Json -Compress) `
    -Expect @(401)))

Write-Host ''
Write-Host 'TENANT REGISTRY  :8081' -ForegroundColor Cyan
$results.Add((Invoke-Check -Label 'GET  /healthz' -Method GET -Uri "$TENANT/healthz" -Headers @{}))
$results.Add((Invoke-Check -Label 'GET  /readyz (touches the database)' -Method GET -Uri "$TENANT/readyz" -Headers @{}))
$results.Add((Invoke-Check -Label 'GET  /v1/residency-regions' -Method GET -Uri "$TENANT/v1/residency-regions" `
    -Headers (New-Envelope -Operation 'RESIDENCY_REGION_LIST')))
# 404 is correct: nothing has created this tenant row. POST /v1/tenants cannot,
# because ProvisionTenant generates a fresh tenant_id while the RLS policy on
# tenants requires the row's tenant_id to equal the caller's app.tenant_id — a
# tenant can never be created by a caller scoped to it. See the README note.
$results.Add((Invoke-Check -Label 'GET  /v1/tenants/{id} (not provisioned)' -Method GET -Uri "$TENANT/v1/tenants/$TenantId" `
    -Headers (New-Envelope -Operation 'TENANT_READ') -Expect @(404)))

Write-Host ''
Write-Host 'AUTHORIZATION  :8089' -ForegroundColor Cyan
$results.Add((Invoke-Check -Label 'GET  /healthz' -Method GET -Uri "$AUTHZ/healthz" -Headers @{}))
if (-not $SkipWrites) {
    $r = Invoke-Check -Label 'POST /v1/authorize (granted action)' -Method POST -Uri "$AUTHZ/v1/authorize" `
        -Headers (New-Envelope -Operation 'AUTHORIZE' -Write) `
        -Body (@{ principal_id = $PrincipalId; legal_entity_id = $LegalEntityId; action_type = 'GOVERNANCE_DECISION_RECORD' } | ConvertTo-Json -Compress)
    $results.Add($r)
    if ($r.Ok -and $r.Body -notmatch 'GRANTED') {
        Write-Host '       ^ expected GRANTED. Has deployments/supabase/seed-rbac.sql been applied?' -ForegroundColor Yellow
    }
    # The negative case matters as much as the positive one: an ungranted action
    # must be DENIED, or the grant is blanket rather than scoped.
    $results.Add((Invoke-Check -Label 'POST /v1/authorize (ungranted action)' -Method POST -Uri "$AUTHZ/v1/authorize" `
        -Headers (New-Envelope -Operation 'AUTHORIZE' -Write) `
        -Body (@{ principal_id = $PrincipalId; legal_entity_id = $LegalEntityId; action_type = 'PAYMENT_APPROVE' } | ConvertTo-Json -Compress)))
}

Write-Host ''
Write-Host 'GOVERNANCE  :8083' -ForegroundColor Cyan
$results.Add((Invoke-Check -Label 'GET  /healthz' -Method GET -Uri "$GOVERNANCE/healthz" -Headers @{}))
$results.Add((Invoke-Check -Label 'GET  /v1/decisions (list)' -Method GET -Uri "$GOVERNANCE/v1/decisions" `
    -Headers (New-Envelope -Operation 'DECISION_LIST')))

if (-not $SkipWrites) {
    $decisionId = [guid]::NewGuid().ToString()
    $decision = @{
        decision_id   = $decisionId
        tenant_id     = $TenantId
        legal_entity_id = $LegalEntityId
        actor_id      = $PrincipalId
        action_type   = 'SMOKE_TEST'
        outcome       = 'PERMITTED'
        rule_basis    = 'smoke-entry-services.ps1'
        correlation_id = 'smoke-write'
        workflow_instance_id = 'wf-smoke'
        causation_id  = 'cause-smoke'
    } | ConvertTo-Json -Compress

    Write-Host '       (a write costs ~40s with no Kafka broker running — see the header)' -ForegroundColor DarkYellow
    $results.Add((Invoke-Check -Label 'POST /v1/decisions (write)' -Method POST -Uri "$GOVERNANCE/v1/decisions" `
        -Headers (New-Envelope -Operation 'DECISION_RECORD' -Write) -Body $decision -Expect @(201)))
    $results.Add((Invoke-Check -Label 'GET  /v1/decisions/{id} (read back)' -Method GET -Uri "$GOVERNANCE/v1/decisions/$decisionId" `
        -Headers (New-Envelope -Operation 'DECISION_READ')))
    # Same decision_id again must answer 200 "already recorded", not create a
    # second row and not error.
    $results.Add((Invoke-Check -Label 'POST /v1/decisions (idempotent repeat)' -Method POST -Uri "$GOVERNANCE/v1/decisions" `
        -Headers (New-Envelope -Operation 'DECISION_RECORD' -Write) -Body $decision -Expect @(200)))
}

$passed = ($results | Where-Object { $_.Ok }).Count
$failed = $results.Count - $passed
Write-Host ''
Write-Host ("{0} passed, {1} failed, {2} checks" -f $passed, $failed, $results.Count) `
    -ForegroundColor $(if ($failed -eq 0) { 'Green' } else { 'Red' })
if ($failed -gt 0) {
    $results | Where-Object { -not $_.Ok } | ForEach-Object {
        Write-Host ("  FAIL {0} -> {1}" -f $_.Label, $_.Status) -ForegroundColor Red
    }
    exit 1
}
