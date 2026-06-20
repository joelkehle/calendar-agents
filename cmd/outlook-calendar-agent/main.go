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

	"github.com/joelkehle/calendar-agents/internal/outlookcalendar"
	"github.com/joelkehle/calendar-agents/internal/telemetry"
)

func main() {
	busURL := flag.String("bus-url", envOrDefault("BUS_URL", "http://beelink:8081"), "Pinakes bus URL")
	agentID := flag.String("agent-id", envOrDefault("OUTLOOK_CALENDAR_AGENT_ID", outlookcalendar.DefaultAgentID), "Agent ID")
	httpAddr := flag.String("http-addr", envOrDefault("OUTLOOK_CALENDAR_HTTP_ADDR", outlookcalendar.DefaultHTTPAddr), "Health server address")
	powerShell := flag.String("powershell", envOrDefault("OUTLOOK_CALENDAR_POWERSHELL", ""), "PowerShell executable path")
	includePrivate := flag.Bool("include-private-details", envBool("OUTLOOK_CALENDAR_INCLUDE_PRIVATE_DETAILS", true), "include subject/location for private Outlook appointments")
	flag.Parse()

	secret := requiredEnv("OUTLOOK_CALENDAR_AGENT_SECRET")
	extractor := outlookcalendar.NewPowerShellExtractor()
	if strings.TrimSpace(*powerShell) != "" {
		extractor.Command = strings.TrimSpace(*powerShell)
	}
	extractor.IncludePrivateDetails = *includePrivate

	metrics := telemetry.New(*agentID)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", metrics.HandleHealth)
	mux.HandleFunc("/metrics", metrics.HandleMetrics)
	server := &http.Server{Addr: *httpAddr, Handler: mux}
	go serveHTTP(*agentID, server)

	agent := outlookcalendar.NewAgent(outlookcalendar.AgentConfig{
		BusURL:  *busURL,
		AgentID: *agentID,
		Secret:  secret,
	}, extractor, metrics)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go shutdownHTTP(ctx, *agentID, server)

	log.Printf("starting outlook calendar agent bus=%s agent=%s http=%s include_private_details=%t", *busURL, *agentID, *httpAddr, *includePrivate)
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

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
