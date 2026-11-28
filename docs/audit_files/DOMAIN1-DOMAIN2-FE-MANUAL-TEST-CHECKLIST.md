# Domain 1 + Domain 2: manual test checklist from the admin console

**Written:** 24 Nov 2026
**Frontend read:** `zoiko-suite-frontend-platform` at `ee39b7e` (branch `shashi-changes`, clean tree)
**Scope:** the 22 services of Domain 1 (D1-1 … D1-13) and Domain 2 (D2-1 … D2-9), only what a
person can do from the console. Backend-only routes are not listed.
**How it was built:** every page under `app/admin/<page>` and its components under
`components/admin/<page>` were read. Each item below is a button, form, tab or drawer that exists
in that code, and its expected result is what the page and the service promise.

Use each item's ID (for example `IC-07`) when you report a failure.

---

## 0. Read this first

### 0.1 Pages that will not open yet (found while building this list)

`npx tsc --noEmit` was run on the frontend. 17 of the 20 Domain 1 / Domain 2 pages type-check
clean. These three do not:

| Page | Problem | What you will see |
|---|---|---|
| `/admin/tenants` (D1-2) | `app/admin/tenants/page.tsx` does not parse. `ApprovalsPanel` (around line 726) ends with `)` and `);` where it needs `)}`, `</div>` and `);`. Committed in `ee39b7e` on 20 Nov. | Build error overlay. The whole page fails, not just one panel. |
| `/admin/workflows` (D2-6) | `components/admin/workflow/console/forms.tsx` imports `./constants`, which does not exist. The page also imports `formsRegister`, `href`, `listWorkflowInstances`, `DecideWorkflowDialog` and `LifecycleDialog`, and none of them is exported. | Build error ("Module not found" or "export … doesn't exist"). |
| `/admin/obligations` (D2-4) | `SessionChip` is called with `identity` and `attention`, but it takes `session` and `now`. `ControlsFooter` is given `items`, but it takes `children`. | The page probably crashes when it renders the session chip. If it renders, the footer is empty. |

Run the items for these three pages after the pages are fixed. Until then, mark them ⛔.

### 0.2 Code that may fail when you click (check these on purpose)

- **Tenants: inline `onClick` handlers in a server page.** These buttons are in `app/admin/tenants/page.tsx`,
  which has no `"use client"`: Approvals → Approve / Reject; Ownership → Validate SoD;
  Parties → Create party and Decide (resolution case); Tax identity bundles → View details;
  Data residency → Create policy. React does not allow a server component to pass a function to
  `onClick`, so expect a render error or a dead button once the page parses. They also use the
  browser's `prompt()` and `alert()`.
- **Evidence → "Submit response" and "Add note"** (`components/admin/evidence/console/pbc.tsx`, also a
  server file with `onClick`). The response is sent with no file: the code comment says "For now,
  just submit with a placeholder".
- **Workflows Overview.** The "Stages of Selected Definition" and "Instances by Type" panels show
  fixed demo values ("Stage 1: Alice (Finance)", "PURCHASE_APPROVAL: 12"). They are not live data.

### 0.3 Services with no console page

| Service | State in the console |
|---|---|
| D2-8 schema-registry-svc | `app/admin/schemas/actions.ts` and `components/admin/schemas/*` exist, but no page mounts them. Nothing to click. |
| D2-9 retention-registry-svc | Same: `app/admin/retention/actions.ts` and `components/admin/retention/*` exist, with no page. |
| D1-5 gateway-auth-svc | Has a page, but it is read-only and diagnostic (§ D1-5). |
| D1-6 search-indexer-svc | No sidebar entry. Open `/admin/search` directly, or use the top-bar search box. |

### 0.4 Before you start

1. **Start only the services the page needs** (memory rule: start what you need, stop it afterwards):
   `docker compose -f deployments/docker-compose.yml up -d --build <svc>`. Every page needs
   `identity-context-svc`, `tenant-entity-registry-svc` and `authorization-svc` as well as its own
   service. In `next dev`, `proxy.ts` can start a page's services on demand.
2. **Sign in through identity-context-svc, not the fallback.** After login, the response's
   `identityProvider` must be `identity-context-svc`. A `local-credentials` session has no
   envelope, and most pages then show **"Sign in again"**.
3. **Have a second (and third) person ready.** Many items need someone other than you: approving
   a password reset, approving support access, tenant termination, maker-checker on rules and
   policies, and workflow stages. Accounts are in `lib/auth.ts` (admin, tax.officer, cfo,
   legal.counsel, hr.director, procurement, security.audit). Per memory, tax.officer and cfo
   already hold grants. Ask before you grant more.
4. **Have a second factor enrolled on your own account** (§ D1-1b). Reset 2FA, merge and approving a
   reset all ask for a fresh step-up.

### 0.5 Checks that apply to every page (run once per page)

- [ ] **ALL-01 Page loads.** Header title and description show, with no red "unavailable" banner while the service is up.
- [ ] **ALL-02 KPI row** shows numbers. With the service stopped, the cards show an unavailable or "could not be read" state, never a fake 0.
- [ ] **ALL-03 Tabs.** Each tab opens, the URL gains `?tab=<key>`, and the tab's count matches the rows shown.
- [ ] **ALL-04 Drawer.** Clicking a row opens its drawer on the right (below the table on narrow screens). Closing it adds `drawer=closed`, and it stays closed until you pick a row.
- [ ] **ALL-05 Signed out.** Clear the `zoiko_session` cookie. The page shows "Sign in again" (or redirects to `/login?redirect=…`), never another tenant's data.
- [ ] **ALL-06 Refused write.** As a person without the grant, try any write. Expect a clear 403 message naming the permission, and no row change. Services fail closed when authorization-svc is down: stop it and try again, and the write must be refused.
- [ ] **ALL-07 Tenant isolation.** Sign in as a user of another tenant. None of the first tenant's rows appear.
- [ ] **ALL-08 Reason fields.** Every write that asks for a reason refuses to submit with it blank.

---

# DOMAIN 1: Identity, Scope & Foundation

## D1-1 · identity-context-svc

### D1-1a · Login and session (`/login`, user menu)

