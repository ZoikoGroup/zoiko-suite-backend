# **ZOIKOSUITE**

## **Data Model / ERD Pack**

### **Governed Business Operations Intelligence Platform**

**Classification**
 CONFIDENTIAL — INTERNAL STRATEGIC DOCUMENT

**Standard**
Enterprise SaaS · Data Architecture Specification

**Control Targets**
 ISO 27001 · SOC 2 Type II · GDPR · CCPA-aligned posture · jurisdiction-specific residency readiness

**Architecture Style**
 Governance-First · Event-Driven · API-First · Multi-Entity · Multi-Jurisdiction

**Version**
 1.1 — Sovereign Data Model / ERD Pack Refined

**Document**
 04 of 06 in the ZoikoSuite Architecture Series

## **ARCHITECTURE SERIES**

**01** Sovereign Back-End Architecture
 **02** System Architecture Diagram Pack
 **03** Microservices Specification Pack
 **04** Data Model / ERD Pack *(this document)*
 **05** Security Architecture Specification
 **06** Engineering Build Blueprint

# **01 · PURPOSE OF THIS DOCUMENT**

This document defines the **canonical truth model** of ZoikoSuite.

It specifies:

- authoritative business entities

- cross-domain relationship boundaries

- source-truth ownership rules

- key propagation across tenant, legal entity, jurisdiction, and residency scopes

- effective-dated and versioned modeling requirements

- audit and evidence lineage requirements

- immutable ledger and evidential record patterns

- ERD-level structure for core platform domains

- schema-governance constraints required to prevent architectural drift

- residency, sovereignty, and traceability rules required for regulator-grade operation

This document must be read with:

- **Document 01** — Sovereign Back-End Architecture

- **Document 02** — System Architecture Diagram Pack

- **Document 03** — Microservices Specification Pack

If Document 03 defines **which service owns which truth**, this document defines **what that truth must look like structurally**.

This is not a generic schema guide.
 It is the **canonical structural specification of business truth** for ZoikoSuite.

# **02 · DATA MODELING DOCTRINE**

ZoikoSuite data modeling is governed by the same doctrine as the platform itself.

## **2.1 Source Truth Must Be Singular**

Every material object has one authoritative owning service and one authoritative write boundary.

## **2.2 Tenant, Entity, Jurisdiction, and Residency Are Mandatory Context Dimensions**

Every material record must be explicitly traceable to:

- tenant

- legal entity

- jurisdictional applicability

- physical residency policy where relevant

- time of applicability

## **2.3 Effective Dating Is Not Optional**

Tax rules, payroll rules, contracts, policies, delegated authority, organizational structures, residency assignments, and jurisdiction mappings change over time. Historical state must remain explainable against the rule state active at the time of execution.

## **2.4 No Silent Overwrite of Material History**

Material business history must be append-only, versioned, adjustment-based, or tombstoned. Destructive overwrite is prohibited for critical objects.

## **2.5 Evidence Must Be Correlatable**

Transactions, approvals, documents, events, decisions, workflows, and intelligence artifacts must be linkable through stable identifiers and correlation chains.

## **2.6 Derived Stores Are Not Authoritative**

Warehouses, search indexes, caches, analytics projections, and benchmark datasets are derivative. Source domains remain authoritative.

## **2.7 Keys Must Be Stable and Portable**

Every authoritative entity must have durable identifiers suitable for:

- event payloads

- integrations

- audit retrieval

- cross-domain joins

- evidence manifests

- external references where appropriate

## **2.8 Sensitive Data Must Be Classifiable at Schema Level**

PII, payroll, banking, tax, legal, and privileged data must be classifiable at model level, not only in surrounding infrastructure.

## **2.9 Jurisdiction Is More Than Geography**

Jurisdiction may exist at:

- country

- state or province

- tax authority boundary

- labor law boundary

- filing authority boundary

- document-residency boundary

The model must support this hierarchy explicitly.

## **2.10 Data Residency Is a Structural Constraint**

The platform must know not only **which law applies**, but also **where the bytes are allowed to reside**.

This requires:

- explicit residency-region modeling

- residency policy assignment

- document and data-store placement awareness

- conflict handling where jurisdiction and residency obligations differ

## **2.11 Material Objects Do Not Soft-Delete**

Critical objects must not disappear behind silent soft-delete flags. They must use:

- status transitions

- tombstone records

- archival state

- effective end-dating

This is essential for audit defensibility.

## **2.12 Schema Governance Is Mandatory**

With many services and many teams, local schema drift will destroy platform coherence unless canonical structure is governed centrally.

ZoikoSuite therefore requires:

- centralized schema registry

- version-controlled schema definitions

- compatibility rules for event contracts

- controlled schema evolution discipline

# **03 · KEYING STRATEGY**

ZoikoSuite requires a disciplined key strategy across services and stores.

## **3.1 Primary Identifiers**

Every authoritative entity must have:

- internal UUID / ULID primary key

- stable public-safe reference where appropriate

- correlation support for events and evidence

## **3.2 Context Keys**

Every material record must carry, directly or resolvably:

- tenant_id

- legal_entity_id

- jurisdiction_id or jurisdiction chain

- data_residency_policy_id where relevant

- residency_region_id where storage/location controls apply

- created_at

