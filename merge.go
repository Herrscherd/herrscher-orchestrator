package orchestrator

import (
	"context"
	"log/slog"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

const defaultMergeMax = 40

// MetaMergedInto, when set on a node, names the umbrella Key that subsumed it.
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
	nodes, err := l.mem.Search(ctx, contracts.Query{})
	if err != nil {
		return err
	}
	groups := map[string][]contracts.Node{}
	for _, n := range nodes {
		if n.Meta[MetaMergedInto] != "" {
			continue
		}
		if !l.mergeEligible(n) {
			continue
		}
		groups[n.Meta["domain"]] = append(groups[n.Meta["domain"]], n)
	}
	collisionSource := nodes
	if all, aerr := l.mem.Search(ctx, contracts.Query{IncludeArchived: true}); aerr != nil {
		slog.Warn("memory: merge collision set degraded to live nodes", "err", aerr)
	} else {
		collisionSource = all
	}
	existing := make(map[string]bool, len(collisionSource))
	for _, n := range collisionSource {
		existing[n.Key] = true
	}
	var firstErr error
	for domain, group := range groups {
		if len(group) < l.mergeMin {
			continue
		}
		if len(group) > l.mergeMax {
			group = group[:l.mergeMax]
		}
		umbrellas, merr := m.Merge(ctx, group)
		if merr != nil {
			slog.Warn("memory: merge group failed", "domain", domain, "size", len(group), "err", merr)
			if firstErr == nil {
				firstErr = merr
			}
			continue
		}
		for _, u := range umbrellas {
			if aerr := l.applyUmbrella(ctx, u, group, existing); aerr != nil && firstErr == nil {
				firstErr = aerr
			}
		}
	}
	return firstErr
}

func mergeExcludedKind(k contracts.NodeKind) bool {
	return k == ReportKind || k == contracts.KindSession || k == contracts.KindTranscript
}

func (l *Learner) mergeEligible(n contracts.Node) bool {
	if mergeExcludedKind(n.Kind) {
		return false
	}
	if n.Meta[MetaPromotedTo] != "" {
		return false
	}
	state := n.Meta[contracts.MetaState]
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

func (l *Learner) applyUmbrella(ctx context.Context, u Umbrella, group []contracts.Node, existing map[string]bool) error {
	byKey := make(map[string]contracts.Node, len(group))
	for _, n := range group {
		byKey[n.Key] = n
	}
	if !l.validUmbrella(u, byKey, existing) {
		return nil
	}
	if err := l.mem.Record(ctx, u.Node); err != nil {
		return err
	}
	existing[u.Node.Key] = true
	l.transitions = append(l.transitions, Transition{Key: u.Node.Key, From: "", To: contracts.StateActive, Kind: "merge"})
	var firstErr error
	applied := make(map[string]bool, len(u.Merged))
	for _, k := range u.Merged {
		if applied[k] {
			continue
		}
		applied[k] = true
		orig := byKey[k]
		prevState := orig.Meta[contracts.MetaState]
		if prevState == "" {
			prevState = contracts.StateActive
		}
		if orig.Meta == nil {
			orig.Meta = map[string]string{}
		}
		orig.Meta[MetaMergedInto] = u.Node.Key
		orig.Meta[contracts.MetaState] = contracts.StateArchived
		if err := l.mem.Record(ctx, orig); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		l.transitions = append(l.transitions, Transition{Key: k, From: prevState, To: contracts.StateArchived, Kind: "merge"})
		if err := l.mem.Links(ctx, k, u.Node.Key, "merged-into"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (l *Learner) validUmbrella(u Umbrella, byKey map[string]contracts.Node, existing map[string]bool) bool {
	reason := ""
	switch {
	case u.Node.Key == "":
		reason = "empty umbrella key"
	case u.Node.Body == "":
		reason = "empty umbrella body"
	}
	if reason == "" {
		if existing[u.Node.Key] {
			reason = "umbrella key collides with an existing node"
		}
	}
	if reason == "" {
		distinct := map[string]bool{}
		for _, k := range u.Merged {
			if _, ok := byKey[k]; !ok {
				reason = "merged key outside candidate group"
				break
			}
			distinct[k] = true
		}
		if reason == "" && len(distinct) < 2 {
			reason = "fewer than 2 distinct originals"
		}
	}
	if reason != "" {
		slog.Warn("memory: rejecting invalid umbrella",
			"key", u.Node.Key, "reason", reason, "merged", len(u.Merged))
		return false
	}
	return true
}
