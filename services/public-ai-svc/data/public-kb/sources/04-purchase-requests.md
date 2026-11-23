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

A purchase request asks for something to be bought. It authorizes nothing on its own: no order can be placed against it until somebody else approves it.
To raise a purchase request:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Legal, Corporate & Commercial**, then select **Purchase Requests**.
3. Go to **Raise a purchase request**.
4. Enter **What is being requested** (such as a laptop refresh), the **Amount** and the **Currency**.
5. Select **Raise request**. The request lands as a DRAFT.
6. Submit it for approval with **Submit for approval**.
The same form is also on the Commercial Ops page.

## Approve or reject a request

A request must be submitted before it can be decided, and nobody can approve or reject their own request. One decision, either way, and it is final.
To submit, approve, reject or cancel a request:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Legal, Corporate & Commercial**, then select **Purchase Requests**.
3. Go to **Approve or reject a request** and enter the **Purchase request ID**.
4. For a DRAFT request, select **Submit for approval**.
5. To decide it, another person selects **Approve**, or enters a **Reason** and selects **Reject**.
6. To withdraw it instead, enter a **Reason** and select **Cancel request**.
Rejection and cancellation need a reason, which is kept as the record of why.

## Requisition register

The Requisition register lists every request for your organization, newest first, with the value awaiting a decision shown per currency.
To filter the register:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Legal, Corporate & Commercial**, then select **Purchase Requests**.
3. Go to the **Requisition register**.
4. Choose a tab: **All**, **Draft**, **Pending**, **Approved**, **Rejected** or **Cancelled**.
5. Optionally enter a **Legal entity ID (UUID)** and select **Filter by entity**.

## Look up a request

Look up a request reads one request's decision trail: who approved or rejected it, when, and the reason recorded for a rejection.
To look up a request:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Legal, Corporate & Commercial**, then select **Purchase Requests**.
3. Go to **Look up a request**.
4. Enter the **Purchase request ID**, from the register or from an order issued against it.
5. Select **Look up**.
