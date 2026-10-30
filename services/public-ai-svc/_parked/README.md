# Parked work (not built)

These files were uncommitted work whose matching changes to the tracked code
were never saved, so the service did not compile. They are kept here, at their
original paths under `_parked/`, so the build and tests run again. Go ignores
directories that start with `_`. Move a file back once the code it needs exists.

| File | Needs |
|---|---|
| `internal/release/gate.go` | `release.Operator` type (actor, approval) |
| `internal/release/conflicts.go` | `claims.Claim.OwnerID`, `claims.Manifest.Conflicts()`, `store.Conflict.BlockedClaim` / `AssignedOwner` |
| `internal/events/replay.go`, `replay_test.go` | `sourceOf(type)`, per-producer sources (`sourceControl`), `KillSwitchChanged`, `Backoff`, UUIDv4 event IDs |
| `internal/session/session_test.go` | `Service.WithIdleTTL` and last-activity tracking |
| `internal/policy/fold_test.go` | `Redaction.Contacts` |
| `internal/flags/flags_test.go` | `flags.SecureLeadIntakeEnabled` |
| `internal/eval/doc07_test.go` | `eval.RunWith` |
| `cmd/server/main_test.go` | `shutdownTimeout` in `cmd/server` |
| `internal/acceptance/doc02_scripts_test.go` | Doc 02 script behaviour not implemented yet (S04, S05, S07, S08, S10, market reuse, acronyms, secondary-intent chip) |
| `internal/acceptance/doc03_kb_test.go`, `doc04_test.go` | `store.Store.Conflicts`, the promotion gate and conflict cases above |
| `internal/metrics/`, `internal/httpapi/metrics_test.go` | Prometheus module in `go.mod` and a `/metrics` route |
| `deployments/migrations/000004`–`000006` | the control audit, conflict owner and session activity code above |
