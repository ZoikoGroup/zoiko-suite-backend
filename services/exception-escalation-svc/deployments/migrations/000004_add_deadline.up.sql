-- +migrate Up
BEGIN;

-- BIZ-08 Business Deadline — a fourth bolted-on domain in this service,
-- beside ExceptionCase/EscalationRecord, AuditFinding, and Task/Case. The
-- doc's own recommended implementation sequence pairs BIZ-05 Task/Case
-- with BIZ-08 Business Deadline in the same wave, and BIZ-05's own
-- dependency list references "BIZ-08 deadlines" — these two domains are
-- meant to be operationally coupled, which this service lets them be
-- without a network hop. TEXT ids ("deadline-<uuid>") to match this
-- service's own existing convention, not native UUID columns.
--
-- BIZ-08 owns generic operational deadline instances for non-tax/
-- non-legal business obligations. It may MIRROR an authoritative TAX/LEG
-- deadline for work-coordination purposes, but per the doc's own
-- prohibited anti-pattern ("Recalculating legal/tax deadlines in BIZ-08
-- instead of consuming authoritative source"), a mirrored deadline's due
-- date can never be recalculated locally once source_type is set — see
-- internal/store/deadline_store.go's own doc comment on Recalculate
-- (Wave 2).
--
-- Upcoming/Due/Overdue are NOT stored statuses — same "no background
-- clock service" doctrine as BIZ-05's own SLAState/GetSLAState (see
-- migration 000003's doc comment): they are computed live from due_at
-- against SCHEDULED, so there is no clock that could silently fail to
-- auto-transition a deadline.
CREATE TABLE deadlines (
    deadline_id           TEXT        NOT NULL,
    tenant_id             TEXT        NOT NULL,
    legal_entity_id       TEXT        NOT NULL,
    title                 TEXT        NOT NULL,
    -- No hard FK — same opaque-reference posture this service already
    -- uses for tasks.linked_object_type/linked_object_id.
    linked_object_type    TEXT        NOT NULL DEFAULT '',
    linked_object_id      TEXT        NOT NULL DEFAULT '',
    due_at                TIMESTAMPTZ NOT NULL,
    owner_principal_id    TEXT        NOT NULL DEFAULT '',
    status                TEXT        NOT NULL DEFAULT 'SCHEDULED'
        CHECK (status IN ('SCHEDULED', 'COMPLETED', 'WAIVED', 'CANCELLED', 'SUPERSEDED')),

    -- Mirroring / source lock. source_type = '' means this deadline is
    -- BIZ-08's own — Recalculate is only ever valid on such a row.
    source_type           TEXT        NOT NULL DEFAULT '',
    source_ref            TEXT        NOT NULL DEFAULT '',
    source_version        TEXT        NOT NULL DEFAULT '',

    -- Backs ExplainCalculation (Wave 2) — recorded at creation/
    -- recalculation time, not derived after the fact.
    calc_rule              TEXT        NOT NULL DEFAULT '',
    calc_inputs              TEXT        NOT NULL DEFAULT '',

    completed_at                TIMESTAMPTZ,
    completed_by_principal_id     TEXT        NOT NULL DEFAULT '',
    waived_at                       TIMESTAMPTZ,
    waived_by_principal_id            TEXT        NOT NULL DEFAULT '',
    waiver_reason                       TEXT        NOT NULL DEFAULT '',
    cancelled_at                          TIMESTAMPTZ,
    cancelled_by_principal_id               TEXT        NOT NULL DEFAULT '',
    cancel_reason                             TEXT        NOT NULL DEFAULT '',
    -- Set by a later MirrorAuthoritativeDeadline call that supersedes
    -- this row rather than silently overwriting due_at — see
    -- deadline_store.go's own doc comment on MirrorAuthoritativeDeadline.
    superseded_at                               TIMESTAMPTZ,
    superseded_by_deadline_id                     TEXT,

    created_by                                    TEXT        NOT NULL,
    created_at                                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (deadline_id, tenant_id)
);
CREATE INDEX idx_deadlines_tenant_entity ON deadlines (tenant_id, legal_entity_id);
CREATE INDEX idx_deadlines_status ON deadlines (tenant_id, status);
CREATE INDEX idx_deadlines_due_at ON deadlines (tenant_id, due_at) WHERE status = 'SCHEDULED';
CREATE INDEX idx_deadlines_owner ON deadlines (tenant_id, owner_principal_id) WHERE owner_principal_id <> '';
-- Only one live (non-superseded) mirror per authoritative source, per
-- tenant — the row this service treats as "the" local copy of that
-- source's deadline.
CREATE UNIQUE INDEX idx_deadlines_live_source ON deadlines (tenant_id, source_type, source_ref)
    WHERE source_type <> '' AND status != 'SUPERSEDED';

CREATE TABLE deadline_escalations (
    escalation_id             TEXT        NOT NULL,
    deadline_id                TEXT        NOT NULL,
    tenant_id                    TEXT        NOT NULL,
    escalated_to_role              TEXT        NOT NULL,
    reason                            TEXT        NOT NULL DEFAULT '',
    escalated_at                        TIMESTAMPTZ NOT NULL DEFAULT now(),
    escalated_by_principal_id             TEXT        NOT NULL,
    PRIMARY KEY (escalation_id, tenant_id)
);
CREATE INDEX idx_deadline_escalations_deadline ON deadline_escalations (tenant_id, deadline_id, escalated_at);

ALTER TABLE deadlines ENABLE ROW LEVEL SECURITY;
ALTER TABLE deadlines FORCE ROW LEVEL SECURITY;
CREATE POLICY deadlines_tenant_isolation ON deadlines
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE deadline_escalations ENABLE ROW LEVEL SECURITY;
ALTER TABLE deadline_escalations FORCE ROW LEVEL SECURITY;
CREATE POLICY deadline_escalations_tenant_isolation ON deadline_escalations
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Append-only, same doctrine as this service's existing evidentiary
-- child tables (migrations 000002, 000003).
CREATE OR REPLACE FUNCTION reject_deadline_escalation_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'deadline escalations are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_deadline_escalation_mutation
    BEFORE UPDATE OR DELETE ON deadline_escalations
    FOR EACH ROW EXECUTE FUNCTION reject_deadline_escalation_mutation();

COMMIT;
