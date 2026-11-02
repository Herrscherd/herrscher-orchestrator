package orchestrator

import (
	"context"
	"strings"
	"time"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

// MetaPromotedTo is set on a private original once its shared copy exists; its
// value is the Key of that copy. The original stays private and is never
// promoted again.
const MetaPromotedTo = "promotedTo"

// MetaPromotedFrom is set on a shared copy; its value is the Key of the private
// node it was promoted from.
const MetaPromotedFrom = "promotedFrom"

const metaCapturedAt = "capturedAt"

// SetPromote sets the minimum age, from capture to last use, that a private node
// must reach before it is promoted to the shared project scope. Zero or less
// disables promotion, the default.
func (l *Learner) SetPromote(minAge time.Duration) {
	l.promoteMinAge = minAge
}

func (l *Learner) promoteEligible(node contracts.Node) bool {
	state := node.Meta[contracts.MetaState]
	if state != "" && state != contracts.StateActive {
		return false
	}
	if node.Meta[MetaMergedInto] != "" || node.Meta[MetaPromotedTo] != "" {
		return false
	}
	if node.Kind == contracts.KindSkill && node.Meta[MetaApproved] == "" {
		return false
	}
	capturedAt, err1 := time.Parse(time.RFC3339, node.Meta[metaCapturedAt])
	lastSeen, err2 := time.Parse(time.RFC3339, node.Meta[contracts.MetaLastSeen])
	if err1 != nil || err2 != nil {
		return false
	}
	return lastSeen.Sub(capturedAt) >= l.promoteMinAge
}

func promotedKey(project, agentKey string) string {
	_, rest, _ := strings.Cut(agentKey, "/")
	_, tail, _ := strings.Cut(rest, "/")
	return project + "/" + tail
}

func cloneMeta(meta map[string]string) map[string]string {
	out := make(map[string]string, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	return out
}

func (l *Learner) sharedKeyTaken(ctx context.Context, target string) bool {
	recalled, err := l.mem.Recall(ctx, target, 0)
	if err != nil || recalled.Root.Key != target {
		return false
	}
	if recalled.Root.Body == "" && recalled.Root.Title == "" {
		return false
	}
	return recalled.Root.Meta[MetaPromotedFrom] == ""
}

func (l *Learner) applyPromotion(ctx context.Context, node contracts.Node) error {
	scope := l.scopeOf()
	dup := node
	dup.Key = promotedKey(scope.Project, node.Key)
	dup.Meta = cloneMeta(node.Meta)
	delete(dup.Meta, MetaPromotedTo)
	dup.Meta[MetaPromotedFrom] = node.Key
	if l.sharedKeyTaken(ctx, dup.Key) {
		l.log.Warn("memory: skipping promotion over an unrelated shared node", "from", node.Key, "to", dup.Key)
		return nil
	}
	if err := contracts.RecordShared(ctx, l.mem, scope, dup); err != nil {
		return err
	}
	if err := l.mem.Links(ctx, node.Key, dup.Key, "promoted-to"); err != nil {
		return err
	}
	if node.Meta == nil {
		node.Meta = make(map[string]string)
	}
	node.Meta[MetaPromotedTo] = dup.Key
	return l.mem.Record(ctx, node)
}

// Promote copies each eligible private node of this agent into the shared
// project scope, where peer agents see it. It is best-effort: a disabled pass or
// a missing scope is a no-op, and a failed node does not stop the rest.
func (l *Learner) Promote(ctx context.Context) error {
	scope := l.scopeOf()
	if l.promoteMinAge <= 0 || l.mem == nil || scope.Project == "" || scope.Agent == "" {
		return nil
	}
	nodes, err := l.mem.Search(ctx, contracts.Query{})
	if err != nil {
		return err
	}
	prefix := scope.Agent + "/"
	var firstErr error
	for _, node := range nodes {
		if !strings.HasPrefix(node.Key, prefix) {
			continue
		}
		if !l.promoteEligible(node) {
			continue
		}
		if err := l.applyPromotion(ctx, node); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
