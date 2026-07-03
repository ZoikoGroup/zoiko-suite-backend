# **ZOIKOSUITE**

## **System Architecture Diagram Pack**

### **Governed Business Operations Intelligence Platform**

**Classification**
 CONFIDENTIAL — INTERNAL STRATEGIC DOCUMENT

**Standard**
Enterprise SaaS · Architecture Blueprint

**Architecture Style**
 Governance-First · Event-Driven · API-First · Multi-Entity · Multi-Jurisdiction

**Version**
 1.1 — Sovereign Architecture Diagram Pack 

**Document**
 02 of 06 in the ZoikoSuite Architecture Series

## **ARCHITECTURE SERIES**

**01** Sovereign Back-End Architecture
 **02** System Architecture Diagram Pack *(this document)*
 **03** Microservices Specification Pack
 **04** Data Model / ERD Pack
 **05** Security Architecture Specification
 **06** Engineering Build Blueprint

# **01 · PURPOSE OF THIS DOCUMENT**

This document translates the **ZoikoSuite Sovereign Back-End Architecture** into visual system topology.

It is the visual architecture counterpart to Document 01 and defines how the platform must be understood, communicated, and implemented across engineering, infrastructure, architecture, security, and enterprise stakeholder audiences.

The diagrams in this pack enable engineering teams, enterprise architects, infrastructure teams, institutional investors, and enterprise buyers to understand the platform at the correct level of depth:

- How the platform is structured — from client channels to the intelligence plane

- Where governance lives in the execution path — and why it cannot be bypassed

- How domain services are bounded and how they interact

- How data flows across the platform and which stores own which truth

- How events and evidence propagate through the system

- How identity, access, and entity context are enforced

- How infrastructure supports the platform at the Kubernetes, data, and cloud layers

This pack is not illustrative.

It defines the **actual system topology** engineering teams are expected to implement.

Each diagram is accompanied by **Diagram Notes** that clarify:

- architectural intent

- control assumptions

- non-negotiable constraints

- implementation consequences

# **02 · DIAGRAM INDEX**

This pack contains eight architecture diagrams. Each communicates a distinct layer of the ZoikoSuite platform. Together, they form the complete visual blueprint for engineering, infrastructure, and enterprise architecture teams.

| **#** | **Diagram Title** | **Purpose** |
| --- | --- | --- |
| 1 | Platform Macro Architecture | Complete platform stack — client channels through intelligence plane |
| 2 | Governance Spine Architecture | Non-bypassable control layer between request and execution |
| 3 | Domain Services Architecture | Bounded domain topology across all operational domains |
| 4 | Event Architecture | Event-driven backbone for propagation, evidence, and intelligence |
| 5 | Data Architecture | Polyglot data model, routing, storage purpose, and source-truth rules |
| 6 | Identity & Access Architecture | Zero-trust authentication, authorization, and entity scoping |
| 7 | Evidence Architecture | Defensible evidence system — types, sources, properties, and stores |
| 8 | Infrastructure & Cloud Topology | Cloud-native deployment topology and supported isolation models |

**Implementation Note**
 This document defines actual platform topology. It is not conceptual artwork. Engineering teams are expected to implement in conformance with these architectural views.

# **DIAGRAM 1 · PLATFORM MACRO ARCHITECTURE**

## **Complete system stack — from client channel to governed intelligence**

CLIENTS & CHANNELS

Web Application · Mobile App · Admin Console · API Clients · Integration Partners

        ▼

API GATEWAY / EDGE LAYER

Auth Enforcement · Rate Limiting · Schema Validation · Tenant Context Propagation · Distributed Tracing

        ▼

IDENTITY & SESSION LAYER

SSO / SAML 2.0 · OAuth 2.0 / OIDC · MFA · Session Trust · Delegated Authority Resolution

        ▼

⬡ GOVERNANCE CONTROL PLANE ⬡

Policy Engine

Jurisdiction Engine

Authorization Engine

Workflow & Approvals Engine

Obligations Engine

Evidence Requirements Engine

Decision Log

Non-bypassable — every request passes through this layer

        ▼

DOMAIN EXECUTION SERVICES

Finance

Payroll

HR & Workforce

