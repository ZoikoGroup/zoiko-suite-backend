package handler_test

import (
	"context"
	"time"

	"zoiko.io/financial-control-svc/internal/domain"
)

type wave7State struct {
	resolveErr    error
	resolveExpect int
	resolveReq    domain.ResolveExceptionRequest
	submitErr     error
}

func (f *fakeStore) ResolveException(_ context.Context, _, _, _, _, key string, expected int, req domain.ResolveExceptionRequest) (*domain.ControlException, bool, error) {
	f.calls = append(f.calls, "ResolveException")
	f.w7.resolveExpect, f.w7.resolveReq = expected, req
	f.idemKey = key
	if f.w7.resolveErr != nil {
		return nil, false, f.w7.resolveErr
	}
	c := *f.w1.exception
	c.Version++
	c.State = domain.ExceptionState(req.ToState)
	return &c, f.idemReplay, nil
}
func (f *fakeStore) ListExceptionTransitions(context.Context, string, string) ([]domain.ExceptionTransition, error) {
	return nil, nil
}
func (f *fakeStore) SubmitForCertification(_ context.Context, _, _, _, _, key string, _ int, _ string) (*domain.ControlRun, bool, error) {
	f.calls = append(f.calls, "SubmitForCertification")
	f.idemKey = key
	if f.w7.submitErr != nil {
		return nil, false, f.w7.submitErr
	}
	c := *f.run
	c.Version++
	c.LifecycleState = domain.LifecycleReadyToCertify
	return &c, f.idemReplay, nil
}

func (f *fakeStore) Monitoring(context.Context, string, string, string, time.Time) (*domain.MonitoringSnapshot, error) {
	f.calls = append(f.calls, "Monitoring")
	m := &domain.MonitoringSnapshot{}
	m.Derive()
	return m, nil
}
