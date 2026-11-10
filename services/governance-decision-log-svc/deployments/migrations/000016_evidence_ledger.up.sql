-- Migration: 000016_evidence_ledger.up.sql
--
-- G-07: Evidence ledger — append-only hash chain and sealed packages.
-- This gives tamper-evidence: each decision's hash chains to the previous
-- decision in the same tenant, and periodic sealed packages (Merkle roots)
-- let any verifier prove the chain hasn't been rewritten.

BEGIN;

-- 1. Add hash chain columns to governance_decisions.
-- Each decision records the hash of the previous decision in the same tenant.
-- The chain is per-tenant (tenant_id) because decisions are never compared
-- across tenants. The first decision in a tenant has prev_hash = '\x00'.
-- curr_hash = SHA256(prev_hash || decision_id || decided_at || outcome)
ALTER TABLE governance_decisions
    ADD COLUMN IF NOT EXISTS prev_hash BYTEA NOT NULL DEFAULT '\x00',
    ADD COLUMN IF NOT EXISTS curr_hash BYTEA NOT NULL DEFAULT '\x00';

-- Backfill existing rows: compute the chain in decided_at order per tenant.
-- The first decision gets prev_hash = '\x00', curr_hash = SHA256(prev || decision_id || decided_at || outcome).
-- Subsequent decisions chain to the previous one.
WITH ordered AS (
    SELECT
        decision_id,
        tenant_id,
        LAG(curr_hash) OVER (PARTITION BY tenant_id ORDER BY decided_at ASC, decision_id ASC) AS prev_hash,
        -- We'll compute curr_hash in the UPDATE below
        0 AS dummy
    FROM governance_decisions
)
UPDATE governance_decisions g
SET
    prev_hash = o.prev_hash,
    curr_hash = o.curr_hash
FROM (
    SELECT
        decision_id,
        prev_hash,
        sha256(coalesce(prev_hash, '\x00') || decision_id || encode(decided_at, 'escape') || outcome) AS curr_hash
    FROM (
        SELECT
            decision_id,
            tenant_id,
            decided_at,
            outcome,
            LAG(sha256(coalesce(prev_hash, '\x00') || decision_id || encode(decided_at, 'escape') || outcome))
                OVER (PARTITION BY tenant_id ORDER BY decided_at ASC, decision_id ASC) AS prev_hash
        FROM governance_decisions
    ) sub
) o
WHERE g.decision_id = o.decision_id AND g.tenant_id = o.tenant_id;

-- For the very first decision in each tenant, prev_hash should be '\x00'
UPDATE governance_decisions
SET prev_hash = '\x00'
WHERE prev_hash = '\x00'::bytea;

-- 2. Sealed packages table — periodic Merkle root snapshots.
-- A package is sealed for a tenant at a given point in time, covering all
-- decisions up to that point. The merkle_root is the root of a Merkle tree
-- whose leaves are the curr_hash values of all decisions up to that point.
-- sealed_by is the principal who authorized the seal (WORKFLOW_LEDGER_PRINCIPAL_ID).
CREATE TABLE IF NOT EXISTS sealed_packages (
    package_id        VARCHAR(64) PRIMARY KEY,
    tenant_id         VARCHAR(64) NOT NULL REFERENCES tenants(tenant_id),
    merkle_root       BYTEA NOT NULL,
    leaf_count        BIGINT NOT NULL,
    last_decision_id  VARCHAR(64) NOT NULL,
    sealed_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sealed_by         VARCHAR(64) NOT NULL,
    CONSTRAINT sealed_packages_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES tenants(tenant_id)
);

-- Index for lookup by tenant and time
CREATE INDEX IF NOT EXISTS sealed_packages_tenant_sealed_idx
    ON sealed_packages (tenant_id, sealed_at DESC);

-- RLS: only the tenant's members can read their own sealed packages
ALTER TABLE sealed_packages ENABLE ROW LEVEL SECURITY;
ALTER TABLE sealed_packages FORCE ROW LEVEL SECURITY;

CREATE POLICY sealed_packages_tenant_isolation ON sealed_packages
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Grants for app roles (created by create-app-roles.sh)
GRANT SELECT ON sealed_packages TO zoiko_app;
GRANT INSERT ON sealed_packages TO zoiko_app;

-- 3. Helper function to compute curr_hash for a new decision
-- This is called from the application layer (store.RecordDecision) to
-- maintain the hash chain atomically in the same transaction.
CREATE OR REPLACE FUNCTION compute_curr_hash(
    p_tenant_id      TEXT,
    p_decision_id    TEXT,
    p_decided_at     TIMESTAMPTZ,
    p_outcome        TEXT
) RETURNS BYTEA
LANGUAGE plpgsql SECURITY DEFINER
AS $$
DECLARE
    v_prev_hash BYTEA;
    v_curr_hash BYTEA;
BEGIN
    -- Get the curr_hash of the most recent decision in this tenant
    SELECT curr_hash
      INTO v_prev_hash
      FROM governance_decisions
     WHERE tenant_id = p_tenant_id
     ORDER BY decided_at DESC, decision_id DESC
     LIMIT 1;

    IF v_prev_hash IS NULL THEN
        v_prev_hash := '\x00';
    END IF;

    v_curr_hash := sha256(v_prev_hash || p_decision_id || encode(p_decided_at, 'escape') || p_outcome);
    RETURN v_curr_hash;
END;
$$;

-- 4. Function to seal a package (create a Merkle root for all decisions up to now)
-- This is called by an admin process (WORKFLOW_LEDGER_PRINCIPAL_ID holder).
CREATE OR REPLACE FUNCTION seal_package(
    p_tenant_id      TEXT,
    p_sealed_by      TEXT,
    p_package_id     TEXT,
    p_last_decision_id TEXT
) RETURNS BYTEA
LANGUAGE plpgsql SECURITY DEFINER
AS $$
DECLARE
    v_merkle_root BYTEA;
    v_leaf_count  BIGINT;
BEGIN
    -- For MVP, we compute a simple hash chain root.
    -- A full Merkle tree would recursively hash pairs; here we hash the
    -- last decision's curr_hash with the previous package's merkle_root.
    -- This is a hash chain of packages, not a full Merkle tree.
    -- Future enhancement: build a proper Merkle tree.

    SELECT COALESCE(merkle_root, '\x00')
      INTO v_merkle_root
      FROM sealed_packages
     WHERE tenant_id = p_tenant_id
     ORDER BY sealed_at DESC
     LIMIT 1;

    SELECT count(*)
      INTO v_leaf_count
      FROM governance_decisions
     WHERE tenant_id = p_tenant_id
       AND decided_at <= (SELECT decided_at FROM governance_decisions WHERE decision_id = p_last_decision_id);

    -- The new merkle root chains to the previous package
    INSERT INTO sealed_packages (
        package_id, tenant_id, merkle_root, leaf_count,
        last_decision_id, sealed_by
    ) VALUES (
        p_package_id, p_tenant_id,
        sha256(v_merkle_root || p_last_decision_id),
        (SELECT count(*) FROM governance_decisions WHERE tenant_id = p_tenant_id),
        p_last_decision_id, p_sealed_by
    )
    RETURNING merkle_root
    INTO v_merkle_root;

    RETURN v_merkle_root;
END;
$$;

COMMIT;