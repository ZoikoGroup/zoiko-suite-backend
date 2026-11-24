# Governance Log (governance-decision-log-svc): test guide from the console

**Written:** 24 Nov 2026, against the live local stack.
**Page:** `http://localhost:3000/admin/governance` (sidebar: Governance Platform → Governance Log)
**State when written:** tenant `11111111-…` holds 342 decisions (GD-2026-0001 … 0342, 11–18 Nov),
12 replays and 36 sealed packages. Every decision up to #342 is already sealed.

Each item below gives the input to type and what must happen. IDs (`GL-…`) match the
Domain 1 + 2 checklist, with sub-numbers for each input.

---

## 1. What has to run

| Container | Why the Governance Log needs it |
|---|---|
| zoiko-postgres, zoiko-kafka, zoiko-redis | storage, events, identity sessions |
| mtls-management-svc | workload certificates (GDL :8450, identity → registry, authorization) |
| identity-context-svc | sign-in, and the envelope GDL verifies (`IDENTITY_VERIFY=require`) |
| tenant-entity-registry-svc | the legal-entity lists (filter and New Decision form); sign-in context |
| access-control-svc | permission bundles read during sign-in |
| authorization-svc | GDL authorizes every read and write (`GOVERNANCE_DECISION_*`) |
| policy-svc | Replay fetches the exact policy version a decision used |
| governance-decision-log-svc | the service under test (:8083) |
| console (`next dev`, :3000) | the page |

Health check (all should print 200):

```bash
for p in 8080/health 8081/healthz 8137/healthz 8089/healthz 8085/healthz 8083/healthz; do curl -s -o /dev/null -w "$p %{http_code}\n" http://127.0.0.1:$p; done
```

When you finish, stop what you started:
`docker stop governance-decision-log-svc policy-svc authorization-svc access-control-svc identity-context-svc tenant-entity-registry-svc mtls-management-svc zoiko-kafka zoiko-redis`
(Keep zoiko-postgres running if other work uses it.)

---

## 2. Signing in

| Account | Password | Second factor | Governance permissions |
|---|---|---|---|
| admin@zoikosuite.com | `Zoiko@Governance1` | none (TOTP retired 24 Nov, see below) | READ, RECORD, REPLAY, SEAL |
| legal.counsel@zoikosuite.com | `Zoiko@Legal2026!` | none (TOTP retired 24 Nov) | READ, RECORD, REPLAY, SEAL |
| cfo@zoikosuite.com | `Zoiko@Finance2026!` | none | READ, RECORD, REPLAY, SEAL |
| tax.officer@zoikosuite.com | `Zoiko@Tax2026!` | none | READ, RECORD, REPLAY, SEAL |
| hr.director@zoikosuite.com | `Zoiko@People2026!` | none | **none** (use for refusal tests) |
| procurement@zoikosuite.com | `Zoiko@Commercial2026!` | none | **none** |
| security.audit@zoikosuite.com | `Zoiko@Audit2026!` | none | **none** |

Every account signs in with email and password only: one step, no code.

**Why admin@ and legal.counsel@ have no code any more.** On 23 Nov the demo seed
(`services/identity-context-svc/cmd/seed-demo-principals -totp`) enrolled the fixed dev secret
`JBSWY3DPEHPK3PXP` on both accounts, which added a 6-digit code step. On 24 Nov those two TOTP factors were
retired in the local `identity_context` database (`retired_reason = 'demo_password_only_login'`), so login is
password-only again. `LOGIN_MFA` is unset, so it defaults to `enrolled`: a second factor is asked for only when
one is enrolled.

To bring the code step back, re-run the seed with `-totp`. It refuses outside `ENV=local`. After that, print
the current code with:

```bash
C:/Users/kaman/AppData/Local/Python/bin/python3.14.exe -c "import base64,hmac,struct,time,hashlib;k=base64.b32decode('JBSWY3DPEHPK3PXP');h=hmac.new(k,struct.pack('>Q',int(time.time()//30)),hashlib.sha1).digest();o=h[-1]&15;print('%06d'%((struct.unpack('>I',h[o:o+4])[0]&0x7fffffff)%1000000))"
```

**Check you have a real session.** After sign-in, `/api/auth/login` answers with `"identityProvider":"identity-context-svc"`.
If it says `local-credentials`, the console fell back to a demo session with no envelope, and this page will
show "Sign in again".

---

## 3. Page header

