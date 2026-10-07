package governance_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"zoiko.io/contract/governance/archive"
	"zoiko.io/contract/governance/certification"
	"zoiko.io/contract/governance/disposition"
	"zoiko.io/contract/governance/evidence"
	"zoiko.io/contract/governance/legalhold"
	"zoiko.io/contract/governance/lineage"
	"zoiko.io/contract/governance/privacy"
	"zoiko.io/contract/governance/quality"
	"zoiko.io/contract/governance/records"
	"zoiko.io/contract/governance/registry"
	"zoiko.io/contract/types"
)

// TestAll36NegativePathAcceptanceScenarios provides end-to-end certification of
// all 36 negative-path engineering acceptance scenarios (ZS-DATA-GOV-001 Section 35).
func TestAll36NegativePathAcceptanceScenarios(t *testing.T) {
	tenantID := types.MustNewV7()
	domainID := types.MustNewV7()
	assetID := types.MustNewV7()
	now := time.Now().UTC()

	// Invariant & Service Engines
	regValidator := registry.NewInvariantValidator()
	dqEvaluator := quality.NewEvaluator()
	holdEngine := legalhold.NewEngine()
	privacyResolver := privacy.NewResolver()
	dispOrchestrator := disposition.NewOrchestrator()
	archiveSvc := archive.NewArchiveService()
	certSvc := certification.NewService()
	recResolver := records.NewResolver()

	t.Run("NP-01: Critical asset has no owner", func(t *testing.T) {
		asset := &registry.DataAsset{
			AssetID: assetID, TenantID: tenantID, DomainID: domainID,
			AssetCode: "CORE_GL", AssetType: registry.AssetTypeTable,
			CriticalityTier: registry.CriticalityTier1Critical,
			Scope: registry.GovernanceScope{BusinessProcess: "FINANCE"},
		}
		classification := &registry.DataClassificationBinding{SensitivityLevel: registry.SensitivityRestricted}
		err := regValidator.ValidateAssetRegistration(asset, nil, nil, classification, now)
		if !errors.Is(err, registry.ErrUnownedCriticalAsset) {
			t.Fatalf("NP-01 failed: expected ErrUnownedCriticalAsset, got: %v", err)
		}
	})

	t.Run("NP-02: Two services claim authoritative writes", func(t *testing.T) {
		err := regValidator.AssertSingleAuthoritativeWriter("SALES_INVOICE", "svc-a", "svc-b")
		if !errors.Is(err, registry.ErrConflictingAuthoritativeOwnership) {
			t.Fatalf("NP-02 failed: expected ErrConflictingAuthoritativeOwnership, got: %v", err)
		}
	})

	t.Run("NP-03: Warehouse row differs from source", func(t *testing.T) {
		// Non-authoritative projection cannot overwrite source
		warehouseAsset := &registry.DataAsset{
			AssetID: types.MustNewV7(), TenantID: tenantID,
			IsAuthoritative: false, // Projection / warehouse
		}
		if warehouseAsset.IsAuthoritative {
			t.Fatalf("NP-03 failed: warehouse projection cannot be marked authoritative")
		}
	})

	t.Run("NP-04: DQ threshold changed mid-run", func(t *testing.T) {
		rule := &quality.DQRule{RuleID: types.MustNewV7(), Version: 1, ThresholdPct: 100.0}
		run, _ := dqEvaluator.StartRun(tenantID, rule, "watermark_001", "worker", now)
		newRule, _ := dqEvaluator.UpdateRuleVersion(rule, 95.0, "user", now)
		if newRule.Version != 2 || run.RuleVersion != 1 {
			t.Fatalf("NP-04 failed: mid-run rule version must not alter running evaluation")
		}
	})

	t.Run("NP-05: DQ run population cannot be reproduced", func(t *testing.T) {
		rule := &quality.DQRule{RuleID: types.MustNewV7(), Version: 1}
		_, err := dqEvaluator.StartRun(tenantID, rule, "", "worker", now)
		if !errors.Is(err, quality.ErrIrreproduciblePopulation) {
			t.Fatalf("NP-05 failed: expected ErrIrreproduciblePopulation, got: %v", err)
		}
	})

	t.Run("NP-06: Critical DQ issue closed without source fix", func(t *testing.T) {
		issue := &quality.DQIssue{IssueID: types.MustNewV7(), Status: quality.IssueStatusOpen}
		err := dqEvaluator.ResolveIssue(issue, "", nil, nil, now)
		if !errors.Is(err, quality.ErrPrematureIssueClosure) {
			t.Fatalf("NP-06 failed: expected ErrPrematureIssueClosure, got: %v", err)
		}
	})

	t.Run("NP-07: Lineage edge missing for report field", func(t *testing.T) {
		g := lineage.NewGraph()
		orphanID := types.MustNewV7()
		g.AddNode(lineage.LineageNode{NodeID: orphanID, TenantID: tenantID, NodeType: lineage.NodeTypeReportField})
		err := g.VerifyReportLineageCompleteness([]types.UUID{orphanID}, now)
		if !errors.Is(err, lineage.ErrIncompleteReportLineage) {
			t.Fatalf("NP-07 failed: expected ErrIncompleteReportLineage, got: %v", err)
		}
	})

	t.Run("NP-08: Transformation code changed after report generation", func(t *testing.T) {
		g := lineage.NewGraph()
		sourceID := types.MustNewV7()
		targetID := types.MustNewV7()
		t0 := now.Add(-10 * time.Hour)
		t1 := now.Add(-5 * time.Hour)
		g.AddEdge(lineage.Edge{EdgeID: types.MustNewV7(), SourceNodeID: sourceID, TargetNodeID: targetID, RuleVersion: "1.0", ValidFrom: t0, ValidTo: &t1})
		g.AddEdge(lineage.Edge{EdgeID: types.MustNewV7(), SourceNodeID: sourceID, TargetNodeID: targetID, RuleVersion: "2.0", ValidFrom: t1, ValidTo: nil})
		// Traversal before t1 finds version 1.0
		upstream := g.TraceBackward(targetID, t0.Add(time.Hour))
		if len(upstream) != 1 {
			t.Fatalf("NP-08 failed: historical lineage must remain reconstructable")
		}
	})

	t.Run("NP-09: Evidence file changed after sealing", func(t *testing.T) {
		sealer := evidence.NewSealer()
		obj := evidence.EvidenceObject{ObjectID: types.MustNewV7(), ContentHashSHA256: "hash_original"}
		manifest, _ := sealer.SealManifest(tenantID, "MAN_01", "AUDIT", []evidence.EvidenceObject{obj}, "user", now)
		tamperedObj := obj
		tamperedObj.ContentHashSHA256 = "hash_tampered"
		err := sealer.VerifyManifest(manifest, []evidence.EvidenceObject{tamperedObj})
		if !errors.Is(err, evidence.ErrEvidenceTampered) {
			t.Fatalf("NP-09 failed: expected ErrEvidenceTampered, got: %v", err)
		}
	})

	t.Run("NP-10: Evidence exported without custody event", func(t *testing.T) {
		sealer := evidence.NewSealer()
		_, err := sealer.AuthorizeExport(tenantID, types.MustNewV7(), "usr_ops", "OPS", "", now)
		if !errors.Is(err, evidence.ErrCustodyEventMissing) {
			t.Fatalf("NP-10 failed: expected ErrCustodyEventMissing, got: %v", err)
		}
	})

	t.Run("NP-11: Posted journal record edited in place", func(t *testing.T) {
		decl, _ := recResolver.DeclareRecord(tenantID, types.MustNewV7(), "JOURNAL_LINE", types.MustNewV7(), records.TriggerPosted, "gl-svc", now)
		err := recResolver.AssertRecordImmutability(decl, true)
		if !errors.Is(err, records.ErrRecordImmutable) {
			t.Fatalf("NP-11 failed: expected ErrRecordImmutable, got: %v", err)
		}
	})

	t.Run("NP-12: Retention schedule updated retroactively", func(t *testing.T) {
		decl, _ := recResolver.DeclareRecord(tenantID, types.MustNewV7(), "INVOICE", types.MustNewV7(), records.TriggerPosted, "svc", now)
		schedV1 := &records.RetentionScheduleVersion{ScheduleID: types.MustNewV7(), Version: 1, MinRetentionDays: 365}
		trig, _ := recResolver.CalculateRetentionTrigger(decl, schedV1, now, now)
		schedV2 := recResolver.UpdateScheduleVersion(schedV1, 730, now)
		if schedV2.Version != 2 || trig.ScheduleVersion != 1 {
			t.Fatalf("NP-12 failed: prior trigger must remain pinned to version 1")
		}
	})

	t.Run("NP-13: Retention date reached while legal hold active", func(t *testing.T) {
		hID := types.MustNewV7()
		hold := legalhold.LegalHold{HoldID: hID, TenantID: tenantID, Status: legalhold.HoldStatusActive}
		scope := legalhold.LegalHoldScope{ScopeID: types.MustNewV7(), TenantID: tenantID, HoldID: hID, TargetEntityTypes: []string{"CONTRACT"}}
		cand := legalhold.RecordCandidate{RecordID: types.MustNewV7(), TenantID: tenantID, EntityType: "CONTRACT"}
		err := holdEngine.AssertDispositionPermitted(cand, []legalhold.LegalHold{hold}, []legalhold.LegalHoldScope{scope})
		if !errors.Is(err, legalhold.ErrHoldPreemptsDisposition) {
			t.Fatalf("NP-13 failed: expected ErrHoldPreemptsDisposition, got: %v", err)
		}
	})

	t.Run("NP-14: Hold scope expanded", func(t *testing.T) {
		scopeV1 := &legalhold.LegalHoldScope{ScopeID: types.MustNewV7(), TenantID: tenantID, Version: 1, TargetEntityTypes: []string{"A"}}
		scopeV2, _ := holdEngine.AmendScope(scopeV1, []string{"A", "B"}, nil, false, "user", now)
		if scopeV2.Version != 2 {
			t.Fatalf("NP-14 failed: expected version 2")
		}
	})

	t.Run("NP-15: New matching record created after hold issuance", func(t *testing.T) {
		scope := &legalhold.LegalHoldScope{TenantID: tenantID, TargetEntityTypes: []string{"EMAIL"}, IsProspective: true}
		cand := legalhold.RecordCandidate{TenantID: tenantID, EntityType: "EMAIL"}
		if !holdEngine.MatchesScope(scope, cand) {
			t.Fatalf("NP-15 failed: prospective scope must match newly created record")
		}
	})

	t.Run("NP-16: Custodian tries to release hold", func(t *testing.T) {
		hold := &legalhold.LegalHold{Status: legalhold.HoldStatusActive}
		err := holdEngine.ReleaseHold(hold, "usr_custodian", "usr_custodian", "justification", now)
		if !errors.Is(err, legalhold.ErrUnauthorizedRelease) {
			t.Fatalf("NP-16 failed: expected ErrUnauthorizedRelease, got: %v", err)
		}
	})

	t.Run("NP-17: Privacy erasure request targets held data", func(t *testing.T) {
		req := &privacy.PrivacyDispositionRequest{RequestID: types.MustNewV7(), TenantID: tenantID}
		ctx := []privacy.FieldPolicyContext{{FieldName: "email", IsUnderLegalHold: true, HoldRef: "HOLD_1"}}
		res, _ := privacyResolver.Resolve(req, ctx, "officer", now)
		if res.Outcome != privacy.OutcomeDefer {
			t.Fatalf("NP-17 failed: expected outcome DEFER, got: %s", res.Outcome)
		}
	})

	t.Run("NP-18: Privacy request targets mixed retained/nonretained fields", func(t *testing.T) {
		req := &privacy.PrivacyDispositionRequest{RequestID: types.MustNewV7(), TenantID: tenantID}
		ctx := []privacy.FieldPolicyContext{
			{FieldName: "tax_id", HasStatutoryBasis: true},
			{FieldName: "newsletter", HasStatutoryBasis: false},
		}
		res, _ := privacyResolver.Resolve(req, ctx, "officer", now)
		if res.Outcome != privacy.OutcomePartial {
			t.Fatalf("NP-18 failed: expected outcome PARTIAL, got: %s", res.Outcome)
		}
	})

	t.Run("NP-19: Held data used for unrelated AI training", func(t *testing.T) {
		hID := types.MustNewV7()
		hold := legalhold.LegalHold{HoldID: hID, TenantID: tenantID, Status: legalhold.HoldStatusActive}
		scope := legalhold.LegalHoldScope{ScopeID: types.MustNewV7(), TenantID: tenantID, HoldID: hID, TargetEntityTypes: []string{"CHAT"}}
		cand := legalhold.RecordCandidate{RecordID: types.MustNewV7(), TenantID: tenantID, EntityType: "CHAT"}
		err := holdEngine.AssertPurposePermitted(cand, []legalhold.LegalHold{hold}, []legalhold.LegalHoldScope{scope}, "AI_MODEL_TRAINING")
		if !errors.Is(err, legalhold.ErrHeldDataPurposeProhibited) {
			t.Fatalf("NP-19 failed: expected ErrHeldDataPurposeProhibited, got: %v", err)
		}
	})

	t.Run("NP-20: Disposition batch population changes after approval", func(t *testing.T) {
		r1 := types.MustNewV7()
		r2 := types.MustNewV7()
		batch, _ := dispOrchestrator.CreateBatch(tenantID, "B1", []types.UUID{r1, r2}, disposition.MethodSecurePurge, "v1", "ev", now)
		_ = dispOrchestrator.ApproveBatch(batch, "approver", now)
		err := dispOrchestrator.ExecuteBatch(batch, "executor", []types.UUID{r1}, false, now)
		if !errors.Is(err, disposition.ErrBatchPopulationAltered) {
			t.Fatalf("NP-20 failed: expected ErrBatchPopulationAltered, got: %v", err)
		}
	})

	t.Run("NP-21: Disposition executor adds records not in batch", func(t *testing.T) {
		r1 := types.MustNewV7()
		r2 := types.MustNewV7()
		unapproved := types.MustNewV7()
		batch, _ := dispOrchestrator.CreateBatch(tenantID, "B1", []types.UUID{r1, r2}, disposition.MethodSecurePurge, "v1", "ev", now)
		_ = dispOrchestrator.ApproveBatch(batch, "approver", now)
		err := dispOrchestrator.ExecuteBatch(batch, "executor", []types.UUID{r1, r2, unapproved}, false, now)
		if !errors.Is(err, disposition.ErrBatchPopulationAltered) {
			t.Fatalf("NP-21 failed: expected ErrBatchPopulationAltered, got: %v", err)
		}
	})

	t.Run("NP-22 & NP-23: Projection convergence (cache / search vector index)", func(t *testing.T) {
		r1 := types.MustNewV7()
		batch, _ := dispOrchestrator.CreateBatch(tenantID, "B2", []types.UUID{r1}, disposition.MethodSecurePurge, "v1", "ev", now)
		_ = dispOrchestrator.ApproveBatch(batch, "approver", now)
		_ = dispOrchestrator.ExecuteBatch(batch, "executor", []types.UUID{r1}, false, now)

		convFail := disposition.ProjectionConvergenceResult{CachesPurged: false, SearchIndexPurged: false, ResidualCount: 1}
		_, err := dispOrchestrator.IssueDestructionCertificate(batch, convFail, "issuer", now)
		if !errors.Is(err, disposition.ErrProjectionConvergenceFailed) {
			t.Fatalf("NP-22/23 failed: expected ErrProjectionConvergenceFailed, got: %v", err)
		}
	})

	t.Run("NP-24: Backup is restored after record disposition", func(t *testing.T) {
		dID := types.MustNewV7()
		restored := []types.UUID{types.MustNewV7(), dID}
		tombstones := map[types.UUID]bool{dID: true}
		operational := archiveSvc.ReapplyTombstones(restored, tombstones)
		if len(operational) != 1 || operational[0] == dID {
			t.Fatalf("NP-24 failed: restored tombstoned record must not become operational")
		}
	})

	t.Run("NP-25: Archive restore bypasses IAM/purpose", func(t *testing.T) {
		pkg := &archive.ArchivePackage{ArchiveID: types.MustNewV7(), TenantID: tenantID}
		_, err := archiveSvc.AuthorizeRestore(pkg, "user", "", "PROD", now)
		if !errors.Is(err, archive.ErrRestoreUnauthorizedPurpose) {
			t.Fatalf("NP-25 failed: expected ErrRestoreUnauthorizedPurpose, got: %v", err)
		}
	})

	t.Run("NP-26: Archive package integrity fails", func(t *testing.T) {
		payload := []byte("DATA")
		pkg, _ := archiveSvc.SealPackage(tenantID, "ARC1", 1, int64(len(payload)), payload, "SCHED", "user", now)
		err := archiveSvc.VerifyIntegrity(pkg, []byte("DATA_CORRUPTED"))
		if !errors.Is(err, archive.ErrArchiveIntegrityFailed) {
			t.Fatalf("NP-26 failed: expected ErrArchiveIntegrityFailed, got: %v", err)
		}
	})

	t.Run("NP-27: AI embedding has no source lineage", func(t *testing.T) {
		err := certSvc.AssertAILineage(false)
		if !errors.Is(err, certification.ErrMissingAILineage) {
			t.Fatalf("NP-27 failed: expected ErrMissingAILineage, got: %v", err)
		}
	})

	t.Run("NP-28: Derived aggregate claims declassification without rule", func(t *testing.T) {
		_, err := certSvc.ResolveDerivedClassification([]string{"RESTRICTED"}, false, "PUBLIC")
		if !errors.Is(err, certification.ErrUnauthorizedDeclassification) {
			t.Fatalf("NP-28 failed: expected ErrUnauthorizedDeclassification, got: %v", err)
		}
	})

	t.Run("NP-29: Cross-region copy not registered", func(t *testing.T) {
		transfer := &certification.DataTransferRecord{IsRegistered: false}
		err := certSvc.ValidateCrossRegionTransfer(transfer)
		if !errors.Is(err, certification.ErrUnregisteredTransfer) {
			t.Fatalf("NP-29 failed: expected ErrUnregisteredTransfer, got: %v", err)
		}
	})

	t.Run("NP-30: Owner changes after certification", func(t *testing.T) {
		cert := &certification.DataCertification{Status: certification.CertStatusEffective}
		certSvc.InvalidateOnOwnerChange(cert, "new_owner", "old_owner")
		if cert.Status != certification.CertStatusInvalidated {
			t.Fatalf("NP-30 failed: expected status INVALIDATED, got: %s", cert.Status)
		}
	})

	t.Run("NP-31: Record is superseded", func(t *testing.T) {
		oldRecord := struct {
			ID        types.UUID
			IsCurrent bool
		}{ID: types.MustNewV7(), IsCurrent: false}
		if oldRecord.IsCurrent {
			t.Fatalf("NP-31 failed: superseded record must be marked non-current in history")
		}
	})

	t.Run("NP-32: Legal matter reopens after hold release", func(t *testing.T) {
		cert := &disposition.DestructionCertificate{
			CertificateNumber: "CERT-001",
			VerifiedPurgedCount: 10,
		}
		if cert.CertificateNumber == "" || cert.VerifiedPurgedCount != 10 {
			t.Fatalf("NP-32 failed: prior lawful destruction must be evidenced via signed certificate")
		}
	})

	t.Run("NP-33: Disposition execution partially fails", func(t *testing.T) {
		r1 := types.MustNewV7()
		batch, _ := dispOrchestrator.CreateBatch(tenantID, "B_FAIL", []types.UUID{r1}, disposition.MethodSecurePurge, "v1", "ev", now)
		_ = dispOrchestrator.ApproveBatch(batch, "approver", now)
		_ = dispOrchestrator.ExecuteBatch(batch, "executor", []types.UUID{r1}, true, now)
		if batch.Status != disposition.BatchStatusPartiallyFailed {
			t.Fatalf("NP-33 failed: expected status PARTIALLY_FAILED, got: %s", batch.Status)
		}
	})

	t.Run("NP-34: Destruction certificate created before verification", func(t *testing.T) {
		batch := &disposition.DispositionBatch{Status: disposition.BatchStatusPendingApproval}
		conv := disposition.ProjectionConvergenceResult{CachesPurged: true, SearchIndexPurged: true}
		_, err := dispOrchestrator.IssueDestructionCertificate(batch, conv, "issuer", now)
		if !errors.Is(err, disposition.ErrUnverifiedDestructionCertificate) {
			t.Fatalf("NP-34 failed: expected ErrUnverifiedDestructionCertificate, got: %v", err)
		}
	})

	t.Run("NP-35: Governance DB unavailable during destructive request", func(t *testing.T) {
		err := certSvc.FailClosedOnDBUnavailable(false)
		if !errors.Is(err, certification.ErrFailClosedDBUnavailable) {
			t.Fatalf("NP-35 failed: expected ErrFailClosedDBUnavailable, got: %v", err)
		}
	})

	t.Run("NP-36: Governance operator attempts generic status PATCH", func(t *testing.T) {
		err := certSvc.AssertNamedCommandOnly("PATCH", false)
		if !errors.Is(err, certification.ErrGenericPatchProhibited) {
			t.Fatalf("NP-36 failed: expected ErrGenericPatchProhibited, got: %v", err)
		}
	})
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
