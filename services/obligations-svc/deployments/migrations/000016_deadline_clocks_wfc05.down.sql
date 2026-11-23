-- 000016_deadline_clocks_wfc05.down.sql
-- Restores the 000014 shape of obligation_clocks. Successor clocks created by
-- a recalculation stay as rows (their supersedes link is dropped with the
-- column); escalation, milestone and transition history is dropped with its
-- tables.

BEGIN;

DROP TABLE IF EXISTS consumed_events;
DROP INDEX IF EXISTS idx_event_outbox_destination_due;
ALTER TABLE event_outbox DROP COLUMN IF EXISTS destination;

DROP TABLE IF EXISTS escalation_events;
DROP TABLE IF EXISTS obligation_clock_transitions;
DROP TABLE IF EXISTS obligation_clock_milestones;
DROP TABLE IF EXISTS escalation_policies;
DROP FUNCTION IF EXISTS escalation_policies_immutable();
DROP FUNCTION IF EXISTS obligation_clock_milestones_fire_once();
DROP FUNCTION IF EXISTS obligations_append_only();

DROP POLICY IF EXISTS obligation_clocks_sweep_select ON obligation_clocks;
DROP POLICY IF EXISTS obligation_clocks_tenant_isolation ON obligation_clocks;
CREATE POLICY obligation_clocks_tenant_isolation ON obligation_clocks
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

DROP TRIGGER IF EXISTS obligation_clocks_no_delete ON obligation_clocks;
DROP FUNCTION IF EXISTS obligations_refuse_delete();
DROP TRIGGER IF EXISTS obligation_clocks_immutable_provenance ON obligation_clocks;
DROP FUNCTION IF EXISTS obligation_clocks_immutable_provenance();

DROP INDEX IF EXISTS obligation_clocks_due_idx;
DROP INDEX IF EXISTS obligation_clocks_calendar_version_idx;
DROP INDEX IF EXISTS obligation_clocks_page_idx;

ALTER TABLE obligation_clocks DROP CONSTRAINT IF EXISTS obligation_clocks_obligation_id_fkey;
ALTER TABLE obligation_clocks ADD CONSTRAINT obligation_clocks_obligation_id_fkey
    FOREIGN KEY (obligation_id) REFERENCES obligations (obligation_id) ON DELETE CASCADE;

-- Carry the authority fields back into the 000014 columns.
UPDATE obligation_clocks SET
    waived_by_principal_id = CASE WHEN authority_action = 'WAIVE' THEN requested_by ELSE waived_by_principal_id END,
    waived_reason = CASE WHEN authority_action = 'WAIVE' THEN COALESCE(reason_text, reason_code) ELSE waived_reason END,
    not_applicable_by_principal_id = CASE WHEN authority_action = 'NOT_APPLICABLE' THEN requested_by ELSE not_applicable_by_principal_id END,
    not_applicable_reason = CASE WHEN authority_action = 'NOT_APPLICABLE' THEN COALESCE(reason_text, reason_code) ELSE not_applicable_reason END;

ALTER TABLE obligation_clocks DROP CONSTRAINT IF EXISTS obligation_clocks_na_decision_chk;
ALTER TABLE obligation_clocks DROP CONSTRAINT IF EXISTS obligation_clocks_authority_fields_chk;
ALTER TABLE obligation_clocks DROP CONSTRAINT IF EXISTS obligation_clocks_fulfilled_evidence_chk;
ALTER TABLE obligation_clocks DROP CONSTRAINT IF EXISTS obligation_clocks_plane_chk;
ALTER TABLE obligation_clocks DROP CONSTRAINT IF EXISTS obligation_clocks_priority_chk;
ALTER TABLE obligation_clocks DROP CONSTRAINT IF EXISTS obligation_clocks_authority_chk;
ALTER TABLE obligation_clocks DROP CONSTRAINT IF EXISTS obligation_clocks_key_uq;

