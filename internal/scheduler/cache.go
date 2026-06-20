package scheduler

import (
	"strings"
	"sync"
	"time"
)

type replyCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]replyCacheEntry
}

type replyCacheEntry struct {
	expiresAt time.Time
	reply     Reply
}

func newReplyCache() *replyCache {
	return &replyCache{
		now:     time.Now,
		entries: make(map[string]replyCacheEntry),
	}
}

func (c *replyCache) Get(key string) (Reply, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Reply{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return Reply{}, false
	}
	if !c.now().Before(entry.expiresAt) {
		delete(c.entries, key)
		return Reply{}, false
	}
	return cloneReply(entry.reply), true
}

func (c *replyCache) Put(key string, reply Reply) {
	key = strings.TrimSpace(key)
	if key == "" || !reply.Terminal() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = replyCacheEntry{
		expiresAt: c.now().Add(24 * time.Hour),
		reply:     cloneReply(reply),
	}
}

func cloneReply(reply Reply) Reply {
	out := reply
	if reply.NearestAlternative != nil {
		alt := *reply.NearestAlternative
		out.NearestAlternative = &alt
	}
	return out
}

func canonicalKey(sender, requestID string) string {
	sender = strings.TrimSpace(sender)
	requestID = strings.TrimSpace(requestID)
	if sender == "" {
		return requestID
	}
	return sender + ":" + requestID
}
