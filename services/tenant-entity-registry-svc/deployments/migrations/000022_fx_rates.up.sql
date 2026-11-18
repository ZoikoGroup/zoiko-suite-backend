-- 000022_fx_rates.up.sql
--
-- REF-03 Foreign Exchange Rates (ZS-SVC-I-001 s4.13).
--
-- An exchange rate is a provenance-rich observation: who said it, for which pair, of which type, for which market
-- date, with the raw payload reference and hash. Rates are exact decimals, never floats. Rate type is mandatory and
-- types are never conflated. Observations are approved into rate sets; a closed set is certified and immutable, and a
-- correction is a superseding set, so a historical rate is never silently replaced and "what did we know when" has an
-- answer. Nothing is deleted. Currency identity belongs to currency-registry-svc, which is consulted read-only before
-- an observation is accepted.

CREATE TABLE fx_sources (
    source_id       UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL REFERENCES tenants(tenant_id),
    source_code     VARCHAR(40) NOT NULL,
    name            TEXT NOT NULL,
    source_class    VARCHAR(20) NOT NULL,
    -- The source's own timezone, so a market date and its cutoff read the same to everyone.
    timezone        VARCHAR(64) NOT NULL DEFAULT 'UTC',
    status          VARCHAR(10) NOT NULL DEFAULT 'ACTIVE',
    record_version  BIGINT NOT NULL DEFAULT 1,
    created_by      VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    withdrawn_by    VARCHAR(255),
    withdrawn_at    TIMESTAMP WITH TIME ZONE,
    withdraw_reason TEXT,
    correlation_id  VARCHAR(255),
    CONSTRAINT fxs_tenant_unique UNIQUE (tenant_id, source_id),
    CONSTRAINT fxs_code_unique UNIQUE (tenant_id, source_code),
    CONSTRAINT fxs_class_known CHECK (source_class IN ('PROVIDER', 'CENTRAL_BANK', 'INTERNAL_TREASURY', 'MANUAL_EXCEPTION')),
    CONSTRAINT fxs_status_known CHECK (status IN ('ACTIVE', 'WITHDRAWN')),
    CONSTRAINT fxs_present CHECK (btrim(source_code) <> '' AND btrim(name) <> ''),
    CONSTRAINT fxs_withdrawn_consistent CHECK ((status = 'WITHDRAWN') = (withdrawn_at IS NOT NULL AND withdrawn_by IS NOT NULL AND btrim(COALESCE(withdraw_reason, '')) <> ''))
);

