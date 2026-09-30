-- Schema for semantic-model-svc (DATA-05, ZS-SVC-N-001 §4)
-- Migration: 000001_initial_schema.up.sql

-- Semantic Models: the named, stable identity of a model.
CREATE TABLE IF NOT EXISTS semantic_models (
    model_id             TEXT         PRIMARY KEY
        CHECK (model_id ~ '^dsm_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id              VARCHAR(64)  NOT NULL,
    name                     VARCHAR(128) NOT NULL,
    created_at                 TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                   VARCHAR(128) NOT NULL,
    UNIQUE (tenant_id, name)
);

-- Semantic Versions: one versioned build. Published versions are
-- immutable — a dimension hierarchy or metric definition change always
-- creates a NEW version, never rewrites the one already live.
CREATE TABLE IF NOT EXISTS semantic_versions (
    version_id                TEXT         PRIMARY KEY
        CHECK (version_id ~ '^dsv_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                   VARCHAR(64)  NOT NULL,
    model_id                      TEXT         NOT NULL REFERENCES semantic_models(model_id),
    version_number                   INT          NOT NULL CHECK (version_number >= 1),
    status                              VARCHAR(32)  NOT NULL DEFAULT 'Draft'
        CHECK (status IN ('Draft', 'Validating', 'Published', 'Deprecated')),
    created_at                            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                              VARCHAR(128) NOT NULL,
    published_at                              TIMESTAMPTZ,
    published_by                                VARCHAR(128),
    UNIQUE (tenant_id, model_id, version_number),
    CHECK ((published_at IS NULL) = (published_by IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_semantic_versions_model ON semantic_versions(tenant_id, model_id);
CREATE INDEX IF NOT EXISTS idx_semantic_versions_status ON semantic_versions(tenant_id, status);

-- Metric Bindings: binds a metric_key to a DATA-04 dataset version's
-- column, with explicit unit/currency/sign/aggregation. source_column is
-- validated at the application layer against a strict allowlist (never
-- raw SQL) before it ever reaches this table.
CREATE TABLE IF NOT EXISTS metric_bindings (
    metric_binding_id        TEXT         PRIMARY KEY
        CHECK (metric_binding_id ~ '^dmb_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                   VARCHAR(64)  NOT NULL,
    version_id                    TEXT         NOT NULL REFERENCES semantic_versions(version_id),
    metric_key                      VARCHAR(128) NOT NULL,
    dataset_version_id                 VARCHAR(128) NOT NULL,
    source_column                        VARCHAR(256) NOT NULL,
    aggregation                            VARCHAR(16)  NOT NULL CHECK (aggregation IN ('SUM', 'AVG', 'COUNT', 'MIN', 'MAX')),
    unit                                     VARCHAR(32)  NOT NULL,
    currency                                   CHAR(3),
    sign                                         VARCHAR(16)  NOT NULL CHECK (sign IN ('Positive', 'Negative', 'Signed')),
    retired                                        BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at                                       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                                         VARCHAR(128) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_metric_bindings_version ON metric_bindings(tenant_id, version_id);
CREATE INDEX IF NOT EXISTS idx_metric_bindings_key ON metric_bindings(tenant_id, version_id, metric_key) WHERE NOT retired;

-- Dimension Bindings: binds a dimension_key to a dataset version's
-- column, with a hierarchy position. Immutable once written — a
-- hierarchy change is always a new version's binding, never an edit of
-- this row (doc's own named acceptance test).
CREATE TABLE IF NOT EXISTS dimension_bindings (
    dimension_binding_id     TEXT         PRIMARY KEY
        CHECK (dimension_binding_id ~ '^ddb_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                   VARCHAR(64)  NOT NULL,
    version_id                    TEXT         NOT NULL REFERENCES semantic_versions(version_id),
    dimension_key                    VARCHAR(128) NOT NULL,
    dataset_version_id                  VARCHAR(128) NOT NULL,
    source_column                         VARCHAR(256) NOT NULL,
    hierarchy_level                         INT          NOT NULL DEFAULT 0 CHECK (hierarchy_level >= 0),
    parent_dimension_key                      VARCHAR(128),
    created_at                                  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                                    VARCHAR(128) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_dimension_bindings_version ON dimension_bindings(tenant_id, version_id);

-- Calculation Plans: how a derived metric combines base metrics — a
-- structured expression, never raw SQL. Immutable once written.
CREATE TABLE IF NOT EXISTS calculation_plans (
    calc_plan_id             TEXT         PRIMARY KEY
        CHECK (calc_plan_id ~ '^dcp_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                   VARCHAR(64)  NOT NULL,
    version_id                    TEXT         NOT NULL REFERENCES semantic_versions(version_id),
    metric_key                      VARCHAR(128) NOT NULL,
    expression                        JSONB        NOT NULL,
    created_at                          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                            VARCHAR(128) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_calculation_plans_version ON calculation_plans(tenant_id, version_id);

-- Idempotency Keys: scoped per tenant from the start.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           VARCHAR(64)  NOT NULL,
    owner_scope           VARCHAR(32)  NOT NULL,
    principal_id             VARCHAR(128) NOT NULL,
    idempotency_key            VARCHAR(128) NOT NULL,
    operation                    VARCHAR(64)  NOT NULL,
    request_sha256                 VARCHAR(64)  NOT NULL,
    resource_id                      VARCHAR(128) NOT NULL,
    created_at                         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, owner_scope, principal_id, idempotency_key)
);

-- Outbox Events: transactional outbox for event emission.
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type        VARCHAR(64)  NOT NULL,
    aggregate_id            VARCHAR(128) NOT NULL,
    event_type                VARCHAR(64)  NOT NULL,
    payload                      JSONB        NOT NULL,
    tenant_id                      VARCHAR(64),
    created_at                       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at                       TIMESTAMPTZ,
    publish_attempts                     INT          NOT NULL DEFAULT 0,
    last_error                             TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished ON outbox_events(published_at) WHERE published_at IS NULL;

-- ── Row Level Security ──────────────────────────────────────────────────────

ALTER TABLE semantic_models ENABLE ROW LEVEL SECURITY;
ALTER TABLE semantic_models FORCE ROW LEVEL SECURITY;
CREATE POLICY semantic_models_tenant_isolation ON semantic_models
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE semantic_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE semantic_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY semantic_versions_read ON semantic_versions FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY semantic_versions_insert ON semantic_versions FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY semantic_versions_update ON semantic_versions FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY semantic_versions_delete ON semantic_versions FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE metric_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE metric_bindings FORCE ROW LEVEL SECURITY;
CREATE POLICY metric_bindings_read ON metric_bindings FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY metric_bindings_insert ON metric_bindings FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY metric_bindings_update ON metric_bindings FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY metric_bindings_delete ON metric_bindings FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE dimension_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE dimension_bindings FORCE ROW LEVEL SECURITY;
CREATE POLICY dimension_bindings_read ON dimension_bindings FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dimension_bindings_insert ON dimension_bindings FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dimension_bindings_update ON dimension_bindings FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dimension_bindings_delete ON dimension_bindings FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE calculation_plans ENABLE ROW LEVEL SECURITY;
ALTER TABLE calculation_plans FORCE ROW LEVEL SECURITY;
CREATE POLICY calculation_plans_read ON calculation_plans FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY calculation_plans_insert ON calculation_plans FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY calculation_plans_update ON calculation_plans FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY calculation_plans_delete ON calculation_plans FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- ── Immutability ─────────────────────────────────────────────────────────────

CREATE OR REPLACE FUNCTION enforce_semantic_model_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'semantic model is immutable once created';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_semantic_model_immutability
    BEFORE UPDATE OR DELETE ON semantic_models
    FOR EACH ROW EXECUTE FUNCTION enforce_semantic_model_immutability();

CREATE OR REPLACE FUNCTION enforce_dimension_binding_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'dimension binding is immutable once written';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_dimension_binding_immutability
    BEFORE UPDATE OR DELETE ON dimension_bindings
    FOR EACH ROW EXECUTE FUNCTION enforce_dimension_binding_immutability();

CREATE OR REPLACE FUNCTION enforce_calculation_plan_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'calculation plan is immutable once written';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_calculation_plan_immutability
    BEFORE UPDATE OR DELETE ON calculation_plans
    FOR EACH ROW EXECUTE FUNCTION enforce_calculation_plan_immutability();

-- Metric Bindings: the ONLY allowed mutation is retired false -> true
-- (RetireMetricBinding), and only while the owning version is still
-- Draft/Validating (checked at the application layer, since the version
-- itself becomes immutable at Publish, which transitively blocks further
-- retires anyway once the parent is Published — see the version trigger).
CREATE OR REPLACE FUNCTION enforce_metric_binding_retire_only()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'metric binding cannot be deleted';
    END IF;
    IF OLD.retired THEN
        RAISE EXCEPTION 'metric binding % is already retired and immutable', OLD.metric_binding_id;
    END IF;
    IF NOT NEW.retired THEN
        RAISE EXCEPTION 'metric binding % may only change retired from false to true', OLD.metric_binding_id;
    END IF;
    IF NEW.metric_key <> OLD.metric_key OR NEW.dataset_version_id <> OLD.dataset_version_id
        OR NEW.source_column <> OLD.source_column OR NEW.aggregation <> OLD.aggregation
        OR NEW.unit <> OLD.unit OR NEW.sign <> OLD.sign THEN
        RAISE EXCEPTION 'metric binding % retirement may only change retired', OLD.metric_binding_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_metric_binding_retire_only
    BEFORE UPDATE OR DELETE ON metric_bindings
    FOR EACH ROW EXECUTE FUNCTION enforce_metric_binding_retire_only();

-- Semantic Versions: Deprecated is fully terminal. Published allows
-- exactly one further transition (to Deprecated) and nothing else — same
-- pattern as analytical-data-platform-svc's dataset_versions (a lesson
-- learned earlier this session: a naive "Published = fully terminal"
-- trigger blocks the deprecate path that's supposed to remain reachable).
CREATE OR REPLACE FUNCTION enforce_semantic_version_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'semantic version cannot be deleted';
    END IF;
    IF OLD.status = 'Deprecated' THEN
        RAISE EXCEPTION 'semantic version % is Deprecated and immutable', OLD.version_id;
    END IF;
    IF OLD.status = 'Published' THEN
        IF NEW.status <> 'Deprecated' THEN
            RAISE EXCEPTION 'semantic version % is Published; only Deprecated remains reachable', OLD.version_id;
        END IF;
        IF NEW.model_id <> OLD.model_id OR NEW.version_number <> OLD.version_number
            OR NEW.published_at <> OLD.published_at OR NEW.published_by <> OLD.published_by THEN
            RAISE EXCEPTION 'semantic version % is Published; only its status may change (to Deprecated)', OLD.version_id;
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'Draft' THEN
                IF NEW.status NOT IN ('Validating', 'Published') THEN
                    RAISE EXCEPTION 'invalid semantic version transition from Draft to %', NEW.status;
                END IF;
            WHEN 'Validating' THEN
                IF NEW.status <> 'Published' THEN
                    RAISE EXCEPTION 'invalid semantic version transition from Validating to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown semantic version status %', OLD.status;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_semantic_version_lifecycle
    BEFORE UPDATE OR DELETE ON semantic_versions
    FOR EACH ROW EXECUTE FUNCTION enforce_semantic_version_lifecycle();
