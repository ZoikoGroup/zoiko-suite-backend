-- ORG-01 wave 2: MergeDuplicateIdentity.
--
-- A merge keeps the survivor's id and retires the duplicate's. The duplicate's principals row is never
-- deleted or re-keyed, so its id can never be issued again (principal_id is the primary key) and every row that
-- ever pointed at it stays explainable. This table is the alias: duplicate_id resolves to survivor_id, with who
-- decided, why, and what moved. One duplicate is merged once; replaying the same merge is a no-op.

CREATE TABLE principal_merges (
    merge_id        VARCHAR(64)  PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    survivor_id     VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    duplicate_id    VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    reason          TEXT         NOT NULL,
    merged_by       VARCHAR(255) NOT NULL,
    correlation_id  VARCHAR(255) NOT NULL DEFAULT '',
    merged_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- what happened to each of the duplicate's provider links: [{link_id, issuer, action: MOVED|RETIRED}]
    link_outcomes   JSONB        NOT NULL DEFAULT '[]'::jsonb,
    CONSTRAINT principal_merges_distinct CHECK (survivor_id <> duplicate_id),
    CONSTRAINT principal_merges_reason CHECK (length(btrim(reason)) > 0),
    CONSTRAINT principal_merges_independent CHECK (merged_by <> survivor_id AND merged_by <> duplicate_id),
    CONSTRAINT principal_merges_once UNIQUE (tenant_id, duplicate_id)
);
CREATE INDEX idx_principal_merges_survivor ON principal_merges (tenant_id, survivor_id);

ALTER TABLE principal_merges ENABLE ROW LEVEL SECURITY;
ALTER TABLE principal_merges FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON principal_merges FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Append-only, and a merge never chains: a duplicate cannot be a survivor, nor a survivor a duplicate.
CREATE OR REPLACE FUNCTION principal_merges_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'identity merges are append-only' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM principal_merges WHERE tenant_id = NEW.tenant_id AND duplicate_id = NEW.survivor_id) THEN
        RAISE EXCEPTION 'the survivor was itself merged away' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM principal_merges WHERE tenant_id = NEW.tenant_id AND survivor_id = NEW.duplicate_id) THEN
        RAISE EXCEPTION 'the duplicate is the survivor of an earlier merge' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER principal_merges_guard BEFORE INSERT OR UPDATE OR DELETE ON principal_merges
    FOR EACH ROW EXECUTE FUNCTION principal_merges_guard();
