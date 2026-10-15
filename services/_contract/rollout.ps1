#!/usr/bin/env pwsh
# Push the canonical input contract (ZS-ARCH-SVC-001 v2.0 §4) into every service.
#
# services/ has no shared Go module -- each service is its own zoiko.io/<svc> with
# no replace directives -- so the package is vendored rather than imported. That is
# the pattern the repo already uses for tenant scope (internal/middleware/tenant.go
# is duplicated across 73 services). This script is what keeps the copies identical:
# edit services/_contract/envelope/, then re-run it. Never edit a vendored copy.
#
#   ./rollout.ps1            vendor into every service and regenerate contract.go
#   ./rollout.ps1 <svc>...   only the named services
#
# Wiring the middleware into each router is deliberately NOT done here -- main.go
# differs enough between services that a scripted edit would be guesswork. Run
# rollout.ps1, then wire each service's router with the two lines printed at the end.

$ErrorActionPreference = "Stop"

$SRC = Join-Path $PSScriptRoot "envelope"
$SERVICES_DIR = Join-Path $PSScriptRoot ".."

# Entity-scoped services: their records identify an authoritative legal entity, so
# §4 makes legal_entity_id mandatory for them (INV-02). Derived from which services
# already carry legal_entity_id in internal/domain -- this reflects the platform as
# built, not an aspiration.
$ENTITY_SCOPED = @(
    "access-control-svc", "accounts-payable-svc", "accounts-receivable-svc",
    "anomaly-detection-svc", "asset-management-svc", "authorization-svc", "bank-reconciliation-svc", "banking-connector-svc",
    "benefits-svc", "board-resolutions-svc", "carta-svc", "clause-template-svc", "comments-collaboration-svc", "commercial-account-svc",
    "compensation-svc", "compliance-risk-scoring-svc", "compliance-status-svc",
    "connectivity-api-bridge-svc", "consolidation-svc", "contract-lifecycle-svc",
    "corporate-actions-svc", "corporate-tax-svc", "counterparty-management-svc", "decision-support-svc",
    "delegated-authority-svc", "document-vault-svc", "employee-master-svc", "employment-contracts-svc",
    "esignature-integration-svc", "evidence-manifest-svc", "evidence-requirements-svc",
    "exception-escalation-svc", "external-data-feed-svc", "filing-preparation-svc", "filing-tracker-svc",
    "financial-close-svc", "financial-control-svc", "forecasting-svc", "general-ledger-svc", "governance-decision-log-svc",
    "hris-connector-svc", "identity-context-svc", "inventory-management-svc", "invoice-approval-svc", "key-management-svc",
    "leave-absence-svc", "migration-integrity-svc", "mtls-management-svc", "notification-svc",
    "obligation-tracking-svc", "obligations-svc", "offboarding-severance-svc", "org-structure-svc",
    "payroll-run-svc", "payroll-tax-svc", "performance-review-svc", "policy-svc", "procurement-workflow-svc",
    "project-accounting-svc", "purchase-order-svc", "purchase-request-svc", "reconciliation-intelligence-svc",
    "reporting-orchestration-svc", "siem-integration-svc", "spend-controls-svc",
    "tax-authority-interface-svc", "tax-determination-svc", "treasury-svc",
    "vat-gst-svc", "vendor-due-diligence-svc", "withholding-tax-svc", "workflow-svc", "workforce-compliance-svc"
)

# Services whose reads touch personal, bank, tax, payroll or privileged content.
# §4 requires purpose_context "for governed sensitive access", and §15 (INV-15)
# forbids emitting that content into telemetry -- capturing WHY it was accessed is
# what makes the access reviewable afterwards.
$SENSITIVE = @(
    "document-vault-svc", "comments-collaboration-svc", "employee-master-svc", "compensation-svc", "benefits-svc",
    "payroll-run-svc", "payroll-tax-svc", "payroll-exceptions-svc", "offboarding-severance-svc",
    "leave-absence-svc", "performance-review-svc", "employment-contracts-svc", "carta-svc",
    "key-management-svc", "secret-vault-integration-svc", "mtls-management-svc",
    "counterparty-management-svc", "vendor-due-diligence-svc", "banking-connector-svc",
    "treasury-svc", "corporate-tax-svc", "vat-gst-svc", "withholding-tax-svc", "tax-determination-svc",
    "tax-authority-interface-svc", "evidence-manifest-svc", "governance-decision-log-svc",
    "audit-event-store-svc", "hris-connector-svc", "search-indexer-svc"
)

