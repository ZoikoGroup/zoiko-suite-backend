package store_test

// The §9.3 canonical DRC events for retention, legal holds and preservation
// are written to the transactional outbox in the same transaction as the change
// they announce. Real PostgreSQL.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/store"
)

type outboxRow struct {
	EventType     string
	AggregateType string
	AggregateID   string
	LegalEntityID *string
	Payload       map[string]any
}

func eventsOfType(t *testing.T, pool *pgxpool.Pool, eventType string) []outboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT event_type, aggregate_type, aggregate_id, legal_entity_id::text, payload FROM outbox_events WHERE event_type = $1 ORDER BY created_at`, eventType)
	require.NoError(t, err)
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var raw []byte
		require.NoError(t, rows.Scan(&r.EventType, &r.AggregateType, &r.AggregateID, &r.LegalEntityID, &raw))
		var env struct {
			Payload map[string]any `json:"payload"`
			Source  string         `json:"source_service"`
		}
		require.NoError(t, json.Unmarshal(raw, &env))
		assert.Equal(t, "document-vault-svc", env.Source)
		r.Payload = env.Payload
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func clearOutbox(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `TRUNCATE outbox_events`)
	require.NoError(t, err)
}

func TestDRC03_RetentionEvents_ScheduleResolvedAndDispositionDue(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "evt-retention", string(domain.RecordClassTaxReturnSupport), "US-CA")
	rule := createActiveRetentionRule(t, s, string(domain.RecordClassTaxReturnSupport), "US-CA", 30)
	clearOutbox(t, pool)

	// Bound without a trigger: the schedule is "resolved" to a waiting condition.
	waiting, err := s.BindRetentionRule(tenantCtx(), domain.BindRetentionRuleParams{
		RecordID: rec.RecordID, RetentionRuleVersionID: rule.RetentionRuleVersionID, CreatedByPrincipalID: "operator-dave",
	})
	require.NoError(t, err)
	ev := eventsOfType(t, pool, "retention_schedule_resolved")
	require.Len(t, ev, 1)
	assert.Equal(t, rec.RecordID, ev[0].Payload["record_id"])
	assert.Equal(t, rule.RetentionRuleVersionID, ev[0].Payload["schedule_version_id"])
	assert.Equal(t, string(domain.TriggerDeclaredAt), ev[0].Payload["trigger"])
	assert.Equal(t, "WAITING_FOR_TRIGGER", ev[0].Payload["condition"])
	assert.Nil(t, ev[0].Payload["due_at"], "no due date is guessed before the trigger")
	require.NotNil(t, ev[0].LegalEntityID)
	assert.Equal(t, rec.LegalEntityID, *ev[0].LegalEntityID)

	// The trigger arrives: the schedule resolves to a concrete due date.
	trigger := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = s.RecordTriggerEvent(tenantCtx(), domain.RecordTriggerEventParams{
		RetentionStateID: waiting.RetentionStateID, TriggerDate: trigger, TriggeredByPrincipalID: "operator-dave",
	})
	require.NoError(t, err)
	ev = eventsOfType(t, pool, "retention_schedule_resolved")
	require.Len(t, ev, 2)
	assert.Equal(t, "TRIGGER_RESOLVED", ev[1].Payload["condition"])
	assert.NotNil(t, ev[1].Payload["due_at"])

	// Becoming due announces record_disposition_due with the hold state.
	_, err = s.EvaluateDue(tenantCtx(), domain.EvaluateDueParams{
		RetentionStateID: waiting.RetentionStateID, AsOf: trigger.AddDate(0, 0, 31), EvaluatedByPrincipalID: "operator-dave",
	})
	require.NoError(t, err)
	due := eventsOfType(t, pool, "record_disposition_due")
	require.Len(t, due, 1)
	assert.Equal(t, rec.RecordID, due[0].Payload["record_id"])
	assert.Equal(t, "NO_HOLD", due[0].Payload["hold_state"])
	assert.NotNil(t, due[0].Payload["due_at"])
}

func TestDRC03_LegalHoldEvents_TargetAddedActivatedReleased(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec1 := createTestRecord(t, s, "evt-hold-1", string(domain.RecordClassLegalMatterRecord), "US-DE")
	rec2 := createTestRecord(t, s, "evt-hold-2", string(domain.RecordClassLegalMatterRecord), "US-DE")
	clearOutbox(t, pool)

	hold, err := s.CreateLegalHold(tenantCtx(), domain.CreateLegalHoldParams{
		MatterRef: "Matter-evt", AuthorityRef: "court-order-9", HoldReasonCode: "LITIGATION",
		RecordIDs: []string{rec1.RecordID}, IssuedByPrincipalID: "counsel-frank",
	})
	require.NoError(t, err)
	added := eventsOfType(t, pool, "legal_hold_target_added")
	require.Len(t, added, 1, "the target given at creation is announced")
	assert.Equal(t, hold.HoldID, added[0].Payload["hold_id"])
	assert.Equal(t, rec1.RecordID, added[0].Payload["record_id"])
	assert.Equal(t, false, added[0].Payload["applied"], "a DRAFT hold has applied nothing yet")
	assert.Nil(t, added[0].LegalEntityID, "a hold is tenant-level: no legal entity")

	_, err = s.ActivateLegalHold(tenantCtx(), domain.ActivateLegalHoldParams{HoldID: hold.HoldID, ActivatedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)
	act := eventsOfType(t, pool, "legal_hold_activated")
	require.Len(t, act, 1)
	assert.Equal(t, hold.HoldID, act[0].Payload["hold_id"])
	assert.Equal(t, "court-order-9", act[0].Payload["authority_ref"])
	assert.EqualValues(t, 1, act[0].Payload["targets_applied"])

	_, err = s.AddLegalHoldTarget(tenantCtx(), domain.AddLegalHoldTargetParams{HoldID: hold.HoldID, RecordID: rec2.RecordID, CreatedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)
	added = eventsOfType(t, pool, "legal_hold_target_added")
	require.Len(t, added, 2)
	assert.Equal(t, true, added[1].Payload["applied"], "a target added to an ACTIVE hold is applied at once")

	_, err = s.ReleaseLegalHold(tenantCtx(), domain.ReleaseLegalHoldParams{
		HoldID: hold.HoldID, ReleaseReason: "resolved", ReleasedByPrincipalID: "counsel-frank", ReleaseApprovedByPrincipalID: "director-dana",
	})
	require.NoError(t, err)
	rel := eventsOfType(t, pool, "legal_hold_released")
	require.Len(t, rel, 1)
	assert.Equal(t, hold.HoldID, rel[0].Payload["hold_id"])
	assert.Equal(t, "director-dana", rel[0].Payload["release_approval_ref"])
	assert.EqualValues(t, 2, rel[0].Payload["targets_released"])
}

func TestDRC03_RefusedOperation_LeavesNoEvent(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	rec := createTestRecord(t, s, "evt-refused", string(domain.RecordClassLegalMatterRecord), "US-DE")
	hold, err := s.CreateLegalHold(tenantCtx(), domain.CreateLegalHoldParams{
		MatterRef: "Matter-refused", HoldReasonCode: "LITIGATION", RecordIDs: []string{rec.RecordID}, IssuedByPrincipalID: "counsel-frank",
	})
	require.NoError(t, err)
	_, err = s.ActivateLegalHold(tenantCtx(), domain.ActivateLegalHoldParams{HoldID: hold.HoldID, ActivatedByPrincipalID: "counsel-frank"})
	require.NoError(t, err)
	clearOutbox(t, pool)

	// A self-release is refused: the change rolls back and so does its event.
	_, err = s.ReleaseLegalHold(tenantCtx(), domain.ReleaseLegalHoldParams{
		HoldID: hold.HoldID, ReleaseReason: "x", ReleasedByPrincipalID: "same", ReleaseApprovedByPrincipalID: "same",
	})
	require.ErrorIs(t, err, domain.ErrLegalHoldSelfRelease)
	assert.Empty(t, eventsOfType(t, pool, "legal_hold_released"))
}

func TestDRC04_PreservationEvents_RenditionIntegrityExport(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	doc, v := createTestDocument(t, s, "evt-preservation")
	clearOutbox(t, pool)

	rd := createTestRendition(t, s, v, domain.RenditionPreservation, "evt-rendition")
	ev := eventsOfType(t, pool, "rendition_created")
	require.Len(t, ev, 1)
	assert.Equal(t, rd.RenditionID, ev[0].Payload["rendition_id"])
	assert.Equal(t, v.DocumentVersionID, ev[0].Payload["source_version_id"])
	assert.Equal(t, "PDF_TO_PDFA_V1", ev[0].Payload["profile_id"])
	assert.Equal(t, rd.ChecksumSHA256, ev[0].Payload["output_hash"])
	require.NotNil(t, ev[0].LegalEntityID)
	assert.Equal(t, doc.LegalEntityID, *ev[0].LegalEntityID)

	manifest, err := s.GetFixityManifestByRendition(tenantCtx(), rd.RenditionID)
	require.NoError(t, err)

	// A clean re-check is not an incident.
	_, err = s.RecordVerification(tenantCtx(), domain.RecordVerificationParams{
		ManifestID: manifest.ManifestID, ObservedHash: rd.ChecksumSHA256,
		ClaimedOutcome: string(domain.FixityVerified), VerifiedByPrincipalID: "verifier-1",
	})
	require.NoError(t, err)
	assert.Empty(t, eventsOfType(t, pool, "integrity_check_failed"))

	// A real mismatch is.
	_, err = s.RecordVerification(tenantCtx(), domain.RecordVerificationParams{
		ManifestID: manifest.ManifestID, ObservedHash: sha256Hex("corrupted"),
		ClaimedOutcome: string(domain.FixityMismatch), VerifiedByPrincipalID: "verifier-1",
	})
	require.NoError(t, err)
	bad := eventsOfType(t, pool, "integrity_check_failed")
	require.Len(t, bad, 1)
	assert.Equal(t, manifest.ManifestID, bad[0].Payload["manifest_id"])
	assert.Equal(t, rd.RenditionID, bad[0].Payload["object_id"])
	assert.Equal(t, rd.ChecksumSHA256, bad[0].Payload["expected_hash"], "the expected hash is the manifest's, never rewritten")
	assert.Equal(t, string(domain.FixityMismatch), bad[0].Payload["integrity"])

	pkg, err := s.CreateExportPackage(tenantCtx(), domain.CreateExportPackageParams{
		RequestedByPrincipalID: "exporter-1",
		Items:                  []domain.ExportPackageItemInput{{ItemType: string(domain.ExportItemDocumentVersion), RefID: v.DocumentVersionID}},
	})
	require.NoError(t, err)
	exp := eventsOfType(t, pool, "export_package_created")
	require.Len(t, exp, 1)
	assert.Equal(t, pkg.PackageID, exp[0].Payload["package_id"])
	assert.EqualValues(t, 1, exp[0].Payload["item_count"])
	assert.Equal(t, pkg.PackageHash, exp[0].Payload["package_hash"])
	assert.Nil(t, exp[0].LegalEntityID)
}
