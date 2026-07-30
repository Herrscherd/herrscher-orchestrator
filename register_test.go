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
