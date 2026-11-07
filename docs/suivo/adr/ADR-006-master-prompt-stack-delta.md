# ADR-006: Master implementation prompt vs the discovered stack

- Status: Proposed. Pending Global CTO, Backend Lead and Frontend Lead.
- Date: 2026-10-28
- Context: The Suivo master implementation prompt (§2–§3) proposes a Node 20 / Fastify / Zod API, a React web-component widget, Redis, pgvector, Vitest/Playwright/k6 and a `suivo/apps/*` + `packages/*` monorepo. The prompt also says these are REPLACEABLE: the discovered stack wins, and the delta is recorded as an ADR (§2, §16.5).

## Decision

| Prompt default | Discovered fact / decision | Why |
|---|---|---|
| Node 20 + Fastify + Zod API | Go 1.25 service `services/public-ai-svc` (chi, pgx, zap, kafka-go) | Every backend service in `zoiko-suite-backend` is Go with this toolchain and CI matrix (G0 delta). |
| `suivo/apps`, `packages/*` monorepo | One service with in-process packages: `internal/{policy,claims,knowledge,model,validate,release,events,...}` | Same module split as §3, inside the repo's service layout. In-process modules, not microservices, as §7 requires. |
| Zod / JSON Schema | Go types with `Validate()` plus `api/openapi.yaml` (3.1) and `api/asyncapi.yaml` (3.0) | Unknown fields are refused by `DisallowUnknownFields`, matching `additionalProperties: false`. |
| Redis session store | Postgres `public_ai` schema with TTL purge | One dependency and transactional idempotency (ADR-002 context). Can move behind the `store.Store` interface later. |
| pgvector hybrid search | Lexical BM25 over the signed, release-pinned index (ADR-002) | No embedding model is approved yet (DEC-05). `knowledge.Query` keeps a vector seam. |
| Model call #1 classifies (L2-INTENT-001) | Deterministic classifier in `internal/policy` | It is safer, testable and cannot be prompt-injected. The L2-INTENT-001 text is pinned by hash (`model.IntentPromptVersion`) for when a provider classifier is approved. |
| Model call #2 composes | `model.Gateway` (timeout, one retry, breaker, tools never sent) with the verbatim L1/L2-RESPONSE/L3 layers pinned by hash (`model.PromptVersion`) | Same contract. A fake extractive provider stands in until DEC-05 (ADR-003). |
| React embeddable widget (script tag) | `AssistantWidget.tsx` in `zoiko-suite-frontend-platform` (Next.js 16, React 19), mounted in the admin console | Placement was the product owner's decision (widget in the platform console only, not the public NextJS site). A script-tag embed is deferred. |
| Vitest / Playwright / k6 | Go tests (unit, contract, acceptance G01–G36, S01–S10, e2e, isolation), `evalrun`, drills. Widget state reducer is tested with `node --test` | No JS test runner exists in the frontend repo; the reducer test needs no new dependency. Playwright/axe/k6 remain open (r0-readiness). |
| Terraform + signed provenance | Dockerfile (non-root, read-only), compose stack, CI matrix, gitleaks | IaC owner and hosting are DEC-01. |

## Turn envelope additions

The turn response gains `answer_class` (VERIFIED_FACT, STATUS_QUALIFIED, PROCESS_EXPLANATION, HANDOFF_UNKNOWN). It also gains optional `status_label` (governed vocabulary only, from the claim registry), `market_code` (the market a qualified answer is scoped to) and `clarification` ({slot, question, options}, CLARIFY only, options = market codes + `GENERAL_OVERVIEW`).

These are the prompt's §6.3 envelope fields that the UI needs (status badge, clarification chips). The model's own output never fills them. The orchestrator derives them from typed claims.

`safety_disposition`, `source_ids`, `link_ids`, `primary_cta_id` and `trace_id` from §6.3 are already carried as `status`, `citations[]`, `actions[]` and `request_id`.

`GET /config` gains `microcopy` (ui.* registry) and `chips` (starters by page_context).

## Consequences

- No second runtime or language is introduced; CI and ops stay uniform.
- Retention has no production default. Outside dev/test the service refuses to start unless `PUBLIC_AI_RETENTION` is set from the signed policy (DEC-06).
- Revisit when DEC-05 (model vendor) or DEC-01 (hosting/IaC) closes.
