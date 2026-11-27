package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/certreliance"
	"zoiko.io/general-ledger-svc/internal/certwatch"
)

// The event-driven path: a reliance-withdrawn event makes the ledger check the certification and flag, in seconds and without a
// sweep. The event is only a trigger: what the issuing service says decides.

func (e *kenv) reactor(principal string) *certwatch.Reactor {
	return &certwatch.Reactor{
		Store: e.watch, Sweeper: &certwatch.Sweeper{Store: e.watch, Status: e.status}, Authz: fakeAuthz{e}, Principal: principal,
	}
}

func dqWithdrawn(tenant, run, rev, relyingService string) []byte {
	return []byte(fmt.Sprintf(`{"event_id":"evt-%s","event_type":"dqc.certification.reliance-withdrawn","tenant_id":%q,"source_service":"data-quality-svc",
		"payload":{"reliance_id":"dqr_1","run_id":%q,"revocation_id":%q,"relying_service":%q,"relying_object_type":"journal","relying_object_id":"j"}}`,
		uuid.NewString(), tenant, run, rev, relyingService))
}

func recWithdrawn(tenant, run, rev string) []byte {
	return []byte(fmt.Sprintf(`{"id":%q,"type":"com.zoikosuite.accounting.reconciliation-certification.reliance-withdrawn","tenantid":%q,
		"data":{"reliance_id":"rrl_1","run_id":%q,"revocation_id":%q,"relying_service":"general-ledger-svc","relying_object_type":"journal","relying_object_id":"j"}}`,
		uuid.NewString(), tenant, run, rev))
}

func TestCertWithdrawalEvents_AnEventMakesTheLedgerCheckAndFlagWithoutASweep(t *testing.T) {
	e := newKEnv(t)
	jX := e.postManual(t, "ev-1", certX)
	e.postManual(t, "ev-2", certY)
	jZ := e.postManual(t, "ev-3", certZ)
	e.status.set(certX, revokedX)
	e.status.set(certZ, revokedZ)
	c := &certwatch.Consumer{Reactor: e.reactor("svc-sweeper"), RetryDelay: time.Millisecond}

	// data-quality-svc's legacy envelope: certX is revoked, so its journal is flagged. No sweep ran.
	if err := c.Handle(e.ctx(), dqWithdrawn(e.tenant, "dqn_x", "dqx_1", "general-ledger-svc")); err != nil {
		t.Fatal(err)
	}
	flags := e.flags(t, e.entity, "OPEN")
	if len(flags) != 1 || flags[0]["journal_id"] != jX || flags[0]["revocation_id"] != "dqx_1" {
		t.Fatalf("after the data-quality event: %v", flags)
	}
	// reconciliation-engine-svc's canonical envelope: certZ.
	if err := c.Handle(e.ctx(), recWithdrawn(e.tenant, "rcr_z", "rcv_2")); err != nil {
		t.Fatal(err)
	}
	flags = e.flags(t, e.entity, "OPEN")
	if len(flags) != 2 {
		t.Fatalf("after the reconciliation event: %v", flags)
	}
	got := map[string]bool{}
	for _, f := range flags {
		got[f["journal_id"].(string)] = true
	}
	if !got[jX] || !got[jZ] {
		t.Fatalf("flagged %v, want %s and %s", got, jX, jZ)
	}
	// The same event again (at-least-once delivery) changes nothing; certY, never mentioned, is untouched.
	for i := 0; i < 3; i++ {
		if err := c.Handle(e.ctx(), dqWithdrawn(e.tenant, "dqn_x", "dqx_1", "general-ledger-svc")); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.flags(t, e.entity, "")); n != 2 {
		t.Fatalf("a redelivered event changed the flags: %d", n)
	}
}

func TestCertWithdrawalEvents_AForgedEventCannotRaiseAFlagAndIrrelevantOnesAreIgnored(t *testing.T) {
	e := newKEnv(t)
	e.postManual(t, "fe-1", certX)
	// The event claims certX was withdrawn; the issuing service says it is in force (the fake answers ACTIVE by default).
	c := &certwatch.Consumer{Reactor: e.reactor("svc-sweeper"), RetryDelay: time.Millisecond}
	if err := c.Handle(e.ctx(), dqWithdrawn(e.tenant, "dqn_x", "dqx_forged", "general-ledger-svc")); err != nil {
		t.Fatal(err)
	}
	if n := len(e.flags(t, e.entity, "")); n != 0 {
		t.Fatalf("a forged event raised %d flags", n)
	}
	if e.scalar(t, `SELECT COALESCE(last_status, '') FROM certification_watch WHERE run_id = 'dqn_x'`) != certreliance.StateActive {
		t.Fatal("the check the event triggered was not recorded")
	}
	// Another relying service's withdrawal, another tenant's, and an unknown certification: nothing happens.
	e.status.set(certX, revokedX)
	asked := len(e.status.asked)
	for name, raw := range map[string][]byte{
		"another relying service":      dqWithdrawn(e.tenant, "dqn_x", "dqx_1", "tax-svc"),
		"another tenant":               dqWithdrawn(uuid.NewString(), "dqn_x", "dqx_1", "general-ledger-svc"),
		"a certification never relied": dqWithdrawn(e.tenant, "dqn_never", "dqx_1", "general-ledger-svc"),
		"undecodable":                  []byte(`not json`),
	} {
		if err := c.Handle(e.ctx(), raw); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n := len(e.flags(t, e.entity, "")); n != 0 {
			t.Fatalf("%s raised %d flags", name, n)
		}
	}
	if len(e.status.asked) != asked {
		t.Fatalf("an irrelevant event made the ledger ask the issuing service %d times", len(e.status.asked)-asked)
	}
}

