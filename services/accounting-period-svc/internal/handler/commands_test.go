package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/service"
)

var cmdNames = map[domain.Command]string{
	domain.CmdSoftClose: "request-soft-close", domain.CmdHardClose: "hard-close",
	domain.CmdAuthorizeReopen: "authorize-reopen", domain.CmdReclose: "reclose",
}

// The complete (state x command) table, executed through the real HTTP API and
// store: legal pairs succeed and land in the documented state; EVERY other pair
// is INVALID_TRANSITION and changes nothing.
func TestStateMachine_EveryStateCommandPair(t *testing.T) {
	legal := map[domain.State]map[domain.Command]domain.State{
		domain.StateOpen:             {domain.CmdSoftClose: domain.StateSoftClosed},
		domain.StateSoftClosed:       {domain.CmdHardClose: domain.StateHardClosed},
		domain.StateHardClosed:       {domain.CmdAuthorizeReopen: domain.StateReopenAuthorized},
		domain.StateReopenAuthorized: {domain.CmdReclose: domain.StateReclosed},
		domain.StateReclosed:         {domain.CmdAuthorizeReopen: domain.StateReopenAuthorized},
	}
	pairs := 0
	for _, from := range domain.AllStates {
		for _, c := range domain.StateCommands {
			pairs++
			t.Run(string(from)+"/"+string(c), func(t *testing.T) {
				e := newEnv(t)
				res := e.materialize("v1", nil)
				id := e.periodByKey(res, "FY2026-P01").PeriodID
				e.drive(id, from)
				before := e.get(id)
				histBefore := len(e.store.History())
				eventsBefore := len(e.store.Outbox())

				actor := approver
				if c == domain.CmdSoftClose {
					actor = closer
				}
				rec := e.cmd(id, cmdNames[c], actor)
				if to, ok := legal[from][c]; ok {
					got := e.must(rec)
					assert.Equal(t, to, got.State)
					assert.Equal(t, before.Version+1, got.Version)
					assert.Equal(t, histBefore+1, len(e.store.History()))
					assert.Equal(t, eventsBefore+1, len(e.store.Outbox()))
					return
				}
				e.expectErr(rec, http.StatusConflict, domain.CodeInvalidTransition)
				after := e.get(id)
				assert.Equal(t, before.State, after.State)
				assert.Equal(t, before.Version, after.Version)
				assert.Equal(t, histBefore, len(e.store.History()), "a refused command writes no history")
				assert.Equal(t, eventsBefore, len(e.store.Outbox()), "and no event")
			})
		}
	}
	assert.Equal(t, 5*4, pairs)
}

func TestStateMachine_DomainTableMatchesAndSoftCloseToOpenDoesNotExist(t *testing.T) {
	for _, from := range domain.AllStates {
		for _, c := range domain.StateCommands {
			to, err := domain.Next(c, from)
			if err == nil {
				assert.NotEqual(t, from, to, "no self-transitions")
				continue
			}
			de, ok := domain.AsError(err)
			require.True(t, ok)
			assert.Equal(t, domain.CodeInvalidTransition, de.Code)
		}
	}
	for _, c := range domain.StateCommands {
		to, err := domain.Next(c, domain.StateSoftClosed)
		if err == nil {
			assert.NotEqual(t, domain.StateOpen, to, "SOFT_CLOSED -> OPEN is not a spec transition")
		}
	}
	_, err := domain.Next("NOPE", domain.StateOpen)
	assert.Error(t, err)
	_, err = domain.Next(domain.CmdSoftClose, "BOGUS")
	assert.Error(t, err)
}

func TestFullLifecycleIncludingSecondReopen(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
	e.drive(id, domain.StateReclosed)
	e.must(e.cmd(id, "authorize-reopen", approver)) // a later authorised reopen
	e.must(e.cmd(id, "reclose", approver))
	assert.Equal(t, domain.StateReclosed, e.get(id).State)
	types := e.outboxTypes()
	assert.Equal(t, 1, countType(types, "PeriodSoftClosed"))
	assert.Equal(t, 1, countType(types, "PeriodHardClosed"))
	assert.Equal(t, 2, countType(types, "PeriodReopened"))
	assert.Equal(t, 2, countType(types, "PeriodReclosed"))
}

