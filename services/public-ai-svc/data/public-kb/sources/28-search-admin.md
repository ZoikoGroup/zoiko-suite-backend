---
source_id: kb-search-admin
title: Search administration
domain: Governance Platform
module_id: search-admin
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

## Search administration

Search administration controls what can be searched, what is serving, and what has been removed from search.

## Register a source

Register a source declares which events feed a search surface. Search is built from the events each area publishes, and every event carries the organization that produced it.
To register a source:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. Open **Search administration** from the search box at the top of the console.
3. Go to **Register a source**.
4. Enter the **Owner service**, the **Source type**, the **Event topic** and the **Sensitivity ceiling**.
5. List the **Indexed event types** and the **Restriction event types**.
6. Select **Register source**.

## Registry: what may be indexed

A search contract describes the shape of a search surface that every organization shares; publishing it creates the surface.
To draft and publish a contract:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. Open **Search administration** from the search box at the top of the console.
3. Go to **Draft a contract**.
4. Enter the **Source type**, the **Retrieval class**, the **Authorization action** and the **Fields**, then select **Draft contract**.
5. In **Publish a contract**, choose the **Contract** and its **Target state**, then select **Transition**.

## Search surfaces

Search surfaces are the scopes a caller may name, and what each field may be used for. A field absent from a scope is not disclosed to exist at all. Depending on the surface, results are re-checked against the caller's current permissions, and may be reloaded from the original record rather than trusted from the index.

## Build a generation

Build a generation creates a new search index from the published contract. A field the contract does not declare is rejected rather than silently indexed.
To build a generation:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. Open **Search administration** from the search box at the top of the console.
3. Go to **Build a generation**.
4. Choose the **Scope**.
5. Select **Build generation**.
The previous generation keeps serving while the new one is built.

## Validate and activate

Validation runs real checks before an index can serve, and a generation that fails validation is never activated.
To validate and activate a generation:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. Open **Search administration** from the search box at the top of the console.
3. Go to **Validate and activate**.
4. Choose the **Generation** and its **Target state**.
5. Select **Transition**.
Switching over happens in one atomic step once validation has passed.

## Lifecycle: what is serving

A new generation is built while the previous one still serves, validated, and then switched over in one atomic step. Ready and active are separate states, because switching over requires validation to have passed. Index generations lists them all.

## Freshness and completeness

Freshness and completeness compares indexed counts with an independent count, so an index that has silently skipped events can be detected.

## Restrictions: removing visibility

Removing a record from search removes its discoverability; it is not record deletion and cannot meet a retention or erasure obligation on its own.
To remove a record from search:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. Open **Search administration** from the search box at the top of the console.
3. Go to **Remove a record from search**.
4. Enter the **Scope**, the **Source type**, the **Source id**, a **Reason** and the **Authoritative source event id**.
5. Select **Remove from search**.
Restriction propagation shows when invisibility was independently proven (VERIFIED).

## Restriction propagation

Restriction propagation shows each removal's progress. APPLIED means the removal was written. VERIFIED means invisibility was independently proven by a retrieval test, and only then is a privacy or records obligation met in search. FAILED means the content may still be discoverable.

## Request an export

An export is a separately authorized act with its own purpose and evidence. Permission to search is not permission to extract in bulk.
To request an export:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. Open **Search administration** from the search box at the top of the console.
3. Go to **Request an export**.
4. Choose the **Scope** and give the **Reason for the export**.
5. Select **Request export**.
