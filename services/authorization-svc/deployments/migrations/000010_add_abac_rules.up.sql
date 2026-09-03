-- Migration: 000010_add_abac_rules.up.sql
--
-- Gives the service the attribute-condition (ABAC) layer its spec assigns it
-- and that it has never had.
--
-- ── THE OBJECTION THIS ANSWERS, AND WHAT IT DOES NOT DO ─────────────────────
--
-- progress.md's v1 note is right on the facts: "No attribute-condition rules
-- exist anywhere in the architecture docs to encode — implementing it now
-- would mean inventing business logic." Every concrete rule (which attribute,
-- which threshold, which action) is a product decision, and inventing one and
-- shipping it enabled would be worse than the gap.
--
-- What was conflated there is the RULE with the RULE ENGINE. The spec assigns
-- this service the "ABAC decision logic"; that is a mechanism, and a mechanism
-- can be built without knowing a single rule. This migration adds the table
-- rules are DECLARED in, and ships ZERO rows. With no rows, /v1/authorize
-- behaves exactly as it does today — the layer is a no-op until somebody who
-- knows the business declares a rule through POST /v1/admin/abac-rules.
--
-- That is the same shape as sod_rules, which nobody calls invented business
-- logic: a table of declared conflicts, a query that evaluates them, and no
-- opinion in the code about which conflicts exist.
--
-- ── DENY-ONLY, DELIBERATELY ─────────────────────────────────────────────────
--
-- An ABAC rule can only ever REMOVE an action the earlier layers already
-- granted. It cannot grant anything. Two reasons, and they are not stylistic:
--
--   1. Composition. RBAC and delegation answer "does this principal hold this
--      action". An attribute condition answers "may this be exercised right
--      now, against this thing". Letting an attribute rule grant would make
--      the answer depend on which layer ran last.
--   2. Blast radius. A malformed grant-capable rule silently widens access
--      platform-wide. A malformed deny-capable rule narrows it, which is loud,
--      recoverable, and the direction this service already fails in everywhere
--      else.
--
-- ── THE TWO EFFECTS ─────────────────────────────────────────────────────────
--
--   REQUIRE  the condition MUST hold, or the action is denied.
--            "approval over 10000 requires attribute dual_approved = true"
--   FORBID   the condition must NOT hold, or the action is denied.
--            "payment release is forbidden when channel = SELF_SERVICE"
--
-- REQUIRE with an attribute the caller did not send is a DENIAL, not a skip.
-- A required condition that cannot be evaluated has not been satisfied, and
-- the alternative — treating an absent attribute as a pass — means any caller
-- can bypass an ABAC rule by omitting a JSON field. Callers therefore see a
-- new denial the moment a REQUIRE rule is declared for an action they were
-- already performing; that is what declaring the rule means, and the decision
-- basis names the rule_code so it is diagnosable from the log rather than from
-- an incident.
--
-- ── operator IS DATA, AND IS STILL VALIDATED ────────────────────────────────
--
-- operator is VARCHAR, not an enum, same doctrine as role_scope_type /
-- conflict_type. But the evaluator has to actually implement each one, so an
-- operator it does not recognise is refused at CREATE time (400) rather than
-- discovered at evaluation. Should one reach evaluation anyway — a row written
-- around the API — the evaluator denies and logs, because an authorization
-- condition nobody can evaluate has not been met. Loud and closed, in that
-- order.

BEGIN;

CREATE TABLE abac_rules (
    abac_rule_id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- NULL = applies to every tenant, exactly sod_rules' convention.
    tenant_id                UUID,

    -- Stable, human-readable identifier. It is what decision_basis names, so
    -- an operator reading a denial can find the rule that caused it.
    rule_code                VARCHAR(128) NOT NULL,

    -- The action this condition guards. One row = one condition on one action.
    action_type              VARCHAR(128) NOT NULL,

    -- REQUIRE | FORBID. Data only; see the header for the semantics.
    effect                   VARCHAR(16)  NOT NULL,

    -- The attribute the calling service sends in /v1/authorize's `attributes`
    -- map, e.g. "amount", "channel", "resource_classification".
    attribute_key            VARCHAR(128) NOT NULL,

    -- eq | ne | in | not_in | lt | lte | gt | gte | exists | not_exists |
    -- contains. Data only, validated at creation — see the header.
    operator                 VARCHAR(32)  NOT NULL,

    -- The comparison operand. NULL for exists/not_exists, which take none.
    -- For in/not_in this is a comma-separated list, which is the shape a
    -- console form produces and needs no JSON parsing in the evaluator.
    attribute_value          TEXT,

    active_flag              BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT         NOT NULL
);

-- rule_code is unique within its scope. Two partial indexes rather than one
-- UNIQUE(tenant_id, rule_code), because in Postgres NULLs are distinct in a
-- unique index — a single constraint would let the same platform-wide
-- rule_code be created any number of times.
CREATE UNIQUE INDEX idx_abac_rules_tenant_code_unique
    ON abac_rules (tenant_id, rule_code) WHERE tenant_id IS NOT NULL;
CREATE UNIQUE INDEX idx_abac_rules_global_code_unique
    ON abac_rules (rule_code) WHERE tenant_id IS NULL;

-- The evaluation lookup: every active rule guarding this action, in this
-- tenant's scope or platform-wide.
CREATE INDEX idx_abac_rules_action ON abac_rules (action_type) WHERE active_flag;

COMMENT ON TABLE abac_rules IS
    'Declared attribute conditions evaluated as layer 5 of POST /v1/authorize. Deny-only: a rule can remove an action the RBAC/delegation layers granted, never add one. Ships empty — with no rows the layer is a no-op.';

-- Same policy shape as sod_rules (000004): a NULL-tenant rule is
-- platform-wide and must be visible in every scope, a tenant-scoped rule only
-- in its own. NULLIF guards the '' that withRLS installs for a genuinely
-- tenantless call, since ''::uuid raises rather than returning NULL.
--
-- WITH CHECK admits NULL for the same reason sod_rules' does — the INSERT uses
-- RETURNING, and Postgres applies the SELECT side of a FOR ALL policy to an
-- INSERT ... RETURNING, so excluding NULL here would make creating a
-- platform-wide rule fail outright. Authoring one is gated in the handler by
-- the platform-scope grant (ActionABACRuleManageGlobal), exactly as authoring
-- a platform-wide SoD rule already is.
ALTER TABLE abac_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE abac_rules FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON abac_rules
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

COMMIT;
