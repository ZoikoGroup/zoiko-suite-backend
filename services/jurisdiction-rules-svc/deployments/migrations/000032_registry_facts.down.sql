-- 000032_registry_facts.down.sql
-- The projection can be rebuilt from the topic, so dropping it loses no source data.
DROP TABLE IF EXISTS registry_address_countries;
DROP TABLE IF EXISTS registry_fact_events;
