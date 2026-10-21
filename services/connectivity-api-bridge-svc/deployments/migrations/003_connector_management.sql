-- Migration: 003_connector_management.sql
--
-- ZS-SVC-M-001 INT-05 Connector Management. api_bridges becomes the Connector:
-- a governed external-connection configuration with an approval lifecycle,
-- pinned provider profile, secret REFERENCE (never a secret), explicit
-- scope/purpose/destination/region, optimistic versioning and sync checkpoints.
--
-- The REST resource and table keep their names (/v1/bridges, api_bridges): the
-- decision was to extend this service in place and keep existing paths.
--
-- ── What happens to rows that already exist ────────────────────────────────
--   * They stay ACTIVE and keep working, flagged legacy_unapproved = true so
--     they are visible as never having been through approval. Their first
--     protected change sends them through approval like any other connector.
--   * They get the single scope 'ingest', which is all the old service could do.
--   * Any row whose endpoint_url carries an embedded credential (user:pass@ or
--     a secret-looking query parameter) is SUSPENDED and the credential is
--     STRIPPED from the stored URL. A plaintext secret must not be stored
--     (INT-CTRL-013, INT-NP-019); the operator re-registers the connection with
--     a credential_ref. Copies of the old value may still exist in backups and
--     logs and should be rotated at the source.
--
-- ── Guarantees held by the database itself ──────────────────────────────────
--   * A live connector's configuration equals what was approved
--     (config_fingerprint = approved_fingerprint), so a protected-field change
--     cannot leave a connector ACTIVE without re-approval (INT-NP-021/023).
--   * The approver is never the person who last changed the configuration.
--   * Approvals, configuration changes and health transitions are append-only.
--   * A sync checkpoint only moves forward, one version at a time, unless an
--     approved reset sets the transaction-local flag (INT-NP "checkpoint rollback").

-- ── 1. The connector ────────────────────────────────────────────────────────

ALTER TABLE api_bridges
    ADD COLUMN IF NOT EXISTS purpose                  VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS provider_key             VARCHAR(64)  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS provider_profile_version INTEGER      NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS credential_ref           VARCHAR(64),
    ADD COLUMN IF NOT EXISTS credential_verified_at   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS consent_expires_at       TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS destination_host         VARCHAR(255) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS destination_region       VARCHAR(32)  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS sync_mode                VARCHAR(16)  NOT NULL DEFAULT 'PUSH',
    ADD COLUMN IF NOT EXISTS scopes                   TEXT[]       NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS version                  BIGINT       NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS config_fingerprint       VARCHAR(80)  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS approved_fingerprint     VARCHAR(80)  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS approved_version         BIGINT       NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS created_by               VARCHAR(64)  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_changed_by          VARCHAR(64)  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS approved_by              VARCHAR(64)  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS approved_at              TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS status_reason            VARCHAR(255) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS legacy_unapproved        BOOLEAN      NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS idempotency_key          VARCHAR(128),
    ADD COLUMN IF NOT EXISTS idempotency_hash         VARCHAR(80);

-- Rows that exist now predate approval. (Idempotent: only rows not yet marked.)
UPDATE api_bridges
   SET legacy_unapproved = true,
       scopes            = ARRAY['ingest'],
       created_by        = CASE WHEN created_by = '' THEN 'legacy' ELSE created_by END,
       last_changed_by   = CASE WHEN last_changed_by = '' THEN 'legacy' ELSE last_changed_by END,
       destination_host  = COALESCE(substring(endpoint_url from '://(?:[^@/]*@)?([^/:?#]+)'), '')
 WHERE version = 1 AND config_fingerprint = '' AND approved_fingerprint = '' AND NOT legacy_unapproved;

-- Strip embedded credentials from stored URLs and stop using such connectors.
UPDATE api_bridges
   SET status        = 'SUSPENDED',
       status_reason = 'endpoint_url carried an embedded credential and was stripped; re-register with a credential_ref',
       endpoint_url  = regexp_replace(regexp_replace(endpoint_url, '://[^/@]*@', '://'), '\?.*$', '')
 WHERE endpoint_url ~* '://[^/@]*@'
    OR endpoint_url ~* '[?&](api[_-]?key|apikey|token|access[_-]?token|secret|client[_-]?secret|password|passwd|auth|authorization|signature|sig)=';

ALTER TABLE api_bridges ALTER COLUMN status SET DEFAULT 'DRAFT';

