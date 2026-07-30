package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"unicode/utf8"

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

// Consolidator shrinks an over-budget memory candidate so a refused Record can be
// retried instead of dropped (memory roadmap G1). The merge/summarise heuristics
// are the closed part of the moat; this package defines only the seam and the
// open plumbing (Learner) that drives it. The returned node's Body SHOULD fit the
// given rune limit; the Learner re-checks and, if it still does not, holds the
// candidate for a later pass.
type Consolidator interface {
	Consolidate(ctx context.Context, over contracts.Node, limit int) (contracts.Node, error)
}

// errEnqueue signals that a candidate was refused for budget and could not be
// resolved this pass; the caller holds it on the pending queue. It never escapes
// Consolidate.
var errEnqueue = errors.New("orchestrator: candidate over budget, queued for retry")

// Learner is the richer Orchestrator that adds the learning loop on top of the
// default Curator: it keeps the same per-turn Context/Observe behaviour and
// implements a real Consolidate that runs an Extractor over the journal +
// transcript and persists facts (shared) and skills (private) via the P1 scope.
//
// A Learner is single-goroutine per session: Consolidate runs synchronously from
// Observe on the turn path, so pending/seen/offset are intentionally not
// mutex-guarded.
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
	// pending holds candidates refused for the per-node budget that could not be
	// consolidated to fit this pass. It is drained at the top of each Consolidate
	// (a consolidator may now be wired, or the budget may have changed) and is
	// in-memory only — the raw journal on disk remains the durable source, so a
	// chronically-unmergeable fact is simply retried for the life of the session.
	pending []Candidate

	// mergeMin/mergeMax/mergeTarget configure the G2 semantic-merge pass
	// (Learner.Merge). mergeMin <= 0 disables it (opt-in, default off); set via
	// SetMerge. mergeTarget is one of "stale" (default) / "active" / "all".
	mergeMin    int
	mergeMax    int
	mergeTarget string

	// reportEnabled/reportPrefix configure the G4 audit-report pass
	// (Learner.report). Both are set via SetReport; register.go always calls
	// it (default enabled=true, prefix="reports/" — see the config table), so
	// an unconfigured host still gets a report.
	reportEnabled bool
	reportPrefix  string
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
	var firstErr error
	// Drain candidates a prior pass refused for budget before taking new ones: a
	// consolidator may now be wired, or the budget may have changed.
	l.drain(ctx, &firstErr)

	journal := l.readJournalTail() // best-effort: missing file / no new bytes → ""
	var transcript string
	if sg, err := l.mem.Recall(ctx, l.session, 0); err == nil {
		transcript = sg.Root.Body
	}
	cands, err := l.extract.Extract(ctx, journal, transcript)
	if err != nil && firstErr == nil {
		firstErr = err // record it, but still run the sweep below (invariant:
	} // learning never breaks a turn) and preserve any drain error
	for _, c := range cands {
		if l.seen[c.Node.Key] {
			continue // already persisted this session — keep Consolidate idempotent
		}
		switch perr := l.persist(ctx, c); {
		case perr == nil:
			l.seen[c.Node.Key] = true
		case errors.Is(perr, errEnqueue):
			l.enqueue(c)
		default:
			if firstErr == nil {
				firstErr = perr // record the first failure and keep going: one bad
			} // candidate must not drop its siblings or skip the sweep below
		}
	}
	// Best-effort staleness sweep at the end of a consolidation pass. A sweep
	// error must never propagate out of Consolidate (invariant: learning never
	// breaks a turn).
	_ = l.Sweep(ctx)
	// Best-effort semantic merge after the sweep (opt-in via SetMerge; a no-op
	// when disabled or no Merger is wired). A merge error must never propagate
	// out of Consolidate (invariant: learning never breaks a turn).
	_ = l.Merge(ctx)
	// Best-effort audit report of this pass's transitions (opt-in via
	// SetReport, default enabled; a no-op when disabled or when the pass made
	// no transitions). A report error must never propagate out of Consolidate
	// (invariant: learning never breaks a turn).
	_ = l.report(ctx)
	// Reset the pass-scoped audit trail regardless of write outcome, so the
	// next pass never re-reports this pass's transitions.
	l.transitions = nil
	return firstErr
}

