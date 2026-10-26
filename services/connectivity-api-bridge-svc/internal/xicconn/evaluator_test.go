package xicconn_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

func TestEvaluateDue_AppliesTimeDrivenRulesAcrossTenants(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	a := e.sandbox(tenantA, v, "acc-a", refOAuth, refHMAC)
	b := e.sandbox(tenantB, v, "acc-b", refOAuth2, refHMAC2)
	e.advance(50 * 24 * time.Hour) // inside the warning window of both
	n, err := e.svc.EvaluateDue(context.Background(), 50)
	if err != nil || n != 2 {
		t.Fatalf("%v %d", err, n)
	}
	for tenant, c := range map[string]*xicconn.Connection{tenantA: a, tenantB: b} {
		got := e.get(tenant, c)
		if got.Status != xicconn.StatusDegraded || got.StatusReason != "CREDENTIAL_EXPIRING" {
			t.Fatalf("%s: %+v", tenant, got)
		}
	}
	// a second pass has nothing new to change but the connections are still "due" until the credential is replaced
	if _, err := e.svc.EvaluateDue(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.svc.EvaluateDue(context.Background(), 1); n != 1 {
		t.Fatalf("the batch limit was ignored: %d", n)
	}
}

func TestRunEvaluator_StopsWithItsContextAndCanBeSwitchedOff(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	e.sandbox(tenantA, v, "acc-a", refOAuth, refHMAC)
	// interval 0 returns at once
	done := make(chan struct{})
	go func() { e.svc.RunEvaluator(context.Background(), 0, 10, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a disabled evaluator kept running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var passes int32
	stopped := make(chan struct{})
	go func() {
		e.svc.RunEvaluator(ctx, 5*time.Millisecond, 10, func(int, error) { atomic.AddInt32(&passes, 1) })
		close(stopped)
	}()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("the evaluator ignored cancellation")
	}
	if atomic.LoadInt32(&passes) == 0 {
		t.Fatal("the evaluator never ran")
	}
}
