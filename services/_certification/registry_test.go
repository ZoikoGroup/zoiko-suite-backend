package certification

import "testing"

// The ledger may not cite a test that does not exist, skip a control, or claim coverage without evidence.
func TestLedgerIsWellFormedAndEveryCitedTestExists(t *testing.T) {
	if p := Problems(".."); len(p) > 0 {
		for _, s := range p {
			t.Error(s)
		}
	}
}

func TestParse(t *testing.T) {
	r, err := Parse("IC:store/pg_federation_test.go::TestFederation_Links")
	if err != nil || r.Service != "identity-context-svc" || r.Dir != "internal/store" || r.Test != "TestFederation_Links" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = Parse("EV:outbox/outbox_test.go::TestRelay_X")
	if err != nil || r.Service != "eventing" || r.Dir != "outbox" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"", "IC", "ZZ:a_test.go::TestX", "IC:a.go::TestX", "IC:a_test.go::NotATest", "IC:a_test.go"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// A DEVIATION is not accepted by being written down: an owner has to sign it. Withdrawing the signature reopens it.
func TestADeviationIsOpenUntilAnOwnerAccepts(t *testing.T) {
	isOpen := func(id string) bool {
		for _, e := range Open() {
			if e.ID == id {
				return true
			}
		}
		return false
	}
	for id := range Acceptances {
		if isOpen(id) {
			t.Fatalf("%s is accepted and must not be open", id)
		}
		saved := Acceptances[id]
		delete(Acceptances, id)
		reopened := isOpen(id)
		Acceptances[id] = saved
		if !reopened {
			t.Fatalf("%s must reopen when its acceptance is withdrawn", id)
		}
	}
	for _, e := range All() {
		if _, ok := Acceptances[e.ID]; ok && e.Status != Deviation && e.Status != OutOfScope {
			t.Fatalf("%s has an acceptance but is %s: only a deviation or out-of-scope entry takes one", e.ID, e.Status)
		}
	}
}

// Negative control: the checker must actually notice a fabricated citation, a missing file, and a hollow claim.
func TestCheckerNoticesBadCitations(t *testing.T) {
	saved := Controls[0]
	defer func() { Controls[0] = saved }()
	for name, e := range map[string]Entry{
		"no such test":  {ID: saved.ID, Status: Covered, Evidence: []string{"IC:store/pg_federation_test.go::TestNoSuchThing"}},
		"no such file":  {ID: saved.ID, Status: Covered, Evidence: []string{"IC:store/nope_test.go::TestX"}},
		"hollow claim":  {ID: saved.ID, Status: Covered},
		"unexplained":   {ID: saved.ID, Status: Partial, Evidence: saved.Evidence},
		"unknown state": {ID: saved.ID, Status: "DONE", Evidence: saved.Evidence},
	} {
		Controls[0] = e
		if len(Problems("..")) == 0 {
			t.Errorf("%s: the checker accepted it", name)
		}
	}
}
