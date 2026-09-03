-- Migration: 000003_add_period_reopen.up.sql
--
-- ACC-14 (Period Close) invariant #6: "Hard-closed periods reject ordinary
-- posting; reopen is explicit, scoped, approved and evidenced." Before this
-- migration, LOCKED was a terminal state — no code path anywhere reopened
-- a period, so the "reopen is explicit... and evidenced" half of the
-- invariant had nothing to be evidenced. This adds the append-only audit
-- trail a real reopen needs; the state transition itself is a plain UPDATE
-- on fiscal_periods (LOCKED -> OPEN), same pattern as 000001's lock.

CREATE TABLE IF NOT EXISTS period_reopen_events (
    reopen_event_id         UUID PRIMARY KEY,
    tenant_id                VARCHAR(255) NOT NULL,
    fiscal_period_id         UUID NOT NULL REFERENCES fiscal_periods(fiscal_period_id) ON DELETE CASCADE,
    reason                   TEXT NOT NULL,
    reopened_by_principal_id VARCHAR(255) NOT NULL,
    reopened_at              TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Append-only: a reopen event is permanent evidence of the fact a period was
-- reopened and why. No UPDATE/DELETE path is ever wired to this table from
-- application code, and the trigger below makes that a database-enforced
-- invariant too, same doctrine as this platform's other append-only
-- evidence tables (e.g. privacy-consent-svc's receipt tables).
CREATE OR REPLACE FUNCTION reject_period_reopen_event_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'period_reopen_events is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_period_reopen_event_update
    BEFORE UPDATE ON period_reopen_events
    FOR EACH ROW EXECUTE FUNCTION reject_period_reopen_event_mutation();

CREATE TRIGGER trg_reject_period_reopen_event_delete
    BEFORE DELETE ON period_reopen_events
    FOR EACH ROW EXECUTE FUNCTION reject_period_reopen_event_mutation();

ALTER TABLE period_reopen_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE period_reopen_events FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON period_reopen_events
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_period_reopen_events_period ON period_reopen_events (fiscal_period_id);
CREATE INDEX idx_period_reopen_events_tenant ON period_reopen_events (tenant_id);