ALTER TABLE api_bridges
    ADD CONSTRAINT api_bridges_status_valid CHECK (status IN
        ('DRAFT', 'PENDING_APPROVAL', 'ACTIVE', 'DEGRADED', 'SUSPENDED', 'REVOKED', 'ARCHIVED', 'INACTIVE')),
    ADD CONSTRAINT api_bridges_sync_mode_valid CHECK (sync_mode IN ('PULL', 'PUSH', 'NONE')),
    ADD CONSTRAINT api_bridges_version_positive CHECK (version >= 1),
    -- A live connector's configuration is exactly what was approved.
    ADD CONSTRAINT api_bridges_live_is_approved CHECK
        (status NOT IN ('ACTIVE', 'DEGRADED') OR legacy_unapproved
         OR (approved_fingerprint <> '' AND config_fingerprint = approved_fingerprint)),
    -- Separation of duties: the approver did not make the change they approved.
    ADD CONSTRAINT api_bridges_approver_not_maker CHECK
        (approved_by = '' OR approved_by <> last_changed_by),
    -- A connector that can be approved or used is fully specified.
    ADD CONSTRAINT api_bridges_live_is_specified CHECK
        (status NOT IN ('PENDING_APPROVAL', 'ACTIVE', 'DEGRADED') OR legacy_unapproved
         OR (purpose <> '' AND provider_key <> '' AND provider_profile_version > 0
             AND destination_region <> '' AND destination_host <> '' AND cardinality(scopes) > 0)),
    -- The stored URL never carries a credential.
    ADD CONSTRAINT api_bridges_no_credential_in_url CHECK
        (endpoint_url !~* '://[^/@]*@'
         AND endpoint_url !~* '[?&](api[_-]?key|apikey|token|access[_-]?token|secret|client[_-]?secret|password|passwd|auth|authorization|signature|sig)=');

