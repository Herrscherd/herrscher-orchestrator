package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

func TestStalenessFromConfigUsesDefaultsWhenUnsetAndFailsOnGarbage(t *testing.T) {
	unset := contracts.PluginConfig{}
	window, err := stalenessFromConfig(unset)
	if err != nil || window.stale != 30*24*time.Hour || window.archive != 90*24*time.Hour {
		t.Fatalf("unset staleness = %+v, %v; want the 30 and 90 day defaults", window, err)
	}

	garbage := contracts.PluginConfig{Settings: map[string]string{"stale-days": "garbage"}}
	_, err = stalenessFromConfig(garbage)
	if settingErr, ok := errors.AsType[*SettingError](err); !ok || settingErr.Key != "stale-days" {
		t.Fatalf("garbage stale-days error = %v, want a SettingError naming stale-days", err)
	}

	zero := contracts.PluginConfig{Settings: map[string]string{"archive-days": "0"}}
	if window, err := stalenessFromConfig(zero); err != nil || window.archive != 0 {
		t.Fatalf("archive-days=0 = %+v, %v; want a zero archive window (disabled) without error", window, err)
	}
}

func TestNewOrchestratorRefusesAnInvalidIdleHours(t *testing.T) {
	cfg := contracts.PluginConfig{Settings: map[string]string{"idle-days": "7", "idle-hours": "two"}}
	_, err := newLearnerFromConfig(cfg, newRec(), contracts.MemoryScope{}, plainExt{})
	if settingErr, ok := errors.AsType[*SettingError](err); !ok || settingErr.Key != "idle-hours" {
		t.Fatalf("invalid idle-hours error = %v, want a SettingError naming idle-hours", err)
	}
}

func TestManifestHasPromoteSetting(t *testing.T) {
	var found *contracts.Setting
	for _, p := range contracts.Default.Orchestrators() {
		if p.Manifest.Category != contracts.CategoryOrchestrator {
			continue
		}
		for i := range p.Manifest.Config {
			if p.Manifest.Config[i].Key == "promote-min-age-days" {
				found = &p.Manifest.Config[i]
			}
		}
	}
	if found == nil {
		t.Fatal("manifest missing promote-min-age-days setting")
	}
	if found.Env != "MEMORY_PROMOTE_MIN_AGE_DAYS" {
		t.Errorf("env = %q, want MEMORY_PROMOTE_MIN_AGE_DAYS", found.Env)
	}
}

func TestManifestHasIdleSettings(t *testing.T) {
	want := map[string]string{
		"idle-days":  "MEMORY_IDLE_DAYS",
		"idle-hours": "MEMORY_IDLE_HOURS",
	}
	found := make(map[string]string)
	for _, p := range contracts.Default.Orchestrators() {
		if p.Manifest.Category != contracts.CategoryOrchestrator {
			continue
		}
		for i := range p.Manifest.Config {
			s := p.Manifest.Config[i]
			if _, ok := want[s.Key]; ok {
				found[s.Key] = s.Env
			}
		}
	}
	for key, env := range want {
		if found[key] != env {
			t.Errorf("manifest setting %q: env=%q, want %q", key, found[key], env)
		}
	}
}

func TestManifestHasRawArchiveSetting(t *testing.T) {
	var found *contracts.Setting
	for _, o := range contracts.Default.Orchestrators() {
		for i := range o.Manifest.Config {
			if o.Manifest.Config[i].Key == "raw-archive" {
				found = &o.Manifest.Config[i]
			}
		}
	}
	if found == nil {
		t.Fatal("raw-archive setting missing from orchestrator manifest")
	}
	if found.Env != "MEMORY_RAW_ARCHIVE" {
		t.Fatalf("raw-archive Env = %q, want MEMORY_RAW_ARCHIVE", found.Env)
	}
}

func TestOrchestratorFactoryWiresStaleness(t *testing.T) {
	var plugin contracts.Plugin
	for _, p := range contracts.Default.Orchestrators() {
		if p.Orchestrator != nil {
			plugin = p
			break
		}
	}
	if plugin.Orchestrator == nil {
		t.Fatal("no orchestrator plugin registered")
	}
	cfg := contracts.PluginConfig{Settings: map[string]string{
		"session":      "s",
		"stale-days":   "10",
		"archive-days": "20",
	}}
	o, err := plugin.Orchestrator(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := o.(*Curator)
	if !ok {
		t.Fatalf("want *Curator, got %T", o)
	}
	if c.staleAfter != 10*24*time.Hour || c.archiveAfter != 20*24*time.Hour {
		t.Fatalf("staleness not wired: stale=%v archive=%v", c.staleAfter, c.archiveAfter)
	}
}

func TestBoolSettingReadsTruthAndFalsyWordsAndKeepsDefaultOnAnythingElse(t *testing.T) {
	cases := []struct {
		in   string
		def  bool
		want bool
	}{
		{"true", false, true},
		{"1", false, true},
		{"on", false, true},
		{"yes", false, true},
		{"true", true, true},
		{"1", true, true},
		{"on", true, true},
		{"yes", true, true},
		{"false", true, false},
		{"0", true, false},
		{"off", true, false},
		{"no", true, false},
		{"false", false, false},
		{"0", false, false},
		{"off", false, false},
		{"no", false, false},
		{"TRUE", false, true},
		{"False", true, false},
		{"  on  ", false, true},
		{"\tOFF\n", true, false},
		{"", true, true},
		{"", false, false},
		{"garbage", true, true},
		{"garbage", false, false},
		{"2", true, true},
		{"2", false, false},
	}
	for _, c := range cases {
		if got := boolSetting(c.in, c.def); got != c.want {
			t.Errorf("boolSetting(%q, %v) = %v, want %v", c.in, c.def, got, c.want)
		}
	}
}

func TestReportAndRawArchiveSettingsKeepTheirOwnDefaultWhenUnusable(t *testing.T) {
	RegisterExtractor("bool-ex", &fakeExtractor{})
	cases := []struct {
		name          string
		report        string
		raw           string
		wantReport    bool
		wantRawArchiv bool
	}{
		{"unset", "", "", true, false},
		{"explicit true", "true", "true", true, true},
		{"explicit false", "false", "false", false, false},
		{"1/0", "0", "1", false, true},
		{"on/off", "off", "on", false, true},
		{"garbage keeps each default", "garbage", "garbage", true, false},
		{"case is normalised, not ignored", "FALSE", "TRUE", false, true},
		{"whitespace is trimmed", " off ", " on ", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orch := buildOrch(t, map[string]string{
				"session":          "alpha",
				"memory.extractor": "bool-ex",
				"report-enabled":   c.report,
				"raw-archive":      c.raw,
			})
			l, ok := orch.(*Learner)
			if !ok {
				t.Fatalf("want *Learner, got %T", orch)
			}
			if l.reportEnabled != c.wantReport {
				t.Errorf("report-enabled=%q → reportEnabled=%v, want %v", c.report, l.reportEnabled, c.wantReport)
			}
			if l.rawArchive != c.wantRawArchiv {
				t.Errorf("raw-archive=%q → rawArchive=%v, want %v", c.raw, l.rawArchive, c.wantRawArchiv)
			}
		})
	}
}
