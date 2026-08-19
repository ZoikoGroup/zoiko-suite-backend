-- 0003_delegated_authority_svc.sql
-- delegated-authority-svc → schema `delegated_authority`
--
-- Squashed end state of 000001_initial_schema and 000002 (the FORCE RLS +
-- invariants pass). One table: delegation_grants.
--
-- ── This is the tenant-scoped template ───────────────────────────────────────
-- jurisdiction-rules (the previous migration) is platform reference data with
-- no tenant dimension. This file is the shape the other eighteen services
-- follow: a tenant_id column, ENABLE + FORCE row-level security, and a policy
-- with BOTH a USING and a WITH CHECK clause written against
-- app.current_tenant_id().
--
-- ── Two deliberate departures from the compose migrations ────────────────────
--
-- 1. The CHECK constraints are added VALID here, not NOT VALID.
--    000002 added them NOT VALID on purpose: the dev database holds real
--    history, and a migration must not rewrite an append-then-transition
--    register to make a constraint pass. That reasoning is about an EXISTING
--    table with a backlog. This is an empty database, so there is no history to
--    protect and no reason to ship a constraint that has never been verified
--    against its own table. The open "run VALIDATE CONSTRAINT once the backlog
--    is clean" item does not carry over to Supabase.
--
-- 2. The policy reads app.current_tenant_id() rather than
--    current_setting('app.tenant_id', true) directly, so the same policy serves
--    a Go service that calls set_config and a PostgREST request carrying a JWT
--    claim. See the foundation migration for why both are accepted.

CREATE SCHEMA IF NOT EXISTS delegated_authority;

COMMENT ON SCHEMA delegated_authority IS
    'delegated-authority-svc. Register of which principal may act for which other principal, on which action, for how long.';

GRANT USAGE ON SCHEMA delegated_authority TO zoiko_backend, authenticated;

-- ── delegation_grants ────────────────────────────────────────────────────────

CREATE TABLE delegated_authority.delegation_grants (
    delegation_id            UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    tenant_id                VARCHAR(255) NOT NULL,
    legal_entity_id          VARCHAR(255) NOT NULL,

    -- Who is handing authority over, and to whom.
    delegator_principal_id   VARCHAR(255) NOT NULL,
    delegate_principal_id    VARCHAR(255) NOT NULL,

    action_type              VARCHAR(100) NOT NULL,

    effective_from           TIMESTAMPTZ  NOT NULL,
    effective_to             TIMESTAMPTZ  NOT NULL,

    -- ACTIVE | REVOKED | EXPIRED
    status                   VARCHAR(20)  NOT NULL,

    -- Attribution defaults to the VERIFIED principal rather than whatever the
    -- request body carried. Fail-closed: with no principal on the connection
    -- this resolves NULL and the NOT NULL rejects the insert.
    created_by_principal_id  VARCHAR(255) NOT NULL DEFAULT app.current_principal_id(),

    correlation_id           VARCHAR(255) NOT NULL,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    revoked_by_principal_id  VARCHAR(255),
    revoked_at               TIMESTAMPTZ,
    expired_at               TIMESTAMPTZ,

    CONSTRAINT delegation_grants_period_ordered
        CHECK (effective_to > effective_from),

    -- The status vocabulary the domain defines. Enforced in Go today and
    -- nowhere else, so any other writer — a fix-up script, a future service —
    -- could otherwise leave a status no consumer knows how to read.
    CONSTRAINT delegation_grants_status_known
        CHECK (status IN ('ACTIVE', 'REVOKED', 'EXPIRED')),

    -- A terminal state must carry its evidence. A REVOKED grant with no
    -- revoked_at/revoked_by is a record that someone withdrew an authority with
    -- no account of who or when — which is the only thing that row is for.
    --
    -- Written as an equivalence in both directions so a revoked_at cannot
    -- linger on a row later rewritten to ACTIVE.
    CONSTRAINT delegation_grants_revoked_has_evidence
        CHECK ((status = 'REVOKED') = (revoked_at IS NOT NULL AND revoked_by_principal_id IS NOT NULL)),

    CONSTRAINT delegation_grants_expired_has_evidence
        CHECK ((status = 'EXPIRED') = (expired_at IS NOT NULL)),

    -- A delegation delegates authority to someone ELSE. A grant whose delegator
    -- and delegate are the same principal is not a delegation, it is a no-op
    -- that reads as one — and it is the shape a self-elevation attempt takes.
    CONSTRAINT delegation_grants_delegate_differs
        CHECK (delegator_principal_id <> delegate_principal_id)
);

-- Idempotency: a retried create with the same (tenant_id, correlation_id) must
-- resolve to the original record, never a duplicate.
CREATE UNIQUE INDEX idx_delegation_grants_tenant_correlation
    ON delegated_authority.delegation_grants (tenant_id, correlation_id);

CREATE INDEX idx_delegation_grants_tenant_entity_status
    ON delegated_authority.delegation_grants (tenant_id, legal_entity_id, status);

CREATE INDEX idx_delegation_grants_tenant_delegate
    ON delegated_authority.delegation_grants (tenant_id, delegate_principal_id, status);

-- Serves the lazy-expiry sweep (status = 'ACTIVE' AND effective_to < now()).
CREATE INDEX idx_delegation_grants_tenant_active_effective_to
    ON delegated_authority.delegation_grants (tenant_id, effective_to)
    WHERE status = 'ACTIVE';

-- The register is read newest-first and paged. created_at alone is not a total
-- order, so the index carries the primary key as a tiebreaker for the same
-- reason the ORDER BY does — without it two grants created in the same
-- transaction can straddle a page boundary and be returned twice, or not at all.
CREATE INDEX idx_delegation_grants_tenant_created
    ON delegated_authority.delegation_grants (tenant_id, created_at DESC, delegation_id DESC);

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE delegated_authority.delegation_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE delegated_authority.delegation_grants FORCE  ROW LEVEL SECURITY;

-- USING governs which rows are visible; WITH CHECK governs which rows may be
-- written. Both are required. With only USING, a caller can INSERT a row into
-- another tenant that it then cannot see — a write-side integrity gap that is
-- still open in obligations-svc on the compose estate.
--
-- NULL-safe by SQL semantics rather than by an explicit guard: when no tenant
-- is installed, app.current_tenant_id() is NULL, `tenant_id = NULL` is NULL,
-- and NULL is not true — so a connection that never set a tenant sees nothing
-- and can write nothing. Never rewrite this as
-- `app.current_tenant_id() IS NULL OR tenant_id = ...`; that is a filter which
-- switches itself off exactly when identity is missing.
CREATE POLICY tenant_isolation ON delegated_authority.delegation_grants
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

-- Console sessions read their own tenant's register and never write it: every
-- mutation is authorized by authorization-svc against DELEGATION_CREATE /
-- DELEGATION_REVOKE and must arrive through the service that performs that check.
CREATE POLICY tenant_read ON delegated_authority.delegation_grants
    FOR SELECT
    TO authenticated
    USING (tenant_id = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────
-- RLS narrows what a role may touch; it does not grant access in the first
-- place. Both are required.

GRANT SELECT ON delegated_authority.delegation_grants TO authenticated;

-- No DELETE. The register is append-then-transition: a grant is revoked or
-- expired, never removed, because its history is the evidence of what authority
-- was held and when.
GRANT SELECT, INSERT, UPDATE ON delegated_authority.delegation_grants TO zoiko_backend;
