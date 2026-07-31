package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

func TestStaleDuration(t *testing.T) {
	cases := []struct {
		in       string
		def      int
		wantDays float64
	}{
		{"", 30, 30},
		{"45", 30, 45},
		{"garbage", 30, 30},
		{"0", 30, 0},
		{"-5", 30, -5},
	}
	for _, c := range cases {
		got := staleDuration(c.in, c.def)
		if got != time.Duration(c.wantDays*24)*time.Hour {
			t.Fatalf("staleDuration(%q,%d) = %v, want %v days", c.in, c.def, got, c.wantDays)
		}
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
	found := map[string]string{}
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

// TestBoolSetting pins the single boolean parser: only the recognised
// spellings flip the switch; every other string — empty, unset, typo,
// garbage — falls through to the caller's explicit default.
func TestBoolSetting(t *testing.T) {
	cases := []struct {
		in   string
		def  bool
		want bool
	}{
		// recognised true spellings override either default
		{"true", false, true}, {"1", false, true}, {"on", false, true}, {"yes", false, true},
		{"true", true, true}, {"1", true, true}, {"on", true, true}, {"yes", true, true},
		// recognised false spellings override either default
		{"false", true, false}, {"0", true, false}, {"off", true, false}, {"no", true, false},
		{"false", false, false}, {"0", false, false}, {"off", false, false}, {"no", false, false},
		// case and surrounding whitespace are normalised, so an operator who
		// writes TRUE or " true " gets what they plainly meant
		{"TRUE", false, true}, {"False", true, false},
		{"  on  ", false, true}, {"\tOFF\n", true, false},
		// empty / unrecognised → default, identically in both directions
		{"", true, true}, {"", false, false},
		{"garbage", true, true}, {"garbage", false, false},
		{"2", true, true}, {"2", false, false},
	}
	for _, c := range cases {
		if got := boolSetting(c.in, c.def); got != c.want {
			t.Errorf("boolSetting(%q, %v) = %v, want %v", c.in, c.def, got, c.want)
		}
	}
}

// TestReportEnabledAndRawArchiveDefaults pins the two settings end to end
// through the plugin factory: their DEFAULTS differ (report on, raw off) but
// their PARSING is identical — an unrecognised or empty value leaves each at
// its own default rather than silently flipping it.
func TestReportEnabledAndRawArchiveDefaults(t *testing.T) {
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