- [ ] **LG-01 Sign in with email and password.** Valid credentials take you to `/admin` (or to the `?redirect=` page). Cookie `zoiko_session` is set for 8 h.
- [ ] **LG-02 Wrong password.** Shows "Unable to sign in…" or the service's message. No cookie is set.
- [ ] **LG-03 Second-factor step at login.** For an account with TOTP, after the password the form asks "Enter the 6-digit code from your authenticator app". A wrong code is refused and the right one signs you in.
- [ ] **LG-04 SSO buttons** (Microsoft / Google / Apple / Company SSO). Each goes to `/api/auth/oidc/start` and on to the identity service's hosted page. An unconfigured provider shows an error, not a blank page.
- [ ] **LG-05 Passkey / SSO-only account.** The form sends you to the identity service's own page instead of asking for a password.
- [ ] **LG-06 "Forgot password?"** ⚠ In `components/login/LoginForm.tsx` this is a `<button type="button">` with no handler, so expect nothing to happen. Today a reset is only requested by an admin (IC-11) and approved by a second admin (IC-12).
- [ ] **LG-10 "Remember me for 30 days"** checkbox. Check whether it changes the cookie lifetime; the session is documented as 8 h (`SESSION_MAX_AGE_SECONDS`).
- [ ] **LG-07 Deep link.** Signed out, open `/admin/policies`. You are sent to `/login?redirect=/admin/policies` and come back there after sign-in.
- [ ] **LG-08 Sign out** (user menu → "Sign out"). The cookie is cleared and `/admin` sends you to `/login`.
- [ ] **LG-09 Service down.** With identity-context-svc stopped, a demo account still signs in with `identityProvider: local-credentials`. Pages that need an envelope then show "Sign in again".

### D1-1b · My sign-in security (`/admin/account/security`, linked from any step-up prompt)

- [ ] **AS-01 Page loads.** KPIs: Second factors, Passkeys, Authenticator app, This session, Verified for changes. Panels: "How your sign-in is checked", "Assurance ladder", "Your recent sign-ins by assurance".
- [ ] **AS-02 Set up authenticator app (TOTP).** A QR code shows ("Scan this with your authenticator app…"). Enter the 6-digit code and the factor becomes Active, with "Two-step verification is on with this app". A wrong code leaves it "Not confirmed".
- [ ] **AS-03 Add a passkey.** Give it a name (e.g. "Work laptop") and complete the browser prompt. You see "Passkey added…" and a row with kind, synced, transports and user verification.
- [ ] **AS-04 Step-up (verify now).** With a code or passkey, "Verified for changes" turns on for a short window.
- [ ] **AS-05 Remove a factor.** Needs a fresh verification. Afterwards the row shows Removed and "This factor no longer signs you in".
- [ ] **AS-06 Second factors tab and Recent sign-ins tab** both list rows. The drawer shows assurance, basis, time and correlation ID.

### D1-1c · Identity Context page (`/admin/identity`)

Tabs: Identity context register · Live sessions · Sign-in events · Support access · Password resets · Federation · Diagnostics.

- [ ] **IC-01 KPIs:** Active identities in scope, Strong authentication, Elevated-risk sessions, Support access awaiting approval, Failed sign-ins. Panels: Current identity assurance (Assurance satisfied / Step-up required / Restricted-blocked), Authentication & federation (Passkeys, TOTP, Federated SSO, Step-up challenges, Sessions ended in the last 24 h), Access context signals.
- [ ] **IC-02 Header buttons:** "Recent events" opens the Sign-in events tab. "Live sessions" opens Live sessions filtered to ACTIVE.
- [ ] **IC-03 Add person (Type = Person).** Name and email, with "Send the activation link now" ticked. A row appears with status **Invited**. Without an email, it is refused ("Required for a person").
- [ ] **IC-04 Add person (Type = Service account / API client).** No email needed. The row is **Active** straight away, with no password.
- [ ] **IC-05 Register filters** by status (Active / Invited / Suspended / Disabled) and risk (High / Medium / Low).
- [ ] **IC-06 Person drawer.** Panes: Effective · Auth & factors · Identities · Audit. "Decision context" shows subject, tenant, entity and channel, and session and assurance. Empty states read "No second factor…", "No external identity provider is linked…" and "No sign-in events…".
- [ ] **IC-07 Suspend access.** Every session that person holds ends at once. Their next request is refused everywhere. The row shows Suspended.
- [ ] **IC-08 Restore access.** They can sign in again with the same roles.
- [ ] **IC-09 Your own row.** The drawer says "This is you. Someone else changes your own access." Suspend, reset and reset-2FA are not offered.
- [ ] **IC-10 Reset second factor.** Needs your fresh step-up. Without it, the dialog links to `/admin/account/security`. Afterwards the person has no factors and must enrol again before any privileged action.
- [ ] **IC-11 Request password reset.** It creates a row in the Password resets tab with status **Awaiting approval**. No link is sent yet.
- [ ] **IC-12 Approve reset, as a second admin.** Needs step-up. Status becomes **Link sent**.
  - The requester sees "You requested this reset…" and cannot approve.
  - The subject sees "This reset is for your own account…" and cannot approve.
  - An admin who already decided sees "You have already decided…".
- [ ] **IC-13 Reject reset.** Status becomes **Rejected** and no link is sent.
- [ ] **IC-14 Merge a duplicate.** In the survivor's drawer, choose "Duplicate to retire" and confirm with step-up. The duplicate's sessions end, its external identities move to the survivor, and its ID resolves to the survivor. Irreversible: test it on throwaway principals only.
- [ ] **IC-15 Live sessions tab.** Rows show subject, acting entity, authentication, status, risk and issued time. The drawer shows trust posture, second factor, risk score, channel and expiry.
- [ ] **IC-16 End session.** The session row ends. That user's next request fails within the revocation window.
- [ ] **IC-17 Sign-in events tab.** Rows show event, outcome, assurance, basis and time. The drawer says "append-only".
- [ ] **IC-18 Support access tab.** A request shows **Awaiting approval**. "Approve support access" makes it **Active** for its window. "Revoke" makes it **Revoked**. An expired one shows **Expired**.
- [ ] **IC-19 Federation tab.** Lists identity providers (Active / Awaiting approval / Keys stale / Suspended / Revoked) with approved and latest version. When federation is off, the tab has no count.
- [ ] **IC-20 Diagnostics tab.** Shows service health, envelope signing keys (JWKS: key IDs and algorithm), and hosted-login OIDC metadata (issuer, endpoints, PKCE, DPoP).
- [ ] **IC-21 Diagnostics → Explain a session.** Paste a session context ID from Live sessions. All six dimensions show where each came from. An unknown ID shows "No session with that ID in this tenant".

---

## D1-2 · tenant-entity-registry-svc: Tenants & Entities (`/admin/tenants`)

⛔ Blocked by § 0.1. Items are listed so you can run them once the page compiles.

