<#
.SYNOPSIS
    Register the admin console's demo tenant and legal entity in
    tenant-entity-registry-svc.

.DESCRIPTION
    A thin wrapper over seed-demo-registry.sql, which carries the full rationale.
    In short: zoiko-suite-fe's DEMO_IDENTITY names a fixed tenant and legal entity,
    every write in this platform authorizes against that legal entity, and nothing
    had ever registered either of them.

    Run this ALONGSIDE seed-demo-rbac.ps1 on a fresh stack. They answer two
    different questions and a console needs both:

        seed-demo-registry.ps1  ->  does the demo legal entity exist?
        seed-demo-rbac.ps1      ->  may the demo principal act on it?

    Nothing enforced the first until accounts-receivable-svc began reconciling the
    legal entity on a write, which is why this script is newer than the other.

    Idempotent. Re-running changes nothing.

.PARAMETER Container
    The Postgres container to run psql in. Defaults to the compose container name.

.EXAMPLE
    ./seed-demo-registry.ps1
#>
param(
    [string] $Container = "zoiko-postgres",
    [string] $Database = "tenant_entity_registry",
    [string] $User = "postgres"
)

$ErrorActionPreference = "Stop"

$sqlPath = Join-Path $PSScriptRoot "seed-demo-registry.sql"
if (-not (Test-Path $sqlPath)) {
    throw "Cannot find seed-demo-registry.sql next to this script (looked in $PSScriptRoot)."
}

# Fail with something actionable rather than a raw docker error.
$running = docker ps --filter "name=$Container" --format "{{.Names}}"
if ($running -notcontains $Container) {
    throw "Container '$Container' is not running. Start the stack first: docker compose up -d postgres"
}

Write-Host "Seeding the demo tenant and legal entity into $Database (via $Container)" -ForegroundColor Cyan

# -v ON_ERROR_STOP=1 so a failure is a failure. Piped rather than passed with -f
# because the file lives on the host, not in the container.
Get-Content -Raw $sqlPath | docker exec -i $Container psql -v ON_ERROR_STOP=1 -U $User -d $Database
if ($LASTEXITCODE -ne 0) {
    throw "psql exited $LASTEXITCODE — the demo tenant and entity were NOT seeded."
}

Write-Host ""
Write-Host "Done. The row printed above is the exact lookup accounts-receivable-svc" -ForegroundColor Green
Write-Host "performs on every write; an empty result there means the seed did not take." -ForegroundColor Green
Write-Host "Now run seed-demo-rbac.ps1 if you have not already." -ForegroundColor Green
