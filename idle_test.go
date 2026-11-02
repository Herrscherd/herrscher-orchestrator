package orchestrator

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

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
	l.SetIdle(0, 2)
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

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func TestConsolidateStampsLastRun(t *testing.T) {
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
	clk.add(3 * time.Hour)
	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate 2: %v", err)
	}
	if !l.lastRun.After(first) {
		t.Fatalf("lastRun did not advance across a second Consolidate: %v then %v", first, l.lastRun)
	}
}

func TestObserveStampsLastActivity(t *testing.T) {
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

func TestConcurrentObserveAndConsolidateIsRaceFree(t *testing.T) {
	mem := &mergeMem{}
	l := NewLearner(mem, "s", contracts.MemoryScope{}, plainExt{}, "", 1)
	l.SetIdle(1, 1)

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 400)
	go func() {
		defer wg.Done()
		for range 200 {
			if err := l.Observe(context.Background(), contracts.Prompt{Author: "a", Content: "hi"}, "yo"); err != nil {
				errs <- err
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			if err := l.Consolidate(context.Background()); err != nil {
				errs <- err
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

type blockExt struct {
	entered chan struct{}
	release chan struct{}
}

func (b blockExt) Extract(context.Context, string, string) ([]Candidate, error) {
	close(b.entered)
	<-b.release
	return nil, nil
}

func TestObserveDoesNotBlockDuringIdleConsolidate(t *testing.T) {
	ext := blockExt{entered: make(chan struct{}), release: make(chan struct{})}
	l := NewLearner(&mergeMem{}, "s", contracts.MemoryScope{}, ext, "", 0)

	consolidated := make(chan error, 1)
	go func() { consolidated <- l.Consolidate(context.Background()) }()
	<-ext.entered

	observed := make(chan error, 1)
	go func() {
		observed <- l.Observe(context.Background(), contracts.Prompt{Author: "a", Content: "hi"}, "yo")
	}()

	select {
	case err := <-observed:
		close(ext.release)
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(ext.release)
		t.Fatal("Observe blocked while an idle Consolidate held mu (invariant 2 violation)")
	}
	if err := <-consolidated; err != nil {
		t.Fatal(err)
	}
}

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

func idleTickLearner(days int, now time.Time) (*Learner, *countExt) {
	ext := &countExt{}
	l := NewLearner(&mergeMem{}, "s", contracts.MemoryScope{}, ext, "", 0)
	l.SetIdle(days, 2)
	l.now = func() time.Time { return now }
	return l, ext
}

func TestIdleTickFiresWhenDue(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	l, ext := idleTickLearner(7, now)
	l.lastRun = now.Add(-10 * 24 * time.Hour)
	l.lastActivity = now.Add(-5 * time.Hour)
	l.idleTick(context.Background())
	if ext.count() != 1 {
		t.Fatalf("idleTick did not fire Consolidate once when due; extract calls=%d", ext.count())
	}
}

func TestIdleTickSkipsWhenNotDue(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	l, ext := idleTickLearner(7, now)
	l.lastRun = now.Add(-10 * 24 * time.Hour)
	l.lastActivity = now.Add(-30 * time.Minute)
	l.idleTick(context.Background())
	if ext.count() != 0 {
		t.Fatalf("idleTick fired when not due; extract calls=%d", ext.count())
	}
}

func TestStartDisabledSpawnsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		l, ext := idleTickLearner(0, now)
		l.lastRun = now.Add(-100 * 24 * time.Hour)
		l.lastActivity = now.Add(-100 * time.Hour)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		l.Start(ctx)
		synctest.Sleep(30 * time.Millisecond)
		if ext.count() != 0 {
			t.Fatalf("disabled Start must never fire Consolidate; extract calls=%d", ext.count())
		}
	})
}

func TestStartFiresWhenDueThenStopsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		l, ext := idleTickLearner(7, now)
		l.lastRun = now.Add(-10 * 24 * time.Hour)
		l.lastActivity = now.Add(-5 * time.Hour)

		ctx, cancel := context.WithCancel(context.Background())
		l.Start(ctx)

		synctest.Sleep(defaultIdlePoll + time.Second)
		if ext.count() == 0 {
			t.Fatal("Start never fired an idle Consolidate when due")
		}

		cancel()
		synctest.Wait()
		stopped := ext.count()
		synctest.Sleep(30 * time.Millisecond)
		if ext.count() != stopped {
			t.Fatalf("idle loop kept firing after ctx cancel: %d -> %d", stopped, ext.count())
		}
	})
}