- [ ] **TE-01 Summary KPIs:** Active tenants, Active legal entities, Approvals awaiting decision, Open registry conflicts, Open resolution cases.
- [ ] **TE-02 This tenant:** Tenant overview shows name, lifecycle state, operational status, tenant ID, default residency policy, hosting region, currency/timezone/locale and creator.
- [ ] **TE-03 Provision a tenant.** Fill code (e.g. ACME), legal name, trading name, currency, timezone and locale. A new tenant ID is shown in state **ONBOARDING**.
- [ ] **TE-04 Lifecycle transition.** Only legal next states are offered: ONBOARDING→ACTIVE; ACTIVE→SUSPENDED/OFFBOARDING; SUSPENDED→ACTIVE/OFFBOARDING.
- [ ] **TE-05 Tenant command: ActivateTenant.** Onboarding ends.
- [ ] **TE-06 SuspendTenant**, with a reason. Every write for that tenant starts refusing on every page (try creating an entity). Reads still work.
- [ ] **TE-07 ResumeTenant.** Writes work again.
- [ ] **TE-08 InitiateTermination.** Needs a second approver's principal ID. Writes stop for good.
- [ ] **TE-09 CompleteTermination.** Needs a second approver. State becomes TERMINATED with no way back. Use a throwaway tenant.
- [ ] **TE-10 Lifecycle history.** Each move shows from → to, actor, approver and record version.
- [ ] **TE-11 Change defaults** (locale, timezone), with a reason. The overview updates.
- [ ] **TE-12 Bind a hostname** (e.g. `acme.zoiko.example`). It appears in the Hostnames table.
- [ ] **TE-13 Create a legal entity.** Fill code, legal name, type, currency, primary jurisdiction, fiscal calendar and residency policy. The new row appears in the entity register as ACTIVE. An unknown jurisdiction is refused, and a jurisdiction service that is down gives 503 (not "no").
- [ ] **TE-14 Update an entity.** Legal name, and trading name with "Leave unchanged / Set to… / Clear it".
- [ ] **TE-15 Entity status.** Offers ACTIVE→DORMANT/SUSPENDED/DISSOLVED, DORMANT→ACTIVE/DISSOLVED and SUSPENDED→ACTIVE/DISSOLVED. An illegal move is refused.
- [ ] **TE-16 Assign a jurisdiction** to an entity, with type, effective date and source basis (e.g. "Certificate of incorporation"). An assignment ID is shown.
- [ ] **TE-17 End-date a jurisdiction assignment** by its ID. It stops applying from that date.
- [ ] **TE-18 Legal profile versions: amend the profile** (legal name / registered office / other). It needs a second approver and a registry source. A new version row appears.
- [ ] **TE-19 Entity as of a date.** Pick an entity and a past date. It shows the legal name, version and registry number in force then.
- [ ] **TE-20 Find by registry number.** Number plus jurisdiction returns the matching entity.
- [ ] **TE-21 Registry conflicts: resolve** with a reason. The conflict leaves the open list.
- [ ] **TE-22 Entity hierarchy: create** parent → child with relationship type (OWNERSHIP …). It appears in the hierarchy table.
- [ ] **TE-23 End-date a hierarchy** by its ID.
- [ ] **TE-24 Approvals:** Approve or Reject (reject asks for a reason). The approval leaves the pending list. See § 0.2.
- [ ] **TE-25 Ownership & control: Validate SoD** for parent, child and percentages. Shows ALLOWED or "BLOCKED: <violations>". See § 0.2.
- [ ] **TE-26 Parties: create party (match-first).** If a match exists, it is shown instead of a duplicate.
- [ ] **TE-27 Resolution cases: Decide** MERGE / SPLIT / DISMISS. See § 0.2.
- [ ] **TE-28 Tax identity bundles: View details.** RESTRICTED and CONFIDENTIAL bundles are badged red.
- [ ] **TE-29 Data residency: create policy** with name, code, mode (STRICT / PREFER / DENY), conflict mode (REJECT / QUEUE) and region. It appears in the policy list. An empty region list says "Regions are provisioned via IaC".
- [ ] **TE-30 Workspaces: create** with name, classification, billing source and optional entity scope. A commercial class warns that it "can produce live Zoiko charges".

---

## D1-3 · configuration-feature-flag-svc: Feature Flags & Config (`/admin/settings`)

- [ ] **CF-01 Page sections:** "Features that can be switched on and off", "Settings services read at runtime", "Check exactly what a service would see", and "Current session" (name, email, role, status).
- [ ] **CF-02 Set a feature flag.** Fill key (e.g. `payroll.parallel-run`), environment (local/dev/staging/prod), scope (Just my organisation / Everyone in this environment), rollout % and enabled. A row appears in the flag table.
- [ ] **CF-03 Toggle a flag from the table** ("Switch on/off <key> in <env>"). The row flips, and the summary shows who set it and since when.
- [ ] **CF-04 Organisation override wins.** Set the same key globally ON and for your organisation OFF. The lookup for this organisation answers OFF.
- [ ] **CF-05 Save a config entry** with key (e.g. `payroll.cutoff_hour`), value, environment and scope. A row appears, and every later change is kept as history.
- [ ] **CF-06 Lookup "Is a feature switched on?"** (`key env this-organisation|everyone`). It returns on/off with the version reference.
- [ ] **CF-07 Lookup "What is a setting set to?"** returns the value. "Nothing found" does **not** mean unset: read the note on the page.
- [ ] **CF-08 Empty and down states:** "No features have been set yet", "Cannot reach the configuration service".

---

## D1-4 · secret-vault-integration-svc: Secret Vault (`/admin/secrets`)

The page holds policy, lease and audit records, never a secret value. The four steps must happen in order.

- [ ] **SV-01 "How access actually works"** card shows the 4 steps and the 404-versus-403 rule.
- [ ] **SV-02 Step 1: register a policy** with secret class, data classification and path (e.g. `integrations/stripe/webhook-signing-key`). It appears, and grants nothing yet.
- [ ] **SV-03 Step 2: create a version** with allowed workload IDs, max lease seconds and effective from. It is created as **DRAFT**.
- [ ] **SV-04 Version history:** paste a policy ID to list every version. An unknown ID shows "No such secret policy". A policy with no versions shows "Policy exists but has no versions".
- [ ] **SV-05 Step 3: activate the version.** The path becomes visible to the broker.
- [ ] **SV-06 Step 4: seed the material.** Without it, a broker request passes policy but fails at the vault, and reads as an outage.
- [ ] **SV-07 Active secret policies:** filter by scope (Global only / This tenant / This legal entity). An empty class says the broker would refuse "by absence — a 404".
- [ ] **SV-08 Request access (broker).** An allowed workload gets a lease row marked **Live**, GRANTED.
- [ ] **SV-09 Broker as a workload not on the list.** Expect **403** ("policy said no").
- [ ] **SV-10 Broker a path with no active version.** Expect **404** ("no policy"), shown differently from 403.
- [ ] **SV-11 Issued leases:** filter by date range. A past-expiry lease shows "Past expiry, still GRANTED".
- [ ] **SV-12 Read one lease** by ID. Shows the full record including `revoked_at`.
- [ ] **SV-13 Revoke lease.** It shows **Revoked**.
- [ ] **SV-14 Rotate a secret** (blank request ID generates one). A new version appears.
- [ ] **SV-15 Access audit log:** filter by date range. Every register, version, activate, broker, revoke and rotate shows up.
- [ ] **SV-16 A write without the grant** gets a 403 about your grants. The broker's 403 is about the workload, not you.

