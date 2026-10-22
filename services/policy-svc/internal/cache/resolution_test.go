package cache

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/policy-svc/internal/domain"
)

type fakeSource struct {
	watermark    int64
	watermarkErr error
	loads        int
}

func (f *fakeSource) ResolutionCandidates(context.Context, string, *string, *string) ([]*domain.ApplicablePolicyVersion, error) {
	f.loads++
	return []*domain.ApplicablePolicyVersion{{}}, nil
}

func (f *fakeSource) ResolutionWatermark(context.Context) (int64, error) {
	return f.watermark, f.watermarkErr
}

func strp(s string) *string { return &s }

// GCP domain negative path #30: a lifecycle change anywhere (here, a
// suspension bumping the watermark) must be seen on the very next request.
func TestCandidates_ReusedOnlyWhileWatermarkUnchanged(t *testing.T) {
	src := &fakeSource{watermark: 1}
	c := New(src)
	ctx := context.Background()
	t1 := strp("t1")

	_, _ = c.Candidates(ctx, "T", t1, nil)
	_, _ = c.Candidates(ctx, "T", t1, nil)
	if src.loads != 1 {
		t.Fatalf("expected one load while nothing changed, got %d", src.loads)
	}
	src.watermark = 2
	_, _ = c.Candidates(ctx, "T", t1, nil)
	if src.loads != 2 {
		t.Fatalf("a watermark change must force a reload, got %d loads", src.loads)
	}
	_, _ = c.Candidates(ctx, "T", strp("t2"), nil)
	if src.loads != 3 {
		t.Fatal("tenants must not share entries")
	}
}

func TestCandidates_BypassedWhenWatermarkUnreadable(t *testing.T) {
	src := &fakeSource{watermarkErr: errors.New("down")}
	c := New(src)
	_, _ = c.Candidates(context.Background(), "T", nil, nil)
	_, _ = c.Candidates(context.Background(), "T", nil, nil)
	if src.loads != 2 || c.Len() != 0 {
		t.Fatalf("an unverifiable cache must never be trusted: loads %d, entries %d", src.loads, c.Len())
	}
}

func TestInvalidateTenant(t *testing.T) {
	src := &fakeSource{watermark: 1}
	c := New(src)
	ctx := context.Background()
	_, _ = c.Candidates(ctx, "T", strp("t1"), strp("e1"))
	_, _ = c.Candidates(ctx, "T", strp("t2"), nil)
	c.InvalidateTenant("t1")
	if c.Len() != 1 {
		t.Fatalf("expected only t2's entry left, got %d", c.Len())
	}
	_, _ = c.Candidates(ctx, "T", strp("t1"), strp("e1"))
	if src.loads != 3 {
		t.Fatal("an invalidated tenant must reload")
	}
}
