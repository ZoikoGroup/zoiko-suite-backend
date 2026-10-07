package operating

import (
	"testing"
	"time"

	"zoiko.io/contract/types"
)

func TestReceivableOpenItem_SettlementLifecycle(t *testing.T) {
	item := ReceivableOpenItem{
		ReceivableOpenItemID: types.MustNewV7(),
		TenantID:             types.MustNewV7(),
		LegalEntityID:        types.MustNewV7(),
		SourceDocumentID:     types.MustNewV7(),
		CustomerPartyRoleID:  types.MustNewV7(),
		OriginalAmount:       types.MustParseMoney("100.00"),
		OpenAmount:           types.MustParseMoney("100.00"),
		CurrencyCode:         "EUR",
		DueDate:              time.Now().AddDate(0, 0, 30),
		Status:               OpenItemStatusOpen,
	}

	// 1. Partial settlement: apply 40.00
	err := item.ApplyPayment(types.MustParseMoney("40.00"))
	if err != nil {
		t.Fatalf("partial payment failed: %v", err)
	}
	if item.OpenAmount.StringTrimmed() != "60.00" {
		t.Fatalf("expected open amount 60.00, got %s", item.OpenAmount.StringTrimmed())
	}
	if item.Status != OpenItemStatusPartSettled {
		t.Fatalf("expected status PART_SETTLED, got %s", item.Status)
	}

	// 2. Over-settlement attempt: apply 70.00 (exceeds 60.00) -> must fail
	err = item.ApplyPayment(types.MustParseMoney("70.00"))
	if err == nil {
		t.Fatalf("expected over-settlement to be rejected")
	}

	// 3. Final settlement: apply remaining 60.00
	err = item.ApplyPayment(types.MustParseMoney("60.00"))
	if err != nil {
		t.Fatalf("final payment failed: %v", err)
	}
	if !item.OpenAmount.IsZero() {
		t.Fatalf("expected open amount 0, got %s", item.OpenAmount.StringTrimmed())
	}
	if item.Status != OpenItemStatusSettled {
		t.Fatalf("expected status SETTLED, got %s", item.Status)
	}
}

func TestPayableOpenItem_SettlementLifecycle(t *testing.T) {
	item := PayableOpenItem{
		PayableOpenItemID:   types.MustNewV7(),
		TenantID:            types.MustNewV7(),
		LegalEntityID:       types.MustNewV7(),
		SourceDocumentID:    types.MustNewV7(),
		SupplierPartyRoleID: types.MustNewV7(),
		OriginalAmount:      types.MustParseMoney("250.00"),
		OpenAmount:          types.MustParseMoney("250.00"),
		CurrencyCode:        "USD",
		DueDate:             time.Now().AddDate(0, 0, 15),
		Status:              OpenItemStatusOpen,
	}

	// Full payment: apply 250.00
	err := item.ApplyPayment(types.MustParseMoney("250.00"))
	if err != nil {
		t.Fatalf("payment failed: %v", err)
	}
	if !item.OpenAmount.IsZero() {
		t.Fatalf("expected open amount 0, got %s", item.OpenAmount.StringTrimmed())
	}
	if item.Status != OpenItemStatusSettled {
		t.Fatalf("expected status SETTLED, got %s", item.Status)
	}
}
