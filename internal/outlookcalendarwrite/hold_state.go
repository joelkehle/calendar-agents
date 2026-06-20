package outlookcalendarwrite

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/joelkehle/pinakes/pkg/busclient"
)

func holdIdempotencyKey(evt busclient.InboxEvent) string {
	requestID := strings.TrimSpace(metaString(evt.Meta, "request_id"))
	if requestID != "" {
		sender := strings.TrimSpace(evt.From)
		if sender == "" {
			return requestID
		}
		return sender + "/" + requestID
	}
	if requestID == "" {
		requestID = strings.TrimSpace(evt.MessageID)
	}
	if requestID == "" {
		return ""
	}
	conversationID := strings.TrimSpace(evt.ConversationID)
	if conversationID == "" {
		return requestID
	}
	return conversationID + "/" + requestID
}

// travelIdempotencyKey mirrors holdIdempotencyKey but class/action-qualifies
// the key (action is "event-insert" or "event-patch") so a request_id reused
// across classes can never replay the other class's cached response.
func travelIdempotencyKey(evt busclient.InboxEvent, action string) string {
	qualifier := strings.TrimSpace(action) + "/travel-block/"
	requestID := strings.TrimSpace(metaString(evt.Meta, "request_id"))
	if requestID != "" {
		sender := strings.TrimSpace(evt.From)
		if sender == "" {
			return qualifier + requestID
		}
		return sender + "/" + qualifier + requestID
	}
	requestID = strings.TrimSpace(evt.MessageID)
	if requestID == "" {
		return ""
	}
	conversationID := strings.TrimSpace(evt.ConversationID)
	if conversationID == "" {
		return qualifier + requestID
	}
	return conversationID + "/" + qualifier + requestID
}

func metaString(meta any, key string) string {
	switch value := meta.(type) {
	case map[string]any:
		if raw, ok := value[key]; ok {
			return fmt.Sprint(raw)
		}
	case map[string]string:
		return value[key]
	}
	if meta == nil {
		return ""
	}
	blob, err := json.Marshal(meta)
	if err != nil {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal(blob, &decoded); err != nil {
		return ""
	}
	if raw, ok := decoded[key]; ok {
		return fmt.Sprint(raw)
	}
	return ""
}

func storedEventLocalDate(event StoredEvent) string {
	start, err := parseDateTime(event.Start.DateTime)
	if err != nil {
		return ""
	}
	location, err := time.LoadLocation(DefaultTimeZone)
	if err != nil {
		return ""
	}
	return start.In(location).Format("2006-01-02")
}

type holdResponseCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]holdResponseCacheEntry
}

type holdResponseCacheEntry struct {
	expiresAt time.Time
	response  MutationResponse
}

func newHoldResponseCache() *holdResponseCache {
	return &holdResponseCache{
		now:     time.Now,
		entries: make(map[string]holdResponseCacheEntry),
	}
}

func (c *holdResponseCache) Get(key string) (MutationResponse, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return MutationResponse{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return MutationResponse{}, false
	}
	if !c.now().Before(entry.expiresAt) {
		delete(c.entries, key)
		return MutationResponse{}, false
	}
	return cloneMutationResponse(entry.response), true
}

func (c *holdResponseCache) Put(key string, response MutationResponse) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = holdResponseCacheEntry{
		expiresAt: c.now().Add(24 * time.Hour),
		response:  cloneMutationResponse(response),
	}
	// Entries are otherwise only lazily evicted on Get of the same key;
	// sweep expired entries so dry-run traffic with unique request ids
	// cannot grow writer memory without bound.
	if len(c.entries) > maxResponseCacheEntriesBeforeSweep {
		now := c.now()
		for existingKey, entry := range c.entries {
			if !now.Before(entry.expiresAt) {
				delete(c.entries, existingKey)
			}
		}
	}
}

const maxResponseCacheEntriesBeforeSweep = 1024

func cloneMutationResponse(response MutationResponse) MutationResponse {
	out := response
	if response.Event != nil {
		event := *response.Event
		out.Event = &event
	}
	if response.WouldWrite != nil {
		event := *response.WouldWrite
		out.WouldWrite = &event
	}
	return out
}

const (
	maxHoldInsertsPerRequesterPerDate = 2
	maxHoldInsertsPerDate             = 5

	maxTravelInsertsPerRequesterPerDate = 8
	maxTravelInsertsPerDate             = 20
)

type holdRateLimiter struct {
	mu           sync.Mutex
	perRequester int
	perDate      int
	counts       map[string]*holdDateCounts
}

type holdDateCounts struct {
	total       int
	byRequester map[string]int
}

func newRateLimiter(perRequester, perDate int) *holdRateLimiter {
	return &holdRateLimiter{
		perRequester: perRequester,
		perDate:      perDate,
		counts:       make(map[string]*holdDateCounts),
	}
}

func newHoldRateLimiter() *holdRateLimiter {
	return newRateLimiter(maxHoldInsertsPerRequesterPerDate, maxHoldInsertsPerDate)
}

func newTravelRateLimiter() *holdRateLimiter {
	return newRateLimiter(maxTravelInsertsPerRequesterPerDate, maxTravelInsertsPerDate)
}

func (l *holdRateLimiter) Allow(requester, eventDate string) bool {
	requester = strings.TrimSpace(requester)
	eventDate = strings.TrimSpace(eventDate)
	if requester == "" || eventDate == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	counts := l.counts[eventDate]
	if counts == nil {
		return true
	}
	return counts.total < l.perDate && counts.byRequester[requester] < l.perRequester
}

// Release returns one unit of budget for the requester + event local date
// (decrement, floor 0). Called on every LIVE successful travel cancel-patch
// (SCHEDULER_TRAVEL_SPEC §3.6) so failed-booking compensation and watcher
// orphan cancels cannot permanently starve a date's travel protection.
func (l *holdRateLimiter) Release(requester, eventDate string) {
	requester = strings.TrimSpace(requester)
	eventDate = strings.TrimSpace(eventDate)
	if requester == "" || eventDate == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	counts := l.counts[eventDate]
	if counts == nil {
		return
	}
	if counts.total > 0 {
		counts.total--
	}
	if counts.byRequester[requester] > 0 {
		counts.byRequester[requester]--
	}
}

func (l *holdRateLimiter) Record(requester, eventDate string) {
	requester = strings.TrimSpace(requester)
	eventDate = strings.TrimSpace(eventDate)
	if requester == "" || eventDate == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	counts := l.counts[eventDate]
	if counts == nil {
		counts = &holdDateCounts{byRequester: make(map[string]int)}
		l.counts[eventDate] = counts
	}
	counts.total++
	counts.byRequester[requester]++
}
