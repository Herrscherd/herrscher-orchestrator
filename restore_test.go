package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

// restoreMem is a minimal fake Memory dedicated to Restore's tests: unlike
// mergeMem/sweepFakeMem (which return a zero Node with no error on a missing
// key), Recall here models the real contract Restore relies on — an absent
// key is a genuine error, not a silent zero value.
type restoreMem struct {
	nodes   map[string]contracts.Node
	records []contracts.Node
}

func newRestoreMem() *restoreMem { return &restoreMem{nodes: map[string]contracts.Node{}} }

func (m *restoreMem) Recall(_ context.Context, key string, _ int) (contracts.Subgraph, error) {
	n, ok := m.nodes[key]
	if !ok {
		return contracts.Subgraph{}, fmt.Errorf("restoreMem: no node at key %q", key)
	}
	return contracts.Subgraph{Root: n}, nil
}
func (m *restoreMem) Record(_ context.Context, n contracts.Node) error {
	m.nodes[n.Key] = n
	m.records = append(m.records, n)
	return nil
}
func (m *restoreMem) Search(context.Context, contracts.Query) ([]contracts.Node, error) {
	return nil, nil
}
func (m *restoreMem) Links(context.Context, string, string, string) error { return nil }
func (m *restoreMem) Close() error                                        { return nil }

func TestRestoreHappyPath(t *testing.T) {
	m := newRestoreMem()
	m.nodes["facts/a"] = contracts.Node{Key: "facts/a", Meta: map[string]string{
		contracts.MetaState:    contracts.StateArchived,
		contracts.MetaLastSeen: "2020-01-01T00:00:00Z",
	}}
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	prior, err := Restore(context.Background(), m, "facts/a", withClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if prior != contracts.StateArchived {
		t.Errorf("prior = %q, want archived", prior)
	}
	got := m.nodes["facts/a"]
	if got.Meta[contracts.MetaState] != contracts.StateActive {
		t.Errorf("state = %q, want active", got.Meta[contracts.MetaState])
	}
	if want := now.UTC().Format(time.RFC3339); got.Meta[contracts.MetaLastSeen] != want {
		t.Errorf("lastSeen = %q, want %q", got.Meta[contracts.MetaLastSeen], want)
	}
}

func TestRestoreRefusesMergedOriginal(t *testing.T) {
	m := newRestoreMem()
	m.nodes["facts/a"] = contracts.Node{Key: "facts/a", Meta: map[string]string{
		MetaMergedInto:         "facts/u",
		contracts.MetaState:    contracts.StateArchived,
		contracts.MetaLastSeen: "2020-01-01T00:00:00Z",
	}}
	_, err := Restore(context.Background(), m, "facts/a")
	if !errors.Is(err, ErrMergedOriginal) {
		t.Fatalf("err = %v, want ErrMergedOriginal", err)
	}
	if len(m.records) != 0 {
		t.Fatalf("a refused restore must not write; got %d records", len(m.records))
	}
	got := m.nodes["facts/a"]
	if got.Meta[contracts.MetaState] != contracts.StateArchived {
		t.Errorf("node was mutated despite refusal: state=%q", got.Meta[contracts.MetaState])
	}
}

func TestRestoreWithForceDetaches(t *testing.T) {
	m := newRestoreMem()
	m.nodes["facts/a"] = contracts.Node{Key: "facts/a", Meta: map[string]string{
		MetaMergedInto:         "facts/u",
		contracts.MetaState:    contracts.StateArchived,
		contracts.MetaLastSeen: "2020-01-01T00:00:00Z",
	}}
	if _, err := Restore(context.Background(), m, "facts/a", Force(true)); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got := m.nodes["facts/a"]
	if got.Meta[contracts.MetaState] != contracts.StateActive {
		t.Errorf("state = %q, want active", got.Meta[contracts.MetaState])
	}
	if got.Meta[MetaMergedInto] != "" {
		t.Errorf("mergedInto = %q, want cleared", got.Meta[MetaMergedInto])
	}
}

func TestRestoreAbsentKeyErrors(t *testing.T) {
	m := newRestoreMem()
	if _, err := Restore(context.Background(), m, "nope"); err == nil {
		t.Fatal("expected an error restoring an absent key")
	}
	if len(m.records) != 0 {
		t.Fatalf("an absent-key restore must not write; got %d records", len(m.records))
	}
}

func TestLearnerRestoreAppendsTransition(t *testing.T) {
	m := newRestoreMem()
	m.nodes["facts/a"] = contracts.Node{Key: "facts/a", Meta: map[string]string{
		contracts.MetaState:    contracts.StateArchived,
		contracts.MetaLastSeen: "2020-01-01T00:00:00Z",
	}}
	l := NewLearner(m, "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	if err := l.Restore(context.Background(), "facts/a"); err != nil {
		t.Fatalf("Learner.Restore: %v", err)
	}
	if len(l.transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(l.transitions))
	}
	tr := l.transitions[0]
	if tr.Key != "facts/a" || tr.Kind != "restore" || tr.To != contracts.StateActive {
		t.Fatalf("unexpected transition: %+v", tr)
	}
	if tr.From != contracts.StateArchived {
		t.Errorf("From = %q, want the node's real prior state archived", tr.From)
	}
}

// TestLearnerRestoreRecordsRealPriorState proves the audit From is the node's
// observed prior state, not a hardcoded "archived": restoring a merely-stale
// node must record From=stale.
func TestLearnerRestoreRecordsRealPriorState(t *testing.T) {
	m := newRestoreMem()
	m.nodes["facts/a"] = contracts.Node{Key: "facts/a", Meta: map[string]string{
		contracts.MetaState:    contracts.StateStale,
		contracts.MetaLastSeen: "2020-01-01T00:00:00Z",
	}}
	l := NewLearner(m, "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	if err := l.Restore(context.Background(), "facts/a"); err != nil {
		t.Fatalf("Learner.Restore: %v", err)
	}
	if l.transitions[0].From != contracts.StateStale {
		t.Errorf("From = %q, want stale", l.transitions[0].From)
	}
}
