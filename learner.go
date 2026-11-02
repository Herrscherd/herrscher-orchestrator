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

	contracts "github.com/Herrscherd/herrscher-contracts"
)

// Candidate is a memory node the curator proposes to persist, tagged for the
// shared project scope (a fact every agent of the game should know) or the
// private agent scope (a learned skill that stays with this agent).
type Candidate struct {
	Node    contracts.Node
	Private bool
}

// Extractor turns the raw record of a stretch of work (the call journal written
// by `neublox serve` plus the session transcript) into memory candidates. The
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

var errEnqueue = errors.New("orchestrator: candidate over budget, queued for retry")

// Learner is the richer Orchestrator that adds the learning loop on top of the
// default Curator: it keeps the same per-turn Context/Observe behaviour and
// implements a real Consolidate that runs an Extractor over the journal +
// transcript and persists facts (shared) and skills (private) via the P1 scope.
//
// The G5 idle loop adds a second Consolidate trigger on its own goroutine, so a
// turn-path Consolidate and an idle one can now race. mu serialises the whole
// Consolidate body (they never interleave), which is also why pending/seen/offset
// touched only inside that mu-guarded body, need no separate guard. The
// activity clock (lastActivity/lastRun/n), read/written off the consolidation
// body, is guarded by the lighter stampMu instead (see the field comments).
type Learner struct {
	*Curator

	extract Extractor
	journal string
	every   int
	n       int
	offset  int64
	seen    map[string]bool
	pending []Candidate

	mergeMin    int
	mergeMax    int
	mergeTarget string

	promoteMinAge time.Duration

	reportEnabled bool
	reportPrefix  string

	idleDays     int
	idleHours    int
	lastRun      time.Time
	lastActivity time.Time
	mu           sync.Mutex
	stampMu      sync.Mutex

	rawArchive bool
	rawEpoch   string
	rawSeq     int

	stopIdle context.CancelFunc
	idleDone chan struct{}
	idlePoll time.Duration
}

var _ contracts.Orchestrator = (*Learner)(nil)

const defaultIdlePoll = 10 * time.Minute

// NewLearner builds a learning orchestrator. With a nil extractor it behaves
// exactly like the default Curator (Consolidate is a no-op).
func NewLearner(mem contracts.Memory, session string, scope contracts.MemoryScope, ex Extractor, journal string, every int) *Learner {
	return &Learner{Curator: NewScoped(mem, session, scope), extract: ex, journal: journal, every: every, seen: make(map[string]bool), idlePoll: defaultIdlePoll}
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
// orchestrator; it never blocks a turn. The loop only ever fires Consolidate
// from its own goroutine, single-flighted against the turn path via TryLock.
func (l *Learner) Start(ctx context.Context) {
	if l.idleDays <= 0 || l.stopIdle != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	l.stopIdle = cancel
	l.idleDone = make(chan struct{})
	go func() {
		defer close(l.idleDone)
		l.idleLoop(ctx)
	}()
}

// Close stops the idle loop, waits for it to exit, then closes the Curator.
func (l *Learner) Close() error {
	if l.stopIdle != nil {
		l.stopIdle()
		<-l.idleDone
		l.stopIdle = nil
		l.idleDone = nil
	}
	return l.Curator.Close()
}

func (l *Learner) idleLoop(ctx context.Context) {
	ticker := time.NewTicker(l.idlePoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.idleTick(ctx)
		}
	}
}

func (l *Learner) idleTick(ctx context.Context) {
	l.stampMu.Lock()
	due := l.DueForIdleRun(l.now(), l.lastActivity)
	l.stampMu.Unlock()
	if !due {
		return
	}
	if !l.mu.TryLock() {
		return
	}
	l.logFailure("memory: idle consolidation failed", l.consolidateLocked(ctx))
	l.mu.Unlock()
}

// Observe records the turn (default behaviour), stamps the last-activity clock
// (the G5 idle trigger's activity signal), and, every `every` turns, fires a
// best-effort Consolidate out of band so learning never breaks the turn loop.
//
// The stamp and counter advance under stampMu, a lightweight lock, NOT the
// consolidation mu, so a turn-path Observe never waits on a slow background idle
// Consolidate holding mu (invariant 2). stampMu is released before the cadence
// l.Consolidate call (which acquires mu), so the two locks are never held nested
// here.
func (l *Learner) Observe(ctx context.Context, prompt contracts.Prompt, reply string) error {
	err := l.Curator.Observe(ctx, prompt, reply)
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
		l.recordRaw(ctx, prompt, reply, rawEpoch, rawSeq)
	}
	if due {
		l.logFailure("memory: consolidation after turn failed", l.Consolidate(ctx))
	}
	return err
}