Legal & Contracts

Tax

Compliance

Commercial Ops

        ▼

EVENT BACKBONE

Kafka / MSK — Durable · Ordered · Append-Only · Entity-Scoped · Jurisdiction-Aware

        ▼

EVIDENCE SYSTEM

Audit Event Store

Document Vault

Policy Decision Log

Workflow History

        ▼

DATA INFRASTRUCTURE

PostgreSQL / Aurora

Immutable Ledger Store

Redis / Valkey Cache

Object Storage

OpenSearch / Elasticsearch

        ▼

ANALYTICS WAREHOUSE

Snowflake · BigQuery · Redshift

Board & Executive Reporting

Cross-Domain Analytics

        ▼

INTELLIGENCE PLANE

Forecasting

Risk Detection

Compliance Gap Analysis

Anomaly Detection

Decision Support

### **Diagram Notes**

- Every client and machine channel enters through the API Gateway. There is no uncontrolled ingress path.

- The Governance Control Plane sits between identity resolution and domain execution. This is the principal structural distinction of ZoikoSuite.

- The Event Backbone is the propagation and decoupling layer. Domain services emit facts; downstream consumers derive state without mutating source truth.

- Evidence is generated as a natural product of operations, not as a retrospective audit workflow.

- The Intelligence Plane is downstream of governed events and evidence. It never bypasses the Governance Plane.

# **DIAGRAM 2 · GOVERNANCE SPINE ARCHITECTURE**

## **The non-bypassable control layer between every request and every material action**

CLIENT REQUEST

Web · Mobile · API · Integration Partner · Scheduled Workflow

        ▼

API GATEWAY / EDGE LAYER

        ▼

IDENTITY RESOLUTION

Principal · Tenant · Legal Entity · Role Profile · Delegated Authority · Session Trust Context

        ▼

⬡ GOVERNANCE CONTROL PLANE ⬡

NON-BYPASSABLE — every material action is evaluated here before execution

01  Policy Engine

    Evaluates business, financial, legal, and internal control policies

    Approval thresholds · Spend limits · Signatory matrices · SoD rules

02  Jurisdiction Engine

    Determines regional or national rule sets

    Payroll law · Tax rules · Termination constraints · Filing deadlines

03  Authorization Engine

    Resolves whether the principal may act in the given tenant, entity, and workflow state

04  Workflow & Approval Engine

    Converts governed actions into structured approval chains where auto-execution is not permitted

05  Obligations Engine

    Maintains active statutory and policy obligations in real time

06  Evidence Requirements Engine

    Determines what supporting evidence must exist before an action may proceed

07  Decision Logging Layer

    Records every governance decision as immutable evidence

    Subject · Actor · Rule set · Outcome · Timestamp · Approval reference

        ▼

✓ AUTHORIZED

Action proceeds to domain service

✗ BLOCKED / ESCALATED

Denied, routed for approval, or escalated

        ▼

DOMAIN SERVICE EXECUTES

        ▼

DOMAIN EVENT EMITTED

        ▼

EVIDENCE GENERATED

### **Diagram Notes**

- The Governance Control Plane is the architectural spine of ZoikoSuite. It is not a module, not an optional feature, and not a policy overlay.

- No UI shortcut, internal API path, batch job, workflow runner, or external integration may circumvent this layer.

- Governance evaluation has only explicit outcomes: authorized, denied, or escalated. Silent bypass and silent failure are prohibited.

- Evidence begins at governance evaluation, not merely after transactional execution.

- This diagram is the single most important visual representation of the ZoikoSuite category thesis.

# **DIAGRAM 3 · DOMAIN SERVICES ARCHITECTURE**

## **Bounded domains with explicit ownership under a unified governance spine**

⬡ ALL DOMAINS OPERATE UNDER THE GOVERNANCE CONTROL PLANE ⬡

        ▼

FINANCE DOMAIN

General Ledger Service

Accounts Payable / Receivable

Treasury & Cash Position

Bank Reconciliation Service

Intercompany Accounting

Consolidation Service

Financial Close Service

PAYROLL DOMAIN

Payroll Run Orchestrator

Compensation Service

Deductions Service

Benefits Service

