-- 000019_establishments.down.sql
-- Establishments and the registered-office history are legal-presence records. Refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM establishments) OR EXISTS (SELECT 1 FROM entity_address_links) THEN
        RAISE EXCEPTION '000019 down refused: establishments or entity address links exist';
    END IF;
END
$$;

DROP TABLE IF EXISTS entity_address_links;
DROP TABLE IF EXISTS establishment_transitions;
DROP TABLE IF EXISTS establishments;
DROP FUNCTION IF EXISTS entity_address_link_guard();
DROP FUNCTION IF EXISTS establishment_transition_guard();
DROP FUNCTION IF EXISTS establishment_guard();
DROP FUNCTION IF EXISTS establishment_entity_tenant_guard();
