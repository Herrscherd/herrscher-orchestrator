package orchestrator

import (
	"context"
	"strconv"

	"github.com/Herrscherd/herrscher-contracts"
)

func init() {
	contracts.Register(contracts.Plugin{
		Manifest: contracts.Manifest{
			Kind:     "basic",
			Category: contracts.CategoryOrchestrator,
		},
		Orchestrator: func(ctx context.Context, cfg contracts.PluginConfig, mem contracts.Memory) (contracts.Orchestrator, error) {
			// The host passes the raw project/agent names (runtime state); we key
			// them onto the shared spine here: projects/<name>, agents/<name>.
			var scope contracts.MemoryScope
			if p := cfg.Get("memory.project"); p != "" {
				scope.Project = "projects/" + p
			}
			if a := cfg.Get("memory.agent"); a != "" {
				scope.Agent = "agents/" + a
			}
			// Opt into the learning loop when the host names a registered
			// extractor (the closed curation heuristics, plugged in by blank
			// import). Without one we keep the plain Curator, so an unconfigured
			// host is unaffected. memory.journal points at the call journal;
			// memory.consolidate-every runs Consolidate every N turns (0 = manual).
			if ex := lookupExtractor(cfg.Get("memory.extractor")); ex != nil {
				every, _ := strconv.Atoi(cfg.Get("memory.consolidate-every"))
				return NewLearner(mem, cfg.Get("session"), scope, ex, cfg.Get("memory.journal"), every), nil
			}
			return NewScoped(mem, cfg.Get("session"), scope), nil
		},
	})
}
