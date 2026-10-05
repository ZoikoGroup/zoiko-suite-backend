# scratch/test_transfer_live.ps1
# Comprehensive Live Verification Suite for privacy-transfer-svc (PRV-05 : 8155)
$ErrorActionPreference = "Stop"

$baseUrl = "http://localhost:8155"
$tenantId = "11111111-1111-1111-1111-111111111111"
$principalId = "33333333-3333-3333-3333-333333333333"

$headers = @{
    "X-Tenant-Id"      = $tenantId
    "X-Principal-Id"   = $principalId
    "X-Correlation-Id" = "corr-transfer-test-001"
}

Write-Host "=== 1. Checking Health & Readiness ==="
$health = Invoke-RestMethod -Uri "$baseUrl/healthz" -Method Get
$ready = Invoke-RestMethod -Uri "$baseUrl/readyz" -Method Get
Write-Host "Healthz:" $health.status "Readyz:" $ready.status
if ($health.status -ne "ok" -or $ready.status -ne "ready") {
    Write-Error "Health check failed"
}

Write-Host "`n=== 2. Create Governed Transfer Mechanism (Canonical & Idempotency) ==="
$idemKeyMech = "idem-mech-" + [guid]::NewGuid().ToString()
$mechHeaders = $headers.Clone()
$mechHeaders["Idempotency-Key"] = $idemKeyMech

$mechBody = @{
    mechanism_type = "STANDARD_CONTRACTUAL_CLAUSES"
    evidence_ref   = "s3://contracts/scc-2026-eu-us.pdf"
    conditions     = "ENCRYPTED_IN_TRANSIT_AND_REST"
    valid_from     = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
} | ConvertTo-Json

$mechResp = Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/privacy/transfer-mechanisms" -Method Post -Headers $mechHeaders -Body $mechBody -ContentType "application/json"
Write-Host "Mechanism created. Status:" $mechResp.StatusCode
$mech = $mechResp.Content | ConvertFrom-Json
$mechId = $mech.mechanism_id
Write-Host "Mechanism ID:" $mechId

Write-Host "`n=== 3. Idempotent Replay on Transfer Mechanism ==="
$mechReplay = Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/privacy/transfer-mechanisms" -Method Post -Headers $mechHeaders -Body $mechBody -ContentType "application/json"
Write-Host "Replay Status:" $mechReplay.StatusCode "Idempotency-Replay header:" $mechReplay.Headers["Idempotency-Replay"]
if ($mechReplay.Headers["Idempotency-Replay"] -ne "true") {
    Write-Error "Expected Idempotency-Replay: true header"
}

Write-Host "`n=== 4. Idempotency Conflict on Transfer Mechanism (409 Conflict) ==="
$conflictBody = @{
    mechanism_type = "BINDING_CORPORATE_RULES"
    evidence_ref   = "s3://contracts/bcr-conflicting.pdf"
} | ConvertTo-Json
try {
    $conflictResp = Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/privacy/transfer-mechanisms" -Method Post -Headers $mechHeaders -Body $conflictBody -ContentType "application/json"
    Write-Error "Expected 409 Conflict, but succeeded with $($conflictResp.StatusCode)"
} catch {
    $statusCode = $_.Exception.Response.StatusCode.value__
    Write-Host "Got expected error response. StatusCode:" $statusCode
    if ($statusCode -ne 409) {
        Write-Error "Expected 409 Conflict, got $statusCode"
    }
}

Write-Host "`n=== 5. Create Processor Relationship ==="
$relBody = @{
    controller_ref          = "zoiko-enterprise-uk"
    processor_ref           = "aws-cloud-services-us"
    service                 = "cloud-infrastructure-hosting"
    processing_instructions = "Process only customer support telemetry per standard agreement."
    jurisdictions           = @("GB", "US")
    data_categories         = @("COMMERCIAL_USAGE_METRICS")
    subject_classes         = @("ENTERPRISE_USERS")
} | ConvertTo-Json

$relResp = Invoke-RestMethod -Uri "$baseUrl/privacy/processor-relationships" -Method Post -Headers $headers -Body $relBody -ContentType "application/json"
$relId = $relResp.relationship_id
Write-Host "Relationship created. ID:" $relId "Status:" $relResp.status

Write-Host "`n=== 6. Attach Subprocessor ==="
$subBody = @{
    provider_identity     = "datadog-observability"
    service               = "apm-monitoring"
    data_scope            = "APPLICATION_LOGS"
    processing_locations  = @("US-EAST-1")
    onward_subprocessors  = @()
} | ConvertTo-Json
$subResp = Invoke-RestMethod -Uri "$baseUrl/privacy/processor-relationships/$relId/subprocessors" -Method Post -Headers $headers -Body $subBody -ContentType "application/json"
Write-Host "Subprocessor attached. ID:" $subResp.subprocessor_id

Write-Host "`n=== 7. Record Transfer Assessment with Structured Measures (/v1/ route) ==="
$assessBody = @{
    relationship_id         = $relId
    outcome                 = "APPROVE"
    residual_risk           = "LOW"
    evidence_ref            = "s3://evidence/tia-aws-2026.pdf"
    government_access_risk  = "FISA_702_MITIGATED_BY_E2E_ENCRYPTION"
    technical_measures      = "AES_256_GCM_ENCRYPTION_WITH_CUSTOMER_MANAGED_KEYS"
    organizational_measures = "MANDATORY_LEGAL_CHALLENGE_FOR_FOREIGN_GOVERNMENT_DISCLOSURE_REQUESTS"
    review_trigger_at       = (Get-Date).AddYears(1).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
} | ConvertTo-Json

$assessResp = Invoke-RestMethod -Uri "$baseUrl/v1/privacy/transfer-assessments" -Method Post -Headers $headers -Body $assessBody -ContentType "application/json"
Write-Host "Assessment recorded. ID:" $assessResp.assessment_id "Outcome:" $assessResp.outcome
Write-Host "Measures: GovAccess=" $assessResp.government_access_risk "Tech=" $assessResp.technical_measures

Write-Host "`n=== 8a. Evaluate Transfer Decision (CONDITIONAL due to measures) ==="
$evalBody = @{
    relationship_id          = $relId
    transfer_mechanism_id    = $mechId
    destination_jurisdiction = "US"
    assessment_required      = $true
} | ConvertTo-Json

$evalResp = Invoke-RestMethod -Uri "$baseUrl/privacy/transfer-decisions" -Method Post -Headers $headers -Body $evalBody -ContentType "application/json"
Write-Host "Evaluation Result:" $evalResp.result "AssessmentID:" $evalResp.assessment_id "Conditions:" $evalResp.conditions
if ($evalResp.result -ne "CONDITIONAL") {
    Write-Error "Expected CONDITIONAL, got $($evalResp.result)"
}

Write-Host "`n=== 8b. Evaluate Transfer Decision (AUTHORIZED without assessment) ==="
$evalAuthBody = @{
    relationship_id          = $relId
    transfer_mechanism_id    = $mechId
    destination_jurisdiction = "US"
    assessment_required      = $false
} | ConvertTo-Json

$evalAuthResp = Invoke-RestMethod -Uri "$baseUrl/privacy/transfer-decisions" -Method Post -Headers $headers -Body $evalAuthBody -ContentType "application/json"
Write-Host "Evaluation Result:" $evalAuthResp.result
if ($evalAuthResp.result -ne "AUTHORIZED") {
    Write-Error "Expected AUTHORIZED, got $($evalAuthResp.result)"
}

Write-Host "`n=== 9. Verification Successful! All Live Checks Passed! ==="

