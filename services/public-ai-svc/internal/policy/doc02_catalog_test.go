package policy

import "testing"

// Every topic in the Doc 02 intent catalog (I01-I30, the "visitor asks"
// column, generated from the document) classifies to its own intent.
func TestDoc02CatalogExamples(t *testing.T) {
	cases := []struct{ want, q string }{
		{"I01", "What is ZoikoSuite?"},
		{"I02", "List modules/features"},
		{"I03", "Multi-entity / consolidation"},
		{"I04", "Cross-border / jurisdiction"},
		{"I05", "Business pricing"},
		{"I06", "Trial eligibility"},
		{"I07", "Upgrade/downgrade"},
		{"I08", "Book a demo"},
		{"I09", "Sales / enterprise inquiry"},
		{"I10", "Integrations and APIs"},
		{"I11", "Migration and onboarding"},
		{"I12", "Security and certifications"},
		{"I13", "Privacy, data processing"},
		{"I14", "Data residency"},
		{"I15", "Governance and approvals"},
		{"I16", "Evidence and audit"},
		{"I17", "Tax and compliance features"},
		{"I18", "Payroll and workforce"},
		{"I19", "Legal and contracts"},
		{"I20", "Reporting / analytics / AI"},
		{"I21", "Help and product documentation"},
		{"I22", "Account access / sign-in"},
		{"I23", "Existing customer ticket"},
		{"I24", "Contact human / sales"},
		{"I25", "Product availability / roadmap"},
		{"I26", "Competitor comparison"},
		{"I27", "Accessibility or site feedback"},
		{"I28", "Billing / cancellation questions"},
		{"I29", "Unexpected reply / unknown"},
		{"I30", "General Trust / responsible AI"},
	}
	for _, c := range cases {
		if got := Classify(c.q).Primary; got != c.want {
			t.Errorf("%s %q: got %s", c.want, c.q, got)
		}
	}
}
