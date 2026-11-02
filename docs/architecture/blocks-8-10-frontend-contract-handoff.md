# Blocks 8–10: backend API contract handoff for the frontend session

Written for: the frontend session that owns `E:/zwork/frontend` (branch `fix/blocks-6-10-fe`). Backend source of truth: `E:/zwork/backend-final` (branch `fix/blocks-6-10-final`, uncommitted, based on origin/main f4a9d9f5).

Every request/response/error example below was **captured from the running service** (native processes, real Postgres, 2026-11-02), not written by hand. The frontend repo was not modified.

## 1. Rules that apply to every call

- **Headers (every request):** `X-Tenant-Id`, `X-Principal-Id`, `X-Legal-Entity-Id`, `X-Request-Id`, `X-Correlation-ID`, `X-Source-Channel`, `X-Purpose-Context`. **Writes** (POST/PUT/PATCH/DELETE) also need `Idempotency-Key`.
- **Tenant comes from `X-Tenant-Id` only.** `tenant_id` in a query string is ignored; in a body it is rejected or must equal the header. Remove `tenant_id` from every client call.
- **Missing tenant → 401; missing principal → 401; missing Idempotency-Key on a write → 400.** Authorization denial → 403. Unknown id or another tenant's id → 404 (indistinguishable by design).
- **Replays:** the same `Idempotency-Key` with the same body returns the first answer (header `X-Idempotent-Replay: true`); with a different body → 409 (`idempotency_mismatch`).
- **Error bodies differ by service:** most Block 8–10 services return RFC 9457 `application/problem+json` (`type,title,status,detail,code`); some return `{error,message}`. Read `status` + `code`/`error`, not the message text.
- **Routing:** use gateway mode (`ZOIKO_USE_GATEWAY=1`, prefix `/<service-name>`). Direct host ports are listed below; 16 services are no longer on their container port (collisions with main-stack services).
- **Do not turn errors into empty lists or mock data** (`app/api/v1/[...path]/route.ts` returns `[]` on any failure; `tax.ts ENABLE_MOCK_FALLBACK` defaults to true; `audit-events.ts` falls back to fixtures).

## 2. Per service: verified core contract, then the frontend fixes

### carta-svc

Gateway prefix `/carta-svc` · container port 8142 · host port 8142

