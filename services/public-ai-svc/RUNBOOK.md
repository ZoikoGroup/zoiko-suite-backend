# public-ai-svc runbook (Suivo R0)

Adapted from Doc 08 §16–§19 to what this service actually implements. Commands assume the
compose stack in `deployments/docker-compose.public-ai.yml`; in other environments run the same
`kbctl` commands with `PUBLIC_AI_DATABASE_URL` and `PUBLIC_AI_SIGNING_PUBLIC_KEY` set.

**Status: DRAFT.** Owners, on-call rota, paging targets, SLOs and notification timelines are not
assigned (DEC-08, DEC-16, K02). Every step that says *owner* needs a named person before launch.

## Controls at a glance

| Control | How | Takes effect |
|---|---|---|
| Kill switch (whole assistant) | `kbctl flag -name public_ai_enabled -value false -actor <you> -approval <CHG>` | next refresh (≤5 s); turns answer `UNAVAILABLE` with reason `KILL_SWITCH`, `/config` shows `capabilities.chat_enabled=false`. Readiness and `public_ai_release_ready` stay up (a kill is not a release failure, OPR-011) |
| Re-enable after a kill (dual control) | `kbctl flag -name public_ai_enabled -value true -actor <second person> …` | refused if the actor is the one who disabled it (also `model_generation_enabled`, ADM-T046) |
| Remove commercial CTAs | `kbctl flag -name commercial_routes_enabled -value false …` | ≤5 s; answers keep working without demo/trial/contact routes |
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

## Promotion freeze (Doc 08 §13, OPR-021/029)

Alerts labelled `freeze_promotion="true"` (error-budget burns, a revocation caught at commit) mean
no new release may be promoted until the incident is closed. The on-call sets the freeze:
`kbctl flag -name promotion_frozen -value true -actor <you> -approval <incident>`. While it is on,
`kbctl promote` refuses every promotion; `kbctl promote -rollback` still works, because rollback is the
recovery. Clear it with `-value false` and a second approval once the incident is resolved.

Dashboards: the isolated stack's Grafana is `public-ai-grafana` (127.0.0.1:3002), whose only data
source is `public-ai-prometheus`. The "Useful grounded resolution", "tokens per 1,000 useful
resolutions", "Knowledge index health" and "Privacy and revocation integrity" panels read the
recording rules and gauges added for Doc 08 §12/§15/§25 and Doc 03 §22.

## RB-01 Model provider degraded (Doc 08 §16)

Providers: the deterministic mock (dev/test only) and Groq (`MODEL_PROVIDER=groq`, which outside dev/test
needs `PUBLIC_AI_PROVIDER_APPROVAL_REF` and an approved region). `cmd/drill` DR-04 exercises this runbook.

1. **Detect:** rise in `UNAVAILABLE` turn outcomes (`chat.turn.completed.v1` status field) or provider
   timeouts in logs. Do not page on the vendor status page alone.
2. **Triage:** confirm it is the provider (logs show `draft rejected` vs timeouts/errors).
3. **Contain:** the service already answers `UNAVAILABLE` + official route on provider failure. For a
   prolonged outage set `approved_fallback_mode` to `"static"` so signed claim answers continue.
   **Never** switch to an unapproved vendor or region (Doc 06 §23, OPR-028).
4. **Recover:** restore `approved_fallback_mode` to `""`; run the synthetic canary against the environment
   (cited answer, route-only CTA, abstention, private-data refusal):
   `go run ./cmd/canary -base https://<env> -origin https://<allowed origin> [-admin-base http://<admin addr>]`.
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
5. **Post-rollback check:** `go run ./cmd/canary -base https://<env> -origin https://<allowed origin> -out canary-report.json`
   must pass; keep the report with the incident record.
6. Close-out needs two people and a signed record (OPS evidence).
7. While `PublicAIErrorBudgetFastBurn` or `PublicAIErrorBudgetSlowBurn` fires (label `freeze_promotion="true"`),
   promote nothing except the rollback itself.

## RB-05 Dead-lettered events

Alert: `PublicAIEventsDeadLettered`. An outbox event failed repeatedly and was moved to a `.dlq` topic.

1. **Triage:** relay logs for the event type and error; check broker health and topic ACLs.
2. **Contain:** nothing visitor-facing breaks (turns are stored first; events are the outbox), so do not
   kill the assistant for this alone.
3. **Recover:** fix the cause, then replay with `kbctl dlq-replay` (an audited control action, CT-042).
4. **Verify:** `public_ai_events_dead_lettered_total` stops rising and `PublicAIOutboxLag` clears.

## RB-06 Spend ceiling

Alerts: `PublicAISpendHalf` (50 %), `PublicAISpendHigh` (80 %), `PublicAISpendCeilingReached` (100 %),
`PublicAITokenBurnHigh`.

