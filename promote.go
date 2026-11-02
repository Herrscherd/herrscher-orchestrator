package orchestrator

import (
	"context"
	"strings"
	"time"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

// MetaPromotedTo names the Key of the shared copy that a private node was
// promoted to. It is a terminal marker on the ORIGINAL: the original is
// kept (reversible, still private, still usable by its own agent) but is never
// re-promoted. Orchestrator-internal; obsidian stores Meta generically, so no
// contracts change is needed.
const MetaPromotedTo = "promotedTo"

// MetaPromotedFrom names, on a shared copy, the Key of the private node it
// was promoted from (the back-pointer counterpart to MetaPromotedTo).
const MetaPromotedFrom = "promotedFrom"

const metaCapturedAt = "capturedAt"

// SetPromote configures the ★ cross-agent promotion pass. minAge <= 0 disables
// the pass (default).
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

// Promote copies each eligible private node of this agent's own scope into the
// shared project scope, so peer agents inherit it via RecallScoped. Best-effort
// and idempotent: disabled (promoteMinAge<=0), no project/agent scope, or nil
// Memory all yield a clean no-op; a per-node write failure is recorded as the
// first error but never aborts the rest of the pass.
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
