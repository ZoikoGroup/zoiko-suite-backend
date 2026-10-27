-- Migration 000016: employment.changed (Doc 03 §8.3 consumed event of
-- authorization-svc; Governance Platform audit row "Consume employment.changed").
--
-- authorization-svc could not consume employee.terminated: it names an
-- employee, and nothing mapped an employee to a principal. This service now
-- holds that mapping, administered (000015), so when a LINKED employee leaves
-- (employee.terminated, or a status move into TERMINATED / RESIGNED /
-- DEACTIVATED / INACTIVE / ARCHIVED) it publishes employment.changed naming
-- the principal. authorization-svc projects it as the principal's status, and
-- layer 0 then denies. An unlinked employee publishes nothing.

ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known CHECK (event_type IN (
    'role.created', 'role.updated', 'permission.bundle.updated',
    'iam.role.published',
    'iam.assignment.requested', 'iam.assignment.granted', 'iam.assignment.revoked',
    'iam.access_review.started', 'iam.access_review.completed',
    'employment.changed'
));
