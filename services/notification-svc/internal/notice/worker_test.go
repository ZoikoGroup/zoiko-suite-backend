package notice

import (
	"context"
	"errors"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/store"
)

type fakeStore struct {
	open      []store.OpenNotice
	findErr   error
	failOn    string
	refreshed []string
	tenants   []string
	nows      []time.Time
}

func (f *fakeStore) FindOpenNotices(context.Context, int) ([]store.OpenNotice, error) {
	return f.open, f.findErr
}

func (f *fakeStore) RefreshNotice(ctx context.Context, id string, now time.Time) (*domain.Notice, error) {
	if id == f.failOn {
		return nil, errors.New("boom")
	}
	f.refreshed = append(f.refreshed, id)
	f.tenants = append(f.tenants, svcmiddleware.TenantFromContext(ctx))
	f.nows = append(f.nows, now)
	return &domain.Notice{NoticeID: id}, nil
}

func TestSweeperRefreshesEachOpenNoticeUnderItsOwnTenant(t *testing.T) {
	f := &fakeStore{open: []store.OpenNotice{{NoticeID: "n1", TenantID: "t1"}, {NoticeID: "n2", TenantID: "t2"}}}
	w := NewWorker(f, time.Minute, 10, nil)
	fixed := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return fixed }

	if got := w.RunOnce(context.Background()); got != 2 {
		t.Fatalf("processed %d, want 2", got)
	}
	if f.tenants[0] != "t1" || f.tenants[1] != "t2" {
		t.Errorf("each notice must be refreshed under its own tenant, got %v", f.tenants)
	}
	if !f.nows[0].Equal(fixed) {
		t.Errorf("the sweeper must use its clock, got %v", f.nows[0])
	}
}

func TestSweeperSurvivesOneBadNoticeAndAListingFailure(t *testing.T) {
	f := &fakeStore{open: []store.OpenNotice{{NoticeID: "bad", TenantID: "t1"}, {NoticeID: "good", TenantID: "t1"}}, failOn: "bad"}
	if got := NewWorker(f, time.Minute, 10, nil).RunOnce(context.Background()); got != 1 || len(f.refreshed) != 1 || f.refreshed[0] != "good" {
		t.Fatalf("one failure must not stop the batch: processed=%d refreshed=%v", got, f.refreshed)
	}
	f = &fakeStore{findErr: errors.New("db down")}
	if got := NewWorker(f, time.Minute, 10, nil).RunOnce(context.Background()); got != 0 {
		t.Fatalf("a listing failure processes nothing, got %d", got)
	}
}

func TestSweeperDefaultsAreSafe(t *testing.T) {
	w := NewWorker(&fakeStore{}, 0, 0, nil)
	if w.interval <= 0 || w.batchSize <= 0 {
		t.Fatalf("a misconfigured sweeper must fall back to safe values: %v %d", w.interval, w.batchSize)
	}
}
