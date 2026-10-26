package handler

import (
	"testing"

	"zoiko.io/general-ledger-svc/internal/domain"
)

func TestParseManualJournalPolicy(t *testing.T) {
	p, err := ParseManualJournalPolicy("10000.00", "opening, closing")
	if err != nil || p.EvidenceThresholdCents != 1000000 || !p.EvidenceTypes[domain.JournalTypeOpening] || !p.EvidenceTypes[domain.JournalTypeClosing] {
		t.Fatalf("%+v %v", p, err)
	}
	if p, err = ParseManualJournalPolicy("", ""); err != nil || p.EvidenceThresholdCents != 0 || p.EvidenceTypes != nil {
		t.Fatalf("an empty configuration requires nothing: %+v %v", p, err)
	}
	// a typo must stop the service, not quietly switch the control off
	for _, bad := range [][2]string{{"ten thousand", ""}, {"-5", ""}, {"", "ADJUSTMENTS"}} {
		if _, err := ParseManualJournalPolicy(bad[0], bad[1]); err == nil {
			t.Fatalf("%v must be refused", bad)
		}
	}
}

func TestEvidenceRule(t *testing.T) {
	p := ManualJournalPolicy{EvidenceThresholdCents: 1000000, EvidenceTypes: map[domain.JournalType]bool{domain.JournalTypeOpening: true}}
	manual := &domain.JournalHeader{JournalType: domain.JournalTypeStandard}
	if p.evidenceRequired(manual, 1000000) || !p.evidenceRequired(manual, 1000001) {
		t.Fatal("evidence is needed above the threshold, not at it")
	}
	if !p.evidenceRequired(&domain.JournalHeader{JournalType: domain.JournalTypeOpening}, 1) {
		t.Fatal("a designated type always needs evidence")
	}
	if p.evidenceRequired(&domain.JournalHeader{JournalType: domain.JournalTypeOpening, EvidenceRefs: []string{"d"}}, 99999999) {
		t.Fatal("evidence supplied")
	}
	src := "evt"
	if p.evidenceRequired(&domain.JournalHeader{JournalType: domain.JournalTypeOpening, SourceEventID: &src}, 99999999) {
		t.Fatal("a system journal is not a manual journal")
	}
	if (ManualJournalPolicy{}).evidenceRequired(manual, 99999999) {
		t.Fatal("the zero policy requires nothing")
	}
}