---

## D1-5 · gateway-auth-svc: Gateway Authentication (`/admin/gateway`)

Diagnostic page. The gateway itself has no admin writes.

- [ ] **GA-01 KPIs:** Gateway process, Verifying sign-ins, Signing keys, Your envelope, Your assurance level.
- [ ] **GA-02 Panel "What /verify checks":** stages Admission → Identity → Risk → Tenant, with refusal counts per stage.
- [ ] **GA-03 Panel "Who may set an identity header"** and **"This console and the gateway"** (verify, console reads, readiness, keys seen).
- [ ] **GA-04 Denial reasons tab.** Every code, with status, stage and meaning. Click one for "What it means" and "What to do", plus whether a retry helps.
- [ ] **GA-05 Verify my session tab.** Enter a method and URI and submit. It shows the gateway's answer, the reason, tenant context, what the risk engine said, retry-after, timing, and "What the service would be told" (the headers it would inject).
- [ ] **GA-06 Verify with an expired or invalid envelope** (sign out, or wait for expiry). Expect a refusal with a reason code from the Denial reasons list.
- [ ] **GA-07 Signing keys tab.** Shows the identity-context-svc JWKS key IDs, algorithm and use. They must match Identity → Diagnostics (IC-20).

---

## D1-6 · search-indexer-svc: top-bar search and `/admin/search`

- [ ] **SR-01 Top-bar search box** (every page). The surface dropdown lists published surfaces, or "None serving". Type a query to see results in a pop-up. Close it with the X or Esc.
- [ ] **SR-02 Result authorization.** A record you may not see is withheld. Results show "Withheld candidates" with a reason badge, not the record.
- [ ] **SR-03 Search administration page panels:** Search surfaces, Registered sources, Index contracts, Index generations, Freshness and completeness, Restriction propagation, Search evidence.
- [ ] **SR-04 Register a source** with owner service, source type, event topic, sensitivity ceiling, event types and restriction event types. It appears in Registered sources.
- [ ] **SR-05 Draft a contract** with source type, retrieval class, authz action (e.g. `OBLIGATION_READ`) and fields. It appears as DRAFT.
- [ ] **SR-06 Publish a contract.** Transitions CERTIFIED → PUBLISHED (→ RETIRED). An illegal jump is refused.
- [ ] **SR-07 Build a generation.** Only allowed for a scope with a PUBLISHED contract, otherwise refused.
- [ ] **SR-08 Validate and activate** a generation: VALIDATING → READY → ACTIVE (→ RETIRED). The scope becomes a serving surface in SR-01.
- [ ] **SR-09 Remove a record from search** (restriction) with scope, source type, source ID, reason (e.g. `PRV_ERASURE`) and event ID. It shows in Restriction propagation and disappears from search results.
- [ ] **SR-10 Request an export** with scope and reason. It is recorded in Search evidence.
- [ ] **SR-11 Search evidence** lists each search you ran (SR-01) for this tenant.

---

## D1-7 · notification-svc: Notifications (`/admin/notifications`, top-bar bell)

- [ ] **NT-01 Send a notification** with recipient principal ID, channel, an optional template (or subject and body), an optional address, source event type (e.g. `PAYROLL_RUN`) and source reference. A row appears in the Delivery register.
- [ ] **NT-02 IN_APP.** Status **SENT** (delivered by being recorded). The recipient's bell shows it as unread.
- [ ] **NT-03 EMAIL.** SENT means the provider accepted it. Hovering the provider response shows "Provider acceptance evidence — not proof of receipt".
- [ ] **NT-04 Leave the address empty for EMAIL.** It is resolved from identity-context-svc.
- [ ] **NT-05 WEBHOOK.** Recorded as **FAILED**, naming what is missing. Never shown as SENT.
- [ ] **NT-06 SMS** is not offered. Forcing it gives a 400 at the boundary, and no stored row.
- [ ] **NT-07 Idempotency.** Resending with the same correlation ID replays the stored notification and sends no second one.
- [ ] **NT-08 Delivery register states** include CREATED, SUBMITTED_TO_PROVIDER, PROVIDER_ACCEPTED, RETRYING and FAILED. There is never a generic "sent" for a provider channel.
- [ ] **NT-09 Bell.** The unread count shows. Opening it lists items. "Mark as read" clears that item. With the service down: "The inbox could not be read".
- [ ] **NT-10 Down state.** "Delivery register unavailable", with the reason.

---

## D1-8 · delegated-authority-svc: Delegated Authority (`/admin/delegations`)

- [ ] **DA-01 Delegation register:** delegator, delegate, action, window, status and ended.
- [ ] **DA-02 Grant a delegation of your own authority** (delegate, action e.g. `PAYMENT_APPROVE`, legal entity, from/to). A row appears as active.
- [ ] **DA-03 Delegate an action you do not hold.** Refused: "A delegation cannot exceed its delegator".
- [ ] **DA-04 Name someone else as delegator** without `DELEGATION_ADMINISTER`. Refused.
- [ ] **DA-05 As an administrator, name yourself as delegate** of someone else's authority. Refused, however the grant is worded.
- [ ] **DA-06 The delegate acts.** Sign in as the delegate and do the action within the window and entity. It is allowed. Outside the window or on another entity, it is refused.
- [ ] **DA-07 Revoke.** The row shows ended. The delegate's next attempt is refused.
- [ ] **DA-08 Cross-check.** The same delegation appears in Authorization → Delegations (AZ-22).

---

## D1-9 · access-control-svc: Roles & Access (`/admin/access-control`)

- [ ] **AC-01 Role catalogue:** role, scope, status and permission bundles. Empty state: "A role grants nothing until a permission bundle is attached".
- [ ] **AC-02 "Who holds these roles"** shows Live grants and "Roles nobody holds".
- [ ] **AC-03 Define a role** with code (e.g. `FINANCE_APPROVER`), name and scope (LEGAL_ENTITY + entity, or TENANT). It appears ACTIVE with no bundles.
- [ ] **AC-04 Attach a permission bundle** with role, bundle code (e.g. `APPROVE_PAYMENTS`) and actions (e.g. `PO_ISSUE, PO_CLOSE`). The bundle shows under the role. An unregistered action is refused.
- [ ] **AC-05 Edit a bundle's actions.** The change is visible at once in Authorization → Roles → Permission sets.
- [ ] **AC-06 Detach a bundle** (needs the "Detach this bundle" confirm box). It is removed.
- [ ] **AC-07 Rename a role.** Grants are unchanged ("A rename changes nothing about what the role grants").
- [ ] **AC-08 Retire a role.** Status RETIRED. Holders lose its permissions on the next authorization decision.
- [ ] **AC-09 Reactivate a role.** Status ACTIVE.
- [ ] **AC-10 Fail closed.** With authorization-svc stopped, every write here is refused, so the catalogue never claims a role that is not enforced.
- [ ] **AC-11 Defined is not held.** A new role grants nothing until you assign it in Authorization → Grant role (AZ-03).

