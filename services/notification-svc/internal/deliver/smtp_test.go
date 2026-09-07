package deliver_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/deliver"
)

// fakeSMTP is an in-process SMTP server.
//
// The alternative was a mock Provider, and a mock would have proved nothing
// that matters here: every defect this transport can have â€” an unterminated
// header, a body line over the 998-octet limit, a subject that ends its own
// header and injects another, a 4xx classified as permanent â€” lives in the
// bytes on the wire and in the reply codes coming back. A double that accepts
// a Message struct never sees any of them.
type fakeSMTP struct {
	ln       net.Listener
	mu       sync.Mutex
	messages []string
	mailFrom []string
	rcptTo   []string

	// rcptReply overrides the RCPT TO response, so a test can produce a real
	// 4xx or 5xx from a real server rather than asserting on a fabricated
	// error value.
	rcptReply string
	dataReply string

	// noops counts NOOP commands, so a test can prove Verify completed a real
	// session rather than merely opening a socket.
	noops int

	wg sync.WaitGroup
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSMTP{ln: ln}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeSMTP) addr() (host string, port int) {
	a := s.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (s *fakeSMTP) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.handle(conn)
		_ = conn.Close()
	}
}

func (s *fakeSMTP) handle(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(line string) {
		_, _ = w.WriteString(line + "\r\n")
		_ = w.Flush()
	}

	write("220 fake.test ESMTP ready")

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))

		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			// STARTTLS is deliberately not advertised: these tests run in
			// TLSNone against loopback, and advertising it would exercise a
			// path the fake cannot complete.
			write("250-fake.test greets you")
			write("250 SIZE 10485760")

		case strings.HasPrefix(cmd, "HELO"):
			write("250 fake.test")

		case strings.HasPrefix(cmd, "MAIL FROM"):
			s.mu.Lock()
			s.mailFrom = append(s.mailFrom, strings.TrimSpace(line))
			s.mu.Unlock()
			write("250 2.1.0 Ok")

		case strings.HasPrefix(cmd, "RCPT TO"):
			s.mu.Lock()
			s.rcptTo = append(s.rcptTo, strings.TrimSpace(line))
			reply := s.rcptReply
			s.mu.Unlock()
			if reply != "" {
				write(reply)
				continue
			}
			write("250 2.1.5 Ok")

		case cmd == "DATA":
			s.mu.Lock()
			reply := s.dataReply
			s.mu.Unlock()
			write("354 End data with <CR><LF>.<CR><LF>")

			var body strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" || l == ".\n" {
					break
				}
				body.WriteString(l)
			}
			s.mu.Lock()
			s.messages = append(s.messages, body.String())
			s.mu.Unlock()
			if reply != "" {
				write(reply)
				continue
			}
			write("250 2.0.0 Ok: queued as FAKE123")

		case cmd == "QUIT":
			write("221 2.0.0 Bye")
			return

		case cmd == "RSET":
			write("250 2.0.0 Ok")

		case cmd == "NOOP":
			// What Verify uses to confirm a live session without sending
			// anything.
			s.mu.Lock()
			s.noops++
			s.mu.Unlock()
			write("250 2.0.0 Ok")

		default:
			write("500 5.5.2 Unrecognized command")
		}
	}
}

func (s *fakeSMTP) lastMessage(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		t.Fatal("no message reached the server")
	}
	return s.messages[len(s.messages)-1]
}

func newProvider(t *testing.T, s *fakeSMTP, from string) *deliver.SMTPProvider {
	t.Helper()
	host, port := s.addr()
	p, err := deliver.NewSMTPProvider(deliver.SMTPConfig{
		Host: host, Port: port, From: from,
		TLSMode: deliver.TLSNone,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSMTPProvider: %v", err)
	}
	return p
}

// â”€â”€ the delivery actually happening â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func TestSMTPProvider_SendsAWellFormedMessage(t *testing.T) {
	s := newFakeSMTP(t)
	p := newProvider(t, s, "Zoiko Platform <no-reply@zoiko.test>")

	receipt, err := p.Send(context.Background(), deliver.Message{
		To:            "employee@example.com",
		Subject:       "Your organization has been approved",
		HTMLBody:      "<html><body><p>Welcome.</p></body></html>",
		CorrelationID: "corr-42",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !strings.Contains(receipt, "message-id=") {
		t.Errorf("receipt carries no message id, so nothing ties this send to the relay's logs: %q", receipt)
	}

	msg := s.lastMessage(t)
	for _, want := range []string{
		"From: \"Zoiko Platform\" <no-reply@zoiko.test>",
		"To: <employee@example.com>",
		"Subject: Your organization has been approved",
		"MIME-Version: 1.0",
		"Content-Type: text/html; charset=UTF-8",
		"Content-Transfer-Encoding: quoted-printable",
		"X-Zoiko-Correlation-Id: corr-42",
		"Auto-Submitted: auto-generated",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q\n--- got ---\n%s", want, msg)
		}
	}

	if !strings.Contains(msg, "Welcome.") {
		t.Errorf("message body did not survive encoding:\n%s", msg)
	}

	// The envelope sender is the bare address, never the display name.
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.mailFrom) != 1 || !strings.Contains(s.mailFrom[0], "<no-reply@zoiko.test>") {
		t.Errorf("unexpected MAIL FROM: %v", s.mailFrom)
	}
}

