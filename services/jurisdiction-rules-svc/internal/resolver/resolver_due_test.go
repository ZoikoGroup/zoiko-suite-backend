package resolver

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// addObligationPack compiles, signs and certifies a pack carrying one calendar
// and one obligation rule (no jurisdiction rule modules).
func (w *world) addObligationPack(t *testing.T, ref, version, obligationID string, holidays ...domain.Holiday) string {
	t.Helper()
	id := ref + "@" + version
	calID := "cal-" + id
	in := domain.CompileInput{
		PackRef: ref, Version: version, ManifestDigest: "sha256:" + strings.Repeat("a", 64),
		Manifest:      domain.PackManifest{PackID: ref, PackVersion: version, JurisdictionIDs: []string{"GB"}, Regimes: []string{"VAT"}, EffectiveFrom: "2026-01-01"},
		Jurisdictions: []domain.Named{{ID: "j-gb", Code: "GB"}}, Regimes: []domain.Named{{ID: "r-vat", Code: "VAT"}},
		Sources: []domain.CompileSource{{SourceID: "s-1", JurisdictionID: "j-gb", Authority: "HMRC", SourceType: "STATUTE", AuthorityLevel: "BINDING_LAW",
			Title: "VAT Act", Location: "https://x", SnapshotHash: "sha256:" + strings.Repeat("b", 64), Language: "en", Reviewed: true}},
		Interpretations: []domain.CompileInterpretation{{InterpretationID: "i-1", JurisdictionID: "j-gb", Subject: "s", Decision: "d", Rationale: "r", Approved: true, SourceIDs: []string{"s-1"}}},
		Calendars: []domain.CompileCalendar{{CalendarVersion: domain.CalendarVersion{CalendarVersionID: calID, CalendarCode: "gb-hmrc", Version: 1,
			EffectiveFrom: day(2026, 1, 1), Timezone: "Europe/London", WeekendDays: []int{0, 6}, Holidays: holidays, SourceIDs: []string{"s-1"}}, JurisdictionID: "j-gb", Published: true}},
		Obligations: []domain.CompileObligation{{ObligationRule: domain.ObligationRule{ObligationRuleID: obligationID, JurisdictionID: "j-gb", ObligationCode: "VAT_RETURN",
			RuleVersion: 1, Name: "VAT return", PeriodBasis: domain.BasisMonthly, Anchor: domain.AnchorPeriodEnd, OffsetMonths: 1, OffsetToMonthEnd: true, OffsetDays: 7,
			BusinessDayAdjustment: domain.AdjustNext, CalendarCode: "gb-hmrc", EffectiveFrom: day(2026, 1, 1), RegimeID: ptr("r-vat"), InterpretationID: ptr("i-1"),
			SourceIDs: []string{"s-1"}}, Published: true}},
	}
	out := domain.Compile(in)
	require.False(t, out.Report.HasErrors(), "%+v", out.Report.Findings)

	sig, _ := w.signer.Sign(domain.SigningMessage(out.ArtifactDigest))
	b64 := base64.StdEncoding.EncodeToString(sig)
	report := `{"artifact_digest":"` + out.ArtifactDigest + `","certified_by":"certifier"}`
	canon, _ := domain.CanonicalJSON([]byte(report))
	rd, _ := domain.DigestOf(canon)
	csig, _ := w.signer.Sign(domain.CertificationSigningMessage(rd))

	w.src.packs[id] = domain.EligiblePack{PackVersionID: id, PackRef: ref, Version: version, Status: "RELEASED", EffectiveFrom: day(2026, 1, 1), ScopeCodes: []string{"GB"}}
	w.src.loaded[id] = &domain.LoadedPack{PackVersionID: id, ArtifactJSON: string(out.ArtifactJSON), ArtifactDigest: out.ArtifactDigest, VersionDigest: out.ArtifactDigest,
		Signature: &b64, KeyRef: ptr("k1"), Key: w.key,
		Cert: &domain.LoadedCertification{CertificationID: "cert-" + id, ArtifactDigest: out.ArtifactDigest, ReportJSON: string(canon), ReportDigest: rd,
			Signature: base64.StdEncoding.EncodeToString(csig), KeyRef: "k1", Key: w.key}}
	return id
}

