-- Migration 000012 down: drop the PBC request history and its triggers.
DROP TRIGGER IF EXISTS evidence_request_record_transition ON evidence_requests;
DROP FUNCTION IF EXISTS evidence_request_record_transition();
DROP TABLE IF EXISTS evidence_request_transitions;
DROP FUNCTION IF EXISTS evidence_request_transitions_append_only();
