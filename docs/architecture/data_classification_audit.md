# Data Classification Audit & Applied Baseline Map

Per the mandate of **docs/architecture/04-data-model.md §20**, this document establishes the taxonomy, audit results, and enforcement design patterns for the ZoikoSuite data classification model across all platform services.

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

## 2. Platform-wide Service Audit (§20.2)

An audit of all platform services (both currently implemented and planned) maps every domain to its data classification profiles:

### 2.1 Identity Plane (`identity-context-svc`)
- **Tiers represented**: RESTRICTED, INTERNAL.
- **RESTRICTED fields**:
  - `principals.email` (PII - email address)
  - `principals.display_name` (PII - name)
  - `principals.identity_provider_subject` (PII - authentication identifier)
- **INTERNAL fields**:
  - `principals.principal_id`, `principals.tenant_id`, `principals.principal_type`

### 2.2 Tenant & Entity Plane (`tenant-entity-registry-svc`)
- **Tiers represented**: RESTRICTED, CONFIDENTIAL, INTERNAL.
- **RESTRICTED fields**:
  - `tax_identity_bundles` table: Holds structural links to regulatory tax registrations (RESTRICTED metadata).
- **CONFIDENTIAL fields**:
  - `data_residency_policies` table: Data residency boundaries and compliance policy codes.
  - `legal_entities.registration_number` (corporate tax identifier / legal registration)
- **INTERNAL fields**:
  - `tenants`, `legal_entities` general metadata, and `entity_hierarchies` structure.

### 2.3 Security Plane (`secret-vault-integration-svc`)
- **Tiers represented**: RESTRICTED, CONFIDENTIAL.
- **RESTRICTED fields**:
  - `secret_policies.secret_path` (contains credential/token paths referencing master secrets)
  - `secret_leases.secret_path` (path of the leased credentials)
  - `secret_access_audit_log.secret_path` (path metadata of brokered secrets)
- **CONFIDENTIAL fields**:
  - `secret_policy_versions.allowed_workload_ids` (identity/workload permission mappings)

### 2.4 Evidence Plane (`audit-event-store-svc`)
- **Tiers represented**: RESTRICTED, CONFIDENTIAL, INTERNAL.
- **RESTRICTED fields**:
  - `audit_events.payload` JSONB: Downstream consumer of event payloads which may carry RESTRICTED fields (e.g. principal context, individual compensation updates, database credential rotations).

### 2.5 Governance Plane (`authorization-svc`)
- **Tiers represented**: RESTRICTED, CONFIDENTIAL.
- **RESTRICTED fields**:
  - `principal_role_assignments` table: principal_id + role_id mappings (privilege-sensitive metadata).
  - `delegated_authorities` table: delegator/delegate relations and limits.
  - `access_decision_log` table: details of access grants/denials and basis.
- **CONFIDENTIAL fields**:
  - `permission_bundles` table: action-type lists and bundles.

### 2.6 Work Management Plane (`workflow-svc`)
- **Tiers represented**: RESTRICTED, CONFIDENTIAL.
- **RESTRICTED fields**:
  - `workflow_stages.approver_principal_id` + `workflow_stages.rationale` (potential sensitive PII or approval comments on payroll/spend).
- **CONFIDENTIAL fields**:
  - `workflow_instances` and `workflow_transitions` (tracking state transitions of corporate approvals).

### 2.7 Configuration Plane (`configuration-feature-flag-svc`)
- **Tiers represented**: RESTRICTED, INTERNAL.
- **RESTRICTED fields**:
  - `config_entries.value` JSONB (could carry credentials or token references if not stored in Vault).
- **INTERNAL fields**:
  - `feature_flags` and environment settings.

### 2.8 Governance Decision Log Service (`governance-decision-log-svc`)
- **Tiers represented**: RESTRICTED, CONFIDENTIAL.
- **RESTRICTED fields**:
  - `governance_decisions.actor_id` (principal identifier mapping to physical actor).
  - `governance_decisions.evaluation_context` JSONB (can contain transaction values or payroll figures).
- **CONFIDENTIAL fields**:
  - `governance_decisions.rule_basis` and metadata.

### 2.9 Obligations Service (`obligations-svc`)
- **Tiers represented**: CONFIDENTIAL, INTERNAL.
- **CONFIDENTIAL fields**:
  - `obligations.source_reference` (details referencing corporate contracts or specific regulatory filing rules).
  - `filing_requirements` table (filing status, authority, and submission channels).

### 2.10 Policy Service (`policy-svc`)
- **Tiers represented**: CONFIDENTIAL, INTERNAL.
- **CONFIDENTIAL fields**:
  - `policy_versions.rule_payload` (contains company-wide spend thresholds and signing matrices).

### 2.11 Jurisdiction Rules Service (`jurisdiction-rules-svc`)
- **Tiers represented**: PUBLIC, INTERNAL.
- **PUBLIC fields**:
  - `jurisdictions` (names, region codes, country codes).
- **INTERNAL fields**:
  - `jurisdiction_rules` (rule domain settings and legislative metadata).

### 2.12 Schema Registry Service (`schema-registry-svc`)
- **Tiers represented**: INTERNAL.
- **INTERNAL fields**:
  - `event_schemas` (event schemas are structural definitions of payload contracts).

### 2.13 API Gateway ForwardAuth Service (`gateway-auth-svc`)
- **Tiers represented**: Stateless / RESTRICTED.
- **RESTRICTED fields**:
  - Exposes no DB, but parses and transiently caches JWT envelopes containing principal IDs, roles, and session trust postures.

### 2.14 Document Vault Service (`document-vault-svc` — Planned)
- **Tiers represented**: RESTRICTED, CONFIDENTIAL, PUBLIC.
- **RESTRICTED fields**:
  - Document payloads containing tax returns, UBO identity verification, payroll summaries, or individual contracts.
- **CONFIDENTIAL fields**:
  - Corporate board resolutions, draft policies, internal workflow documents.

### 2.15 Evidence Manifest Service (`evidence-manifest-svc` — Planned)
- **Tiers represented**: RESTRICTED, CONFIDENTIAL.
- **RESTRICTED fields**:
  - Contains cryptographic hash links and actor/principal details pointing to specific regulatory actions.

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