---

## D1-10 … D1-13 · Security & Trust (`/admin/security`)

One page serves four services. Summary bar: mTLS Certificates, SIEM Events, CARTA Assessments, Registered Keys, AI Governance.

### D1-10 · mtls-management-svc
- [ ] **MT-01 Certificate list.** Active certificates, or "No active certificates found".
- [ ] **MT-02 Issue an mTLS certificate** with service name, common name and rotation days. The private key is shown **once**, with copy ("Copied to the clipboard"). The certificate and CA certificate PEMs are shown. A reload never shows the private key again.

### D1-11 · siem-integration-svc
- [ ] **SI-01 Register a SIEM exporter** with name, platform, endpoint URL and auth token. It appears in the list. The page says it does not connect to the endpoint.
- [ ] **SI-02 Send test event** with exporter, source service, event type, severity, message and JSON payload. It is stored against that exporter and shows under it. Nothing leaves the system ("no external delivery is configured").
- [ ] **SI-03 Invalid JSON payload** is refused.

### D1-12 · carta-svc
- [ ] **CA-01 Assessment list** with a decision filter (ALL / ALLOW / STEP_UP_MFA / ISOLATE / DENY) and a refresh button.
- [ ] **CA-02 Evaluate access** with subject ID, type (User / Service account / Bot), device trust 0–100, IP, resource sensitivity (Low…Restricted), action and hour 0–23. A new assessment is added on top, with trust score and decision. The button stays disabled until every field is filled.
- [ ] **CA-03 Risk changes the answer.** Low device trust, RESTRICTED sensitivity and an odd hour give STEP_UP_MFA, ISOLATE or DENY. High trust and LOW sensitivity give ALLOW.

### D1-13 · key-management-svc
- [ ] **KM-01 Register key metadata** with alias, model, provider and external reference (ARN or key URI). A row appears. Metadata only: no cryptographic operations.
- [ ] **KM-02 Rotate a key.** It records the rotation.
- [ ] **KM-03 Disable a key.** The first click asks for confirmation and the second disables it. The row shows disabled.

---

# DOMAIN 2: Governance Platform

## D2-1 · jurisdiction-rules-svc: Jurisdictions & Rules (`/admin/jurisdictions`)

Tabs: Jurisdiction registry · Rules · Rule packs · Applicability decisions · Source authority ·
Change history · Rollouts · Calendars · Payment terms · Obligation rules · Remediation.
Maker-checker applies throughout: the proposer can never be the approver.

**Header and overview**
- [ ] **JR-01 KPIs:** Jurisdictions configured, Published rule packs, Source checks pending, Drift & remediation, Awaiting a second person. Panels: Jurisdiction coverage, Controlled rule publication, Review & change queue.
- [ ] **JR-02 Export registry** downloads a file of the registry.
- [ ] **JR-03 Propose rule** (header button). Same form as JR-10.

**Jurisdiction registry**
- [ ] **JR-04 New jurisdiction** with code, name, type, authority type, parent, and effective from/to. A row appears.
- [ ] **JR-05 Add alias** (map an external code: code system, code, source standard and version, rationale, evidence, validity).
- [ ] **JR-06 End alias.**
- [ ] **JR-07 Retire jurisdiction** with reason, evidence and "superseded by". It is end-dated, not deleted, so past dates still resolve.
- [ ] **JR-08 Propose import** (source standard, version, evidence, summary, JSON rows). It appears in Change history.
- [ ] **JR-09 Import approval, by a second person:** "Approve and apply", or "Reject" with a reason. The proposer cannot approve.

**Rules**
- [ ] **JR-10 Propose rule change** with jurisdiction, domain, code, name, effective from/to, JSON payload, source reference, feed reference and supersedes. It is created pending approval.
- [ ] **JR-11 Request approval** (name the approver).
- [ ] **JR-12 Approve and activate,** by someone other than the proposer, with workflow ID and rationale. The rule becomes active. Done by the proposer, it is refused.
- [ ] **JR-13 Reject approval** with a reason.
- [ ] **JR-14 Record drift** (drift state and evidence). It emits legal drift. The rule shows drift.
- [ ] **JR-15 Supersede or retire** a rule (new status and end date).
- [ ] **JR-16 Set provenance** (regime, interpretation ID, source IDs, precedence, published on).
- [ ] **JR-17 Set typed parameters** (JSON). Invalid JSON is refused.

**Rule packs** (lifecycle, in order)
- [ ] **JR-18 New pack** (ref e.g. `jur.gb.tax.core`, name, owner, support owner).
- [ ] **JR-19 New version**, then Submit for review → Compile → Run tests → Submit test bundle (JSON) → Add review (role, decision, findings) → Certify → Sign → Verify → Publish. Each step is offered only when the previous one is done, and each moves the status.
- [ ] **JR-20 Deploy** to a ring and region. Skipping ring order needs a "ring-order exception".
- [ ] **JR-21 Roll back** (ring, region, restore version, reason).
- [ ] **JR-22 Supersede** (replaced by, reason, evidence). **Withdraw.** **Emergency block / Unblock.**
- [ ] **JR-23 Declare hotfix** (severity, incident, incident commander, rollback target, scope), then **Record retrospective**. A hotfix needs a 4th granted person (per memory).
- [ ] **JR-24 Cancel operation** on a running operation.
- [ ] **JR-25 Trusted signing keys:** Register a key. Retire it (signatures stay valid) or revoke it (compromised).
- [ ] **JR-26 Deployment rings and regions:** New ring, New region.

**Applicability decisions**
- [ ] **JR-27 Determine jurisdiction** for an entity (purpose and claimed code). It returns the governing jurisdiction.
- [ ] **JR-28 Backfill** one entity, and **Backfill all**.
- [ ] **JR-29 Resolve a rule** (jurisdiction code, domain, rule code). It returns the one winning rule across the parent chain, and names where it came from.
- [ ] **JR-30 Validate an action** (jurisdiction, action type, entity type, JSON payload). It returns allowed or blocked with reasons.
- [ ] **JR-31 Evaluate** (choose an evaluator, JSON request). It returns the result.
- [ ] **JR-32 Register submission** (code, kind, profile, subject, payload hash), then **Record provider event** (provider event ID, status, receipt, occurred).
- [ ] **JR-33 Drop resolver cache.** The next resolve reads fresh.

**Source authority**
- [ ] **JR-34 Capture source** (jurisdiction, authority, type, level, title, official identifier, language, URL, snapshot hash, publish and effective dates).
- [ ] **JR-35 Review source**, and **Supersede** with a replacement.
- [ ] **JR-36 New regime.** **Record interpretation**, then **Approve interpretation** (by a second person).
- [ ] **JR-37 Report a change** (notice), then in Change history: **Take review** → **Close with outcome**.