Payroll Tax Service

Employer Contributions Service

Payroll Exceptions Service

HR & WORKFORCE DOMAIN

Employee Master Service

Onboarding Service

Contract Issuance Service

Leave & Absence Service

Position & Organization Service

Performance Review Service

Offboarding Service

LEGAL & CONTRACTS DOMAIN

Contract Lifecycle Service

Clause & Template Service

Obligation Tracking Service

Board Resolution Service

Corporate Actions Service

Counterparty Management Service

TAX DOMAIN

Tax Rules Service

Tax Determination Service

VAT / GST Engine

Payroll Tax Engine

Corporate Tax Estimation Service

Withholding Tax Service

Filing Preparation Service

COMPLIANCE DOMAIN

Obligations Registry

Deadline Engine

Filing Tracker

Compliance Status Service

Evidence Sufficiency Service

Exception & Escalation Service

COMMERCIAL OPS DOMAIN

Procurement Workflow Service

Purchase Request Service

Purchase Order Service

Invoice Approval Service

Vendor Due Diligence Service

Spend Controls Service

SHARED PLATFORM CAPABILITIES

Event Backbone · Evidence System · Identity Layer · Data Plane · Observability

### **Diagram Notes**

- Each domain is bounded and must have explicit ownership, service contracts, and data responsibility.

- No domain self-authorizes material actions. Governance is externalized into the Governance Plane and enforced consistently.

- Domain interaction should prefer event-driven propagation over tight synchronous coupling.

- Shared platform capabilities are not owned by any single business domain. They are platform engineering responsibilities.

- This structure allows modular capability without compromising unified control.

# **DIAGRAM 4 · EVENT ARCHITECTURE**

## **Event-driven backbone for propagation, evidence capture, and governed intelligence**

EVENT PRODUCERS — DOMAIN SERVICES

Every domain service emits typed, governed events after authorized execution

Examples:

journal.posted

payment.initiated

period.closed

reconciliation.completed

payroll.run.completed

payroll.exception.raised

employee.hired

employee.terminated

contract.executed

obligation.created

tax.liability.updated

filing.submitted

obligation.overdue

compliance.gap.detected

exception.escalated

resolution.approved

        ▼

EVENT BACKBONE

Kafka / MSK — Durable · Ordered · Partitioned by Entity · High-Throughput

Immutable

Events are facts, not commands

Append-Only

The event log is the historical record

Entity-Scoped

Every event carries tenant and legal-entity context

Jurisdiction-Aware

Jurisdiction context propagates with event

        ▼

EVENT CONSUMERS

Evidence System

Captures every event as immutable evidence

Intelligence Plane

Consumes stream for anomaly detection, forecasting, and risk scoring

Analytics Warehouse

Feeds executive reporting and cross-domain analytics

Workflow Engine

Triggers approvals, escalations, and obligation follow-up

Observability System

Tracks platform health, SLA metrics, and business-event rates

### **Diagram Notes**

- Event consumers must never mutate source domain truth.

- Every event payload must carry tenant ID, entity ID, actor identity, correlation ID, and jurisdiction context.

- The Event Backbone enables domain decoupling and governed propagation.

- The event log is itself part of the evidence model.

- Domain events are facts after execution, not instructions for upstream mutation.

# **DIAGRAM 5 · DATA ARCHITECTURE**

## **Polyglot, fit-for-purpose data stores with strict routing and source-truth discipline**

**Architectural Principle**
 No single database solves every platform need. Each store exists for a specific purpose, under a specific routing rule, with a specific governance obligation.

### **OPERATIONAL RELATIONAL STORE**

**Technology**
 PostgreSQL / Aurora PostgreSQL-Compatible

**Purpose**
 Primary source of truth for transactional domain data

**Governing Rule**
 Authoritative operational store. Never bypassed by analytics or reporting layers.

**Primary Uses**

- Finance transactions and journal metadata

- HR and payroll records

- Contract and obligation metadata

- Compliance state and exceptions

### **IMMUTABLE LEDGER STORE**

**Technology**
 Append-Only Relational / Event-Sourced / Hash-Chained Ledger

**Purpose**
 Tamper-resistant store for audit-critical financial and governed records

