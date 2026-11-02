package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

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

func TestSetScopeIsSafeAlongsideAConsolidate(t *testing.T) {
	l := NewLearner(newFake(), "s", contracts.MemoryScope{Project: contracts.ProjectKey("a")}, nil, "", 0)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 50 {
			if err := l.Consolidate(context.Background()); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			l.SetScope(contracts.MemoryScope{Project: contracts.ProjectKey("b")})
		}
	}()
	wg.Wait()
}
