-- Reverts 000039.
DROP TRIGGER IF EXISTS workload_bindings_bump_version ON workload_bindings;
ALTER TABLE workload_bindings DROP COLUMN IF EXISTS version;
