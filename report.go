package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

// ReportKind is the orchestrator-local Node.Kind used for a per-pass audit
// report. No contracts change: NodeKind is a plain string type and nothing in
// contracts/obsidian switches on Kind to gate behaviour — obsidian only
// compares it inside Query.Kinds filtering.
const ReportKind contracts.NodeKind = "report"

// defaultReportPrefix roots every report under one recallable key so `memory
// search --kind report` (existing generic Search) finds them; Sweep/Merge/
// Restore already exclude archived nodes from ordinary browsing, so reports
// living as ordinary nodes do not clutter Context/Recall (Context only ever
// recalls the session key + P1 scope roots, never an arbitrary Search).
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

// report writes one audit-trail node for the transitions this Consolidate
// pass made, if any. It is best-effort: disabled, a nil Memory, or a quiet
// pass with zero transitions all yield a clean no-op — the vault is never
// cluttered with empty reports. Each call writes a freshly-keyed node
// (reportPrefix + timestamp), never upserting a fixed key, so re-running
// Consolidate produces an append-only audit log rather than overwriting
// yesterday's report.
func (l *Learner) report(ctx context.Context) error {
	if !l.reportEnabled || len(l.transitions) == 0 || l.mem == nil {
		return nil
	}
	now := l.now().UTC()
	stamp := now.Format(time.RFC3339)
	// The key uses nanosecond precision so two passes landing in the same
	// wall-clock second (e.g. under forced back-to-back consolidation) still
	// get distinct keys — Record upserts by key, so a second-precision
	// collision would silently overwrite the earlier report and break the
	// append-only guarantee. The human-facing stamp stays second-precision.
	key := l.reportPrefix + now.Format(time.RFC3339Nano)
	return l.mem.Record(ctx, contracts.Node{
		Key:   key,
		Kind:  ReportKind,
		Title: "consolidate report " + stamp,
		Body:  renderReport(stamp, l.transitions),
	})
}

// renderReport renders the markdown body: a one-line header with the pass
// timestamp and per-Kind counts, then one table row per Transition.
func renderReport(stamp string, transitions []Transition) string {
	counts := map[string]int{}
	for _, tr := range transitions {
		counts[tr.Kind]++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# consolidate report %s\n\n", stamp)
	fmt.Fprintf(&b, "sweep=%d merge=%d restore=%d\n\n", counts["sweep"], counts["merge"], counts["restore"])
	b.WriteString("| key | kind | from | to |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, tr := range transitions {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", tr.Key, tr.Kind, tr.From, tr.To)
	}
	return b.String()
}
