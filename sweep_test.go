package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

type sweepFakeMem struct {
	nodes  map[string]contracts.Node
	writes int
	failOn string
}

func newSweepFakeMem() *sweepFakeMem { return &sweepFakeMem{nodes: make(map[string]contracts.Node)} }

func (f *sweepFakeMem) Recall(_ context.Context, key string, _ int) (contracts.Subgraph, error) {
	return contracts.Subgraph{Root: f.nodes[key]}, nil
}

func (f *sweepFakeMem) Record(_ context.Context, n contracts.Node) error {
	if n.Key == f.failOn {
		return errors.New("record failed")
	}
	f.writes++
	f.nodes[n.Key] = n
	return nil
}

func (f *sweepFakeMem) Search(_ context.Context, q contracts.Query) ([]contracts.Node, error) {
	var out []contracts.Node
	for _, n := range f.nodes {
		if n.Meta[contracts.MetaState] == contracts.StateArchived && !q.IncludeArchived {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}
func (f *sweepFakeMem) Links(_ context.Context, _, _, _ string) error { return nil }
func (f *sweepFakeMem) Unlink(context.Context, string, string) error  { return nil }
func (f *sweepFakeMem) Close() error                                  { return nil }

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

func TestSweepSkipsReportNodes(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	f := newSweepFakeMem()
	f.nodes["reports/old"] = contracts.Node{Key: "reports/old", Kind: ReportKind, Meta: map[string]string{
		contracts.MetaLastSeen: now.Add(-200 * 24 * time.Hour).Format(time.RFC3339),
	}}
	c := NewScoped(f, "s", contracts.MemoryScope{})
	c.now = func() time.Time { return now }
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.nodes["reports/old"].Meta[contracts.MetaState]; got != "" {
		t.Fatalf("report node was swept to %q, want left untouched", got)
	}
	if len(c.transitions) != 0 {
		t.Fatalf("sweeping a report appended %d transitions, want 0", len(c.transitions))
	}
}

func TestSweepNoChurn(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	f := newSweepFakeMem()
	seed(f, "fresh", 1, now)
	c := NewScoped(f, "s", contracts.MemoryScope{})
	c.now = func() time.Time { return now }
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.writes != 0 {
		t.Fatalf("unchanged node rewritten: writes=%d", f.writes)
	}
}

func TestSweepContinuesPastRecordError(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	f := newSweepFakeMem()
	seed(f, "bad", 45, now)
	seed(f, "ancient", 200, now)
	f.failOn = "bad"

	c := NewScoped(f, "s", contracts.MemoryScope{})
	c.now = func() time.Time { return now }
	err := c.Sweep(context.Background())
	if err == nil {
		t.Fatal("expected the failing node's error to be returned")
	}
	if got := f.nodes["ancient"].Meta[contracts.MetaState]; got != contracts.StateArchived {
		t.Fatalf("node after the failing one was skipped: ancient state = %q", got)
	}
}

func TestSweepReactivation(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	f := newSweepFakeMem()
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

func TestSweepPopulatesTransitionOnChange(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	f := newSweepFakeMem()
	seed(f, "old", 45, now)
	c := NewScoped(f, "s", contracts.MemoryScope{})
	c.now = func() time.Time { return now }
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(c.transitions))
	}
	tr := c.transitions[0]
	if tr.Key != "old" || tr.From != contracts.StateActive || tr.To != contracts.StateStale || tr.Kind != "sweep" {
		t.Fatalf("unexpected transition: %+v", tr)
	}
}

func TestSweepNoTransitionWhenUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	f := newSweepFakeMem()
	seed(f, "fresh", 1, now)
	c := NewScoped(f, "s", contracts.MemoryScope{})
	c.now = func() time.Time { return now }
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.transitions) != 0 {
		t.Fatalf("transitions = %d, want 0 for an unchanged node", len(c.transitions))
	}
}

func TestSweepWriteFailureProducesNoTransition(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	f := newSweepFakeMem()
	seed(f, "bad", 45, now)
	f.failOn = "bad"
	c := NewScoped(f, "s", contracts.MemoryScope{})
	c.now = func() time.Time { return now }
	if err := c.Sweep(context.Background()); err == nil {
		t.Fatal("expected the failing node's error to be returned")
	}
	if len(c.transitions) != 0 {
		t.Fatalf("transitions = %d, want 0: a failed write must not claim a transition", len(c.transitions))
	}
}
