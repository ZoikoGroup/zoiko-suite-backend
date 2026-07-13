# ZoikoSuite Kubernetes — Deployment Readiness Audit

> **⚠️ Status: NOT a live run.** A full 13-service Kind boot has **not** been
> executed. No local/agent environment available to the team can currently run
> the whole stack — Docker Desktop's named socket (`//./pipe/dockerDesktopLinuxEngine`)
> returns an access-denied / privilege-isolation error under the agent sandbox,
> and the full stack exceeds available local resources. This document is a
> **static deployment-readiness audit** plus the **expected** runtime behaviour
> and the exact commands to verify it. The pod listing in §3 is **illustrative
> (expected), not observed.** Real end-to-end Kind evidence is an open follow-up
> that requires a machine capable of running all 13 services.

This documents what is verified about the Kubernetes manifests, and what remains
to be verified by an actual run of `deployments/kubernetes/deploy-local.ps1`.

## What IS verified (static, no cluster required)
- All 13 services have Deployment + Service manifests under `manifests/`, each
  in the correct namespace, with resource requests/limits, liveness/readiness
  probes, and non-root security contexts.
- Env config is complete and uses in-cluster DNS (e.g. `postgres.zoiko-infra.svc.cluster.local`,
  `authorization-svc.zoiko-governance.svc.cluster.local:8089`,
  `identity-svc.zoiko-identity.svc.cluster.local:8080/.well-known/jwks.json`) —
  no reliance on wrong code-default hostnames.
- Each service has a scoped NetworkPolicy with egress only to its real
  dependencies (Postgres/Kafka/Redis/peers) plus DNS.
- `deploy-local.ps1` builds, loads, applies, rollout-waits, and health-probes
  every service.

## What is NOT yet verified (requires a live run)
- Actual pod startup / Ready state on a real cluster.
- Cross-namespace traffic actually flowing under the NetworkPolicies (a
  policy-enforcing CNI behaves differently from Kind's default `kindnet`, which
  does not enforce NetworkPolicy — so even a green Kind boot would not prove the
  policies; that needs a Calico/Cilium cluster).
- The identity-svc signing-key Secret wiring end to end.

## 1. Local Environment (intended)
- **Host OS**: Windows (PowerShell)
- **Docker**: Docker Desktop
- **Kubernetes Client**: `kubectl`
- **Target Context**: `kind-zoiko-cluster`

## 2. Deployment Sequence (`deploy-local.ps1`)

```powershell
# 1. Boot kind
kind create cluster --name zoiko-cluster

# 2. Build + load local images for all 13 services
#    identity-svc, tenant-svc, jurisdiction-svc, governance-svc, audit-svc,
#    policy-svc, authorization-svc, workflow-svc, configuration-svc,
#    secret-vault-svc, obligations-svc, gateway-auth-svc, schema-registry-svc

# 3. Namespaces + the identity signing-key Secret (generated locally)
kubectl apply -f manifests/00-namespaces.yaml

# 4. Infra
kubectl apply -f manifests/02-infra-postgres.yaml
kubectl apply -f manifests/03-infra-redis.yaml
kubectl apply -f manifests/04-infra-kafka.yaml

# 5. NetworkPolicies + app deployments (05–17)
kubectl apply -f manifests/01-network-policies.yaml
kubectl apply -f manifests/05-app-identity.yaml
# ... through ...
kubectl apply -f manifests/17-app-schema-registry.yaml
```

## 3. Expected Runtime (illustrative — NOT observed)

On a successful run, `kubectl get pods -A` is **expected** to show all 13
services plus infra `Running`/`Ready` across the `zoiko-infra`, `zoiko-identity`,
`zoiko-governance`, and `zoiko-evidence` namespaces. This has not been captured
from a real cluster yet — do not treat it as observed output.

## 4. Health-Check Smoke Tests (to run after boot)

| Service | Port | Namespace | Probe |
|---|---|---|---|
| `identity-svc` | 8080 | `zoiko-identity` | `/health` |
| `tenant-svc` | 8081 | `zoiko-identity` | `/healthz` |
| `jurisdiction-svc` | 8082 | `zoiko-governance` | `/healthz` |
| `governance-svc` | 8083 | `zoiko-governance` | `/healthz` |
| `audit-svc` | 8084 | `zoiko-evidence` | `/healthz` |
| `policy-svc` | 8085 | `zoiko-governance` | `/healthz` |
| `configuration-svc` | 8086 | `zoiko-governance` | (TCP connect) |
| `secret-vault-svc` | 8087 | `zoiko-governance` | `/healthz` |
| `obligations-svc` | 8088 | `zoiko-governance` | `/healthz` |
| `authorization-svc` | 8089 | `zoiko-governance` | `/healthz` |
| `workflow-svc` | 8090 | `zoiko-governance` | `/healthz` |
| `gateway-auth-svc` | 8092 | `zoiko-identity` | `/healthz` |
| `schema-registry-svc` | 8093 | `zoiko-governance` | `/healthz` |

```powershell
kubectl port-forward svc/gateway-auth-svc -n zoiko-identity 8092:8092
curl http://localhost:8092/healthz
```

## 5. Cross-Service Paths to Verify (after boot)

**Path A — Gateway ForwardAuth:** `gateway-auth-svc` fetches JWKS from
`identity-svc.zoiko-identity.svc.cluster.local:8080/.well-known/jwks.json`
(cross-namespace egress) and validates an envelope JWT.

**Path B — Schema publish gate:** `schema-registry-svc` calls
`authorization-svc.zoiko-governance.svc.cluster.local:8089/v1/authorize`
(intra-namespace egress) before accepting a registration.

Verifying these two proves DNS + NetworkPolicies allow the real traffic, not
just that pods start. **Both are pending a live run.**
