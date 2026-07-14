package busagent

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joelkehle/calendar-agents/internal/telemetry"
	"github.com/joelkehle/pinakes/pkg/busclient"
)

var (
	heartbeatInterval = 60 * time.Second
	newTicker         = time.NewTicker
	afterFunc         = time.AfterFunc
	sleep             = time.Sleep
)

type EventHandler func(context.Context, busclient.InboxEvent) error
type EventPriority func(busclient.InboxEvent) int
type TickHandler func(context.Context, time.Time) error

type LoopConfig struct {
	BusURL        string
	AgentID       string
	Secret        string
	Capabilities  []string
	Description   string
	PollWaitSec   int
	TickInterval  time.Duration
	ShutdownGrace time.Duration
	AgentClass    string
	MutationClass string
	CursorFile    string
	StartAtLatest bool
	Metrics       *telemetry.Registry
	HandleEvent   EventHandler
	EventPriority EventPriority
	HandleTick    TickHandler
}

type Loop struct {
	cfg    LoopConfig
	client *busclient.Client
	cursor int

	mu             sync.Mutex
	shuttingDown   bool
	pollCancel     context.CancelFunc
	stopOperations context.CancelFunc
	shutdownOnce   sync.Once
}

func New(cfg LoopConfig) *Loop {
	if cfg.PollWaitSec <= 0 {
		cfg.PollWaitSec = 5
	}
	if cfg.ShutdownGrace <= 0 {
		cfg.ShutdownGrace = 8 * time.Second
	}
	return &Loop{
		cfg:    cfg,
		client: busclient.NewClient(cfg.BusURL),
	}
}

func (l *Loop) Client() *busclient.Client {
	return l.client
}

func (l *Loop) Run(ctx context.Context) error {
	if err := l.registerWithRetry(ctx); err != nil {
		return err
	}
	if err := l.initializeCursor(ctx); err != nil {
		return err
	}

	intakeCtx, stopIntake := context.WithCancel(context.Background())
	defer stopIntake()

	operationCtx, stopOperations := context.WithCancel(context.Background())
	defer stopOperations()
	l.setOperationCancel(stopOperations)

	stopShutdownHook := context.AfterFunc(ctx, func() {
		l.beginShutdown(stopIntake)
	})
	defer stopShutdownHook()

	heartbeatCtx, stopHeartbeat := context.WithCancel(intakeCtx)
	defer stopHeartbeat()
	go l.runHeartbeat(heartbeatCtx)

	var tick *time.Ticker
	if l.cfg.TickInterval > 0 {
		tick = newTicker(l.cfg.TickInterval)
		defer tick.Stop()
		if l.cfg.HandleTick != nil {
			if err := l.cfg.HandleTick(operationCtx, time.Now().UTC()); err != nil {
				if l.shouldSuppressShutdownErr(err) {
					return shutdownErr(ctx)
				}
				log.Printf("%s initial tick failed: %v", l.cfg.AgentID, err)
			}
		}
	}

	for {
		if l.isShuttingDown() {
			return shutdownErr(ctx)
		}
		select {
		case <-ctx.Done():
			return shutdownErr(ctx)
		case t := <-tickChan(tick):
			if l.isShuttingDown() {
				return shutdownErr(ctx)
			}
			if l.cfg.HandleTick != nil {
				if err := l.cfg.HandleTick(operationCtx, t.UTC()); err != nil {
					if l.shouldSuppressShutdownErr(err) {
						return shutdownErr(ctx)
					}
					log.Printf("%s tick failed: %v", l.cfg.AgentID, err)
					l.metricError()
				}
			}
		default:
			if l.isShuttingDown() {
				return shutdownErr(ctx)
			}
			events, next, err := l.pollInbox(intakeCtx)
			if err != nil {
				if l.shouldSuppressShutdownErr(err) {
					return shutdownErr(ctx)
				}
				if unauthorized(err) {
					if regErr := l.register(intakeCtx); regErr != nil && !l.shouldSuppressShutdownErr(regErr) {
						log.Printf("%s poll re-register failed: %v", l.cfg.AgentID, regErr)
					}
				}
				log.Printf("%s poll failed: %v", l.cfg.AgentID, err)
				l.metricError()
				sleep(500 * time.Millisecond)
				continue
			}
			if l.isShuttingDown() || intakeCtx.Err() != nil {
				return shutdownErr(ctx)
			}
			if len(events) == 0 {
				l.advanceCursor(next)
				continue
			}
			l.prioritize(events)
			log.Printf("%s handling %d bus event(s) cursor=%d next=%d", l.cfg.AgentID, len(events), l.cursor, next)
			for _, evt := range events {
				if l.isShuttingDown() {
					return shutdownErr(ctx)
				}
				if l.cfg.HandleEvent == nil {
					continue
				}
				started := time.Now()
				if l.cfg.Metrics != nil {
					l.cfg.Metrics.SetGauge("bus_event_inflight", 1)
					l.cfg.Metrics.SetGauge("bus_last_event_started_unix", started.Unix())
				}
				if err := l.cfg.HandleEvent(operationCtx, evt); err != nil {
					if l.shouldSuppressShutdownErr(err) {
						return shutdownErr(ctx)
					}
					log.Printf("%s handle event failed: %v", l.cfg.AgentID, err)
					l.metricError()
				}
				elapsed := time.Since(started)
				if l.cfg.Metrics != nil {
					l.cfg.Metrics.SetGauge("bus_event_inflight", 0)
					l.cfg.Metrics.SetGauge("bus_last_event_finished_unix", time.Now().Unix())
					l.cfg.Metrics.SetGauge("bus_last_event_duration_ms", elapsed.Milliseconds())
					l.cfg.Metrics.IncCounter("bus_events_handled")
				}
				if elapsed > 10*time.Second {
					log.Printf("%s slow bus event message=%s from=%s type=%s conversation=%s elapsed=%s", l.cfg.AgentID, evt.MessageID, evt.From, evt.Type, evt.ConversationID, elapsed.Round(time.Millisecond))
				}
			}
			l.advanceCursor(next)
		}
	}
}

