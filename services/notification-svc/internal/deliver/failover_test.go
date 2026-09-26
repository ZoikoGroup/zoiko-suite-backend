package deliver_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/deliver"
	"zoiko.io/notification-svc/internal/domain"
)

type namedRecordingProvider struct {
	name    string
	sent    []deliver.Message
	receipt string
	err     error
}

func (p *namedRecordingProvider) Name() string { return p.name }

func (p *namedRecordingProvider) Send(_ context.Context, m deliver.Message) (string, error) {
	p.sent = append(p.sent, m)
	if p.err != nil {
		return "", p.err
	}
	return p.receipt, nil
}

func TestFailoverRouter_PrimarySuccess(t *testing.T) {
	primary := &namedRecordingProvider{name: "primary-smtp", receipt: "primary-receipt-123"}
	secondary := &namedRecordingProvider{name: "secondary-smtp", receipt: "secondary-receipt-456"}

	router := deliver.NewFailoverRouter(primary, secondary, zap.NewNop())

	outcome := router.Deliver(context.Background(), domain.Notification{
		Channel:          domain.ChannelEmail,
		RecipientAddress: "user@example.com",
		Subject:          "Security Alert",
		Body:             "<p>Login detected</p>",
		From:             "ZoikoSuite Security <security@security.zoikosuite.com>",
	})

	if !outcome.Delivered {
		t.Fatalf("expected delivery success, got reason: %s", outcome.Reason)
	}
	if outcome.ProviderName != "primary-smtp" {
		t.Errorf("expected provider primary-smtp, got %s", outcome.ProviderName)
	}
	if outcome.ProviderResponse != "primary-receipt-123" {
		t.Errorf("expected provider response primary-receipt-123, got %s", outcome.ProviderResponse)
	}
	if len(primary.sent) != 1 {
		t.Fatalf("expected 1 message sent to primary, got %d", len(primary.sent))
	}
	if len(secondary.sent) != 0 {
		t.Fatalf("secondary should not be called when primary succeeds, got %d sends", len(secondary.sent))
	}
}

func TestFailoverRouter_PrimaryTransient_FailsOverToSecondary(t *testing.T) {
	primary := &namedRecordingProvider{
		name: "primary-smtp",
		err:  deliver.Retryable(errors.New("connection timeout to primary relay")),
	}
	secondary := &namedRecordingProvider{
		name:    "secondary-smtp",
		receipt: "secondary-receipt-789",
	}

	router := deliver.NewFailoverRouter(primary, secondary, zap.NewNop())

	headers := map[string]string{
		"List-Unsubscribe": "<https://notify.zoiko.com/unsub>",
	}

	outcome := router.Deliver(context.Background(), domain.Notification{
		Channel:          domain.ChannelEmail,
		RecipientAddress: "user@example.com",
		Subject:          "Weekly Digest",
		Body:             "<p>News</p>",
		From:             "ZoikoSuite <hello@news.zoikosuite.com>",
		Headers:          headers,
	})

	if !outcome.Delivered {
		t.Fatalf("expected failover to succeed, got reason: %s", outcome.Reason)
	}
	if outcome.ProviderName != "secondary-smtp" {
		t.Errorf("expected provider secondary-smtp, got %s", outcome.ProviderName)
	}
	if outcome.ProviderResponse != "secondary-receipt-789" {
		t.Errorf("expected secondary receipt, got %s", outcome.ProviderResponse)
	}
	if len(primary.sent) != 1 {
		t.Errorf("expected 1 message attempted at primary, got %d", len(primary.sent))
	}
	if len(secondary.sent) != 1 {
		t.Fatalf("expected 1 message sent to secondary on failover, got %d", len(secondary.sent))
	}

	// Verify headers and From were forwarded to secondary
	secMsg := secondary.sent[0]
	if secMsg.From != "ZoikoSuite <hello@news.zoikosuite.com>" {
		t.Errorf("expected secondary to receive From header, got %s", secMsg.From)
	}
	if secMsg.Headers["List-Unsubscribe"] != "<https://notify.zoiko.com/unsub>" {
		t.Errorf("expected secondary to receive List-Unsubscribe header, got %s", secMsg.Headers["List-Unsubscribe"])
	}
}

