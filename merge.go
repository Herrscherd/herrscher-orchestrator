package orchestrator

import (
	"context"
	"log/slog"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

const defaultMergeMax = 40

// MetaMergedInto names the umbrella Key that subsumed a node, and is set on that node.
// It is a terminal marker: the node is kept on disk (reversible) but archived
// and excluded from recall. Orchestrator-internal; obsidian stores Meta
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
// maxNodes <= 0 falls back to defaultMergeMax; an unrecognised target falls back
// to "stale".
func (l *Learner) SetMerge(minNodes, maxNodes int, target string) {
	l.mergeMin = minNodes
	if maxNodes <= 0 {
		maxNodes = defaultMergeMax
	}
	l.mergeMax = maxNodes
	switch target {
	case "all", "active", "stale":
		l.mergeTarget = target
	default:
		l.mergeTarget = "stale"
	}
}

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
	merger, ok := l.merger()
	if !ok {
		return nil
	}
	nodes, err := l.mem.Search(ctx, contracts.Query{})
	if err != nil {
		return err
	}
	groups := mergeGroups(nodes, l.mergeEligible)
	existing := l.existingKeys(ctx, nodes)
	var firstErr error
	for domain, group := range groups {
		if err := l.mergeDomain(ctx, merger, domain, group, existing); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func mergeGroups(nodes []contracts.Node, eligible func(contracts.Node) bool) map[string][]contracts.Node {
	groups := make(map[string][]contracts.Node)
	for _, node := range nodes {
		if node.Meta[MetaMergedInto] != "" || !eligible(node) {
			continue
		}
		groups[node.Meta["domain"]] = append(groups[node.Meta["domain"]], node)
	}
	return groups
}

func (l *Learner) existingKeys(ctx context.Context, nodes []contracts.Node) map[string]bool {
	collisionSource := nodes
	if all, err := l.mem.Search(ctx, contracts.Query{IncludeArchived: true}); err != nil {
		slog.Warn("memory: merge collision set degraded to live nodes", "err", err)
	} else {
		collisionSource = all
	}
	existing := make(map[string]bool, len(collisionSource))
	for _, n := range collisionSource {
		existing[n.Key] = true
	}
	return existing
}

func (l *Learner) mergeDomain(ctx context.Context, merger Merger, domain string, group []contracts.Node, existing map[string]bool) error {
	if len(group) < l.mergeMin {
		return nil
	}
	if len(group) > l.mergeMax {
		group = group[:l.mergeMax]
	}
	umbrellas, err := merger.Merge(ctx, group)
	if err != nil {
		slog.Warn("memory: merge group failed", "domain", domain, "size", len(group), "err", err)
		return err
	}
	var firstErr error
	for _, u := range umbrellas {
		if err := l.applyUmbrella(ctx, u, group, existing); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func mergeExcludedKind(k contracts.NodeKind) bool {
	return k == ReportKind || k == contracts.KindSession || k == contracts.KindTranscript
}

func (l *Learner) mergeEligible(node contracts.Node) bool {
	if mergeExcludedKind(node.Kind) {
		return false
	}
	if node.Meta[MetaPromotedTo] != "" {
		return false
	}
	state := node.Meta[contracts.MetaState]
	if state == "" {
		state = contracts.StateActive
	}
	switch l.mergeTarget {
	case "all":
		return state != contracts.StateArchived
	case "active":
		return state == contracts.StateActive
	default:
		return state == contracts.StateStale
	}
}

func (l *Learner) applyUmbrella(ctx context.Context, umbrella Umbrella, group []contracts.Node, existing map[string]bool) error {
	byKey := make(map[string]contracts.Node, len(group))
	for _, n := range group {
		byKey[n.Key] = n
	}
	if !l.validUmbrella(umbrella, byKey, existing) {
		return nil
	}
	if err := l.mem.Record(ctx, umbrella.Node); err != nil {
		return err
	}
	existing[umbrella.Node.Key] = true
	l.transitions = append(l.transitions, Transition{Key: umbrella.Node.Key, From: "", To: contracts.StateActive, Kind: "merge"})
	return l.archiveMerged(ctx, umbrella, byKey)
}

func (l *Learner) archiveMerged(ctx context.Context, umbrella Umbrella, byKey map[string]contracts.Node) error {
	var firstErr error
	applied := make(map[string]bool, len(umbrella.Merged))
	for _, mergedKey := range umbrella.Merged {
		if applied[mergedKey] {
			continue
		}
		applied[mergedKey] = true
		if err := l.archiveOriginal(ctx, umbrella, mergedKey, byKey[mergedKey]); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (l *Learner) archiveOriginal(ctx context.Context, umbrella Umbrella, key string, orig contracts.Node) error {
	prevState := orig.Meta[contracts.MetaState]
	if prevState == "" {
		prevState = contracts.StateActive
	}
	if orig.Meta == nil {
		orig.Meta = make(map[string]string)
	}
	orig.Meta[MetaMergedInto] = umbrella.Node.Key
	orig.Meta[contracts.MetaState] = contracts.StateArchived
	if err := l.mem.Record(ctx, orig); err != nil {
		return err
	}
	l.transitions = append(l.transitions, Transition{Key: key, From: prevState, To: contracts.StateArchived, Kind: "merge"})
	return l.mem.Links(ctx, key, umbrella.Node.Key, "merged-into")
}

func (l *Learner) validUmbrella(umbrella Umbrella, byKey map[string]contracts.Node, existing map[string]bool) bool {
	reason := ""
	switch {
	case umbrella.Node.Key == "":
		reason = "empty umbrella key"
	case umbrella.Node.Body == "":
		reason = "empty umbrella body"
	}
	if reason == "" {
		if existing[umbrella.Node.Key] {
			reason = "umbrella key collides with an existing node"
		}
	}
	if reason == "" {
		distinct := make(map[string]bool)
		for _, mergedKey := range umbrella.Merged {
			if _, ok := byKey[mergedKey]; !ok {
				reason = "merged key outside candidate group"
				break
			}
			distinct[mergedKey] = true
		}
		if reason == "" && len(distinct) < 2 {
			reason = "fewer than 2 distinct originals"
		}
	}
	if reason != "" {
		slog.Warn("memory: rejecting invalid umbrella",
			"key", umbrella.Node.Key, "reason", reason, "merged", len(umbrella.Merged))
		return false
	}
	return true
}
