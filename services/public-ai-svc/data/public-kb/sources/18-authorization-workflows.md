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

Authorization is how access is controlled. It answers the question every service asks before it acts: may this person do this, for this entity, now? Each answer is recorded in the decision log. Conditions can only take an action away, never grant one, and privileged roles are held for an independent approver before they grant anything.

## Segregation of duties

Authorization also records which actions may never be held together, such as raising and approving the same payment. A conflicting pair can only be held under a time-boxed exception with a compensating control, decided by someone other than the requester and the person it covers. When access is removed, it ends on the next permission check.

## How a permission check is decided

A permission check applies its layers in order: role grants first; then delegated and elevated authority, which can extend what someone may do; then separation of duties and access conditions, which can only refuse; and finally authority limits and two-step approval, which cap what is allowed.

## Check access and decision reasons

Check access asks exactly what a service asks before it acts, for a person, an action and a legal entity, and the answer is recorded. The decision reason shows which layer allowed or refused a recorded check.

## Grant role

Grant role gives a person a role for a legal entity from a start date.
To grant a role:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Authorization**.
3. Select **Grant role**.
4. Enter the **Person (principal ID)**, the **Role**, what it **Applies to** and the **Legal entity**.
5. Set **Starts** and, if the grant should end, **Ends**, and add an **Approval reference** if you have one.
6. Select **Grant**.
A privileged role is held for an independent approver and grants nothing until approved. Revoking a grant takes effect on the next permission check.

## Elevated access lifecycle

Elevated access covers just-in-time, emergency and support sessions. Each is requested, becomes active, names exact actions, ends on its own, and is then reviewed until someone attests to what was done with it.
To request just-in-time access:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Authorization**.
3. Select **Request JIT access**.
4. Enter the **Actions**, **For (principal ID)**, the **Ticket**, the **Resource**, the **Environment**, the **Hours** needed and a **Reason**.
5. Select **Request**.
Emergency access uses **Break glass** with an incident reference and is reviewed after it ends.

## Workflows & Approvals

Workflows & Approvals runs multi-stage approvals for actions that need more than one person. An approval adds to authorization and never replaces it: an approver must also be authorized, and every step is recorded as evidence. Three rules hold whatever anyone's grants say: the person who started a workflow never decides it, nobody decides two stages of the same workflow, and a service account never decides one. When an approval deadline passes, the workflow expires; when a stage deadline passes, that stage escalates.

## Start a workflow

Start a workflow opens an approval for something that needs one.
To start a workflow:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Workflows & Approvals**.
3. Go to **Start a workflow**.
4. Enter the **Workflow type**, the **Legal entity** and **What is being approved**, such as a purchase order and its ID.
5. Optionally choose who to **Escalate to**.
6. Pick a saved **Route**, or add stages: for each, **Who decides**, the approver or approver pool, the **Approvals needed** and a **Deadline**.
7. Select **Start workflow**.
You cannot approve a workflow you started, and nobody decides two stages of the same workflow.

## Decide the current stage

A workflow stage can be decided by the stage's named approver, their delegate, a holder of the stage's role or the person it was escalated to.
To decide the current stage:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Workflows & Approvals**.
3. Go to **Decide the current stage**.
4. Enter the **Workflow ID** and choose Approve or Reject.
5. Give a **Reason**; a reason is required to reject.
6. Select **Record decision**.
Each decision is checked against authorization and recorded.

## Waiting for a decision

Waiting for a decision lists pending and escalated workflows in your organization. Without permission to list them all, you can still open any workflow you started or are due to decide. View a workflow shows its stages, decisions and current state.

## Escalate, cancel, reopen or invalidate

These change a workflow without deciding it. Escalate passes the current stage on, cancel stops a workflow, reopen returns a closed one for a fresh decision, and invalidate voids an approval whose subject has changed.
To change a workflow:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Workflows & Approvals**.
3. Go to **Escalate, cancel, reopen or invalidate**.
4. Enter the **Workflow ID** and choose the **Action**.
5. When escalating, enter who to **Escalate to (principal ID)**.
6. Add a **Reason code**, a **Reason** and the **Version you last read**, then submit.
Each of these actions needs its own permission.

## Can this approval still be released against?

An approval is tied to a fingerprint of what was approved, so it cannot be reused for something that has changed since.
To check an approval:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Workflows & Approvals**.
3. Go to **Can this approval still be released against?**.
4. Enter the **Workflow ID**, the **Subject fingerprint now** and the **Subject version now**.
5. Select **Check**.

## Workflow definitions

Workflow definitions are versioned routes a workflow can start from: a type, a version, a name and its stages. A running workflow keeps the route it started with, so changing a definition never changes a decision already in progress.

## Register a definition

Register a definition saves a new versioned route a workflow can start from.
To register a definition:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Workflows & Approvals**.
3. Go to **Register a definition**.
4. Enter the **Workflow type**, the **Version**, a **Name** and a **Description**, and add its stages.
5. Select **Register definition**.
Workflows already running keep the version they started with.

## Activate, suspend, retire or supersede

Only an active definition can start new workflows; a superseded one is replaced by a newer version.
To change a definition's state:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Workflows & Approvals**.
3. Go to **Workflow definitions**.
4. Choose the **Definition** and the **Action**, and when superseding, what it is **Replaced by**.
5. Select **Apply**.
Workflows already running keep the version they started with.
