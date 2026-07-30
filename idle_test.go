package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

// idleLearner builds a Learner with the G5 idle trigger configured. A nil
// extractor keeps Consolidate a clean no-op except for stamping lastRun.
func idleLearner(days, hours int) *Learner {
	l := NewLearner(nil, "s", contracts.MemoryScope{}, nil, "", 0)
	l.SetIdle(days, hours)
	return l
}

func TestSetIdle(t *testing.T) {
	l := idleLearner(7, 3)
	if l.idleDays != 7 || l.idleHours != 3 {
		t.Fatalf("SetIdle did not apply: idleDays=%d idleHours=%d", l.idleDays, l.idleHours)
	}
	l.SetIdle(0, 2) // 0 days disables
	if l.idleDays != 0 {
		t.Fatalf("SetIdle(0,..) must leave idleDays=0 (disabled), got %d", l.idleDays)
	}
}

func TestDueForIdleRun(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	cases := []struct {
		name         string
		idleDays     int
		idleHours    int
		lastRun      time.Time
		lastActivity time.Time
		want         bool
	}{
		{
			name:     "disabled (idleDays<=0) always false",
			idleDays: 0, idleHours: 2,
			lastRun:      now.Add(-30 * day),
			lastActivity: now.Add(-30 * day),
			want:         false,
		},
		{
			name:     "lastRun zero (never consolidated) false",
			idleDays: 7, idleHours: 2,
			lastRun:      time.Time{},
			lastActivity: now.Add(-30 * day),
			want:         false,
		},
		{
			name:     "sinceLastRun < idleDays false even if activity ancient",
			idleDays: 7, idleHours: 2,
			lastRun:      now.Add(-3 * day),
			lastActivity: now.Add(-30 * day),
			want:         false,
		},
		{
			name:     "idle < idleHours (recent turn) false",
			idleDays: 7, idleHours: 2,
			lastRun:      now.Add(-10 * day),
			lastActivity: now.Add(-1 * time.Hour),
			want:         false,
		},
		{
			name:     "both thresholds met true",
			idleDays: 7, idleHours: 2,
			lastRun:      now.Add(-10 * day),
			lastActivity: now.Add(-5 * time.Hour),
			want:         true,
		},
		{
			name:     "boundary equality inclusive (>=) true",
			idleDays: 7, idleHours: 2,
			lastRun:      now.Add(-7 * day),
			lastActivity: now.Add(-2 * time.Hour),
			want:         true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := idleLearner(c.idleDays, c.idleHours)
			l.lastRun = c.lastRun
			if got := l.DueForIdleRun(now, c.lastActivity); got != c.want {
				t.Errorf("DueForIdleRun = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeClock returns a time it can be advanced past for deterministic stamping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func TestConsolidateStampsLastRun(t *testing.T) {
	// nil extractor + nil mem: Consolidate is a no-op except for stamping lastRun.
	l := NewLearner(nil, "s", contracts.MemoryScope{}, nil, "", 0)
	clk := &fakeClock{t: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)}
	l.now = clk.now

	if !l.lastRun.IsZero() {
		t.Fatal("lastRun should start zero")
	}
	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate 1: %v", err)
	}
	first := l.lastRun
	if first != clk.t {
		t.Fatalf("lastRun not stamped on first Consolidate: got %v want %v", first, clk.t)
	}
	clk.add(3 * time.Hour) // a later manual/no-trigger call must re-stamp
	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate 2: %v", err)
	}
	if !l.lastRun.After(first) {
		t.Fatalf("lastRun did not advance across a second Consolidate: %v then %v", first, l.lastRun)
	}
}

func TestObserveStampsLastActivity(t *testing.T) {
	// nil mem: Curator.Observe is a no-op, cadence disabled (every=0), so Observe
	// only stamps lastActivity — no Consolidate fires.
	l := NewLearner(nil, "s", contracts.MemoryScope{}, nil, "", 0)
	clk := &fakeClock{t: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)}
	l.now = clk.now

	if err := l.Observe(context.Background(), contracts.Prompt{Author: "a", Content: "hi"}, "yo"); err != nil {
		t.Fatalf("Observe 1: %v", err)
	}
	first := l.lastActivity
	if first != clk.t {
		t.Fatalf("lastActivity not stamped: got %v want %v", first, clk.t)
	}
	clk.add(time.Hour)
	if err := l.Observe(context.Background(), contracts.Prompt{Author: "a", Content: "hi"}, "yo"); err != nil {
		t.Fatalf("Observe 2: %v", err)
	}
	if !l.lastActivity.After(first) {
		t.Fatalf("lastActivity did not advance across a second Observe: %v then %v", first, l.lastActivity)
	}
}

