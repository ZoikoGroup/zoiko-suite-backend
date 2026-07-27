# ZoikoSuite — Lightweight native demo launcher
#
# Runs the 5 services needed for the "governed approval flow" CTO demo
# (identity -> authorization+SoD -> workflow, plus jurisdiction as a sample
# read-only backend) as plain native Go processes, instead of Docker
# containers. Only Postgres + Redis run in Docker (light, standard images) —
# reusing the existing docker-compose.yml so all per-service databases and
# migrations are applied automatically on first boot.
#
# This intentionally does NOT start Traefik/gateway-auth-svc-behind-a-proxy —
# Traefik's Docker-label discovery only sees Docker containers, not native
# processes, so the "gateway blocks unauthenticated traffic" demo needs the
# full Docker stack (see README). This script covers everything else:
# identity resolution, the authorization/SoD decision, and the workflow
# orchestration — the core "why this platform exists" story.
#
# Usage:
#   cd deployments
#   .\demo-native.ps1
#
# Then open Postman and hit the services on their normal ports:
#   identity-context-svc   http://localhost:8080
#   jurisdiction-rules-svc http://localhost:8082
#   authorization-svc      http://localhost:8089
#   workflow-svc           http://localhost:8090
#   gateway-auth-svc       http://localhost:8092
#
# Stop everything: close the 5 opened PowerShell windows, then
#   docker compose stop postgres redis

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$deployDir = $PSScriptRoot

Write-Host "=== 1. Starting Postgres + Redis (Docker) ===" -ForegroundColor Cyan
Set-Location $deployDir
docker compose up -d postgres redis
if ($LASTEXITCODE -ne 0) { throw "docker compose up failed" }

Write-Host "Waiting for Postgres to become healthy..." -ForegroundColor Cyan
$deadline = (Get-Date).AddSeconds(60)
do {
    Start-Sleep -Seconds 2
    $status = docker inspect --format='{{.State.Health.Status}}' zoiko-postgres 2>$null
    if ($status -eq "healthy") { break }
} while ((Get-Date) -lt $deadline)
if ($status -ne "healthy") { throw "Postgres did not become healthy in time" }
Write-Host "Postgres is healthy." -ForegroundColor Green

Write-Host "=== 2. Generating a throwaway RSA signing key for identity-context-svc ===" -ForegroundColor Cyan
$keyDir = Join-Path $env:TEMP "zoiko-demo-keys"
New-Item -ItemType Directory -Force -Path $keyDir | Out-Null
$keyPath = Join-Path $keyDir "envelope_signing_key.pem"
if (-not (Test-Path $keyPath)) {
    & openssl genrsa -out $keyPath 2048 2>$null
    Write-Host "Generated $keyPath" -ForegroundColor Green
} else {
    Write-Host "Reusing existing key at $keyPath" -ForegroundColor Yellow
}

Write-Host "=== 3. Launching services natively (one window each) ===" -ForegroundColor Cyan

function Start-Service {
    param(
        [string]$Name,
        [string]$Dir,
        [hashtable]$EnvVars
    )
    $envLines = ($EnvVars.GetEnumerator() | ForEach-Object { "`$env:$($_.Key)='$($_.Value)'" }) -join "; "
    $cmd = "cd '$Dir'; $envLines; Write-Host 'Starting $Name...' -ForegroundColor Cyan; go run ./cmd/server"
    Start-Process powershell -ArgumentList "-NoExit", "-Command", $cmd
    Write-Host "  launched: $Name" -ForegroundColor Green
}

Start-Service -Name "identity-context-svc (:8080)" -Dir (Join-Path $repoRoot "services\identity-context-svc") -EnvVars @{
    PORT                          = "8080"
    DB_HOST                       = "localhost"
    DB_NAME                       = "identity_context"
    DB_USER                       = "postgres"
    DB_PASSWORD                   = "postgres"
    DB_SSLMODE                    = "disable"
    REDIS_HOST                    = "localhost"
    JWT_SIGNING_SECRET            = "local-dev-jwt-signing-secret-key-32-chars-long"
    JWT_SIGNING_PRIVATE_KEY_PATH  = $keyPath
    JWT_KEY_ID                    = "local-dev-key-1"
}

Start-Service -Name "jurisdiction-rules-svc (:8082)" -Dir (Join-Path $repoRoot "services\jurisdiction-rules-svc") -EnvVars @{
    PORT             = "8082"
    DB_HOST          = "localhost"
    DB_NAME          = "jurisdiction_rules"
    DB_USER          = "postgres"
    DB_PASSWORD      = "postgres"
    DB_SSLMODE       = "disable"
    AUTHZ_SERVICE_URL = "http://localhost:8089"
}

Start-Service -Name "authorization-svc (:8089)" -Dir (Join-Path $repoRoot "services\authorization-svc") -EnvVars @{
    PORT                   = "8089"
    DB_HOST                = "localhost"
    DB_NAME                = "authorization_svc"
    DB_USER                = "postgres"
    DB_PASSWORD            = "postgres"
    DB_SSLMODE             = "disable"
    JURISDICTION_RULES_URL = "http://localhost:8082"
}

Start-Service -Name "workflow-svc (:8090)" -Dir (Join-Path $repoRoot "services\workflow-svc") -EnvVars @{
    PORT                     = "8090"
    DB_HOST                  = "localhost"
    DB_NAME                  = "workflow"
    DB_USER                  = "postgres"
    DB_PASSWORD              = "postgres"
    DB_SSLMODE               = "disable"
    AUTHORIZATION_SERVICE_URL = "http://localhost:8089"
}

Start-Service -Name "gateway-auth-svc (:8092)" -Dir (Join-Path $repoRoot "services\gateway-auth-svc") -EnvVars @{
    PORT              = "8092"
    IDENTITY_JWKS_URL = "http://localhost:8080/.well-known/jwks.json"
}

Write-Host ""
Write-Host "=== All 5 services launching in separate windows. ===" -ForegroundColor Cyan
Write-Host "Give them ~10-15s to connect to Postgres/Redis, then check:" -ForegroundColor Yellow
Write-Host "  curl http://localhost:8080/health"
Write-Host "  curl http://localhost:8082/healthz"
Write-Host "  curl http://localhost:8089/healthz"
Write-Host "  curl http://localhost:8090/healthz"
Write-Host "  curl http://localhost:8092/healthz"
Write-Host ""
Write-Host "Then drive the demo from Postman against these same localhost ports." -ForegroundColor Cyan
