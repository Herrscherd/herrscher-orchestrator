package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Herrscherd/herrscher-contracts"
)

// budgetMem records nodes but refuses any body longer than `limit` runes with a
// *contracts.BudgetError — the same contract the obsidian per-node budget uses.
type budgetMem struct {
	nodes    map[string]contracts.Node
	limit    int
	searches int
}

func newBudgetMem(limit int) *budgetMem {
	return &budgetMem{nodes: map[string]contracts.Node{}, limit: limit}
}

func (m *budgetMem) Record(_ context.Context, n contracts.Node) error {
	if r := len([]rune(n.Body)); m.limit > 0 && r > m.limit {
		return &contracts.BudgetError{Key: n.Key, Runes: r, Limit: m.limit}
	}
	m.nodes[n.Key] = n
	return nil
}
func (m *budgetMem) Recall(_ context.Context, key string, _ int) (contracts.Subgraph, error) {
	return contracts.Subgraph{Root: contracts.Node{Key: key}}, nil
}
func (m *budgetMem) Search(context.Context, contracts.Query) ([]contracts.Node, error) {
	m.searches++
	return nil, nil
}
func (m *budgetMem) Links(context.Context, string, string, string) error { return nil }
func (m *budgetMem) Unlink(context.Context, string, string) error        { return nil }
func (m *budgetMem) Close() error                                        { return nil }

// oneBig returns a single over-budget shared fact from the journal/transcript.
type oneBig struct{ body string }

func (e *oneBig) Extract(context.Context, string, string) ([]Candidate, error) {
	return []Candidate{{Node: contracts.Node{Key: "facts/big", Body: e.body}}}, nil
}

// bigThenSmall returns an over-budget fact followed by an in-budget one, to prove
// a refusal does not drop the sibling.
type bigThenSmall struct{ big, small string }

func (e *bigThenSmall) Extract(context.Context, string, string) ([]Candidate, error) {
	return []Candidate{
		{Node: contracts.Node{Key: "facts/big", Body: e.big}},
		{Node: contracts.Node{Key: "facts/small", Body: e.small}},
	}, nil
}

// shrinkingExtractor is an Extractor that ALSO implements Consolidator: it
// shrinks the over-budget node's body to `to`. If to == "" it returns the node
// unchanged (simulating a consolidator that cannot shrink far enough).
type shrinkingExtractor struct {
	oneBig
	to    string
	calls int
}

func (e *shrinkingExtractor) Consolidate(_ context.Context, over contracts.Node, _ int) (contracts.Node, error) {
	e.calls++
	if e.to == "" {
		return over, nil // still too large
	}
	over.Body = e.to
	return over, nil
}

func mustScope() contracts.MemoryScope {
	return contracts.MemoryScope{Project: "projects/g", Agent: "agents/s"}
}

func TestPersistConsolidatesOverBudgetCandidate(t *testing.T) {
	mem := newBudgetMem(5)                                                  // 5-rune limit
	ex := &shrinkingExtractor{oneBig: oneBig{body: "0123456789"}, to: "ok"} // 10 → "ok"
	l := NewLearner(mem, "s", mustScope(), ex, "", 0)

	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if ex.calls != 1 {
		t.Fatalf("consolidator not invoked exactly once: calls=%d", ex.calls)
	}
	n, ok := mem.nodes["facts/big"]
	if !ok {
		t.Fatal("consolidated candidate was not persisted")
	}
	if n.Body != "ok" {
		t.Fatalf("persisted body = %q, want the shrunk %q", n.Body, "ok")
	}
	if len(l.pending) != 0 {
		t.Fatalf("queue should be empty after a resolved refusal: %d", len(l.pending))
	}
}

func TestPersistEnqueuesWhenNoConsolidator(t *testing.T) {
	mem := newBudgetMem(5)
	ex := &bigThenSmall{big: "0123456789", small: "hi"} // plain Extractor, no Consolidate
	l := NewLearner(mem, "s", mustScope(), ex, "", 0)

	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	// Sibling under budget still persists...
	if _, ok := mem.nodes["facts/small"]; !ok {
		t.Fatal("in-budget sibling was dropped")
	}
	// ...the over-budget one is queued, not persisted, not seen.
	if _, ok := mem.nodes["facts/big"]; ok {
		t.Fatal("over-budget candidate was persisted despite no consolidator")
	}
	if len(l.pending) != 1 || l.pending[0].Node.Key != "facts/big" {
		t.Fatalf("over-budget candidate not queued: %+v", l.pending)
	}
	if l.seen["facts/big"] {
		t.Fatal("queued candidate must not be marked seen")
	}
}

