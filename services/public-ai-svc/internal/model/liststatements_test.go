package model

import "testing"

// Doc 02 §16 / ServerContract: verbatim statements; three or more as a list.
func TestListStatements(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"A."}, "A."},
		{[]string{"A.", "B."}, "A. B."},
		{[]string{"A.", "B.", "C."}, "- A.\n- B.\n- C."},
	}
	for _, c := range cases {
		if got := ListStatements(c.in); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}
