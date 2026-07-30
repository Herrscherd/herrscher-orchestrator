package orchestrator

import (
	"context"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
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
		stamp := n.Meta[contracts.MetaLastSeen]
		if stamp == "" {
			stamp = n.Meta["capturedAt"]
		}
		if stamp == "" {
			continue // no age basis
		}
		lastSeen, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			continue // unparseable timestamp
		}
		next := contracts.NextState(lastSeen, now, c.staleAfter, c.archiveAfter)
		cur := n.Meta[contracts.MetaState]
		if cur == "" {
			cur = contracts.StateActive
		}
		if next == cur {
			continue // no change: don't rewrite
		}
		if n.Meta == nil {
			n.Meta = map[string]string{}
		}
		n.Meta[contracts.MetaState] = next
		// Re-supply lastSeen so the state-only write does not reset the age.
		if n.Meta[contracts.MetaLastSeen] == "" {
			n.Meta[contracts.MetaLastSeen] = stamp
		}
		// Best-effort: a per-node write failure (e.g. a node that trips the
		// obsidian budget) must not abort the rest of the sweep, or one bad
		// node would freeze decay for every node after it, every pass. Record
		// the first error and keep going; Consolidate swallows the return.
		if err := c.mem.Record(ctx, n); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
