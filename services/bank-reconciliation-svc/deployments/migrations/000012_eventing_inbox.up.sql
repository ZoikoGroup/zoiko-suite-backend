-- Migration: 000012_eventing_inbox.up.sql
--
-- The shared consumer inbox (ZS-SVC-M-001 INT-03) and the append-only repair log,
-- copied verbatim from zoiko.io/eventing/inbox.SchemaSQL, repair.SchemaSQL and
-- their RLSSQL. Replaces evidence_conflict_inbox for the evidence-conflict
-- consumer; that table is left in place (it is evidence of what was processed).

CREATE TABLE eventing_inbox (
    inbox_id          UUID         PRIMARY KEY,
    consumer          TEXT         NOT NULL,
    event_id          TEXT         NOT NULL,
    event_type        TEXT         NOT NULL,
    tenant_id         TEXT         NOT NULL,
    aggregate_id      TEXT,
    aggregate_version BIGINT,
    payload_hash      TEXT         NOT NULL,
    state             TEXT         NOT NULL DEFAULT 'processing',
    attempt_count     INTEGER      NOT NULL DEFAULT 1,
    duplicate_count   INTEGER      NOT NULL DEFAULT 0,
    received_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,
    result_ref        TEXT,
    last_error_code   TEXT,
    last_error        TEXT,

    CONSTRAINT eventing_inbox_unique_delivery UNIQUE (consumer, tenant_id, event_id),
    CONSTRAINT eventing_inbox_state_valid CHECK (state IN
        ('received', 'processing', 'completed', 'failed_retryable', 'quarantined', 'superseded')),
    CONSTRAINT eventing_inbox_attempts_nonneg CHECK (attempt_count >= 0 AND duplicate_count >= 0),
    CONSTRAINT eventing_inbox_completed_has_time CHECK
        (state NOT IN ('completed', 'superseded') OR completed_at IS NOT NULL)
);

-- Per-aggregate ordering check: highest completed version for an aggregate.
CREATE INDEX eventing_inbox_aggregate_idx
    ON eventing_inbox (consumer, tenant_id, aggregate_id, aggregate_version)
    WHERE state = 'completed' AND aggregate_id IS NOT NULL;

-- Everything that needs a decision, oldest first.
CREATE INDEX eventing_inbox_attention_idx ON eventing_inbox (updated_at)
    WHERE state IN ('failed_retryable', 'quarantined', 'processing', 'received');

CREATE TABLE eventing_repair_log (
    repair_id     UUID         PRIMARY KEY,
    target_kind   TEXT         NOT NULL,
    consumer      TEXT         NOT NULL DEFAULT '',
    tenant_id     TEXT         NOT NULL,
    event_id      TEXT         NOT NULL,
    action        TEXT         NOT NULL,
    operator      TEXT         NOT NULL,
    approver      TEXT         NOT NULL,
    approval_ref  TEXT         NOT NULL,
    reason        TEXT         NOT NULL,
    prior_state   TEXT         NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT eventing_repair_log_kind_valid CHECK (target_kind IN ('outbox', 'inbox')),
    CONSTRAINT eventing_repair_log_distinct_people CHECK (operator <> approver)
);

CREATE INDEX eventing_repair_log_event_idx ON eventing_repair_log (event_id, created_at);

CREATE FUNCTION eventing_repair_log_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'eventing_repair_log is append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER eventing_repair_log_no_change
    BEFORE UPDATE OR DELETE ON eventing_repair_log
    FOR EACH ROW EXECUTE FUNCTION eventing_repair_log_immutable();

ALTER TABLE eventing_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_inbox FORCE ROW LEVEL SECURITY;

CREATE POLICY eventing_inbox_tenant_isolation ON eventing_inbox FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE eventing_repair_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_repair_log FORCE ROW LEVEL SECURITY;

CREATE POLICY eventing_repair_log_tenant_isolation ON eventing_repair_log FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
