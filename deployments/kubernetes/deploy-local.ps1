# deploy-local.ps1
# Automates the setup, build, deployment, and verification of the ZoikoSuite base Kubernetes platform in a local Kind cluster.

$ErrorActionPreference = "Stop"

# ── 1. Ensure Docker Desktop is Running ───────────────────────────────────────
Write-Host "Checking Docker status..." -ForegroundColor Cyan
try {
    & docker ps > $null 2>&1
    Write-Host "Docker is running." -ForegroundColor Green
} catch {
    Write-Host "Docker is not running. Attempting to start Docker Desktop..." -ForegroundColor Yellow
    $dockerPath = "C:\Program Files\Docker\Docker\Docker Desktop.exe"
    if (Test-Path $dockerPath) {
        Start-Process $dockerPath
        Write-Host "Waiting for Docker Desktop to boot..." -ForegroundColor Yellow
        $timeout = 60
        $elapsed = 0
        while ($elapsed -lt $timeout) {
            try {
                & docker ps > $null 2>&1
                Write-Host "Docker is now ready!" -ForegroundColor Green
                break
            } catch {
                Start-Sleep -Seconds 5
                $elapsed += 5
            }
        }
        if ($elapsed -ge $timeout) {
            throw "Docker failed to start within $timeout seconds."
        }
    } else {
        throw "Docker Desktop not found at $dockerPath. Please start Docker manually."
    }
}

# ── 2. Locate or Install Kind ───────────────────────────────────────────────
Write-Host "Locating Kind executable..." -ForegroundColor Cyan
$kindCmd = "kind"
if (-not (Get-Command kind -ErrorAction SilentlyContinue)) {
    $localKindPath = Join-Path $PSScriptRoot "kind.exe"
    if (Test-Path $localKindPath) {
        $kindCmd = $localKindPath
        Write-Host "Found local Kind binary: $kindCmd" -ForegroundColor Green
    } else {
        Write-Host "Kind not found in PATH or locally. Downloading Kind v0.32.0 standalone binary..." -ForegroundColor Yellow
        $url = "https://github.com/kubernetes-sigs/kind/releases/download/v0.32.0/kind-windows-amd64"
        Invoke-WebRequest -Uri $url -OutFile $localKindPath -UseBasicParsing
        $kindCmd = $localKindPath
        Write-Host "Kind standalone binary downloaded successfully to: $kindCmd" -ForegroundColor Green
    }
} else {
    Write-Host "Kind is installed in PATH: $(kind version)" -ForegroundColor Green
}

# ── 3. Create Kind Kubernetes Cluster ─────────────────────────────────────────
Write-Host "Checking for existing Kind cluster..." -ForegroundColor Cyan
$clusters = & $kindCmd get clusters
if ($clusters -contains "zoiko-cluster") {
    Write-Host "Cluster 'zoiko-cluster' already exists. Re-using current cluster context." -ForegroundColor Green
} else {
    Write-Host "Creating local Kind cluster 'zoiko-cluster'..." -ForegroundColor Yellow
    & $kindCmd create cluster --name zoiko-cluster
}

# Ensure Kubectl is set to the correct context
kubectl config use-context kind-zoiko-cluster | Out-Null
Write-Host "Active Kubernetes Context: $(kubectl config current-context)" -ForegroundColor Green

# ── 4. Build Application Docker Images ────────────────────────────────────────
Write-Host "Building Docker images for 6 services..." -ForegroundColor Cyan

$services = @{
    "identity-svc"         = "services/identity-context-svc"
    "tenant-svc"           = "services/tenant-entity-registry-svc"
    "jurisdiction-svc"     = "services/jurisdiction-rules-svc"
    "governance-svc"       = "services/governance-decision-log-svc"
    "audit-svc"            = "services/audit-event-store-svc"
    "policy-svc"           = "services/policy-svc"
}

foreach ($svcName in $services.Keys) {
    $svcPath = $services[$svcName]
    Write-Host "Building image: $svcName:latest from $svcPath..." -ForegroundColor Yellow
    docker build -t "$svcName:latest" "$svcPath"
}
Write-Host "Docker images built successfully." -ForegroundColor Green

# ── 5. Load Images into Kind Cluster ──────────────────────────────────────────
Write-Host "Loading Docker images into Kind cluster..." -ForegroundColor Cyan
foreach ($svcName in $services.Keys) {
    Write-Host "Loading $svcName:latest into cluster..." -ForegroundColor Yellow
    & $kindCmd load docker-image "$svcName:latest" --name zoiko-cluster
}
Write-Host "Docker images loaded into Kind." -ForegroundColor Green

# ── 6. Setup Namespaces and ConfigMaps ────────────────────────────────────────
Write-Host "Applying Namespaces..." -ForegroundColor Cyan
kubectl apply -f deployments/kubernetes/manifests/00-namespaces.yaml

Write-Host "Generating Migration ConfigMaps for PostgreSQL..." -ForegroundColor Cyan
# Generate configmaps dynamically from folder contents, ensuring idempotency using dry-run output piped to apply
kubectl create configmap postgres-init-script -n zoiko-infra --from-file=init-db.sh=deployments/init-db.sh --dry-run=client -o yaml | kubectl apply -f -

$migrationConfigs = @{
    "audit-migrations"        = "services/audit-event-store-svc/deployments/migrations"
    "identity-migrations"     = "services/identity-context-svc/deployments/migrations"
    "tenant-migrations"       = "services/tenant-entity-registry-svc/deployments/migrations"
    "jurisdiction-migrations" = "services/jurisdiction-rules-svc/deployments/migrations"
    "governance-migrations"   = "services/governance-decision-log-svc/deployments/migrations"
    "policy-migrations"       = "services/policy-svc/deployments/migrations"
}

