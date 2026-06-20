package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joelkehle/calendar-agents/internal/scheduler"
	"github.com/joelkehle/calendar-agents/internal/telemetry"
)

func main() {
	busURL := flag.String("bus-url", envOrDefault("SCHEDULER_BUS_URL", scheduler.DefaultBusURL), "Pinakes bus URL")
	agentID := flag.String("agent-id", envOrDefault("SCHEDULER_AGENT_ID", scheduler.DefaultAgentID), "Agent ID")
	httpAddr := flag.String("http-addr", envOrDefault("SCHEDULER_HTTP_ADDR", scheduler.DefaultHTTPAddr), "Health server address")
	readAgent := flag.String("calendar-read-agent", envOrDefault("SCHEDULER_CALENDAR_READ_AGENT", scheduler.DefaultCalendarReadAgent), "calendar read agent id")
	writeAgent := flag.String("calendar-write-agent", envOrDefault("SCHEDULER_CALENDAR_WRITE_AGENT", scheduler.DefaultCalendarWriteAgent), "calendar write agent id")
	// Travel knowledge paths resolve relative to the process working
	// directory; the systemd unit must set absolute paths at deploy time.
	locationsPath := flag.String("locations-path", envOrDefault("SCHEDULER_LOCATIONS_PATH", "data/locations.json"), "Joel's origins file (data/locations.json)")
	venuesPath := flag.String("venues-path", envOrDefault("SCHEDULER_VENUES_PATH", "data/venues.json"), "venue knowledge file (data/venues.json)")
	offsiteCategory := flag.String("offsite-category", envOrDefault("SCHEDULER_OFFSITE_CATEGORY", scheduler.DefaultOffsiteCategory), "Outlook category marking a meeting offsite")
	watchIntervalMin := flag.Int("watch-interval-min", envInt("SCHEDULER_WATCH_INTERVAL_MIN", 15), "reconciliation watcher tick interval in minutes (0 or negative disables the watcher)")
	watchHorizonDays := flag.Int("watch-horizon-days", envInt("SCHEDULER_WATCH_HORIZON_DAYS", 3), "number of days beyond today each watcher tick scans")
	flag.Parse()

	metrics := telemetry.New(scheduler.DefaultAgentID)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", metrics.HandleHealth)
	mux.HandleFunc("/metrics", metrics.HandleMetrics)
	server := &http.Server{Addr: *httpAddr, Handler: mux}
	go serveHTTP(scheduler.DefaultAgentID, server)

	agent := scheduler.NewAgent(scheduler.Config{
		BusURL:             *busURL,
		AgentID:            *agentID,
		Secret:             requiredEnv("SCHEDULER_AGENT_SECRET"),
		CalendarReadAgent:  *readAgent,
		CalendarWriteAgent: *writeAgent,
		LocationsPath:      *locationsPath,
		VenuesPath:         *venuesPath,
		OffsiteCategory:    *offsiteCategory,
		WatchIntervalMin:   *watchIntervalMin,
		WatchHorizonDays:   *watchHorizonDays,
	}, metrics)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go shutdownHTTP(ctx, scheduler.DefaultAgentID, server)

	log.Printf("starting %s bus=%s agent=%s http=%s read_agent=%s write_agent=%s watch_interval_min=%d watch_horizon_days=%d", scheduler.DefaultAgentID, *busURL, *agentID, *httpAddr, *readAgent, *writeAgent, *watchIntervalMin, *watchHorizonDays)
	if err := agent.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func serveHTTP(name string, server *http.Server) {
	log.Printf("%s health listening on %s", name, server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func shutdownHTTP(ctx context.Context, name string, server *http.Server) {
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("%s http shutdown failed: %v", name, err)
	}
}

func requiredEnv(key string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		log.Fatalf("missing required env var %s", key)
	}
	return value
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		log.Printf("invalid %s=%q, using default %d", key, value, fallback)
		return fallback
	}
	return parsed
}
