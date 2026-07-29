# purchase-order-svc — Progress

## Status: v1 built and verified end-to-end 2026-07-23 (real infra, not mocks)

Built from scratch per `docs/architecture/03-microservices.md` §12.9 and
`docs/architecture/04-data-model.md` §14.1, directly mirroring
`purchase-request-svc`'s proven skeleton. Full design rationale in
`context.md`.

## What's implemented

- `PurchaseOrder` issue/get/list/amend/close, mirroring
  `purchase-request-svc`'s handler/store/authz-client shape file-for-file.
- State machine: `ISSUED → CLOSED` (terminal). Amend does not change
  status — it appends a `PurchaseOrderAmendment` row and increments
  `version`, mirroring `employment-contracts-svc`'s version-lineage
  pattern rather than a destructive in-place update.
- Real cross-service verification: `Issue` synchronously checks a
  caller-supplied `purchase_request_id` against the live
  `purchase-request-svc` (same tenant/entity, `status == APPROVED`),
  fail-closed on any error — see `context.md` §6.3 for why this matters
  (the 2026-07-23 platform audit found the opposite anti-pattern —
  trusting an unverified foreign ID — in `corporate-actions-svc`).
- Authorization gating on every mutation (`PO_ISSUE`/`PO_AMEND`/`PO_CLOSE`
  via `authorization-svc`'s `/v1/authorize`), fail-closed — built correctly
  from day one, unlike the Phase 5 services the same audit found wire an
  authz client but never call it.
- Idempotent `Issue`: unique `(tenant_id, correlation_id)` constraint,
  `ON CONFLICT DO NOTHING` + re-select — a real improvement over most
  Phase 3/4/5 siblings, which store `correlation_id` without deduping it.
- Postgres RLS + explicit `tenant_id` filters in every store query (the
  RLS-alone-is-insufficient lesson this platform learned from
  `general-ledger-svc`'s and `tenant-entity-registry-svc`'s CI failures).
- Real Kafka producer: `purchase.order.issued`, `purchase.order.amended`,
  `purchase.order.closed`.
- OpenTelemetry tracing + Prometheus metrics (`internal/telemetry`, copied
  verbatim from `purchase-request-svc`, same as every other service post
  the 2026-07-09 observability retrofit).
- Wired into `deployments/docker-compose.yml`, `deployments/init-db.sh`,
  `.github/workflows/ci.yml` (matrix + `TEST_DATABASE_URL` condition +
  store-isolation-test condition), and a new
  `deployments/kubernetes/manifests/23-app-purchase-order.yaml`.

## Deliberate v1 scope decisions (not oversights)

- **No vendor-profile validation.** `vendor_profile_id` is stored as an
  unvalidated optional field — Vendor Due Diligence Service (the entity's
  real owner per `04-data-model.md`) doesn't exist yet. Same documented-gap
  posture as `accounts-payable-svc`'s "no vendor-master dependency."
- **No goods-receipt/three-way-match workflow.** `close` is a single
  explicit action. No spec section describes a Goods Receipt entity.
- **No `CANCELLED` status.** Only the three events `03-microservices.md`
  §12.9 names (`issued`/`amended`/`closed`) are in scope.
- **No Kafka consumer for `purchase.request.approved`.** Verification of an
  upstream request is synchronous HTTP at Issue time, matching every
  sibling Phase 3 service's actual cross-service-check pattern (none of
  them auto-create records from consumed events).

## Verified (real infra, not mocks) — 2026-07-23

Written with no Go toolchain or running Docker available (see git history
of this file for that caveat as it originally stood) — verification below
happened in a later pass, once Docker Desktop was available, against the
actual platform compose stack (`postgres` reusing the existing
`deployments_postgres_data` volume — `purchase_request`/`purchase_order`
databases created and migrated onto it non-destructively, `authorization_svc`
already present from a prior run).

- **`go build` succeeded for real**: `docker compose up -d --build
  purchase-order-svc` compiled the actual Dockerfile build stage — the
  first genuine proof this code compiles, not an inference from mirroring.
- **Hand-written migration SQL applied cleanly** against real Postgres 16
  with zero errors (sequence, both tables, all indexes, both RLS policies).
