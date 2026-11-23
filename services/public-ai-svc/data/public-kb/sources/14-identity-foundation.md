---
source_id: kb-identity-foundation
title: Identity, Scope & Foundation services
domain: Identity, Scope & Foundation
module_id: identity-foundation
audience: PUBLIC_ANONYMOUS
status: DRAFT_PENDING_REVIEW
owner: owner-product
effective_from: 2026-01-01
expires_at: 2027-12-31
locale: en-US
intent_ids: I12,I13,I02
market_codes: ALL
classification: PUBLIC_APPROVED
pii_state: CLEAR
license: OWNED_PUBLIC
signing_ref: kbctl-dev
---

## Identity, Scope & Foundation

Identity, Scope & Foundation groups the platform services every other area depends on: Identity Context, Tenants & Entities, Feature Flags & Config, Secret Vault, Gateway Authentication, Notifications, Delegated Authority and Roles & Access.

## Tenants & Entities

Tenants & Entities is the authoritative registry every other service depends on. A tenant scopes data separation across the whole platform, and a legal entity is the scope that authorization decisions are evaluated against.

## Feature Flags & Config

Feature Flags & Config holds session, identity and platform configuration, so behaviour changes are governed settings rather than code changes.

## Secret Vault

Secret Vault is the policy-gated broker in front of secret material. It holds policy, lease and audit metadata, never a secret value: material lives behind a vault backend and is reachable only as a short-lived lease.

## Gateway Authentication

Gateway Authentication is the check every gated request passes before it reaches a service: is the caller who they say they are, may their tenant transact, and does the risk engine object.

## Notifications and Roles & Access

Notifications provides governed delivery of notices for workflows, deadlines, escalations and approvals. Roles & Access is the governed authoring layer for the roles and permission bundles every other service is authorized by. Delegated Authority records who may act for whom.
