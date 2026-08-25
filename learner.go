package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
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

// idlePollInterval is how often the G5 idle loop checks DueForIdleRun. It is a
// package var (not a const) solely so tests can shorten it to exercise
// Start/idleLoop and ctx-cancellation without real waits; it only needs to be
// finer than idle-hours' granularity to detect the threshold promptly.
var idlePollInterval = 10 * time.Minute

// Learner is the richer Orchestrator that adds the learning loop on top of the
// default Curator: it keeps the same per-turn Context/Observe behaviour and
// implements a real Consolidate that runs an Extractor over the journal +
// transcript and persists facts (shared) and skills (private) via the P1 scope.
//
// The G5 idle loop adds a second Consolidate trigger on its own goroutine, so a
// turn-path Consolidate and an idle one can now race. mu serialises the whole
// Consolidate body (they never interleave), which is also why pending/seen/offset
// — touched only inside that mu-guarded body — need no separate guard. The
// activity clock (lastActivity/lastRun/n), read/written off the consolidation
// body, is guarded by the lighter stampMu instead (see the field comments).
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

	// promoteMinAge configures the ★ cross-agent promotion pass (Learner.Promote):
	// the minimum a private node's lastSeen must exceed its capturedAt before the
	// node is eligible to be copied into the shared project scope. <=0 disables the
	// pass (opt-in, default off); set via SetPromote.
	promoteMinAge time.Duration

	// reportEnabled/reportPrefix configure the G4 audit-report pass
	// (Learner.report). Both are set via SetReport; register.go always calls
	// it (default enabled=true, prefix="reports/" — see the config table), so
	// an unconfigured host still gets a report.
	reportEnabled bool
	reportPrefix  string

	// idleDays/idleHours configure the G5 inactivity trigger (SetIdle).
	// idleDays <= 0 disables the trigger entirely (opt-in, default off).
	idleDays  int
	idleHours int
	// lastRun is stamped at the top of every Consolidate (zero = never run);
	// lastActivity is stamped at the top of every Observe. Both — and the turn
	// counter n — are guarded by stampMu, a lightweight lock held only for the
	// stamp/read itself, NEVER across a Consolidate. mu separately serialises the
	// whole Consolidate body so a turn-path call and the idle-loop call never
	// interleave (Go mutexes are not reentrant, so public Consolidate locks mu and
	// delegates to consolidateLocked). Keeping the activity stamp off mu is
	// invariant 2: a turn-path Observe must never block waiting for a slow
	// background idle consolidation that holds mu. Lock order where both are held:
	// mu → stampMu (only inside consolidateLocked); no path takes mu while holding
	// stampMu, so the two can never deadlock.
	lastRun      time.Time
	lastActivity time.Time
	mu           sync.Mutex
	stampMu      sync.Mutex

	// rawArchive enables the G7 raw-session archival tier: when true, every
	// Observe best-effort records one verbatim KindTranscript node per turn. Off
	// by default (append-heavy); set via SetRawArchive.
	rawArchive bool
	// rawEpoch and rawSeq build the raw node key raw/<tail>/<epoch>-<seq>. rawSeq
	// is the per-process monotonic turn counter; rawEpoch is a one-shot wall-clock
	// stamp captured on the first raw record of this process. The epoch prefix is
	// what keeps the append-only tier intact across a bridge restart of a resumed
	// session: the bridge runs as a supervised subprocess, so a plain counter
	// would reset to 1 and overwrite raw/<tail>/1.. of the earlier run — but each
	// run gets a distinct epoch (restarts are seconds apart via supervisor
	// backoff), so keys never collide across runs, while the dense counter keeps
	// them unique and ordered within a run. Both advance under stampMu alongside n.
	rawEpoch string
	rawSeq   int
}

var _ contracts.Orchestrator = (*Learner)(nil)

// NewLearner builds a learning orchestrator. With a nil extractor it behaves
// exactly like the default Curator (Consolidate is a no-op).
func NewLearner(mem contracts.Memory, session string, scope contracts.MemoryScope, ex Extractor, journal string, every int) *Learner {
	return &Learner{Curator: NewScoped(mem, session, scope), extract: ex, journal: journal, every: every, seen: map[string]bool{}}
}

// SetIdle configures the G5 inactivity trigger. sinceLastRunDays <= 0 disables
// it (opt-in, default off); idleHours is the quiet-period threshold measured
// against lastActivity. Mirrors SetStaleness/SetMerge's setter shape.
func (l *Learner) SetIdle(sinceLastRunDays, idleHours int) {
	l.idleDays = sinceLastRunDays
	l.idleHours = idleHours
}

