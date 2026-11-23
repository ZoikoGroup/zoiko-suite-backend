package main

import (
	"strings"
	"testing"
)

func TestUnlistedSkipFails(t *testing.T) {
	in := strings.Join([]string{
		`{"Action":"skip","Package":"p","Test":"TestAllowed"}`,
		`{"Action":"skip","Package":"p","Test":"TestNotAllowed/sub"}`,
		`{"Action":"pass","Package":"p","Test":"TestOther"}`,
	}, "\n")
	bad, failed, err := check(strings.NewReader(in), map[string]bool{"TestAllowed": true})
	if err != nil || failed || len(bad) != 1 || bad[0] != "p TestNotAllowed" {
		t.Fatalf("bad %v failed %v err %v", bad, failed, err)
	}
}
