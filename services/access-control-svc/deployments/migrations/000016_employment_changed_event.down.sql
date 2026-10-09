DELETE FROM event_outbox WHERE event_type = 'employment.changed';
ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known CHECK (event_type IN (
    'role.created', 'role.updated', 'permission.bundle.updated',
    'iam.role.published',
    'iam.assignment.requested', 'iam.assignment.granted', 'iam.assignment.revoked',
    'iam.access_review.started', 'iam.access_review.completed'
));
