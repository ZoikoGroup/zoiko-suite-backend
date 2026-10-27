package scan_test

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/document-vault-svc/internal/scan"
)

// mockClamAVServer simulates a clamd instance responding to INSTREAM commands.
func mockClamAVServer(t *testing.T, response string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	done := make(chan struct{})

	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Read command: "zINSTREAM\x00" is exactly 10 bytes (9 ASCII characters + 1 null byte)
		cmdBuf := make([]byte, 10)
		if _, err := io.ReadFull(conn, cmdBuf); err != nil {
			return
		}

		// Read streamed chunks until 0-length chunk
		for {
			var length uint32
			if err := binary.Read(conn, binary.BigEndian, &length); err != nil {
				break
			}
			if length == 0 {
				break
			}
			data := make([]byte, length)
			if _, err := io.ReadFull(conn, data); err != nil {
				break
			}
		}

		// Write response
		_, _ = conn.Write([]byte(response))
	}()

	cleanup := func() {
		_ = ln.Close()
		<-done
	}

	return ln.Addr().String(), cleanup
}

func TestClamAVScanner_Clean(t *testing.T) {
	addr, cleanup := mockClamAVServer(t, "stream: OK\x00")
	defer cleanup()

	scanner := scan.NewClamAVScanner(addr, 2*time.Second)
	res, err := scanner.Scan(context.Background(), []byte("harmless document content"), "application/pdf")
	require.NoError(t, err)
	assert.True(t, res.Clean)
	assert.Empty(t, res.Reason)
}

func TestClamAVScanner_VirusFound(t *testing.T) {
	addr, cleanup := mockClamAVServer(t, "stream: Win.Test.EICAR_HDB-1 FOUND\x00")
	defer cleanup()

	scanner := scan.NewClamAVScanner(addr, 2*time.Second)
	res, err := scanner.Scan(context.Background(), []byte("X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*"), "text/plain")
	require.NoError(t, err)
	assert.False(t, res.Clean)
	assert.Contains(t, res.Reason, "Win.Test.EICAR_HDB-1")
}

func TestClamAVScanner_ProtocolFraming(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	payload := []byte("verification payload for framing check")
	receivedPayload := make([]byte, 0)
	commandReceived := ""

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		cmdBuf := make([]byte, 10)
		_, _ = io.ReadFull(conn, cmdBuf)
		commandReceived = string(cmdBuf)

		for {
			var length uint32
			if err := binary.Read(conn, binary.BigEndian, &length); err != nil {
				break
			}
			if length == 0 {
				break
			}
			chunk := make([]byte, length)
			_, _ = io.ReadFull(conn, chunk)
			receivedPayload = append(receivedPayload, chunk...)
		}
		_, _ = conn.Write([]byte("stream: OK\x00"))
	}()

	scanner := scan.NewClamAVScanner(ln.Addr().String(), 2*time.Second)
	res, err := scanner.Scan(context.Background(), payload, "text/plain")
	<-done

	require.NoError(t, err)
	assert.True(t, res.Clean)
	assert.Equal(t, "zINSTREAM\x00", commandReceived, "scanner must send exact zINSTREAM\\0 null-terminated command")
	assert.Equal(t, payload, receivedPayload, "server must receive exact streamed chunks")
}

func TestClamAVScanner_LargeFileMultiChunk(t *testing.T) {
	addr, cleanup := mockClamAVServer(t, "stream: OK\x00")
	defer cleanup()

	// 70 KB payload forces multi-chunk streaming across 32 KB boundaries
	largePayload := make([]byte, 70*1024)
	for i := range largePayload {
		largePayload[i] = byte(i % 256)
	}

	scanner := scan.NewClamAVScanner(addr, 2*time.Second)
	res, err := scanner.Scan(context.Background(), largePayload, "application/octet-stream")
	require.NoError(t, err)
	assert.True(t, res.Clean)
}

func TestClamAVScanner_Timeout_FailsClosed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read command and wait longer than scanner timeout without answering
		cmdBuf := make([]byte, 10)
		_, _ = io.ReadFull(conn, cmdBuf)
		time.Sleep(300 * time.Millisecond)
	}()

	scanner := scan.NewClamAVScanner(ln.Addr().String(), 50*time.Millisecond)
	_, err = scanner.Scan(context.Background(), []byte("payload that times out"), "text/plain")
	<-done

	require.Error(t, err, "scanner must fail closed on timeout")
}

func TestClamAVScanner_Unreachable_FailsClosed(t *testing.T) {
	// Pick an address that is guaranteed not listening
	scanner := scan.NewClamAVScanner("127.0.0.1:59999", 500*time.Millisecond)
	_, err := scanner.Scan(context.Background(), []byte("some payload"), "text/plain")
	require.Error(t, err, "unreachable scanner must return error so handler fails closed")
}

func TestClamAVScanner_MalformedResponse_FailsClosed(t *testing.T) {
	addr, cleanup := mockClamAVServer(t, "stream: UNEXPECTED_ERROR\x00")
	defer cleanup()

	scanner := scan.NewClamAVScanner(addr, 2*time.Second)
	_, err := scanner.Scan(context.Background(), []byte("test"), "text/plain")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected clamav response")
}
