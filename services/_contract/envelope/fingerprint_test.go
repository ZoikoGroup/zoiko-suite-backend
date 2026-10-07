package envelope

import (
	"testing"
)

func TestFingerprintFormat(t *testing.T) {
	b := NewFingerprintBuilder()
	b.Set("invoice_id", "inv-001")
	fp := b.Build()

	if !IsValidFingerprint(fp) {
		t.Fatalf("expected valid fingerprint format, got %q", fp)
	}

	if len(fp) != 71 { // "sha256:" (7) + 64 hex = 71
		t.Errorf("len(fp) = %d, want 71", len(fp))
	}

	// Invalid fingerprint checks
	invalidFingerprints := []string{
		"",
		"sha256:",
		"sha256:abc",
		"SHA256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // uppercase prefix
		"sha256:E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855", // uppercase hex
		"md5:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",    // wrong prefix
		"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85g", // invalid hex char 'g'
	}

	for _, ifp := range invalidFingerprints {
		if IsValidFingerprint(ifp) {
			t.Errorf("expected %q to be invalid fingerprint", ifp)
		}
	}
}

func TestDeterministicOutputAcrossRuns(t *testing.T) {
	build := func() string {
		return NewFingerprintBuilder().
			Set("vendor_id", "v-99").
			Set("bank_account", "GB82WEST12345678").
			SetAmount("total_amount", 1250.75, "GBP").
			AddCollection("line_ids", []string{"line-1", "line-2", "line-3"}).
			Build()
	}

	fp1 := build()
	fp2 := build()

	if fp1 != fp2 {
		t.Fatalf("non-deterministic output across runs: %q vs %q", fp1, fp2)
	}
}

func TestOrderIndependenceCanonicalization(t *testing.T) {
	// Builder 1: inserted in order A
	fp1 := NewFingerprintBuilder().
		Set("currency", "USD").
		Set("legal_entity_id", "le-alpha").
		SetAmount("net_amount", 5000.00, "USD").
		Set("vendor_id", "supp-100").
		AddCollection("items", []string{"z-item", "a-item", "m-item"}).
		Build()

	// Builder 2: inserted in reversed / randomized order
	fp2 := NewFingerprintBuilder().
		AddCollection("items", []string{"m-item", "z-item", "a-item"}). // scrambled collection
		Set("vendor_id", "supp-100").
		SetAmount("net_amount", 5000.00, "USD").
		Set("legal_entity_id", "le-alpha").
		Set("currency", "USD").
		Build()

	if fp1 != fp2 {
		t.Fatalf("expected identical fingerprints despite different insertion order: %q != %q", fp1, fp2)
	}

	// ComputeFingerprint map ordering test
	map1 := map[string]string{
		"a": "1",
		"b": "2",
		"c": "3",
	}
	map2 := map[string]string{
		"c": "3",
		"a": "1",
		"b": "2",
	}

	if ComputeFingerprint(map1) != ComputeFingerprint(map2) {
		t.Fatalf("ComputeFingerprint must be map-iteration order independent")
	}
}

func TestMaterialValueChangeProducesDifferentFingerprint(t *testing.T) {
	baseline := NewFingerprintBuilder().
		Set("invoice_id", "inv-42").
		Set("vendor_id", "v-1").
		Set("bank_account", "IBAN-ORIGINAL").
		SetAmount("net_amount", 1000.00, "EUR").
		Build()

	// 1. Modifying amount
	diffAmount := NewFingerprintBuilder().
		Set("invoice_id", "inv-42").
		Set("vendor_id", "v-1").
		Set("bank_account", "IBAN-ORIGINAL").
		SetAmount("net_amount", 1000.01, "EUR").
		Build()

	if baseline == diffAmount {
		t.Error("modifying net_amount must produce a different fingerprint")
	}

	// 2. Modifying currency
	diffCurrency := NewFingerprintBuilder().
		Set("invoice_id", "inv-42").
		Set("vendor_id", "v-1").
		Set("bank_account", "IBAN-ORIGINAL").
		SetAmount("net_amount", 1000.00, "GBP").
		Build()

	if baseline == diffCurrency {
		t.Error("modifying currency must produce a different fingerprint")
	}

	// 3. Modifying bank account (Critical Negative Path #1)
	diffBank := NewFingerprintBuilder().
		Set("invoice_id", "inv-42").
		Set("vendor_id", "v-1").
		Set("bank_account", "IBAN-FRAUDULENT").
		SetAmount("net_amount", 1000.00, "EUR").
		Build()

	if baseline == diffBank {
		t.Error("modifying bank_account must produce a different fingerprint")
	}

	// 4. Modifying collection line items
	withLines1 := NewFingerprintBuilder().
		Set("invoice_id", "inv-42").
		AddCollection("lines", []string{"line-1", "line-2"}).
		Build()

	withLines2 := NewFingerprintBuilder().
		Set("invoice_id", "inv-42").
		AddCollection("lines", []string{"line-1", "line-3"}).
		Build()

	if withLines1 == withLines2 {
		t.Error("modifying collection items must produce a different fingerprint")
	}
}

func TestWhitespaceAndNilNormalization(t *testing.T) {
	clean := NewFingerprintBuilder().
		Set("vendor_id", "v-100").
		Set("memo", "payment for services").
		Build()

	unclean := NewFingerprintBuilder().
		Set("  vendor_id  ", "  v-100  ").
		Set("memo", "payment for services\n").
		Build()

	if clean != unclean {
		t.Errorf("whitespace normalization failed: %q != %q", clean, unclean)
	}

	// Empty keys are ignored and do not affect fingerprint
	withEmpty := NewFingerprintBuilder().
		Set("", "ignored").
		Set("   ", "also ignored").
		Set("vendor_id", "v-100").
		Set("memo", "payment for services").
		Build()

	if clean != withEmpty {
		t.Errorf("empty keys must be ignored: %q != %q", clean, withEmpty)
	}
}

func TestAmountNormalization(t *testing.T) {
	// Test precision formatting: 100.5 and 100.50 must produce identical string
	fp1 := NewFingerprintBuilder().
		SetAmount("amount", 100.5, "usd").
		Build()

	fp2 := NewFingerprintBuilder().
		SetAmount("amount", 100.50, "USD"). // lowercase currency normalized to uppercase
		Build()

	if fp1 != fp2 {
		t.Errorf("amount normalization failed: %q != %q", fp1, fp2)
	}

	// Test negative amount handling
	fpNeg := NewFingerprintBuilder().
		SetAmount("adjustment", -45.20, "EUR").
		Build()

	if !IsValidFingerprint(fpNeg) {
		t.Errorf("negative amount fingerprint invalid: %q", fpNeg)
	}
}

func TestComputeCompositeFingerprint(t *testing.T) {
	fp1 := ComputeCompositeFingerprint("prop-01", "FROZEN", "1000.00", "800.00")
	fp2 := ComputeCompositeFingerprint("prop-01", "FROZEN", "1000.00", "800.00")
	fp3 := ComputeCompositeFingerprint("prop-01", "FROZEN", "1000.00", "850.00")

	if !IsValidFingerprint(fp1) {
		t.Fatalf("composite fingerprint format invalid: %q", fp1)
	}

	if fp1 != fp2 {
		t.Errorf("identical composite parts produced different fingerprints")
	}

	if fp1 == fp3 {
		t.Errorf("different composite parts produced identical fingerprints")
	}
}
