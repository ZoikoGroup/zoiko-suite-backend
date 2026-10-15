-- Migration: 000015_jurisdiction_context.up.sql
--
-- Doc 03 §19 requires every event to carry jurisdiction context, and Doc 04
-- §7.1 names the decision's basis jurisdiction_rule_basis. This service stored
-- the rule but no jurisdiction, and filled the event's jurisdiction_context
-- with rule_basis as a stand-in. The decision now records the jurisdiction
-- context of the request that produced it: the canonical envelope's
-- X-Jurisdiction-Context, or the body's jurisdiction_context, which must agree
-- when both are sent. Nullable: a decision made with no jurisdiction context
-- (platform-scope policy) has none to record.
--
-- Adding a nullable column rewrites no row, so the append-only triggers are
-- not involved.

BEGIN;

ALTER TABLE governance_decisions
    ADD COLUMN IF NOT EXISTS jurisdiction_context VARCHAR(64);

COMMIT;
