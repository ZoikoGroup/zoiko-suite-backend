package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/eventing/egress"
	"zoiko.io/eventing/inbox"
	"zoiko.io/notification-svc/internal/config"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/mtls"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/whout"
)

// webhookMetrics exposes what INT-04 does, so a stuck or hostile destination is
// visible without reading logs.
type webhookMetrics struct {
	attempts *prometheus.CounterVec
	dead     *prometheus.CounterVec
	control  prometheus.Counter
	breaker  prometheus.Counter
}

func newWebhookMetrics(reg prometheus.Registerer) *webhookMetrics {
	m := &webhookMetrics{
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notification_webhook_attempts_total", Help: "Outbound webhook delivery attempts, by outcome.",
			ConstLabels: prometheus.Labels{"service": "notification-svc"}}, []string{"outcome"}),
		dead: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notification_webhook_dead_letters_total", Help: "Outbound webhook deliveries dead-lettered, by reason.",
			ConstLabels: prometheus.Labels{"service": "notification-svc"}}, []string{"reason"}),
		control: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "notification_webhook_control_failures_total", Help: "Webhook bodies blocked before send for being outside the approved scope.",
			ConstLabels: prometheus.Labels{"service": "notification-svc"}}),
		breaker: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "notification_webhook_breaker_deferrals_total", Help: "Deliveries deferred because the destination's circuit breaker is open.",
			ConstLabels: prometheus.Labels{"service": "notification-svc"}}),
	}
	reg.MustRegister(m.attempts, m.dead, m.control, m.breaker)
	return m
}

func (m *webhookMetrics) Attempt(o whout.AttemptOutcome) { m.attempts.WithLabelValues(string(o)).Inc() }
func (m *webhookMetrics) DeadLetter(reason string)       { m.dead.WithLabelValues(reason).Inc() }
func (m *webhookMetrics) ControlFailure()                { m.control.Inc() }
func (m *webhookMetrics) BreakerDeferred()               { m.breaker.Inc() }

// wireWebhookOut mounts the INT-04 API and starts the fan-out consumer and the
// dispatcher. It does nothing when the feature is off.
func wireWebhookOut(ctx context.Context, cfg *config.Config, log *zap.Logger, pool *pgxpool.Pool, pg *store.PgStore,
	authzClient handler.AuthZClient, r chi.Router) {
	wc := cfg.WebhookOut
	if !wc.Enabled {
		log.Info("outbound webhooks are off (NOTIFICATION_WEBHOOK_OUT_ENABLED is not true)")
		return
	}

	// The shared inbox and its schema (migration 000032) must be there: fail at deploy
	// time, not on the first event.
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := inbox.VerifySchema(checkCtx, pool); err != nil {
		log.Fatal("eventing inbox schema missing (apply migration 000032)", zap.Error(err))
	}

	var vaultHTTP *http.Client
	if wc.VaultMTLS {
		c, err := mtls.NewClientHTTPClient(ctx, cfg.MTLSManagementServiceURL, "notification-svc", platformScopeID)
		if err != nil {
			log.Fatal("webhooks: mtls client identity for the vault", zap.Error(err))
		}
		vaultHTTP = c
		if vaultHTTP.Timeout <= 0 {
			vaultHTTP.Timeout = 5 * time.Second
		}
	}
	vault, err := whout.NewVaultClient(wc.VaultURL, vaultHTTP, wc.SignerPrincipal)
	if err != nil {
		log.Fatal("webhooks: vault client", zap.Error(err))
	}

	policy := egress.Policy{AllowHTTP: wc.AllowHTTP, AllowedPorts: wc.AllowedPorts}
	svc, err := whout.NewService(whout.ServiceConfig{Store: pg, Keys: vault, Egress: policy, AllowedRegions: wc.AllowedRegions})
	if err != nil {
		log.Fatal("webhooks: service", zap.Error(err))
	}
	handler.RegisterWebhookRoutes(r, handler.NewWebhookHandler(svc, authzClient, log))

	dcfg := whout.DefaultDispatcherConfig()
	dcfg.Workers, dcfg.BatchSize, dcfg.PollInterval = wc.Workers, wc.BatchSize, wc.PollInterval
	dcfg.RequestTimeout = wc.RequestTimeout
	dcfg.Retry = whout.RetryPolicy{Base: wc.RetryBase, Cap: wc.RetryCap}
	dcfg.PerDestinationConcurrency, dcfg.BreakerThreshold, dcfg.BreakerCooldown = wc.PerDestinationConcurrency, wc.BreakerThreshold, wc.BreakerCooldown
	dcfg.DisableAfterDeadLetters = wc.DisableAfterDeadLetters
	disp, err := whout.NewDispatcher(whout.DispatcherDeps{
		Store: pg, Signing: vault, Egress: policy, AllowedRegions: wc.AllowedRegions, Log: log,
		Observer: newWebhookMetrics(prometheus.DefaultRegisterer),
	}, dcfg)
	if err != nil {
		log.Fatal("webhooks: dispatcher", zap.Error(err))
	}
	go disp.Run(ctx)

	if len(wc.SourceTopics) == 0 || len(cfg.Kafka.Brokers) == 0 {
		log.Warn("webhook fan-out consumer is not running: WEBHOOK_SOURCE_TOPICS and KAFKA_BROKERS are both required",
			zap.Strings("topics", wc.SourceTopics), zap.Int("brokers", len(cfg.Kafka.Brokers)))
		return
	}
	in, err := inbox.New(inbox.PoolRunner{Pool: pool, TenantSetting: "app.tenant_id"}, whout.ConsumerName, inbox.DefaultConfig())
	if err != nil {
		log.Fatal("webhooks: inbox", zap.Error(err))
	}
	if len(wc.AllowedRegions) == 1 { // a single-region deployment pins its receipts; a multi-region one cannot name one
		in.SetRegion(strings.ToLower(strings.TrimSpace(wc.AllowedRegions[0])))
	}
	consumer := whout.NewConsumer(log, pg, in)
	go whout.SuperviseTopics(ctx, log, wc.SourceTopics, kafkaTopicLister(cfg.Kafka.Brokers),
		func(runCtx context.Context, topics []string) {
			consumer.Run(runCtx, kafka.NewReader(kafka.ReaderConfig{
				Brokers: cfg.Kafka.Brokers, GroupID: wc.ConsumerGroup, GroupTopics: topics,
				// A new group starts at the beginning, not the end: events produced before its
				// first assignment are not lost. FanOut only delivers events that postdate a
				// subscription activation, so reading history floods nobody.
				StartOffset:            kafka.FirstOffset,
				WatchPartitionChanges:  true,
				PartitionWatchInterval: 10 * time.Second,
				ErrorLogger: kafka.LoggerFunc(func(msg string, args ...interface{}) {
					log.Warn("webhook fan-out kafka reader: " + fmt.Sprintf(msg, args...))
				}),
			}))
		}, 15*time.Second)
}

// kafkaTopicLister asks the first reachable broker which topics exist.
func kafkaTopicLister(brokers []string) whout.TopicLister {
	return func(ctx context.Context) (map[string]bool, error) {
		var lastErr error
		for _, b := range brokers {
			dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			conn, err := kafka.DialContext(dctx, "tcp", b)
			cancel()
			if err != nil {
				lastErr = err
				continue
			}
			parts, err := conn.ReadPartitions()
			_ = conn.Close()
			if err != nil {
				lastErr = err
				continue
			}
			out := map[string]bool{}
			for _, p := range parts {
				out[p.Topic] = true
			}
			return out, nil
		}
		return nil, lastErr
	}
}