| # | Input | Expected |
|---|---|---|
| GL-H1 | Period selector: Last 7 days | KPIs, both charts and Trend change. With data from 11–18 Nov and today 24 Nov, only decisions since 17 Nov count. URL gets `?days=7`. |
| GL-H2 | Last 30 days (default) | All 342 decisions. Approved 117, Pending 24, Escalated 132, Denied 62 (as of writing). |
| GL-H3 | Last 90 days / Last 12 months | Same totals as 30 days (no older data). The "vs. previous period" comparison changes. |
| GL-H4 | URL `?days=5` (not offered) | Falls back to 30 days. No error. |
| GL-H5 | **Export** | Downloads a CSV with header `Reference,Decision ID,Title,Status,Outcome,Domain,…,Chain hash`, one row per decision under the current filters (343 lines unfiltered). |
| GL-H6 | Export while filtered (e.g. Status = Denied) | The CSV holds only the filtered rows. |
| GL-H7 | Export after signing out (open `/admin/governance/export` in a private window) | JSON `{"error":"sign in again"}` with 401. |

---

## 4. KPI row and charts

| # | Check | Expected |
|---|---|---|
| GL-K1 | Total / Approved / Pending / Escalated / Denied cards | Numbers match the "Decisions by Status" chart. Percentages add up to 100 minus "Other". |
| GL-K2 | Note under the status chart: "N with an outcome no status category lists — filter by Other" | Set Status = Other outcome. The register shows exactly N rows (7 as of writing). |
| GL-K3 | Decisions by Domain | The counts per domain match the register filtered by that Domain. |
| GL-K4 | Trend Over Time | Its own period selector moves the trend only. |
| GL-K5 | Stop governance-decision-log-svc and reload | KPIs and charts show an unavailable state, never zeros. Start it again afterwards. |

---

## 5. Decisions tab: toolbar inputs

Type each value, press Enter (or **Apply filters** inside More Filters), and check the URL and the rows.

| # | Input | Value to try | Expected |
|---|---|---|---|
| GL-F1 | Search | `GD-2026-0342` | Exactly that decision. |
| GL-F2 | Search | `Signatory` | Decisions whose title, reference, entity or user matches. |
| GL-F3 | Search | `zzzz-no-match` | Empty state, with the "Clear filters" link. |
| GL-F4 | Search | 201+ characters | The input stops at 200 (`maxLength`). Forced in the URL (`?q=` + 201 chars), the service answers 400 "q exceeds 200 characters". |
| GL-F5 | Domain | each domain | Rows only from that domain. The count matches the domain chart. |
| GL-F6 | Status | Approved / Pending / Escalated / Denied / Other outcome | Each matches its KPI count. |
| GL-F7 | Status in the URL | `?status=FOO` | 400 from the service, shown as an error on the register: "status must be APPROVED, PENDING, ESCALATED, DENIED or OTHER". |
| GL-F8 | Priority | Critical / High / Medium / Low / Not set | Only rows with that priority. "Not set" is `UNSET`. |
| GL-F9 | Jurisdiction | a value from the list | Only rows with that jurisdiction. The list comes from the data (facets). |
| GL-F10 | More Filters → Legal entity | an entity | Only that entity's rows. The badge shows "More Filters (1)". |
| GL-F11 | More Filters → Decision owner | `svc-policy` | Decisions recorded by policy-svc. A random UUID gives an empty result. |
| GL-F12 | More Filters → Action type | `APPROVAL_THRESHOLD` | Only that action type. |
| GL-F13 | More Filters → Rule basis | `SIGNATORY_MATRIX:unresolved` | Matching rows. |
| GL-F14 | More Filters → Decided from / to | from 2026-11-18 to 2026-11-18 | Only 18 Nov rows. |
| GL-F15 | Decided from later than to | from 2026-11-20, to 2026-11-10 | Empty result or a clear error, never every row. |
| GL-F16 | Several filters at once | Status = Denied + Action type = SIGNATORY_MATRIX | Intersection only. |
| GL-F17 | **Clear filters** | — | All filters drop. Period and page size are kept. |
| GL-F18 | Pager | page 2, Next, Previous | Rows change, and `?page=` follows. |
| GL-F19 | Rows per page | 10 / 25 / 50 | That many rows. `?size=7` in the URL falls back to 10. |
| GL-F20 | Share a URL | copy a filtered URL into a new tab | The same view opens: every input lives in the URL. |

---

## 6. Decision drawer and full page

