package domain_test

import (
	"testing"
	"time"

	"zoiko.io/migration-integrity-svc/internal/domain"
)

func TestCreateProgramRequest_Validate(t *testing.T) {
	valid := domain.CreateProgramRequest{
		ProgramID: "ACME-ONB-2026", SourceEstate: "LEGACY_ERP", TargetScope: "general-ledger-svc",
		OwnerPrincipalID: "owner-1", CutoverStrategy: "PHASED",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("expected a valid request to pass, got %v", err)
	}

	missing := valid
	missing.ProgramID = ""
	if err := missing.Validate(); err == nil {
		t.Fatal("expected a missing program_id to fail validation")
	}

	badStrategy := valid
	badStrategy.CutoverStrategy = "LIGHTSPEED"
	if err := badStrategy.Validate(); err == nil {
		t.Fatal("expected an unknown cutover_strategy to fail validation")
	}
}

func TestNextProgramStatus(t *testing.T) {
	cases := []struct {
		name      string
		current   domain.ProgramStatus
		requested domain.ProgramStatus
		wantErr   bool
	}{
		{"open to in_progress is allowed", domain.ProgramOpen, domain.ProgramInProgress, false},
		{"in_progress to complete is allowed", domain.ProgramInProgress, domain.ProgramComplete, false},
		{"open to complete skips a step and is refused", domain.ProgramOpen, domain.ProgramComplete, true},
		{"complete to in_progress goes backward and is refused", domain.ProgramComplete, domain.ProgramInProgress, true},
		{"requesting the current status replays idempotently", domain.ProgramOpen, domain.ProgramOpen, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := domain.NextProgramStatus(c.current, c.requested)
			if c.wantErr && err == nil {
				t.Fatalf("expected an error for %s -> %s", c.current, c.requested)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("expected no error for %s -> %s, got %v", c.current, c.requested, err)
			}
		})
	}
}

func TestRegisterExtractRequest_Validate(t *testing.T) {
	valid := domain.RegisterExtractRequest{
		LegalEntityID: "le-uk-1", SourceWatermark: "cursor-0001", GeneratedAt: time.Now(),
		ManifestHash: "sha256:aaa", RecordCount: 10,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("expected a valid request to pass, got %v", err)
	}

	noWatermark := valid
	noWatermark.SourceWatermark = ""
	if err := noWatermark.Validate(); err == nil {
		t.Fatal("expected a missing source_watermark to fail validation")
	}

	noGeneratedAt := valid
	noGeneratedAt.GeneratedAt = time.Time{}
	if err := noGeneratedAt.Validate(); err == nil {
		t.Fatal("expected a missing generated_at to fail validation")
	}

	negativeCount := valid
	negativeCount.RecordCount = -1
	if err := negativeCount.Validate(); err == nil {
		t.Fatal("expected a negative record_count to fail validation")
	}
}