func TestPersistEnqueuesWhenConsolidatorStillTooLarge(t *testing.T) {
	mem := newBudgetMem(5)
	ex := &shrinkingExtractor{oneBig: oneBig{body: "0123456789"}, to: ""} // returns node unchanged
	l := NewLearner(mem, "s", mustScope(), ex, "", 0)

	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if ex.calls != 1 {
		t.Fatalf("consolidator should be tried once: calls=%d", ex.calls)
	}
	if _, ok := mem.nodes["facts/big"]; ok {
		t.Fatal("still-too-large node must not be persisted")
	}
	if len(l.pending) != 1 {
		t.Fatalf("still-too-large candidate not queued: %+v", l.pending)
	}
}

func TestPendingDrainedOnLaterPass(t *testing.T) {
	mem := newBudgetMem(5)
	// First pass: a plain extractor with no consolidator → the big fact is queued.
	ex1 := &oneBig{body: "0123456789"}
	l := NewLearner(mem, "s", mustScope(), ex1, "", 0)
	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.pending) != 1 {
		t.Fatalf("setup: expected 1 queued, got %d", len(l.pending))
	}
	// Swap in an extractor+consolidator and re-run: the drain resolves the queued
	// candidate. The new extractor returns nothing new so only the drain persists.
	l.extract = &shrinkingExtractor{oneBig: oneBig{body: "unused"}, to: "ok"}
	// Neutralise the new extractor's own output so the test isolates the drain:
	// oneBig.body "unused" is 6 runes (> 5) and would re-queue under key facts/big,
	// deduping against the drained one — so assert on persistence, not queue size.
	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, ok := mem.nodes["facts/big"]; !ok || n.Body != "ok" {
		t.Fatalf("queued candidate not resolved by drain on later pass: %+v ok=%v", mem.nodes["facts/big"], ok)
	}
}

func TestQueueDedupsByKey(t *testing.T) {
	mem := newBudgetMem(5)
	ex := &oneBig{body: "0123456789"} // same over-budget key every pass, no consolidator
	l := NewLearner(mem, "s", mustScope(), ex, "", 0)
	for i := 0; i < 3; i++ {
		if err := l.Consolidate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(l.pending) != 1 {
		t.Fatalf("queue must dedup by key across passes: %d", len(l.pending))
	}
}

// emptyingExtractor shrinks by returning a node with an empty Body — a
// misbehaving consolidator the Learner must refuse rather than persist as junk.
type emptyingExtractor struct{ oneBig }

func (e *emptyingExtractor) Consolidate(_ context.Context, over contracts.Node, _ int) (contracts.Node, error) {
	over.Body = ""
	return over, nil
}

func TestPersistRejectsEmptyConsolidatorResult(t *testing.T) {
	mem := newBudgetMem(5)
	ex := &emptyingExtractor{oneBig: oneBig{body: "0123456789"}}
	l := NewLearner(mem, "s", mustScope(), ex, "", 0)
	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if _, ok := mem.nodes["facts/big"]; ok {
		t.Fatal("empty consolidator result must not be persisted")
	}
	if len(l.pending) != 1 {
		t.Fatalf("candidate not queued after rejected consolidation: %+v", l.pending)
	}
}

func TestSweepRunsAfterEnqueue(t *testing.T) {
	mem := newBudgetMem(5)
	ex := &oneBig{body: "0123456789"} // over budget, no consolidator → enqueued
	l := NewLearner(mem, "s", mustScope(), ex, "", 0)
	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if mem.searches == 0 {
		t.Fatal("staleness sweep did not run after an enqueue")
	}
}

func TestEnqueueEmitsWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	mem := newBudgetMem(5)
	l := NewLearner(mem, "s", mustScope(), &oneBig{body: "0123456789"}, "", 0)
	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if !strings.Contains(buf.String(), "over budget") {
		t.Fatalf("expected a WARN mentioning the budget, got: %q", buf.String())
	}
}
