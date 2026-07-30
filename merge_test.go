package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

// mergeMem is a fake Memory: Search returns a fixed node set, Record/Links
// capture writes. recErrOn, if set, makes Record fail for that one Key.
type mergeMem struct {
	nodes    []contracts.Node
	records  []contracts.Node
	links    [][3]string
	recErrOn string
}

func (m *mergeMem) Record(_ context.Context, n contracts.Node) error {
	if m.recErrOn != "" && n.Key == m.recErrOn {
		return context.DeadlineExceeded // any non-budget error
	}
	m.records = append(m.records, n)
	return nil
}
func (m *mergeMem) Recall(_ context.Context, key string, _ int) (contracts.Subgraph, error) {
	return contracts.Subgraph{Root: contracts.Node{Key: key}}, nil
}
func (m *mergeMem) Search(context.Context, contracts.Query) ([]contracts.Node, error) {
	return m.nodes, nil
}
func (m *mergeMem) Links(_ context.Context, from, to, rel string) error {
	m.links = append(m.links, [3]string{from, to, rel})
	return nil
}
func (m *mergeMem) Close() error { return nil }

// fakeMerger is an Extractor that ALSO implements Merger. It records each Merge
// call's candidate slice and returns a fixed result.
type fakeMerger struct {
	calls  [][]contracts.Node
	result []Umbrella
	err    error
}

func (f *fakeMerger) Extract(context.Context, string, string) ([]Candidate, error) {
	return nil, nil
}
func (f *fakeMerger) Merge(_ context.Context, cands []contracts.Node) ([]Umbrella, error) {
	f.calls = append(f.calls, cands)
	return f.result, f.err
}

// plainExt is an Extractor that does NOT implement Merger.
type plainExt struct{}

func (plainExt) Extract(context.Context, string, string) ([]Candidate, error) { return nil, nil }

// stale builds a stale node with a domain.
func stale(key, domain string) contracts.Node {
	return contracts.Node{Key: key, Body: key, Meta: map[string]string{
		"domain": domain, contracts.MetaState: contracts.StateStale,
	}}
}

// learnerWith builds a Learner over mem+ex with merge configured.
func learnerWith(mem contracts.Memory, ex Extractor, min, max int, target string) *Learner {
	l := NewLearner(mem, "s", contracts.MemoryScope{}, ex, "", 0)
	l.SetMerge(min, max, target)
	return l
}

