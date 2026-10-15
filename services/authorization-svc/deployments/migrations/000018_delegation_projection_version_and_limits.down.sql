ALTER TABLE delegated_authorities
    DROP COLUMN IF EXISTS delegation_limit_quantity,
    DROP COLUMN IF EXISTS delegation_limit_currency,
    DROP COLUMN IF EXISTS delegation_limit_minor,
    DROP COLUMN IF EXISTS source_version;
