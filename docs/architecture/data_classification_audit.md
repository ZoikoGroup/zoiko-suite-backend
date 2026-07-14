# Data Classification Audit & Applied Baseline Map

Per the mandate of **docs/architecture/04-data-model.md §20**, this document establishes the taxonomy, audit results, and enforcement design patterns for the ZoikoSuite data classification model.

---

## 1. Classification Tiers (§20.1)

Every data entity in ZoikoSuite is bound to one of the following classification tiers:

| Tier | Sensitivity | Example Platform Context | Enforcement Action |
| :--- | :--- | :--- | :--- |
| **PUBLIC** | Publicly safe | Regulatory public filing references, active countries/regions, general public schemas. | No restriction on egress or indexing. |
| **INTERNAL** | Low sensitivity, non-public | Tenant structures, internal routing configurations, metadata definitions. | Requires valid tenant session context. |
| **CONFIDENTIAL** | Sensitive business data | Approval limits, workflow stages, commercial contracts without PII, policies. | Access restricted via RBAC + Tenant isolation. |
| **RESTRICTED** | Regulator-grade / PII / Secrets | Personal data (email, name), tax identifiers, secret paths, payroll details, roles/permissions. | Fields require field-level access control, encryption in transit/rest, and residency pinning. |

---

## 2. Platform Audit: RESTRICTED Columns & Fields (§20.2)

An audit of all 13 services' schemas identifies the following tables and fields carrying **RESTRICTED** classification:

### 2.1 Identity Plane (`identity-context-svc`)
- **`principals` table**:
  - `email` (PII - email address)
  - `display_name` (PII - name)
  - `identity_provider_subject` (PII - authentication identifier)
- **`delegated_authorities` table**:
  - `authority_limit_value` (can contain sensitive financial thresholds or privilege ranges)

### 2.2 Tenant & Entity Plane (`tenant-entity-registry-svc`)
- **`tax_identity_bundles` table**:
  - Entire table represents the link to tax identifiers (RESTRICTED).
- **`legal_entities` table**:
  - `registration_number` (corporate tax identifier / legal registration)

### 2.3 Evidence Plane (`audit-event-store-svc`)
- **`audit_events` table**:
  - `payload` JSONB: Downstream consumer of event payloads which may carry RESTRICTED fields (e.g., individual payroll figures, banking detail updates, user PII).

### 2.4 Security Plane (`secret-vault-integration-svc`)
- **`secret_policies` table**:
  - `secret_path` (contains credential/token paths referencing master secrets)
- **`secret_leases` table**:
  - `secret_path` (path of the leased credentials)
- **`secret_access_audit_log` table**:
  - `secret_path` (path metadata of brokered secrets)

### 2.5 Governance Plane (`authorization-svc`)
- **`principal_role_assignments` table**:
  - `principal_id` + `role_id` (privilege-sensitive relationship mapping)
- **`delegated_authorities` table**:
  - `delegator_principal_id` + `delegate_principal_id` + limits (privilege delegation)
- **`access_decision_log` table**:
  - `decision_outcome` + `decision_basis` (security evaluation details)

### 2.6 Work Management Plane (`workflow-svc`)
- **`workflow_stages` table**:
  - `approver_principal_id` + `rationale` (potential sensitive payroll/spend comments or actor identifiers)

### 2.7 Configuration Plane (`configuration-feature-flag-svc`)
- **`config_entries` table**:
  - `value` JSONB (could carry secrets-adjacent configs, credential endpoints, or tokens)

---

## 3. Metadata Consumption Design Note (§20.2)

To ensure this classification model controls runtime data flows, the Document Vault Service and other future services must adhere to the following enforcement rules:

### 3.1 Access-Control Enforcement
1. **Field-Level Masking**: API gateways or egress layers handling `RESTRICTED` metadata fields must automatically mask them unless the requesting principal carries a specific `RESTRICTED_READ` permission bundle.
2. **Audit Logging**: Any access (Read or Write) to a record where `data_classification = 'RESTRICTED'` must trigger a high-integrity event publish to the `audit-event-store-svc` containing the principal, tenant, entity, and correlation ID.

### 3.2 Residency-Enforcement (GTRM)
1. **Regional Pinning**: Data with `RESTRICTED` classification (e.g. documents in `zoiko-restricted` or `zoiko-confidential` S3 buckets) must have their physical storage pinned to the residency region of the tenant's legal entity.
2. **Routing Isolation**: The Global Traffic & Residency Manager (GTRM) must verify that all ingress/egress requests containing `RESTRICTED` data are routed to regional pools (`eu-pool`, `us-pool`, etc.) that align with the residency policy.
3. **Index Splitting**: In Phase 2.x, the search-indexer-svc must route index requests for `RESTRICTED` classifications to regional OpenSearch instances, rather than a shared global instance, to satisfy regional sovereignty laws.
