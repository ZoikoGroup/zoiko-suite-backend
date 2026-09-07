package deliver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TLSMode selects how the SMTP connection is protected.
type TLSMode string

const (
	// TLSStartTLS upgrades a plaintext connection with STARTTLS. The default,
	// and what submission port 587 expects at every managed provider.
	TLSStartTLS TLSMode = "starttls"

	// TLSImplicit negotiates TLS before the SMTP greeting — "SMTPS", port 465.
	TLSImplicit TLSMode = "implicit"

	// TLSNone sends credentials and message bodies in the clear. Rejected for
	// any non-loopback host unless AllowCleartext is set explicitly: it exists
	// for a local mail catcher and for nothing else. config.Load refuses it in
	// production outright, whatever AllowCleartext says.
	TLSNone TLSMode = "none"
)

// SMTPProvider delivers mail over SMTP.
//
// net/smtp rather than a client library, deliberately: it is in the standard
// library, it adds no dependency to a service that already vendors 30, and
// every managed provider (SES, SendGrid, Postmark, Resend) exposes an SMTP
// relay, so moving between them is an environment change rather than a code
// change. What it costs is the provider's queue id — net/smtp consumes the
// final 250 response internally and does not expose its text — so acceptance
// evidence is keyed on the Message-ID this provider generates instead. That
// id travels in the delivered mail's headers and appears in the relay's own
// logs, which makes it the more useful join key of the two anyway.
type SMTPProvider struct {
	host     string
	port     int
	username string
	password string
	tlsMode  TLSMode

	// from is the envelope sender and the From: header, pre-parsed so a
	// malformed NOTIFICATION_EMAIL_FROM fails at startup rather than on the
	// first notification of the day.
	from mail.Address

	timeout time.Duration
}

// SMTPConfig is the settings SMTPProvider needs.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string // RFC 5322: "Zoiko Platform <no-reply@zoiko.io>" or a bare address
	TLSMode  TLSMode
	Timeout  time.Duration

	// AllowCleartext permits TLSMode "none" against a host that is not
	// loopback. It exists for exactly one situation: a mail catcher such as
	// Mailpit running as a sibling container, reachable as "mailpit:1025"
	// rather than over loopback, on a developer's compose estate.
	//
	// It is a separate flag rather than a relaxation of the loopback rule
	// because the two are different claims. "The host is loopback" is
	// checkable and cannot be got wrong; "this network is safe enough for
	// cleartext" is a judgement, and a judgement should have to be written
	// down somewhere a reviewer can see it. config.Load refuses cleartext in
	// production regardless of this flag.
	AllowCleartext bool
}

// NewSMTPProvider validates configuration and returns a provider.
//
// Validation happens here, at construction, because every failure mode below
// is permanent and identical for every message. A provider that accepts a
// malformed From address turns one configuration mistake into a FAILED record
// per notification, each one describing the same problem in the vocabulary of
// a delivery failure.
func NewSMTPProvider(cfg SMTPConfig) (*SMTPProvider, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("smtp: host is required")
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("smtp: port %d is out of range", cfg.Port)
	}
	if strings.TrimSpace(cfg.From) == "" {
		return nil, errors.New("smtp: from address is required")
	}
	addr, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("smtp: from address %q is not a valid RFC 5322 address: %w", cfg.From, err)
	}

	mode := cfg.TLSMode
	if mode == "" {
		mode = TLSStartTLS
	}
	switch mode {
	case TLSStartTLS, TLSImplicit:
	case TLSNone:
		// Loopback, or an explicit opt-out. Without this check, a single
		// environment variable turns every notification — including password
		// resets carrying a temporary password — into cleartext on the wire,
		// and nothing in the service's behaviour would look any different.
		if !isLoopbackHost(cfg.Host) && !cfg.AllowCleartext {
			return nil, fmt.Errorf(
				"smtp: tls mode %q is only permitted for a loopback host, not %q — "+
					"credentials and message bodies would cross the network in the clear. "+
					"Set SMTP_ALLOW_CLEARTEXT=true if this is a local mail catcher on a "+
					"container network; it is refused in production either way",
				TLSNone, cfg.Host)
		}
	default:
		return nil, fmt.Errorf("smtp: unknown tls mode %q (want starttls, implicit or none)", mode)
	}

	// Authenticating over an unprotected connection sends the password in
	// base64, which is not encryption. net/smtp's PlainAuth enforces this too;
	// catching it here names the offending variable instead of failing inside
	// the first send.
	//
	// AllowCleartext does not waive this. That flag says "this network is
	// acceptable for a test message"; it does not say "put a real SMTP
	// credential on it", and a mail catcher does not need one anyway.
	if cfg.Username != "" && mode == TLSNone && !isLoopbackHost(cfg.Host) {
		return nil, errors.New("smtp: refusing to send credentials over an unprotected connection")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	return &SMTPProvider{
		host:     cfg.Host,
		port:     cfg.Port,
		username: cfg.Username,
		password: cfg.Password,
		tlsMode:  mode,
		from:     *addr,
		timeout:  timeout,
	}, nil
}

