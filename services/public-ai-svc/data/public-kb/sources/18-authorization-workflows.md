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

## How a permission check is decided

A permission check applies its layers in order: role grants first; then delegated and elevated authority, which can extend what someone may do; then separation of duties and access conditions, which can only refuse; and finally authority limits and two-step approval, which cap what is allowed.

## Check access and decision reasons

Check access asks exactly what a service asks before it acts, for a person, an action and a legal entity, and the answer is recorded. The decision reason shows which layer allowed or refused a recorded check.

## Grant role

Grant role gives a person a role for a legal entity from a start date. A privileged role is held for an independent approver unless you have approval rights, and it grants nothing until approved. Revoking a grant takes effect on the next permission check.

## Elevated access lifecycle

Elevated access covers just-in-time, emergency and support sessions. Each is requested, becomes active, names exact actions, ends on its own, and is then due for review until someone attests to what was done with it.

## Workflows & Approvals

Workflows & Approvals runs multi-stage approvals for actions that need more than one person. An approval adds to authorization and never replaces it: an approver must also be authorized, and every step is recorded as evidence. Three rules hold whatever anyone's grants say: the person who started a workflow never decides it, nobody decides two stages of the same workflow, and a service account never decides one. A workflow past its deadline expires and a stage past its deadline escalates.

## Start a workflow

Start a workflow opens an approval for something that needs one. Enter the workflow type, the legal entity, what is being approved, such as a purchase order and its ID, an optional person to escalate to, and either a saved route or the stages: for each stage, who decides and a deadline in hours. You cannot approve your own workflow.

## Decide the current stage

The stage's named approver, their delegate, a holder of the stage's role or the person it was escalated to records Approve or Reject, and a reason is required to reject. Each decision is checked against authorization and recorded. Escalate, cancel, reopen or invalidate changes a workflow without deciding it, and each needs its own permission.

## Waiting for a decision

Waiting for a decision lists pending and escalated workflows in your organization. Without permission to list them all, you can still open any workflow you started or are due to decide. View a workflow shows its stages, decisions and current state.

## Escalate, cancel, reopen or invalidate

These change a workflow without deciding it. Escalate passes the current stage to the escalation contact, cancel stops a workflow, reopen returns a closed one for a fresh decision, and invalidate voids an approval whose subject has changed. Each needs its own permission.

## Can this approval still be released against?

An approval is tied to a fingerprint of what was approved. Before anything is released against it, the platform checks that the subject has not changed since, so an approval cannot be reused for something different.

## Workflow definitions

Workflow definitions are versioned routes a workflow can start from: a type, a version, a name and its stages. A running workflow keeps the route it started with, so changing a definition never changes a decision already in progress.

## Register a definition

Register a definition saves a new route: the workflow type, its version, a name and its stages.

## Activate, suspend, retire or supersede

Activate, suspend, retire or supersede moves a definition through its lifecycle; only an active definition can start new workflows, and a superseded one is replaced by a newer version while workflows already running keep the version they started with.
