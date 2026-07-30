package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

type sweepFakeMem struct {
	nodes  map[string]contracts.Node
	writes int
}

func newSweepFakeMem() *sweepFakeMem { return &sweepFakeMem{nodes: map[string]contracts.Node{}} }

func (f *sweepFakeMem) Recall(ctx context.Context, key string, depth int) (contracts.Subgraph, error) {
	return contracts.Subgraph{Root: f.nodes[key]}, nil
}
func (f *sweepFakeMem) Record(ctx context.Context, n contracts.Node) error {
	f.writes++
	f.nodes[n.Key] = n
	return nil
}
func (f *sweepFakeMem) Search(ctx context.Context, q contracts.Query) ([]contracts.Node, error) {
	var out []contracts.Node
	for _, n := range f.nodes {
		if n.Meta[contracts.MetaState] == contracts.StateArchived && !q.IncludeArchived {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}
func (f *sweepFakeMem) Links(ctx context.Context, from, to, rel string) error { return nil }
func (f *sweepFakeMem) Close() error                                          { return nil }

func seed(f *sweepFakeMem, key string, ageDays int, now time.Time) {
	f.nodes[key] = contracts.Node{Key: key, Meta: map[string]string{
		contracts.MetaLastSeen: now.Add(-time.Duration(ageDays) * 24 * time.Hour).Format(time.RFC3339),
	}}
}

func TestSweepTransitions(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	f := newSweepFakeMem()
	seed(f, "fresh", 1, now)
	seed(f, "old", 45, now)
	seed(f, "ancient", 200, now)

	c := NewScoped(f, "s", contracts.MemoryScope{})
	c.now = func() time.Time { return now }
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	// "fresh" is already active (absent state == active per NextState) and must
	// not be churned into an explicit "active" write — see TestSweepNoChurn.
	want := map[string]string{"old": contracts.StateStale, "ancient": contracts.StateArchived}
	for k, w := range want {
		if got := f.nodes[k].Meta[contracts.MetaState]; got != w {
			t.Fatalf("%s state = %q, want %q", k, got, w)
		}
	}
	if got := f.nodes["fresh"].Meta[contracts.MetaState]; got != "" {
		t.Fatalf("fresh state = %q, want unwritten (no churn)", got)
	}
}

func TestSweepNoChurn(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	f := newSweepFakeMem()
	seed(f, "fresh", 1, now) // already active (absent state == active)
	c := NewScoped(f, "s", contracts.MemoryScope{})
	c.now = func() time.Time { return now }
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.writes != 0 {
		t.Fatalf("unchanged node rewritten: writes=%d", f.writes)
	}
}

func TestSweepReactivation(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	f := newSweepFakeMem()
	// A node that was archived but re-observed (fresh lastSeen) returns to active.
	f.nodes["r"] = contracts.Node{Key: "r", Meta: map[string]string{
		contracts.MetaState:    contracts.StateArchived,
		contracts.MetaLastSeen: now.Add(-time.Hour).Format(time.RFC3339),
	}}
	c := NewScoped(f, "s", contracts.MemoryScope{})
	c.now = func() time.Time { return now }
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.nodes["r"].Meta[contracts.MetaState]; got != contracts.StateActive {
		t.Fatalf("reactivation failed: state = %q", got)
	}
	if got := f.nodes["r"].Meta[contracts.MetaLastSeen]; got != now.Add(-time.Hour).Format(time.RFC3339) {
		t.Fatalf("lastSeen disturbed by state write: %q", got)
	}
}
