-- Dropping these discards imported-batch evidence (content, items, history). Do it only
-- to undo a deployment that never imported a real file.
DROP TABLE IF EXISTS import_batch_log;
DROP TABLE IF EXISTS import_items;
DROP TABLE IF EXISTS import_chunks;
DROP TABLE IF EXISTS import_batches;
DROP TABLE IF EXISTS import_profiles;
DROP FUNCTION IF EXISTS import_log_immutable();
DROP FUNCTION IF EXISTS import_item_guard();
DROP FUNCTION IF EXISTS import_chunk_guard();
DROP FUNCTION IF EXISTS import_batch_guard();
DROP FUNCTION IF EXISTS import_profile_guard();
