// Package orchestrator is the default conversation-policy plugin: it primes each
// turn with a compact background block recalled from Memory and records the turn
// afterwards as a bounded rolling transcript on the session node. It is the
// minimal implementation of contracts.Orchestrator; a richer one (summarisation,
// multi-agent routing) registers under the same category to replace it.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

const maxTurns = 20

const (
	maxContentChars = 100
	maxReplyChars   = 200
)

// TransitionKind names the curator pass that made a Transition.
type TransitionKind string

// Transition kinds: TransitionSweep for G3 staleness, TransitionMerge for G2
// umbrella folds, and TransitionRestore for an explicit reactivation.
const (
	TransitionSweep   TransitionKind = "sweep"
	TransitionMerge   TransitionKind = "merge"
	TransitionRestore TransitionKind = "restore"
)

// Transition is one state change a curator pass made to one node, for the
// audit report. Kind is TransitionSweep (G3 staleness), TransitionMerge (G2
// umbrella-fold), or TransitionRestore (an explicit reactivation).
type Transition struct {
	Key  string
	From string
	To   string
	Kind TransitionKind
}

// Curator is the default Orchestrator. With a nil Memory it still answers, just
// without continuity (Context returns "", Observe no-ops).
type Curator struct {
	mem     contracts.Memory
	session string
	log     *slog.Logger
	scopeMu sync.RWMutex
	scope   contracts.MemoryScope
	pending []contracts.Node

	learnedSkills bool

	projectedMu sync.RWMutex
	projected   map[string]string

	now          func() time.Time
	staleAfter   time.Duration
	archiveAfter time.Duration

	transitions []Transition
}

// New builds the default orchestrator for session over mem (mem may be nil),
// with no memory scope (the session transcript is the only continuity).
func New(mem contracts.Memory, session string) *Curator {
	return NewScoped(mem, session, contracts.MemoryScope{})
}

// NewScoped is New plus a MemoryScope (P1): Context also surfaces the shared
// project memory and this agent's private skills via contracts.RecallScoped.
func NewScoped(mem contracts.Memory, session string, scope contracts.MemoryScope) *Curator {
	return &Curator{
		mem:          mem,
		session:      "sessions/" + session,
		log:          slog.New(slog.DiscardHandler),
		scope:        scope,
		now:          time.Now,
		staleAfter:   30 * 24 * time.Hour,
		archiveAfter: 90 * 24 * time.Hour,
	}
}

// SetLogger routes the warnings of best-effort memory writes to log.
func (c *Curator) SetLogger(log *slog.Logger) {
	c.log = log
}

func (c *Curator) logFailure(msg string, err error) {
	if err != nil {
		c.log.Warn("best-effort step failed", "step", msg, "error", err)
	}
}

// SetStaleness configures the age windows used by Sweep. A window <= 0 disables
// that transition (nodes never reach that state). The host wires this from
// AGENT_STALE_DAYS / AGENT_ARCHIVE_DAYS in register.go.
func (c *Curator) SetStaleness(staleAfter, archiveAfter time.Duration) {
	c.staleAfter = staleAfter
	c.archiveAfter = archiveAfter
}

func (c *Curator) scopeOf() contracts.MemoryScope {
	c.scopeMu.RLock()
	defer c.scopeMu.RUnlock()
	return c.scope
}

// SetScope re-roots memory mid-session. The host calls it once, when a session's
// first prompt settles a project its launch could only guess at; what was already
// filed stays filed, because a fact recorded under the guess is still true.
func (c *Curator) SetScope(scope contracts.MemoryScope) {
	c.scopeMu.Lock()
	c.scope = scope
	c.scopeMu.Unlock()
}

var _ contracts.Orchestrator = (*Curator)(nil)

// Context recalls the session node and its neighbours into a compact background
// block to prepend to the next prompt. It returns "" (never a turn-breaking
// error) when nothing is recalled yet: a first turn or a missing node.
func (c *Curator) Context(ctx context.Context) string {
	if c.mem == nil {
		return ""
	}
	var builder strings.Builder
	if scope := c.scopeOf(); scope.Project != "" {
		if recalled, err := contracts.RecallScoped(ctx, c.mem, scope, 1); err == nil {
			writeNode(&builder, recalled.Root)
			for _, node := range recalled.Nodes {
				if node.Kind == contracts.KindSkill {
					continue
				}
				writeNode(&builder, node)
			}
		}
	}
	if recalled, err := c.mem.Recall(ctx, c.session, 1); err == nil {
		writeNode(&builder, recalled.Root)
		for _, n := range recalled.Nodes {
			writeNode(&builder, n)
		}
	}
	return c.frame(strings.TrimSpace(builder.String()))
}

func writeNode(builder *strings.Builder, node contracts.Node) {
	if title := oneline(node.Title, maxContentChars); title != "" {
		fmt.Fprintf(builder, "## %s\n", strings.ReplaceAll(title, "→", "->"))
	}
	if body := strings.TrimSpace(node.Body); body != "" {
		builder.WriteString(strings.ReplaceAll(body, "→", "->"))
		builder.WriteByte('\n')
	}
}

// Observe records the turn by upserting the session node with a bounded rolling
// transcript, so the next Context call has continuity.
func (c *Curator) Observe(ctx context.Context, prompt contracts.Prompt, reply string) error {
	if c.mem == nil {
		return nil
	}
	var prev string
	if sg, err := c.mem.Recall(ctx, c.session, 0); err == nil {
		prev = sg.Root.Body
	}
	var builder strings.Builder
	builder.WriteString(turnLine(prompt.Author, prompt.Content, reply))
	if prev != "" {
		builder.WriteByte('\n')
		builder.WriteString(capLines(prev, maxTurns-1))
	}
	return c.mem.Record(ctx, contracts.Node{
		Key:   c.session,
		Kind:  contracts.KindSession,
		Title: "session " + strings.TrimPrefix(c.session, "sessions/"),
		Body:  builder.String(),
	})
}

// Consolidate satisfies contracts.CurationHook. The default keeps a bounded
// rolling transcript inline (see Observe), so there is nothing to consolidate; a
// richer orchestrator overrides this with summarisation/pruning.
func (c *Curator) Consolidate(_ context.Context) error { return nil }

// Close releases resources. The default orchestrator holds none (the Memory it
// composes is owned and closed by the host).
func (c *Curator) Close() error { return nil }

func turnLine(author, content, reply string) string {
	author = strings.ReplaceAll(oneline(author, maxContentChars), "→", "->")
	content = strings.ReplaceAll(oneline(content, maxContentChars), "→", "->")
	return fmt.Sprintf("- %s: %s → %s", author, content, oneline(reply, maxReplyChars))
}

func oneline(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	if r := []rune(text); len(r) > limit {
		text = string(r[:limit]) + "…"
	}
	return text
}

func capLines(text string, maxLines int) string {
	if maxLines <= 0 {
		return ""
	}
	count := 0
	for index := range len(text) {
		if text[index] == '\n' {
			count++
			if count == maxLines {
				return text[:index]
			}
		}
	}
	return text
}
