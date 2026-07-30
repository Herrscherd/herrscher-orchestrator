package orchestrator

import (
	"testing"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

// promoNode builds a private node with the given state and a capturedAt/lastSeen
// gap of ageDays (lastSeen = capturedAt + ageDays).
func promoNode(key, state string, ageDays int) contracts.Node {
	captured := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	last := captured.Add(time.Duration(ageDays) * 24 * time.Hour)
	m := map[string]string{
		"capturedAt":           captured.Format(time.RFC3339),
		contracts.MetaLastSeen: last.Format(time.RFC3339),
	}
	if state != "" {
		m[contracts.MetaState] = state
	}
	return contracts.Node{Key: key, Meta: m}
}

func TestPromoteEligible(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	l := NewLearner(nil, "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	l.SetPromote(10 * 24 * time.Hour) // 10-day bar

	cases := []struct {
		name string
		node contracts.Node
		want bool
	}{
		{"active old enough", promoNode("agents/a/skills/x", contracts.StateActive, 20), true},
		{"empty-state old enough", promoNode("agents/a/skills/x", "", 20), true},
		{"too young", promoNode("agents/a/skills/x", contracts.StateActive, 3), false},
		{"exactly at bar", promoNode("agents/a/skills/x", contracts.StateActive, 10), true},
		{"stale excluded", promoNode("agents/a/skills/x", contracts.StateStale, 20), false},
		{"archived excluded", promoNode("agents/a/skills/x", contracts.StateArchived, 20), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := l.promoteEligible(c.node, now); got != c.want {
				t.Errorf("promoteEligible = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPromoteEligibleTerminalLabels(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	l := NewLearner(nil, "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	l.SetPromote(10 * 24 * time.Hour)

	merged := promoNode("agents/a/skills/x", contracts.StateActive, 20)
	merged.Meta[MetaMergedInto] = "agents/a/u"
	if l.promoteEligible(merged, now) {
		t.Error("a merged-away node must never be eligible")
	}
	promoted := promoNode("agents/a/skills/x", contracts.StateActive, 20)
	promoted.Meta[MetaPromotedTo] = "projects/p/skills/x"
	if l.promoteEligible(promoted, now) {
		t.Error("an already-promoted node must never be re-eligible")
	}
}

func TestPromoteEligibleBadTimestamps(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	l := NewLearner(nil, "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	l.SetPromote(10 * 24 * time.Hour)

	noCaptured := contracts.Node{Key: "agents/a/x", Meta: map[string]string{
		contracts.MetaLastSeen: now.Format(time.RFC3339),
	}}
	if l.promoteEligible(noCaptured, now) {
		t.Error("missing capturedAt must fail eligibility, not panic")
	}
	badStamp := contracts.Node{Key: "agents/a/x", Meta: map[string]string{
		"capturedAt":           "not-a-time",
		contracts.MetaLastSeen: now.Format(time.RFC3339),
	}}
	if l.promoteEligible(badStamp, now) {
		t.Error("unparseable capturedAt must fail eligibility")
	}
}

func TestSetPromoteDisabledByDefault(t *testing.T) {
	l := NewLearner(nil, "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	if l.promoteMinAge != 0 {
		t.Fatalf("promoteMinAge = %v, want 0 (disabled by default)", l.promoteMinAge)
	}
}
