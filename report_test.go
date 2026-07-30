package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

func TestReportSkippedWhenNoTransitions(t *testing.T) {
	mem := &mergeMem{}
	l := NewLearner(mem, "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	l.SetReport(true, "")
	if err := l.report(context.Background()); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(mem.records) != 0 {
		t.Fatalf("a quiet pass must write 0 reports; got %d", len(mem.records))
	}
}

func TestReportWrittenWithRightShape(t *testing.T) {
	mem := &mergeMem{}
	l := NewLearner(mem, "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	l.SetReport(true, "reports/")
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	l.transitions = []Transition{
		{Key: "facts/a", From: contracts.StateActive, To: contracts.StateStale, Kind: "sweep"},
		{Key: "facts/b", From: contracts.StateStale, To: contracts.StateArchived, Kind: "merge"},
	}
	if err := l.report(context.Background()); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(mem.records) != 1 {
		t.Fatalf("want exactly 1 report record, got %d", len(mem.records))
	}
	r := mem.records[0]
	if r.Kind != ReportKind {
		t.Errorf("kind = %q, want %q", r.Kind, ReportKind)
	}
	if !strings.HasPrefix(r.Key, "reports/") {
		t.Errorf("key %q does not start with the configured prefix", r.Key)
	}
	for _, key := range []string{"facts/a", "facts/b"} {
		if !strings.Contains(r.Body, key) {
			t.Errorf("body missing transitioned key %q:\n%s", key, r.Body)
		}
	}
}

func TestReportDisabledNoWrite(t *testing.T) {
	mem := &mergeMem{}
	l := NewLearner(mem, "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	l.SetReport(false, "")
	l.transitions = []Transition{{Key: "facts/a", From: "", To: contracts.StateStale, Kind: "sweep"}}
	if err := l.report(context.Background()); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(mem.records) != 0 {
		t.Fatalf("a disabled report must not write; got %d", len(mem.records))
	}
}

func TestSetReportDefaultPrefix(t *testing.T) {
	l := NewLearner(&mergeMem{}, "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	l.SetReport(true, "")
	if l.reportPrefix != defaultReportPrefix {
		t.Errorf("reportPrefix = %q, want %q", l.reportPrefix, defaultReportPrefix)
	}
}

func TestConsolidateResetsTransitionsRegardless(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	mem := &mergeMem{nodes: []contracts.Node{
		{Key: "old", Meta: map[string]string{contracts.MetaLastSeen: now.Add(-45 * 24 * time.Hour).Format(time.RFC3339)}},
	}}
	l := NewLearner(mem, "s", contracts.MemoryScope{}, plainExt{}, "", 0)
	l.now = func() time.Time { return now }
	l.SetStaleness(30*24*time.Hour, 90*24*time.Hour)
	l.SetReport(true, "")
	if err := l.Consolidate(context.Background()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if l.transitions != nil {
		t.Fatalf("transitions not reset after Consolidate: %+v", l.transitions)
	}
	var sawReport bool
	for _, r := range mem.records {
		if r.Kind == ReportKind {
			sawReport = true
		}
	}
	if !sawReport {
		t.Fatal("expected a report node after a pass that made a sweep transition")
	}
}