func (p *SMTPProvider) Name() string { return "smtp" }

// Send transmits one message and returns acceptance evidence.
//
// Every error is classified transient or permanent before it leaves this
// function, because the distinction is only visible here: an SMTP 4xx
// (greylisted, mailbox busy, rate limited) and an SMTP 5xx (no such user,
// message refused) are the same *shape* of failure and opposite *facts*.
// Collapsing them is how a greylisted payslip notice becomes a permanent
// FAILED that nothing ever revisits.
func (p *SMTPProvider) Send(ctx context.Context, msg Message) (string, error) {
	to, err := mail.ParseAddress(msg.To)
	if err != nil {
		// Permanent by construction: the address will not become valid.
		return "", fmt.Errorf("recipient address %q is not valid: %w", msg.To, err)
	}

	messageID := fmt.Sprintf("<%s@%s>", uuid.NewString(), p.messageIDDomain())
	raw, err := p.buildMessage(msg, *to, messageID)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	client, err := p.dial(ctx)
	if err != nil {
		// A refused or timed-out connection is the canonical transient
		// failure — the relay may be restarting, and the message is still
		// perfectly deliverable.
		return "", Retryable(fmt.Errorf("connecting to %s: %w", p.addr(), err))
	}
	defer func() {
		// Quit sends QUIT and closes. If it fails the message may already have
		// been accepted, so the error is deliberately not returned: reporting
		// a failure after a successful DATA would produce a duplicate on any
		// re-attempt, which §0.4 names explicitly.
		_ = client.Quit()
	}()

	if err := p.authenticate(client); err != nil {
		return "", err
	}

	if err := client.Mail(p.from.Address); err != nil {
		return "", classifySMTPError(fmt.Errorf("MAIL FROM %s: %w", p.from.Address, err))
	}
	if err := client.Rcpt(to.Address); err != nil {
		return "", classifySMTPError(fmt.Errorf("RCPT TO: %w", err))
	}

	w, err := client.Data()
	if err != nil {
		return "", classifySMTPError(fmt.Errorf("DATA: %w", err))
	}
	if _, err := w.Write(raw); err != nil {
		return "", Retryable(fmt.Errorf("writing message body: %w", err))
	}
	// Close is where the server's accept-or-reject verdict arrives. An error
	// here means the message was NOT accepted, which is why it is checked
	// rather than deferred.
	if err := w.Close(); err != nil {
		return "", classifySMTPError(fmt.Errorf("completing DATA: %w", err))
	}

	return fmt.Sprintf("smtp %s accepted; message-id=%s", p.addr(), messageID), nil
}

// authenticate performs AUTH when a username is configured.
//
// The AUTH extension is checked first so the two failures stay distinguishable.
// Without it, a relay that offers no AUTH at all and a relay that rejected the
// password both surface as "authenticating to host: ..." — and the fixes are
// opposite: one means the credentials are wrong, the other means they should
// not have been set.
func (p *SMTPProvider) authenticate(client *smtp.Client) error {
	if p.username == "" {
		return nil
	}
	if ok, _ := client.Extension("AUTH"); !ok {
		return fmt.Errorf(
			"server %s does not offer AUTH, but SMTP_USERNAME is set — "+
				"either the relay expects an unauthenticated connection (clear SMTP_USERNAME "+
				"and SMTP_PASSWORD) or the port is wrong for authenticated submission "+
				"(587 for STARTTLS, 465 for implicit TLS)", p.addr())
	}
	// PlainAuth refuses a connection it does not consider secure, so on
	// starttls/implicit this is PLAIN over TLS and never a cleartext password.
	if err := client.Auth(smtp.PlainAuth("", p.username, p.password, p.host)); err != nil {
		// Bad credentials are permanent; retrying re-sends them and, at some
		// providers, contributes to a lockout.
		return classifySMTPError(fmt.Errorf("authenticating to %s as %q: %w", p.addr(), p.username, err))
	}
	return nil
}

// Verify opens a session, authenticates and hangs up without sending anything.
//
// This exists because of how a mail credential is actually introduced: it is
// pasted into an environment at deploy time, and until this ran, the first
// thing that discovered a wrong password was a real password-reset email
// failing for a real person. Every configuration fault below — unreachable
// relay, no STARTTLS, wrong port, rejected credentials — is permanent,
// identical for every message, and knowable at startup.
//
// It sends no MAIL FROM, so it cannot deliver anything and cannot count
// against a provider's quota. Callers should log the result rather than refuse
// to start: notification-svc's own doctrine is that a delivery problem must not
// collapse the workflows that depend on it, and IN_APP delivery does not touch
// SMTP at all.
func (p *SMTPProvider) Verify(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	client, err := p.dial(ctx)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", p.addr(), err)
	}
	defer func() { _ = client.Quit() }()

	if err := p.authenticate(client); err != nil {
		return err
	}
	if err := client.Noop(); err != nil {
		return fmt.Errorf("session check against %s: %w", p.addr(), err)
	}
	return nil
}