- **Create:** `POST /v1/carta/evaluate` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "context": {"subject_id": "65c1002f-d169-4c60-bd57-a608bd7a5067", "subject_type": "USER", "device_trust_level": 80, "ip_address": "10.0.0.1", "is_known_location": true, "resource_sensitivity": "HIGH", "action_requested": "READ", "time_of_day_hour": 10}}`
  - response shape: `{"id": "str", "tenant_id": "str", "legal_entity_id": "str", "subject_id": "str", "context": "object", "trust_score": "int", "risk_level": "str", "decision": "str", "risk_factors": "array", "assessment_timestamp": "str"}`
- **Get:** `GET /v1/carta/assessments/{id}` → 200
- **List:** `GET /v1/carta/assessments ?legal_entity_id=328effbb-fd61-4862-93a4-7c9fbf02e58f` → 200; shape `{"count": "int", "data": "array"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/problem+json` {"code": "missing_tenant_scope", "correlation_id": "dfb1ca1f-0f2d-44c8-87ef-330a11c6902c", "detail": "X-Tenant-ID is required \u2014 the gateway sets it from a verified identity envelope", "instance": "urn:zoikosuite:pro
  - missing X-Principal-Id: 201 `application/json` {"id": "1810a088-11a3-40ed-a31b-1275a38f557a", "tenant_id": "6d220548-3ffc-4a29-aa46-8d7172da8cdb", "legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "subject_id": "90d0e06b-491c-49d3-a779-fd72ad1ef84f", "contex
  - invalid body: 400 `application/problem+json` {"code": "invalid_request", "correlation_id": "243600ab-dbae-40e9-8899-7906a23c020d", "detail": "context.subject_id is required", "instance": "urn:zoikosuite:problem:caa3f6cd-21ae-4f4c-ae4e-c24a74e975f4", "request_id": "
  - authorization denied: 201 `application/json` {"id": "4e23744e-4919-4dc5-816e-c6e6dfdf4137", "tenant_id": "6d220548-3ffc-4a29-aa46-8d7172da8cdb", "legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "subject_id": "2c7f8836-9467-4ed2-8c97-5984f04649d8", "contex
  - unknown id: 404 `application/problem+json` {"code": "not_found", "correlation_id": "495101c4-87da-4769-b263-04628eda5765", "detail": "assessment not found", "instance": "urn:zoikosuite:problem:a70a8b77-da81-4035-aa93-56a8d1ce0173", "request_id": "a70a8b77-da81-40
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/security-trust.ts:247` | GET /v1/carta/assessments (no legal_entity_id) | GET /v1/carta/assessments?legal_entity_id=… | missing legal_entity_id → 400 |

### key-management-svc

Gateway prefix `/key-management-svc` · container port 8143 · host port 8143

- **Create:** `POST /v1/keys` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "key_alias": "alias-4dafa38a", "key_model": "BYOK", "key_provider": "AWS_KMS", "external_key_arn": "arn:aws:kms:eu-west-2:111122223333:key/80d5d974-d4eb-4b15-8bda-1ea90eb8785e"}`
  - response shape: `{"id": "str", "tenant_id": "str", "legal_entity_id": "str", "key_alias": "str", "key_model": "str", "key_provider": "str", "external_key_arn": "str", "key_version": "int", "state": "str", "rotation_count": "int", "created_at": "str", "updated_at": "str"}`
- **Get:** `GET /v1/keys/{id}` → 200
- **List:** `GET /v1/keys ?legal_entity_id=328effbb-fd61-4862-93a4-7c9fbf02e58f` → 200; shape `{"count": "int", "data": "array"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/problem+json` {"code": "identity_missing", "correlation_id": "8631643e-3a87-4206-8abd-13ecac67d3d4", "detail": "canonical input contract violated: tenant_id", "error": "envelope_incomplete", "instance": "urn:zoikosuite:problem:4e5e834
  - missing X-Principal-Id: 401 `application/problem+json` {"code": "identity_missing", "correlation_id": "812fd4de-aa83-4e9f-b767-73162b50a763", "detail": "canonical input contract violated: actor_subject_id", "error": "envelope_incomplete", "instance": "urn:zoikosuite:problem:
  - invalid body: 400 `application/problem+json` {"code": "invalid_request", "correlation_id": "471abd46-58b7-4713-8159-f72f54aad2bd", "detail": "key_alias is required", "instance": "urn:zoikosuite:problem:7f9d1616-e49f-46b1-aaa2-80e080277fa7", "request_id": "7f9d1616-
  - authorization denied: 403 `application/problem+json` {"code": "authorization_denied", "correlation_id": "f360e6c4-5365-4133-b9b7-9867cfcc3d4b", "detail": "not authorized to perform this action", "instance": "urn:zoikosuite:problem:1832e53a-f29c-41e1-988a-9c9d397a4b49", "re
  - unknown id: 404 `application/problem+json` {"code": "not_found", "correlation_id": "1b6fd1be-9706-42ce-8fab-e1b3eef8cf06", "detail": "key not found", "instance": "urn:zoikosuite:problem:327b8acc-63c9-412f-b894-d22ab0a03ac2", "request_id": "327b8acc-63c9-412f-b894
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/security-trust.ts:174` | GET /v1/keys (no legal_entity_id) | GET /v1/keys?legal_entity_id=… | missing legal_entity_id → 400 |

### external-data-feed-svc

Gateway prefix `/external-data-feed-svc` · container port 8149 · host port 8201

- **Create:** `POST /v1/external-data-feeds/subscriptions` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "provider": "ECB", "feed_type": "FX_RATE", "symbol": "EURGBP"}`
  - response shape: `{"feed_id": "str", "tenant_id": "str", "legal_entity_id": "str", "provider": "str", "feed_type": "str", "symbol": "str", "status": "str", "created_at": "str", "updated_at": "str"}`
- **Get:** `GET /v1/external-data-feeds/subscriptions/{id}` → 200
- **List:** `GET /v1/external-data-feeds/subscriptions` → 200; shape `{"subscriptions": "array", "total": "int"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/json` {"error": "missing_tenant_scope", "message": "X-Tenant-Id is required \u2014 the gateway sets it from a verified identity envelope"}
  - missing X-Principal-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: actor_subject_id", "service": "external-data-feed-svc", "violations": [{"field": "actor_subject_id", "header": "X-Principal-Id", "reason": "m
  - invalid body: 400 `application/json` {"error": "legal_entity_id, provider, and feed_type are required"}
  - authorization denied: 403 `application/json` {"error": "authorization denied"}
  - unknown id: 404 `application/json` {"error": "subscription not found"}
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/integration.ts:198-222` | /api/v1/subscriptions {feed_name, provider, data_type} | /v1/external-data-feeds/subscriptions {legal_entity_id, provider, feed_type, symbol} | feed_type ∈ MARKET_DATA|CREDIT_SCORE|COMPANY_INFO|FX_RATE|ESG_DATA; id field feed_id |

### tax-authority-interface-svc

Gateway prefix `/tax-authority-interface-svc` · container port 8147 · host port 8199

- **Create:** `POST /v1/tax-authority/interfaces` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "jurisdiction": "GB", "authority_name": "HMRC"}`
  - response shape: `{"interface_id": "str", "tenant_id": "str", "legal_entity_id": "str", "jurisdiction": "str", "authority_name": "str", "protocol": "str", "status": "str", "created_at": "str", "updated_at": "str"}`
- **Get:** `GET /v1/tax-authority/interfaces/{id}` → 200
- **List:** `GET /v1/tax-authority/interfaces` → 200; shape `{"interfaces": "array", "total": "int"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/json` {"error": "missing_tenant_scope", "message": "X-Tenant-Id is required \u2014 the gateway sets it from a verified identity envelope"}
  - missing X-Principal-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: actor_subject_id", "service": "tax-authority-interface-svc", "violations": [{"field": "actor_subject_id", "header": "X-Principal-Id", "reason
  - invalid body: 400 `application/json` {"error": "legal_entity_id, jurisdiction, and authority_name are required"}
  - authorization denied: 403 `application/json` {"error": "not authorized to create tax interface"}
  - unknown id: 404 `application/json` {"error": "tax interface not found"}
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/tax.ts:1025-1050` | POST interfaces {jurisdiction_id, authority_code, api_endpoint, auth_type} | POST /v1/tax-authority/interfaces {legal_entity_id, jurisdiction, authority_name, [protocol, provider_key]} | current body → 400 |
  | `lib/api/tax.ts:1052-1064, :1220` | POST /interfaces/{id}/test, PATCH /interfaces/{id} | no such routes; use GET /v1/tax-authority/health?provider_key=… | PATCH needs a spec decision |
  | `lib/api/tax.ts:568-590, :1227` | invented defaults + ENABLE_MOCK_FALLBACK=true | read jurisdiction, authority_name, protocol, status; default fallback to false |  |

### forecasting-svc

Gateway prefix `/forecasting-svc` · container port 8135 · host port 8191

- **Create:** `POST /v1/forecasts/generate` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "model_name": "cash-ffda31", "domain": "CASH_FLOW", "historical_data": [100, 110, 125, 130, 142, 150], "historical_start_date": "2026-01-01"}`
  - response shape: `{"id": "str", "tenant_id": "str", "legal_entity_id": "str", "model_name": "str", "domain": "str", "scenario_type": "str", "algorithm_type": "str", "granularity": "str", "horizon_periods": "int", "historical_data": "array", "historical_start_date": "str", "historical_end_date": "str", "growth_rate_adjustment": "int", "status": "str", "version": "int", "confidence_level": "int", "metadata": "object", "created_by": "str", "created_at": "str", "updated_at": "str", "projections": "array"}`
- **Get:** `GET /v1/forecasts/{id}` → 200
- **List:** `GET /v1/forecasts` → 200; shape `{"data": "array", "count": "int"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/problem+json` {"code": "missing_tenant_scope", "correlation_id": "5d49781a-ae19-484b-bf64-097dcc997bf5", "detail": "X-Tenant-Id is required; the gateway sets it from a verified identity envelope", "instance": "/v1/forecasts/generate",
  - missing X-Principal-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: actor_subject_id", "service": "forecasting-svc", "violations": [{"field": "actor_subject_id", "header": "X-Principal-Id", "reason": "mandator
  - invalid body: 422 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/domain-validation", "title": "The request failed domain validation.", "status": 422, "detail": "model_name is required", "instance": "/v1/forecasts/generate", "code": "valida
  - authorization denied: 403 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/authorization-denied", "title": "The request is not permitted.", "status": 403, "detail": "authorization-svc denied this action", "instance": "/v1/forecasts/generate", "code"
  - unknown id: 404 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/not-found", "title": "The resource was not found.", "status": 404, "detail": "forecast not found", "instance": "/v1/forecasts/b8f0995e-cea3-4ec2-8a0d-879d20ddfa69", "code": "
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/intelligence.ts:47` | GET /api/v1/forecasts?tenant_id=… | GET /v1/forecasts | response {data, count, next_cursor} (not {forecasts,total}); status ∈ CALCULATED|REVIEWED|APPROVED|PUBLISHED|SUPERSEDED|ARCHIVED |
  | `lib/api/intelligence.ts:56` | POST /api/v1/forecasts {tenant_id, model_type, domain, period} | POST /v1/forecasts/generate {legal_entity_id, model_name, domain, historical_data[≥2], historical_start_date} | domain ∈ FINANCIAL|PAYROLL|CASH_FLOW|WORKFORCE|TAX_LIABILITY; response is the forecast object (not {forecast}) |

### compliance-risk-scoring-svc

Gateway prefix `/compliance-risk-scoring-svc` · container port 8177 · host port 8177

- **Create:** `POST /v1/risk-scores/calculate` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "assessment_name": "q3-7fbf29", "subject_type": "LEGAL_ENTITY", "subject_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "open_obligations_count": 2, "policy_violations_count": 0, "audit_exceptions_count": 1, "privacy_incidents_count": 0, "tax_penalties_count": 0}`
  - response shape: `{"id": "str", "tenant_id": "str", "legal_entity_id": "str", "subject_type": "str", "subject_id": "str", "assessment_name": "str", "open_obligations_count": "int", "policy_violations_count": "int", "audit_exceptions_count": "int", "privacy_incidents_count": "int", "tax_penalties_count": "int", "composite_risk_score": "str", "risk_tier": "str", "model_version": "str", "input_hash": "str", "thresholds_applied": "object", "band_rationale": "str", "assurance_state": "str", "requires_human_review": "bool", "review_status": "str", "reviewed_by": "null", "reviewed_at": "null", "review_note": "null", "`
- **Get:** `GET /v1/risk-scores/{id}` → 200
- **List:** `GET /v1/risk-scores ?legal_entity_id=328effbb-fd61-4862-93a4-7c9fbf02e58f` → 200; shape `{"data": "array", "count": "int", "next_cursor": "null"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/missing-tenant-scope", "title": "Unauthorized", "status": 401, "detail": "X-Tenant-Id is required; the gateway sets it from a verified identity envelope", "instance": "/v1/ri
  - missing X-Principal-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: actor_subject_id", "service": "compliance-risk-scoring-svc", "violations": [{"field": "actor_subject_id", "header": "X-Principal-Id", "reason
  - invalid body: 400 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/validation-failed", "title": "Bad Request", "status": 400, "detail": "request body failed validation", "instance": "/v1/risk-scores/calculate", "code": "validation_failed", "
  - authorization denied: 403 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/authorization-denied", "title": "Forbidden", "status": 403, "detail": "principal is not authorized to perform RISK_SCORE_CREATE", "instance": "/v1/risk-scores/calculate", "co
  - unknown id: 404 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/not-found", "title": "Not Found", "status": 404, "detail": "risk score eb563113-cce3-4554-9b83-4678176d48bb not found", "instance": "/v1/risk-scores/eb563113-cce3-4554-9b83-4
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/intelligence.ts:79` | GET /api/v1/scores | GET /v1/risk-scores?legal_entity_id=… | response {data, count, next_cursor}; id (not score_id); composite_risk_score is a decimal STRING e.g. "53.75"; risk_tier ∈ LOW|MODERATE|HIGH|CRITICAL; factor_breakdowns[] |
  | `lib/api/compliance.ts:198` | GET /v1/scores | GET /v1/risk-scores?legal_entity_id=… | same as above |
  | `lib/api/config.ts` | complianceRiskScoring → 127.0.0.1:8136 | gateway /compliance-risk-scoring-svc or host 8177 (container port is now 8177) | 8136 is delegated-authority-svc |

### anomaly-detection-svc

Gateway prefix `/anomaly-detection-svc` · container port 8134 · host port 8190

- **Create:** `POST /v1/anomalies/rules` → **201**
  - body (accepted): `{"rule_name": "r-addc12", "domain_name": "FINANCE", "metric_type": "AMOUNT", "threshold_value": 1000, "z_score_cutoff": 3}`
  - response shape: `{"rule_id": "str", "tenant_id": "str", "rule_name": "str", "domain_name": "str", "metric_type": "str", "threshold_value": "int", "z_score_cutoff": "int", "is_active": "bool", "version": "int", "created_by": "str", "created_at": "str", "updated_at": "str"}`
- **Get:** `GET /v1/anomalies/rules/{id}` → 200
- **List:** `GET /v1/anomalies/rules` → 200; shape `{"rules": "array", "total": "int"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/json` {"error": "missing_tenant_scope", "message": "X-Tenant-ID is required \u2014 the gateway sets it from a verified identity envelope"}
  - missing X-Principal-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: actor_subject_id", "service": "anomaly-detection-svc", "violations": [{"field": "actor_subject_id", "header": "X-Principal-Id", "reason": "ma
  - invalid body: 400 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/malformed_json", "title": "Bad Request", "status": 400, "detail": "the request body is not valid JSON for this endpoint: json: unknown field \"legal_entity_id\"", "instance":
  - authorization denied: 403 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/permission_denied", "title": "Forbidden", "status": 403, "detail": "authorization-svc denied this action for the caller", "instance": "/v1/anomalies/rules", "code": "permissi
  - unknown id: 404 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/anomaly_rule_not_found", "title": "Not Found", "status": 404, "detail": "anomaly rule not found", "instance": "/v1/anomalies/rules/arule-672cbf9c-6b14-427b-ae7b-93c2788be14d"
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/intelligence.ts:25` | GET /api/v1/anomalies?tenant_id=… | GET /v1/anomalies (tenant from X-Tenant-Id; filters: legal_entity_id, status) | Anomaly type: use domain_name, source_entity_id, rule_id, anomaly_score, observed_value, expected_value, legal_entity_id, assurance_state, version; status ∈ OPEN|UNDER_INVESTIGATION|CONFIRMED_ANOMALY|FALSE_POSITIVE|RESOLVED (no ACKNOWLEDGED) |
  | `lib/api/config.ts:95` | anomalyDetection → 127.0.0.1:8153 | gateway /anomaly-detection-svc (host 8190 direct; container 8134) | 8153 is privacy-decision-svc in the backend main stack |

### reporting-orchestration-svc

Gateway prefix `/reporting-orchestration-svc` · container port 8138 · host port 8194

- **Create:** `POST /v1/reports/definitions` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "report_name": "rpt-7e1387", "report_type": "FINANCIAL_SUMMARY", "output_format": "CSV", "data_sources": ["general-ledger-svc"]}`
  - response shape: `{"id": "str", "tenant_id": "str", "legal_entity_id": "str", "report_name": "str", "report_type": "str", "output_format": "str", "data_sources": "array", "is_scheduled": "bool", "status": "str", "version": "int", "created_by": "str", "created_at": "str", "updated_at": "str"}`
- **Get:** `GET /v1/reports/definitions/{id}` → 200
- **List:** `GET /v1/reports/definitions` → 200; shape `{"data": "array", "count": "int", "limit": "int", "offset": "int"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/problem+json` {"code": "TENANT_REQUIRED", "detail": "X-Tenant-Id is required \u2014 the gateway sets it from a verified identity envelope", "instance": "/v1/reports/definitions", "status": 401, "title": "Unauthorized", "type": "https:
  - missing X-Principal-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: actor_subject_id", "service": "reporting-orchestration-svc", "violations": [{"field": "actor_subject_id", "header": "X-Principal-Id", "reason
  - invalid body: 400 `application/problem+json` {"type": "https://errors.zoiko.io/reporting/validation-failed", "title": "Bad Request", "status": 400, "detail": "report_name is required", "code": "VALIDATION_FAILED", "instance": "/v1/reports/definitions"}
  - authorization denied: 403 `application/problem+json` {"type": "https://errors.zoiko.io/reporting/authorization-denied", "title": "Forbidden", "status": 403, "detail": "authorization denied", "code": "AUTHORIZATION_DENIED", "instance": "/v1/reports/definitions"}
  - unknown id: 404 `application/problem+json` {"type": "https://errors.zoiko.io/reporting/not-found", "title": "Not Found", "status": 404, "detail": "report definition not found", "code": "NOT_FOUND", "instance": "/v1/reports/definitions/10d89c90-05f9-4c66-9e8c-c073
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/intelligence.ts:128` | GET /api/v1/reports | GET /v1/reports/definitions | GET /v1/reports/runs | response {data, count, limit, offset} |
  | `lib/api/intelligence.ts:137` | POST /api/v1/reports {report_type, title} | POST /v1/reports/definitions {legal_entity_id, report_name, report_type, output_format, data_sources[]} → PATCH /v1/reports/definitions/{id}/status VALIDATED → ACTIVE → POST /v1/reports/definitions/{id}/runs | report_type ∈ FINANCIAL_SUMMARY|PAYROLL_SUMMARY|COMPLIANCE_OVERVIEW|AUDIT_TRAIL|CASH_FLOW|WORKFORCE_ANALYTICS; output_format ∈ JSON|CSV|PDF; runs are recorded NOT_IMPLEMENTED (no data produced yet) |

### esignature-integration-svc

Gateway prefix `/esignature-integration-svc` · container port 8148 · host port 8200

- **Create:** `POST /v1/esignature/envelopes` → **503**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "document_title": "NDA", "signer_email": "signer@example.com", "signer_name": "Signer", "provider": "DOCUSIGN"}`
  - response shape: `{"error": "str", "message": "str"}`
- **List:** `GET /v1/esignature/envelopes` → 200; shape `{"envelopes": "array", "total": "int"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/json` {"error": "missing_tenant_scope", "message": "X-Tenant-Id is required \u2014 the gateway sets it from a verified identity envelope"}
  - missing X-Principal-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: actor_subject_id", "service": "esignature-integration-svc", "violations": [{"field": "actor_subject_id", "header": "X-Principal-Id", "reason"
  - invalid body: 400 `application/json` {"error": "legal_entity_id, document_title, and signer_email are required"}
  - authorization denied: 403 `application/json` {"error": "authorization denied"}
  - unknown id: 404 `application/json` {"error": "envelope not found"}
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/integration.ts:154-176` | GET/POST /api/v1/envelopes {document_id, provider, signatories[]} | GET/POST /v1/esignature/envelopes {legal_entity_id, document_title, signer_email, signer_name, provider} | response is the envelope object; status ∈ PENDING|SENT|DELIVERED|SIGNED|VOIDED; handle 503 esignature_provider_not_configured (no provider is configured in any environment yet) |

### decision-support-svc

Gateway prefix `/decision-support-svc` · container port 8138 · host port 8138

- **Create:** `POST /v1/recommendations` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "subject_type": "JOURNAL", "subject_reference": "jr-085dfeac", "action_type": "REVIEW", "oversight_class": "O1", "evidence_refs": ["ev-1"], "proposal": {"recommended_action": "APPROVE", "confidence_score": 0.8, "rationale": "within tolerance", "model_id": "m-1", "model_version": "1.0", "prompt_version": "p1"}}`
  - response shape: `{"recommendation_id": "str", "tenant_id": "str", "legal_entity_id": "str", "subject_type": "str", "subject_reference": "str", "action_type": "str", "recommended_action": "str", "confidence_score": "float", "rationale": "str", "prior_decisions_sampled": "int", "outcome_distribution": "object", "history_status": "str", "evidence_refs": "array", "source": "str", "model_id": "str", "model_version": "str", "prompt_version": "str", "assurance_state": "str", "oversight_class": "str", "requires_independent_reviewer": "bool", "status": "str", "version": "int", "expires_at": "str", "downstream_action_ex`
- **Get:** `GET /v1/recommendations/{id}` → 200
- **List:** `GET /v1/recommendations` → 200; shape `{"items": "array", "next_cursor": "null"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: tenant_id", "service": "decision-support-svc", "violations": [{"field": "tenant_id", "header": "X-Tenant-Id", "reason": "mandatory: tenant au
  - missing X-Principal-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: actor_subject_id", "service": "decision-support-svc", "violations": [{"field": "actor_subject_id", "header": "X-Principal-Id", "reason": "man
  - invalid body: 400 `application/problem+json` {"type": "https://zoiko.io/problems/decision-support/validation_failed", "title": "Request validation failed", "status": 400, "detail": "subject_type: is required", "instance": "/v1/recommendations", "code": "validation_
  - authorization denied: 403 `application/problem+json` {"type": "https://zoiko.io/problems/decision-support/forbidden", "title": "Not authorized", "status": 403, "detail": "authorization denied for this decision support action", "instance": "/v1/recommendations", "code": "fo
  - unknown id: 404 `application/problem+json` {"type": "https://zoiko.io/problems/decision-support/not_found", "title": "Recommendation not found", "status": 404, "detail": "recommendation not found", "instance": "/v1/recommendations/0a483616-8229-4182-9ccc-b69645c9
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/intelligence.ts` | (no client) | POST/GET /v1/recommendations, POST /{id}/accept|reject|expire|supersede |  |

### migration-integrity-svc

Gateway prefix `/migration-integrity-svc` · container port 8139 · host port 8195

- **Create:** `POST /v1/migrations/validate` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "migration_name": "mig-3145e4", "source_system": "LEGACY_ERP", "target_service": "general-ledger-svc", "exception_owner_id": "921be5d3-4b4f-4028-be35-276e7f3dca0f", "control_totals": {"amount_field": "amount", "record_count": 2, "amount_total": "15.00"}, "required_fields": ["account", "amount"], "records": [{"ref": "r1", "fields": {"account": "1000", "amount": "10.00"}}, {"ref": "r2", "fields": {"account": "2000", "amount": "5.00"}}]}`
  - response shape: `{"id": "str", "tenant_id": "str", "legal_entity_id": "str", "migration_name": "str", "source_system": "str", "target_service": "str", "requested_by": "str", "total_records_count": "int", "valid_records_count": "int", "invalid_records_count": "int", "integrity_score": "int", "integrity_score_threshold": "int", "blocker_count": "int", "open_quarantine_count": "int", "status": "str", "version": "int", "control_totals": "object", "integrity_checks": "array", "started_at": "str", "completed_at": "str", "created_at": "str", "updated_at": "str"}`
- **Get:** `GET /v1/migrations/{id}` → 200
- **List:** `GET /v1/migrations` → 200; shape `{"data": "array", "count": "int", "limit": "int", "offset": "int", "next_offset": "null"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: tenant_id", "service": "migration-integrity-svc", "violations": [{"field": "tenant_id", "header": "X-Tenant-Id", "reason": "mandatory: tenant
  - missing X-Principal-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: actor_subject_id", "service": "migration-integrity-svc", "violations": [{"field": "actor_subject_id", "header": "X-Principal-Id", "reason": "
  - invalid body: 400 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/migration-integrity/invalid-request", "title": "Invalid request", "status": 400, "detail": "migration_name is required", "instance": "/v1/migrations/validate", "code": "inval
  - authorization denied: 403 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/migration-integrity/not-authorized", "title": "Not authorized", "status": 403, "detail": "authorization denied", "instance": "/v1/migrations/validate", "code": "not-authorize
  - unknown id: 404 `application/problem+json` {"type": "https://api.zoikosuite.com/problems/migration-integrity/job-not-found", "title": "Migration job not found", "status": 404, "detail": "migration job not found", "instance": "/v1/migrations/3f2c8259-e1bc-483c-94f
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/migration-integrity.ts` | (see below) | POST /v1/migrations/validate {legal_entity_id, migration_name, source_system, target_service, required_fields[], records[{ref, fields{}}], exception_owner_id, control_totals{amount_field, record_count, amount_total}} |  |

### mtls-management-svc

Gateway prefix `/mtls-management-svc` · container port 8140 · host port 8140

- **Create:** `POST /v1/mtls/certificates` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "service_name": "svc-777a23", "common_name": "svc.zoiko.local", "rotation_days": 30, "auto_rotate": true}`
  - response shape: `{"certificate": "object", "private_key_pem": "str", "ca_certificate_pem": "str"}`
- **Get:** `GET /v1/mtls/certificates/{id}` → 200
- **List:** `GET /v1/mtls/certificates ?legal_entity_id=328effbb-fd61-4862-93a4-7c9fbf02e58f` → 200; shape `{"count": "int", "data": "array"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/problem+json` {"code": "identity_missing", "correlation_id": "c4fe0727-37e0-4530-9064-d53e7789cf66", "detail": "canonical input contract violated: tenant_id", "error": "envelope_incomplete", "instance": "urn:zoikosuite:problem:7fd64bb
  - missing X-Principal-Id: 401 `application/problem+json` {"code": "identity_missing", "correlation_id": "47b56b02-6edd-4210-ba51-e7ac55e32055", "detail": "canonical input contract violated: actor_subject_id", "error": "envelope_incomplete", "instance": "urn:zoikosuite:problem:
  - invalid body: 400 `application/problem+json` {"code": "invalid_request", "correlation_id": "77383e5d-8a55-4f58-b7bd-f8f1e717ffca", "detail": "service_name is required", "instance": "urn:zoikosuite:problem:d1a922dc-1ef9-461a-a1f5-546fbbfa3672", "request_id": "d1a922
  - authorization denied: 403 `application/problem+json` {"code": "authorization_denied", "correlation_id": "c0d71737-29a2-4070-9eea-b121fa199819", "detail": "not authorized to perform this action", "instance": "urn:zoikosuite:problem:44166cc3-c9ed-4bb5-840e-8579810e0341", "re
  - unknown id: 404 `application/problem+json` {"code": "not_found", "correlation_id": "cdcf3043-e066-4b43-a2c4-d5dee770357d", "detail": "certificate not found", "instance": "urn:zoikosuite:problem:5fca297f-0003-4dbc-9419-19487d5c6b03", "request_id": "5fca297f-0003-4
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/security-trust.ts:67` | GET /v1/mtls/certificates?tenant_id=… | GET /v1/mtls/certificates?legal_entity_id=… | tenant_id query ignored; legal_entity_id required (400) |
  | `lib/api/security-trust.ts (provision/rotate)` | — | handle 409 idempotency_secret_not_replayable on a replayed provision/rotate | private keys are returned once and never stored, so a replay cannot return them |

### siem-integration-svc

Gateway prefix `/siem-integration-svc` · container port 8141 · host port 8141

- **Create:** `POST /v1/siem/exporters` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "name": "exp-6dcacd", "platform": "SPLUNK", "endpoint_url": "https://splunk.example.com:8088"}`
  - response shape: `{"id": "str", "tenant_id": "str", "legal_entity_id": "str", "name": "str", "platform": "str", "endpoint_url": "str", "status": "str", "events_sent": "int", "created_at": "str", "updated_at": "str"}`
- **Get:** `GET /v1/siem/exporters/{id}` → 200
- **List:** `GET /v1/siem/exporters ?legal_entity_id=328effbb-fd61-4862-93a4-7c9fbf02e58f` → 200; shape `{"count": "int", "data": "array"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/problem+json` {"code": "identity_missing", "correlation_id": "ec3efb12-3d30-4fb9-b76a-c418d56944fd", "detail": "canonical input contract violated: tenant_id", "error": "envelope_incomplete", "instance": "urn:zoikosuite:problem:580a76a
  - missing X-Principal-Id: 401 `application/problem+json` {"code": "identity_missing", "correlation_id": "9b133abf-a814-49fc-b083-2f4103272884", "detail": "canonical input contract violated: actor_subject_id", "error": "envelope_incomplete", "instance": "urn:zoikosuite:problem:
  - invalid body: 400 `application/problem+json` {"code": "invalid_request", "correlation_id": "4269250b-fcf2-40e9-a738-84b88e080677", "detail": "name is required", "instance": "urn:zoikosuite:problem:13d47f3f-2b38-43f3-a2ef-1f95687fcc61", "request_id": "13d47f3f-2b38-
  - authorization denied: 403 `application/problem+json` {"code": "authorization_denied", "correlation_id": "ba5d7103-6f48-4f02-b21c-2e3d3f8bb0b9", "detail": "not authorized to perform this action", "instance": "urn:zoikosuite:problem:8d6ae876-f98c-4792-a6b9-e6c7ee09c6b0", "re
  - unknown id: 404 `application/problem+json` {"code": "not_found", "correlation_id": "1f6804ef-2a8f-46a6-a2fc-a7ac0a91d404", "detail": "exporter not found", "instance": "urn:zoikosuite:problem:63d57f73-e93e-42dd-9755-d7bd86f2332c", "request_id": "63d57f73-e93e-42dd
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/security-trust.ts:127` | GET /v1/siem/events (no exporter_id) | GET /v1/siem/events?exporter_id=… | exporter_id required (400) |
  | `lib/api/security-trust.ts:135` | GET /v1/siem/exporters (no legal_entity_id) | GET /v1/siem/exporters?legal_entity_id=… | legal_entity_id required (400); auth_token is write-only, never returned |
  | `lib/api/security-trust.ts:106` | SiemExporter type | add status, events_sent, last_streamed, created_at, updated_at |  |

### banking-connector-svc

Gateway prefix `/banking-connector-svc` · container port 8145 · host port 8197

- **Create:** `POST /v1/banking/connections` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "bank_name": "Barclays-5d69", "bic": "BARCGB22", "account_number": "12345678", "iban": "GB33BUKB20201555555555", "currency": "GBP", "account_type": "CURRENT"}`
  - response shape: `{"connection_id": "str", "account_id": "str", "tenant_id": "str", "legal_entity_id": "str", "bank_name": "str", "bic": "str", "swift_bic": "str", "account_number": "str", "iban": "str", "currency": "str", "account_type": "str", "status": "str", "created_at": "str", "updated_at": "str"}`
- **Get:** `GET /v1/banking/connections/{id}` → 200
- **List:** `GET /v1/banking/connections ?legal_entity_id=328effbb-fd61-4862-93a4-7c9fbf02e58f` → 200; shape `{"accounts": "array", "connections": "array", "total": "int"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/json` {"error": "missing_tenant_scope", "message": "X-Tenant-Id is required \u2014 the gateway sets it from a verified identity envelope"}
  - missing X-Principal-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: actor_subject_id", "service": "banking-connector-svc", "violations": [{"field": "actor_subject_id", "header": "X-Principal-Id", "reason": "ma
  - invalid body: 400 `application/json` {"error": "legal_entity_id and bank_name are required"}
  - authorization denied: 403 `application/json` {"error": "authorization denied"}
  - unknown id: 404 `application/json` {"error": "bank connection not found"}
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/integration.ts:70` | GET /api/v1/connections?tenant_id | GET /v1/banking/connections?legal_entity_id=… | response {connections, accounts, total} |
  | `lib/api/integration.ts:78` | POST /api/v1/connections/{id}/sync | POST /v1/banking/connections/{id}/refresh |  |

### connectivity-api-bridge-svc

Gateway prefix `/connectivity-api-bridge-svc` · container port 8144 · host port 8196

- **Create:** `POST /v1/bridges` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "bridge_name": "br-54246c", "provider_name": "Acme", "protocol": "REST", "endpoint_url": "https://api.example.com", "auth_type": "OAUTH2"}`
  - response shape: `{"bridge_id": "str", "connection_id": "str", "tenant_id": "str", "legal_entity_id": "str", "bridge_name": "str", "provider_name": "str", "protocol": "str", "endpoint_url": "str", "base_url": "str", "auth_type": "str", "status": "str", "created_at": "str", "updated_at": "str", "sync_mode": "str", "scopes": "array", "destination_host": "str", "version": "int", "created_by": "str", "last_changed_by": "str"}`
- **Get:** `GET /v1/bridges/{id}` → 200
- **List:** `GET /v1/bridges` → 200; shape `{"bridges": "array", "connections": "array", "total": "int"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/json` {"error": "missing_tenant_scope", "message": "X-Tenant-Id is required \u2014 the gateway sets it from a verified identity envelope"}
  - missing X-Principal-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: actor_subject_id", "service": "connectivity-api-bridge-svc", "violations": [{"field": "actor_subject_id", "header": "X-Principal-Id", "reason
  - invalid body: 400 `application/problem+json` {"code": "INT-SCHEMA-004", "correlation_id": "3c8a4974-0fbb-476f-8bb5-29956383df2b", "detail": "legal_entity_id and bridge_name/provider_name are required", "error": "legal_entity_id and bridge_name/provider_name are req
  - authorization denied: 403 `application/problem+json` {"code": "INT-AUTH-003", "correlation_id": "35d2e4f9-05d9-4e54-86f4-36e43e8d28fa", "detail": "authorization denied", "error": "authorization denied", "instance": "/v1/bridges", "retryable": false, "status": 403, "title":
  - unknown id: 404 `application/problem+json` {"correlation_id": "f21b22c5-23fa-4356-852a-626e0e8f11f3", "detail": "bridge endpoint not found", "error": "bridge endpoint not found", "instance": "/v1/bridges/8bc46967-2119-45b0-95f6-33046ae7eca4", "status": 404, "titl
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/integration.ts` | connections via proxy | /v1/bridges, /v1/api-bridge/connections, /v1/integration-connections |  |

### hris-connector-svc

Gateway prefix `/hris-connector-svc` · container port 8146 · host port 8198

- **Create:** `POST /v1/hris/integrations` → **201**
  - body (accepted): `{"legal_entity_id": "328effbb-fd61-4862-93a4-7c9fbf02e58f", "provider_name": "Workday-67af", "sync_type": "EMPLOYEES", "direction": "INBOUND", "api_endpoint": "https://wd.example.com/api"}`
  - response shape: `{"integration_id": "str", "sync_id": "str", "tenant_id": "str", "legal_entity_id": "str", "provider_name": "str", "provider": "str", "sync_type": "str", "direction": "str", "api_endpoint": "str", "status": "str", "created_at": "str", "updated_at": "str"}`
- **Get:** `GET /v1/hris/integrations/{id}` → 200
- **List:** `GET /v1/hris/integrations` → 200; shape `{"integrations": "array", "syncs": "array", "total": "int"}`
- **Errors (captured):**
  - missing X-Tenant-Id: 401 `application/json` {"error": "missing_tenant_scope", "message": "X-Tenant-Id is required \u2014 the gateway sets it from a verified identity envelope"}
  - missing X-Principal-Id: 401 `application/json` {"error": "envelope_incomplete", "detail": "canonical input contract violated: actor_subject_id", "service": "hris-connector-svc", "violations": [{"field": "actor_subject_id", "header": "X-Principal-Id", "reason": "manda
  - invalid body: 400 `application/json` {"error": "legal_entity_id and provider_name/provider are required"}
  - authorization denied: 403 `application/json` {"error": "authorization denied"}
  - unknown id: 404 `application/json` {"error": "HRIS integration not found"}
- **Frontend fixes:**

  | Frontend location | Current call | Correct call | Notes |
  |---|---|---|---|
  | `lib/api/integration.ts` | HRIS calls via proxy integration/hris | POST/GET /v1/hris/integrations, POST /v1/hris/sync, GET /v1/hris/sync/jobs |  |

## 3. Known backend gaps the UI must handle

- esignature: create/status changes return **503 `esignature_provider_not_configured`** until a real DocuSign/Adobe adapter exists.
- tax-authority: adapter routes return **503 `tax_authority_adapter_not_configured`** while `AUTHORITY_ADAPTER_ENABLED` is off; filings stay `PENDING` (202) with no acknowledgement.
- reporting-orchestration: report runs are recorded `NOT_IMPLEMENTED` (no data sources wired).
- key-management: metadata only — no cryptographic operations exist; do not present it as a KMS.
- carta `/v1/carta/evaluate` needs only a tenant (no principal check, tracker row 84d).
- reconciliation-intelligence: error bodies are `{error}` (not problem+json); list is paged `limit` 1..200 (default 50) + `offset`.
