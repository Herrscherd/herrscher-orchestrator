package orchestrator

import (
	"context"
	"log/slog"

	"github.com/Herrscherd/herrscher-contracts"
)

// defaultMergeMax caps the number of nodes handed to a Merger per domain group,
// bounding the closed LLM pass's prompt size / cost when a caller passes max<=0.
const defaultMergeMax = 40

// MetaMergedInto, when set on a node, names the umbrella Key that subsumed it.
// It is a terminal marker: the node is kept on disk (reversible) but archived
// and excluded from recall. Orchestrator-internal — obsidian stores Meta
// generically, so no contracts change is needed.
const MetaMergedInto = "mergedInto"

// Umbrella is one merge proposal from a Merger: a fused node (Node) that
// subsumes the originals named by Merged (their Keys, >= 2). The plumbing writes
// Node, then labels, links, and archives each original. The fused node's
// Key/Title/Body/Meta and the overlap decision are the closed merger's to make;
// this package only validates and applies.
type Umbrella struct {
	Node   contracts.Node
	Merged []string
}

// Merger fuses semantically overlapping candidates into umbrella nodes (memory
// roadmap G2). Given a pre-filtered, single-domain slice of candidates it returns
// zero or more umbrellas; an empty result means "nothing worth merging". The
// heuristics are the closed part of the moat; this package defines only the seam
// and the open plumbing (Learner.Merge) that drives it.
type Merger interface {
	Merge(ctx context.Context, cands []contracts.Node) ([]Umbrella, error)
}

// SetMerge configures the G2 semantic-merge pass. minNodes <= 0 disables it;
// max <= 0 falls back to defaultMergeMax; an unrecognised target falls back to
// "stale".
func (l *Learner) SetMerge(minNodes, max int, target string) {
	l.mergeMin = minNodes
	if max <= 0 {
		max = defaultMergeMax
	}
	l.mergeMax = max
	switch target {
	case "all", "active", "stale":
		l.mergeTarget = target
	default:
		l.mergeTarget = "stale"
	}
}

// merger returns the Merger the extractor also implements, if any. The closed
// extractor typically implements Extract, Consolidate, and Merge, so the merge
// pass needs no new constructor parameter.
func (l *Learner) merger() (Merger, bool) {
	m, ok := l.extract.(Merger)
	return m, ok
}

// Merge groups eligible non-archived nodes by Meta["domain"] and folds each
// group of at least mergeMin nodes into an umbrella via the wired Merger. It is
// best-effort: disabled (mergeMin<=0), no merger, or nil Memory all yield a clean
// no-op, and a per-group/per-node error is recorded but never aborts the rest.
func (l *Learner) Merge(ctx context.Context) error {
	if l.mergeMin <= 0 || l.mem == nil {
		return nil
	}
	m, ok := l.merger()
	if !ok {
		return nil
	}
	nodes, err := l.mem.Search(ctx, contracts.Query{}) // active+stale, never archived
	if err != nil {
		return err
	}
	groups := map[string][]contracts.Node{}
	for _, n := range nodes {
		if n.Meta[MetaMergedInto] != "" {
			continue // already folded; terminal
		}
		if !l.mergeEligible(n) {
			continue
		}
		groups[n.Meta["domain"]] = append(groups[n.Meta["domain"]], n)
	}
	var firstErr error
	for _, group := range groups {
		if len(group) < l.mergeMin {
			continue
		}
		if len(group) > l.mergeMax {
			group = group[:l.mergeMax]
		}
		umbrellas, merr := m.Merge(ctx, group)
		if merr != nil {
			if firstErr == nil {
				firstErr = merr // record and keep going: one bad group must not
			} // abort the others
			continue
		}
		for _, u := range umbrellas {
			if aerr := l.applyUmbrella(ctx, u, group); aerr != nil && firstErr == nil {
				firstErr = aerr
			}
		}
	}
	return firstErr
}

// mergeEligible reports whether node n is in scope for the current merge target.
// "all" = anything not archived; "active" = active/absent-state only; "stale"
// (default) = stale only.
func (l *Learner) mergeEligible(n contracts.Node) bool {
	state := n.Meta[contracts.MetaState]
	if state == "" {
		state = contracts.StateActive
	}
	switch l.mergeTarget {
	case "all":
		return state != contracts.StateArchived
	case "active":
		return state == contracts.StateActive
	default: // "stale"
		return state == contracts.StateStale
	}
}

// applyUmbrella validates one Umbrella against the group it came from, then — if
// valid — writes the fused node and labels/archives/links each original. A
// malformed proposal is rejected (WARN, skipped) so it cannot corrupt the graph
// or drop valid proposals from the same batch. Per-original write failures are
// best-effort (record first, continue) so one bad original never strands the
// umbrella or its siblings.
func (l *Learner) applyUmbrella(ctx context.Context, u Umbrella, group []contracts.Node) error {
	byKey := make(map[string]contracts.Node, len(group))
	for _, n := range group {
		byKey[n.Key] = n
	}
	if !l.validUmbrella(u, byKey) {
		return nil // rejected (WARN already emitted)
	}
	if err := l.mem.Record(ctx, u.Node); err != nil {
		return err
	}
	var firstErr error
	for _, k := range u.Merged {
		orig := byKey[k]
		if orig.Meta == nil {
			orig.Meta = map[string]string{}
		}
		orig.Meta[MetaMergedInto] = u.Node.Key
		orig.Meta[contracts.MetaState] = contracts.StateArchived
		// orig already carries its lastSeen from Search; re-recording with it
		// present keeps obsidian's per-write stamp from bumping the age (same
		// contract Sweep relies on).
		if err := l.mem.Record(ctx, orig); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := l.mem.Links(ctx, k, u.Node.Key, "merged-into"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// validUmbrella rejects (WARN + false) an umbrella that would corrupt the graph:
// empty Key/Body, fewer than 2 merged originals, a Key that collides with a
// candidate node (an umbrella must be a NEW node, never an original reused), or a
// merged Key outside the candidate group.
func (l *Learner) validUmbrella(u Umbrella, byKey map[string]contracts.Node) bool {
	reason := ""
	switch {
	case u.Node.Key == "":
		reason = "empty umbrella key"
	case u.Node.Body == "":
		reason = "empty umbrella body"
	case len(u.Merged) < 2:
		reason = "fewer than 2 originals"
	}
	if reason == "" {
		if _, collides := byKey[u.Node.Key]; collides {
			reason = "umbrella key reuses a candidate node"
		}
	}
	if reason == "" {
		for _, k := range u.Merged {
			if _, ok := byKey[k]; !ok {
				reason = "merged key outside candidate group"
				break
			}
		}
	}
	if reason != "" {
		slog.Warn("memory: rejecting invalid umbrella",
			"key", u.Node.Key, "reason", reason, "merged", len(u.Merged))
		return false
	}
	return true
}
