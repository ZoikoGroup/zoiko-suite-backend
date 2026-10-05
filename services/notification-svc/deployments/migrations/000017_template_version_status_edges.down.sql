-- 000017 down: removes the template version status-edge guard. Content immutability (000005) is unaffected.
DROP TRIGGER IF EXISTS trg_template_version_status_edges ON template_versions;
DROP FUNCTION IF EXISTS guard_template_version_status();
