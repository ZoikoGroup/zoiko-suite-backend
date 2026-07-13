---
trigger: always_on
---

# ZoikoSuite Build Doctrine — non-negotiable

- No domain service self-authorizes a material action. All authorization,
  policy, and approval logic routes through the Governance Plane
  (see docs/architecture/01-backend.md §07).
- Every state-changing API and every event consumer must be idempotent.
- No soft-delete on material objects. Use status transitions, tombstones,
  or effective end-dating only.
- Every material record carries tenant_id, legal_entity_id, and
  effective_from/effective_to (see docs/architecture/04-data-model.md §3.2).
- Events are facts, not commands. Append-only. Never mutate source truth
  from a downstream consumer.
- Do not start a Tier 1 service until its Tier 0 dependency has met its
  exit criteria in docs/architecture/06-blueprint.md.
- When in doubt about ownership or contract shape, stop and ask — do not
  improvise architecture.


# .agents/rules/doctrine.md — add this entry

- No service may hardcode a country, jurisdiction, currency, or
  tax-rule value as a code constant, enum, or switch/case branch.
  All jurisdiction-specific behavior must be expressed as versioned
  data consumed at runtime from Jurisdiction Rules Service / Tax
  Service. Adding a new jurisdiction must never require a code change
  or redeploy of any domain service.