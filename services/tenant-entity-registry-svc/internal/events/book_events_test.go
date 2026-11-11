package events_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
)

func TestBookEvents(t *testing.T) {
	b := domain.AccountingBook{BookID: "0190a000-0000-7000-8000-0000000b0001", TenantID: "0190a000-0000-7000-8000-0000000000aa",
		LegalEntityID: "0190a000-0000-7000-8000-00000000dddd", BookCode: "STAT-IFRS", BookType: domain.BookStatutory, Status: domain.BookActive,
		Material: true, RecordVersion: 3, CreatedBy: "p", CreatedAt: time.Now()}
	prev := domain.BookBasisVersion{VersionNo: 1, Framework: domain.FrameworkIFRS, FunctionalCurrency: "USD", PresentationCurrency: "USD", ValidFrom: "2025-01-01", EvidenceRef: "e1"}
	next := domain.BookBasisVersion{VersionNo: 2, Framework: domain.FrameworkLocalGAAP, FunctionalCurrency: "GBP", PresentationCurrency: "USD", ValidFrom: "2027-01-01", EvidenceRef: "e2", Reason: "local reporting"}

	created, err := events.BookCreatedRecord(b, prev, "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventBookCreated, created.EventType)
	assert.Contains(t, string(created.Payload), `"framework":"IFRS"`)

	changed, err := events.BookBasisChangedRecord(b, prev, next, "p2", "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventBookBasisChanged, changed.EventType)
	p := string(changed.Payload)
	assert.Contains(t, p, `"effective_date":"2027-01-01"`, "the new basis starts on its own date and never reaches back")
	assert.Contains(t, p, `"previous_framework":"IFRS"`)
	assert.Contains(t, p, `"previous_functional_currency":"USD"`)
	assert.Contains(t, p, `"functional_currency":"GBP"`)
	assert.Contains(t, p, `"basis_version":2`)

	for to, want := range map[string]string{
		domain.BookActive: events.EventBookActivated, domain.BookPostingSuspended: events.EventBookPostingSuspend, domain.BookRetired: events.EventBookRetired,
	} {
		rec, err := events.BookStatusRecord(b, next, domain.BookTransition{From: domain.BookDraft, To: to, EffectiveDate: "2026-03-01", Command: "X", EvidenceRef: "e", Actor: "p3"}, "c")
		require.NoError(t, err)
		assert.Equal(t, want, rec.EventType)
		assert.Contains(t, string(rec.Payload), `"effective_date":"2026-03-01"`)
		assert.Contains(t, string(rec.Payload), `"material":true`)
	}
	assert.Empty(t, events.BookStatusEvent(domain.BookDraft))
	assert.Len(t, events.BookEventNames, 5)

	g := b
	g.LegalEntityID, g.GroupCode = "", "GRP"
	rec, err := events.BookCreatedRecord(g, prev, "c")
	require.NoError(t, err)
	assert.Contains(t, string(rec.Payload), `"group_code":"GRP"`)
	assert.NotContains(t, string(rec.Payload), "legal_entity_id")
}
