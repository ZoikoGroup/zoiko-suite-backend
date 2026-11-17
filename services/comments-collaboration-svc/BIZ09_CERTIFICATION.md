# BIZ-09 Comments & Collaboration — Certification Status

**Service host:** comments-collaboration-svc (its own service; tables `comment_threads`, `comments`, `comment_versions`, `mentions`, `reactions`, `comment_attachments`, `moderation_trail`, `idempotency_keys`)
**Assessed against:** `ZoikoSuite_Business_Operations_Content_Services_Detailed_Service_Specifications_v1_0` (ZS-SVC-P-001) section 11
**Status: NOT CERTIFIED.** The command and query set is implemented, with earlier remediation already in place (read authorization, restricted threads on reads, classification, idempotency, uniform mention refusal, retention-hold check). Four defects were found and fixed in this audit, all in who may do what. Events are not on the shared outbox, and several spec rows are open.

A certificate listing only successes certifies nothing, so the last two sections say what was **not** verified.

## Contract coverage (spec section 11)

| Spec row | State | Where |
|---|---|---|
| Commands: AddComment, EditComment, DeleteComment, Mention, React, Moderate, ResolveThread, AttachReference | All implemented (no CreateThread: AddComment finds or creates the thread, and reopens a resolved one) | `internal/handler/handler.go` |
| Queries: GetThread, ListComments, GetCommentHistory, GetMentions, GetModerationTrail | All implemented, each authorized on the thread's legal entity: `COMMENT_READ`, or `COMMENT_READ_RESTRICTED` for a restricted thread | handler |
| Lifecycle Active / Edited / Resolved / Moderated / Deleted-Redacted; original history preserved | Comment: ACTIVE, EDITED, MODERATED, DELETED_REDACTED. Thread: ACTIVE, RESOLVED. Versions, mentions, attachments and the moderation trail are append-only by trigger. **Deleted comments are now redacted from the ordinary read** (below) | `internal/store/pg_store.go`, migration 000001 |
| Authorization / SoD | `COMMENT_CREATE`, `COMMENT_MANAGE`, `COMMENT_READ`, `COMMENT_READ_RESTRICTED`, and **`COMMENT_MODERATE` (new)**. Fixed in this audit: author-only edit, moderation apart from participation, no self-moderation, restricted threads for writes (below) | handler |
| Restricted threads need purpose and membership controls | Restricted is a thread-level flag fixed at creation; it requires `COMMENT_READ_RESTRICTED` to read and, now, to write. **No purpose or membership list exists**: the control is a single entity-scoped action, not per-thread membership | handler |
| Events (5) | `CommentAdded`, `CommentEdited`, `CommentModerated`, `MentionCreated`, `ThreadResolved` are published by the handler **after** the commit through a Kafka writer, with the publisher interface returning nothing, so a failure cannot even be reported. Not on the shared eventing outbox, no schemas, no catalogue entries. See open item 1 | `internal/events` |
| Failure: unauthorized mention must not disclose object existence | Enforced: every refusal (unknown comment, not authorized, no visibility checker, mentioned principal cannot see the object) returns the same 403 body, and no row is written. With no checker registered for an object type every mention is refused | `Mention`, `TestMention_AllRefusalsAreIndistinguishable` |
| Failure: delete under hold/retention becomes restricted/redacted, not destruction | Delete never destroys (the comment moves to DELETED_REDACTED, the history is kept). Under a legal hold the delete is **refused outright (409)** rather than turned into restricted treatment; retention-registry-svc being unreachable also refuses (503) | `DeleteComment` |
| Evidence: author, timestamps, original/revised content, object link, mentions, attachments, moderation reason, retention status | Author, timestamps, every version, object link, mentions, attachments, moderation reason and classification are recorded. **Retention status is not recorded** on the comment, only checked at delete time | tables |

## Defects found and fixed in this audit

1. **Anyone with `COMMENT_MANAGE` could edit anyone's comment.** Edit checked only that action, so a comment, which the spec says is attributable, could be rewritten by any participant (the old text survived only in the history). Edit is now author-only (403 otherwise, nothing changed or published). A change to someone else's comment is moderation, which has its own command, mandatory reason and trail.
2. **Moderation was not separate from business authority.** Moderate, delete, react, mention, resolve, attach and edit all needed the same `COMMENT_MANAGE`. Moderate now needs a new action, `COMMENT_MODERATE`, and a moderator cannot moderate their own comment. Deleting someone else's comment needs `COMMENT_MODERATE`; the author deletes their own with `COMMENT_MANAGE`.
3. **Restricted threads could be changed by principals who could not read them.** Reads required `COMMENT_READ_RESTRICTED` on a restricted thread, but edit, delete, react, mention, moderate, resolve and attach did not. A new `checkWrite` requires `COMMENT_READ_RESTRICTED` in addition to the command's own action for those writes. Mention keeps its uniform refusal body.
4. **A deleted comment's body was still returned by `ListComments`.** DELETED_REDACTED was a status only. The ordinary read now returns an empty body for a deleted comment; the deletion reason, actor and time are still shown, and the original text stays in the append-only history.

