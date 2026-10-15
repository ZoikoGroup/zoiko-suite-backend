package store_test

import (
	"strings"
	"testing"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ncd"
)

// Rows of the §14 negative-path matrix whose controls existed in code with
// nothing proving them. Found while scoring the matrix row by row (audit gap
// G-10); each test names its row.

// NP-10 / INV-18: an attachment is pinned by exact DRC version and hash.
// "latest" is a mutable pointer and is never accepted as evidence.
func TestNP10_LatestAttachmentPointerIsRefused(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil, emailInvoice)
	r := h.recipient("pat@example.com")
	hash := strings.Repeat("ab", 32)
	for name, att := range map[string]ncd.Attachment{
		"latest pointer": {Slot: "invoice_pdf", DRCRecordID: "rec-1", DRCVersion: "LATEST", SHA256: hash},
		"no hash":        {Slot: "invoice_pdf", DRCRecordID: "rec-1", DRCVersion: "3"},
		"no version":     {Slot: "invoice_pdf", DRCRecordID: "rec-1", SHA256: hash},
	} {
		_, _, err := h.svc.CreateCommunication(h.ctx, h.author, ncd.CommunicationInput{
			IntentID: i.IntentID, LegalEntityID: h.entity, RecipientPrincipalID: r, Locale: "en-GB",
			Variables: map[string]string{"invoice_no": "INV-1"}, SourceEventID: "evt-" + name,
			Attachments: []ncd.Attachment{att},
		})
		h.kind(err, ncd.KindInvalid, "invalid_attachment")
	}
}

// NP-56: if the suppression lists cannot be read, the legacy send path sends
// nothing and leaves the attempt retryable. It must never treat "could not
// check" as "not suppressed".
func TestNP56_LegacyGateFailsClosedWhenSuppressionsUnreadable(t *testing.T) {
	h := newHarness(t)
	gate := ncd.GatedDeliverer{Inner: h.email, Svc: h.svc}
	n := domain.Notification{NotificationID: "n-np56", TenantID: h.tenant, RecipientPrincipalID: "pat",
		Channel: domain.ChannelEmail, RecipientAddress: "pat@example.com", Subject: "s", Body: "b"}

	// Make both lists unreadable to the app role, as an outage would.
	_, err := h.admin.Exec(h.ctx, `REVOKE SELECT ON ncd_suppressions, email_suppressions FROM `+ncdAppRole)
	h.must(err)
	t.Cleanup(func() {
		_, _ = h.admin.Exec(h.ctx, `GRANT SELECT ON ncd_suppressions, email_suppressions TO `+ncdAppRole)
	})
	out := gate.Deliver(h.ctx, n)
	if out.Delivered || !out.Retryable || !strings.Contains(out.Reason, "NP-56") {
		t.Fatalf("unreadable suppressions must fail closed and stay retryable, got %+v", out)
	}
	if h.email.count() != 0 {
		t.Fatalf("nothing may reach the provider while suppression state is unknown, got %d", h.email.count())
	}

	// Positive control: readable again, the same message goes out.
	_, err = h.admin.Exec(h.ctx, `GRANT SELECT ON ncd_suppressions, email_suppressions TO `+ncdAppRole)
	h.must(err)
	if out := gate.Deliver(h.ctx, n); !out.Delivered || h.email.count() != 1 {
		t.Fatalf("with suppressions readable the send should proceed, got %+v (%d sent)", out, h.email.count())
	}
}
