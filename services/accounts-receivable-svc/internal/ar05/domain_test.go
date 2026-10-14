package ar05

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

func TestStateMachine(t *testing.T) {
	legal := []struct {
		from State
		tr   Transition
		to   State
	}{
		{StatePrepared, TrValidate, StateValidated},
		{StateValidated, TrBeginAttempt, StateUnknown},
		{StateUnknown, TrRetryAttempt, StateUnknown},
		{StateUnknown, TrAck, StateSubmitted},
		{StateUnknown, TrNotSent, StateValidated},
		{StateUnknown, TrAccept, StateAccepted},
		{StateSubmitted, TrAccept, StateAccepted},
		{StateUnknown, TrReject, StateRejected},
		{StateSubmitted, TrReject, StateRejected},
		{StateUnknown, TrReceived, StateSubmitted},
		{StatePrepared, TrCancel, StateCancelled},
		{StateValidated, TrCancel, StateCancelled},
	}
	for _, c := range legal {
		to, err := Next(c.from, c.tr)
		require.NoError(t, err, "%s %s", c.from, c.tr)
		require.Equal(t, c.to, to)
	}
	// Every other (state, transition) pair is refused with INVALID_STATE_TRANSITION.
	states := []State{StatePrepared, StateValidated, StateSubmitted, StateUnknown, StateAccepted, StateRejected, StateCancelled}
	trs := []Transition{TrValidate, TrBeginAttempt, TrRetryAttempt, TrAck, TrNotSent, TrAccept, TrReject, TrReceived, TrCancel}
	allowed := map[string]bool{}
	for _, c := range legal {
		allowed[string(c.from)+"|"+string(c.tr)] = true
	}
	for _, s := range states {
		for _, tr := range trs {
			if allowed[string(s)+"|"+string(tr)] {
				continue
			}
			_, err := Next(s, tr)
			var p *kit.Problem
			require.True(t, errors.As(err, &p), "%s %s", s, tr)
			require.Equal(t, http.StatusConflict, p.Status)
			require.Equal(t, kit.CodeInvalidTransition, p.Code)
		}
	}
}

func TestTerminalStatesNeverLeave(t *testing.T) {
	for _, s := range []State{StateAccepted, StateRejected, StateCancelled} {
		require.True(t, s.Terminal())
		for _, tr := range []Transition{TrValidate, TrBeginAttempt, TrRetryAttempt, TrAck, TrNotSent, TrAccept, TrReject, TrReceived, TrCancel} {
			_, err := Next(s, tr)
			require.Error(t, err, "%s must be final (%s)", s, tr)
		}
	}
	for _, s := range []State{StatePrepared, StateValidated, StateSubmitted, StateUnknown} {
		require.False(t, s.Terminal())
	}
}

func TestDimensionMirror(t *testing.T) {
	require.Equal(t, "PREPARED", StatePrepared.Dimension(true))
	require.Equal(t, "PREPARED", StateValidated.Dimension(true), "VALIDATED is internal to AR-05")
	require.Equal(t, "SUBMITTED", StateSubmitted.Dimension(true))
	require.Equal(t, "PENDING_UNKNOWN", StateUnknown.Dimension(true))
	require.Equal(t, "ACCEPTED", StateAccepted.Dimension(true))
	require.Equal(t, "REJECTED", StateRejected.Dimension(true))
	require.Equal(t, "REQUIRED", StateCancelled.Dimension(true))
	require.Equal(t, "NOT_REQUIRED", StateCancelled.Dimension(false))
}

func TestTimeoutIsNeverTheFailureStates(t *testing.T) {
	// invariant 5: an ambiguous outcome has exactly one home, PENDING_UNKNOWN; no transition leads from "sent" to a retryable state.
	for _, s := range []State{StateSubmitted, StateUnknown} {
		_, err := Next(s, TrNotSent)
		if s == StateSubmitted {
			require.Error(t, err, "once the provider acknowledged, the document is never 'not sent'")
		}
	}
	st := classifyCall(nil, errors.New("context deadline exceeded"))
	require.Equal(t, OutAmbiguous, st.outcome)
	st = classifyCall(nil, errors.New("connection reset by peer"))
	require.Equal(t, OutAmbiguous, st.outcome)
	st = classifyCall(nil, ErrNotSent)
	require.Equal(t, OutNotSent, st.outcome)
	st = classifyCall(nil, nil)
	require.Equal(t, OutAmbiguous, st.outcome, "no answer is not an answer")
	st = classifyCall(&SubmitResult{Disposition: "WHATEVER"}, nil)
	require.Equal(t, OutAmbiguous, st.outcome, "an unknown disposition is ambiguous, not success")
	st = classifyCall(&SubmitResult{Disposition: DispAccepted}, nil)
	require.Equal(t, OutAcknowledged, st.outcome, "ACCEPTED without a receipt is only an acknowledgement")
	require.True(t, st.downgrade)
	st = classifyCall(&SubmitResult{Disposition: DispAccepted, ReceiptID: "r1"}, nil)
	require.Equal(t, OutAccepted, st.outcome)
	st = classifyCall(&SubmitResult{Disposition: DispRejected}, nil)
	require.Equal(t, OutRejected, st.outcome)
}

func TestProfileMatchRequest(t *testing.T) {
	p := defaultProfile()
	p.Jurisdiction = "GB"
	require.NoError(t, p.MatchRequest("GB", DocInvoice, "PEPPOL_INVOICE", "peppol"), "network is compared case-insensitively")
	require.NoError(t, p.MatchRequest("gb", DocInvoice, "", ""))

	bad := map[string]func() error{
		"wrong jurisdiction": func() error { return p.MatchRequest("DE", DocInvoice, "PEPPOL_INVOICE", "") },
		"wrong doc type":     func() error { return p.MatchRequest("GB", DocCreditNote, "PEPPOL_INVOICE", "") },
		"wrong profile":      func() error { return p.MatchRequest("GB", DocInvoice, "OTHER", "") },
		"wrong network":      func() error { return p.MatchRequest("GB", DocInvoice, "PEPPOL_INVOICE", "SDI") },
		"incomplete profile": func() error {
			q := p
			q.Syntax = ""
			return q.MatchRequest("GB", DocInvoice, "", "")
		},
	}
	for name, fn := range bad {
		err := fn()
		var pr *kit.Problem
		require.True(t, errors.As(err, &pr), name)
		require.Equal(t, http.StatusUnprocessableEntity, pr.Status, name)
		require.Equal(t, kit.CodeEInvoiceValidationFailed, pr.Code, name)
	}
}

func TestDependencyWindow(t *testing.T) {
	to := "2026-06-30"
	d := Dependency{Kind: "SCHEMA", Ref: "x", Version: "1", ValidFrom: "2026-01-01", ValidTo: &to}
	require.True(t, d.ValidOn(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	require.True(t, d.ValidOn(time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)))
	require.False(t, d.ValidOn(time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)))
	require.False(t, d.ValidOn(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)))
	require.False(t, Dependency{ValidFrom: "garbage"}.ValidOn(testNow))
}

func TestAvailableActionsFollowState(t *testing.T) {
	require.Contains(t, available(StatePrepared), "validateEInvoice")
	require.Contains(t, available(StateValidated), "submitEInvoice")
	require.Contains(t, available(StateUnknown), "retrySubmission")
	require.Contains(t, available(StateUnknown), "reconcileSubmissionState")
	require.NotContains(t, available(StateUnknown), "submitEInvoice", "a pending-unknown submission can never be submitted blindly")
	require.Empty(t, available(StateAccepted))
	require.Equal(t, []string{"prepareEInvoice"}, available(StateRejected))
}