// Describe summarises the configuration for a startup log line, without the
// password. Useful precisely when the credential is new and wrong.
func (p *SMTPProvider) Describe() string {
	auth := "unauthenticated"
	if p.username != "" {
		auth = "as " + p.username
	}
	return fmt.Sprintf("%s (tls=%s, %s, from=%s)", p.addr(), p.tlsMode, auth, p.from.Address)
}

func (p *SMTPProvider) addr() string { return net.JoinHostPort(p.host, fmt.Sprint(p.port)) }

// messageIDDomain takes the domain of the From address, so the Message-ID is
// rooted in a domain this platform actually sends as. Falling back to the SMTP
// host would generate ids under the relay's domain, which is not ours.
func (p *SMTPProvider) messageIDDomain() string {
	if at := strings.LastIndex(p.from.Address, "@"); at >= 0 && at+1 < len(p.from.Address) {
		return p.from.Address[at+1:]
	}
	return p.host
}

// dial establishes an SMTP session honouring the configured TLS mode and the
// context's deadline.
func (p *SMTPProvider) dial(ctx context.Context) (*smtp.Client, error) {
	addr := p.addr()
	tlsConf := &tls.Config{ServerName: p.host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	var err error
	if p.tlsMode == TLSImplicit {
		conn, err = (&tls.Dialer{Config: tlsConf}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}

	// net/smtp predates context and will otherwise block on a silent server
	// for as long as the kernel allows. The deadline covers the whole session,
	// which is what the caller's timeout meant.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, p.host)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	if p.tlsMode == TLSStartTLS {
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			_ = client.Close()
			// Not retryable, and not silently downgraded: a relay that does
			// not offer STARTTLS when we require it is a misconfiguration, and
			// continuing in the clear is the failure this check exists for.
			return nil, fmt.Errorf("server %s does not offer STARTTLS and tls mode is %q", addr, TLSStartTLS)
		}
		if err := client.StartTLS(tlsConf); err != nil {
			_ = client.Close()
			return nil, err
		}
	}
	return client, nil
}

// buildMessage renders RFC 5322 bytes for one HTML message.
func (p *SMTPProvider) buildMessage(msg Message, to mail.Address, messageID string) ([]byte, error) {
	var b strings.Builder

	// Subject reaches a header, and the templates render it from
	// caller-supplied variables — an organization name, a first name. A value
	// containing CRLF would end the Subject header and let the rest be read as
	// further headers, which is how a Bcc: gets appended to somebody else's
	// notification. mime.QEncoding.Encode both escapes non-ASCII and folds,
	// but it does not defend against an injected newline, so the value is
	// stripped first.
	subject := mime.QEncoding.Encode("utf-8", sanitizeHeaderValue(msg.Subject))

	fmt.Fprintf(&b, "From: %s\r\n", p.from.String())
	fmt.Fprintf(&b, "To: %s\r\n", to.String())
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: %s\r\n", messageID)
	if msg.CorrelationID != "" {
		// The same correlation id the notification row carries, so a mail in
		// the relay's logs can be tied to the send that produced it.
		fmt.Fprintf(&b, "X-Zoiko-Correlation-Id: %s\r\n", sanitizeHeaderValue(msg.CorrelationID))
	}
	// Transactional mail. Without this, a bulk-mail auto-responder can reply
	// to a password reset and loop.
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	b.WriteString("\r\n")

	// quoted-printable, not raw: SMTP limits a line to 998 octets, and the
	// templates are formatted HTML whose lines are usually short but are not
	// guaranteed to be — an organization name is interpolated into them. A
	// message that trips the limit is mangled by the relay rather than
	// rejected, so this would surface as a corrupted email nobody could
	// reproduce, not as a delivery failure.
	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(msg.HTMLBody)); err != nil {
		return nil, fmt.Errorf("encoding message body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return nil, fmt.Errorf("encoding message body: %w", err)
	}

	return []byte(b.String()), nil
}

// sanitizeHeaderValue removes CR and LF so a value cannot terminate its header
// and inject another.
func sanitizeHeaderValue(v string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
}

// classifySMTPError marks 4xx replies and network faults transient, and
// everything else settled.
func classifySMTPError(err error) error {
	var proto *textproto.Error
	if errors.As(err, &proto) {
		if proto.Code >= 400 && proto.Code < 500 {
			return Retryable(err)
		}
		return err // 5xx: the server has refused this message for good.
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return Retryable(err)
	}
	// An unrecognised transport failure is treated as transient. The cost of
	// being wrong differs: a retried permanent failure wastes an attempt, an
	// un-retried transient one silently loses a notice.
	return Retryable(err)
}

// isLoopbackHost reports whether a host refers to this machine.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