- effective_from / effective_to where applicable

- created_by_principal_id where human-caused

- source_service

## **3.3 Event Linkage Keys**

For event and evidence correlation:

- correlation_id

- causation_id

- workflow_instance_id

- governance_decision_id

- evidence_manifest_id

- document_version_id

- schema_version

## **3.4 External Reference Keys**

Where integrations exist:

- external provider ID

- bank reference

- filing authority reference

- tax authority reference

- HRIS source ID

- e-signature envelope ID

These never replace canonical internal identifiers.

## **3.5 Integrity Keys**

Where evidential integrity matters, records may additionally carry:

- payload hash

- previous hash reference

- snapshot signature reference

- digital signature reference

These strengthen traceability and tamper detection.

# **04 · TOP-LEVEL ENTITY MAP**

The ZoikoSuite data model is organized around these top-level canonical domains:

- Tenant, Entity & Residency Core

- Identity, Access & Authority

- Policy, Jurisdiction & Governance

- Finance

- Payroll

- HR & Workforce

- Legal, Corporate & Contracts

- Tax

- Compliance & Obligations

- Commercial Operations

- Evidence, Events & Audit

- Intelligence & Analytical Projection

- Schema Governance & Interoperability

Each is specified below.

# **05 · TENANT, ENTITY ****&**** RESIDENCY CORE MODEL**

This is the root of enterprise scope and sovereignty.

## **5.1 Core Entities**

### **Tenant**

Represents the customer organization boundary.

**Core Attributes**

- tenant_id

- tenant_code

- legal_name

- trading_name

- status

- default_currency_code

- primary_timezone

- primary_locale

- default_data_residency_policy_id

- created_at

- created_by

- lifecycle_state

### **LegalEntity**

Represents a subsidiary, company, branch, or operational entity.

**Core Attributes**

- legal_entity_id

- tenant_id

- entity_code

- legal_name

- trading_name

- registration_number

- tax_registration_number

- tax_identity_bundle_id

- entity_type

- incorporation_date

- default_currency_code

- fiscal_calendar_id

- parent_legal_entity_id

- entity_status

- primary_jurisdiction_id

- data_residency_policy_id

- created_at

### **EntityHierarchy**

Represents parent-child and reporting relationships.

**Core Attributes**

- hierarchy_id

- tenant_id

- parent_legal_entity_id

- child_legal_entity_id

- relationship_type

- effective_from

- effective_to

### **EntityJurisdictionAssignment**

Represents the jurisdictions applicable to a legal entity.

**Core Attributes**

- assignment_id

- tenant_id  *(added migration 000002 — direct RLS key; replaces correlated subquery policy)*

- legal_entity_id

- jurisdiction_id  *(opaque UUID — no local FK; validated via Jurisdiction Rules Service API)*

- assignment_type

- effective_from

- effective_to

- source_basis

### **DataResidencyPolicy**

Defines where data for a tenant or entity is permitted to physically reside.

**Core Attributes**

- data_residency_policy_id

- tenant_id

- policy_name

- policy_code

- residency_mode

- conflict_resolution_mode

- active_flag

### **ResidencyRegion**

Represents a physical hosting/storage region.

**Core Attributes**

- residency_region_id

- region_code

- region_name

- cloud_provider

- country_code

- sovereign_flag

- active_flag

### **TaxIdentityBundle**

Versioned structural header linking a legal entity to its tax registration window in a jurisdiction.
Actual tax registration numbers and evidence reside in the Tax Service (Q3 resolution).

**Core Attributes**

- tax_identity_bundle_id

- tenant_id  *(added migration 000002 — direct RLS key; replaces correlated subquery policy)*

- legal_entity_id

- jurisdiction_id  *(opaque UUID — no local FK; validated via Jurisdiction Rules Service API)*

- bundle_status

- effective_from

- effective_to

- primary_document_version_id nullable

### **UltimateBeneficialOwner**

Represents the ownership chain up to beneficial owner level.

**Core Attributes**

- ubo_id

- tenant_id

- legal_entity_id

- owner_type

- owner_name

- ownership_percentage

- control_type

- sanctions_screening_status

- effective_from

- effective_to

### **FiscalCalendar**

Represents reporting and close structure.

**Core Attributes**

- fiscal_calendar_id

- tenant_id

- calendar_name

- calendar_type

- start_month

- close_policy

- active_flag

## **5.2 ERD — Tenant, Entity ****&**** Residency Core**

Tenant

 ├──< LegalEntity

 │      ├──< EntityJurisdictionAssignment >── Jurisdiction

 │      ├──< EntityHierarchy (parent/child self-reference)

 │      ├──> FiscalCalendar

 │      ├──> DataResidencyPolicy

 │      ├──> TaxIdentityBundle

 │      └──< UltimateBeneficialOwner

 ├──< FiscalCalendar

 └──< DataResidencyPolicy >── ResidencyRegion

## **5.3 Modeling Rules**

- every material execution object must resolve to one legal entity

- no legal_entity_id may exist without a data_residency_policy_id

- entity hierarchy is effective-dated

- jurisdiction assignment is effective-dated

- residency policy must be enforceable at platform and storage layers

- one entity may have multiple applicable jurisdictions simultaneously

- tax identity evidence must be document-linked and version-preserving