**Change history**
- [ ] **JR-38** lists imports, notices and failed outbox events. **Requeue** a failed event, with what was fixed and evidence.

**Rollouts**
- [ ] **JR-39 Seed portfolio. New rollout** (family ref, name, layer, jurisdiction, owners, scope). **Move rollout** (with a reason). **Expert decision. Set owner. Link packs. Attest** (with evidence).

**Calendars**
- [ ] **JR-40 New calendar.** **New version** (effective from, time zone, weekend days, cut-off, holidays). **Publish.**
- [ ] **JR-41 Check a date** (date plus N business days). It respects holidays and weekends.
- [ ] **JR-42 Import holidays** (source authority, reference, version …). **Set sources** on a draft version.

**Payment terms**
- [ ] **JR-43 New payment term.** **New version** (basis date, due method, days, day of month, business-day convention, calendar, grace). **Approve** (second person). **Supersede. Retire.**
- [ ] **JR-44 Calculate due date** from invoice, receipt and delivery dates. It is correct across a DST change.

**Obligation rules**
- [ ] **JR-45 New obligation rule** (period basis, anchor, offsets, month end, business-day adjustment, calendar). **Publish. Set provenance.**
- [ ] **JR-46 Calculate a due date** (JSON request).

**Remediation**
- [ ] **JR-47 Open case** (pack, version, ring, region, reason, evidence). **Record disposition. Close case.**

**Failure check**
- [ ] **JR-48** With jurisdiction-rules-svc stopped, saving an entity or obligation with a `jurisdiction_id` elsewhere is refused with 503 ("cannot answer"), never treated as "no".

---

## D2-2 · governance-decision-log-svc: Governance Log (`/admin/governance`)

Tabs: Decisions · Replays · Evidence chain.

- [ ] **GL-01 KPIs** for the chosen period: Total, Approved, Pending, Escalated, Denied, each with a trend arrow. Charts: Decisions by Status, Decisions by Domain, Trend Over Time. An empty period says "No decision was made in this period".
- [ ] **GL-02 Period selector** changes every number and chart.
- [ ] **GL-03 Decision register:** search by text, filter by domain, status, priority, jurisdiction, entity and action type (e.g. `APPROVAL_THRESHOLD`). Paging and rows per page work.
- [ ] **GL-04 Drawer** (the first row opens by default). Panes: Details · Approvals (follow-ups) · Evidence · Audit Trail. Evidence shows the outcome, rule basis, chain position, hash, previous hash, integrity, sealed flag and policy version.
- [ ] **GL-05 Open a decision as a full page** (`/admin/governance/decisions/<id>`). All fields show. An unknown ID shows "Decision not available".
- [ ] **GL-06 New Decision** (decision, summary, action type, outcome, domain, priority, entity, jurisdiction, rule basis, reference, related obligation, tags, decided, JSON context). A new row appears and is linked into the chain.
- [ ] **GL-07 Immutability.** There is no edit or delete anywhere. **Record follow-up** on a decision appends a new decision that names the original as its cause, and the original is unchanged.
- [ ] **GL-08 Replay** a decision (against the exact policy version it used). The result shows in the Replays tab: match or differs.
- [ ] **GL-09 Evidence chain tab:** decisions in the chain, sealed through, not yet sealed, head hash.
- [ ] **GL-10 Seal Package** (label, reason, seal through ID). A new package with a Merkle root appears. Before the first seal: "No package has been sealed yet".
- [ ] **GL-11 Verify Chain.** Recomputes every hash and reports intact (or the first broken link).
- [ ] **GL-12 Verify Package #n.** Recomputes that package's root and chain hash, and reports a match.
- [ ] **GL-13 Export** downloads a CSV of the filtered decisions. Signed out, it returns 401.
- [ ] **GL-14 Evidence bundle download** (`/admin/governance/evidence-bundle`) for a package.
- [ ] **GL-15 Writes appear here.** After a governed action on another page (for example AZ-03 grant, PC-05 activate, WF approve), its decision appears here.

---

## D2-3 · policy-svc: Policies & Controls (`/admin/policies`)

- [ ] **PC-01 KPIs:** Total Policies, Total Controls, Compliant Controls, Control Issues, Review Overdue. Charts: Control Effectiveness by Domain, Policies by Status, Control Reviews (with a review-window selector).
- [ ] **PC-02 Register:** filter by domain, status, type and effectiveness. Select rows. Paging works. The first row opens in the drawer.
- [ ] **PC-03 Export** downloads the ticked rows, or every filtered row when none is ticked.
- [ ] **PC-04 New menu → New policy** (type APPROVAL_THRESHOLD / SPEND_CONTROL / SOD_RULE / SIGNATORY_MATRIX, scope, code, name). It appears with no version.
- [ ] **PC-05 New → Draft a policy version** (policy, threshold amount, effective from, rule payload). It is created DRAFT.
- [ ] **PC-06 Version lifecycle in the drawer:** Submit for review → Approve (a different person) → Activate or Schedule. Only "activate" changes what the system enforces.
- [ ] **PC-07 Supersede.** A new version supersedes the active one and the old one ends.
- [ ] **PC-08 Suspend, Emergency suspend, Review suspension, Retire version.**
- [ ] **PC-09 New → Activate a version** (policy and version picker). Same result as PC-06.
- [ ] **PC-10 New → Evaluate an action** (amount). An APPROVAL_THRESHOLD policy answers allow or needs-approval. Any other type answers **501** (no evaluator). The page says so.
- [ ] **PC-11 Certification.** Activating a SPEND_CONTROL or SOD_RULE version reports EXEMPT_NO_EVALUATOR. APPROVAL_THRESHOLD and SIGNATORY_MATRIX are certified.
- [ ] **PC-12 Drawer → Compare versions** (base and target). It shows the differences.
- [ ] **PC-13 Drawer → "How it resolves here now"** shows the version that applies to your entity today.
- [ ] **PC-14 Test suite:** **Add test case**, then **Run test suite**. Results show. **Request waiver for <test>**, then **Approve waiver** (a second person).
- [ ] **PC-15 Attestations:** **Sign attestation**, then **Revoke**.
- [ ] **PC-16 Evidence:** **Re-deliver** a failed evidence delivery.
- [ ] **PC-17 New → Define a control test** (control ref e.g. FIN-AP-01, test code, frequency, title, methodology, sample). It appears as a control row.
- [ ] **PC-18 Record a test result** (period from/to, result EFFECTIVE / EFFECTIVE_WITH_EXCEPTIONS / NOT_EFFECTIVE, exceptions, evidence refs). The effectiveness KPIs update.
- [ ] **PC-19 New → Draft an accounting policy** (domain, framework e.g. IFRS, version, precedence, applies to, book, effective dates, framework source, text ref, impact, FUTURE_ONLY or OPEN_PERIODS, a retro justification when OPEN_PERIODS, rule mapping, rationale).
- [ ] **PC-20 Retire policy** (the whole family). It leaves the active list.

