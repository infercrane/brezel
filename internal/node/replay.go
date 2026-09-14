package node

import (
	"container/heap"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	ErrCapabilityReplay = errors.New("operation capability was already used")
	ErrReplayCapacity   = errors.New("operation capability replay cache is full")
)

// ReplayCache consumes capability identifiers exactly once. It never evicts a
// live identifier to make room: saturation fails closed until an entry expires.
// The cache is intentionally process-local because every node boot has a new
// boot epoch and capabilities from an earlier process are invalid.
type ReplayCache struct {
	mu      sync.Mutex
	used    map[string]time.Time
	expires replayExpiryHeap
	maxLive int
	now     func() time.Time
}

func NewReplayCache(maxLive int) (*ReplayCache, error) {
	if maxLive < 1 {
		return nil, errors.New("replay cache capacity must be positive")
	}
	return &ReplayCache{used: make(map[string]time.Time), maxLive: maxLive, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Use atomically consumes jti until expiresAt. Call it only after signature and
// claim verification; malformed tokens must not consume capacity.
func (c *ReplayCache) Use(jti string, expiresAt time.Time) error {
	if c == nil || c.now == nil || c.maxLive < 1 || c.used == nil {
		return errors.New("replay cache is not initialized")
	}
	if strings.TrimSpace(jti) == "" || len(jti) > 256 || strings.ContainsAny(jti, "\x00\r\n") {
		return errors.New("invalid capability identifier")
	}
	now := c.now()
	if expiresAt.IsZero() || !now.Before(expiresAt) {
		return errors.New("capability is already expired")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.expires) > 0 && !now.Before(c.expires[0].expiresAt) {
		expired := heap.Pop(&c.expires).(replayExpiry)
		if recorded, exists := c.used[expired.jti]; exists && recorded.Equal(expired.expiresAt) {
			delete(c.used, expired.jti)
		}
	}
	if _, exists := c.used[jti]; exists {
		return ErrCapabilityReplay
	}
	if len(c.used) >= c.maxLive {
		return ErrReplayCapacity
	}
	c.used[jti] = expiresAt
	heap.Push(&c.expires, replayExpiry{jti: jti, expiresAt: expiresAt})
	return nil
}

func (c *ReplayCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.used)
}

type replayExpiry struct {
	jti       string
	expiresAt time.Time
}

type replayExpiryHeap []replayExpiry

func (h replayExpiryHeap) Len() int { return len(h) }
func (h replayExpiryHeap) Less(left, right int) bool {
	return h[left].expiresAt.Before(h[right].expiresAt)
}
func (h replayExpiryHeap) Swap(left, right int) { h[left], h[right] = h[right], h[left] }
func (h *replayExpiryHeap) Push(value any)      { *h = append(*h, value.(replayExpiry)) }
func (h *replayExpiryHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	old[last] = replayExpiry{}
	*h = old[:last]
	return value
}