**The new action `COMMENT_MODERATE` is not registered or granted in authorization-svc.** Until it is, moderating and deleting another principal's comment returns 403 (or 503) in a real deployment, and so does any `COMMENT_MANAGE`-only moderator who relied on the old behaviour.

## Implementation gate (spec: "must pass ... before production certification")

| Gate | Evidence | State |
|---|---|---|
| Authority boundary | Comments hold no business facts; the linked object is an opaque type and id with no foreign key; attachments are references, never bytes. No test asserts it | Partial |
| Tenant isolation | RLS forced on the tables. Reads: `TestPgStore_Reads_AreTenantIsolated` (existing, non-superuser role). Writes: `TestPgStore_Writes_AreTenantIsolated` (added): tenant B cannot edit, delete, mention, react to, moderate, resolve or attach to tenant A's comment and thread; A's data is unchanged; a raw count under B's context sees 0 rows in all seven tables. Mutation-checked: fails with RLS removed from `comments` | Covered, including RLS |
| Classification / retention | Thread classification is required, inherited by comments and survives every change (existing tests); the retention-hold check blocks delete. Retention status is not stored and no retention-policy treatment other than the block exists | Partial |
| Negative paths | Non-author edit, non-moderator delete or moderation, self-moderation, restricted writes, missing reason or action, unauthorized and indistinguishable mentions, retention hold and outage, double delete or resolve, idempotency replay and conflict | Covered |
| Evidence lineage | Versions, moderation trail, mentions, attachments and classification are append-only and queryable; retention status is not | Mostly covered |

## What was verified, and how

```
go build ./... && go vet ./...   clean
go test ./... -count=1           all packages pass with TEST_DATABASE_URL set to a throwaway
                                 postgres:16-alpine container
new tests
  handler: TestEdit_OnlyTheAuthorMayEdit                                      fails with the check removed
  handler: TestDelete_NonAuthorNeedsTheModerationAction                       fails with the check removed
  handler: TestModerate_UsesTheModerationActionAndRefusesSelfModeration       fails with the checks removed
  handler: TestRestrictedThread_WritesRequireRestrictedRead (six write routes) fails with the check removed
  handler: TestRestrictedThread_MentionWithoutRestrictedReadIsTheUniformRefusal
  store:   TestPgStore_ListComments_RedactsADeletedCommentsBody               fails with the redaction removed
  store:   TestPgStore_Writes_AreTenantIsolated                               fails with RLS off
```

Date: 2026-11-17. The Postgres container was disposable and has been removed.

## NOT verified

- No Docker image build or container run, and the docker-compose stack was not run.
- Real authorization-svc decisions for any `COMMENT_*` action (tests use stubs), and that the actions, including the new one, are registered there.
- Real retention-registry-svc and real object-visibility checkers: no checker is registered in the service's `main`, so by design every mention is refused in a real deployment until a domain service registers one. This was not checked in the running service.
- No event was delivered through Kafka; event delivery is not reliable (open item 1).
- A MODERATED comment's body is still returned by the ordinary read: the moderation `action` is free text with no vocabulary, so what a moderation does to visibility is undefined. Only DELETED_REDACTED is redacted.
- `GetCommentHistory` returns every original version, including a deleted comment's, to any holder of the read action on the thread. Whether that needs a stronger action was not decided.

## Open items to reach certification

1. Events can be lost: they are published after the commit by a publisher that cannot report failure. Move the five events to the shared eventing outbox in the same transaction, with canonical names, schemas and catalogue entries, as done for BIZ-01, 02, 03, 06 and 08.
2. Register and grant `COMMENT_MODERATE` (and verify the other `COMMENT_*` actions) in authorization-svc; run the service in the compose stack.
3. Define what a moderation does (an action vocabulary, and whether a MODERATED body is hidden), and who may read a deleted or moderated comment's original history.
4. Restricted threads: purpose and membership controls beyond a single entity-scoped action, if the spec means per-thread membership.
5. Delete under a hold: decide between refusing (current) and the spec's restricted/redacted treatment, and record retention status as evidence.
6. Register a real `ObjectVisibilityChecker` for at least the BIZ-01 document type, so mentions work at all in a deployment.
7. Add an OpenAPI contract for the routes (none exists) and a route-drift test; add an authority-boundary test.
8. Dependencies the spec lists (BIZ-01, BIZ-10 notification of mentions, DATA-GOV, search) are not linked.