// SetRawArchive toggles the G7 raw-session archival tier. When true, Observe
// records one untruncated KindTranscript node per turn (best-effort, never
// blocking the turn). Off by default. The host wires this from MEMORY_RAW_ARCHIVE.
func (l *Learner) SetRawArchive(on bool) {
	l.rawArchive = on
}

// DueForIdleRun reports whether an inactivity-triggered Consolidate should fire,
// given the current time and the timestamp of the last observed activity. It is
// pure: it reads now as an explicit parameter (not l.now()), so it is unit-
// testable with fixed times and needs no real timers. The rule is
// (now-lastRun >= idleDays) AND (now-lastActivity >= idleHours), both inclusive.
// It returns false when the trigger is disabled (idleDays <= 0), when lastRun is
// zero (never consolidated: no baseline yet), or when either threshold is unmet.
func (l *Learner) DueForIdleRun(now, lastActivity time.Time) bool {
	if l.idleDays <= 0 {
		return false
	}
	if l.lastRun.IsZero() {
		return false
	}
	if now.Sub(l.lastRun) < time.Duration(l.idleDays)*24*time.Hour {
		return false
	}
	if now.Sub(lastActivity) < time.Duration(l.idleHours)*time.Hour {
		return false
	}
	return true
}

// Start runs the G5 inactivity poll loop until ctx is cancelled. It is a no-op
// if the idle trigger is disabled (SetIdle never called or idleDays <= 0). The
// host calls this once per bridge process, right after constructing the
// orchestrator; it never blocks a turn — the loop only ever fires Consolidate
// from its own goroutine, single-flighted against the turn path via TryLock.
func (l *Learner) Start(ctx context.Context) {
	if l.idleDays <= 0 {
		return
	}
	go l.idleLoop(ctx)
}

// idleLoop ticks every idlePollInterval and evaluates one idleTick per tick,
// returning when ctx is cancelled (the bridge process's root context, so the
// goroutine is cleaned up automatically on session teardown).
func (l *Learner) idleLoop(ctx context.Context) {
	t := time.NewTicker(idlePollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.idleTick(ctx)
		}
	}
}

// idleTick evaluates the inactivity predicate and, if due, runs one Consolidate
// out of band. It first reads lastActivity/lastRun under stampMu — the same
// lightweight lock Observe stamps under, never the consolidation mu, so this
// evaluation cannot be delayed by an in-flight consolidation. If due, it takes
// the consolidation mu with TryLock — never Lock — so it never blocks the turn
// path (invariant 2): on success it runs consolidateLocked while holding the
// lock; on TryLock failure (a turn-path Consolidate holds the lock) it skips this
// tick and retries at the next one. Extracted from idleLoop so tests can drive it
// directly with a fake clock.
//
// The predicate read and the TryLock are not atomic: a turn-path Observe may
// stamp a fresh lastActivity in the gap, so an idle Consolidate can fire just as
// activity resumes. This is harmless — Consolidate is best-effort and the next
// tick re-evaluates against the fresh clock — so the gap is left unguarded rather
// than widen the locking.
func (l *Learner) idleTick(ctx context.Context) {
	l.stampMu.Lock()
	due := l.DueForIdleRun(l.now(), l.lastActivity)
	l.stampMu.Unlock()
	if !due {
		return
	}
	if !l.mu.TryLock() {
		return // turn-path Consolidate holds the lock; never block — retry next tick
	}
	_ = l.consolidateLocked(ctx) // best-effort; error swallowed, like the cadence path
	l.mu.Unlock()
}

// Observe records the turn (default behaviour), stamps the last-activity clock
// (the G5 idle trigger's activity signal), and, every `every` turns, fires a
// best-effort Consolidate out of band so learning never breaks the turn loop.
//
// The stamp and counter advance under stampMu — a lightweight lock, NOT the
// consolidation mu — so a turn-path Observe never waits on a slow background idle
// Consolidate holding mu (invariant 2). stampMu is released before the cadence
// l.Consolidate call (which acquires mu), so the two locks are never held nested
// here.
func (l *Learner) Observe(ctx context.Context, p contracts.Prompt, reply string) error {
	err := l.Curator.Observe(ctx, p, reply)
	l.stampMu.Lock()
	l.lastActivity = l.now()
	var due bool
	if l.every > 0 {
		l.n++
		due = l.n%l.every == 0
	}
	var rawSeq int
	var rawEpoch string
	if l.rawArchive {
		if l.rawEpoch == "" {
			l.rawEpoch = l.now().UTC().Format("20060102T150405.000000000Z")
		}
		l.rawSeq++
		rawSeq = l.rawSeq
		rawEpoch = l.rawEpoch
	}
	l.stampMu.Unlock()
	if rawSeq > 0 {
		l.recordRaw(ctx, p, reply, rawEpoch, rawSeq) // best-effort; never breaks the turn
	}
	if due {
		_ = l.Consolidate(ctx)
	}
	return err
}

