---
source_id: kb-authorization-workflows
title: Authorization and Workflows & Approvals
domain: Governance Platform
module_id: authorization-workflows
audience: PUBLIC_ANONYMOUS
status: DRAFT_PENDING_REVIEW
owner: owner-product
effective_from: 2026-01-01
expires_at: 2027-12-31
locale: en-US
intent_ids: I02
market_codes: ALL
classification: PUBLIC_APPROVED
pii_state: CLEAR
license: OWNED_PUBLIC
signing_ref: kbctl-dev
---

## Authorization

Authorization answers the question every service asks before it acts: may this person do this, for this entity, now? Each answer is recorded in the decision log. Conditions can only take an action away, never grant one, and privileged roles are held for an independent approver before they grant anything.

## Segregation of duties

Authorization also records which actions may never be held together, such as raising and approving the same payment. A conflicting pair can only be held under a time-boxed exception with a compensating control, decided by someone other than the requester and the person it covers. When access is removed, it ends on the next permission check.

## Workflows & Approvals

Workflows & Approvals runs multi-stage approvals for actions that need more than one person. An approval adds to authorization and never replaces it: an approver must also be authorized, and every step is recorded as evidence.
