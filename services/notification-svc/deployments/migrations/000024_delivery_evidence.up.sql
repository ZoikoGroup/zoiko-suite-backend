-- 000024_delivery_evidence.up.sql
-- ZS-SVC-Y-001 NCD-04 section 7.1 (evidence normalization), NP-27.
--
-- A provider callback about a direct send (delivered, bounced, complaint ...) used to leave
-- no trace on the notification: the callback processor only wrote evidence for ledger
-- attempts, whose foreign keys point at ledger rows. This table is the direct path's
-- evidence trail: one normalized FACT per callback, tied to the exact attempt.
--
-- A fact has a strength and stated limits and never claims more than a provider can know;
-- none of them means a person saw or acknowledged a message. The table is append-only (a
-- late correction is a later fact, not an edited one) and idempotent per provider event.
-- No raw provider payload is stored: complaint feedback can contain personal data, so only
-- a short diagnostic code is kept.

CREATE TABLE IF NOT EXISTS notification_delivery_evidence (
    evidence_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        VARCHAR(255) NOT NULL,
    notification_id  UUID NOT NULL REFERENCES notifications (notification_id),
    attempt_id       UUID NOT NULL REFERENCES notification_delivery_attempts (attempt_id),
    source_event_id  VARCHAR(255) NOT NULL,
    provider         VARCHAR(64)  NOT NULL,
    fact             VARCHAR(24)  NOT NULL,
    strength         VARCHAR(24)  NOT NULL,
    diagnostic       VARCHAR(200),
    occurred_at      TIMESTAMPTZ  NOT NULL,
    recorded_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT ck_nde_fact CHECK (fact IN ('MAILBOX_ACCEPTED','BOUNCED','DEFERRED','REJECTED','COMPLAINT','UNSUBSCRIBED')),
    CONSTRAINT ck_nde_strength CHECK (strength IN ('PROVIDER_LEVEL','MAILBOX_LEVEL','RECIPIENT_SIGNAL')),
    CONSTRAINT uq_nde_event UNIQUE (tenant_id, attempt_id, source_event_id)
);

CREATE INDEX IF NOT EXISTS idx_nde_notification ON notification_delivery_evidence (tenant_id, notification_id, occurred_at);

ALTER TABLE notification_delivery_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_delivery_evidence FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS nde_tenant_isolation ON notification_delivery_evidence;
CREATE POLICY nde_tenant_isolation ON notification_delivery_evidence FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Append-only: evidence is never edited or removed.
CREATE OR REPLACE FUNCTION guard_delivery_evidence() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'delivery evidence is append-only; add a later fact instead' USING ERRCODE = '23514';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_guard_delivery_evidence ON notification_delivery_evidence;
CREATE TRIGGER trg_guard_delivery_evidence BEFORE UPDATE OR DELETE ON notification_delivery_evidence
    FOR EACH ROW EXECUTE FUNCTION guard_delivery_evidence();
