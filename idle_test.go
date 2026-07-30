package orchestrator

import (
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
