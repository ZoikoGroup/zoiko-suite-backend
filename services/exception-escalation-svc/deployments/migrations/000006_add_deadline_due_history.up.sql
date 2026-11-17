-- +migrate Up
BEGIN;

-- BIZ-08: a manual Recalculate used to overwrite due_at in place, so the previous due date,
-- the rule it came from and the reason for the change were gone. The spec requires a manual
-- override to carry a reason and the evidence to keep the calculation lineage. Every
-- recalculation now writes one row here in the same transaction, append-only like the other
-- evidentiary tables in this service.
CREATE TABLE deadline_due_history (
    change_id          TEXT        NOT NULL,
    deadline_id        TEXT        NOT NULL,
    tenant_id          TEXT        NOT NULL,
    previous_due_at    TIMESTAMPTZ NOT NULL,
    new_due_at         TIMESTAMPTZ NOT NULL,
    previous_calc_rule TEXT        NOT NULL DEFAULT '',
    new_calc_rule      TEXT        NOT NULL DEFAULT '',
    reason             TEXT        NOT NULL CHECK (reason <> ''),
    changed_by         TEXT        NOT NULL,
    changed_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (change_id, tenant_id)
);
CREATE INDEX idx_deadline_due_history_deadline ON deadline_due_history (tenant_id, deadline_id, changed_at);

ALTER TABLE deadline_due_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE deadline_due_history FORCE ROW LEVEL SECURITY;
CREATE POLICY deadline_due_history_tenant_isolation ON deadline_due_history
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION reject_deadline_due_history_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'deadline due-date history is append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_deadline_due_history_mutation
    BEFORE UPDATE OR DELETE ON deadline_due_history
    FOR EACH ROW EXECUTE FUNCTION reject_deadline_due_history_mutation();

COMMIT;
