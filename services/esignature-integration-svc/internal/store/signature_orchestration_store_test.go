package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/esignature-integration-svc/internal/domain"
	"zoiko.io/esignature-integration-svc/internal/middleware"
	"zoiko.io/esignature-integration-svc/internal/store"
)

// DRC-05 Signature, Seal & Attestation Orchestrator (ZS-SVC-S-001 §7),
// against real Postgres as the FORCE-RLS app role this service runs
// under (see openAdminPool / appRolePool in rls_test.go).

// newOrchestrationStores returns the admin pool alongside the app-role
// stores so a raw-trigger negative control can reuse the SAME admin
// connection rather than calling openAdminPool a second time — a
// second call resets the whole schema (DROP + replay every migration),
// which would silently wipe out whatever the test already created.
func newOrchestrationStores(t *testing.T) (*store.PgOrchestrationStore, store.Store, *pgxpool.Pool) {
	t.Helper()
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	return store.NewPgOrchestrationStore(appPool), store.NewPgStore(appPool), admin
}

func sha256Hex(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func createActiveProfile(t *testing.T, os *store.PgOrchestrationStore, ctx context.Context) *domain.SignatureProfile {
	t.Helper()
	pr, err := os.CreateSignatureProfile(ctx, &domain.CreateSignatureProfileRequest{
		LegalEntityID: "le-a", AssuranceLevel: string(domain.AssuranceAdvanced), Jurisdiction: "US-CA",
		IdentityRequirement: string(domain.IdentityKBA), WitnessRequired: false,
	}, "creator-1")
	require.NoError(t, err)
	activated, err := os.ActivateSignatureProfile(ctx, pr.ProfileID)
	require.NoError(t, err)
	return activated
}

func createTestEnvelope(t *testing.T, es store.Store, ctx context.Context, title string) *domain.SignatureEnvelope {
	t.Helper()
	env := &domain.SignatureEnvelope{
		LegalEntityID: "le-a", Provider: "DOCUSIGN", DocumentTitle: title,
		SignerEmail: "signer@example.com", SignerName: "A Signer", Status: string(domain.EnvelopeSent),
	}
	require.NoError(t, es.CreateEnvelope(ctx, env))
	return env
}

// ── Signature Profiles ───────────────────────────────────────────────────────

func TestDRC05_SignatureProfile_DraftToActiveToRetired(t *testing.T) {
	os, _, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")

	pr, err := os.CreateSignatureProfile(ctx, &domain.CreateSignatureProfileRequest{
		LegalEntityID: "le-a", AssuranceLevel: string(domain.AssuranceQualified), Jurisdiction: "EU",
		IdentityRequirement: string(domain.IdentityGovID), WitnessRequired: true,
	}, "creator-1")
	require.NoError(t, err)
	require.Equal(t, domain.ProfileDraft, pr.Status)

	active, err := os.ActivateSignatureProfile(ctx, pr.ProfileID)
	require.NoError(t, err)
	require.Equal(t, domain.ProfileActive, active.Status)
	require.NotNil(t, active.ActivatedAt)

	retired, err := os.RetireSignatureProfile(ctx, pr.ProfileID)
	require.NoError(t, err)
	require.Equal(t, domain.ProfileRetired, retired.Status)
}

func TestDRC05_SignatureProfile_InvalidAssuranceLevel_Refused(t *testing.T) {
	os, _, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")

	_, err := os.CreateSignatureProfile(ctx, &domain.CreateSignatureProfileRequest{
		LegalEntityID: "le-a", AssuranceLevel: "NOT_REAL", Jurisdiction: "US", IdentityRequirement: string(domain.IdentityKBA),
	}, "creator-1")
	require.ErrorIs(t, err, domain.ErrInvalidAssuranceLevel)
}

func TestDRC05_SignatureProfile_ActivateNonDraft_Refused(t *testing.T) {
	os, _, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	active := createActiveProfile(t, os, ctx)

	_, err := os.ActivateSignatureProfile(ctx, active.ProfileID)
	require.ErrorIs(t, err, domain.ErrSignatureProfileNotDraft)
}

// Raw-trigger negative control: a DRAFT profile's terms are mutable,
// but once ACTIVE the trigger refuses a direct attempt to rewrite them,
// not merely the store's own validation.
func TestDRC05_SignatureProfile_RawUpdateOfActiveTerms_RejectedByTrigger(t *testing.T) {
	os, _, admin := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	active := createActiveProfile(t, os, ctx)

	_, err := admin.Exec(context.Background(),
		`UPDATE signature_profiles SET jurisdiction = 'TAMPERED' WHERE profile_id = $1`, active.ProfileID)
	require.Error(t, err, "an ACTIVE profile's terms must be immutable at the database level")
}

// ── Envelope binding / amendment ─────────────────────────────────────────────

func TestDRC05_BindSignatureProfile_HappyPath(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	active := createActiveProfile(t, os, ctx)
	env := createTestEnvelope(t, es, ctx, "bind-happy")

	bound, err := os.BindSignatureProfile(ctx, env.EnvelopeID, active.ProfileID)
	require.NoError(t, err)
	require.Equal(t, env.EnvelopeID, bound.EnvelopeID)
}

func TestDRC05_BindSignatureProfile_NotActive_Refused(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "bind-draft-profile")

	draft, err := os.CreateSignatureProfile(ctx, &domain.CreateSignatureProfileRequest{
		LegalEntityID: "le-a", AssuranceLevel: string(domain.AssuranceSimple), Jurisdiction: "US",
		IdentityRequirement: string(domain.IdentityEmailOnly),
	}, "creator-1")
	require.NoError(t, err)

	_, err = os.BindSignatureProfile(ctx, env.EnvelopeID, draft.ProfileID)
	require.ErrorIs(t, err, domain.ErrSignatureProfileNotActive)
}

