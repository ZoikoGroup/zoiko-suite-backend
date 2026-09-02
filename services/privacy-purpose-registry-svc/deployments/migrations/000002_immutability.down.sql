DROP TRIGGER IF EXISTS activity_versions_content_immutable ON processing_activity_versions;
DROP FUNCTION IF EXISTS reject_activity_content_mutation();

DROP TRIGGER IF EXISTS purpose_versions_immutable_once_published ON purpose_versions;
DROP FUNCTION IF EXISTS reject_published_purpose_mutation();