func TestCertWithdrawalEvents_APrincipalWithoutTheSweepGrantRaisesNothing(t *testing.T) {
	e := newKEnv(t)
	e.postManual(t, "np-1", certX)
	e.status.set(certX, revokedX)
	e.denied.Store("svc-no-grant|GL_CERTIFICATION_WATCH_SWEEP", true)
	c := &certwatch.Consumer{Reactor: e.reactor("svc-no-grant"), RetryDelay: time.Millisecond}
	if err := c.Handle(e.ctx(), dqWithdrawn(e.tenant, "dqn_x", "dqx_1", "general-ledger-svc")); err != nil {
		t.Fatal(err)
	}
	if len(e.flags(t, e.entity, "")) != 0 {
		t.Fatal("an unauthorized principal acted on an event")
	}
}

// Through a real broker: a real consumer group reading BOTH issuing services' topics with kafka-go, offsets committed after handling.
// Skipped without TEST_KAFKA_BROKERS (for example, localhost:19092).
func TestCertWithdrawalEvents_ThroughARealBroker(t *testing.T) {
	brokers := os.Getenv("TEST_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("TEST_KAFKA_BROKERS not set")
	}
	addrs := strings.Split(brokers, ",")
	e := newKEnv(t)
	jX := e.postManual(t, "kb-1", certX)
	jZ := e.postManual(t, "kb-2", certZ)
	e.status.set(certX, revokedX)
	e.status.set(certZ, revokedZ)

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	dqTopic, recTopic := "test.dq.events."+suffix, "test.recon.events."+suffix
	// A freshly started broker accepts connections before its controller is ready, and answers EOF until it is: wait for it, rather than
	// fail a test on the broker's start-up.
	var createErr error
	for deadline := time.Now().Add(45 * time.Second); ; {
		conn, err := kafka.Dial("tcp", addrs[0])
		if err == nil {
			createErr = conn.CreateTopics(kafka.TopicConfig{Topic: dqTopic, NumPartitions: 1, ReplicationFactor: 1},
				kafka.TopicConfig{Topic: recTopic, NumPartitions: 1, ReplicationFactor: 1})
			_ = conn.Close()
			if createErr == nil {
				break
			}
		} else {
			createErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("the broker never became ready to create topics: %v", createErr)
		}
		time.Sleep(time.Second)
	}

	reader := kafka.NewReader(kafka.ReaderConfig{Brokers: addrs, GroupTopics: []string{dqTopic, recTopic}, GroupID: "test.cert-withdrawals." + suffix,
		StartOffset: kafka.FirstOffset, MaxWait: 200 * time.Millisecond})
	consumer := &certwatch.Consumer{Reactor: e.reactor("svc-sweeper"), Log: zap.NewNop(), RetryDelay: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { consumer.Run(ctx, reader); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the consumer did not stop")
		}
	}()

	w := &kafka.Writer{Addr: kafka.TCP(addrs...), Balancer: &kafka.LeastBytes{}, AllowAutoTopicCreation: false}
	defer w.Close()
	pubCtx, pubCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer pubCancel()
	if err := w.WriteMessages(pubCtx,
		kafka.Message{Topic: dqTopic, Key: []byte(e.tenant), Value: dqWithdrawn(e.tenant, "dqn_x", "dqx_1", "general-ledger-svc")},
		kafka.Message{Topic: recTopic, Value: recWithdrawn(e.tenant, "rcr_z", "rcv_2")},
		kafka.Message{Topic: dqTopic, Value: []byte(`{"event_type":"something.else"}`)},
	); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		flags := e.flags(t, e.entity, "OPEN")
		got := map[string]bool{}
		for _, f := range flags {
			got[f["journal_id"].(string)] = true
		}
		if got[jX] && got[jZ] {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no flags after 60s: %v", flags)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
