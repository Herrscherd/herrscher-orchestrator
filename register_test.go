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