func (l *Loop) initializeCursor(ctx context.Context) error {
	if strings.TrimSpace(l.cfg.CursorFile) != "" {
		cursor, ok, err := loadCursor(l.cfg.CursorFile)
		if err != nil {
			return err
		}
		if ok {
			l.cursor = cursor
			return nil
		}
	}
	if !l.cfg.StartAtLatest {
		return nil
	}
	_, next, err := l.client.PollInbox(ctx, l.cfg.AgentID, l.cfg.Secret, math.MaxInt, 0)
	if err != nil {
		return err
	}
	l.cursor = next
	_ = saveCursor(l.cfg.CursorFile, next)
	return nil
}

func (l *Loop) pollInbox(ctx context.Context) ([]busclient.InboxEvent, int, error) {
	timeout := time.Duration(l.cfg.PollWaitSec+10) * time.Second
	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	}
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	l.setPollCancel(cancel)
	defer func() {
		cancel()
		l.clearPollCancel()
	}()
	started := time.Now()
	if l.cfg.Metrics != nil {
		l.cfg.Metrics.SetGauge("bus_poll_inflight", 1)
		l.cfg.Metrics.SetGauge("bus_last_poll_started_unix", started.Unix())
	}
	events, next, err := l.client.PollInbox(pollCtx, l.cfg.AgentID, l.cfg.Secret, l.cursor, l.cfg.PollWaitSec)
	elapsed := time.Since(started)
	if l.cfg.Metrics != nil {
		l.cfg.Metrics.SetGauge("bus_poll_inflight", 0)
		l.cfg.Metrics.SetGauge("bus_last_poll_finished_unix", time.Now().Unix())
		l.cfg.Metrics.SetGauge("bus_last_poll_duration_ms", elapsed.Milliseconds())
		l.cfg.Metrics.SetGauge("bus_last_poll_events", int64(len(events)))
	}
	if elapsed > timeout/2 || len(events) > 0 {
		log.Printf("%s polled bus cursor=%d next=%d events=%d elapsed=%s", l.cfg.AgentID, l.cursor, next, len(events), elapsed.Round(time.Millisecond))
	}
	return events, next, err
}

func (l *Loop) runHeartbeat(ctx context.Context) {
	heartbeat := newTicker(heartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if l.isShuttingDown() {
				return
			}
			if err := l.register(ctx); err != nil {
				if l.shouldSuppressShutdownErr(err) {
					return
				}
				log.Printf("%s heartbeat register failed: %v", l.cfg.AgentID, err)
			}
		}
	}
}

