package oidc

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the provider's IAM §26 signals: logins by outcome, throttling,
// refresh-token reuse and code replay (token replay indicators), tokens by
// grant, ended login sessions, and session-status answers.
type Metrics struct {
	logins        *prometheus.CounterVec
	throttled     *prometheus.CounterVec
	tokens        *prometheus.CounterVec
	refreshReuse  prometheus.Counter
	codeReplay    prometheus.Counter
	clientAuthBad prometheus.Counter
	sessionsEnded *prometheus.CounterVec
	statusChecks  *prometheus.CounterVec
}

func newMetrics() *Metrics {
	return &Metrics{
		logins: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "identity_login_attempts_total",
			Help: "Hosted and internal login attempts by step and result."}, []string{"step", "result"}),
		throttled: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "identity_login_throttled_total",
			Help: "Credential requests refused 429 by reason (AU-04)."}, []string{"reason"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "identity_tokens_issued_total",
			Help: "Tokens issued by the token endpoint by grant type."}, []string{"grant"}),
		refreshReuse: prometheus.NewCounter(prometheus.CounterOpts{Name: "identity_refresh_token_reuse_total",
			Help: "Refresh tokens presented after rotation (family revoked, session ended)."}),
		codeReplay: prometheus.NewCounter(prometheus.CounterOpts{Name: "identity_authorization_code_replay_total",
			Help: "Authorization codes presented twice (session ended)."}),
		clientAuthBad: prometheus.NewCounter(prometheus.CounterOpts{Name: "identity_client_auth_failures_total",
			Help: "OAuth client authentication failures."}),
		sessionsEnded: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "identity_login_sessions_ended_total",
			Help: "Login sessions ended by reason."}, []string{"reason"}),
		statusChecks: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "identity_session_status_checks_total",
			Help: "Relying-party session status answers (AU-01)."}, []string{"result"}),
	}
}

// NewMetrics builds and registers the provider's metrics with reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := newMetrics()
	for _, c := range []prometheus.Collector{m.logins, m.throttled, m.tokens, m.refreshReuse, m.codeReplay,
		m.clientAuthBad, m.sessionsEnded, m.statusChecks} {
		_ = reg.Register(c)
	}
	return m
}

// NopMetrics are unregistered (tests).
func NopMetrics() *Metrics { return newMetrics() }

func (m *Metrics) Login(step, result string)     { m.logins.WithLabelValues(step, result).Inc() }
func (m *Metrics) Throttled(reason string)       { m.throttled.WithLabelValues(reason).Inc() }
func (m *Metrics) Issued(grant string)           { m.tokens.WithLabelValues(grant).Inc() }
func (m *Metrics) RefreshReuse()                 { m.refreshReuse.Inc() }
func (m *Metrics) CodeReplay()                   { m.codeReplay.Inc() }
func (m *Metrics) ClientAuthFailure()            { m.clientAuthBad.Inc() }
func (m *Metrics) LoginSessionEnded(r string)    { m.sessionsEnded.WithLabelValues(r).Inc() }
func (m *Metrics) StatusCheck(result string)     { m.statusChecks.WithLabelValues(result).Inc() }
