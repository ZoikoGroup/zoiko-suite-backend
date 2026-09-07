-- Reverses 000003_tax03_determination_inputs.up.sql.
--
-- Dropping these columns discards the parties, place of supply, classification
-- and exemption basis of every determination made while they existed — the
-- facts a tax position is defended with at audit. Safe only where the tax ledger
-- is disposable.

BEGIN;

DROP INDEX IF EXISTS idx_tax_determinations_buyer;
DROP INDEX IF EXISTS idx_tax_determinations_supply;

ALTER TABLE tax_determinations
    DROP COLUMN IF EXISTS seller_registration_status,
    DROP COLUMN IF EXISTS seller_registration_id,
    DROP COLUMN IF EXISTS exemption_certificate_ref,
    DROP COLUMN IF EXISTS exemption_reason,
    DROP COLUMN IF EXISTS buyer_tax_registration_id,
    DROP COLUMN IF EXISTS supply_type,
    DROP COLUMN IF EXISTS supply_kind,
    DROP COLUMN IF EXISTS product_classification,
    DROP COLUMN IF EXISTS place_of_supply_basis,
    DROP COLUMN IF EXISTS supply_date,
    DROP COLUMN IF EXISTS supply_jurisdiction_id,
    DROP COLUMN IF EXISTS ship_to_jurisdiction_id,
    DROP COLUMN IF EXISTS ship_from_jurisdiction_id,
    DROP COLUMN IF EXISTS buyer_establishment_id,
    DROP COLUMN IF EXISTS seller_establishment_id,
    DROP COLUMN IF EXISTS buyer_party_id,
    DROP COLUMN IF EXISTS seller_party_id;

COMMIT;
