package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

// Candidate is a memory node proposed for persistence. Private keeps it in the
// agent's own scope instead of the shared project scope.
type Candidate struct {
	Node    contracts.Node
	Private bool
}

// Extractor turns the call journal and the session transcript of a stretch of
// work into memory candidates. Implementations decide what is worth remembering.
type Extractor interface {
	Extract(ctx context.Context, journal, transcript string) ([]Candidate, error)
}

// Consolidator shrinks an over-budget candidate so a refused Record can be
// retried. The returned Body should fit the limit in runes.
type Consolidator interface {
	Consolidate(ctx context.Context, over contracts.Node, limit int) (contracts.Node, error)
}

var errEnqueue = errors.New("orchestrator: candidate over budget, queued for retry")

// Learner is an Orchestrator that learns. Consolidate runs the Extractor over
// the journal and transcript and persists facts under the shared project scope
// and skills under the agent scope. Consolidation passes never interleave.
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
	mergeTarget mergeTarget

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

	openJournal func(string) (journalSource, error)
}

var _ contracts.Orchestrator = (*Learner)(nil)

const defaultIdlePoll = 10 * time.Minute

// NewLearner returns a Learner over mem. A nil extractor makes Consolidate a no-
// op.
func NewLearner(mem contracts.Memory, session string, scope contracts.MemoryScope, ex Extractor, journal string, every int) *Learner {
	return &Learner{Curator: NewScoped(mem, session, scope), extract: ex, journal: journal, every: every, seen: make(map[string]bool), idlePoll: defaultIdlePoll, mergeTarget: mergeTargetStale, openJournal: openJournalFile}
}

// SetIdle enables the inactivity trigger when sinceLastRunDays is positive.
// Consolidate then runs in the background once that many days have passed since
// the last run and idleHours have passed without a turn. A non-positive
// sinceLastRunDays disables it.
func (l *Learner) SetIdle(sinceLastRunDays, idleHours int) {
	l.idleDays = sinceLastRunDays
	l.idleHours = idleHours
}

// SetRawArchive turns the raw transcript archive on or off. When on, each
// observed turn is stored untruncated, best-effort. It is off by default.
func (l *Learner) SetRawArchive(on bool) {
	l.rawArchive = on
}

// DueForIdleRun reports whether an idle Consolidate should fire at now, given
// the time of the last observed activity. It is pure: now is a parameter. It is
// false when the trigger is disabled or no pass has run yet.
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

// Start launches the idle loop until ctx is cancelled. It does nothing when the
// idle trigger is disabled or the loop is already running.
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

// Observe records the turn and stamps the activity clock. Every `every` turns it
// also runs Consolidate before returning. The stamp itself never waits on a
// Consolidate in progress.
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

// Consolidate runs one learning pass: it retries queued candidates, extracts new
// ones from the journal tail and the transcript, and persists them under the
// right scope. It is best-effort: a nil extractor or a nil Memory is a no-op,
// and a missing journal is skipped.
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

// SetScope re-roots memory. It waits for a Consolidate in progress to finish, so
// one pass never files findings under two projects.
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
		l.log.Warn("memory: candidate over budget and no consolidator; queued for retry",
			"key", candidate.Node.Key, "runes", budgetErr.Runes, "limit", budgetErr.Limit)
		return errEnqueue
	}
	merged, cerr := cons.Consolidate(ctx, candidate.Node, budgetErr.Limit)
	if cerr != nil || merged.Key != candidate.Node.Key || merged.Body == "" || utf8.RuneCountInString(merged.Body) > budgetErr.Limit {
		l.log.Warn("memory: consolidation did not bring candidate within budget; queued for retry",
			"key", candidate.Node.Key, "runes", budgetErr.Runes, "limit", budgetErr.Limit, "err", cerr)
		return errEnqueue
	}
	candidate.Node = merged
	rerr := l.record(ctx, candidate)
	if rerr == nil {
		return nil
	}
	if errors.As(rerr, &budgetErr) {
		l.log.Warn("memory: consolidated candidate still over budget; queued for retry",
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

type journalSource interface {
	io.ReadSeekCloser
	Stat() (fs.FileInfo, error)
}

func openJournalFile(name string) (journalSource, error) {
	file, err := os.Open(filepath.Clean(name))
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (l *Learner) readJournalTail() string {
	if l.journal == "" {
		return ""
	}
	open := l.openJournal
	if open == nil {
		open = openJournalFile
	}
	journalFile, err := open(l.journal)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			l.logFailure("memory: open journal failed", err)
		}
		return ""
	}
	defer func() { l.logFailure("memory: close journal failed", journalFile.Close()) }()
	if fi, statErr := journalFile.Stat(); statErr == nil && fi.Size() < l.offset {
		l.offset = 0
	}
	if l.offset > 0 {
		if _, seekErr := journalFile.Seek(l.offset, io.SeekStart); seekErr != nil {
			l.logFailure("memory: seek journal failed", seekErr)
			return ""
		}
	}
	data, err := io.ReadAll(journalFile)
	if err != nil {
		l.logFailure("memory: read journal failed", err)
		return ""
	}
	l.offset += int64(len(data))
	return string(data)
}
