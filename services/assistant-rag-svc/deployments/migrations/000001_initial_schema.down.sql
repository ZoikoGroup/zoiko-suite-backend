-- Rollback for assistant-rag-svc (AI-03) initial schema.

DROP TRIGGER IF EXISTS trigger_tool_proposal_lifecycle ON tool_proposals;
DROP FUNCTION IF EXISTS enforce_tool_proposal_lifecycle();

DROP TRIGGER IF EXISTS trigger_ai_response_evidence_immutable ON ai_response_evidence;
DROP TRIGGER IF EXISTS trigger_citations_immutable ON citations;
DROP TRIGGER IF EXISTS trigger_prompt_executions_immutable ON prompt_executions;
DROP TRIGGER IF EXISTS trigger_retrieved_items_immutable ON retrieved_items;
DROP TRIGGER IF EXISTS trigger_retrieval_sets_immutable ON retrieval_sets;
DROP FUNCTION IF EXISTS reject_immutable_row();

DROP TRIGGER IF EXISTS trigger_session_lifecycle ON assistant_sessions;
DROP FUNCTION IF EXISTS enforce_session_lifecycle();

DROP TRIGGER IF EXISTS trigger_tool_policies_no_delete ON tool_policies;
DROP TRIGGER IF EXISTS trigger_source_grants_no_delete ON source_grants;
DROP FUNCTION IF EXISTS reject_delete();

DROP TABLE IF EXISTS outbox_events CASCADE;
DROP TABLE IF EXISTS idempotency_keys CASCADE;
DROP TABLE IF EXISTS tool_proposals CASCADE;
DROP TABLE IF EXISTS ai_response_evidence CASCADE;
DROP TABLE IF EXISTS citations CASCADE;
DROP TABLE IF EXISTS prompt_executions CASCADE;
DROP TABLE IF EXISTS retrieved_items CASCADE;
DROP TABLE IF EXISTS retrieval_sets CASCADE;
DROP TABLE IF EXISTS assistant_sessions CASCADE;
DROP TABLE IF EXISTS tool_policies CASCADE;
DROP TABLE IF EXISTS source_grants CASCADE;
