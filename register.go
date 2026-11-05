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
	defaultIdleHours   = 2
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

// SettingError reports a plugin setting whose value is not a valid integer. It
// unwraps to the parse error.
type SettingError struct {
	Key   string
	Value string
	Err   error
}

func (e *SettingError) Error() string {
	return fmt.Sprintf("setting %s: %q is not an integer: %v", e.Key, e.Value, e.Err)
}

func (e *SettingError) Unwrap() error {
	return e.Err
}

// Notice returns the setting.invalid notice naming the setting and its value.
func (e *SettingError) Notice() contracts.Notice {
	return contracts.Notice{
		Code: contracts.CodeSettingInvalid,
		Args: []contracts.Arg{contracts.Text("Setting", e.Key), contracts.Text("Value", e.Value)},
	}
}

func intSetting(cfg contracts.PluginConfig, key string, def int) (int, error) {
	raw := cfg.Get(key)
	if raw == "" {
		return def, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &SettingError{Key: key, Value: raw, Err: err}
	}
	return value, nil
}

func daysDuration(n int) time.Duration {
	return time.Duration(n) * 24 * time.Hour
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

type staleness struct {
	stale   time.Duration
	archive time.Duration
}

func stalenessFromConfig(cfg contracts.PluginConfig) (staleness, error) {
	staleDays, err := intSetting(cfg, "stale-days", defaultStaleDays)
	if err != nil {
		return staleness{}, err
	}
	archiveDays, err := intSetting(cfg, "archive-days", defaultArchiveDays)
	if err != nil {
		return staleness{}, err
	}
	return staleness{stale: daysDuration(staleDays), archive: daysDuration(archiveDays)}, nil
}

func newOrchestrator(_ context.Context, cfg contracts.PluginConfig, mem contracts.Memory) (contracts.Orchestrator, error) {
	var scope contracts.MemoryScope
	if project := cfg.Get("memory.project"); project != "" {
		scope.Project = contracts.ProjectKey(project)
	}
	if agent := cfg.Get("memory.agent"); agent != "" {
		scope.Agent = contracts.AgentKey(agent)
	}
	if extractor := lookupExtractor(cfg.Get("memory.extractor")); extractor != nil {
		return newLearnerFromConfig(cfg, mem, scope, extractor)
	}
	window, err := stalenessFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	curator := NewScoped(mem, cfg.Get("session"), scope)
	curator.SetLogger(slog.Default())
	curator.SetStaleness(window.stale, window.archive)
	curator.SetLearnedSkills(boolSetting(cfg.Get("learned-skills"), false))
	return curator, nil
}

func newLearnerFromConfig(cfg contracts.PluginConfig, mem contracts.Memory, scope contracts.MemoryScope, extractor Extractor) (contracts.Orchestrator, error) {
	every, err := intSetting(cfg, "memory.consolidate-every", 0)
	if err != nil {
		return nil, err
	}
	window, err := stalenessFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	learner := NewLearner(mem, cfg.Get("session"), scope, extractor, cfg.Get("memory.journal"), every)
	learner.SetLogger(slog.Default())
	learner.SetStaleness(window.stale, window.archive)
	if err := configureLearner(learner, cfg); err != nil {
		return nil, err
	}
	return learner, nil
}

func configureLearner(learner *Learner, cfg contracts.PluginConfig) error {
	mergeMin, err := intSetting(cfg, "merge-min-nodes", 0)
	if err != nil {
		return err
	}
	mergeMax, err := intSetting(cfg, "merge-max", 0)
	if err != nil {
		return err
	}
	learner.SetMerge(mergeMin, mergeMax, cfg.Get("merge-target"))
	learner.SetReport(boolSetting(cfg.Get("report-enabled"), true), cfg.Get("report-prefix"))
	promoteDays, err := intSetting(cfg, "promote-min-age-days", 0)
	if err != nil {
		return err
	}
	learner.SetPromote(daysDuration(promoteDays))
	idleDays, err := intSetting(cfg, "idle-days", 0)
	if err != nil {
		return err
	}
	idleHours, err := intSetting(cfg, "idle-hours", defaultIdleHours)
	if err != nil {
		return err
	}
	learner.SetIdle(idleDays, idleHours)
	learner.SetRawArchive(boolSetting(cfg.Get("raw-archive"), false))
	learner.SetLearnedSkills(boolSetting(cfg.Get("learned-skills"), false))
	return nil
}
