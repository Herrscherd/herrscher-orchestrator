package orchestrator

import (
	"context"
	"time"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

// Sweep re-derives each node's lifecycle state from its last-seen time and
// persists the changes. It is best-effort: an error on one node does not stop
// the others.
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

func (c *Curator) sweepNode(ctx context.Context, node contracts.Node, now time.Time) error {
	if node.Meta[MetaMergedInto] != "" || node.Kind == ReportKind {
		return nil
	}
	stamp := lastSeenStamp(node)
	if stamp == "" {
		return nil
	}
	lastSeen, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		c.log.Warn("sweep skipped a node with an unreadable last-seen stamp", "key", node.Key, "stamp", stamp, "error", err)
		return nil
	}
	next := contracts.NextState(lastSeen, now, c.staleAfter, c.archiveAfter)
	cur := node.Meta[contracts.MetaState]
	if cur == "" {
		cur = contracts.StateActive
	}
	if next == cur {
		return nil
	}
	if node.Meta == nil {
		node.Meta = make(map[string]string)
	}
	node.Meta[contracts.MetaState] = next
	if node.Meta[contracts.MetaLastSeen] == "" {
		node.Meta[contracts.MetaLastSeen] = stamp
	}
	if err := c.mem.Record(ctx, node); err != nil {
		return err
	}
	c.transitions = append(c.transitions, Transition{Key: node.Key, From: cur, To: next, Kind: TransitionSweep})
	return nil
}

func lastSeenStamp(node contracts.Node) string {
	if stamp := node.Meta[contracts.MetaLastSeen]; stamp != "" {
		return stamp
	}
	return node.Meta[metaCapturedAt]
}
