// Package users provides per-API-key concurrency queueing. Each client
// credential identifies a user; each user gets a FIFO queue of requests
// bounded by a runtime-adjustable concurrency limit, so a single user can
// never push the upstream past its per-key limits. Users are identified
// only by the sha256 of their credential — plaintext keys never live
// here (config.Load hashes key: entries; the hot path hashes header
// values on the fly and keeps only the digest).
package users

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
)

// Errors reported by Acquire. QueueFull/QueueTimeout map to HTTP 429;
// context errors mean the client went away while queued.
var (
	ErrQueueFull    = errStr("queue full")
	ErrQueueTimeout = errStr("queue wait exceeded")
)

type errStr string

func (e errStr) Error() string { return string(e) }

// Registry resolves credentials to users and tracks the global queue
// depth. Safe for concurrent use.
type Registry struct {
	mu         sync.RWMutex
	byHash     map[string]*User
	defaultLim int
	maxQueue   int
	timeout    time.Duration
	depth      atomic.Int64 // live queued waiters across all users
}

// User is one credential's concurrency limiter. The zero-copy hot path is
// Acquire/Release; SetLimit adjusts the cap live.
type User struct {
	id   string // display prefix of the hash
	name string // from config; "" for auto-registered users
	hash string // full "sha256:<hex>"

	reg       *Registry
	maxQueue  int
	timeout   time.Duration
	createdAt time.Time

	mu       sync.Mutex
	limit    int
	inFlight int
	queue    []*waiter // FIFO; heads popped as they admit or drop out
	live     int       // waiters still waiting (not admitted, not canceled)

	requests atomic.Int64
	queued   atomic.Int64
	rejected atomic.Int64
	lastSeen atomic.Int64 // unix ms
}

// waiter is one queued Acquire. State transitions (waiting → admitted |
// dead) happen under the owning User's mutex.
type waiter struct {
	ready    chan struct{}
	admitted bool
	dead     bool
}

// NewRegistry builds the registry from the users config section. Call it
// only when users.enabled is set; a disabled section needs no registry.
// Configured entries pre-name users and may override their concurrency;
// every other key auto-registers at default concurrency on first sight.
func NewRegistry(cfg *config.Users) *Registry {
	r := &Registry{
		byHash:     make(map[string]*User, len(cfg.Keys)),
		defaultLim: cfg.DefaultConcurrency,
		maxQueue:   cfg.MaxQueue,
		timeout:    cfg.QueueTimeout,
	}
	for _, k := range cfg.Keys {
		limit := k.Concurrency
		if limit <= 0 {
			limit = cfg.DefaultConcurrency
		}
		u := r.newUser(k.KeyHash, k.Name, limit)
		r.byHash[k.KeyHash] = u
	}
	return r
}

func (r *Registry) newUser(hash, name string, limit int) *User {
	return &User{
		id:        strings.TrimPrefix(hash, "sha256:")[:16],
		name:      name,
		hash:      hash,
		reg:       r,
		maxQueue:  r.maxQueue,
		timeout:   r.timeout,
		limit:     limit,
		createdAt: time.Now(),
	}
}

// credentialOf picks the identifying header: Authorization (Bearer ...)
// first, then the API-key headers Anthropic-style clients send.
func credentialOf(h http.Header) string {
	if v := h.Get("Authorization"); v != "" {
		return v
	}
	if v := h.Get("X-Api-Key"); v != "" {
		return v
	}
	return h.Get("Api-Key")
}

// Identify resolves a request's credential to a user, auto-registering
// unknown keys at the default concurrency — every key queues, none get
// special treatment. Requests with no credential at all share one
// anonymous bucket (the upstream rejects them anyway; they churn fast).
func (r *Registry) Identify(h http.Header) *User {
	hash := config.HashCredential(credentialOf(h))
	r.mu.RLock()
	u := r.byHash[hash]
	r.mu.RUnlock()
	if u != nil {
		return u
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if u = r.byHash[hash]; u != nil {
		return u
	}
	u = r.newUser(hash, "", r.defaultLim)
	r.byHash[hash] = u
	return u
}

// Find locates a user for admin operations by hash prefix (as shown in
// /users output, ≥4 hex chars) or exact name. Returns nil when nothing
// matches.
func (r *Registry) Find(ref string) *User {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if len(ref) < 2 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, u := range r.byHash {
		if u.name != "" && u.name == ref {
			return u
		}
		if strings.HasPrefix(strings.TrimPrefix(u.hash, "sha256:"), ref) {
			return u
		}
	}
	return nil
}

// Stats is the admin/API view of one user.
type Stats struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Concurrency int       `json:"concurrency"`
	Active      int       `json:"active"`
	Queued      int       `json:"queued"`
	Requests    int64     `json:"requests_total"`
	QueuedTotal int64     `json:"queued_total"`
	Rejected    int64     `json:"rejected_total"`
	LastSeen    time.Time `json:"last_seen,omitempty"`
}

