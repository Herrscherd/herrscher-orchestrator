package orchestrator

import (
	"context"
	"fmt"
	"time"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

// ErrMergedOriginal is returned by Restore for a node folded into an umbrella,
// unless Force(true) is passed.
var ErrMergedOriginal error = mergedOriginalError{}

type mergedOriginalError struct{}

func (mergedOriginalError) Error() string {
	return "orchestrator: node is folded into an umbrella; restore with Force to detach it"
}

func (mergedOriginalError) Notice() contracts.Notice {
	return contracts.Notice{Code: contracts.CodeNodeMerged}
}

type restoreConfig struct {
	force bool
	clock func() time.Time
}

// RestoreOption configures one Restore call.
type RestoreOption func(*restoreConfig)

// Force returns a RestoreOption that allows restoring a merged original by
// detaching it from its umbrella.
func Force(force bool) RestoreOption {
	return func(c *restoreConfig) { c.force = force }
}

func withClock(clock func() time.Time) RestoreOption {
	return func(c *restoreConfig) { c.clock = clock }
}

// Restore reactivates the node at key: it sets the state to active and refreshes
// its last-seen time, so the next Sweep does not age it out again. It returns
// the state the node had before. It refuses a merged original unless Force(true)
// is passed.
func Restore(ctx context.Context, mem contracts.Memory, key string, opts ...RestoreOption) (string, error) {
	cfg := restoreConfig{clock: time.Now}
	for _, o := range opts {
		o(&cfg)
	}
	recalled, err := mem.Recall(ctx, key, 0)
	if err != nil {
		return "", err
	}
	root := recalled.Root
	if root.Meta[MetaMergedInto] != "" && !cfg.force {
		return "", ErrMergedOriginal
	}
	prior := root.Meta[contracts.MetaState]
	if prior == "" {
		prior = contracts.StateActive
	}
	if root.Meta == nil {
		root.Meta = make(map[string]string)
	}
	root.Meta[contracts.MetaState] = contracts.StateActive
	root.Meta[contracts.MetaLastSeen] = cfg.clock().UTC().Format(time.RFC3339)
	umbrella := ""
	if cfg.force {
		umbrella = root.Meta[MetaMergedInto]
		delete(root.Meta, MetaMergedInto)
	}
	if err := mem.Record(ctx, root); err != nil {
		return "", err
	}
	if umbrella != "" {
		if err := mem.Unlink(ctx, key, umbrella); err != nil {
			return "", fmt.Errorf("unlink %s from %s: %w", key, umbrella, err)
		}
	}
	return prior, nil
}

// Restore reactivates key like the package-level Restore and records a
// TransitionRestore for the next report, with the node's real prior state as
// From.
func (l *Learner) Restore(ctx context.Context, key string, opts ...RestoreOption) error {
	prior, err := Restore(ctx, l.mem, key, opts...)
	if err == nil {
		l.mu.Lock()
		l.transitions = append(l.transitions, Transition{Key: key, From: prior, To: contracts.StateActive, Kind: TransitionRestore})
		l.mu.Unlock()
	}
	return err
}