- UBO lineage must support compliance, KYC, and sanctions screening use cases

# **06 · IDENTITY, ACCESS ****&**** AUTHORITY MODEL**

This model governs who may do what, where, and under what delegated authority.

## **6.1 Core Entities**

### **Principal**

- principal_id

- tenant_id

- principal_type

- identity_provider_subject

- email

- display_name

- status

- created_at

### **Role**

- role_id

- tenant_id

- role_name

- role_code

- role_scope_type

- active_flag

### **PermissionBundle**

- permission_bundle_id

- tenant_id

- bundle_name

- bundle_code

- active_flag

### **PrincipalRoleAssignment**

- assignment_id

- principal_id

- role_id

- legal_entity_id nullable

- effective_from

- effective_to

- assigned_by

### **DelegatedAuthority**

- delegated_authority_id

- delegator_principal_id

- delegate_principal_id

- scope_type

- legal_entity_id nullable

- authority_limit_type

- authority_limit_value

- effective_from

- effective_to

- revocation_status

### **SoDRule**

- sod_rule_id

- tenant_id

- domain_code

- action_a

- action_b

- conflict_type

- jurisdiction_id nullable

- active_flag

### **AccessDecisionLog**

- access_decision_log_id

- principal_id

- legal_entity_id

- action_type

- decision_outcome

- decision_basis

- correlation_id

- decided_at

### **SessionContext**

Ephemeral by lifecycle, evidential by obligation. Written once per resolved
session; never mutated. Carries PII-classified signals and must have an
explicit residency assignment.

**Data Classification:** PII — device fingerprints, IP signals, geolocation
derivatives. Subject to jurisdiction-specific retention and residency rules.

- session_context_id  *(ULID — primary key)*

- principal_id

- tenant_id

- legal_entity_id  *(active entity scope for this session)*

- correlation_id  *(per-request propagated identifier)*

- trust_posture  *(`STANDARD` | `ELEVATED` | `MFA_VERIFIED` | `HIGH_RISK` | `BLOCKED`)*

- mfa_verified  *(boolean — point-in-time attestation only)*

- device_trust_score  *(0–100)*

- adaptive_risk_score  *(0–100; sourced from async risk-signal cache — never from live Intelligence Plane call)*

- risk_signal_source  *(e.g. `IP_GEO`, `DEVICE_FINGERPRINT`, `RULES_ENGINE`)*

- envelope_jwt_jti  *(JWT ID of the issued IdentityContextEnvelope — for revocation and audit correlation)*

- issued_at

- expires_at

- invalidated_at  *(nullable — append-only; record never deleted)*

- invalidation_reason  *(nullable — e.g. `LOGOUT`, `ADMIN_REVOKE`, `RISK_ESCALATION`, `DELEGATION_REVOKED`)*

- data_residency_policy_id  *(mandatory — PII fields subject to residency constraints)*

- source_service  *(`identity-context-svc`)*

- schema_version

### **RiskSignalCache**

Asynchronously populated feed of risk signals consumed by the identity
resolution path. Never sourced from a live Intelligence Plane call during
`resolve()`. Decouples the synchronous hot path from eventually-consistent
intelligence services.

- risk_signal_id  *(ULID)*

- principal_id

- tenant_id

- signal_type  *(e.g. `IP_REPUTATION`, `DEVICE_ANOMALY`, `GEO_VELOCITY`, `BEHAVIORAL_SCORE`)*

- signal_value  *(numeric or enum payload)*

- signal_source  *(producing service or rules engine identifier)*

- valid_from

- valid_to  *(TTL-bound; stale signals are superseded not deleted)*

- superseded_by  *(nullable — FK to newer RiskSignalCache record)*

- created_at

## **6.2 ERD — Identity & Authority**

Principal

 ├──< PrincipalRoleAssignment >── Role

 ├──< DelegatedAuthority (delegator)

 ├──< DelegatedAuthority (delegate)

 ├──< AccessDecisionLog

 ├──< SessionContext

 └──< RiskSignalCache

Role

 └──< RolePermissionBundle >── PermissionBundle

Tenant

 ├──< Principal

 ├──< Role

 ├──< PermissionBundle

 └──< SoDRule

SessionContext

 ├──> Principal

 ├──> LegalEntity

 └──> DataResidencyPolicy

RiskSignalCache

 └──> Principal

## **6.3 Modeling Rules**

- access assignments must support entity scope

- delegated authority must be effective-dated

- SoD rules may be domain-specific and jurisdiction-specific

- access decisions are evidence and must not be ephemeral

- denials are as important as grants from an evidential standpoint

- every SessionContext record must carry a data_residency_policy_id — PII-classified fields (device signals, IP derivatives, geolocation) are subject to jurisdiction-specific residency constraints and must be stored in the assigned residency region

- SessionContext records are never deleted — invalidation is recorded via invalidated_at append; the record persists for the full retention period dictated by the applicable data residency policy

- adaptive_risk_score on SessionContext must be sourced from the RiskSignalCache only — never from a synchronous call to any Intelligence Plane or Tier 2/3 service during the resolve() hot path

- RiskSignalCache entries are superseded not deleted — valid_to bounds the signal window; superseded_by links form a traceable signal history

# **07 · POLICY, JURISDICTION ****&**** GOVERNANCE MODEL**

