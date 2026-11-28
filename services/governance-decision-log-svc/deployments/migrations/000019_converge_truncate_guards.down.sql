-- Migration: 000019_converge_truncate_guards.down.sql
--
-- Nothing to undo. The up file only makes an old-lineage database match what
-- the current 000011 creates; restoring the old lineage's extra triggers would
-- recreate the drift it removes. The guards themselves belong to 000011, whose
-- down file removes them.
SELECT 1;
