-- Migration: 000007_attestation.up.sql
--
-- Runtime attestation and drift findings: runtime_attestations and
-- drift_events.
--
-- WHY (ZS-SVC-AA-001, §8.3, Table 24 POST /runtime/attest, INV-22/23/24,
-- TC-07/TC-08, Table 22 drift classes, NP-26/51/54):
--
-- A fleet running stale configuration is undetectable today. The runtime
-- attestation reports the workload's identity and the exact snapshot/version
-- it believes it is serving; the store compares that against the current
-- attested snapshot and, when they disagree, records a DriftFinding (TC-08 —
-- desired vs observed state, side by side, never a label comparison).
--
--   runtime_attestations — workload identity + observed snapshot/epoch/digest
--                          + freshness metadata. Anti-replay is a UNIQUE
--                          (runtime_id, attest_key) constraint: a captured
--                          attestation cannot be replayed because the key
--                          is only ever consumed once (NP-26's "cannot attest
--                          yet system assumes current" is answered by the
--                          read path requiring a *fresh* attestation for the
--                          safety classes that need one).
--   drift_events         — recorded desired/observed pairs with the Table 22
--                          drift_class (STALE / UNAUTHORIZED / PARTIAL /
--                          INCOMPATIBLE / UNKNOWN), severity and remediation
--                          status (TC-08, NP-51: compares exact hashes, not
--                          human-readable labels).
--
-- RLS: both are runtime-scoped operational records that the resolution and
-- reconciliation paths read across tenants (the whole point is detecting
-- fleet-wide divergence), so reads are USING (true); writes come from the
-- attestation endpoint's session (request path, tenant confined) and the
-- reconciliation sweep (app.ops_sweep, the same named background escape
-- 000006 establishes).

-- ── runtime_attestations ──────────────────────────────────────────────────────

CREATE TABLE runtime_attestations (
    attestation_id       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Workload identity (service instance / runtime identity, TC-07).
    runtime_id           VARCHAR(255) NOT NULL,

    -- Anti-replay: one attestation per runtime per attest_key, consumed
    -- once (NP-26 / NP-24's "digest validation fails but runtime continues").
    attest_key           VARCHAR(255) NOT NULL,

    environment          VARCHAR(64) NOT NULL,
    tenant_id            UUID,

    -- The exact immutable snapshot the runtime reports serving (TC-07).
    observed_snapshot_id UUID        REFERENCES config_snapshots(snapshot_id),
    observed_epoch       BIGINT      NOT NULL,
    observed_digest      VARCHAR(64) NOT NULL,

    -- Observed per-runtime version hashes for finer-grain drift, if the
    -- runtime reports them.
    observed_versions    JSONB,

    reported_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    freshness_deadline   TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '24 hours',

    CONSTRAINT uq_runtime_attestations_key UNIQUE (runtime_id, attest_key)
);

CREATE INDEX idx_runtime_attestations_env_runtime
    ON runtime_attestations (environment, runtime_id, reported_at DESC);

-- ── drift_events ──────────────────────────────────────────────────────────────

CREATE TABLE drift_events (
    drift_id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    runtime_id           VARCHAR(255) NOT NULL,

    environment          VARCHAR(64) NOT NULL,
    tenant_id            UUID,

    -- Desired state as recorded in the attested snapshot (TC-08).
    desired_snapshot_id  UUID        REFERENCES config_snapshots(snapshot_id),
    desired_epoch        BIGINT,
    desired_digest       VARCHAR(64),

    -- Observed state as reported by the runtime (TC-08, NP-51: exact
    -- hashes side by side, never compared by label).
    observed_snapshot_id UUID        REFERENCES config_snapshots(snapshot_id),
    observed_epoch       BIGINT,
    observed_digest      VARCHAR(64),

    drift_class          VARCHAR(16) NOT NULL
        CONSTRAINT chk_drift_events_class CHECK (
            drift_class IN ('STALE','UNAUTHORIZED','PARTIAL','INCOMPATIBLE','UNKNOWN')
        ),

    severity             VARCHAR(16) NOT NULL DEFAULT 'MEDIUM'
        CONSTRAINT chk_drift_events_severity CHECK (
            severity IN ('LOW','MEDIUM','HIGH','CRITICAL')
        ),

    detected_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    remediated_at        TIMESTAMPTZ,
    remediation_status   VARCHAR(16) NOT NULL DEFAULT 'OPEN'
        CONSTRAINT chk_drift_events_remediation CHECK (
            remediation_status IN ('OPEN','REMEDIATED','ESCALATED')
        )
);

CREATE INDEX idx_drift_events_open
    ON drift_events (environment, detected_at DESC)
    WHERE remediation_status = 'OPEN';

-- ── RLS ───────────────────────────────────────────────────────────────────────

ALTER TABLE runtime_attestations ENABLE ROW LEVEL SECURITY;
ALTER TABLE runtime_attestations FORCE ROW LEVEL SECURITY;
ALTER TABLE drift_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE drift_events FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS attestations_read_all_write_scoped ON runtime_attestations;
CREATE POLICY attestations_read_all_write_scoped ON runtime_attestations FOR ALL
    USING (true)
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    );

DROP POLICY IF EXISTS drift_read_all_write_scoped ON drift_events;
CREATE POLICY drift_read_all_write_scoped ON drift_events FOR ALL
    USING (true)
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    );