// A header value carrying CRLF must not be able to end its header and start
// another. This is the injection that turns a template variable â€” an
// organization name, a first name â€” into a Bcc on somebody else's notice.
func TestSMTPProvider_SubjectCannotInjectHeaders(t *testing.T) {
	s := newFakeSMTP(t)
	p := newProvider(t, s, "no-reply@zoiko.test")

	_, err := p.Send(context.Background(), deliver.Message{
		To:       "employee@example.com",
		Subject:  "Payslip ready\r\nBcc: attacker@evil.test",
		HTMLBody: "<p>hi</p>",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg := s.lastMessage(t)
	headers, _, _ := strings.Cut(msg, "\r\n\r\n")

	// The property is that no LINE begins a new header â€” not that the text
	// "Bcc:" is absent. Flattening the CRLF to a space is a correct defence,
	// and it leaves that text sitting harmlessly inside the Subject value,
	// where it is data. Asserting on mere presence would fail a working
	// implementation.
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Fatalf("header injection succeeded â€” a Bcc became its own header:\n%s", headers)
		}
	}

	// And the subject must still be one header, on one line.
	subjectLines := 0
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(line, "Subject:") {
			subjectLines++
		}
	}
	if subjectLines != 1 {
		t.Fatalf("expected exactly one Subject header, found %d:\n%s", subjectLines, headers)
	}
}

