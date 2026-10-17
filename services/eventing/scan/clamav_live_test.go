package scan

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// Runs against a real clamd when CLAMD_ADDR is set (host:port); skipped otherwise. It is what shows the
// INSTREAM client works against the real protocol, not only against the fake daemon in scan_test.go.
func TestClamAVAgainstARealDaemon(t *testing.T) {
	addr := os.Getenv("CLAMD_ADDR")
	if addr == "" {
		t.Skip("CLAMD_ADDR not set")
	}
	c := NewClamAV(addr, 30*time.Second)

	v, err := c.Scan(bg, strings.NewReader("name,amount\nAcme,10.00\n"), "text/csv")
	if err != nil || !v.Clean || v.Engine != "clamav" {
		t.Fatalf("a clean file: %+v %v", v, err)
	}
	// the engine itself, not the built-in check, recognises the test file
	v, err = c.Scan(bg, strings.NewReader(eicar), "text/csv")
	if err != nil || v.Clean || !strings.Contains(v.Signature, "Eicar") {
		t.Fatalf("EICAR was not flagged by clamd: %+v %v", v, err)
	}
	// clamd's EICAR signature is whole-file: the test string buried inside a CSV is NOT flagged by clamd.
	// That is why an import scans with the built-in content checks as well: they flag it anywhere.
	embedded := "name,note\nAcme," + eicar + "\n"
	if v, err = c.Scan(bg, strings.NewReader(embedded), "text/csv"); err != nil || !v.Clean {
		t.Logf("clamd now flags an embedded test string too: %+v %v", v, err)
	}
	// a stream larger than several INSTREAM chunks still scans
	big := strings.Repeat("a,b,c\n", 400000)
	if v, err = c.Scan(bg, strings.NewReader(big), "text/csv"); err != nil || !v.Clean {
		t.Fatalf("a large clean stream: %+v %v", v, err)
	}

	// the whole chain, as the import runs it
	chain, err := Require(addr, false, 0, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	open := func(s string) func() (io.ReadCloser, error) {
		return func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(s)), nil }
	}
	if ev, err := chain.ScanOpen(bg, open(embedded), "text/csv"); err != nil || ev.Clean {
		t.Fatalf("the chain must flag the embedded test string (built-in check): %+v %v", ev, err)
	}
	cv, err := chain.ScanOpen(bg, open("a,b\n1,2\n"), "text/csv")
	if err != nil || !cv.Clean || cv.Engine != "builtin-content-checks+clamav" {
		t.Fatalf("chain: %+v %v", cv, err)
	}
}
