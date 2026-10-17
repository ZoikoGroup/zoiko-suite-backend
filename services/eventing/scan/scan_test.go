package scan

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

var bg = context.Background()

func scanBuiltin(t *testing.T, declared string, content []byte) Verdict {
	t.Helper()
	v, err := Builtin{}.Scan(bg, bytes.NewReader(content), declared)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestBuiltinAcceptsRealText(t *testing.T) {
	for name, c := range map[string][]byte{
		"csv":       []byte("name,amount\nAcme,10.00\n"),
		"jsonl":     []byte("{\"a\":1}\n{\"a\":2}\n"),
		"unicode":   []byte("name\nZoë Müller 東京\n"),
		"bom":       append([]byte{0xEF, 0xBB, 0xBF}, []byte("a,b\n1,2\n")...),
		"crlf":      []byte("a,b\r\n1,2\r\n"),
		"long line": []byte(strings.Repeat("x", 100000) + "\n"),
	} {
		if v := scanBuiltin(t, "text/csv", c); !v.Clean {
			t.Errorf("%s: flagged %q", name, v.Signature)
		}
	}
	if v := scanBuiltin(t, "text/csv; charset=utf-8", []byte("a\n1\n")); !v.Clean {
		t.Errorf("declared type parameters must be ignored: %q", v.Signature)
	}
}

// INT-NP: malware. The EICAR string stands in for it; every real engine flags it too.
func TestBuiltinFlagsEICARAndHidingPlaces(t *testing.T) {
	e := []byte(eicar)
	for name, c := range map[string][]byte{
		"alone":        e,
		"inside csv":   []byte("name,note\nAcme," + eicar + "\n"),
		"at the end":   append([]byte(strings.Repeat("a,b\n", 50000)), e...),
		"at the start": append(append([]byte(nil), e...), []byte("\na,b\n")...),
	} {
		if v := scanBuiltin(t, "text/csv", c); v.Clean || v.Signature != "Eicar-Test-Signature" {
			t.Errorf("%s: %+v", name, v)
		}
	}
}

func TestBuiltinFindsEICARSplitAcrossReads(t *testing.T) {
	// a reader that returns one byte at a time puts the signature across every boundary
	content := []byte("a,b\n1," + eicar + "\n")
	v, err := Builtin{}.Scan(bg, iotestOneByte{bytes.NewReader(content)}, "text/csv")
	if err != nil || v.Clean {
		t.Fatalf("%+v %v", v, err)
	}
}

type iotestOneByte struct{ r io.Reader }

func (o iotestOneByte) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestBuiltinRefusesContentThatIsNotWhatItClaims(t *testing.T) {
	exe := append([]byte("MZ\x90\x00\x03\x00\x00\x00"), bytes.Repeat([]byte{0x41}, 64)...)
	zip := []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00abcdefgh")
	pdf := []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n")
	for name, c := range map[string][]byte{
		"executable renamed .csv": exe,
		"zip renamed .csv":        zip,
		"pdf renamed .csv":        pdf,
		"nul bytes":               []byte("a,b\n1,\x002\n"),
		"invalid utf-8":           []byte("a,b\n1,\xff\xfe\n"),
		"empty":                   {},
	} {
		if v := scanBuiltin(t, "text/csv", c); v.Clean {
			t.Errorf("%s was accepted", name)
		}
	}
	for _, declared := range []string{"application/pdf", "application/zip", "application/x-msdownload", "image/png", ""} {
		if v := scanBuiltin(t, declared, []byte("a,b\n1,2\n")); v.Clean {
			t.Errorf("declared type %q was accepted", declared)
		}
	}
}

func TestBuiltinEnforcesItsSizeBound(t *testing.T) {
	b := Builtin{MaxBytes: 100}
	if v, _ := b.Scan(bg, strings.NewReader(strings.Repeat("a", 101)), "text/csv"); v.Clean || v.Signature != "Content.TooLarge" {
		t.Fatalf("%+v", v)
	}
	if v, _ := b.Scan(bg, strings.NewReader(strings.Repeat("a", 100)), "text/csv"); !v.Clean {
		t.Fatalf("%+v", v)
	}
}

func TestBuiltinAcceptsAMultiByteRuneSplitAcrossReads(t *testing.T) {
	content := []byte("name\n" + strings.Repeat("東京", 30000) + "\n")
	v, err := Builtin{}.Scan(bg, iotestOneByte{bytes.NewReader(content)}, "text/csv")
	if err != nil || !v.Clean {
		t.Fatalf("a rune split across reads is valid UTF-8: %+v %v", v, err)
	}
}

// ── ClamAV against a fake clamd ─────────────────────────────────────────────

type fakeClamd struct {
	ln       net.Listener
	mu       sync.Mutex
	received []byte
	answer   string
	hangup   bool
}

func newFakeClamd(t *testing.T, answer string) *fakeClamd {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeClamd{ln: ln, answer: answer}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeClamd) serve(c net.Conn) {
	defer c.Close()
	cmd := make([]byte, len("zINSTREAM\x00"))
	if _, err := io.ReadFull(c, cmd); err != nil || string(cmd) != "zINSTREAM\x00" {
		return
	}
	var got []byte
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 {
			break
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(c, chunk); err != nil {
			return
		}
		got = append(got, chunk...)
	}
	f.mu.Lock()
	f.received = got
	hang := f.hangup
	f.mu.Unlock()
	if hang {
		return
	}
	_, _ = c.Write([]byte(f.answer + "\x00"))
}

func TestClamAVProtocolAndVerdicts(t *testing.T) {
	content := bytes.Repeat([]byte("a,b\n1,2\n"), 20000) // several INSTREAM chunks
	ok := newFakeClamd(t, "stream: OK")
	v, err := NewClamAV(ok.ln.Addr().String(), 5*time.Second).Scan(bg, bytes.NewReader(content), "text/csv")
	if err != nil || !v.Clean || v.Engine != "clamav" {
		t.Fatalf("%+v %v", v, err)
	}
	if !bytes.Equal(ok.received, content) {
		t.Fatal("clamd did not receive exactly the content")
	}
	bad := newFakeClamd(t, "stream: Win.Test.EICAR_HDB-1 FOUND")
	v, err = NewClamAV(bad.ln.Addr().String(), 5*time.Second).Scan(bg, bytes.NewReader(content), "text/csv")
	if err != nil || v.Clean || v.Signature != "Win.Test.EICAR_HDB-1" {
		t.Fatalf("%+v %v", v, err)
	}
}

// A scanner that cannot answer is never a clean verdict.
func TestClamAVFailsClosed(t *testing.T) {
	for name, c := range map[string]*ClamAV{
		"unreachable": NewClamAV("127.0.0.1:1", time.Second),
		"errors":      NewClamAV(newFakeClamd(t, "INSTREAM size limit exceeded. ERROR").ln.Addr().String(), time.Second),
		"garbage":     NewClamAV(newFakeClamd(t, "hello").ln.Addr().String(), time.Second),
	} {
		v, err := c.Scan(bg, strings.NewReader("a,b\n"), "text/csv")
		if err == nil || v.Clean || !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %+v %v", name, v, err)
		}
	}
	hang := newFakeClamd(t, "")
	hang.hangup = true
	if v, err := NewClamAV(hang.ln.Addr().String(), time.Second).Scan(bg, strings.NewReader("a,b\n"), "text/csv"); err == nil || v.Clean {
		t.Errorf("a daemon that hangs up: %+v %v", v, err)
	}
}

