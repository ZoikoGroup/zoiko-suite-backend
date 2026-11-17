package domain

import "testing"

func req(min int, seats ...OfficeSeat) *SignatureRequirement {
	return &SignatureRequirement{Kind: "SIGNATURE_QUORUM", MinDistinctSigners: min, RequiredOffices: seats}
}

// C02-P0-06: two offices held by one human fill one seat, not two.
func TestOfficesFilled_DistinctHumansPerSeat(t *testing.T) {
	cfoCeo := req(2, OfficeSeat{"CFO", 1}, OfficeSeat{"CEO", 1})
	if cfoCeo.OfficesFilled([]Signer{{"p1", []string{"CFO", "CEO"}}}) {
		t.Fatal("one person holding CFO and CEO must not satisfy both seats")
	}
	if !cfoCeo.OfficesFilled([]Signer{{"p1", []string{"CFO", "CEO"}}, {"p2", []string{"CEO"}}}) {
		t.Fatal("CFO+CEO (p1 as CFO) and CEO (p2) fill both seats")
	}
	if cfoCeo.OfficesFilled([]Signer{{"p1", []string{"CEO"}}, {"p2", []string{"CEO"}}}) {
		t.Fatal("two CEOs do not make a CFO")
	}
	// Matching, not greedy: p1 (DIRECTOR, CFO) must take CFO so p2 can be DIRECTOR... or the reverse.
	dirCfo := req(2, OfficeSeat{"DIRECTOR", 1}, OfficeSeat{"CFO", 1})
	if !dirCfo.OfficesFilled([]Signer{{"p1", []string{"DIRECTOR", "CFO"}}, {"p2", []string{"DIRECTOR"}}}) {
		t.Fatal("an assignment exists: p1 as CFO, p2 as DIRECTOR")
	}
	twoDirectors := req(2, OfficeSeat{"DIRECTOR", 2})
	if twoDirectors.OfficesFilled([]Signer{{"p1", []string{"DIRECTOR"}}, {"p2", []string{"FINANCE_SIGNER"}}}) {
		t.Fatal("two Director seats need two Directors")
	}
	if !req(2).OfficesFilled(nil) {
		t.Fatal("no office seats are trivially filled (the count is checked separately)")
	}
}

func TestNextInOrder_SequentialSignatures(t *testing.T) {
	r := &SignatureRequirement{MinDistinctSigners: 2, Sequencing: "SEQUENTIAL", Order: []string{"CFO", "CEO"},
		RequiredOffices: []OfficeSeat{{"CFO", 1}, {"CEO", 1}}}
	if got := r.NextInOrder(nil); got != "CFO" {
		t.Fatalf("first signature must be CFO, got %q", got)
	}
	if got := r.NextInOrder([]Signer{{"p1", []string{"CFO"}}}); got != "CEO" {
		t.Fatalf("then CEO, got %q", got)
	}
	if got := r.NextInOrder([]Signer{{"p1", []string{"CFO"}}, {"p2", []string{"CEO"}}}); got != "" {
		t.Fatalf("order complete, got %q", got)
	}
	parallel := &SignatureRequirement{MinDistinctSigners: 2, Order: []string{"CFO"}}
	if parallel.NextInOrder(nil) != "" {
		t.Fatal("parallel signatures have no order")
	}
	if got := r.AcceptedOffices(); len(got) != 2 || got[0] != "CFO" || got[1] != "CEO" {
		t.Fatalf("accepted offices %v", got)
	}
}