CREATE UNIQUE INDEX IF NOT EXISTS api_bridges_idempotency_idx
    ON api_bridges (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS api_bridges_tenant_status_idx ON api_bridges (tenant_id, status);
CREATE INDEX IF NOT EXISTS api_bridges_profile_idx ON api_bridges (provider_key, provider_profile_version);

-- ── 2. Append-only evidence ─────────────────────────────────────────────────

CREATE FUNCTION connector_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

-- Every configuration change, with what changed (field names, never values).
CREATE TABLE connector_changes (
    change_id        UUID         PRIMARY KEY,
    connector_id     VARCHAR(64)  NOT NULL REFERENCES api_bridges(bridge_id),
    tenant_id        VARCHAR(64)  NOT NULL,
    from_version     BIGINT       NOT NULL,
    to_version       BIGINT       NOT NULL,
    change_kind      VARCHAR(32)  NOT NULL,
    changed_by       VARCHAR(64)  NOT NULL,
    changed_fields   TEXT[]       NOT NULL DEFAULT '{}',
    scopes_added     TEXT[]       NOT NULL DEFAULT '{}',
    scopes_removed   TEXT[]       NOT NULL DEFAULT '{}',
    prior_fingerprint VARCHAR(80) NOT NULL DEFAULT '',
    new_fingerprint  VARCHAR(80)  NOT NULL DEFAULT '',
    reason           VARCHAR(255) NOT NULL DEFAULT '',
    changed_at       TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX connector_changes_connector_idx ON connector_changes (connector_id, changed_at);
CREATE TRIGGER connector_changes_no_change BEFORE UPDATE OR DELETE ON connector_changes
    FOR EACH ROW EXECUTE FUNCTION connector_append_only();

-- Approval decisions, each bound to the exact configuration it covered.
CREATE TABLE connector_approvals (
    approval_id       UUID         PRIMARY KEY,
    connector_id      VARCHAR(64)  NOT NULL REFERENCES api_bridges(bridge_id),
    tenant_id         VARCHAR(64)  NOT NULL,
    connector_version BIGINT       NOT NULL,
    fingerprint       VARCHAR(80)  NOT NULL,
    decision          VARCHAR(16)  NOT NULL,
    requested_by      VARCHAR(64)  NOT NULL,
    decided_by        VARCHAR(64)  NOT NULL,
    reason            VARCHAR(255) NOT NULL DEFAULT '',
    decided_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT connector_approvals_decision_valid CHECK (decision IN ('APPROVED', 'REJECTED')),
    CONSTRAINT connector_approvals_distinct_people CHECK (decided_by <> requested_by)
);
CREATE INDEX connector_approvals_connector_idx ON connector_approvals (connector_id, decided_at);
CREATE TRIGGER connector_approvals_no_change BEFORE UPDATE OR DELETE ON connector_approvals
    FOR EACH ROW EXECUTE FUNCTION connector_append_only();

-- ── 3. Health ───────────────────────────────────────────────────────────────

CREATE TABLE connector_health (
    connector_id         VARCHAR(64) PRIMARY KEY REFERENCES api_bridges(bridge_id),
    tenant_id            VARCHAR(64) NOT NULL,
    state                VARCHAR(16) NOT NULL DEFAULT 'HEALTHY',
    reason_code          VARCHAR(48) NOT NULL DEFAULT '',
    detail               VARCHAR(255) NOT NULL DEFAULT '',
    consecutive_failures INTEGER     NOT NULL DEFAULT 0,
    last_checked_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_success_at      TIMESTAMPTZ,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT connector_health_state_valid CHECK (state IN ('HEALTHY', 'DEGRADED', 'UNHEALTHY')),
    CONSTRAINT connector_health_failures_nonneg CHECK (consecutive_failures >= 0)
);

CREATE TABLE connector_health_transitions (
    transition_id UUID        PRIMARY KEY,
    connector_id  VARCHAR(64) NOT NULL REFERENCES api_bridges(bridge_id),
    tenant_id     VARCHAR(64) NOT NULL,
    from_state    VARCHAR(16) NOT NULL,
    to_state      VARCHAR(16) NOT NULL,
    reason_code   VARCHAR(48) NOT NULL DEFAULT '',
    detail        VARCHAR(255) NOT NULL DEFAULT '',
    at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX connector_health_transitions_idx ON connector_health_transitions (connector_id, at);
CREATE TRIGGER connector_health_transitions_no_change BEFORE UPDATE OR DELETE ON connector_health_transitions
    FOR EACH ROW EXECUTE FUNCTION connector_append_only();

-- ── 4. Sync checkpoints ─────────────────────────────────────────────────────

CREATE TABLE sync_checkpoints (
    connector_id       VARCHAR(64)  NOT NULL REFERENCES api_bridges(bridge_id),
    tenant_id          VARCHAR(64)  NOT NULL,
    stream             VARCHAR(64)  NOT NULL DEFAULT 'default',
    cursor             TEXT         NOT NULL,
    watermark          TIMESTAMPTZ,
    checkpoint_version BIGINT       NOT NULL DEFAULT 1,
    updated_by         VARCHAR(64)  NOT NULL,
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (connector_id, stream),
    CONSTRAINT sync_checkpoints_cursor_bounded CHECK (length(cursor) BETWEEN 1 AND 2048),
    CONSTRAINT sync_checkpoints_version_positive CHECK (checkpoint_version >= 1)
);

-- A checkpoint advances one version at a time and its watermark never goes
-- back. Only an approved reset (which sets app.checkpoint_reset for its own
-- transaction) may move it backwards; that is the single sanctioned path.
CREATE FUNCTION sync_checkpoints_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.connector_id <> OLD.connector_id OR NEW.stream <> OLD.stream OR NEW.tenant_id <> OLD.tenant_id THEN
        RAISE EXCEPTION 'sync checkpoint identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.checkpoint_version <> OLD.checkpoint_version + 1 THEN
        RAISE EXCEPTION 'sync checkpoint version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF COALESCE(current_setting('app.checkpoint_reset', true), '') <> 'true'
       AND OLD.watermark IS NOT NULL AND (NEW.watermark IS NULL OR NEW.watermark < OLD.watermark) THEN
        RAISE EXCEPTION 'sync checkpoint watermark cannot move backwards' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER sync_checkpoints_guard_trg BEFORE UPDATE ON sync_checkpoints
    FOR EACH ROW EXECUTE FUNCTION sync_checkpoints_guard();
CREATE TRIGGER sync_checkpoints_no_delete BEFORE DELETE ON sync_checkpoints
    FOR EACH ROW EXECUTE FUNCTION connector_append_only();

CREATE TABLE checkpoint_reset_requests (
    request_id   UUID         PRIMARY KEY,
    connector_id VARCHAR(64)  NOT NULL REFERENCES api_bridges(bridge_id),
    tenant_id    VARCHAR(64)  NOT NULL,
    stream       VARCHAR(64)  NOT NULL,
    to_cursor    TEXT         NOT NULL,
    to_watermark TIMESTAMPTZ,
    reason       VARCHAR(255) NOT NULL,
    requested_by VARCHAR(64)  NOT NULL,
    status       VARCHAR(16)  NOT NULL DEFAULT 'PENDING',
    decided_by   VARCHAR(64),
    decided_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT checkpoint_reset_status_valid CHECK (status IN ('PENDING', 'APPLIED', 'REJECTED')),
    CONSTRAINT checkpoint_reset_distinct_people CHECK (decided_by IS NULL OR decided_by <> requested_by),
    CONSTRAINT checkpoint_reset_cursor_bounded CHECK (length(to_cursor) BETWEEN 1 AND 2048)
);
CREATE INDEX checkpoint_reset_connector_idx ON checkpoint_reset_requests (connector_id, created_at);

-- ── 5. External provider profiles (platform-scope, versioned) ───────────────

CREATE TABLE external_profiles (
    profile_id            UUID         PRIMARY KEY,
    provider_key          VARCHAR(64)  NOT NULL,
    provider_type         VARCHAR(24)  NOT NULL,
    version               INTEGER      NOT NULL,
    status                VARCHAR(16)  NOT NULL DEFAULT 'DRAFT',
    allowed_regions       TEXT[]       NOT NULL,
    allowed_host_suffixes TEXT[]       NOT NULL,
    supported_scopes      TEXT[]       NOT NULL,
    schema_ref            VARCHAR(255) NOT NULL DEFAULT '',
    certification_ref     VARCHAR(128) NOT NULL DEFAULT '',
    published_by          VARCHAR(64)  NOT NULL,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deprecated_at         TIMESTAMPTZ,
    sunset_at             TIMESTAMPTZ,
    CONSTRAINT external_profiles_unique_version UNIQUE (provider_key, version),
    CONSTRAINT external_profiles_version_positive CHECK (version >= 1),
    CONSTRAINT external_profiles_type_valid CHECK (provider_type IN
        ('BANK', 'PSP', 'TAX_AUTHORITY', 'EINVOICE_NETWORK', 'IDP', 'HRIS', 'ESIGNATURE', 'FILE_TRANSFER', 'API')),
    CONSTRAINT external_profiles_status_valid CHECK (status IN ('DRAFT', 'CERTIFIED', 'DEPRECATED', 'UNSUPPORTED')),
    CONSTRAINT external_profiles_regions_nonempty CHECK (cardinality(allowed_regions) > 0),
    CONSTRAINT external_profiles_hosts_nonempty CHECK (cardinality(allowed_host_suffixes) > 0)
);

-- A profile's meaning is fixed once published: only its status (moving
-- forward) and the dates that go with it may change. A breaking change is a new
-- version, never an edit (spec section 6).
CREATE FUNCTION external_profiles_guard() RETURNS trigger AS $$
DECLARE
    old_rank INT; new_rank INT;
BEGIN
    IF NEW.profile_id <> OLD.profile_id OR NEW.provider_key <> OLD.provider_key OR NEW.provider_type <> OLD.provider_type
       OR NEW.version <> OLD.version OR NEW.allowed_regions <> OLD.allowed_regions
       OR NEW.allowed_host_suffixes <> OLD.allowed_host_suffixes OR NEW.supported_scopes <> OLD.supported_scopes
       OR NEW.schema_ref <> OLD.schema_ref OR NEW.published_by <> OLD.published_by THEN
        RAISE EXCEPTION 'a published provider profile cannot be edited; publish a new version'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    old_rank := CASE OLD.status WHEN 'DRAFT' THEN 0 WHEN 'CERTIFIED' THEN 1 WHEN 'DEPRECATED' THEN 2 ELSE 3 END;
    new_rank := CASE NEW.status WHEN 'DRAFT' THEN 0 WHEN 'CERTIFIED' THEN 1 WHEN 'DEPRECATED' THEN 2 ELSE 3 END;
    IF new_rank < old_rank THEN
        RAISE EXCEPTION 'a provider profile status only moves forward' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER external_profiles_guard_trg BEFORE UPDATE ON external_profiles
    FOR EACH ROW EXECUTE FUNCTION external_profiles_guard();
CREATE TRIGGER external_profiles_no_delete BEFORE DELETE ON external_profiles
    FOR EACH ROW EXECUTE FUNCTION connector_append_only();

-- ── 6. Row-level security ───────────────────────────────────────────────────
-- Same posture as 002: FORCE'd, tenant equality, NULLIF so an empty setting
-- matches nothing.

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['connector_changes', 'connector_approvals', 'connector_health',
                             'connector_health_transitions', 'sync_checkpoints', 'checkpoint_reset_requests']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))$p$, t);
    END LOOP;