// consolidator returns the Consolidator the extractor also implements, if any.
// The closed extractor typically implements both seams, so forced consolidation
// needs no new constructor parameter.
func (l *Learner) consolidator() (Consolidator, bool) {
	c, ok := l.extract.(Consolidator)
	return c, ok
}

// record writes one candidate under the scope chosen by c.Private.
func (l *Learner) record(ctx context.Context, c Candidate) error {
	if c.Private {
		return contracts.RecordPrivate(ctx, l.mem, l.scope, c.Node)
	}
	return contracts.RecordShared(ctx, l.mem, l.scope, c.Node)
}

// persist records one candidate, responding to a per-node budget refusal
// (*contracts.BudgetError) by asking the Consolidator, if wired, to shrink the
// node to the refusal's Limit and retrying the write once. Returns nil on
// success, errEnqueue when a budget refusal is unresolved this pass (caller
// queues), or the underlying error for a non-budget failure (caller records it
// and keeps going).
func (l *Learner) persist(ctx context.Context, c Candidate) error {
	err := l.record(ctx, c)
	if err == nil {
		return nil
	}
	var be *contracts.BudgetError
	if !errors.As(err, &be) {
		return err // non-budget failure: caller keeps going
	}
	cons, ok := l.consolidator()
	if !ok {
		slog.Warn("memory: candidate over budget and no consolidator; queued for retry",
			"key", c.Node.Key, "runes", be.Runes, "limit", be.Limit)
		return errEnqueue
	}
	merged, cerr := cons.Consolidate(ctx, c.Node, be.Limit)
	if cerr != nil || merged.Key != c.Node.Key || merged.Body == "" || utf8.RuneCountInString(merged.Body) > be.Limit {
		slog.Warn("memory: consolidation did not bring candidate within budget; queued for retry",
			"key", c.Node.Key, "runes", be.Runes, "limit", be.Limit, "err", cerr)
		return errEnqueue
	}
	c.Node = merged
	rerr := l.record(ctx, c)
	if rerr == nil {
		return nil
	}
	if errors.As(rerr, &be) {
		// Still refused after shrinking — hold it rather than loop.
		slog.Warn("memory: consolidated candidate still over budget; queued for retry",
			"key", c.Node.Key, "runes", be.Runes, "limit", be.Limit)
		return errEnqueue
	}
	return rerr // a non-budget failure on retry surfaces as a first-error
}

// enqueue holds a budget-refused candidate for a later pass, deduped by node key
// so a chronically-unmergeable fact cannot grow the queue unbounded.
func (l *Learner) enqueue(c Candidate) {
	for _, p := range l.pending {
		if p.Node.Key == c.Node.Key {
			return
		}
	}
	l.pending = append(l.pending, c)
}

// drain re-attempts each pending candidate through persist. A now-successful
// candidate is marked seen and removed from the queue; anything still failing —
// whether still over budget or a transient non-budget error — stays queued for a
// later pass, since the journal offset already advanced past it and the queue is
// its only remaining lifeline.
func (l *Learner) drain(ctx context.Context, firstErr *error) {
	var still []Candidate
	for _, c := range l.pending {
		switch perr := l.persist(ctx, c); {
		case perr == nil:
			l.seen[c.Node.Key] = true
		case errors.Is(perr, errEnqueue):
			still = append(still, c)
		default:
			if *firstErr == nil {
				*firstErr = perr
			}
			still = append(still, c) // keep it queued: the journal offset already
			// advanced past this candidate, so the queue is its only lifeline; a
			// transient non-budget failure must not drop a fact we committed to retry
		}
	}
	l.pending = still
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
