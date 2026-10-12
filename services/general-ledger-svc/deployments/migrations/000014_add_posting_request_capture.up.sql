-- Migration: 000014_add_posting_request_capture.up.sql
--
-- ACC-04 ReprocessFailedPosting for executions that failed BEFORE any journal
-- was written.
--
-- PostAccountingEvent records an execution, then writes the journal. If that
-- write fails (a store error after the execution row exists), the execution
-- is FAILED with no journal — and it could never be fixed: reprocess refused
-- it ("no persisted original request to safely replay"), and resubmitting the
-- source event returned the prior FAILED result, because a duplicate source
-- event must return the prior result (ZS-SVC-B-001 ACC-04). Such an execution
-- sits in general-ledger's posting backlog forever, and the period close now
-- blocks on that backlog (financial-close-svc, ACC-14).
--
-- request_payload keeps the accepted request so reprocess can replay it.
-- Nullable: executions recorded before this migration have none, and are
-- refused exactly as before.
--
-- reprocess_claimed_at timestamps the claim a reprocess takes (status
-- VALIDATING) so two concurrent reprocesses cannot both create a journal,
-- and so a claim abandoned by a crashed process can be taken over later
-- rather than stranding the execution in VALIDATING.

ALTER TABLE posting_executions ADD COLUMN request_payload JSONB;
ALTER TABLE posting_executions ADD COLUMN reprocess_claimed_at TIMESTAMP WITH TIME ZONE;