This is the runtime rules foundation of ZoikoSuite.

## **7.1 Core Entities**

### **Policy**

- policy_id

- tenant_id

- policy_code

- policy_name

- policy_domain

- policy_status

- versioning_mode

### **PolicyVersion**

- policy_version_id

- policy_id

- version_number

- effective_from

- effective_to

- policy_payload

- activation_status

- activated_by

- activated_at

### **Jurisdiction**

- jurisdiction_id

- jurisdiction_code

- jurisdiction_name

- jurisdiction_type

- parent_jurisdiction_id

- authority_type

- active_flag

### **JurisdictionRule**

- jurisdiction_rule_id

- jurisdiction_id

- rule_domain

- rule_code

- rule_name

- effective_from

- effective_to

- rule_payload

- source_reference

- rule_status

- external_feed_reference nullable

- legal_drift_state

### **GovernanceDecision**

- governance_decision_id

- tenant_id

- legal_entity_id

- principal_id

- action_type

- action_subject_type

- action_subject_id

- policy_version_id nullable

- jurisdiction_rule_basis

- authorization_outcome

- workflow_instance_id nullable

- correlation_id

- decision_timestamp

### **EvidenceRequirement**

- evidence_requirement_id

- tenant_id

- domain_code

- action_type

- evidence_type

- requirement_payload

- effective_from

- effective_to

### **TaxLogicSnapshot**

Immutable point-in-time record of tax/rule basis used at execution.

**Core Attributes**

- tax_logic_snapshot_id

- legal_entity_id

- jurisdiction_id

- rule_source_type

- source_rule_id

- source_version

- snapshot_payload

- created_at

- correlation_id

## **7.2 ERD — Governance Model**

Policy

 └──< PolicyVersion

Jurisdiction

 └──< JurisdictionRule

        └──< TaxLogicSnapshot

GovernanceDecision

 ├──> Tenant

 ├──> LegalEntity

 ├──> Principal

 ├──> PolicyVersion

 └──> WorkflowInstance

EvidenceRequirement

 └── scoped by domain/action/effective date

## **7.3 Modeling Rules**

- policies are versioned, never overwritten

- jurisdiction rules are effective-dated

- governance decisions must store basis, not just outcome

- tax/rule basis snapshots should be storable for downstream financial and payroll explainability

- legal drift indicators must not overwrite prior rule state

# **08 · FINANCE DOMAIN MODEL**

Finance is a source-truth domain and must be modeled with strong consistency, audit discipline, and scalable read patterns.

## **8.1 Core Entities**

### **Account**

- account_id

- legal_entity_id

- chart_code

- account_code

- account_name

- account_type

- currency_mode

- active_flag

### **FiscalPeriod**

- fiscal_period_id

- fiscal_calendar_id

- period_name

- period_start

- period_end

- close_status

- close_locked_at

### **JournalEntry**

- journal_entry_id

- legal_entity_id

- fiscal_period_id

- journal_number

- journal_type

- posting_state

- source_event_id nullable

- governance_decision_id

- created_at

- posted_at

- posted_by

### **JournalLine**

- journal_line_id

- journal_entry_id

- account_id

- line_number

- debit_amount

- credit_amount

- currency_code

- fx_rate

- tax_code nullable

- tax_logic_snapshot_id

- memo

### **Invoice**

- invoice_id

- legal_entity_id

- counterparty_id

- invoice_type

- invoice_number

- invoice_date

- due_date

- invoice_status

- total_amount

- currency_code

- source_contract_id nullable

### **Payment**

- payment_id

- legal_entity_id

- payment_type

- payment_status

- payment_reference

- bank_account_id

- counterparty_id

- amount

- currency_code

- requested_at

- settled_at

### **BankAccount**

- bank_account_id

- legal_entity_id

- account_name

- masked_account_number

- bank_identifier

- currency_code

- account_status

### **ReconciliationRecord**

- reconciliation_record_id

- legal_entity_id

- bank_account_id

- statement_date

- reconciliation_status

- matched_amount

- unmatched_amount

### **IntercompanyEntry**

- intercompany_entry_id

- source_legal_entity_id

- target_legal_entity_id

- source_journal_entry_id

- target_journal_entry_id nullable

- match_status

- amount

- currency_code

### **BalanceSnapshot**

Pre-computed signed balance view for performance and audit confidence.

**Core Attributes**

- balance_snapshot_id

- legal_entity_id

- fiscal_period_id

- snapshot_type

- snapshot_date

- account_id nullable

- balance_amount

- currency_code

- snapshot_signature

- generated_at

## **8.2 ERD — Finance**

LegalEntity

 ├──< Account

 ├──< BankAccount

 ├──< Invoice

 ├──< Payment

 ├──< ReconciliationRecord

 ├──< BalanceSnapshot

 └──< JournalEntry

         ├──< JournalLine >── Account

         │      └──> TaxLogicSnapshot

         └──> FiscalPeriod

JournalEntry

 └──< IntercompanyEntry >── LegalEntity (target)

Invoice

 ├──> Counterparty

 └──> Contract (optional source)

## **8.3 Modeling Rules**

- journal entry and line are append-safe

- posting states must support pending, validated, finalized

- every tax-sensitive posting should be traceable to a tax logic snapshot