// TestConcurrentObserveAndConsolidate exercises the interleaving of a turn-path
// Observe (which may fire the cadence Consolidate) and a forced idle-triggered
// Consolidate from a second goroutine. Its acceptance bar is a clean `-race`
// run: no data race, no panic. Uses a real (empty) mergeMem so the full
// Consolidate body (extract/sweep/merge/promote/report) executes.
func TestConcurrentObserveAndConsolidate(t *testing.T) {
	mem := &mergeMem{}
	l := NewLearner(mem, "s", contracts.MemoryScope{}, plainExt{}, "", 1) // every=1: Observe fires Consolidate each turn
	l.SetIdle(1, 1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = l.Observe(context.Background(), contracts.Prompt{Author: "a", Content: "hi"}, "yo")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = l.Consolidate(context.Background()) // forced idle-style call
		}
	}()
	wg.Wait()
}

// countExt is an Extractor that counts Extract calls — a proxy for "Consolidate
// actually ran its body". Consolidate only calls Extract when both extract and
// mem are non-nil, so pair it with a non-nil mergeMem.
type countExt struct {
	mu sync.Mutex
	n  int
}

func (c *countExt) Extract(context.Context, string, string) ([]Candidate, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return nil, nil
}
func (c *countExt) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// idleTickLearner builds a Learner whose Consolidate body runs (non-nil mem +
// countExt) with the idle trigger configured and a fixed clock.
func idleTickLearner(days, hours int, now time.Time) (*Learner, *countExt) {
	ext := &countExt{}
	l := NewLearner(&mergeMem{}, "s", contracts.MemoryScope{}, ext, "", 0)
	l.SetIdle(days, hours)
	l.now = func() time.Time { return now }
	return l, ext
}

func TestIdleTickFiresWhenDue(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	l, ext := idleTickLearner(7, 2, now)
	l.lastRun = now.Add(-10 * 24 * time.Hour) // 10 days ago >= 7
	l.lastActivity = now.Add(-5 * time.Hour)  // 5h idle >= 2h
	l.idleTick(context.Background())
	if ext.count() != 1 {
		t.Fatalf("idleTick did not fire Consolidate once when due; extract calls=%d", ext.count())
	}
}

func TestIdleTickSkipsWhenNotDue(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	l, ext := idleTickLearner(7, 2, now)
	l.lastRun = now.Add(-10 * 24 * time.Hour)
	l.lastActivity = now.Add(-30 * time.Minute) // only 30m idle < 2h → not due
	l.idleTick(context.Background())
	if ext.count() != 0 {
		t.Fatalf("idleTick fired when not due; extract calls=%d", ext.count())
	}
}

func TestStartDisabledSpawnsNothing(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	l, ext := idleTickLearner(0, 2, now) // idleDays=0 → disabled
	l.lastRun = now.Add(-100 * 24 * time.Hour)
	l.lastActivity = now.Add(-100 * time.Hour)

	old := idlePollInterval
	idlePollInterval = time.Millisecond
	defer func() { idlePollInterval = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.Start(ctx)
	time.Sleep(30 * time.Millisecond) // several would-be ticks
	if ext.count() != 0 {
		t.Fatalf("disabled Start must never fire Consolidate; extract calls=%d", ext.count())
	}
}

func TestStartFiresWhenDueThenStopsOnCancel(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	l, ext := idleTickLearner(7, 2, now)
	l.lastRun = now.Add(-10 * 24 * time.Hour)
	l.lastActivity = now.Add(-5 * time.Hour)

	old := idlePollInterval
	idlePollInterval = time.Millisecond
	defer func() { idlePollInterval = old }()

	ctx, cancel := context.WithCancel(context.Background())
	l.Start(ctx)

	// Wait (bounded) for at least one idle-triggered Consolidate.
	deadline := time.Now().Add(2 * time.Second)
	for ext.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if ext.count() == 0 {
		t.Fatal("Start never fired an idle Consolidate when due")
	}

	cancel()
	time.Sleep(20 * time.Millisecond) // let the loop observe ctx.Done and return
	stopped := ext.count()
	time.Sleep(30 * time.Millisecond)
	if ext.count() != stopped {
		t.Fatalf("idle loop kept firing after ctx cancel: %d -> %d", stopped, ext.count())
	}
}
