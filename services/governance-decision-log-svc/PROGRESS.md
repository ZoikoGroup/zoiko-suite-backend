# Governance Decision Log Service — Progress Log

Status tracker for this service. Update this file whenever meaningful
work happens (spec changes, decisions made, code milestones). Keep
entries dated and short — this is a log, not a design doc (see
CONTEXT.md for the spec/context).

## Current status

**Phase:** Spec drafted, not yet approved for scaffolding.
**Code:** None exists yet.

## Log

### 2026-07-06 — Spec drafted
- Assembled initial service spec into `CONTEXT.md` from
  `docs/architecture/01-backend.md`, `03-microservices.md` §8.7, and
  `04-data-model.md` §7.1–7.3.
- Identified one open cross-service design question that needs a
  decision before scaffolding: whether writes to this service should be
  fail-closed (block the authorizing action if the decision can't be
  logged) or fail-safe via a durable async outbox. See CONTEXT.md
  "Open design question."
- No code written yet.

## Next steps

- [ ] Resolve the fail-closed vs. fail-safe-async question (needs
      coordination with whoever owns Authorization Service's
      implementation, since it's the primary caller).
- [ ] Decide idempotency key: `governance_decision_id` vs `correlation_id`.
- [ ] Decide read-API auth model (who/what may call `GET /v1/decisions`
      — audit tooling? Evidence Manifest Service? admin console once it
      exists?).
- [ ] Once above are resolved, scaffold `cmd/`, `internal/`, Postgres
      migration, and OpenAPI stub following the pattern in
      `identity-context-svc` / `tenant-entity-registry-svc` /
      `jurisdiction-rules-svc`.
- [ ] Wire into `.github/workflows/ci.yml` build/test matrix.
