package orchestrator

import (
	"context"

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
			return NewScoped(mem, cfg.Get("session"), scope), nil
		},
	})
}