ALTER TABLE obligation_clocks
    DROP COLUMN clock_key,
    DROP COLUMN request_hash,
    DROP COLUMN calculation_request,
    DROP COLUMN jurisdiction_code,
    DROP COLUMN rule_obligation_code,
    DROP COLUMN rule_content_digest,
    DROP COLUMN calendar_version,
    DROP COLUMN calendar_version_id,
    DROP COLUMN timezone,
    DROP COLUMN business_day_adjustment,
    DROP COLUMN anchor_date,
    DROP COLUMN unadjusted_date,
    DROP COLUMN local_due_date,
    DROP COLUMN extension_days_granted,
    DROP COLUMN pack_ref,
    DROP COLUMN pack_version,
    DROP COLUMN pack_version_id,
    DROP COLUMN calculation_ref,
    DROP COLUMN calculation_explanation,
    DROP COLUMN trigger_event_id,
    DROP COLUMN trigger_event_type,
    DROP COLUMN trigger_event_time,
    DROP COLUMN business_event_time,
    DROP COLUMN source_object_type,
    DROP COLUMN source_object_id,
    DROP COLUMN source_object_version,
    DROP COLUMN ingested_at,
    DROP COLUMN ingestion_latency_ms,
    DROP COLUMN supersedes_clock_id,
    DROP COLUMN supersession_reason,
    DROP COLUMN evidence_refs,
    DROP COLUMN evidence_evaluation_id,
    DROP COLUMN evidence_outcome,
    DROP COLUMN evidence_exempt_reason,
    DROP COLUMN domain_confirmation_ref,
    DROP COLUMN subject_state,
    DROP COLUMN authority_action,
    DROP COLUMN requested_by,
    DROP COLUMN approved_by,
    DROP COLUMN approval_reference,
    DROP COLUMN reason_code,
    DROP COLUMN reason_text,
    DROP COLUMN authority_evidence_refs,
    DROP COLUMN applicability_decision_id,
    DROP COLUMN overdue_since,
    DROP COLUMN escalation_policy_id,
    DROP COLUMN escalation_policy_version,
    DROP COLUMN priority,
    DROP COLUMN additional_owners,
    DROP COLUMN plane,
    DROP COLUMN correlation_id,
    DROP COLUMN causation_id,
    DROP COLUMN data_class,
    DROP COLUMN residency_region,
    DROP COLUMN created_by,
    DROP COLUMN updated_by;

ALTER TABLE obligation_clocks RENAME COLUMN legacy_waived_evidence_ids TO waived_evidence_ids;
ALTER TABLE obligation_clocks RENAME COLUMN legacy_satisfied_evidence_ids TO satisfied_evidence_ids;
ALTER TABLE obligation_clocks RENAME COLUMN legacy_escalation_ladder TO escalation_ladder;
ALTER TABLE obligation_clocks RENAME COLUMN row_version TO version;
ALTER TABLE obligation_clocks RENAME COLUMN rule_version TO calendar_rule_version;
-- A successor created after 000016 may carry a rule id that is not a uuid;
-- 000014 typed the column uuid. Such ids are mapped to NULL-safe uuids only
-- when they parse; otherwise the down refuses rather than invent one.
ALTER TABLE obligation_clocks ALTER COLUMN obligation_rule_id TYPE uuid USING obligation_rule_id::uuid;
ALTER TABLE obligation_clocks RENAME COLUMN obligation_rule_id TO calendar_rule_id;
ALTER TABLE obligation_clocks RENAME COLUMN due_at TO computed_deadline;
UPDATE obligation_clocks SET calendar_code = '' WHERE calendar_code IS NULL;
ALTER TABLE obligation_clocks ALTER COLUMN calendar_code SET NOT NULL;

ALTER TABLE obligation_clocks DROP CONSTRAINT IF EXISTS obligation_clocks_state_chk;
ALTER TABLE obligation_clocks DROP CONSTRAINT IF EXISTS obligation_clocks_class_chk;
UPDATE obligation_clocks SET clock_class = legacy_clock_class WHERE legacy_clock_class IS NOT NULL;
UPDATE obligation_clocks SET clock_class = 'SOFT_INTERNAL' WHERE clock_class IN ('INTERNAL_SLA', 'REGULATORY', 'CONTRACTUAL');
ALTER TABLE obligation_clocks DROP COLUMN legacy_clock_class;

COMMIT;
