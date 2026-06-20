package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
	"github.com/joelkehle/calendar-agents/pkg/outlookwritecontract"
	"github.com/joelkehle/calendar-agents/pkg/schedulercontract"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

const (
	defaultSender     = "jk-calendar-guard-agent"
	defaultJKBUS      = "http://localhost:8081"
	defaultUCLABUS    = "http://localhost:8080"
	defaultReadLimit  = 3
	defaultWait       = 45 * time.Second
	defaultSmokeVenue = "200 Medical Plaza, Los Angeles, CA 90024"
)

type config struct {
	from         string
	secret       string
	jkBus        string
	uclaBus      string
	date         time.Time
	location     string
	maxEvents    int
	wait         time.Duration
	skipWriter   bool
	skipReads    bool
	skipEstimate bool
}

type check struct {
	name     string
	busURL   string
	target   string
	body     string
	meta     map[string]any
	validate func(string) error
	summary  func(string) string
}

type conversationMessage struct {
	Type string `json:"type"`
	From string `json:"from"`
	Body string `json:"body"`
}

type conversationTranscript struct {
	Messages []conversationMessage `json:"messages"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseConfig(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	checks, err := buildChecks(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.wait*time.Duration(len(checks))+10*time.Second)
	defer cancel()
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for i, check := range checks {
		client := busclient.NewClient(check.busURL)
		conversationID := fmt.Sprintf("calendar-live-smoke-%s-%02d-%s", stamp, i+1, check.name)
		requestID := fmt.Sprintf("calendar-live-smoke-%s-%02d", stamp, i+1)
		if _, err := client.SendMessage(ctx, cfg.from, cfg.secret, check.target, conversationID, requestID, "request", check.body, nil, check.meta); err != nil {
			fmt.Fprintf(stderr, "%s send failed: %v\n", check.name, err)
			return 1
		}
		body, err := waitResponse(ctx, client, conversationID, check.target, cfg.wait)
		if err != nil {
			fmt.Fprintf(stderr, "%s response failed: %v\n", check.name, err)
			return 1
		}
		if err := check.validate(body); err != nil {
			fmt.Fprintf(stderr, "%s validation failed: %v\nbody=%s\n", check.name, err, body)
			return 1
		}
		fmt.Fprintf(stdout, "%s ok: %s\n", check.name, check.summary(body))
	}
	return 0
}

func parseConfig(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("calendar-live-smoke", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.from, "from", envOrDefault("CALENDAR_SMOKE_FROM", defaultSender), "registered sender agent id")
	fs.StringVar(&cfg.secret, "secret", strings.TrimSpace(os.Getenv("CALENDAR_SMOKE_AGENT_SECRET")), "sender HMAC secret")
	fs.StringVar(&cfg.jkBus, "jk-bus", envOrDefault("CALENDAR_SMOKE_JK_BUS", defaultJKBUS), "JK Pinakes bus URL")
	fs.StringVar(&cfg.uclaBus, "ucla-bus", envOrDefault("CALENDAR_SMOKE_UCLA_BUS", defaultUCLABUS), "UCLA Pinakes bus URL")
	dateText := fs.String("date", strings.TrimSpace(os.Getenv("CALENDAR_SMOKE_DATE")), "local date to read, YYYY-MM-DD; default today in America/Los_Angeles")
	fs.StringVar(&cfg.location, "location", envOrDefault("CALENDAR_SMOKE_LOCATION", defaultSmokeVenue), "location for scheduler travel-estimate")
	fs.IntVar(&cfg.maxEvents, "max-events", envInt("CALENDAR_SMOKE_MAX_EVENTS", defaultReadLimit), "max events to request per read smoke")
	waitText := fs.String("wait", envOrDefault("CALENDAR_SMOKE_WAIT", defaultWait.String()), "response wait duration")
	fs.BoolVar(&cfg.skipWriter, "skip-writer", envBool("CALENDAR_SMOKE_SKIP_WRITER", false), "skip write-agent refusal smoke")
	fs.BoolVar(&cfg.skipReads, "skip-reads", envBool("CALENDAR_SMOKE_SKIP_READS", false), "skip Outlook read smokes")
	fs.BoolVar(&cfg.skipEstimate, "skip-estimate", envBool("CALENDAR_SMOKE_SKIP_ESTIMATE", false), "skip scheduler travel-estimate smoke")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if strings.TrimSpace(cfg.from) == "" {
		return cfg, errors.New("-from is required")
	}
	if strings.TrimSpace(cfg.secret) == "" {
		return cfg, errors.New("CALENDAR_SMOKE_AGENT_SECRET or -secret is required")
	}
	if cfg.maxEvents <= 0 {
		cfg.maxEvents = defaultReadLimit
	}
	wait, err := time.ParseDuration(*waitText)
	if err != nil || wait <= 0 {
		return cfg, fmt.Errorf("invalid -wait %q", *waitText)
	}
	cfg.wait = wait
	loc, err := time.LoadLocation(schedulercontract.DefaultTimeZone)
	if err != nil {
		return cfg, err
	}
	if strings.TrimSpace(*dateText) == "" {
		now := time.Now().In(loc)
		cfg.date = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		return cfg, nil
	}
	parsed, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(*dateText), loc)
	if err != nil {
		return cfg, fmt.Errorf("invalid -date %q: %w", *dateText, err)
	}
	cfg.date = parsed
	return cfg, nil
}

func buildChecks(cfg config) ([]check, error) {
	var checks []check
	if !cfg.skipReads {
		body, err := marshalBody(calendarreadcontract.Request{
			Action: "events-list",
			Query: calendarreadcontract.EventsQuery{
				TimeMin:      cfg.date.Format(time.RFC3339),
				TimeMax:      cfg.date.AddDate(0, 0, 1).Format(time.RFC3339),
				SingleEvents: true,
				OrderBy:      "startTime",
				MaxResults:   cfg.maxEvents,
			},
		})
		if err != nil {
			return nil, err
		}
		checks = append(checks,
			check{
				name:     "ucla-read",
				busURL:   cfg.uclaBus,
				target:   "ucla-tdg-outlook-calendar-agent",
				body:     body,
				validate: validateEvents,
				summary:  summarizeEvents,
			},
			check{
				name:     "jk-read",
				busURL:   cfg.jkBus,
				target:   "jk-outlook-calendar-agent",
				body:     body,
				validate: validateEvents,
				summary:  summarizeEvents,
			},
		)
	}
	if !cfg.skipEstimate {
		body, err := marshalBody(schedulercontract.Request{
			Action:     schedulercontract.CapabilityEstimate,
			RequestID:  "calendar-live-smoke-estimate-" + time.Now().UTC().Format("20060102T150405.000000000Z"),
			EventStart: time.Date(cfg.date.Year(), cfg.date.Month(), cfg.date.Day(), 9, 0, 0, 0, cfg.date.Location()).AddDate(0, 0, 2).Format(time.RFC3339),
			Location:   cfg.location,
		})
		if err != nil {
			return nil, err
		}
		checks = append(checks, check{
			name:     "scheduler-estimate",
			busURL:   cfg.uclaBus,
			target:   schedulercontract.DefaultAgentID,
			body:     body,
			validate: validateEstimate,
			summary:  summarizeEstimate,
		})
	}
	if !cfg.skipWriter {
		summary := "Joel + live smoke refusal " + time.Now().UTC().Format("20060102T150405Z")
		description := "Non-destructive smoke: this sender is not allowlisted for working holds."
		showAs := "busy"
		start := cfg.date.AddDate(0, 0, 2).Add(7 * time.Hour).Format(time.RFC3339)
		end := cfg.date.AddDate(0, 0, 2).Add(7*time.Hour + 30*time.Minute).Format(time.RFC3339)
		body, err := marshalBody(outlookwritecontract.Request{
			Action:     "event-insert",
			CalendarID: "default",
			Event: outlookwritecontract.EventInput{
				Summary:     &summary,
				Description: &description,
				Start:       &outlookwritecontract.EventDateTime{DateTime: start, TimeZone: schedulercontract.DefaultTimeZone},
				End:         &outlookwritecontract.EventDateTime{DateTime: end, TimeZone: schedulercontract.DefaultTimeZone},
				ShowAs:      &showAs,
			},
		})
		if err != nil {
			return nil, err
		}
		checks = append(checks, check{
			name:     "writer-refusal",
			busURL:   cfg.uclaBus,
			target:   "ucla-tdg-outlook-calendar-write-agent",
			body:     body,
			meta:     map[string]any{"request_id": "calendar-live-smoke-refusal-" + time.Now().UTC().Format("20060102T150405.000000000Z")},
			validate: validateRefusal,
			summary:  summarizeRefusal,
		})
	}
	if len(checks) == 0 {
		return nil, errors.New("all checks are skipped")
	}
	return checks, nil
}

func waitResponse(ctx context.Context, client *busclient.Client, conversationID, target string, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	path := "/v1/conversations/" + url.PathEscape(conversationID) + "/messages?cursor=0&limit=50"
	for time.Now().Before(deadline) {
		out, _, err := client.DoJSON(ctx, http.MethodGet, path, nil, nil)
		if err != nil {
			return "", err
		}
		var transcript conversationTranscript
		if err := json.Unmarshal(out, &transcript); err != nil {
			return "", err
		}
		for _, msg := range transcript.Messages {
			if msg.From == target && msg.Type == "response" {
				return msg.Body, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return "", fmt.Errorf("timed out waiting for %s in %s", target, conversationID)
}

func validateEvents(body string) error {
	var resp calendarreadcontract.EventsListResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return err
	}
	var errResp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &errResp)
	if strings.TrimSpace(errResp.Error) != "" {
		return errors.New(errResp.Error)
	}
	if resp.Events == nil {
		return errors.New("missing events array")
	}
	return nil
}

func validateEstimate(body string) error {
	var resp schedulercontract.Reply
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return err
	}
	if resp.Status != schedulercontract.StatusEstimated {
		return fmt.Errorf("status=%q error_code=%q message=%q", resp.Status, resp.ErrorCode, resp.Message)
	}
	if resp.Estimate == nil {
		return errors.New("missing estimate")
	}
	return nil
}

func validateRefusal(body string) error {
	var resp outlookwritecontract.MutationResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return err
	}
	if resp.ErrorCode != "not_allowlisted" {
		return fmt.Errorf("error_code=%q error=%q", resp.ErrorCode, resp.Error)
	}
	if resp.Event != nil || resp.WouldWrite != nil {
		return errors.New("refusal response unexpectedly included an event")
	}
	return nil
}

func summarizeEvents(body string) string {
	var resp calendarreadcontract.EventsListResponse
	_ = json.Unmarshal([]byte(body), &resp)
	return fmt.Sprintf("events=%d", len(resp.Events))
}

func summarizeEstimate(body string) string {
	var resp schedulercontract.Reply
	_ = json.Unmarshal([]byte(body), &resp)
	if resp.Estimate == nil {
		return "status=" + resp.Status
	}
	return fmt.Sprintf("status=%s minutes=%d source=%s", resp.Status, resp.Estimate.Minutes, resp.Estimate.Source)
}

func summarizeRefusal(body string) string {
	var resp outlookwritecontract.MutationResponse
	_ = json.Unmarshal([]byte(body), &resp)
	return "error_code=" + resp.ErrorCode
}

func marshalBody(value any) (string, error) {
	blob, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(blob), nil
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
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "1", "t", "true", "yes", "y":
		return true
	case "0", "f", "false", "no", "n":
		return false
	default:
		return fallback
	}
}
