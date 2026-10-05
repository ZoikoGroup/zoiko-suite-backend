-- 000018_template_version_subject.up.sql
-- ZS-SVC-Y-001 NCD-01 (sections 4.1, 4.4), INV-17, NP-32, Wave 2 slice 2
-- (audit finding F-05).
--
-- A governed template rendered only a BODY. The subject came from the caller as free
-- text, so wording that had been drafted, reviewed, approved and frozen could be
-- sent under a subject nobody reviewed, and a sensitive fact (a salary, a
-- disciplinary reason) could reach a subject line, one of the surfaces the
-- standard says must stay free of them.
--
-- A version now carries a reviewed, frozen SUBJECT, written with the body:
--
--   subject            the subject text, with {{.variable}} placeholders only
--   subject_variables  the variables an author has declared safe to appear in a
--                      subject. A reviewer sees this list. It must be a subset of
--                      the version's variable_schema, and the subject may reference
--                      nothing outside it.
--
-- Both are nullable/empty by default: a version with no subject keeps the old
-- behaviour (the caller supplies the subject), so nothing existing changes. A
-- version WITH a subject owns it: the API then refuses a caller-supplied one.
--
-- Like the body, they are frozen from the moment the version is written, by
-- extending the immutability trigger from 000005.

ALTER TABLE template_versions
    ADD COLUMN IF NOT EXISTS subject TEXT,
    ADD COLUMN IF NOT EXISTS subject_variables JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE template_versions
    DROP CONSTRAINT IF EXISTS ck_template_subject_shape,
    ADD CONSTRAINT ck_template_subject_shape CHECK (
        subject IS NULL OR (length(btrim(subject)) BETWEEN 1 AND 200 AND subject !~ E'[\\r\\n]')),
    DROP CONSTRAINT IF EXISTS ck_template_subject_variables_declared,
    -- An empty list is always valid. variable_schema may be JSON null (a version
    -- created with no variables), so containment is only asked of a non-empty list.
    ADD CONSTRAINT ck_template_subject_variables_declared CHECK (
        jsonb_typeof(subject_variables) = 'array'
        AND (subject_variables = '[]'::jsonb
             OR (jsonb_typeof(variable_schema) = 'array' AND subject_variables <@ variable_schema))),
    DROP CONSTRAINT IF EXISTS ck_template_subject_variables_need_subject,
    ADD CONSTRAINT ck_template_subject_variables_need_subject CHECK (
        subject IS NOT NULL OR subject_variables = '[]'::jsonb);

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
        OR NEW.subject IS DISTINCT FROM OLD.subject
        OR NEW.subject_variables IS DISTINCT FROM OLD.subject_variables
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