// A body line longer than SMTP's 998-octet limit must not be emitted raw. A
// relay mangles such a line rather than rejecting it, so the failure would
// surface as a corrupted email nobody can reproduce, not as a delivery error.
func TestSMTPProvider_LongBodyLinesAreEncoded(t *testing.T) {
	s := newFakeSMTP(t)
	p := newProvider(t, s, "no-reply@zoiko.test")

	long := "<p>" + strings.Repeat("compensation-review-", 200) + "</p>"
	if _, err := p.Send(context.Background(), deliver.Message{
		To: "employee@example.com", Subject: "Long", HTMLBody: long,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	for _, line := range strings.Split(s.lastMessage(t), "\n") {
		if len(strings.TrimRight(line, "\r")) > 998 {
			t.Fatalf("a %d-octet line went out; SMTP permits 998", len(line))
		}
	}
}

// â”€â”€ failure classification â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
//
// The distinction these two tests draw is the whole reason DeliveryOutcome
// carries Retryable. Both are "the server said no"; only one of them is worth
// ever attempting again.

func TestSMTPProvider_TransientRejectionIsRetryable(t *testing.T) {
	s := newFakeSMTP(t)
	s.rcptReply = "451 4.7.1 Greylisted, try again later"
	p := newProvider(t, s, "no-reply@zoiko.test")

	_, err := p.Send(context.Background(), deliver.Message{
		To: "employee@example.com", Subject: "s", HTMLBody: "<p>b</p>",
	})
	if err == nil {
		t.Fatal("expected a rejection")
	}
	var re deliver.RetryableError
	if !errors.As(err, &re) {
		t.Fatalf("greylisting (SMTP 451) was classified permanent; the notice would never be re-sent: %v", err)
	}
}

func TestSMTPProvider_PermanentRejectionIsNotRetryable(t *testing.T) {
	s := newFakeSMTP(t)
	s.rcptReply = "550 5.1.1 No such user here"
	p := newProvider(t, s, "no-reply@zoiko.test")

	_, err := p.Send(context.Background(), deliver.Message{
		To: "nobody@example.com", Subject: "s", HTMLBody: "<p>b</p>",
	})
	if err == nil {
		t.Fatal("expected a rejection")
	}
	var re deliver.RetryableError
	if errors.As(err, &re) {
		t.Fatalf("an unknown mailbox (SMTP 550) was marked retryable; it would be re-attempted forever: %v", err)
	}
}

func TestSMTPProvider_UnreachableServerIsRetryable(t *testing.T) {
	// A port nothing is listening on: the canonical transient failure.
	p, err := deliver.NewSMTPProvider(deliver.SMTPConfig{
		Host: "127.0.0.1", Port: 1, From: "no-reply@zoiko.test",
		TLSMode: deliver.TLSNone, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSMTPProvider: %v", err)
	}
	_, err = p.Send(context.Background(), deliver.Message{
		To: "employee@example.com", Subject: "s", HTMLBody: "<p>b</p>",
	})
	var re deliver.RetryableError
	if !errors.As(err, &re) {
		t.Fatalf("a refused connection was classified permanent: %v", err)
	}
}

// â”€â”€ configuration refusals â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func TestNewSMTPProvider_RefusesCleartextToRemoteHost(t *testing.T) {
	_, err := deliver.NewSMTPProvider(deliver.SMTPConfig{
		Host: "smtp.example.com", Port: 25, From: "no-reply@zoiko.test",
		TLSMode: deliver.TLSNone,
	})
	if err == nil {
		t.Fatal("cleartext SMTP to a remote host was accepted; " +
			"password-reset mail carrying a temporary password would cross the network in the clear")
	}
}

func TestNewSMTPProvider_RefusesMalformedFrom(t *testing.T) {
	_, err := deliver.NewSMTPProvider(deliver.SMTPConfig{
		Host: "127.0.0.1", Port: 1025, From: "not an address", TLSMode: deliver.TLSNone,
	})
	if err == nil {
		t.Fatal("a malformed From was accepted at construction; it would have become " +
			"one FAILED notification per send, each describing the same misconfiguration")
	}
}

func TestSMTPProvider_RefusesMalformedRecipientWithoutDialing(t *testing.T) {
	s := newFakeSMTP(t)
	p := newProvider(t, s, "no-reply@zoiko.test")

	_, err := p.Send(context.Background(), deliver.Message{
		To: "not an address", Subject: "s", HTMLBody: "<p>b</p>",
	})
	if err == nil {
		t.Fatal("expected a malformed recipient to be refused")
	}
	var re deliver.RetryableError
	if errors.As(err, &re) {
		t.Error("a malformed address was marked retryable; it will never become valid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.mailFrom) != 0 {
		t.Error("a connection was opened for an address that could not be parsed")
	}
}

// ── Verify: proving a credential before anything depends on it ──────────────

// Verify must complete a real SMTP session — not merely open a socket — and
// must send no mail while doing it. A "verification" that only dialled would
// pass against a relay that rejects every credential.
func TestSMTPProvider_Verify_CompletesASessionWithoutSending(t *testing.T) {
	s := newFakeSMTP(t)
	p := newProvider(t, s, "Zoiko <no-reply@zoiko.test>")

	if err := p.Verify(context.Background()); err != nil {
		t.Fatalf("Verify against a healthy server: %v", err)
	}

	s.mu.Lock()
	noops, messages, mailFrom := s.noops, len(s.messages), len(s.mailFrom)
	s.mu.Unlock()

	if noops != 1 {
		t.Errorf("NOOP count = %d, want 1 — Verify must prove the session is live", noops)
	}
	if messages != 0 || mailFrom != 0 {
		t.Errorf("Verify sent mail (messages=%d, MAIL FROM=%d); it must never deliver or "+
			"consume provider quota", messages, mailFrom)
	}
}

// The whole point: an unreachable or misconfigured relay is reported at
// startup, not on somebody's password reset.
func TestSMTPProvider_Verify_ReportsAnUnreachableRelay(t *testing.T) {
	p, err := deliver.NewSMTPProvider(deliver.SMTPConfig{
		// Port 1 on loopback: nothing listens, and loopback keeps TLSNone legal.
		Host: "127.0.0.1", Port: 1, From: "no-reply@zoiko.test",
		TLSMode: deliver.TLSNone,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSMTPProvider: %v", err)
	}
	if err := p.Verify(context.Background()); err == nil {
		t.Fatal("Verify reported success against a port nothing is listening on")
	}
}

// A username against a relay offering no AUTH is a configuration mistake with
// two opposite fixes, so the message has to say which one it is rather than
// surfacing as a generic auth failure.
func TestSMTPProvider_Verify_ExplainsAMissingAuthExtension(t *testing.T) {
	s := newFakeSMTP(t)
	host, port := s.addr()
	// TLSNone on loopback, so the cleartext-credential guard in the
	// constructor permits a username here.
	p, err := deliver.NewSMTPProvider(deliver.SMTPConfig{
		Host: host, Port: port, From: "no-reply@zoiko.test",
		Username: "apikey", Password: "secret",
		TLSMode: deliver.TLSNone,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSMTPProvider: %v", err)
	}

	err = p.Verify(context.Background())
	if err == nil {
		t.Fatal("expected Verify to fail: the fake server advertises no AUTH")
	}
	if !strings.Contains(err.Error(), "does not offer AUTH") {
		t.Errorf("error = %q, want it to name the missing AUTH extension so the "+
			"operator knows to clear SMTP_USERNAME or fix the port", err)
	}
}

// Describe feeds a startup log line and must never carry the password — that
// line is written precisely when a credential is new and suspected wrong.
func TestSMTPProvider_Describe_OmitsThePassword(t *testing.T) {
	s := newFakeSMTP(t)
	host, port := s.addr()
	p, err := deliver.NewSMTPProvider(deliver.SMTPConfig{
		Host: host, Port: port, From: "no-reply@zoiko.test",
		Username: "apikey", Password: "sup3r-s3cret-value",
		TLSMode: deliver.TLSNone,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSMTPProvider: %v", err)
	}

	d := p.Describe()
	if strings.Contains(d, "sup3r-s3cret-value") {
		t.Fatalf("Describe leaked the password: %q", d)
	}
	if !strings.Contains(d, "apikey") {
		t.Errorf("Describe = %q, want the username so the operator can see which credential is in use", d)
	}
}