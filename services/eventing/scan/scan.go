// Package scan is the shared content gate of ZS-SVC-M-001 (INT-06, §5.2): inbound
// files are malware-scanned and integrity-checked before they are parsed or acted on.
//
// It streams: a scanner reads the content through an io.Reader and never needs the
// whole file in memory. It fails closed: a scanner that cannot answer returns an
// error, and a deployment with no scanner configured is not given a "clean" default
// (see Required).
package scan

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Verdict is one scanner's answer.
type Verdict struct {
	Clean bool
	// Signature names what was found (empty when clean).
	Signature string
	// Engine names the scanner that produced the verdict, kept as evidence of what
	// actually looked at the bytes.
	Engine string
}

// Scanner inspects content. declaredType is what the uploader claimed (never trusted).
type Scanner interface {
	Name() string
	Scan(ctx context.Context, content io.Reader, declaredType string) (Verdict, error)
}

// ErrNoScanner is returned when scanning is demanded and nothing is configured.
var ErrNoScanner = errors.New("no malware scanner is configured")

// ErrUnavailable wraps every failure to obtain a verdict. The caller must treat the
// content as unscanned, never as clean.
var ErrUnavailable = errors.New("malware scanner unavailable")

// ── ClamAV (clamd INSTREAM) ─────────────────────────────────────────────────

// ClamAV scans through a clamd daemon over TCP using the INSTREAM protocol.
type ClamAV struct {
	address string
	timeout time.Duration
}

// NewClamAV returns a scanner for the clamd at address. timeout bounds the whole scan.
func NewClamAV(address string, timeout time.Duration) *ClamAV {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &ClamAV{address: address, timeout: timeout}
}

func (c *ClamAV) Name() string { return "clamav" }

// Scan streams content to clamd. clamd refuses a stream longer than its
// StreamMaxLength (25 MiB by default); that refusal comes back as an error, never as
// a clean verdict, so oversize content cannot slip through unscanned.
func (c *ClamAV) Scan(ctx context.Context, content io.Reader, _ string) (Verdict, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return Verdict{}, fmt.Errorf("%w: dial: %v", ErrUnavailable, err)
	}
	defer conn.Close()
	deadline := time.Now().Add(c.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return Verdict{}, fmt.Errorf("%w: command: %v", ErrUnavailable, err)
	}
	buf := make([]byte, 32*1024)
	for {
		n, rerr := content.Read(buf)
		if n > 0 {
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], uint32(n))
			if _, err := conn.Write(hdr[:]); err != nil {
				return Verdict{}, fmt.Errorf("%w: write: %v", ErrUnavailable, err)
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return Verdict{}, fmt.Errorf("%w: write: %v", ErrUnavailable, err)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return Verdict{}, fmt.Errorf("read content: %w", rerr)
		}
	}
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return Verdict{}, fmt.Errorf("%w: terminator: %v", ErrUnavailable, err)
	}
	resp, err := io.ReadAll(io.LimitReader(conn, 4096))
	if err != nil && len(resp) == 0 {
		return Verdict{}, fmt.Errorf("%w: read: %v", ErrUnavailable, err)
	}
	return parseClamResponse(string(resp))
}

// parseClamResponse reads "stream: OK", "stream: <name> FOUND" or "... ERROR".
func parseClamResponse(raw string) (Verdict, error) {
	r := strings.TrimSpace(strings.Trim(raw, "\x00"))
	switch {
	case strings.HasSuffix(r, " OK"):
		return Verdict{Clean: true, Engine: "clamav"}, nil
	case strings.HasSuffix(r, " FOUND"):
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(r, "stream:"), " FOUND"))
		return Verdict{Clean: false, Signature: name, Engine: "clamav"}, nil
	}
	return Verdict{}, fmt.Errorf("%w: unexpected answer %q", ErrUnavailable, truncate(r, 80))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ── Built-in checks ─────────────────────────────────────────────────────────

// eicar is the standard antivirus test string. Every real engine flags it, and the
// built-in check does too, so the malware path can be exercised without real malware.
const eicar = `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`

// Builtin applies engine-independent checks to structured text imports: the bytes must
// really be text of the declared kind (not an executable or archive renamed), contain
// no NUL bytes, be valid UTF-8, and not carry the EICAR test signature. It is NOT a
// malware engine and says so in its verdicts; deployments that import real files
// pair it with ClamAV in a Chain.
type Builtin struct {
	// MaxBytes bounds the content (0 means no bound here; the caller enforces its own).
	MaxBytes int64
}

func (b Builtin) Name() string { return "builtin-content-checks" }

// textTypes are the declared types Builtin accepts.
var textTypes = map[string]bool{
	"text/csv": true, "text/plain": true, "application/x-ndjson": true, "application/jsonl": true,
	"application/json": true, "application/csv": true,
}