# Services that post to, or report from, an accounting book (INV-03).
$ACCOUNTING = @(
    "general-ledger-svc", "accounts-payable-svc", "accounts-receivable-svc",
    "asset-management-svc", "consolidation-svc", "intercompany-accounting-svc", "financial-close-svc", "financial-control-svc", "migration-integrity-svc",
    "inventory-management-svc", "project-accounting-svc",
    "reporting-orchestration-svc", "metric-registry-svc", "corporate-tax-svc", "vat-gst-svc",
    "withholding-tax-svc", "tax-determination-svc", "treasury-svc",
    "bank-reconciliation-svc", "reconciliation-intelligence-svc"
)

# Paths that must bypass the contract because they PRODUCE its mandatory fields
# rather than consume them.
$EXEMPT_PATHS = @(
    "gateway-auth-svc:/verify",
    "identity-context-svc:/v1/authenticate"
)

# Services whose POST routes resolve or evaluate but change nothing (a body-
# carrying read). They still get the full envelope; only the idempotency_key
# obligation drops, because the material-write gate would otherwise demand one
# for a read that can safely be repeated.
$POST_READ = @(
    "configuration-feature-flag-svc:/v1/config/resolve",
    "configuration-feature-flag-svc:/v1/flags/*/evaluate"
)

function In-List($needle, $hay) {
    foreach ($item in $hay) {
        if ($item -eq $needle) { return $true }
    }
    return $false
}

