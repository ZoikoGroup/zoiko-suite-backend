package catalogue

import (
	"encoding/json"
	"fmt"
	"strings"

	"zoiko.io/financial-control-svc/internal/domain"
)

// Registered source system names added in Wave 4.
const (
	SysIntercompany  = "intercompany-accounting"
	SysConsolidation = "consolidation"
)

// Wave4Options carries the entity-specific inputs a definition cannot guess.
type Wave4Options struct {
	// GL accounts (comma separated) where an entity books the RECEIVABLE side of an intercompany
	// transaction (debit-normal) and the PAYABLE side (credit-normal).
	ICReceivableAccounts string `json:"ic_receivable_accounts"`
	ICPayableAccounts    string `json:"ic_payable_accounts"`
}

// Wave4 returns the intercompany and consolidation controls that can run on data
// the platform holds today.
//
// Convention (must match how journals are booked): the SOURCE entity of an
// intercompany entry books its receivable (debit) and the TARGET entity its payable
// (credit).
//
// These controls are PARTIAL against the catalogue wording — see Status():
//   - A run covers ONE legal entity, so 014/015 tie each entity's intercompany legs to
//     that entity's own ledger. Because an intercompany entry must equal both legs, the
//     two entities' runs together establish reciprocity transitively; a direct
//     entity-A-versus-entity-B comparison in one run is not supported.
//   - 017 compares the ledger trial balance to the consolidation input, but no
//     CERTIFIED per-account entity trial balance exists, so the ledger side is uncertified.
func Wave4(effectiveFrom string, o Wave4Options) ([]domain.CreateControlDefinitionRequest, error) {
	var missing []string
	if effectiveFrom == "" {
		missing = append(missing, "effective_from")
	}
	if strings.TrimSpace(o.ICReceivableAccounts) == "" {
		missing = append(missing, "ic_receivable_accounts")
	}
	if strings.TrimSpace(o.ICPayableAccounts) == "" {
		missing = append(missing, "ic_payable_accounts")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: catalogue needs %s", domain.ErrInvalidArgument, strings.Join(missing, ", "))
	}

	evidence := j(map[string]any{"retention_class": "FINANCIAL_CONTROL", "retention_years": 7})
	// Every Wave 4 source rejects period_id (lifetime legs, or scoped by fiscal_period / run_id).
	pop := func(sys, name string, params map[string]string) json.RawMessage {
		return j(map[string]any{"system": sys, "population": name, "params": params, "no_period": true})
	}
	// One intercompany entry pairs with exactly one journal per leg, so no grouping is allowed:
	// two ledger journals for one entry are a defect, not an allocation.
	exact := j(map[string]any{"kind": "MATCH"})

	defs := []domain.CreateControlDefinitionRequest{
		{
			ControlCode: "FIN-CTRL-014", Name: "Intercompany reciprocal balances (source leg to ledger)", Domain: "Intercompany",
			ControlType: "BALANCE", Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "KEY",
			Frequency: "PERIOD_END", CloseGating: true,
			OwnerRole: "INTERCOMPANY_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			SourceSpec:   pop(SysIntercompany, "entry-legs", map[string]string{"leg": "source"}),
			TargetSpec:   pop(SysGL, "journal-account-totals", map[string]string{"account_codes": o.ICReceivableAccounts, "normal_balance": "DEBIT"}),
			InitialLogic: exact,
		},
		{
			ControlCode: "FIN-CTRL-015", Name: "Intercompany reciprocal activity (target leg to ledger)", Domain: "Intercompany",
			ControlType: "TRANSACTION", Assertions: []string{"COMPLETENESS", "CUTOFF"}, RiskTier: "STANDARD",
			Frequency: "PERIOD_END", OwnerRole: "INTERCOMPANY_OWNER",
			SourceSpec:   pop(SysIntercompany, "entry-legs", map[string]string{"leg": "target"}),
			TargetSpec:   pop(SysGL, "journal-account-totals", map[string]string{"account_codes": o.ICPayableAccounts, "normal_balance": "CREDIT"}),
			InitialLogic: exact,
		},
		{
			ControlCode: "FIN-CTRL-017", Name: "Entity trial balance to consolidation input", Domain: "Consolidation",
			ControlType: "INTERFACE", Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "KEY",
			Frequency: "PERIOD_END", CloseGating: true,
			OwnerRole: "CONSOLIDATION_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			// Bound per run: create the run with scope
			// {"fiscal_period": "<ledger fiscal period text>", "consolidation_run_id": "<uuid>"}
			// and the CHILD entity as the run's legal entity.
			SourceSpec:   pop(SysGL, "trial-balance", map[string]string{"fiscal_period": "${scope.fiscal_period}"}),
			TargetSpec:   pop(SysConsolidation, "balance-contributions", map[string]string{"run_id": "${scope.consolidation_run_id}"}),
			InitialLogic: exact,
		},
	}

	for i := range defs {
		d := &defs[i]
		d.EvidencePolicy = evidence
		d.InitialEffectiveFrom = effectiveFrom
		d.InitialTestPack = "wave4/1"
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("catalogue %s: %w", d.ControlCode, err)
		}
	}
	return defs, nil
}
