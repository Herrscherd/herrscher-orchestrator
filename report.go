package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

// ReportKind is the orchestrator-local Node.Kind used for a per-pass audit
// report. No contracts change: NodeKind is a plain string type and nothing in
// contracts/obsidian switches on Kind to gate behaviour; obsidian only
// compares it inside Query.Kinds filtering.
const ReportKind contracts.NodeKind = "report"

const defaultReportPrefix = "reports/"

// SetReport configures G4 report emission. enabled=false is a clean no-op;
// prefix="" falls back to defaultReportPrefix.
func (l *Learner) SetReport(enabled bool, prefix string) {
	l.reportEnabled = enabled
	if prefix == "" {
		prefix = defaultReportPrefix
	}
	l.reportPrefix = prefix
}

func (l *Learner) report(ctx context.Context) error {
	if !l.reportEnabled || len(l.transitions) == 0 || l.mem == nil {
		return nil
	}
	now := l.now().UTC()
	stamp := now.Format(time.RFC3339)
	key := l.reportPrefix + now.Format("20060102T150405.000000000Z")
	return l.mem.Record(ctx, contracts.Node{
		Key:   key,
		Kind:  ReportKind,
		Title: "consolidate report " + stamp,
		Body:  renderReport(stamp, l.transitions),
	})
}

func renderReport(stamp string, transitions []Transition) string {
	counts := make(map[string]int)
	for _, tr := range transitions {
		counts[tr.Kind]++
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "# consolidate report %s\n\n", stamp)
	fmt.Fprintf(&builder, "sweep=%d merge=%d restore=%d\n\n", counts["sweep"], counts["merge"], counts["restore"])
	builder.WriteString("| key | kind | from | to |\n")
	builder.WriteString("|---|---|---|---|\n")
	for _, tr := range transitions {
		fmt.Fprintf(&builder, "| %s | %s | %s | %s |\n", tr.Key, tr.Kind, tr.From, tr.To)
	}
	return builder.String()
}
