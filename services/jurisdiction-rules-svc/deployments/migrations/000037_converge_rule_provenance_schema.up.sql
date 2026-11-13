-- 000037_converge_rule_provenance_schema.up.sql
-- Brings a database migrated on the pre-registry lineage to the schema every
-- fresh database has. A no-op on a database built from this directory.
--
-- WHY. Databases created before 2 Nov 2026 ran 000008_add_precedence_metadata,
-- which adds jurisdiction_rules.supersedes_rule_id. 000005_jurisdiction_pack_
-- registries adds the same column without IF NOT EXISTS, so on those databases
-- it failed; it was then applied by hand and recorded without its ALTER of
-- jurisdiction_rules. The local stack showed the result on 13 Nov 2026 (D2-1
-- step 15): provenance and parameter columns, the provenance guard trigger and
-- the foreign keys to jurisdictions and jurisdiction_rules were missing, so a
-- rule's provenance and every pack compile answered 503
-- (column "regime_id" does not exist).
--
-- Every statement checks before it adds. Foreign keys are added NOT VALID and
-- then validated, so a legacy row without its parent fails this migration by
-- name instead of being kept silently.

ALTER TABLE jurisdiction_rules
    ADD COLUMN IF NOT EXISTS regime_id          UUID,
    ADD COLUMN IF NOT EXISTS interpretation_id  UUID,
    ADD COLUMN IF NOT EXISTS supersedes_rule_id UUID,
    ADD COLUMN IF NOT EXISTS precedence         INTEGER,
    ADD COLUMN IF NOT EXISTS published_on       DATE,
    ADD COLUMN IF NOT EXISTS rule_parameters    JSONB;

DO $$
DECLARE
    fk RECORD;
BEGIN
    FOR fk IN SELECT * FROM (VALUES
        ('jurisdiction_rules',         'jurisdiction_rules_regime_id_fkey',              'regime_id',            'regulatory_regimes(regime_id)'),
        ('jurisdiction_rules',         'jurisdiction_rules_interpretation_id_fkey',      'interpretation_id',    'interpretation_records(interpretation_id)'),
        ('interpretation_records',     'interpretation_records_jurisdiction_id_fkey',    'jurisdiction_id',      'jurisdictions(jurisdiction_id)'),
        ('obligation_rules',           'obligation_rules_jurisdiction_id_fkey',          'jurisdiction_id',      'jurisdictions(jurisdiction_id)'),
        ('pack_version_jurisdictions', 'pack_version_jurisdictions_jurisdiction_id_fkey', 'jurisdiction_id',     'jurisdictions(jurisdiction_id)'),
        ('payment_terms',              'payment_terms_jurisdiction_id_fkey',             'jurisdiction_id',      'jurisdictions(jurisdiction_id)'),
        ('regulatory_calendars',       'regulatory_calendars_jurisdiction_id_fkey',      'jurisdiction_id',      'jurisdictions(jurisdiction_id)'),
        ('regulatory_sources',         'regulatory_sources_jurisdiction_id_fkey',        'jurisdiction_id',      'jurisdictions(jurisdiction_id)'),
        ('rule_sources',               'rule_sources_jurisdiction_rule_id_fkey',         'jurisdiction_rule_id', 'jurisdiction_rules(jurisdiction_rule_id)'),
        ('source_change_notices',      'source_change_notices_jurisdiction_id_fkey',     'jurisdiction_id',      'jurisdictions(jurisdiction_id)')
    ) AS t(tbl, name, col, target)
    LOOP
        -- Any foreign key on the same column counts: an older lineage may have named it differently.
        IF NOT EXISTS (
            SELECT 1 FROM pg_constraint c
            JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
            WHERE c.contype = 'f' AND c.conrelid = fk.tbl::regclass AND a.attname = fk.col
              AND array_length(c.conkey, 1) = 1
        ) THEN
            EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I FOREIGN KEY (%I) REFERENCES %s NOT VALID',
                           fk.tbl, fk.name, fk.col, fk.target);
            EXECUTE format('ALTER TABLE %I VALIDATE CONSTRAINT %I', fk.tbl, fk.name);
        END IF;
    END LOOP;

    -- jur_rule_provenance_guard (000005, replaced by 000011) keeps provenance and
    -- parameters immutable once a rule leaves DRAFT.
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_jurisdiction_rules_provenance_guard'
                   AND tgrelid = 'jurisdiction_rules'::regclass) THEN
        CREATE TRIGGER trg_jurisdiction_rules_provenance_guard BEFORE UPDATE ON jurisdiction_rules
            FOR EACH ROW EXECUTE FUNCTION jur_rule_provenance_guard();
    END IF;
END $$;