// registerWithRetry retries the initial bus registration with capped backoff.
// Agents start at Windows logon before Tailscale DNS is ready; a one-shot
// initial register turns that race into a dead agent until the next logon
// (cause of the 2026-06/07 calendar pipeline outage).
func (l *Loop) registerWithRetry(ctx context.Context) error {
	backoff := time.Second
	for {
		err := l.register(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || l.shouldSuppressShutdownErr(err) {
			return err
		}
		log.Printf("%s initial register failed (retrying in %s): %v", l.cfg.AgentID, backoff, err)
		sleep(backoff)
		if ctx.Err() != nil {
			return err
		}
		if backoff *= 2; backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
	}
}

func (l *Loop) register(ctx context.Context) error {
	var err error
	if strings.TrimSpace(l.cfg.AgentClass) != "" || strings.TrimSpace(l.cfg.MutationClass) != "" {
		req := map[string]any{
			"agent_id":       l.cfg.AgentID,
			"secret":         l.cfg.Secret,
			"capabilities":   l.cfg.Capabilities,
			"description":    strings.TrimSpace(l.cfg.Description),
			"agent_class":    strings.TrimSpace(l.cfg.AgentClass),
			"mutation_class": strings.TrimSpace(l.cfg.MutationClass),
			"mode":           "pull",
			"ttl":            120,
		}
		body, _ := json.Marshal(req)
		_, _, err = l.client.DoJSON(ctx, http.MethodPost, "/v1/agents/register", body, nil)
	} else if strings.TrimSpace(l.cfg.Description) != "" {
		err = l.client.RegisterAgentWithDescription(ctx, l.cfg.AgentID, l.cfg.Secret, l.cfg.Capabilities, l.cfg.Description)
	} else {
		err = l.client.RegisterAgent(ctx, l.cfg.AgentID, l.cfg.Secret, l.cfg.Capabilities)
	}
	if err != nil {
		l.metricError()
		return err
	}
	if l.cfg.Metrics != nil {
		l.cfg.Metrics.IncCounter("register")
		l.cfg.Metrics.SetHealthy(true, "")
	}
	return nil
}

func (l *Loop) metricError() {
	if l.cfg.Metrics != nil {
		l.cfg.Metrics.IncCounter("errors")
	}
}

func (l *Loop) prioritize(events []busclient.InboxEvent) {
	if l.cfg.EventPriority == nil || len(events) < 2 {
		return
	}
	sort.SliceStable(events, func(i, j int) bool {
		return l.cfg.EventPriority(events[i]) < l.cfg.EventPriority(events[j])
	})
}

func (l *Loop) advanceCursor(next int) {
	l.cursor = next
	_ = saveCursor(l.cfg.CursorFile, next)
	if l.cfg.Metrics != nil {
		l.cfg.Metrics.SetGauge("bus_cursor", int64(next))
	}
}

func (l *Loop) beginShutdown(stopIntake context.CancelFunc) {
	l.shutdownOnce.Do(func() {
		stopIntake()

		l.mu.Lock()
		l.shuttingDown = true
		pollCancel := l.pollCancel
		stopOperations := l.stopOperations
		l.mu.Unlock()

		if l.cfg.Metrics != nil {
			l.cfg.Metrics.SetHealthy(false, "shutting down")
		}
		if pollCancel != nil {
			pollCancel()
		}
		if stopOperations != nil {
			afterFunc(l.cfg.ShutdownGrace, stopOperations)
		}
	})
}

func (l *Loop) isShuttingDown() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.shuttingDown
}

func (l *Loop) setPollCancel(cancel context.CancelFunc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pollCancel = cancel
}

func (l *Loop) clearPollCancel() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pollCancel = nil
}

func (l *Loop) setOperationCancel(cancel context.CancelFunc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopOperations = cancel
}

func (l *Loop) shouldSuppressShutdownErr(err error) bool {
	if !l.isShuttingDown() {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func loadCursor(path string) (int, bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, false, nil
	}
	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	cursor, err := strconv.Atoi(strings.TrimSpace(string(blob)))
	if err != nil {
		return 0, false, err
	}
	return cursor, true, nil
}

func saveCursor(path string, cursor int) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(cursor)+"\n"), 0o644)
}

func shutdownErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

func tickChan(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

func unauthorized(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status=401") || strings.Contains(msg, "status=403")
}
