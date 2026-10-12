package ar05

import (
	"context"
	"sync"
)

// FakeTransport is a fully functional in-memory provider for tests (and for local development of the callback flow).
// It keeps provider-side state per external attempt id, so QueryStatus answers from what Submit "delivered", exactly
// like a real network that deduplicates on the client reference. It is NOT wired by default.
type FakeTransport struct {
	mu          sync.Mutex
	ProviderID  string
	script      []FakeSubmit
	provider    map[string]string // external attempt id -> provider-side state
	refs        map[string]string
	Submits     []SubmitRequest
	Queries     []QueryRequest
	QueryErr    error  // returned by QueryStatus when set
	QueryAnswer string // when set, QueryStatus answers this state regardless of provider-side state (e.g. ProviderUnknown)
}

// FakeSubmit scripts one Submit call.
type FakeSubmit struct {
	Result *SubmitResult
	Err    error
	// Delivered: the provider received the document even though Err is set (a timeout AFTER delivery: the dangerous case).
	Delivered bool
	// Hook runs inside Submit before it returns (tests use it to deliver a callback or inspect the database mid-flight).
	Hook func(SubmitRequest)
}

func NewFakeTransport(provider string) *FakeTransport {
	return &FakeTransport{ProviderID: provider, provider: map[string]string{}, refs: map[string]string{}}
}

// Script appends scripted Submit behaviours (consumed in order; once exhausted Submit acknowledges).
func (f *FakeTransport) Script(s ...FakeSubmit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, s...)
}

// SetProviderState forces the provider-side state of an attempt (as if the network progressed it).
func (f *FakeTransport) SetProviderState(attemptID, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provider[attemptID] = state
}

// Delivered reports whether the provider holds an attempt.
func (f *FakeTransport) Delivered(attemptID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.provider[attemptID]
	return ok
}

// SubmitCount is the number of Submit calls seen.
func (f *FakeTransport) SubmitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Submits)
}

func (f *FakeTransport) Name() string { return f.ProviderID }

func (f *FakeTransport) Submit(ctx context.Context, req SubmitRequest) (*SubmitResult, error) {
	f.mu.Lock()
	f.Submits = append(f.Submits, req)
	var step FakeSubmit
	if len(f.script) > 0 {
		step, f.script = f.script[0], f.script[1:]
	}
	if step.Result == nil && step.Err == nil {
		step.Result = &SubmitResult{Disposition: DispAcknowledged, ProviderRef: "prov-" + req.ExternalAttemptID, ResponseDigest: Sha256Hex([]byte("ack:" + req.ExternalAttemptID))}
	}
	if step.Err == nil || step.Delivered {
		st := ProviderReceived
		if step.Result != nil {
			switch step.Result.Disposition {
			case DispAccepted:
				st = ProviderAccepted
			case DispRejected:
				st = ProviderRejected
			}
			f.refs[req.ExternalAttemptID] = step.Result.ProviderRef
		}
		f.provider[req.ExternalAttemptID] = st
	}
	hook := step.Hook
	f.mu.Unlock()
	if hook != nil {
		hook(req)
	}
	if step.Err != nil {
		return nil, step.Err
	}
	return step.Result, nil
}

func (f *FakeTransport) QueryStatus(ctx context.Context, req QueryRequest) (*StatusResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Queries = append(f.Queries, req)
	if f.QueryErr != nil {
		return nil, f.QueryErr
	}
	state := f.QueryAnswer
	if state == "" {
		var ok bool
		if state, ok = f.provider[req.ExternalAttemptID]; !ok {
			state = ProviderNotReceived
		}
	}
	res := &StatusResult{State: state, ProviderRef: f.refs[req.ExternalAttemptID], ResponseDigest: Sha256Hex([]byte("q:" + req.ExternalAttemptID + state))}
	if state == ProviderAccepted {
		res.ReceiptID = "rcpt-" + req.ExternalAttemptID
	}
	return res, nil
}
