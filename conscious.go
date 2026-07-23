package orchestrator

import (
	"context"
	"regexp"
	"strings"

	"github.com/Herrscherd/herrscher-contracts"
)

// recallK bounds how many nodes a single <recall> surfaces, keeping the
// next turn's primed context small.
const recallK = 5

// maxPending caps how many recall hits accumulate before the next Context
// surfaces them, so a reply emitting many <recall> markers can't grow the
// following prompt without bound.
const maxPending = 12

// memoryPreamble is the compact, always-on affordance that makes every backend
// conscious of its persistent memory: that it exists, spans sessions, and can be
// actively searched and written — not just passively received.
const memoryPreamble = "This is your persistent memory (session · project · agent), recalled across sessions. " +
	"Search it any time by emitting <recall>your query</recall> — its hits arrive next turn. " +
	"Store a durable fact with <remember>the fact</remember>."

// recallMarker / rememberMarker mirror the skills engine's useMarker: case- and
// whitespace-tolerant, non-greedy, matched across newlines so a multi-line fact
// is captured whole.
var (
	recallMarker   = regexp.MustCompile(`(?is)<\s*recall\s*>\s*(.+?)\s*<\s*/\s*recall\s*>`)
	rememberMarker = regexp.MustCompile(`(?is)<\s*remember\s*>\s*(.+?)\s*<\s*/\s*remember\s*>`)
)

var _ contracts.TurnReactor = (*Curator)(nil)

// React handles the in-band memory markers the model emits: <remember> stores a
// durable fact immediately, <recall> searches memory and stashes the hits for the
// next Context call. Both markers are stripped from the returned reply so the
// human never sees them. It is best-effort — a memory failure never breaks the
// turn — and a no-op when there is no Memory.
func (c *Curator) React(ctx context.Context, reply string) string {
	if c.mem == nil {
		return reply
	}
	for _, m := range rememberMarker.FindAllStringSubmatch(reply, -1) {
		c.remember(ctx, strings.TrimSpace(m[1]))
	}
	for _, m := range recallMarker.FindAllStringSubmatch(reply, -1) {
		c.recall(ctx, strings.TrimSpace(m[1]))
	}
	reply = rememberMarker.ReplaceAllString(reply, "")
	reply = recallMarker.ReplaceAllString(reply, "")
	return strings.TrimSpace(reply)
}

// recall searches memory for query and stashes the top-k hits so the next
// Context call surfaces them (the two-turn progressive-disclosure round trip).
func (c *Curator) recall(ctx context.Context, query string) {
	if query == "" || len(c.pending) >= maxPending {
		return
	}
	var hits []contracts.Node
	if c.scope.Project != "" {
		hits, _ = contracts.RecallRelevant(ctx, c.mem, c.scope, query, recallK)
	} else {
		hits, _ = c.mem.Search(ctx, contracts.Query{Text: query, Ranked: true, Limit: recallK})
	}
	if room := maxPending - len(c.pending); len(hits) > room {
		hits = hits[:room]
	}
	c.pending = append(c.pending, hits...)
}

// remember stores fact durably. With a project scope it is shared (visible to
// future sessions and agents of that project); with none it hangs off the session
// node so it is at least not lost. The Key is deterministic (a slug of the fact
// head) so re-remembering the same fact updates in place instead of duplicating.
func (c *Curator) remember(ctx context.Context, fact string) {
	if fact == "" {
		return
	}
	title := oneline(fact, maxContentChars)
	node := contracts.Node{Kind: contracts.KindDecision, Title: title, Body: fact}
	if c.scope.Project != "" {
		node.Key = c.scope.Project + "/notes/" + slug(title)
		_ = contracts.RecordShared(ctx, c.mem, c.scope, node)
		return
	}
	node.Key = c.session + "/notes/" + slug(title)
	_ = c.mem.Record(ctx, node)
}

// frame wraps the recalled digest in the always-on <memory> affordance and
// appends any pending recall hits (from the model's last <recall>), clearing them
// so each hit is surfaced exactly once.
func (c *Curator) frame(digest string) string {
	var b strings.Builder
	b.WriteString("<memory>\n")
	b.WriteString(memoryPreamble)
	if digest != "" {
		b.WriteString("\n\n")
		b.WriteString(digest)
	}
	if len(c.pending) > 0 {
		b.WriteString("\n\n## results of your last <recall>\n")
		for _, n := range c.pending {
			writeNode(&b, n)
		}
		c.pending = nil
	}
	b.WriteString("\n</memory>")
	return b.String()
}

// slug folds a fact head into a stable, filesystem-safe key segment: lowercase,
// separator runs collapsed to a single hyphen, trimmed, and length-capped so a
// long fact still yields a short key.
func slug(s string) string {
	var b strings.Builder
	pendingSep := false
	for _, r := range strings.TrimSpace(strings.ToLower(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if pendingSep && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingSep = false
			b.WriteRune(r)
			if b.Len() >= 60 {
				return b.String()
			}
		default:
			pendingSep = true
		}
	}
	if b.Len() == 0 {
		return "note"
	}
	return b.String()
}
