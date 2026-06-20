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

	"github.com/joelkehle/calendar-agents/internal/outlookcalendarwrite"
	"github.com/joelkehle/calendar-agents/internal/telemetry"
)

func main() {
	busURL := flag.String("bus-url", envOrDefault("BUS_URL", "http://beelink:8080"), "Pinakes bus URL")
	agentID := flag.String("agent-id", envOrDefault("OUTLOOK_CALENDAR_WRITE_AGENT_ID", outlookcalendarwrite.DefaultAgentID), "Agent ID")
	httpAddr := flag.String("http-addr", envOrDefault("OUTLOOK_CALENDAR_WRITE_HTTP_ADDR", outlookcalendarwrite.DefaultHTTPAddr), "Health server address")
	powerShell := flag.String("powershell", envOrDefault("OUTLOOK_CALENDAR_WRITE_POWERSHELL", ""), "PowerShell executable path")
	dryRun := flag.Bool("dry-run", envBool("OUTLOOK_CALENDAR_WRITE_DRY_RUN", true), "return would-write responses without mutating Outlook")
	holdRequesters := flag.String("hold-requesters", envOrDefault("OUTLOOK_CALENDAR_WRITE_HOLD_REQUESTERS", ""), "comma-separated bus agent ids allowed to create working holds")
	flag.Parse()

	secret := requiredEnv("OUTLOOK_CALENDAR_WRITE_AGENT_SECRET")
	service := outlookcalendarwrite.NewPowerShellService()
	if strings.TrimSpace(*powerShell) != "" {
		service.Command = strings.TrimSpace(*powerShell)
	}
	service.TravelCategories = strings.TrimSpace(os.Getenv("OUTLOOK_CALENDAR_WRITE_TRAVEL_CATEGORIES"))
	service.HoldCategories = strings.TrimSpace(os.Getenv("OUTLOOK_CALENDAR_WRITE_HOLD_CATEGORIES"))
	holdTimeZoneOK := true
	holdTimeZoneError := ""
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := service.CheckHostTimeZone(probeCtx, outlookcalendarwrite.DefaultTimeZone); err != nil {
		holdTimeZoneOK = false
		holdTimeZoneError = err.Error()
		log.Printf("%s working-hold and travel-block writes disabled: %v", outlookcalendarwrite.DefaultAgentID, err)
	}
	probeCancel()

	metrics := telemetry.New(outlookcalendarwrite.DefaultAgentID)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", metrics.HandleHealth)
	mux.HandleFunc("/metrics", metrics.HandleMetrics)
	server := &http.Server{Addr: *httpAddr, Handler: mux}
	go serveHTTP(outlookcalendarwrite.DefaultAgentID, server)

	agent := outlookcalendarwrite.NewAgent(outlookcalendarwrite.AgentConfig{
		BusURL:            *busURL,
		AgentID:           *agentID,
		Secret:            secret,
		DryRun:            *dryRun,
		HoldRequesters:    outlookcalendarwrite.ParseHoldRequesters(*holdRequesters),
		HoldTimeZoneOK:    holdTimeZoneOK,
		HoldTimeZoneError: holdTimeZoneError,
	}, service, metrics)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go shutdownHTTP(ctx, outlookcalendarwrite.DefaultAgentID, server)

	log.Printf("starting %s bus=%s agent=%s http=%s dry_run=%t", outlookcalendarwrite.DefaultAgentID, *busURL, *agentID, *httpAddr, *dryRun)
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