func TestFailoverRouter_PrimaryPermanent_DoesNotFailOver(t *testing.T) {
	primary := &namedRecordingProvider{
		name: "primary-smtp",
		err:  errors.New("550 5.1.1 User unknown"),
	}
	secondary := &namedRecordingProvider{
		name:    "secondary-smtp",
		receipt: "secondary-receipt-unused",
	}

	router := deliver.NewFailoverRouter(primary, secondary, zap.NewNop())

	outcome := router.Deliver(context.Background(), domain.Notification{
		Channel:          domain.ChannelEmail,
		RecipientAddress: "invalid@example.com",
		Subject:          "Invoice",
		Body:             "<p>Invoice</p>",
	})

	if outcome.Delivered {
		t.Fatalf("expected delivery to fail")
	}
	if outcome.Retryable {
		t.Errorf("permanent error must not be marked retryable")
	}
	if outcome.ProviderName != "primary-smtp" {
		t.Errorf("expected primary-smtp provider name, got %s", outcome.ProviderName)
	}
	if len(secondary.sent) != 0 {
		t.Errorf("secondary should never be called on permanent failure, got %d sends", len(secondary.sent))
	}
}

func TestFailoverRouter_BothProvidersTransientFail(t *testing.T) {
	primary := &namedRecordingProvider{
		name: "primary-smtp",
		err:  deliver.Retryable(errors.New("primary timeout")),
	}
	secondary := &namedRecordingProvider{
		name: "secondary-smtp",
		err:  deliver.Retryable(errors.New("secondary rate limited (421)")),
	}

	router := deliver.NewFailoverRouter(primary, secondary, zap.NewNop())

	outcome := router.Deliver(context.Background(), domain.Notification{
		Channel:          domain.ChannelEmail,
		RecipientAddress: "user@example.com",
		Subject:          "Alert",
		Body:             "<p>Alert</p>",
	})

	if outcome.Delivered {
		t.Fatalf("expected delivery to fail when both fail")
	}
	if !outcome.Retryable {
		t.Errorf("expected outcome to be retryable when both fail transiently")
	}
	if outcome.ProviderName != "secondary-smtp" {
		t.Errorf("expected secondary provider name on final failure, got %s", outcome.ProviderName)
	}
	if !strings.Contains(outcome.Reason, "secondary rate limited") {
		t.Errorf("expected outcome reason to reference secondary error, got %s", outcome.Reason)
	}
}

func TestSMTPProvider_CustomFromAndHeadersWireVerification(t *testing.T) {
	fake := newFakeSMTP(t)
	host, port := fake.addr()

	p, err := deliver.NewSMTPProvider(deliver.SMTPConfig{
		Host:           host,
		Port:           port,
		From:           "Fallback System <fallback@zoiko.test>",
		TLSMode:        deliver.TLSNone,
		AllowCleartext: true,
	})
	if err != nil {
		t.Fatalf("NewSMTPProvider: %v", err)
	}

	customFrom := "ZoikoSuite Security <security@security.zoikosuite.com>"
	receipt, err := p.Send(context.Background(), deliver.Message{
		From: customFrom,
		Headers: map[string]string{
			"List-Unsubscribe":      "<https://notify.zoiko.com/unsub>",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		},
		To:            "recipient@example.com",
		Subject:       "Security Incident",
		HTMLBody:      "<p>Passcode updated</p>",
		CorrelationID: "corr-sec-99",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(receipt, "accepted") {
		t.Errorf("unexpected receipt: %s", receipt)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.mailFrom) != 1 {
		t.Fatalf("expected 1 MAIL FROM, got %d", len(fake.mailFrom))
	}
	// Verify MAIL FROM used the parsed custom From address
	if !strings.Contains(fake.mailFrom[0], "security@security.zoikosuite.com") {
		t.Errorf("expected MAIL FROM to contain security@security.zoikosuite.com, got %s", fake.mailFrom[0])
	}

	if len(fake.messages) != 1 {
		t.Fatalf("expected 1 message body received, got %d", len(fake.messages))
	}
	rawMsg := fake.messages[0]

	if !strings.Contains(rawMsg, "From: ZoikoSuite Security <security@security.zoikosuite.com>") {
		t.Errorf("wire message missing custom From header: %s", rawMsg)
	}
	if !strings.Contains(rawMsg, "List-Unsubscribe: <https://notify.zoiko.com/unsub>") {
		t.Errorf("wire message missing List-Unsubscribe header: %s", rawMsg)
	}
	if !strings.Contains(rawMsg, "List-Unsubscribe-Post: List-Unsubscribe=One-Click") {
		t.Errorf("wire message missing List-Unsubscribe-Post header: %s", rawMsg)
	}
	if !strings.Contains(rawMsg, "X-Zoiko-Correlation-Id: corr-sec-99") {
		t.Errorf("wire message missing correlation header: %s", rawMsg)
	}
}
