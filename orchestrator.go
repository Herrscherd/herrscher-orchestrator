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
}

// New builds the default orchestrator for session over mem (mem may be nil),
// with no memory scope (the session transcript is the only continuity).
func New(mem contracts.Memory, session string) *Curator {
	return NewScoped(mem, session, contracts.MemoryScope{})
}

// NewScoped is New plus a MemoryScope (P1): Context also surfaces the shared
// project memory and this agent's private skills via contracts.RecallScoped.
func NewScoped(mem contracts.Memory, session string, scope contracts.MemoryScope) *Curator {
	return &Curator{mem: mem, session: "sessions/" + session, scope: scope}
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
	return strings.TrimSpace(b.String())
}

func writeNode(b *strings.Builder, n contracts.Node) {
	if n.Title != "" {
		fmt.Fprintf(b, "## %s\n", n.Title)
	}
	if body := strings.TrimSpace(n.Body); body != "" {
		b.WriteString(body)
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
	body := turnLine(p.Author, p.Content, reply)
	if prev != "" {
		body += "\n" + prev
	}
	return c.mem.Record(ctx, contracts.Node{
		Key:   c.session,
		Kind:  contracts.KindSession,
		Title: "session " + strings.TrimPrefix(c.session, "sessions/"),
		Body:  capLines(body, maxTurns),
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

// oneline collapses s to a single space-separated line capped at max runes.
func oneline(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// capLines keeps the first n newline-separated lines (newest-first transcript).
func capLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
