-- 000004_add_rule_code_index.up.sql
-- Index supporting the two queries added with rule-pack resolution and
-- overlap detection, neither of which existed when 000001 chose its indexes:
--
--   * hasOverlappingRule filters on (jurisdiction_id, rule_domain, rule_code)
--     before comparing effective periods, on every rule create and every
--     transition into a live status.
--   * FindRulePack de-duplicates with DISTINCT ON (rule_domain, rule_code)
--     across a whole ancestor chain.
--
-- idx_jrules_effective is (jurisdiction_id, rule_domain, effective_from,
-- effective_to) and cannot serve either — rule_code is not in it.

CREATE INDEX IF NOT EXISTS idx_jrules_domain_code
    ON jurisdiction_rules (jurisdiction_id, rule_domain, rule_code, effective_from);
