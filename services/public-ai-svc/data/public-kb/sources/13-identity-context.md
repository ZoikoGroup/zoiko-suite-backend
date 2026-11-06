---
source_id: kb-identity-context
title: Identity Context
domain: Identity, Scope & Foundation
module_id: identity-context
audience: PUBLIC_ANONYMOUS
status: DRAFT_PENDING_REVIEW
owner: owner-product
effective_from: 2026-01-01
expires_at: 2027-12-31
locale: en-US
intent_ids: I12,I02
market_codes: ALL
classification: PUBLIC_APPROVED
pii_state: CLEAR
license: OWNED_PUBLIC
signing_ref: kbctl-dev
---

## Identity Context

Identity Context traces who is acting, under which tenant and entity, with what assurance, and with which session. Every request in ZoikoSuite carries this context, so approvals, audit evidence and access decisions always name the person or workload behind them.

## People and workloads

A person starts invited and activates from their link. A workload, such as an integration or automated job, starts active with no password and acts only within the scope it was given.

## Authentication and federation

Authentication and federation shows how principals prove who they are: signing in directly or through a federated identity provider, and whether a second factor is in place. Current identity assurance shows who has a second factor, who would need one, and who is stopped.

## Live sessions

Live sessions show who is signed in and with what assurance. Sessions end when an identity is disabled or merged, and tenant isolation keeps each organization's identities separate.

## How support access and password resets work

Support access is temporary access granted to help an organization with an issue. Support access and password resets each need a second person to approve them, so no one can grant or use support access alone. Privileged changes need a step-up check, and every change records a reason, so each support session can be traced afterwards.
