// Package orchestrator provides the default Orchestrator. It recalls memory into
// each turn's context and records the turn on the session node.
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

// TransitionSweep, TransitionMerge and TransitionRestore are the kinds of
// Transition.
const (
	TransitionSweep   TransitionKind = "sweep"
	TransitionMerge   TransitionKind = "merge"
	TransitionRestore TransitionKind = "restore"
)

// Transition is one state change a curator pass made to one node, reported in
// the audit report. Kind names the pass that made it.
type Transition struct {
	Key  string
	From string
	To   string
	Kind TransitionKind
}

// Curator is the default Orchestrator. With a nil Memory it still answers,
// without continuity.
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

// New returns the default orchestrator for session over mem, with no memory
// scope.
func New(mem contracts.Memory, session string) *Curator {
	return NewScoped(mem, session, contracts.MemoryScope{})
}

// NewScoped is New with a MemoryScope. Context then also recalls the scoped
// memory.
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

// SetLogger sets where warnings from best-effort memory writes are logged.
func (c *Curator) SetLogger(log *slog.Logger) {
	c.log = log
}

func (c *Curator) logFailure(msg string, err error) {
	if err != nil {
		c.log.Warn("best-effort step failed", "step", msg, "error", err)
	}
}

// SetStaleness sets the age windows used by Sweep. A window of zero or less
// disables that transition.
func (c *Curator) SetStaleness(staleAfter, archiveAfter time.Duration) {
	c.staleAfter = staleAfter
	c.archiveAfter = archiveAfter
}

func (c *Curator) scopeOf() contracts.MemoryScope {
	c.scopeMu.RLock()
	defer c.scopeMu.RUnlock()
	return c.scope
}

// SetScope re-roots memory. Nodes already filed stay where they were.
func (c *Curator) SetScope(scope contracts.MemoryScope) {
	c.scopeMu.Lock()
	c.scope = scope
	c.scopeMu.Unlock()
}

var _ contracts.Orchestrator = (*Curator)(nil)

// Context returns a compact background block to prepend to the next prompt, or
// the empty string when nothing is recalled yet. It never fails the turn.
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

// Observe records the turn on the session node as a bounded rolling transcript,
// so the next Context has continuity.
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

// Consolidate satisfies contracts.CurationHook. The default has nothing to
// consolidate; richer orchestrators override it.
func (c *Curator) Consolidate(_ context.Context) error { return nil }

// Close releases the Curator's resources.
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
