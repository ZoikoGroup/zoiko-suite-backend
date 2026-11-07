# Runs public-ai-svc for local development on the dedicated Postgres
# (database public_ai, role public_ai_app). Reads PUBLIC_AI_DATABASE_URL and
# any other settings from the gitignored .env.local; nothing secret is printed.
# Values already set in the terminal take precedence, e.g. for Groq:
#   $env:GROQ_API_KEY = '<key>'; $env:MODEL_PROVIDER = 'groq'
# Dev/test only: seeds and activates the public KB on start.
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
$envFile = Join-Path (Get-Location) '.env.local'
if (-not (Test-Path $envFile)) { throw '.env.local not found: create it with PUBLIC_AI_DATABASE_URL=postgres://...' }
foreach ($line in Get-Content $envFile) {
    if ($line -match '^\s*#' -or $line -notmatch '=') { continue }
    $name, $value = $line -split '=', 2
    # A value already set in this terminal wins, so a key can stay out of files.
    if (-not (Test-Path ("env:" + $name.Trim()))) { Set-Item -Path ("env:" + $name.Trim()) -Value $value.Trim() }
}
if (-not $env:PUBLIC_AI_ENABLED) { $env:PUBLIC_AI_ENABLED = 'true' }
if (-not $env:PUBLIC_AI_SEED_ON_START) { $env:PUBLIC_AI_SEED_ON_START = 'true' }
if (-not $env:PUBLIC_AI_DATASET) { $env:PUBLIC_AI_DATASET = 'public-kb' }
if (-not $env:PUBLIC_AI_HTTP_ADDR) { $env:PUBLIC_AI_HTTP_ADDR = '127.0.0.1:8001' }
Write-Host "public-ai-svc on $env:PUBLIC_AI_HTTP_ADDR, Postgres, dataset $env:PUBLIC_AI_DATASET, model $(if ($env:MODEL_PROVIDER) { $env:MODEL_PROVIDER } else { 'mock' })"
go run ./cmd/server
