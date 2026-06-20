package telemetry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistryHealthAndMetricsHandlers(t *testing.T) {
	t.Parallel()

	reg := New("telemetry-test")
	reg.IncCounter("query.requests")
	reg.AddCounter("query.requests", 2)
	reg.SetGauge("bus-cursor", 7)
	reg.SetHealthy(false, "shutting down")

	healthReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	healthRes := httptest.NewRecorder()
	reg.HandleHealth(healthRes, healthReq)
	if healthRes.Code != http.StatusServiceUnavailable {
		t.Fatalf("health status = %d, want %d", healthRes.Code, http.StatusServiceUnavailable)
	}
	if body := healthRes.Body.String(); !strings.Contains(body, `"error":"shutting down"`) {
		t.Fatalf("health body = %q", body)
	}

	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRes := httptest.NewRecorder()
	reg.HandleMetrics(metricsRes, metricsReq)
	body := metricsRes.Body.String()
	if !strings.Contains(body, `email_agents_query_requests_total{service="telemetry-test"} 3`) {
		t.Fatalf("metrics body missing counter: %q", body)
	}
	if !strings.Contains(body, `email_agents_bus_cursor{service="telemetry-test"} 7`) {
		t.Fatalf("metrics body missing gauge: %q", body)
	}
}
