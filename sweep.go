package orchestrator

import (
	"context"
	"time"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

// Sweep re-derives every node's lifecycle state from its lastSeen timestamp and
// persists any change. It is deterministic (clock injected via Curator.now) and
// best-effort: callers on the turn path must not fail a turn if Sweep errors.
//
// It enumerates archived nodes too (Query.IncludeArchived) so a re-observed node
// can transition back out of archived. A node with neither a lastSeen nor a
// capturedAt stamp has no age basis and is skipped. Unchanged nodes are never
// rewritten (no churn). When a state change is written, the existing lastSeen is
// re-supplied so obsidian's per-write lastSeen stamp does not bump the node's age
// (which would spuriously reactivate it).
func (c *Curator) Sweep(ctx context.Context) error {
	if c.mem == nil {
		return nil
	}
	nodes, err := c.mem.Search(ctx, contracts.Query{IncludeArchived: true})
	if err != nil {
		return err
	}
	now := c.now().UTC()
	var firstErr error
	for _, n := range nodes {
		if err := c.sweepNode(ctx, n, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *Curator) sweepNode(ctx context.Context, n contracts.Node, now time.Time) error {
	if n.Meta[MetaMergedInto] != "" || n.Kind == ReportKind {
		return nil
	}
	stamp := lastSeenStamp(n)
	if stamp == "" {
		return nil
	}
	lastSeen, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return nil
	}
	next := contracts.NextState(lastSeen, now, c.staleAfter, c.archiveAfter)
	cur := n.Meta[contracts.MetaState]
	if cur == "" {
		cur = contracts.StateActive
	}
	if next == cur {
		return nil
	}
	if n.Meta == nil {
		n.Meta = map[string]string{}
	}
	n.Meta[contracts.MetaState] = next
	if n.Meta[contracts.MetaLastSeen] == "" {
		n.Meta[contracts.MetaLastSeen] = stamp
	}
	if err := c.mem.Record(ctx, n); err != nil {
		return err
	}
	c.transitions = append(c.transitions, Transition{Key: n.Key, From: cur, To: next, Kind: "sweep"})
	return nil
}

func lastSeenStamp(n contracts.Node) string {
	if stamp := n.Meta[contracts.MetaLastSeen]; stamp != "" {
		return stamp
	}
	return n.Meta["capturedAt"]
}
