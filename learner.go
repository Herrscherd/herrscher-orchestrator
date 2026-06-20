package orchestrator

import (
	"context"
	"os"

	"github.com/Herrscherd/herrscher-contracts"
)

// Candidate is a memory node the curator proposes to persist, tagged for the
// shared project scope (a fact every agent of the game should know) or the
// private agent scope (a learned skill that stays with this agent).
type Candidate struct {
	Node    contracts.Node
	Private bool // true → under the Agent (a skill); false → under the Project (a fact)
}

// Extractor turns the raw record of a stretch of work — the call journal written
// by `neublox serve` plus the session transcript — into memory candidates. The
// heuristics/prompts that decide *what is worth remembering* (e.g. the Roblox
// curation strategy) are the **closed part of the moat**; this package defines
// only the seam and the open plumbing (Learner) that persists what it returns.
type Extractor interface {
	Extract(ctx context.Context, journal, transcript string) ([]Candidate, error)
}

// Learner is the richer Orchestrator that adds the learning loop on top of the
// default Curator: it keeps the same per-turn Context/Observe behaviour and
// implements a real Consolidate that runs an Extractor over the journal +
// transcript and persists facts (shared) and skills (private) via the P1 scope.
type Learner struct {
	*Curator
	extract Extractor
	journal string // path to the call journal (e.g. <worktree>/.neublox/calls.log)
	every   int    // run Consolidate every N observed turns (0 = manual only)
	n       int
}

var _ contracts.Orchestrator = (*Learner)(nil)

// NewLearner builds a learning orchestrator. With a nil extractor it behaves
// exactly like the default Curator (Consolidate is a no-op).
func NewLearner(mem contracts.Memory, session string, scope contracts.MemoryScope, ex Extractor, journal string, every int) *Learner {
	return &Learner{Curator: NewScoped(mem, session, scope), extract: ex, journal: journal, every: every}
}

// Observe records the turn (default behaviour) and, every `every` turns, fires a
// best-effort Consolidate out of band so learning never breaks the turn loop.
func (l *Learner) Observe(ctx context.Context, p contracts.Prompt, reply string) error {
	err := l.Curator.Observe(ctx, p, reply)
	if l.every > 0 {
		l.n++
		if l.n%l.every == 0 {
			_ = l.Consolidate(ctx)
		}
	}
	return err
}

// Consolidate runs the extractor over the journal + transcript and persists each
// candidate under the right scope. It is best-effort: a missing journal, a nil
// extractor, or a nil Memory all yield a clean no-op.
func (l *Learner) Consolidate(ctx context.Context) error {
	if l.extract == nil || l.mem == nil {
		return nil
	}
	journal, _ := os.ReadFile(l.journal) // best-effort: missing file → ""
	var transcript string
	if sg, err := l.mem.Recall(ctx, l.session, 0); err == nil {
		transcript = sg.Root.Body
	}
	cands, err := l.extract.Extract(ctx, string(journal), transcript)
	if err != nil {
		return err
	}
	for _, c := range cands {
		if c.Private {
			if err := contracts.RecordPrivate(ctx, l.mem, l.scope, c.Node); err != nil {
				return err
			}
		} else {
			if err := contracts.RecordShared(ctx, l.mem, l.scope, c.Node); err != nil {
				return err
			}
		}
	}
	return nil
}