func dueReq(facts domain.DueFacts) DueRequest {
	return DueRequest{Jurisdiction: "GB", ObligationCode: "VAT_RETURN", Facts: facts}
}

func TestCalculateDue_FromAVerifiedArtifact(t *testing.T) {
	w := newWorld(t)
	w.addObligationPack(t, "jur.gb.obl.core", "1.0", "obl-1", domain.Holiday{Date: "2026-11-09", Name: "Closure"})
	r := w.resolver(t, time.Minute, nil, nil)

	d, err := r.CalculateDue(context.Background(), dueReq(domain.DueFacts{PeriodEnd: "2026-09-30"}))
	require.NoError(t, err)
	require.Equal(t, domain.DueCalculated, d.Outcome, "%v", d.Explanation)
	assert.Equal(t, "2026-11-10", d.Due.DueDate)
	assert.Equal(t, "obl-1", d.Due.ObligationRuleID)
	assert.Equal(t, "jur.gb.obl.core", d.Pack.PackRef)
	assert.Equal(t, "cert-jur.gb.obl.core@1.0", d.Pack.CertificationID)
	require.Len(t, d.Sources, 1)
	assert.Equal(t, "HMRC", d.Sources[0].Authority)
	assert.NotNil(t, d.InterpretationID)
	assert.Contains(t, strings.Join(d.Explanation, "|"), "Closure")
}

func TestCalculateDue_ExplicitAnswersNeverGuess(t *testing.T) {
	w := newWorld(t)
	w.addObligationPack(t, "jur.gb.obl.core", "1.0", "obl-1")
	r := w.resolver(t, time.Minute, nil, nil)
	ctx := context.Background()

	d, _ := r.CalculateDue(ctx, DueRequest{Jurisdiction: "FR", ObligationCode: "VAT_RETURN", Facts: domain.DueFacts{PeriodEnd: "2026-09-30"}})
	assert.Equal(t, domain.DueUnsupported, d.Outcome)
	d, _ = r.CalculateDue(ctx, dueReq(domain.DueFacts{PeriodEnd: "2025-06-30"}))
	assert.Equal(t, domain.DueNoRule, d.Outcome, "before any pack is in force the answer uses the obligation vocabulary, not the rule one")
	d, _ = r.CalculateDue(ctx, DueRequest{Jurisdiction: "GB", ObligationCode: "UNKNOWN_CODE", Facts: domain.DueFacts{PeriodEnd: "2026-09-30"}})
	assert.Equal(t, domain.DueNoRule, d.Outcome)
	d, _ = r.CalculateDue(ctx, dueReq(domain.DueFacts{PeriodEnd: "2026-09-15"}))
	assert.Equal(t, domain.DueInvalidFacts, d.Outcome)
	assert.Contains(t, d.Due.Message, "month end")
	d, _ = r.CalculateDue(ctx, dueReq(domain.DueFacts{PeriodEnd: "2026-09-30", ExtensionDays: 3}))
	assert.Equal(t, domain.DueExtensionNotAllowed, d.Outcome)

	for _, bad := range []DueRequest{
		{ObligationCode: "VAT_RETURN", Facts: domain.DueFacts{PeriodEnd: "2026-09-30"}},
		{Jurisdiction: "GB", Facts: domain.DueFacts{PeriodEnd: "2026-09-30"}},
		{Jurisdiction: "GB", ObligationCode: "VAT_RETURN"}, // no anchor fact at all
		{Jurisdiction: "GB", ObligationCode: "VAT_RETURN", Facts: domain.DueFacts{PeriodEnd: "2026-09-30"}, PackVersion: "1.0"},
	} {
		_, err := r.CalculateDue(ctx, bad)
		assert.ErrorIs(t, err, ErrBadRequest)
	}
}

