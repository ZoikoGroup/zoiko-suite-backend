-- Migration: 000013_int10_provisioning.up.sql
--
-- INT-10 Identity Provider / Provisioning Adapter, part 2: directory provisioning (SCIM), the approved
-- mapping from directory attributes to baseline assignments, the ledger of provisioning events, the
-- assignment work queue, sync exceptions and deprovisioning evidence.
--
-- Nothing here grants authority. Assignments are REQUESTED from access-control-svc, which owns role
-- authority and SoD, and which refuses anything above baseline risk when the request carries origin
-- FEDERATION. These tables record what was asked, what was answered and what is still owed.

-- ---------------------------------------------------------------------------
-- 1. Link and connection additions
-- ---------------------------------------------------------------------------

ALTER TABLE federation_connections ADD COLUMN active_mapping_version INT;

ALTER TABLE federation_links
    ADD COLUMN username                  VARCHAR(320) NOT NULL DEFAULT '',
    -- The directory attributes mappings read (groups, roles, department, title, userType, displayName). Never an
    -- email address: that lives on the principal and is not an identity binding.
    ADD COLUMN attributes                JSONB        NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN attributes_rev            INT          NOT NULL DEFAULT 1,
    ADD COLUMN deprovision_requested_at  TIMESTAMP WITH TIME ZONE,
    ADD COLUMN deprovision_due_at        TIMESTAMP WITH TIME ZONE,
    ADD COLUMN sessions_evicted_at       TIMESTAMP WITH TIME ZONE,
    ADD COLUMN access_revoked_at         TIMESTAMP WITH TIME ZONE,
    ADD COLUMN deprovision_escalated_at  TIMESTAMP WITH TIME ZONE;

-- ---------------------------------------------------------------------------
-- 2. SCIM bearer tokens (only the SHA-256 is stored; the token is shown once)
-- ---------------------------------------------------------------------------

CREATE TABLE federation_scim_tokens (
    token_hash     VARCHAR(64)  PRIMARY KEY,
    connection_id  VARCHAR(255) NOT NULL REFERENCES federation_connections(connection_id),
    tenant_id      VARCHAR(255) NOT NULL,
    status         VARCHAR(10)  NOT NULL DEFAULT 'ACTIVE',
    created_by     VARCHAR(255) NOT NULL,
    created_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    revoked_at     TIMESTAMP WITH TIME ZONE,
    last_used_at   TIMESTAMP WITH TIME ZONE,
    CONSTRAINT federation_scim_tokens_status_check CHECK (status IN ('ACTIVE', 'REVOKED'))
);
CREATE UNIQUE INDEX idx_federation_scim_one_active ON federation_scim_tokens (connection_id) WHERE status = 'ACTIVE';

-- ---------------------------------------------------------------------------
-- 3. Provisioning mapping: immutable, maker-checker versions (like connection versions)
-- ---------------------------------------------------------------------------

CREATE TABLE federation_mapping_versions (
    connection_id   VARCHAR(255) NOT NULL REFERENCES federation_connections(connection_id),
    tenant_id       VARCHAR(255) NOT NULL,
    version         INT          NOT NULL,
    status          VARCHAR(20)  NOT NULL DEFAULT 'PENDING_APPROVAL',
    config          JSONB        NOT NULL,
    fingerprint     VARCHAR(64)  NOT NULL,
    created_by      VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    decided_by      VARCHAR(255),
    decided_at      TIMESTAMP WITH TIME ZONE,
    decision_reason TEXT         NOT NULL DEFAULT '',
    approved_fingerprint VARCHAR(64),
    PRIMARY KEY (connection_id, version),
    CONSTRAINT federation_mapping_status_check CHECK (status IN ('PENDING_APPROVAL', 'APPROVED', 'REJECTED', 'SUPERSEDED')),
    CONSTRAINT federation_mapping_maker_checker CHECK (decided_by IS NULL OR decided_by <> created_by),
    CONSTRAINT federation_mapping_approval_fingerprint CHECK (status <> 'APPROVED' OR approved_fingerprint = fingerprint)
);

