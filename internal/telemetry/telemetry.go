package telemetry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Registry struct {
	service   string
	startedAt time.Time
	healthy   atomic.Bool
	errorText atomic.Value

	mu       sync.Mutex
	counters map[string]*atomic.Int64
	gauges   map[string]*atomic.Int64
}

func New(service string) *Registry {
	r := &Registry{
		service:   service,
		startedAt: time.Now().UTC(),
		counters:  make(map[string]*atomic.Int64),
		gauges:    make(map[string]*atomic.Int64),
	}
	r.healthy.Store(true)
	r.errorText.Store("")
	return r
}

func (r *Registry) IncCounter(name string) {
	r.counter(name).Add(1)
}

func (r *Registry) AddCounter(name string, delta int64) {
	r.counter(name).Add(delta)
}

func (r *Registry) SetGauge(name string, value int64) {
	r.gauge(name).Store(value)
}

func (r *Registry) SetHealthy(ok bool, message string) {
	r.healthy.Store(ok)
	r.errorText.Store(strings.TrimSpace(message))
}

func (r *Registry) HandleHealth(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{
		"ok":         r.healthy.Load(),
		"service":    r.service,
		"started_at": r.startedAt.Format(time.RFC3339),
	}
	if msg, _ := r.errorText.Load().(string); strings.TrimSpace(msg) != "" {
		payload["error"] = msg
	}
	status := http.StatusOK
	if !r.healthy.Load() {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (r *Registry) HandleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "email_agents_service_info{service=%q} 1\n", r.service)
	fmt.Fprintf(w, "email_agents_service_uptime_seconds{service=%q} %d\n", r.service, int64(time.Since(r.startedAt).Seconds()))
	fmt.Fprintf(w, "email_agents_service_healthy{service=%q} %d\n", r.service, boolToFloat(r.healthy.Load()))

	r.mu.Lock()
	counterNames := sortedKeys(r.counters)
	gaugeNames := sortedKeys(r.gauges)
	r.mu.Unlock()

	for _, name := range counterNames {
		fmt.Fprintf(w, "email_agents_%s_total{service=%q} %d\n", sanitize(name), r.service, r.counter(name).Load())
	}
	for _, name := range gaugeNames {
		fmt.Fprintf(w, "email_agents_%s{service=%q} %d\n", sanitize(name), r.service, r.gauge(name).Load())
	}
}

func (r *Registry) counter(name string) *atomic.Int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := &atomic.Int64{}
	r.counters[name] = c
	return c
}

func (r *Registry) gauge(name string) *atomic.Int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	g := &atomic.Int64{}
	r.gauges[name] = g
	return g
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sanitize(name string) string {
	replacer := strings.NewReplacer("-", "_", ".", "_", " ", "_")
	return replacer.Replace(name)
}

func boolToFloat(ok bool) int {
	if ok {
		return 1
	}
	return 0
}
