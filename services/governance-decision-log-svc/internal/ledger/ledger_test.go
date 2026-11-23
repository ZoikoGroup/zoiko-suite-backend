package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestLinkValid_DetectsAnyChange(t *testing.T) {
	prev := Genesis
	curr := Link(prev, `{"decision_id":"d1"}`)
	if !LinkValid(prev, curr, `{"decision_id":"d1"}`) {
		t.Fatal("a link must verify against the canonical form it was computed from")
	}
	if LinkValid(prev, curr, `{"decision_id":"d2"}`) {
		t.Fatal("a changed canonical form must not verify")
	}
	other := Link(prev, "x")
	if LinkValid(other, curr, `{"decision_id":"d1"}`) {
		t.Fatal("a changed prev_hash must not verify")
	}
}

// RFC 6962 test vectors (certificate-transparency-go merkle tests), over the
// leaves "", 0x00, 0x10, 0x2021, 0x3031, 0x40414243, ...
func TestMerkleRoot_RFC6962Vectors(t *testing.T) {
	leaves := [][]byte{
		{},
		{0x00},
		{0x10},
		{0x20, 0x21},
		{0x30, 0x31},
		{0x40, 0x41, 0x42, 0x43},
		{0x50, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57},
		{0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f},
	}
	want := map[int]string{
		1: "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d",
		2: "fac54203e7cc696cf0dfcb42c92a1d9dbaf70ad9e621f4bd8d98662f00e3c125",
		3: "aeb6bcfe274b70a14fb067a5e5578264db0fa9b51af5e0ba159158f329e06e77",
		4: "d37ee418976dd95753c1c73862b9398fa2a2cf9b4ff0fdfe8b30cd95209614b7",
		5: "4e3bbb1f7b478dcfe71fb631631519a3bca12c9aefca1612bfce4c13a86264d4",
		6: "76e67dadbcdf1e10e1b74ddc608abd2f98dfb16fbce75277b5232a127f2087ef",
		7: "ddb89be403809e325750d3d263cd78929c2942b7942a34b77e122c9594a74c8c",
		8: "5dc9da79a70659a9ad559cb701ded9a2ab9d823aad2f4960cfe370eff4604328",
	}
	for n := 1; n <= len(leaves); n++ {
		got := hex.EncodeToString(MerkleRoot(leaves[:n]))
		if got != want[n] {
			t.Errorf("root over %d leaves = %s, want %s", n, got, want[n])
		}
	}
}

func TestMerkleRoot_Empty(t *testing.T) {
	empty := sha256.Sum256(nil)
	if !bytes.Equal(MerkleRoot(nil), empty[:]) {
		t.Fatal("the root of no leaves is sha256 of the empty string")
	}
}

func TestPackageHash_BindsEveryInput(t *testing.T) {
	a, b := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	base := PackageHash(Genesis, a, b, 5)
	for name, other := range map[string][]byte{
		"prev":  PackageHash(a, a, b, 5),
		"root":  PackageHash(Genesis, b, b, 5),
		"head":  PackageHash(Genesis, a, a, 5),
		"toSeq": PackageHash(Genesis, a, b, 6),
	} {
		if bytes.Equal(base, other) {
			t.Errorf("changing %s did not change the package hash", name)
		}
	}
}
