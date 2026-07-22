# Phase 6 — Trust, Maturity & Intelligence Engine Architecture

## Executive Overview
Phase 6 establishes the enterprise intelligence, adaptive security, continuous compliance, and data trust tier for ZoikoSuite. It introduces 10 dedicated microservices providing real-time anomaly detection, financial/workforce forecasting, organization-wide compliance risk scoring, AI/rule-driven reconciliation intelligence, cross-service reporting orchestration, data migration integrity validation, mTLS certificate management, SIEM event streaming, CARTA dynamic access control, and BYOK/HYOK key management.

---

## Service Architecture & Port Directory

| # | Service Name | Service Folder | Port | Primary Purpose & Key Capabilities |
|---|---|---|---|---|
| 01 | **Anomaly Detection Service** | `services/anomaly-detection-svc` | `8134` | Detects unusual financial transactions, payroll spikes, and operational anomalies using configurable rule engines and statistical models. |
| 02 | **Forecasting Service** | `services/forecasting-svc` | `8135` | Generates predictive financial, payroll, cash flow, and workforce headcount forecasts using historical platform trends. |
| 03 | **Compliance Risk Scoring Service** | `services/compliance-risk-scoring-svc` | `8136` | Computes organizational compliance risk scores based on regulatory framework obligations, policy violations, and audit history. |
| 04 | **Reconciliation Intelligence Service** | `services/reconciliation-intelligence-svc` | `8137` | Performs multi-source transaction matching, identifies unmatched items, and suggests automated resolution actions. |
| 05 | **Reporting Orchestration Service** | `services/reporting-orchestration-svc` | `8138` | Coordinates enterprise report generation, scheduled cron workflows, and cross-service data aggregation across domain bounds. |
| 06 | **Migration Integrity Service** | `services/migration-integrity-svc` | `8139` | Validates data migration batches, performs schema/duplicate/format integrity checks, and logs audit remediation trails. |
| 07 | **mTLS Management Service** | `services/mtls-management-svc` | `8140` | Manages service-to-service mutual TLS certificate lifecycle, automated rotations, trust stores, and network communication policies. |
| 08 | **SIEM Integration Service** | `services/siem-integration-svc` | `8141` | Streams platform audit logs, security alerts, and operational events to external SIEM systems (Splunk, Datadog, Elastic, Sentinel). |
| 09 | **CARTA Service** | `services/carta-svc` | `8142` | Implements Continuous Adaptive Risk and Trust Assessment to dynamically adjust access decisions (`ALLOW`, `STEP_UP_MFA`, `ISOLATE`, `DENY`). |
| 10 | **BYOK/HYOK Key Management Service** | `services/key-management-svc` | `8143` | Integrates Bring Your Own Key (BYOK) & Hold Your Own Key (HYOK) models across external KMS providers (AWS KMS, Azure KV, GCP KMS, Vault). |

---

## Key Technical Standards & Compliance

- **Language & Framework**: Go 1.22 with `go-chi/v5` lightweight router.
- **Tenant Isolation**: Multi-tenant Row-Level Security (RLS) enforcement via PostgreSQL transaction variables (`SET LOCAL app.tenant_id = $1`).
- **Authorization**: Delegates all material action authorization requests to the Governance Plane (`authorization-svc`).
- **Event Streaming**: Async event publishing via Kafka (`segmentio/kafka-go`).
- **Health Checks**: Standardized `/healthz` endpoints and binary health check executables (`cmd/healthcheck`).
- **Testing**: 100% unit test pass rate across all 10 microservices using standard `go test`.

---

## Deployment & Verification

- **Docker Compose Setup**: [deployments/docker-compose.phase6.yml](file:///c:/Users/Dell/Downloads/Audit_Event_Store/zoiko-suite%20project/zoiko-suite/deployments/docker-compose.phase6.yml)
- **Database Initializer**: [deployments/init-db-phase6.sh](file:///c:/Users/Dell/Downloads/Audit_Event_Store/zoiko-suite%20project/zoiko-suite/deployments/init-db-phase6.sh)
- **Postman API Collection**: [docs/postman/ZoikoSuite_Phase6_TrustIntelligence.postman_collection.json](file:///c:/Users/Dell/Downloads/Audit_Event_Store/zoiko-suite%20project/zoiko-suite/docs/postman/ZoikoSuite_Phase6_TrustIntelligence.postman_collection.json)