---

## D2-4 · obligations-svc: Obligations (`/admin/obligations`)

⚠ See § 0.1 (the page may crash on the session chip). Tabs: Obligations · Deadline Clocks · Definitions & Mappings · Filing Requirements · Applicability.

- [ ] **OB-01 KPIs:** Open, Due Soon (7 d), Overdue, Filings Pending, Unassessed Applicability, Escalation Steps Due. Panels: Pipeline (clocks by state), Escalation Ladder, By Jurisdiction / Clock Class.
- [ ] **OB-02 New Obligation** (source type and ID, code, type FILING / TAX_PAYMENT / REGULATORY_REPORT / DISCLOSURE / LICENCE_RENEWAL …, jurisdiction, due date, severity, function, source reference). A row appears OPEN. An unknown jurisdiction is refused. With jurisdiction-rules-svc down, it is refused with 503.
- [ ] **OB-03 Transition Obligation:** OPEN → IN_PROGRESS → CLOSED (OVERDUE is set by time). An illegal move is refused.
- [ ] **OB-04 Link Workflow** (workflow instance ID). The obligation shows the linked workflow.
- [ ] **OB-05 Check Applicability** (obligation, jurisdiction code, entity, action, the artifacts present). It answers whether the required evidence exists.
- [ ] **OB-06 Deadline Clocks tab:** each clock shows class, calendar, deadline and its escalation ladder steps.
- [ ] **OB-07 Satisfy clock (FULFILLED)** with evidence IDs.
- [ ] **OB-08 Waive clock (WAIVED)** with reason, authority ID and optional evidence.
- [ ] **OB-09 Mark not applicable (NOT_APPLICABLE)** with reason and authority. Each action is hidden once the clock is already in that state.
- [ ] **OB-10 Definitions & Mappings, Filing Requirements and Applicability tabs** are **read-only lists** ("Create one with the API" / "Record one via the API"). Filing status has no advance action: every filing requirement stays PENDING.
- [ ] **OB-11 Authorization:** create and status changes need OBLIGATION_CREATE / OBLIGATION_STATUS_UPDATE on the obligation's entity, and fail closed.

---

## D2-5 · authorization-svc: Authorization (`/admin/authorization`)

Tabs: Role grants · Roles · Approvals · Duty conflicts · Conditions · Authority limits · Elevated access · Delegations · Decision log · Service access · Check access.

- [ ] **AZ-01 KPIs:** Live role grants, Awaiting approval, Duty-conflict rules, Elevated sessions, Refused checks. Panels: Elevated access lifecycle, How a permission check is decided, Decision outcomes.
- [ ] **AZ-02 Session chip** in the header shows your session trust and step-up state.

**Role grants**
- [ ] **AZ-03 Grant role** (person, role, applies-to, legal entity, starts/ends, approval reference). It appears pending approval, or live.
- [ ] **AZ-04 Approve / Reject a grant,** by a second person. Granting to yourself, or approving your own grant, is refused.
- [ ] **AZ-05 Suspend / Resume a grant.** The suspension history shows Resumed / Still suspended. A suspended grant stops allowing at once (test with AZ-27).
- [ ] **AZ-06 Revoke a grant.**
- [ ] **AZ-07 "Grants as of"** a past date shows the grants in force then. The drawer panes are Overview, Approval, Suspensions and Holder.

**Roles**
- [ ] **AZ-08 Roles tab:** the drawer shows Permission sets. **Retire role / Reactivate role. Retire set / Reactivate set.** The same changes show on the Roles & Access page.

**Approvals**
- [ ] **AZ-09** Every pending request (grants, limits, sessions, rules) is in one queue. **Approve / Reject.** Your own requests cannot be approved by you.

**Duty conflicts (SoD)**
- [ ] **AZ-10 New rule** (area, kind, action, conflicts with). It is proposed, then **Publish** (second person) or **Reject**. Later **Retire / Reactivate.**
- [ ] **AZ-11 "What conflicts?"** Look up an action and get its conflicting actions.
- [ ] **AZ-12 Request exception** (person, compensating control, from, expires). **Approve / Reject / Revoke** the exception.
- [ ] **AZ-13 Live SoD.** A person holding two conflicting roles is refused the second action (Layer 3). Approving your own request is refused (Layer 4, own-object SoD).

**Conditions (ABAC)**
- [ ] **AZ-14 New condition** (code, action, effect, attribute, comparison, value). Proposed, then Publish / Reject / Retire / Reactivate. Once live, a check that breaks the condition is refused (Layer 5).

**Authority limits**
- [ ] **AZ-15 Request limit** (for a person or a role, action, currency, entity, from/up-to amount, dates). **Approve / Reject** (second person).
- [ ] **AZ-16 Request change** to a limit. **Retire** a limit.
- [ ] **AZ-17 Live limit.** An amount above the limit is refused in Check access (AZ-27, with amount and currency).

**Elevated access**
- [ ] **AZ-18 Request JIT access** (actions, for, ticket, resource, environment, hours, reason). Approve, after which the session is active. Revoke.
- [ ] **AZ-19 Break glass** (incident reference, for, minutes, actions, reason). Request it, approve it, then a required **Review session** afterwards.
- [ ] **AZ-20 Support access** (ticket, organisation consented?, purpose, operator, hours, writes allowed). Open it, approve it, revoke it.
- [ ] **AZ-21 Session drawer** panes: Overview, Approval, Review, Activity. An expired session stops allowing.

**Delegations**
- [ ] **AZ-22 Lend authority** (borrower, what to lend, actions, from/until, entity, reason, approval reference). It appears in the list. **Withdraw.**
- [ ] **AZ-23** A delegation never exceeds the lender's live grants: revoke the lender's grant and the borrower is refused too (Layer 2).

**Decision log**
- [ ] **AZ-24** Search by person, action or decision ID. Filter by answer: "Not allowed" finds refusals beyond the newest page. The drawer panes are Overview and Context.
- [ ] **AZ-25 Explain a decision** (decision ID). It shows the layer-by-layer reason.

**Service access**
- [ ] **AZ-26 Save a workload binding** (workload, audience, actions). **Deactivate / Reactivate.** **Refresh cache** for a principal: the cached decisions are dropped.

