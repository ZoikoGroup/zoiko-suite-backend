---
source_id: kb-delegated-authority
title: Delegated Authority
domain: Delegated Authority
module_id: delegated-authority
audience: PUBLIC_ANONYMOUS
status: DRAFT_PENDING_REVIEW
owner: owner-product
effective_from: 2026-01-01
expires_at: 2027-12-31
locale: en-US
intent_ids: I15,I02
---

## Delegated Authority

Delegated Authority is a register of who may act for whom: time-bound, entity-scoped grants of authority. Each grant names the person, the entity it applies to and the period it is valid for, so approvals can be checked against authority that was in force at the time.

## Limits on a grant

A delegate never receives more authority than the person delegating it holds. A grant can also be limited in scope, and it can itself require approval before it takes effect. This keeps every delegated approval within limits the original holder could have used.

## Revocation and expiry

A grant can be revoked, and it lapses when its period ends. When authority is revoked, expires or is suspended, pending approvals that depend on it are checked again and escalated if the approver may no longer approve.

## How the register is used

Approval workflows and permission checks use the register to decide whether a delegate may take an action. The exact enforcement model depends on the configured role and product scope.
