package orchestrator

import (
	"context"
	"errors"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

// ErrMergedOriginal is returned by Restore when key names a node folded into an
// umbrella (Meta[MetaMergedInto] != "") and the caller did not pass Force(true).
// Restoring a merged fragment without detaching it would resurrect a fragment
// the merge pass deliberately subsumed, while Sweep's mergedInto guard would
// immediately re-skip it and a later Merge pass could re-fold it right back.
var ErrMergedOriginal = errors.New("orchestrator: node is folded into an umbrella; restore with Force to detach it")

// restoreConfig holds the options one Restore call was given.
type restoreConfig struct {
	force bool
	clock func() time.Time
}

// RestoreOption configures one Restore call.
type RestoreOption func(*restoreConfig)

// Force, when true, allows restoring a merged-into original by also clearing
// Meta[MetaMergedInto] so the node becomes independent again. The umbrella
// node itself is untouched — it may now have one fewer live member, which is
// fine; umbrellas are additive, not authoritative.
func Force(force bool) RestoreOption {
	return func(c *restoreConfig) { c.force = force }
}

// withClock overrides Restore's injected clock. Unexported: it exists only so
// this package's own tests can assert on a deterministic Meta[MetaLastSeen]
// stamp; every real caller gets time.Now.
func withClock(clock func() time.Time) RestoreOption {
	return func(c *restoreConfig) { c.clock = clock }
}

// Restore reactivates an archived (or merged-into) node at key: it clears
// Meta[MetaState] to active and refreshes Meta[MetaLastSeen] to now, so the
// very next Sweep does not immediately re-derive it back to stale/archived
// from a stale timestamp. Idempotent: restoring an already-active node is a
// no-op write that still refreshes lastSeen.
//
// A node still carrying Meta[MetaMergedInto] is a folded fragment, not an
// independent archived node — restoring it without also detaching it from its
// umbrella would resurrect a fragment the merge pass deliberately subsumed.
// Restore therefore REFUSES a merged original by default: it returns
// ErrMergedOriginal unless the caller passes Force(true), in which case it
// also clears Meta[MetaMergedInto].
//
// This is a free function taking contracts.Memory, not a *Curator/*Learner
// method: it needs no orchestrator state beyond the injected clock, and the
// host's `memory restore` CLI verb builds a bare contracts.Memory with no
// session/scope, the same shape as the existing memory forget/record verbs.
//
// An error from Recall, including "not found", surfaces unchanged — restoring
// an absent key is a real error (nothing to reactivate), unlike Deleter.Delete's
// intentional idempotent-on-absent contract.
//
// It returns the node's state immediately before the restore (its prior
// Meta[MetaState], or StateActive when unset), so callers can record a faithful
// audit "from" value; the returned prior is meaningful only when err is nil.
func Restore(ctx context.Context, mem contracts.Memory, key string, opts ...RestoreOption) (prior string, err error) {
	cfg := restoreConfig{clock: time.Now}
	for _, o := range opts {
		o(&cfg)
	}
	sg, err := mem.Recall(ctx, key, 0)
	if err != nil {
		return "", err
	}
	n := sg.Root
	if n.Meta[MetaMergedInto] != "" && !cfg.force {
		return "", ErrMergedOriginal
	}
	prior = n.Meta[contracts.MetaState]
	if prior == "" {
		prior = contracts.StateActive
	}
	if n.Meta == nil {
		n.Meta = map[string]string{}
	}
	n.Meta[contracts.MetaState] = contracts.StateActive
	n.Meta[contracts.MetaLastSeen] = cfg.clock().UTC().Format(time.RFC3339)
	umbrella := ""
	if cfg.force {
		umbrella = n.Meta[MetaMergedInto]
		delete(n.Meta, MetaMergedInto)
	}
	if err := mem.Record(ctx, n); err != nil {
		return "", err
	}
	if umbrella != "" {
		// Best-effort: the reactivation already succeeded; a failure to drop the
		// residual merged-into edge must not fail the restore (learning never
		// breaks the turn). The umbrella is additive, so a stale edge is cosmetic.
		_ = mem.Unlink(ctx, key, umbrella)
	}
	return prior, nil
}

// Restore reactivates key over l's Memory and, on success, appends a
// Transition{Kind:"restore"} to the pass's audit trail — surfaced in the next
// report if Consolidate runs before the field resets, or inspectable directly
// via l.transitions since Restore is typically called out of band between
// passes. The transition's From is the node's real prior state, not an
// assumed "archived".
func (l *Learner) Restore(ctx context.Context, key string, opts ...RestoreOption) error {
	prior, err := Restore(ctx, l.mem, key, opts...)
	if err == nil {
		l.transitions = append(l.transitions, Transition{Key: key, From: prior, To: contracts.StateActive, Kind: "restore"})
	}
	return err
}