**Check access**
- [ ] **AZ-27 Check a permission** (action, person, where, entity, amount, currency, record owner, other attributes). Answers ALLOW or DENY with the deciding layer: suspended principal (0), no role (1), delegation (2), SoD (3), own object (4), ABAC (5).
- [ ] **AZ-28 What you can do.** Lists your allowed actions.
- [ ] **AZ-29 Actions on a record** (record type and ID, person, entity).
- [ ] **AZ-30 Maker and checker** (action, maker, checker, subject). The same person as maker and checker is refused.
- [ ] **AZ-31 Before granting** (the actions a grant adds, person, entity). Warns of SoD conflicts before you grant.
- [ ] **AZ-32 Where can someone act?** (entities, person, action).
- [ ] **AZ-33 Borrowed authority?** (person, entity, action, lent by).
- [ ] **AZ-34 Effective access → Recompute** for a person.
- [ ] **AZ-35 Suspended principal.** Suspend a user in Identity (IC-07). Every check for them answers DENY at layer 0.

---

## D2-6 · workflow-svc: Workflows & Approvals (`/admin/workflows`)

⛔ Blocked by § 0.1. Tabs: Pending Approvals · Instances · Definitions · Audit Engagements · Forms · Cases · State Machines.

- [ ] **WF-01 KPIs:** Pending My Approval, Active Instances, Escalated, Expiring in 24 h, Active Definitions, Pipeline Velocity. (The demo panels are noted in § 0.2.)
- [ ] **WF-02 Define workflow** (type, name, description, stages: each "One named person / Some of a named group / Anyone with a role", plus due-in hours and escalation target). It appears in Definitions as Active v1.
- [ ] **WF-03 Start workflow** (subject type and ID, escalation target; use a definition, or define stages inline with at least one stage). It appears in Instances and in the approver's Pending Approvals.
- [ ] **WF-04 Decide: Approve** as the stage-1 approver. It moves to stage 2. The last stage makes it Approved.
- [ ] **WF-05 Decide: Reject.** The instance becomes Rejected.
- [ ] **WF-06 The initiator tries to decide.** Refused: "The initiator never decides their own workflow".
- [ ] **WF-07 The same person decides two stages.** Refused.
- [ ] **WF-08 A service account decides.** Refused.
- [ ] **WF-09 Each decision is checked against authorization-svc** with workflow_type, approver_role and stage_order. An approver without the grant is refused.
- [ ] **WF-10 Lifecycle actions:** Escalate, Escalate to a named owner, Cancel, Request changes, Invalidate. Each status changes (Escalated, Cancelled, "Invalidated — subject changed").
- [ ] **WF-11 Re-check.** A policy or authority change while a workflow is pending re-checks the pending approvers and escalates if needed.
- [ ] **WF-12 Definition commands:** Activate, Suspend, Retire, Supersede with another version.
- [ ] **WF-13 Expiry.** An instance past its due time shows Expired.
- [ ] **WF-14 Audit Engagements, Cases and State Machines tabs** list rows with their statuses (Proposed … Closed; Open … Escalated; terminal states).
- [ ] **WF-15 Governance link.** A completed approval appears in the Governance Log (GL-15).

---

## D2-7 · evidence-requirements-svc: Evidence Requirements (`/admin/evidence`)

Tabs: Requirements · Evaluations · Missing · Evidence requests · My evidence requests · Check evidence.

- [ ] **ER-01 KPIs:** Active requirements, Satisfied (30 d), Missing (30 d), Actions with no requirements (30 d), Open evidence requests. Panels: Gate pipeline (Requested → Catalogue resolved → Artifacts verified → Decision recorded → Event published), Evidence types, Catalogue coverage (ungated checks).
- [ ] **ER-02 Add requirement** (business area, action, evidence, how many, of the kind, entity, starts, what to produce). It appears in Requirements.
- [ ] **ER-03 Withdraw** a requirement (end at; blank means now). It shows as ended. Past checks keep their answer.
- [ ] **ER-04 Check evidence** (entity, area, action, evidence on file). It returns one of three outcomes: satisfied, missing (with what it is short of), or no requirement. The check is recorded in Evaluations.
- [ ] **ER-05 Evaluations tab:** the drawer shows the decision, outcome, entity, correlation and reference. **Missing tab** lists the evaluations that fell short.
- [ ] **ER-06 New request** (title, from person, due, entity, requirement, what is needed). It is created **Draft**.
- [ ] **ER-07 Request commands:** Send (Sent) → Reassign (to a person) → Extend (new due date) → Ask to clarify (Clarification asked) → Mark satisfied → Close. Close is final and cannot be reopened.
- [ ] **ER-08 Mark satisfied is refused** until the latest response is scanned clean.
- [ ] **ER-09 Record scan** (response ID, verdict). A clean verdict allows ER-08. An infected verdict blocks it.
- [ ] **ER-10 My evidence requests** (as the assignee): Submit response and Add note. See § 0.2: no real file is attached.
- [ ] **ER-11 Overdue.** A request past due shows Overdue.

---

## D2-8 · schema-registry-svc and D2-9 · retention-registry-svc

No console page (see § 0.3). Nothing to test from the FE. When a page is mounted, the existing
actions to test are:
- schema-registry: register a schema, look up a contract.
- retention-registry: create a retention policy, create a legal hold, release a hold, resolve
  retention for a record.

---

## Appendix · Page ↔ service map

| Unit | Service | Page | Sidebar label |
|---|---|---|---|
| D1-1 | identity-context-svc | `/login`, `/admin/identity`, `/admin/account/security` | Identity Context |
| D1-2 | tenant-entity-registry-svc | `/admin/tenants` | Tenants & Entities |
| D1-3 | configuration-feature-flag-svc | `/admin/settings` | Feature Flags & Config |
| D1-4 | secret-vault-integration-svc | `/admin/secrets` | Secret Vault |
| D1-5 | gateway-auth-svc | `/admin/gateway` | Gateway Authentication |
| D1-6 | search-indexer-svc | top-bar search, `/admin/search` | (none) |
| D1-7 | notification-svc | `/admin/notifications`, bell | Notifications |
| D1-8 | delegated-authority-svc | `/admin/delegations` | Delegated Authority |
| D1-9 | access-control-svc | `/admin/access-control` | Roles & Access |
| D1-10 | mtls-management-svc | `/admin/security` | Security & Trust |
| D1-11 | siem-integration-svc | `/admin/security` | Security & Trust |
| D1-12 | carta-svc | `/admin/security` | Security & Trust |
| D1-13 | key-management-svc | `/admin/security` | Security & Trust |
| D2-1 | jurisdiction-rules-svc | `/admin/jurisdictions` | Jurisdictions & Rules |
| D2-2 | governance-decision-log-svc | `/admin/governance` | Governance Log |
| D2-3 | policy-svc | `/admin/policies` | Policies & Controls |
| D2-4 | obligations-svc | `/admin/obligations` | Obligations |
| D2-5 | authorization-svc | `/admin/authorization` | Authorization |
| D2-6 | workflow-svc | `/admin/workflows` | Workflows & Approvals |
| D2-7 | evidence-requirements-svc | `/admin/evidence` | Evidence |
| D2-8 | schema-registry-svc | (no page) | (none) |
| D2-9 | retention-registry-svc | (no page) | (none) |