**Governing Rule**
 Committed records may not be overwritten. Corrections occur only through explicit adjustments.

**Primary Uses**

- Journal entry ledger

- Financial close snapshots

- Payroll run archives

- Cryptographic integrity chains

### **DOCUMENT ****&**** EVIDENCE STORE**

**Technology**
 Object Storage + Relational Metadata

**Purpose**
 Version-controlled storage for contracts, resolutions, filings, payslips, and evidential artifacts

**Governing Rule**
 Residency, retention, integrity, and access lineage must be policy-enforced.

**Primary Uses**

- Contract documents with version lineage

- Board resolution archives

- Filing artifacts

- Tamper-detection hashes

### **SEARCH LAYER**

**Technology**
 OpenSearch / Elasticsearch

**Purpose**
 Fast search and retrieval across documents, evidence, obligations, and legal artifacts

**Governing Rule**
 Read-only derivative. Never a source of operational truth.

**Primary Uses**

- Audit retrieval

- Contract clause search

- Obligation lookup

- Evidence discovery

### **CACHE LAYER**

**Technology**
 Redis / Valkey

**Purpose**
 Session acceleration, hot rule caching, and non-authoritative performance optimization

**Governing Rule**
 Never authoritative. All business decisions validate against source stores.

**Primary Uses**

- Session and token caching

- Governance-rule hot cache

- API response acceleration

- Rate limiting and quota support

### **ANALYTICS WAREHOUSE**

**Technology**
 Snowflake / BigQuery / Redshift

**Purpose**
 Read-optimized analytical store for executive reporting, forecasting, and cross-domain intelligence

**Governing Rule**
 Strictly read-derived. Analytics may not mutate source operational truth.

**Primary Uses**

- Board and executive reporting

- Multi-entity reporting

- Anomaly and trend analysis

- Compliance gap analytics

### **Diagram Notes**

- Operational truth remains in source domain systems.

- Entity and jurisdiction tags must persist throughout the data model and in all derived stores.

- Sensitive data such as payroll, HR, and PII must be field-classified and access-restricted.

- Historical records must preserve effective-dated rule context, not merely current-state interpretation.

- Data-store specialization is architectural, not incidental.

# **DIAGRAM 6 · IDENTITY ****&**** ACCESS ARCHITECTURE**

## **Zero-trust, entity-scoped, deeply modeled access tied to governance at every layer**

IDENTITY PROVIDERS

Enterprise IdP

SSO · SAML 2.0 · LDAP

API & Service Identity

OAuth 2.0 · OIDC · mTLS

Trust Controls

MFA · Device Trust · Session Limits

        ▼

IDENTITY SERVICE — TOKEN VALIDATION & CONTEXT ASSEMBLY

Authenticated Principal · Tenant · Active Legal Entity · Role Profile · Delegated Authority · Session Trust

        ▼

AUTHORIZATION ENGINE

RBAC

Defines broad access roles and functional reach

ABAC

Makes context-sensitive decisions using entity, jurisdiction, workflow state, action type, and risk level

SoD

Enforces segregation-of-duties constraints

Entity Scope

Hard-enforces legal-entity authority boundaries

Action Approval

Triggers explicit approval chains for high-risk actions

        ▼

GOVERNANCE PLANE RECEIVES RESOLVED IDENTITY + AUTHORIZATION CONTEXT

        ▼

ALL ACCESS DECISIONS ARE LOGGED

Successful · Denied · Escalated · Delegated

### **Diagram Notes**

- Access is not a bolt-on. Identity and authorization are deeply modeled platform primitives.

- Zero-trust means every request is authenticated and re-evaluated, regardless of origin.

- SoD enforcement is architectural and runtime-enforced, not policy-only.

- Access decisions are evidential records and must be logged as such.

- Entity scope is mandatory, not optional, for every material action.

# **DIAGRAM 7 · EVIDENCE ARCHITECTURE**

## **Defensible evidence as an architectural output, not an audit overlay**

Most enterprise systems log activity. ZoikoSuite produces defensible evidence. That is a materially higher standard.

EVIDENCE SOURCES

User Actions

Service Events

Workflow Transitions

Document Versioning

