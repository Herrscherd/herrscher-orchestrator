package orchestrator

import (
	"context"
	"testing"

	"github.com/Herrscherd/herrscher-contracts"
)

func buildOrch(t *testing.T, settings map[string]string) contracts.Orchestrator {
	t.Helper()
	var build func(context.Context, contracts.PluginConfig, contracts.Memory) (contracts.Orchestrator, error)
	for _, p := range contracts.Default.Orchestrators() {
		if p.Manifest.Kind == "basic" {
			build = p.Orchestrator
			break
		}
	}
	if build == nil {
		t.Fatal("basic orchestrator not registered")
	}
	orch, err := build(context.Background(), contracts.PluginConfig{Settings: settings}, newRec())
	if err != nil {
		t.Fatalf("build orchestrator: %v", err)
	}
	return orch
}

func TestRegisterBuildsLearnerWhenExtractorRegistered(t *testing.T) {
	RegisterExtractor("test-ex", &fakeExtractor{})
	orch := buildOrch(t, map[string]string{"session": "alpha", "memory.extractor": "test-ex"})
	if _, ok := orch.(*Learner); !ok {
		t.Fatalf("expected *Learner when an extractor is registered, got %T", orch)
	}
}

func TestRegisterFallsBackToCuratorWithoutExtractor(t *testing.T) {
	orch := buildOrch(t, map[string]string{"session": "alpha"})
	if _, ok := orch.(*Curator); !ok {
		t.Fatalf("expected plain *Curator with no extractor, got %T", orch)
	}
}

func TestRegisterIgnoresUnknownExtractorName(t *testing.T) {
	orch := buildOrch(t, map[string]string{"session": "alpha", "memory.extractor": "does-not-exist"})
	if _, ok := orch.(*Curator); !ok {
		t.Fatalf("expected *Curator for an unregistered extractor name, got %T", orch)
	}
}