1. **Triage:** `public_ai_spend_ratio{window}` and `public_ai_model_tokens_total` on the dashboard; check
   `PublicAIRateLimitingSurge` / `PublicAISecuritySignalSurge` for abuse (then RB-08).
2. **At 100 %** the spend guard already serves static content only (reason `SPEND_CEILING`); nothing to do
   to stop the spend. Do **not** raise the ceiling without the budget owner's approval (Doc 04 §25).
3. **Contain earlier if needed:** `approved_fallback_mode="static"` or lower `max_concurrent_turns`.
4. **Close:** record the cause (traffic, abuse, prompt change) and any ceiling change with its approval.

## RB-07 Outbox lag

Alert: `PublicAIOutboxLag` (oldest pending event > 10 min for 10 min).

1. **Triage:** is the relay running (logs), is the broker reachable, is the relay breaker open?
2. **Contain:** visitors are unaffected; downstream analytics and audit consumers are delayed.
3. **Recover:** restore the broker or relay; events are kept in the outbox and drain in order.
4. **Verify:** `public_ai_outbox_oldest_pending_seconds` falls below 60 s. Persistent failures become
   dead letters (RB-05).

## RB-08 Security signals and abuse

Alerts: `PublicAISecuritySignalSurge`, `PublicAIDeniedRequests`, `PublicAIRateLimitingSurge`.

1. **Triage:** dashboard *Security signals by class*, *Denied requests* and *Rate limited by scope*. Signals
   carry a reason class only, never visitor text.
2. **Contain probing:** tighten edge rate limits / block at the WAF; the service already refuses injected
   instructions, denied origins and hosts, and caps sessions and in-flight turns.
3. **Escalate to RB-03** if any answer leaked private data or followed an injected instruction, or if a
   knowledge source is the vector (revoke it).
4. **Close:** record the classes, volumes and edge changes with Security.

## RB-09 Service health and missing telemetry

Alerts: `PublicAIScrapeDown`, `PublicAIMetricsAbsent`, `PublicAIServerErrors`, `PublicAIGateLatencyP95High`.

1. **Triage:** container health (`/health/live`, `/health/startup`, `/health/ready` on the admin listener
   when `PUBLIC_AI_ADMIN_ADDR` is set), recent deploys, database reachability, logs.
2. Silence is never health: a missing series is treated as an outage until shown otherwise.
3. **Recover:** roll back the application image digest (RB-04) if a deploy caused it.
4. **Verify:** `go run ./cmd/canary` passes and the alert clears.

## RB-10 Synthetic canary failing

Alerts: `PublicAICanaryFailing`, `PublicAICanaryStale`. The canary runs on a schedule with
`go run ./cmd/canary -base … -origin … -metrics-file <textfile dir>/public_ai_canary.prom` (node exporter
textfile collector).

1. **Triage:** read `canary-report.json` / `public_ai_canary_step_success{step}`: `ready`, `config`, `session`,
   `citation`, `safe_cta`, `abstain`, `boundary`, `end`.
2. A failing `citation`/`abstain`/`boundary` step is a behaviour change: compare with the last release
   (RB-04) and the abstain-rate panel (RB-02). A failing `ready`/`session` step is RB-09.
3. `PublicAICanaryStale` means the scheduler or the textfile collector stopped: fix the job, not the service.

## RB-11 Rotating secrets and keys

Doc 06 §12. Rotate on schedule (set by Security, DEC-09), on staff departure, and at once on any
suspected exposure. Every rotation is a change with an approval reference; record it with
`kbctl audit` and check `kbctl audit -verify` afterwards.

| Secret | Where | Rotation |
|---|---|---|
| Release signing key (Ed25519) | private key offline with the release signers; public key in `PUBLIC_AI_SIGNING_PUBLIC_KEY` | Below. Tested by `release.TestSigningKeyRotation`. |
| Model provider key (`GROQ_API_KEY`) | secret manager → service env | Issue the new key at the provider, deploy it, confirm `public_ai_model_requests_total{outcome="ok"}` rises, then revoke the old key at the provider. |
| Database password (`PUBLIC_AI_DB_PASSWORD`) | secret manager → compose / service env | `ALTER ROLE public_ai_app PASSWORD …` in a transaction, deploy the new value, restart replicas one by one (each reconnects), confirm readiness. |
| Rate-limit salt (`PUBLIC_AI_RATE_KEY_SALT`) | secret manager → every replica | Deploy the same new value to all replicas together. Counts restart from zero for one minute; nothing else changes. |
| Policy digest (`PUBLIC_AI_POLICY_DIGEST`) | release record | Not a secret: it changes with an approved policy bundle (RB-04). |

