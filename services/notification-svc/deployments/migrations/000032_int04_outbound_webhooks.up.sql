-- 000032 ZS-SVC-M-001 INT-04: tenant-configured outbound webhooks.
--
-- NOT to be confused with the inbound provider-receipt webhooks of migration 000009
-- (webhook_dlq): those are events a provider sends US about a notification. These
-- are events WE send to a destination a tenant configured.
--
-- What the database guarantees on its own (the service cannot be trusted to be the
-- only writer):
--   * a subscription is ACTIVE only if the configuration now stored is exactly the
--     configuration that was approved, and the approver is not the last person who
--     changed it (separation of duties);
--   * a destination never carries credentials, a query string or a fragment;
--   * the subscription log and the delivery-attempt evidence are append-only;
--   * a delivery's body, hash and identity never change, and a succeeded delivery
--     never changes at all;
--   * every table is tenant-isolated by row-level security, FORCEd.

CREATE TABLE IF NOT EXISTS webhook_subscriptions (
    subscription_id           UUID         PRIMARY KEY,
    tenant_id                 VARCHAR(255) NOT NULL,
    legal_entity_id           VARCHAR(255) NOT NULL CHECK (legal_entity_id <> ''),
    name                      VARCHAR(200) NOT NULL CHECK (name <> ''),
    destination_url           TEXT         NOT NULL,
    event_types               TEXT[]       NOT NULL,
    -- Minimization (INT-CTRL-011): the only payload fields this destination may receive.
    allowed_fields            TEXT[]       NOT NULL DEFAULT '{}',
    api_version               VARCHAR(20)  NOT NULL DEFAULT 'v1',
    -- A reference to a signing secret policy path in secret-vault-integration-svc.
    -- The key itself is never stored here and never leaves the vault.
    signing_key_path          TEXT         NOT NULL CHECK (signing_key_path <> ''),
    previous_signing_key_path TEXT,
    previous_key_valid_until  TIMESTAMPTZ,
    destination_region        VARCHAR(40)  NOT NULL CHECK (destination_region <> ''),
    max_attempts              INTEGER      NOT NULL DEFAULT 8 CHECK (max_attempts BETWEEN 1 AND 20),
    status                    VARCHAR(20)  NOT NULL DEFAULT 'DRAFT' CHECK (status IN
                                 ('DRAFT','PENDING_APPROVAL','ACTIVE','SUSPENDED','DISABLED','REVOKED')),
    status_reason             TEXT         NOT NULL DEFAULT '',
    version                   INTEGER      NOT NULL DEFAULT 1 CHECK (version >= 1),
    -- SHA-256 over everything an approver is approving (destination, event types,
    -- fields, signing key reference, region). Any change moves it.
    config_fingerprint        VARCHAR(64)  NOT NULL CHECK (config_fingerprint ~ '^[0-9a-f]{64}$'),
    approved_fingerprint      VARCHAR(64),
    created_by                VARCHAR(255) NOT NULL,
    last_changed_by           VARCHAR(255) NOT NULL,
    approved_by               VARCHAR(255),
    approved_at               TIMESTAMPTZ,
    created_at                TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ  NOT NULL DEFAULT now(),

    -- destination health (INT-04 NFR: circuit breaker, no global retry storm)
    consecutive_failures      INTEGER      NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    consecutive_dead_letters  INTEGER      NOT NULL DEFAULT 0 CHECK (consecutive_dead_letters >= 0),
    breaker_open_until        TIMESTAMPTZ,
    last_success_at           TIMESTAMPTZ,
    last_failure_at           TIMESTAMPTZ,

    CONSTRAINT webhook_sub_event_types_nonempty CHECK (cardinality(event_types) >= 1),
    CONSTRAINT webhook_sub_url_has_no_credentials CHECK (destination_url !~ '[@?#]' AND destination_url ~* '^https?://[^/]+'),
    CONSTRAINT webhook_sub_previous_key_pair CHECK ((previous_signing_key_path IS NULL) = (previous_key_valid_until IS NULL)),
    CONSTRAINT webhook_sub_previous_key_differs CHECK (previous_signing_key_path IS NULL OR previous_signing_key_path <> signing_key_path),
    -- A live subscription is the approved configuration, approved by someone other
    -- than the person who last changed it.
    CONSTRAINT webhook_sub_live_is_approved CHECK (
        status NOT IN ('ACTIVE', 'SUSPENDED', 'DISABLED')
        OR (approved_fingerprint IS NOT NULL AND approved_fingerprint = config_fingerprint
            AND approved_by IS NOT NULL AND approved_at IS NOT NULL AND approved_by <> last_changed_by)),
    CONSTRAINT webhook_sub_pending_not_approved CHECK (status <> 'PENDING_APPROVAL' OR approved_by IS NULL OR approved_fingerprint IS DISTINCT FROM config_fingerprint)
);
CREATE INDEX IF NOT EXISTS idx_webhook_sub_tenant_status ON webhook_subscriptions (tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_webhook_sub_event_types ON webhook_subscriptions USING GIN (event_types) WHERE status = 'ACTIVE';

CREATE OR REPLACE FUNCTION webhook_sub_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'webhook subscriptions are never deleted; revoke them' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'REVOKED' THEN
        RAISE EXCEPTION 'webhook subscription % is revoked and cannot change', OLD.subscription_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.subscription_id IS DISTINCT FROM OLD.subscription_id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'webhook subscription identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version < OLD.version THEN
        RAISE EXCEPTION 'webhook subscription version cannot go backwards' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- A protected change to a subscription that was approved must withdraw the approval.
    IF NEW.config_fingerprint IS DISTINCT FROM OLD.config_fingerprint
       AND NEW.status IN ('ACTIVE', 'SUSPENDED', 'DISABLED')
       AND NEW.approved_fingerprint IS NOT DISTINCT FROM OLD.approved_fingerprint THEN
        RAISE EXCEPTION 'a changed webhook configuration needs a new approval' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_webhook_sub_guard ON webhook_subscriptions;
CREATE TRIGGER trg_webhook_sub_guard BEFORE UPDATE OR DELETE ON webhook_subscriptions
    FOR EACH ROW EXECUTE FUNCTION webhook_sub_guard();

-- Everything that happens to a subscription, append-only: the evidence behind
-- "who approved this destination and what exactly did they approve".
CREATE TABLE IF NOT EXISTS webhook_subscription_log (
    log_id          BIGSERIAL    PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    subscription_id UUID         NOT NULL REFERENCES webhook_subscriptions (subscription_id),
    kind            VARCHAR(30)  NOT NULL CHECK (kind IN
                       ('CREATED','UPDATED','SUBMITTED','APPROVED','REJECTED','SUSPENDED','RESUMED',
                        'DISABLED','REVOKED','KEY_ROTATED','REPLAY_REQUESTED','GRACE_KEY_RETIRED')),
    actor           VARCHAR(255) NOT NULL,
    approver        VARCHAR(255),
    approval_ref    VARCHAR(255),
    from_status     VARCHAR(20),
    to_status       VARCHAR(20),
    version         INTEGER      NOT NULL,
    fingerprint     VARCHAR(64),
    delivery_id     UUID,
    reason          TEXT         NOT NULL DEFAULT '',
    correlation_id  VARCHAR(255) NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT webhook_log_distinct_people CHECK (approver IS NULL OR approver <> actor)
);
CREATE INDEX IF NOT EXISTS idx_webhook_log_sub ON webhook_subscription_log (subscription_id, log_id);

-- One row per (subscription, event). delivery_id is derived from that pair
-- (UUIDv5), so a redelivered event can never create a second delivery and every
-- retry carries the same identity (INT-04 idempotency).
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    delivery_id        UUID         PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    subscription_id    UUID         NOT NULL REFERENCES webhook_subscriptions (subscription_id),
    event_id           TEXT         NOT NULL CHECK (event_id <> ''),
    event_type         TEXT         NOT NULL,
    event_occurred_at  TIMESTAMPTZ  NOT NULL,
    -- The minimized, final request body, exactly as it is signed and sent.
    body               BYTEA        NOT NULL,
    body_hash          VARCHAR(64)  NOT NULL CHECK (body_hash ~ '^[0-9a-f]{64}$'),
    -- The scope the body was minimized to; a body outside the CURRENT approved
    -- scope is blocked before it is sent.
    scope_fingerprint  VARCHAR(64)  NOT NULL,
    status             VARCHAR(20)  NOT NULL DEFAULT 'PENDING' CHECK (status IN
                          ('PENDING','SENT','SUCCEEDED','RETRYING','DEAD_LETTERED')),
    attempt_count      INTEGER      NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    locked_until       TIMESTAMPTZ,
    last_attempt_at    TIMESTAMPTZ,
    last_response_code INTEGER,
    last_error_class   VARCHAR(40),
    dead_letter_reason VARCHAR(60),
    dead_lettered_at   TIMESTAMPTZ,
    succeeded_at       TIMESTAMPTZ,
    replay_count       INTEGER      NOT NULL DEFAULT 0 CHECK (replay_count >= 0),
    -- attempts already spent when the delivery was last replayed: the retry budget of the
    -- current run is attempt_count - budget_base (attempt_count itself never decreases).
    budget_base        INTEGER      NOT NULL DEFAULT 0 CHECK (budget_base >= 0),
    correlation_id     VARCHAR(255) NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT webhook_delivery_unique_event UNIQUE (subscription_id, event_id),
    CONSTRAINT webhook_delivery_dlq_has_reason CHECK (status <> 'DEAD_LETTERED' OR (dead_letter_reason IS NOT NULL AND dead_lettered_at IS NOT NULL)),
    CONSTRAINT webhook_delivery_success_has_time CHECK (status <> 'SUCCEEDED' OR succeeded_at IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_due ON webhook_deliveries (next_attempt_at)
    WHERE status IN ('PENDING', 'RETRYING', 'SENT');
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_sub ON webhook_deliveries (subscription_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_dlq ON webhook_deliveries (tenant_id, dead_lettered_at DESC) WHERE status = 'DEAD_LETTERED';

CREATE OR REPLACE FUNCTION webhook_delivery_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'webhook deliveries are evidence and are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.delivery_id IS DISTINCT FROM OLD.delivery_id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.subscription_id IS DISTINCT FROM OLD.subscription_id OR NEW.event_id IS DISTINCT FROM OLD.event_id
       OR NEW.body IS DISTINCT FROM OLD.body OR NEW.body_hash IS DISTINCT FROM OLD.body_hash THEN
        RAISE EXCEPTION 'webhook delivery identity and body are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'SUCCEEDED' THEN
        RAISE EXCEPTION 'a succeeded webhook delivery never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'DEAD_LETTERED' AND NEW.status <> 'DEAD_LETTERED' AND NEW.replay_count <= OLD.replay_count THEN
        RAISE EXCEPTION 'a dead-lettered delivery returns to the queue only through an approved replay' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.attempt_count < OLD.attempt_count THEN
        RAISE EXCEPTION 'attempt_count never decreases' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_webhook_delivery_guard ON webhook_deliveries;
CREATE TRIGGER trg_webhook_delivery_guard BEFORE UPDATE OR DELETE ON webhook_deliveries
    FOR EACH ROW EXECUTE FUNCTION webhook_delivery_guard();

-- One row per attempt, written BEFORE the request leaves (IN_FLIGHT) and finished
-- once, so a crash mid-send leaves evidence of an attempt whose outcome is unknown
-- rather than no trace of it.
CREATE TABLE IF NOT EXISTS webhook_delivery_attempts (
    attempt_id          BIGSERIAL    PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    delivery_id         UUID         NOT NULL REFERENCES webhook_deliveries (delivery_id),
    subscription_id     UUID         NOT NULL,
    attempt_no          INTEGER      NOT NULL CHECK (attempt_no >= 1),
    started_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ,
    outcome             VARCHAR(20)  NOT NULL DEFAULT 'IN_FLIGHT' CHECK (outcome IN
                           ('IN_FLIGHT','SUCCEEDED','HTTP_ERROR','RATE_LIMITED','TIMEOUT','TLS_FAILURE',
                            'NETWORK_ERROR','EGRESS_BLOCKED','SIGNING_FAILED','BLOCKED','UNKNOWN')),
    response_code       INTEGER,
    response_body_hash  VARCHAR(64),
    response_body_bytes INTEGER,
    error_class         VARCHAR(40),
    destination_host    VARCHAR(255) NOT NULL DEFAULT '',
    payload_hash        VARCHAR(64)  NOT NULL,
    signature_profile   VARCHAR(10)  NOT NULL DEFAULT 'v1',
    signature_timestamp BIGINT,
    signature_key_paths TEXT[]       NOT NULL DEFAULT '{}',
    CONSTRAINT webhook_attempt_unique UNIQUE (delivery_id, attempt_no),
    CONSTRAINT webhook_attempt_finished CHECK (outcome = 'IN_FLIGHT' OR finished_at IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idx_webhook_attempts_sub ON webhook_delivery_attempts (subscription_id, attempt_id DESC);

CREATE OR REPLACE FUNCTION webhook_attempt_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'webhook delivery attempts are append-only evidence' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.outcome <> 'IN_FLIGHT' THEN
        RAISE EXCEPTION 'a finished webhook delivery attempt never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.delivery_id IS DISTINCT FROM OLD.delivery_id OR NEW.attempt_no IS DISTINCT FROM OLD.attempt_no
       OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN
        RAISE EXCEPTION 'webhook delivery attempt identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_webhook_attempt_guard ON webhook_delivery_attempts;
CREATE TRIGGER trg_webhook_attempt_guard BEFORE UPDATE OR DELETE ON webhook_delivery_attempts
    FOR EACH ROW EXECUTE FUNCTION webhook_attempt_guard();

CREATE OR REPLACE FUNCTION webhook_log_immutable() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'webhook_subscription_log is append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_webhook_log_immutable ON webhook_subscription_log;
CREATE TRIGGER trg_webhook_log_immutable BEFORE UPDATE OR DELETE ON webhook_subscription_log
    FOR EACH ROW EXECUTE FUNCTION webhook_log_immutable();

-- ── row-level security ──────────────────────────────────────────────────────
ALTER TABLE webhook_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_subscriptions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS webhook_subscriptions_tenant ON webhook_subscriptions;
CREATE POLICY webhook_subscriptions_tenant ON webhook_subscriptions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE webhook_subscription_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_subscription_log FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS webhook_subscription_log_tenant ON webhook_subscription_log;
CREATE POLICY webhook_subscription_log_tenant ON webhook_subscription_log FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE webhook_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_deliveries FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS webhook_deliveries_tenant ON webhook_deliveries;
CREATE POLICY webhook_deliveries_tenant ON webhook_deliveries FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
-- The dispatcher discovers due work across tenants from one loop. It names itself
-- (app.webhook_dispatch) instead of running unscoped; the flag admits it to SELECT
-- and UPDATE deliveries only, to claim work, and to nothing else. Every delivery is
-- then processed in a tenant-scoped transaction.
DROP POLICY IF EXISTS webhook_deliveries_dispatch_read ON webhook_deliveries;
CREATE POLICY webhook_deliveries_dispatch_read ON webhook_deliveries FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.webhook_dispatch', true), ''), 'false') = 'true');
DROP POLICY IF EXISTS webhook_deliveries_dispatch_claim ON webhook_deliveries;
CREATE POLICY webhook_deliveries_dispatch_claim ON webhook_deliveries FOR UPDATE
    USING (COALESCE(NULLIF(current_setting('app.webhook_dispatch', true), ''), 'false') = 'true')
    WITH CHECK (COALESCE(NULLIF(current_setting('app.webhook_dispatch', true), ''), 'false') = 'true');

ALTER TABLE webhook_delivery_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_delivery_attempts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS webhook_delivery_attempts_tenant ON webhook_delivery_attempts;
CREATE POLICY webhook_delivery_attempts_tenant ON webhook_delivery_attempts FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- ── shared consumer inbox + repair log (INT-03) ─────────────────────────────
-- Verbatim from zoiko.io/eventing/inbox.SchemaSQL, repair.SchemaSQL and their RLSSQL
-- (the same text bank-reconciliation-svc applies in its migration 000012). The inbox
-- is what makes the event fan-out exactly-once in effect.
CREATE TABLE IF NOT EXISTS eventing_inbox (
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
CREATE INDEX IF NOT EXISTS eventing_inbox_aggregate_idx
    ON eventing_inbox (consumer, tenant_id, aggregate_id, aggregate_version)
    WHERE state = 'completed' AND aggregate_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS eventing_inbox_attention_idx ON eventing_inbox (updated_at)
    WHERE state IN ('failed_retryable', 'quarantined', 'processing', 'received');

CREATE TABLE IF NOT EXISTS eventing_repair_log (
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
CREATE INDEX IF NOT EXISTS eventing_repair_log_event_idx ON eventing_repair_log (event_id, created_at);

CREATE OR REPLACE FUNCTION eventing_repair_log_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'eventing_repair_log is append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS eventing_repair_log_no_change ON eventing_repair_log;
CREATE TRIGGER eventing_repair_log_no_change
    BEFORE UPDATE OR DELETE ON eventing_repair_log
    FOR EACH ROW EXECUTE FUNCTION eventing_repair_log_immutable();

ALTER TABLE eventing_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_inbox FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS eventing_inbox_tenant_isolation ON eventing_inbox;
CREATE POLICY eventing_inbox_tenant_isolation ON eventing_inbox FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE eventing_repair_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_repair_log FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS eventing_repair_log_tenant_isolation ON eventing_repair_log;
CREATE POLICY eventing_repair_log_tenant_isolation ON eventing_repair_log FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );

-- ── events this domain emits through the transactional outbox ───────────────
-- Starts from the union list set by 000030 (its header: "Any later migration that
-- adds an event type must start from this list") and adds the INT-04 events.
ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known
    CHECK (event_type IN (
        'notification.sent',
        'notification.failed',
        'notification.outcome_unknown',
        'template.created',
        'template.version_approved',
        'template.published',
        'template.retired',
        'delivery.attempt.created',
        'delivery.attempt.unknown',
        'notice.dispatched',
        'notice.delivery_evidenced',
        'notice.exception',
        'notice.acknowledged',
        'notice.declined',
        'notice.disputed',
        'notice.expired',
        'notice.corrected',
        'communication.prepared',
        'communication.blocked',
        'delivery.evidence.recorded',
        'endpoint.suppressed',
        'notice.deadline.at_risk',
        'communication.cancelled',
        'communication.correction.issued',
        'communication.record.declared',
        'webhook.subscription.activated',
        'webhook.subscription.disabled',
        'webhook.delivery.failed',
        'webhook.delivery.dead-lettered',
        'webhook.replay.requested',
        'webhook.control.failed'
    ));
