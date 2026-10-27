package store

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	kafka "github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/privacy-consent-svc/internal/domain"
	"zoiko.io/privacy-consent-svc/internal/middleware"
	"zoiko.io/privacy-consent-svc/internal/outbox"
	"zoiko.io/privacy-consent-svc/internal/testdb"
)

// Real PostgreSQL, real migrations, restricted RLS-bound role — see package
// testdb. They skip without TEST_DATABASE_URL and FAIL (not skip) when
// REQUIRE_DB_TESTS=1.

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

func tctx(tenant string) context.Context {
	return middleware.WithTenant(context.Background(), tenant)
}

func newStore(t *testing.T) (*PgStore, *testdb.DB) {
	t.Helper()
	db := testdb.Setup(t)
	return NewPgStore(db.App, zap.NewNop()), db
}

// noticeIn drives a notice to the requested state using the store's own
// lifecycle methods — the same transitions production uses, so the rows the
// tests bind receipts to are genuinely in that state.
func noticeIn(t *testing.T, s *PgStore, tenant string, state domain.NoticeVersionStatus) *domain.NoticeVersion {
	t.Helper()
	ctx := tctx(tenant)
	_, v, err := s.CreateNotice(ctx, tenant, domain.CreateNoticeRequest{Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:" + uuid.NewString()}, "maker")
	if err != nil {
		t.Fatalf("CreateNotice: %v", err)
	}
	if state == domain.NoticeStatusDraft {
		return v
	}
	if v, err = s.ApproveNoticeVersion(ctx, v.NoticeID, v.NoticeVersionID, "checker"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if state == domain.NoticeStatusApproved {
		return v
	}
	if v, err = s.PublishNoticeVersion(ctx, v.NoticeID, v.NoticeVersionID, "publisher", "corr-"+uuid.NewString()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	switch state {
	case domain.NoticeStatusPublished:
		return v
	case domain.NoticeStatusWithdrawn:
		if v, err = s.WithdrawNoticeVersion(ctx, v.NoticeID, v.NoticeVersionID); err != nil {
			t.Fatalf("Withdraw: %v", err)
		}
		return v
	case domain.NoticeStatusSuperseded:
		first := v
		succ, err := s.CreateNoticeVersion(ctx, first.NoticeID, domain.CreateNoticeVersionRequest{
			ParentVersionID: first.NoticeVersionID, Locale: "en-US", Audience: "CUSTOMER", ContentHash: "sha256:v2-" + uuid.NewString(),
		}, "maker")
		if err != nil {
			t.Fatalf("CreateNoticeVersion: %v", err)
		}
		if _, err = s.ApproveNoticeVersion(ctx, succ.NoticeID, succ.NoticeVersionID, "checker"); err != nil {
			t.Fatalf("Approve successor: %v", err)
		}
		if _, err = s.PublishNoticeVersion(ctx, succ.NoticeID, succ.NoticeVersionID, "publisher", "corr-"+uuid.NewString()); err != nil {
			t.Fatalf("Publish successor: %v", err)
		}
		return first // now SUPERSEDED
	}
	t.Fatalf("unhandled state %s", state)
	return nil
}

func count(t *testing.T, db *testdb.DB, table string) int {
	t.Helper()
	var n int
	if err := db.Admin.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

type bindCase struct {
	name  string
	setup func(t *testing.T, s *PgStore) (noticeID, versionID string)
	valid bool
}

func bindCases() []bindCase {
	inState := func(st domain.NoticeVersionStatus) func(*testing.T, *PgStore) (string, string) {
		return func(t *testing.T, s *PgStore) (string, string) {
			v := noticeIn(t, s, tenantA, st)
			return v.NoticeID, v.NoticeVersionID
		}
	}
	return []bindCase{
		{"PUBLISHED is accepted", inState(domain.NoticeStatusPublished), true},
		{"DRAFT is rejected", inState(domain.NoticeStatusDraft), false},
		{"APPROVED is rejected", inState(domain.NoticeStatusApproved), false},
		{"WITHDRAWN is rejected", inState(domain.NoticeStatusWithdrawn), false},
		{"SUPERSEDED is rejected", inState(domain.NoticeStatusSuperseded), false},
		{"nonexistent version is rejected", func(t *testing.T, s *PgStore) (string, string) {
			v := noticeIn(t, s, tenantA, domain.NoticeStatusPublished)
			return v.NoticeID, uuid.NewString()
		}, false},
		{"malformed version id is rejected", func(t *testing.T, s *PgStore) (string, string) {
			v := noticeIn(t, s, tenantA, domain.NoticeStatusPublished)
			return v.NoticeID, "not-a-uuid"
		}, false},
		{"another tenant's PUBLISHED version is rejected (RLS)", func(t *testing.T, s *PgStore) (string, string) {
			v := noticeIn(t, s, tenantB, domain.NoticeStatusPublished)
			return v.NoticeID, v.NoticeVersionID
		}, false},
	}
}

func TestRecordPresentation_BindsOnlyToPublishedNoticeVersion(t *testing.T) {
	for _, tc := range bindCases() {
		t.Run(tc.name, func(t *testing.T) {
			s, db := newStore(t)
			noticeID, versionID := tc.setup(t, s)
			before := count(t, db, "presentation_receipts")

			r, err := s.RecordPresentation(tctx(tenantA), tenantA, noticeID, versionID,
				domain.RecordPresentationRequest{SubjectRef: "subject-1", Channel: "WEB", Locale: "en-US"})

			if tc.valid {
				if err != nil || r == nil {
					t.Fatalf("expected the receipt to be recorded, got err=%v", err)
				}
				if count(t, db, "presentation_receipts") != before+1 {
					t.Fatalf("expected exactly one new presentation receipt")
				}
				return
			}
			if !errors.Is(err, domain.ErrNoticeVersionInvalid) {
				t.Fatalf("expected ErrNoticeVersionInvalid (PRV-008), got %v", err)
			}
			if count(t, db, "presentation_receipts") != before {
				t.Fatalf("a rejected presentation must write nothing")
			}
		})
	}
}

func TestRecordPresentation_VersionBelongingToAnotherNotice_IsRejected(t *testing.T) {
	s, db := newStore(t)
	a := noticeIn(t, s, tenantA, domain.NoticeStatusPublished)
	b := noticeIn(t, s, tenantA, domain.NoticeStatusPublished)
	before := count(t, db, "presentation_receipts")

	// a's published version presented under b's notice id.
	_, err := s.RecordPresentation(tctx(tenantA), tenantA, b.NoticeID, a.NoticeVersionID,
		domain.RecordPresentationRequest{SubjectRef: "s", Channel: "WEB", Locale: "en-US"})
	if !errors.Is(err, domain.ErrNoticeVersionInvalid) {
		t.Fatalf("expected ErrNoticeVersionInvalid, got %v", err)
	}
	if count(t, db, "presentation_receipts") != before {
		t.Fatalf("a rejected presentation must write nothing")
	}
}

func TestRecordConsent_BindsOnlyToPublishedNoticeVersion(t *testing.T) {
	for _, tc := range bindCases() {
		t.Run(tc.name, func(t *testing.T) {
			s, db := newStore(t)
			_, versionID := tc.setup(t, s)
			receiptsBefore, outboxBefore := count(t, db, "consent_receipts"), count(t, db, "event_outbox")

			rcpt, err := s.RecordConsent(tctx(tenantA), tenantA, domain.RecordConsentRequest{
				SubjectRef: "subject-1", PurposeID: "purpose-1", Action: "GRANTED", CaptureChannel: "WEB",
				NoticeVersionID: versionID,
			}, "principal-1", "corr-1")

			if tc.valid {
				if err != nil || rcpt == nil {
					t.Fatalf("expected the consent to be recorded, got err=%v", err)
				}
				if rcpt.NoticeVersionID == nil || *rcpt.NoticeVersionID != versionID {
					t.Fatalf("receipt must bind the exact version, got %v", rcpt.NoticeVersionID)
				}
				if count(t, db, "consent_receipts") != receiptsBefore+1 {
					t.Fatalf("expected exactly one new consent receipt")
				}
				return
			}
			if !errors.Is(err, domain.ErrNoticeVersionInvalid) {
				t.Fatalf("expected ErrNoticeVersionInvalid (PRV-008), got %v", err)
			}
			if count(t, db, "consent_receipts") != receiptsBefore {
				t.Fatalf("a rejected consent must write no receipt")
			}
			if count(t, db, "event_outbox") != outboxBefore {
				t.Fatalf("a rejected consent must enqueue no event")
			}
		})
	}
}

func TestRecordConsent_WithoutNoticeVersion_IsRejected(t *testing.T) {
	s, db := newStore(t)
	receiptsBefore, outboxBefore := count(t, db, "consent_receipts"), count(t, db, "event_outbox")

	_, err := s.RecordConsent(tctx(tenantA), tenantA, domain.RecordConsentRequest{
		SubjectRef: "subject-1", PurposeID: "purpose-1", Action: "GRANTED", CaptureChannel: "WEB",
	}, "principal-1", "corr-1")
	if !errors.Is(err, domain.ErrNoticeVersionInvalid) {
		t.Fatalf("expected ErrNoticeVersionInvalid (PRV-008) when notice_version_id is missing, got %v", err)
	}
	if count(t, db, "consent_receipts") != receiptsBefore {
		t.Fatalf("a rejected consent must write no receipt")
	}
	if count(t, db, "event_outbox") != outboxBefore {
		t.Fatalf("a rejected consent must enqueue no event")
	}
}

func TestRecordConsent_EmptyNoticeVersion_IsRejected(t *testing.T) {
	s, db := newStore(t)
	receiptsBefore, outboxBefore := count(t, db, "consent_receipts"), count(t, db, "event_outbox")

	_, err := s.RecordConsent(tctx(tenantA), tenantA, domain.RecordConsentRequest{
		SubjectRef: "subject-1", PurposeID: "purpose-1", Action: "GRANTED", CaptureChannel: "WEB",
		NoticeVersionID: "   ",
	}, "principal-1", "corr-1")
	if !errors.Is(err, domain.ErrNoticeVersionInvalid) {
		t.Fatalf("expected ErrNoticeVersionInvalid (PRV-008) when notice_version_id is blank, got %v", err)
	}
	if count(t, db, "consent_receipts") != receiptsBefore {
		t.Fatalf("a rejected consent must write no receipt")
	}
	if count(t, db, "event_outbox") != outboxBefore {
		t.Fatalf("a rejected consent must enqueue no event")
	}
}

// The check runs BEFORE the PRV-N04 dedup: an existing receipt must never be
// returned as the answer to a request citing a version that has since been
// withdrawn.
func TestRecordConsent_ExistingReceiptDoesNotAnswerRequestCitingUnpublishedVersion(t *testing.T) {
	s, _ := newStore(t)
	v := noticeIn(t, s, tenantA, domain.NoticeStatusPublished)
	req := domain.RecordConsentRequest{
		SubjectRef: "subject-1", PurposeID: "purpose-1", Action: "GRANTED", CaptureChannel: "WEB", NoticeVersionID: v.NoticeVersionID,
	}
	if _, err := s.RecordConsent(tctx(tenantA), tenantA, req, "p", "c1"); err != nil {
		t.Fatalf("first consent: %v", err)
	}
	if _, err := s.WithdrawNoticeVersion(tctx(tenantA), v.NoticeID, v.NoticeVersionID); err != nil {
		t.Fatalf("withdraw notice: %v", err)
	}
	if _, err := s.RecordConsent(tctx(tenantA), tenantA, req, "p", "c2"); !errors.Is(err, domain.ErrNoticeVersionInvalid) {
		t.Fatalf("an identical replay citing a now-WITHDRAWN version must be rejected, not answered with the old receipt; got %v", err)
	}
}

// requirePublishedNoticeVersion reads the row FOR SHARE. This proves the
// lock is real: while a receipt transaction holds a validated version, a
// concurrent WithdrawNoticeVersion must WAIT — otherwise it could commit
// between the check and the INSERT and leave a receipt bound to a withdrawn
// version.
func TestRequirePublished_HoldsRowLock_WithdrawWaitsForReceiptTransaction(t *testing.T) {
	s, db := newStore(t)
	v := noticeIn(t, s, tenantA, domain.NoticeStatusPublished)

	ctx := context.Background()
	tx, err := db.App.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantA); err != nil {
		t.Fatal(err)
	}
	if err := requirePublishedNoticeVersion(ctx, tx, "", v.NoticeVersionID); err != nil {
		t.Fatalf("PUBLISHED version should validate: %v", err)
	}

	var withdrawn atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, err := s.WithdrawNoticeVersion(tctx(tenantA), v.NoticeID, v.NoticeVersionID)
		withdrawn.Store(true)
		done <- err
	}()

	time.Sleep(500 * time.Millisecond)
	if withdrawn.Load() {
		t.Fatalf("RACE: WithdrawNoticeVersion committed while a receipt transaction held the validated version — FOR SHARE is not holding")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("withdraw after the receipt transaction ended: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("withdraw never completed after the receipt transaction committed")
	}
}

// The store's outbox write and the relay, end to end on the real database:
// a recorded consent produces exactly one event, the relay delivers it, and
// the row is marked published.
type captureWriter struct{ msgs []kafka.Message }

func (w *captureWriter) WriteMessages(_ context.Context, m ...kafka.Message) error {
	w.msgs = append(w.msgs, m...)
	return nil
}

func TestRecordConsent_EventIsDeliveredByRelay_AndMarkedPublished(t *testing.T) {
	s, db := newStore(t)
	v := noticeIn(t, s, tenantA, domain.NoticeStatusPublished)
	// noticeIn's publish already enqueued privacy.notice.published; drain it
	// so the assertions below are about the consent event alone.
	w := &captureWriter{}
	relay := outbox.NewRelay(db.App, w, zap.NewNop())
	if _, err := relay.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain notice event: %v", err)
	}
	w.msgs = nil

	if _, err := s.RecordConsent(tctx(tenantA), tenantA, domain.RecordConsentRequest{
		SubjectRef: "subject-1", PurposeID: "purpose-1", Action: "GRANTED", CaptureChannel: "WEB", NoticeVersionID: v.NoticeVersionID,
	}, "principal-1", "corr-e2e"); err != nil {
		t.Fatalf("RecordConsent: %v", err)
	}
	if n, err := relay.DrainOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("expected the relay to claim exactly the one consent event, claimed=%d err=%v", n, err)
	}
	if len(w.msgs) != 1 {
		t.Fatalf("expected 1 delivered message, got %d", len(w.msgs))
	}
	var unpublished int
	if err := db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM event_outbox WHERE published_at IS NULL`).Scan(&unpublished); err != nil || unpublished != 0 {
		t.Fatalf("every event must be marked published after delivery, %d are not (err=%v)", unpublished, err)
	}
}