func (l *Learner) recordRaw(ctx context.Context, prompt contracts.Prompt, reply, epoch string, seq int) {
	if l.mem == nil {
		return
	}
	tail := strings.ReplaceAll(strings.TrimPrefix(l.session, "sessions/"), "/", "-")
	l.logFailure("memory: raw archive failed", l.mem.Record(ctx, contracts.Node{
		Key:   fmt.Sprintf("raw/%s/%s-%d", tail, epoch, seq),
		Kind:  contracts.KindTranscript,
		Title: fmt.Sprintf("turn %d", seq),
		Body:  fmt.Sprintf("%s: %s\n\nassistant: %s", prompt.Author, prompt.Content, reply),
	}))
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

func (l *Learner) consolidateLocked(ctx context.Context) error {
	l.stampMu.Lock()
	l.lastRun = l.now()
	l.stampMu.Unlock()
	defer func() { l.transitions = nil }()
	if l.extract == nil || l.mem == nil {
		return nil
	}
	firstErr := l.drain(ctx)

	journal := l.readJournalTail()
	var transcript string
	if sg, err := l.mem.Recall(ctx, l.session, 0); err == nil {
		transcript = sg.Root.Body
	}
	cands, err := l.extract.Extract(ctx, journal, transcript)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	for _, candidate := range cands {
		if l.seen[candidate.Node.Key] {
			continue
		}
		switch perr := l.persist(ctx, candidate); {
		case perr == nil:
			l.seen[candidate.Node.Key] = true
		case errors.Is(perr, errEnqueue):
			l.enqueue(candidate)
		default:
			if firstErr == nil {
				firstErr = perr
			}
		}
	}
	l.logFailure("memory: sweep failed", l.Sweep(ctx))
	l.logFailure("memory: merge failed", l.Merge(ctx))
	l.logFailure("memory: promote failed", l.Promote(ctx))
	l.logFailure("memory: report failed", l.report(ctx))
	return firstErr
}

func (l *Learner) consolidator() (Consolidator, bool) {
	c, ok := l.extract.(Consolidator)
	return c, ok
}

func (l *Learner) record(ctx context.Context, candidate Candidate) error {
	scope := l.scopeOf()
	if l.learnedSkills {
		candidate = asLearnedSkill(candidate, scope.Agent != "")
	}
	if candidate.Private {
		return contracts.RecordPrivate(ctx, l.mem, scope, candidate.Node)
	}
	return contracts.RecordShared(ctx, l.mem, scope, candidate.Node)
}

// SetScope re-roots memory, waiting for any consolidation in flight to finish
// first, so one pass never files half its findings under the old project and the
// other half under the new one. Lock order is mu → scopeMu, the same direction as
// mu → stampMu, so the three can never deadlock.
func (l *Learner) SetScope(scope contracts.MemoryScope) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Curator.SetScope(scope)
}

func (l *Learner) persist(ctx context.Context, candidate Candidate) error {
	err := l.record(ctx, candidate)
	if err == nil {
		return nil
	}
	var budgetErr *contracts.BudgetError
	if !errors.As(err, &budgetErr) {
		return err
	}
	cons, ok := l.consolidator()
	if !ok {
		slog.Warn("memory: candidate over budget and no consolidator; queued for retry",
			"key", candidate.Node.Key, "runes", budgetErr.Runes, "limit", budgetErr.Limit)
		return errEnqueue
	}
	merged, cerr := cons.Consolidate(ctx, candidate.Node, budgetErr.Limit)
	if cerr != nil || merged.Key != candidate.Node.Key || merged.Body == "" || utf8.RuneCountInString(merged.Body) > budgetErr.Limit {
		slog.Warn("memory: consolidation did not bring candidate within budget; queued for retry",
			"key", candidate.Node.Key, "runes", budgetErr.Runes, "limit", budgetErr.Limit, "err", cerr)
		return errEnqueue
	}
	candidate.Node = merged
	rerr := l.record(ctx, candidate)
	if rerr == nil {
		return nil
	}
	if errors.As(rerr, &budgetErr) {
		slog.Warn("memory: consolidated candidate still over budget; queued for retry",
			"key", candidate.Node.Key, "runes", budgetErr.Runes, "limit", budgetErr.Limit)
		return errEnqueue
	}
	return rerr
}

func (l *Learner) enqueue(candidate Candidate) {
	for _, p := range l.pending {
		if p.Node.Key == candidate.Node.Key {
			return
		}
	}
	l.pending = append(l.pending, candidate)
}

func (l *Learner) drain(ctx context.Context) error {
	var firstErr error
	var still []Candidate
	for _, candidate := range l.pending {
		switch perr := l.persist(ctx, candidate); {
		case perr == nil:
			l.seen[candidate.Node.Key] = true
		case errors.Is(perr, errEnqueue):
			still = append(still, candidate)
		default:
			if firstErr == nil {
				firstErr = perr
			}
			still = append(still, candidate)
		}
	}
	l.pending = still
	return firstErr
}

func (l *Learner) readJournalTail() string {
	if l.journal == "" {
		return ""
	}
	journalFile, err := os.Open(l.journal)
	if err != nil {
		return ""
	}
	defer func() { l.logFailure("memory: close journal failed", journalFile.Close()) }()
	if fi, statErr := journalFile.Stat(); statErr == nil && fi.Size() < l.offset {
		l.offset = 0
	}
	if l.offset > 0 {
		if _, seekErr := journalFile.Seek(l.offset, io.SeekStart); seekErr != nil {
			return ""
		}
	}
	data, err := io.ReadAll(journalFile)
	if err != nil {
		return ""
	}
	l.offset += int64(len(data))
	return string(data)
}
