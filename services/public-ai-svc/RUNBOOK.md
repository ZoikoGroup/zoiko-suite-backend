# public-ai-svc runbook (Suivo R0)

Adapted from Doc 08 §16–§19 to what this service actually implements. Commands assume the
compose stack in `deployments/docker-compose.public-ai.yml`; in other environments run the same
`kbctl` commands with `PUBLIC_AI_DATABASE_URL` and `PUBLIC_AI_SIGNING_PUBLIC_KEY` set.

**Status: DRAFT.** Owners, on-call rota, paging targets, SLOs and notification timelines are not
assigned (DEC-08, DEC-16, K02). Every step that says *owner* needs a named person before launch.

## Controls at a glance

| Control | How | Takes effect |
|---|---|---|
| Kill switch (whole assistant) | `kbctl flag -name public_ai_enabled -value false -actor <you> -approval <CHG>` | next refresh (≤5 s); `/health/ready` → 503 |
| Stop generation, serve signed claims only | `kbctl flag -name approved_fallback_mode -value '"static"' …` | ≤5 s |
| Stop all price/trial numbers | `kbctl flag -name pricing_claims_enabled -value false …` | ≤5 s |
| Disable specific intents | `kbctl flag -name per_intent_disable -value '["I05","I06"]' …` | ≤5 s |
| Revoke a source or claim | `kbctl revoke -kind source|claim -id <id> -reason <CODE>` | retrieval ≤5 s; **every answer re-checks the ledger at commit** |
| Roll back knowledge or claims | `kbctl promote -kind kb|claims -id <previous release>` | ≤5 s; revocations still apply |
| Roll back the application | redeploy the previous image digest from the release manifest (never `latest`) | deploy time |

Inside the container: `docker exec public-ai-svc /app/kbctl <command> …` (Git Bash on Windows:
prefix with `MSYS_NO_PATHCONV=1`). Every flag change needs an actor and an approval/change
reference and writes `publicai.kill_switch.changed.v1` to the outbox.

Each artifact rolls back independently (Doc 08 §01): app image, prompt/policy (ships with the app
image; see `artifacts.prompt_policy` in the manifest), knowledge release, claim release.

## RB-01 Model provider degraded (Doc 08 §16)

Today the only provider is the fake model, so this runbook is exercised by `cmd/drill` DR-04.

1. **Detect:** rise in `UNAVAILABLE` turn outcomes (`chat.turn.completed.v1` status field) or provider
   timeouts in logs. Do not page on the vendor status page alone.
2. **Triage:** confirm it is the provider (logs show `draft rejected` vs timeouts/errors).
3. **Contain:** the service already answers `UNAVAILABLE` + official route on provider failure. For a
   prolonged outage set `approved_fallback_mode` to `"static"` so signed claim answers continue.
   **Never** switch to an unapproved vendor or region (Doc 06 §23, OPR-028).
4. **Recover:** restore `approved_fallback_mode` to `""`; verify one factual answer, one P1 price answer
   and one refusal (`go run ./cmd/evalrun` against staging, or the curl checks in README).
5. **Close:** record timeline, flag changes (from the outbox) and evidence.

## RB-02 Stale or wrong commercial claim (Doc 08 §17) — treat as P0

1. **Contain immediately:** `kbctl revoke -kind claim -id <claim_id> -reason <CODE>`. If unsure which
   claim, also set `pricing_claims_enabled=false`.
2. **Verify:** ask the price question; it must abstain. Revocation is enforced at retrieval and again
   at commit, so in-flight answers are also blocked.
3. **Fix:** the commercial owner corrects the registry; build a new claims release with a **new
   claim_id** (revocations are monotonic — a revoked id never comes back), two approvals, then
   `kbctl publish -file … -promote` by a second person.
4. **Never** "fix" by rolling back the app or by deleting a revocation (the database rejects it).

## RB-03 Privacy leak or prompt injection (Doc 08 §18)

1. **Contain:** kill switch (`public_ai_enabled=false`). If a knowledge source is the vector, revoke
   it (`kbctl revoke -kind source`).
2. **Preserve evidence** without copying visitor text: request ids, `chat.security.signal.v1` events,
   release ids from the manifest. Raw prompts are not stored by design; do not enable capture.
3. **Escalate** to Security and DPO. Breach-notification decisions belong to DPO/Legal (72-hour rule
   applies only to reportable breaches, Doc 06 §22).
4. **Fix and re-test:** add the case to `testdata/golden`, re-run `evalrun` and `go test ./...`.
5. **Re-enable** only with Security sign-off.

## RB-04 Rollback and safe-static (Doc 08 §19)

1. Find the last known-good release in the release manifests (`kbctl manifest` output).
2. Knowledge/claims: `kbctl promote -kind kb -id <id>` / `-kind claims -id <id>`. Revocations made
   since still apply (drill DR-03).
3. Application: redeploy the previous image **digest**.
4. If no known-good state exists: `approved_fallback_mode="static"` or the kill switch.
5. Close-out needs two people and a signed record (OPS evidence).

## Drills

```bash
go run ./cmd/drill -out drill-report.json                       # in-memory
PUBLIC_AI_DATABASE_URL=postgres://…/drilldb go run ./cmd/drill -reset-drill-db -out drill-report.json
```

`-reset-drill-db` **drops the public_ai schema** of the target database: dedicated drill databases only.
Drills: DR-01 kill switch, DR-02 claim revocation, DR-03 rollback without resurrection, DR-04 provider
outage, DR-05 safe-static. Repeat them on staging against the deployed artifacts before launch.

## Release record

```bash
go run ./cmd/evalrun -out eval-report.json
kbctl manifest -id suivo-r0-YYYYMMDD-NN -git <commit> -image <image digest> -eval eval-report.json -out release-manifest.json
```

The manifest is signed, lists the four artifact versions and digests, records assurance runs and
always starts with every approval `PENDING` and decision `NO_GO_UNTIL_SIGNED`.