| # | Input | Expected |
|---|---|---|
| GL-D1 | Click a row (the first one opens by default) | The drawer opens with the reference, title and outcome badge. Close it with X and the URL gets `drawer=closed`. |
| GL-D2 | Details pane | Domain, entity, jurisdiction, priority, decision date, owner, related obligation, reference, tags. "Follows up" / "Caused by" links appear when linked. |
| GL-D3 | Approvals pane | Lists the follow-ups recorded against this decision; the count shows on the pane. |
| GL-D4 | Evidence pane | Outcome, rule basis, chain position, hash, previous hash, record integrity, chain link, sealed (yes for #1–342), policy version. |
| GL-D5 | Audit Trail pane | Entries for this decision. |
| GL-D6 | Open the full page (`/admin/governance/decisions/<decision_id>`) | All fields. A made-up ID shows "Decision not available". |

---

## 7. New Decision (header button): every input

Sign in as admin@ or cfo@. A successful save shows: "Decision recorded as GD-2026-0343. It is now immutable and
linked into the evidence chain", with the reference, status, domain, chain position and chain hash.

| # | Field | Value to try | Expected |
|---|---|---|---|
| GL-N1 | All required filled | Decision `Approve vendor contract renewal`, Action type `VENDOR_CONTRACT_RENEWAL`, Outcome Approved, Legal entity (any), Rule basis `PROCUREMENT-POLICY:v2.1` | Recorded. A new top row. Total +1, Approved +1. |
| GL-N2 | Action type blank | — | "Say what kind of action was decided (action type)." Nothing is written. |
| GL-N3 | Action type with spaces / lower case | `vendor contract renewal` | Saved as `VENDOR_CONTRACT_RENEWAL` (the console upper-cases it and joins with `_`). |
| GL-N4 | Rule basis blank | — | "A rule basis is required — an outcome without its basis is not evidence." |
| GL-N5 | Rule basis 257+ chars | long text | The input stops at 256. |
| GL-N6 | Outcome | each of Approved, Pending, In progress, Escalated, Denied | The KPI for that status moves by 1. "In progress" counts under "Other outcome". |
| GL-N7 | Domain | "Classify from the action type" | The service picks the domain (an unknown action type gives Other). |
| GL-N8 | Domain | pick one explicitly | Saved with that domain. |
| GL-N9 | Priority | Not set / Low / Medium / High / Critical | Saved. Filter GL-F8 finds it. |
| GL-N10 | Legal entity | a different entity | Saved under that entity. Filter GL-F10 finds it. |
| GL-N11 | Jurisdiction | `GB` | Saved. It appears in the Jurisdiction filter list. Max 64 characters. |
| GL-N12 | Reference | `CON-2026-7890` | Saved as the subject reference. Searchable (GL-F1 style). Max 64. |
| GL-N13 | Related obligation | `OB-2026-1123` | Shown in Details. Max 64. |
| GL-N14 | Tags | `Vendor, Contract, Renewal, vendor` | Three tags (duplicates dropped, case-insensitive). |
| GL-N15 | Tags | 11 tags | Refused: "at most 10 tags". |
| GL-N16 | Tags | one tag of 33+ characters | Refused: "each tag must be at most 32 characters". |
| GL-N17 | Decided | blank | Decision time = now. |
| GL-N18 | Decided | a past date (e.g. 2026-11-15 10:00) | Saved with that time. It falls inside "Last 30 days". |
| GL-N19 | Evaluation context (JSON) | `{"amount": 12000}` | Saved. Visible in the Evidence pane or on the full page. |
| GL-N20 | Evaluation context (JSON) | `{amount: 12000` | "Evaluation context must be valid JSON, or left blank." Nothing is written. |
| GL-N21 | Decision (title) 201+ chars, Summary 2001+ chars | long text | The inputs stop at 200 and 2000. |
| GL-N22 | Submit the exact same form twice quickly | — | The second answer: "This exact decision was already recorded as GD-…; nothing new was written." (idempotent) |
| GL-N23 | As hr.director@ (no RECORD grant) | any valid form | Refused (403) with a message naming the missing permission. No row is added. |
| GL-N24 | Immutability | look for edit or delete on any decision | There is none, anywhere. |

---

## 8. Record Outcome / Record Follow-up (in the drawer)

The button reads **Record Outcome** on a Pending or Escalated decision and **Record Follow-up** on the rest.

| # | Input | Expected |
|---|---|---|
| GL-U1 | Open `GD-2026-0259` (ESCALATED). Click Record Outcome. Keep the prefilled title "Resolve: …", choose Outcome Approved and save | "Follow-up recorded as GD-2026-03xx". The new decision shows "Follows up GD-2026-0259". The original's Approvals pane lists it. **The original stays ESCALATED**: the ledger does not change it. |
| GL-U2 | Prefilled fields | Action type and domain are carried over (hidden). Entity, jurisdiction, rule basis, reference, obligation and tags are prefilled from the original and can be changed. |
| GL-U3 | Clear Rule basis | Same refusal as GL-N4. |
| GL-U4 | Record Follow-up on an Approved decision | Works the same way. A chain of follow-ups shows in "Follow-ups". |

---

## 9. Replay (drawer) and the Replays tab

Replay re-evaluates a decision against the **exact policy version** it used, through policy-svc. Only
`APPROVAL_THRESHOLD` decisions that record a policy version and an amount can replay.

| # | Decision to open | Expected |
|---|---|---|
| GL-R1 | `GD-2026-0285` (APPROVAL_THRESHOLD, with a policy version) → Replay → Replay now | "The replay reproduced the recorded outcome…", with original outcome, replayed outcome, Reproduced = Yes, policy version and time. Replays tab count +1. |
| GL-R2 | `GD-2026-0342` (SIGNATORY_MATRIX) | Refused (501): "Replay not implemented for action type: SIGNATORY_MATRIX". No manifest. |
| GL-R3 | `GD-2026-0261` (APPROVAL_THRESHOLD, no policy version) | Refused (400): "Decision records no policy version to replay against…". |
| GL-R4 | The decision you recorded in GL-N1 | Refused (501): your action type cannot be replayed. |
| GL-R5 | Stop policy-svc, then replay GL-R1 again | Error (503) "Policy service unavailable". Start policy-svc again afterwards. |
| GL-R6 | As hr.director@ | Refused (403, no REPLAY grant). |
| GL-R7 | Replays tab | Lists every manifest (12 + yours). |
| GL-R8 | Replays tab → "Show mismatches only" | Only manifests where Reproduced = No. With none: "No replay has failed to reproduce its decision." Click again to show all. |

---

## 10. Evidence chain tab

| # | Input | Expected |
|---|---|---|
| GL-E1 | Open the tab | Shows decisions in the chain, sealed through, not yet sealed and head hash, plus the 36 packages. |
| GL-E2 | **Verify Chain** → Verify | "All N decisions and 36 sealed package(s) re-compute. The ledger is intact." The table lists every package as "Verifies". Nothing is written. |
| GL-E3 | **Seal Package** before recording anything new | Refused (409): every decision is already sealed ("Nothing To Seal"). |
| GL-E4 | Record a decision (GL-N1), then Seal Package with Label `November 2026 close`, a Reason, and through-ID blank | "Package #37 sealed decisions 1–343…", with Merkle root and package hash. Not yet sealed = 0. |
| GL-E5 | Seal Package with through-ID `not-a-real-id` | Refused (404): "through_decision_id is not a decision of this tenant". |
| GL-E6 | Seal Package with through-ID = an already-sealed decision | Refused (409): nothing to seal through that point. |
| GL-E7 | Label 201+ chars / Reason 501+ chars | The inputs stop at 200 and 500. |
| GL-E8 | **Verify Package** on any package | "Package #n verifies: decisions 1–x are exactly as they were when sealed." |
| GL-E9 | **Export Evidence** | Downloads a JSON evidence bundle (attachment). Signed out, it returns 401 JSON. |
| GL-E10 | As hr.director@: Seal Package | Refused (403, needs GOVERNANCE_EVIDENCE_SEAL). Verify needs only READ. |

---

## 11. Access and session

| # | Test | Expected |
|---|---|---|
| GL-A1 | Sign in as hr.director@ and open the page | Reads are refused (403, no GOVERNANCE_DECISION_READ). The page shows an unavailable or refused state, never another tenant's data. |
| GL-A2 | Delete the `zoiko_session` cookie and reload | "Sign in again" (or a redirect to /login). |
| GL-A3 | Call the service directly with no envelope: `curl -i http://127.0.0.1:8083/v1/decisions` | 401. Headers alone are not accepted (`IDENTITY_VERIFY=require`). |

---

## 12. Local-stack notes from 24 Nov (why login was failing)

- **Console login failed for every account.** Commit `2bb4dc43` (20 Nov) set tenant-svc's identity gate to `strict`
  (require). identity-context-svc called the registry over plain HTTP with no envelope, so the context resolve got
  401 and sign-in ended with "Sign-in did not complete" or "The sign-in service isn't reachable".
  **Fix (not committed):** identity-context-svc now calls the registry over mTLS as a registered peer. `strict` stays on.
  - New env `TENANT_REGISTRY_MTLS_URL` (`internal/config/config.go`); `RegistryClient.WithMTLS`
    (`internal/upstream/registry_client.go`); wiring in `cmd/server/main.go`; tests in
    `internal/config/config_registry_mtls_test.go` and `internal/upstream/registry_client_test.go`.
  - `deployments/docker-compose.yml`: identity-svc gets `TENANT_REGISTRY_MTLS_URL=https://tenant-entity-registry-svc:8449`;
    tenant-svc `MTLS_ALLOWED_CLIENTS` adds `identity-context-svc`.
  - Image `deployments-identity-svc:latest` was rebuilt from host binaries (current HEAD plus this fix). The previous
    image (two weeks old) is kept as `deployments-identity-svc:pre-24nov`.
- **Containers created by Kilo on 23 Nov** (`identity-context-svc`, `access-control-svc`: their own env, attached
  only to `deployments-net`, no OIDC client file) were renamed to `identity-context-svc-kilo` and
  `access-control-svc-kilo`, not deleted. The compose services run in their place.
- **zoiko-postgres was stopped at 08:31:30 UTC** by something outside this session (a `docker stop`, fast
  shutdown), then started again.