END $$;

-- Profiles are platform reference data: every tenant may READ them (a connector
-- must be able to see the profile it is pinned to), but only the platform scope
-- may write them. 00000000-0000-0000-0000-00000000f001 is the platform-scope id
-- the rest of the estate uses for platform-wide reference data.
ALTER TABLE external_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE external_profiles FORCE ROW LEVEL SECURITY;
CREATE POLICY external_profiles_read ON external_profiles FOR SELECT USING (true);
CREATE POLICY external_profiles_platform_insert ON external_profiles FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.tenant_id', true), '') = '00000000-0000-0000-0000-00000000f001');
CREATE POLICY external_profiles_platform_update ON external_profiles FOR UPDATE
    USING (NULLIF(current_setting('app.tenant_id', true), '') = '00000000-0000-0000-0000-00000000f001')
    WITH CHECK (NULLIF(current_setting('app.tenant_id', true), '') = '00000000-0000-0000-0000-00000000f001');

-- ── 7. Transactional outbox, repair log and their guarantees ────────────────
-- Copied verbatim from zoiko.io/eventing (outbox.SchemaSQL, outbox.HardeningSQL,
-- repair.SchemaSQL, repair.RLSSQL). This service published straight to Kafka
-- after commit, which loses an event whenever the process dies between the two.


