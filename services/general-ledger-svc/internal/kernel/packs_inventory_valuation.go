package kernel

// The inventory-valuation pack: the valuation postings of an inventory service that owns quantities, cost layers and valuation runs,
// beyond the stock movements the inventory-standard pack covers. The inventory service computes (a period cost of sales, a landed
// cost spread over the layers and the cost of goods already consumed, a net-realisable-value write-down); these rules turn the result
// into balanced, dimensioned, evidenced journals and refuse one that lacks its evidence.

func inventoryValuationPacks() []RulePack {
	inv := map[string]string{"location": "WH-1", "item_class": "FINISHED"}
	return []RulePack{{
		Name:        "inventory-valuation",
		Description: "Inventory valuation: period cost of sales from a valuation run, the reclass of goods-received cost already expensed, landed cost spread over stock and cost of sales, and net-realisable-value write-downs",
		Rules: []PackRule{
			{Name: "inventory-cogs-recognized", Domain: "INVENTORY", EventType: "INVENTORY.COGS_RECOGNIZED",
				Content: withDims(content(nil, exists("run_ref", "valuation_policy"), line("L1", Debit, "COGS", "amount"), line("L2", Credit, "INVENTORY", "amount")), "location", "item_class"),
				TestPack: []TestCase{
					goldenD("cost of sales from a valuation run", "INVENTORY.COGS_RECOGNIZED", "", inv, f{"amount": "1800.00", "run_ref": "VR-1", "valuation_policy": "fifo-v2"}, dr("5000", "1800.00"), cr("1400", "1800.00")),
					refusedD("a run without its reference is not posted", "INVENTORY.COGS_RECOGNIZED", "", inv, f{"amount": "1800.00", "valuation_policy": "fifo-v2"}, FamRule),
					refusedD("the location and item class are required", "INVENTORY.COGS_RECOGNIZED", "", map[string]string{"location": "WH-1"}, f{"amount": "1800.00", "run_ref": "VR-1", "valuation_policy": "fifo-v2"}, FamDim),
				}},
			{Name: "inventory-grni-expense-reclassified", Domain: "INVENTORY", EventType: "INVENTORY.GRNI_EXPENSE_RECLASSIFIED",
				Content: withDims(content(nil, exists("run_ref", "receipt_basis"), line("L1", Debit, "INVENTORY", "amount"), line("L2", Credit, "GRNI_EXPENSE_RECLASS", "amount")), "location", "item_class"),
				TestPack: []TestCase{
					goldenD("receipt cost the payables side expensed moves into stock", "INVENTORY.GRNI_EXPENSE_RECLASSIFIED", "", inv, f{"amount": "950.00", "run_ref": "VR-1", "receipt_basis": "ap-linked"}, dr("1400", "950.00"), cr("5200", "950.00")),
					refusedD("the receipt basis is required", "INVENTORY.GRNI_EXPENSE_RECLASSIFIED", "", inv, f{"amount": "950.00", "run_ref": "VR-1"}, FamRule),
				}},
			{Name: "inventory-landed-cost-allocated", Domain: "INVENTORY", EventType: "INVENTORY.LANDED_COST_ALLOCATED",
				Content: withDims(content(nil, exists("allocation_ref", "valuation_evidence_ref"),
					line("L1", Debit, "INVENTORY", "inventory_share", gt0("inventory_share")),
					line("L2", Debit, "COGS", "cogs_share", gt0("cogs_share")),
					line("L3", Credit, "LANDED_COST_OFFSET", "amount")), "location", "item_class"),
				TestPack: []TestCase{
					goldenD("landed cost split between stock still held and goods already sold", "INVENTORY.LANDED_COST_ALLOCATED", "", inv,
						f{"amount": "100.00", "inventory_share": "60.00", "cogs_share": "40.00", "allocation_ref": "LC-1", "valuation_evidence_ref": "freight-inv-7"}, dr("1400", "60.00"), dr("5000", "40.00"), cr("2080", "100.00")),
					goldenD("landed cost on stock that is all still held", "INVENTORY.LANDED_COST_ALLOCATED", "", inv,
						f{"amount": "100.00", "inventory_share": "100.00", "cogs_share": "0.00", "allocation_ref": "LC-2", "valuation_evidence_ref": "freight-inv-8"}, dr("1400", "100.00"), cr("2080", "100.00")),
					refusedD("shares that do not add up to the amount are the inventory service error, not the ledger to fix", "INVENTORY.LANDED_COST_ALLOCATED", "", inv,
						f{"amount": "100.00", "inventory_share": "60.00", "cogs_share": "30.00", "allocation_ref": "LC-3", "valuation_evidence_ref": "freight-inv-9"}, FamBal),
					refusedD("landed cost needs its evidence", "INVENTORY.LANDED_COST_ALLOCATED", "", inv,
						f{"amount": "100.00", "inventory_share": "100.00", "cogs_share": "0.00", "allocation_ref": "LC-4"}, FamRule),
				}},
			{Name: "inventory-nrv-write-down", Domain: "INVENTORY", EventType: "INVENTORY.NRV_WRITE_DOWN",
				Content: withDims(content(nil, exists("write_down_ref", "valuation_evidence_ref"), line("L1", Debit, "INVENTORY_WRITE_DOWN_EXPENSE", "amount"), line("L2", Credit, "INVENTORY", "amount")), "location", "item_class"),
				TestPack: []TestCase{
					goldenD("a write-down to net realisable value", "INVENTORY.NRV_WRITE_DOWN", "", inv, f{"amount": "400.00", "write_down_ref": "WD-1", "valuation_evidence_ref": "nrv-memo-3"}, dr("5300", "400.00"), cr("1400", "400.00")),
					refusedD("a write-down without valuation evidence is refused", "INVENTORY.NRV_WRITE_DOWN", "", inv, f{"amount": "400.00", "write_down_ref": "WD-1"}, FamRule),
				}},
		},
	}}
}
