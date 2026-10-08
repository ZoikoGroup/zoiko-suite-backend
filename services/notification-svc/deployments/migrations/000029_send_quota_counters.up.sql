-- 000029_send_quota_counters.up.sql
-- ZS-SVC-Y-001 NCD-03 section 6.5: per-tenant, per-recipient and per-intent send quotas, with
-- protected capacity for security messages.
--
-- One row per budget per fixed window. The counter is shared by every replica (that is why it
-- lives here and not in memory), and it is incremented in the SAME transaction that creates the
-- communication, so a send refused for exceeding a budget leaves no communication and no count.
-- Rows older than a day are removed as new windows open (see the store).

CREATE TABLE IF NOT EXISTS send_quota_counters (
    tenant_id     VARCHAR(255) NOT NULL,
    bucket        VARCHAR(400) NOT NULL,
    window_start  TIMESTAMPTZ  NOT NULL,
    count         INTEGER      NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, bucket, window_start),
    CONSTRAINT ck_quota_count_nonneg CHECK (count >= 0)
);

CREATE INDEX IF NOT EXISTS idx_quota_counters_window ON send_quota_counters (tenant_id, window_start);

ALTER TABLE send_quota_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE send_quota_counters FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS quota_tenant_isolation ON send_quota_counters;
CREATE POLICY quota_tenant_isolation ON send_quota_counters FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
