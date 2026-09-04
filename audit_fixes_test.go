package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

type archiveAwareMem struct {
	live     []contracts.Node
	archived []contracts.Node
	records  []contracts.Node
}

func (m *archiveAwareMem) Record(_ context.Context, n contracts.Node) error {
	m.records = append(m.records, n)
	return nil
}
func (m *archiveAwareMem) Recall(_ context.Context, key string, _ int) (contracts.Subgraph, error) {
	return contracts.Subgraph{Root: contracts.Node{Key: key}}, nil
}
func (m *archiveAwareMem) Search(_ context.Context, q contracts.Query) ([]contracts.Node, error) {
	out := append([]contracts.Node{}, m.live...)
	if q.IncludeArchived {
		out = append(out, m.archived...)
	}
	return out, nil
}
func (m *archiveAwareMem) Links(context.Context, string, string, string) error { return nil }
func (m *archiveAwareMem) Unlink(context.Context, string, string) error        { return nil }
func (m *archiveAwareMem) Close() error                                        { return nil }

func TestMergeSkipsStructuralKinds(t *testing.T) {
	cases := []struct {
		name string
		kind contracts.NodeKind
		want bool
	}{
		{"report", ReportKind, false},
		{"session", contracts.KindSession, false},
		{"transcript", contracts.KindTranscript, false},
		{"decision", contracts.KindDecision, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := stale("k", "d")
			n.Kind = c.kind
			l := learnerWith(nil, plainExt{}, 2, 40, "stale")
			if got := l.mergeEligible(n); got != c.want {
				t.Fatalf("mergeEligible(%s) = %v, want %v", c.kind, got, c.want)
			}
		})
	}
}

func TestMergeNeverOffersStructuralKindsToMerger(t *testing.T) {
	report := stale("reports/1", "d")
	report.Kind = ReportKind
	session := stale("sessions/s", "d")
	session.Kind = contracts.KindSession
	raw := stale("raw/s/1-1", "d")
	raw.Kind = contracts.KindTranscript
	mem := &mergeMem{nodes: []contracts.Node{report, session, raw}}
	f := &fakeMerger{}
	l := learnerWith(mem, f, 2, 40, "stale")
	if err := l.Merge(context.Background()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("structural kinds must never reach the merger; got %d calls", len(f.calls))
	}
}

