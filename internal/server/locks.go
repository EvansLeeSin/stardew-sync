package server

import (
	"sync"
	"time"

	"github.com/EvansLeeSin/stardew-sync/internal/api"
)

// Locks is an in-memory advisory play lock table keyed by save ID.
type Locks struct {
	mu    sync.Mutex
	ttl   time.Duration
	locks map[string]*api.Lock
	now   func() time.Time
}

func NewLocks(ttl time.Duration) *Locks {
	return &Locks{ttl: ttl, locks: map[string]*api.Lock{}, now: time.Now}
}

func (l *Locks) active(id string) *api.Lock {
	lk, ok := l.locks[id]
	if !ok {
		return nil
	}
	if l.now().After(lk.ExpiresAt) {
		delete(l.locks, id)
		return nil
	}
	return lk
}

// Get returns a copy of the active lock, or nil.
func (l *Locks) Get(id string) *api.Lock {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lk := l.active(id); lk != nil {
		c := *lk
		return &c
	}
	return nil
}

// Acquire acquires or renews the lock for device. If held by someone else
// and !force, returns (current, false).
func (l *Locks) Acquire(id, device string, force bool, gameDate string) (api.Lock, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().UTC()
	cur := l.active(id)
	if cur != nil && cur.Holder != device && !force {
		return *cur, false
	}
	if cur == nil || cur.Holder != device {
		cur = &api.Lock{Holder: device, Since: now}
		l.locks[id] = cur
	}
	cur.LastHeartbeat = now
	cur.ExpiresAt = now.Add(l.ttl)
	if gameDate != "" {
		cur.GameDate = gameDate
	}
	return *cur, true
}

// Release releases the lock if held by device. Returns false if held by another device.
func (l *Locks) Release(id, device string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur := l.active(id)
	if cur == nil {
		return true
	}
	if cur.Holder != device {
		return false
	}
	delete(l.locks, id)
	return true
}
