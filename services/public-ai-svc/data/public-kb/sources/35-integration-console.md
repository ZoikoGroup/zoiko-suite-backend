---
source_id: kb-integration-console
title: Integration & Extensibility page
domain: Integrations
module_id: integration-console
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

## Integration & Extensibility page

The Integration & Extensibility page manages external protocol adaptors, open banking feeds, HRIS synchronization and digital signature pipelines. It shows bridge connections, banking feeds, synced HR systems and eSign envelopes, with **Poll Connectors**, **Sync All Feeds** and **Add Connection** at the top.

## Connectivity Fabric

The connectivity fabric has five adaptors: the Protocol Gateway (REST, GraphQL, SOAP and webhooks), Financial Pipelines (statement ingestion, SWIFT messaging and ACH), Employee Sync (Workday, BambooHR and others), Digital Envelopes (multi-party contract signing) and Market Feeds (live FX rates and market data).

## Add Connection

Add Connection starts a new bridge connection to an external system.
To add a connection:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Integration & Extensibility**, then select **Integrations**.
3. At the top of the page, select **Add Connection**.
4. Follow the prompts for the system you are connecting.
**Sync All Feeds** refreshes every connected feed, and **Poll Connectors** checks each connector's status.

## Sync All Feeds and Poll Connectors

Sync All Feeds refreshes every connected banking, HR, eSignature and market feed at once. Poll Connectors checks the status of each connector.
To sync all feeds:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Integration & Extensibility**, then select **Integrations**.
3. At the top of the page, select **Sync All Feeds**, or **Poll Connectors** to check connector status.

## Integration panels

The Integrations page has one panel per connector. The Connectivity API Bridge transforms protocols such as REST, GraphQL, gRPC and webhooks through protocol adaptors and API gateways. Open Banking & SWIFT Feeds verify bank accounts, bring in transactions and sync statements automatically, over open banking, SWIFT and clearing feeds. HRIS Directory Sync keeps the worker directory in step with HR systems such as Workday, BambooHR and Rippling, in both directions. eSignature Orchestration routes digital signature envelopes for multi-party signing through providers such as DocuSign and Adobe Sign. External Market Feeds bring in live FX rates, market data and regulatory lists through ingestion streams.
