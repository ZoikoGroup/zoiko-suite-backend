DROP TRIGGER IF EXISTS discovery_manifests_append_only ON discovery_manifests;
DROP TRIGGER IF EXISTS identity_verification_events_append_only ON identity_verification_events;
DROP FUNCTION IF EXISTS reject_evidence_mutation();

DROP TRIGGER IF EXISTS rights_requests_closed_immutable ON rights_requests;
DROP FUNCTION IF EXISTS reject_closed_request_mutation();
