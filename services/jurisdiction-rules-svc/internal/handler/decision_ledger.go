package handler

// Decision capture (tracker "Assignment 25 Nov 2026", step DL-4; Doc 01 s7).
//
// The governed decisions of the Jurisdiction engine reach
// governance-decision-log-svc as svc-jurisdiction-rules: pack certify,
// release (publish), block, unblock, withdraw, rollback and supersede;
// interpretation approval; jurisdiction import approval; calendar publication
// and payment-term approval. Each record goes into decision_log_outbox
// (000038) in the same transaction as the change and the registry event that
// announces it, and identity-envelope/decisionledger's relay delivers it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/identity-envelope/decisionledger"

	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// LedgerPlatformScope is the ledger's legal_entity_id for a decision that
// belongs to no single legal entity: a jurisdiction pack, calendar or payment
// term is platform reference data.
const LedgerPlatformScope = "PLATFORM"

// EventImportApproved is the ledger's name for an approved jurisdiction import
// batch (the batch itself announces each applied row, not the approval).
const EventImportApproved = "jurisdiction-import.approved"

// ledgerVerdicts maps a registry event that records a governed decision to the
// ledger's action type and outcome (GDL 000020 categorises every outcome).
var ledgerVerdicts = map[string]struct{ action, outcome string }{
	events.EventPackCertified:           {"JURISDICTION_PACK_CERTIFY", "CERTIFIED"},
	events.EventPackReleased:            {"JURISDICTION_PACK_RELEASE", "RELEASED"},
	events.EventPackBlocked:             {"JURISDICTION_PACK_BLOCK", "BLOCKED"},
	events.EventPackUnblocked:           {"JURISDICTION_PACK_UNBLOCK", "UNBLOCKED"},
	events.EventPackWithdrawn:           {"JURISDICTION_PACK_WITHDRAW", "WITHDRAWN"},
	events.EventPackRolledBack:          {"JURISDICTION_PACK_ROLLBACK", "ROLLED_BACK"},
	events.EventPackSuperseded:          {"JURISDICTION_PACK_SUPERSEDE", "SUPERSEDED"},
	events.EventInterpretationApproved:  {"JURISDICTION_INTERPRETATION_APPROVE", "APPROVED"},
	EventImportApproved:                 {"JURISDICTION_IMPORT_APPROVE", "APPROVED"},
	events.EventBusinessCalendarUpdated: {"JURISDICTION_CALENDAR_PUBLISH", "PUBLISHED"},
	events.EventPaymentTermActivated:    {"JURISDICTION_PAYMENT_TERM_APPROVE", "ACTIVATED"},
}

// WithDecisionLedger turns decision capture on: db returns the request's
// transaction (store.PgStore.DB joins the unit of work).
func (h *Handler) WithDecisionLedger(db func(context.Context) store.Querier) *Handler {
	h.ledgerDB = db
	return h
}

func payloadString(p map[string]any, key string) string {
	switch v := p[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// ledgerRuleBasis is the version of the rule set the decision is about.
func ledgerRuleBasis(eventType, aggregateID string, p map[string]any) (basis, subjectType, subjectID string) {
	switch {
	case payloadString(p, "pack_ref") != "":
		ref := payloadString(p, "pack_ref") + "@" + payloadString(p, "version")
		return ref, "jurisdiction_pack", ref
	case eventType == events.EventBusinessCalendarUpdated:
		ref := payloadString(p, "calendar_code") + "@" + payloadString(p, "calendar_version")
		return "calendar:" + ref, "business_calendar", ref
	case eventType == events.EventPaymentTermActivated:
		ref := payloadString(p, "term_code") + "@" + payloadString(p, "term_version")
		return "payment_term:" + ref, "payment_term", ref
	case eventType == events.EventInterpretationApproved:
		return "interpretation:" + aggregateID, "regulatory_interpretation", aggregateID
	case eventType == EventImportApproved:
		return "import:" + aggregateID, "jurisdiction_import", aggregateID
	}
	return eventType + ":" + aggregateID, "", aggregateID
}

// ledgerDecisionID is stable for one change: the event, the record it is
// about, the record's version after the change and the request's own key.
func ledgerDecisionID(r *http.Request, eventType, aggregateID string, p map[string]any) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{eventType, aggregateID, payloadString(p, events.AggregateVersionKey),
		r.Header.Get("Idempotency-Key"), r.Header.Get("X-Correlation-ID"), r.Header.Get("X-Request-Id")}, "|")))
	return "jur-" + hex.EncodeToString(sum[:16])
}

