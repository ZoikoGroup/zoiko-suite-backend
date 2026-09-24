-- Migration: 000006_operations.up.sql
--
-- Governed change machinery: kill_switches, config_changes and
-- emergency_changes.
--
-- WHY (ZS-SVC-AA-001, Table 24 /configs /changes /emergency-changes, §8,
-- Table 7 lifecycles, Table 21 change classes, INV-14/15/16/17):
--
-- The service stores values, but until now the ONLY reversal mechanism for a
-- bad value was "write another value" — no before/after, no approval binding,
-- no verification, no emergency expiry. §A0 states the control plane exists so
-- a runtime change is versioned, bounded, attributable, testable and
-- REVERSIBLE. These three tables make the reversal paths real:
--
--   kill_switches     — predeclared, narrow, temporary disable/degrade signals
--                       for a flag key (INV-16, NP-49). A switch admits only a
--                       predefined safe behavior — disable or degrade-only,
--                       never a broader access — and always carries an expiry,
--                       recorded via expired_at exactly like the values'
--                       effective_to pattern, so at most one active switch can
--                       exist per scope.
--   config_changes    — atomic ChangeSets with class, before/after snapshot
--                       links, approval binding and the Table 7 lifecycle
--                       PROPOSED -> VALIDATED -> APPROVED -> SCHEDULED ->
--                       APPLYING -> VERIFIED / FAILED / ROLLED_BACK (INV-14:
--                       every material production change is attributable).
--                       C2/C3 require approval (TC-04/Table 21); a rollback is
--                       itself a new change pointing at its rollback_change_id
--                       — evidence is retained, never deleted (NP-47).
--   emergency_changes — time-boxed break-glass mutations (INV-15, NP-42/43/44).
--                       Expiry is mandatory; the sweep that flips a change to
--                       EXPIRED and reverts it is 000008's caller in the
--                       server's background loop. Retrospective is mandatory.
--
-- All three are operations metadata (tenant-scoped rows where a tenant exists,
-- environment-wide rows where not) and follow the same RLS doctrine as the
-- values they govern: a tenant sees its own rows and the global rows, the
-- background sweep names itself through app.ops_sweep, and every request-path
-- session stays confined (000002's NULLIF branches preserved verbatim).

-- ── kill_switches ─────────────────────────────────────────────────────────────

CREATE TABLE kill_switches (
    kill_switch_id       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- The flag key this switch disables or degrades.
    flag_key             VARCHAR(255) NOT NULL,

    environment          VARCHAR(64) NOT NULL,

    -- NULL tenant_id = environment-wide switch, applying to every tenant in
    -- the environment, the same meaning as the value tables.
    tenant_id            UUID,

    reason               TEXT        NOT NULL,
    incident_id          TEXT,

    -- INV-16 / NP-49: a kill switch may only DISABLE or DEGRADE the path,
    -- never broaden it. There is no "ENABLE" value this column can hold.
    safe_behavior        VARCHAR(16) NOT NULL
        CONSTRAINT chk_kill_switches_safe_behavior CHECK (
            safe_behavior IN ('DISABLE','DEGRADE_ONLY')
        ),

    -- No permanent kill switch. Temporary by construction (NP-43's sibling
    -- for switches), and expired_at is the append-only "ended" marker so the
    -- table keeps the full switch history for evidence and replay.
    expires_at           TIMESTAMPTZ NOT NULL,
    expired_at           TIMESTAMPTZ,

    created_by_principal_id TEXT        NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_kill_switches_one_active_per_scope UNIQUE (
        flag_key,
        environment,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID)
    ) WHERE expired_at IS NULL
);

-- ── config_changes ────────────────────────────────────────────────────────────

