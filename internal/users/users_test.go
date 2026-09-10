package users

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
)

func ctxBg() context.Context { return context.Background() }

func TestAcquire_FillsLimitThenQueues(t *testing.T) {
	reg := NewRegistry(&config.Users{
		DefaultConcurrency: 2,
		MaxQueue:           10,
		QueueTimeout:       time.Minute,
	})
	u := reg.Identify(hdr("Bearer a"))
	if u == nil {
		t.Fatal("limit registry must return a user")
	}

	if _, err := u.Acquire(ctxBg()); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Acquire(ctxBg()); err != nil {
		t.Fatal(err)
	}
	if s := u.Snapshot(); s.Active != 2 || s.Queued != 0 {
		t.Fatalf("after 2 acquires: %+v", s)
	}

	done := make(chan time.Duration, 1)
	go func() {
		w, err := u.Acquire(ctxBg())
		if err != nil {
			t.Error(err)
		}
		done <- w
	}()
	deadline := time.After(2 * time.Second)
	for u.Snapshot().Queued != 1 {
		select {
		case <-deadline:
			t.Fatal("third acquire never queued")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	u.Release() // a slot frees; the queued request must take it
	select {
	case w := <-done:
		if w <= 0 {
			t.Fatalf("queued wait not reported: %v", w)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued acquire never admitted after release")
	}
	u.Release()
	u.Release() // one for the queued request, one of the first two
}

func hdr(auth string) http.Header {
	h := http.Header{}
	h.Set("Authorization", auth)
	return h
}

func TestAcquire_QueueFull(t *testing.T) {
	reg := NewRegistry(&config.Users{
		DefaultConcurrency: 1,
		MaxQueue:           1,
		QueueTimeout:       time.Minute,
	})
	user := reg.Identify(hdr("Bearer k"))
	if _, err := user.Acquire(ctxBg()); err != nil {
		t.Fatal(err)
	}
	// First queued request fits (max_queue 1).
	q1 := make(chan error, 1)
	go func() {
		_, err := user.Acquire(ctxBg())
		q1 <- err
	}()
	waitFor(t, func() bool { return user.Snapshot().Queued == 1 })
	// Second exceeds the queue: immediate rejection.
	if _, err := user.Acquire(ctxBg()); err != ErrQueueFull {
		t.Fatalf("want ErrQueueFull, got %v", err)
	}
	select {
	case err := <-q1:
		t.Fatalf("queued request failed prematurely: %v", err)
	default:
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition never became true")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestAcquire_QueueTimeout(t *testing.T) {
	reg := NewRegistry(&config.Users{
		DefaultConcurrency: 1,
		MaxQueue:           10,
		QueueTimeout:       50 * time.Millisecond,
	})
	u := reg.Identify(hdr("Bearer k"))
	if _, err := u.Acquire(ctxBg()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := u.Acquire(ctxBg()); err != ErrQueueTimeout {
		t.Fatalf("want ErrQueueTimeout, got %v", err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("timed out before the configured wait")
	}
	// The slot accounting must survive: after release, a fresh acquire works.
	u.Release()
	if _, err := u.Acquire(ctxBg()); err != nil {
		t.Fatal(err)
	}
	if d := reg.Depth(); d != 0 {
		t.Fatalf("depth after drain: %d", d)
	}
}

func TestAcquire_ClientCancelWhileQueued(t *testing.T) {
	reg := NewRegistry(&config.Users{
		DefaultConcurrency: 1,
		MaxQueue:           10,
		QueueTimeout:       time.Minute,
	})
	u := reg.Identify(hdr("Bearer k"))
	if _, err := u.Acquire(ctxBg()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := u.Acquire(ctx)
		errCh <- err
	}()
	waitFor(t, func() bool { return u.Snapshot().Queued == 1 })
	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled acquire never returned")
	}
	if s := u.Snapshot(); s.Queued != 0 {
		t.Fatalf("canceled waiter still queued: %+v", s)
	}
	// Slot handoff must skip the dead waiter: release admits nothing, then
	// a new acquire succeeds.
	u.Release()
	if _, err := u.Acquire(ctxBg()); err != nil {
		t.Fatal(err)
	}
	if d := reg.Depth(); d != 0 {
		t.Fatalf("depth after cancels: %d", d)
	}
}

func TestSetLimit_RaiseAdmitsLowerDrains(t *testing.T) {
	reg := NewRegistry(&config.Users{
		DefaultConcurrency: 1,
		MaxQueue:           10,
		QueueTimeout:       time.Minute,
	})
	u := reg.Identify(hdr("Bearer k"))
	if _, err := u.Acquire(ctxBg()); err != nil {
		t.Fatal(err)
	}

	admitted := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			if _, err := u.Acquire(ctxBg()); err != nil {
				t.Error(err)
			}
			admitted <- struct{}{}
		}()
	}
	waitFor(t, func() bool { return u.Snapshot().Queued == 2 })

	u.SetLimit(3) // raise: both queued must be admitted immediately
	for i := 0; i < 2; i++ {
		select {
		case <-admitted:
		case <-time.After(2 * time.Second):
			t.Fatal("raise did not admit queued waiters")
		}
	}
	if s := u.Snapshot(); s.Active != 3 || s.Queued != 0 {
		t.Fatalf("after raise: %+v", s)
	}

	u.SetLimit(1) // lower: in-flight drains naturally, no preemption
	if s := u.Snapshot(); s.Active != 3 {
		t.Fatalf("lowering must not preempt: %+v", s)
	}
	u.Release()
	u.Release()
	u.Release()
	if s := u.Snapshot(); s.Active != 0 {
		t.Fatalf("after drain: %+v", s)
	}
}

func TestIdentify_HeaderPriorityAndAutoRegister(t *testing.T) {
	secret := "Bearer sk-live-xyz"
	reg := NewRegistry(&config.Users{
		DefaultConcurrency: 5,
		MaxQueue:           10,
		QueueTimeout:       time.Minute,
		Keys: []config.UserKey{{
			Name:    "dean",
			KeyHash: config.HashCredential(secret),
		}},
	})

	// Unknown keys are NOT special: they auto-register at the default
	// concurrency like everyone else.
	unknown := reg.Identify(hdr("Bearer other"))
	if unknown == nil {
		t.Fatal("unknown key must auto-register as a user")
	}
	if s := unknown.Snapshot(); s.Concurrency != 5 || s.Name != "" {
		t.Fatalf("auto-registered user wrong: %+v", s)
	}
	if again := reg.Identify(hdr("Bearer other")); again != unknown {
		t.Fatal("auto-registration is not idempotent")
	}

	u := reg.Identify(hdr(secret))
	if u == nil || u.name != "dean" {
		t.Fatalf("registered key not identified: %+v", u)
	}
	if s := u.Snapshot(); s.Concurrency != 5 {
		t.Fatalf("default concurrency not applied: %+v", s)
	}

	// x-api-key and api-key identify too.
	h := http.Header{}
	h.Set("X-Api-Key", "anthropic-style")
	r2 := NewRegistry(&config.Users{
		DefaultConcurrency: 3,
		MaxQueue:           10,
		QueueTimeout:       time.Minute,
	})
	if u := r2.Identify(h); u == nil {
		t.Fatal("x-api-key not identified")
	}
	h2 := http.Header{}
	h2.Set("Api-Key", "azure-style")
	if u := r2.Identify(h2); u == nil {
		t.Fatal("api-key not identified")
	}

	// Authorization wins over x-api-key when both are present.
	h3 := http.Header{}
	h3.Set("Authorization", "Bearer primary")
	h3.Set("X-Api-Key", "secondary")
	reg3 := NewRegistry(&config.Users{
		DefaultConcurrency: 3,
		MaxQueue:           10,
		QueueTimeout:       time.Minute,
		Keys: []config.UserKey{
			{Name: "auth", KeyHash: config.HashCredential("Bearer primary")},
		},
	})
	if u := reg3.Identify(h3); u == nil || u.name != "auth" {
		t.Fatalf("Authorization must take priority: %+v", u)
	}

	// Find matches by name and by hash prefix.
	if reg.Find("dean") == nil {
		t.Fatal("find by name failed")
	}
	prefix := strings.TrimPrefix(config.HashCredential(secret), "sha256:")[:8]
	if reg.Find(prefix) == nil {
		t.Fatal("find by hash prefix failed")
	}
	if reg.Find("nobody") != nil {
		t.Fatal("find matched a stranger")
	}
}

// All registered keys share one bucket per key; concurrency spread over
// many keys must not serialize each other.
func TestAcquire_UsersAreIndependent(t *testing.T) {
	reg := NewRegistry(&config.Users{
		DefaultConcurrency: 1,
		MaxQueue:           10,
		QueueTimeout:       time.Minute,
	})
	for i := 0; i < 20; i++ {
		u := reg.Identify(hdr(fmt.Sprintf("Bearer key-%d", i)))
		if _, err := u.Acquire(ctxBg()); err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	if d := reg.Depth(); d != 0 {
		t.Fatalf("depth: %d", d)
	}
}

// The admit/cancel race: a release and a context cancellation firing at
// once must neither leak a slot nor strand a waiter.
func TestAcquire_RaceAdmitVsCancel(t *testing.T) {
	reg := NewRegistry(&config.Users{
		DefaultConcurrency: 1,
		MaxQueue:           10,
		QueueTimeout:       time.Minute,
	})
	u := reg.Identify(hdr("Bearer k"))
	if _, err := u.Acquire(ctxBg()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(ctxBg())
			go cancel()
			if _, err := u.Acquire(ctx); err != nil {
				if err != context.Canceled && err != ErrQueueTimeout && err != ErrQueueFull {
					t.Error(err)
				}
				return
			}
			u.Release()
		}()
	}
	wg.Wait()

	// Drain: eventually the limiter must be usable again and consistent.
	time.Sleep(10 * time.Millisecond)
	u.Release()
	if _, err := u.Acquire(ctxBg()); err != nil {
		t.Fatal(err)
	}
	s := u.Snapshot()
	if s.Active < 1 {
		t.Fatalf("slot leaked: %+v", s)
	}
}

// Hot-path costs, for the README's queueing overhead numbers.
func BenchmarkAcquireRelease(b *testing.B) {
	reg := NewRegistry(&config.Users{
		DefaultConcurrency: 90,
		MaxQueue:           1000,
		QueueTimeout:       time.Minute,
	})
	u := reg.Identify(hdr("Bearer bench"))
	ctx := context.Background()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := u.Acquire(ctx); err != nil {
				b.Fatal(err)
			}
			u.Release()
		}
	})
}

func BenchmarkIdentify(b *testing.B) {
	reg := NewRegistry(&config.Users{
		DefaultConcurrency: 90,
		MaxQueue:           1000,
		QueueTimeout:       time.Minute,
		Keys:               []config.UserKey{{Name: "bench", KeyHash: config.HashCredential("Bearer bench")}},
	})
	h := hdr("Bearer bench")
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if reg.Identify(h) == nil {
				b.Fatal("identify failed")
			}
		}
	})
}
