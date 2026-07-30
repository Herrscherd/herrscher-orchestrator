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
				{Key: "merge-min-nodes", Env: "MEMORY_MERGE_MIN", Help: "min nodes in a domain group before the merge pass folds them into an umbrella; <=0 disables (default 0, off)", Required: false},
				{Key: "merge-target", Env: "MEMORY_MERGE_TARGET", Help: "which nodes the merge pass considers: stale | active | all (default stale)", Required: false},
				{Key: "merge-max", Env: "MEMORY_MERGE_MAX", Help: "cap on nodes handed to the merger per domain group (default 40)", Required: false},
				{Key: "report-enabled", Env: "MEMORY_REPORT_ENABLED", Help: "write a REPORT node at the end of a Consolidate pass that made >=1 transition; false/0/off disables (default true)", Required: false},
				{Key: "report-prefix", Env: "MEMORY_REPORT_PREFIX", Help: "key prefix each report node is written under, a timestamp is appended (default reports/)", Required: false},
				{Key: "promote-min-age-days", Env: "MEMORY_PROMOTE_MIN_AGE_DAYS", Help: "days a private node's lastSeen must exceed its capturedAt before the curator promotes it to the shared project scope; <=0 disables (default 0, off)", Required: false},
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
				mergeMin, _ := strconv.Atoi(cfg.Get("merge-min-nodes"))
				mergeMax, _ := strconv.Atoi(cfg.Get("merge-max"))
				l.SetMerge(mergeMin, mergeMax, cfg.Get("merge-target"))
				reportEnabled := cfg.Get("report-enabled") != "false" && cfg.Get("report-enabled") != "0" && cfg.Get("report-enabled") != "off"
				l.SetReport(reportEnabled, cfg.Get("report-prefix"))
				promoteDays, _ := strconv.Atoi(cfg.Get("promote-min-age-days"))
				l.SetPromote(time.Duration(promoteDays) * 24 * time.Hour)
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