- balance snapshots are derived but signed for integrity and performance efficiency

- derived consolidation must not overwrite base journal truth

# **09 · PAYROLL DOMAIN MODEL**

Payroll must preserve calculation lineage, snapshot integrity, and mathematical explainability.

## **9.1 Core Entities**

### **PayrollCycle**

- payroll_cycle_id

- legal_entity_id

- payroll_frequency

- cycle_start

- cycle_end

- pay_date

- cycle_status

### **PayrollRun**

- payroll_run_id

- legal_entity_id

- payroll_cycle_id

- run_number

- run_status

- snapshot_hash

- finalized_at

- governance_decision_id

### **PayrollResult**

- payroll_result_id

- payroll_run_id

- employee_id

- gross_pay

- net_pay

- employer_cost

- currency_code

- result_status

### **PayrollTaxCalculation**

- payroll_tax_calculation_id

- payroll_result_id

- jurisdiction_rule_id

- tax_engine_source

- tax_type

- taxable_amount

- tax_amount

- calculation_basis_payload

### **BenefitEnrollment**

- benefit_enrollment_id

- employee_id

- benefit_plan_id

- enrollment_status

- effective_from

- effective_to

### **DeductionRecord**

- deduction_record_id

- payroll_result_id

- deduction_type

- amount

- source_reference

### **GrossToNetCalculationLog**

Explainability record for payroll math.

**Core Attributes**

- gross_to_net_log_id

- payroll_result_id

- calculation_sequence

- step_type

- step_label

- input_payload

- output_payload

- formula_expression

- engine_reference

- created_at

## **9.2 ERD — Payroll**

LegalEntity

 └──< PayrollCycle

        └──< PayrollRun

               └──< PayrollResult >── Employee

                        ├──< PayrollTaxCalculation

                        ├──< DeductionRecord

                        └──< GrossToNetCalculationLog

Employee

 └──< BenefitEnrollment >── BenefitPlan

## **9.3 Modeling Rules**

- payroll runs must be immutable after finalization

- payroll calculations must preserve jurisdiction and engine basis

- gross-to-net math must be explainable from stored records, not only code

- retroactive changes generate adjustments, not destructive rewrite

- payroll snapshots must be reproducible for regulator and audit review

# **10 · HR ****&**** WORKFORCE MODEL**

This domain owns workforce core truth and lifecycle state.

## **10.1 Core Entities**

### **Employee**

- employee_id

- legal_entity_id

- employee_number

- principal_id nullable

- employment_status

- hire_date

- termination_date nullable

- worker_type

- primary_jurisdiction_id

- department_id

- position_id

### **EmploymentContract**

- employment_contract_id

- employee_id

- contract_version_number

- contract_status

- contract_type

- effective_from

- effective_to

- compensation_reference

- signed_document_version_id nullable

### **Department**

- department_id

- legal_entity_id

- department_code

- department_name

- parent_department_id nullable

### **Position**

- position_id

- legal_entity_id

- position_code

- position_title

- job_family

- active_flag

### **LeaveRequest**

- leave_request_id

- employee_id

- leave_type

- leave_start

- leave_end

- leave_status

- approver_principal_id

### **PerformanceReview**

- performance_review_id

- employee_id

- review_cycle

- review_status

- reviewer_principal_id

- completed_at nullable

### **TerminationCase**

- termination_case_id

- employee_id

- termination_reason

- notice_rule_basis

- case_status

- initiated_at

- effective_termination_date

## **10.2 ERD — HR ****&**** Workforce**

LegalEntity

 ├──< Employee

 ├──< Department

 └──< Position

Employee

 ├──< EmploymentContract

 ├──< LeaveRequest

 ├──< PerformanceReview

 └──< TerminationCase

Employee

 ├──> Department

 └──> Position

## **10.3 Modeling Rules**

- employee is authoritative workforce identity

- employment contracts are versioned

- termination must preserve legal basis and evidence linkage

- sensitive workforce fields require field-level classification and restricted access

# **11 · LEGAL, CORPORATE ****&**** CONTRACTS MODEL**

This domain must support machine-trackable obligations and full lineage.

## **11.1 Core Entities**

### **Contract**

- contract_id

- legal_entity_id

- counterparty_id

- contract_number

- contract_status

- contract_type

- effective_from

- effective_to

- current_version_number

- governing_jurisdiction_id

### **ContractVersion**

- contract_version_id

- contract_id

- version_number

- version_status

- drafted_at

- approved_at nullable

- executed_at nullable

- document_version_id

### **ContractClause**

- contract_clause_id

- contract_version_id

- clause_code

- clause_type

- clause_text_hash

- clause_metadata

### **ContractObligation**

- contract_obligation_id

- contract_id

- contract_version_id

- contract_clause_id

- obligation_type

- due_logic

- obligation_status

- responsible_role_code

- legal_entity_id

### **Resolution**

- resolution_id

- legal_entity_id

- resolution_type

- resolution_number

- resolution_status

- effective_date

- document_version_id

### **CorporateAction**

- corporate_action_id

- legal_entity_id

- action_type

- action_status

- filed_at nullable

- filing_reference nullable

### **Counterparty**

- counterparty_id

- tenant_id

- counterparty_type

- legal_name

- registration_number

- risk_status

