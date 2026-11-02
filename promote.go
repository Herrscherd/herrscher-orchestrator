package orchestrator

import (
	"context"
	"log/slog"
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

// SetPromote configures the ★ cross-agent promotion pass. minAge <= 0 disables
// the pass (default).
func (l *Learner) SetPromote(minAge time.Duration) {
	l.promoteMinAge = minAge
}

func (l *Learner) promoteEligible(n contracts.Node, _ time.Time) bool {
	state := n.Meta[contracts.MetaState]
	if state != "" && state != contracts.StateActive {
		return false
	}
	if n.Meta[MetaMergedInto] != "" || n.Meta[MetaPromotedTo] != "" {
		return false
	}
	if n.Kind == contracts.KindSkill && n.Meta[MetaApproved] == "" {
		return false
	}
	capturedAt, err1 := time.Parse(time.RFC3339, n.Meta["capturedAt"])
	lastSeen, err2 := time.Parse(time.RFC3339, n.Meta[contracts.MetaLastSeen])
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

func cloneMeta(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (l *Learner) sharedKeyTaken(ctx context.Context, target string) bool {
	sg, err := l.mem.Recall(ctx, target, 0)
	if err != nil || sg.Root.Key != target {
		return false
	}
	if sg.Root.Body == "" && sg.Root.Title == "" {
		return false
	}
	return sg.Root.Meta[MetaPromotedFrom] == ""
}

func (l *Learner) applyPromotion(ctx context.Context, n contracts.Node) error {
	scope := l.scopeOf()
	dup := n
	dup.Key = promotedKey(scope.Project, n.Key)
	dup.Meta = cloneMeta(n.Meta)
	delete(dup.Meta, MetaPromotedTo)
	dup.Meta[MetaPromotedFrom] = n.Key
	if l.sharedKeyTaken(ctx, dup.Key) {
		slog.Warn("memory: skipping promotion over an unrelated shared node", "from", n.Key, "to", dup.Key)
		return nil
	}
	if err := contracts.RecordShared(ctx, l.mem, scope, dup); err != nil {
		return err
	}
	if err := l.mem.Links(ctx, n.Key, dup.Key, "promoted-to"); err != nil {
		return err
	}
	if n.Meta == nil {
		n.Meta = make(map[string]string)
	}
	n.Meta[MetaPromotedTo] = dup.Key
	return l.mem.Record(ctx, n)
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
	now := l.now().UTC()
	var firstErr error
	for _, n := range nodes {
		if !strings.HasPrefix(n.Key, prefix) {
			continue
		}
		if !l.promoteEligible(n, now) {
			continue
		}
		if err := l.applyPromotion(ctx, n); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