func TestDRC05_BindSignatureProfile_AlreadyBound_Refused(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	active := createActiveProfile(t, os, ctx)
	env := createTestEnvelope(t, es, ctx, "bind-twice")

	_, err := os.BindSignatureProfile(ctx, env.EnvelopeID, active.ProfileID)
	require.NoError(t, err)

	_, err = os.BindSignatureProfile(ctx, env.EnvelopeID, active.ProfileID)
	require.ErrorIs(t, err, domain.ErrEnvelopeAlreadyBoundToProfile)
}

// The central amendment/voiding invariant: amending a non-terminal
// envelope voids it and creates a fresh one referencing it; a SIGNED
// envelope cannot be amended at all.
func TestDRC05_AmendEnvelope_VoidsOldCreatesNew(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "amend-me")

	newEnv, err := os.AmendEnvelope(ctx, env.EnvelopeID, &domain.CreateEnvelopeRequest{
		LegalEntityID: "le-a", Provider: "DOCUSIGN", DocumentTitle: "amend-me-corrected",
		SignerEmail: "corrected@example.com", SignerName: "Corrected Signer",
	}, "amender-1")
	require.NoError(t, err)
	require.NotEqual(t, env.EnvelopeID, newEnv.EnvelopeID)

	old, err := es.GetEnvelopeByID(ctx, env.EnvelopeID)
	require.NoError(t, err)
	require.Equal(t, string(domain.EnvelopeVoided), old.Status)
}

func TestDRC05_AmendEnvelope_SignedEnvelope_Refused(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "amend-signed")

	_, err := es.UpdateEnvelopeStatus(ctx, env.EnvelopeID, &domain.UpdateStatusRequest{Status: string(domain.EnvelopeDelivered)})
	require.NoError(t, err)
	_, err = es.UpdateEnvelopeStatus(ctx, env.EnvelopeID, &domain.UpdateStatusRequest{Status: string(domain.EnvelopeSigned)})
	require.NoError(t, err)

	_, err = os.AmendEnvelope(ctx, env.EnvelopeID, &domain.CreateEnvelopeRequest{
		LegalEntityID: "le-a", DocumentTitle: "x", SignerEmail: "x@example.com", SignerName: "X",
	}, "amender-1")
	require.ErrorIs(t, err, domain.ErrEnvelopeNotAmendable)
}

// ── Envelope lifecycle trigger (replaces the old free-write) ────────────────

