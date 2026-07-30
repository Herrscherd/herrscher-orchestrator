package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/Herrscherd/herrscher-contracts"
)

func TestObserveRecordsRawTurnWhenEnabled(t *testing.T) {
	mem := newRec()
	l := NewLearner(mem, "sess-1", contracts.MemoryScope{}, nil, "", 0)
	l.SetRawArchive(true)
	ctx := context.Background()

	if err := l.Observe(ctx, contracts.Prompt{Author: "alice", Content: "how do I deploy?"}, "run make ship"); err != nil {
		t.Fatal(err)
	}
	if err := l.Observe(ctx, contracts.Prompt{Author: "alice", Content: "and rollback?"}, "make unship"); err != nil {
		t.Fatal(err)
	}

	first, ok := mem.nodes["raw/sess-1/1"]
	if !ok {
		t.Fatalf("first raw node raw/sess-1/1 not recorded; have %v", mem.nodes)
	}
	if first.Kind != contracts.KindTranscript {
		t.Fatalf("raw node kind = %q, want KindTranscript", first.Kind)
	}
	if !strings.Contains(first.Body, "how do I deploy?") || !strings.Contains(first.Body, "run make ship") {
		t.Fatalf("raw body must contain the verbatim prompt and reply, got %q", first.Body)
	}
	if _, ok := mem.nodes["raw/sess-1/2"]; !ok {
		t.Fatal("second raw node raw/sess-1/2 not recorded; seq must advance")
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
