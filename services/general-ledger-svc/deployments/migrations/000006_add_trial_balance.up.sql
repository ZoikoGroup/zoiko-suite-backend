-- Migration: 000006_add_trial_balance.up.sql
--
-- ACC-15 (Trial Balance): before this migration, no service owned a
-- persisted, reproducible trial-balance dataset — financial-close-svc
-- recompiled one ad hoc, client-side, by paging raw journals on every
-- close attempt (see master-register-findings-2026-08-27.md §3.29's
-- audit). Invariant #11 requires "trial balances reconcile to an explicit
-- ledger watermark" — this adds both: a real monotonic watermark on the
-- ledger itself, and the durable trial-balance dataset pinned to it.

-- journal_seq is a real, monotonic, gap-tolerant ordering of every journal
-- header ever created — NOT the same thing as created_at (two journals can
-- share a timestamp at this platform's insert rate; same doctrine already
-- applied to privacy-purpose-registry-svc/privacy-consent-svc's
-- sequence_no tie-breaker columns for the identical reason). A trial
-- balance's watermark is the MAX(journal_seq) among the FINALIZED/REVERSED
-- journals it actually included — an unambiguous, reproducible answer to
-- "as of what point in the ledger was this compiled."
ALTER TABLE journal_headers ADD COLUMN journal_seq BIGSERIAL;

-- Backfill existing rows in their real creation order — ADD COLUMN
-- ... BIGSERIAL assigns values in an unspecified row order for rows that
-- already existed, which would make the watermark meaningless for any
-- journal posted before this migration ran.
WITH ordered AS (
    SELECT journal_id, ROW_NUMBER() OVER (ORDER BY created_at ASC, journal_id ASC) AS rn
    FROM journal_headers
)
UPDATE journal_headers jh
SET journal_seq = ordered.rn
FROM ordered
WHERE jh.journal_id = ordered.journal_id;

-- Keep the sequence itself ahead of the highest backfilled value so the
-- next real INSERT never collides with a backfilled journal_seq.
SELECT setval(
    pg_get_serial_sequence('journal_headers', 'journal_seq'),
    GREATEST((SELECT COALESCE(MAX(journal_seq), 0) FROM journal_headers), 1)
);

CREATE UNIQUE INDEX idx_journal_headers_journal_seq ON journal_headers (journal_seq);

CREATE TABLE trial_balance_snapshots (
    trial_balance_snapshot_id UUID PRIMARY KEY,
    tenant_id                 UUID NOT NULL,
    legal_entity_id           UUID NOT NULL,
    fiscal_period              VARCHAR(20) NOT NULL,
    ledger_watermark           BIGINT NOT NULL, -- MAX(journal_seq) of journals included
    compiled_at                TIMESTAMP WITH TIME ZONE NOT NULL,
    compiled_by_principal_id   VARCHAR(255) NOT NULL
);

CREATE TABLE trial_balance_lines (
    trial_balance_snapshot_id UUID NOT NULL REFERENCES trial_balance_snapshots(trial_balance_snapshot_id) ON DELETE CASCADE,
    tenant_id                  UUID NOT NULL,
    account_code               VARCHAR(64) NOT NULL,
    net_balance                NUMERIC(18,2) NOT NULL,
    PRIMARY KEY (trial_balance_snapshot_id, account_code)
);

-- Append-only: a trial balance snapshot is a permanent, reproducible
-- record of what the ledger said at a specific watermark. Never updated
-- or deleted from application code, and these triggers make that a
-- database-enforced invariant, not just discipline.
CREATE OR REPLACE FUNCTION reject_trial_balance_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_tb_snapshot_update
    BEFORE UPDATE ON trial_balance_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_trial_balance_mutation();
CREATE TRIGGER trg_reject_tb_snapshot_delete
    BEFORE DELETE ON trial_balance_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_trial_balance_mutation();

CREATE TRIGGER trg_reject_tb_line_update
    BEFORE UPDATE ON trial_balance_lines
    FOR EACH ROW EXECUTE FUNCTION reject_trial_balance_mutation();
CREATE TRIGGER trg_reject_tb_line_delete
    BEFORE DELETE ON trial_balance_lines
    FOR EACH ROW EXECUTE FUNCTION reject_trial_balance_mutation();

ALTER TABLE trial_balance_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE trial_balance_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON trial_balance_snapshots
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

ALTER TABLE trial_balance_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE trial_balance_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON trial_balance_lines
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

CREATE INDEX idx_tb_snapshots_tenant_entity_period ON trial_balance_snapshots (tenant_id, legal_entity_id, fiscal_period, compiled_at DESC);
CREATE INDEX idx_tb_lines_snapshot ON trial_balance_lines (trial_balance_snapshot_id);