func TestMergeRejectsUmbrellaCollidingWithArchivedKey(t *testing.T) {
	archived := contracts.Node{Key: "facts/u", Body: "original archived body", Meta: map[string]string{
		contracts.MetaState: contracts.StateArchived,
	}}
	mem := &archiveAwareMem{
		live:     []contracts.Node{stale("facts/a", "d"), stale("facts/b", "d")},
		archived: []contracts.Node{archived},
	}
	f := &fakeMerger{result: []Umbrella{{
		Node:   contracts.Node{Key: "facts/u", Body: "umbrella"},
		Merged: []string{"facts/a", "facts/b"},
	}}}
	l := learnerWith(mem, f, 2, 40, "stale")
	if err := l.Merge(context.Background()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	for _, r := range mem.records {
		if r.Key == "facts/u" {
			t.Fatalf("umbrella overwrote the archived node at facts/u")
		}
	}
}

func TestValidUmbrellaRejectsDuplicateMergedKeys(t *testing.T) {
	byKey := map[string]contracts.Node{"facts/a": stale("facts/a", "d")}
	l := learnerWith(nil, plainExt{}, 2, 40, "stale")
	u := Umbrella{Node: contracts.Node{Key: "facts/u", Body: "b"}, Merged: []string{"facts/a", "facts/a"}}
	if l.validUmbrella(u, byKey, map[string]bool{}) {
		t.Fatal("an umbrella over one repeated original must be rejected")
	}
}

func TestPromoteSkipsCuratedSharedNode(t *testing.T) {
	orig := promoNode("agents/a/skills/x", contracts.StateActive, 20)
	orig.Body = "private note"
	mem := &curatedMem{mergeMem: mergeMem{nodes: []contracts.Node{orig}}, curated: map[string]contracts.Node{
		"projects/p/skills/x": {Key: "projects/p/skills/x", Body: "curated shared fact"},
	}}
	l := promoteLearner(mem, 10)
	if err := l.Promote(context.Background()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	for _, r := range mem.records {
		if r.Key == "projects/p/skills/x" {
			t.Fatalf("promotion overwrote the curated shared node")
		}
	}
}

func TestPromoteOverwritesItsOwnEarlierCopy(t *testing.T) {
	orig := promoNode("agents/a/skills/x", contracts.StateActive, 20)
	orig.Body = "private note"
	mem := &curatedMem{mergeMem: mergeMem{nodes: []contracts.Node{orig}}, curated: map[string]contracts.Node{
		"projects/p/skills/x": {Key: "projects/p/skills/x", Body: "older copy", Meta: map[string]string{
			MetaPromotedFrom: "agents/b/skills/x",
		}},
	}}
	l := promoteLearner(mem, 10)
	if err := l.Promote(context.Background()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	found := false
	for _, r := range mem.records {
		if r.Key == "projects/p/skills/x" {
			found = true
		}
	}
	if !found {
		t.Fatal("a previous promotion copy must still be upserted")
	}
}

type curatedMem struct {
	mergeMem
	curated map[string]contracts.Node
}

func (m *curatedMem) Recall(_ context.Context, key string, _ int) (contracts.Subgraph, error) {
	if n, ok := m.curated[key]; ok {
		return contracts.Subgraph{Root: n}, nil
	}
	return contracts.Subgraph{Root: contracts.Node{Key: key}}, nil
}

func TestFactKeyDistinguishesLongFactsSharingAHead(t *testing.T) {
	head := strings.Repeat("the studio tree map for the lobby place lives under ", 2)
	a := head + "Workspace/Lobby"
	b := head + "ServerStorage/Lobby"
	if factKey("projects/p", a) == factKey("projects/p", b) {
		t.Fatalf("two distinct facts collided on one key: %s", factKey("projects/p", a))
	}
	if factKey("projects/p", a) != factKey("projects/p", a) {
		t.Fatal("factKey must be deterministic")
	}
}

func TestLearnerCloseStopsIdleLoop(t *testing.T) {
	l := NewLearner(newSweepFakeMem(), "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	l.SetIdle(1, 1)
	l.Start(context.Background())
	done := l.idleDone
	if done == nil {
		t.Fatal("Start must record the idle loop's done channel")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not stop the idle loop")
	}
}

type lockedMem struct {
	mu    sync.Mutex
	nodes map[string]contracts.Node
}

func (m *lockedMem) Record(_ context.Context, n contracts.Node) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodes[n.Key] = n
	return nil
}
func (m *lockedMem) Recall(_ context.Context, key string, _ int) (contracts.Subgraph, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return contracts.Subgraph{Root: m.nodes[key]}, nil
}
func (m *lockedMem) Search(context.Context, contracts.Query) ([]contracts.Node, error) {
	return nil, nil
}
func (m *lockedMem) Links(context.Context, string, string, string) error { return nil }
func (m *lockedMem) Unlink(context.Context, string, string) error        { return nil }
func (m *lockedMem) Close() error                                        { return nil }

func TestRestoreTransitionAppendIsSynchronised(t *testing.T) {
	mem := &lockedMem{nodes: map[string]contracts.Node{}}
	for i := 0; i < 8; i++ {
		key := string(rune('a' + i))
		mem.nodes[key] = contracts.Node{Key: key}
	}
	l := NewLearner(mem, "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = l.Restore(context.Background(), string(rune('a'+i)))
		}(i)
	}
	wg.Wait()
	if len(l.transitions) != 8 {
		t.Fatalf("transitions = %d, want 8", len(l.transitions))
	}
}
