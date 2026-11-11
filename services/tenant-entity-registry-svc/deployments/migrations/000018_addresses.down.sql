-- 000018_addresses.down.sql
-- Addresses are master data and the legal record of where a party was. Refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM addresses) THEN
        RAISE EXCEPTION '000018 down refused: addresses exist and are referenced master data';
    END IF;
END
$$;

DROP TABLE IF EXISTS address_party_links;
DROP TABLE IF EXISTS address_verifications;
DROP TABLE IF EXISTS address_versions;
DROP TABLE IF EXISTS addresses;
DROP FUNCTION IF EXISTS address_link_guard();
DROP FUNCTION IF EXISTS address_verification_guard();
DROP FUNCTION IF EXISTS address_version_guard();