function Get-ExemptLiteral($svc) {
    foreach ($entry in $EXEMPT_PATHS) {
        if ($entry.Split(':')[0] -eq $svc) {
            $paths = $entry.Substring($entry.IndexOf(':') + 1)
            $out = ""
            foreach ($p in $paths.Split(',')) {
                $out += "`"$p`", "
            }
            return $out.TrimEnd(", ")
        }
    }
    return $null
}

$targets = @()
if ($args.Count -gt 0) {
    $targets = $args
} else {
    foreach ($d in Get-ChildItem $SERVICES_DIR -Directory) {
        $name = $d.Name
        if ($name -eq "_contract") { continue }
        if ($name -eq "search-client") { continue } # library module with no HTTP server
        if (-not (Test-Path (Join-Path $SERVICES_DIR $name "go.mod"))) { continue }
        $targets += $name
    }
}

$SRC = Join-Path $PSScriptRoot "envelope"
$SERVICES_DIR = (Get-Item $SERVICES_DIR).FullName

foreach ($svc in $targets) {
    $dest = Join-Path (Join-Path $SERVICES_DIR $svc) "internal\envelope"
    if (-not (Test-Path (Join-Path (Join-Path $SERVICES_DIR $svc) "go.mod"))) {
        Write-Host "skip $svc (no go.mod)" -ForegroundColor Yellow
        continue
    }
    if (-not (Test-Path $dest)) {
        New-Item -ItemType Directory -Path $dest -Force | Out-Null
    }

    # Copy envelope files
    Copy-Item (Join-Path $SRC "envelope.go") $dest -Force
    Copy-Item (Join-Path $SRC "policy.go") $dest -Force
    Copy-Item (Join-Path $SRC "middleware.go") $dest -Force
    Copy-Item (Join-Path $SRC "resolver.go") $dest -Force
    Copy-Item (Join-Path $SRC "reporter.go") $dest -Force
    Copy-Item (Join-Path $SRC "reason.go") $dest -Force
    Copy-Item (Join-Path $SRC "fingerprint.go") $dest -Force
    Copy-Item (Join-Path $SRC "transition.go") $dest -Force

    $legal_entity = "NotRequired"
    if (In-List $svc $ENTITY_SCOPED) { $legal_entity = "RequiredOnWrite" }

    $purpose = "NotRequired"
    if (In-List $svc $SENSITIVE) { $purpose = "RequiredOnWrite" }

    $exempt_literal = Get-ExemptLiteral $svc
    $exempt_block = ""
    if ($exempt_literal) {
        $exempt_block = @"
        // Endpoints that produce the envelope's mandatory fields rather than
        // consume them, so requiring those fields as input would be circular.
        // See EXEMPT_PATHS in rollout.ps1.
        ExemptPaths: []string{$exempt_literal},
"@
    }

    $book_note = "// This service does not post to an accounting book."
    if (In-List $svc $ACCOUNTING) {
        $book_note = @"
// BookID stays NotRequired despite §4 marking book_id mandatory for
// accounting actions: REF-06 Accounting Book / Ledger Basis does not exist in
// services/, so no book_id can be issued or validated. See rollout.ps1.
// Flip to Required once REF-06 ships."
    }

    $go_imports = ""
    $read_post_block = ""
    if (In-List $svc $POST_READ) {
        $go_imports = @"
import (
    "net/http"
    "strings"
)
"@
        $read_post_block = @"
        // POST endpoints that resolve or evaluate and change nothing get the
        // full envelope except the idempotency_key, which the material-write
        // gate would otherwise demand for a read that can safely be repeated.
        // A resolve/evaluate can be repeated safely. Matches POST_READ in rollout.ps1.
        MaterialWrite: func(r *http.Request) bool {
            switch {
            case r.URL.Path == "/v1/config/resolve":
                return \$false
            case strings.HasPrefix(r.URL.Path, "/v1/flags/") && strings.HasSuffix(r.URL.Path, "/evaluate"):
                return \$false
            }
            return defaultMaterialWrite(r)
        },
"@
    }

$contract = @"
// Code generated by services/_contract/rollout.ps1. DO NOT EDIT.
//
// Regenerate with: services/_contract/rollout.ps1 $svc

package envelope

// ServicePolicy is this service's §4 conditional-field policy.
//
// The unconditionally mandatory fields (tenant_id, actor_subject_id,
// request_id, correlation_id, source_channel) and idempotency_key on material
// writes are enforced for every service and are not expressible here.
func ServicePolicy() Policy {
    return Policy{
        ServiceName: "$svc",

        // §4: mandatory for entity-specific records (INV-02).
        LegalEntityID: $legal_entity,

        // §4: required for governed sensitive access.
        PurposeContext: $purpose,

        $book_note
        BookID: NotRequired,
$exempt_block$read_post_block    }
}
"@
    $contractPath = Join-Path $dest "contract.go"
    [IO.File]::WriteAllText($contractPath, $contract)

    # Format the generated file
    & gofmt -w (Join-Path (Join-Path $SERVICES_DIR $svc) "internal/envelope/")
    Write-Host "vendored -> services/$svc/internal/envelope/"
}

# Print wiring instructions
Write-Host ""
Write-Host "Vendored. Now wire each service's router (cmd/server/main.go), after the"
Write-Host "existing chi middleware and before the routes:"
Write-Host ""
Write-Host "    import svcenvelope `"zoiko.io/<svc>/internal/envelope`""
Write-Host ""
Write-Host "    r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(),"
Write-Host "        func(req *http.Request, e svcenvelope.Envelope, err *svcenvelope.ValidationError) {"
Write-Host "            logger.Warn(`"canonical input contract violated`","
Write-Host "                zap.String(`"operation`", e.Operation),"
Write-Host "                zap.String(`"correlation_id`", e.CorrelationID),"
Write-Host "                zap.Strings(`"missing`", svcenvelope.Fields(err)))"
Write-Host "        }))"
Write-Host ""
Write-Host "Enforcement mode comes from ZS_ENVELOPE_ENFORCEMENT (strict | write-strict |"
Write-Host "observe), defaulting to write-strict."