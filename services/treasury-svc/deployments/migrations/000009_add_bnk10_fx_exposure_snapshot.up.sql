-- BNK-10 Wave 15: the spec'd FXExposureSnapshot entity — a real,
-- persisted Calculated -> Published -> Superseded lifecycle, mirroring
-- Wave 14's cash_position_snapshots exactly (same append-only/
-- superseded-by shape). Before this migration, GetFXExposure was (and
-- remains) a pure live-composition read — see bnk10.go's own doc comment
-- — that never persisted anything.
--
-- horizon is deliberately NOT a column here: buildExposure's maturity
-- bucketing (0-30D/31-90D/91-180D/181-365D/365D+) is a fixed window, not
-- a caller-selectable horizon anywhere in this codebase today — adding a
-- column for a selection mechanism that doesn't exist would be
-- fabricating a feature, not persisting one.
--
-- netting_scope is real but deliberately narrow: this service has no
-- multi-entity netting algorithm anywhere — buildExposure composes
-- exactly one legal entity per call. netting_scope records that single-
-- entity scope explicitly (auditable, per the doc's own "no silent
-- netting across entities/currencies without approved basis" rule) —
-- it does not imply, and this migration does not add, any actual
-- cross-entity netting capability.
CREATE TABLE fx_exposure_snapshots (
    snapshot_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         UUID NOT NULL,
    exposure_currency       VARCHAR(3) NOT NULL,
    functional_currency     VARCHAR(3) NOT NULL,
    as_of_timestamp         TIMESTAMPTZ NOT NULL,

    rate_used               NUMERIC(20, 8) NOT NULL,
    rate_as_of              TIMESTAMPTZ NOT NULL,
    rate_version            VARCHAR(64) NOT NULL DEFAULT '',
    netting_scope           TEXT NOT NULL,

    buckets                 JSONB NOT NULL DEFAULT '[]',
    gross_exposure_amount   NUMERIC(20, 4) NOT NULL,
    net_exposure_amount     NUMERIC(20, 4) NOT NULL,

    status                  VARCHAR(32) NOT NULL DEFAULT 'CALCULATED',
    has_stale_component     BOOLEAN NOT NULL DEFAULT FALSE,
    published_by_principal_id VARCHAR(128) NOT NULL DEFAULT '',
    published_at            TIMESTAMPTZ NULL,
    superseded_by           UUID REFERENCES fx_exposure_snapshots (snapshot_id),

    calculated_by_principal_id VARCHAR(128) NOT NULL,
    correlation_id           VARCHAR(255) NOT NULL DEFAULT '',
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_fx_exposure_snapshots_entity_pair
    ON fx_exposure_snapshots (tenant_id, legal_entity_id, exposure_currency, functional_currency, created_at DESC);

ALTER TABLE fx_exposure_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_exposure_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON fx_exposure_snapshots
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Same shape as migration 000008's reject_cash_position_mutation: SUPERSEDED
-- is fully terminal; every calculated figure is immutable from the moment
-- the row is written; only status/published_*/superseded_by ever change
-- afterward, and superseded_by may only be set once.
CREATE OR REPLACE FUNCTION reject_fx_exposure_snapshot_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'fx_exposure_snapshots rows are never deleted';
    END IF;
    IF OLD.status = 'SUPERSEDED' THEN
        RAISE EXCEPTION 'fx exposure snapshot % is SUPERSEDED and can no longer be modified', OLD.snapshot_id;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.exposure_currency IS DISTINCT FROM OLD.exposure_currency
        OR NEW.functional_currency IS DISTINCT FROM OLD.functional_currency
        OR NEW.as_of_timestamp IS DISTINCT FROM OLD.as_of_timestamp
        OR NEW.rate_used IS DISTINCT FROM OLD.rate_used
        OR NEW.rate_as_of IS DISTINCT FROM OLD.rate_as_of
        OR NEW.rate_version IS DISTINCT FROM OLD.rate_version
        OR NEW.netting_scope IS DISTINCT FROM OLD.netting_scope
        OR NEW.buckets IS DISTINCT FROM OLD.buckets
        OR NEW.gross_exposure_amount IS DISTINCT FROM OLD.gross_exposure_amount
        OR NEW.net_exposure_amount IS DISTINCT FROM OLD.net_exposure_amount
        OR NEW.has_stale_component IS DISTINCT FROM OLD.has_stale_component
        OR NEW.calculated_by_principal_id IS DISTINCT FROM OLD.calculated_by_principal_id
        OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'fx exposure snapshot % calculated figures are immutable once written', OLD.snapshot_id;
    END IF;
    IF OLD.superseded_by IS NOT NULL AND NEW.superseded_by IS DISTINCT FROM OLD.superseded_by THEN
        RAISE EXCEPTION 'fx exposure snapshot % is already superseded', OLD.snapshot_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_fx_exposure_snapshot_mutation
    BEFORE UPDATE OR DELETE ON fx_exposure_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_fx_exposure_snapshot_mutation();