CREATE TABLE eventing_outbox (
    outbox_id           UUID         PRIMARY KEY,
    event_id            TEXT         NOT NULL UNIQUE,
    event_type          TEXT         NOT NULL,
    schema_version      TEXT         NOT NULL,
    tenant_id           TEXT         NOT NULL,
    region              TEXT         NOT NULL,
    aggregate_id        TEXT,
    aggregate_version   BIGINT,
    partition_key       TEXT         NOT NULL,
    payload             BYTEA        NOT NULL,
    payload_hash        TEXT         NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    publish_state       TEXT         NOT NULL DEFAULT 'pending',
    attempt_count       INTEGER      NOT NULL DEFAULT 0,
    next_attempt_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    claimed_until       TIMESTAMPTZ,
    dispatched_at       TIMESTAMPTZ,
    published_at        TIMESTAMPTZ,
    last_error_code     TEXT,
    last_error          TEXT,

    CONSTRAINT eventing_outbox_state_valid CHECK (publish_state IN
        ('pending', 'claimed', 'published', 'failed', 'quarantined')),
    CONSTRAINT eventing_outbox_attempts_nonneg CHECK (attempt_count >= 0),
    CONSTRAINT eventing_outbox_published_has_time CHECK
        (publish_state <> 'published' OR published_at IS NOT NULL),
    CONSTRAINT eventing_outbox_claimed_has_lease CHECK
        (publish_state <> 'claimed' OR claimed_until IS NOT NULL)
);

-- Everything not yet delivered, oldest first. Serves the dispatcher's claim
-- scan and the backlog signals; published rows (which accumulate until
-- retention removes them) are outside it, so neither query slows as history
-- grows.
CREATE INDEX eventing_outbox_backlog_idx ON eventing_outbox (created_at)
    WHERE publish_state IN ('pending', 'failed', 'claimed');
CREATE INDEX eventing_outbox_quarantined_idx ON eventing_outbox (created_at)
    WHERE publish_state = 'quarantined';

CREATE FUNCTION eventing_outbox_payload_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.event_id IS DISTINCT FROM OLD.event_id
       OR NEW.event_type IS DISTINCT FROM OLD.event_type
       OR NEW.schema_version IS DISTINCT FROM OLD.schema_version
       OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.region IS DISTINCT FROM OLD.region
       OR NEW.partition_key IS DISTINCT FROM OLD.partition_key
       OR NEW.payload IS DISTINCT FROM OLD.payload
       OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'eventing_outbox event content is immutable'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER eventing_outbox_content_immutable
    BEFORE UPDATE ON eventing_outbox
    FOR EACH ROW EXECUTE FUNCTION eventing_outbox_payload_immutable();

ALTER TABLE eventing_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_outbox FORCE ROW LEVEL SECURITY;

-- The relay drains every tenant's backlog from one background loop and
-- authenticates as app.outbox_relay, not as a tenant (eventing/outbox inRelayTx).
CREATE POLICY eventing_outbox_tenant_isolation ON eventing_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );

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
