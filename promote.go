package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

// MetaPromotedTo, when set on a private node, names the Key of the shared copy
// it was promoted to. It is a terminal marker on the ORIGINAL: the original is
// kept (reversible, still private, still usable by its own agent) but is never
// re-promoted. Orchestrator-internal — obsidian stores Meta generically, so no
// contracts change is needed.
const MetaPromotedTo = "promotedTo"

// MetaPromotedFrom, set on a shared copy, names the Key of the private node it
// was promoted from (the back-pointer counterpart to MetaPromotedTo).
const MetaPromotedFrom = "promotedFrom"

// SetPromote configures the ★ cross-agent promotion pass. minAge <= 0 disables
// the pass (default).
func (l *Learner) SetPromote(minAge time.Duration) {
	l.promoteMinAge = minAge
}

// promoteEligible reports whether private node n has proven itself enough to be
// copied into the shared project scope. The rule is deterministic over Meta
// stamps already on the node (no counter field exists): active (or implicit
// active), not merged-away, not already promoted, with both age stamps present
// and parseable, and a lastSeen that has advanced past capturedAt by at least
// promoteMinAge (i.e. the node was re-observed, not written once and left).
//
// now is accepted for signature symmetry with Sweep/NextState but unused: age
// is measured between the two stamps, not against the wall clock. It is kept so
// a future refinement (e.g. "also require now-lastSeen recency") needs no
// call-site churn.
func (l *Learner) promoteEligible(n contracts.Node, now time.Time) bool {
	state := n.Meta[contracts.MetaState]
	if state != "" && state != contracts.StateActive {
		return false
	}
	if n.Meta[MetaMergedInto] != "" || n.Meta[MetaPromotedTo] != "" {
		return false
	}
	// A skill is instructions, not a fact. Promotion is the one place in the
	// system where the blast radius changes, from what this agent believes to what
	// every agent of the project executes, so it is the one place the boundary is
	// held. A fact keeps crossing on maturity alone.
	if n.Kind == contracts.KindSkill && n.Meta[MetaApproved] == "" {
		return false
	}
	capturedAt, err1 := time.Parse(time.RFC3339, n.Meta["capturedAt"])
	lastSeen, err2 := time.Parse(time.RFC3339, n.Meta[contracts.MetaLastSeen])
	if err1 != nil || err2 != nil {
		return false // no reliable age basis
	}
	return lastSeen.Sub(capturedAt) >= l.promoteMinAge
}

// promotedKey derives the stable shared Key for a promoted private node from the
// project key and the tail of the private key, e.g.
// ("projects/neublox", "agents/roblox-dev/skills/retry-http") ->
// "projects/neublox/skills/retry-http". A pure function of its inputs, so a
// retry after a mid-promotion crash recomputes the same Key and upserts rather
// than duplicating the shared copy.
//
// project is a plain string (MemoryScope.Project's own type in this contracts
// version — there is no distinct ProjectKey type to bind to).
//
// This intentionally collides same-tail nodes from different agents onto one
// shared project node: e.g. both "agents/x/skills/retry-http" and
// "agents/y/skills/retry-http" map to "projects/<p>/skills/retry-http". That
// is by design — peers converging on the same node dedup onto a single
// shared copy rather than each getting their own; the last promotion to run
// upserts and wins on Body.
func promotedKey(project string, agentKey string) string {
	_, rest, _ := strings.Cut(agentKey, "/") // "agents/<agent>/<tail>" -> "<agent>/<tail>"
	_, tail, _ := strings.Cut(rest, "/")     // -> "<tail>"
	return project + "/" + tail
}

// cloneMeta returns a shallow copy of m (or an empty map for nil), so mutating
// the copy's Meta cannot alias the original node's map.
func cloneMeta(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// applyPromotion writes a shared copy of a proven private node n under the
// project scope, links the original to it, and labels the original so it is
// never re-promoted. Reversible: the original is kept, untouched apart from the
// label; nothing is archived or deleted. Write-then-label order mirrors
// applyUmbrella so a crash between the two leaves the original still eligible
// for a safe retry (Record upserts by Key) rather than labeled-but-copyless.
func (l *Learner) applyPromotion(ctx context.Context, n contracts.Node) error {
	scope := l.scopeOf()
	dup := n
	dup.Key = promotedKey(scope.Project, n.Key)
	dup.Meta = cloneMeta(n.Meta)
	delete(dup.Meta, MetaPromotedTo) // the copy is not itself "promoted"
	dup.Meta[MetaPromotedFrom] = n.Key
	if err := contracts.RecordShared(ctx, l.mem, scope, dup); err != nil {
		return err
	}
	if err := l.mem.Links(ctx, n.Key, dup.Key, "promoted-to"); err != nil {
		return err // label not yet set: a retry re-attempts the whole write
	}
	if n.Meta == nil {
		n.Meta = map[string]string{}
	}
	n.Meta[MetaPromotedTo] = dup.Key
	// Re-supply lastSeen implicitly by re-Recording n unchanged apart from the
	// label: n already carries its lastSeen from Search, so this state-only write
	// does not reset the age the promotion rule depends on (same discipline as
	// Sweep/applyUmbrella).
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
	nodes, err := l.mem.Search(ctx, contracts.Query{}) // active+stale, never archived
	if err != nil {
		return err
	}
	prefix := scope.Agent + "/"
	now := l.now().UTC()
	var firstErr error
	for _, n := range nodes {
		if !strings.HasPrefix(n.Key, prefix) {
			continue // only this agent's own private subtree
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
