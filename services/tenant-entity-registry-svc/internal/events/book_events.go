package events

// REF-06 accounting book events (ZS-SVC-I-001 s4.16: AccountingBookCreated, AccountingBookActivated, BookBasisChanged,
// BookPostingSuspended). A retirement is published too: a consumer must stop posting to a retired book.
//
// They carry identifiers, the book type, status, effective date and the basis (framework and currencies), none of which
// is sensitive. Chart and policy references travel as references, never content.

import (
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

const (
	EventBookCreated        = "accounting.book.created"           // AccountingBookCreated
	EventBookActivated      = "accounting.book.activated"         // AccountingBookActivated
	EventBookBasisChanged   = "accounting.book.basis_changed"     // BookBasisChanged
	EventBookPostingSuspend = "accounting.book.posting_suspended" // BookPostingSuspended
	EventBookRetired        = "accounting.book.retired"           // (no spec name)
)

// BookEventNames is every accounting-book wire name the code can emit.
var BookEventNames = []string{EventBookCreated, EventBookActivated, EventBookBasisChanged, EventBookPostingSuspend, EventBookRetired}

// BookStatusEvent is the event for a book moving to status to.
func BookStatusEvent(to string) string {
	switch to {
	case domain.BookActive:
		return EventBookActivated
	case domain.BookPostingSuspended:
		return EventBookPostingSuspend
	case domain.BookRetired:
		return EventBookRetired
	}
	return ""
}

func bookSpec(eventType string, b domain.AccountingBook, basis domain.BookBasisVersion, actor, corr, evidence, effective string, payload map[string]any) RecordSpec {
	payload["tenant_id"] = b.TenantID
	payload["book_id"] = b.BookID
	payload["book_code"] = b.BookCode
	payload["book_type"] = b.BookType
	payload["status"] = b.Status
	payload["material"] = b.Material
	payload["effective_date"] = effective
	payload["basis_version"] = basis.VersionNo
	payload["framework"] = basis.Framework
	payload["functional_currency"] = basis.FunctionalCurrency
	payload["presentation_currency"] = basis.PresentationCurrency
	if b.LegalEntityID != "" {
		payload["legal_entity_id"] = b.LegalEntityID
	}
	if b.GroupCode != "" {
		payload["group_code"] = b.GroupCode
	}
	return RecordSpec{
		EventType: eventType, TenantID: b.TenantID, LegalEntityID: b.LegalEntityID, ActorID: actor, CorrelationID: corr,
		ObjectID: b.BookID, ObjectVersion: b.RecordVersion, PartitionKey: b.BookID, EvidenceRef: evidence, Payload: payload,
	}
}

// BookCreatedRecord is accounting.book.created.
func BookCreatedRecord(b domain.AccountingBook, basis domain.BookBasisVersion, corr string) (*outbox.Record, error) {
	return BuildRecord(bookSpec(EventBookCreated, b, basis, b.CreatedBy, corr, basis.EvidenceRef, basis.ValidFrom, map[string]any{}))
}

// BookBasisChangedRecord is accounting.book.basis_changed: the new basis starts on its valid_from and the previous one
// ends there. It never reaches back: how earlier periods were booked is unchanged.
func BookBasisChangedRecord(b domain.AccountingBook, prev, next domain.BookBasisVersion, actor, corr string) (*outbox.Record, error) {
	return BuildRecord(bookSpec(EventBookBasisChanged, b, next, actor, corr, next.EvidenceRef, next.ValidFrom, map[string]any{
		"previous_basis_version": prev.VersionNo, "previous_framework": prev.Framework,
		"previous_functional_currency": prev.FunctionalCurrency, "previous_presentation_currency": prev.PresentationCurrency,
		"reason": next.Reason,
	}))
}

// BookStatusRecord is accounting.book.activated, .posting_suspended or .retired.
func BookStatusRecord(b domain.AccountingBook, basis domain.BookBasisVersion, t domain.BookTransition, corr string) (*outbox.Record, error) {
	return BuildRecord(bookSpec(BookStatusEvent(t.To), b, basis, t.Actor, corr, t.EvidenceRef, t.EffectiveDate, map[string]any{
		"from_status": t.From, "to_status": t.To, "command": t.Command,
	}))
}