// The core defect this migration fixes: before it, UpdateEnvelopeStatus
// could set ANY status from ANY other — a SIGNED envelope could be
// "voided" after the fact. Now the trigger refuses it.
func TestDRC05_EnvelopeLifecycle_CannotVoidAfterSigned(t *testing.T) {
	_, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "cannot-void-after-signed")

	_, err := es.UpdateEnvelopeStatus(ctx, env.EnvelopeID, &domain.UpdateStatusRequest{Status: string(domain.EnvelopeDelivered)})
	require.NoError(t, err)
	_, err = es.UpdateEnvelopeStatus(ctx, env.EnvelopeID, &domain.UpdateStatusRequest{Status: string(domain.EnvelopeSigned)})
	require.NoError(t, err)

	_, err = es.UpdateEnvelopeStatus(ctx, env.EnvelopeID, &domain.UpdateStatusRequest{Status: string(domain.EnvelopeVoided)})
	require.Error(t, err, "a SIGNED envelope must never be voidable after the fact")
}

func TestDRC05_EnvelopeLifecycle_CannotSkipDelivered(t *testing.T) {
	_, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "cannot-skip-delivered")

	_, err := es.UpdateEnvelopeStatus(ctx, env.EnvelopeID, &domain.UpdateStatusRequest{Status: string(domain.EnvelopeSigned)})
	require.Error(t, err, "SENT must go through DELIVERED before SIGNED")
}

func TestDRC05_EnvelopeLifecycle_VoidFromSentIsAllowed(t *testing.T) {
	_, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "void-from-sent")

	voided, err := es.UpdateEnvelopeStatus(ctx, env.EnvelopeID, &domain.UpdateStatusRequest{Status: string(domain.EnvelopeVoided)})
	require.NoError(t, err)
	require.Equal(t, string(domain.EnvelopeVoided), voided.Status)
}

// ── Provider Attempts ────────────────────────────────────────────────────────

func TestDRC05_RecordProviderAttempt_IdempotentReplay(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "attempt-idempotent")

	first, err := os.RecordProviderAttempt(ctx, env.EnvelopeID, &domain.RecordProviderAttemptRequest{
		IdempotencyKey: "req-1", Provider: "DOCUSIGN", AttemptedAction: string(domain.AttemptActionSend), Outcome: string(domain.AttemptUnknown),
	})
	require.NoError(t, err)

	replay, err := os.RecordProviderAttempt(ctx, env.EnvelopeID, &domain.RecordProviderAttemptRequest{
		IdempotencyKey: "req-1", Provider: "DOCUSIGN", AttemptedAction: string(domain.AttemptActionSend), Outcome: string(domain.AttemptUnknown),
	})
	require.NoError(t, err)
	require.Equal(t, first.AttemptID, replay.AttemptID, "a replay with the same idempotency_key must return the SAME attempt, not a new one")

	attempts, err := os.ListProviderAttempts(ctx, env.EnvelopeID)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
}

// The core UNKNOWN-resolution invariant: an UNKNOWN attempt can only
// become SUCCEEDED/FAILED via ReconcileAttempt, never a bare retry, and
// once reconciled it is terminal.
func TestDRC05_ReconcileAttempt_FromUnknownToSucceeded(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "reconcile-unknown")

	att, err := os.RecordProviderAttempt(ctx, env.EnvelopeID, &domain.RecordProviderAttemptRequest{
		IdempotencyKey: "req-timeout", Provider: "DOCUSIGN", AttemptedAction: string(domain.AttemptActionSend), Outcome: string(domain.AttemptUnknown),
	})
	require.NoError(t, err)
	require.Equal(t, domain.AttemptUnknown, att.Outcome)

	resolved, err := os.ReconcileAttempt(ctx, att.AttemptID, &domain.ReconcileAttemptRequest{
		Outcome: string(domain.AttemptSucceeded), ProviderResponseRef: "docusign-ref-123",
	}, "reconciler-1")
	require.NoError(t, err)
	require.Equal(t, domain.AttemptSucceeded, resolved.Outcome)
	require.NotNil(t, resolved.ResolvedByPrincipalID)
}