// Negative path 30: a direct hard-close without ACC-14 evidence is rejected.
func TestNegativePath30_DirectHardCloseWithoutEvidenceRejected(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
	e.drive(id, domain.StateSoftClosed)
	before := e.get(id)

	for name, mod := range map[string]func(map[string]any){
		"no refs at all":      func(b map[string]any) { delete(b, "acc14_workflow_ref"); delete(b, "control_snapshot_ref") },
		"no workflow ref":     func(b map[string]any) { delete(b, "acc14_workflow_ref") },
		"no control snapshot": func(b map[string]any) { delete(b, "control_snapshot_ref") },
		"empty strings":       func(b map[string]any) { b["acc14_workflow_ref"] = ""; b["control_snapshot_ref"] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			callsBefore := len(e.prov.calls)
			rec := e.cmd(id, "hard-close", approver, mod)
			e.expectErr(rec, http.StatusUnprocessableEntity, domain.CodeContextInvalid)
			assert.Equal(t, before.State, e.get(id).State)
			assert.Equal(t, callsBefore, len(e.prov.calls), "no point asking ACC-14 about a missing ref")
		})
	}
	assert.Equal(t, 4, countType(e.outboxTypes(), "PeriodCommandRejected"), "each refusal leaves a control event")
}

// Negative path 31: reopen without an authorised workflow -> reject AND a
// security/control event.
func TestNegativePath31_ReopenWithoutWorkflowRejectedWithControlEvent(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
	e.drive(id, domain.StateHardClosed)
	eventsBefore := len(e.store.Outbox())

	rec := e.cmd(id, "authorize-reopen", approver, func(b map[string]any) { delete(b, "acc14_workflow_ref") })
	e.expectErr(rec, http.StatusUnprocessableEntity, domain.CodeContextInvalid)
	assert.Equal(t, domain.StateHardClosed, e.get(id).State)

	out := e.store.Outbox()
	require.Len(t, out, eventsBefore+1)
	last := out[len(out)-1]
	assert.Equal(t, "PeriodCommandRejected", last.EventType)
	assert.Equal(t, id, last.ObjectID)
	var env struct {
		TenantID string
		Payload  map[string]any `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(last.Payload, &env))
	assert.Equal(t, "AUTHORIZE_REOPEN", env.Payload["command"])
	assert.Equal(t, "CONTEXT_INVALID", env.Payload["rejection_code"])
	assert.Equal(t, approver, env.Payload["actor"])
	assert.Equal(t, "FY2026-P01", env.Payload["period_key"])
	assert.Equal(t, "HARD_CLOSED", env.Payload["state"])
}

func TestProvenance_FailureModesAllRejectAndRecordControlEvent(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
		code   domain.Code
	}{
		"ACC-14 does not recognise the workflow": {domain.Errf(domain.CodeSourceUnverified, "unknown workflow"), http.StatusUnprocessableEntity, domain.CodeSourceUnverified},
		"ACC-14 unreachable":                     {domain.Errf(domain.CodeDependencyUnavailable, "down"), http.StatusServiceUnavailable, domain.CodeDependencyUnavailable},
		"verifier returns an untyped error":      {assert.AnError, http.StatusServiceUnavailable, domain.CodeDependencyUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
			e.drive(id, domain.StateSoftClosed)
			before := e.get(id)
			histBefore := len(e.store.History())

			e.prov.err = tc.err
			rec := e.cmd(id, "hard-close", approver)
			e.expectErr(rec, tc.status, tc.code)
			after := e.get(id)
			assert.Equal(t, before.State, after.State, "fails closed: the state did not change")
			assert.Equal(t, before.Version, after.Version)
			assert.Equal(t, histBefore, len(e.store.History()))
			assert.Equal(t, 1, countType(e.outboxTypes(), "PeriodCommandRejected"))

			// ACC-14 recovers: the same period can now be closed.
			e.prov.err = nil
			assert.Equal(t, domain.StateHardClosed, e.must(e.cmd(id, "hard-close", approver)).State)
		})
	}
}

func TestProvenance_SuccessRecordsRefsInHistoryAndEvent(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P02").PeriodID
	e.must(e.cmd(id, "request-soft-close", closer))
	rec := e.cmd(id, "hard-close", approver, func(b map[string]any) {
		b["acc14_workflow_ref"], b["control_snapshot_ref"] = "WF-2026-02-HARD", "SNAP-9"
		b["reason"] = "month-end certified"
	})
	e.must(rec)

	// The verifier was asked about exactly this period, entity, command and refs.
	last := e.prov.calls[len(e.prov.calls)-1]
	assert.Equal(t, service.ProvenanceRequest{TenantID: "tenant-a", LegalEntityID: entityA, PeriodKey: "FY2026-P02",
		Command: domain.CmdHardClose, Acc14WorkflowRef: "WF-2026-02-HARD", ControlSnapshotRef: "SNAP-9"}, last)

	hist := decode[struct {
		Items []domain.HistoryEntry `json:"items"`
	}](t, e.do(http.MethodGet, "/v1/accounting-periods/"+id+"/state-history", nil)).Items
	require.Len(t, hist, 3, "materialize, soft close, hard close")
	h := hist[2]
	assert.Equal(t, domain.CmdHardClose, h.Command)
	assert.Equal(t, domain.StateSoftClosed, h.FromState)
	assert.Equal(t, domain.StateHardClosed, h.ToState)
	assert.Equal(t, "WF-2026-02-HARD", h.Acc14WorkflowRef)
	assert.Equal(t, "SNAP-9", h.ControlSnapshotRef)
	assert.Equal(t, approver, h.RequestedBy)
	assert.Equal(t, "month-end certified", h.Reason)
	assert.Equal(t, int64(2), h.ExpectedVersion)
	assert.Equal(t, int64(3), h.ResultingVersion)
	assert.Len(t, h.DecisionFingerprint, 64)
	assert.Equal(t, domain.CmdMaterialize, hist[0].Command)

	out := e.store.Outbox()
	var env map[string]any
	require.NoError(t, json.Unmarshal(out[len(out)-1].Payload, &env))
	assert.Equal(t, "PeriodHardClosed", env["event_type"])
	p := env["payload"].(map[string]any)
	for _, k := range []string{"tenant_id", "object_id", "object_version", "effective_at", "recorded_at", "actor", "correlation_id", "acc14_workflow_ref"} {
		assert.Contains(t, p, k)
	}
	assert.Equal(t, "WF-2026-02-HARD", p["acc14_workflow_ref"])
	assert.Equal(t, float64(3), p["object_version"])
	assert.Equal(t, "corr-test-1", p["correlation_id"])
}

func TestDecisionFingerprint_AWorkflowDecisionAppliesOnce(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
	e.drive(id, domain.StateHardClosed)
	// Reuse the exact workflow/snapshot refs of the reopen across a second reopen cycle.
	set := func(b map[string]any) { b["acc14_workflow_ref"], b["control_snapshot_ref"] = "wf-reuse", "snap-reuse" }
	e.must(e.cmd(id, "authorize-reopen", approver, set))
	e.must(e.cmd(id, "reclose", approver))
	rec := e.cmd(id, "authorize-reopen", approver, set)
	// from-state differs (RECLOSED vs HARD_CLOSED) so the fingerprint differs: allowed.
	e.must(rec)
	e.must(e.cmd(id, "reclose", approver, func(b map[string]any) { b["acc14_workflow_ref"], b["control_snapshot_ref"] = "wf-r2", "s2" }))
	// Reusing the very same reclose decision (same from-state REOPEN_AUTHORIZED, same refs) is refused.
	e.must(e.cmd(id, "authorize-reopen", approver, func(b map[string]any) { b["acc14_workflow_ref"], b["control_snapshot_ref"] = "wf-r3", "s3" }))
	e.expectErr(e.cmd(id, "reclose", approver, func(b map[string]any) { b["acc14_workflow_ref"], b["control_snapshot_ref"] = "wf-r2", "s2" }),
		http.StatusUnprocessableEntity, domain.CodeContextInvalid)
}

func TestSoD_HardCloseAndReopenNeedSomeoneOtherThanTheSoftCloseRequester(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
	e.must(e.cmd(id, "request-soft-close", closer))

	rec := e.cmd(id, "hard-close", closer)
	e.expectErr(rec, http.StatusForbidden, domain.CodeSoDDenied)
	assert.Equal(t, domain.StateSoftClosed, e.get(id).State)
	assert.Equal(t, 1, countType(e.outboxTypes(), "PeriodCommandRejected"), "SoD refusal is a control event")

	e.must(e.cmd(id, "hard-close", approver))
	rec = e.cmd(id, "authorize-reopen", closer)
	e.expectErr(rec, http.StatusForbidden, domain.CodeSoDDenied)
	assert.Equal(t, domain.StateHardClosed, e.get(id).State)
	assert.Equal(t, 2, countType(e.outboxTypes(), "PeriodCommandRejected"))

	// A different actor may reopen; reclose carries no SoD rule (spec names hard close / reopen).
	e.must(e.cmd(id, "authorize-reopen", approver))
	e.must(e.cmd(id, "reclose", closer))
}

func TestVersionConflictAndMissingExpectedVersion(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
	rec := e.cmd(id, "request-soft-close", closer, func(b map[string]any) { b["expected_version"] = 7 })
	e.expectErr(rec, http.StatusConflict, domain.CodeVersionConflict)
	assert.Equal(t, domain.StateOpen, e.get(id).State)

	rec = e.cmd(id, "request-soft-close", closer, func(b map[string]any) { delete(b, "expected_version") })
	e.expectErr(rec, http.StatusBadRequest, domain.CodeContextInvalid)

}

func TestIdempotentReplayReturnsOriginalAndWritesNothingNew(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
	body := map[string]any{"expected_version": 1, "reason": "r", "acc14_workflow_ref": "w1", "control_snapshot_ref": "s1"}
	first := e.do(http.MethodPost, "/v1/accounting-periods/"+id+":request-soft-close", body, opts{actor: closer, key: "K1"})
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	hist, ev := len(e.store.History()), len(e.store.Outbox())
	calls := len(e.prov.calls)

	replay := e.do(http.MethodPost, "/v1/accounting-periods/"+id+":request-soft-close", body, opts{actor: closer, key: "K1"})
	require.Equal(t, http.StatusOK, replay.Code)
	assert.Equal(t, "true", replay.Header().Get("Idempotent-Replay"))
	assert.JSONEq(t, first.Body.String(), replay.Body.String())
	assert.Equal(t, hist, len(e.store.History()))
	assert.Equal(t, ev, len(e.store.Outbox()))
	assert.Equal(t, calls, len(e.prov.calls), "a replay does not call ACC-14 again")

	// Same key, different request: refused.
	body2 := map[string]any{"expected_version": 1, "reason": "different", "acc14_workflow_ref": "w1", "control_snapshot_ref": "s1"}
	rec := e.do(http.MethodPost, "/v1/accounting-periods/"+id+":request-soft-close", body2, opts{actor: closer, key: "K1"})
	e.expectErr(rec, http.StatusUnprocessableEntity, domain.CodeContextInvalid)

	// Missing Idempotency-Key and missing reason.
	rec = e.do(http.MethodPost, "/v1/accounting-periods/"+id+":request-soft-close", body, opts{actor: closer, noKey: true})
	e.expectErr(rec, http.StatusBadRequest, domain.CodeContextInvalid)
	rec = e.do(http.MethodPost, "/v1/accounting-periods/"+id+":request-soft-close", map[string]any{"expected_version": 2}, opts{actor: closer})
	e.expectErr(rec, http.StatusBadRequest, domain.CodeContextInvalid)
}

func TestAuthz_OrdinaryUsersCannotMutateState(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
	e.authz.deny["PERIOD_STATE_COMMAND"] = errDenied()
	rec := e.cmd(id, "request-soft-close", "ordinary-user")
	e.expectErr(rec, http.StatusForbidden, domain.CodeForbidden)
	assert.Equal(t, domain.StateOpen, e.get(id).State)
	assert.Empty(t, e.prov.calls, "no ACC-14 call for an unauthorised caller")
	assert.Equal(t, entityA, e.authz.calledEnts[len(e.authz.calledEnts)-1], "authorised against the period's legal entity")

	e.authz.deny["PERIOD_STATE_COMMAND"] = domain.ErrAuthzServiceUnavailable
	rec = e.cmd(id, "request-soft-close", closer)
	e.expectErr(rec, http.StatusServiceUnavailable, domain.CodeDependencyUnavailable)
	assert.Equal(t, domain.StateOpen, e.get(id).State)
}

func TestAuthorizeReopen_MustBeTimeBoundAndScoped(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
	e.drive(id, domain.StateHardClosed)
	at := func(d time.Duration) func(map[string]any) {
		return func(b map[string]any) { b["expires_at"] = e.now.Add(d).Format(time.RFC3339) }
	}
	cases := map[string]func(map[string]any){
		"no expires_at":         func(b map[string]any) { delete(b, "expires_at") },
		"no reopen_scope":       func(b map[string]any) { delete(b, "reopen_scope") },
		"expires in the past":   at(-time.Minute),
		"expires right now":     at(0),
		"beyond the max window": at(73 * time.Hour),
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			rec := e.cmd(id, "authorize-reopen", approver, mod)
			e.expectErr(rec, http.StatusUnprocessableEntity, domain.CodeContextInvalid)
			assert.Equal(t, domain.StateHardClosed, e.get(id).State)
		})
	}
	// The maximum is inclusive.
	got := e.must(e.cmd(id, "authorize-reopen", approver, at(72*time.Hour)))
	require.NotNil(t, got.Reopen)
	assert.True(t, got.Reopen.ExpiresAt.Equal(e.now.Add(72*time.Hour)))
}

func TestAuthorizeReopen_ScopeMayOnlyNarrowThePeriodScope(t *testing.T) {
	e := newEnv(t)
	res := e.materialize("v1", map[string]any{"book_scope": "BOOK-1"})
	id := e.periodByKey(res, "FY2026-P01").PeriodID
	e.drive(id, domain.StateHardClosed)
	rec := e.cmd(id, "authorize-reopen", approver, func(b map[string]any) {
		b["reopen_scope"] = map[string]any{"book_scope": "BOOK-2"}
	})
	e.expectErr(rec, http.StatusUnprocessableEntity, domain.CodeContextInvalid)
	got := e.must(e.cmd(id, "authorize-reopen", approver, func(b map[string]any) {
		b["reopen_scope"] = map[string]any{"book_scope": "BOOK-1", "module_scope": "AP"}
	}))
	assert.Equal(t, "AP", got.Reopen.ModuleScope)
}

func TestRecloseClearsTheReopenWindowButHistoryKeepsIt(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
	e.drive(id, domain.StateReclosed)
	assert.Nil(t, e.get(id).Reopen)
	var withWindow int
	for _, h := range e.store.History() {
		if h.PeriodID == id && h.Reopen != nil {
			withWindow++
		}
	}
	assert.Equal(t, 1, withWindow)
}

func TestOutboxAtomicity_FailedEventRollsBackTheStateChange(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
	e.store.FailEnqueue = true
	rec := e.cmd(id, "request-soft-close", closer)
	e.expectErr(rec, http.StatusServiceUnavailable, domain.CodeDependencyUnavailable)
	e.store.FailEnqueue = false
	after := e.get(id)
	assert.Equal(t, domain.StateOpen, after.State)
	assert.Equal(t, int64(1), after.Version)
	assert.Len(t, e.store.History(), 12, "only the 12 materialisation rows: no history for the failed command")

	// And materialise is atomic too: nothing is half-created.
	e2 := newEnv(t)
	e2.store.FailEnqueue = true
	rec = e2.do(http.MethodPost, "/v1/accounting-periods:materialize",
		map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026, "calendar_version_id": "v1", "reason": "r"})
	e2.expectErr(rec, http.StatusServiceUnavailable, domain.CodeDependencyUnavailable)
	assert.Empty(t, e2.store.Periods())
	assert.Empty(t, e2.store.History())
}

func TestUnknownCommandAndPeriod(t *testing.T) {
	e := newEnv(t)
	id := e.periodByKey(e.materialize("v1", nil), "FY2026-P01").PeriodID
	rec := e.do(http.MethodPost, "/v1/accounting-periods/"+id+":unlock", map[string]any{"reason": "x"})
	e.expectErr(rec, http.StatusNotFound, domain.CodeNotFound)
	rec = e.do(http.MethodPost, "/v1/accounting-periods/"+id, map[string]any{"reason": "x"})
	e.expectErr(rec, http.StatusNotFound, domain.CodeNotFound)
	rec = e.do(http.MethodPost, "/v1/accounting-periods/018f0000-0000-7000-8000-000000000000:hard-close",
		map[string]any{"reason": "x", "expected_version": 1, "acc14_workflow_ref": "w", "control_snapshot_ref": "s"})
	e.expectErr(rec, http.StatusNotFound, domain.CodeNotFound)
	rec = e.do(http.MethodPost, "/v1/accounting-periods/not-a-uuid:hard-close",
		map[string]any{"reason": "x", "expected_version": 1})
	e.expectErr(rec, http.StatusNotFound, domain.CodeNotFound)
}