- **Full HTTP lifecycle exercised live** against real Postgres/Kafka/
  authorization-svc/purchase-request-svc (after seeding a real
  `PO_ISSUE`/`PO_AMEND`/`PO_CLOSE` role+permission-bundle+role-assignment
  via authorization-svc's actual admin API — no stubs):
  - Health/readiness probes: 200
  - Validation: missing `tenant_id` on list → 400; missing `X-Principal-Id`
    → 401; zero-amount → 400 (covered by unit tests, confirmed live too)
  - Authorization gate fail-closed confirmed twice: once genuinely
    (a malformed non-UUID `legal_entity_id` caused authorization-svc's own
    store to error, and this service correctly surfaced 503 rather than
    proceeding) and once via real deny-by-default (403 before any role was
    granted)
  - Issue → 201, `po_number` generated correctly (`PO-000001`, sequential)
  - Idempotent replay (same `correlation_id`) → 200, identical body, **not**
    republished (`purchase.order.issued` published exactly once — confirmed
    via a real Kafka consume, not just a mock-publisher counter)
  - Amend → 200, `version` incremented, `total_amount` updated
  - Close → 200, terminal
  - Amend-after-close → 422 `invalid_transition`, confirming the state
    machine's CAS guard holds against real Postgres, not just the stub
    store in `handler_test.go`
  - **Cross-service verification (§6.3) confirmed both branches for
    real**: issuing a PO against a genuinely `APPROVED` purchase request
    (created and approved live via `purchase-request-svc`) → 201 with
    `purchase_request_id` correctly stamped; issuing against a `PENDING`
    (not yet approved) request → 422 `purchase_request_not_approved`
  - **Kafka events confirmed landing** (see fix below) via a real
    `kafka-console-consumer` read of `zoiko.purchase-order.events` — full
    envelope, correct `event_type`/`correlation_id`/`source_service`/payload

## A real platform-wide bug found and fixed (in this service) during verification

Every Kafka publish from this service initially failed silently with
`[3] Unknown Topic Or Partition`, even though the broker has
`KAFKA_AUTO_CREATE_TOPICS_ENABLE=true`. Root cause: `segmentio/kafka-go`'s
`Writer` defaults `AllowAutoTopicCreation` to `false` and never requests
auto-creation in its metadata calls, so the broker-side setting is never
actually exercised. **This is not specific to this service** — checked
`purchase-request-svc`'s live logs during the same session and it has the
identical silent failure on the identical `kafka.Writer{}` construction
used platform-wide. Every service sharing this exact pattern (which per the
2026-07-23 audit is most of the ~49 services) has likely never actually
published a single Kafka event in any environment using this compose
file's Kafka config, despite "real Kafka producer" being claimed across
many `progress.md`/`context.md` files (including this platform's own
verified-live claims for `policy-svc`, `obligations-svc`, and others).
Fixed here by setting `AllowAutoTopicCreation: true` on this service's
`kafka.Writer` in `cmd/server/main.go` — confirmed the fix works (events
now land). **Not fixed platform-wide** — that's a separate, larger
follow-up affecting every other service, out of scope for this build. See
`[[zoikosuite-known-issues]]` project memory for the tracked finding.

## Two pre-existing `docker-compose.yml` bugs found and fixed (unrelated to this service)

`workforce-compliance-svc` and `offboarding-severance-svc`'s `depends_on`
blocks referenced `jurisdiction-rules-svc` as the dependency key — but the
actual compose service key is `jurisdiction-svc` (`jurisdiction-rules-svc`
is only that service's `container_name`, not a valid `depends_on`
reference). This is a hard schema error that blocked `docker compose up`
for the **entire file**, regardless of which service was targeted — not
something introduced by this change, but it had to be fixed to start
anything at all. Fixed both occurrences to `jurisdiction-svc:`. Separately
noted but **not fixed**: ~13 services' `JURISDICTION_RULES_URL` env var
values point at `http://jurisdiction-rules-svc:8081` (wrong hostname *and*
wrong port — should be `http://jurisdiction-svc:8082`, the pattern
correctly used elsewhere in the same file) — this doesn't block compose
from starting, but means those services' jurisdiction-rules lookups will
fail-closed/fall back at runtime. Worth a dedicated follow-up; out of scope
here since none of the services started in this pass depend on it.

## Not yet done

- CI hasn't run this yet (this branch hasn't been pushed) — a real CI
  run is still worth doing to catch anything this local Docker verification
  didn't (e.g. the embedded-postgres `tenant_isolation_test.go`, which
  wasn't run in this pass — `go test` itself still requires a Go toolchain
  this environment doesn't have; only the Docker-built binary was
  exercised, via real HTTP calls, not `go test`).
- `services/README.md` was not updated — that file already stopped
  tracking services past `bank-reconciliation-svc` (8102), a pre-existing
  staleness this build didn't try to fix.
- The platform-wide Kafka `AllowAutoTopicCreation` gap and the
  `JURISDICTION_RULES_URL` misconfiguration above remain unfixed outside
  this one service.
