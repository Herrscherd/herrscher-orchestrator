package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Herrscherd/herrscher-contracts"
)

// recMem records nodes and links so we can assert what the learner persisted.
type recMem struct {
	nodes  map[string]contracts.Node
	links  [][3]string // {from, to, rel}
	failOn string      // key whose Record returns an error (node not stored)
}

func newRec() *recMem { return &recMem{nodes: map[string]contracts.Node{}} }

func (m *recMem) Record(_ context.Context, n contracts.Node) error {
	if n.Key == m.failOn {
		return errors.New("record failed")
	}
	m.nodes[n.Key] = n
	return nil
}
func (m *recMem) Recall(_ context.Context, key string, _ int) (contracts.Subgraph, error) {
	if n, ok := m.nodes[key]; ok {
		return contracts.Subgraph{Root: n}, nil
	}
	return contracts.Subgraph{Root: contracts.Node{Key: key}}, nil
}
func (m *recMem) Search(context.Context, contracts.Query) ([]contracts.Node, error) { return nil, nil }
func (m *recMem) Links(_ context.Context, from, to, rel string) error {
	m.links = append(m.links, [3]string{from, to, rel})
	return nil
}
func (m *recMem) Close() error { return nil }

func (m *recMem) hasLink(from, to string) bool {
	for _, l := range m.links {
		if l[0] == from && l[1] == to {
			return true
		}
	}
	return false
}

// fakeExtractor returns one shared fact and one private skill, and records what
// journal/transcript it was handed.
type fakeExtractor struct {
	gotJournal    string
	gotTranscript string
	calls         int
}

func (e *fakeExtractor) Extract(_ context.Context, journal, transcript string) ([]Candidate, error) {
	e.calls++
	e.gotJournal, e.gotTranscript = journal, transcript
	return []Candidate{
		{Node: contracts.Node{Key: "facts/eco", Title: "economy", Body: "DataStore"}, Private: false},
		{Node: contracts.Node{Key: "skills/ds", Title: "datastore skill", Body: "retry"}, Private: true},
	}, nil
}

func TestConsolidatePersistsFactsSharedAndSkillsPrivate(t *testing.T) {
	mem := newRec()
	ex := &fakeExtractor{}
	scope := contracts.MemoryScope{Project: "projects/game", Agent: "agents/scripter"}
	l := NewLearner(mem, "alpha", scope, ex, "", 0)

	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if _, ok := mem.nodes["facts/eco"]; !ok {
		t.Fatal("shared fact not recorded")
	}
	if _, ok := mem.nodes["skills/ds"]; !ok {
		t.Fatal("private skill not recorded")
	}
	if !mem.hasLink("projects/game", "facts/eco") {
		t.Fatalf("fact not linked under project: %+v", mem.links)
	}
	if !mem.hasLink("agents/scripter", "skills/ds") {
		t.Fatalf("skill not linked under agent: %+v", mem.links)
	}
	if mem.hasLink("projects/game", "skills/ds") {
		t.Fatalf("private skill leaked under project: %+v", mem.links)
	}
}

func TestConsolidateIsIdempotent(t *testing.T) {
	mem := newRec()
	ex := &fakeExtractor{}
	scope := contracts.MemoryScope{Project: "projects/game", Agent: "agents/scripter"}
	l := NewLearner(mem, "alpha", scope, ex, "", 0)
	ctx := context.Background()

	if err := l.Consolidate(ctx); err != nil {
		t.Fatalf("first Consolidate: %v", err)
	}
	links := len(mem.links)
	if links == 0 {
		t.Fatal("first Consolidate recorded no links")
	}
	// The extractor re-returns the same candidates; a second run over the same
	// journal must not re-link facts/skills already persisted this session.
	if err := l.Consolidate(ctx); err != nil {
		t.Fatalf("second Consolidate: %v", err)
	}
	if len(mem.links) != links {
		t.Fatalf("second Consolidate wrote duplicate links: %d then %d", links, len(mem.links))
	}
}

func TestConsolidateReadsJournalFile(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, "calls.log")
	if err := os.WriteFile(journal, []byte("123 tool=roblox__script_write ms=4 ok=true args={}"), 0o644); err != nil {
		t.Fatal(err)
	}
	mem := newRec()
	ex := &fakeExtractor{}
	l := NewLearner(mem, "alpha", contracts.MemoryScope{Project: "projects/g"}, ex, journal, 0)
	_ = l.Consolidate(context.Background())
	if ex.gotJournal == "" || ex.gotJournal[:3] != "123" {
		t.Fatalf("extractor did not receive journal content: %q", ex.gotJournal)
	}
}

func TestNilExtractorMakesConsolidateNoOp(t *testing.T) {
	mem := newRec()
	l := NewLearner(mem, "alpha", contracts.MemoryScope{Project: "p"}, nil, "", 0)
	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("nil extractor should no-op, got %v", err)
	}
	if len(mem.nodes) != 0 || len(mem.links) != 0 {
		t.Fatalf("nil extractor wrote to memory: %+v", mem.nodes)
	}
}

// TestConsolidateResetsTransitionsOnEarlyReturn guards the deferred reset:
// Restore appends transitions out of band between passes, and a nil-extractor
// Learner's Consolidate returns early (before report). Without the deferred
// reset those transitions would accumulate unbounded across passes. The defer
// caps them on every return path.
func TestConsolidateResetsTransitionsOnEarlyReturn(t *testing.T) {
	mem := newRec()
	l := NewLearner(mem, "alpha", contracts.MemoryScope{Project: "p"}, nil, "", 0)
	l.transitions = []Transition{{Key: "facts/a", From: contracts.StateArchived, To: contracts.StateActive, Kind: "restore"}}
	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("nil extractor should no-op, got %v", err)
	}
	if l.transitions != nil {
		t.Fatalf("early-return Consolidate did not reset transitions: %+v", l.transitions)
	}
}

func TestConsolidateContinuesPastRecordError(t *testing.T) {
	mem := newRec()
	mem.failOn = "facts/eco" // the shared fact fails; the private skill must still persist
	ex := &fakeExtractor{}
	l := NewLearner(mem, "alpha", contracts.MemoryScope{Project: "projects/g", Agent: "agents/s"}, ex, "", 0)

	err := l.Consolidate(context.Background())
	if err == nil {
		t.Fatal("expected the failing candidate's error to be returned")
	}
	// A per-candidate failure must not abort the batch: the sibling still persists.
	if _, ok := mem.nodes["skills/ds"]; !ok {
		t.Fatal("candidate after the failing one was skipped")
	}
}

func TestObserveTriggersConsolidateEveryN(t *testing.T) {
	mem := newRec()
	ex := &fakeExtractor{}
	l := NewLearner(mem, "alpha", contracts.MemoryScope{Project: "p"}, ex, "", 2)
	ctx := context.Background()
	_ = l.Observe(ctx, contracts.Prompt{Author: "a", Content: "one"}, "r1")
	if ex.calls != 0 {
		t.Fatalf("should not consolidate after 1 turn (every=2), calls=%d", ex.calls)
	}
	_ = l.Observe(ctx, contracts.Prompt{Author: "a", Content: "two"}, "r2")
	if ex.calls != 1 {
		t.Fatalf("should consolidate on the 2nd turn, calls=%d", ex.calls)
	}
}
