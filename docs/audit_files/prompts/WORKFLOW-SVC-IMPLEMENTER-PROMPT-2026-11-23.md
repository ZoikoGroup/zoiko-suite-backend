# Implementer prompt: workflow-svc (D2-6), 23 Nov 2026

Paste everything below the line into the implementing model.

---

You are implementing the D2-6 workflow-svc plan for ZoikoSuite. Work alone, directly in the repositories.

## Repositories

- Backend: `d:\zoikocomp\zoiko-suite-backend-platform`. The service is `services/workflow-svc` (Go).
- Frontend: `d:\zoikocomp\zoiko-suite-frontend-platform`. The console is `app/admin/workflows`, `components/admin/workflow`, `lib/api/workflow.ts` (Next.js).
- Branch `shashi-changes` in both. Make one commit per step. Do not push.

## Read first, in full, before step 1

1. `docs/architecture/01-backend.md` to `06-blueprint.md`. This is the doctrine every change must follow.
2. `docs/architecture/ZS-SVC-R-001_Workflow_Approval_Case_Obligation_Control_Detailed_Service_Specifications_v1.0.docx`: invariants R-INV-01…30, negative paths NP-01…40, sections 3–12.
3. `docs/audit_files/DOMAIN1-DOMAIN2-COMPLETION-TRACKER-2026-11-06.md`: "How to use this file", "Status legend", "Assignment 23 Nov 2026", "Frontend rules for every console", and the whole "D2-6 · workflow-svc" section. The D2-6 step table is your task list. Its "Done when" column is what the verifier re-runs.
4. In `services/workflow-svc`: `progress.md`, `SPEC_DEVIATIONS.md`, `BIZ04_CERTIFICATION.md`, `openapi.yaml`, `openapi.biz04-forms.yaml`, every file in `deployments/migrations`, `internal/outbox`, `internal/store/pg_store.go`, `decision_store.go`, `lifecycle_store.go`, `internal/handler/handler.go` and `form.go`.
5. The frontend `AGENTS.md`. This Next.js differs from your training data: read the relevant guide in `node_modules/next/dist/docs` before you write Next code. Console pages use the v2.0 kit in `components/admin/console`. The reference page is `app/admin/authorization`.

## Known defects (found 23 Nov; the steps fix them)

- The handler test package does not compile, so `go vet` and `go test` fail (WF-S0a).
- The store suite reset drops a hand-written table list and breaks after its first test (WF-S0a).
- `progress.md` names tests and routes that do not exist (WF-S0).
- Cases fail at runtime: events go to a table `outbox` that does not exist, and the store writes columns that 000023 never created (WF-S1, WF-S2).
- The state-machine, available-actions and transition-history endpoints return invented data, and available-actions queries the bare pool through a type assertion (WF-S8, WF-S9).
- An approval never expires and can be released any number of times; refused decisions leave no evidence (WF-S11a).
- There is no instance list or summary route, and the console calls a client that does not exist (WF-S11b, WF-FE-1).
- The console shows invented names and numbers (WF-FE-2).

## Order

WF-S0a, S0, S1…S7, S8…S10, S11, S11a, S11b, S22, S23, S24, S25, S12…S20, WF-FE-1, FE-2, FE-3, then S21 (hand-back). The rows in the tracker are in this order. Do not skip ahead: a later step may depend on an earlier one.

## Status rules (you write only these)

- Set 🔵 when the step's "Done when" holds. In Evidence write the commit id, the exact commands you ran and the counts, for example: `a1b2c3d4 · go test -count=1 ./... 412/0 · store IT as NOBYPASSRLS 160/0/1 · mutants 2/2`.
- Set ⛔ when you cannot finish a step. Name the reason and the owner. Go on to the next step unless it depends on the blocked one.
- Never write ✅ or ❌. Only the verifier writes those.
- Also set the D2-6 feature rows (F01–F31) that a step closes to 🔵, with the same evidence.

## Honesty rules (the previous model broke these; the verifier re-runs everything)

- Never cite a test, route, table, column, metric or event that does not exist in the code.
- Never mark 🔵 without running the command in the same session. Paste counts, not adjectives.
- No placeholder or sample data in the UI: no invented names, numbers or "placeholder" KPIs. Show live data, honest empty states and error states.
- No TODO stubs and no commented-out checks.
- No type assertions to `*store.PgStore` and no queries on the bare pool. Every store read and write runs inside `withTenantTx`, so `app.tenant_id` is set.
- Every new table or column is created by a migration with a working down file. Run the migrations up, down and up again on a scratch `postgres:16` before you set 🔵.
- Run the store suite as a `NOSUPERUSER NOBYPASSRLS` role through `TEST_DATABASE_URL`, not as `postgres`.
- Run `gofmt` on every Go file you touch. `go vet ./...` must stay clean.
- Keep `progress.md` true: remove any claim that no test you ran supports.

## Doctrine checklist (apply to every step)

- Governance first: authorize every command through authorization-svc and fail closed. The only outcomes are authorized, denied or escalated. A denial is recorded as evidence.
- Context keys on records and events: `tenant_id`, `legal_entity_id`, jurisdiction, actor, `correlation_id`, `causation_id`, `source_service`, `schema_version`, `plane`, `data_class`, `residency_region`.
- Events are facts. Write them with `outbox.Insert` (`internal/outbox/outbox.go`) in the same transaction as the state change. Consumers are idempotent.
- Every state-changing route takes `Idempotency-Key` with a request hash (same key and body: 200 replay; changed body: 409) and `If-Match` against `row_version` (stale: 412).
- No soft delete and no silent overwrite. Use status transitions and append-only history tables enforced by triggers.
- New action codes go in a new access-control migration (use the next free number) and in `deployments/scripts/seed-demo-rbac.ps1`.
- Document every route in `openapi.yaml` (the parity test checks it) and every event in a new `asyncapi.yaml`.

## Environment

- Windows host; Docker data is on D:.
- Start only the containers a step needs: postgres, kafka, and the services named in the tracker's "Containers" table. Stop the containers you started when the step ends.
- Build images from host binaries over the existing base image while the stack is up.
- If `npx tsc` is blocked by errors in files outside the workflow console (on 23 Nov: `app/admin/tenants/page.tsx`), record it in Evidence and do not edit other services' pages.

## Hand-back

When WF-S21 is done, stop. Reply with one table (step · status · commit · commands · counts) and a list of every ⛔ step with its reason. Do not claim the service is finished; the verifier decides that.
