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

// Curator is the default Orchestrator. With a nil Memory it still answers, just
// without continuity (Context returns "", Observe no-ops).
type Curator struct {
	mem     contracts.Memory
	session string                // session node key
	scope   contracts.MemoryScope // P1: shared project + private agent roots ({} = none)
	pending []contracts.Node      // hits from the model's last <recall>, surfaced next Context

	now          func() time.Time // injectable clock (defaults to time.Now); tests override
	staleAfter   time.Duration    // age before a node is marked stale (<=0 disables)
	archiveAfter time.Duration    // age before a node is archived (<=0 disables)
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
	if c.scope.Project != "" {
		if sg, err := contracts.RecallScoped(ctx, c.mem, c.scope, 1); err == nil {
			writeNode(&b, sg.Root)
			for _, n := range sg.Nodes {
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