func TestChainNeedsEveryScannerToPass(t *testing.T) {
	open := func(s string) func() (io.ReadCloser, error) {
		return func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(s)), nil }
	}
	clean := newFakeClamd(t, "stream: OK")
	infected := newFakeClamd(t, "stream: Bad.Thing FOUND")
	for name, tc := range map[string]struct {
		chain    Chain
		content  string
		wantErr  bool
		wantOK   bool
		wantSign string
	}{
		"both clean":          {Chain{[]Scanner{Builtin{}, NewClamAV(clean.ln.Addr().String(), time.Second)}}, "a,b\n1,2\n", false, true, ""},
		"builtin flags first": {Chain{[]Scanner{Builtin{}, NewClamAV(clean.ln.Addr().String(), time.Second)}}, eicar, false, false, "Eicar-Test-Signature"},
		"clamav flags":        {Chain{[]Scanner{Builtin{}, NewClamAV(infected.ln.Addr().String(), time.Second)}}, "a,b\n", false, false, "Bad.Thing"},
		"engine down":         {Chain{[]Scanner{Builtin{}, NewClamAV("127.0.0.1:1", time.Second)}}, "a,b\n", true, false, ""},
		"no scanners at all":  {Chain{}, "a,b\n", true, false, ""},
	} {
		v, err := tc.chain.ScanOpen(bg, open(tc.content), "text/csv")
		if (err != nil) != tc.wantErr || v.Clean != tc.wantOK || v.Signature != tc.wantSign {
			t.Errorf("%s: %+v err=%v", name, v, err)
		}
	}
	if _, err := (Chain{}).ScanOpen(bg, open("x"), "text/csv"); !errors.Is(err, ErrNoScanner) {
		t.Errorf("an empty chain must say there is no scanner: %v", err)
	}
	v, _ := Chain{[]Scanner{Builtin{}, NewClamAV(clean.ln.Addr().String(), time.Second)}}.ScanOpen(bg, open("a,b\n"), "text/csv")
	if v.Engine != "builtin-content-checks+clamav" {
		t.Errorf("the verdict must record every engine that looked: %q", v.Engine)
	}
}