External Integrations

Policy Evaluations

AI Recommendations & Overrides

        ▼

EVIDENCE SYSTEM

Audit Event Store

Document Vault

Policy Decision Log

Workflow History Store

Approval Chain Archive

Evidence Manifest

        ▼

EVIDENCE TYPES

Transaction Evidence

Approval Evidence

Document Evidence

Policy Decision Evidence

Workflow Evidence

Compliance Evidence

Audit Log Evidence

### **Required Properties of Every Evidential Record**

- **Timestamped** — precision timestamps with full provenance chain

- **Actor-Bound** — the human or system actor is always identified

- **Entity-Bound** — every record is scoped to legal entity context

- **Jurisdiction-Aware** — rule basis and jurisdictional logic are retained

- **Immutable** — append-only and tamper-resistant by architecture

- **Source-Linked** — correlated to the originating action and event chain

- **Retrievable** — structured for audit, regulator, and legal discovery use

### **Diagram Notes**

- Evidence is generated automatically as part of the governance and execution cycle.

- Every governance decision is itself evidence.

- The Document Vault must preserve version history, approval lineage, access history, and integrity validation.

- Evidence design must support regulator examination, internal compliance review, and legal discovery scenarios.

- Evidence is not merely stored; it is structured for defensibility.

# **DIAGRAM 8 · INFRASTRUCTURE ****&**** CLOUD TOPOLOGY**

## **Cloud-native, Kubernetes-orchestrated, security-first deployment architecture**

INTERNET / CLIENT INGRESS

        ▼

EDGE PROTECTION LAYER

CDN / Global Edge Caching

WAF / OWASP Protection

DDoS Shield / Layer 3, 4, and 7 Controls

        ▼

LOAD BALANCER

SSL Termination · Health Checks · Traffic Distribution

        ▼

KUBERNETES CLUSTER (AWS EKS)

API Gateway Pods

Auth · Rate Limiting · Schema Validation

Governance Services

Policy · Jurisdiction · Authorization · Workflow

Domain Microservices

Finance · HR · Payroll · Legal · Tax · Compliance · Commercial Ops

Platform Services

Evidence · Identity · Observability · Workflow Support

        ▼

MANAGED DATA INFRASTRUCTURE

PostgreSQL / Aurora

Kafka / MSK

Redis / Valkey

S3-Class Object Storage

OpenSearch / Elasticsearch

## **Supported Deployment Modes**

| **Deployment Mode** | **Description** | **Isolation Level** |
| --- | --- | --- |
| Multi-Tenant SaaS | Shared infrastructure with logical tenant isolation | Standard |
| Dedicated Private Cloud | Single-tenant logical isolation on shared substrate | Enhanced |
| Enterprise Single-Tenant | Dedicated infrastructure for compliance-mandated segregation | Highest |
| Sovereign / On-Premise | Customer-controlled deployment for legal or residency constraints | Sovereign Requirement |

### **Diagram Notes**

- AWS is the practical default, but the architecture should remain cloud-portable where commercially sensible.

- All workloads run under Kubernetes orchestration with network policy enforcement, service isolation, and controlled rollout discipline.

- Infrastructure as Code is mandatory. All platform changes must be version-controlled and audit-logged.

- Blue-green and canary patterns are preferred for zero-downtime deployment.

- Schema migrations must be backward-compatible by default.

- Deployment activity is part of the evidence record.

# **FINAL ARCHITECTURAL DOCTRINE**

Every diagram in this pack exists to support one non-negotiable principle:

## **Business operations must execute inside governance.**

Not around it.
 Not after it.
 Not as an audit overlay.

Inside it.

That is the ZoikoSuite architecture.

Each diagram in this pack is a different cross-section of the same system — the same doctrine expressed through topology, control flow, domain structure, event propagation, data routing, identity enforcement, evidence production, and infrastructure deployment.

## **NEXT DOCUMENT IN SERIES**

**Document 03 — Microservices Specification Pack**

This document will define:

- every microservice

- service responsibilities and boundaries

- service APIs and contracts

- event outputs and schemas

- data ownership rules

- scaling and deployment models

It is one of the most important engineering documents in the ZoikoSuite platform series.