func (b Builtin) Scan(_ context.Context, content io.Reader, declaredType string) (Verdict, error) {
	v := Verdict{Engine: b.Name()}
	dt := strings.ToLower(strings.TrimSpace(strings.SplitN(declaredType, ";", 2)[0]))
	if !textTypes[dt] {
		v.Signature = "Content.UnsupportedDeclaredType"
		return v, nil
	}
	limit := b.MaxBytes
	if limit <= 0 {
		limit = 1 << 62
	}
	var head []byte
	buf := make([]byte, 64*1024)
	var total int64
	var tail []byte    // the last bytes of the stream, so the EICAR string is found across reads
	var pending []byte // an incomplete trailing rune, completed by the next read
	for {
		n, err := content.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > limit {
				v.Signature = "Content.TooLarge"
				return v, nil
			}
			chunk := buf[:n]
			if len(head) < 512 {
				head = append(head, chunk[:minInt(n, 512-len(head))]...)
			}
			if bytes.IndexByte(chunk, 0) >= 0 {
				v.Signature = "Content.BinaryNUL"
				return v, nil
			}
			window := append(append([]byte(nil), tail...), chunk...)
			if bytes.Contains(window, []byte(eicar)) {
				v.Signature = "Eicar-Test-Signature"
				return v, nil
			}
			keep := minInt(len(eicar), len(window))
			tail = append([]byte(nil), window[len(window)-keep:]...)

			// UTF-8 is checked on the new bytes only, after the rune left incomplete by the
			// previous read; an incomplete rune at the end waits for the next read.
			data := append(append([]byte(nil), pending...), chunk...)
			cut := len(data)
			for i := 1; i <= 3 && i <= len(data); i++ {
				if utf8.RuneStart(data[len(data)-i]) {
					if !utf8.FullRune(data[len(data)-i:]) {
						cut = len(data) - i
					}
					break
				}
			}
			if !utf8.Valid(data[:cut]) {
				v.Signature = "Content.InvalidUTF8"
				return v, nil
			}
			pending = data[cut:]
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return Verdict{}, fmt.Errorf("read content: %w", err)
		}
	}
	if total == 0 {
		v.Signature = "Content.Empty"
		return v, nil
	}
	if len(pending) > 0 {
		v.Signature = "Content.InvalidUTF8" // the stream ended inside a character
		return v, nil
	}
	// declared text but the bytes sniff as something else (an executable, an archive, a PDF)
	if sniffed := http.DetectContentType(head); !strings.HasPrefix(sniffed, "text/") {
		v.Signature = "Content.TypeMismatch(" + strings.SplitN(sniffed, ";", 2)[0] + ")"
		return v, nil
	}
	v.Clean = true
	return v, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── Composition ─────────────────────────────────────────────────────────────

// Chain runs every scanner over the same content (it must be re-readable: open is
// called once per scanner) and is clean only if all of them are.
type Chain struct {
	Scanners []Scanner
}

func (c Chain) Name() string {
	names := make([]string, 0, len(c.Scanners))
	for _, s := range c.Scanners {
		names = append(names, s.Name())
	}
	return strings.Join(names, "+")
}

// ScanOpen runs the chain, calling open for a fresh reader per scanner. The first
// finding wins; any scanner error is returned as an error (never as clean).
func (c Chain) ScanOpen(ctx context.Context, open func() (io.ReadCloser, error), declaredType string) (Verdict, error) {
	if len(c.Scanners) == 0 {
		return Verdict{}, ErrNoScanner
	}
	engines := make([]string, 0, len(c.Scanners))
	for _, s := range c.Scanners {
		rc, err := open()
		if err != nil {
			return Verdict{}, fmt.Errorf("open content: %w", err)
		}
		v, err := s.Scan(ctx, rc, declaredType)
		_ = rc.Close()
		if err != nil {
			return Verdict{}, err
		}
		engines = append(engines, v.Engine)
		if !v.Clean {
			return v, nil
		}
	}
	return Verdict{Clean: true, Engine: strings.Join(engines, "+")}, nil
}

// Require returns the chain a deployment should scan with, or ErrNoScanner. A real
// engine address is required unless the operator explicitly accepts built-in checks
// only (development): there is no silent "everything is clean" default.
func Require(clamAddr string, allowBuiltinOnly bool, maxBytes int64, clamTimeout time.Duration) (Chain, error) {
	builtin := Builtin{MaxBytes: maxBytes}
	switch {
	case strings.TrimSpace(clamAddr) != "":
		return Chain{Scanners: []Scanner{builtin, NewClamAV(strings.TrimSpace(clamAddr), clamTimeout)}}, nil
	case allowBuiltinOnly:
		return Chain{Scanners: []Scanner{builtin}}, nil
	}
	return Chain{}, ErrNoScanner
}