func TestRequireHasNoSilentCleanDefault(t *testing.T) {
	if _, err := Require("", false, 0, 0); !errors.Is(err, ErrNoScanner) {
		t.Fatalf("nothing configured must be an error, not a pass: %v", err)
	}
	if c, err := Require("  ", false, 0, 0); err == nil || len(c.Scanners) != 0 {
		t.Fatalf("a blank address is not an address: %v", err)
	}
	dev, err := Require("", true, 1<<20, 0)
	if err != nil || dev.Name() != "builtin-content-checks" {
		t.Fatalf("explicit built-in only: %v %q", err, dev.Name())
	}
	prod, err := Require("clamd:3310", false, 1<<20, time.Second)
	if err != nil || len(prod.Scanners) != 2 || prod.Name() != "builtin-content-checks+clamav" {
		t.Fatalf("%v %q", err, prod.Name())
	}
}

func TestParseClamResponse(t *testing.T) {
	for raw, want := range map[string]struct {
		clean bool
		sig   string
		err   bool
	}{
		"stream: OK\x00":                      {true, "", false},
		"stream: Eicar-Signature FOUND\x00":   {false, "Eicar-Signature", false},
		"stream: Some Name With Spaces FOUND": {false, "Some Name With Spaces", false},
		"INSTREAM size limit exceeded. ERROR": {false, "", true},
		"":                                    {false, "", true},
		"OK but not at the end":               {false, "", true},
	} {
		v, err := parseClamResponse(raw)
		if (err != nil) != want.err || v.Clean != want.clean || v.Signature != want.sig {
			t.Errorf("%q: %+v %v", raw, v, err)
		}
	}
}

func TestBuiltinRefusesAStreamThatEndsInsideACharacter(t *testing.T) {
	trunc := append([]byte("name\n"), []byte("東")[:2]...)
	if v := scanBuiltin(t, "text/csv", trunc); v.Clean {
		t.Fatal("a truncated multi-byte character was accepted")
	}
}
