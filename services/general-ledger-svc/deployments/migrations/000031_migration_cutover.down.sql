DROP TRIGGER IF EXISTS migration_batch_events_append_only ON migration_batch_events;
DROP TRIGGER IF EXISTS migration_batch_lines_append_only ON migration_batch_lines;
DROP FUNCTION IF EXISTS migration_append_only();
DROP TABLE IF EXISTS migration_batch_events;
DROP TABLE IF EXISTS migration_batch_lines;
DROP TRIGGER IF EXISTS migration_batches_guard_trg ON migration_batches;
DROP FUNCTION IF EXISTS migration_batches_guard();
DROP TABLE IF EXISTS migration_batches;
