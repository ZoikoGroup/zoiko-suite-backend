# Observability Baseline — Audit & Plan (DRAFT, no code yet)

**Status: planning document only.** No service code or `docker-compose.yml`
has been touched to produce this. This is the audit + recommended
approach for review before any implementation starts.

## 1. Source requirement (verbatim, cited)

**`03-microservices.md` §3.8, "Observability Is a Readiness Criterion"**
(a hard per-service gate):
> No service is production-ready unless it exposes:
> - structured logs
> - OpenTelemetry-compatible traces
> - health probes
> - service-level metrics
> - correlation IDs
> - alertable failure states

**`01-backend.md` §15.1** additionally lists six observability *layers*
platform-wide (infrastructure metrics, service metrics, distributed
tracing, audit observability, security event monitoring, business event
monitoring), and §16.2 names "centralized logging, tracing, and
alerting" as a core infrastructure component — i.e. a collection point
is expected to exist, not just per-service instrumentation.

OpenTelemetry is the only named technology anywhere in this set.
Prometheus/Grafana/Jaeger below are a judgment call, with reasoning.

## 2. Audit — what's already true, checked directly against the code

| Criterion | Status | Evidence |
|---|---|---|
| Structured logs | ✅ 10/10 services | every service uses `go.uber.org/zap` |
| Health probes | ✅ 10/10 services | every service has `/healthz` + `/readyz` |
| Correlation IDs | ✅ 10/10 services | every service has its own `X-Correlation-ID` middleware, threaded into log fields |
| OpenTelemetry traces | ❌ 0/10 | zero `go.opentelemetry.io` imports anywhere in the repo |
| Service-level metrics | ❌ 0/10 | no Prometheus client, no `/metrics` endpoint anywhere |
| Alertable failure states | ❌ 0/10 | no alerting integration or rule set at all |
| Central collection point | ❌ none | `docker-compose.yml` has no collector, Jaeger, Prometheus, Grafana, or Loki |

**The gap is narrower than the doctrine text suggests**: 3 of 6
per-service criteria are already done, consistently, across every
service. The real work is tracing, metrics, and a place to send both —
not a from-scratch retrofit of everything.

## 3. Recommended stack, with reasoning

**OpenTelemetry Collector → Jaeger (traces) + Prometheus (metrics) →
Grafana (dashboards), self-hosted.**

Why this and not a SaaS vendor (Datadog/New Relic) or a different
open-source combination:
- OTel is the only technology the doctrine names — not a choice, a
  constraint.
- A teammate's in-progress `feat/api-gateway` branch already picked
  **Traefik v3.1** as the API gateway. Traefik v3 natively emits both
  OTel traces and Prometheus metrics for every request it proxies —
  pairing with this stack costs zero extra integration once that branch
  merges, versus a SaaS vendor requiring its own separate agent/SDK
  wiring into Traefik as well as every service.
- Self-hosted avoids the vendor lock-in `06-blueprint.md` explicitly
  wants minimized (cloud-portability principle).
- Metrics stay pull-based (Prometheus scrapes each service's own
  `/metrics`) rather than pushed through the collector — keeps the
  collector single-purpose (traces only) rather than a second thing
  every service has to push into.

**Alertable failure states**: a Prometheus native alerting rule file
(`rule_files:`), not Grafana-provisioned alerts. Simpler schema, and
rules evaluate and show as Pending/Firing in Prometheus's own UI without
needing Alertmanager deployed — the right minimum bar. Two rules cover
this criterion to start: an error-rate threshold (5xx ratio over a
window) and a readiness-probe-failing signal, both driven by metrics the
per-service instrumentation would already produce — nothing invented
solely for alerting. Alertmanager + real paging is legitimate future
work, explicitly out of scope for this pass.

## 4. Shared instrumentation mechanism

No shared Go module. Verified constraint: this repo has no cross-service
Go module today, and every service's Docker build `context:` in
`docker-compose.yml` is scoped to its own `services/<svc>` directory — a
shared module pulled in via a `replace` directive can't be `COPY`'d into
any service's build without rewriting every Dockerfile's build context
(a large, unrelated change on its own).

