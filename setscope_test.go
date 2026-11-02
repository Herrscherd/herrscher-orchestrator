package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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

type gatedRecordMem struct {
	*fakeMem

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *gatedRecordMem) Record(ctx context.Context, n contracts.Node) error {
	m.once.Do(func() {
		close(m.entered)
		<-m.release
	})
	return m.fakeMem.Record(ctx, n)
}

type fixedExtractor struct {
	candidates []Candidate
}

func (e fixedExtractor) Extract(context.Context, string, string) ([]Candidate, error) {
	return e.candidates, nil
}

func TestSetScopeIsSafeAlongsideAConsolidate(t *testing.T) {
	mem := &gatedRecordMem{fakeMem: newFake(), entered: make(chan struct{}), release: make(chan struct{})}
	ex := fixedExtractor{candidates: []Candidate{
		{Node: contracts.Node{Key: "fact/one", Title: "one", Body: "first"}},
		{Node: contracts.Node{Key: "fact/two", Title: "two", Body: "second"}},
	}}
	l := NewLearner(mem, "s", contracts.MemoryScope{Project: contracts.ProjectKey("a")}, ex, "", 0)

	consolidated := make(chan error, 1)
	go func() { consolidated <- l.Consolidate(context.Background()) }()
	<-mem.entered

	scoped := make(chan struct{})
	go func() {
		l.SetScope(contracts.MemoryScope{Project: contracts.ProjectKey("b")})
		close(scoped)
	}()
	select {
	case <-scoped:
	case <-time.After(500 * time.Millisecond):
	}
	close(mem.release)

	if err := <-consolidated; err != nil {
		t.Fatal(err)
	}
	<-scoped

	filedUnder := make(map[string]string)
	for _, link := range mem.links {
		filedUnder[link[1]] = link[0]
	}
	before := contracts.ProjectKey("a")
	for _, key := range []string{"fact/one", "fact/two"} {
		if got := filedUnder[key]; got != before {
			t.Fatalf("%s filed under %q, want %q: one pass must not straddle a re-scope", key, got, before)
		}
	}
}
