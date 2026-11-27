-- ZS-SVC-Z-001 RB-05 / DQC-017 (docs/architecture/DQC_GAP_MATRIX.md, "certification watch metrics").
--
-- Two gauges are read at scrape time, like the outbox backlog: how many flags are OPEN (journals and postings resting on a revoked
-- certification that no person has decided on) and how old the oldest is. They are counts across ALL tenants, and certification_flags is
-- tenant-isolated with row-level security FORCEd, so the read needs the same narrow capability migration 000037 gave the sweep's scan
-- of certification_watch: SELECT only, recognised by a transaction-scoped setting. The code reads only count and min(raised_at); it
-- never selects a row's content.
--
-- The partial index keeps that read cheap whatever the history: it covers only the flags still open.

CREATE POLICY certification_sweeper_scan ON certification_flags
    FOR SELECT
    USING (current_setting('app.certification_sweeper', true) = 'true');

CREATE INDEX idx_certification_flags_open_raised ON certification_flags (raised_at) WHERE status = 'OPEN';
