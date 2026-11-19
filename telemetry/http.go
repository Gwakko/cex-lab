package telemetry

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// ContentType is the Prometheus text exposition content type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Handler serves the registry at /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentType)
		if err := r.WriteText(w); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

// Health serves liveness and readiness. Liveness only says the process
// answers; readiness runs every registered check.
type Health struct {
	mu     sync.Mutex
	checks []check
}

type check struct {
	name string
	fn   func() error
}

// AddReadinessCheck registers a check that must pass for /readyz to be 200.
func (h *Health) AddReadinessCheck(name string, fn func() error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks = append(h.checks, check{name, fn})
}

// Liveness always answers 200 while the process can serve HTTP.
func (h *Health) Liveness() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
}

// Readiness answers 503 listing failing checks, or 200.
func (h *Health) Readiness() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		checks := append([]check(nil), h.checks...)
		h.mu.Unlock()
		var failed []string
		for _, c := range checks {
			if err := c.fn(); err != nil {
				failed = append(failed, c.name+": "+err.Error())
			}
		}
		if len(failed) > 0 {
			http.Error(w, strings.Join(failed, "\n"), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
}

// NewMux mounts /metrics, /healthz and /readyz.
func NewMux(r *Registry, h *Health) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", r.Handler())
	mux.Handle("GET /healthz", h.Liveness())
	mux.Handle("GET /readyz", h.Readiness())
	return mux
}