// ledgerRecord builds the governance-ledger record of a governed registry
// change, or nil when eventType records no decision.
func ledgerRecord(r *http.Request, eventType, aggregateID, actor string, p map[string]any) *decisionledger.Record {
	v, ok := ledgerVerdicts[eventType]
	if !ok {
		return nil
	}
	action := v.action
	if eventType == events.EventPaymentTermActivated && payloadString(p, "supersedes_version") != "" {
		action = "JURISDICTION_PAYMENT_TERM_SUPERSEDE"
	}
	basis, subjectType, subjectID := ledgerRuleBasis(eventType, aggregateID, p)
	eval := map[string]any{
		decisionledger.OnBehalfOfKey: actor,
		"decision_engine":            "jurisdiction",
		"event_type":                 eventType,
		"aggregate_id":               aggregateID,
	}
	for _, k := range []string{"from_status", "to_status", "reason", "evidence_ref", "certification_id", "artifact_digest",
		"report_digest", "content_digest", "superseded_by", "ring", "region", "restored_version", "decisions_affected",
		"jurisdiction_id", "calendar_version_id", "term_version_id", "supersedes_version", "added", "updated", "retired"} {
		if s := payloadString(p, k); s != "" {
			eval[k] = s
		}
	}
	entity := LedgerPlatformScope
	if id := payloadString(p, "legal_entity_id"); id != "" {
		if _, err := uuid.Parse(id); err == nil {
			entity = id
		}
	}
	corr := r.Header.Get("X-Correlation-ID")
	if corr == "" || len(corr) > 64 {
		corr = "jurisdiction:" + aggregateID
		if len(corr) > 64 {
			corr = corr[:64]
		}
	}
	return &decisionledger.Record{
		DecisionID:        ledgerDecisionID(r, eventType, aggregateID, p),
		TenantID:          r.Header.Get("X-Tenant-Id"),
		LegalEntityID:     entity,
		ActionType:        action,
		Outcome:           v.outcome,
		RuleBasis:         basis,
		ActionSubjectType: subjectType,
		ActionSubjectID:   subjectID,
		EvaluationContext: eval,
		CorrelationID:     corr,
		CausationID:       r.Header.Get("X-Causation-Id"),
		DecidedAt:         time.Now().UTC(),
		Title:             strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(action, "JURISDICTION_"), "_", " ")) + " " + basis,
	}
}

// recordDecision queues the ledger record of a governed change through q (the
// change's transaction). A record that cannot be queued aborts the request's
// unit, exactly as a registry event that cannot be written does (J-11): the
// change, its event and its evidence commit together or not at all.
func (h *Handler) recordDecision(r *http.Request, q store.Querier, eventType, aggregateID, actor string, payload map[string]any) error {
	if h.ledgerDB == nil {
		return nil
	}
	rec := ledgerRecord(r, eventType, aggregateID, actor, payload)
	if rec == nil {
		return nil
	}
	if rec.TenantID == "" {
		// The ledger is tenant-scoped; a decision with no tenant stays in this
		// service's own history and event.
		h.log.Warn("decision not relayed to the governance ledger: no tenant", zap.String("event_type", eventType),
			zap.String("aggregate_id", aggregateID))
		return nil
	}
	if q == nil {
		q = h.ledgerDB(r.Context())
	}
	if err := decisionledger.Enqueue(r.Context(), q, *rec); err != nil {
		store.AbortUnit(r.Context(), fmt.Errorf("decision-log record for %s not written: %w", eventType, err))
		h.log.Error("decision-log record not written; the change is rolled back", zap.String("event_type", eventType), zap.Error(err))
		return err
	}
	return nil
}

// intPtrValue is the int a pointer points to, or nil.
func intPtrValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
