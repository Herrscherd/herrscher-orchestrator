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
			return New(mem, cfg.Get("session")), nil
		},
	})
}
