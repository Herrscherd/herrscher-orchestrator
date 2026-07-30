package orchestrator

import (
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

// MetaPromotedTo, when set on a private node, names the Key of the shared copy
// it was promoted to. It is a terminal marker on the ORIGINAL: the original is
// kept (reversible, still private, still usable by its own agent) but is never
// re-promoted. Orchestrator-internal — obsidian stores Meta generically, so no
// contracts change is needed.
const MetaPromotedTo = "promotedTo"

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
	capturedAt, err1 := time.Parse(time.RFC3339, n.Meta["capturedAt"])
	lastSeen, err2 := time.Parse(time.RFC3339, n.Meta[contracts.MetaLastSeen])
	if err1 != nil || err2 != nil {
		return false // no reliable age basis
	}
	return lastSeen.Sub(capturedAt) >= l.promoteMinAge
}
