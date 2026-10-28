package handler

import (
	"net/http"
	"time"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

// WithObserver wires the operational metrics of the kernel (standard §38). Without one nothing is observed.
func (h *KernelHandler) WithObserver(o kernel.Observer) *KernelHandler {
	h.obs = o
	return h
}

func (h *KernelHandler) observer() kernel.Observer {
	if h.obs == nil {
		return kernel.NopObserver{}
	}
	return h.obs
}

type obsRecorder struct {
	http.ResponseWriter
	status int
}

func (r *obsRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// idempotentCommands answer 200 for a repeat that returned the earlier result and 201 for new work.
var idempotentCommands = map[string]bool{
	"register_event": true, "post_event": true, "allocation_run": true, "consolidation_run": true, "revaluation_run": true,
}

// outcomeOf classifies a response: a refusal by an accounting control, an unavailable dependency and an unexpected failure are
// told apart, because they are handled by different people.
func outcomeOf(command string, status int) string {
	switch {
	case status == http.StatusOK && idempotentCommands[command]:
		return kernel.OutcomeExisting
	case status < 400:
		return kernel.OutcomeOK
	case status == http.StatusServiceUnavailable:
		return kernel.OutcomeDependency
	case status < 500:
		return kernel.OutcomeRefused
	}
	return kernel.OutcomeError
}

// observe records one kernel command: its outcome and how long it took.
func (h *KernelHandler) observe(command string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &obsRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			h.observer().Command(command, outcomeOf(command, rec.status), time.Since(start))
		})
	}
}
