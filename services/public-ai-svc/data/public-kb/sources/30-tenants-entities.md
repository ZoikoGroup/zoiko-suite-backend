---
source_id: kb-tenants-entities
title: Tenants and legal entities
domain: Governance Platform
module_id: tenants-entities
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

## Tenants

A tenant is your organization in ZoikoSuite. Each tenant holds its own legal entities, workspaces and settings, and nothing in one tenant is visible to another tenant.

## This tenant

This tenant shows your own organization's record. Status and lifecycle are separate: a new tenant is ACTIVE in status while still ONBOARDING in its lifecycle, and only the lifecycle moves through defined stages.

## Provision a tenant

Provision a tenant creates a new organization. It is permitted at platform level, because there is no tenant yet to check it against.

## Lifecycle commands and history

Lifecycle commands move a tenant through its lifecycle with named governance commands, which are preferred because the history then records which command was used. Lifecycle history lists everything that has happened to the tenant.

## Hostnames

Hostnames lists the web addresses that resolve to your organization.

## Create a legal entity

Create a legal entity adds a company to your organization with its type, jurisdiction, fiscal calendar, residency policy and entity code. Assign a jurisdiction links it to a jurisdiction in Jurisdictions & Rules.

## Update an entity

Update an entity changes only the descriptive fields. Entity type, jurisdiction, fiscal calendar, residency policy and entity code are fixed at creation because posted transactions reference them. A blank field means leave it alone, so clearing a trading name is an explicit choice. Change an entity's status moves it through its own transitions.

## Entity hierarchy

Entity hierarchy shows parent and child relationships between your legal entities, effective-dated. The same pair may hold several relationship types at once, such as ownership and reporting.

## Create a data residency policy

Data residency says which hosting region an organization's data lives in. A new tenant starts with a default setting and no region, so its hosting region reads as unresolved until one is assigned.

## Workspaces

A workspace sits beneath the tenant and may be scoped to one legal entity. Create a workspace needs a billing classification, which decides whether the workspace can ever produce a live charge; an unrecognised classification is refused.

## Registry integrity

Two active entities in one jurisdiction cannot hold the same registration number. An attempt to create one is refused and quarantined with its details intact, never merged. Find an entity by registry number helps avoid creating one.

## Quarantined duplicates

Quarantined duplicates lists refused attempts to create an entity whose registration number is already held in that jurisdiction, with their details kept intact for review.

## Legal profile versions

An entity's legal profile is versioned, so every change is kept.

## Reconstruct as of a date

Reconstruct as of a date shows an entity's legal profile as it stood on any earlier date, from its version history.