CREATE TABLE config_changes (
    change_id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- C0 cosmetic / C1 operational / C2 material / C3 critical (Table 21).
    change_class         VARCHAR(4)  NOT NULL
        CONSTRAINT chk_config_changes_class CHECK (
            change_class IN ('C0','C1','C2','C3')
        ),

    environment          VARCHAR(64) NOT NULL,
    tenant_id            UUID,

    -- The immutable before/after evidence the change will (or did) move
    -- between. Linked to snapshots, never to mutable rows, so "what changed
    -- and to what" stays reproducible forever (TC-01, NP-59).
    before_snapshot_id   UUID        REFERENCES config_snapshots(snapshot_id),
    proposed_snapshot_id UUID        REFERENCES config_snapshots(snapshot_id),

    -- The atomic set of mutations as an ordered JSONB array, each element
    -- {kind: config|flag, key, scope: {environment, tenant_id?},
    --  new_value|new_enabled/rollout_percentage, expected_before_hash?}.
    parts                JSONB       NOT NULL,

    -- Table 7 lifecycle. VERIFIED / FAILED / ROLLED_BACK are terminal.
    status               VARCHAR(16) NOT NULL DEFAULT 'PROPOSED'
        CONSTRAINT chk_config_changes_status CHECK (
            status IN ('PROPOSED','VALIDATED','APPROVED','SCHEDULED',
                       'APPLYING','VERIFIED','FAILED','ROLLED_BACK')
        ),

    -- Approval binding as {approved, by_principal_id, approved_at,
    -- wfc_reference?, approval_required}. C2/C3 MUST carry a true
    -- approval_required and an approved record before ACTIVATION (TC-04).
    approval             JSONB,
    approval_required    BOOLEAN     NOT NULL DEFAULT false,

    planned_effective_at TIMESTAMPTZ,
    activated_at         TIMESTAMPTZ,
    verified_at          TIMESTAMPTZ,

    -- A rollback is a new change; this points at the change it rolls back.
    rollback_change_id   UUID        REFERENCES config_changes(change_id),

    created_by_principal_id TEXT        NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_config_changes_scope_status
    ON config_changes (environment, status, created_at);

-- ── emergency_changes ─────────────────────────────────────────────────────────

CREATE TABLE emergency_changes (
    emergency_change_id  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Break-glass scope is a small allowlist of C2/C3 keys, not arbitrary
    -- free-form keys (NP-42) — the store enforces that against the change
    -- class the key's definition declares.
    key                  VARCHAR(255) NOT NULL,

    environment          VARCHAR(64) NOT NULL,
    tenant_id            UUID,

    new_value            JSONB       NOT NULL,

    -- INV-09: for a SECRET_REFERENCE_ONLY key this must be a secret
    -- *reference*, and the store rejects material values — the same guard the
    -- write path applies, because this is a write path.

    reason               TEXT        NOT NULL,
    incident_id          TEXT        NOT NULL,
    actor_principal_id   TEXT        NOT NULL,

    -- INV-15 / NP-43: expiry is mandatory. A NULL would silently leave the
    -- change active forever; the column is the database backstop behind the
    -- handler's validation.
    expires_at           TIMESTAMPTZ NOT NULL,

    -- The value rows this change created and superseded, so expiry can
    -- revert exactly and evidence of the failure is retained (NP-44/47).
    activated_config_entry_id UUID REFERENCES config_entries(config_id),
    prior_config_entry_id     UUID REFERENCES config_entries(config_id),
    reverted_to_prior         BOOLEAN     NOT NULL DEFAULT false,

    -- Table 7 emergency lifecycle.
    status                 VARCHAR(32) NOT NULL DEFAULT 'OPEN'
        CONSTRAINT chk_emergency_changes_status CHECK (
            status IN ('OPEN','ACTIVE','EXPIRED','RETROSPECTIVE_PENDING','CLOSED')
        ),

    retrospective_due_at    TIMESTAMPTZ,
    retrospective_closed_at TIMESTAMPTZ,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_emergency_changes_expiry
    ON emergency_changes (environment, status, expires_at);

-- ── RLS ───────────────────────────────────────────────────────────────────────
-- Tenant-scoped reads (own + global), request-path writes confined to the
-- session tenant, and one named background escape — app.ops_sweep — for the
-- expiry/reversion loop that must touch every tenant's rows, mirroring the
-- outbox relay's app.outbox_relay in 000003.

ALTER TABLE kill_switches ENABLE ROW LEVEL SECURITY;
ALTER TABLE kill_switches FORCE ROW LEVEL SECURITY;
ALTER TABLE config_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE config_changes FORCE ROW LEVEL SECURITY;
ALTER TABLE emergency_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE emergency_changes FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON kill_switches;
CREATE POLICY tenant_isolation_policy ON kill_switches
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    );

DROP POLICY IF EXISTS tenant_isolation_policy ON config_changes;
CREATE POLICY tenant_isolation_policy ON config_changes
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    );

DROP POLICY IF EXISTS tenant_isolation_policy ON emergency_changes;
CREATE POLICY tenant_isolation_policy ON emergency_changes
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    );