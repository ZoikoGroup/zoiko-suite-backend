-- Migration 000015: the employee-to-principal subject link (Authorization
-- Standard §24 event-triggered reviews; Group 1 audit gap S9-C2).
--
-- employee-master-svc publishes employee.updated (manager_employee_id),
-- employee.status.changed and employee.terminated; offboarding-severance-svc
-- publishes employee.terminated. Every one names an employee_id, and no
-- service on this estate maps an employee to the principal that holds access
-- (authorization-svc's 000013 records the same gap). Joining on a guess — a
-- matching email, a principal id that happens to equal the employee id —
-- would open, or worse act on, the wrong person's review.
--
-- So the link is administered, not inferred: a ROLE_MANAGE holder records
-- "employee E is principal P" (POST /v1/iam/subject-links), and the HR-event
-- consumer acts only on linked employees. An unlinked employee's event is
-- counted and skipped, never guessed.
--
-- last_manager_employee_id / last_status are what this service last saw, so
-- an employee.updated (which carries the new manager only) can be recognised
-- as a manager CHANGE, and a status event as a transition out of ACTIVE.

CREATE TABLE IF NOT EXISTS iam_subject_links (
    tenant_id                VARCHAR(255) NOT NULL,
    employee_id              VARCHAR(255) NOT NULL,
    principal_id             VARCHAR(255) NOT NULL,
    legal_entity_id          VARCHAR(255) NOT NULL,
    last_manager_employee_id VARCHAR(255),
    last_status              VARCHAR(50),
    linked_by_principal_id   VARCHAR(255) NOT NULL,
    correlation_id           VARCHAR(255) NOT NULL,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, employee_id)
);
CREATE INDEX IF NOT EXISTS idx_iam_subject_links_principal ON iam_subject_links (tenant_id, principal_id);

ALTER TABLE iam_subject_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE iam_subject_links FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON iam_subject_links;
CREATE POLICY tenant_isolation_policy ON iam_subject_links FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_app') THEN
        GRANT SELECT, INSERT, UPDATE ON iam_subject_links TO zoiko_app;
    END IF;
END
$$;
