package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

const (
	defaultStaleDays   = 30
	defaultArchiveDays = 90
)

func init() {
	contracts.Register(contracts.Plugin{
		Manifest: contracts.Manifest{
			Kind:     "basic",
			Category: contracts.CategoryOrchestrator,
			Status:   contracts.StatusLive,
			Config: []contracts.Setting{
				{Key: "stale-days", Env: "AGENT_STALE_DAYS", Help: "days of no re-observation before a node is marked stale; <=0 disables (default 30)", Required: false},
				{Key: "archive-days", Env: "AGENT_ARCHIVE_DAYS", Help: "days of no re-observation before a node is archived; <=0 disables (default 90)", Required: false},
				{Key: "merge-min-nodes", Env: "MEMORY_MERGE_MIN", Help: "min nodes in a domain group before the merge pass folds them into an umbrella; <=0 disables (default 0, off)", Required: false},
				{Key: "merge-target", Env: "MEMORY_MERGE_TARGET", Help: "which nodes the merge pass considers: stale | active | all (default stale)", Required: false},
				{Key: "merge-max", Env: "MEMORY_MERGE_MAX", Help: "cap on nodes handed to the merger per domain group (default 40)", Required: false},
				{Key: "report-enabled", Env: "MEMORY_REPORT_ENABLED", Help: "write a REPORT node at the end of a Consolidate pass that made >=1 transition; false/0/off/no disables, true/1/on/yes enables, anything else keeps the default (default true)", Required: false},
				{Key: "report-prefix", Env: "MEMORY_REPORT_PREFIX", Help: "key prefix each report node is written under, a timestamp is appended (default reports/)", Required: false},
				{Key: "promote-min-age-days", Env: "MEMORY_PROMOTE_MIN_AGE_DAYS", Help: "days a private node's lastSeen must exceed its capturedAt before the curator promotes it to the shared project scope; <=0 disables (default 0, off)", Required: false},
				{Key: "idle-days", Env: "MEMORY_IDLE_DAYS", Help: "days since the last Consolidate run before the G5 inactivity trigger may fire; <=0 disables G5 (default 0, off)", Required: false},
				{Key: "idle-hours", Env: "MEMORY_IDLE_HOURS", Help: "hours of quiet (no observed turn) required, once idle-days has elapsed, before the idle trigger fires; only consulted when idle-days > 0; 0 removes the quiet-period gate so it fires as soon as idle-days elapses (default 2)", Required: false},
				{Key: "raw-archive", Env: "MEMORY_RAW_ARCHIVE", Help: "when true/1/on/yes, the learner archives one untruncated raw transcript node per turn (G7 full-text tier), surfaced only by memory search --raw; anything else keeps the default (default off, append-heavy)", Required: false},
				{Key: "learned-skills", Env: "MEMORY_LEARNED_SKILLS", Help: `when true/1/on/yes, the agent may write its own skills with <skill name="...">, the host projects them into the session worktree as SKILL.md, and using one keeps it alive; sharing one with the project's other agents still needs an explicit approval (default off)`, Required: false},
			},
		},
		Orchestrator: newOrchestrator,
	})
}

type SettingError struct {
	Key   string
	Value string
	Err   error
}

func (e SettingError) Error() string {
	return fmt.Sprintf("setting %s: %q is not an integer: %v", e.Key, e.Value, e.Err)
}

func (e SettingError) Unwrap() error {
	return e.Err
}

func intSetting(cfg contracts.PluginConfig, key string, def int) (int, error) {
	raw := cfg.Get(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, SettingError{Key: key, Value: raw, Err: err}
	}
	return n, nil
}

func staleDuration(v string, defaultDays int) time.Duration {
	days := defaultDays
	if v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			days = n
		}
	}
	return time.Duration(days) * 24 * time.Hour
}

func boolSetting(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "on", "yes":
		return true
	case "false", "0", "off", "no":
		return false
	default:
		return def
	}
}

func newOrchestrator(ctx context.Context, cfg contracts.PluginConfig, mem contracts.Memory) (contracts.Orchestrator, error) {
	var scope contracts.MemoryScope
	if p := cfg.Get("memory.project"); p != "" {
		scope.Project = contracts.ProjectKey(p)
	}
	if a := cfg.Get("memory.agent"); a != "" {
		scope.Agent = contracts.AgentKey(a)
	}
	if ex := lookupExtractor(cfg.Get("memory.extractor")); ex != nil {
		return newLearnerFromConfig(cfg, mem, scope, ex)
	}
	stale, archive := stalenessFromConfig(cfg)
	c := NewScoped(mem, cfg.Get("session"), scope)
	c.SetLogger(slog.Default())
	c.SetStaleness(stale, archive)
	c.SetLearnedSkills(boolSetting(cfg.Get("learned-skills"), false))
	return c, nil
}

func newLearnerFromConfig(cfg contracts.PluginConfig, mem contracts.Memory, scope contracts.MemoryScope, ex Extractor) (contracts.Orchestrator, error) {
	every, err := intSetting(cfg, "memory.consolidate-every", 0)
	if err != nil {
		return nil, err
	}
	l := NewLearner(mem, cfg.Get("session"), scope, ex, cfg.Get("memory.journal"), every)
	l.SetLogger(slog.Default())
	stale, archive := stalenessFromConfig(cfg)
	l.SetStaleness(stale, archive)
	if err := configureLearner(l, cfg); err != nil {
		return nil, err
	}
	return l, nil
}

func configureLearner(l *Learner, cfg contracts.PluginConfig) error {
	mergeMin, err := intSetting(cfg, "merge-min-nodes", 0)
	if err != nil {
		return err
	}
	mergeMax, err := intSetting(cfg, "merge-max", 0)
	if err != nil {
		return err
	}
	l.SetMerge(mergeMin, mergeMax, cfg.Get("merge-target"))
	l.SetReport(boolSetting(cfg.Get("report-enabled"), true), cfg.Get("report-prefix"))
	promoteDays, err := intSetting(cfg, "promote-min-age-days", 0)
	if err != nil {
		return err
	}
	l.SetPromote(time.Duration(promoteDays) * 24 * time.Hour)
	idleDays, err := intSetting(cfg, "idle-days", 0)
	if err != nil {
		return err
	}
	l.SetIdle(idleDays, idleHoursFromConfig(cfg))
	l.SetRawArchive(boolSetting(cfg.Get("raw-archive"), false))
	l.SetLearnedSkills(boolSetting(cfg.Get("learned-skills"), false))
	return nil
}

func idleHoursFromConfig(cfg contracts.PluginConfig) int {
	if v, err := strconv.Atoi(cfg.Get("idle-hours")); err == nil {
		return v
	}
	return 2
}

func stalenessFromConfig(cfg contracts.PluginConfig) (stale, archive time.Duration) {
	return staleDuration(cfg.Get("stale-days"), defaultStaleDays),
		staleDuration(cfg.Get("archive-days"), defaultArchiveDays)
}
