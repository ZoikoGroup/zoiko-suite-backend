---
source_id: kb-purchase-requests
title: Purchase Requests
domain: Commercial Ops
module_id: purchase-requests
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

## Purchase Requests

Purchase Requests manages the requisition lifecycle: a request is raised, then approved or rejected before any purchase order is created. Approval happens before commitment, so spending is agreed by the right people before a purchase order exists.

## How a request moves

A requester raises a request describing what is needed and for which entity. The request is checked against spend controls and routed to approvers. Only an approved request can go on to become a purchase order, and a rejected request stops there.

## Controls and evidence

Each approve or reject decision is checked against the approver's permissions, so no one approves a request on their own authority alone. The decision, the person who made it and the time are recorded, giving reviewers a clear history from request to order.

## What depends on your setup

Approval routes, thresholds and who may approve follow each organization's policies and delegated authority. A request that needs more than one approval stage passes through each stage in turn. The workflows available to you depend on market, plan and deployment status.

## Raise a purchase request

A purchase request asks for something to be bought, and is approved or rejected before any purchase order is created.
To raise a purchase request:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Legal, Corporate & Commercial**, then select **Purchase Requests**.
3. Go to **Raise a purchase request**.
4. Enter **What is being requested**, the **Amount** and the **Currency**.
5. Select **Raise request**.
The request then waits for an approver; nobody approves their own request.

## Approve or reject a request

Each approve or reject decision is checked against the approver's permissions and recorded with who decided and when.
To approve or reject a request:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Legal, Corporate & Commercial**, then select **Purchase Requests**.
3. Go to **Approve or reject a request**.
4. Enter the **Purchase request ID**.
5. Approve it, or give a **Reason** and reject it.
