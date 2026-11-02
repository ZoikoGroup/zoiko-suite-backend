package provider

import (
	"context"
	"errors"
	"testing"
)

type countingAdapter struct{ n int }

func (c *countingAdapter) Sync(context.Context, SyncRequest) (SyncResult, error) {
	c.n++
	return SyncResult{RecordsSynced: 7}, nil
}

func TestEmptyRegistryFailsClosed(t *testing.T) {
	for _, r := range []*Registry{NewRegistry(), nil} {
		if _, err := r.Sync(context.Background(), SyncRequest{Provider: "WORKDAY"}); !errors.Is(err, ErrProviderNotConfigured) {
			t.Fatalf("err = %v, want ErrProviderNotConfigured", err)
		}
	}
}

func TestRegisteredAdapterIsUsed(t *testing.T) {
	r := NewRegistry()
	a := &countingAdapter{}
	r.Register("workday", a)
	res, err := r.Sync(context.Background(), SyncRequest{Provider: "WORKDAY"})
	if err != nil || res.RecordsSynced != 7 || a.n != 1 {
		t.Fatalf("got %+v %v (calls=%d)", res, err, a.n)
	}
	if _, err := r.Sync(context.Background(), SyncRequest{Provider: "ADP"}); !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("unregistered provider must fail closed, got %v", err)
	}
}