foreach ($cmName in $migrationConfigs.Keys) {
    $path = $migrationConfigs[$cmName]
    Write-Host "Creating ConfigMap $cmName in zoiko-infra..." -ForegroundColor Yellow
    kubectl create configmap "$cmName" -n zoiko-infra --from-file="$path" --dry-run=client -o yaml | kubectl apply -f -
}

# Apply Network Policies to isolate namespaces
Write-Host "Applying Network Policies..." -ForegroundColor Cyan
kubectl apply -f deployments/kubernetes/manifests/01-network-policies.yaml

# ── 7. Deploy Infrastructure (Postgres, Redis, Kafka) ─────────────────────────
Write-Host "Deploying database and message queue infrastructure..." -ForegroundColor Cyan
kubectl apply -f deployments/kubernetes/manifests/02-infra-postgres.yaml
kubectl apply -f deployments/kubernetes/manifests/03-infra-redis.yaml
kubectl apply -f deployments/kubernetes/manifests/04-infra-kafka.yaml

# Wait for infrastructure to spin up
Write-Host "Waiting for database and infra components to become healthy..." -ForegroundColor Yellow
kubectl rollout status statefulset/postgres -n zoiko-infra --timeout=120s
kubectl rollout status deployment/redis -n zoiko-infra --timeout=120s
kubectl rollout status deployment/kafka -n zoiko-infra --timeout=180s
Write-Host "Database and messaging infrastructure are ready." -ForegroundColor Green

# ── 8. Deploy Application Services ───────────────────────────────────────────
Write-Host "Deploying Application Services..." -ForegroundColor Cyan
kubectl apply -f deployments/kubernetes/manifests/05-app-identity.yaml
kubectl apply -f deployments/kubernetes/manifests/06-app-tenant.yaml
kubectl apply -f deployments/kubernetes/manifests/07-app-jurisdiction.yaml
kubectl apply -f deployments/kubernetes/manifests/08-app-governance.yaml
kubectl apply -f deployments/kubernetes/manifests/09-app-audit.yaml
kubectl apply -f deployments/kubernetes/manifests/10-app-policy.yaml

# Wait for application services to spin up
Write-Host "Waiting for application pods to roll out..." -ForegroundColor Yellow
kubectl rollout status deployment/identity-svc -n zoiko-identity --timeout=120s
kubectl rollout status deployment/tenant-svc -n zoiko-identity --timeout=120s
kubectl rollout status deployment/jurisdiction-svc -n zoiko-governance --timeout=120s
kubectl rollout status deployment/governance-svc -n zoiko-governance --timeout=120s
kubectl rollout status deployment/audit-svc -n zoiko-evidence --timeout=120s
kubectl rollout status deployment/policy-svc -n zoiko-governance --timeout=120s
Write-Host "All deployments rolled out successfully." -ForegroundColor Green

# ── 9. Verify Deployments ─────────────────────────────────────────────────────
Write-Host "`n=== Running Service Health Verification ===" -ForegroundColor Cyan

$testPorts = @{
    "identity-svc"     = @{ "port" = "8080"; "ns" = "zoiko-identity";   "path" = "health" }
    "tenant-svc"       = @{ "port" = "8081"; "ns" = "zoiko-identity";   "path" = "healthz" }
    "jurisdiction-svc" = @{ "port" = "8082"; "ns" = "zoiko-governance"; "path" = "healthz" }
    "governance-svc"   = @{ "port" = "8083"; "ns" = "zoiko-governance"; "path" = "healthz" }
    "audit-svc"        = @{ "port" = "8084"; "ns" = "zoiko-evidence";   "path" = "healthz" }
    "policy-svc"       = @{ "port" = "8085"; "ns" = "zoiko-governance"; "path" = "healthz" }
}

$pfProcesses = @()

foreach ($svc in $testPorts.Keys) {
    $cfg = $testPorts[$svc]
    $port = $cfg["port"]
    $ns = $cfg["ns"]
    $path = $cfg["path"]
    
    Write-Host "Exposing $svc port $port..." -ForegroundColor Yellow
    # Start port forwarding in background
    $job = Start-Job -ScriptBlock {
        param($s, $p, $n)
        kubectl port-forward "svc/$s" -n "$n" "${p}:${p}"
    } -ArgumentList $svc, $port, $ns
    
    $pfProcesses += $job
}

# Wait for port-forwarding to establish
Start-Sleep -Seconds 5

$success = $true
foreach ($svc in $testPorts.Keys) {
    $cfg = $testPorts[$svc]
    $port = $cfg["port"]
    $path = $cfg["path"]
    $url = "http://localhost:$port/$path"
    
    try {
        Write-Host "Querying health for $svc ($url)..." -ForegroundColor Yellow
        $resp = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 5
        if ($resp.StatusCode -eq 200) {
            Write-Host "  SUCCESS: $svc is healthy! Response: $($resp.Content)" -ForegroundColor Green
        } else {
            Write-Host "  FAILED: $svc returned HTTP status $($resp.StatusCode)" -ForegroundColor Red
            $success = $false
        }
    } catch {
        Write-Host "  FAILED: Could not connect to $svc on port ${port}: $($_.Exception.Message)" -ForegroundColor Red
        $success = $false
    }
}

# Cleanup port forward background jobs
Write-Host "Cleaning up background port-forwarding..." -ForegroundColor Yellow
foreach ($job in $pfProcesses) {
    Stop-Job $job
    Remove-Job $job
}

if ($success) {
    Write-Host "`n=== ZoikoSuite Base Kubernetes Platform Verified Successfully! ===" -ForegroundColor Green
} else {
    Write-Warning "`nVerification failed for one or more services. Check logs above."
}
