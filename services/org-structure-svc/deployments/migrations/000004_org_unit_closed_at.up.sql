-- 000004_org_unit_closed_at.up.sql
--
-- ORG-05 hierarchy reads "as known at" a recorded time. Closing a placement or
-- a manager reference sets valid_to on the existing row, which on its own loses
-- WHEN the row was closed; closed_at keeps that, so a read as of an earlier
-- recorded time can still show the row open-ended (what was known then).

ALTER TABLE org_unit_hierarchy_versions ADD COLUMN closed_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE org_unit_manager_refs       ADD COLUMN closed_at TIMESTAMP WITH TIME ZONE;

-- Rows closed before this migration: a close always came with the successor
-- row inserted in the same transaction (valid_from = the closed row's valid_to),
-- so the successor's recorded_at is when the close was recorded.
UPDATE org_unit_hierarchy_versions h
   SET closed_at = s.recorded_at
  FROM org_unit_hierarchy_versions s
 WHERE h.valid_to IS NOT NULL AND h.closed_at IS NULL
   AND s.unit_id = h.unit_id AND s.valid_from = h.valid_to;

UPDATE org_unit_manager_refs m
   SET closed_at = s.recorded_at
  FROM org_unit_manager_refs s
 WHERE m.valid_to IS NOT NULL AND m.closed_at IS NULL
   AND s.unit_id = m.unit_id AND s.valid_from = m.valid_to;
