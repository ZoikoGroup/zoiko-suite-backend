---
source_id: kb-governance
title: Governance and approvals
domain: Platform
module_id: governance
audience: PUBLIC_ANONYMOUS
status: DRAFT_PENDING_REVIEW
owner: owner-product
effective_from: 2026-01-01
expires_at: 2027-12-31
locale: en-US
intent_ids: I15
---

## Governance and approvals

The governance layer describes policy-aware approvals and delegated authority. The exact enforcement model and available workflows depend on the configured role and product scope. Governance is shared across the platform rather than rebuilt in each area, so the same rules apply to a payment, a contract or a filing.

## How a governed action proceeds

When someone starts a material action, the platform first checks whether that person may take it for that entity and at that workflow step. It then applies the relevant policy, such as approval thresholds, signatory matrices or segregation of duties, and routes the action through any approval stages it needs. Approval workflows add to these permission checks rather than replacing them.

## Approving on behalf of someone else

A person can approve on behalf of someone else only through delegated authority that is time-bound, limited in scope and never wider than the delegator's own authority. Pending approvals can carry deadlines and escalate when they are not acted on in time. If an approver's authority changes, pending approvals that depend on it are checked again.

## Decisions on record

Each governance decision is recorded with its context, the rule it relied on and its outcome. Records can be searched by actor, entity, action, rule basis and time range, and a past decision can be checked against the policy version that applied when it was made.