// Snapshot reads the user's current state.
func (u *User) Snapshot() Stats {
	u.mu.Lock()
	active, queued, limit := u.inFlight, u.live, u.limit
	u.mu.Unlock()
	last := u.lastSeen.Load()
	s := Stats{
		ID:          u.id,
		Name:        u.name,
		Concurrency: limit,
		Active:      active,
		Queued:      queued,
		Requests:    u.requests.Load(),
		QueuedTotal: u.queued.Load(),
		Rejected:    u.rejected.Load(),
	}
	if last > 0 {
		s.LastSeen = time.UnixMilli(last)
	}
	return s
}

// All returns every known user, auto-registered ones included, ordered
// by name then id for stable output.
func (r *Registry) All() []Stats {
	r.mu.RLock()
	out := make([]Stats, 0, len(r.byHash))
	for _, u := range r.byHash {
		out = append(out, u.Snapshot())
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Depth reports how many requests are waiting in queues right now
// (across all users) for the /metrics gauge.
func (r *Registry) Depth() int64 { return r.depth.Load() }

// SetLimit changes the concurrency cap live. Raising it immediately
// admits queued waiters; lowering it lets in-flight requests drain
// naturally (the new cap applies to admissions from now on).
func (u *User) SetLimit(n int) {
	u.mu.Lock()
	u.limit = n
	u.admitLocked()
	u.mu.Unlock()
}

// Acquire takes one of the user's concurrency slots, queueing FIFO until
// one frees. It returns the time spent waiting in the queue (0 when a
// slot was free immediately) and one of ErrQueueFull, ErrQueueTimeout,
// or the context's error when no slot could be taken. The client context
// canceling while queued aborts the wait cleanly.
func (u *User) Acquire(ctx context.Context) (time.Duration, error) {
	u.requests.Add(1)
	u.lastSeen.Store(time.Now().UnixMilli())

	u.mu.Lock()
	if u.maxQueue > 0 && u.live >= u.maxQueue {
		u.mu.Unlock()
		u.rejected.Add(1)
		return 0, ErrQueueFull
	}
	w := &waiter{ready: make(chan struct{})}
	u.queue = append(u.queue, w)
	u.live++
	u.reg.depth.Add(1)
	u.admitLocked()
	admitted := w.admitted
	u.mu.Unlock()

	if admitted { // fast path: a slot was free
		return 0, nil
	}
	u.queued.Add(1)

	start := time.Now()
	timer := time.NewTimer(u.timeout)
	defer timer.Stop()

	select {
	case <-w.ready:
		return time.Since(start), nil
	case <-ctx.Done():
		if u.abandon(w) {
			return 0, ctx.Err() // still queued: simply dropped
		}
		// Raced with an admission: we hold a slot but the client is gone.
		u.Release()
		return 0, ctx.Err()
	case <-timer.C:
		if !u.abandon(w) {
			// Raced with an admission: serve the request after all — it
			// was about to go out anyway.
			return time.Since(start), nil
		}
		u.rejected.Add(1)
		return 0, ErrQueueTimeout
	}
}

// abandon marks a waiter dead. It reports whether the waiter was still
// queued (true) or had already been admitted (false — the caller now
// holds a slot and must Release or serve it).
func (u *User) abandon(w *waiter) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if w.admitted {
		return false
	}
	if !w.dead {
		w.dead = true
		u.live--
		u.reg.depth.Add(-1)
	}
	return true
}

// Release returns a slot and immediately hands it to the queue head.
func (u *User) Release() {
	u.mu.Lock()
	u.inFlight--
	u.admitLocked()
	u.mu.Unlock()
}

// admitLocked grants slots to queued waiters in FIFO order while
// capacity lasts. Callers hold u.mu.
func (u *User) admitLocked() {
	for u.inFlight < u.limit {
		for len(u.queue) > 0 && (u.queue[0].admitted || u.queue[0].dead) {
			u.queue = u.queue[1:]
		}
		if len(u.queue) == 0 {
			return
		}
		w := u.queue[0]
		w.admitted = true
		close(w.ready)
		u.queue = u.queue[1:]
		u.inFlight++
		u.live--
		u.reg.depth.Add(-1)
	}
}