**Recommendation**: a small `internal/telemetry` package, duplicated per
service — the same convention this repo already uses for
`correlationIDMiddleware`, which is already near-identically copy-pasted
across all 10 services today. A doc comment in the canonical copy would
mark it as the one to mirror changes from. This isn't a new pattern for
the repo, just applying the existing one consistently.

## 5. Reference service: `jurisdiction-rules-svc`

Chosen because it's already this repo's established Tier-0 reference
pattern for CRUD-shape services — the same role it plays for the base
service scaffold. Proving the observability pattern here first, the same
way every other cross-cutting convention in this repo got proven on one
service before rolling out, keeps the initial risk contained to one
service rather than eleven.

What it would need, at a level of detail sufficient to plan around
(not final code): an OTel trace exporter initialized at startup pointed
at the collector; a chi-compatible tracing middleware so every HTTP
request gets a span, with the pgx connection pool tracing wired in too
so DB queries show as child spans automatically; a small set of
Prometheus collectors (request count by status code, request latency,
a readiness gauge) registered once at startup and served on `/metrics`;
and one new config field for where the collector lives. All of this is
additive to `main.go` and one new package — no changes to the existing
handler, store, or health code, and so no expected impact on existing
tests.

## 6. Rollout order, and what the audit already surfaced about risk

1. **`policy-svc`, `authorization-svc`** — same dependency shape as the
   reference service, already wired into `docker-compose.yml`. Lowest
   risk, natural second/third proof points.
2. **`governance-decision-log-svc`** — same shape, already in compose.
3. **`configuration-feature-flag-svc`, `secret-vault-integration-svc`**
   — same dependency shape, but **verified: neither has any
   `docker-compose.yml` entry at all today** — a pre-existing gap, not
   caused by this work. Adding their compose blocks would need to be a
   small prerequisite bundled with instrumenting them.
4. **`obligations-svc`, `workflow-svc`** — same HTTP-path shape; both
   also produce/consume Kafka events. Kafka-level tracing is real future
   work but outside the doctrine's six criteria, which are scoped to a
   service's own request path — worth stating explicitly as a boundary
   rather than silently skipping it later.
5. **`tenant-entity-registry-svc`** — same dependency shape, but it
   carries its own `internal/middleware` package (tenant RLS context) —
   worth a specific look before assuming the same low-risk shape as 1–4.
6. **`audit-event-store-svc`** — structurally different: it's a
   Kafka-consumer service with no `internal/handler` package and no HTTP
   surface beyond health probes. It's also the one service in the repo
   with its own `cmd/server` integration test. This one needs its own
   observability shape (a span per consumed message, a
   messages-consumed counter) rather than the reference pattern, and
   its own care during rollout given that test.
7. **`identity-context-svc`** — do last. Verified directly: its
   `go.mod` pins `go 1.23.0`, and lists `pgx/v5` as an *indirect*
   dependency — meaning this service may not call Postgres through pgx
   directly at all, unlike the blanket assumption that every service
   does. Confirm what its persistence layer actually calls, and bump its
   Go toolchain, before assuming the reference pattern applies here
   unmodified.

## 7. How this would be verified once implemented

Bring up the central stack plus one instrumented service first, not the
whole fleet — generate some traffic, confirm a trace appears in Jaeger
with a child database span (proving context propagation actually
worked), confirm the new metrics appear on Prometheus with the service
correctly scraped, confirm the alert rules are loaded (and, deliberately,
trigger one — e.g. stop the database briefly — to see it actually go
from Pending to Firing and back), and confirm the existing test suite
for that service still passes. Only then repeat, scaled down, for each
service as it's rolled out per the order above.

## 8. Open, not silently resolved

- Exact package choices and versions are deferred to implementation
  time, not pinned in this plan.
- `policy-svc`'s `go.mod` lists Kafka client library as an *indirect*
  dependency despite `docker-compose.yml` setting Kafka env vars for it
  and prior work describing a real producer as wired — a discrepancy
  worth someone's attention, independent of this plan.
- Whether to route `feat/api-gateway`'s Traefik traffic through this same
  collector once that branch merges is a natural follow-on, not decided
  here.
