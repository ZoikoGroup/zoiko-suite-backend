DROP TRIGGER IF EXISTS trg_reject_record_relationship_mutation ON record_relationships;
DROP FUNCTION IF EXISTS reject_record_relationship_mutation();
DROP TABLE IF EXISTS record_relationships CASCADE;

DROP TRIGGER IF EXISTS trg_reject_record_mutation ON records;
DROP FUNCTION IF EXISTS reject_record_mutation();
DROP TABLE IF EXISTS records CASCADE;
