package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

// rawNodes returns the KindTranscript nodes recorded so far, keyed by their node
// key, so tests can assert on the raw archival tier without hardcoding the
// wall-clock epoch embedded in each key.
func rawNodes(mem *recMem) map[string]contracts.Node {
	out := map[string]contracts.Node{}
	for k, n := range mem.nodes {
		if n.Kind == contracts.KindTranscript {
			out[k] = n
		}
	}
	return out
}

func TestObserveRecordsRawTurnWhenEnabled(t *testing.T) {
	mem := newRec()
	l := NewLearner(mem, "sess-1", contracts.MemoryScope{}, nil, "", 0)
	l.now = func() time.Time { return time.Date(2026, 1, 21, 9, 0, 0, 0, time.UTC) }
	l.SetRawArchive(true)
	ctx := context.Background()

	if err := l.Observe(ctx, contracts.Prompt{Author: "alice", Content: "how do I deploy?"}, "run make ship"); err != nil {
		t.Fatal(err)
	}
	if err := l.Observe(ctx, contracts.Prompt{Author: "alice", Content: "and rollback?"}, "make unship"); err != nil {
		t.Fatal(err)
	}

	raw := rawNodes(mem)
	if len(raw) != 2 {
		t.Fatalf("want 2 raw nodes, got %d: %v", len(raw), raw)
	}
	var bodies []string
	for k, n := range raw {
		if !strings.HasPrefix(k, "raw/sess-1/") {
			t.Fatalf("raw key %q must be under raw/<sessionTail>/", k)
		}
		bodies = append(bodies, n.Body)
	}
	joined := strings.Join(bodies, "\n")
	for _, want := range []string{"how do I deploy?", "run make ship", "and rollback?", "make unship"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("raw bodies must contain %q verbatim, got %q", want, joined)
		}
	}
}

// TestRawSurvivesLearnerRestart locks the G7 append-only invariant across a
// bridge restart: the supervised bridge runs as a subprocess, so a resumed
// session rebuilds the Learner from scratch (rawSeq resets to 1). A per-process
// epoch prefix must keep the fresh run's turns from overwriting the earlier
// run's raw/<tail>/1.. nodes in the shared vault.
func TestRawSurvivesLearnerRestart(t *testing.T) {
	mem := newRec() // the durable vault, shared across both runs of the session
	ctx := context.Background()

	run := func(at time.Time, content, reply string) {
		l := NewLearner(mem, "sess-1", contracts.MemoryScope{}, nil, "", 0)
		l.now = func() time.Time { return at }
		l.SetRawArchive(true)
		if err := l.Observe(ctx, contracts.Prompt{Author: "alice", Content: content}, reply); err != nil {
			t.Fatal(err)
		}
	}

	run(time.Date(2026, 1, 21, 9, 0, 0, 0, time.UTC), "first-run turn", "first-run reply")
	// "restart": a brand-new Learner for the same session, seq counter back at 1.
	run(time.Date(2026, 1, 21, 9, 5, 0, 0, time.UTC), "second-run turn", "second-run reply")

	raw := rawNodes(mem)
	if len(raw) != 2 {
		t.Fatalf("restart overwrote an earlier raw turn: want 2 nodes, got %d: %v", len(raw), raw)
	}
	joined := ""
	for _, n := range raw {
		joined += n.Body + "\n"
	}
	if !strings.Contains(joined, "first-run turn") || !strings.Contains(joined, "second-run turn") {
		t.Fatalf("both runs' turns must survive, got %q", joined)
	}
}

// TestRawKeyConfinesSessionTail verifies a '/' in the session id cannot make the
// raw node escape its raw/<tail>/ segment (belt-and-suspenders; session is
// trusted host config).
func TestRawKeyConfinesSessionTail(t *testing.T) {
	mem := newRec()
	l := NewLearner(mem, "team/../evil", contracts.MemoryScope{}, nil, "", 0)
	l.now = func() time.Time { return time.Date(2026, 1, 21, 9, 0, 0, 0, time.UTC) }
	l.SetRawArchive(true)
	if err := l.Observe(context.Background(), contracts.Prompt{Author: "a", Content: "x"}, "y"); err != nil {
		t.Fatal(err)
	}
	for k := range rawNodes(mem) {
		if strings.Contains(strings.TrimPrefix(k, "raw/"), "/../") || !strings.HasPrefix(k, "raw/team-..-evil/") {
			t.Fatalf("raw key not confined to its segment: %q", k)
		}
	}
}

func TestObserveSkipsRawWhenDisabled(t *testing.T) {
	mem := newRec()
	l := NewLearner(mem, "sess-1", contracts.MemoryScope{}, nil, "", 0)
	// SetRawArchive not called → default off.
	ctx := context.Background()
	if err := l.Observe(ctx, contracts.Prompt{Author: "a", Content: "x"}, "y"); err != nil {
		t.Fatal(err)
	}
	for k, n := range mem.nodes {
		if n.Kind == contracts.KindTranscript {
			t.Fatalf("raw node %q written with raw-archive off; must be no-op", k)
		}
	}
}
