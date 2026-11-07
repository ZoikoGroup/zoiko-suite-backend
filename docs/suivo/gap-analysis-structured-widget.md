# Gap analysis - Public AI Service vs Frontend Widget Requirements

**Generated:** 2026-10-29  
**Context:** Extend public chat contract for structured widget (menu + follow-up chips pattern)

## Current State vs Required

### 1. Chat Response (turns endpoint)
**Current (contract.Envelope):**
- schema_version, turn_id, request_id, turn_sequence
- status (ANSWERED|CLARIFY|REFUSED_ACTION|ABSTAIN|HANDOFF|UNAVAILABLE) 
- intent_id, text, citations[], actions[], knowledge_release_id, policy_version
- flags, answer_class, status_label, market_code, clarification
- secondary_intent_id, reason, suggestions[], draft
- outcome type is implicit from status/reason; missing explicit typed outcome like GROUNDED | ABSTAIN | SAFE_FALLBACK | REFUSED | HANDOFF_OFFER
- citations only have {source_version_id, claim_ids, url_key} - frontend needs {title, route_key, url} from signed registry
- no blocks[] structure
- suggestions[] exist as chips but model doesn't select them - but need followups[] with {id, label, intent_id} max 4 from intent catalog's approved follow-up map
- actions[] limited to OPEN_OFFICIAL_ROUTE with route_key; need cta with {primary?, secondary?} max 1+1
- no release tuple metadata visible in envelope? (has knowledge_release_id, policy_version - need to confirm if "release tuple metadata" required)

**Required additions (additive, optional):**
- `outcome` or map status to GROUNDED|ABSTAIN|SAFE_FALLBACK|REFUSED|HANDOFF_OFFER (typed outcome) + release tuple metadata
- `blocks[]`: {kind: lead|paragraph|ordered_list|unordered_list|howto|callout, text|items[], severity?: important|note} - model may only supply wording that passes schema validation; server assembles deterministically from approved content/templates
- `citations[]` extended to include title, route_key, url resolved from signed route/knowledge registry
- `followups[]`: {id, label, intent_id} max 4, chosen deterministically from intent catalog's approved follow-up map (model never selects)
- `cta`: {primary?: {route_key,label}, secondary?: {route_key,label}} max one primary + one secondary

### 2. GET /config (widget-config equivalent)
**Current:** GET /public-ai/v1/config returns ConfigResponse with config_revision, assistant_display_name, capabilities, supported_locales, market_choices, public_routes[], citation_links[], privacy_notice_key, microcopy{}, chips{}
**Required:** {assistant_name, badge_text, public_ai_enabled, static_fallback{text, route_key, url}, trust_claims[{label, verified}], feature flags}
- trust_claims[].verified must be computed from real checkable server state (e.g. citation enforcement active), never static
- need to add/extend config endpoint

### 3. GET /menu (new endpoint)
**Required:** {categories[{id,label,items[{id,label,intent_id}]}]} built from canonical intent families and ten canonical modules; no invented categories

### 4. Feature flags
Need `structured_response_enabled` (default off) - feature flag to keep old clients working

### 5. Kill switch/static fallback
When public_ai_enabled=false or kill switch on: widget-config and chat return approved static handoff with zero model calls; must survive edge caching and rollback

### 6. Security/Isolation (must preserve)
- No route to private finance/HR/payroll/tax/bank services
- Model has no tools, authorizes nothing
- Price/plan/trial/market/certification/residency from signed effective-dated P1 claims only, revocation checked at final commit; if unverifiable -> ABSTAIN with official link
- Revocation monotonic
- No raw message/PII in logs/traces/metrics/analytics; metric labels low-cardinality, no session IDs as labels
- One active turn per session enforced server-side across replicas
- Route click not a case/lead/booking; never return "received/confirmed" without receipt from system of record
- Existing tests must not be weakened

## Gaps to Address
1. Extend contract types for new response fields (blocks, followups, cta, outcome/metadata)
2. Add menu endpoint
3. Extend/enhance widget-config (or add new endpoint) - check if frontend expects /widget-config or /config
4. Add structured block assembly (deterministic server code, model only supplies validated wording)
5. Add followup selection from intent catalog with approved follow-up map
6. Add CTA construction logic
7. Add feature flag `structured_response_enabled` (default false)
8. Enhance kill switch handling for static fallback with zero model calls
9. Add OpenAPI/JSON Schema
10. Add contract tests and fixtures
11. Add ADRs for any differences from existing contracts
12. Ensure backward compatibility