- validation_status

## **11.2 ERD — Legal ****&**** Contracts**

LegalEntity

 ├──< Contract >── Counterparty

 │      └──< ContractVersion

 │              └──< ContractClause

 │

 ├──< Resolution

 └──< CorporateAction

Contract

 └──< ContractObligation

ContractVersion

 └──< ContractObligation

ContractClause

 └──< ContractObligation

## **11.3 Modeling Rules**

- contracts must support version lineage

- obligations must be atomically linked to source clause and version

- legal objects must preserve signatory and approval evidence linkage

- no active contract state without approved governance path

# **12 · TAX MODEL**

This domain must preserve effective-dated rule truth and explainability.

## **12.1 Core Entities**

### **TaxRule**

- tax_rule_id

- jurisdiction_id

- tax_rule_code

- tax_type

- effective_from

- effective_to

- calculation_payload

- source_reference

- status

### **TaxDetermination**

- tax_determination_id

- legal_entity_id

- source_object_type

- source_object_id

- jurisdiction_id

- tax_rule_id

- determination_status

- taxable_basis

- calculated_tax_amount

- calculation_timestamp

### **TaxLiability**

- tax_liability_id

- legal_entity_id

- tax_type

- filing_period_id

- jurisdiction_id

- accrued_amount

- settled_amount

- liability_status

### **TaxReturnDraft**

- tax_return_draft_id

- legal_entity_id

- filing_period_id

- tax_type

- draft_status

- evidence_manifest_id nullable

### **TaxPayment**

- tax_payment_id

- legal_entity_id

- tax_liability_id

- payment_status

- amount

- currency_code

- paid_at nullable

### **NexusRecord**

- nexus_record_id

- legal_entity_id

- jurisdiction_id

- nexus_type

- effective_from

- effective_to

- source_basis

## **12.2 ERD — Tax**

Jurisdiction

 └──< TaxRule

LegalEntity

 ├──< TaxDetermination

 ├──< TaxLiability

 ├──< TaxReturnDraft

 ├──< TaxPayment

 └──< NexusRecord

TaxLiability

 └──< TaxPayment

TaxRule

 └──< TaxDetermination

## **12.3 Modeling Rules**

- tax rules are effective-dated

- determinations must retain exact rule basis

- liabilities and returns are entity-scoped and jurisdiction-scoped

- filing drafts should link to evidence sets where required

# **13 · COMPLIANCE ****&**** OBLIGATIONS MODEL**

This is the governed accountability model.

## **13.1 Core Entities**

### **Obligation**

- obligation_id

- legal_entity_id

- jurisdiction_id

- obligation_source_type

- obligation_source_id

- obligation_code

- obligation_type

- obligation_status

- due_date

- severity_level

- responsible_function

- source_reference

### **FilingRequirement**

- filing_requirement_id

- obligation_id

- filing_type

- filing_authority

- submission_channel

- filing_status

### **ComplianceStatus**

- compliance_status_id

- legal_entity_id

- domain_code

- status_code

- status_reason

- evaluated_at

- evidence_sufficiency_state

### **ExceptionCase**

- exception_case_id

- legal_entity_id

- exception_type

- severity_level

- linked_object_type

- linked_object_id

- case_status

- escalated_at nullable

### **EscalationRecord**

- escalation_record_id

- exception_case_id

- escalated_to_role

- escalation_reason

- escalation_status

- escalated_at

## **13.2 ERD — Compliance**

LegalEntity

 ├──< Obligation

 ├──< ComplianceStatus

 └──< ExceptionCase

        └──< EscalationRecord

Obligation

 └──< FilingRequirement

## **13.3 Modeling Rules**

- obligation must always resolve to source origin

- obligation must be entity-bound and jurisdiction-bound

- compliance status must be explainable

- exceptions and escalations must preserve timeline integrity

# **14 · COMMERCIAL OPERATIONS MODEL**

Commercial operations must align spend, approvals, and due diligence.

## **14.1 Core Entities**

### **VendorProfile**

- vendor_profile_id

- legal_entity_id

- counterparty_id

- vendor_status

- due_diligence_status

- risk_rating

- approved_at nullable

### **PurchaseRequest**

- purchase_request_id

- legal_entity_id

- requester_principal_id

- request_status

- spend_category

- requested_amount

- currency_code

- justification_text

### **PurchaseOrder**

- purchase_order_id

- legal_entity_id

- vendor_profile_id

- purchase_request_id nullable

- po_number

- po_status

- total_amount

- currency_code

### **InvoiceApproval**

- invoice_approval_id

- invoice_id

- workflow_instance_id

- approval_status

- approved_at nullable

### **SpendThresholdConsumption**

- spend_threshold_consumption_id

- legal_entity_id

- policy_version_id

- threshold_type

- consumed_amount

- period_reference

## **14.2 ERD — Commercial Ops**

LegalEntity

 ├──< VendorProfile >── Counterparty

 ├──< PurchaseRequest

 ├──< PurchaseOrder >── VendorProfile

 └──< SpendThresholdConsumption

Invoice

 └──< InvoiceApproval >── WorkflowInstance

PurchaseRequest

 └──< PurchaseOrder

## **14.3 Modeling Rules**

- commercial spend must resolve to policy and workflow evidence