CREATE OR REPLACE FUNCTION federation_mapping_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'federation mapping versions are append-only' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.connection_id <> OLD.connection_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.version <> OLD.version
       OR NEW.config::text <> OLD.config::text OR NEW.fingerprint <> OLD.fingerprint
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a mapping version is immutable once written' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'PENDING_APPROVAL' AND NEW.status IS DISTINCT FROM OLD.status
       AND NOT (OLD.status = 'APPROVED' AND NEW.status = 'SUPERSEDED') THEN
        RAISE EXCEPTION 'a decided mapping version cannot change status' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'PENDING_APPROVAL' AND (NEW.decided_by IS DISTINCT FROM OLD.decided_by
       OR NEW.decided_at IS DISTINCT FROM OLD.decided_at
       OR NEW.approved_fingerprint IS DISTINCT FROM OLD.approved_fingerprint
       OR NEW.decision_reason IS DISTINCT FROM OLD.decision_reason) THEN
        RAISE EXCEPTION 'a mapping decision is recorded once' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER federation_mapping_guard BEFORE UPDATE OR DELETE ON federation_mapping_versions
    FOR EACH ROW EXECUTE FUNCTION federation_mapping_guard();

-- ---------------------------------------------------------------------------
-- 4. The provisioning event ledger (deduplicated by the sender's event identity)
-- ---------------------------------------------------------------------------

CREATE TABLE federation_provisioning_events (
    event_id       VARCHAR(255) PRIMARY KEY,
    tenant_id      VARCHAR(255) NOT NULL,
    connection_id  VARCHAR(255) NOT NULL REFERENCES federation_connections(connection_id),
    dedupe_key     VARCHAR(200) NOT NULL,
    source         VARCHAR(10)  NOT NULL,
    operation      VARCHAR(12)  NOT NULL,
    subject        VARCHAR(255) NOT NULL DEFAULT '',
    payload_hash   VARCHAR(64)  NOT NULL,
    status         VARCHAR(12)  NOT NULL DEFAULT 'RECEIVED',
    reason         VARCHAR(80)  NOT NULL DEFAULT '',
    result         JSONB        NOT NULL DEFAULT '{}'::jsonb,
    received_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    decided_at     TIMESTAMP WITH TIME ZONE,
    CONSTRAINT federation_prov_source_check CHECK (source IN ('SCIM', 'JIT', 'RECONCILE')),
    CONSTRAINT federation_prov_operation_check CHECK (operation IN ('CREATE', 'UPDATE', 'DEPROVISION', 'RECONCILE')),
    CONSTRAINT federation_prov_status_check CHECK (status IN ('RECEIVED', 'VALIDATED', 'APPLIED', 'REJECTED', 'RECONCILED')),
    UNIQUE (connection_id, dedupe_key)
);
CREATE INDEX idx_federation_prov_conn ON federation_provisioning_events (connection_id, received_at DESC);

-- A decided event keeps its decision: the sender's duplicate is answered from the record, never re-applied.
CREATE OR REPLACE FUNCTION federation_prov_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'federation_provisioning_events is append-only' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.event_id <> OLD.event_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.connection_id <> OLD.connection_id
       OR NEW.dedupe_key <> OLD.dedupe_key OR NEW.payload_hash <> OLD.payload_hash OR NEW.source <> OLD.source
       OR NEW.operation <> OLD.operation OR NEW.received_at <> OLD.received_at THEN
        RAISE EXCEPTION 'a provisioning event is immutable except for its outcome' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status IN ('APPLIED', 'REJECTED', 'RECONCILED') AND
       (NEW.status <> OLD.status OR NEW.reason <> OLD.reason OR NEW.result::text <> OLD.result::text) THEN
        RAISE EXCEPTION 'a decided provisioning event cannot change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER federation_prov_guard BEFORE UPDATE OR DELETE ON federation_provisioning_events
    FOR EACH ROW EXECUTE FUNCTION federation_prov_guard();

-- ---------------------------------------------------------------------------
-- 5. Assignments federation asked access-control-svc for: also the work queue
-- ---------------------------------------------------------------------------
--
-- A row is written in the same transaction as the provisioning decision that implies it, so the intent is
-- durable before anything is asked of access control. A worker (and the request that wrote it) then
-- drives it: PLANNED -> PROVISIONED | REFUSED, and REVOKE_PENDING -> REVOKED. Every remote call is
-- idempotent on request_id / correlation, so a retry after a crash asks the same question again.

CREATE TABLE federation_assignments (
    assignment_id     VARCHAR(255) PRIMARY KEY,
    tenant_id         VARCHAR(255) NOT NULL,
    connection_id     VARCHAR(255) NOT NULL REFERENCES federation_connections(connection_id),
    link_id           VARCHAR(255) NOT NULL REFERENCES federation_links(link_id),
    principal_id      VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    mapping_version   INT          NOT NULL,
    entry_key         VARCHAR(64)  NOT NULL,
    role_definition_id VARCHAR(255) NOT NULL,
    legal_entity_id   VARCHAR(255) NOT NULL,
    -- access-control-svc's request id, which is also the authorization-svc assignment id.
    request_id        VARCHAR(255),
    correlation_id    VARCHAR(255) NOT NULL,
    status            VARCHAR(16)  NOT NULL DEFAULT 'PLANNED',
    reason            VARCHAR(80)  NOT NULL DEFAULT '',
    attempts          INT          NOT NULL DEFAULT 0,
    next_attempt_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_error        TEXT         NOT NULL DEFAULT '',
    created_at        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT federation_assignments_status_check CHECK (status IN
        ('PLANNED', 'PROVISIONED', 'REFUSED', 'REVOKE_PENDING', 'REVOKED')),
    CONSTRAINT federation_assignments_attempts_check CHECK (attempts >= 0)
);
-- one live (requested or granted) assignment per link and role; a REVOKE_PENDING one is closing and independent
CREATE UNIQUE INDEX idx_federation_assignments_live ON federation_assignments (link_id, entry_key)
    WHERE status IN ('PLANNED', 'PROVISIONED');
CREATE INDEX idx_federation_assignments_due ON federation_assignments (next_attempt_at)
    WHERE status IN ('PLANNED', 'REVOKE_PENDING');
CREATE INDEX idx_federation_assignments_link ON federation_assignments (tenant_id, link_id);

-- ---------------------------------------------------------------------------
-- 6. Sync exceptions: what provisioning could not do on its own
-- ---------------------------------------------------------------------------

CREATE TABLE federation_sync_exceptions (
    exception_id   VARCHAR(255) PRIMARY KEY,
    tenant_id      VARCHAR(255) NOT NULL,
    connection_id  VARCHAR(255) NOT NULL REFERENCES federation_connections(connection_id),
    event_id       VARCHAR(255),
    link_id        VARCHAR(255),
    assignment_id  VARCHAR(255),
    kind           VARCHAR(40)  NOT NULL,
    detail         VARCHAR(500) NOT NULL DEFAULT '',
    status         VARCHAR(10)  NOT NULL DEFAULT 'OPEN',
    created_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    resolved_by    VARCHAR(255),
    resolved_at    TIMESTAMP WITH TIME ZONE,
    resolution     VARCHAR(500) NOT NULL DEFAULT '',
    CONSTRAINT federation_exceptions_status_check CHECK (status IN ('OPEN', 'RESOLVED'))
);
CREATE INDEX idx_federation_exceptions_conn ON federation_sync_exceptions (connection_id, status, created_at DESC);
-- an assignment raises each kind of exception once
CREATE UNIQUE INDEX idx_federation_exceptions_once ON federation_sync_exceptions (assignment_id, kind)
    WHERE assignment_id IS NOT NULL AND status = 'OPEN';
CREATE UNIQUE INDEX idx_federation_exceptions_link_once ON federation_sync_exceptions (link_id, kind)
    WHERE link_id IS NOT NULL AND assignment_id IS NULL AND status = 'OPEN';

-- ---------------------------------------------------------------------------
-- 7. Row level security
-- ---------------------------------------------------------------------------

ALTER TABLE federation_scim_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE federation_scim_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON federation_scim_tokens FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE federation_mapping_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE federation_mapping_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON federation_mapping_versions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE federation_provisioning_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE federation_provisioning_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON federation_provisioning_events FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE federation_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE federation_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON federation_assignments FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
-- The worker finds due work across tenants; it names this scope for that one query and nothing else.
CREATE POLICY federation_worker_scan ON federation_assignments FOR SELECT
    USING (current_setting('app.federation_worker', true) = 'on');
CREATE POLICY federation_worker_claim ON federation_assignments FOR UPDATE
    USING (current_setting('app.federation_worker', true) = 'on')
    WITH CHECK (current_setting('app.federation_worker', true) = 'on');

ALTER TABLE federation_sync_exceptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE federation_sync_exceptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON federation_sync_exceptions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Links are scanned across tenants for deprovisioning work (late deprovisions, sessions still to evict).
CREATE POLICY federation_worker_scan ON federation_links FOR SELECT
    USING (current_setting('app.federation_worker', true) = 'on');
