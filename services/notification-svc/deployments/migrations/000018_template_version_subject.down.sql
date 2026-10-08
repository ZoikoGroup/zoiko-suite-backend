-- 000018 down: removes the template version subject and restores the 000005 immutability function.
ALTER TABLE template_versions
    DROP CONSTRAINT IF EXISTS ck_template_subject_variables_need_subject,
    DROP CONSTRAINT IF EXISTS ck_template_subject_variables_declared,
    DROP CONSTRAINT IF EXISTS ck_template_subject_shape;

CREATE OR REPLACE FUNCTION reject_template_version_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'template_versions rows are never deleted';
    END IF;
    IF NEW.template_id IS DISTINCT FROM OLD.template_id
        OR NEW.version_number IS DISTINCT FROM OLD.version_number
        OR NEW.locale IS DISTINCT FROM OLD.locale
        OR NEW.content IS DISTINCT FROM OLD.content
        OR NEW.content_hash IS DISTINCT FROM OLD.content_hash
        OR NEW.variable_schema IS DISTINCT FROM OLD.variable_schema
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
    THEN
        RAISE EXCEPTION 'template_versions content is immutable once written (version %)', OLD.version_id;
    END IF;
    IF OLD.status IN ('RETIRED', 'SUPERSEDED') THEN
        RAISE EXCEPTION 'version % is in a terminal state (%) and cannot be changed further', OLD.version_id, OLD.status;
    END IF;
    IF OLD.approved_at IS NOT NULL AND (NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approved_by_principal_id IS DISTINCT FROM OLD.approved_by_principal_id) THEN
        RAISE EXCEPTION 'version % is already approved; approval cannot change', OLD.version_id;
    END IF;
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'version % is already published; publish date cannot change', OLD.version_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE template_versions
    DROP COLUMN IF EXISTS subject_variables,
    DROP COLUMN IF EXISTS subject;
