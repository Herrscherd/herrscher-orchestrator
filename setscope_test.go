package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Herrscherd/herrscher-contracts"
)

// A session that launched under a guessed project must be able to file the rest
// of what it learns somewhere else, once the conversation says where.
func TestSetScopeRedirectsWhatIsRemembered(t *testing.T) {
	mem := newFake()
	c := NewScoped(mem, "s", contracts.MemoryScope{Project: contracts.ProjectKey("herrscher")})
	c.React(context.Background(), "<remember>the launch guess</remember>")
	c.SetScope(contracts.MemoryScope{Project: contracts.ProjectKey("neublox")})
	c.React(context.Background(), "<remember>what it really was</remember>")

	var before, after bool
	for key := range mem.nodes {
		before = before || strings.HasPrefix(key, "projects/herrscher/")
		after = after || strings.HasPrefix(key, "projects/neublox/")
	}
	if !before {
		t.Fatal("the fact remembered before the re-scope should stay where it was filed")
	}
	if !after {
		t.Fatalf("nothing was filed under the new project: %v", mem.nodes)
	}
}

// SetScope arrives from the turn goroutine while the idle loop may be walking
// the old roots. Run under -race: the point of the test is that it is quiet.
func TestSetScopeIsSafeAlongsideAConsolidate(t *testing.T) {
	l := NewLearner(newFake(), "s", contracts.MemoryScope{Project: contracts.ProjectKey("a")}, nil, "", 0)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = l.Consolidate(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			l.SetScope(contracts.MemoryScope{Project: contracts.ProjectKey("b")})
		}
	}()
	wg.Wait()
}