func TestMergeNoMergerIsNoop(t *testing.T) {
	mem := &mergeMem{nodes: []contracts.Node{stale("a", "d"), stale("b", "d")}}
	l := learnerWith(mem, plainExt{}, 2, 40, "stale")
	if err := l.Merge(context.Background()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(mem.records) != 0 || len(mem.links) != 0 {
		t.Fatalf("no-merger must be a no-op; got %d records %d links", len(mem.records), len(mem.links))
	}
}

func TestMergeDisabledWhenMinZero(t *testing.T) {
	mem := &mergeMem{nodes: []contracts.Node{stale("a", "d"), stale("b", "d")}}
	f := &fakeMerger{}
	l := learnerWith(mem, f, 0, 40, "stale") // min 0 => off
	if err := l.Merge(context.Background()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("mergeMin<=0 must not call the merger; got %d calls", len(f.calls))
	}
}

func TestMergeBelowThresholdNoCall(t *testing.T) {
	mem := &mergeMem{nodes: []contracts.Node{stale("a", "d")}} // 1 < min 2
	f := &fakeMerger{}
	l := learnerWith(mem, f, 2, 40, "stale")
	if err := l.Merge(context.Background()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("below threshold must not call merger; got %d", len(f.calls))
	}
}

func TestMergeHappyPathWritesAndArchives(t *testing.T) {
	a, b := stale("facts/a", "d"), stale("facts/b", "d")
	a.Meta[contracts.MetaLastSeen] = "2026-01-01T00:00:00Z"
	mem := &mergeMem{nodes: []contracts.Node{a, b}}
	f := &fakeMerger{result: []Umbrella{{
		Node:   contracts.Node{Key: "facts/umbrella", Body: "fused"},
		Merged: []string{"facts/a", "facts/b"},
	}}}
	l := learnerWith(mem, f, 2, 40, "stale")
	if err := l.Merge(context.Background()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	// umbrella written
	var sawUmbrella bool
	for _, r := range mem.records {
		if r.Key == "facts/umbrella" && r.Body == "fused" {
			sawUmbrella = true
		}
	}
	if !sawUmbrella {
		t.Fatal("umbrella node was not recorded")
	}
	// each original archived + labeled + lastSeen preserved
	for _, k := range []string{"facts/a", "facts/b"} {
		var got *contracts.Node
		for i := range mem.records {
			if mem.records[i].Key == k {
				got = &mem.records[i]
			}
		}
		if got == nil {
			t.Fatalf("original %s not re-recorded", k)
		}
		if got.Meta[MetaMergedInto] != "facts/umbrella" {
			t.Errorf("%s: mergedInto=%q, want facts/umbrella", k, got.Meta[MetaMergedInto])
		}
		if got.Meta[contracts.MetaState] != contracts.StateArchived {
			t.Errorf("%s: state=%q, want archived", k, got.Meta[contracts.MetaState])
		}
	}
	// lastSeen of facts/a preserved (not bumped)
	for i := range mem.records {
		if mem.records[i].Key == "facts/a" && mem.records[i].Meta[contracts.MetaLastSeen] != "2026-01-01T00:00:00Z" {
			t.Errorf("facts/a lastSeen changed to %q", mem.records[i].Meta[contracts.MetaLastSeen])
		}
	}
	// links original -> umbrella
	want := map[string]bool{"facts/a": false, "facts/b": false}
	for _, ln := range mem.links {
		if ln[1] == "facts/umbrella" && ln[2] == "merged-into" {
			want[ln[0]] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("missing merged-into link from %s", k)
		}
	}
}

func TestMergeRejectsInvalidUmbrellaKeepsValid(t *testing.T) {
	mem := &mergeMem{nodes: []contracts.Node{stale("facts/a", "d"), stale("facts/b", "d")}}
	f := &fakeMerger{result: []Umbrella{
		{Node: contracts.Node{Key: "facts/bad", Body: "x"}, Merged: []string{"facts/a"}},          // <2
		{Node: contracts.Node{Key: "", Body: "x"}, Merged: []string{"facts/a", "facts/b"}},        // empty key
		{Node: contracts.Node{Key: "facts/e", Body: ""}, Merged: []string{"facts/a", "facts/b"}},  // empty body
		{Node: contracts.Node{Key: "facts/a", Body: "x"}, Merged: []string{"facts/a", "facts/b"}}, // key reuses candidate
		{Node: contracts.Node{Key: "facts/f", Body: "x"}, Merged: []string{"facts/a", "facts/z"}}, // key outside group
		{Node: contracts.Node{Key: "facts/good", Body: "ok"}, Merged: []string{"facts/a", "facts/b"}},
	}}
	l := learnerWith(mem, f, 2, 40, "stale")
	if err := l.Merge(context.Background()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	umbrellas := 0
	for _, r := range mem.records {
		if strings.HasPrefix(r.Key, "facts/") && r.Body == "ok" {
			umbrellas++
		}
		if r.Key == "facts/bad" || r.Key == "facts/e" || r.Key == "facts/f" {
			t.Errorf("invalid umbrella %s was recorded", r.Key)
		}
	}
	if umbrellas != 1 {
		t.Fatalf("expected exactly the 1 valid umbrella recorded, got %d", umbrellas)
	}
}

func TestMergeGroupsByDomainNotMixed(t *testing.T) {
	// two domains, each below threshold (1), jointly 2 — must NOT merge together.
	mem := &mergeMem{nodes: []contracts.Node{stale("a", "d1"), stale("b", "d2")}}
	f := &fakeMerger{}
	l := learnerWith(mem, f, 2, 40, "stale")
	if err := l.Merge(context.Background()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("distinct domains below threshold must not be merged; got %d calls", len(f.calls))
	}
}

func TestMergeRespectsCap(t *testing.T) {
	var nodes []contracts.Node
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		nodes = append(nodes, stale(k, "d"))
	}
	mem := &mergeMem{nodes: nodes}
	f := &fakeMerger{}
	l := learnerWith(mem, f, 2, 3, "stale") // cap 3
	if err := l.Merge(context.Background()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(f.calls) != 1 || len(f.calls[0]) != 3 {
		t.Fatalf("cap not respected: want 1 call of 3 nodes, got %d calls, first len %d", len(f.calls), func() int {
			if len(f.calls) == 0 {
				return -1
			}
			return len(f.calls[0])
		}())
	}
}

func TestMergeTargetFiltersToStale(t *testing.T) {
	active := stale("act", "d")
	active.Meta[contracts.MetaState] = contracts.StateActive
	mem := &mergeMem{nodes: []contracts.Node{stale("s1", "d"), stale("s2", "d"), active}}
	f := &fakeMerger{}
	l := learnerWith(mem, f, 2, 40, "stale")
	if err := l.Merge(context.Background()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(f.calls))
	}
	for _, n := range f.calls[0] {
		if n.Key == "act" {
			t.Error("active node leaked into a stale-target merge")
		}
	}
}

func TestSweepSkipsMergedOriginals(t *testing.T) {
	// A merged original with a FRESH lastSeen would, without the guard, be
	// re-derived as active and reactivated. The guard must keep it archived.
	fresh := "2026-07-30T00:00:00Z"
	n := contracts.Node{Key: "facts/a", Body: "x", Meta: map[string]string{
		MetaMergedInto:         "facts/u",
		contracts.MetaState:    contracts.StateArchived,
		contracts.MetaLastSeen: fresh,
	}}
	mem := &mergeMem{nodes: []contracts.Node{n}}
	c := NewScoped(mem, "s", contracts.MemoryScope{})
	c.SetStaleness(30*24*time.Hour, 90*24*time.Hour)
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	for _, r := range mem.records {
		if r.Key == "facts/a" {
			t.Fatalf("merged original was rewritten by Sweep (state now %q)", r.Meta[contracts.MetaState])
		}
	}
}

func TestMergeBestEffortOnOriginalRecordError(t *testing.T) {
	mem := &mergeMem{
		nodes:    []contracts.Node{stale("facts/a", "d"), stale("facts/b", "d")},
		recErrOn: "facts/a", // archiving facts/a fails
	}
	f := &fakeMerger{result: []Umbrella{{
		Node:   contracts.Node{Key: "facts/u", Body: "fused"},
		Merged: []string{"facts/a", "facts/b"},
	}}}
	l := learnerWith(mem, f, 2, 40, "stale")
	// error is surfaced (non-nil) but the umbrella + sibling still applied.
	_ = l.Merge(context.Background())
	var sawUmbrella, sawB bool
	for _, r := range mem.records {
		if r.Key == "facts/u" {
			sawUmbrella = true
		}
		if r.Key == "facts/b" && r.Meta[contracts.MetaState] == contracts.StateArchived {
			sawB = true
		}
	}
	if !sawUmbrella {
		t.Error("umbrella not written despite a per-original failure")
	}
	if !sawB {
		t.Error("sibling facts/b not archived despite facts/a failing")
	}
}

func TestSetMergeDefaults(t *testing.T) {
	l := NewLearner(&mergeMem{}, "s", contracts.MemoryScope{}, &fakeMerger{}, "", 0)
	l.SetMerge(3, 0, "bogus") // max<=0 -> default; bad target -> stale
	if l.mergeMax != defaultMergeMax {
		t.Errorf("mergeMax=%d, want %d", l.mergeMax, defaultMergeMax)
	}
	if l.mergeTarget != "stale" {
		t.Errorf("mergeTarget=%q, want stale", l.mergeTarget)
	}
	l.SetMerge(2, 10, "active")
	if l.mergeMax != 10 || l.mergeTarget != "active" || l.mergeMin != 2 {
		t.Errorf("SetMerge did not apply valid values: min=%d max=%d target=%q", l.mergeMin, l.mergeMax, l.mergeTarget)
	}
}