// recordRaw writes one verbatim raw-transcript node for the turn (G7). It is
// best-effort: any error is discarded so archival never breaks the turn loop
// (invariant 2). The Body is the full untruncated prompt+reply — the obsidian
// per-node budget is bypassed for KindTranscript, so nothing is lost. Keyed
// raw/<sessionTail>/<epoch>-<seq>: append-only and monotonic within a run, and
// the per-process epoch keeps a resumed session's earlier turns from being
// overwritten after a bridge restart (see the rawEpoch/rawSeq field comment).
func (l *Learner) recordRaw(ctx context.Context, p contracts.Prompt, reply, epoch string, seq int) {
	if l.mem == nil {
		return
	}
	// session is trusted host config, but keep the raw tier's key confined to its
	// own segment regardless: a stray '/' in the tail would otherwise nest the
	// node elsewhere in the vault (or, with '..', escape the raw subtree).
	tail := strings.ReplaceAll(strings.TrimPrefix(l.session, "sessions/"), "/", "-")
	_ = l.mem.Record(ctx, contracts.Node{
		Key:   fmt.Sprintf("raw/%s/%s-%d", tail, epoch, seq),
		Kind:  contracts.KindTranscript,
		Title: fmt.Sprintf("turn %d", seq),
		Body:  fmt.Sprintf("%s: %s\n\nassistant: %s", p.Author, p.Content, reply),
	})
}

// Consolidate runs the extractor over the journal + transcript and persists each
// candidate under the right scope. It is best-effort: a missing journal, a nil
// extractor, or a nil Memory all yield a clean no-op. It is safe to call from
// any goroutine (turn-path cadence, the G5 idle loop, or a standalone/test
// call): it serialises every pass under mu so two passes never interleave.
func (l *Learner) Consolidate(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.consolidateLocked(ctx)
}

// consolidateLocked holds the full consolidation body. The caller MUST already
// hold l.mu (public Consolidate acquires it; the idle loop acquires it via
// TryLock). It stamps lastRun at the top so every trigger path — per-turn
// cadence, idle, or a manual call — uniformly resets the inactivity clock. The
// stamp itself takes stampMu briefly (lock order mu → stampMu) so DueForIdleRun,
// which reads lastRun under stampMu, sees a consistent value.
func (l *Learner) consolidateLocked(ctx context.Context) error {
	l.stampMu.Lock()
	l.lastRun = l.now()
	l.stampMu.Unlock()
	// Reset the pass-scoped audit trail on every return path, so the next pass
	// never re-reports this pass's transitions. A deferred reset also caps the
	// out-of-band buffer: Learner.Restore appends transitions between passes, so
	// a nil-extractor Learner (whose Consolidate returns early below) would
	// otherwise accumulate them forever. report() runs before this fires and so
	// still sees the full trail on the normal path.
	defer func() { l.transitions = nil }()
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
	// Best-effort cross-agent promotion after the merge (opt-in via SetPromote;
	// a no-op when disabled or when nothing is eligible). Promote must see the
	// post-sweep, post-merge state so a freshly-archived merge original is never
	// promoted. A promotion error must never propagate out of Consolidate
	// (invariant: learning never breaks a turn).
	_ = l.Promote(ctx)
	// Best-effort audit report of this pass's transitions (opt-in via
	// SetReport, default enabled; a no-op when disabled or when the pass made
	// no transitions). A report error must never propagate out of Consolidate
	// (invariant: learning never breaks a turn).
	_ = l.report(ctx)
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
	scope := l.scopeOf()
	if l.learnedSkills {
		c = asLearnedSkill(c, scope.Agent != "")
	}
	if c.Private {
		return contracts.RecordPrivate(ctx, l.mem, scope, c.Node)
	}
	return contracts.RecordShared(ctx, l.mem, scope, c.Node)
}

// SetScope re-roots memory, waiting for any consolidation in flight to finish
// first, so one pass never files half its findings under the old project and the
// other half under the new one. Lock order is mu → scopeMu, the same direction as
// mu → stampMu, so the three can never deadlock.
func (l *Learner) SetScope(s contracts.MemoryScope) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Curator.SetScope(s)
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
