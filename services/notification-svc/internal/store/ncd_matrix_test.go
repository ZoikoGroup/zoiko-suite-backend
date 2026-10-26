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

// NP-34: a provider's post-upload attachment manifest must match the pinned
// DRC versions and hashes exactly. A mismatch is evidence and an incident,
// never a delivery claim; a matching manifest applies normally.
func TestNP34_ProviderAttachmentHashMismatchIsQuarantined(t *testing.T) {
	withCallbackSecret(t)
	h := newHarness(t)
	pinned := ncd.Attachment{
		Slot: "invoice_pdf", DRCRecordID: "drc-1", DRCVersion: "3",
		SHA256: strings.Repeat("ab", 32),
	}
	i := h.intent(func(in *ncd.IntentInput) {
		in.AllowedChannels = []string{"EMAIL"}
		in.AttachmentContract = []ncd.AttachmentSlot{{
			Slot: "invoice_pdf", DRCRecordType: "invoice", MaxSensitivity: ncd.Level("S1"),
		}}
	}, emailInvoice)
	c := h.comm(i.IntentID, h.recipient("pat@example.com"), func(in *ncd.CommunicationInput) {
		in.Attachments = []ncd.Attachment{pinned}
	})
	v := h.send(c)
	a := v.Attempts[0]

	res := h.callback(ncd.ProviderEvent{EventID: "att-mismatch", EventType: "delivered",
		AttemptToken: a.IdempotencyToken, AttachmentHashes: map[string]string{"invoice_pdf": strings.Repeat("cd", 32)}})
	if res[0].Status != "APPLIED" {
		t.Fatalf("a mismatched manifest is recorded as evidence, got %+v", res)
	}
	v = h.view(c.CommunicationID)
	if v.Attempts[0].State == ncd.AttemptDelivered {
		t.Fatal("NP-34: a mismatched provider package must not be claimed as delivered")
	}
	ex, err := h.svc.ListExceptions(h.ctx, h.author, true)
	h.must(err)
	if !hasException(ex, "ATTACHMENT_HASH_MISMATCH") {
		t.Fatalf("NP-34: mismatch must raise an exception, got %+v", ex)
	}

	// Positive control: the exact pinned manifest applies as delivery.
	res = h.callback(ncd.ProviderEvent{EventID: "att-match", EventType: "delivered",
		AttemptToken: a.IdempotencyToken, AttachmentHashes: map[string]string{"invoice_pdf": pinned.SHA256}})
	if res[0].Status != "APPLIED" {
		t.Fatalf("matching manifest should apply: %+v", res)
	}
	if v = h.view(c.CommunicationID); v.Attempts[0].State != ncd.AttemptDelivered {
		t.Fatalf("matching manifest should deliver, got %s", v.Attempts[0].State)
	}
}

// NP-57: if the submitting process dies after the provider may have accepted,
// the attempt is recovered as UNKNOWN with evidence and a resolution deadline,
// never silently dropped or blindly retried.
func TestNP57_StrandedSubmittingAttemptBecomesUnknown(t *testing.T) {
	h := newHarness(t)
	i := h.intent(func(in *ncd.IntentInput) { in.AllowedChannels = []string{"EMAIL"} }, emailInvoice)
	c := h.comm(i.IntentID, h.recipient("pat@example.com"), nil)
	v := h.send(c)
	a := v.Attempts[0]

	// The schema refuses ACCEPTED -> SUBMITTING (it is not a legal 6.2 edge).
	// Disable the guard only to manufacture the in-flight row this test targets.
	_, err := h.admin.Exec(h.ctx, `ALTER TABLE ncd_attempts DISABLE TRIGGER trg_ncd_attempt_transition`)
	h.must(err)
	t.Cleanup(func() {
		_, _ = h.admin.Exec(h.ctx, `ALTER TABLE ncd_attempts ENABLE TRIGGER trg_ncd_attempt_transition`)
	})
	_, err = h.admin.Exec(h.ctx, `UPDATE ncd_attempts SET state = 'SUBMITTING' WHERE attempt_id = $1`, a.AttemptID)
	h.must(err)
	h.must(h.svc.MarkStranded(h.ctx, ncd.WorkRef{ID: a.AttemptID, TenantID: h.tenant}))

	v = h.view(c.CommunicationID)
	if v.Attempts[0].State != ncd.AttemptUnknown || v.Attempts[0].ResolutionDueAt == nil {
		t.Fatalf("NP-57: stranded attempt must be UNKNOWN with a resolution deadline, got %+v", v.Attempts[0])
	}
	var evidence int
	h.must(h.admin.QueryRow(h.ctx,
		`SELECT count(*) FROM ncd_delivery_evidence WHERE attempt_id = $1 AND evidence_type = 'PROVIDER_UNKNOWN'`,
		a.AttemptID).Scan(&evidence))
	if evidence != 1 {
		t.Fatalf("NP-57: the ambiguity must be durable evidence, got %d row(s)", evidence)
	}
}

// INV-29: analytics/projection work may read authoritative state only. A
// read-only transaction cannot mutate any NCD control-plane table.
func TestINV29_ProjectionRoleCannotMutateAuthoritativeState(t *testing.T) {
	h := newHarness(t)
	for _, table := range []string{"ncd_communications", "ncd_attempts", "ncd_suppressions"} {
		tx, err := h.admin.Begin(h.ctx)
		h.must(err)
		if _, err = tx.Exec(h.ctx, `SET TRANSACTION READ ONLY`); err != nil {
			_ = tx.Rollback(h.ctx)
			h.must(err)
		}
		var n int
		if err = tx.QueryRow(h.ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			_ = tx.Rollback(h.ctx)
			t.Fatalf("INV-29: a projection must still be able to read %s: %v", table, err)
		}
		if _, err = tx.Exec(h.ctx, `UPDATE `+table+` SET tenant_id = tenant_id`); err == nil {
			_ = tx.Rollback(h.ctx)
			t.Errorf("INV-29: a read-only projection transaction mutated %s", table)
			continue
		}
		_ = tx.Rollback(h.ctx)
	}
}
