package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"zoiko.io/treasury-svc/internal/domain"
)

// legacyHashFromFloat is the exact pre-A34 implementation, kept here only
// to produce the value an existing row would have stored.
func legacyHashFromFloat(amount float64, currencyCode, srcAcctID, tgtAcctID string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%.4f|%s|%s|%s", amount, currencyCode, srcAcctID, tgtAcctID)))
	return hex.EncodeToString(sum[:])
}

// For ordinary amounts the exact four-place text and the old float
// formatting agree, so the unversioned exact hash is also a valid legacy
// hash. New rows add the v2 marker on top of this digest.
func TestHashProtectedFields_MatchesTheLegacyFormatForOrdinaryAmounts(t *testing.T) {
	for _, amount := range []float64{0, 0.1, 10.5, 10.005, 250.5, 999999.9999} {
		d := domain.Decimal(strconv.FormatFloat(amount, 'f', -1, 64))
		got := hashProtectedFields(d.MoneyText(), "USD", "src-1", "tgt-2")
		want := legacyHashFromFloat(amount, "USD", "src-1", "tgt-2")
		if got != want {
			t.Errorf("amount %v: hashProtectedFields = %s, legacy = %s", amount, got, want)
		}
	}
}

func TestProtectedFieldHash_IsVersionedAndFitsTheColumn(t *testing.T) {
	got := protectedFieldHash("250.5", "USD", "src-1", "tgt-2")
	if !strings.HasPrefix(got, protectedHashV2Prefix) {
		t.Fatalf("new hash %q lacks the %q version marker", got, protectedHashV2Prefix)
	}
	if len(got) > 64 {
		t.Fatalf("new hash is %d characters, column is VARCHAR(64)", len(got))
	}
}

// This is the case the old "byte-identical for every NUMERIC(20,4) value"
// assumption missed. The float64 nearest 596267352650.6170 formats as
// ...6169 under %.4f, so the stored legacy hash is not the exact-decimal
// hash. Approval must still accept it for a row written before A34.
func TestProtectedFieldHashMatches_AcceptsTheLegacyHashWhereFloat64Rounded(t *testing.T) {
	const (
		storedAmount = domain.Decimal("596267352650.6170")
		legacyText   = "596267352650.6169"
	)
	legacy := hashProtectedFields(legacyText, "USD", "src-1", "tgt-2")
	exact := protectedFieldHash(storedAmount, "USD", "src-1", "tgt-2")
	if exact == legacy {
		t.Fatal("test fixture no longer distinguishes the exact and legacy renderings")
	}
	if !protectedFieldHashMatches(storedAmount, "USD", "src-1", "tgt-2", legacy) {
		t.Fatal("a pre-A34 row's legacy hash must still verify")
	}
	if !protectedFieldHashMatches(storedAmount, "USD", "src-1", "tgt-2", exact) {
		t.Fatal("a new row's v2 exact hash must verify")
	}
	unversionedExact := hashProtectedFields(storedAmount.MoneyText(), "USD", "src-1", "tgt-2")
	if !protectedFieldHashMatches(storedAmount, "USD", "src-1", "tgt-2", unversionedExact) {
		t.Fatal("an unversioned exact hash from an intermediate build must verify")
	}
	if protectedFieldHashMatches(storedAmount, "USD", "src-1", "tgt-2", "deadbeef") {
		t.Fatal("an unrelated stored hash must not verify")
	}
	if protectedFieldHashMatches("596267352650.6171", "USD", "src-1", "tgt-2", legacy) {
		t.Fatal("a tampered amount must not match the legacy hash")
	}
}

// At this magnitude one float64 represents both ...6169 and ...6170, so the
// old float rendering of the latter is the exact text of the former. A
// versioned new-row hash must not accept that legacy collision; only the
// pre-A34 unversioned hash path may.
func TestProtectedFieldHashMatches_V2RejectsTheLegacyCollision(t *testing.T) {
	const (
		storedAmount = domain.Decimal("596267352650.6169")
		tampered     = domain.Decimal("596267352650.6170")
	)
	unversionedExact := hashProtectedFields(storedAmount.MoneyText(), "USD", "src-1", "tgt-2")
	if legacyProtectedFieldHash(tampered, "USD", "src-1", "tgt-2") != unversionedExact {
		t.Fatal("test fixture no longer produces the legacy collision")
	}
	v2 := protectedFieldHash(storedAmount, "USD", "src-1", "tgt-2")
	if protectedFieldHashMatches(tampered, "USD", "src-1", "tgt-2", v2) {
		t.Fatal("a v2 exact hash accepted a different amount through the legacy rendering")
	}
}

// A NUMERIC(20,4) read back as text carries trailing zeros ("250.5000");
// the exact hash must still match the value created from "250.5".
func TestProtectedFieldHash_ReadBackScaleDoesNotChangeTheHash(t *testing.T) {
	created := protectedFieldHash("250.5", "USD", "src-1", "tgt-2")
	readBack := protectedFieldHash("250.5000", "USD", "src-1", "tgt-2")
	if created != readBack {
		t.Fatalf("created hash %s != read-back hash %s", created, readBack)
	}
}

// The fingerprint is live, so both sides of the BNK-06 comparison use the
// same algorithm. For ordinary amounts it still equals the old value.
func TestTransferFingerprint_MatchesTheLegacyFormatForOrdinaryAmounts(t *testing.T) {
	transfer := &domain.TreasuryTransfer{
		TransferID: "t1", Status: domain.TransferApproved, Amount: "250.5", CurrencyCode: "USD",
		SourceBankAccountID: "src-1", TargetBankAccountID: "tgt-2", CheckerPrincipalID: "checker-1",
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%.4f|%s|%s|%s|%s",
		transfer.TransferID, transfer.Status, 250.5, transfer.CurrencyCode,
		transfer.SourceBankAccountID, transfer.TargetBankAccountID, transfer.CheckerPrincipalID)
	want := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got := domain.TransferFingerprint(transfer); got != want {
		t.Fatalf("TransferFingerprint = %s, legacy = %s", got, want)
	}
}