func TestDRC05_ReconcileAttempt_NotUnknown_Refused(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "reconcile-not-unknown")

	att, err := os.RecordProviderAttempt(ctx, env.EnvelopeID, &domain.RecordProviderAttemptRequest{
		IdempotencyKey: "req-pending", Provider: "DOCUSIGN", AttemptedAction: string(domain.AttemptActionSend), Outcome: string(domain.AttemptPending),
	})
	require.NoError(t, err)

	_, err = os.ReconcileAttempt(ctx, att.AttemptID, &domain.ReconcileAttemptRequest{Outcome: string(domain.AttemptSucceeded)}, "reconciler-1")
	require.ErrorIs(t, err, domain.ErrAttemptNotUnknown)
}

// Raw-trigger negative control: a SUCCEEDED attempt is immutable even
// against a direct UPDATE, not just through the store's own guard.
func TestDRC05_ProviderAttempt_RawUpdateOfSucceeded_RejectedByTrigger(t *testing.T) {
	os, es, admin := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "attempt-raw-update")

	att, err := os.RecordProviderAttempt(ctx, env.EnvelopeID, &domain.RecordProviderAttemptRequest{
		IdempotencyKey: "req-raw", Provider: "DOCUSIGN", AttemptedAction: string(domain.AttemptActionSend), Outcome: string(domain.AttemptSucceeded),
	})
	require.NoError(t, err)

	_, err = admin.Exec(context.Background(), `UPDATE provider_attempts SET outcome = 'FAILED' WHERE attempt_id = $1`, att.AttemptID)
	require.Error(t, err, "a SUCCEEDED attempt must be immutable at the database level")
}

// ── Participants ─────────────────────────────────────────────────────────────

func TestDRC05_Participant_FullLifecycle(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "participant-lifecycle")

	pt, err := os.AddParticipant(ctx, env.EnvelopeID, &domain.AddParticipantRequest{Email: "alice@example.com", Name: "Alice", Role: string(domain.RoleSigner)})
	require.NoError(t, err)
	require.Equal(t, domain.ParticipantInvited, pt.ParticipantState)

	viewed, err := os.MarkParticipantViewed(ctx, pt.ParticipantID)
	require.NoError(t, err)
	require.Equal(t, domain.ParticipantViewed, viewed.ParticipantState)

	signed, err := os.MarkParticipantSigned(ctx, pt.ParticipantID)
	require.NoError(t, err)
	require.Equal(t, domain.ParticipantSigned, signed.ParticipantState)
}

func TestDRC05_Participant_CannotSignWithoutViewing(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "participant-skip-view")

	pt, err := os.AddParticipant(ctx, env.EnvelopeID, &domain.AddParticipantRequest{Email: "bob@example.com", Name: "Bob"})
	require.NoError(t, err)

	_, err = os.MarkParticipantSigned(ctx, pt.ParticipantID)
	require.ErrorIs(t, err, domain.ErrParticipantNotSignable)
}

func TestDRC05_Participant_DeclineThenImmutable(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "participant-decline")

	pt, err := os.AddParticipant(ctx, env.EnvelopeID, &domain.AddParticipantRequest{Email: "carol@example.com", Name: "Carol"})
	require.NoError(t, err)

	declined, err := os.DeclineParticipant(ctx, pt.ParticipantID, "not authorized to sign")
	require.NoError(t, err)
	require.Equal(t, domain.ParticipantDeclined, declined.ParticipantState)

	_, err = os.MarkParticipantViewed(ctx, pt.ParticipantID)
	require.ErrorIs(t, err, domain.ErrParticipantNotViewable)
}

// Two independent participants on the same envelope track their own
// state — proving the orthogonal-dimension requirement structurally.
func TestDRC05_Participant_IndependentPerSigner(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "participant-independent")

	alice, err := os.AddParticipant(ctx, env.EnvelopeID, &domain.AddParticipantRequest{Email: "alice2@example.com", Name: "Alice"})
	require.NoError(t, err)
	bob, err := os.AddParticipant(ctx, env.EnvelopeID, &domain.AddParticipantRequest{Email: "bob2@example.com", Name: "Bob"})
	require.NoError(t, err)

	_, err = os.MarkParticipantViewed(ctx, alice.ParticipantID)
	require.NoError(t, err)
	_, err = os.MarkParticipantSigned(ctx, alice.ParticipantID)
	require.NoError(t, err)

	list, err := os.ListParticipants(ctx, env.EnvelopeID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, p := range list {
		if p.ParticipantID == alice.ParticipantID {
			require.Equal(t, domain.ParticipantSigned, p.ParticipantState)
		} else {
			require.Equal(t, bob.ParticipantID, p.ParticipantID)
			require.Equal(t, domain.ParticipantInvited, p.ParticipantState)
		}
	}
}

