-- 000008_add_com02_changes.down.sql
-- Rolling back refuses if any version already records a 2b change type: the
-- restored constraint cannot hold those rows, and dropping them silently
-- would destroy subscription history.

DROP TABLE IF EXISTS subscription_changes;

ALTER TABLE subscription_versions DROP CONSTRAINT subscription_versions_change_type_check;
ALTER TABLE subscription_versions ADD CONSTRAINT subscription_versions_change_type_check
    CHECK (change_type IN ('STARTED', 'TRIAL_CONVERSION', 'TRIAL_EXPIRY', 'ACTIVATED',
                           'CANCELLATION_SCHEDULED', 'CANCELLATION_EFFECTIVE', 'CANCELED_NOW',
                           'REACTIVATED', 'TERM_EXPIRY'));

DROP TABLE IF EXISTS plan_transition_rules;
DROP FUNCTION IF EXISTS enforce_transition_rule_retire_only();