CREATE TABLE fx_observations (
    observation_id          UUID PRIMARY KEY,
    tenant_id               UUID NOT NULL,
    source_id               UUID NOT NULL,
    base_currency           CHAR(3) NOT NULL,
    quote_currency          CHAR(3) NOT NULL,
    rate_type               VARCHAR(16) NOT NULL,
    -- Exact decimal: units of quote currency per one unit of base currency.
    rate                    NUMERIC(38, 18) NOT NULL,
    market_date             DATE NOT NULL,
    observed_at             TIMESTAMP WITH TIME ZONE NOT NULL,
    provider_observation_id TEXT,
    -- The raw provider payload, by reference and hash: kept so a rate can be proved against what was received.
    payload_ref             TEXT NOT NULL,
    payload_hash            VARCHAR(64),
    status                  VARCHAR(10) NOT NULL,
    manual                  BOOLEAN NOT NULL,
    outlier                 BOOLEAN NOT NULL,
    -- A manual or outlying rate is approved by someone other than whoever ingested it.
    needs_independent       BOOLEAN NOT NULL,
    -- The validation checks run at ingestion and their results, retained as evidence.
    checks                  JSONB NOT NULL DEFAULT '[]'::jsonb,
    reason                  TEXT,
    evidence_ref            TEXT,
    ingested_by             VARCHAR(255) NOT NULL,
    ingested_at             TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    superseded_by_id        UUID,
    superseded_at           TIMESTAMP WITH TIME ZONE,
    supersede_reason        TEXT,
    correlation_id          VARCHAR(255),
    CONSTRAINT fxo_source_in_tenant FOREIGN KEY (tenant_id, source_id) REFERENCES fx_sources (tenant_id, source_id),
    CONSTRAINT fxo_tenant_unique UNIQUE (tenant_id, observation_id),
    CONSTRAINT fxo_currency_shape CHECK (base_currency ~ '^[A-Z]{3}$' AND quote_currency ~ '^[A-Z]{3}$' AND base_currency <> quote_currency),
    CONSTRAINT fxo_type_known CHECK (rate_type IN ('SPOT', 'AVERAGE', 'CLOSING', 'TAX', 'CONTRACTUAL', 'OTHER_GOVERNED')),
    CONSTRAINT fxo_rate_positive CHECK (rate > 0),
    CONSTRAINT fxo_status_known CHECK (status IN ('OBSERVED', 'VALIDATED', 'APPROVED', 'SUPERSEDED')),
    CONSTRAINT fxo_hash_shape CHECK (payload_hash IS NULL OR payload_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT fxo_payload_present CHECK (btrim(payload_ref) <> ''),
    -- s4.13: a manual exceptional rate carries its reason and evidence.
    CONSTRAINT fxo_manual_evidenced CHECK (NOT manual OR (btrim(COALESCE(reason, '')) <> '' AND btrim(COALESCE(evidence_ref, '')) <> '')),
    CONSTRAINT fxo_independent_when_exceptional CHECK (needs_independent = (manual OR outlier)),
    CONSTRAINT fxo_superseded_consistent CHECK ((status = 'SUPERSEDED') = (superseded_at IS NOT NULL))
);
-- A provider's own observation id, or the same source, pair, type and instant, names one observation.
CREATE UNIQUE INDEX fxo_one_per_provider_id ON fx_observations (tenant_id, source_id, provider_observation_id) WHERE provider_observation_id IS NOT NULL;
CREATE UNIQUE INDEX fxo_one_per_instant ON fx_observations (tenant_id, source_id, base_currency, quote_currency, rate_type, observed_at);
CREATE INDEX idx_fxo_pair ON fx_observations (tenant_id, base_currency, quote_currency, rate_type, market_date);
CREATE INDEX idx_fxo_source ON fx_observations (tenant_id, source_id, market_date);

CREATE TABLE fx_rate_sets (
    set_id               UUID PRIMARY KEY,
    tenant_id            UUID NOT NULL REFERENCES tenants(tenant_id),
    set_name             VARCHAR(40) NOT NULL DEFAULT 'DEFAULT',
    rate_type            VARCHAR(16) NOT NULL,
    market_date          DATE NOT NULL,
    set_version          INTEGER NOT NULL DEFAULT 1,
    status               VARCHAR(10) NOT NULL DEFAULT 'APPROVED',
    supersedes_set_id    UUID,
    superseded_by_set_id UUID,
    override_reason      TEXT,
    evidence_ref         TEXT NOT NULL,
    approved_by          VARCHAR(255) NOT NULL,
    approved_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    closed_by            VARCHAR(255),
    closed_at            TIMESTAMP WITH TIME ZONE,
    superseded_at        TIMESTAMP WITH TIME ZONE,
    correlation_id       VARCHAR(255),
    CONSTRAINT fxr_tenant_unique UNIQUE (tenant_id, set_id),
    CONSTRAINT fxr_type_known CHECK (rate_type IN ('SPOT', 'AVERAGE', 'CLOSING', 'TAX', 'CONTRACTUAL', 'OTHER_GOVERNED')),
    CONSTRAINT fxr_status_known CHECK (status IN ('APPROVED', 'CLOSED', 'SUPERSEDED')),
    CONSTRAINT fxr_version_positive CHECK (set_version > 0),
    CONSTRAINT fxr_evidence CHECK (btrim(evidence_ref) <> '' AND btrim(set_name) <> ''),
    CONSTRAINT fxr_closed_consistent CHECK ((closed_at IS NULL) = (closed_by IS NULL)),
    CONSTRAINT fxr_superseded_consistent CHECK ((status = 'SUPERSEDED') = (superseded_at IS NOT NULL))
);
-- One live set of a name, type and date: a correction supersedes it rather than sitting beside it.
CREATE UNIQUE INDEX fxr_one_live ON fx_rate_sets (tenant_id, set_name, rate_type, market_date) WHERE status IN ('APPROVED', 'CLOSED');
CREATE INDEX idx_fxr_date ON fx_rate_sets (tenant_id, rate_type, market_date, set_name);

CREATE TABLE fx_rate_set_members (
    tenant_id      UUID NOT NULL,
    set_id         UUID NOT NULL,
    observation_id UUID NOT NULL,
    base_currency  CHAR(3) NOT NULL,
    quote_currency CHAR(3) NOT NULL,
    PRIMARY KEY (set_id, observation_id),
    CONSTRAINT fxm_set_in_tenant FOREIGN KEY (tenant_id, set_id) REFERENCES fx_rate_sets (tenant_id, set_id),
    CONSTRAINT fxm_observation_in_tenant FOREIGN KEY (tenant_id, observation_id) REFERENCES fx_observations (tenant_id, observation_id),
    -- One rate per pair in a set.
    CONSTRAINT fxm_one_per_pair UNIQUE (set_id, base_currency, quote_currency)
);
CREATE INDEX idx_fxm_observation ON fx_rate_set_members (tenant_id, observation_id);

-- A rate a member of a set must match the set, and an exceptional one is approved by someone other than whoever ingested it.
CREATE OR REPLACE FUNCTION fx_member_guard() RETURNS trigger AS $$
DECLARE
    o fx_observations%ROWTYPE;
    s fx_rate_sets%ROWTYPE;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'rate set membership is append-only' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT * INTO o FROM fx_observations WHERE tenant_id = NEW.tenant_id AND observation_id = NEW.observation_id;
    SELECT * INTO s FROM fx_rate_sets WHERE tenant_id = NEW.tenant_id AND set_id = NEW.set_id;
    IF s.status <> 'APPROVED' THEN
        RAISE EXCEPTION 'a % rate set takes no new members', s.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF o.rate_type <> s.rate_type OR o.market_date <> s.market_date
       OR o.base_currency <> NEW.base_currency OR o.quote_currency <> NEW.quote_currency THEN
        RAISE EXCEPTION 'a rate set holds only observations of its own rate type and market date' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF o.needs_independent AND o.ingested_by = s.approved_by THEN
        RAISE EXCEPTION 'a manual or outlying rate is approved by someone other than the principal who ingested it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER fx_member_guard BEFORE INSERT OR UPDATE OR DELETE ON fx_rate_set_members
    FOR EACH ROW EXECUTE FUNCTION fx_member_guard();

-- An observation's content never changes: only its status moves, and only forward.
CREATE OR REPLACE FUNCTION fx_observation_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'exchange-rate observations are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.source_id <> OLD.source_id OR NEW.base_currency <> OLD.base_currency
       OR NEW.quote_currency <> OLD.quote_currency OR NEW.rate_type <> OLD.rate_type OR NEW.rate <> OLD.rate
       OR NEW.market_date <> OLD.market_date OR NEW.observed_at <> OLD.observed_at
       OR NEW.provider_observation_id IS DISTINCT FROM OLD.provider_observation_id OR NEW.payload_ref <> OLD.payload_ref
       OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash OR NEW.manual <> OLD.manual OR NEW.outlier <> OLD.outlier
       OR NEW.checks <> OLD.checks OR NEW.ingested_by <> OLD.ingested_by OR NEW.ingested_at <> OLD.ingested_at THEN
        RAISE EXCEPTION 'a historical rate is immutable; supersede it with a new observation' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'SUPERSEDED' THEN
        RAISE EXCEPTION 'a superseded observation cannot change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER fx_observation_guard BEFORE UPDATE OR DELETE ON fx_observations
    FOR EACH ROW EXECUTE FUNCTION fx_observation_guard();

-- A set's content is fixed at approval. It then moves APPROVED to CLOSED (certified) or SUPERSEDED, and CLOSED to
-- SUPERSEDED by a correction; a certified set is never edited.
CREATE OR REPLACE FUNCTION fx_rate_set_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'rate sets are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.set_name <> OLD.set_name OR NEW.rate_type <> OLD.rate_type OR NEW.market_date <> OLD.market_date
       OR NEW.set_version <> OLD.set_version OR NEW.supersedes_set_id IS DISTINCT FROM OLD.supersedes_set_id
       OR NEW.override_reason IS DISTINCT FROM OLD.override_reason OR NEW.evidence_ref <> OLD.evidence_ref
       OR NEW.approved_by <> OLD.approved_by OR NEW.approved_at <> OLD.approved_at THEN
        RAISE EXCEPTION 'a rate set is immutable once approved; correct it with a superseding set' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'SUPERSEDED' THEN
        RAISE EXCEPTION 'a superseded rate set cannot change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'CLOSED' AND (NEW.status <> 'SUPERSEDED' OR NEW.closed_by IS DISTINCT FROM OLD.closed_by OR NEW.closed_at IS DISTINCT FROM OLD.closed_at) THEN
        RAISE EXCEPTION 'a certified rate set is immutable; correct it with a superseding set' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER fx_rate_set_guard BEFORE UPDATE OR DELETE ON fx_rate_sets
    FOR EACH ROW EXECUTE FUNCTION fx_rate_set_guard();

-- A source's identity does not change; it is withdrawn, never edited or deleted.
CREATE OR REPLACE FUNCTION fx_source_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'exchange-rate sources are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.source_code <> OLD.source_code OR NEW.name <> OLD.name
       OR NEW.source_class <> OLD.source_class OR NEW.timezone <> OLD.timezone OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'an exchange-rate source is immutable; withdraw it and add another' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'WITHDRAWN' THEN
        RAISE EXCEPTION 'a withdrawn source cannot change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER fx_source_guard BEFORE UPDATE OR DELETE ON fx_sources
    FOR EACH ROW EXECUTE FUNCTION fx_source_guard();

ALTER TABLE fx_sources ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_sources FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON fx_sources FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE fx_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_observations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON fx_observations FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE fx_rate_sets ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_rate_sets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON fx_rate_sets FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE fx_rate_set_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_rate_set_members FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON fx_rate_set_members FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
