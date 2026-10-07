package types

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMoneyDecimal_ExactArithmetic(t *testing.T) {
	// Floating point fail case: 0.1 + 0.2 != 0.3 in float64
	m1 := MustParseMoney("0.1")
	m2 := MustParseMoney("0.2")
	sum := m1.Add(m2)

	if sum.StringTrimmed() != "0.30" {
		t.Fatalf("expected 0.30, got %s", sum.StringTrimmed())
	}

	// Large numbers where float64 collapses bits
	large1 := MustParseMoney("10000000000000000.000000000001")
	large2 := MustParseMoney("0.000000000002")
	largeSum := large1.Add(large2)

	expectedLarge := "10000000000000000.000000000003"
	if largeSum.StringTrimmed() != expectedLarge {
		t.Fatalf("expected %s, got %s", expectedLarge, largeSum.StringTrimmed())
	}
}

func TestMoneyDecimal_BankersRounding(t *testing.T) {
	// Round half to even:
	// 2.225 -> 2.22 (even)
	// 2.235 -> 2.24 (even)
	m1 := MustParseMoney("2.225")
	r1 := m1.RoundToCurrencyMinorUnits(2)
	if r1.StringTrimmed() != "2.22" {
		t.Fatalf("expected 2.22, got %s", r1.StringTrimmed())
	}

	m2 := MustParseMoney("2.235")
	r2 := m2.RoundToCurrencyMinorUnits(2)
	if r2.StringTrimmed() != "2.24" {
		t.Fatalf("expected 2.24, got %s", r2.StringTrimmed())
	}

	// Negative half to even
	m3 := MustParseMoney("-2.225")
	r3 := m3.RoundToCurrencyMinorUnits(2)
	if r3.StringTrimmed() != "-2.22" {
		t.Fatalf("expected -2.22, got %s", r3.StringTrimmed())
	}
}

func TestMoneyDecimal_JSON_And_SQL(t *testing.T) {
	type Sample struct {
		Amount MoneyDecimal `json:"amount"`
	}

	original := Sample{Amount: MustParseMoney("12345.6789")}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded Sample
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if original.Amount.Cmp(decoded.Amount) != 0 {
		t.Fatalf("expected %s, got %s", original.Amount.String(), decoded.Amount.String())
	}

	// Test SQL Value and Scan
	val, err := original.Amount.Value()
	if err != nil {
		t.Fatalf("value error: %v", err)
	}

	var scanned MoneyDecimal
	if err := scanned.Scan(val); err != nil {
		t.Fatalf("scan error: %v", err)
	}

	if original.Amount.Cmp(scanned) != 0 {
		t.Fatalf("expected %s, got %s", original.Amount.String(), scanned.String())
	}
}

func TestRateDecimal_Multiplication(t *testing.T) {
	amount := MustParseMoney("1000.00")
	// 20% VAT = 0.200000000000000000
	rate, err := FromPercentage("20.0")
	if err != nil {
		t.Fatalf("rate parse error: %v", err)
	}

	tax := amount.MulRate(rate)
	if tax.RoundToCurrencyMinorUnits(2).StringTrimmed() != "200.00" {
		t.Fatalf("expected 200.00, got %s", tax.RoundToCurrencyMinorUnits(2).StringTrimmed())
	}
}

func TestUUIDv7_RFC9562(t *testing.T) {
	fixedTime := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	u, err := NewV7AtTime(fixedTime)
	if err != nil {
		t.Fatalf("NewV7AtTime error: %v", err)
	}

	version, err := u.Version()
	if err != nil {
		t.Fatalf("version error: %v", err)
	}
	if version != 7 {
		t.Fatalf("expected version 7, got %d", version)
	}

	ts, err := u.Timestamp()
	if err != nil {
		t.Fatalf("timestamp error: %v", err)
	}

	diff := ts.Sub(fixedTime)
	if diff < -time.Millisecond || diff > time.Millisecond {
		t.Fatalf("expected timestamp within 1ms of %v, got %v (diff %v)", fixedTime, ts, diff)
	}
}

func TestBitemporal_AsOf_Evaluation(t *testing.T) {
	validFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	validTo := time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC)
	recordedAt := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	supersededAt := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	record := BitemporalRecord{
		ValidFrom:    validFrom,
		ValidTo:      &validTo,
		RecordedAt:   recordedAt,
		SupersededAt: &supersededAt,
	}

	if err := record.Validate(); err != nil {
		t.Fatalf("record validation failed: %v", err)
	}

	// 1. Business active in Feb 2026? Yes
	febDate := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
	if !record.IsBusinessActiveAt(febDate) {
		t.Fatalf("expected active in Feb 2026")
	}

	// 2. Business active in August 2026? No (validTo was June 30)
	augDate := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if record.IsBusinessActiveAt(augDate) {
		t.Fatalf("expected inactive in Aug 2026")
	}

	// 3. System knowledge on Feb 10 (before supersession)? Yes
	febSysTime := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	if !record.WasKnownAt(febSysTime) {
		t.Fatalf("expected known on Feb 10")
	}

	// 4. System knowledge on April 1 (after supersession)? No
	aprSysTime := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if record.WasKnownAt(aprSysTime) {
		t.Fatalf("expected superseded on April 1")
	}

	// 5. As-Of evaluation: On Feb 15 business date, querying on Feb 20 system time -> True
	if !record.AsOf(febDate, time.Date(2026, 2, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expected AsOf query to be true")
	}

	// 6. As-Of evaluation: On Feb 15 business date, querying on April 20 system time -> False (superseded)
	if record.AsOf(febDate, time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expected AsOf query to be false due to supersession")
	}
}

func TestLineageEdge_Validation(t *testing.T) {
	edge := LineageEdge{
		EdgeID:            MustNewV7(),
		TenantID:          MustNewV7(),
		LegalEntityID:     MustNewV7(),
		FromStage:         Stage1SourceRecord,
		FromObjectType:    "SupplierInvoice",
		FromObjectID:      MustNewV7(),
		FromObjectVersion: 1,
		ToStage:           Stage3AccountingTaxEvent,
		ToObjectType:      "AccountingEvent",
		ToObjectID:        MustNewV7(),
		ToObjectVersion:   1,
		CreatedAt:         time.Now().UTC(),
	}

	if err := edge.Validate(); err != nil {
		t.Fatalf("expected valid lineage edge, got: %v", err)
	}

	invalidEdge := edge
	invalidEdge.TenantID = NilUUID
	if err := invalidEdge.Validate(); err == nil {
		t.Fatalf("expected invalid lineage edge without tenant_id")
	}
}
