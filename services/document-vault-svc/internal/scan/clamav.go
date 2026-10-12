package scan

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// ClamAVScanner connects to a ClamAV daemon (clamd) via TCP
// and scans document content using the INSTREAM protocol.
type ClamAVScanner struct {
	address string
	timeout time.Duration
}

// NewClamAVScanner returns a Scanner backed by a ClamAV daemon at the given address.
func NewClamAVScanner(address string, timeout time.Duration) *ClamAVScanner {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &ClamAVScanner{
		address: address,
		timeout: timeout,
	}
}

// Scan sends the content to clamd using the INSTREAM command and returns
// a Result indicating whether the content is clean or infected.
// If clamd is unreachable or returns an error, Scan returns a non-nil error
// so callers can fail closed.
func (s *ClamAVScanner) Scan(ctx context.Context, content []byte, _ string) (Result, error) {
	d := net.Dialer{Timeout: s.timeout}
	conn, err := d.DialContext(ctx, "tcp", s.address)
	if err != nil {
		return Result{}, fmt.Errorf("clamav dial error (%s): %w", s.address, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(s.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	// Send zINSTREAM command with null terminator
	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return Result{}, fmt.Errorf("clamav command write error: %w", err)
	}

	// Stream chunks in format: <length (4 bytes, big endian)><data>
	reader := bytes.NewReader(content)
	buf := make([]byte, 32*1024)
	for {
		n, rErr := reader.Read(buf)
		if n > 0 {
			var lengthBuf [4]byte
			binary.BigEndian.PutUint32(lengthBuf[:], uint32(n))
			if _, err := conn.Write(lengthBuf[:]); err != nil {
				return Result{}, fmt.Errorf("clamav chunk header write error: %w", err)
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return Result{}, fmt.Errorf("clamav chunk data write error: %w", err)
			}
		}
		if rErr == io.EOF {
			break
		}
		if rErr != nil {
			return Result{}, fmt.Errorf("read content error: %w", rErr)
		}
	}

	// Send 4-byte 0 length terminator
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return Result{}, fmt.Errorf("clamav terminator write error: %w", err)
	}

	// Read response (e.g., "stream: OK\x00" or "stream: Eicar-Signature FOUND\x00")
	var resp bytes.Buffer
	respBuf := make([]byte, 1024)
	var readErr error
	for {
		n, err := conn.Read(respBuf)
		if n > 0 {
			resp.Write(respBuf[:n])
		}
		if err != nil {
			readErr = err
			break
		}
	}

	if resp.Len() == 0 && readErr != nil && !errors.Is(readErr, io.EOF) {
		return Result{}, fmt.Errorf("clamav read error: %w", readErr)
	}

	respStr := strings.TrimSpace(strings.Trim(resp.String(), "\x00"))
	if strings.Contains(respStr, "FOUND") {
		parts := strings.Split(respStr, "FOUND")
		reason := strings.TrimSpace(parts[0])
		reason = strings.TrimPrefix(reason, "stream:")
		reason = strings.TrimSpace(reason)
		if reason == "" {
			reason = respStr
		}
		return Result{Clean: false, Reason: reason}, nil
	}

	if strings.Contains(respStr, "OK") {
		return Result{Clean: true}, nil
	}

	return Result{}, fmt.Errorf("unexpected clamav response: %q", respStr)
}
