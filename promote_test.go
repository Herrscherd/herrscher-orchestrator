package orchestrator

import (
	"context"
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

func TestPromotedKey(t *testing.T) {
	got := promotedKey("projects/neublox", "agents/roblox-dev/skills/retry-http")
	if want := "projects/neublox/skills/retry-http"; got != want {
		t.Errorf("promotedKey = %q, want %q", got, want)
	}
}

// promoteLearner builds a Learner over mem with a project+agent scope and the
// promotion bar set.
func promoteLearner(mem contracts.Memory, minAgeDays int) *Learner {
	l := NewLearner(mem, "s", contracts.MemoryScope{
		Project: "projects/p",
		Agent:   "agents/a",
	}, plainExt{}, "", 0)
	l.SetPromote(time.Duration(minAgeDays) * 24 * time.Hour)
	return l
}

func TestPromoteDisabledIsNoop(t *testing.T) {
	mem := &mergeMem{nodes: []contracts.Node{promoNode("agents/a/skills/x", contracts.StateActive, 20)}}
	l := promoteLearner(mem, 0) // disabled
	if err := l.Promote(context.Background()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(mem.records) != 0 || len(mem.links) != 0 {
		t.Fatalf("disabled promotion must be a no-op; got %d records %d links", len(mem.records), len(mem.links))
	}
}

func TestPromoteNoScopeIsNoop(t *testing.T) {
	mem := &mergeMem{nodes: []contracts.Node{promoNode("agents/a/skills/x", contracts.StateActive, 20)}}
	l := NewLearner(mem, "s", contracts.MemoryScope{Agent: "agents/a"}, plainExt{}, "", 0) // no Project
	l.SetPromote(10 * 24 * time.Hour)
	if err := l.Promote(context.Background()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(mem.records) != 0 {
		t.Fatalf("no project scope must be a no-op; got %d records", len(mem.records))
	}
}

func TestPromoteHappyPath(t *testing.T) {
	orig := promoNode("agents/a/skills/x", contracts.StateActive, 20)
	orig.Body = "retry with backoff"
	mem := &mergeMem{nodes: []contracts.Node{orig}}
	l := promoteLearner(mem, 10)
	if err := l.Promote(context.Background()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	// shared copy written under the project scope
	var copy *contracts.Node
	for i := range mem.records {
		if mem.records[i].Key == "projects/p/skills/x" {
			copy = &mem.records[i]
		}
	}
	if copy == nil {
		t.Fatal("shared copy not recorded under projects/p/skills/x")
	}
	if copy.Body != "retry with backoff" {
		t.Errorf("copy body = %q, want the original body", copy.Body)
	}
	if copy.Meta["promotedFrom"] != "agents/a/skills/x" {
		t.Errorf("copy promotedFrom = %q, want the original key", copy.Meta["promotedFrom"])
	}
	if copy.Meta[MetaPromotedTo] != "" {
		t.Errorf("copy must not itself carry promotedTo: %q", copy.Meta[MetaPromotedTo])
	}
	// original labeled (last record for that key)
	var labeled *contracts.Node
	for i := range mem.records {
		if mem.records[i].Key == "agents/a/skills/x" {
			labeled = &mem.records[i]
		}
	}
	if labeled == nil || labeled.Meta[MetaPromotedTo] != "projects/p/skills/x" {
		t.Fatalf("original not labeled with promotedTo: %+v", labeled)
	}
	// original state untouched, lastSeen preserved
	if labeled.Meta[contracts.MetaState] != contracts.StateActive {
		t.Errorf("original state changed to %q", labeled.Meta[contracts.MetaState])
	}
	if labeled.Meta[contracts.MetaLastSeen] != orig.Meta[contracts.MetaLastSeen] {
		t.Errorf("original lastSeen bumped: %q", labeled.Meta[contracts.MetaLastSeen])
	}
	// link original -> copy
	var linked bool
	for _, ln := range mem.links {
		if ln == [3]string{"agents/a/skills/x", "projects/p/skills/x", "promoted-to"} {
			linked = true
		}
	}
	if !linked {
		t.Errorf("missing promoted-to link: %+v", mem.links)
	}
}

func TestPromoteScopeIsolation(t *testing.T) {
	// A different agent's private node must never be scanned/promoted.
	mem := &mergeMem{nodes: []contracts.Node{promoNode("agents/other/skills/x", contracts.StateActive, 20)}}
	l := promoteLearner(mem, 10)
	if err := l.Promote(context.Background()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(mem.records) != 0 {
		t.Fatalf("another agent's node was promoted; got %d records", len(mem.records))
	}
}

func TestPromoteIdempotentRerun(t *testing.T) {
	mem := &mergeMem{nodes: []contracts.Node{promoNode("agents/a/skills/x", contracts.StateActive, 20)}}
	l := promoteLearner(mem, 10)
	if err := l.Promote(context.Background()); err != nil {
		t.Fatalf("Promote 1: %v", err)
	}
	// The fake's Search returns the ORIGINAL nodes slice, not the labeled write;
	// simulate the durable label by replacing the node with its labeled version.
	for i := range mem.nodes {
		if mem.nodes[i].Key == "agents/a/skills/x" {
			mem.nodes[i].Meta[MetaPromotedTo] = "projects/p/skills/x"
		}
	}
	before := len(mem.records)
	if err := l.Promote(context.Background()); err != nil {
		t.Fatalf("Promote 2: %v", err)
	}
	if len(mem.records) != before {
		t.Fatalf("re-run promoted again: records %d -> %d", before, len(mem.records))
	}
}

func TestPromoteBestEffortOnRecordError(t *testing.T) {
	// Two eligible nodes; the shared copy of the first fails to write. The second
	// must still be promoted, and Promote returns the first error.
	mem := &mergeMem{
		nodes: []contracts.Node{
			promoNode("agents/a/skills/x", contracts.StateActive, 20),
			promoNode("agents/a/skills/y", contracts.StateActive, 20),
		},
		recErrOn: "projects/p/skills/x",
	}
	l := promoteLearner(mem, 10)
	if err := l.Promote(context.Background()); err == nil {
		t.Fatal("expected the failing copy's error to surface")
	}
	var sawY bool
	for _, r := range mem.records {
		if r.Key == "projects/p/skills/y" {
			sawY = true
		}
	}
	if !sawY {
		t.Fatal("sibling promotion was skipped after the first node's failure")
	}
}

// orderMem records the order of key-writes so we can assert the pass
// ordering Sweep -> Merge -> Promote.
type orderMem struct {
	mergeMem
	order []string
}

func (m *orderMem) Record(ctx context.Context, n contracts.Node) error {
	m.order = append(m.order, n.Key)
	return m.mergeMem.Record(ctx, n)
}

func TestConsolidateRunsPromoteAfterMerge(t *testing.T) {
	// A single eligible private node; with a nil extractor Extract yields nothing,
	// but Sweep/Merge/Promote still run at the Consolidate tail. Promote must fire
	// and produce the shared copy.
	mem := &mergeMem{nodes: []contracts.Node{promoNode("agents/a/skills/x", contracts.StateActive, 20)}}
	l := promoteLearner(mem, 10)
	// Pin the clock to the node's own age window: promoNode anchors its
	// capturedAt/lastSeen stamps at a fixed 2026-01-01 date, but Sweep (which runs
	// before Promote in Consolidate) measures staleness against the wall clock by
	// default. Without pinning, a run on a real date far past that anchor would
	// have Sweep archive the node before Promote ever sees it, making this test's
	// pass/fail depend on the calendar date it happens to run on.
	l.now = func() time.Time { return time.Date(2026, 1, 21, 0, 0, 0, 0, time.UTC) }
	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	var promoted bool
	for _, r := range mem.records {
		if r.Key == "projects/p/skills/x" {
			promoted = true
		}
	}
	if !promoted {
		t.Fatal("Consolidate did not run Promote (no shared copy written)")
	}
}

func TestMergeSkipsPromotedOriginal(t *testing.T) {
	// A promoted original in a mergeable domain group must be excluded from merge
	// candidacy so it is never re-fused with (or instead of) its shared copy.
	a := stale("agents/a/skills/x", "http")
	a.Meta[MetaPromotedTo] = "projects/p/skills/x"
	b := stale("agents/a/skills/y", "http")
	mem := &mergeMem{nodes: []contracts.Node{a, b}}
	f := &fakeMerger{result: nil} // capture what it is handed
	l := learnerWith(mem, f, 2, 40, "stale")
	if err := l.Merge(context.Background()); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	// With the promoted original excluded, only 1 candidate remains in the group,
	// which is below the min-2 threshold, so the merger is never called.
	if len(f.calls) != 0 {
		t.Fatalf("merger was called with a promoted original in the group: %+v", f.calls)
	}
}
