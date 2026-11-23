-- 000016_parties.down.sql
-- Parties are master data other services reference. The down migration is refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM parties) THEN
        RAISE EXCEPTION '000016 down refused: parties exist and are referenced master data';
    END IF;
END
$$;

DROP TABLE IF EXISTS party_create_requests;
DROP TABLE IF EXISTS resolution_cases;
DROP TABLE IF EXISTS party_roles;
DROP TABLE IF EXISTS party_identifiers;
DROP TABLE IF EXISTS party_facts;
DROP TABLE IF EXISTS parties;
DROP FUNCTION IF EXISTS party_legal_entity_tenant_guard();
DROP FUNCTION IF EXISTS party_request_guard();
DROP FUNCTION IF EXISTS party_fact_guard();
