// Package orchestrator is the default conversation-policy plugin: it primes each
// turn with a compact background block recalled from Memory and records the turn
// afterwards as a bounded rolling transcript on the session node. It is the
// minimal implementation of contracts.Orchestrator; a richer one (summarisation,
// multi-agent routing) registers under the same category to replace it.
package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

// maxTurns bounds the rolling transcript kept on a session node so memory never
// grows without bound for a long-lived channel.
const maxTurns = 20

// Transcript line budgets (in runes) for the inbound message and the reply.
const (
	maxContentChars = 100
	maxReplyChars   = 200
)

// Transition is one state change a curator pass made to one node, for the
// audit report. Kind is "sweep" (G3 staleness), "merge" (G2 umbrella-fold), or
// "restore" (an explicit reactivation).
type Transition struct {
	Key  string // node Key that changed
	From string // prior Meta[MetaState] ("" treated as StateActive)
	To   string // new Meta[MetaState]
	Kind string // "sweep" | "merge" | "restore"
}

// Curator is the default Orchestrator. With a nil Memory it still answers, just
// without continuity (Context returns "", Observe no-ops).
type Curator struct {
	mem     contracts.Memory
	session string // session node key
	// scope is guarded by scopeMu because SetScope can re-root a live session
	// from the turn goroutine while a background idle Consolidate is walking the
	// roots it is about to replace. Every read goes through scopeOf.
	scopeMu sync.RWMutex
	scope   contracts.MemoryScope // P1: shared project + private agent roots ({} = none)
	pending []contracts.Node      // hits from the model's last <recall>, surfaced next Context

	// learnedSkills gates the whole self-authored-skill feature: the <skill>
	// marker, the sentence in the preamble that announces it, the normalisation of
	// extractor candidates, and what LearnedSkills answers. One switch rather than
	// one per surface, because a marker that writes nothing and a preamble that
	// advertises it are two halves of the same mistake.
	learnedSkills bool

	// projected is name -> node Key for what LearnedSkills last answered, which is
	// what SkillUsed resolves an activated name against. It is written from the
	// host's projection goroutine and read from the turn goroutine, hence the lock.
	projectedMu sync.RWMutex
	projected   map[string]string

	now          func() time.Time // injectable clock (defaults to time.Now); tests override
	staleAfter   time.Duration    // age before a node is marked stale (<=0 disables)
	archiveAfter time.Duration    // age before a node is archived (<=0 disables)

	// transitions is this Consolidate pass's audit trail: every state change
	// Sweep/Merge/Restore made. Reset to nil at the end of Consolidate (see
	// learner.go) regardless of outcome, so a report never accumulates
	// duplicate rows across passes.
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
		scope:        scope,
		now:          time.Now,
		staleAfter:   30 * 24 * time.Hour,
		archiveAfter: 90 * 24 * time.Hour,
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
func (c *Curator) SetScope(s contracts.MemoryScope) {
	c.scopeMu.Lock()
	c.scope = s
	c.scopeMu.Unlock()
}

var _ contracts.Orchestrator = (*Curator)(nil)

// Context recalls the session node and its neighbours into a compact background
// block to prepend to the next prompt. It returns "" (never a turn-breaking
// error) when nothing is recalled yet — a first turn or a missing node.
func (c *Curator) Context(ctx context.Context) string {
	if c.mem == nil {
		return ""
	}
	var b strings.Builder
	// P1: durable shared (project) + private (agent) memory first, so the agent
	// recalls the game's lore/conventions and its own learned skills every turn.
	if scope := c.scopeOf(); scope.Project != "" {
		if sg, err := contracts.RecallScoped(ctx, c.mem, scope, 1); err == nil {
			writeNode(&b, sg.Root)
			for _, n := range sg.Nodes {
				// A KindSkill node is projected to disk and announced in the skills
				// menu, so reciting its body here would put the same instructions in
				// the same prompt twice: once in full, once in summary. The gate is
				// here and not in the store, where the raw transcript tier put its own,
				// precisely because a skill must stay reachable by memory search and
				// memory restore. It must simply not be recited.
				if n.Kind == contracts.KindSkill {
					continue
				}
				writeNode(&b, n)
			}
		}
	}
	// Then this session's rolling transcript for short-term continuity.
	if sg, err := c.mem.Recall(ctx, c.session, 1); err == nil {
		writeNode(&b, sg.Root)
		for _, n := range sg.Nodes {
			writeNode(&b, n)
		}
	}
	return c.frame(strings.TrimSpace(b.String()))
}

func writeNode(b *strings.Builder, n contracts.Node) {
	// Title/Body are attacker-controlled: shared project memory is multi-writer
	// (any agent of the game can RecordShared), so a forged "author → reply" turn
	// planted in a fact would be injected into every agent's context. Defang the
	// "→" separator the transcript uses (see turnLine); the title is collapsed to
	// one line so it can't break out of its "## " header.
	if title := oneline(n.Title, maxContentChars); title != "" {
		fmt.Fprintf(b, "## %s\n", strings.ReplaceAll(title, "→", "->"))
	}
	if body := strings.TrimSpace(n.Body); body != "" {
		b.WriteString(strings.ReplaceAll(body, "→", "->"))
		b.WriteByte('\n')
	}
}

// Observe records the turn by upserting the session node with a bounded rolling
// transcript, so the next Context call has continuity.
func (c *Curator) Observe(ctx context.Context, p contracts.Prompt, reply string) error {
	if c.mem == nil {
		return nil
	}
	var prev string
	if sg, err := c.mem.Recall(ctx, c.session, 0); err == nil {
		prev = sg.Root.Body
	}
	var b strings.Builder
	b.WriteString(turnLine(p.Author, p.Content, reply))
	if prev != "" {
		// prev is itself already capped to maxTurns lines; keep the newest
		// maxTurns-1 so the new line plus the tail stays within maxTurns.
		b.WriteByte('\n')
		b.WriteString(capLines(prev, maxTurns-1))
	}
	return c.mem.Record(ctx, contracts.Node{
		Key:   c.session,
		Kind:  contracts.KindSession,
		Title: "session " + strings.TrimPrefix(c.session, "sessions/"),
		Body:  b.String(),
	})
}

// Consolidate satisfies contracts.CurationHook. The default keeps a bounded
// rolling transcript inline (see Observe), so there is nothing to consolidate; a
// richer orchestrator overrides this with summarisation/pruning.
func (c *Curator) Consolidate(ctx context.Context) error { return nil }

// Close releases resources. The default orchestrator holds none (the Memory it
// composes is owned and closed by the host).
func (c *Curator) Close() error { return nil }

// turnLine renders one transcript line. The author and content are
// attacker-controlled (a display name, a message), so their " → " separators are
// defanged to "->" — collapsing whitespace already strips newlines — so a turn
// can't forge extra "author → reply" turns inside its own line.
func turnLine(author, content, reply string) string {
	author = strings.ReplaceAll(oneline(author, maxContentChars), "→", "->")
	content = strings.ReplaceAll(oneline(content, maxContentChars), "→", "->")
	return fmt.Sprintf("- %s: %s → %s", author, content, oneline(reply, maxReplyChars))
}

// oneline collapses s to a single space-separated line capped at limit runes.
func oneline(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s // byte len ≥ rune count, so no truncation possible: skip the []rune copy
	}
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "…"
	}
	return s
}

// capLines keeps the first n newline-separated lines (newest-first transcript).
func capLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// Slice at the n-th newline instead of splitting the whole string and re-joining.
	count := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if count++; count == n {
				return s[:i]
			}
		}
	}
	return s
}
