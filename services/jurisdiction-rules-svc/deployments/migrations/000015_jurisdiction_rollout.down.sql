-- 000015 down: removes the rollout governance tables.
DROP TABLE IF EXISTS rollout_events;
DROP TABLE IF EXISTS rollout_expert_approvals;
DROP TABLE IF EXISTS rollout_attestations;
DROP TABLE IF EXISTS rollout_packs;
DROP TABLE IF EXISTS jurisdiction_rollouts;
DROP FUNCTION IF EXISTS jur_rollout_guard();
DROP FUNCTION IF EXISTS jur_rollout_independence();
