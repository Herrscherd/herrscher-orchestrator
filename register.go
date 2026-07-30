package orchestrator

import (
	"context"
	"strconv"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

func init() {
	contracts.Register(contracts.Plugin{
		Manifest: contracts.Manifest{
			Kind:     "basic",
			Category: contracts.CategoryOrchestrator,
			Config: []contracts.Setting{
				{Key: "stale-days", Env: "AGENT_STALE_DAYS", Help: "days of no re-observation before a node is marked stale; <=0 disables (default 30)", Required: false},
				{Key: "archive-days", Env: "AGENT_ARCHIVE_DAYS", Help: "days of no re-observation before a node is archived; <=0 disables (default 90)", Required: false},
			},
		},
		Orchestrator: func(ctx context.Context, cfg contracts.PluginConfig, mem contracts.Memory) (contracts.Orchestrator, error) {
			// The host passes the raw project/agent names (runtime state); we key
			// them onto the shared spine via contracts.ProjectKey/AgentKey
			// (single source of truth).
			var scope contracts.MemoryScope
			if p := cfg.Get("memory.project"); p != "" {
				scope.Project = contracts.ProjectKey(p)
			}
			if a := cfg.Get("memory.agent"); a != "" {
				scope.Agent = contracts.AgentKey(a)
			}
			stale := staleDuration(cfg.Get("stale-days"), 30)
			archive := staleDuration(cfg.Get("archive-days"), 90)
			// Opt into the learning loop when the host names a registered
			// extractor (the closed curation heuristics, plugged in by blank
			// import). Without one we keep the plain Curator, so an unconfigured
			// host is unaffected. memory.journal points at the call journal;
			// memory.consolidate-every runs Consolidate every N turns (0 = manual).
			if ex := lookupExtractor(cfg.Get("memory.extractor")); ex != nil {
				every, _ := strconv.Atoi(cfg.Get("memory.consolidate-every"))
				l := NewLearner(mem, cfg.Get("session"), scope, ex, cfg.Get("memory.journal"), every)
				l.SetStaleness(stale, archive)
				return l, nil
			}
			c := NewScoped(mem, cfg.Get("session"), scope)
			c.SetStaleness(stale, archive)
			return c, nil
		},
	})
}

// staleDuration parses an integer-days config value into a Duration. Empty or
// unparseable → the default days. A value <= 0 is preserved (NextState treats
// it as "disable this transition").
func staleDuration(v string, defaultDays int) time.Duration {
	days := defaultDays
	if v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			days = n
		}
	}
	return time.Duration(days) * 24 * time.Hour
}
