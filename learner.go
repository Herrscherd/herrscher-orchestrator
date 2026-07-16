package orchestrator

import (
	"context"
	"io"
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
	// offset is the byte position read up to on the last Consolidate. The journal
	// is append-only, so each run only reads/parses the tail appended since, turning
	// per-consolidation cost from O(total journal) into O(new bytes).
	offset int64
	// seen tracks candidate keys already persisted this session so re-running
	// Consolidate over the same journal does not re-link duplicate facts/skills
	// every turn. Nodes upsert by Key anyway; this also guards the link writes.
	seen map[string]bool
}

var _ contracts.Orchestrator = (*Learner)(nil)

// NewLearner builds a learning orchestrator. With a nil extractor it behaves
// exactly like the default Curator (Consolidate is a no-op).
func NewLearner(mem contracts.Memory, session string, scope contracts.MemoryScope, ex Extractor, journal string, every int) *Learner {
	return &Learner{Curator: NewScoped(mem, session, scope), extract: ex, journal: journal, every: every, seen: map[string]bool{}}
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
	journal := l.readJournalTail() // best-effort: missing file / no new bytes → ""
	var transcript string
	if sg, err := l.mem.Recall(ctx, l.session, 0); err == nil {
		transcript = sg.Root.Body
	}
	cands, err := l.extract.Extract(ctx, journal, transcript)
	if err != nil {
		return err
	}
	for _, c := range cands {
		if l.seen[c.Node.Key] {
			continue // already persisted this session — keep Consolidate idempotent
		}
		if c.Private {
			if err := contracts.RecordPrivate(ctx, l.mem, l.scope, c.Node); err != nil {
				return err
			}
		} else {
			if err := contracts.RecordShared(ctx, l.mem, l.scope, c.Node); err != nil {
				return err
			}
		}
		l.seen[c.Node.Key] = true
	}
	return nil
}

// readJournalTail returns the journal bytes appended since the last Consolidate,
// advancing the saved offset. It is best-effort: a missing/unreadable file yields
// "". A shrink (log rotation/truncation) resets the offset and re-reads from the
// start so no appended work is skipped.
func (l *Learner) readJournalTail() string {
	if l.journal == "" {
		return ""
	}
	f, err := os.Open(l.journal)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() < l.offset {
		l.offset = 0
	}
	if l.offset > 0 {
		if _, err := f.Seek(l.offset, io.SeekStart); err != nil {
			return ""
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	l.offset += int64(len(b))
	return string(b)
}