func TestCalculateDue_TwoPacksDefiningTheSameObligationIsBlocked(t *testing.T) {
	w := newWorld(t)
	w.addObligationPack(t, "jur.gb.obl.core", "1.0", "obl-1")
	w.addObligationPack(t, "jur.gb.obl.extra", "1.0", "obl-2")
	r := w.resolver(t, time.Minute, nil, nil)

	d, err := r.CalculateDue(context.Background(), dueReq(domain.DueFacts{PeriodEnd: "2026-09-30"}))
	require.NoError(t, err)
	assert.Equal(t, domain.DueAmbiguous, d.Outcome, "JUR-NEG-24: a cross-pack conflict is blocked, not guessed")
	assert.Len(t, d.PacksConsulted, 2)
	assert.Nil(t, d.Due)

	rq := dueReq(domain.DueFacts{PeriodEnd: "2026-09-30"})
	rq.PackRef = "jur.gb.obl.extra"
	d, err = r.CalculateDue(context.Background(), rq)
	require.NoError(t, err)
	assert.Equal(t, domain.DueCalculated, d.Outcome, "restricting to one pack is an explicit choice")
	assert.Equal(t, "obl-2", d.Due.ObligationRuleID)
}

func TestCalculateDue_FailsClosedLikeRuleResolution(t *testing.T) {
	w := newWorld(t)
	id := w.addObligationPack(t, "jur.gb.obl.core", "1.0", "obl-1", domain.Holiday{Date: "2026-11-09", Name: "Closure"})
	// Tamper with a holiday after the artifact was signed.
	lp := w.src.loaded[id]
	lp.ArtifactJSON = strings.Replace(lp.ArtifactJSON, "2026-11-09", "2026-11-11", 1)
	var gotReasons []string
	r := w.resolver(t, time.Minute, nil, func(_, _ string, reasons []string) { gotReasons = reasons })

	d, err := r.CalculateDue(context.Background(), dueReq(domain.DueFacts{PeriodEnd: "2026-09-30"}))
	assert.Nil(t, d, "no answer at all: a quietly shifted holiday would shift a legal deadline")
	var ue *UnverifiedError
	require.True(t, errors.As(err, &ue))
	assert.Contains(t, ue.Failures[0].Reasons, domain.ReasonDigestMismatch)
	assert.Contains(t, gotReasons, domain.ReasonDigestMismatch, "a security event is raised")
}

func TestCalculateDue_PinnedReplayAndVersionSelection(t *testing.T) {
	w := newWorld(t)
	w.addObligationPack(t, "jur.gb.obl.core", "1.0", "obl-1", domain.Holiday{Date: "2026-11-09", Name: "Closure"})
	w.addObligationPack(t, "jur.gb.obl.core", "2.0", "obl-1", domain.Holiday{Date: "2026-11-09", Name: "Closure"}, domain.Holiday{Date: "2026-11-10", Name: "Amendment"})
	r := w.resolver(t, time.Minute, nil, nil)
	ctx := context.Background()

	d, err := r.CalculateDue(ctx, dueReq(domain.DueFacts{PeriodEnd: "2026-09-30"}))
	require.NoError(t, err)
	assert.Equal(t, "2.0", d.Pack.Version, "the highest released version in force")
	assert.Equal(t, "2026-11-11", d.Due.DueDate)

	rq := dueReq(domain.DueFacts{PeriodEnd: "2026-09-30"})
	rq.PackRef, rq.PackVersion = "jur.gb.obl.core", "1.0"
	d, err = r.CalculateDue(ctx, rq)
	require.NoError(t, err)
	assert.Equal(t, "2026-11-10", d.Due.DueDate, "JUR-NEG-13: a pinned replay reproduces the original date")

	rq.PackVersion = "9.9"
	_, err = r.CalculateDue(ctx, rq)
	assert.ErrorIs(t, err, ErrPackNotFound)
}
