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

Register a source declares which events feed a search surface. Search is built from the events each area publishes rather than by reading other areas directly, and every event carries the organization that produced it. Registered sources lists what may be indexed and from which events.

## Registry: what may be indexed

A search contract describes the shape of a search surface that every organization shares. Draft a contract, then publish it; publishing creates the search surface.

## Search surfaces

Search surfaces are the scopes a caller may name, and what each field may be used for. A field absent from a scope is not disclosed to exist at all. Depending on the surface, results are re-checked against the caller's current permissions, and may be reloaded from the original record rather than trusted from the index.

## Build a generation

Build a generation creates a new search index from the published contract. A field the contract does not declare is rejected rather than silently indexed.

## Validate and activate

Validation runs real checks before an index can serve: any document without an organization fails the whole generation, and the number of documents is compared against an independent count. A generation that fails validation is never activated.

## Lifecycle: what is serving

A new generation is built while the previous one still serves, validated, and then switched over in one atomic step. Ready and active are separate states, because switching over requires validation to have passed. Index generations lists them all.

## Freshness and completeness

Freshness and completeness compares indexed counts with an independent count, so an index that has silently skipped events can be detected.

## Restrictions: removing visibility

Removing a record from search removes its discoverability; it is not record deletion and cannot meet a retention or erasure obligation on its own.

## Restriction propagation

Restriction propagation shows each removal's progress. APPLIED means the removal was written. VERIFIED means invisibility was independently proven by a retrieval test, and only then is a privacy or records obligation met in search. FAILED means the content may still be discoverable.

## Request an export

An export is a separately authorized act with its own purpose and evidence. Permission to search is not permission to extract in bulk.
