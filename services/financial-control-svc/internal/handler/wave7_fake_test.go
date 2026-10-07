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

func (f *fakeStore) ResolveException(_ context.Context, _, _, _, _ string, expected int, req domain.ResolveExceptionRequest) (*domain.ControlException, error) {
	f.calls = append(f.calls, "ResolveException")
	f.w7.resolveExpect, f.w7.resolveReq = expected, req
	if f.w7.resolveErr != nil {
		return nil, f.w7.resolveErr
	}
	c := *f.w1.exception
	c.Version++
	c.State = domain.ExceptionState(req.ToState)
	return &c, nil
}
func (f *fakeStore) ListExceptionTransitions(context.Context, string, string) ([]domain.ExceptionTransition, error) {
	return nil, nil
}
func (f *fakeStore) SubmitForCertification(context.Context, string, string, string, string, int, string) (*domain.ControlRun, error) {
	f.calls = append(f.calls, "SubmitForCertification")
	if f.w7.submitErr != nil {
		return nil, f.w7.submitErr
	}
	c := *f.run
	c.Version++
	c.LifecycleState = domain.LifecycleReadyToCertify
	return &c, nil
}

func (f *fakeStore) Monitoring(context.Context, string, string, string, time.Time) (*domain.MonitoringSnapshot, error) {
	f.calls = append(f.calls, "Monitoring")
	m := &domain.MonitoringSnapshot{}
	m.Derive()
	return m, nil
}