// ── Completion Evidence ──────────────────────────────────────────────────────

func TestDRC05_SealCompletionEvidence_RequiresSignedEnvelope(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "evidence-not-signed")

	_, err := os.SealCompletionEvidence(ctx, env.EnvelopeID, &domain.SealCompletionEvidenceRequest{
		CompletionCertificateRef: "cert-ref", CompletedArtifactHash: sha256Hex("final-doc"),
	}, "sealer-1")
	require.ErrorIs(t, err, domain.ErrEnvelopeNotSigned)
}

func TestDRC05_SealCompletionEvidence_HappyPath_AndOnlyOncePerEnvelope(t *testing.T) {
	os, es, _ := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "evidence-happy")

	_, err := es.UpdateEnvelopeStatus(ctx, env.EnvelopeID, &domain.UpdateStatusRequest{Status: string(domain.EnvelopeDelivered)})
	require.NoError(t, err)
	_, err = es.UpdateEnvelopeStatus(ctx, env.EnvelopeID, &domain.UpdateStatusRequest{Status: string(domain.EnvelopeSigned)})
	require.NoError(t, err)

	ev, err := os.SealCompletionEvidence(ctx, env.EnvelopeID, &domain.SealCompletionEvidenceRequest{
		CompletionCertificateRef: "cert-ref-1", CompletedArtifactHash: sha256Hex("final-doc-1"),
	}, "sealer-1")
	require.NoError(t, err)
	require.Equal(t, env.EnvelopeID, ev.EnvelopeID)

	fetched, err := os.GetCompletionEvidence(ctx, env.EnvelopeID)
	require.NoError(t, err)
	require.Equal(t, ev.EvidenceID, fetched.EvidenceID)

	_, err = os.SealCompletionEvidence(ctx, env.EnvelopeID, &domain.SealCompletionEvidenceRequest{
		CompletionCertificateRef: "cert-ref-2", CompletedArtifactHash: sha256Hex("final-doc-2"),
	}, "sealer-2")
	require.ErrorIs(t, err, domain.ErrCompletionEvidenceExists)
}

// Raw-trigger negative control: completion evidence is immutable even
// against a direct UPDATE.
func TestDRC05_CompletionEvidence_RawUpdate_RejectedByTrigger(t *testing.T) {
	os, es, admin := newOrchestrationStores(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-a")
	env := createTestEnvelope(t, es, ctx, "evidence-raw-update")

	_, err := es.UpdateEnvelopeStatus(ctx, env.EnvelopeID, &domain.UpdateStatusRequest{Status: string(domain.EnvelopeDelivered)})
	require.NoError(t, err)
	_, err = es.UpdateEnvelopeStatus(ctx, env.EnvelopeID, &domain.UpdateStatusRequest{Status: string(domain.EnvelopeSigned)})
	require.NoError(t, err)

	ev, err := os.SealCompletionEvidence(ctx, env.EnvelopeID, &domain.SealCompletionEvidenceRequest{
		CompletionCertificateRef: "cert-ref", CompletedArtifactHash: sha256Hex("raw-update-doc"),
	}, "sealer-1")
	require.NoError(t, err)

	_, err = admin.Exec(context.Background(), `UPDATE completion_evidence SET completion_certificate_ref = 'tampered' WHERE evidence_id = $1`, ev.EvidenceID)
	require.Error(t, err, "sealed completion evidence must be immutable at the database level")
}

// ── Tenant isolation ─────────────────────────────────────────────────────────

func TestDRC05_TenantIsolation_Profile(t *testing.T) {
	os, _, _ := newOrchestrationStores(t)
	ctxA := middleware.WithTenant(context.Background(), "tenant-a")
	active := createActiveProfile(t, os, ctxA)

	ctxB := middleware.WithTenant(context.Background(), "tenant-b")
	_, err := os.GetSignatureProfile(ctxB, active.ProfileID)
	require.ErrorIs(t, err, domain.ErrSignatureProfileNotFound)
}