- vendor state must link to due-diligence evidence

- invoice approval is workflow-backed, not a boolean-only field

# **15 · EVIDENCE, EVENTS ****&**** AUDIT MODEL**

This is a core moat layer and must withstand Big Four and regulator scrutiny.

## **15.1 Core Entities**

### **AuditEvent**

- audit_event_id

- tenant_id

- legal_entity_id

- event_name

- event_version

- source_service

- actor_principal_id nullable

- correlation_id

- causation_id nullable

- event_timestamp

- payload_hash

- previous_event_hash nullable

- payload_reference

### **WorkflowInstance**

- workflow_instance_id

- tenant_id

- legal_entity_id

- workflow_type

- workflow_status

- initiated_by

- started_at

- completed_at nullable

### **WorkflowTransition**

- workflow_transition_id

- workflow_instance_id

- from_state

- to_state

- acted_by

- acted_at

- rationale nullable

### **Document**

- document_id

- tenant_id

- legal_entity_id nullable

- document_type

- storage_uri

- retention_policy_code

- residency_policy_code

- created_at

### **DocumentVersion**

- document_version_id

- document_id

- version_number

- checksum_hash

- uploaded_by

- uploaded_at

- supersedes_document_version_id nullable

- virus_scan_status

- digital_signature_id nullable

### **EvidenceManifest**

- evidence_manifest_id

- tenant_id

- legal_entity_id

- manifest_type

- manifest_status

- generated_at

- generated_by

- scenario_reference

### **EvidenceManifestItem**

- evidence_manifest_item_id

- evidence_manifest_id

- item_type

- item_reference_id

- source_service

- integrity_hash

## **15.2 ERD — Evidence ****&**** Audit**

Tenant

 ├──< AuditEvent

 ├──< WorkflowInstance

 ├──< Document

 └──< EvidenceManifest

WorkflowInstance

 └──< WorkflowTransition

Document

 └──< DocumentVersion

EvidenceManifest

 └──< EvidenceManifestItem

## **15.3 Modeling Rules**

- audit events are append-only

- workflow transitions are full-state history

- document versions must preserve lineage

- evidence manifests are structured, generated evidence sets

- evidential records must be retrieval-ready, not merely archived

## **15.4 Hash-Chain Integrity Rule**

Every audit event should support a previous_event_hash reference to create a tamper-evident integrity chain.

This is not a marketing blockchain claim.
 It is a regulator-grade integrity control.

## **15.5 Document Integrity Rule**

Critical document versions should support:

- checksum validation

- malware / virus scanning status

- optional digital signature linkage

- residency-policy binding

- access lineage through evidential records

# **16 · INTELLIGENCE ****&**** ANALYTICAL PROJECTION MODEL**

These are read-derived models and must not claim source truth.

## **16.1 Core Entities**

### **AnomalyCase**

- anomaly_case_id

- tenant_id

- legal_entity_id

- anomaly_type

- source_object_type

- source_object_id

- model_reference

- confidence_score

- anomaly_status

- detected_at

### **ForecastSnapshot**

- forecast_snapshot_id

- tenant_id

- legal_entity_id

- forecast_type

- forecast_period

- model_reference

- generated_at

- scenario_label

### **ComplianceRiskScore**

- compliance_risk_score_id

- legal_entity_id

- obligation_id nullable

- risk_score

- scoring_model_reference

- scored_at

### **ReconciliationSuggestion**

- reconciliation_suggestion_id

- legal_entity_id

- source_record_a

- source_record_b

- confidence_score

- suggestion_status

- generated_at

## **16.2 Modeling Rules**

- intelligence outputs are explainable artifacts

- intelligence records must preserve model reference and score basis

- intelligence does not mutate source truth

- acted-upon intelligence should remain evidentially linked to human outcomes

## **16.3 Commercial Projection Rule**

Where legally and contractually permitted, anonymized and aggregated derived data may support:

- benchmarking products

- premium reporting

- peer-comparison analytics

- governed data-sharing tiers

This is derivative commercialization, never reuse of source truth in breach of tenant or privacy obligations.

# **17 · SCHEMA GOVERNANCE ****&**** INTEROPERABILITY MODEL**

This is necessary to prevent service drift across a large platform.

## **17.1 Core Entities / Concepts**

### **SchemaRegistryArtifact**

- schema_registry_artifact_id

- schema_name

- schema_type

- version_number

- compatibility_mode

- owning_service

- registered_at

- active_flag

### **SchemaDependencyMap**

- schema_dependency_map_id

- producer_service

- consumer_service

- schema_registry_artifact_id

- dependency_type

## **17.2 Modeling Rules**

- all event schemas must be centrally registered

- compatibility mode must be declared

- breaking changes require controlled rollout

- local service convenience schemas may exist, but canonical event contracts must remain centrally governed

- schema evolution is an architecture discipline, not an ad hoc team decision

# **18 · CROSS-DOMAIN RELATIONSHIP RULES**

The platform must preserve the following discipline:

## **18.1 Finance to Payroll**

Payroll results may generate ledger impacts, but payroll does not own ledger truth.

## **18.2 HR to Payroll**

Employee and contract truth influence payroll, but payroll results remain payroll-owned snapshots.

## **18.3 Contracts to Obligations**

