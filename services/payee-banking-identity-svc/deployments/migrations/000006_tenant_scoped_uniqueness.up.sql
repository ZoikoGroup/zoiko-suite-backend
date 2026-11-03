-- SPEC_DEVIATIONS A11: the two uniqueness invariants required by §4.10's
-- "Idempotency / concurrency" row ("Destination candidate fingerprint
-- detects duplicates; activation only one active version per party/scope")
-- were not tenant-scoped. party_ref is an ORG-07 UUID and very likely
-- globally unique in practice, but a PostgreSQL unique index is a structural
-- guarantee, not a probabilistic one, so two tenants that ever did collide on
-- (party_ref, fingerprint) or (party_ref, scope) would wrongly conflict with
-- each other.
--
-- tenant_id stays nullable (legacy rows predate per-tenant tagging), and
-- COALESCE(tenant_id::text, '') — matching the same idiom already used by
-- payment-status-svc's 000009 migration — folds every legacy NULL-tenant row
-- into one shared bucket so the pre-migration global-uniqueness behavior is
-- preserved for them, while every tenant-tagged row now gets a real
-- per-tenant boundary.
DROP INDEX uq_payee_destinations_fingerprint;
CREATE UNIQUE INDEX uq_payee_destinations_fingerprint
    ON payee_destinations (COALESCE(tenant_id::text, ''), party_ref, fingerprint)
    WHERE status <> 'SUPERSEDED';

DROP INDEX uq_payee_destinations_active_scope;
CREATE UNIQUE INDEX uq_payee_destinations_active_scope
    ON payee_destinations (COALESCE(tenant_id::text, ''), party_ref, scope)
    WHERE status = 'ACTIVE';