Session grants are random tokens with no signing key: ending every session (`kbctl` maintenance or a
restart with an empty session table) is the only "rotation", and visitors simply start a new chat.

**Release signing key:**

1. `kbctl keygen` on the signers' offline machine. Store the new private key under the same custody
   as the old one (DEC-09).
2. Re-sign the current content as **new release ids**: releases are immutable (migration 000007), so
   `kbctl build-kb`/`build-claims` with the new key, then
   `PUBLIC_AI_SIGNING_PUBLIC_KEY=<new public key> kbctl publish -file …` (publish verifies with that key).
3. `kbctl promote` the new KB and claims releases. Running replicas (old key) refuse them and keep
   serving the last good snapshot: this is expected for the few minutes of the switch.
4. Deploy `PUBLIC_AI_SIGNING_PUBLIC_KEY=<new public key>` and restart replicas. Readiness returns once the
   rotated releases load.
5. Retire the old private key. Rolling back to a release signed with it now fails closed; to roll
   back content, re-sign the older content with the new key (step 2) and promote that.

## Alert → runbook map

Every alert in `deployments/prometheus-rules-public-ai.yml` carries `runbook` and `runbook_url` annotations.
Dashboard: `deployments/grafana/provisioning/dashboards/public-ai-svc.json`.

| Alert | Severity | Runbook |
|---|---|---|
| PublicAIUnavailableTurns, PublicAIModelErrors, PublicAIAnswerLatencyP95High | page / ticket | RB-01 |
| PublicAIErrorBudgetFastBurn (14.4×), PublicAIErrorBudgetSlowBurn (6×) | page, freeze promotion | RB-01 (then RB-04) |
| PublicAIErrorBudgetGradualBurn (1× over 3 d) | ticket | RB-01 |
| PublicAIClaimConflictTurns, PublicAIExpiredSourceTurns, PublicAIAbstainRateShift | warn / ticket | RB-02 |
| PublicAIValidationRejectionsSpike | ticket | RB-03 |
| PublicAIReleaseNotReady | page | RB-04 |
| PublicAIEventsDeadLettered | ticket | RB-05 |
| PublicAISpendHalf, PublicAISpendHigh, PublicAISpendCeilingReached, PublicAITokenBurnHigh | ticket / warn / page | RB-06 |
| PublicAIOutboxLag | warn | RB-07 |
| PublicAISecuritySignalSurge, PublicAIDeniedRequests, PublicAIRateLimitingSurge | warn / ticket | RB-08 |
| PublicAIScrapeDown, PublicAIMetricsAbsent, PublicAIServerErrors, PublicAIGateLatencyP95High | page / ticket | RB-09 |
| PublicAICanaryFailing, PublicAICanaryStale | page / ticket | RB-10 |

The kill switch (`KILL_SWITCH`) is excluded from the error budget: it is an operator decision.
Rules are unit-tested: `promtool test rules deployments/prometheus-rules-public-ai.test.yml` (CI).

## Drills

```bash
go run ./cmd/drill -out drill-report.json                       # in-memory
PUBLIC_AI_DATABASE_URL=postgres://…/drilldb go run ./cmd/drill -reset-drill-db -out drill-report.json
```

`-reset-drill-db` **drops the public_ai schema** of the target database: dedicated drill databases only.
Drills: DR-01 kill switch (with dual-control re-enable), DR-02 claim revocation, DR-03 rollback without
resurrection, DR-04 provider outage, DR-05 safe-static, DR-06 restore from signed release files (signatures,
manifest digests, revocations still applied), DR-07 prompt injection in a knowledge source (quarantined at
ingest, never retrieved). CI runs them on every change and keeps `drill-report.json`. Repeat them on staging
against the deployed artifacts before launch.

## Release record

```bash
go run ./cmd/evalrun -out eval-report.json
go run ./cmd/drill -out drill-report.json
kbctl manifest -id suivo-r0-YYYYMMDD-NN -git <commit> -env staging \
  -image <registry>/public-ai-svc@sha256:<64 hex> \
  -eval eval-report.json -drill drill-report.json -sbom sbom.cdx.json -scan trivy.json \
  [-previous-kb <id> -previous-claims <id>] -out release-manifest.json
```

The manifest is signed, lists the four artifact versions and digests, the environment, the release ids it
replaces and the revocation watermark, and records each assurance run as the sha256 of its evidence file or
`NOT_RECORDED`. `model_profile` is derived from the configured provider and model id
(`PUBLIC_AI_MODEL_PROVIDER` / `PUBLIC_AI_MODEL_ID` / `GROQ_MODEL`, e.g. `groq:openai/gpt-oss-120b`); a manifest
whose eval report was run with another profile is refused. It always starts with every approval `PENDING`
and decision `NO_GO_UNTIL_SIGNED`.