Contracts may generate obligations, but obligation lifecycle is governed in obligation models.

## **18.4 Tax to Finance / Payroll**

Tax determinations derive from financial or payroll activity, but retain independent rule-basis records.

## **18.5 Workflow to Everything**

Workflow may govern many actions, but does not own the domain object itself.

## **18.6 Evidence to Everything**

Evidence links to all domains but does not replace domain truth.

## **18.7 Residency to Everything Sensitive**

Residency policy is not isolated to storage architecture alone. Sensitive objects must be resolvable to residency policy at model level.

# **19 · EFFECTIVE-DATED MODELING RULES**

The following objects must support effective dating or versioning:

- policy versions

- jurisdiction rules

- delegated authority

- entity hierarchy

- entity-jurisdiction assignment

- data residency assignment

- employment contracts

- compensation

- benefits eligibility

- tax rules

- obligation basis

- organizational structure where reporting changes matter

- tax identity bundles

- UBO structures

For all such objects:

- current state must be queryable

- historical state must be replayable

- future-dated state must be schedulable where business-valid

# **20 · DATA CLASSIFICATION MODEL**

Each material field should be classifiable at schema design level.

## **20.1 Recommended Classification Tiers**

- **Public** — safe for public publication

- **Internal** — non-public, low sensitivity

- **Confidential** — sensitive business data

- **Restricted** — PII, payroll, banking, tax, privilege-sensitive, or secrets-adjacent

## **20.2 Mandatory Restricted Categories**

- national identifiers

- payroll amounts at individual level

- bank account references

- disciplinary records

- termination data

- legal draft artifacts where privileged

- tax identifiers

- credential or token references

- UBO identity data

- regulated compliance evidence where protected

Classification metadata should be accessible to access-control and residency-enforcement layers.

# **21 · AUTHORITATIVE KEY PROPAGATION RULES**

Every material event, evidence record, and derived dataset must preserve:

- tenant_id

- legal_entity_id

- jurisdiction_id or resolvable jurisdiction chain

- data_residency_policy_id where relevant

- source_service

- source_object_type

- source_object_id

- correlation_id

- effective timestamp

Stripping context is architecturally prohibited.

# **22 · DATA INTEGRITY ****&**** CONSISTENCY RULES**

## **22.1 Strong Consistency Required**

For:

- journal posting

- payroll finalization

- authorization outcomes

- workflow transitions

- tenant/entity registry updates

- governance decision logging

- tax logic snapshot attachment to critical financial/payroll records

## **22.2 Eventual Consistency Acceptable**

For:

- search indexes

- analytics projections

- intelligence projections

- benchmarking datasets

- notification delivery states

## **22.3 Compensating Transaction Discipline**

For long-running distributed flows, corrections occur through explicit compensating actions, not hidden rewrite.

## **22.4 Tombstone Rule**

Where a material object must be retired, archived, or logically removed, the model must prefer:

- state transitions

- tombstone records

- effective end-dating
 over soft-delete semantics that erase historical meaning.

# **23 · ERD PRIORITY PACK FOR ENGINEERING**

The first ERDs engineering should implement are:

## **Phase A — The Root**

- Tenant / LegalEntity / Jurisdiction / DataResidencyPolicy / ResidencyRegion

- Principal / Role / DelegatedAuthority

- Policy / PolicyVersion / JurisdictionRule

- GovernanceDecision / WorkflowInstance / AuditEvent

## **Phase B — The Movement**

- JournalEntry / JournalLine / TaxLogicSnapshot / BalanceSnapshot

- PayrollResult / PayrollTaxCalculation / GrossToNetCalculationLog

- Contract / ContractVersion / ContractClause

## **Phase C — The Proof**

- AuditEvent / Document / DocumentVersion

- EvidenceManifest / EvidenceManifestItem

- WorkflowTransition

## **Phase D — The Intelligence**

- AnomalyCase

- ForecastSnapshot

- ComplianceRiskScore

- ReconciliationSuggestion

This sequencing protects the truth architecture before optimization layers proliferate.

# **24 · FINAL DATA MODEL DOCTRINE**

ZoikoSuite data is not organized around screens, forms, or convenience tables.

It is organized around:

- truth ownership

- legal entity context

- jurisdictional applicability

- residency constraint

- governance dependency

- evidential consequence

- historical explainability

- structural interoperability

Every canonical object must answer six questions clearly:

- Who owns this truth?

- Which tenant and entity does it belong to?

- Which jurisdictional and residency rules affect it?

- How does its historical state remain explainable?

- Which evidence and events must link to it?

- Can it survive regulator, auditor, and forensic review without ambiguity?

If a data model cannot answer those questions clearly, it is not yet fit for ZoikoSuite.

# **CTO ASSESSMENT**

This Data Model / ERD Pack brings ZoikoSuite to a genuine **distributed truth architecture standard** because it:

- translates service boundaries into authoritative truth models

- strengthens residency and sovereignty controls at model level

- preserves atomic traceability between action, rule, and evidence

- supports Big Four audit and regulator-grade explainability

- adds high-scale structures such as balance snapshots and calculation logs

- prevents schema drift through centralized contract governance

- creates a foundation for future monetization through governed analytical derivatives

This is where ZoikoSuite becomes not only service-oriented and truth-disciplined, but **forensically defensible**.